package execution

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

var ErrPlatformIdentityMismatch = errors.New("platform execution identity mismatch")

var (
	ErrInvalidPreparedEnsure  = errors.New("invalid prepared platform ensure")
	ErrPreparedEnsureConsumed = errors.New("prepared platform ensure already consumed")
	ErrInvalidClaimedEnsure   = errors.New("invalid claimed platform ensure")
	ErrClaimedEnsureConsumed  = errors.New("claimed platform ensure already consumed")
	ErrInvalidPreparedCancel  = errors.New("invalid prepared platform cancel")
	ErrPreparedCancelConsumed = errors.New("prepared platform cancel already consumed")
)

type BindingPurpose string

const (
	PurposeExecutionSnapshot BindingPurpose = "execution_snapshot"
	PurposeAgentStagePlan    BindingPurpose = "agent_stage_plan"
	PurposeReviewInput       BindingPurpose = "review_input"
	PurposeUpstream          BindingPurpose = "upstream"
	PurposeStageOutput       BindingPurpose = "stage_output"
	PurposeTraceManifest     BindingPurpose = "trace_manifest"
)

// ArtifactResolver verifies authorization and immutable content binding. A URI
// is never handed to the platform until the resolver has checked the current
// tenant/workspace authority, digest, size, and declared contract.
type ArtifactResolver interface {
	Verify(
		context.Context,
		string,
		string,
		contractsv1alpha1.ArtifactBinding,
		BindingPurpose,
	) error
}

// DurableClaimAuthorizer independently proves that the exact request already
// has a durable dispatch claim. Both first Ensure and recovery pass this gate;
// a caller cannot turn Preflight or possession of request bytes into provider
// start authority.
type DurableClaimAuthorizer interface {
	AuthorizeClaimedEnsure(
		context.Context,
		contractsv1alpha1.StageExecutionRequest,
	) error
	AuthorizeTerminalCancel(context.Context, CancelRequest) error
}

type Gateway struct {
	port     PlatformPort
	resolver ArtifactResolver
	claims   DurableClaimAuthorizer
}

// PreparedEnsure is an opaque, single-use proof that the exact request it
// contains passed validation and immutable artifact resolution on its owning
// Gateway. Its payload is private so callers cannot preflight request A and
// substitute request B after acquiring durable ensure ownership.
type PreparedEnsure struct {
	value *preparedEnsure
}

type preparedEnsure struct {
	owner         *Gateway
	request       contractsv1alpha1.StageExecutionRequest
	requestSHA256 string
	consumed      atomic.Bool
}

// PreparedClaimedEnsure is an opaque, single-use durable-claim proof for an
// exact persisted request. It can be recreated after restart by rechecking the
// durable claim and never reauthorizes governed artifacts.
type PreparedClaimedEnsure struct {
	value *preparedClaimedEnsure
}

type preparedClaimedEnsure struct {
	owner    *Gateway
	request  contractsv1alpha1.StageExecutionRequest
	consumed atomic.Bool
}

// PreparedCancel is an opaque, single-use proof that a durable terminal
// cancellation gate authorizes the exact provider target.
type PreparedCancel struct {
	value *preparedCancel
}

type preparedCancel struct {
	owner    *Gateway
	request  CancelRequest
	consumed atomic.Bool
}

// Request returns a detached copy suitable for durable intent persistence.
func (prepared PreparedEnsure) Request() contractsv1alpha1.StageExecutionRequest {
	if prepared.value == nil {
		return contractsv1alpha1.StageExecutionRequest{}
	}
	return cloneStageExecutionRequest(prepared.value.request)
}

func (prepared PreparedEnsure) RequestSHA256() string {
	if prepared.value == nil {
		return ""
	}
	return prepared.value.requestSHA256
}

func NewGateway(
	port PlatformPort,
	resolver ArtifactResolver,
	claims DurableClaimAuthorizer,
) (*Gateway, error) {
	if port == nil || resolver == nil || claims == nil {
		return nil, fmt.Errorf(
			"platform port, artifact resolver, and durable claim authorizer are required",
		)
	}
	return &Gateway{port: port, resolver: resolver, claims: claims}, nil
}

