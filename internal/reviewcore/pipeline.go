package reviewcore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/scanner"
	"go/token"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
)

var orderedStages = []StageName{
	StageMaterializeTarget,
	StagePlanContext,
	StageDetect,
	StageNormalize,
	StageVerify,
	StageAdjudicate,
	StageReport,
	StagePublish,
	StageCaptureFeedback,
	StageExportEvaluation,
}

// Run executes the exact same deterministic stages used by production,
// evaluation, and replay. Reused artifacts must be a complete, digest-linked
// prefix ending immediately before StartAt.
func Run(ctx context.Context, input ReviewInput, options RunOptions) (RunResult, error) {
	if err := checkContext(ctx); err != nil {
		return RunResult{}, err
	}
	inputDigest, err := digestReviewInputContext(ctx, input)
	if err != nil {
		return RunResult{}, fmt.Errorf("validate review input: %w", err)
	}
	start := options.StartAt
	if start == "" {
		start = StageMaterializeTarget
	}
	startIndex := slices.Index(orderedStages, start)
	if startIndex < 0 {
		return RunResult{}, fmt.Errorf("unsupported start stage %q", start)
	}
	if len(options.Upstream) != startIndex {
		return RunResult{}, fmt.Errorf(
			"start stage %q requires %d upstream artifacts, got %d",
			start, startIndex, len(options.Upstream),
		)
	}

	stages := make([]StageResult, 0, len(orderedStages))
	reused := make([]StageName, 0, len(options.Upstream))
	expectedInputDigest := inputDigest
	for index, artifact := range options.Upstream {
		if err := checkContext(ctx); err != nil {
			return RunResult{}, err
		}
		expectedStage := orderedStages[index]
		if artifact.Stage != expectedStage {
			return RunResult{}, fmt.Errorf(
				"upstream artifact %d is %q, want %q", index, artifact.Stage, expectedStage,
			)
		}
		if err := artifact.Validate(); err != nil {
			return RunResult{}, fmt.Errorf("validate reused %s artifact: %w", expectedStage, err)
		}
		if artifact.InputDigest != expectedInputDigest {
			return RunResult{}, fmt.Errorf("reused %s artifact does not extend the requested input", expectedStage)
		}
		if err := validateArtifactTarget(artifact, inputDigest); err != nil {
			return RunResult{}, fmt.Errorf("validate reused %s target: %w", expectedStage, err)
		}
		if err := validateStageAnchors(ctx, input, artifact); err != nil {
			return RunResult{}, fmt.Errorf("validate reused %s anchors: %w", expectedStage, err)
		}
		stages = append(stages, artifact)
		reused = append(reused, artifact.Stage)
		expectedInputDigest = artifact.ArtifactDigest
	}

	for index := startIndex; index < len(orderedStages); index++ {
		if err := checkContext(ctx); err != nil {
			return RunResult{}, err
		}
		stage := orderedStages[index]
		var prior StageResult
		if len(stages) > 0 {
			prior = stages[len(stages)-1]
		}
		artifact, err := ExecuteStage(ctx, input, stage, prior)
		if err != nil {
			return RunResult{}, fmt.Errorf("%s stage: %w", stage, err)
		}
		if artifact.InputDigest != expectedInputDigest {
			return RunResult{}, fmt.Errorf("%s stage produced a broken artifact chain", stage)
		}
		if err := validateStageAnchors(ctx, input, artifact); err != nil {
			return RunResult{}, fmt.Errorf("%s stage produced an unauthorized anchor: %w", stage, err)
		}
		stages = append(stages, artifact)
		expectedInputDigest = artifact.ArtifactDigest
	}

	var report *Report
	for index := range stages {
		if stages[index].Stage == StageReport {
			report = stages[index].Output.Report
			break
		}
	}
	if report == nil {
		return RunResult{}, fmt.Errorf("workflow produced no report artifact")
	}
	reportJSON, err := MarshalReportJSON(*report)
	if err != nil {
		return RunResult{}, err
	}
	markdown, err := RenderReportMarkdown(ctx, *report)
	if err != nil {
		return RunResult{}, err
	}
	return RunResult{
		InputDigest: inputDigest, Stages: stages, ReusedStages: reused,
		Report: *report, ReportJSON: reportJSON, ReportMarkdown: markdown,
	}, nil
}

// ExecuteStage runs exactly one stage, enabling the caller to durably persist
// the returned checkpoint before scheduling the next stage. For materialize_target,
// upstream must be the zero StageResult; every later stage requires the
// immediately preceding artifact.
func ExecuteStage(
	ctx context.Context,
	input ReviewInput,
	stage StageName,
	upstream StageResult,
) (StageResult, error) {
	return ExecuteStageWithPolicy(ctx, input, stage, upstream, DefaultRuntimePolicy())
}

// ExecuteStageWithPolicy runs one stage under an explicit, already-frozen
// runtime policy. The policy is validated for every stage so an unsupported
// enabled rule can never degrade into a silent no-op.
func ExecuteStageWithPolicy(
	ctx context.Context,
	input ReviewInput,
	stage StageName,
	upstream StageResult,
	policy RuntimePolicy,
) (StageResult, error) {
	if err := checkContext(ctx); err != nil {
		return StageResult{}, err
	}
	if err := policy.Validate(); err != nil {
		return StageResult{}, fmt.Errorf("validate runtime policy: %w", err)
	}
	targetDigest, err := digestReviewInputContext(ctx, input)
	if err != nil {
		return StageResult{}, fmt.Errorf("validate review input: %w", err)
	}
	stageIndex := slices.Index(orderedStages, stage)
	if stageIndex < 0 {
		return StageResult{}, fmt.Errorf("unsupported stage %q", stage)
	}
	if stage == StageMaterializeTarget {
		if upstream.Stage != "" || upstream.ArtifactDigest != "" {
			return StageResult{}, fmt.Errorf(
				"materialize_target stage does not accept an upstream artifact",
			)
		}
		return materializeTargetStage(targetDigest)
	}
	expectedPrior := orderedStages[stageIndex-1]
	if err := validatePrior(upstream, expectedPrior); err != nil {
		return StageResult{}, err
	}
	if upstream.TargetDigest != targetDigest {
		return StageResult{}, fmt.Errorf("upstream artifact belongs to another review input")
	}
	if err := validateStageAnchors(ctx, input, upstream); err != nil {
		return StageResult{}, fmt.Errorf("upstream artifact contains an unauthorized anchor: %w", err)
	}
	var result StageResult
	switch stage {
	case StagePlanContext:
		result, err = planContextStage(input, upstream)
	case StageDetect:
		var detected StageResult
		detected, err = detectWithPolicy(ctx, input, policy)
		if err == nil {
			result, err = newStageResult(
				StageDetect,
				detected.TargetDigest,
				upstream.ArtifactDigest,
				detected.Output,
			)
		}
	case StageNormalize:
		result, err = normalizeWithPolicy(ctx, upstream, policy)
	case StageVerify:
		result, err = VerifyWithPolicy(ctx, input, upstream, policy)
	case StageAdjudicate:
		result, err = adjudicateWithPolicy(ctx, upstream, policy)
	case StageReport:
		result, err = BuildReport(ctx, upstream)
	case StagePublish:
		result, err = publishProjectionStage(upstream)
	case StageCaptureFeedback:
		result, err = captureFeedbackRegistrationStage(upstream)
	case StageExportEvaluation:
		result, err = exportEvaluationRegistrationStage(upstream)
	default:
		return StageResult{}, fmt.Errorf("unsupported stage %q", stage)
	}
	if err != nil {
		return StageResult{}, err
	}
	if err := validateStageAnchors(ctx, input, result); err != nil {
		return StageResult{}, fmt.Errorf("stage %q produced an unauthorized anchor: %w", stage, err)
	}
	return result, nil
}

