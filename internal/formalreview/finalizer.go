package formalreview

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"time"

	"github.com/abietic/argus/internal/application"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/source/gitadapter"
	"github.com/abietic/argus/internal/targetmodel"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

// FinalizeCommand contains only immutable execution facts and the two report
// artifacts derived from the admitted hypothesis set. The finalizer resolves
// the execution binding and hypothesis evidence from the authoritative
// ledgers instead of accepting caller-built projections.
type FinalizeCommand struct {
	Initialized        InitializedRun
	Subject            application.AgentPlanningSubject
	Terminal           application.FormalAgentReviewTerminal
	CandidateSet       *runmodel.ArtifactRef
	VerificationLedger *runmodel.ArtifactRef
	CalibrationLedger  *runmodel.ArtifactRef
	SuppressionLedger  *runmodel.ArtifactRef
	GovernedReport     *runmodel.ArtifactRef
	GovernedMarkdown   *runmodel.ArtifactRef
}

type FinalizedRun struct {
	Run       runmodel.ReviewRun
	Ref       runmodel.ArtifactRef
	Recovered bool
}

type Finalizer struct {
	runs *runrepo.Repository
}

type formalAttemptProjection struct {
	Intent  runmodel.AgentStageDispatchIntent
	Binding runmodel.AgentStageExecutionBinding
	Gate    runmodel.AgentStageTerminalGate
	Attempt runmodel.StageAttempt
	Failure *runmodel.Failure
}

func NewFinalizer(runs *runrepo.Repository) (*Finalizer, error) {
	if runs == nil {
		return nil, fmt.Errorf("run repository is required")
	}
	return &Finalizer{runs: runs}, nil
}

