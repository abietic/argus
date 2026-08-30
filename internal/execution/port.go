// Package execution defines Argus' anti-corruption boundary to a platform
// runtime. It deliberately contains no Hailix or ACP SDK types.
package execution

import (
	"context"
	"errors"
	"fmt"

	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

var ErrPlatformIntegrationDisabled = errors.New("platform execution integration is disabled")

type Handle struct {
	ProviderHandle   string
	ExecutionID      string
	Attempt          int
	Generation       int
	FencingToken     uint64
	IdempotencyKey   string
	RequestSHA256    string
	CapabilitySHA256 string
}

// TerminalCancelAuthority is the exact durable host decision that authorizes
// provider cancellation. It is not trusted by itself: Gateway always asks its
// subject-bound DurableClaimAuthorizer to reload and verify this coordinate
// before minting an opaque PreparedCancel.
type TerminalCancelAuthority struct {
	TenantID             string
	WorkspaceID          string
	ReviewRunID          string
	StageID              string
	StageRevision        string
	StageSHA256          string
	IntentID             string
	IntentSHA256         string
	TerminalGateID       string
	TerminalGateSHA256   string
	CancelIdempotencyKey string
}

type CancelRequest struct {
	Authority            TerminalCancelAuthority
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

type PlatformPort interface {
	// Ensure atomically binds IdempotencyKey to the exact immutable request and
	// returns the one resulting handle. Repeating Ensure with the same key and
	// exact request MUST return the same handle without starting another
	// execution. Reusing the key for different request content MUST fail closed.
	// A caller may therefore retry any error, including an unknown transport
	// outcome; providers that cannot implement this contract must not be wired
	// into formal dispatch.
	Ensure(
		context.Context,
		contractsv1alpha1.StageExecutionRequest,
	) (Handle, error)
	// Lookup resolves an earlier Ensure by the exact immutable request identity.
	Lookup(
		context.Context,
		contractsv1alpha1.StageExecutionRequest,
	) (Handle, bool, error)
	// Cancel atomically binds CancelIdempotencyKey to the exact target tuple.
	// Exact retries MUST have no additional effect; reusing the key for a
	// changed target MUST fail closed. Any transport error may be retried.
	Cancel(context.Context, CancelRequest) error
}

// AdmitResult validates the provider's exact echoed request identity. It does
// not replace the host-observed lease/deadline terminal gate required before a
// typed result can enter Argus' evidence ledger.
func AdmitResult(
	request contractsv1alpha1.StageExecutionRequest,
	result contractsv1alpha1.StageExecutionResult,
) error {
	return contractsv1alpha1.ValidateStageExecutionResultBinding(request, result)
}

// DisabledHailixPort makes the current external boundary explicit. Direct task
// smoke inputs are intentionally not accepted here: formal execution remains
// disabled until Hailix exposes immutable input binding, capability
// attestation, typed results, exact-generation cancellation, and trace/artifact
// contracts.
type DisabledHailixPort struct {
	Reason string
}

func (port DisabledHailixPort) Ensure(
	ctx context.Context,
	request contractsv1alpha1.StageExecutionRequest,
) (Handle, error) {
	if ctx == nil {
		return Handle{}, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return Handle{}, err
	}
	if err := request.Validate(); err != nil {
		return Handle{}, fmt.Errorf("validate platform execution request: %w", err)
	}
	return Handle{}, port.disabledError()
}

func (port DisabledHailixPort) Lookup(
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
		return Handle{}, false, fmt.Errorf("validate platform execution lookup: %w", err)
	}
	return Handle{}, false, port.disabledError()
}

func (port DisabledHailixPort) Cancel(
	ctx context.Context,
	request CancelRequest,
) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateTerminalCancelAuthority(request.Authority); err != nil {
		return err
	}
	if request.CancelIdempotencyKey != request.Authority.CancelIdempotencyKey {
		return fmt.Errorf("cancel idempotency key differs from terminal authority")
	}
	if request.ProviderHandle == "" || request.ExecutionID == "" || request.Attempt < 1 ||
		request.Generation < 1 || request.FencingToken == 0 ||
		uint64(request.Attempt) > maxStageExecutionJSONInteger ||
		uint64(request.Generation) > maxStageExecutionJSONInteger ||
		request.FencingToken > maxStageExecutionJSONInteger ||
		request.CreateIdempotencyKey == "" || request.CancelIdempotencyKey == "" ||
		request.CreateIdempotencyKey == request.CancelIdempotencyKey ||
		request.RequestSHA256 == "" || request.CapabilitySHA256 == "" {
		return fmt.Errorf("exact cancel identity is incomplete")
	}
	return port.disabledError()
}

func (port DisabledHailixPort) disabledError() error {
	if port.Reason == "" {
		return ErrPlatformIntegrationDisabled
	}
	return fmt.Errorf("%w: %s", ErrPlatformIntegrationDisabled, port.Reason)
}