func materializeTargetStage(targetDigest string) (StageResult, error) {
	return newStageResult(
		StageMaterializeTarget,
		targetDigest,
		targetDigest,
		StageOutput{Lifecycle: &LifecycleFact{
			Kind:        "target_materialized",
			State:       LifecycleComplete,
			ReasonCodes: []string{},
			RecordCount: 1,
		}},
	)
}

func planContextStage(
	input ReviewInput,
	materialized StageResult,
) (StageResult, error) {
	reasons := make([]string, 0)
	for _, binding := range input.Contexts {
		if binding.Gap != nil {
			reasons = append(reasons, binding.Gap.ReasonCode)
		}
	}
	sort.Strings(reasons)
	reasons = slices.Compact(reasons)
	state := LifecycleComplete
	if len(reasons) > 0 {
		state = LifecyclePartial
	}
	return newStageResult(
		StagePlanContext,
		materialized.TargetDigest,
		materialized.ArtifactDigest,
		StageOutput{Lifecycle: &LifecycleFact{
			Kind:        "context_planned",
			State:       state,
			ReasonCodes: reasons,
			RecordCount: uint32(len(input.Contexts)),
		}},
	)
}

func publishProjectionStage(report StageResult) (StageResult, error) {
	if report.Output.Report == nil {
		return StageResult{}, fmt.Errorf("publish requires a report payload")
	}
	return newStageResult(
		StagePublish,
		report.TargetDigest,
		report.ArtifactDigest,
		StageOutput{Lifecycle: &LifecycleFact{
			Kind:        "publication_projection",
			State:       LifecycleRemoteDisabled,
			ReasonCodes: []string{"remote_writes_denied"},
			RecordCount: report.Output.Report.Summary.Publish,
		}},
	)
}

func captureFeedbackRegistrationStage(
	publication StageResult,
) (StageResult, error) {
	if publication.Output.Lifecycle == nil {
		return StageResult{}, fmt.Errorf("capture_feedback requires publication lifecycle")
	}
	return newStageResult(
		StageCaptureFeedback,
		publication.TargetDigest,
		publication.ArtifactDigest,
		StageOutput{Lifecycle: &LifecycleFact{
			Kind:        "feedback_registration",
			State:       LifecycleAwaitingFeedback,
			ReasonCodes: []string{"external_feedback_not_recorded"},
			RecordCount: publication.Output.Lifecycle.RecordCount,
		}},
	)
}

func exportEvaluationRegistrationStage(
	feedback StageResult,
) (StageResult, error) {
	if feedback.Output.Lifecycle == nil {
		return StageResult{}, fmt.Errorf(
			"export_evaluation requires feedback registration lifecycle",
		)
	}
	return newStageResult(
		StageExportEvaluation,
		feedback.TargetDigest,
		feedback.ArtifactDigest,
		StageOutput{Lifecycle: &LifecycleFact{
			Kind:        "evaluation_export_registration",
			State:       LifecycleCandidateOnly,
			ReasonCodes: []string{"governance_review_required"},
			RecordCount: feedback.Output.Lifecycle.RecordCount,
		}},
	)
}

func validateStageAnchors(ctx context.Context, input ReviewInput, artifact StageResult) error {
	for _, candidate := range artifact.Output.CandidateFindings {
		if !inputAuthorizesAnchor(
			ctx,
			input,
			candidate.Path,
			candidate.StartLine,
			candidate.EndLine,
		) {
			return fmt.Errorf(
				"candidate %q anchor %s:%d-%d is outside the frozen target",
				candidate.ID,
				candidate.Path,
				candidate.StartLine,
				candidate.EndLine,
			)
		}
	}
	for _, gap := range artifact.Output.DetectionGaps {
		if !inputAuthorizesAnchor(ctx, input, gap.Path, gap.Line, gap.Line) {
			return fmt.Errorf(
				"detection gap %q location %s:%d is outside the frozen target",
				gap.ID,
				gap.Path,
				gap.Line,
			)
		}
	}
	for _, finding := range artifact.Output.Findings {
		if !inputAuthorizesAnchor(ctx, input, finding.Path, finding.StartLine, finding.EndLine) {
			return fmt.Errorf(
				"finding %q anchor %s:%d-%d is outside the frozen target",
				finding.ID,
				finding.Path,
				finding.StartLine,
				finding.EndLine,
			)
		}
	}
	if artifact.Output.Report != nil {
		for _, finding := range artifact.Output.Report.Findings {
			if !inputAuthorizesAnchor(ctx, input, finding.Path, finding.StartLine, finding.EndLine) {
				return fmt.Errorf(
					"report finding %q anchor %s:%d-%d is outside the frozen target",
					finding.ID,
					finding.Path,
					finding.StartLine,
					finding.EndLine,
				)
			}
		}
	}
	return nil
}