// Finalize projects the formal agent ledgers into the same immutable
// ReviewRun terminal consumed by history and downstream platform features.
// Every append uses a deterministic ID, so recovery after any partial write is
// exact-idempotent. FinalizeRun performs the authoritative whole-closure check.
func (finalizer *Finalizer) Finalize(
	ctx context.Context,
	command FinalizeCommand,
) (FinalizedRun, error) {
	if ctx == nil {
		return FinalizedRun{}, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return FinalizedRun{}, err
	}
	if finalizer == nil || finalizer.runs == nil {
		return FinalizedRun{}, fmt.Errorf("formal review finalizer is not initialized")
	}
	if command.Initialized.RunID == "" || command.Initialized.SourceRunID == "" ||
		command.Initialized.Snapshot.ExecutionSnapshotID == "" {
		return FinalizedRun{}, fmt.Errorf("initialized formal run is required")
	}
	if command.Terminal.Result.ExecutionID == "" || command.Terminal.Gate.GateID == "" {
		return FinalizedRun{}, fmt.Errorf("admitted formal terminal is required")
	}
	subject := runmodel.AgentPlanningSubject{
		TenantID: command.Subject.TenantID, OrganizationID: command.Subject.OrganizationID,
		WorkspaceID: command.Subject.WorkspaceID, RepositoryID: command.Subject.RepositoryID,
	}
	gate := command.Terminal.Gate
	if gate.ReviewRunID != command.Initialized.RunID ||
		gate.Subject != subject || gate.ExecutionID != command.Terminal.Result.ExecutionID ||
		!formalTerminalKindMatchesStatus(gate.Kind, command.Terminal.Result.Status) {
		return FinalizedRun{}, fmt.Errorf("formal terminal escaped initialized run, subject, or status")
	}
	if command.Terminal.Result.Status == contractsv1alpha1.StageExecutionSucceeded {
		if command.Terminal.Hypotheses == nil || command.CandidateSet == nil ||
			command.VerificationLedger == nil ||
			command.CalibrationLedger == nil || command.SuppressionLedger == nil ||
			command.GovernedReport == nil ||
			command.GovernedMarkdown == nil {
			return FinalizedRun{}, fmt.Errorf("succeeded formal run requires admitted hypotheses, candidates, and governed reports")
		}
	} else if command.Terminal.Hypotheses != nil || command.CandidateSet != nil ||
		command.VerificationLedger != nil ||
		command.CalibrationLedger != nil || command.SuppressionLedger != nil ||
		command.GovernedReport != nil ||
		command.GovernedMarkdown != nil {
		return FinalizedRun{}, fmt.Errorf("non-succeeded formal run cannot contain hypotheses, candidates, or governed reports")
	}
	if existing, err := finalizer.runs.LoadRun(command.Initialized.RunID); err == nil {
		return finalizer.recoverExisting(existing, command)
	} else if !errors.Is(err, os.ErrNotExist) {
		return FinalizedRun{}, fmt.Errorf("load existing formal ReviewRun: %w", err)
	}

	history, err := finalizer.loadFormalAttemptHistory(command, subject)
	if err != nil {
		return FinalizedRun{}, err
	}
	finalAttempt := history[len(history)-1]
	binding := finalAttempt.Binding

	source, err := finalizer.runs.LoadRun(command.Initialized.SourceRunID)
	if err != nil {
		return FinalizedRun{}, fmt.Errorf("load formal source run: %w", err)
	}
	var target targetmodel.MaterializedTarget
	if err := finalizer.runs.ReadJSONArtifact(
		command.Initialized.Snapshot.TargetSnapshotRef, &target,
	); err != nil {
		return FinalizedRun{}, fmt.Errorf("read formal materialized target: %w", err)
	}
	startedAt := command.Initialized.Snapshot.CreatedAt
	finishedAt := gate.EventTime()
	if finishedAt.IsZero() {
		return FinalizedRun{}, fmt.Errorf("formal terminal has no event time")
	}
	platformBinding := runrepo.FormalPlatformExecutionBinding(binding)
	attempt := finalAttempt.Attempt
	runKind := command.Initialized.Kind
	if runKind == "" {
		runKind = runmodel.RunKindReview
	}
	run := runmodel.ReviewRun{
		SchemaVersion: runmodel.RunSchemaVersion,
		RunID:         command.Initialized.RunID, Kind: runKind,
		RepositoryPath: source.RepositoryPath, TargetMode: source.TargetMode,
		BaseRevision: source.BaseRevision, HeadRevision: source.HeadRevision,
		ExecutionSnapshotID: command.Initialized.Snapshot.ExecutionSnapshotID,
		TargetSnapshotRef:   command.Initialized.Snapshot.TargetSnapshotRef,
		StageAttempts:       make([]runmodel.StageAttempt, 0, len(history)),
		Bindings:            make([]runmodel.PlatformExecutionBinding, 0, len(history)),
		Evidence:            []runmodel.RunEvidence{},
		CreatedAt:           command.Initialized.Snapshot.CreatedAt, StartedAt: &startedAt,
	}
	if runKind == runmodel.RunKindReplay {
		run.SourceRunID = command.Initialized.SourceRunID
		run.ReplayFromStage = command.Initialized.ReplayFromStage
		run.ReplayRootRunID = command.Initialized.ReplayRootRunID
		run.ReplayNamespace = command.Initialized.ReplayNamespace
		run.ReplayVariable = command.Initialized.ReplayVariable
	}

	completedAt := finishedAt
	var stageArtifact *runmodel.ArtifactRef
	switch command.Terminal.Result.Status {
	case contractsv1alpha1.StageExecutionSucceeded:
		if gate.Completion == nil {
			return FinalizedRun{}, fmt.Errorf("succeeded formal run requires admitted hypotheses and governed reports")
		}
		evidenceFact, found, lookupErr := finalizer.runs.LookupAgentStageHypothesisEvidence(
			command.Initialized.RunID, binding.BindingID, subject,
		)
		if lookupErr != nil || !found {
			if lookupErr == nil {
				lookupErr = fmt.Errorf("hypothesis evidence does not exist")
			}
			return FinalizedRun{}, fmt.Errorf("resolve formal hypothesis evidence: %w", lookupErr)
		}
		completeness, reasons := formalTargetCompleteness(target)
		evidence := runmodel.RunEvidence{
			EvidenceID: evidenceFact.EvidenceID, BindingID: binding.BindingID,
			StageID: gate.Stage.ID, Attempt: gate.Attempt, Generation: gate.Generation,
			IdempotencyKey: binding.CreateIdempotencyKey,
			FencingToken:   binding.FencingToken,
			ArtifactRef:    evidenceFact.Output.Local, Completeness: completeness,
			CompletenessNotes: reasons, RecordedAt: evidenceFact.AdmittedAt,
		}
		attempt.Status = runmodel.StageStatusSucceeded
		attempt.OutputRef = &evidenceFact.Output.Local
		run.Status = runmodel.RunStatusSucceeded
		run.Evidence = []runmodel.RunEvidence{evidence}
		run.HypothesisSetRef = &evidenceFact.Output.Local
		if command.Terminal.Result.AgentRawCandidates != nil {
			ref := localArtifactRefFromBinding(*command.Terminal.Result.AgentRawCandidates)
			run.RawCandidateCollectionRef = &ref
		}
		if command.Terminal.Result.AgentTaskEvidence != nil {
			ref := localArtifactRefFromBinding(*command.Terminal.Result.AgentTaskEvidence)
			run.AgentTaskEvidenceRef = &ref
		}
		if command.Terminal.Result.AgentExecutionReceipts != nil {
			ref := localArtifactRefFromBinding(*command.Terminal.Result.AgentExecutionReceipts)
			run.AgentExecutionReceiptRef = &ref
		}
		run.GovernedReportRef = command.GovernedReport
		run.CandidateSetRef = command.CandidateSet
		run.VerificationLedgerRef = command.VerificationLedger
		run.CalibrationLedgerRef = command.CalibrationLedger
		run.SuppressionLedgerRef = command.SuppressionLedger
		run.GovernedMarkdownRef = command.GovernedMarkdown
		stageArtifact = &evidenceFact.Output.Local
		if evidenceFact.AdmittedAt.After(completedAt) {
			completedAt = evidenceFact.AdmittedAt
		}
	case contractsv1alpha1.StageExecutionFailed, contractsv1alpha1.StageExecutionCanceled:
		if command.Terminal.Result.Failure == nil {
			return FinalizedRun{}, fmt.Errorf("non-succeeded formal run has invalid failure or report projection")
		}
		failure := command.Terminal.Result.Failure
		attempt.ErrorCode = failure.Code
		attempt.ErrorMessage = failure.Message
		attempt.Retryable = failure.Retryable
		run.Failure = &runmodel.Failure{
			Code: failure.Code, Message: failure.Message,
			StageID: gate.Stage.ID, Retryable: failure.Retryable,
		}
		if command.Terminal.Result.Status == contractsv1alpha1.StageExecutionCanceled {
			attempt.Status = runmodel.StageStatusCanceled
			run.Status = runmodel.RunStatusCanceled
		} else {
			attempt.Status = runmodel.StageStatusFailed
			run.Status = runmodel.RunStatusFailed
		}
	default:
		return FinalizedRun{}, fmt.Errorf(
			"unsupported formal terminal status %q", command.Terminal.Result.Status,
		)
	}
	for _, item := range history[:len(history)-1] {
		run.StageAttempts = append(run.StageAttempts, item.Attempt)
		run.Bindings = append(run.Bindings, runrepo.FormalPlatformExecutionBinding(item.Binding))
	}
	run.StageAttempts = append(run.StageAttempts, attempt)
	run.Bindings = append(run.Bindings, platformBinding)
	run.CompletedAt = &completedAt

	for index, item := range history {
		artifact := (*runmodel.ArtifactRef)(nil)
		projection := run
		projection.Evidence = []runmodel.RunEvidence{}
		projection.Failure = item.Failure
		if index == len(history)-1 {
			artifact = stageArtifact
			projection.Evidence = run.Evidence
		}
		if err := finalizer.appendRunFacts(
			projection, projection.StageAttempts[index], projection.Bindings[index], artifact,
		); err != nil {
			return FinalizedRun{}, err
		}
	}
	ref, err := finalizer.runs.FinalizeRun(
		runrepo.TerminalEventID(run.RunID), completedAt, run,
	)
	if err != nil {
		return FinalizedRun{}, fmt.Errorf("commit formal ReviewRun terminal: %w", err)
	}
	return FinalizedRun{Run: run, Ref: ref}, nil
}

