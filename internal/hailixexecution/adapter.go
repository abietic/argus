// Package hailixexecution implements Argus' anti-corruption adapter for the
// future public Hailix platform-execution API. It intentionally depends on a
// narrow client contract instead of importing Hailix internal packages.
package hailixexecution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/abietic/argus/internal/application"
	"github.com/abietic/argus/internal/execution"
	"github.com/abietic/argus/internal/runmodel"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

var ErrClientContractViolation = errors.New("Hailix platform-execution client contract violation")

const maxJSONSafeInteger = uint64(1<<53 - 1)

// Client is the minimum public Hailix service contract Argus needs. The
// current Hailix DirectTaskInput endpoint does not implement this contract.
// Implementations must provide atomic exact-idempotency for Ensure and Cancel,
// and must authenticate terminal result provenance before returning a callback
// verification receipt.
type Client interface {
	GetCapabilitySnapshot(
		context.Context,
		CapabilityCommand,
	) (CapabilityReceipt, error)
	EnsureExecution(context.Context, EnsureCommand) (EnsureReceipt, error)
	LookupExecution(context.Context, LookupCommand) (EnsureReceipt, bool, error)
	CancelExecution(context.Context, CancelCommand) error
	AwaitExecution(context.Context, AwaitCommand) (TerminalEnvelope, error)
	VerifyResultCallback(
		context.Context,
		VerifyCallbackCommand,
	) (CallbackVerificationReceipt, error)
}

type VerifierRef struct {
	ID       string `json:"id"`
	Revision string `json:"revision"`
	SHA256   string `json:"sha256"`
}

type TrustConfig struct {
	CapabilityVerifier VerifierRef
	CallbackVerifier   VerifierRef
}

type CapabilityCommand struct {
	Subject           Subject
	PlanID            string
	PlanSHA256        string
	CanonicalPlanJSON []byte
}

type CapabilityReceipt struct {
	Subject    Subject
	PlanID     string
	PlanSHA256 string
	Capability contractsv1alpha1.ExecutorCapabilitySnapshot
	Verifier   VerifierRef
}

type EnsureCommand struct {
	Subject              Subject
	CanonicalRequestJSON []byte
}

type LookupCommand struct {
	Subject              Subject
	CanonicalRequestJSON []byte
}

// EnsureReceipt is Hailix' exact acknowledgement of one immutable execution.
// ProviderHandle remains opaque; Hailix may encode Task/WorkerSession/
// WorkerRun/AgentTurn coordinates behind it without exposing internal types.
type EnsureReceipt struct {
	Subject          Subject
	ProviderHandle   string
	ExecutionID      string
	Attempt          int
	Generation       int
	FencingToken     uint64
	IdempotencyKey   string
	RequestSHA256    string
	CapabilitySHA256 string
}

type CancelCommand struct {
	Subject              Subject
	ReviewRunID          string
	StageID              string
	StageRevision        string
	StageSHA256          string
	IntentID             string
	IntentSHA256         string
	TerminalGateID       string
	TerminalGateSHA256   string
	ProviderHandle       string
	ExecutionID          string
	Attempt              int
	Generation           int
	FencingToken         uint64
	CreateIdempotencyKey string
	CancelIdempotencyKey string
	RequestSHA256        string
	CapabilitySHA256     string
}

type AwaitCommand struct {
	Subject          Subject
	ProviderHandle   string
	ExecutionID      string
	Attempt          int
	Generation       int
	FencingToken     uint64
	IdempotencyKey   string
	RequestSHA256    string
	CapabilitySHA256 string
}

type TerminalEnvelope struct {
	CanonicalResultJSON []byte
	CallbackProof       []byte
}

type Subject struct {
	TenantID       string
	OrganizationID string
	WorkspaceID    string
	RepositoryID   string
}

type CallbackBinding struct {
	BindingID        string `json:"binding_id"`
	BindingSHA256    string `json:"binding_sha256"`
	ProviderHandle   string `json:"provider_handle"`
	ExecutionID      string `json:"execution_id"`
	Attempt          int    `json:"attempt"`
	Generation       int    `json:"generation"`
	FencingToken     uint64 `json:"fencing_token"`
	IdempotencyKey   string `json:"idempotency_key"`
	RequestSHA256    string `json:"request_sha256"`
	CapabilitySHA256 string `json:"capability_sha256"`
}