func inputAuthorizesAnchor(
	ctx context.Context,
	input ReviewInput,
	repositoryPath string,
	startLine uint32,
	endLine uint32,
) bool {
	if startLine == 0 || endLine < startLine {
		return false
	}
	if input.TargetMode == TargetModeDiff {
		patch, err := parseUnifiedDiffContext(ctx, input.CanonicalPatch)
		if err != nil {
			return false
		}
		authorized := make(map[uint32]struct{})
		for _, line := range patch.Added {
			if line.Path == repositoryPath {
				authorized[line.NewLine] = struct{}{}
			}
		}
		// A deletion-only hunk has no added target line to carry a finding.
		// Its surviving hunk context is the smallest target-side locus that can
		// bind the absence introduced by the deletion without admitting
		// arbitrary unchanged file content.
		for _, line := range patch.DeletionContextTargets {
			if line.Path == repositoryPath {
				authorized[line.NewLine] = struct{}{}
			}
		}
		for line := startLine; line <= endLine; line++ {
			if _, exists := authorized[line]; !exists {
				return false
			}
			if line == ^uint32(0) {
				break
			}
		}
		return true
	}
	for _, region := range input.Regions {
		if region.Path == repositoryPath &&
			startLine >= region.StartLine &&
			endLine <= region.EndLine {
			return true
		}
	}
	return false
}

// InputAuthorizesAnchor exposes the frozen target authorization check to
// read-only host adapters. It does not expand ReviewInput coverage and returns
// false for an invalid context or an invalid input.
func InputAuthorizesAnchor(
	ctx context.Context,
	input ReviewInput,
	repositoryPath string,
	startLine uint32,
	endLine uint32,
) bool {
	if ctx == nil || ctx.Err() != nil {
		return false
	}
	if err := input.validateContext(ctx); err != nil {
		return false
	}
	return inputAuthorizesAnchor(ctx, input, repositoryPath, startLine, endLine)
}

func Detect(ctx context.Context, input ReviewInput) (StageResult, error) {
	return detectWithPolicy(ctx, input, DefaultRuntimePolicy())
}

// MergeDetectShardResults performs deterministic fan-in for independently
// executed scope shards. Candidate and gap identities include TargetDigest,
// so they are rebuilt against the full immutable ReviewInput instead of
// leaking shard-local identity into the downstream workflow.
func MergeDetectShardResults(
	ctx context.Context,
	input ReviewInput,
	upstream StageResult,
	shards []DetectShardResult,
) (StageResult, error) {
	if err := checkContext(ctx); err != nil {
		return StageResult{}, err
	}
	if input.TargetMode != TargetModeScope {
		return StageResult{}, fmt.Errorf("detect shard fan-in requires a scope ReviewInput")
	}
	fullDigest, err := digestReviewInputContext(ctx, input)
	if err != nil {
		return StageResult{}, err
	}
	if err := validatePrior(upstream, StagePlanContext); err != nil {
		return StageResult{}, err
	}
	if upstream.TargetDigest != fullDigest {
		return StageResult{}, fmt.Errorf("plan_context artifact belongs to another full scope input")
	}
	fullFiles := make(map[string]FileManifestEntry, len(input.Files))
	for _, file := range input.Files {
		fullFiles[file.Path] = file
	}
	seenFiles := make(map[string]struct{})
	candidates := make([]CandidateFinding, 0)
	gaps := make([]DetectionGap, 0)
	for index, shard := range shards {
		if err := checkContext(ctx); err != nil {
			return StageResult{}, err
		}
		if shard.Input.TargetMode != TargetModeScope {
			return StageResult{}, fmt.Errorf("shards[%d] is not a scope ReviewInput", index)
		}
		shardDigest, err := digestReviewInputContext(ctx, shard.Input)
		if err != nil {
			return StageResult{}, fmt.Errorf("validate shards[%d] input: %w", index, err)
		}
		if err := validatePrior(shard.Result, StageDetect); err != nil {
			return StageResult{}, fmt.Errorf("validate shards[%d] result: %w", index, err)
		}
		if shard.Result.TargetDigest != shardDigest {
			return StageResult{}, fmt.Errorf("shards[%d] result targets another shard input", index)
		}
		for _, file := range shard.Input.Files {
			full, exists := fullFiles[file.Path]
			if !exists || !reflect.DeepEqual(full, file) {
				return StageResult{}, fmt.Errorf("shards[%d] file %q is outside the exact full input", index, file.Path)
			}
			if _, duplicate := seenFiles[file.Path]; duplicate {
				return StageResult{}, fmt.Errorf("shard fan-in contains duplicate file %q", file.Path)
			}
			seenFiles[file.Path] = struct{}{}
		}
		for _, candidate := range shard.Result.Output.CandidateFindings {
			candidate.TargetDigest = fullDigest
			candidate.ID = stableID(
				"candidate", candidate.DetectorID, candidate.DetectorRevision,
				candidate.TargetDigest, candidate.RuleID, string(candidate.SignalKind),
				candidate.Path, strconv.FormatUint(uint64(candidate.StartLine), 10),
				strconv.FormatUint(uint64(candidate.SignalOffset), 10), candidate.Excerpt,
			)
			for evidenceIndex := range candidate.Evidence {
				candidate.Evidence[evidenceIndex].ID = stableID(
					"evidence", string(candidate.Evidence[evidenceIndex].Kind), candidate.ID,
				)
			}
			if err := candidate.Validate(); err != nil {
				return StageResult{}, fmt.Errorf("rebind shards[%d] candidate: %w", index, err)
			}
			candidates = append(candidates, candidate)
		}
		for _, gap := range shard.Result.Output.DetectionGaps {
			gap.TargetDigest = fullDigest
			gap.ID = stableID(
				"detection-gap", gap.DetectorID, gap.DetectorRevision, gap.RuleID,
				gap.TargetDigest, gap.SourceDigest, gap.Path,
				strconv.FormatUint(uint64(gap.Line), 10), string(gap.ReasonCode),
			)
			if err := gap.Validate(); err != nil {
				return StageResult{}, fmt.Errorf("rebind shards[%d] detection gap: %w", index, err)
			}
			gaps = append(gaps, gap)
		}
	}
	sort.Slice(candidates, func(left, right int) bool {
		if candidates[left].Path != candidates[right].Path {
			return candidates[left].Path < candidates[right].Path
		}
		if candidates[left].StartLine != candidates[right].StartLine {
			return candidates[left].StartLine < candidates[right].StartLine
		}
		if candidates[left].SignalOffset != candidates[right].SignalOffset {
			return candidates[left].SignalOffset < candidates[right].SignalOffset
		}
		return candidates[left].ID < candidates[right].ID
	})
	sort.Slice(gaps, func(left, right int) bool {
		if gaps[left].Path != gaps[right].Path {
			return gaps[left].Path < gaps[right].Path
		}
		if gaps[left].Line != gaps[right].Line {
			return gaps[left].Line < gaps[right].Line
		}
		return gaps[left].ID < gaps[right].ID
	})
	result, err := newStageResult(StageDetect, fullDigest, upstream.ArtifactDigest, StageOutput{
		CandidateFindings: candidates,
		DetectionGaps:     gaps,
	})
	if err != nil {
		return StageResult{}, err
	}
	if err := validateStageAnchors(ctx, input, result); err != nil {
		return StageResult{}, fmt.Errorf("fan-in produced an unauthorized anchor: %w", err)
	}
	return result, nil
}