func (finalizer *Finalizer) loadFormalAttemptHistory(
	command FinalizeCommand,
	subject runmodel.AgentPlanningSubject,
) ([]formalAttemptProjection, error) {
	intents, err := finalizer.runs.ListClaimedAgentStageDispatchIntents(
		command.Initialized.RunID, subject,
	)
	if err != nil {
		return nil, fmt.Errorf("list formal dispatch history: %w", err)
	}
	history := make([]formalAttemptProjection, 0, len(intents))
	unresolved := make([]runmodel.AgentStageDispatchIntent, 0)
	for _, intent := range intents {
		if intent.Stage.ID != command.Terminal.Gate.Stage.ID {
			continue
		}
		binding, found, lookupErr := finalizer.runs.LookupAgentStageExecutionBinding(
			command.Initialized.RunID, intent.IntentID, subject,
		)
		if lookupErr != nil {
			return nil, fmt.Errorf("resolve formal execution binding: %w", lookupErr)
		}
		if !found {
			unresolved = append(unresolved, intent)
			continue
		}
		gate, found, lookupErr := finalizer.runs.LookupAgentStageTerminalGate(
			command.Initialized.RunID, intent.IntentID, subject,
		)
		if lookupErr != nil {
			return nil, fmt.Errorf("resolve formal attempt terminal: %w", lookupErr)
		}
		if !found {
			unresolved = append(unresolved, intent)
			continue
		}
		var resultRef runmodel.ArtifactRef
		switch gate.Kind {
		case runmodel.AgentStageSucceededResultAccepted:
			resultRef = gate.Completion.ResultRef
		case runmodel.AgentStageFailedResultAccepted, runmodel.AgentStageCanceledResultAccepted:
			resultRef = gate.Outcome.ResultRef
		default:
			return nil, fmt.Errorf("formal attempt cannot finalize host-only cancellation")
		}
		resultBytes, readErr := finalizer.runs.ReadArtifact(resultRef)
		if readErr != nil {
			return nil, fmt.Errorf("read formal attempt result: %w", readErr)
		}
		result, decodeErr := contractsv1alpha1.DecodeStageExecutionResult(resultBytes)
		if decodeErr != nil {
			return nil, fmt.Errorf("decode formal attempt result: %w", decodeErr)
		}
		finishedAt := gate.EventTime()
		attempt := runmodel.StageAttempt{
			StageID: gate.Stage.ID, Attempt: gate.Attempt, Generation: gate.Generation,
			BindingID: binding.BindingID,
			InputRefs: []runmodel.ArtifactRef{command.Initialized.Snapshot.ReviewInputRef},
			StartedAt: intent.RecordedAt, FinishedAt: &finishedAt,
			DurationMS: nonNegativeDurationMS(intent.RecordedAt, finishedAt),
		}
		var failure *runmodel.Failure
		switch result.Status {
		case contractsv1alpha1.StageExecutionSucceeded:
			attempt.Status = runmodel.StageStatusSucceeded
		case contractsv1alpha1.StageExecutionFailed, contractsv1alpha1.StageExecutionCanceled:
			if result.Failure == nil {
				return nil, fmt.Errorf("formal non-succeeded attempt has no failure")
			}
			attempt.Status = runmodel.StageStatusFailed
			if result.Status == contractsv1alpha1.StageExecutionCanceled {
				attempt.Status = runmodel.StageStatusCanceled
			}
			attempt.ErrorCode = result.Failure.Code
			attempt.ErrorMessage = result.Failure.Message
			attempt.Retryable = result.Failure.Retryable
			failure = &runmodel.Failure{
				Code: result.Failure.Code, Message: result.Failure.Message,
				StageID: gate.Stage.ID, Retryable: result.Failure.Retryable,
			}
		default:
			return nil, fmt.Errorf("unsupported formal attempt status %q", result.Status)
		}
		history = append(history, formalAttemptProjection{
			Intent: intent, Binding: binding, Gate: gate, Attempt: attempt, Failure: failure,
		})
	}
	sort.Slice(history, func(left, right int) bool {
		if history[left].Attempt.Attempt != history[right].Attempt.Attempt {
			return history[left].Attempt.Attempt < history[right].Attempt.Attempt
		}
		return history[left].Attempt.Generation < history[right].Attempt.Generation
	})
	for _, intent := range unresolved {
		superseded := false
		for _, closed := range history {
			if closed.Attempt.Attempt == intent.Attempt &&
				closed.Attempt.Generation > intent.Generation {
				superseded = true
				break
			}
		}
		if !superseded {
			return nil, fmt.Errorf("formal dispatch history contains an unresolved current attempt")
		}
	}
	if len(history) == 0 || history[len(history)-1].Gate.GateID != command.Terminal.Gate.GateID {
		return nil, fmt.Errorf("formal terminal is not the latest closed attempt")
	}
	for index, item := range history {
		if item.Attempt.Attempt != index+1 {
			return nil, fmt.Errorf("formal attempt history is not contiguous")
		}
		if index < len(history)-1 &&
			(item.Attempt.Status != runmodel.StageStatusFailed || !item.Attempt.Retryable) {
			return nil, fmt.Errorf("formal attempt history advanced after a non-retryable terminal")
		}
	}
	return history, nil
}

