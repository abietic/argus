package application

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/abietic/argus/internal/execution"
	"github.com/abietic/argus/internal/runmodel"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

func TestAgentStageDispatcherClaimsBeforeSingleCreateAndReusesBinding(t *testing.T) {
	dispatcher, fixture, repository, _, capability, gateway := newAgentDispatchFixture(t)

	first, err := dispatcher.DispatchGovernedAgentStage(
		context.Background(),
		fixture.subject,
		DispatchAgentStageCommand{
			ReviewRunID: fixture.command.ReviewRunID,
			StageID:     fixture.command.StageID,
		},
	)
	if err != nil {
		t.Fatalf("DispatchGovernedAgentStage() error = %v", err)
	}
	if gateway.dispatchCalls != 1 || gateway.lookupCalls != 0 || !gateway.sawClaimBeforeCreate {
		t.Fatalf(
			"gateway dispatch=%d lookup=%d writeAhead=%v",
			gateway.dispatchCalls,
			gateway.lookupCalls,
			gateway.sawClaimBeforeCreate,
		)
	}
	if capability.calls != 1 || repository.claimCalls != 1 ||
		repository.intent == nil || repository.binding == nil {
		t.Fatalf("capability/intent/binding were not durably closed")
	}
	if first.Request.Plan != bindingFromAdmission(first.Admission.Plan.Governed) ||
		first.Request.RequestSHA256 != first.Intent.RequestSemanticSHA256 ||
		first.Intent.RequestRef.SHA256 == first.Request.RequestSHA256 ||
		first.Intent.CreateIdempotencyKey == first.Intent.CancelIdempotencyKey ||
		first.Binding.RequestSemanticSHA256 != first.Request.RequestSHA256 {
		t.Fatalf("dispatch digest/plan/key classes were conflated: %+v", first)
	}

	second, err := dispatcher.DispatchGovernedAgentStage(
		context.Background(),
		fixture.subject,
		DispatchAgentStageCommand{
			ReviewRunID: fixture.command.ReviewRunID,
			StageID:     fixture.command.StageID,
		},
	)
	if err != nil {
		t.Fatalf("DispatchGovernedAgentStage(retry) error = %v", err)
	}
	if !second.Recovered || second.Binding != first.Binding ||
		gateway.dispatchCalls != 1 || gateway.lookupCalls != 0 ||
		capability.calls != 1 || repository.claimCalls != 1 {
		t.Fatalf(
			"retry was not binding-only recovery: recovered=%v create=%d lookup=%d capability=%d claim=%d",
			second.Recovered,
			gateway.dispatchCalls,
			gateway.lookupCalls,
			capability.calls,
			repository.claimCalls,
		)
	}
}

func TestAgentStageDispatcherUnknownAfterAcceptedRecoversThroughExactEnsure(t *testing.T) {
	dispatcher, fixture, _, _, capability, gateway := newAgentDispatchFixture(t)
	gateway.dispatchErr = errors.New("transport closed after write")

	_, err := dispatcher.DispatchGovernedAgentStage(
		context.Background(),
		fixture.subject,
		DispatchAgentStageCommand{
			ReviewRunID: fixture.command.ReviewRunID,
			StageID:     fixture.command.StageID,
		},
	)
	if !errors.Is(err, ErrAgentStageDispatchOutcomeUnknown) {
		t.Fatalf("DispatchGovernedAgentStage() error = %v", err)
	}
	if gateway.dispatchCalls != 1 || gateway.lookupCalls != 0 || capability.calls != 1 {
		t.Fatal("first unknown Create did not stop at its durable intent")
	}

	gateway.dispatchErr = nil
	recovered, err := dispatcher.DispatchGovernedAgentStage(
		context.Background(),
		fixture.subject,
		DispatchAgentStageCommand{
			ReviewRunID: fixture.command.ReviewRunID,
			StageID:     fixture.command.StageID,
		},
	)
	if err != nil {
		t.Fatalf("DispatchGovernedAgentStage(recovery) error = %v", err)
	}
	if !recovered.Recovered || gateway.dispatchCalls != 2 || gateway.providerStarts != 1 ||
		gateway.lookupCalls != 0 || capability.calls != 1 {
		t.Fatalf(
			"unknown outcome was not exact-idempotent: recovered=%v ensure=%d starts=%d lookup=%d capability=%d",
			recovered.Recovered,
			gateway.dispatchCalls,
			gateway.providerStarts,
			gateway.lookupCalls,
			capability.calls,
		)
	}
}