func detectWithPolicy(
	ctx context.Context,
	input ReviewInput,
	policy RuntimePolicy,
) (StageResult, error) {
	if err := checkContext(ctx); err != nil {
		return StageResult{}, err
	}
	if err := policy.Validate(); err != nil {
		return StageResult{}, fmt.Errorf("validate runtime policy: %w", err)
	}
	targetDigest, err := digestReviewInputContext(ctx, input)
	if err != nil {
		return StageResult{}, err
	}
	lines, err := reviewTargetLines(ctx, input)
	if err != nil {
		return StageResult{}, err
	}
	candidates := make([]CandidateFinding, 0)
	gaps := make([]DetectionGap, 0)
	request := detectorRequest{
		input: input, targetDigest: targetDigest, policy: policy, targetLines: lines,
	}
	for _, detector := range detectorRegistry() {
		if err := checkContext(ctx); err != nil {
			return StageResult{}, err
		}
		output, detectErr := detector.run(ctx, request)
		if detectErr != nil {
			return StageResult{}, fmt.Errorf(
				"detector %s@%s: %w",
				detector.descriptor.ID,
				detector.descriptor.Revision,
				detectErr,
			)
		}
		candidates = append(candidates, output.candidates...)
		gaps = append(gaps, output.gaps...)
	}
	sort.Slice(candidates, func(left, right int) bool {
		if candidates[left].Path != candidates[right].Path {
			return candidates[left].Path < candidates[right].Path
		}
		if candidates[left].StartLine != candidates[right].StartLine {
			return candidates[left].StartLine < candidates[right].StartLine
		}
		if candidates[left].SignalOffset != candidates[right].SignalOffset {
			return candidates[left].SignalOffset < candidates[right].SignalOffset
		}
		return candidates[left].ID < candidates[right].ID
	})
	sort.Slice(gaps, func(left, right int) bool {
		if gaps[left].Path != gaps[right].Path {
			return gaps[left].Path < gaps[right].Path
		}
		if gaps[left].Line != gaps[right].Line {
			return gaps[left].Line < gaps[right].Line
		}
		return gaps[left].ID < gaps[right].ID
	})
	return newStageResult(StageDetect, targetDigest, targetDigest, StageOutput{
		CandidateFindings: candidates,
		DetectionGaps:     gaps,
	})
}

type reviewTargetLine struct {
	Path         string
	Line         uint32
	Text         string
	EvidenceKind EvidenceKind
	SourceDigest string
}

func reviewTargetLines(ctx context.Context, input ReviewInput) ([]reviewTargetLine, error) {
	switch input.TargetMode {
	case TargetModeDiff:
		patch, err := parseUnifiedDiffContext(ctx, input.CanonicalPatch)
		if err != nil {
			return nil, err
		}
		patchDigest := digestString(input.CanonicalPatch)
		lines := make([]reviewTargetLine, 0, len(patch.Added))
		for _, line := range patch.Added {
			lines = append(lines, reviewTargetLine{
				Path: line.Path, Line: line.NewLine, Text: line.Text,
				EvidenceKind: EvidencePatchLine, SourceDigest: patchDigest,
			})
		}
		return lines, nil
	case TargetModeSelection, TargetModeScope:
		files := make(map[string]FileManifestEntry, len(input.Files))
		fileLines := make(map[string][]string, len(input.Files))
		for _, file := range input.Files {
			files[file.Path] = file
			if file.Content != nil {
				fileLines[file.Path] = splitContentLines(*file.Content)
			}
		}
		lines := make([]reviewTargetLine, 0)
		for _, region := range input.Regions {
			if err := checkContext(ctx); err != nil {
				return nil, err
			}
			file, exists := files[region.Path]
			if !exists || file.Content == nil {
				return nil, fmt.Errorf(
					"review region %s has no frozen content",
					region.Path,
				)
			}
			indexedLines := fileLines[region.Path]
			for lineNumber := region.StartLine; lineNumber <= region.EndLine; lineNumber++ {
				if err := checkContext(ctx); err != nil {
					return nil, err
				}
				if lineNumber == 0 || uint64(lineNumber) > uint64(len(indexedLines)) {
					return nil, fmt.Errorf(
						"review region %s:%d is absent from frozen content",
						region.Path,
						lineNumber,
					)
				}
				line := indexedLines[lineNumber-1]
				lines = append(lines, reviewTargetLine{
					Path: region.Path, Line: lineNumber, Text: line,
					EvidenceKind: EvidenceTargetLine, SourceDigest: region.SHA256,
				})
				if lineNumber == ^uint32(0) {
					break
				}
			}
		}
		return lines, nil
	default:
		return nil, fmt.Errorf("unsupported review target mode %q", input.TargetMode)
	}
}

func Normalize(ctx context.Context, detected StageResult) (StageResult, error) {
	return normalizeWithPolicy(ctx, detected, DefaultRuntimePolicy())
}