func (finalizer *Finalizer) recoverExisting(
	run runmodel.ReviewRun,
	command FinalizeCommand,
) (FinalizedRun, error) {
	if run.ExecutionSnapshotID != command.Initialized.Snapshot.ExecutionSnapshotID {
		return FinalizedRun{}, fmt.Errorf("existing formal ReviewRun changed execution snapshot")
	}
	wantKind := command.Initialized.Kind
	if wantKind == "" {
		wantKind = runmodel.RunKindReview
	}
	if run.Kind != wantKind ||
		(run.Kind == runmodel.RunKindReplay &&
			(run.SourceRunID != command.Initialized.SourceRunID ||
				run.ReplayFromStage != command.Initialized.ReplayFromStage ||
				run.ReplayRootRunID != command.Initialized.ReplayRootRunID ||
				run.ReplayNamespace != command.Initialized.ReplayNamespace ||
				run.ReplayVariable != command.Initialized.ReplayVariable)) {
		return FinalizedRun{}, fmt.Errorf("existing formal ReviewRun changed replay lineage")
	}
	if run.Status != runStatusForFormalResult(command.Terminal.Result.Status) {
		return FinalizedRun{}, fmt.Errorf("existing formal ReviewRun changed terminal status")
	}
	if command.Terminal.Result.Status == contractsv1alpha1.StageExecutionSucceeded {
		if command.CandidateSet == nil || command.VerificationLedger == nil ||
			command.CalibrationLedger == nil || command.SuppressionLedger == nil ||
			command.GovernedReport == nil ||
			command.GovernedMarkdown == nil ||
			run.CandidateSetRef == nil || *run.CandidateSetRef != *command.CandidateSet ||
			run.VerificationLedgerRef == nil || *run.VerificationLedgerRef != *command.VerificationLedger ||
			run.CalibrationLedgerRef == nil || *run.CalibrationLedgerRef != *command.CalibrationLedger ||
			run.SuppressionLedgerRef == nil || *run.SuppressionLedgerRef != *command.SuppressionLedger ||
			run.GovernedReportRef == nil || *run.GovernedReportRef != *command.GovernedReport ||
			run.GovernedMarkdownRef == nil || *run.GovernedMarkdownRef != *command.GovernedMarkdown {
			return FinalizedRun{}, fmt.Errorf("existing formal ReviewRun changed candidates or governed reports")
		}
		var expected *runmodel.ArtifactRef
		if command.Terminal.Result.AgentRawCandidates != nil {
			ref := localArtifactRefFromBinding(*command.Terminal.Result.AgentRawCandidates)
			expected = &ref
		}
		if !equalOptionalArtifactRef(run.RawCandidateCollectionRef, expected) {
			return FinalizedRun{}, fmt.Errorf("existing formal ReviewRun changed raw candidate evidence")
		}
		expected = nil
		if command.Terminal.Result.AgentTaskEvidence != nil {
			ref := localArtifactRefFromBinding(*command.Terminal.Result.AgentTaskEvidence)
			expected = &ref
		}
		if !equalOptionalArtifactRef(run.AgentTaskEvidenceRef, expected) {
			return FinalizedRun{}, fmt.Errorf("existing formal ReviewRun changed agent task evidence")
		}
		expected = nil
		if command.Terminal.Result.AgentExecutionReceipts != nil {
			ref := localArtifactRefFromBinding(*command.Terminal.Result.AgentExecutionReceipts)
			expected = &ref
		}
		if !equalOptionalArtifactRef(run.AgentExecutionReceiptRef, expected) {
			return FinalizedRun{}, fmt.Errorf("existing formal ReviewRun changed agent execution receipts")
		}
	}
	ref, err := finalizer.runs.CommittedRunRef(run.RunID)
	if err != nil {
		return FinalizedRun{}, err
	}
	return FinalizedRun{Run: run, Ref: ref, Recovered: true}, nil
}

