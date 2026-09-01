package application

import (
	"context"
	"fmt"

	"github.com/abietic/argus/internal/runmodel"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

// FormalAgentReviewTerminalRepository is the minimum trusted read boundary
// needed to reconstruct an already-admitted formal stage terminal. It reads
// only immutable local ledger facts; it never consults provider state or a
// mutable configuration revision.
type FormalAgentReviewTerminalRepository interface {
	ReadLocalArtifact(context.Context, runmodel.ArtifactRef) ([]byte, error)
	ListClaimedAgentStageDispatchIntents(
		context.Context,
		string,
		runmodel.AgentPlanningSubject,
	) ([]runmodel.AgentStageDispatchIntent, error)
	LookupAgentStageDispatchIntent(
		context.Context,
		string,
		string,
		int,
		int,
		runmodel.AgentPlanningSubject,
	) (runmodel.AgentStageDispatchIntent, bool, error)
	LookupAgentStageTerminalGate(
		context.Context,
		string,
		string,
		runmodel.AgentPlanningSubject,
	) (runmodel.AgentStageTerminalGate, bool, error)
	LookupAgentStageHypothesisEvidence(
		context.Context,
		string,
		string,
		runmodel.AgentPlanningSubject,
	) (runmodel.AgentStageHypothesisEvidence, bool, error)
}

// ReadLatestFormalAgentReviewTerminal resolves the newest claimed inner stage
// attempt from Argus' immutable dispatch ledger. It deliberately does not use
// the scheduling workload attempt: one outer workload lease may contain
// several plan-authorized formal stage attempts.
func ReadLatestFormalAgentReviewTerminal(
	ctx context.Context,
	repository FormalAgentReviewTerminalRepository,
	subject AgentPlanningSubject,
	reviewRunID string,
	stageID string,
) (FormalAgentReviewTerminal, bool, error) {
	if ctx == nil {
		return FormalAgentReviewTerminal{}, false, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return FormalAgentReviewTerminal{}, false, err
	}
	if repository == nil {
		return FormalAgentReviewTerminal{}, false, fmt.Errorf(
			"formal terminal repository is required",
		)
	}
	if err := subject.validate(); err != nil {
		return FormalAgentReviewTerminal{}, false, fmt.Errorf(
			"validate formal terminal subject: %w", err,
		)
	}
	if err := validateLocalIdentity("review_run_id", reviewRunID); err != nil {
		return FormalAgentReviewTerminal{}, false, err
	}
	if stageID == "" {
		return FormalAgentReviewTerminal{}, false, fmt.Errorf("stage_id is required")
	}
	intents, err := repository.ListClaimedAgentStageDispatchIntents(
		ctx, reviewRunID, planningLedgerSubject(subject),
	)
	if err != nil {
		return FormalAgentReviewTerminal{}, false, fmt.Errorf(
			"list formal terminal dispatch intents: %w", err,
		)
	}
	var latest *runmodel.AgentStageDispatchIntent
	for index := range intents {
		intent := &intents[index]
		if intent.Stage.ID != stageID {
			continue
		}
		if latest == nil || intent.Attempt > latest.Attempt ||
			(intent.Attempt == latest.Attempt && intent.Generation > latest.Generation) {
			latest = intent
		}
	}
	if latest == nil {
		return FormalAgentReviewTerminal{}, false, nil
	}
	return ReadFormalAgentReviewTerminal(
		ctx, repository, subject, ReadFormalAgentReviewTerminalCommand{
			ReviewRunID: reviewRunID, StageID: stageID,
			Attempt: latest.Attempt, Generation: latest.Generation,
		},
	)
}

type ReadFormalAgentReviewTerminalCommand struct {
	ReviewRunID string
	StageID     string
	Attempt     int
	Generation  int
}

// FormalAgentReviewTerminal is a read projection over already-admitted facts.
// Hypotheses is present only for a succeeded provider result. A host-side
// cancellation gate is deliberately not converted into a provider result.
type FormalAgentReviewTerminal struct {
	Result     contractsv1alpha1.StageExecutionResult
	Hypotheses *contractsv1alpha1.ReviewHypothesisSet
	Gate       runmodel.AgentStageTerminalGate
}

// ReadFormalAgentReviewTerminal reconstructs an exact provider terminal from
// the authoritative dispatch coordinate and terminal gate. runrepo revalidates
// the complete upstream request/binding/callback closure while loading these
// facts; this service additionally requires succeeded hypothesis evidence so a
// terminal output cannot be surfaced before formal application admission.
func ReadFormalAgentReviewTerminal(
	ctx context.Context,
	repository FormalAgentReviewTerminalRepository,
	subject AgentPlanningSubject,
	command ReadFormalAgentReviewTerminalCommand,
) (FormalAgentReviewTerminal, bool, error) {
	if ctx == nil {
		return FormalAgentReviewTerminal{}, false, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return FormalAgentReviewTerminal{}, false, err
	}
	if repository == nil {
		return FormalAgentReviewTerminal{}, false, fmt.Errorf(
			"formal terminal repository is required",
		)
	}
	if err := subject.validate(); err != nil {
		return FormalAgentReviewTerminal{}, false, fmt.Errorf(
			"validate formal terminal subject: %w", err,
		)
	}
	if err := validateLocalIdentity("review_run_id", command.ReviewRunID); err != nil {
		return FormalAgentReviewTerminal{}, false, err
	}
	if command.StageID == "" || command.Attempt <= 0 || command.Generation <= 0 {
		return FormalAgentReviewTerminal{}, false, fmt.Errorf(
			"stage_id, positive attempt, and positive generation are required",
		)
	}
	ledgerSubject := planningLedgerSubject(subject)
	intent, found, err := repository.LookupAgentStageDispatchIntent(
		ctx,
		command.ReviewRunID,
		command.StageID,
		command.Attempt,
		command.Generation,
		ledgerSubject,
	)
	if err != nil {
		return FormalAgentReviewTerminal{}, false, fmt.Errorf(
			"lookup formal terminal dispatch intent: %w", err,
		)
	}
	if !found {
		return FormalAgentReviewTerminal{}, false, nil
	}
	gate, found, err := repository.LookupAgentStageTerminalGate(
		ctx,
		command.ReviewRunID,
		intent.IntentID,
		ledgerSubject,
	)
	if err != nil {
		return FormalAgentReviewTerminal{}, false, fmt.Errorf(
			"lookup formal terminal winner: %w", err,
		)
	}
	if !found {
		return FormalAgentReviewTerminal{}, false, nil
	}

	var resultRef runmodel.ArtifactRef
	switch gate.Kind {
	case runmodel.AgentStageSucceededResultAccepted:
		if gate.Completion == nil {
			return FormalAgentReviewTerminal{}, false, fmt.Errorf(
				"succeeded formal terminal has no completion",
			)
		}
		resultRef = gate.Completion.ResultRef
	case runmodel.AgentStageFailedResultAccepted,
		runmodel.AgentStageCanceledResultAccepted:
		if gate.Outcome == nil {
			return FormalAgentReviewTerminal{}, false, fmt.Errorf(
				"non-succeeded formal terminal has no outcome",
			)
		}
		resultRef = gate.Outcome.ResultRef
	case runmodel.AgentStageCancellationRequested:
		return FormalAgentReviewTerminal{}, false, fmt.Errorf(
			"formal terminal is an authoritative host cancellation, not a provider result",
		)
	default:
		return FormalAgentReviewTerminal{}, false, fmt.Errorf(
			"unsupported formal terminal kind %q", gate.Kind,
		)
	}
	resultBytes, err := repository.ReadLocalArtifact(ctx, resultRef)
	if err != nil {
		return FormalAgentReviewTerminal{}, false, fmt.Errorf(
			"read admitted formal StageExecutionResult: %w", err,
		)
	}
	result, err := contractsv1alpha1.DecodeStageExecutionResult(resultBytes)
	if err != nil {
		return FormalAgentReviewTerminal{}, false, fmt.Errorf(
			"strictly decode admitted formal StageExecutionResult: %w", err,
		)
	}
	terminal := FormalAgentReviewTerminal{Result: result, Gate: gate}
	if gate.Kind != runmodel.AgentStageSucceededResultAccepted {
		return terminal, true, nil
	}

	evidence, found, err := repository.LookupAgentStageHypothesisEvidence(
		ctx,
		command.ReviewRunID,
		gate.Completion.BindingID,
		ledgerSubject,
	)
	if err != nil {
		return FormalAgentReviewTerminal{}, false, fmt.Errorf(
			"lookup admitted formal hypothesis evidence: %w", err,
		)
	}
	if !found || evidence.TerminalGateID != gate.GateID ||
		evidence.TerminalGateSHA256 != gate.SHA256 || evidence.ResultRef != resultRef {
		return FormalAgentReviewTerminal{}, false, fmt.Errorf(
			"succeeded formal terminal has no exact admitted hypothesis evidence",
		)
	}
	hypothesisBytes, err := repository.ReadLocalArtifact(ctx, evidence.Output.Local)
	if err != nil {
		return FormalAgentReviewTerminal{}, false, fmt.Errorf(
			"read admitted formal ReviewHypothesisSet: %w", err,
		)
	}
	hypotheses, err := contractsv1alpha1.DecodeReviewHypothesisSet(hypothesisBytes)
	if err != nil {
		return FormalAgentReviewTerminal{}, false, fmt.Errorf(
			"strictly decode admitted formal ReviewHypothesisSet: %w", err,
		)
	}
	terminal.Hypotheses = &hypotheses
	return terminal, true, nil
}
