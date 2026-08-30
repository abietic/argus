package runrepo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"

	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/targetmodel"
	"argus.local/argus/internal/workflow"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

func isFormalAgentWorkflow(definition workflow.Definition) bool {
	return definition.ID == "formal-pi-agent-review" && len(definition.Stages) == 1 &&
		definition.Stages[0].ID == "agent_hypothesize" &&
		definition.Stages[0].Kind == "agent_hypothesize"
}

// FormalAgentTaskEvidenceBinding resolves the governed source of the exact
// host-localized task evidence from authoritative formal terminal facts. It
// first reloads the complete ReviewRun closure, so callers cannot substitute a
// run ID, subject, terminal result, or local evidence ref.
func (repository *Repository) FormalAgentTaskEvidenceBinding(
	runID string,
) (contractsv1alpha1.ArtifactBinding, runmodel.AgentPlanningSubject, bool, error) {
	run, err := repository.LoadRun(runID)
	if err != nil {
		return contractsv1alpha1.ArtifactBinding{}, runmodel.AgentPlanningSubject{}, false, err
	}
	if len(run.StageAttempts) == 0 || run.StageAttempts[len(run.StageAttempts)-1].StageID != "agent_hypothesize" {
		return contractsv1alpha1.ArtifactBinding{}, runmodel.AgentPlanningSubject{}, false,
			fmt.Errorf("formal task evidence requires agent_hypothesize attempt history")
	}
	snapshot, err := repository.ExecutionSnapshotForRun(runID)
	if err != nil {
		return contractsv1alpha1.ArtifactBinding{}, runmodel.AgentPlanningSubject{}, false, err
	}
	specBytes, err := repository.ReadArtifact(snapshot.ReviewSpecRef)
	if err != nil {
		return contractsv1alpha1.ArtifactBinding{}, runmodel.AgentPlanningSubject{}, false,
			fmt.Errorf("read formal task evidence ReviewSpec: %w", err)
	}
	spec, err := contractsv1alpha1.DecodeReviewSpec(specBytes)
	if err != nil {
		return contractsv1alpha1.ArtifactBinding{}, runmodel.AgentPlanningSubject{}, false,
			fmt.Errorf("decode formal task evidence ReviewSpec: %w", err)
	}
	subject := runmodel.AgentPlanningSubject{
		TenantID: spec.TenantID, OrganizationID: "local",
		WorkspaceID: spec.WorkspaceID, RepositoryID: spec.Repository.RepositoryID,
	}
	attempt := run.StageAttempts[len(run.StageAttempts)-1]
	intent, found, err := repository.LookupAgentStageDispatchIntent(
		runID, attempt.StageID, attempt.Attempt, attempt.Generation, subject,
	)
	if err != nil || !found {
		if err == nil {
			err = fmt.Errorf("dispatch intent does not exist")
		}
		return contractsv1alpha1.ArtifactBinding{}, runmodel.AgentPlanningSubject{}, false,
			fmt.Errorf("resolve formal task evidence dispatch: %w", err)
	}
	gate, found, err := repository.LookupAgentStageTerminalGate(runID, intent.IntentID, subject)
	if err != nil || !found {
		if err == nil {
			err = fmt.Errorf("terminal gate does not exist")
		}
		return contractsv1alpha1.ArtifactBinding{}, runmodel.AgentPlanningSubject{}, false,
			fmt.Errorf("resolve formal task evidence terminal: %w", err)
	}
	resultRef, diagnosticTaskEvidence, err := formalAgentEvidenceResultRef(gate)
	if err != nil {
		return contractsv1alpha1.ArtifactBinding{}, runmodel.AgentPlanningSubject{}, false, err
	}
	result, err := repository.readCanonicalAgentStageExecutionResult(resultRef)
	if err != nil {
		return contractsv1alpha1.ArtifactBinding{}, runmodel.AgentPlanningSubject{}, false,
			fmt.Errorf("read formal task evidence terminal result: %w", err)
	}
	if result.AgentTaskEvidence == nil {
		return contractsv1alpha1.ArtifactBinding{}, subject, false, nil
	}
	wantLocal := runmodel.ArtifactRef{
		URI:       "artifact://local/sha256/" + result.AgentTaskEvidence.Ref.SHA256,
		SHA256:    result.AgentTaskEvidence.Ref.SHA256,
		SizeBytes: result.AgentTaskEvidence.Ref.SizeBytes,
		Contract:  result.AgentTaskEvidence.Contract,
	}
	if diagnosticTaskEvidence != nil {
		if run.AgentTaskEvidenceRef != nil ||
			!sameFormalGovernedBinding(diagnosticTaskEvidence.Governed, *result.AgentTaskEvidence) ||
			diagnosticTaskEvidence.Local != wantLocal {
			return contractsv1alpha1.ArtifactBinding{}, runmodel.AgentPlanningSubject{}, false,
				fmt.Errorf("formal failure diagnostic task evidence escaped its terminal gate")
		}
		return *result.AgentTaskEvidence, subject, true, nil
	}
	if run.AgentTaskEvidenceRef == nil || *run.AgentTaskEvidenceRef != wantLocal {
		return contractsv1alpha1.ArtifactBinding{}, runmodel.AgentPlanningSubject{}, false,
			fmt.Errorf("formal task evidence governed source does not bind localized ref")
	}
	return *result.AgentTaskEvidence, subject, true, nil
}