func localArtifactRefFromBinding(binding contractsv1alpha1.ArtifactBinding) runmodel.ArtifactRef {
	return runmodel.ArtifactRef{
		URI:    "artifact://local/sha256/" + binding.Ref.SHA256,
		SHA256: binding.Ref.SHA256, SizeBytes: binding.Ref.SizeBytes,
		Contract: binding.Contract,
	}
}

func equalOptionalArtifactRef(left, right *runmodel.ArtifactRef) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func (finalizer *Finalizer) appendRunFacts(
	run runmodel.ReviewRun,
	attempt runmodel.StageAttempt,
	binding runmodel.PlatformExecutionBinding,
	artifact *runmodel.ArtifactRef,
) error {
	prefix := fmt.Sprintf("%s-%s-%d-%d", run.RunID, attempt.StageID, attempt.Attempt, attempt.Generation)
	if err := finalizer.runs.AppendEvent(prefix+"-started", attempt.StartedAt, runrepo.RunEvent{
		RunID: run.RunID, Kind: run.Kind, Status: runmodel.RunStatusRunning,
		EventType: runrepo.EventStageStarted, ExecutionSnapshotID: run.ExecutionSnapshotID,
		StageID: attempt.StageID, Attempt: attempt.Attempt, Generation: attempt.Generation,
	}); err != nil {
		return fmt.Errorf("append formal stage.started: %w", err)
	}
	if err := finalizer.runs.AppendEvent(prefix+"-binding", binding.CreatedAt, runrepo.RunEvent{
		RunID: run.RunID, Kind: run.Kind, Status: runmodel.RunStatusRunning,
		EventType: runrepo.EventBindingRecorded, ExecutionSnapshotID: run.ExecutionSnapshotID,
		StageID: attempt.StageID, Attempt: attempt.Attempt, Generation: attempt.Generation,
		Binding: &binding,
	}); err != nil {
		return fmt.Errorf("append formal binding.recorded: %w", err)
	}
	eventType := runrepo.EventStageSucceeded
	if attempt.Status == runmodel.StageStatusFailed {
		eventType = runrepo.EventStageFailed
	} else if attempt.Status == runmodel.StageStatusCanceled {
		eventType = runrepo.EventStageCanceled
	}
	if err := finalizer.runs.AppendEvent(prefix+"-"+string(attempt.Status), *attempt.FinishedAt, runrepo.RunEvent{
		RunID: run.RunID, Kind: run.Kind, Status: runmodel.RunStatusRunning,
		EventType: eventType, ExecutionSnapshotID: run.ExecutionSnapshotID,
		StageID: attempt.StageID, Attempt: attempt.Attempt, Generation: attempt.Generation,
		Artifact: artifact, Failure: run.Failure,
	}); err != nil {
		return fmt.Errorf("append formal stage terminal: %w", err)
	}
	if len(run.Evidence) == 1 {
		evidence := run.Evidence[0]
		if err := finalizer.runs.AppendEvent(prefix+"-evidence", evidence.RecordedAt, runrepo.RunEvent{
			RunID: run.RunID, Kind: run.Kind, Status: runmodel.RunStatusRunning,
			EventType: runrepo.EventEvidenceRecorded, ExecutionSnapshotID: run.ExecutionSnapshotID,
			StageID: attempt.StageID, Attempt: attempt.Attempt, Generation: attempt.Generation,
			Artifact: &evidence.ArtifactRef, Evidence: &evidence,
		}); err != nil {
			return fmt.Errorf("append formal evidence.recorded: %w", err)
		}
	}
	return nil
}