func normalizeWithPolicy(
	ctx context.Context,
	detected StageResult,
	policy RuntimePolicy,
) (StageResult, error) {
	if err := checkContext(ctx); err != nil {
		return StageResult{}, err
	}
	if err := policy.Validate(); err != nil {
		return StageResult{}, fmt.Errorf("validate runtime policy: %w", err)
	}
	if err := validatePrior(detected, StageDetect); err != nil {
		return StageResult{}, err
	}
	applicableCandidates := make([]CandidateFinding, 0, len(detected.Output.CandidateFindings))
	for _, candidate := range detected.Output.CandidateFindings {
		runtimeRule, configured := runtimeRuleFor(policy, candidate.RuleID)
		if configured && runtimeRuleApplies(runtimeRule, candidate.Path) {
			applicableCandidates = append(applicableCandidates, candidate)
		}
	}
	locationRanks := buildLocationRanks(applicableCandidates)
	groups := make(map[string][]CandidateFinding)
	for _, candidate := range applicableCandidates {
		if err := checkContext(ctx); err != nil {
			return StageResult{}, err
		}
		anchor := stableAnchor(candidate, locationRanks)
		fingerprint := stableID(
			"fingerprint", candidate.RuleID, anchor, normalizeExcerpt(candidate.Excerpt),
		)
		groups[fingerprint] = append(groups[fingerprint], candidate)
	}
	fingerprints := make([]string, 0, len(groups))
	for fingerprint := range groups {
		fingerprints = append(fingerprints, fingerprint)
	}
	sort.Strings(fingerprints)

	findings := make([]Finding, 0, len(fingerprints))
	for _, fingerprint := range fingerprints {
		if err := checkContext(ctx); err != nil {
			return StageResult{}, err
		}
		group := groups[fingerprint]
		sort.Slice(group, func(i, j int) bool { return group[i].ID < group[j].ID })
		winner := group[0]
		rule, registered := LookupRuleDescriptor(winner.RuleID)
		if !registered {
			return StageResult{}, fmt.Errorf("finding rule %q is not registered", winner.RuleID)
		}
		runtimeRule, configured := runtimeRuleFor(policy, winner.RuleID)
		if !configured || !runtimeRuleApplies(runtimeRule, winner.Path) {
			return StageResult{}, fmt.Errorf("finding rule %q is not enabled for %q", winner.RuleID, winner.Path)
		}
		lineage := make([]CandidateLineage, 0, len(group))
		evidence := make([]Evidence, 0, len(group))
		signalSet := make(map[string]struct{}, len(group))
		for index, candidate := range group {
			disposition := LineageDuplicate
			reason := "semantic_duplicate"
			if index == 0 {
				disposition = LineageWinner
				reason = "canonical_candidate"
			}
			lineage = append(lineage, CandidateLineage{
				CandidateID: candidate.ID, Disposition: disposition, ReasonCode: reason,
			})
			evidence = append(evidence, candidate.Evidence...)
			signalSet[candidate.Signal] = struct{}{}
		}
		sort.Slice(evidence, func(i, j int) bool { return evidence[i].ID < evidence[j].ID })
		signals := make([]string, 0, len(signalSet))
		for signal := range signalSet {
			signals = append(signals, signal)
		}
		sort.Strings(signals)
		anchor := stableAnchor(winner, locationRanks)
		findings = append(findings, Finding{
			ID: stableID("finding", fingerprint), Fingerprint: fingerprint, Anchor: anchor,
			RuleID: winner.RuleID, Title: rule.Title, Severity: runtimeRule.Severity,
			Message: rule.Message, TargetDigest: winner.TargetDigest, Path: winner.Path,
			StartLine: winner.StartLine, EndLine: winner.EndLine, Excerpt: winner.Excerpt,
			Signals: signals, Evidence: evidence, Lineage: lineage,
			Verification: Verification{
				Status: VerificationPending, ReasonCode: "not_verified", EvidenceIDs: []string{},
			},
		})
	}
	return newStageResult(
		StageNormalize, detected.TargetDigest, detected.ArtifactDigest, StageOutput{Findings: findings},
	)
}

func Verify(ctx context.Context, input ReviewInput, normalized StageResult) (StageResult, error) {
	return VerifyWithPolicy(ctx, input, normalized, DefaultRuntimePolicy())
}