type VerifyCallbackCommand struct {
	Subject             Subject
	Binding             CallbackBinding
	CanonicalResultJSON []byte
	ResultSHA256        string
	CallbackProof       []byte
	ProofSHA256         string
}

// CallbackVerificationReceipt is ordinary verification evidence, not a
// credential. Every echoed digest is checked before the verifier ref may enter
// Argus' durable callback receipt.
type CallbackVerificationReceipt struct {
	Subject          Subject
	BindingID        string
	BindingSHA256    string
	ProviderHandle   string
	ResultSHA256     string
	ProofSHA256      string
	VerifierID       string
	VerifierRevision string
	VerifierSHA256   string
}

// Adapter is deliberately not wired to the current Hailix DirectTaskInput API.
// It becomes production-usable only when a Hailix client implements Client
// against a public immutable platform-execution service.
type Adapter struct {
	client  Client
	subject Subject
	trust   TrustConfig
}

func New(
	client Client,
	subject application.AgentPlanningSubject,
	trust TrustConfig,
) (*Adapter, error) {
	if client == nil {
		return nil, fmt.Errorf("Hailix platform-execution client is required")
	}
	ledgerSubject := planningSubject(subject)
	if err := ledgerSubject.Validate(); err != nil {
		return nil, fmt.Errorf("validate Hailix adapter subject: %w", err)
	}
	if err := trust.CapabilityVerifier.validate(); err != nil {
		return nil, fmt.Errorf("validate pinned Hailix capability verifier: %w", err)
	}
	if err := trust.CallbackVerifier.validate(); err != nil {
		return nil, fmt.Errorf("validate pinned Hailix callback verifier: %w", err)
	}
	return &Adapter{
		client: client, subject: clientSubject(subject), trust: trust,
	}, nil
}

var (
	_ execution.PlatformPort                       = (*Adapter)(nil)
	_ application.AgentExecutorCapabilityResolver  = (*Adapter)(nil)
	_ application.FormalAgentStageExecutor         = (*Adapter)(nil)
	_ application.AgentStageResultCallbackVerifier = (*Adapter)(nil)
)

func (adapter *Adapter) ResolveAgentExecutorCapability(
	ctx context.Context,
	subject application.AgentPlanningSubject,
	plan contractsv1alpha1.AgentStagePlan,
) (contractsv1alpha1.ExecutorCapabilitySnapshot, error) {
	if err := adapter.ready(ctx); err != nil {
		return contractsv1alpha1.ExecutorCapabilitySnapshot{}, err
	}
	if clientSubject(subject) != adapter.subject {
		return contractsv1alpha1.ExecutorCapabilitySnapshot{}, fmt.Errorf(
			"Hailix capability request escaped bound subject",
		)
	}
	if err := plan.Validate(); err != nil {
		return contractsv1alpha1.ExecutorCapabilitySnapshot{}, fmt.Errorf(
			"validate Hailix capability plan: %w",
			err,
		)
	}
	canonical, err := json.Marshal(plan)
	if err != nil {
		return contractsv1alpha1.ExecutorCapabilitySnapshot{}, fmt.Errorf(
			"marshal Hailix capability plan: %w",
			err,
		)
	}
	command := CapabilityCommand{
		Subject: adapter.subject, PlanID: plan.PlanID, PlanSHA256: plan.SHA256,
		CanonicalPlanJSON: canonical,
	}
	receipt, err := adapter.client.GetCapabilitySnapshot(ctx, command)
	if err != nil {
		return contractsv1alpha1.ExecutorCapabilitySnapshot{}, err
	}
	if receipt.Subject != command.Subject || receipt.PlanID != command.PlanID ||
		receipt.PlanSHA256 != command.PlanSHA256 {
		return contractsv1alpha1.ExecutorCapabilitySnapshot{}, contractViolation(
			"capability receipt differs from exact subject or plan",
		)
	}
	if receipt.Verifier != adapter.trust.CapabilityVerifier {
		return contractsv1alpha1.ExecutorCapabilitySnapshot{}, contractViolation(
			"capability verifier ref differs from pinned Hailix trust root",
		)
	}
	capability := receipt.Capability
	capability.Authority.AllowedTools = append(
		[]string(nil),
		receipt.Capability.Authority.AllowedTools...,
	)
	if err := capability.Validate(); err != nil {
		return contractsv1alpha1.ExecutorCapabilitySnapshot{}, contractViolation(
			"invalid executor capability: %v",
			err,
		)
	}
	if capability.Trust.Authority != contractsv1alpha1.ExecutorTrustAuthorityPlatform ||
		capability.Trust.CapabilityVerifier != contractVerifierRef(receipt.Verifier) ||
		capability.Trust.CallbackVerifier != contractVerifierRef(adapter.trust.CallbackVerifier) {
		return contractsv1alpha1.ExecutorCapabilitySnapshot{}, contractViolation(
			"capability trust snapshot differs from pinned Hailix verifier roots",
		)
	}
	return capability, nil
}