// Preflight completes every deterministic request and artifact check that can
// fail before durable provider-ensure ownership is claimed. Formal dispatch
// must call this before committing its claim.
func (gateway *Gateway) Preflight(
	ctx context.Context,
	request contractsv1alpha1.StageExecutionRequest,
) (PreparedEnsure, error) {
	if ctx == nil {
		return PreparedEnsure{}, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return PreparedEnsure{}, err
	}
	request = cloneStageExecutionRequest(request)
	if err := request.Validate(); err != nil {
		return PreparedEnsure{}, fmt.Errorf("validate stage execution request: %w", err)
	}
	if err := gateway.verifyRequestArtifacts(ctx, request); err != nil {
		return PreparedEnsure{}, err
	}
	return PreparedEnsure{value: &preparedEnsure{
		owner:         gateway,
		request:       request,
		requestSHA256: request.RequestSHA256,
	}}, nil
}

// EnsurePrepared invokes provider Ensure without accepting another request or
// repeating fallible validation/artifact resolution. It is the first
// post-claim call and consumes the private preflight proof exactly once.
func (gateway *Gateway) EnsurePrepared(
	ctx context.Context,
	prepared PreparedEnsure,
) (Handle, error) {
	if ctx == nil {
		return Handle{}, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return Handle{}, err
	}
	if prepared.value == nil || prepared.value.owner != gateway ||
		prepared.value.requestSHA256 == "" ||
		prepared.value.request.RequestSHA256 != prepared.value.requestSHA256 {
		return Handle{}, ErrInvalidPreparedEnsure
	}
	if err := gateway.claims.AuthorizeClaimedEnsure(
		ctx,
		cloneStageExecutionRequest(prepared.value.request),
	); err != nil {
		return Handle{}, fmt.Errorf("authorize durable claimed ensure: %w", err)
	}
	if !prepared.value.consumed.CompareAndSwap(false, true) {
		return Handle{}, ErrPreparedEnsureConsumed
	}
	return gateway.ensure(ctx, prepared.value.request)
}

// PrepareClaimedEnsure proves durable claim authority without resolving
// governed artifacts. Recovery must survive later input quarantine or
// revocation, but it must not accept mere possession of raw request bytes.
func (gateway *Gateway) PrepareClaimedEnsure(
	ctx context.Context,
	request contractsv1alpha1.StageExecutionRequest,
) (PreparedClaimedEnsure, error) {
	if ctx == nil {
		return PreparedClaimedEnsure{}, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return PreparedClaimedEnsure{}, err
	}
	request = cloneStageExecutionRequest(request)
	if err := request.Validate(); err != nil {
		return PreparedClaimedEnsure{}, fmt.Errorf(
			"validate claimed stage execution ensure: %w",
			err,
		)
	}
	if err := gateway.claims.AuthorizeClaimedEnsure(ctx, request); err != nil {
		return PreparedClaimedEnsure{}, fmt.Errorf("authorize durable claimed ensure: %w", err)
	}
	return PreparedClaimedEnsure{value: &preparedClaimedEnsure{
		owner: gateway, request: request,
	}}, nil
}

// EnsureClaimed consumes only an opaque proof returned by
// PrepareClaimedEnsure. PlatformPort.Ensure supplies the provider-side atomic
// exact-idempotency fence.
func (gateway *Gateway) EnsureClaimed(
	ctx context.Context,
	prepared PreparedClaimedEnsure,
) (Handle, error) {
	if ctx == nil {
		return Handle{}, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return Handle{}, err
	}
	if prepared.value == nil || prepared.value.owner != gateway {
		return Handle{}, ErrInvalidClaimedEnsure
	}
	if !prepared.value.consumed.CompareAndSwap(false, true) {
		return Handle{}, ErrClaimedEnsureConsumed
	}
	return gateway.ensure(ctx, prepared.value.request)
}

func (gateway *Gateway) ensure(
	ctx context.Context,
	request contractsv1alpha1.StageExecutionRequest,
) (Handle, error) {
	handle, err := gateway.port.Ensure(ctx, cloneStageExecutionRequest(request))
	if err != nil {
		return Handle{}, err
	}
	if err := validateHandle(request, handle); err != nil {
		return Handle{}, err
	}
	return handle, nil
}