// VerifyWithPolicy applies the existing source/lexical verification first and
// then enforces the configured independent-evidence threshold. Evidence
// independence is the distinct pair of kind and immutable source digest.
func VerifyWithPolicy(
	ctx context.Context,
	input ReviewInput,
	normalized StageResult,
	policy RuntimePolicy,
) (StageResult, error) {
	if err := checkContext(ctx); err != nil {
		return StageResult{}, err
	}
	if err := policy.Validate(); err != nil {
		return StageResult{}, fmt.Errorf("validate runtime policy: %w", err)
	}
	if err := validatePrior(normalized, StageNormalize); err != nil {
		return StageResult{}, err
	}
	targetDigest, err := digestReviewInputContext(ctx, input)
	if err != nil {
		return StageResult{}, err
	}
	added := make(map[string][]addedLine)
	if input.TargetMode == TargetModeDiff {
		patch, parseErr := parseUnifiedDiffContext(ctx, input.CanonicalPatch)
		if parseErr != nil {
			return StageResult{}, parseErr
		}
		for _, line := range patch.Added {
			added[line.Path] = append(added[line.Path], line)
		}
	}
	files := make(map[string]FileManifestEntry, len(input.Files))
	for _, file := range input.Files {
		files[file.Path] = file
	}
	commentScans := make(map[string]goCommentScan)

	findings := make([]Finding, 0, len(normalized.Output.Findings))
	for _, original := range normalized.Output.Findings {
		if err := checkContext(ctx); err != nil {
			return StageResult{}, err
		}
		finding := cloneFinding(original)
		if finding.TargetDigest != targetDigest {
			return StageResult{}, fmt.Errorf("finding %q belongs to another review input", finding.ID)
		}
		targetEvidenceIDs := evidenceIDs(finding.Evidence)
		targetMatches := hasAddedLine(added[finding.Path], finding.StartLine, finding.Excerpt)
		mismatchReason := "patch_evidence_mismatch"
		if input.TargetMode != TargetModeDiff {
			targetMatches = inputContainsLine(input, finding.Path, finding.StartLine, finding.Excerpt)
			mismatchReason = "target_evidence_mismatch"
		}
		if !targetMatches {
			finding.Verification = Verification{
				Status: VerificationRejected, ReasonCode: mismatchReason,
				EvidenceIDs: targetEvidenceIDs,
			}
			findings = append(findings, finding)
			continue
		}
		file, exists := files[finding.Path]
		if !exists || file.Content == nil {
			finding.Verification = Verification{
				Status: VerificationInconclusive, ReasonCode: "content_unavailable",
				EvidenceIDs: targetEvidenceIDs,
			}
			findings = append(findings, finding)
			continue
		}
		contentLine, exists := lineAt(*file.Content, finding.StartLine)
		contentEvidence := Evidence{
			ID:   stableID("evidence", "content", finding.ID, file.SHA256),
			Kind: EvidenceFileContent, SourceDigest: file.SHA256, Path: finding.Path,
			Line: finding.StartLine, Excerpt: contentLine, Claim: "final_file_line_at_anchor",
		}
		if !exists {
			// Evidence excerpts must be non-empty, so the missing-line decision
			// remains grounded in the already retained patch evidence.
			finding.Verification = Verification{
				Status: VerificationRejected, ReasonCode: "content_line_missing",
				EvidenceIDs: targetEvidenceIDs,
			}
			findings = append(findings, finding)
			continue
		}
		finding.Evidence = append(finding.Evidence, contentEvidence)
		allEvidenceIDs := evidenceIDs(finding.Evidence)
		if contentLine != finding.Excerpt {
			finding.Verification = Verification{
				Status: VerificationRejected, ReasonCode: "content_mismatch",
				EvidenceIDs: allEvidenceIDs,
			}
			findings = append(findings, finding)
			continue
		}
		rule, registered := LookupRuleDescriptor(finding.RuleID)
		if !registered {
			return StageResult{}, fmt.Errorf("finding rule %q is not registered", finding.RuleID)
		}
		switch rule.SignalKind {
		case DetectionSignalLexicalMarker:
			scan, scanned := commentScans[finding.Path]
			if !scanned {
				scan, err = scanGoComments(ctx, *file.Content)
				if err != nil {
					return StageResult{}, err
				}
				commentScans[finding.Path] = scan
			}
			if !scan.LexicallyValid {
				finding.Verification = Verification{
					Status: VerificationInconclusive, ReasonCode: "go_lexing_failed",
					EvidenceIDs: allEvidenceIDs,
				}
				findings = append(findings, finding)
				continue
			}
			if !scan.contains(finding.StartLine, finding.Signals) {
				finding.Verification = Verification{
					Status: VerificationRejected, ReasonCode: "marker_not_in_comment",
					EvidenceIDs: allEvidenceIDs,
				}
				findings = append(findings, finding)
				continue
			}
		case DetectionSignalGoASTPattern:
			confirmed := true
			for _, signal := range finding.Signals {
				var signalConfirmed bool
				signalConfirmed, err = goASTSignalAt(
					*file.Content,
					finding.Path,
					finding.StartLine,
					signal,
				)
				if err != nil {
					finding.Verification = Verification{
						Status: VerificationInconclusive, ReasonCode: "go_parse_failed",
						EvidenceIDs: allEvidenceIDs,
					}
					findings = append(findings, finding)
					confirmed = false
					break
				}
				if !signalConfirmed {
					finding.Verification = Verification{
						Status:      VerificationRejected,
						ReasonCode:  "ast_pattern_not_confirmed",
						EvidenceIDs: allEvidenceIDs,
					}
					findings = append(findings, finding)
					confirmed = false
					break
				}
			}
			if !confirmed {
				continue
			}
		default:
			return StageResult{}, fmt.Errorf(
				"finding rule %q uses unsupported signal kind %q",
				finding.RuleID,
				rule.SignalKind,
			)
		}
		verifiedReason := "patch_and_content_confirmed"
		if input.TargetMode != TargetModeDiff {
			verifiedReason = "target_and_content_confirmed"
		}
		finding.Verification = Verification{
			Status: VerificationVerified, ReasonCode: verifiedReason,
			EvidenceIDs: allEvidenceIDs,
		}
		if !hasMinimumIndependentEvidence(finding, policy) {
			finding.Verification = Verification{
				Status:      VerificationInconclusive,
				ReasonCode:  "insufficient_independent_evidence",
				EvidenceIDs: allEvidenceIDs,
			}
		}
		findings = append(findings, finding)
	}
	return newStageResult(
		StageVerify, normalized.TargetDigest, normalized.ArtifactDigest, StageOutput{Findings: findings},
	)
}

func inputContainsLine(
	input ReviewInput,
	path string,
	lineNumber uint32,
	excerpt string,
) bool {
	for _, region := range input.Regions {
		if region.Path != path || lineNumber < region.StartLine || lineNumber > region.EndLine {
			continue
		}
		for _, file := range input.Files {
			if file.Path != path || file.Content == nil || file.SHA256 != region.SHA256 {
				continue
			}
			line, exists := lineAt(*file.Content, lineNumber)
			return exists && line == excerpt
		}
	}
	return false
}

func Adjudicate(ctx context.Context, verified StageResult) (StageResult, error) {
	return adjudicateWithPolicy(ctx, verified, DefaultRuntimePolicy())
}