func (adapter *Adapter) Ensure(
	ctx context.Context,
	request contractsv1alpha1.StageExecutionRequest,
) (execution.Handle, error) {
	if err := adapter.ready(ctx); err != nil {
		return execution.Handle{}, err
	}
	canonical, err := canonicalRequest(request, adapter.subject)
	if err != nil {
		return execution.Handle{}, err
	}
	receipt, err := adapter.client.EnsureExecution(ctx, EnsureCommand{
		Subject:              adapter.subject,
		CanonicalRequestJSON: canonical,
	})
	if err != nil {
		return execution.Handle{}, err
	}
	return handleFromReceipt(request, receipt, adapter.subject)
}

func (adapter *Adapter) Lookup(
	ctx context.Context,
	request contractsv1alpha1.StageExecutionRequest,
) (execution.Handle, bool, error) {
	if err := adapter.ready(ctx); err != nil {
		return execution.Handle{}, false, err
	}
	canonical, err := canonicalRequest(request, adapter.subject)
	if err != nil {
		return execution.Handle{}, false, err
	}
	receipt, found, err := adapter.client.LookupExecution(ctx, LookupCommand{
		Subject:              adapter.subject,
		CanonicalRequestJSON: canonical,
	})
	if err != nil || !found {
		return execution.Handle{}, found, err
	}
	handle, err := handleFromReceipt(request, receipt, adapter.subject)
	if err != nil {
		return execution.Handle{}, false, err
	}
	return handle, true, nil
}

func (adapter *Adapter) Cancel(ctx context.Context, request execution.CancelRequest) error {
	if err := adapter.ready(ctx); err != nil {
		return err
	}
	if err := validateCancel(request, adapter.subject); err != nil {
		return err
	}
	authority := request.Authority
	return adapter.client.CancelExecution(ctx, CancelCommand{
		Subject:     adapter.subject,
		ReviewRunID: authority.ReviewRunID, StageID: authority.StageID,
		StageRevision: authority.StageRevision, StageSHA256: authority.StageSHA256,
		IntentID: authority.IntentID, IntentSHA256: authority.IntentSHA256,
		TerminalGateID:     authority.TerminalGateID,
		TerminalGateSHA256: authority.TerminalGateSHA256,
		ProviderHandle:     request.ProviderHandle, ExecutionID: request.ExecutionID,
		Attempt: request.Attempt, Generation: request.Generation,
		FencingToken:         request.FencingToken,
		CreateIdempotencyKey: request.CreateIdempotencyKey,
		CancelIdempotencyKey: request.CancelIdempotencyKey,
		RequestSHA256:        request.RequestSHA256,
		CapabilitySHA256:     request.CapabilitySHA256,
	})
}

func (adapter *Adapter) AwaitResult(
	ctx context.Context,
	handle execution.Handle,
) (contractsv1alpha1.StageExecutionResult, []byte, error) {
	if err := adapter.ready(ctx); err != nil {
		return contractsv1alpha1.StageExecutionResult{}, nil, err
	}
	if err := validateHandle(handle); err != nil {
		return contractsv1alpha1.StageExecutionResult{}, nil, err
	}
	envelope, err := adapter.client.AwaitExecution(ctx, awaitCommand(adapter.subject, handle))
	if err != nil {
		return contractsv1alpha1.StageExecutionResult{}, nil, err
	}
	if len(envelope.CallbackProof) == 0 {
		return contractsv1alpha1.StageExecutionResult{}, nil, contractViolation(
			"terminal envelope omitted callback proof",
		)
	}
	result, err := contractsv1alpha1.DecodeStageExecutionResult(
		envelope.CanonicalResultJSON,
	)
	if err != nil {
		return contractsv1alpha1.StageExecutionResult{}, nil, contractViolation(
			"decode terminal result: %v",
			err,
		)
	}
	canonical, err := json.Marshal(result)
	if err != nil || !bytes.Equal(canonical, envelope.CanonicalResultJSON) {
		return contractsv1alpha1.StageExecutionResult{}, nil, contractViolation(
			"terminal result is not canonical JSON",
		)
	}
	if !resultMatchesHandle(result, handle) {
		return contractsv1alpha1.StageExecutionResult{}, nil, contractViolation(
			"terminal result differs from exact execution handle",
		)
	}
	return result, bytes.Clone(envelope.CallbackProof), nil
}