// FormalAgentExecutionReceiptBinding resolves the governed source of the exact
// host-localized receipt collection from the authoritative terminal closure.
func (repository *Repository) FormalAgentExecutionReceiptBinding(
	runID string,
) (contractsv1alpha1.ArtifactBinding, runmodel.AgentPlanningSubject, bool, error) {
	run, err := repository.LoadRun(runID)
	if err != nil {
		return contractsv1alpha1.ArtifactBinding{}, runmodel.AgentPlanningSubject{}, false, err
	}
	if len(run.StageAttempts) == 0 || run.StageAttempts[len(run.StageAttempts)-1].StageID != "agent_hypothesize" {
		return contractsv1alpha1.ArtifactBinding{}, runmodel.AgentPlanningSubject{}, false,
			fmt.Errorf("formal execution receipts require agent_hypothesize attempt history")
	}
	snapshot, err := repository.ExecutionSnapshotForRun(runID)
	if err != nil {
		return contractsv1alpha1.ArtifactBinding{}, runmodel.AgentPlanningSubject{}, false, err
	}
	specBytes, err := repository.ReadArtifact(snapshot.ReviewSpecRef)
	if err != nil {
		return contractsv1alpha1.ArtifactBinding{}, runmodel.AgentPlanningSubject{}, false,
			fmt.Errorf("read formal execution receipt ReviewSpec: %w", err)
	}
	spec, err := contractsv1alpha1.DecodeReviewSpec(specBytes)
	if err != nil {
		return contractsv1alpha1.ArtifactBinding{}, runmodel.AgentPlanningSubject{}, false,
			fmt.Errorf("decode formal execution receipt ReviewSpec: %w", err)
	}
	subject := runmodel.AgentPlanningSubject{
		TenantID: spec.TenantID, OrganizationID: "local",
		WorkspaceID: spec.WorkspaceID, RepositoryID: spec.Repository.RepositoryID,
	}
	attempt := run.StageAttempts[len(run.StageAttempts)-1]
	intent, found, err := repository.LookupAgentStageDispatchIntent(
		runID, attempt.StageID, attempt.Attempt, attempt.Generation, subject,
	)
	if err != nil || !found {
		if err == nil {
			err = fmt.Errorf("dispatch intent does not exist")
		}
		return contractsv1alpha1.ArtifactBinding{}, runmodel.AgentPlanningSubject{}, false,
			fmt.Errorf("resolve formal execution receipt dispatch: %w", err)
	}
	gate, found, err := repository.LookupAgentStageTerminalGate(runID, intent.IntentID, subject)
	if err != nil || !found {
		if err == nil {
			err = fmt.Errorf("terminal gate does not exist")
		}
		return contractsv1alpha1.ArtifactBinding{}, runmodel.AgentPlanningSubject{}, false,
			fmt.Errorf("resolve formal execution receipt terminal: %w", err)
	}
	resultRef, _, diagnosticReceipts, err := formalAgentEvidenceResultRefs(gate)
	if err != nil {
		return contractsv1alpha1.ArtifactBinding{}, runmodel.AgentPlanningSubject{}, false, err
	}
	result, err := repository.readCanonicalAgentStageExecutionResult(resultRef)
	if err != nil {
		return contractsv1alpha1.ArtifactBinding{}, runmodel.AgentPlanningSubject{}, false,
			fmt.Errorf("read formal execution receipt terminal result: %w", err)
	}
	if result.AgentExecutionReceipts == nil {
		return contractsv1alpha1.ArtifactBinding{}, subject, false, nil
	}
	wantLocal := runmodel.ArtifactRef{
		URI:       "artifact://local/sha256/" + result.AgentExecutionReceipts.Ref.SHA256,
		SHA256:    result.AgentExecutionReceipts.Ref.SHA256,
		SizeBytes: result.AgentExecutionReceipts.Ref.SizeBytes,
		Contract:  result.AgentExecutionReceipts.Contract,
	}
	if diagnosticReceipts != nil {
		if run.AgentExecutionReceiptRef != nil ||
			!sameFormalGovernedBinding(diagnosticReceipts.Governed, *result.AgentExecutionReceipts) ||
			diagnosticReceipts.Local != wantLocal {
			return contractsv1alpha1.ArtifactBinding{}, runmodel.AgentPlanningSubject{}, false,
				fmt.Errorf("formal failure diagnostic receipts escaped their terminal gate")
		}
		return *result.AgentExecutionReceipts, subject, true, nil
	}
	if run.AgentExecutionReceiptRef == nil || *run.AgentExecutionReceiptRef != wantLocal {
		return contractsv1alpha1.ArtifactBinding{}, runmodel.AgentPlanningSubject{}, false,
			fmt.Errorf("formal execution receipt governed source does not bind localized ref")
	}
	return *result.AgentExecutionReceipts, subject, true, nil
}

func sameFormalGovernedBinding(
	projection runmodel.GovernedArtifactBinding,
	binding contractsv1alpha1.ArtifactBinding,
) bool {
	return projection.URI == binding.Ref.URI &&
		projection.SHA256 == binding.Ref.SHA256 &&
		projection.SizeBytes == binding.Ref.SizeBytes &&
		projection.Contract == binding.Contract
}