func adjudicateWithPolicy(
	ctx context.Context,
	verified StageResult,
	policy RuntimePolicy,
) (StageResult, error) {
	if err := checkContext(ctx); err != nil {
		return StageResult{}, err
	}
	if err := policy.Validate(); err != nil {
		return StageResult{}, fmt.Errorf("validate runtime policy: %w", err)
	}
	if err := validatePrior(verified, StageVerify); err != nil {
		return StageResult{}, err
	}
	findings := cloneFindings(verified.Output.Findings)
	decisions := make([]FindingDecision, 0, len(findings))
	for _, finding := range findings {
		if err := checkContext(ctx); err != nil {
			return StageResult{}, err
		}
		var action DecisionAction
		switch finding.Verification.Status {
		case VerificationVerified:
			if !severityAtLeast(finding.Severity, policy.MinimumSeverity) {
				action = DecisionReject
			} else if !policy.TargetComplete && !policy.AllowPartialEvidence {
				if policy.HumanReviewInconclusive {
					action = DecisionHumanReview
				} else {
					action = DecisionReject
				}
			} else {
				action = DecisionPublish
			}
		case VerificationRejected:
			action = DecisionReject
		case VerificationInconclusive:
			if !severityAtLeast(finding.Severity, policy.MinimumSeverity) {
				action = DecisionReject
			} else if !policy.TargetComplete && !policy.AllowPartialEvidence {
				if policy.HumanReviewInconclusive {
					action = DecisionHumanReview
				} else {
					action = DecisionReject
				}
			} else if policy.HumanReviewInconclusive {
				action = DecisionHumanReview
			} else {
				action = DecisionReject
			}
		default:
			return StageResult{}, fmt.Errorf("finding %q has not completed verification", finding.ID)
		}
		reasonCode := finding.Verification.ReasonCode
		if finding.Verification.Status != VerificationRejected &&
			!severityAtLeast(finding.Severity, policy.MinimumSeverity) {
			reasonCode = "below_minimum_severity"
		} else if finding.Verification.Status != VerificationRejected &&
			!policy.TargetComplete && !policy.AllowPartialEvidence {
			if policy.HumanReviewInconclusive {
				reasonCode = "partial_target_requires_human_review"
			} else {
				reasonCode = "partial_target_evidence_disallowed"
			}
		} else if finding.Verification.Status == VerificationInconclusive &&
			!policy.HumanReviewInconclusive {
			reasonCode = "inconclusive_human_review_disabled"
		}
		decision := FindingDecision{
			FindingID: finding.ID, Sequence: 1, PriorDecisionID: "", Action: action,
			ReasonCode:  reasonCode,
			EvidenceIDs: slices.Clone(finding.Verification.EvidenceIDs),
		}
		decision.ID = stableID(
			"decision", decision.FindingID, strconv.FormatUint(uint64(decision.Sequence), 10),
			string(decision.Action), decision.ReasonCode, strings.Join(decision.EvidenceIDs, ","),
		)
		decisions = append(decisions, decision)
	}
	return newStageResult(StageAdjudicate, verified.TargetDigest, verified.ArtifactDigest, StageOutput{
		Findings: findings, Decisions: decisions,
	})
}

func BuildReport(ctx context.Context, adjudicated StageResult) (StageResult, error) {
	if err := checkContext(ctx); err != nil {
		return StageResult{}, err
	}
	if err := validatePrior(adjudicated, StageAdjudicate); err != nil {
		return StageResult{}, err
	}
	findings := cloneFindings(adjudicated.Output.Findings)
	decisions := cloneDecisions(adjudicated.Output.Decisions)
	summary, err := summarize(findings, decisions)
	if err != nil {
		return StageResult{}, err
	}
	report := Report{
		SchemaVersion: ReportSchemaVersion, TargetDigest: adjudicated.TargetDigest,
		Summary: summary, Findings: findings, Decisions: decisions,
	}
	return newStageResult(
		StageReport, adjudicated.TargetDigest, adjudicated.ArtifactDigest, StageOutput{Report: &report},
	)
}

func newStageResult(
	stage StageName,
	targetDigest string,
	inputDigest string,
	output StageOutput,
) (StageResult, error) {
	result := StageResult{
		SchemaVersion: ArtifactSchemaVersion, Stage: stage, StageRevision: stageRevisions[stage],
		TargetDigest: targetDigest, InputDigest: inputDigest, Output: output,
	}
	digest, err := digestStageResult(result)
	if err != nil {
		return StageResult{}, err
	}
	result.ArtifactDigest = digest
	if err := result.Validate(); err != nil {
		return StageResult{}, err
	}
	return result, nil
}

func validatePrior(result StageResult, expected StageName) error {
	if result.Stage != expected {
		return fmt.Errorf("requires %s artifact, got %s", expected, result.Stage)
	}
	if err := result.Validate(); err != nil {
		return fmt.Errorf("validate %s artifact: %w", expected, err)
	}
	return nil
}

func validateArtifactTarget(artifact StageResult, targetDigest string) error {
	if artifact.TargetDigest != targetDigest {
		return fmt.Errorf("stage artifact targets another input")
	}
	for _, candidate := range artifact.Output.CandidateFindings {
		if candidate.TargetDigest != targetDigest {
			return fmt.Errorf("candidate %q targets another input", candidate.ID)
		}
	}
	for _, gap := range artifact.Output.DetectionGaps {
		if gap.TargetDigest != targetDigest {
			return fmt.Errorf("detection gap %q targets another input", gap.ID)
		}
	}
	for _, finding := range artifact.Output.Findings {
		if finding.TargetDigest != targetDigest {
			return fmt.Errorf("finding %q targets another input", finding.ID)
		}
	}
	if artifact.Output.Report != nil && artifact.Output.Report.TargetDigest != targetDigest {
		return fmt.Errorf("report targets another input")
	}
	return nil
}

func normalizeExcerpt(excerpt string) string {
	return strings.Join(strings.Fields(excerpt), " ")
}

func buildLocationRanks(candidates []CandidateFinding) map[string]map[uint32]uint32 {
	lineSets := make(map[string]map[uint32]struct{})
	for _, candidate := range candidates {
		key := candidate.Path + "\x00" + normalizeExcerpt(candidate.Excerpt)
		if lineSets[key] == nil {
			lineSets[key] = make(map[uint32]struct{})
		}
		lineSets[key][candidate.StartLine] = struct{}{}
	}
	ranks := make(map[string]map[uint32]uint32, len(lineSets))
	for key, lineSet := range lineSets {
		lines := make([]uint32, 0, len(lineSet))
		for line := range lineSet {
			lines = append(lines, line)
		}
		slices.Sort(lines)
		ranks[key] = make(map[uint32]uint32, len(lines))
		for index, line := range lines {
			ranks[key][line] = uint32(index + 1)
		}
	}
	return ranks
}

func stableAnchor(candidate CandidateFinding, ranks map[string]map[uint32]uint32) string {
	normalized := normalizeExcerpt(candidate.Excerpt)
	key := candidate.Path + "\x00" + normalized
	rank := ranks[key][candidate.StartLine]
	lineIdentity := stableID("line", normalized)[:16]
	return candidate.Path + "#" + lineIdentity + ":" + strconv.FormatUint(uint64(rank), 10)
}

func hasAddedLine(lines []addedLine, number uint32, excerpt string) bool {
	for _, line := range lines {
		if line.NewLine == number && line.Text == excerpt {
			return true
		}
	}
	return false
}

func lineAt(content string, number uint32) (string, bool) {
	lines := splitContentLines(content)
	if number == 0 || int(number) > len(lines) {
		return "", false
	}
	return lines[number-1], true
}