func (adapter *Adapter) VerifyAgentStageResultCallback(
	ctx context.Context,
	subject application.AgentPlanningSubject,
	binding runmodel.AgentStageExecutionBinding,
	trust contractsv1alpha1.ExecutorTrust,
	canonicalResult []byte,
	proof []byte,
) (runmodel.AgentStageResultCallbackVerifierRef, error) {
	if err := adapter.ready(ctx); err != nil {
		return runmodel.AgentStageResultCallbackVerifierRef{}, err
	}
	if clientSubject(subject) != adapter.subject || clientSubjectFromLedger(binding.Subject) != adapter.subject {
		return runmodel.AgentStageResultCallbackVerifierRef{}, fmt.Errorf(
			"Hailix callback escaped bound subject",
		)
	}
	if err := binding.Validate(); err != nil {
		return runmodel.AgentStageResultCallbackVerifierRef{}, fmt.Errorf(
			"validate Hailix callback binding: %w",
			err,
		)
	}
	if err := trust.Validate(); err != nil {
		return runmodel.AgentStageResultCallbackVerifierRef{}, fmt.Errorf(
			"validate frozen Hailix executor trust: %w",
			err,
		)
	}
	if trust.Authority != contractsv1alpha1.ExecutorTrustAuthorityPlatform ||
		trust.CapabilityVerifier != contractVerifierRef(adapter.trust.CapabilityVerifier) ||
		trust.CallbackVerifier != contractVerifierRef(adapter.trust.CallbackVerifier) {
		return runmodel.AgentStageResultCallbackVerifierRef{}, contractViolation(
			"frozen request trust differs from pinned Hailix trust root",
		)
	}
	result, err := contractsv1alpha1.DecodeStageExecutionResult(canonicalResult)
	if err != nil {
		return runmodel.AgentStageResultCallbackVerifierRef{}, err
	}
	wantCanonical, err := json.Marshal(result)
	if err != nil || !bytes.Equal(wantCanonical, canonicalResult) {
		return runmodel.AgentStageResultCallbackVerifierRef{}, fmt.Errorf(
			"Hailix callback result must be canonical JSON",
		)
	}
	if len(proof) == 0 {
		return runmodel.AgentStageResultCallbackVerifierRef{}, fmt.Errorf(
			"Hailix callback proof is required",
		)
	}
	if !resultMatchesBinding(result, binding) {
		return runmodel.AgentStageResultCallbackVerifierRef{}, contractViolation(
			"callback result differs from durable execution binding",
		)
	}
	command := VerifyCallbackCommand{
		Subject: adapter.subject,
		Binding: CallbackBinding{
			BindingID: binding.BindingID, BindingSHA256: binding.SHA256,
			ProviderHandle: binding.ProviderHandle, ExecutionID: binding.ExecutionID,
			Attempt: binding.Attempt, Generation: binding.Generation,
			FencingToken:     binding.FencingToken,
			IdempotencyKey:   binding.CreateIdempotencyKey,
			RequestSHA256:    binding.RequestSemanticSHA256,
			CapabilitySHA256: binding.CapabilitySHA256,
		},
		CanonicalResultJSON: bytes.Clone(canonicalResult),
		ResultSHA256:        shaHex(canonicalResult),
		CallbackProof:       bytes.Clone(proof), ProofSHA256: shaHex(proof),
	}
	receipt, err := adapter.client.VerifyResultCallback(ctx, command)
	if err != nil {
		return runmodel.AgentStageResultCallbackVerifierRef{}, err
	}
	if receipt.Subject != command.Subject || receipt.BindingID != command.Binding.BindingID ||
		receipt.BindingSHA256 != command.Binding.BindingSHA256 ||
		receipt.ProviderHandle != command.Binding.ProviderHandle ||
		receipt.ResultSHA256 != command.ResultSHA256 ||
		receipt.ProofSHA256 != command.ProofSHA256 {
		return runmodel.AgentStageResultCallbackVerifierRef{}, contractViolation(
			"callback verification receipt differs from exact request",
		)
	}
	verifier := runmodel.AgentStageResultCallbackVerifierRef{
		ID: receipt.VerifierID, Revision: receipt.VerifierRevision,
		SHA256: receipt.VerifierSHA256,
	}
	if err := verifier.Validate(); err != nil {
		return runmodel.AgentStageResultCallbackVerifierRef{}, contractViolation(
			"invalid callback verifier ref: %v",
			err,
		)
	}
	if verifierRef(verifier) != adapter.trust.CallbackVerifier {
		return runmodel.AgentStageResultCallbackVerifierRef{}, contractViolation(
			"callback verifier ref differs from pinned Hailix trust root",
		)
	}
	return verifier, nil
}

