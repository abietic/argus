package application

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/abietic/argus/internal/reviewcore"
	"github.com/abietic/argus/internal/runmodel"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

func validateAgentStageResultDispatchFence(
	result contractsv1alpha1.StageExecutionResult,
	admission runmodel.AgentStagePlanAdmission,
	intent runmodel.AgentStageDispatchIntent,
	binding runmodel.AgentStageExecutionBinding,
) error {
	if err := validateAgentStageResultExecutionFence(result, admission, intent, binding); err != nil {
		return err
	}
	if result.Status != contractsv1alpha1.StageExecutionSucceeded {
		return fmt.Errorf(
			"formal hypothesis admission requires a succeeded execution result, got %q",
			result.Status,
		)
	}
	return nil
}

func validateAgentStageResultExecutionFence(
	result contractsv1alpha1.StageExecutionResult,
	admission runmodel.AgentStagePlanAdmission,
	intent runmodel.AgentStageDispatchIntent,
	binding runmodel.AgentStageExecutionBinding,
) error {
	if err := admission.Validate(); err != nil {
		return fmt.Errorf("validate formal agent-stage admission: %w", err)
	}
	if err := intent.ValidateAgainstAdmission(admission); err != nil {
		return fmt.Errorf("validate formal agent-stage dispatch intent: %w", err)
	}
	if err := binding.ValidateAgainstIntent(intent); err != nil {
		return fmt.Errorf("validate formal agent-stage execution binding: %w", err)
	}
	if result.RequestSHA256 != intent.RequestSemanticSHA256 ||
		result.RequestSHA256 != binding.RequestSemanticSHA256 ||
		result.ExecutionID != intent.ExecutionID ||
		result.ExecutionID != binding.ExecutionID ||
		result.Attempt != intent.Attempt ||
		result.Attempt != binding.Attempt ||
		result.Generation != intent.Generation ||
		result.Generation != binding.Generation ||
		result.FencingToken != intent.FencingToken ||
		result.FencingToken != binding.FencingToken ||
		result.IdempotencyKey != intent.CreateIdempotencyKey ||
		result.IdempotencyKey != binding.CreateIdempotencyKey ||
		result.CapabilitySHA256 != intent.CapabilitySHA256 ||
		result.CapabilitySHA256 != binding.CapabilitySHA256 {
		return fmt.Errorf(
			"StageExecutionResult does not match the exact admitted dispatch and execution binding",
		)
	}
	return nil
}

func validateAgentStagePlanAdmissionClosure(
	plan contractsv1alpha1.AgentStagePlan,
	admission runmodel.AgentStagePlanAdmission,
) error {
	if err := plan.Validate(); err != nil {
		return fmt.Errorf("validate admitted AgentStagePlan: %w", err)
	}
	if plan.PlanID != admission.PlanID ||
		plan.SHA256 != admission.PlanSemanticSHA256 ||
		plan.BehaviorSHA256 != admission.PlanBehaviorSHA256 ||
		plan.ReviewRunID != admission.ReviewRunID ||
		admission.Stage != admissionStage(plan.Stage) {
		return fmt.Errorf("AgentStagePlan identities do not match the exact admission")
	}
	if plan.ExecutionSnapshot != bindingFromAdmission(
		admission.Sources.ExecutionSnapshot.Governed,
	) || plan.ConfigBundle != bindingFromAdmission(admission.Sources.ConfigBundle.Governed) ||
		plan.ConfigResolutionReceiptRef != bindingFromAdmission(
			admission.Sources.ConfigResolutionReceipt.Governed,
		) || plan.Workflow != bindingFromAdmission(admission.Sources.Workflow.Governed) ||
		plan.ReviewSpec != bindingFromAdmission(admission.Sources.ReviewSpec.Governed) ||
		plan.ReviewInput != bindingFromAdmission(admission.Sources.ReviewInput.Governed) {
		return fmt.Errorf("AgentStagePlan does not exactly project its admitted source closure")
	}
	return nil
}

