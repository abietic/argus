package application

import (
	"context"
	"errors"
	"testing"
	"time"

	"argus.local/argus/internal/runmodel"
)

func TestHistoricalAgentStageReconcilerLooksUpBindsAndCancelsSupersededUnknownCreate(
	t *testing.T,
) {
	dispatcher, fixture, repository, _, _, gateway := newAgentDispatchFixture(t)
	gateway.dispatchErr = errors.New("transport closed after provider accepted create")
	_, err := dispatcher.DispatchGovernedAgentStage(
		context.Background(),
		fixture.subject,
		DispatchAgentStageCommand{
			ReviewRunID: fixture.command.ReviewRunID,
			StageID:     fixture.command.StageID,
		},
	)
	if !errors.Is(err, ErrAgentStageDispatchOutcomeUnknown) || repository.intent == nil ||
		repository.binding != nil || gateway.dispatchCalls != 1 {
		t.Fatalf(
			"failed to establish unknown historical Create: error=%v intent=%v binding=%v creates=%d",
			err,
			repository.intent != nil,
			repository.binding != nil,
			gateway.dispatchCalls,
		)
	}
	repository.superseded = []runmodel.AgentStageDispatchIntent{*repository.intent}
	gateway.dispatchErr = nil
	gateway.lookupFound = true
	cancelRequestedAt := repository.intent.RecordedAt.Add(time.Minute)
	bindingRecordedAt := cancelRequestedAt.Add(time.Minute)
	clockCalls := 0
	reconciler, err := NewAgentStageHistoricalReconciler(
		repository,
		historicalIntentStalenessStub{
			disposition: AgentStageHistoricalIntentDisposition{
				Stale:  true,
				Reason: "authoritative scheduling lease was reassigned",
			},
		},
		gateway,
		"argus-recovery-worker",
		func() time.Time {
			clockCalls++
			if clockCalls == 1 {
				return cancelRequestedAt
			}
			return bindingRecordedAt
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	report, err := reconciler.ReconcileSupersededAgentStages(
		context.Background(),
		fixture.subject,
		ReconcileHistoricalAgentStagesCommand{
			ReviewRunID: fixture.command.ReviewRunID,
		},
	)
	if err != nil {
		t.Fatalf("ReconcileSupersededAgentStages() error = %v", err)
	}
	if len(report.Items) != 1 || !report.Items[0].ProviderFound ||
		!report.Items[0].CancelDelivered || report.Items[0].Binding == nil ||
		report.Items[0].Terminal.Kind != runmodel.AgentStageCancellationRequested ||
		repository.binding == nil || repository.terminal == nil ||
		gateway.lookupCalls != 1 || gateway.cancelCalls != 1 ||
		gateway.dispatchCalls != 1 || gateway.lastCancelKey != repository.intent.CancelIdempotencyKey {
		t.Fatalf("historical reconciliation did not lookup-only bind+cancel: %+v", report)
	}
	if repository.binding.RecordedAt != bindingRecordedAt ||
		repository.terminal.Cancellation.RequestedAt != cancelRequestedAt {
		t.Fatalf(
			"provider lookup did not receive a fresh binding timestamp: binding=%s cancel=%s",
			repository.binding.RecordedAt,
			repository.terminal.Cancellation.RequestedAt,
		)
	}

	retry, err := reconciler.ReconcileSupersededAgentStages(
		context.Background(),
		fixture.subject,
		ReconcileHistoricalAgentStagesCommand{
			ReviewRunID: fixture.command.ReviewRunID,
		},
	)
	if err != nil {
		t.Fatalf("ReconcileSupersededAgentStages(retry) error = %v", err)
	}
	if len(retry.Items) != 1 || !retry.Items[0].CancelDelivered ||
		gateway.lookupCalls != 1 || gateway.cancelCalls != 2 || gateway.dispatchCalls != 1 {
		t.Fatalf("historical retry was not idempotent lookup/cancel: %+v", retry)
	}
}

func TestHistoricalAgentStageReconcilerRejectsLookupProviderHandleDrift(t *testing.T) {
	dispatcher, fixture, repository, _, _, gateway := newAgentDispatchFixture(t)
	gateway.dispatchErr = errors.New("transport closed after provider accepted create")
	_, err := dispatcher.DispatchGovernedAgentStage(
		context.Background(),
		fixture.subject,
		DispatchAgentStageCommand{
			ReviewRunID: fixture.command.ReviewRunID,
			StageID:     fixture.command.StageID,
		},
	)
	if !errors.Is(err, ErrAgentStageDispatchOutcomeUnknown) || repository.intent == nil {
		t.Fatalf("failed to establish unknown dispatch: %v", err)
	}
	repository.superseded = []runmodel.AgentStageDispatchIntent{*repository.intent}
	gateway.lookupFound = true
	gateway.afterLookup = func() {
		winnerHandle := gateway.handle
		winnerHandle.ProviderHandle = "pi-provider-historical-winner"
		winner, sealErr := sealHistoricalAgentStageBinding(
			*repository.intent,
			winnerHandle,
			repository.intent.RecordedAt.Add(2*time.Minute),
		)
		if sealErr != nil {
			t.Fatal(sealErr)
		}
		repository.binding = &winner
	}
	reconciler, err := NewAgentStageHistoricalReconciler(
		repository,
		historicalIntentStalenessStub{disposition: AgentStageHistoricalIntentDisposition{
			Stale: true, Reason: "authoritative scheduling lease was reassigned",
		}},
		gateway,
		"argus-recovery-worker",
		func() time.Time { return repository.intent.RecordedAt.Add(time.Minute) },
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = reconciler.ReconcileSupersededAgentStages(
		context.Background(),
		fixture.subject,
		ReconcileHistoricalAgentStagesCommand{ReviewRunID: fixture.command.ReviewRunID},
	)
	if !errors.Is(err, ErrAgentStageProviderContractViolation) {
		t.Fatalf("historical provider handle drift error = %v", err)
	}
	if gateway.cancelCalls != 0 || repository.binding == nil ||
		repository.binding.ProviderHandle != "pi-provider-historical-winner" {
		t.Fatalf("drift reached cancel or replaced winner: cancel=%d binding=%+v", gateway.cancelCalls, repository.binding)
	}
}

func TestHistoricalAgentStageReconcilerSealsCancellationWhenProviderNotYetVisible(
	t *testing.T,
) {
	dispatcher, fixture, repository, _, _, gateway := newAgentDispatchFixture(t)
	gateway.dispatchErr = errors.New("unknown create")
	_, _ = dispatcher.DispatchGovernedAgentStage(
		context.Background(),
		fixture.subject,
		DispatchAgentStageCommand{
			ReviewRunID: fixture.command.ReviewRunID,
			StageID:     fixture.command.StageID,
		},
	)
	repository.superseded = []runmodel.AgentStageDispatchIntent{*repository.intent}
	gateway.lookupFound = false
	reconciler, err := NewAgentStageHistoricalReconciler(
		repository,
		historicalIntentStalenessStub{
			disposition: AgentStageHistoricalIntentDisposition{
				Stale:  true,
				Reason: "authoritative scheduling workload was canceled",
			},
		},
		gateway,
		"argus-recovery-worker",
		func() time.Time { return repository.intent.RecordedAt.Add(time.Minute) },
	)
	if err != nil {
		t.Fatal(err)
	}
	report, err := reconciler.ReconcileSupersededAgentStages(
		context.Background(),
		fixture.subject,
		ReconcileHistoricalAgentStagesCommand{
			ReviewRunID: fixture.command.ReviewRunID,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Items) != 1 || report.Items[0].ProviderFound ||
		report.Items[0].CancelDelivered || repository.terminal == nil ||
		repository.terminal.Kind != runmodel.AgentStageCancellationRequested ||
		repository.binding != nil || gateway.cancelCalls != 0 || gateway.dispatchCalls != 1 {
		t.Fatalf("missing provider handle did not retain a cancellation fence: %+v", report)
	}
}

func TestHistoricalAgentStageReconcilerDoesNotCancelExactCurrentLease(t *testing.T) {
	dispatcher, fixture, repository, _, _, gateway := newAgentDispatchFixture(t)
	if _, err := dispatcher.DispatchGovernedAgentStage(
		context.Background(),
		fixture.subject,
		DispatchAgentStageCommand{
			ReviewRunID: fixture.command.ReviewRunID,
			StageID:     fixture.command.StageID,
		},
	); err != nil {
		t.Fatal(err)
	}
	repository.superseded = []runmodel.AgentStageDispatchIntent{*repository.intent}
	reconciler, err := NewAgentStageHistoricalReconciler(
		repository,
		historicalIntentStalenessStub{},
		gateway,
		"argus-recovery-worker",
		func() time.Time { return repository.intent.RecordedAt.Add(time.Minute) },
	)
	if err != nil {
		t.Fatal(err)
	}
	report, err := reconciler.ReconcileSupersededAgentStages(
		context.Background(),
		fixture.subject,
		ReconcileHistoricalAgentStagesCommand{ReviewRunID: fixture.command.ReviewRunID},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Items) != 1 || !report.Items[0].Current ||
		repository.terminal != nil || gateway.lookupCalls != 0 || gateway.cancelCalls != 0 {
		t.Fatalf("current lease was not skipped safely: %+v", report)
	}
}

func TestHistoricalAgentStageReconcilerContinuesAfterPerIntentFailure(t *testing.T) {
	dispatcher, fixture, repository, _, _, gateway := newAgentDispatchFixture(t)
	gateway.dispatchErr = errors.New("unknown create")
	_, _ = dispatcher.DispatchGovernedAgentStage(
		context.Background(),
		fixture.subject,
		DispatchAgentStageCommand{
			ReviewRunID: fixture.command.ReviewRunID,
			StageID:     fixture.command.StageID,
		},
	)
	// A duplicated exact fact is sufficient to prove batch isolation: the first
	// classification failure must not starve the later item.
	repository.superseded = []runmodel.AgentStageDispatchIntent{
		*repository.intent,
		*repository.intent,
	}
	gateway.lookupFound = false
	staleness := &historicalIntentStalenessSequenceStub{
		errors: []error{errors.New("temporary workload read failure"), nil},
		dispositions: []AgentStageHistoricalIntentDisposition{
			{},
			{Stale: true, Reason: "authoritative scheduling lease expired"},
		},
	}
	reconciler, err := NewAgentStageHistoricalReconciler(
		repository,
		staleness,
		gateway,
		"argus-recovery-worker",
		func() time.Time { return repository.intent.RecordedAt.Add(time.Minute) },
	)
	if err != nil {
		t.Fatal(err)
	}
	report, err := reconciler.ReconcileSupersededAgentStages(
		context.Background(),
		fixture.subject,
		ReconcileHistoricalAgentStagesCommand{ReviewRunID: fixture.command.ReviewRunID},
	)
	if err == nil || len(report.Items) != 2 || report.Items[0].Error == "" ||
		report.Items[1].Error != "" ||
		report.Items[1].Terminal.Kind != runmodel.AgentStageCancellationRequested ||
		staleness.calls != 2 {
		t.Fatalf("per-intent failure starved later reconciliation: report=%+v error=%v", report, err)
	}
}

type historicalIntentStalenessStub struct {
	disposition AgentStageHistoricalIntentDisposition
	err         error
}

type historicalIntentStalenessSequenceStub struct {
	dispositions []AgentStageHistoricalIntentDisposition
	errors       []error
	calls        int
}

func (stub *historicalIntentStalenessSequenceStub) ClassifyAgentStageHistoricalIntent(
	_ context.Context,
	_ AgentPlanningSubject,
	_ runmodel.AgentStageDispatchIntent,
) (AgentStageHistoricalIntentDisposition, error) {
	index := stub.calls
	stub.calls++
	return stub.dispositions[index], stub.errors[index]
}

func (stub historicalIntentStalenessStub) ClassifyAgentStageHistoricalIntent(
	_ context.Context,
	_ AgentPlanningSubject,
	_ runmodel.AgentStageDispatchIntent,
) (AgentStageHistoricalIntentDisposition, error) {
	return stub.disposition, stub.err
}