func splitContentLines(content string) []string {
	if content == "" {
		return nil
	}
	lines := strings.Split(content, "\n")
	if strings.HasSuffix(content, "\n") {
		lines = lines[:len(lines)-1]
	}
	return lines
}

type goCommentScan struct {
	MarkersByLine  map[uint32]map[string]struct{}
	LexicallyValid bool
}

func scanGoComments(ctx context.Context, content string) (goCommentScan, error) {
	files := token.NewFileSet()
	file := files.AddFile("review.go", -1, len(content))
	errorCount := 0
	var lexer scanner.Scanner
	lexer.Init(file, []byte(content), func(token.Position, string) {
		errorCount++
	}, scanner.ScanComments)
	result := goCommentScan{MarkersByLine: make(map[uint32]map[string]struct{})}
	for {
		if err := checkContext(ctx); err != nil {
			return goCommentScan{}, err
		}
		position, tokenKind, literal := lexer.Scan()
		if tokenKind == token.EOF {
			break
		}
		if tokenKind != token.COMMENT {
			continue
		}
		startLine := uint32(files.Position(position).Line)
		for offset, commentLine := range strings.Split(literal, "\n") {
			line := startLine + uint32(offset)
			for _, rule := range orderedRegisteredRules {
				if rule.SignalKind != DetectionSignalLexicalMarker {
					continue
				}
				for _, signal := range rule.signals {
					if strings.Contains(commentLine, signal) {
						if result.MarkersByLine[line] == nil {
							result.MarkersByLine[line] = make(map[string]struct{})
						}
						result.MarkersByLine[line][signal] = struct{}{}
					}
				}
			}
		}
	}
	result.LexicallyValid = errorCount == 0
	return result, nil
}

func (scan goCommentScan) contains(line uint32, markers []string) bool {
	onLine := scan.MarkersByLine[line]
	for _, marker := range markers {
		if _, exists := onLine[marker]; exists {
			return true
		}
	}
	return false
}

func evidenceIDs(evidence []Evidence) []string {
	ids := make([]string, 0, len(evidence))
	for _, item := range evidence {
		ids = append(ids, item.ID)
	}
	sort.Strings(ids)
	return ids
}

func cloneFinding(finding Finding) Finding {
	copy := finding
	copy.Signals = slices.Clone(finding.Signals)
	copy.Evidence = slices.Clone(finding.Evidence)
	copy.Lineage = slices.Clone(finding.Lineage)
	copy.Verification.EvidenceIDs = slices.Clone(finding.Verification.EvidenceIDs)
	return copy
}

func cloneFindings(findings []Finding) []Finding {
	copies := make([]Finding, len(findings))
	for index, finding := range findings {
		copies[index] = cloneFinding(finding)
	}
	return copies
}

func cloneDecisions(decisions []FindingDecision) []FindingDecision {
	copies := make([]FindingDecision, len(decisions))
	for index, decision := range decisions {
		copies[index] = decision
		copies[index].EvidenceIDs = slices.Clone(decision.EvidenceIDs)
	}
	return copies
}

func summarize(findings []Finding, decisions []FindingDecision) (ReportSummary, error) {
	summary := ReportSummary{Findings: uint32(len(findings))}
	findingIDs := make(map[string]struct{}, len(findings))
	for _, finding := range findings {
		if _, duplicate := findingIDs[finding.ID]; duplicate {
			return ReportSummary{}, fmt.Errorf("report contains duplicate finding %q", finding.ID)
		}
		findingIDs[finding.ID] = struct{}{}
		switch finding.Verification.Status {
		case VerificationVerified:
			summary.Verified++
		case VerificationRejected:
			summary.Rejected++
		case VerificationInconclusive:
			summary.Inconclusive++
		default:
			return ReportSummary{}, fmt.Errorf("finding %q is not verified", finding.ID)
		}
	}
	decisionsByFinding := make(map[string][]FindingDecision, len(findings))
	decisionIDs := make(map[string]struct{}, len(decisions))
	for _, decision := range decisions {
		if _, exists := findingIDs[decision.FindingID]; !exists {
			return ReportSummary{}, fmt.Errorf("decision %q references unknown finding", decision.ID)
		}
		if _, duplicate := decisionIDs[decision.ID]; duplicate {
			return ReportSummary{}, fmt.Errorf("report contains duplicate decision %q", decision.ID)
		}
		decisionIDs[decision.ID] = struct{}{}
		decisionsByFinding[decision.FindingID] = append(
			decisionsByFinding[decision.FindingID], decision,
		)
	}
	for findingID := range findingIDs {
		history := decisionsByFinding[findingID]
		if len(history) == 0 {
			return ReportSummary{}, fmt.Errorf("finding %q requires an adjudication decision", findingID)
		}
		sort.Slice(history, func(i, j int) bool { return history[i].Sequence < history[j].Sequence })
		for index, decision := range history {
			expectedSequence := uint32(index + 1)
			if decision.Sequence != expectedSequence {
				return ReportSummary{}, fmt.Errorf(
					"finding %q decision history is not contiguous", findingID,
				)
			}
			if index > 0 && decision.PriorDecisionID != history[index-1].ID {
				return ReportSummary{}, fmt.Errorf(
					"finding %q decision history has a broken prior link", findingID,
				)
			}
			var finding *Finding
			for findingIndex := range findings {
				if findings[findingIndex].ID == findingID {
					finding = &findings[findingIndex]
					break
				}
			}
			availableEvidence := make(map[string]struct{}, len(finding.Evidence))
			for _, evidence := range finding.Evidence {
				availableEvidence[evidence.ID] = struct{}{}
			}
			for _, evidenceID := range decision.EvidenceIDs {
				if _, exists := availableEvidence[evidenceID]; !exists {
					return ReportSummary{}, fmt.Errorf(
						"decision %q references evidence absent from finding", decision.ID,
					)
				}
			}
		}
		switch history[len(history)-1].Action {
		case DecisionPublish:
			summary.Publish++
		case DecisionHumanReview:
			summary.HumanReview++
		case DecisionReject:
		}
	}
	return summary, nil
}

func checkContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func MarshalReportJSON(report Report) (json.RawMessage, error) {
	if err := report.Validate(); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal report JSON: %w", err)
	}
	return append(data, '\n'), nil
}