func formalTargetCompleteness(
	target targetmodel.MaterializedTarget,
) (runmodel.Completeness, []string) {
	if target.Snapshot.Completeness == gitadapter.CompletenessComplete {
		return runmodel.CompletenessComplete, []string{}
	}
	reasons := make([]string, 0, len(target.Snapshot.CompletenessReason))
	for _, reason := range target.Snapshot.CompletenessReason {
		value := string(reason.Code)
		if reason.Detail != "" {
			value += ": " + reason.Detail
		}
		reasons = append(reasons, value)
	}
	return runmodel.CompletenessPartial, slices.Clone(reasons)
}

func nonNegativeDurationMS(start time.Time, finish time.Time) int64 {
	if !finish.After(start) {
		return 0
	}
	return finish.Sub(start).Milliseconds()
}

func runStatusForFormalResult(
	status contractsv1alpha1.StageExecutionStatus,
) runmodel.RunStatus {
	switch status {
	case contractsv1alpha1.StageExecutionSucceeded:
		return runmodel.RunStatusSucceeded
	case contractsv1alpha1.StageExecutionCanceled:
		return runmodel.RunStatusCanceled
	case contractsv1alpha1.StageExecutionFailed:
		return runmodel.RunStatusFailed
	default:
		return ""
	}
}

func formalTerminalKindMatchesStatus(
	kind runmodel.AgentStageTerminalGateKind,
	status contractsv1alpha1.StageExecutionStatus,
) bool {
	switch status {
	case contractsv1alpha1.StageExecutionSucceeded:
		return kind == runmodel.AgentStageSucceededResultAccepted
	case contractsv1alpha1.StageExecutionFailed:
		return kind == runmodel.AgentStageFailedResultAccepted
	case contractsv1alpha1.StageExecutionCanceled:
		return kind == runmodel.AgentStageCanceledResultAccepted
	default:
		return false
	}
}
