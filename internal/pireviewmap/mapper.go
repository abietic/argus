package pireviewmap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/abietic/argus/internal/reviewcore"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

// MapShadowReviewReport preserves the existing local shadow projection while
// sharing the strict worker-report core with formal platform execution.
func MapShadowReviewReport(
	plan contractsv1alpha1.AgentReviewPlan,
	input reviewcore.ReviewInput,
	completedAt time.Time,
	reportData []byte,
) (
	contractsv1alpha1.ReviewHypothesisSet,
	contractsv1alpha1.AgentExecutionReceiptCollection,
	error,
) {
	return mapPiReviewReport(plan, input, completedAt, reportData, "", "", nil)
}

// MapShadowReviewReportWithRawCandidates additionally returns the bounded,
// diagnostic raw-candidate artifact. Keeping this as an additive API lets
// existing strict-mapping callers continue to validate the established
// Hypothesis/Receipt projection while the shadow import path persists the
// complete pre-normalization lineage.
func MapShadowReviewReportWithRawCandidates(
	plan contractsv1alpha1.AgentReviewPlan,
	input reviewcore.ReviewInput,
	completedAt time.Time,
	reportData []byte,
) (
	contractsv1alpha1.ReviewHypothesisSet,
	contractsv1alpha1.AgentExecutionReceiptCollection,
	contractsv1alpha1.AgentReviewRawCandidateCollection,
	error,
) {
	set, receipts, err := mapPiReviewReport(plan, input, completedAt, reportData, "", "", nil)
	if err != nil {
		return contractsv1alpha1.ReviewHypothesisSet{},
			contractsv1alpha1.AgentExecutionReceiptCollection{},
			contractsv1alpha1.AgentReviewRawCandidateCollection{}, err
	}
	report, err := decodePiReviewReport(reportData)
	if err != nil {
		return contractsv1alpha1.ReviewHypothesisSet{},
			contractsv1alpha1.AgentExecutionReceiptCollection{},
			contractsv1alpha1.AgentReviewRawCandidateCollection{}, err
	}
	raw, err := mapPiRawCandidateCollection(plan, set, report)
	if err != nil {
		return contractsv1alpha1.ReviewHypothesisSet{},
			contractsv1alpha1.AgentExecutionReceiptCollection{},
			contractsv1alpha1.AgentReviewRawCandidateCollection{}, err
	}
	return set, receipts, raw, nil
}

// ShadowReviewArtifacts is the complete Argus-owned projection of a local Pi
// report. TaskEvidence remains sensitive worker self-report and is deliberately
// separate from Hailix Trace/Artifact attestation.
type ShadowReviewArtifacts struct {
	Hypotheses    contractsv1alpha1.ReviewHypothesisSet
	Receipts      contractsv1alpha1.AgentExecutionReceiptCollection
	RawCandidates contractsv1alpha1.AgentReviewRawCandidateCollection
	TaskEvidence  contractsv1alpha1.AgentReviewTaskEvidenceCollection
}

func MapShadowReviewReportWithArtifacts(
	plan contractsv1alpha1.AgentReviewPlan,
	input reviewcore.ReviewInput,
	completedAt time.Time,
	reportData []byte,
) (ShadowReviewArtifacts, error) {
	set, receipts, raw, err := MapShadowReviewReportWithRawCandidates(plan, input, completedAt, reportData)
	if err != nil {
		return ShadowReviewArtifacts{}, err
	}
	report, err := decodePiReviewReport(reportData)
	if err != nil {
		return ShadowReviewArtifacts{}, err
	}
	evidence, err := mapPiTaskEvidenceCollection(plan, receipts, report.Execution)
	if err != nil {
		return ShadowReviewArtifacts{}, err
	}
	return ShadowReviewArtifacts{
		Hypotheses: set, Receipts: receipts, RawCandidates: raw, TaskEvidence: evidence,
	}, nil
}

func mapPiTaskEvidenceCollection(
	plan contractsv1alpha1.AgentReviewPlan,
	receipts contractsv1alpha1.AgentExecutionReceiptCollection,
	execution piExecutionEnvelope,
) (contractsv1alpha1.AgentReviewTaskEvidenceCollection, error) {
	if execution.TaskEvidence.SchemaVersion != "argus.pi-review.task_evidence.v0" ||
		execution.TaskEvidence.Authority != "diagnostic_only" ||
		execution.TaskEvidence.ProvenanceClass != "worker_self_report" ||
		execution.TaskEvidence.ContentPolicy != "exact_local_sensitive" {
		return contractsv1alpha1.AgentReviewTaskEvidenceCollection{}, fmt.Errorf("unsupported Pi task evidence contract")
	}
	observations := make(map[string]piTaskObservation, len(execution.Tasks))
	for _, task := range execution.Tasks {
		observations[task.TaskID] = task
	}
	receiptsByTask := make(map[string]contractsv1alpha1.AgentExecutionReceipt, len(receipts.Receipts))
	for _, receipt := range receipts.Receipts {
		receiptsByTask[receipt.TaskID] = receipt
	}
	tasks := make([]contractsv1alpha1.AgentReviewTaskExecutionEvidence, 0, len(execution.TaskEvidence.Tasks))
	for index, item := range execution.TaskEvidence.Tasks {
		observation, exists := observations[item.TaskID]
		if !exists || observation.TaskKind != item.TaskKind || observation.GroupID != item.GroupID ||
			!reflect.DeepEqual(observation.SkillID, item.SkillID) ||
			!reflect.DeepEqual(observation.CandidateID, item.CandidateID) ||
			observation.TerminalStatus != item.TerminalStatus {
			return contractsv1alpha1.AgentReviewTaskEvidenceCollection{}, fmt.Errorf("Pi task evidence[%d] does not bind its observation", index)
		}
		role, err := mapPiTaskRole(item.TaskKind)
		if err != nil {
			return contractsv1alpha1.AgentReviewTaskEvidenceCollection{}, err
		}
		status, _, err := mapPiTaskStatus(item.TerminalStatus, observation.ErrorCode)
		if err != nil {
			return contractsv1alpha1.AgentReviewTaskEvidenceCollection{}, err
		}
		tools := make([]contractsv1alpha1.AgentReviewTaskToolEvidence, 0, len(item.Tools))
		for _, tool := range item.Tools {
			tools = append(tools, contractsv1alpha1.AgentReviewTaskToolEvidence{
				Sequence: tool.Sequence, ToolName: tool.ToolName,
				Arguments: mapPiTaskEvidenceContent(tool.Arguments),
				Result:    mapOptionalPiTaskEvidenceContent(tool.Result), IsError: tool.IsError,
			})
		}
		receipt, exists := receiptsByTask[item.TaskID]
		if !exists {
			return contractsv1alpha1.AgentReviewTaskEvidenceCollection{}, fmt.Errorf("Pi task evidence[%d] has no mapped receipt", index)
		}
		tasks = append(tasks, contractsv1alpha1.AgentReviewTaskExecutionEvidence{
			TaskID: item.TaskID, TaskRole: role, GroupID: item.GroupID,
			SkillID: item.SkillID, HypothesisOccurrenceID: receipt.HypothesisOccurrenceID,
			SystemPrompt:   mapPiTaskEvidenceContent(item.SystemPrompt),
			UserPrompt:     mapPiTaskEvidenceContent(item.UserPrompt),
			Output:         mapOptionalPiTaskEvidenceContent(item.Output),
			TerminalStatus: status, Tools: tools,
		})
	}
	slices.SortFunc(tasks, func(left, right contractsv1alpha1.AgentReviewTaskExecutionEvidence) int {
		return strings.Compare(left.TaskID, right.TaskID)
	})
	collection := contractsv1alpha1.AgentReviewTaskEvidenceCollection{
		SchemaVersion: contractsv1alpha1.AgentReviewTaskEvidenceCollectionSchemaVersion,
		PlanID:        plan.PlanID, SourceRunID: plan.SourceRunID, ExecutionID: plan.ExecutionID,
		ReviewRunID: plan.ReviewRunID, TargetDigest: plan.TargetDigest,
		Authority:     contractsv1alpha1.AgentReviewTaskEvidenceAuthority,
		Provenance:    contractsv1alpha1.AgentReviewTaskEvidenceProvenance,
		Disposition:   contractsv1alpha1.AgentReviewTaskEvidenceDisposition,
		ContentPolicy: contractsv1alpha1.AgentReviewTaskEvidenceContentPolicy,
		Completeness:  execution.TaskEvidence.Completeness,
		ReasonCodes:   slices.Clone(execution.TaskEvidence.ReasonCodes), TaskExecutions: tasks,
	}
	slices.Sort(collection.ReasonCodes)
	if err := contractsv1alpha1.ValidateAgentReviewTaskEvidenceBindings(collection, plan, receipts); err != nil {
		return contractsv1alpha1.AgentReviewTaskEvidenceCollection{}, fmt.Errorf("validate mapped Pi task evidence: %w", err)
	}
	return collection, nil
}

func mapPiTaskRole(kind string) (contractsv1alpha1.AgentTaskRole, error) {
	switch kind {
	case "context":
		return contractsv1alpha1.AgentTaskContext, nil
	case "review":
		return contractsv1alpha1.AgentTaskReview, nil
	case "verification":
		return contractsv1alpha1.AgentTaskVerification, nil
	default:
		return "", fmt.Errorf("unsupported Pi task evidence kind %q", kind)
	}
}

func mapPiTaskEvidenceContent(value piTaskEvidenceContent) contractsv1alpha1.AgentReviewTaskEvidenceContent {
	return contractsv1alpha1.AgentReviewTaskEvidenceContent{
		SHA256: value.SHA256, SizeBytes: value.SizeBytes,
		Content: value.Content, OmissionReason: value.OmissionReason,
	}
}

func mapOptionalPiTaskEvidenceContent(value *piTaskEvidenceContent) *contractsv1alpha1.AgentReviewTaskEvidenceContent {
	if value == nil {
		return nil
	}
	mapped := mapPiTaskEvidenceContent(*value)
	return &mapped
}