func (ref VerifierRef) validate() error {
	if ref.ID == "" || ref.Revision == "" || len(ref.ID) > 256 || len(ref.Revision) > 256 ||
		strings.TrimSpace(ref.ID) != ref.ID || strings.TrimSpace(ref.Revision) != ref.Revision ||
		!utf8.ValidString(ref.ID) || !utf8.ValidString(ref.Revision) ||
		hasControl(ref.ID) || hasControl(ref.Revision) || !lowerSHA256(ref.SHA256) {
		return fmt.Errorf("verifier ref is incomplete")
	}
	return nil
}

func verifierRef(ref runmodel.AgentStageResultCallbackVerifierRef) VerifierRef {
	return VerifierRef{ID: ref.ID, Revision: ref.Revision, SHA256: ref.SHA256}
}

func contractVerifierRef(ref VerifierRef) contractsv1alpha1.VersionedRef {
	return contractsv1alpha1.VersionedRef{
		ID: ref.ID, Revision: ref.Revision, SHA256: ref.SHA256,
	}
}

func (adapter *Adapter) ready(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if adapter == nil || adapter.client == nil {
		return fmt.Errorf("Hailix execution adapter is not initialized")
	}
	return nil
}

func canonicalRequest(
	request contractsv1alpha1.StageExecutionRequest,
	subject Subject,
) ([]byte, error) {
	if err := request.Validate(); err != nil {
		return nil, fmt.Errorf("validate Hailix stage execution request: %w", err)
	}
	if request.TenantID != subject.TenantID || request.WorkspaceID != subject.WorkspaceID {
		return nil, fmt.Errorf("Hailix stage execution request escaped bound subject")
	}
	data, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("marshal Hailix stage execution request: %w", err)
	}
	return data, nil
}

func handleFromReceipt(
	request contractsv1alpha1.StageExecutionRequest,
	receipt EnsureReceipt,
	subject Subject,
) (execution.Handle, error) {
	handle := execution.Handle{
		ProviderHandle: receipt.ProviderHandle, ExecutionID: receipt.ExecutionID,
		Attempt: receipt.Attempt, Generation: receipt.Generation,
		FencingToken: receipt.FencingToken, IdempotencyKey: receipt.IdempotencyKey,
		RequestSHA256:    receipt.RequestSHA256,
		CapabilitySHA256: receipt.CapabilitySHA256,
	}
	if err := validateHandle(handle); err != nil {
		return execution.Handle{}, contractViolation("invalid ensure receipt: %v", err)
	}
	if receipt.Subject != subject || handle.ExecutionID != request.ExecutionID ||
		handle.Attempt != request.Attempt ||
		handle.Generation != request.Generation ||
		handle.FencingToken != request.FencingToken ||
		handle.IdempotencyKey != request.IdempotencyKey ||
		handle.RequestSHA256 != request.RequestSHA256 ||
		handle.CapabilitySHA256 != request.Capability.SHA256 {
		return execution.Handle{}, contractViolation(
			"ensure receipt differs from exact request",
		)
	}
	return handle, nil
}

func validateHandle(handle execution.Handle) error {
	if handle.ProviderHandle == "" || len(handle.ProviderHandle) > 512 ||
		strings.TrimSpace(handle.ProviderHandle) != handle.ProviderHandle ||
		!utf8.ValidString(handle.ProviderHandle) || hasControl(handle.ProviderHandle) ||
		handle.ExecutionID == "" || handle.Attempt < 1 || handle.Generation < 1 ||
		handle.FencingToken == 0 || handle.IdempotencyKey == "" ||
		!lowerSHA256(handle.RequestSHA256) || !lowerSHA256(handle.CapabilitySHA256) {
		return fmt.Errorf("execution handle is incomplete")
	}
	if uint64(handle.Attempt) > maxJSONSafeInteger ||
		uint64(handle.Generation) > maxJSONSafeInteger ||
		handle.FencingToken > maxJSONSafeInteger {
		return fmt.Errorf("execution handle coordinates are not JSON safe integers")
	}
	return nil
}