func formalAgentEvidenceResultRef(
	gate runmodel.AgentStageTerminalGate,
) (runmodel.ArtifactRef, *runmodel.AgentArtifactProjection, error) {
	result, taskEvidence, _, err := formalAgentEvidenceResultRefs(gate)
	return result, taskEvidence, err
}

func formalAgentEvidenceResultRefs(
	gate runmodel.AgentStageTerminalGate,
) (
	runmodel.ArtifactRef,
	*runmodel.AgentArtifactProjection,
	*runmodel.AgentArtifactProjection,
	error,
) {
	switch gate.Kind {
	case runmodel.AgentStageSucceededResultAccepted:
		if gate.Completion == nil {
			return runmodel.ArtifactRef{}, nil, nil,
				fmt.Errorf("succeeded formal evidence terminal has no completion")
		}
		return gate.Completion.ResultRef, nil, nil, nil
	case runmodel.AgentStageFailedResultAccepted, runmodel.AgentStageCanceledResultAccepted:
		if gate.Outcome == nil {
			return runmodel.ArtifactRef{}, nil, nil,
				fmt.Errorf("non-succeeded formal evidence terminal has no outcome")
		}
		return gate.Outcome.ResultRef,
			gate.Outcome.AgentTaskEvidence,
			gate.Outcome.AgentExecutionReceipts,
			nil
	default:
		return runmodel.ArtifactRef{}, nil, nil,
			fmt.Errorf("formal evidence is unavailable for terminal kind %q", gate.Kind)
	}
}