func mapPiReviewReport(
	plan contractsv1alpha1.AgentReviewPlan,
	input reviewcore.ReviewInput,
	completedAt time.Time,
	reportData []byte,
	expectedPromptRevision string,
	expectedPromptSHA256 string,
	availableContextIDs map[string]struct{},
) (
	contractsv1alpha1.ReviewHypothesisSet,
	contractsv1alpha1.AgentExecutionReceiptCollection,
	error,
) {
	report, err := decodePiReviewReport(reportData)
	if err != nil {
		return contractsv1alpha1.ReviewHypothesisSet{},
			contractsv1alpha1.AgentExecutionReceiptCollection{}, err
	}
	completedAt = normalizedUTC(completedAt)
	if err := validatePiReportPlanBinding(
		plan,
		input,
		completedAt,
		report,
		expectedPromptRevision,
		expectedPromptSHA256,
		availableContextIDs,
	); err != nil {
		return contractsv1alpha1.ReviewHypothesisSet{},
			contractsv1alpha1.AgentExecutionReceiptCollection{}, err
	}

	mapped, err := mapPiCandidates(plan, input, report)
	if err != nil {
		return contractsv1alpha1.ReviewHypothesisSet{},
			contractsv1alpha1.AgentExecutionReceiptCollection{}, err
	}
	receipts, verificationStatus, taskGaps, err := mapPiReceipts(plan, completedAt, report, mapped)
	if err != nil {
		return contractsv1alpha1.ReviewHypothesisSet{},
			contractsv1alpha1.AgentExecutionReceiptCollection{}, err
	}
	if err := validatePiCoverageProjection(plan, report, mapped, receipts); err != nil {
		return contractsv1alpha1.ReviewHypothesisSet{},
			contractsv1alpha1.AgentExecutionReceiptCollection{}, err
	}
	gaps := append([]contractsv1alpha1.AgentReviewCoverageGap{}, taskGaps...)
	gaps = append(gaps, reportCoverageGaps(
		plan, input, report, mapped, availableContextIDs,
	)...)
	for index := range mapped.hypotheses {
		hypothesis := &mapped.hypotheses[index]
		verification, exists := verificationStatus[hypothesis.OccurrenceID]
		if !exists || verification.receiptStatus != contractsv1alpha1.AgentTaskSucceeded {
			gaps = append(gaps, makeCoverageGap(
				contractsv1alpha1.AgentReviewCoverageVerification,
				hypothesis.OccurrenceID,
				"verification_not_succeeded",
			))
			continue
		}
		candidate := mapped.candidateByOccurrence[hypothesis.OccurrenceID]
		if candidate.Verification == nil {
			gaps = append(gaps, makeCoverageGap(
				contractsv1alpha1.AgentReviewCoverageVerification,
				hypothesis.OccurrenceID,
				"verification_result_missing",
			))
			continue
		}
		observation, evidence, err := mapPiVerification(
			plan,
			input,
			hypothesis.OccurrenceID,
			*candidate.Verification,
		)
		if err != nil {
			return contractsv1alpha1.ReviewHypothesisSet{},
				contractsv1alpha1.AgentExecutionReceiptCollection{}, err
		}
		hypothesis.Evidence, err = mergeHypothesisEvidence(hypothesis.Evidence, evidence)
		if err != nil {
			return contractsv1alpha1.ReviewHypothesisSet{},
				contractsv1alpha1.AgentExecutionReceiptCollection{}, err
		}
		observation.EvidenceIDs = verificationEvidenceIDs(evidence)
		hypothesis.Verification = []contractsv1alpha1.HypothesisVerificationObservation{
			observation,
		}
	}
	gaps = uniqueSortedCoverageGaps(gaps)

	groupsTotal := uint32(len(report.Execution.Snapshot.Grouping.Groups))
	reviewSucceeded := uint32(0)
	succeededDimensions := make(map[string]map[contractsv1alpha1.VersionedRef]struct{})
	for _, receipt := range receipts {
		if receipt.TaskRole == contractsv1alpha1.AgentTaskReview &&
			receipt.Status == contractsv1alpha1.AgentTaskSucceeded {
			reviewSucceeded++
			byDimension := succeededDimensions[receipt.GroupID]
			if byDimension == nil {
				byDimension = make(map[contractsv1alpha1.VersionedRef]struct{})
				succeededDimensions[receipt.GroupID] = byDimension
			}
			byDimension[receipt.Dimension] = struct{}{}
		}
	}
	groupsReviewed := uint32(0)
	for _, group := range report.Execution.Snapshot.Grouping.Groups {
		if len(succeededDimensions[group.ID]) == len(plan.ReviewDimensions) {
			groupsReviewed++
		}
	}
	reviewTotal := uint64(groupsTotal) * uint64(len(plan.ReviewDimensions))
	if reviewTotal > uint64(^uint32(0)) {
		return contractsv1alpha1.ReviewHypothesisSet{},
			contractsv1alpha1.AgentExecutionReceiptCollection{},
			fmt.Errorf("review task coverage overflows uint32")
	}
	reasons := coverageReasonCodes(gaps)
	completeness := contractsv1alpha1.AgentReviewPartial
	if len(gaps) == 0 {
		completeness = contractsv1alpha1.AgentReviewComplete
	}
	set := contractsv1alpha1.ReviewHypothesisSet{
		SchemaVersion:   contractsv1alpha1.ReviewHypothesisSetSchemaVersion,
		HypothesisSetID: "hypothesis-set-" + digestText(plan.ExecutionID)[:24],
		PlanID:          plan.PlanID, SourceRunID: plan.SourceRunID,
		ExecutionID: plan.ExecutionID, ReviewRunID: plan.ReviewRunID,
		TargetDigest: plan.TargetDigest,
		Completeness: completeness, CompletenessReasons: reasons,
		NormalizationDecisions: mapped.decisions,
		Hypotheses:             mapped.hypotheses, DedupClusters: mapped.clusters,
		Coverage: contractsv1alpha1.AgentReviewCoverage{
			GroupsTotal: groupsTotal, GroupsReviewed: groupsReviewed,
			ReviewTasksTotal:     uint32(reviewTotal),
			ReviewTasksSucceeded: reviewSucceeded,
			FilesIncluded:        uint32(len(report.Target.Files)), Gaps: gaps,
		},
		GeneratedAt: completedAt,
	}
	collection := contractsv1alpha1.AgentExecutionReceiptCollection{
		SchemaVersion: contractsv1alpha1.AgentExecutionReceiptCollectionSchemaVersion,
		PlanID:        plan.PlanID, SourceRunID: plan.SourceRunID,
		ExecutionID: plan.ExecutionID, ReviewRunID: plan.ReviewRunID,
		Receipts: receipts,
	}
	if err := set.Validate(); err != nil {
		return contractsv1alpha1.ReviewHypothesisSet{},
			contractsv1alpha1.AgentExecutionReceiptCollection{}, err
	}
	if err := collection.Validate(); err != nil {
		return contractsv1alpha1.ReviewHypothesisSet{},
			contractsv1alpha1.AgentExecutionReceiptCollection{}, err
	}
	return set, collection, nil
}

func validatePiReportPlanBinding(
	plan contractsv1alpha1.AgentReviewPlan,
	input reviewcore.ReviewInput,
	completedAt time.Time,
	report piReviewReport,
	expectedPromptRevision string,
	expectedPromptSHA256 string,
	availableContextIDs map[string]struct{},
) error {
	if !completedAt.After(plan.CreatedAt) && !completedAt.Equal(plan.CreatedAt) {
		return fmt.Errorf("worker report completion precedes the plan")
	}
	if report.Status != "complete" && report.Status != "partial" && report.Status != "failed" {
		return fmt.Errorf("unsupported Pi report status %q", report.Status)
	}
	wantProvider := plan.Provider.ID
	wantProfile, err := piProfileForProvider(plan.Provider.ID)
	if err != nil {
		return err
	}
	if report.Provider != wantProvider || report.ProviderProfile != wantProfile ||
		report.Model != plan.Model.ID {
		return fmt.Errorf("Pi report provider/profile/model do not bind the plan")
	}
	if report.Target.Digest != plan.TargetDigest ||
		report.Execution.Snapshot.Target.Digest != plan.TargetDigest {
		return fmt.Errorf("Pi report target digest does not bind the frozen ReviewInput")
	}
	wantTargetKind := "files"
	if input.TargetMode == reviewcore.TargetModeDiff {
		wantTargetKind = "commit_diff"
	}
	if report.Target.Kind != wantTargetKind || report.Target.Repository !=
		"memory://review-input/"+jsEncodeURIComponent(input.TargetID) ||
		report.Target.BaseOID != nil || report.Target.HeadOID != nil ||
		report.Execution.Snapshot.Target.Kind != wantTargetKind ||
		report.Execution.Snapshot.Target.BaseOID != nil ||
		report.Execution.Snapshot.Target.HeadOID != nil {
		return fmt.Errorf("Pi report target identity does not bind the frozen ReviewInput")
	}
	wantPatchDigest := "sha256:" + digestText(input.CanonicalPatch)
	if report.Target.CanonicalPatchDigest == nil ||
		*report.Target.CanonicalPatchDigest != wantPatchDigest {
		return fmt.Errorf("Pi report canonical patch digest does not bind ReviewInput")
	}
	if report.Target.GeneratedAt == nil ||
		normalizedUTC(*report.Target.GeneratedAt).Before(plan.CreatedAt) ||
		normalizedUTC(*report.Target.GeneratedAt).After(completedAt) ||
		!normalizedUTC(report.Target.CapturedAt).Equal(normalizedUTC(*report.Target.GeneratedAt)) {
		return fmt.Errorf("Pi report target capture time is outside worker execution")
	}
	if report.Execution.Authority != "diagnostic_only" ||
		report.Execution.ProvenanceClass != "worker_self_report" {
		return fmt.Errorf("Pi report execution provenance is not diagnostic worker self-report")
	}
	snapshot := report.Execution.Snapshot
	wantWorkflowRevision, err := contractsv1alpha1.CandidateNormalizationWorkflowRevision(
		plan.Normalization.Revision,
	)
	if err != nil {
		return err
	}
	if snapshot.SchemaVersion != "argus.pi-review.execution_snapshot.v0" ||
		snapshot.WorkflowRevision != wantWorkflowRevision ||
		(expectedPromptRevision != "" && snapshot.PromptBundleRevision != expectedPromptRevision) ||
		snapshot.Grouping.ImplementationRevision != "directory-language-v0" ||
		snapshot.VerificationPolicy != "independent_required" ||
		snapshot.Replayability.Status != "non_replayable" {
		return fmt.Errorf("Pi execution snapshot policy does not match the admitted local worker")
	}
	if expectedPromptSHA256 != "" &&
		snapshot.PromptBundleDigest != expectedPromptSHA256 {
		return fmt.Errorf("Pi prompt bundle digest does not bind the formal prompt artifact")
	}
	if snapshot.Provider.Provider != wantProvider ||
		snapshot.Provider.Profile != wantProfile ||
		snapshot.Provider.Protocol != "anthropic-messages" ||
		snapshot.Provider.Model != plan.Model.ID {
		return fmt.Errorf("Pi execution snapshot provider does not bind the plan")
	}
	if snapshot.ToolPolicy.Mode != "read_only" ||
		!slices.Equal(snapshot.ToolPolicy.AllowedTools, plan.ToolPolicy.AllowedTools) {
		return fmt.Errorf("Pi execution snapshot tool policy does not bind the plan")
	}
	budget := snapshot.Budgets
	if budget.Concurrency != plan.Budget.MaxConcurrency ||
		budget.MaxToolCallsPerTask != plan.Budget.MaxToolCalls ||
		budget.MaxFiles != plan.Budget.MaxFiles ||
		budget.MaxGroups != plan.Budget.MaxGroups ||
		budget.MaxGroupBytes != plan.Budget.MaxGroupBytes ||
		budget.MaxTargetBytes != plan.Budget.MaxTargetBytes ||
		budget.MaxCandidates != plan.Budget.MaxCandidates ||
		budget.MaxProviderTurns != plan.Budget.MaxModelCalls ||
		budget.MaxOutputTokensPerTurn != plan.Budget.MaxOutputTokens ||
		budget.TaskTimeoutMS != plan.Budget.TimeoutMS {
		return fmt.Errorf("Pi execution snapshot budgets do not bind the plan")
	}
	if !samePiSkills(plan.ReviewDimensions, snapshot.Skills) {
		return fmt.Errorf("Pi execution snapshot skills do not bind plan review dimensions")
	}
	if !samePiKnowledge(plan.Knowledge, snapshot.Knowledge) {
		return fmt.Errorf("Pi execution snapshot knowledge does not bind plan knowledge")
	}
	if len(snapshot.Grouping.Groups) > int(plan.Budget.MaxGroups) ||
		len(snapshot.Grouping.Groups) != int(report.Coverage.GroupsTotal) {
		return fmt.Errorf("Pi report group coverage does not match its execution snapshot")
	}
	if !report.Coverage.VerificationEnabled {
		return fmt.Errorf("Pi report disabled required independent verification")
	}
	if report.Coverage.FilesIncluded != uint32(len(report.Target.Files)) {
		return fmt.Errorf("Pi report files_included is not the host target recomputation")
	}
	if report.Target.Files == nil || report.Target.Skipped == nil ||
		report.Coverage.Skipped == nil || report.Coverage.ContextGaps == nil ||
		report.Coverage.Failures == nil || report.Findings == nil ||
		report.Candidates == nil || report.RawCandidates == nil ||
		report.NormalizationDecisions == nil || report.Execution.Tasks == nil {
		return fmt.Errorf("Pi report omitted an explicit result array")
	}
	if err := validatePiTargetFiles(input, report.Target.Files, report.Target.Skipped); err != nil {
		return err
	}
	if err := validatePiGroups(input, report.Target.Files, snapshot.Grouping.Groups); err != nil {
		return err
	}
	if err := validateFrozenContextGapsWithAvailability(
		input,
		report.Coverage.ContextGaps,
		availableContextIDs,
	); err != nil {
		return err
	}
	return nil
}