func hasControl(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}

func validateCancel(request execution.CancelRequest, subject Subject) error {
	authority := request.Authority
	if authority.TenantID != subject.TenantID || authority.WorkspaceID != subject.WorkspaceID {
		return fmt.Errorf("Hailix cancel escaped bound subject")
	}
	if authority.ReviewRunID == "" || authority.StageID == "" ||
		authority.StageRevision == "" || !lowerSHA256(authority.StageSHA256) ||
		authority.IntentID == "" || !lowerSHA256(authority.IntentSHA256) ||
		authority.TerminalGateID == "" || !lowerSHA256(authority.TerminalGateSHA256) ||
		authority.CancelIdempotencyKey == "" ||
		request.CancelIdempotencyKey != authority.CancelIdempotencyKey ||
		request.CreateIdempotencyKey == request.CancelIdempotencyKey {
		return fmt.Errorf("Hailix cancel authority is incomplete")
	}
	return validateHandle(execution.Handle{
		ProviderHandle: request.ProviderHandle, ExecutionID: request.ExecutionID,
		Attempt: request.Attempt, Generation: request.Generation,
		FencingToken: request.FencingToken, IdempotencyKey: request.CreateIdempotencyKey,
		RequestSHA256:    request.RequestSHA256,
		CapabilitySHA256: request.CapabilitySHA256,
	})
}

func awaitCommand(subject Subject, handle execution.Handle) AwaitCommand {
	return AwaitCommand{
		Subject:        subject,
		ProviderHandle: handle.ProviderHandle, ExecutionID: handle.ExecutionID,
		Attempt: handle.Attempt, Generation: handle.Generation,
		FencingToken: handle.FencingToken, IdempotencyKey: handle.IdempotencyKey,
		RequestSHA256:    handle.RequestSHA256,
		CapabilitySHA256: handle.CapabilitySHA256,
	}
}

func resultMatchesHandle(
	result contractsv1alpha1.StageExecutionResult,
	handle execution.Handle,
) bool {
	return result.ExecutionID == handle.ExecutionID && result.Attempt == handle.Attempt &&
		result.Generation == handle.Generation && result.FencingToken == handle.FencingToken &&
		result.IdempotencyKey == handle.IdempotencyKey &&
		result.RequestSHA256 == handle.RequestSHA256 &&
		result.CapabilitySHA256 == handle.CapabilitySHA256
}

func resultMatchesBinding(
	result contractsv1alpha1.StageExecutionResult,
	binding runmodel.AgentStageExecutionBinding,
) bool {
	return result.ExecutionID == binding.ExecutionID && result.Attempt == binding.Attempt &&
		result.Generation == binding.Generation && result.FencingToken == binding.FencingToken &&
		result.IdempotencyKey == binding.CreateIdempotencyKey &&
		result.RequestSHA256 == binding.RequestSemanticSHA256 &&
		result.CapabilitySHA256 == binding.CapabilitySHA256
}

func clientSubject(subject application.AgentPlanningSubject) Subject {
	return Subject{
		TenantID: subject.TenantID, OrganizationID: subject.OrganizationID,
		WorkspaceID: subject.WorkspaceID, RepositoryID: subject.RepositoryID,
	}
}

func clientSubjectFromLedger(subject runmodel.AgentPlanningSubject) Subject {
	return Subject{
		TenantID: subject.TenantID, OrganizationID: subject.OrganizationID,
		WorkspaceID: subject.WorkspaceID, RepositoryID: subject.RepositoryID,
	}
}

func planningSubject(subject application.AgentPlanningSubject) runmodel.AgentPlanningSubject {
	return runmodel.AgentPlanningSubject{
		TenantID: subject.TenantID, OrganizationID: subject.OrganizationID,
		WorkspaceID: subject.WorkspaceID, RepositoryID: subject.RepositoryID,
	}
}

func shaHex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func lowerSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

func contractViolation(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrClientContractViolation, fmt.Sprintf(format, args...))
}