func validateFormalReviewHypothesisSet(
	ctx context.Context,
	set contractsv1alpha1.ReviewHypothesisSet,
	plan contractsv1alpha1.AgentStagePlan,
	request contractsv1alpha1.StageExecutionRequest,
	result contractsv1alpha1.StageExecutionResult,
	intent runmodel.AgentStageDispatchIntent,
	input reviewcore.ReviewInput,
) error {
	if set.PlanID != plan.PlanID ||
		set.SourceRunID != plan.ReviewRunID ||
		set.ReviewRunID != plan.ReviewRunID ||
		set.ExecutionID != request.ExecutionID ||
		set.TargetDigest != plan.TargetDigest {
		return fmt.Errorf(
			"ReviewHypothesisSet does not match the admitted plan, run, execution, and target",
		)
	}
	if string(set.Completeness) != result.Completeness ||
		!slices.Equal(set.CompletenessReasons, result.CompletenessNotes) {
		return fmt.Errorf(
			"ReviewHypothesisSet completeness does not exactly match StageExecutionResult",
		)
	}
	if result.RecordedAt.Before(intent.RecordedAt) ||
		result.RecordedAt.After(request.Deadline) ||
		set.GeneratedAt.Before(intent.RecordedAt) ||
		set.GeneratedAt.After(result.RecordedAt) ||
		set.GeneratedAt.After(request.Deadline) {
		return fmt.Errorf(
			"formal hypothesis timestamps fall outside the admitted dispatch and deadline window",
		)
	}
	if len(set.Hypotheses) > plan.Budget.MaxHypotheses ||
		uint64(set.Coverage.GroupsTotal) > uint64(plan.Budget.MaxGroups) ||
		uint64(set.Coverage.FilesIncluded) > uint64(plan.Budget.MaxFiles) ||
		uint64(set.Coverage.FilesIncluded) > uint64(len(input.Files)) {
		return fmt.Errorf("ReviewHypothesisSet exceeds an independently observable plan budget")
	}

	distinctGroups := make(map[string]struct{}, len(set.Hypotheses))
	for _, hypothesis := range set.Hypotheses {
		distinctGroups[hypothesis.GroupID] = struct{}{}
	}
	if uint64(len(distinctGroups)) > uint64(set.Coverage.GroupsTotal) ||
		uint64(len(distinctGroups)) > uint64(plan.Budget.MaxGroups) {
		return fmt.Errorf("hypothesis group identities exceed declared coverage or plan budget")
	}
	for _, decision := range set.NormalizationDecisions {
		if decision.Action == contractsv1alpha1.HypothesisNormalizationExcludedBudget &&
			len(set.Hypotheses) != plan.Budget.MaxHypotheses {
			return fmt.Errorf(
				"budget-excluded hypotheses require the admitted hypothesis limit to be exhausted",
			)
		}
	}

	reviewSkills := make([]contractsv1alpha1.VersionedRef, 0, len(plan.Skills))
	verificationSkills := make([]contractsv1alpha1.VersionedRef, 0, len(plan.Skills))
	for _, skill := range plan.Skills {
		switch skill.Phase {
		case contractsv1alpha1.AgentStageSkillPhaseReview:
			reviewSkills = append(reviewSkills, skill.Ref)
		case contractsv1alpha1.AgentStageSkillPhaseVerification:
			verificationSkills = append(verificationSkills, skill.Ref)
		}
	}
	for index, hypothesis := range set.Hypotheses {
		if !slices.Contains(reviewSkills, hypothesis.Dimension) {
			return fmt.Errorf(
				"hypotheses[%d].dimension is not an exact admitted review-phase skill",
				index,
			)
		}
		if err := validateFormalHypothesisAnchor(
			ctx,
			input,
			hypothesis.Anchor,
			"hypothesis anchor",
			"",
			true,
		); err != nil {
			return fmt.Errorf("hypotheses[%d]: %w", index, err)
		}
		for evidenceIndex, evidence := range hypothesis.Evidence {
			if err := validateFormalHypothesisAnchor(
				ctx,
				input,
				evidence.Anchor,
				"evidence anchor",
				evidence.Excerpt,
				false,
			); err != nil {
				return fmt.Errorf(
					"hypotheses[%d].evidence[%d]: %w",
					index,
					evidenceIndex,
					err,
				)
			}
		}
		for observationIndex, observation := range hypothesis.Verification {
			if !slices.Contains(verificationSkills, observation.Verifier) {
				return fmt.Errorf(
					"hypotheses[%d].verification_observations[%d].verifier is not an exact admitted verification-phase skill",
					index,
					observationIndex,
				)
			}
		}
	}
	return nil
}

func validateFormalHypothesisAnchor(
	ctx context.Context,
	input reviewcore.ReviewInput,
	anchor contractsv1alpha1.HypothesisSourceAnchor,
	label string,
	excerpt string,
	requireTargetAuthorization bool,
) error {
	wantSide := contractsv1alpha1.HypothesisAnchorFile
	if input.TargetMode == reviewcore.TargetModeDiff {
		wantSide = contractsv1alpha1.HypothesisAnchorNew
	}
	if anchor.Side != wantSide {
		return fmt.Errorf(
			"%s side %q is not verifiable from the frozen %s input",
			label,
			anchor.Side,
			input.TargetMode,
		)
	}
	if requireTargetAuthorization && !reviewcore.InputAuthorizesAnchor(
		ctx,
		input,
		anchor.Path,
		anchor.StartLine,
		anchor.EndLine,
	) {
		return fmt.Errorf("%s is outside the frozen review target", label)
	}
	for _, file := range input.Files {
		if file.Path != anchor.Path {
			continue
		}
		if file.SHA256 != anchor.SourceDigest || file.Content == nil {
			return fmt.Errorf("%s does not bind exact frozen file content", label)
		}
		if excerpt == "" {
			return nil
		}
		normalized := strings.ReplaceAll(strings.ReplaceAll(*file.Content, "\r\n", "\n"), "\r", "\n")
		lines := strings.Split(normalized, "\n")
		if strings.HasSuffix(normalized, "\n") {
			lines = lines[:len(lines)-1]
		}
		start := int(anchor.StartLine - 1)
		end := int(anchor.EndLine)
		if start < 0 || end > len(lines) ||
			strings.TrimSpace(strings.Join(lines[start:end], "\n")) != excerpt {
			return fmt.Errorf("%s excerpt is not the exact frozen line range", label)
		}
		return nil
	}
	return fmt.Errorf("%s path is absent from the frozen file manifest", label)
}