// Lookup observes a prior Ensure outcome by exact request identity. Immutable
// inputs were already resolved before the durable Ensure claim; lookup
// intentionally does not reauthorize or dereference
// them because revocation must not make provider-handle reconciliation
// impossible. Lookup may disclose only the exact opaque execution handle.
func (gateway *Gateway) Lookup(
	ctx context.Context,
	request contractsv1alpha1.StageExecutionRequest,
) (Handle, bool, error) {
	if ctx == nil {
		return Handle{}, false, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return Handle{}, false, err
	}
	if err := request.Validate(); err != nil {
		return Handle{}, false, fmt.Errorf("validate stage execution lookup: %w", err)
	}
	handle, found, err := gateway.port.Lookup(ctx, cloneStageExecutionRequest(request))
	if err != nil {
		return Handle{}, false, err
	}
	if !found {
		return Handle{}, false, nil
	}
	if err := validateHandle(request, handle); err != nil {
		return Handle{}, false, err
	}
	return handle, true, nil
}

func (gateway *Gateway) verifyRequestArtifacts(
	ctx context.Context,
	request contractsv1alpha1.StageExecutionRequest,
) error {
	for _, candidate := range []struct {
		binding contractsv1alpha1.ArtifactBinding
		purpose BindingPurpose
	}{
		{request.Plan, PurposeAgentStagePlan},
		{request.ExecutionSnapshot, PurposeExecutionSnapshot},
		{request.ReviewInput, PurposeReviewInput},
	} {
		if err := gateway.resolver.Verify(
			ctx,
			request.TenantID,
			request.WorkspaceID,
			candidate.binding,
			candidate.purpose,
		); err != nil {
			return fmt.Errorf(
				"resolve %s artifact: %w",
				candidate.purpose,
				err,
			)
		}
	}
	for index, binding := range request.Upstream {
		if err := gateway.resolver.Verify(
			ctx,
			request.TenantID,
			request.WorkspaceID,
			binding,
			PurposeUpstream,
		); err != nil {
			return fmt.Errorf("resolve upstream[%d] artifact: %w", index, err)
		}
	}
	return nil
}

// AdmitResult applies the exact echoed-request fence before resolving
// provider-owned result artifacts. The caller must separately apply the
// host-observed lease/deadline terminal gate before evidence admission.
func (gateway *Gateway) AdmitResult(
	ctx context.Context,
	request contractsv1alpha1.StageExecutionRequest,
	result contractsv1alpha1.StageExecutionResult,
) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := contractsv1alpha1.ValidateStageExecutionResultBinding(
		request,
		result,
	); err != nil {
		return fmt.Errorf("%w: %v", ErrPlatformIdentityMismatch, err)
	}
	if result.Output != nil {
		if err := gateway.resolver.Verify(
			ctx,
			request.TenantID,
			request.WorkspaceID,
			*result.Output,
			PurposeStageOutput,
		); err != nil {
			return fmt.Errorf("resolve stage output artifact: %w", err)
		}
	}
	if result.TraceManifest != nil {
		if err := gateway.resolver.Verify(
			ctx,
			request.TenantID,
			request.WorkspaceID,
			*result.TraceManifest,
			PurposeTraceManifest,
		); err != nil {
			return fmt.Errorf("resolve trace manifest artifact: %w", err)
		}
	}
	return nil
}

func (gateway *Gateway) PrepareCancel(
	ctx context.Context,
	handle Handle,
	authority TerminalCancelAuthority,
) (PreparedCancel, error) {
	if ctx == nil {
		return PreparedCancel{}, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return PreparedCancel{}, err
	}
	if err := validateHandleIdentity(handle); err != nil {
		return PreparedCancel{}, err
	}
	if err := validateTerminalCancelAuthority(authority); err != nil {
		return PreparedCancel{}, err
	}
	if authority.CancelIdempotencyKey == handle.IdempotencyKey {
		return PreparedCancel{}, fmt.Errorf(
			"cancel idempotency key must be non-empty and distinct from create",
		)
	}
	request := CancelRequest{
		Authority:            authority,
		ProviderHandle:       handle.ProviderHandle,
		ExecutionID:          handle.ExecutionID,
		Attempt:              handle.Attempt,
		Generation:           handle.Generation,
		FencingToken:         handle.FencingToken,
		CreateIdempotencyKey: handle.IdempotencyKey,
		CancelIdempotencyKey: authority.CancelIdempotencyKey,
		RequestSHA256:        handle.RequestSHA256,
		CapabilitySHA256:     handle.CapabilitySHA256,
	}
	if err := gateway.claims.AuthorizeTerminalCancel(ctx, request); err != nil {
		return PreparedCancel{}, fmt.Errorf("authorize durable terminal cancellation: %w", err)
	}
	return PreparedCancel{value: &preparedCancel{owner: gateway, request: request}}, nil
}