func samePiSkills(
	plan []contractsv1alpha1.VersionedRef,
	reported []piSnapshotSkill,
) bool {
	if len(plan) != len(reported) {
		return false
	}
	for index, ref := range plan {
		if reported[index].ID != ref.ID || reported[index].Revision != ref.Revision ||
			reported[index].Digest != "sha256:"+ref.SHA256 {
			return false
		}
	}
	return true
}

func validatePiGroups(
	input reviewcore.ReviewInput,
	targetFiles []piTargetFile,
	groups []piSnapshotGroup,
) error {
	files := make(map[string]struct{}, len(targetFiles))
	for _, file := range targetFiles {
		files[file.Path] = struct{}{}
	}
	seenGroups := make(map[string]struct{}, len(groups))
	seenFiles := make(map[string]struct{}, len(files))
	for _, group := range groups {
		if group.ID == "" || group.Key == "" || !lowerSHA256(group.PatchDigest) ||
			len(group.Files) == 0 || !slices.IsSorted(group.Files) {
			return fmt.Errorf("Pi execution snapshot contains an invalid group")
		}
		if _, duplicate := seenGroups[group.ID]; duplicate {
			return fmt.Errorf("Pi execution snapshot contains duplicate group %q", group.ID)
		}
		seenGroups[group.ID] = struct{}{}
		groupPatches := make([]string, 0, len(group.Files))
		for index, path := range group.Files {
			if index > 0 && path == group.Files[index-1] {
				return fmt.Errorf("Pi execution group %q contains duplicate file", group.ID)
			}
			if _, exists := files[path]; !exists {
				return fmt.Errorf("Pi execution group %q escaped frozen ReviewInput", group.ID)
			}
			if _, duplicate := seenFiles[path]; duplicate {
				return fmt.Errorf("Pi execution file %q belongs to multiple groups", path)
			}
			patch, _, _, err := frozenPiFilePatch(input, path)
			if err != nil {
				return err
			}
			groupPatches = append(groupPatches, patch)
			seenFiles[path] = struct{}{}
		}
		if group.PatchDigest != digestText(strings.Join(groupPatches, "\n\n")) {
			return fmt.Errorf("Pi execution group %q patch digest is not the host recomputation", group.ID)
		}
	}
	if len(seenFiles) != len(files) {
		return fmt.Errorf("Pi execution groups do not close all frozen target files")
	}
	return nil
}

func validatePiTargetFiles(
	input reviewcore.ReviewInput,
	reported []piTargetFile,
	skipped []piSkipped,
) error {
	want := make(map[string]string, len(input.Files))
	wantSkipped := make([]piSkipped, 0, len(input.Files))
	for _, file := range input.Files {
		if file.Content != nil {
			want[file.Path] = file.SHA256
		} else {
			wantSkipped = append(wantSkipped, piSkipped{
				Path: file.Path, Reason: "frozen_target_content_unavailable",
			})
		}
	}
	slices.SortFunc(wantSkipped, func(left, right piSkipped) int {
		return strings.Compare(left.Path, right.Path)
	})
	if !reflect.DeepEqual(skipped, wantSkipped) {
		return fmt.Errorf("Pi target skipped entries do not close unavailable frozen files")
	}
	seen := make(map[string]struct{}, len(reported))
	for _, file := range reported {
		digest, exists := want[file.Path]
		patch, status, changedLines, patchErr := frozenPiFilePatch(input, file.Path)
		if !exists || file.Digest != digest || file.TargetDigest == nil ||
			*file.TargetDigest != "sha256:"+digest || file.PatchDigest == nil ||
			patchErr != nil || *file.PatchDigest != "sha256:"+digestText(patch) ||
			file.Status != status || file.ChangedLines != changedLines || file.OldPath != nil {
			return fmt.Errorf("Pi target file %q does not bind frozen ReviewInput", file.Path)
		}
		if _, duplicate := seen[file.Path]; duplicate {
			return fmt.Errorf("Pi target contains duplicate file %q", file.Path)
		}
		seen[file.Path] = struct{}{}
	}
	if len(seen) != len(want) {
		return fmt.Errorf("Pi target files do not close the frozen ReviewInput manifest")
	}
	return nil
}