func TestAgentStageDispatcherRejectsConcurrentProviderHandleDrift(t *testing.T) {
	dispatcher, fixture, repository, _, _, gateway := newAgentDispatchFixture(t)
	gateway.afterEnsure = func(_ contractsv1alpha1.StageExecutionRequest) {
		winnerHandle := gateway.handle
		winnerHandle.ProviderHandle = "pi-provider-concurrent-winner"
		winner, err := sealHistoricalAgentStageBinding(
			*repository.intent,
			winnerHandle,
			repository.intent.RecordedAt.Add(time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
		repository.binding = &winner
	}

	_, err := dispatcher.DispatchGovernedAgentStage(
		context.Background(),
		fixture.subject,
		DispatchAgentStageCommand{
			ReviewRunID: fixture.command.ReviewRunID,
			StageID:     fixture.command.StageID,
		},
	)
	if !errors.Is(err, ErrAgentStageProviderContractViolation) {
		t.Fatalf("provider handle drift error = %v", err)
	}
	if gateway.providerStarts != 1 || repository.binding == nil ||
		repository.binding.ProviderHandle != "pi-provider-concurrent-winner" {
		t.Fatalf(
			"provider drift did not preserve durable winner: starts=%d binding=%+v",
			gateway.providerStarts,
			repository.binding,
		)
	}
}

func TestAgentStageDispatcherTerminalCancellationStopsClaimedRecovery(t *testing.T) {
	dispatcher, fixture, repository, _, _, gateway := newAgentDispatchFixture(t)
	gateway.dispatchErr = errors.New("unknown create")
	_, err := dispatcher.DispatchGovernedAgentStage(
		context.Background(),
		fixture.subject,
		DispatchAgentStageCommand{
			ReviewRunID: fixture.command.ReviewRunID,
			StageID:     fixture.command.StageID,
		},
	)
	if !errors.Is(err, ErrAgentStageDispatchOutcomeUnknown) || repository.intent == nil {
		t.Fatalf("failed to establish claimed unknown dispatch: %v", err)
	}
	requestBytes, err := repository.ReadLocalArtifact(
		context.Background(),
		repository.intent.RequestRef,
	)
	if err != nil {
		t.Fatal(err)
	}
	request, err := contractsv1alpha1.DecodeStageExecutionRequest(requestBytes)
	if err != nil {
		t.Fatal(err)
	}
	cancelGate, err := runmodel.SealAgentStageTerminalGate(runmodel.AgentStageTerminalGate{
		SchemaVersion:         runmodel.AgentStageTerminalGateSchemaVersion,
		Kind:                  runmodel.AgentStageCancellationRequested,
		Subject:               repository.intent.Subject,
		ReviewRunID:           repository.intent.ReviewRunID,
		Stage:                 repository.intent.Stage,
		AdmissionID:           repository.intent.AdmissionID,
		AdmissionSHA256:       repository.intent.AdmissionSHA256,
		IntentID:              repository.intent.IntentID,
		IntentSHA256:          repository.intent.SHA256,
		WorkloadID:            repository.intent.WorkloadID,
		LeaseID:               repository.intent.LeaseID,
		LeaseWorker:           repository.intent.LeaseWorker,
		ExecutionID:           repository.intent.ExecutionID,
		Attempt:               repository.intent.Attempt,
		Generation:            repository.intent.Generation,
		FencingToken:          repository.intent.FencingToken,
		RequestRef:            repository.intent.RequestRef,
		RequestSemanticSHA256: repository.intent.RequestSemanticSHA256,
		RequestDeadline:       request.Deadline,
		CapabilitySHA256:      repository.intent.CapabilitySHA256,
		Cancellation: &runmodel.AgentStageCancellationRequest{
			CancelIdempotencyKey: repository.intent.CancelIdempotencyKey,
			Actor:                "test-operator",
			Reason:               "formal cancellation won before recovery",
			RequestedAt:          repository.intent.RecordedAt.Add(time.Second),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	repository.terminal = &cancelGate
	gateway.dispatchErr = nil
	gateway.lookupFound = true
	_, err = dispatcher.DispatchGovernedAgentStage(
		context.Background(),
		fixture.subject,
		DispatchAgentStageCommand{
			ReviewRunID: fixture.command.ReviewRunID,
			StageID:     fixture.command.StageID,
		},
	)
	if !errors.Is(err, ErrAgentStageDispatchCanceled) {
		t.Fatalf("DispatchGovernedAgentStage(recovery) error = %v", err)
	}
	if gateway.dispatchCalls != 1 || gateway.lookupCalls != 0 {
		t.Fatalf(
			"terminal cancellation reached provider recovery: create=%d lookup=%d",
			gateway.dispatchCalls,
			gateway.lookupCalls,
		)
	}
}

func TestAgentStageDispatcherTransientTerminalLookupNeverReachesEnsure(t *testing.T) {
	dispatcher, fixture, repository, _, _, gateway := newAgentDispatchFixture(t)
	gateway.dispatchErr = errors.New("transport closed after provider acceptance")
	_, err := dispatcher.DispatchGovernedAgentStage(
		context.Background(),
		fixture.subject,
		DispatchAgentStageCommand{
			ReviewRunID: fixture.command.ReviewRunID,
			StageID:     fixture.command.StageID,
		},
	)
	if !errors.Is(err, ErrAgentStageDispatchOutcomeUnknown) || gateway.providerStarts != 1 {
		t.Fatalf("failed to establish accepted unknown Ensure: %v", err)
	}
	gateway.dispatchErr = nil
	repository.terminalLookupErr = errors.New("terminal ledger temporarily unavailable")
	_, err = dispatcher.DispatchGovernedAgentStage(
		context.Background(),
		fixture.subject,
		DispatchAgentStageCommand{
			ReviewRunID: fixture.command.ReviewRunID,
			StageID:     fixture.command.StageID,
		},
	)
	if err == nil || !strings.Contains(err.Error(), "terminal ledger temporarily unavailable") {
		t.Fatalf("transient terminal lookup error = %v", err)
	}
	if gateway.dispatchCalls != 1 || gateway.providerStarts != 1 {
		t.Fatal("transient terminal lookup reached provider Ensure")
	}
	repository.terminalLookupErr = nil
	if _, err := dispatcher.DispatchGovernedAgentStage(
		context.Background(),
		fixture.subject,
		DispatchAgentStageCommand{
			ReviewRunID: fixture.command.ReviewRunID,
			StageID:     fixture.command.StageID,
		},
	); err != nil {
		t.Fatalf("recovery after terminal ledger restored error = %v", err)
	}
	if gateway.dispatchCalls != 2 || gateway.providerStarts != 1 {
		t.Fatalf("ensure calls=%d starts=%d", gateway.dispatchCalls, gateway.providerStarts)
	}
}

func TestAgentStageDispatcherExpiredClaimConvergesToDurableCancellation(t *testing.T) {
	dispatcher, fixture, repository, _, _, gateway := newAgentDispatchFixture(t)
	gateway.dispatchErr = errors.New("known failure before provider start")
	gateway.rejectBeforeAccept = true
	_, err := dispatcher.DispatchGovernedAgentStage(
		context.Background(),
		fixture.subject,
		DispatchAgentStageCommand{
			ReviewRunID: fixture.command.ReviewRunID,
			StageID:     fixture.command.StageID,
		},
	)
	if !errors.Is(err, ErrAgentStageDispatchOutcomeUnknown) || repository.intent == nil {
		t.Fatalf("failed to establish claimed known-before-start request: %v", err)
	}
	requestBytes, err := repository.ReadLocalArtifact(
		context.Background(), repository.intent.RequestRef,
	)
	if err != nil {
		t.Fatal(err)
	}
	request, err := contractsv1alpha1.DecodeStageExecutionRequest(requestBytes)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher.now = func() time.Time { return request.Deadline.Add(time.Second) }
	gateway.dispatchErr = nil
	_, err = dispatcher.DispatchGovernedAgentStage(
		context.Background(),
		fixture.subject,
		DispatchAgentStageCommand{
			ReviewRunID: fixture.command.ReviewRunID,
			StageID:     fixture.command.StageID,
		},
	)
	if !errors.Is(err, ErrAgentStageDispatchCanceled) {
		t.Fatalf("expired claimed recovery error = %v", err)
	}
	if repository.terminal == nil ||
		repository.terminal.Kind != runmodel.AgentStageCancellationRequested ||
		repository.terminal.Cancellation == nil ||
		repository.terminal.Cancellation.CancelIdempotencyKey !=
			repository.intent.CancelIdempotencyKey {
		t.Fatalf("expired claim did not persist exact cancellation: %+v", repository.terminal)
	}
	if gateway.dispatchCalls != 1 || gateway.providerStarts != 0 {
		t.Fatalf("expired claim reached Ensure: calls=%d starts=%d", gateway.dispatchCalls, gateway.providerStarts)
	}
}

func TestAgentStageDispatcherRejectsBindingObservedAfterImmutableDeadline(t *testing.T) {
	dispatcher, fixture, repository, _, _, gateway := newAgentDispatchFixture(t)
	// Provider calls use the immutable deadline with the real context clock.
	// Keep that deadline in the future while controlling host observations with
	// the injected clock; otherwise this test becomes date-dependent.
	clock := time.Now().UTC().Add(2 * time.Minute)
	dispatcher.now = func() time.Time { return clock }
	gateway.afterEnsure = func(request contractsv1alpha1.StageExecutionRequest) {
		clock = request.Deadline
	}
	_, err := dispatcher.DispatchGovernedAgentStage(
		context.Background(),
		fixture.subject,
		DispatchAgentStageCommand{
			ReviewRunID: fixture.command.ReviewRunID,
			StageID:     fixture.command.StageID,
		},
	)
	if !errors.Is(err, ErrAgentStageDispatchOutcomeUnknown) ||
		!strings.Contains(err.Error(), "crossed immutable request deadline") {
		t.Fatalf("late binding observation error = %v", err)
	}
	if gateway.providerStarts != 1 || repository.binding != nil {
		t.Fatalf("late provider response starts=%d binding=%+v", gateway.providerStarts, repository.binding)
	}
	clock = clock.Add(time.Second)
	gateway.afterEnsure = nil
	_, err = dispatcher.DispatchGovernedAgentStage(
		context.Background(),
		fixture.subject,
		DispatchAgentStageCommand{
			ReviewRunID: fixture.command.ReviewRunID,
			StageID:     fixture.command.StageID,
		},
	)
	if !errors.Is(err, ErrAgentStageDispatchCanceled) || repository.terminal == nil {
		t.Fatalf("late response retry did not converge to cancellation: %v", err)
	}
	if gateway.providerStarts != 1 {
		t.Fatalf("late response retry started provider %d times", gateway.providerStarts)
	}
}

func TestAgentStageDispatcherKnownFailureBeforeCreateRetriesExactEnsure(t *testing.T) {
	dispatcher, fixture, _, _, _, gateway := newAgentDispatchFixture(t)
	gateway.rejectBeforeAccept = true
	gateway.dispatchErr = errors.New("unknown create")
	_, _ = dispatcher.DispatchGovernedAgentStage(
		context.Background(),
		fixture.subject,
		DispatchAgentStageCommand{
			ReviewRunID: fixture.command.ReviewRunID,
			StageID:     fixture.command.StageID,
		},
	)
	gateway.dispatchErr = nil

	recovered, err := dispatcher.DispatchGovernedAgentStage(
		context.Background(),
		fixture.subject,
		DispatchAgentStageCommand{
			ReviewRunID: fixture.command.ReviewRunID,
			StageID:     fixture.command.StageID,
		},
	)
	if err != nil || !recovered.Recovered || gateway.dispatchCalls != 2 ||
		gateway.providerStarts != 1 || gateway.lookupCalls != 0 {
		t.Fatalf(
			"known-before-create recovery = %v, ensure=%d starts=%d lookup=%d",
			err,
			gateway.dispatchCalls,
			gateway.providerStarts,
			gateway.lookupCalls,
		)
	}
}

func TestAgentStageDispatcherRejectsCapabilityWideningBeforeClaim(t *testing.T) {
	dispatcher, fixture, repository, _, capability, gateway := newAgentDispatchFixture(t)
	capability.mutate = func(value *contractsv1alpha1.ExecutorCapabilitySnapshot) {
		value.Authority.AllowedTools = append(value.Authority.AllowedTools, "remote_shell")
		value.Authority.AllowedTools = []string{"codegraph", "remote_shell"}
		digest, err := contractsv1alpha1.DigestExecutorCapability(*value)
		if err != nil {
			t.Fatal(err)
		}
		value.SHA256 = digest
	}

	_, err := dispatcher.DispatchGovernedAgentStage(
		context.Background(),
		fixture.subject,
		DispatchAgentStageCommand{
			ReviewRunID: fixture.command.ReviewRunID,
			StageID:     fixture.command.StageID,
		},
	)
	if err == nil || !strings.Contains(err.Error(), "does not exactly implement") {
		t.Fatalf("DispatchGovernedAgentStage() error = %v", err)
	}
	if repository.intent != nil || repository.claimCalls != 0 || gateway.dispatchCalls != 0 {
		t.Fatal("widened capability reached a durable claim or provider Create")
	}
}

func TestAgentStageDispatcherRecoversPersistedUnclaimedIntentByClaimingAndCreatingOnce(
	t *testing.T,
) {
	dispatcher, fixture, repository, _, _, gateway := newAgentDispatchFixture(t)
	repository.claimErr = errors.New("claim store unavailable after intent append")

	_, err := dispatcher.DispatchGovernedAgentStage(
		context.Background(),
		fixture.subject,
		DispatchAgentStageCommand{
			ReviewRunID: fixture.command.ReviewRunID,
			StageID:     fixture.command.StageID,
		},
	)
	if err == nil || errors.Is(err, ErrAgentStageDispatchOutcomeUnknown) {
		t.Fatalf("first unclaimed append error = %v", err)
	}
	if repository.intent == nil || repository.claimed || gateway.dispatchCalls != 0 {
		t.Fatal("intent append failure either lost the intent or reached provider Create")
	}

	repository.claimErr = nil
	recovered, err := dispatcher.DispatchGovernedAgentStage(
		context.Background(),
		fixture.subject,
		DispatchAgentStageCommand{
			ReviewRunID: fixture.command.ReviewRunID,
			StageID:     fixture.command.StageID,
		},
	)
	if err != nil {
		t.Fatalf("DispatchGovernedAgentStage(recover unclaimed) error = %v", err)
	}
	if !recovered.Recovered || !repository.claimed || gateway.dispatchCalls != 1 ||
		gateway.lookupCalls != 0 {
		t.Fatalf(
			"unclaimed recovery = recovered %v claimed %v create %d lookup %d",
			recovered.Recovered,
			repository.claimed,
			gateway.dispatchCalls,
			gateway.lookupCalls,
		)
	}
}

func TestAgentStageDispatcherRejectsPreflightSubstitutionWhileRecoveringUnclaimedIntent(
	t *testing.T,
) {
	dispatcher, fixture, repository, _, _, gateway := newAgentDispatchFixture(t)
	repository.claimErr = errors.New("claim store unavailable after intent append")

	_, err := dispatcher.DispatchGovernedAgentStage(
		context.Background(),
		fixture.subject,
		DispatchAgentStageCommand{
			ReviewRunID: fixture.command.ReviewRunID,
			StageID:     fixture.command.StageID,
		},
	)
	if err == nil || repository.intent == nil || repository.claimed || gateway.dispatchCalls != 0 {
		t.Fatalf("failed to establish persisted unclaimed intent: %v", err)
	}

	repository.claimErr = nil
	gateway.preflightMutate = func(call int, request *contractsv1alpha1.StageExecutionRequest) {
		if call == 2 {
			request.ExecutionID += "-substituted"
			request.RequestSHA256 = ""
			sealed, sealErr := contractsv1alpha1.SealStageExecutionRequest(*request)
			if sealErr != nil {
				t.Fatalf("SealStageExecutionRequest(substituted) error = %v", sealErr)
			}
			*request = sealed
		}
	}
	_, err = dispatcher.DispatchGovernedAgentStage(
		context.Background(),
		fixture.subject,
		DispatchAgentStageCommand{
			ReviewRunID: fixture.command.ReviewRunID,
			StageID:     fixture.command.StageID,
		},
	)
	if err == nil || !strings.Contains(err.Error(), "did not preserve the exact formal request") {
		t.Fatalf("preflight substitution error = %v", err)
	}
	if repository.claimed || gateway.dispatchCalls != 0 {
		t.Fatal("substituted recovery token reached durable claim or provider Create")
	}
}

func TestAgentStageDispatcherPreflightFailureDoesNotPersistClaimOrIntent(t *testing.T) {
	dispatcher, fixture, repository, _, _, gateway := newAgentDispatchFixture(t)
	gateway.preflightErr = errors.New("governed input is unavailable")

	_, err := dispatcher.DispatchGovernedAgentStage(
		context.Background(),
		fixture.subject,
		DispatchAgentStageCommand{
			ReviewRunID: fixture.command.ReviewRunID,
			StageID:     fixture.command.StageID,
		},
	)
	if err == nil || errors.Is(err, ErrAgentStageDispatchOutcomeUnknown) {
		t.Fatalf("preflight error = %v", err)
	}
	if repository.intent != nil || repository.claimed || repository.claimCalls != 0 ||
		gateway.dispatchCalls != 0 || gateway.preflightCalls != 1 {
		t.Fatal("deterministic preflight failure consumed provider Create authority")
	}
}

func TestAgentStageDispatcherCanceledAuthorityStopsBeforePlanning(t *testing.T) {
	dispatcher, fixture, repository, authority, capability, gateway :=
		newAgentDispatchFixture(t)
	authority.err = errors.New("run is canceled")

	_, err := dispatcher.DispatchGovernedAgentStage(
		context.Background(),
		fixture.subject,
		DispatchAgentStageCommand{
			ReviewRunID: fixture.command.ReviewRunID,
			StageID:     fixture.command.StageID,
		},
	)
	if err == nil || !strings.Contains(err.Error(), "run is canceled") {
		t.Fatalf("canceled authority error = %v", err)
	}
	if repository.snapshotReads != 0 || repository.admissionWrites != 0 ||
		repository.intent != nil || capability.calls != 0 || gateway.preflightCalls != 0 ||
		gateway.dispatchCalls != 0 {
		t.Fatal("canceled run reached planning, persistence, capability, or provider work")
	}
}

func TestAgentStageDispatcherAuthorityCanceledAfterPreflightDoesNotClaim(t *testing.T) {
	dispatcher, fixture, repository, authority, _, gateway := newAgentDispatchFixture(t)
	authority.errorOnCall = 3
	authority.callError = errors.New("run canceled during provider preflight")

	_, err := dispatcher.DispatchGovernedAgentStage(
		context.Background(),
		fixture.subject,
		DispatchAgentStageCommand{
			ReviewRunID: fixture.command.ReviewRunID,
			StageID:     fixture.command.StageID,
		},
	)
	if err == nil || !strings.Contains(err.Error(), "canceled during provider preflight") {
		t.Fatalf("authority race error = %v", err)
	}
	if repository.claimCalls != 0 || repository.intent != nil || repository.claimed ||
		gateway.preflightCalls != 1 || gateway.dispatchCalls != 0 {
		t.Fatal("authority cancellation after preflight consumed provider Create authority")
	}
}

func TestAgentStageDispatcherFinishesClaimedCreateAfterCallerCancellation(t *testing.T) {
	dispatcher, fixture, repository, _, _, gateway := newAgentDispatchFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	repository.afterClaim = cancel

	dispatched, err := dispatcher.DispatchGovernedAgentStage(
		ctx,
		fixture.subject,
		DispatchAgentStageCommand{
			ReviewRunID: fixture.command.ReviewRunID,
			StageID:     fixture.command.StageID,
		},
	)
	if err != nil {
		t.Fatalf("DispatchGovernedAgentStage() error = %v", err)
	}
	if ctx.Err() != context.Canceled || gateway.dispatchCalls != 1 ||
		dispatched.Binding.BindingID == "" {
		t.Fatalf(
			"post-claim cancellation burned Create authority: ctx=%v creates=%d binding=%+v",
			ctx.Err(),
			gateway.dispatchCalls,
			dispatched.Binding,
		)
	}
}

func TestAgentStageDispatcherUsesRenewedLeaseAndFreshPostAdmissionTime(t *testing.T) {
	dispatcher, fixture, _, authority, _, _ := newAgentDispatchFixture(t)
	base := time.Now().UTC().Add(2 * time.Minute)
	initial := authority.authority
	initial.AcquiredAt = base.Add(-2 * time.Hour)
	initial.ExpiresAt = base.Add(-time.Hour + 10*time.Second)
	renewed := initial
	renewed.ExpiresAt = base.Add(time.Hour)
	authority.authorities = []AgentStageExecutionAuthority{initial, renewed, renewed}

	times := []time.Time{
		base.Add(-time.Hour),
		base,
		base.Add(time.Second),
		base.Add(2 * time.Second),
		base.Add(3 * time.Second),
		base.Add(4 * time.Second),
	}
	var clockCalls int
	dispatcher.now = func() time.Time {
		index := clockCalls
		clockCalls++
		if index >= len(times) {
			return times[len(times)-1]
		}
		return times[index]
	}

	dispatched, err := dispatcher.DispatchGovernedAgentStage(
		context.Background(),
		fixture.subject,
		DispatchAgentStageCommand{
			ReviewRunID: fixture.command.ReviewRunID,
			StageID:     fixture.command.StageID,
		},
	)
	if err != nil {
		t.Fatalf("DispatchGovernedAgentStage() error = %v", err)
	}
	if !dispatched.Request.Deadline.After(initial.ExpiresAt) ||
		dispatched.Request.Deadline.After(renewed.ExpiresAt) {
		t.Fatalf(
			"request deadline %s did not use renewed lease window (%s, %s]",
			dispatched.Request.Deadline,
			initial.ExpiresAt,
			renewed.ExpiresAt,
		)
	}
	if dispatched.Intent.RecordedAt.Before(dispatched.Admission.RecordedAt) {
		t.Fatalf(
			"intent time %s precedes admission %s",
			dispatched.Intent.RecordedAt,
			dispatched.Admission.RecordedAt,
		)
	}
}

func TestAgentStageExecutionAuthorityRejectsJSONUnsafeCoordinate(t *testing.T) {
	authority := AgentStageExecutionAuthority{
		WorkloadID: "workload-1", LeaseID: "lease-1", LeaseWorker: "worker-1",
		Attempt: int(maxAgentJSONSafeInteger + 1), Generation: 1, FencingToken: 1,
		AcquiredAt: time.Date(2026, 8, 24, 1, 0, 0, 0, time.UTC),
		ExpiresAt:  time.Date(2026, 8, 24, 2, 0, 0, 0, time.UTC),
	}
	if err := authority.validate(time.Date(2026, 8, 24, 1, 30, 0, 0, time.UTC)); err == nil ||
		!strings.Contains(err.Error(), "JSON-safe") {
		t.Fatalf("AgentStageExecutionAuthority.validate() error = %v", err)
	}
}

func newAgentDispatchFixture(
	t *testing.T,
) (
	*AgentStageDispatcher,
	agentPreparationFixture,
	*agentDispatchRepositoryStub,
	*agentExecutionAuthorityResolverStub,
	*agentCapabilityResolverStub,
	*agentExecutionGatewayStub,
) {
	t.Helper()
	fixture := newAgentPreparationFixture(t, preparationContextsEmpty)
	preparer, err := NewAgentStagePreparer(
		fixture.provider,
		fixture.resolver,
		fixture.repository,
		fixture.repository,
		func() time.Time { return fixture.recordedAt },
	)
	if err != nil {
		t.Fatal(err)
	}
	repository := &agentDispatchRepositoryStub{
		agentPreparationRepositoryStub: fixture.repository,
	}
	capability := &agentCapabilityResolverStub{}
	base := time.Now().UTC().Add(2 * time.Minute)
	authority := &agentExecutionAuthorityResolverStub{authority: AgentStageExecutionAuthority{
		WorkloadID: "run-1-workload", LeaseID: "run-1-workload-g1",
		LeaseWorker: "local-worker", Attempt: 1, Generation: 1, FencingToken: 1,
		AcquiredAt: base.Add(-time.Hour),
		ExpiresAt:  base.Add(time.Hour),
	}}
	gateway := &agentExecutionGatewayStub{repository: repository}
	dispatcher, err := NewAgentStageDispatcher(
		preparer,
		authority,
		capability,
		gateway,
		repository,
		func() time.Time { return base },
	)
	if err != nil {
		t.Fatal(err)
	}
	return dispatcher, fixture, repository, authority, capability, gateway
}

type agentExecutionAuthorityResolverStub struct {
	authority   AgentStageExecutionAuthority
	authorities []AgentStageExecutionAuthority
	err         error
	calls       int
	errorOnCall int
	callError   error
}

func (resolver *agentExecutionAuthorityResolverStub) CurrentAgentStageExecutionAuthority(
	_ context.Context,
	_ AgentPlanningSubject,
	_ string,
	_ string,
) (AgentStageExecutionAuthority, error) {
	resolver.calls++
	if resolver.errorOnCall > 0 && resolver.calls == resolver.errorOnCall {
		return AgentStageExecutionAuthority{}, resolver.callError
	}
	if len(resolver.authorities) > 0 {
		index := resolver.calls - 1
		if index >= len(resolver.authorities) {
			index = len(resolver.authorities) - 1
		}
		return resolver.authorities[index], resolver.err
	}
	return resolver.authority, resolver.err
}

type agentDispatchRepositoryStub struct {
	*agentPreparationRepositoryStub
	intent                       *runmodel.AgentStageDispatchIntent
	claimed                      bool
	binding                      *runmodel.AgentStageExecutionBinding
	terminal                     *runmodel.AgentStageTerminalGate
	callbackReceipt              *runmodel.AgentStageResultCallbackReceipt
	callbackReceiptRef           runmodel.ArtifactRef
	callbackReceiptAppendErr     error
	callbackReceiptCommitThenErr bool
	evidence                     *runmodel.AgentStageHypothesisEvidence
	evidenceAppendErr            error
	superseded                   []runmodel.AgentStageDispatchIntent
	claimCalls                   int
	claimErr                     error
	afterClaim                   func()
	terminalLookupErr            error
}

func (repository *agentDispatchRepositoryStub) ListClaimedAgentStageDispatchIntents(
	_ context.Context,
	runID string,
	subject runmodel.AgentPlanningSubject,
) ([]runmodel.AgentStageDispatchIntent, error) {
	result := make([]runmodel.AgentStageDispatchIntent, 0, len(repository.superseded))
	for _, intent := range repository.superseded {
		if intent.ReviewRunID != runID || intent.Subject != subject {
			return nil, errors.New("superseded intent mismatch")
		}
		result = append(result, intent)
	}
	if repository.intent != nil && repository.claimed {
		present := false
		for _, existing := range result {
			present = present || existing.IntentID == repository.intent.IntentID
		}
		if !present {
			if repository.intent.ReviewRunID != runID || repository.intent.Subject != subject {
				return nil, errors.New("claimed intent mismatch")
			}
			result = append(result, *repository.intent)
		}
	}
	return result, nil
}

func (repository *agentDispatchRepositoryStub) LookupAgentStageDispatchIntent(
	_ context.Context,
	runID string,
	stageID string,
	attempt int,
	generation int,
	subject runmodel.AgentPlanningSubject,
) (runmodel.AgentStageDispatchIntent, bool, error) {
	if repository.intent == nil {
		return runmodel.AgentStageDispatchIntent{}, false, nil
	}
	intent := *repository.intent
	if intent.ReviewRunID != runID || intent.Stage.ID != stageID ||
		intent.Attempt != attempt || intent.Generation != generation ||
		intent.Subject != subject {
		return runmodel.AgentStageDispatchIntent{}, false, errors.New("intent mismatch")
	}
	return intent, true, nil
}

func (repository *agentDispatchRepositoryStub) ClaimAgentStageDispatchIntent(
	_ context.Context,
	intent runmodel.AgentStageDispatchIntent,
) (bool, error) {
	repository.claimCalls++
	if repository.intent == nil {
		copy := intent
		repository.intent = &copy
		if repository.claimErr != nil {
			return false, repository.claimErr
		}
		repository.claimed = true
		if repository.afterClaim != nil {
			repository.afterClaim()
		}
		return true, nil
	}
	if repository.intent != nil {
		if *repository.intent != intent {
			return false, errors.New("intent conflict")
		}
		if !repository.claimed {
			repository.claimed = true
			if repository.afterClaim != nil {
				repository.afterClaim()
			}
			return true, nil
		}
		return false, nil
	}
	panic("unreachable")
}

func (repository *agentDispatchRepositoryStub) IsAgentStageDispatchIntentClaimed(
	_ context.Context,
	intent runmodel.AgentStageDispatchIntent,
) (bool, error) {
	if repository.intent == nil || *repository.intent != intent {
		return false, errors.New("intent mismatch")
	}
	return repository.claimed, nil
}

func (repository *agentDispatchRepositoryStub) LookupAgentStageExecutionBinding(
	_ context.Context,
	runID string,
	intentID string,
	subject runmodel.AgentPlanningSubject,
) (runmodel.AgentStageExecutionBinding, bool, error) {
	if repository.binding == nil {
		return runmodel.AgentStageExecutionBinding{}, false, nil
	}
	binding := *repository.binding
	if binding.ReviewRunID != runID || binding.IntentID != intentID || binding.Subject != subject {
		return runmodel.AgentStageExecutionBinding{}, false, errors.New("binding mismatch")
	}
	return binding, true, nil
}

func (repository *agentDispatchRepositoryStub) AppendAgentStageExecutionBinding(
	_ context.Context,
	binding runmodel.AgentStageExecutionBinding,
) error {
	if repository.intent == nil || !repository.claimed {
		return errors.New("binding before claimed intent")
	}
	if repository.binding != nil {
		if *repository.binding != binding {
			return errors.New("binding conflict")
		}
		return nil
	}
	copy := binding
	repository.binding = &copy
	return nil
}

func (repository *agentDispatchRepositoryStub) LookupAgentStageTerminalGate(
	_ context.Context,
	runID string,
	intentID string,
	subject runmodel.AgentPlanningSubject,
) (runmodel.AgentStageTerminalGate, bool, error) {
	if repository.terminalLookupErr != nil {
		return runmodel.AgentStageTerminalGate{}, false, repository.terminalLookupErr
	}
	if repository.terminal == nil {
		return runmodel.AgentStageTerminalGate{}, false, nil
	}
	gate := *repository.terminal
	if gate.ReviewRunID != runID || gate.IntentID != intentID || gate.Subject != subject {
		return runmodel.AgentStageTerminalGate{}, false, errors.New("terminal gate mismatch")
	}
	return gate, true, nil
}

func (repository *agentDispatchRepositoryStub) AppendAgentStageTerminalGate(
	_ context.Context,
	gate runmodel.AgentStageTerminalGate,
) error {
	if repository.terminal != nil {
		if repository.terminal.SHA256 == gate.SHA256 {
			return nil
		}
		return errors.New("terminal gate conflict")
	}
	copy := gate
	repository.terminal = &copy
	return nil
}

func (repository *agentDispatchRepositoryStub) LookupAgentStageResultCallbackReceipt(
	_ context.Context,
	runID string,
	bindingID string,
	subject runmodel.AgentPlanningSubject,
) (runmodel.AgentStageResultCallbackReceipt, runmodel.ArtifactRef, bool, error) {
	if repository.callbackReceipt == nil {
		return runmodel.AgentStageResultCallbackReceipt{}, runmodel.ArtifactRef{}, false, nil
	}
	receipt := *repository.callbackReceipt
	if receipt.ReviewRunID != runID || receipt.BindingID != bindingID ||
		receipt.Subject != subject {
		return runmodel.AgentStageResultCallbackReceipt{}, runmodel.ArtifactRef{}, false,
			errors.New("callback receipt mismatch")
	}
	return receipt, repository.callbackReceiptRef, true, nil
}

func (repository *agentDispatchRepositoryStub) AppendAgentStageResultCallbackReceipt(
	_ context.Context,
	receipt runmodel.AgentStageResultCallbackReceipt,
	receiptRef runmodel.ArtifactRef,
) error {
	commit := func() error {
		if repository.callbackReceipt != nil {
			if *repository.callbackReceipt != receipt || repository.callbackReceiptRef != receiptRef {
				return errors.New("callback receipt conflict")
			}
			return nil
		}
		copy := receipt
		repository.callbackReceipt = &copy
		repository.callbackReceiptRef = receiptRef
		return nil
	}
	if repository.callbackReceiptAppendErr != nil {
		err := repository.callbackReceiptAppendErr
		repository.callbackReceiptAppendErr = nil
		if repository.callbackReceiptCommitThenErr {
			if commitErr := commit(); commitErr != nil {
				return commitErr
			}
		}
		return err
	}
	return commit()
}

func (repository *agentDispatchRepositoryStub) LookupAgentStageHypothesisEvidence(
	_ context.Context,
	runID string,
	bindingID string,
	subject runmodel.AgentPlanningSubject,
) (runmodel.AgentStageHypothesisEvidence, bool, error) {
	if repository.evidence == nil {
		return runmodel.AgentStageHypothesisEvidence{}, false, nil
	}
	evidence := *repository.evidence
	if evidence.ReviewRunID != runID || evidence.BindingID != bindingID ||
		evidence.Subject != subject {
		return runmodel.AgentStageHypothesisEvidence{}, false, errors.New("evidence mismatch")
	}
	return evidence, true, nil
}

func (repository *agentDispatchRepositoryStub) AppendAgentStageHypothesisEvidence(
	_ context.Context,
	evidence runmodel.AgentStageHypothesisEvidence,
) error {
	if repository.evidenceAppendErr != nil {
		err := repository.evidenceAppendErr
		repository.evidenceAppendErr = nil
		return err
	}
	if repository.evidence != nil {
		if *repository.evidence == evidence {
			return nil
		}
		return errors.New("hypothesis evidence conflict")
	}
	copy := evidence
	repository.evidence = &copy
	return nil
}

type agentCapabilityResolverStub struct {
	calls  int
	mutate func(*contractsv1alpha1.ExecutorCapabilitySnapshot)
}

func (resolver *agentCapabilityResolverStub) ResolveAgentExecutorCapability(
	_ context.Context,
	_ AgentPlanningSubject,
	plan contractsv1alpha1.AgentStagePlan,
) (contractsv1alpha1.ExecutorCapabilitySnapshot, error) {
	resolver.calls++
	capability := contractsv1alpha1.ExecutorCapabilitySnapshot{
		RuntimeKind:     plan.Runtime.Ref.ID,
		RuntimeID:       "pi-worker-1",
		RuntimeRevision: plan.Runtime.Ref.Revision,
		RuntimeSHA256:   plan.Runtime.Ref.SHA256,
		BuildIdentity:   plan.BuildIdentity,
		Authority: contractsv1alpha1.ExecutionAuthority{
			AllowedTools:       append([]string{}, plan.ToolAuthority.Tools...),
			ModelEgress:        plan.ModelAuthority.ModelEgress,
			ToolNetwork:        plan.ToolAuthority.ToolNetwork,
			WorkspaceReads:     plan.ToolAuthority.WorkspaceReads,
			WorkspaceWrites:    plan.ToolAuthority.WorkspaceWrites,
			RemoteWrites:       plan.ToolAuthority.RemoteWrites,
			MaxDelegationDepth: plan.ToolAuthority.MaxDelegationDepth,
		},
		Trust: contractsv1alpha1.ExecutorTrust{
			Authority: contractsv1alpha1.ExecutorTrustAuthorityLocalHost,
			CapabilityVerifier: contractsv1alpha1.VersionedRef{
				ID: "test-capability-verifier", Revision: "1", SHA256: plan.Runtime.Ref.SHA256,
			},
			CallbackVerifier: contractsv1alpha1.VersionedRef{
				ID: "test-callback-verifier", Revision: "1", SHA256: plan.Runtime.Ref.SHA256,
			},
		},
	}
	digest, err := contractsv1alpha1.DigestExecutorCapability(capability)
	if err != nil {
		return contractsv1alpha1.ExecutorCapabilitySnapshot{}, err
	}
	capability.SHA256 = digest
	if resolver.mutate != nil {
		resolver.mutate(&capability)
	}
	return capability, nil
}

type agentExecutionGatewayStub struct {
	repository           *agentDispatchRepositoryStub
	delegate             *execution.Gateway
	dispatchCalls        int
	lookupCalls          int
	dispatchErr          error
	preflightErr         error
	preflightCalls       int
	lookupFound          bool
	cancelCalls          int
	cancelErr            error
	lastCancelHandle     execution.Handle
	lastCancelKey        string
	handle               execution.Handle
	preflightRequest     contractsv1alpha1.StageExecutionRequest
	preflightMutate      func(int, *contractsv1alpha1.StageExecutionRequest)
	sawClaimBeforeCreate bool
	providerStarts       int
	ensureAccepted       bool
	rejectBeforeAccept   bool
	afterEnsure          func(contractsv1alpha1.StageExecutionRequest)
	afterLookup          func()
}

func (gateway *agentExecutionGatewayStub) Preflight(
	ctx context.Context,
	request contractsv1alpha1.StageExecutionRequest,
) (execution.PreparedEnsure, error) {
	gateway.preflightCalls++
	if gateway.preflightErr != nil {
		return execution.PreparedEnsure{}, gateway.preflightErr
	}
	if gateway.preflightMutate != nil {
		gateway.preflightMutate(gateway.preflightCalls, &request)
	}
	delegate, err := execution.NewGateway(
		agentExecutionPlatformPortStub{gateway: gateway},
		agentDispatchArtifactResolverStub{},
		agentDispatchClaimAuthorizerStub{repository: gateway.repository},
	)
	if err != nil {
		return execution.PreparedEnsure{}, err
	}
	gateway.delegate = delegate
	gateway.preflightRequest = request
	return delegate.Preflight(ctx, request)
}

func (gateway *agentExecutionGatewayStub) EnsurePrepared(
	ctx context.Context,
	prepared execution.PreparedEnsure,
) (execution.Handle, error) {
	if gateway.delegate == nil {
		return execution.Handle{}, errors.New("execution gateway preflight was not called")
	}
	return gateway.delegate.EnsurePrepared(ctx, prepared)
}

func (gateway *agentExecutionGatewayStub) PrepareClaimedEnsure(
	ctx context.Context,
	request contractsv1alpha1.StageExecutionRequest,
) (execution.PreparedClaimedEnsure, error) {
	if gateway.delegate == nil {
		return execution.PreparedClaimedEnsure{}, errors.New(
			"execution gateway preflight was not called",
		)
	}
	return gateway.delegate.PrepareClaimedEnsure(ctx, request)
}

func (gateway *agentExecutionGatewayStub) EnsureClaimed(
	ctx context.Context,
	prepared execution.PreparedClaimedEnsure,
) (execution.Handle, error) {
	if gateway.delegate == nil {
		return execution.Handle{}, errors.New("execution gateway preflight was not called")
	}
	return gateway.delegate.EnsureClaimed(ctx, prepared)
}

func (gateway *agentExecutionGatewayStub) platformCreate(
	_ context.Context,
	request contractsv1alpha1.StageExecutionRequest,
) (execution.Handle, error) {
	gateway.dispatchCalls++
	gateway.sawClaimBeforeCreate = gateway.repository.intent != nil && gateway.repository.claimed
	if gateway.dispatchErr != nil && gateway.rejectBeforeAccept {
		return execution.Handle{}, gateway.dispatchErr
	}
	if !gateway.ensureAccepted {
		gateway.ensureAccepted = true
		gateway.providerStarts++
	}
	gateway.handle = execution.Handle{
		ProviderHandle: "pi-provider-" + request.ExecutionID,
		ExecutionID:    request.ExecutionID, Attempt: request.Attempt,
		Generation: request.Generation, FencingToken: request.FencingToken,
		IdempotencyKey: request.IdempotencyKey, RequestSHA256: request.RequestSHA256,
		CapabilitySHA256: request.Capability.SHA256,
	}
	if gateway.afterEnsure != nil {
		gateway.afterEnsure(request)
	}
	if gateway.dispatchErr != nil && gateway.dispatchCalls == 1 {
		return execution.Handle{}, gateway.dispatchErr
	}
	return gateway.handle, nil
}

func (gateway *agentExecutionGatewayStub) Lookup(
	_ context.Context,
	_ contractsv1alpha1.StageExecutionRequest,
) (execution.Handle, bool, error) {
	gateway.lookupCalls++
	handle := gateway.handle
	if gateway.afterLookup != nil {
		gateway.afterLookup()
	}
	return handle, gateway.lookupFound, nil
}

func (gateway *agentExecutionGatewayStub) PrepareCancel(
	ctx context.Context,
	handle execution.Handle,
	authority execution.TerminalCancelAuthority,
) (execution.PreparedCancel, error) {
	if gateway.delegate == nil {
		return execution.PreparedCancel{}, errors.New("execution gateway preflight was not called")
	}
	return gateway.delegate.PrepareCancel(ctx, handle, authority)
}

func (gateway *agentExecutionGatewayStub) CancelPrepared(
	ctx context.Context,
	prepared execution.PreparedCancel,
) error {
	if gateway.delegate == nil {
		return errors.New("execution gateway preflight was not called")
	}
	return gateway.delegate.CancelPrepared(ctx, prepared)
}

func (gateway *agentExecutionGatewayStub) platformCancel(
	_ context.Context,
	request execution.CancelRequest,
) error {
	gateway.cancelCalls++
	gateway.lastCancelHandle = execution.Handle{
		ProviderHandle: request.ProviderHandle,
		ExecutionID:    request.ExecutionID, Attempt: request.Attempt,
		Generation: request.Generation, FencingToken: request.FencingToken,
		IdempotencyKey: request.CreateIdempotencyKey,
		RequestSHA256:  request.RequestSHA256, CapabilitySHA256: request.CapabilitySHA256,
	}
	gateway.lastCancelKey = request.CancelIdempotencyKey
	return gateway.cancelErr
}

type agentExecutionPlatformPortStub struct {
	gateway *agentExecutionGatewayStub
}

func (port agentExecutionPlatformPortStub) Ensure(
	ctx context.Context,
	request contractsv1alpha1.StageExecutionRequest,
) (execution.Handle, error) {
	return port.gateway.platformCreate(ctx, request)
}

func (port agentExecutionPlatformPortStub) Lookup(
	ctx context.Context,
	request contractsv1alpha1.StageExecutionRequest,
) (execution.Handle, bool, error) {
	return port.gateway.Lookup(ctx, request)
}

func (port agentExecutionPlatformPortStub) Cancel(
	ctx context.Context,
	request execution.CancelRequest,
) error {
	return port.gateway.platformCancel(ctx, request)
}

type agentDispatchArtifactResolverStub struct{}

func (agentDispatchArtifactResolverStub) Verify(
	context.Context,
	string,
	string,
	contractsv1alpha1.ArtifactBinding,
	execution.BindingPurpose,
) error {
	return nil
}

type agentDispatchClaimAuthorizerStub struct {
	repository *agentDispatchRepositoryStub
}

func (authorizer agentDispatchClaimAuthorizerStub) AuthorizeClaimedEnsure(
	_ context.Context,
	request contractsv1alpha1.StageExecutionRequest,
) error {
	if authorizer.repository == nil || authorizer.repository.intent == nil ||
		!authorizer.repository.claimed {
		return errors.New("durable dispatch claim is absent")
	}
	intent := *authorizer.repository.intent
	if request.ReviewRunID != intent.ReviewRunID || request.Stage.ID != intent.Stage.ID ||
		request.ExecutionID != intent.ExecutionID || request.Attempt != intent.Attempt ||
		request.Generation != intent.Generation || request.FencingToken != intent.FencingToken ||
		request.IdempotencyKey != intent.CreateIdempotencyKey ||
		request.RequestSHA256 != intent.RequestSemanticSHA256 {
		return errors.New("durable dispatch claim does not bind exact request")
	}
	return nil
}

func (authorizer agentDispatchClaimAuthorizerStub) AuthorizeTerminalCancel(
	_ context.Context,
	request execution.CancelRequest,
) error {
	if authorizer.repository == nil || authorizer.repository.terminal == nil ||
		authorizer.repository.terminal.Kind != runmodel.AgentStageCancellationRequested ||
		authorizer.repository.terminal.Cancellation == nil {
		return errors.New("durable terminal cancellation is absent")
	}
	terminal := *authorizer.repository.terminal
	var intent *runmodel.AgentStageDispatchIntent
	if authorizer.repository.intent != nil &&
		authorizer.repository.intent.IntentID == terminal.IntentID {
		intent = authorizer.repository.intent
	}
	for index := range authorizer.repository.superseded {
		if authorizer.repository.superseded[index].IntentID == terminal.IntentID {
			intent = &authorizer.repository.superseded[index]
		}
	}
	if intent == nil {
		return errors.New("durable terminal cancellation has no exact dispatch intent")
	}
	if terminal.Cancellation.CancelIdempotencyKey != request.CancelIdempotencyKey ||
		request.Authority.TenantID != intent.Subject.TenantID ||
		request.Authority.WorkspaceID != intent.Subject.WorkspaceID ||
		request.Authority.ReviewRunID != intent.ReviewRunID ||
		request.Authority.StageID != intent.Stage.ID ||
		request.Authority.StageRevision != intent.Stage.Revision ||
		request.Authority.StageSHA256 != intent.Stage.SHA256 ||
		request.Authority.IntentID != intent.IntentID ||
		request.Authority.IntentSHA256 != intent.SHA256 ||
		request.Authority.TerminalGateID != terminal.GateID ||
		request.Authority.TerminalGateSHA256 != terminal.SHA256 ||
		request.Authority.CancelIdempotencyKey != request.CancelIdempotencyKey ||
		request.ExecutionID != terminal.ExecutionID || request.Attempt != terminal.Attempt ||
		request.Generation != terminal.Generation ||
		request.FencingToken != terminal.FencingToken ||
		request.CreateIdempotencyKey != intent.CreateIdempotencyKey ||
		request.RequestSHA256 != terminal.RequestSemanticSHA256 ||
		request.CapabilitySHA256 != terminal.CapabilitySHA256 ||
		authorizer.repository.binding == nil ||
		request.ProviderHandle != authorizer.repository.binding.ProviderHandle {
		return errors.New("durable terminal cancellation does not bind exact target")
	}
	return nil
}