func validateTerminalCancelAuthority(authority TerminalCancelAuthority) error {
	for name, value := range map[string]string{
		"tenant_id":              authority.TenantID,
		"workspace_id":           authority.WorkspaceID,
		"review_run_id":          authority.ReviewRunID,
		"stage_id":               authority.StageID,
		"stage_revision":         authority.StageRevision,
		"stage_sha256":           authority.StageSHA256,
		"dispatch_intent_id":     authority.IntentID,
		"dispatch_intent_sha256": authority.IntentSHA256,
		"terminal_gate_id":       authority.TerminalGateID,
		"terminal_gate_sha256":   authority.TerminalGateSHA256,
		"cancel_idempotency_key": authority.CancelIdempotencyKey,
	} {
		if value == "" {
			return fmt.Errorf("terminal cancel authority %s is required", name)
		}
	}
	if authority.CancelIdempotencyKey == authority.IntentID {
		return fmt.Errorf("terminal cancel authority uses a conflated idempotency key")
	}
	return nil
}

func (gateway *Gateway) CancelPrepared(ctx context.Context, prepared PreparedCancel) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if prepared.value == nil || prepared.value.owner != gateway {
		return ErrInvalidPreparedCancel
	}
	if !prepared.value.consumed.CompareAndSwap(false, true) {
		return ErrPreparedCancelConsumed
	}
	return gateway.port.Cancel(ctx, prepared.value.request)
}

func validateHandle(
	request contractsv1alpha1.StageExecutionRequest,
	handle Handle,
) error {
	if err := validateHandleIdentity(handle); err != nil {
		return err
	}
	if handle.ExecutionID != request.ExecutionID ||
		handle.Attempt != request.Attempt ||
		handle.Generation != request.Generation ||
		handle.FencingToken != request.FencingToken ||
		handle.IdempotencyKey != request.IdempotencyKey ||
		handle.RequestSHA256 != request.RequestSHA256 ||
		handle.CapabilitySHA256 != request.Capability.SHA256 {
		return ErrPlatformIdentityMismatch
	}
	return nil
}

func validateHandleIdentity(handle Handle) error {
	if err := validateProviderHandle(handle.ProviderHandle); err != nil {
		return err
	}
	if handle.ExecutionID == "" || handle.Attempt < 1 ||
		handle.Generation < 1 || handle.FencingToken == 0 ||
		handle.IdempotencyKey == "" || handle.RequestSHA256 == "" ||
		handle.CapabilitySHA256 == "" {
		return fmt.Errorf("%w: platform handle is incomplete", ErrPlatformIdentityMismatch)
	}
	if uint64(handle.Attempt) > maxStageExecutionJSONInteger ||
		uint64(handle.Generation) > maxStageExecutionJSONInteger ||
		handle.FencingToken > maxStageExecutionJSONInteger {
		return fmt.Errorf(
			"%w: platform handle coordinates are not JSON safe integers",
			ErrPlatformIdentityMismatch,
		)
	}
	return nil
}

const maxStageExecutionJSONInteger = uint64(1<<53 - 1)

func validateProviderHandle(value string) error {
	if value == "" || len(value) > 512 || value != strings.TrimSpace(value) ||
		!utf8.ValidString(value) {
		return fmt.Errorf(
			"%w: provider handle must be a non-empty bounded opaque identifier",
			ErrPlatformIdentityMismatch,
		)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return fmt.Errorf(
				"%w: provider handle must be a non-empty bounded opaque identifier",
				ErrPlatformIdentityMismatch,
			)
		}
	}
	return nil
}

func cloneStageExecutionRequest(
	request contractsv1alpha1.StageExecutionRequest,
) contractsv1alpha1.StageExecutionRequest {
	request.Upstream = cloneExecutionSlice(request.Upstream)
	request.Capability.Authority.AllowedTools = cloneExecutionSlice(
		request.Capability.Authority.AllowedTools,
	)
	return request
}

func cloneExecutionSlice[T any](values []T) []T {
	if values == nil {
		return nil
	}
	return append(make([]T, 0, len(values)), values...)
}