func frozenPiFilePatch(
	input reviewcore.ReviewInput,
	filePath string,
) (string, string, uint32, error) {
	var content *string
	for _, file := range input.Files {
		if file.Path == filePath {
			content = file.Content
			break
		}
	}
	if content == nil {
		return "", "", 0, fmt.Errorf("frozen file content is unavailable")
	}
	patch := ""
	status := "full"
	if input.TargetMode == reviewcore.TargetModeDiff {
		marker := "+++ b/" + filePath + "\n"
		position := strings.Index(input.CanonicalPatch, marker)
		if position < 0 {
			return "", "", 0, fmt.Errorf("canonical patch omitted target file")
		}
		startMarker := strings.LastIndex(input.CanonicalPatch[:position], "\ndiff --git ")
		start := 0
		if startMarker >= 0 {
			start = startMarker + 1
		}
		nextOffset := strings.Index(input.CanonicalPatch[position+len(marker):], "\ndiff --git ")
		end := len(input.CanonicalPatch)
		if nextOffset >= 0 {
			end = position + len(marker) + nextOffset + 1
		}
		patch = input.CanonicalPatch[start:end]
		status = "modified"
		for _, line := range strings.Split(patch, "\n") {
			if strings.HasPrefix(line, "new file mode ") {
				status = "added"
				break
			}
			if strings.HasPrefix(line, "deleted file mode ") {
				status = "deleted"
				break
			}
		}
	} else {
		lines := strings.Split(*content, "\n")
		body := make([]string, len(lines))
		for index, line := range lines {
			body[index] = "+" + line
		}
		patch = strings.Join([]string{
			"diff --git a/" + filePath + " b/" + filePath,
			"new file mode 100644",
			"--- /dev/null",
			"+++ b/" + filePath,
			fmt.Sprintf("@@ -0,0 +1,%d @@", len(lines)),
			strings.Join(body, "\n"),
		}, "\n")
	}
	var changed uint64
	for _, line := range strings.Split(patch, "\n") {
		if (strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++")) ||
			(strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---")) {
			changed++
		}
	}
	if changed > uint64(^uint32(0)) {
		return "", "", 0, fmt.Errorf("changed line count overflows uint32")
	}
	return patch, status, uint32(changed), nil
}

// FrozenPiFilePatch is exposed for the existing exact-report fixture builder.
func FrozenPiFilePatch(
	input reviewcore.ReviewInput,
	path string,
) (string, string, uint32, error) {
	return frozenPiFilePatch(input, path)
}

func validateFrozenContextGaps(input reviewcore.ReviewInput, reported []string) error {
	return validateFrozenContextGapsWithAvailability(input, reported, nil)
}

func validateFrozenContextGapsWithAvailability(
	input reviewcore.ReviewInput,
	reported []string,
	availableContextIDs map[string]struct{},
) error {
	required := make(map[string]int, len(input.Contexts))
	for _, binding := range input.Contexts {
		var value string
		if binding.Ref != nil {
			if _, available := availableContextIDs[binding.Ref.ContextID]; available {
				continue
			}
			value = fmt.Sprintf(
				"context %s is reference-only and unavailable to the frozen-input worker",
				binding.Ref.ContextID,
			)
		} else {
			value = fmt.Sprintf(
				"context %s is unavailable: %s",
				binding.Gap.ContextID,
				binding.Gap.ReasonCode,
			)
		}
		required[value]++
	}
	for _, value := range reported {
		if required[value] > 0 {
			required[value]--
		}
	}
	for value, count := range required {
		if count != 0 {
			return fmt.Errorf("Pi report omitted required frozen context gap %q", value)
		}
	}
	return nil
}

type mappedPiCandidates struct {
	hypotheses            []contractsv1alpha1.ReviewHypothesis
	clusters              []contractsv1alpha1.HypothesisDedupCluster
	decisions             []contractsv1alpha1.HypothesisNormalizationDecision
	candidateByReportID   map[string]piCandidate
	candidateByOccurrence map[string]piCandidate
	occurrenceByReportID  map[string]string
	rawByID               map[string]piRawCandidate
	verificationRaw       map[string]json.RawMessage
}

func (mapped mappedPiCandidates) verificationRawByReportID(id string) ([]byte, bool) {
	value, exists := mapped.verificationRaw[id]
	return value, exists
}

func mapPiCandidates(
	plan contractsv1alpha1.AgentReviewPlan,
	input reviewcore.ReviewInput,
	report piReviewReport,
) (mappedPiCandidates, error) {
	mapped := mappedPiCandidates{
		hypotheses:            []contractsv1alpha1.ReviewHypothesis{},
		clusters:              []contractsv1alpha1.HypothesisDedupCluster{},
		decisions:             []contractsv1alpha1.HypothesisNormalizationDecision{},
		candidateByReportID:   make(map[string]piCandidate, len(report.Candidates)),
		candidateByOccurrence: make(map[string]piCandidate, len(report.Candidates)),
		occurrenceByReportID:  make(map[string]string, len(report.Candidates)),
		rawByID:               make(map[string]piRawCandidate, len(report.RawCandidates)),
		verificationRaw:       report.verificationRawByID,
	}
	groupIDs := make(map[string]struct{}, len(report.Execution.Snapshot.Grouping.Groups))
	for _, group := range report.Execution.Snapshot.Grouping.Groups {
		groupIDs[group.ID] = struct{}{}
	}
	for _, raw := range report.RawCandidates {
		if _, duplicate := mapped.rawByID[raw.RawCandidateID]; duplicate {
			return mappedPiCandidates{}, fmt.Errorf("duplicate raw candidate %q", raw.RawCandidateID)
		}
		if _, exists := groupIDs[raw.GroupID]; !exists {
			return mappedPiCandidates{}, fmt.Errorf("raw candidate %q escaped frozen groups", raw.RawCandidateID)
		}
		if _, exists := planReviewDimension(plan, raw.Skill); !exists {
			return mappedPiCandidates{}, fmt.Errorf("raw candidate %q escaped plan skills", raw.RawCandidateID)
		}
		mapped.rawByID[raw.RawCandidateID] = raw
	}
	for _, candidate := range report.Candidates {
		if _, duplicate := mapped.candidateByReportID[candidate.ID]; duplicate {
			return mappedPiCandidates{}, fmt.Errorf("duplicate normalized candidate %q", candidate.ID)
		}
		if _, exists := groupIDs[candidate.GroupID]; !exists {
			return mappedPiCandidates{}, fmt.Errorf("candidate %q escaped frozen groups", candidate.ID)
		}
		fingerprint, err := piClusterFingerprint(candidate.PiCandidateClaim)
		if err != nil {
			return mappedPiCandidates{}, err
		}
		if candidate.Fingerprint != fingerprint ||
			candidate.ID != "candidate-"+fingerprint[:16] {
			return mappedPiCandidates{}, fmt.Errorf("candidate %q fingerprint is not the host recomputation", candidate.ID)
		}
		occurrenceID := "occurrence-" + digestText(candidate.ID)[:24]
		mapped.candidateByReportID[candidate.ID] = candidate
		mapped.candidateByOccurrence[occurrenceID] = candidate
		mapped.occurrenceByReportID[candidate.ID] = occurrenceID
	}
	if len(mapped.candidateByReportID) > int(plan.Budget.MaxCandidates) {
		return mappedPiCandidates{}, fmt.Errorf("normalized candidates exceed the plan budget")
	}

	retained := make(map[string]struct{}, len(report.Candidates))
	seenDecisions := make(map[string]struct{}, len(report.NormalizationDecisions))
	for _, decision := range report.NormalizationDecisions {
		raw, exists := mapped.rawByID[decision.RawCandidateID]
		if !exists {
			return mappedPiCandidates{}, fmt.Errorf("normalization references unknown raw candidate %q", decision.RawCandidateID)
		}
		if _, duplicate := seenDecisions[decision.RawCandidateID]; duplicate {
			return mappedPiCandidates{}, fmt.Errorf("raw candidate %q has multiple decisions", decision.RawCandidateID)
		}
		seenDecisions[decision.RawCandidateID] = struct{}{}
		claimDigest, err := contractsv1alpha1.DigestAgentReviewRawCandidateClaim(
			mapPiRawCandidateClaim(raw.Claim),
		)
		if err != nil {
			return mappedPiCandidates{}, err
		}
		mappedDecision := contractsv1alpha1.HypothesisNormalizationDecision{
			RawCandidateID: raw.RawCandidateID,
			ClaimDigest:    claimDigest,
		}
		switch decision.Action {
		case "retained":
			if decision.ReasonCode != "normalized_candidate_retained" ||
				decision.NormalizedCandidateID == nil {
				return mappedPiCandidates{}, fmt.Errorf("retained decision %q is malformed", raw.RawCandidateID)
			}
			candidate, exists := mapped.candidateByReportID[*decision.NormalizedCandidateID]
			if !exists || !samePiCandidateClaim(raw, candidate) {
				return mappedPiCandidates{}, fmt.Errorf("retained candidate does not bind raw claim %q", raw.RawCandidateID)
			}
			if _, duplicate := retained[candidate.ID]; duplicate {
				return mappedPiCandidates{}, fmt.Errorf("candidate %q retained more than once", candidate.ID)
			}
			retained[candidate.ID] = struct{}{}
			occurrenceID := mapped.occurrenceByReportID[candidate.ID]
			mappedDecision.Action = contractsv1alpha1.HypothesisNormalizationRetained
			mappedDecision.ReasonCode = "normalized_candidate_retained"
			mappedDecision.OccurrenceID = &occurrenceID
			hypothesis, err := mapPiHypothesis(plan, input, occurrenceID, candidate)
			if err != nil {
				return mappedPiCandidates{}, err
			}
			mapped.hypotheses = append(mapped.hypotheses, hypothesis)
			mapped.clusters = append(mapped.clusters, contractsv1alpha1.HypothesisDedupCluster{
				ClusterID:             "cluster-" + hypothesis.ClusterFingerprint[:24],
				Fingerprint:           hypothesis.ClusterFingerprint,
				CanonicalOccurrenceID: occurrenceID,
				OccurrenceIDs:         []string{occurrenceID},
			})
		case "merged_duplicate":
			if (decision.ReasonCode != "duplicate_fingerprint" &&
				decision.ReasonCode != "semantic_duplicate") ||
				decision.NormalizedCandidateID == nil {
				return mappedPiCandidates{}, fmt.Errorf("duplicate decision %q is malformed", raw.RawCandidateID)
			}
			candidate, exists := mapped.candidateByReportID[*decision.NormalizedCandidateID]
			if !exists {
				return mappedPiCandidates{}, fmt.Errorf("duplicate decision points to unknown canonical candidate")
			}
			rawFingerprint, err := piClusterFingerprint(raw.Claim)
			if err != nil {
				return mappedPiCandidates{}, err
			}
			switch decision.ReasonCode {
			case "duplicate_fingerprint":
				if rawFingerprint != candidate.Fingerprint {
					return mappedPiCandidates{}, fmt.Errorf("duplicate decision is not the host fingerprint recomputation")
				}
			case "semantic_duplicate":
				if rawFingerprint == candidate.Fingerprint ||
					!piSemanticDuplicateForRevision(
						plan.Normalization.Revision,
						raw.GroupID,
						raw.Claim,
						candidate,
					) {
					return mappedPiCandidates{}, fmt.Errorf("semantic duplicate decision is not the host recomputation")
				}
			}
			occurrenceID := mapped.occurrenceByReportID[candidate.ID]
			mappedDecision.Action = contractsv1alpha1.HypothesisNormalizationMergedDuplicate
			mappedDecision.ReasonCode = decision.ReasonCode
			mappedDecision.CanonicalOccurrenceID = &occurrenceID
		case "rejected_invalid":
			if decision.ReasonCode != "invalid_candidate" || decision.NormalizedCandidateID != nil {
				return mappedPiCandidates{}, fmt.Errorf("invalid rejection decision %q", raw.RawCandidateID)
			}
			mappedDecision.Action = contractsv1alpha1.HypothesisNormalizationRejectedInvalid
			mappedDecision.ReasonCode = "invalid_candidate"
		case "excluded_budget":
			if decision.ReasonCode != "candidate_budget_exceeded" ||
				decision.NormalizedCandidateID != nil {
				return mappedPiCandidates{}, fmt.Errorf("invalid budget decision %q", raw.RawCandidateID)
			}
			mappedDecision.Action = contractsv1alpha1.HypothesisNormalizationExcludedBudget
			mappedDecision.ReasonCode = "candidate_budget_exceeded"
		default:
			return mappedPiCandidates{}, fmt.Errorf("unsupported normalization action %q", decision.Action)
		}
		mapped.decisions = append(mapped.decisions, mappedDecision)
	}
	if len(seenDecisions) != len(mapped.rawByID) || len(retained) != len(mapped.candidateByReportID) {
		return mappedPiCandidates{}, fmt.Errorf("normalization does not close raw and retained candidate lineage")
	}
	slices.SortFunc(mapped.decisions, func(left, right contractsv1alpha1.HypothesisNormalizationDecision) int {
		return strings.Compare(left.RawCandidateID, right.RawCandidateID)
	})
	slices.SortFunc(mapped.hypotheses, func(left, right contractsv1alpha1.ReviewHypothesis) int {
		return strings.Compare(left.OccurrenceID, right.OccurrenceID)
	})
	slices.SortFunc(mapped.clusters, func(left, right contractsv1alpha1.HypothesisDedupCluster) int {
		return strings.Compare(left.ClusterID, right.ClusterID)
	})
	if err := validatePiCandidateSummary(report, mapped); err != nil {
		return mappedPiCandidates{}, err
	}
	return mapped, nil
}

func mapPiRawCandidateCollection(
	plan contractsv1alpha1.AgentReviewPlan,
	set contractsv1alpha1.ReviewHypothesisSet,
	report piReviewReport,
) (contractsv1alpha1.AgentReviewRawCandidateCollection, error) {
	decisions := make(map[string]contractsv1alpha1.HypothesisNormalizationDecision, len(set.NormalizationDecisions))
	for _, decision := range set.NormalizationDecisions {
		decisions[decision.RawCandidateID] = decision
	}
	payloads := make([]contractsv1alpha1.AgentReviewRawCandidatePayload, 0, len(report.RawCandidates))
	for _, raw := range report.RawCandidates {
		decision, exists := decisions[raw.RawCandidateID]
		if !exists {
			return contractsv1alpha1.AgentReviewRawCandidateCollection{}, fmt.Errorf(
				"raw candidate %q has no host normalization decision", raw.RawCandidateID,
			)
		}
		dimension, exists := planReviewDimension(plan, raw.Skill)
		if !exists {
			return contractsv1alpha1.AgentReviewRawCandidateCollection{}, fmt.Errorf(
				"raw candidate %q escaped plan dimensions", raw.RawCandidateID,
			)
		}
		claim := mapPiRawCandidateClaim(raw.Claim)
		claimDigest, err := contractsv1alpha1.DigestAgentReviewRawCandidateClaim(claim)
		if err != nil {
			return contractsv1alpha1.AgentReviewRawCandidateCollection{}, err
		}
		if claimDigest != decision.ClaimDigest {
			return contractsv1alpha1.AgentReviewRawCandidateCollection{}, fmt.Errorf(
				"raw candidate %q payload does not bind its normalization digest", raw.RawCandidateID,
			)
		}
		payloads = append(payloads, contractsv1alpha1.AgentReviewRawCandidatePayload{
			RawCandidateID: raw.RawCandidateID, GroupID: raw.GroupID,
			Dimension: dimension, Ordinal: raw.Ordinal, ClaimDigest: claimDigest,
			Action: decision.Action, ReasonCode: decision.ReasonCode, Claim: claim,
		})
	}
	slices.SortFunc(payloads, func(left, right contractsv1alpha1.AgentReviewRawCandidatePayload) int {
		return strings.Compare(left.RawCandidateID, right.RawCandidateID)
	})
	collection := contractsv1alpha1.AgentReviewRawCandidateCollection{
		SchemaVersion: contractsv1alpha1.AgentReviewRawCandidateCollectionSchemaVersion,
		PlanID:        plan.PlanID, SourceRunID: plan.SourceRunID,
		ExecutionID: plan.ExecutionID, ReviewRunID: plan.ReviewRunID,
		TargetDigest:  plan.TargetDigest,
		Authority:     contractsv1alpha1.AgentReviewRawCandidateAuthorityWorkerSelfReport,
		Disposition:   contractsv1alpha1.AgentReviewRawCandidateDispositionShadowOnly,
		RawCandidates: payloads,
	}
	if err := collection.Validate(); err != nil {
		return contractsv1alpha1.AgentReviewRawCandidateCollection{}, fmt.Errorf(
			"validate mapped raw candidate collection: %w", err,
		)
	}
	return collection, nil
}

func mapPiRawCandidateClaim(claim piCandidateClaim) contractsv1alpha1.AgentReviewRawCandidateClaim {
	evidence := make([]contractsv1alpha1.AgentReviewRawCandidateEvidence, 0, len(claim.Evidence))
	for _, item := range claim.Evidence {
		evidence = append(evidence, contractsv1alpha1.AgentReviewRawCandidateEvidence{
			Statement: item.Statement, Anchor: mapPiRawCandidateAnchor(item.Anchor),
			Excerpt: item.Excerpt,
		})
	}
	return contractsv1alpha1.AgentReviewRawCandidateClaim{
		Category:         claim.Category,
		Severity:         contractsv1alpha1.HypothesisSeverity(claim.Severity),
		RawConfidencePPM: cloneUint32Pointer(claim.RawConfidencePPM),
		Title:            claim.Title, Description: claim.Description, Impact: claim.Impact,
		Anchor: mapPiRawCandidateAnchor(claim.Anchor), Evidence: evidence,
		Suggestion: cloneStringPointer(claim.Suggestion),
	}
}

func mapPiRawCandidateAnchor(anchor piSourceAnchor) contractsv1alpha1.AgentReviewRawCandidateSourceAnchor {
	return contractsv1alpha1.AgentReviewRawCandidateSourceAnchor{
		Path: anchor.Path, Side: contractsv1alpha1.HypothesisAnchorSide(anchor.Side),
		StartLine: anchor.StartLine, EndLine: anchor.EndLine,
	}
}

func mapPiHypothesis(
	plan contractsv1alpha1.AgentReviewPlan,
	input reviewcore.ReviewInput,
	occurrenceID string,
	candidate piCandidate,
) (contractsv1alpha1.ReviewHypothesis, error) {
	dimension, exists := planReviewDimension(plan, candidate.Skill)
	if !exists {
		return contractsv1alpha1.ReviewHypothesis{}, fmt.Errorf("candidate skill is outside the plan")
	}
	anchor, _, err := mapFrozenPiAnchor(input, candidate.Anchor, true)
	if err != nil {
		return contractsv1alpha1.ReviewHypothesis{}, err
	}
	evidence, err := mapPiEvidenceList(input, candidate.Evidence)
	if err != nil {
		return contractsv1alpha1.ReviewHypothesis{}, err
	}
	fingerprint, err := piClusterFingerprint(candidate.PiCandidateClaim)
	if err != nil {
		return contractsv1alpha1.ReviewHypothesis{}, err
	}
	return contractsv1alpha1.ReviewHypothesis{
		OccurrenceID: occurrenceID, ClusterFingerprint: fingerprint,
		GroupID: candidate.GroupID, Dimension: dimension,
		Category:               candidate.Category,
		Severity:               contractsv1alpha1.HypothesisSeverity(candidate.Severity),
		RawConfidenceAvailable: candidate.RawConfidencePPM != nil,
		RawConfidencePPM:       valueOrZero(candidate.RawConfidencePPM),
		Title:                  candidate.Title, Description: candidate.Description,
		Impact: candidate.Impact, Anchor: anchor, Evidence: evidence,
		Suggestion:   candidate.Suggestion,
		Verification: []contractsv1alpha1.HypothesisVerificationObservation{},
	}, nil
}

func cloneUint32Pointer(value *uint32) *uint32 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func valueOrZero(value *uint32) uint32 {
	if value == nil {
		return 0
	}
	return *value
}

func samePiCandidateClaim(raw piRawCandidate, candidate piCandidate) bool {
	return raw.GroupID == candidate.GroupID && raw.Skill == candidate.Skill &&
		reflect.DeepEqual(raw.Claim, candidate.PiCandidateClaim)
}

func validatePiCandidateSummary(report piReviewReport, mapped mappedPiCandidates) error {
	confirmed := uint32(0)
	rejected := uint32(0)
	inconclusive := uint32(0)
	confirmedIDs := make(map[string]struct{})
	for id, candidate := range mapped.candidateByReportID {
		if candidate.Verification == nil {
			continue
		}
		if candidate.Verification.CandidateID != id {
			return fmt.Errorf("candidate %q verification does not bind its identity", id)
		}
		switch candidate.Verification.Verdict {
		case "confirmed":
			confirmed++
			confirmedIDs[id] = struct{}{}
		case "rejected":
			rejected++
		case "inconclusive":
			inconclusive++
		default:
			return fmt.Errorf("candidate %q has unsupported verdict", id)
		}
	}
	if report.Summary.Candidates != uint32(len(mapped.candidateByReportID)) ||
		report.Summary.Confirmed != confirmed || report.Summary.Rejected != rejected ||
		report.Summary.Inconclusive != inconclusive {
		return fmt.Errorf("Pi report candidate summary is not the host recomputation")
	}
	seenFindings := make(map[string]struct{}, len(report.Findings))
	for _, finding := range report.Findings {
		candidate, exists := mapped.candidateByReportID[finding.ID]
		if !exists || candidate.Verification == nil ||
			candidate.Verification.Verdict != "confirmed" || !reflect.DeepEqual(candidate, finding) {
			return fmt.Errorf("Pi finding %q is not an exact confirmed candidate", finding.ID)
		}
		seenFindings[finding.ID] = struct{}{}
	}
	if !sameStringSet(seenFindings, confirmedIDs) {
		return fmt.Errorf("Pi findings do not equal confirmed candidate projection")
	}
	return nil
}

func validatePiCoverageProjection(
	plan contractsv1alpha1.AgentReviewPlan,
	report piReviewReport,
	mapped mappedPiCandidates,
	receipts []contractsv1alpha1.AgentExecutionReceipt,
) error {
	if !reflect.DeepEqual(report.Target.Skipped, report.Coverage.Skipped) {
		return fmt.Errorf("Pi target and coverage skipped projections differ")
	}
	groups := report.Execution.Snapshot.Grouping.Groups
	groupDimensions := make(map[string]map[contractsv1alpha1.VersionedRef]struct{})
	var reviewTotal uint32
	var reviewSucceeded uint32
	var verificationTotal uint32
	var verificationSucceeded uint32
	for _, receipt := range receipts {
		switch receipt.TaskRole {
		case contractsv1alpha1.AgentTaskReview:
			reviewTotal++
			if receipt.Status == contractsv1alpha1.AgentTaskSucceeded {
				reviewSucceeded++
				dimensions := groupDimensions[receipt.GroupID]
				if dimensions == nil {
					dimensions = make(map[contractsv1alpha1.VersionedRef]struct{})
					groupDimensions[receipt.GroupID] = dimensions
				}
				dimensions[receipt.Dimension] = struct{}{}
			}
		case contractsv1alpha1.AgentTaskVerification:
			verificationTotal++
			if receipt.Status == contractsv1alpha1.AgentTaskSucceeded {
				verificationSucceeded++
			}
		}
	}
	groupsReviewed := uint32(0)
	for _, group := range groups {
		if len(groupDimensions[group.ID]) == len(plan.ReviewDimensions) {
			groupsReviewed++
		}
	}
	coverage := report.Coverage
	if coverage.GroupsTotal != uint32(len(groups)) ||
		coverage.GroupsReviewed != groupsReviewed ||
		coverage.ReviewTasksTotal != reviewTotal ||
		coverage.ReviewTasksSucceeded != reviewSucceeded ||
		coverage.VerificationTasksTotal != uint32(len(mapped.candidateByReportID)) ||
		coverage.VerificationTasksTotal != verificationTotal ||
		coverage.VerificationTasksSucceeded != verificationSucceeded {
		return fmt.Errorf("Pi coverage counters are not the host receipt recomputation")
	}
	return nil
}

type mappedVerificationReceipt struct {
	receiptStatus contractsv1alpha1.AgentTaskStatus
}

func mapPiReceipts(
	plan contractsv1alpha1.AgentReviewPlan,
	completedAt time.Time,
	report piReviewReport,
	candidates mappedPiCandidates,
) (
	[]contractsv1alpha1.AgentExecutionReceipt,
	map[string]mappedVerificationReceipt,
	[]contractsv1alpha1.AgentReviewCoverageGap,
	error,
) {
	receipts := make([]contractsv1alpha1.AgentExecutionReceipt, 0, len(report.Execution.Tasks))
	verification := make(map[string]mappedVerificationReceipt)
	gaps := []contractsv1alpha1.AgentReviewCoverageGap{}
	seenTasks := make(map[string]struct{}, len(report.Execution.Tasks))
	groupIDs := make(map[string]struct{}, len(report.Execution.Snapshot.Grouping.Groups))
	for _, group := range report.Execution.Snapshot.Grouping.Groups {
		groupIDs[group.ID] = struct{}{}
	}
	for _, task := range report.Execution.Tasks {
		if _, duplicate := seenTasks[task.TaskID]; duplicate {
			return nil, nil, nil, fmt.Errorf("duplicate Pi task %q", task.TaskID)
		}
		seenTasks[task.TaskID] = struct{}{}
		if _, exists := groupIDs[task.GroupID]; !exists {
			return nil, nil, nil, fmt.Errorf("Pi task %q escaped frozen groups", task.TaskID)
		}
		receipt, occurrenceID, err := mapPiReceipt(plan, completedAt, task, candidates)
		if err != nil {
			return nil, nil, nil, err
		}
		receipts = append(receipts, receipt)
		if receipt.Status != contractsv1alpha1.AgentTaskSucceeded {
			subject := task.TaskID
			if occurrenceID != "" {
				subject = occurrenceID
			}
			gaps = append(gaps, makeCoverageGap(
				contractsv1alpha1.AgentReviewCoveragePhase(receipt.TaskRole),
				subject,
				*receipt.FailureReasonCode,
			))
		}
		if receipt.TaskRole == contractsv1alpha1.AgentTaskVerification {
			if _, duplicate := verification[occurrenceID]; duplicate {
				return nil, nil, nil, fmt.Errorf("candidate occurrence has multiple verifier tasks")
			}
			verification[occurrenceID] = mappedVerificationReceipt{receiptStatus: receipt.Status}
		}
	}
	slices.SortFunc(receipts, func(left, right contractsv1alpha1.AgentExecutionReceipt) int {
		if compared := strings.Compare(left.TaskID, right.TaskID); compared != 0 {
			return compared
		}
		return strings.Compare(left.ReceiptID, right.ReceiptID)
	})
	return receipts, verification, gaps, nil
}

// mapPiDiagnosticReceiptCollection validates the task-execution account
// without first admitting business candidate semantics. Verification tasks
// still bind a stable occurrence derived from their exact worker candidate
// ID and, when succeeded, the exact verdict output digest. Candidate claim
// fingerprints, normalization decisions, anchors, and evidence remain owned
// by the later business mapper and may fail independently.
func mapPiDiagnosticReceiptCollection(
	plan contractsv1alpha1.AgentReviewPlan,
	completedAt time.Time,
	report piReviewReport,
) (contractsv1alpha1.AgentExecutionReceiptCollection, error) {
	candidates := mappedPiCandidates{
		candidateByReportID:   make(map[string]piCandidate, len(report.Candidates)),
		candidateByOccurrence: make(map[string]piCandidate, len(report.Candidates)),
		occurrenceByReportID:  make(map[string]string, len(report.Candidates)),
		verificationRaw:       report.verificationRawByID,
	}
	groupIDs := make(map[string]struct{}, len(report.Execution.Snapshot.Grouping.Groups))
	for _, group := range report.Execution.Snapshot.Grouping.Groups {
		groupIDs[group.ID] = struct{}{}
	}
	for _, candidate := range report.Candidates {
		if _, duplicate := candidates.candidateByReportID[candidate.ID]; duplicate {
			return contractsv1alpha1.AgentExecutionReceiptCollection{},
				fmt.Errorf("duplicate diagnostic candidate %q", candidate.ID)
		}
		if _, exists := groupIDs[candidate.GroupID]; !exists {
			return contractsv1alpha1.AgentExecutionReceiptCollection{},
				fmt.Errorf("diagnostic candidate %q escaped frozen groups", candidate.ID)
		}
		occurrenceID := "occurrence-" + digestText(candidate.ID)[:24]
		candidates.candidateByReportID[candidate.ID] = candidate
		candidates.candidateByOccurrence[occurrenceID] = candidate
		candidates.occurrenceByReportID[candidate.ID] = occurrenceID
	}
	if len(candidates.candidateByReportID) > int(plan.Budget.MaxCandidates) {
		return contractsv1alpha1.AgentExecutionReceiptCollection{},
			fmt.Errorf("diagnostic candidates exceed the plan budget")
	}
	receipts, _, _, err := mapPiReceipts(plan, normalizedUTC(completedAt), report, candidates)
	if err != nil {
		return contractsv1alpha1.AgentExecutionReceiptCollection{}, err
	}
	collection := contractsv1alpha1.AgentExecutionReceiptCollection{
		SchemaVersion: contractsv1alpha1.AgentExecutionReceiptCollectionSchemaVersion,
		PlanID:        plan.PlanID, SourceRunID: plan.SourceRunID,
		ExecutionID: plan.ExecutionID, ReviewRunID: plan.ReviewRunID,
		Receipts: receipts,
	}
	if err := collection.Validate(); err != nil {
		return contractsv1alpha1.AgentExecutionReceiptCollection{}, err
	}
	return collection, nil
}

func mapPiReceipt(
	plan contractsv1alpha1.AgentReviewPlan,
	completedAt time.Time,
	task piTaskObservation,
	candidates mappedPiCandidates,
) (contractsv1alpha1.AgentExecutionReceipt, string, error) {
	var role contractsv1alpha1.AgentTaskRole
	var dimension contractsv1alpha1.VersionedRef
	var occurrenceID string
	switch task.TaskKind {
	case "context":
		if task.SkillID != nil || task.CandidateID != nil || len(plan.ContextDimensions) != 1 {
			return contractsv1alpha1.AgentExecutionReceipt{}, "", fmt.Errorf("context task %q has invalid identity", task.TaskID)
		}
		role = contractsv1alpha1.AgentTaskContext
		dimension = plan.ContextDimensions[0]
	case "review":
		if task.SkillID == nil || task.CandidateID != nil {
			return contractsv1alpha1.AgentExecutionReceipt{}, "", fmt.Errorf("review task %q has invalid identity", task.TaskID)
		}
		var exists bool
		dimension, exists = planReviewDimensionByID(plan, *task.SkillID)
		if !exists {
			return contractsv1alpha1.AgentExecutionReceipt{}, "", fmt.Errorf("review task skill is outside the plan")
		}
		role = contractsv1alpha1.AgentTaskReview
	case "verification":
		if task.SkillID != nil || task.CandidateID == nil {
			return contractsv1alpha1.AgentExecutionReceipt{}, "", fmt.Errorf("verification task %q has invalid identity", task.TaskID)
		}
		var exists bool
		occurrenceID, exists = candidates.occurrenceByReportID[*task.CandidateID]
		if !exists {
			return contractsv1alpha1.AgentExecutionReceipt{}, "", fmt.Errorf("verification task points to unknown candidate")
		}
		role = contractsv1alpha1.AgentTaskVerification
		dimension = plan.Verifier
	default:
		return contractsv1alpha1.AgentExecutionReceipt{}, "", fmt.Errorf("unsupported Pi task kind %q", task.TaskKind)
	}
	status, reason, err := mapPiTaskStatus(task.TerminalStatus, task.ErrorCode)
	if err != nil {
		return contractsv1alpha1.AgentExecutionReceipt{}, "", err
	}
	startedAt := normalizedUTC(task.StartedAt)
	finishedAt := normalizedUTC(task.FinishedAt)
	if startedAt.Before(plan.CreatedAt) || finishedAt.Before(startedAt) ||
		finishedAt.After(completedAt) {
		return contractsv1alpha1.AgentExecutionReceipt{}, "", fmt.Errorf("Pi task %q timing is outside worker execution", task.TaskID)
	}
	usage, err := mapPiUsage(task.Usage)
	if err != nil {
		return contractsv1alpha1.AgentExecutionReceipt{}, "", err
	}
	toolUsage := make([]contractsv1alpha1.AgentToolUsage, 0, len(task.ToolUsage))
	toolNames := make([]string, 0, len(task.ToolUsage))
	var calls uint64
	for _, item := range task.ToolUsage {
		toolUsage = append(toolUsage, contractsv1alpha1.AgentToolUsage{
			ToolID: item.ToolID, InvocationCount: item.InvocationCount,
			FailureCount: item.FailureCount,
		})
		if item.InvocationCount > 0 {
			toolNames = append(toolNames, item.ToolID)
		}
		calls += uint64(item.InvocationCount)
	}
	slices.SortFunc(toolUsage, func(left, right contractsv1alpha1.AgentToolUsage) int {
		return strings.Compare(left.ToolID, right.ToolID)
	})
	slices.Sort(toolNames)
	if calls != uint64(task.ToolCalls) || !slices.Equal(toolNames, task.ToolNames) {
		return contractsv1alpha1.AgentExecutionReceipt{}, "", fmt.Errorf("Pi task %q tool usage is inconsistent", task.TaskID)
	}
	promptDigest, err := optionalExternalDigest(task.PromptDigest)
	if err != nil {
		return contractsv1alpha1.AgentExecutionReceipt{}, "", err
	}
	outputDigest, err := optionalExternalDigestPointer(task.OutputDigest)
	if err != nil {
		return contractsv1alpha1.AgentExecutionReceipt{}, "", err
	}
	if status != contractsv1alpha1.AgentTaskSucceeded {
		if outputDigest != nil {
			return contractsv1alpha1.AgentExecutionReceipt{}, "", fmt.Errorf(
				"non-succeeded Pi task %q contains outputDigest",
				task.TaskID,
			)
		}
	} else if role == contractsv1alpha1.AgentTaskVerification {
		candidateID := *task.CandidateID
		rawVerification, exists := candidates.candidateByReportID[candidateID]
		if !exists || rawVerification.Verification == nil {
			return contractsv1alpha1.AgentExecutionReceipt{}, "", fmt.Errorf(
				"succeeded verifier task %q has no typed verdict",
				task.TaskID,
			)
		}
		raw, exists := candidates.verificationRawByReportID(candidateID)
		if !exists || outputDigest == nil || *outputDigest != digestBytesRaw(raw) {
			return contractsv1alpha1.AgentExecutionReceipt{}, "", fmt.Errorf(
				"verifier task %q output digest does not bind its exact verdict JSON",
				task.TaskID,
			)
		}
	}
	receipt := contractsv1alpha1.AgentExecutionReceipt{
		SchemaVersion: contractsv1alpha1.AgentExecutionReceiptSchemaVersion,
		ReceiptID:     "receipt-" + digestText(plan.PlanID + "\x00" + task.TaskID)[:24],
		PlanID:        plan.PlanID, SourceRunID: plan.SourceRunID,
		ExecutionID: plan.ExecutionID, ReviewRunID: plan.ReviewRunID,
		TaskID: task.TaskID, GroupID: task.GroupID,
		TaskRole: role, Dimension: dimension, Runtime: plan.Runtime,
		Profile: plan.Profile, Agent: plan.Agent, Provider: plan.Provider,
		Model: plan.Model, APIProtocol: plan.APIProtocol,
		ProvenanceClass: contractsv1alpha1.AgentReceiptProvenanceWorkerSelfReport,
		Authority:       contractsv1alpha1.AgentReceiptAuthorityDiagnosticOnly,
		Status:          status, FailureReasonCode: reason,
		StartedAt: startedAt, FinishedAt: finishedAt,
		ModelTurnsStarted:   task.ProviderTurnsStarted,
		ModelTurnsCompleted: task.ProviderTurnsCompleted,
		ToolCalls:           task.ToolCalls, ToolUsage: toolUsage,
		Usage: usage, PromptDigest: promptDigest, OutputDigest: outputDigest,
	}
	if occurrenceID != "" {
		receipt.HypothesisOccurrenceID = &occurrenceID
	}
	if err := receipt.Validate(); err != nil {
		return contractsv1alpha1.AgentExecutionReceipt{}, "", fmt.Errorf("map Pi task %q: %w", task.TaskID, err)
	}
	return receipt, occurrenceID, nil
}

func mapPiTaskStatus(
	status string,
	errorCode *string,
) (contractsv1alpha1.AgentTaskStatus, *string, error) {
	switch status {
	case "succeeded":
		if errorCode != nil {
			return "", nil, fmt.Errorf("succeeded Pi task contains error_code")
		}
		return contractsv1alpha1.AgentTaskSucceeded, nil, nil
	case "aborted", "canceled":
		reason := "aborted"
		if errorCode != nil {
			reason = *errorCode
		}
		return contractsv1alpha1.AgentTaskCanceled, &reason, nil
	case "timeout", "failed":
		reason := status
		if errorCode != nil {
			reason = *errorCode
		}
		return contractsv1alpha1.AgentTaskFailed, &reason, nil
	default:
		return "", nil, fmt.Errorf("unsupported Pi task status %q", status)
	}
}

func mapPiUsage(usage piUsage) (contractsv1alpha1.AgentTokenUsage, error) {
	mapped := contractsv1alpha1.AgentTokenUsage{
		Completeness:          contractsv1alpha1.AgentTokenUsageCompleteness(usage.Completeness),
		UnavailableReasonCode: cloneStringPointer(usage.UnavailableReasonCode),
		ReasoningTokens:       cloneUint64Pointer(usage.ReasoningTokens),
	}
	switch mapped.Completeness {
	case contractsv1alpha1.AgentTokenUsageProviderReported,
		contractsv1alpha1.AgentTokenUsagePartial:
		if usage.InputTokens == nil || usage.OutputTokens == nil ||
			usage.CacheReadTokens == nil || usage.CacheWriteTokens == nil ||
			usage.TotalTokens == nil {
			return contractsv1alpha1.AgentTokenUsage{}, fmt.Errorf("observed Pi usage omitted token counters")
		}
		mapped.InputTokens = *usage.InputTokens
		mapped.OutputTokens = *usage.OutputTokens
		mapped.CacheReadTokens = *usage.CacheReadTokens
		mapped.CacheWriteTokens = *usage.CacheWriteTokens
		mapped.TotalTokens = *usage.TotalTokens
	case contractsv1alpha1.AgentTokenUsageUnavailable:
		if usage.InputTokens != nil || usage.OutputTokens != nil || usage.CacheReadTokens != nil ||
			usage.CacheWriteTokens != nil || usage.ReasoningTokens != nil || usage.TotalTokens != nil {
			return contractsv1alpha1.AgentTokenUsage{}, fmt.Errorf("unavailable Pi usage contains observed counters")
		}
	default:
		return contractsv1alpha1.AgentTokenUsage{}, fmt.Errorf("unsupported Pi usage completeness")
	}
	return mapped, nil
}

func mapPiVerification(
	plan contractsv1alpha1.AgentReviewPlan,
	input reviewcore.ReviewInput,
	occurrenceID string,
	verification piVerification,
) (
	contractsv1alpha1.HypothesisVerificationObservation,
	[]contractsv1alpha1.HypothesisEvidence,
	error,
) {
	evidence, err := mapPiEvidenceList(input, verification.Evidence)
	if err != nil {
		return contractsv1alpha1.HypothesisVerificationObservation{}, nil, err
	}
	observation := contractsv1alpha1.HypothesisVerificationObservation{
		ObservationID: "verification-" + digestText(occurrenceID + "\x00" + verification.Verdict + "\x00" + verification.ReasonCode)[:24],
		Sequence:      1, Verifier: plan.Verifier,
		Verdict:    contractsv1alpha1.HypothesisVerificationVerdict(verification.Verdict),
		ReasonCode: verification.ReasonCode, Explanation: verification.Explanation,
		EvidenceIDs: []string{},
	}
	return observation, evidence, nil
}

func mapPiEvidenceList(
	input reviewcore.ReviewInput,
	items []piEvidence,
) ([]contractsv1alpha1.HypothesisEvidence, error) {
	if items == nil {
		return nil, fmt.Errorf("Pi evidence must be an explicit array")
	}
	mapped := make(map[string]contractsv1alpha1.HypothesisEvidence, len(items))
	for _, item := range items {
		anchor, excerpt, err := mapFrozenPiAnchor(input, item.Anchor, false)
		if err != nil {
			return nil, err
		}
		if item.Excerpt != excerpt {
			return nil, fmt.Errorf("Pi evidence excerpt does not equal frozen source")
		}
		evidenceID := "evidence-" + digestText(
			item.Statement + "\x00" + anchor.Path + "\x00" + string(anchor.Side) + "\x00" +
				fmt.Sprintf("%d:%d", anchor.StartLine, anchor.EndLine) + "\x00" + excerpt,
		)[:24]
		evidence := contractsv1alpha1.HypothesisEvidence{
			EvidenceID: evidenceID, Statement: item.Statement,
			Anchor: anchor, Excerpt: excerpt,
		}
		evidence.EvidenceDigest, err = contractsv1alpha1.DigestHypothesisEvidence(evidence)
		if err != nil {
			return nil, err
		}
		if existing, duplicate := mapped[evidenceID]; duplicate && existing != evidence {
			return nil, fmt.Errorf("Pi evidence ID collision")
		}
		mapped[evidenceID] = evidence
	}
	result := make([]contractsv1alpha1.HypothesisEvidence, 0, len(mapped))
	for _, evidence := range mapped {
		result = append(result, evidence)
	}
	slices.SortFunc(result, func(left, right contractsv1alpha1.HypothesisEvidence) int {
		return strings.Compare(left.EvidenceID, right.EvidenceID)
	})
	return result, nil
}

func mapFrozenPiAnchor(
	input reviewcore.ReviewInput,
	anchor piSourceAnchor,
	requireTarget bool,
) (contractsv1alpha1.HypothesisSourceAnchor, string, error) {
	if anchor.Side == "old" {
		return contractsv1alpha1.HypothesisSourceAnchor{}, "", fmt.Errorf("old-side Pi evidence is outside frozen ReviewInput")
	}
	if anchor.Side != "new" && anchor.Side != "file" {
		return contractsv1alpha1.HypothesisSourceAnchor{}, "", fmt.Errorf("unsupported Pi anchor side %q", anchor.Side)
	}
	wantSide := "file"
	if input.TargetMode == reviewcore.TargetModeDiff {
		wantSide = "new"
	}
	// The frozen transport has one target-side content body. Pi accepts the
	// provider-facing new/file aliases, but Argus emits the exact canonical side
	// required by the frozen ReviewInput contract before deriving evidence IDs.
	anchor.Side = wantSide
	for _, file := range input.Files {
		if file.Path != anchor.Path {
			continue
		}
		if file.Content == nil {
			return contractsv1alpha1.HypothesisSourceAnchor{}, "", fmt.Errorf("Pi anchor %q has no frozen content", anchor.Path)
		}
		excerpt, ok := exactFrozenExcerpt(*file.Content, anchor.StartLine, anchor.EndLine)
		if !ok {
			return contractsv1alpha1.HypothesisSourceAnchor{}, "", fmt.Errorf("Pi anchor %q exceeds frozen content", anchor.Path)
		}
		if requireTarget && !reviewcore.InputAuthorizesAnchor(
			context.Background(),
			input,
			anchor.Path,
			anchor.StartLine,
			anchor.EndLine,
		) {
			return contractsv1alpha1.HypothesisSourceAnchor{}, "", fmt.Errorf("Pi candidate anchor is outside frozen target authorization")
		}
		return contractsv1alpha1.HypothesisSourceAnchor{
			Path: anchor.Path, Side: contractsv1alpha1.HypothesisAnchorSide(anchor.Side),
			StartLine: anchor.StartLine, EndLine: anchor.EndLine,
			SourceDigest: file.SHA256,
		}, excerpt, nil
	}
	return contractsv1alpha1.HypothesisSourceAnchor{}, "", fmt.Errorf("Pi anchor %q escaped frozen ReviewInput", anchor.Path)
}

func reportCoverageGaps(
	plan contractsv1alpha1.AgentReviewPlan,
	input reviewcore.ReviewInput,
	report piReviewReport,
	mapped mappedPiCandidates,
	availableContextIDs map[string]struct{},
) []contractsv1alpha1.AgentReviewCoverageGap {
	gaps := []contractsv1alpha1.AgentReviewCoverageGap{}
	if report.Coverage.StaleTarget {
		gaps = append(gaps, makeCoverageGap(contractsv1alpha1.AgentReviewCoverageCapture, plan.PlanID, "stale_target_reported"))
	}
	for index, skipped := range report.Target.Skipped {
		gaps = append(gaps, makeCoverageGap(
			contractsv1alpha1.AgentReviewCoverageCapture,
			skipped.Path,
			fmt.Sprintf("target_skipped_%d", index+1),
		))
	}
	for index := range report.Coverage.ContextGaps {
		gaps = append(gaps, makeCoverageGap(
			contractsv1alpha1.AgentReviewCoverageContext,
			plan.PlanID,
			fmt.Sprintf("context_gap_reported_%d", index+1),
		))
	}
	for _, binding := range input.Contexts {
		contextID := binding.ContextID()
		reason := "frozen_context_reference_unavailable"
		if binding.Gap != nil {
			reason = binding.Gap.ReasonCode
		} else if _, available := availableContextIDs[contextID]; available {
			continue
		}
		gaps = append(gaps, makeCoverageGap(
			contractsv1alpha1.AgentReviewCoverageContext,
			contextID,
			reason,
		))
	}
	for index, failure := range report.Coverage.Failures {
		phase := contractsv1alpha1.AgentReviewCoveragePhase(failure.Phase)
		if phase != contractsv1alpha1.AgentReviewCoverageContext &&
			phase != contractsv1alpha1.AgentReviewCoverageReview &&
			phase != contractsv1alpha1.AgentReviewCoverageVerification {
			phase = contractsv1alpha1.AgentReviewCoverageReview
		}
		subject := failure.GroupID
		if failure.CandidateID != nil {
			if occurrence, exists := mapped.occurrenceByReportID[*failure.CandidateID]; exists {
				subject = occurrence
			}
		}
		gaps = append(gaps, makeCoverageGap(
			phase,
			subject,
			fmt.Sprintf("worker_reported_failure_%d", index+1),
		))
	}
	for _, decision := range mapped.decisions {
		switch decision.Action {
		case contractsv1alpha1.HypothesisNormalizationRejectedInvalid:
			gaps = append(gaps, makeCoverageGap(
				contractsv1alpha1.AgentReviewCoverageReview,
				decision.RawCandidateID,
				"invalid_candidate",
			))
		case contractsv1alpha1.HypothesisNormalizationExcludedBudget:
			gaps = append(gaps, makeCoverageGap(
				contractsv1alpha1.AgentReviewCoverageReview,
				decision.RawCandidateID,
				"candidate_budget_exceeded",
			))
		}
	}
	return gaps
}

func makeCoverageGap(
	phase contractsv1alpha1.AgentReviewCoveragePhase,
	subject string,
	reason string,
) contractsv1alpha1.AgentReviewCoverageGap {
	return contractsv1alpha1.AgentReviewCoverageGap{
		GapID: "gap-" + digestText(string(phase) + "\x00" + subject + "\x00" + reason)[:24],
		Phase: phase, SubjectID: subject, ReasonCode: reason,
	}
}

func uniqueSortedCoverageGaps(
	values []contractsv1alpha1.AgentReviewCoverageGap,
) []contractsv1alpha1.AgentReviewCoverageGap {
	unique := make(map[string]contractsv1alpha1.AgentReviewCoverageGap, len(values))
	for _, value := range values {
		unique[value.GapID] = value
	}
	result := make([]contractsv1alpha1.AgentReviewCoverageGap, 0, len(unique))
	for _, value := range unique {
		result = append(result, value)
	}
	slices.SortFunc(result, func(left, right contractsv1alpha1.AgentReviewCoverageGap) int {
		return strings.Compare(left.GapID, right.GapID)
	})
	return result
}

func coverageReasonCodes(gaps []contractsv1alpha1.AgentReviewCoverageGap) []string {
	unique := make(map[string]struct{}, len(gaps))
	for _, gap := range gaps {
		unique[gap.ReasonCode] = struct{}{}
	}
	reasons := make([]string, 0, len(unique))
	for reason := range unique {
		reasons = append(reasons, reason)
	}
	slices.Sort(reasons)
	return reasons
}

func mergeHypothesisEvidence(
	left []contractsv1alpha1.HypothesisEvidence,
	right []contractsv1alpha1.HypothesisEvidence,
) ([]contractsv1alpha1.HypothesisEvidence, error) {
	unique := make(map[string]contractsv1alpha1.HypothesisEvidence, len(left)+len(right))
	for _, evidence := range append(slices.Clone(left), right...) {
		if existing, duplicate := unique[evidence.EvidenceID]; duplicate && existing != evidence {
			return nil, fmt.Errorf("hypothesis evidence ID collision")
		}
		unique[evidence.EvidenceID] = evidence
	}
	result := make([]contractsv1alpha1.HypothesisEvidence, 0, len(unique))
	for _, evidence := range unique {
		result = append(result, evidence)
	}
	slices.SortFunc(result, func(left, right contractsv1alpha1.HypothesisEvidence) int {
		return strings.Compare(left.EvidenceID, right.EvidenceID)
	})
	return result, nil
}

func verificationEvidenceIDs(evidence []contractsv1alpha1.HypothesisEvidence) []string {
	result := make([]string, 0, len(evidence))
	for _, item := range evidence {
		result = append(result, item.EvidenceID)
	}
	slices.Sort(result)
	return result
}

func planReviewDimension(
	plan contractsv1alpha1.AgentReviewPlan,
	skill piSkillRef,
) (contractsv1alpha1.VersionedRef, bool) {
	for _, dimension := range plan.ReviewDimensions {
		if dimension.ID == skill.ID && dimension.Revision == skill.Revision {
			return dimension, true
		}
	}
	return contractsv1alpha1.VersionedRef{}, false
}

// Task observations carry the governed skill ID while the separately validated
// execution snapshot carries its exact revision and digest. Resolve the task to
// the unique frozen plan entry instead of assuming a built-in revision; custom
// skill artifact replays intentionally change that revision.
func planReviewDimensionByID(
	plan contractsv1alpha1.AgentReviewPlan,
	id string,
) (contractsv1alpha1.VersionedRef, bool) {
	for _, dimension := range plan.ReviewDimensions {
		if dimension.ID == id {
			return dimension, true
		}
	}
	return contractsv1alpha1.VersionedRef{}, false
}

func samePiKnowledge(
	planned []contractsv1alpha1.VersionedRef,
	observed []piSnapshotKnowledge,
) bool {
	if len(planned) != len(observed) {
		return false
	}
	for index := range planned {
		if observed[index].ID != planned[index].ID ||
			observed[index].Digest != "sha256:"+planned[index].SHA256 ||
			observed[index].Bytes == 0 ||
			observed[index].Bytes > contractsv1alpha1.AgentReviewKnowledgeMaxBytes {
			return false
		}
	}
	return true
}

func piClusterFingerprint(claim piCandidateClaim) (string, error) {
	return digestJSON([]any{
		claim.Anchor.Path,
		claim.Anchor.Side,
		claim.Anchor.StartLine,
		claim.Anchor.EndLine,
		strings.ToLower(strings.TrimSpace(claim.Category)),
		strings.Join(strings.Fields(strings.ToLower(claim.Title)), " "),
	})
}

var piSemanticTitleStopWords = map[string]struct{}{
	"a": {}, "an": {}, "and": {}, "are": {}, "as": {}, "at": {},
	"be": {}, "been": {}, "being": {}, "before": {}, "but": {}, "by": {},
	"for": {}, "from": {}, "in": {}, "instead": {}, "into": {}, "is": {},
	"it": {}, "of": {}, "on": {}, "or": {}, "than": {}, "that": {},
	"the": {}, "then": {}, "this": {}, "to": {}, "was": {}, "were": {},
	"when": {}, "while": {}, "without": {}, "with": {},
}

var piSemanticTitleAliases = map[string]string{
	"accept":      "permit",
	"accepted":    "permit",
	"accepting":   "permit",
	"allow":       "permit",
	"allowed":     "permit",
	"allowing":    "permit",
	"applied":     "apply",
	"applies":     "apply",
	"applying":    "apply",
	"approval":    "decision_state",
	"approvals":   "decision_state",
	"approved":    "decision_state",
	"decided":     "decision",
	"decisions":   "decision",
	"expected":    "expect",
	"expecting":   "expect",
	"expects":     "expect",
	"overwrites":  "overwrite",
	"overwriting": "overwrite",
	"overwritten": "overwrite",
	"rejected":    "decision_state",
	"rejecting":   "decision_state",
	"throws":      "throw",
	"throwing":    "throw",
	"transitions": "transition",
}

func piSemanticDuplicateForRevision(
	revision string,
	rawGroupID string,
	raw piCandidateClaim,
	canonical piCandidate,
) bool {
	if rawGroupID != canonical.GroupID ||
		raw.Anchor.Path != canonical.Anchor.Path ||
		!piSameTargetSide(raw.Anchor.Side, canonical.Anchor.Side) ||
		raw.Anchor.StartLine > canonical.Anchor.EndLine ||
		canonical.Anchor.StartLine > raw.Anchor.EndLine {
		return false
	}
	left := piSemanticTitleTokens(raw.Title)
	right := piSemanticTitleTokens(canonical.Title)
	if len(left) < 4 || len(right) < 4 {
		return false
	}
	rightSet := make(map[string]struct{}, len(right))
	union := make(map[string]struct{}, len(left)+len(right))
	for _, token := range right {
		rightSet[token] = struct{}{}
		union[token] = struct{}{}
	}
	intersection := 0
	for _, token := range left {
		union[token] = struct{}{}
		if _, exists := rightSet[token]; exists {
			intersection++
		}
	}
	if intersection >= 4 && intersection*4 >= len(union)*3 {
		return true
	}
	if revision == contractsv1alpha1.CandidateNormalizationRevisionV0 {
		return false
	}
	leftIdentifiers := piSemanticTitleIdentifiers(raw.Title)
	rightIdentifiers := make(map[string]struct{})
	for _, identifier := range piSemanticTitleIdentifiers(canonical.Title) {
		rightIdentifiers[identifier] = struct{}{}
	}
	sharedIdentifier := false
	for _, identifier := range leftIdentifiers {
		if _, exists := rightIdentifiers[identifier]; exists {
			sharedIdentifier = true
			break
		}
	}
	if !sharedIdentifier || !piCandidatesShareEvidenceRange(raw, canonical.PiCandidateClaim) {
		return false
	}
	if intersection >= 4 && intersection*2 >= min(len(left), len(right)) {
		return true
	}
	if revision == contractsv1alpha1.CandidateNormalizationRevisionV1 {
		return false
	}
	leftDescription := piSemanticTitleTokens(raw.Description)
	rightDescription := piSemanticTitleTokens(canonical.Description)
	if len(leftDescription) < 12 || len(rightDescription) < 12 {
		return false
	}
	rightDescriptionSet := make(map[string]struct{}, len(rightDescription))
	for _, token := range rightDescription {
		rightDescriptionSet[token] = struct{}{}
	}
	descriptionIntersection := 0
	for _, token := range leftDescription {
		if _, exists := rightDescriptionSet[token]; exists {
			descriptionIntersection++
		}
	}
	return descriptionIntersection >= 12 &&
		descriptionIntersection*20 >= min(len(leftDescription), len(rightDescription))*11
}

func piSemanticDuplicate(rawGroupID string, raw piCandidateClaim, canonical piCandidate) bool {
	return piSemanticDuplicateForRevision(
		contractsv1alpha1.CandidateNormalizationCurrentRevision,
		rawGroupID,
		raw,
		canonical,
	)
}

func piCandidatesShareEvidenceRange(left, right piCandidateClaim) bool {
	for _, leftEvidence := range left.Evidence {
		for _, rightEvidence := range right.Evidence {
			if leftEvidence.Anchor.Path == rightEvidence.Anchor.Path &&
				piSameTargetSide(leftEvidence.Anchor.Side, rightEvidence.Anchor.Side) &&
				leftEvidence.Anchor.StartLine <= rightEvidence.Anchor.EndLine &&
				rightEvidence.Anchor.StartLine <= leftEvidence.Anchor.EndLine {
				return true
			}
		}
	}
	return false
}

func piSemanticTitleIdentifiers(title string) []string {
	unique := make(map[string]struct{})
	current := make([]byte, 0, len(title))
	hasLowerToUpperBoundary := false
	hasUnderscore := false
	flush := func() {
		if len(current) > 1 && (hasLowerToUpperBoundary || hasUnderscore) {
			unique[strings.ToLower(string(current))] = struct{}{}
		}
		current = current[:0]
		hasLowerToUpperBoundary = false
		hasUnderscore = false
	}
	for _, value := range []byte(title) {
		isLower := value >= 'a' && value <= 'z'
		isUpper := value >= 'A' && value <= 'Z'
		isDigit := value >= '0' && value <= '9'
		if isLower || isUpper || isDigit || value == '_' {
			if isUpper && len(current) > 0 {
				previous := current[len(current)-1]
				if previous >= 'a' && previous <= 'z' {
					hasLowerToUpperBoundary = true
				}
			}
			if value == '_' {
				hasUnderscore = true
			}
			current = append(current, value)
		} else {
			flush()
		}
	}
	flush()
	tokens := make([]string, 0, len(unique))
	for token := range unique {
		tokens = append(tokens, token)
	}
	slices.Sort(tokens)
	return tokens
}

func piSameTargetSide(left, right string) bool {
	return left == right || (left != "old" && right != "old")
}

func piSemanticTitleTokens(title string) []string {
	unique := make(map[string]struct{})
	current := make([]byte, 0, len(title))
	flush := func() {
		if len(current) == 0 {
			return
		}
		token := string(current)
		current = current[:0]
		if _, stop := piSemanticTitleStopWords[token]; stop ||
			(len(token) > 4 && strings.HasSuffix(token, "ly")) {
			return
		}
		if len(token) > 3 && strings.HasSuffix(token, "s") {
			token = strings.TrimSuffix(token, "s")
		}
		if alias, exists := piSemanticTitleAliases[token]; exists {
			token = alias
		}
		if len(token) > 1 {
			unique[token] = struct{}{}
		}
	}
	for _, value := range []byte(strings.ToLower(title)) {
		if value >= 'a' && value <= 'z' || value >= '0' && value <= '9' || value == '_' {
			current = append(current, value)
		} else {
			flush()
		}
	}
	flush()
	tokens := make([]string, 0, len(unique))
	for token := range unique {
		tokens = append(tokens, token)
	}
	slices.Sort(tokens)
	return tokens
}

// PiClusterFingerprint is exposed for existing adversarial fixture builders.
func PiClusterFingerprint(claim PiCandidateClaim) (string, error) {
	return piClusterFingerprint(claim)
}

func digestJSON(value any) (string, error) {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	// The Pi worker hashes JSON.stringify output. encoding/json's default HTML
	// escaping would turn ordinary claim text such as "approved->rejected" into
	// "approved-\u003erejected" and therefore invent a cross-language
	// fingerprint mismatch. Keep the wire bytes aligned with JSON.stringify;
	// Encode's single trailing LF is removed below.
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	data := bytes.TrimSuffix(encoded.Bytes(), []byte{'\n'})
	return digestBytesRaw(data), nil
}

func piProfileForProvider(provider string) (string, error) {
	switch provider {
	case "deepseek-anthropic":
		return "deepseek-anthropic-env@1", nil
	case "anthropic":
		return "anthropic-official@1", nil
	default:
		return "", fmt.Errorf("unsupported provider %q", provider)
	}
}

// PiProfileForProvider exposes the closed worker provider/profile mapping to
// fixture builders and the formal compatibility validator.
func PiProfileForProvider(provider string) (string, error) {
	return piProfileForProvider(provider)
}

func digestText(value string) string {
	return digestBytesRaw([]byte(value))
}

func digestBytesRaw(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func lowerSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

func exactFrozenExcerpt(content string, startLine, endLine uint32) (string, bool) {
	normalized := strings.ReplaceAll(strings.ReplaceAll(content, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(normalized, "\n")
	if strings.HasSuffix(normalized, "\n") {
		lines = lines[:len(lines)-1]
	}
	if startLine == 0 || endLine < startLine || uint64(endLine) > uint64(len(lines)) {
		return "", false
	}
	excerpt := strings.TrimSpace(strings.Join(lines[startLine-1:endLine], "\n"))
	if excerpt == "" {
		return "", false
	}
	return excerpt, true
}

func prefixedSHA256(value string) bool {
	return strings.HasPrefix(value, "sha256:") && lowerSHA256(strings.TrimPrefix(value, "sha256:"))
}

func normalizedUTC(value time.Time) time.Time {
	return time.Unix(0, value.UnixNano()).UTC()
}

func optionalExternalDigest(value string) (*string, error) {
	if value == "" {
		return nil, nil
	}
	value = strings.TrimPrefix(value, "sha256:")
	if !lowerSHA256(value) {
		return nil, fmt.Errorf("worker digest is not a lowercase SHA-256")
	}
	copy := value
	return &copy, nil
}

func optionalExternalDigestPointer(value *string) (*string, error) {
	if value == nil {
		return nil, nil
	}
	return optionalExternalDigest(*value)
}

func cloneStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneUint64Pointer(value *uint64) *uint64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func sameStringSet(left, right map[string]struct{}) bool {
	if len(left) != len(right) {
		return false
	}
	for value := range left {
		if _, exists := right[value]; !exists {
			return false
		}
	}
	return true
}

func jsEncodeURIComponent(value string) string {
	const hexadecimal = "0123456789ABCDEF"
	var result strings.Builder
	result.Grow(len(value))
	for len(value) > 0 {
		current, size := utf8.DecodeRuneInString(value)
		data := []byte(string(current))
		if size == 1 && ((current >= 'a' && current <= 'z') ||
			(current >= 'A' && current <= 'Z') ||
			(current >= '0' && current <= '9') ||
			strings.ContainsRune("-_.!~*'()", current)) {
			result.WriteRune(current)
		} else {
			for _, octet := range data {
				result.WriteByte('%')
				result.WriteByte(hexadecimal[octet>>4])
				result.WriteByte(hexadecimal[octet&0x0f])
			}
		}
		value = value[size:]
	}
	return result.String()
}

// JSEncodeURIComponent mirrors the worker target identity encoding for
// existing exact-report fixture builders.
func JSEncodeURIComponent(value string) string {
	return jsEncodeURIComponent(value)
}