func (repository *Repository) verifyFormalAgentRunClosure(
	run runmodel.ReviewRun,
	snapshot runmodel.ExecutionSnapshot,
	spec contractsv1alpha1.ReviewSpec,
	target targetmodel.MaterializedTarget,
	input reviewcore.ReviewInput,
	targetDigest string,
	config reviewconfig.ConfigBundle,
	ancestors map[string]struct{},
) error {
	if run.Kind == runmodel.RunKindReplay &&
		run.ReplayVariable.IsFilterPolicy() {
		return repository.verifyFormalFindingGovernanceReplayClosure(
			run, snapshot, spec, config, ancestors,
		)
	}
	if len(snapshot.ReplayInputRefs) != 0 {
		return fmt.Errorf("single-stage formal agent run cannot reuse an upstream checkpoint")
	}
	switch run.Kind {
	case runmodel.RunKindReview:
		if snapshot.ReplaySourceRunRef != nil || snapshot.ReplayChangeSetRef != nil {
			return fmt.Errorf("formal review run cannot contain replay lineage")
		}
	case runmodel.RunKindReplay:
		validStart := run.ReplayFromStage == "agent_hypothesize" ||
			(run.ReplayVariable == runmodel.ReplayVariableIndex &&
				run.ReplayFromStage == string(reviewcore.StageMaterializeTarget))
		if !validStart ||
			(run.ReplayVariable != runmodel.ReplayVariableNone &&
				run.ReplayVariable != runmodel.ReplayVariableBudget &&
				run.ReplayVariable != runmodel.ReplayVariableModel &&
				run.ReplayVariable != runmodel.ReplayVariablePrompt &&
				run.ReplayVariable != runmodel.ReplayVariableSkillPack &&
				run.ReplayVariable != runmodel.ReplayVariableKnowledgePack &&
				run.ReplayVariable != runmodel.ReplayVariableRulePack &&
				run.ReplayVariable != runmodel.ReplayVariableWorkflow &&
				run.ReplayVariable != runmodel.ReplayVariableIndex) {
			return fmt.Errorf("formal replay must rerun agent_hypothesize with an executable formal variable")
		}
		if err := repository.verifyReplayLineage(
			run, snapshot, spec, config, ancestors,
		); err != nil {
			return fmt.Errorf("verify formal replay lineage: %w", err)
		}
	default:
		return fmt.Errorf("unsupported formal agent run kind %q", run.Kind)
	}
	if len(run.StageAttempts) == 0 || len(run.Bindings) != len(run.StageAttempts) {
		return fmt.Errorf("formal agent run requires exact stage attempt and binding history")
	}
	if len(snapshot.RuntimeEvidenceRefs) != 1 {
		return fmt.Errorf("formal agent run requires one exact runtime file manifest")
	}
	attempt := run.StageAttempts[len(run.StageAttempts)-1]
	if attempt.StageID != "agent_hypothesize" || attempt.Attempt != len(run.StageAttempts) ||
		len(attempt.InputRefs) != 1 ||
		attempt.InputRefs[0] != snapshot.ReviewInputRef {
		return fmt.Errorf("formal agent stage attempt does not bind exact ReviewInput")
	}
	subject := runmodel.AgentPlanningSubject{
		TenantID: spec.TenantID, OrganizationID: "local",
		WorkspaceID: spec.WorkspaceID, RepositoryID: spec.Repository.RepositoryID,
	}
	for index, historicalAttempt := range run.StageAttempts[:len(run.StageAttempts)-1] {
		if historicalAttempt.StageID != "agent_hypothesize" ||
			historicalAttempt.Attempt != index+1 || len(historicalAttempt.InputRefs) != 1 ||
			historicalAttempt.InputRefs[0] != snapshot.ReviewInputRef ||
			historicalAttempt.Status != runmodel.StageStatusFailed ||
			!historicalAttempt.Retryable || historicalAttempt.OutputRef != nil {
			return fmt.Errorf("formal retry history contains a non-retryable or inexact attempt")
		}
		historicalIntent, found, lookupErr := repository.LookupAgentStageDispatchIntent(
			run.RunID, historicalAttempt.StageID, historicalAttempt.Attempt,
			historicalAttempt.Generation, subject,
		)
		if lookupErr != nil || !found {
			if lookupErr == nil {
				lookupErr = fmt.Errorf("dispatch intent does not exist")
			}
			return fmt.Errorf("resolve formal retry dispatch intent: %w", lookupErr)
		}
		historicalBinding, found, lookupErr := repository.LookupAgentStageExecutionBinding(
			run.RunID, historicalIntent.IntentID, subject,
		)
		if lookupErr != nil || !found {
			if lookupErr == nil {
				lookupErr = fmt.Errorf("execution binding does not exist")
			}
			return fmt.Errorf("resolve formal retry execution binding: %w", lookupErr)
		}
		historicalGate, found, lookupErr := repository.LookupAgentStageTerminalGate(
			run.RunID, historicalIntent.IntentID, subject,
		)
		if lookupErr != nil || !found {
			if lookupErr == nil {
				lookupErr = fmt.Errorf("terminal gate does not exist")
			}
			return fmt.Errorf("resolve formal retry terminal gate: %w", lookupErr)
		}
		if historicalGate.Kind != runmodel.AgentStageFailedResultAccepted ||
			historicalGate.Outcome == nil || !historicalGate.Outcome.Failure.Retryable ||
			historicalAttempt.ErrorCode != historicalGate.Outcome.Failure.Code ||
			historicalAttempt.ErrorMessage != historicalGate.Outcome.Failure.Message ||
			historicalAttempt.BindingID != historicalBinding.BindingID ||
			!reflect.DeepEqual(run.Bindings[index], FormalPlatformExecutionBinding(historicalBinding)) ||
			!historicalAttempt.StartedAt.Equal(historicalIntent.RecordedAt) ||
			historicalAttempt.FinishedAt == nil ||
			!historicalAttempt.FinishedAt.Equal(historicalGate.EventTime()) {
			return fmt.Errorf("formal retry history changed authenticated attempt facts")
		}
	}
	intent, found, err := repository.LookupAgentStageDispatchIntent(
		run.RunID, attempt.StageID, attempt.Attempt, attempt.Generation, subject,
	)
	if err != nil || !found {
		if err == nil {
			err = fmt.Errorf("dispatch intent does not exist")
		}
		return fmt.Errorf("resolve formal dispatch intent: %w", err)
	}
	admission, found, err := repository.LookupAgentStagePlanAdmission(
		run.RunID, attempt.StageID, subject,
	)
	if err != nil || !found {
		if err == nil {
			err = fmt.Errorf("plan admission does not exist")
		}
		return fmt.Errorf("resolve formal plan admission: %w", err)
	}
	planBytes, err := repository.ReadArtifact(admission.Plan.Local)
	if err != nil {
		return fmt.Errorf("read admitted formal AgentStagePlan: %w", err)
	}
	plan, err := contractsv1alpha1.DecodeAgentStagePlan(planBytes)
	if err != nil {
		return fmt.Errorf("decode admitted formal AgentStagePlan: %w", err)
	}
	runtimeEvidenceRef := snapshot.RuntimeEvidenceRefs[0]
	if _, err := repository.ReadArtifact(runtimeEvidenceRef); err != nil {
		return fmt.Errorf("read frozen local runtime file manifest: %w", err)
	}
	if runtimeEvidenceRef.SHA256 != plan.Runtime.Ref.SHA256 ||
		snapshot.BuildIdentity != plan.BuildIdentity ||
		intent.AdmissionID != admission.AdmissionID || intent.AdmissionSHA256 != admission.SHA256 {
		return fmt.Errorf("formal runtime evidence does not bind admitted plan and dispatch")
	}
	binding, found, err := repository.LookupAgentStageExecutionBinding(
		run.RunID, intent.IntentID, subject,
	)
	if err != nil || !found {
		if err == nil {
			err = fmt.Errorf("execution binding does not exist")
		}
		return fmt.Errorf("resolve formal execution binding: %w", err)
	}
	gate, found, err := repository.LookupAgentStageTerminalGate(
		run.RunID, intent.IntentID, subject,
	)
	if err != nil || !found {
		if err == nil {
			err = fmt.Errorf("terminal gate does not exist")
		}
		return fmt.Errorf("resolve formal terminal gate: %w", err)
	}
	wantBinding := FormalPlatformExecutionBinding(binding)
	if !reflect.DeepEqual(run.Bindings[len(run.Bindings)-1], wantBinding) ||
		attempt.BindingID != binding.BindingID {
		return fmt.Errorf("formal ReviewRun binding projection changed")
	}
	if !attempt.StartedAt.Equal(intent.RecordedAt) || attempt.FinishedAt == nil ||
		!attempt.FinishedAt.Equal(gate.EventTime()) {
		return fmt.Errorf("formal stage attempt timing does not bind dispatch and terminal facts")
	}

	resultRef := runmodel.ArtifactRef{}
	switch gate.Kind {
	case runmodel.AgentStageSucceededResultAccepted:
		resultRef = gate.Completion.ResultRef
	case runmodel.AgentStageFailedResultAccepted, runmodel.AgentStageCanceledResultAccepted:
		resultRef = gate.Outcome.ResultRef
	default:
		return fmt.Errorf("formal ReviewRun cannot finalize host cancellation without provider result")
	}
	resultBytes, err := repository.ReadArtifact(resultRef)
	if err != nil {
		return fmt.Errorf("read formal terminal StageExecutionResult: %w", err)
	}
	result, err := contractsv1alpha1.DecodeStageExecutionResult(resultBytes)
	if err != nil {
		return err
	}
	wantRunStatus := runmodel.RunStatusFailed
	wantAttemptStatus := runmodel.StageStatusFailed
	if result.Status == contractsv1alpha1.StageExecutionSucceeded {
		wantRunStatus = runmodel.RunStatusSucceeded
		wantAttemptStatus = runmodel.StageStatusSucceeded
	} else if result.Status == contractsv1alpha1.StageExecutionCanceled {
		wantRunStatus = runmodel.RunStatusCanceled
		wantAttemptStatus = runmodel.StageStatusCanceled
	}
	if run.Status != wantRunStatus || attempt.Status != wantAttemptStatus {
		return fmt.Errorf("formal ReviewRun terminal status does not match provider result")
	}
	if result.Status != contractsv1alpha1.StageExecutionSucceeded {
		if attempt.OutputRef != nil || len(run.Evidence) != 0 ||
			run.HypothesisSetRef != nil || run.RawCandidateCollectionRef != nil ||
			run.AgentTaskEvidenceRef != nil ||
			run.AgentExecutionReceiptRef != nil || run.CandidateSetRef != nil ||
			run.VerificationLedgerRef != nil ||
			run.CalibrationLedgerRef != nil || run.SuppressionLedgerRef != nil ||
			run.GovernedReportRef != nil ||
			run.GovernedMarkdownRef != nil || run.Failure == nil || result.Failure == nil ||
			run.Failure.Code != result.Failure.Code || run.Failure.Message != result.Failure.Message ||
			run.Failure.StageID != attempt.StageID || run.Failure.Retryable != result.Failure.Retryable {
			return fmt.Errorf("formal non-succeeded ReviewRun changed terminal failure facts")
		}
		return repository.verifyLedgerProjection(run)
	}

	evidence, found, err := repository.LookupAgentStageHypothesisEvidence(
		run.RunID, binding.BindingID, subject,
	)
	if err != nil || !found {
		if err == nil {
			err = fmt.Errorf("hypothesis evidence does not exist")
		}
		return fmt.Errorf("resolve formal hypothesis evidence: %w", err)
	}
	if attempt.OutputRef == nil || *attempt.OutputRef != evidence.Output.Local ||
		run.HypothesisSetRef == nil || *run.HypothesisSetRef != evidence.Output.Local ||
		len(run.Evidence) != 1 {
		return fmt.Errorf("formal succeeded run does not project exact hypothesis artifact")
	}
	wantCompleteness, wantNotes := targetEvidenceCompleteness(target)
	wantEvidence := runmodel.RunEvidence{
		EvidenceID: evidence.EvidenceID, BindingID: binding.BindingID,
		StageID: attempt.StageID, Attempt: attempt.Attempt, Generation: attempt.Generation,
		IdempotencyKey: binding.CreateIdempotencyKey, FencingToken: binding.FencingToken,
		ArtifactRef: evidence.Output.Local, Completeness: wantCompleteness,
		CompletenessNotes: wantNotes, RecordedAt: evidence.AdmittedAt,
	}
	if !reflect.DeepEqual(run.Evidence[0], wantEvidence) {
		return fmt.Errorf("formal RunEvidence projection changed")
	}
	hypothesisBytes, err := repository.ReadArtifact(evidence.Output.Local)
	if err != nil {
		return err
	}
	set, err := contractsv1alpha1.DecodeReviewHypothesisSet(hypothesisBytes)
	if err != nil {
		return err
	}
	if set.ReviewRunID != run.RunID || set.ExecutionID != result.ExecutionID ||
		set.TargetDigest != targetDigest {
		return fmt.Errorf("formal hypothesis set escaped run, execution, or target")
	}
	if result.AgentRawCandidates == nil {
		if run.RawCandidateCollectionRef != nil {
			return fmt.Errorf("formal ReviewRun invented raw candidate evidence")
		}
	} else {
		want := runmodel.ArtifactRef{
			URI:       "artifact://local/sha256/" + result.AgentRawCandidates.Ref.SHA256,
			SHA256:    result.AgentRawCandidates.Ref.SHA256,
			SizeBytes: result.AgentRawCandidates.Ref.SizeBytes,
			Contract:  result.AgentRawCandidates.Contract,
		}
		if run.RawCandidateCollectionRef == nil || *run.RawCandidateCollectionRef != want {
			return fmt.Errorf("formal ReviewRun does not project exact raw candidate evidence")
		}
		rawBytes, readErr := repository.ReadArtifact(want)
		if readErr != nil {
			return readErr
		}
		raw, decodeErr := contractsv1alpha1.DecodeAgentReviewRawCandidateCollection(rawBytes)
		if decodeErr != nil {
			return decodeErr
		}
		reviewDimensions := make([]contractsv1alpha1.VersionedRef, 0)
		for _, skill := range plan.Skills {
			if skill.Phase == contractsv1alpha1.AgentStageSkillPhaseReview {
				reviewDimensions = append(reviewDimensions, skill.Ref)
			}
		}
		if err := contractsv1alpha1.ValidateAgentReviewRawCandidateSetBindings(
			raw, set, reviewDimensions,
		); err != nil {
			return err
		}
	}
	if result.AgentTaskEvidence == nil {
		if run.AgentTaskEvidenceRef != nil {
			return fmt.Errorf("formal ReviewRun invented agent task evidence")
		}
	} else {
		want := runmodel.ArtifactRef{
			URI:       "artifact://local/sha256/" + result.AgentTaskEvidence.Ref.SHA256,
			SHA256:    result.AgentTaskEvidence.Ref.SHA256,
			SizeBytes: result.AgentTaskEvidence.Ref.SizeBytes,
			Contract:  result.AgentTaskEvidence.Contract,
		}
		if run.AgentTaskEvidenceRef == nil || *run.AgentTaskEvidenceRef != want {
			return fmt.Errorf("formal ReviewRun does not project exact agent task evidence")
		}
		taskEvidenceBytes, readErr := repository.ReadArtifact(want)
		if readErr != nil {
			return readErr
		}
		taskEvidence, decodeErr := contractsv1alpha1.DecodeAgentReviewTaskEvidenceCollection(taskEvidenceBytes)
		if decodeErr != nil {
			return decodeErr
		}
		if taskEvidence.PlanID != plan.PlanID || taskEvidence.SourceRunID != run.RunID ||
			taskEvidence.ExecutionID != result.ExecutionID || taskEvidence.ReviewRunID != run.RunID ||
			taskEvidence.TargetDigest != targetDigest {
			return fmt.Errorf("formal agent task evidence escaped run, execution, plan, or target")
		}
	}
	if result.AgentExecutionReceipts == nil {
		if run.AgentExecutionReceiptRef != nil {
			return fmt.Errorf("formal ReviewRun invented agent execution receipts")
		}
	} else {
		want := runmodel.ArtifactRef{
			URI:       "artifact://local/sha256/" + result.AgentExecutionReceipts.Ref.SHA256,
			SHA256:    result.AgentExecutionReceipts.Ref.SHA256,
			SizeBytes: result.AgentExecutionReceipts.Ref.SizeBytes,
			Contract:  result.AgentExecutionReceipts.Contract,
		}
		if run.AgentExecutionReceiptRef == nil || *run.AgentExecutionReceiptRef != want {
			return fmt.Errorf("formal ReviewRun does not project exact agent execution receipts")
		}
		receiptBytes, readErr := repository.ReadArtifact(want)
		if readErr != nil {
			return readErr
		}
		receipts, decodeErr := contractsv1alpha1.DecodeAgentExecutionReceiptCollection(receiptBytes)
		if decodeErr != nil {
			return decodeErr
		}
		if receipts.PlanID != plan.PlanID || receipts.SourceRunID != run.RunID ||
			receipts.ExecutionID != result.ExecutionID || receipts.ReviewRunID != run.RunID {
			return fmt.Errorf("formal agent execution receipts escaped run, execution, or plan")
		}
	}
	if run.CandidateSetRef == nil || run.VerificationLedgerRef == nil ||
		run.CalibrationLedgerRef == nil || run.SuppressionLedgerRef == nil ||
		run.GovernedReportRef == nil || run.GovernedMarkdownRef == nil {
		return fmt.Errorf("formal succeeded run omitted candidate, verification, calibration, suppression, or report facts")
	}
	verificationBytes, err := repository.ReadArtifact(*run.VerificationLedgerRef)
	if err != nil {
		return err
	}
	verification, err := contractsv1alpha1.DecodeCandidateVerificationLedger(verificationBytes)
	if err != nil {
		return err
	}
	if err := verification.ValidateAgainstHypothesisSet(set); err != nil {
		return err
	}
	canonicalVerification, err := json.Marshal(verification)
	if err != nil || !bytes.Equal(canonicalVerification, verificationBytes) {
		return fmt.Errorf("candidate verification ledger artifact is not canonical")
	}
	candidateBytes, err := repository.ReadArtifact(*run.CandidateSetRef)
	if err != nil {
		return err
	}
	candidates, err := contractsv1alpha1.DecodeGovernedCandidateSet(candidateBytes)
	if err != nil {
		return err
	}
	if err := candidates.ValidateAgainstHypothesisSet(set); err != nil {
		return err
	}
	if err := candidates.ValidateAgainstVerificationLedger(verification); err != nil {
		return err
	}
	canonicalCandidates, err := json.Marshal(candidates)
	if err != nil || !bytes.Equal(canonicalCandidates, candidateBytes) {
		return fmt.Errorf("governed candidate set artifact is not canonical")
	}
	reportBytes, err := repository.ReadArtifact(*run.GovernedReportRef)
	if err != nil {
		return err
	}
	report, err := contractsv1alpha1.DecodeGovernedReviewReport(reportBytes)
	if err != nil {
		return err
	}
	if err := report.ValidateAgainstHypothesisSet(set); err != nil {
		return err
	}
	if err := report.ValidateAgainstVerificationLedger(verification); err != nil {
		return err
	}
	calibrationBytes, err := repository.ReadArtifact(*run.CalibrationLedgerRef)
	if err != nil {
		return err
	}
	calibration, err := contractsv1alpha1.DecodeFindingCalibrationLedger(calibrationBytes)
	if err != nil {
		return err
	}
	if err := calibration.ValidateAgainst(report, verification); err != nil {
		return err
	}
	canonicalCalibration, err := json.Marshal(calibration)
	if err != nil || !bytes.Equal(canonicalCalibration, calibrationBytes) {
		return fmt.Errorf("finding calibration ledger artifact is not canonical")
	}
	suppressionBytes, err := repository.ReadArtifact(*run.SuppressionLedgerRef)
	if err != nil {
		return err
	}
	suppression, err := contractsv1alpha1.DecodeFindingSuppressionLedger(suppressionBytes)
	if err != nil {
		return err
	}
	if err := suppression.ValidateAgainst(report, calibration); err != nil {
		return err
	}
	canonicalSuppression, err := json.Marshal(suppression)
	if err != nil || !bytes.Equal(canonicalSuppression, suppressionBytes) {
		return fmt.Errorf("finding suppression ledger artifact is not canonical")
	}
	if !reflect.DeepEqual(report.Candidates, candidates.Candidates) ||
		report.Summary.Candidates != candidates.Summary.Candidates ||
		report.Summary.Confirmed != candidates.Summary.Confirmed ||
		report.Summary.Rejected != candidates.Summary.Rejected ||
		report.Summary.Inconclusive != candidates.Summary.Inconclusive {
		return fmt.Errorf("governed report changed candidate ledger facts")
	}
	canonicalReport, err := json.Marshal(report)
	if err != nil || !bytes.Equal(canonicalReport, reportBytes) {
		return fmt.Errorf("governed report artifact is not canonical")
	}
	markdown, err := repository.ReadArtifact(*run.GovernedMarkdownRef)
	if err != nil {
		return err
	}
	wantMarkdown, err := contractsv1alpha1.RenderGovernedReviewMarkdown(report)
	if err != nil || !bytes.Equal(markdown, []byte(wantMarkdown)) {
		return fmt.Errorf("governed Markdown does not exactly match JSON report")
	}
	_ = input // target closure above already validates exact frozen ReviewInput.
	return repository.verifyLedgerProjection(run)
}

// verifyFormalFindingGovernanceReplayClosure admits a replay that performs no
// provider execution. The source ReviewRun remains the owner of all upstream
// review facts; only calibration and suppression are derived under the frozen
// variant policy. This prevents post-processing experiments from inventing
// task, usage, latency, or provider execution evidence.
func (repository *Repository) verifyFormalFindingGovernanceReplayClosure(
	run runmodel.ReviewRun,
	snapshot runmodel.ExecutionSnapshot,
	spec contractsv1alpha1.ReviewSpec,
	config reviewconfig.ConfigBundle,
	ancestors map[string]struct{},
) error {
	if !run.ReplayVariable.IsFilterPolicy() {
		return fmt.Errorf("formal filter policy replay variable is invalid")
	}
	if run.ReplayFromStage != "finding_governance" {
		return fmt.Errorf("finding_governance replay must start at finding_governance")
	}
	if len(snapshot.ReplayInputRefs) != 0 {
		return fmt.Errorf("finding_governance replay must use source formal refs, not stage checkpoints")
	}
	if run.Status != runmodel.RunStatusSucceeded || run.Failure != nil ||
		len(run.StageAttempts) != 0 || len(run.Bindings) != 0 || len(run.Evidence) != 0 ||
		run.RawCandidateCollectionRef != nil || run.AgentTaskEvidenceRef != nil ||
		run.AgentExecutionReceiptRef != nil {
		return fmt.Errorf("finding_governance replay invented provider execution facts")
	}
	if run.StartedAt == nil || run.CompletedAt == nil ||
		!run.CreatedAt.Equal(snapshot.CreatedAt) ||
		!run.StartedAt.Equal(snapshot.CreatedAt) ||
		!run.CompletedAt.Equal(snapshot.CreatedAt) {
		return fmt.Errorf("finding_governance replay timing must represent an instantaneous derivation")
	}
	if err := repository.verifyReplayLineage(run, snapshot, spec, config, ancestors); err != nil {
		return fmt.Errorf("verify formal finding_governance replay lineage: %w", err)
	}
	if snapshot.ReplaySourceRunRef == nil {
		return fmt.Errorf("finding_governance replay source reference is required")
	}
	sourceBytes, err := repository.ReadArtifact(*snapshot.ReplaySourceRunRef)
	if err != nil {
		return fmt.Errorf("read finding_governance source run: %w", err)
	}
	var source runmodel.ReviewRun
	if err := decodeStrictJSON(sourceBytes, &source); err != nil {
		return fmt.Errorf("decode finding_governance source run: %w", err)
	}
	if err := source.Validate(); err != nil {
		return fmt.Errorf("validate finding_governance source run: %w", err)
	}
	for name, pair := range map[string][2]*runmodel.ArtifactRef{
		"hypothesis set":      {run.HypothesisSetRef, source.HypothesisSetRef},
		"candidate set":       {run.CandidateSetRef, source.CandidateSetRef},
		"verification ledger": {run.VerificationLedgerRef, source.VerificationLedgerRef},
		"governed report":     {run.GovernedReportRef, source.GovernedReportRef},
		"governed markdown":   {run.GovernedMarkdownRef, source.GovernedMarkdownRef},
	} {
		if pair[0] == nil || pair[1] == nil || *pair[0] != *pair[1] {
			return fmt.Errorf("finding_governance replay changed source %s ref", name)
		}
	}
	if run.CalibrationLedgerRef == nil || run.SuppressionLedgerRef == nil {
		return fmt.Errorf("finding_governance replay omitted derived governance ledgers")
	}
	if source.CalibrationLedgerRef != nil && *run.CalibrationLedgerRef == *source.CalibrationLedgerRef {
		return fmt.Errorf("finding_governance replay did not derive a new calibration ledger")
	}
	if source.SuppressionLedgerRef != nil && *run.SuppressionLedgerRef == *source.SuppressionLedgerRef {
		return fmt.Errorf("finding_governance replay did not derive a new suppression ledger")
	}
	if config.FindingGovernance == nil {
		return fmt.Errorf("finding_governance replay requires a frozen policy")
	}
	policy := config.FindingGovernance
	if err := policy.Validate(); err != nil {
		return fmt.Errorf("validate frozen finding_governance policy: %w", err)
	}

	verificationBytes, err := repository.ReadArtifact(*run.VerificationLedgerRef)
	if err != nil {
		return err
	}
	verification, err := contractsv1alpha1.DecodeCandidateVerificationLedger(verificationBytes)
	if err != nil {
		return err
	}
	reportBytes, err := repository.ReadArtifact(*run.GovernedReportRef)
	if err != nil {
		return err
	}
	report, err := contractsv1alpha1.DecodeGovernedReviewReport(reportBytes)
	if err != nil {
		return err
	}
	if report.ReviewRunID != verification.ReviewRunID {
		return fmt.Errorf("finding_governance source report and verification have different owners")
	}
	calibrationBytes, err := repository.ReadArtifact(*run.CalibrationLedgerRef)
	if err != nil {
		return err
	}
	calibration, err := contractsv1alpha1.DecodeFindingCalibrationLedger(calibrationBytes)
	if err != nil {
		return err
	}
	if err := calibration.ValidateAgainst(report, verification); err != nil {
		return err
	}
	wantPoints := make([]contractsv1alpha1.FindingCalibrationPoint, len(policy.CalibrationProfile.Points))
	for index, point := range policy.CalibrationProfile.Points {
		wantPoints[index] = contractsv1alpha1.FindingCalibrationPoint{
			RawPPM: point.RawPPM, ConfidencePPM: point.ConfidencePPM,
		}
	}
	wantProfile := contractsv1alpha1.FindingCalibrationProfile{
		SchemaVersion: policy.CalibrationProfile.SchemaVersion,
		ID:            policy.CalibrationProfile.ID, Revision: policy.CalibrationProfile.Revision,
		Points: wantPoints, SHA256: policy.CalibrationProfile.SHA256,
	}
	if calibration.Profile == nil || !reflect.DeepEqual(*calibration.Profile, wantProfile) {
		return fmt.Errorf("finding_governance calibration does not bind the frozen profile")
	}
	canonicalCalibration, err := json.Marshal(calibration)
	if err != nil || !bytes.Equal(canonicalCalibration, calibrationBytes) {
		return fmt.Errorf("finding_governance calibration ledger artifact is not canonical")
	}

	suppressionBytes, err := repository.ReadArtifact(*run.SuppressionLedgerRef)
	if err != nil {
		return err
	}
	suppression, err := contractsv1alpha1.DecodeFindingSuppressionLedger(suppressionBytes)
	if err != nil {
		return err
	}
	if err := suppression.ValidateAgainst(report, calibration); err != nil {
		return err
	}
	wantPolicy := contractsv1alpha1.VersionedRef{
		ID: policy.ID, Revision: policy.Revision, SHA256: policy.SHA256,
	}
	if suppression.Policy == nil || *suppression.Policy != wantPolicy ||
		suppression.MinimumConfidencePPM != policy.MinimumConfidencePPM ||
		suppression.MaxFindings != uint32(policy.MaxFindings) {
		return fmt.Errorf("finding_governance suppression does not bind the frozen policy")
	}
	canonicalSuppression, err := json.Marshal(suppression)
	if err != nil || !bytes.Equal(canonicalSuppression, suppressionBytes) {
		return fmt.Errorf("finding_governance suppression ledger artifact is not canonical")
	}
	return repository.verifyLedgerProjection(run)
}

// FormalPlatformExecutionBinding is the sole projection from the formal agent
// provider acknowledgement into the cross-runtime ReviewRun binding model.
func FormalPlatformExecutionBinding(
	binding runmodel.AgentStageExecutionBinding,
) runmodel.PlatformExecutionBinding {
	return runmodel.PlatformExecutionBinding{
		BindingID: binding.BindingID, RunID: binding.ReviewRunID,
		StageID: binding.Stage.ID, Attempt: binding.Attempt, Generation: binding.Generation,
		IdempotencyKey: binding.CreateIdempotencyKey, FencingToken: binding.FencingToken,
		RuntimeKind: "local_process", RuntimeID: "argus-formal-local-pi",
		CreatedAt: binding.RecordedAt,
	}
}
