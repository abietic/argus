package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/abietic/argus/internal/reviewcore"
	"github.com/abietic/argus/internal/runmodel"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

type RunReader interface {
	LoadRun(string) (runmodel.ReviewRun, error)
	LoadExecutionSnapshot(string) (runmodel.ExecutionSnapshot, error)
	ReadArtifact(runmodel.ArtifactRef) ([]byte, error)
}

type comparableFinding struct {
	Fingerprint string
	Title       string
	Path        string
	Line        int
}

type comparableCandidate struct {
	MatchKey    string
	CandidateID string
	Fingerprint string
	RuleID      string
	Path        string
	Line        int
}

type comparableDecision struct {
	FindingID   string
	Fingerprint string
	Action      string
}

type comparableReport struct {
	TargetDigest string
	Findings     []comparableFinding
	Candidates   []comparableCandidate
	Decisions    []comparableDecision
}

func CompareRuns(
	ctx context.Context,
	reader RunReader,
	baselineRunID string,
	variantRunID string,
	generatedAt time.Time,
) (runmodel.RunComparison, error) {
	if ctx == nil || reader == nil {
		return runmodel.RunComparison{}, fmt.Errorf("comparison dependencies are required")
	}
	if baselineRunID == "" || variantRunID == "" || baselineRunID == variantRunID {
		return runmodel.RunComparison{}, fmt.Errorf("two distinct run IDs are required")
	}
	if generatedAt.IsZero() {
		return runmodel.RunComparison{}, fmt.Errorf("comparison time is required")
	}
	baseline, baselineReport, err := loadComparableReport(ctx, reader, baselineRunID)
	if err != nil {
		return runmodel.RunComparison{}, fmt.Errorf("load baseline: %w", err)
	}
	variant, variantReport, err := loadComparableReport(ctx, reader, variantRunID)
	if err != nil {
		return runmodel.RunComparison{}, fmt.Errorf("load variant: %w", err)
	}
	directIndexVariant := variant.Kind == runmodel.RunKindReplay &&
		variant.SourceRunID == baseline.RunID &&
		variant.ReplayVariable == runmodel.ReplayVariableIndex
	if baseline.TargetSnapshotRef != variant.TargetSnapshotRef && !directIndexVariant {
		return runmodel.RunComparison{}, fmt.Errorf("runs do not share the exact materialized target")
	}
	if baselineReport.TargetDigest != variantReport.TargetDigest && !directIndexVariant {
		return runmodel.RunComparison{}, fmt.Errorf("run reports do not share the exact review input")
	}

	baselineFindings := findingsByFingerprint(baselineReport.Findings)
	variantFindings := findingsByFingerprint(variantReport.Findings)
	comparison := runmodel.RunComparison{
		SchemaVersion:       runmodel.ComparisonSchemaVersion,
		BaselineRunID:       baselineRunID,
		VariantRunID:        variantRunID,
		Added:               []runmodel.FindingChange{},
		Removed:             []runmodel.FindingChange{},
		Unchanged:           []runmodel.FindingChange{},
		CandidatesAdded:     []runmodel.CandidateChange{},
		CandidatesRemoved:   []runmodel.CandidateChange{},
		CandidatesUnchanged: []runmodel.CandidateChange{},
		DecisionChanges:     []runmodel.DecisionChange{},
		StageLatency:        []runmodel.StageLatencyDelta{},
		Quality: runmodel.MetricAvailability{
			Available: false, ReasonCode: "evaluation_labels_required",
		},
		Cost: runmodel.MetricAvailability{
			Available: false, ReasonCode: "cost_evidence_not_recorded",
		},
		GeneratedAt: generatedAt.UTC(),
	}
	for fingerprint, finding := range baselineFindings {
		if _, exists := variantFindings[fingerprint]; exists {
			comparison.Unchanged = append(comparison.Unchanged, findingChange(finding))
		} else {
			comparison.Removed = append(comparison.Removed, findingChange(finding))
		}
	}
	for fingerprint, finding := range variantFindings {
		if _, exists := baselineFindings[fingerprint]; !exists {
			comparison.Added = append(comparison.Added, findingChange(finding))
		}
	}
	for _, changes := range [][]runmodel.FindingChange{
		comparison.Added,
		comparison.Removed,
		comparison.Unchanged,
	} {
		sort.Slice(changes, func(left, right int) bool {
			return changes[left].Fingerprint < changes[right].Fingerprint
		})
	}
	comparison.CandidatesAdded, comparison.CandidatesRemoved,
		comparison.CandidatesUnchanged = compareCandidates(
		baselineReport.Candidates, variantReport.Candidates,
	)
	for _, changes := range [][]runmodel.CandidateChange{
		comparison.CandidatesAdded,
		comparison.CandidatesRemoved,
		comparison.CandidatesUnchanged,
	} {
		sort.Slice(changes, func(left, right int) bool {
			if changes[left].Fingerprint != changes[right].Fingerprint {
				return changes[left].Fingerprint < changes[right].Fingerprint
			}
			return changes[left].CandidateID < changes[right].CandidateID
		})
	}
	comparison.DecisionChanges = compareDecisions(baselineReport, variantReport)
	comparison.StageLatency, err = compareStageLatency(reader, baseline, variant)
	if err != nil {
		return runmodel.RunComparison{}, err
	}
	return comparison, nil
}

func loadComparableReport(
	ctx context.Context,
	reader RunReader,
	runID string,
) (runmodel.ReviewRun, comparableReport, error) {
	if err := ctx.Err(); err != nil {
		return runmodel.ReviewRun{}, comparableReport{}, err
	}
	run, err := reader.LoadRun(runID)
	if err != nil {
		return runmodel.ReviewRun{}, comparableReport{}, err
	}
	if run.Status != runmodel.RunStatusSucceeded {
		return runmodel.ReviewRun{}, comparableReport{}, fmt.Errorf("run %q has no successful report", runID)
	}
	if run.JSONReportRef != nil {
		report, err := loadDeterministicComparableReport(ctx, reader, run)
		return run, report, err
	}
	if run.GovernedReportRef != nil && run.CandidateSetRef != nil {
		report, err := loadFormalComparableReport(reader, run)
		return run, report, err
	}
	return runmodel.ReviewRun{}, comparableReport{}, fmt.Errorf(
		"run %q has no supported successful report closure", runID,
	)
}

func findingsByFingerprint(findings []comparableFinding) map[string]comparableFinding {
	index := make(map[string]comparableFinding, len(findings))
	for _, finding := range findings {
		index[finding.Fingerprint] = finding
	}
	return index
}

func findingChange(finding comparableFinding) runmodel.FindingChange {
	return runmodel.FindingChange{
		Fingerprint: finding.Fingerprint,
		Title:       finding.Title,
		Path:        finding.Path,
		Line:        finding.Line,
	}
}

func candidateChange(candidate comparableCandidate, variantCandidateID string) runmodel.CandidateChange {
	return runmodel.CandidateChange{
		CandidateID:        candidate.CandidateID,
		VariantCandidateID: variantCandidateID,
		Fingerprint:        candidate.Fingerprint,
		RuleID:             candidate.RuleID,
		Path:               candidate.Path,
		Line:               candidate.Line,
	}
}

func candidatesByMatchKey(
	candidates []comparableCandidate,
) map[string]comparableCandidate {
	index := make(map[string]comparableCandidate, len(candidates))
	for _, candidate := range candidates {
		index[candidate.MatchKey] = candidate
	}
	return index
}

func compareCandidates(
	baseline []comparableCandidate,
	variant []comparableCandidate,
) (added, removed, unchanged []runmodel.CandidateChange) {
	baselineCandidates := candidatesByMatchKey(baseline)
	variantCandidates := candidatesByMatchKey(variant)
	for matchKey, candidate := range baselineCandidates {
		if variantCandidate, exists := variantCandidates[matchKey]; exists {
			unchanged = append(
				unchanged,
				candidateChange(candidate, variantCandidate.CandidateID),
			)
		} else {
			removed = append(removed, candidateChange(candidate, ""))
		}
	}
	for matchKey, candidate := range variantCandidates {
		if _, exists := baselineCandidates[matchKey]; !exists {
			added = append(added, candidateChange(candidate, ""))
		}
	}
	return added, removed, unchanged
}

func loadDeterministicComparableReport(
	ctx context.Context,
	reader RunReader,
	run runmodel.ReviewRun,
) (comparableReport, error) {
	data, err := reader.ReadArtifact(*run.JSONReportRef)
	if err != nil {
		return comparableReport{}, err
	}
	report, err := reviewcore.DecodeReport(data)
	if err != nil {
		return comparableReport{}, err
	}
	result, err := loadStageResult(ctx, reader, run, reviewcore.StageDetect)
	if err != nil {
		return comparableReport{}, err
	}
	comparable := comparableReport{TargetDigest: report.TargetDigest}
	for _, finding := range report.Findings {
		comparable.Findings = append(comparable.Findings, comparableFinding{
			Fingerprint: finding.Fingerprint, Title: finding.Title,
			Path: finding.Path, Line: int(finding.StartLine),
		})
	}
	for _, candidate := range result.Output.CandidateFindings {
		matchKey, err := deterministicCandidateComparisonKey(candidate)
		if err != nil {
			return comparableReport{}, err
		}
		comparable.Candidates = append(comparable.Candidates, comparableCandidate{
			MatchKey: matchKey, CandidateID: candidate.ID, Fingerprint: matchKey,
			RuleID: candidate.RuleID,
			Path:   candidate.Path, Line: int(candidate.StartLine),
		})
	}
	fingerprints := make(map[string]string, len(report.Findings))
	for _, finding := range report.Findings {
		fingerprints[finding.ID] = finding.Fingerprint
	}
	for _, decision := range report.Decisions {
		comparable.Decisions = append(comparable.Decisions, comparableDecision{
			FindingID: decision.FindingID, Fingerprint: fingerprints[decision.FindingID],
			Action: string(decision.Action),
		})
	}
	return comparable, nil
}

func deterministicCandidateComparisonKey(
	candidate reviewcore.CandidateFinding,
) (string, error) {
	identity := struct {
		DetectorID       string                         `json:"detector_id"`
		DetectorRevision string                         `json:"detector_revision"`
		RuleID           string                         `json:"rule_id"`
		Path             string                         `json:"path"`
		StartLine        uint32                         `json:"start_line"`
		EndLine          uint32                         `json:"end_line"`
		SignalKind       reviewcore.DetectionSignalKind `json:"signal_kind"`
		Signal           string                         `json:"signal"`
		SignalOffset     uint32                         `json:"signal_offset"`
		Excerpt          string                         `json:"excerpt"`
	}{
		DetectorID: candidate.DetectorID, DetectorRevision: candidate.DetectorRevision,
		RuleID: candidate.RuleID, Path: candidate.Path,
		StartLine: candidate.StartLine, EndLine: candidate.EndLine,
		SignalKind: candidate.SignalKind, Signal: candidate.Signal,
		SignalOffset: candidate.SignalOffset, Excerpt: candidate.Excerpt,
	}
	data, err := json.Marshal(identity)
	if err != nil {
		return "", fmt.Errorf("marshal deterministic candidate comparison identity: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func loadFormalComparableReport(
	reader RunReader,
	run runmodel.ReviewRun,
) (comparableReport, error) {
	if run.CandidateSetRef == nil || run.VerificationLedgerRef == nil ||
		run.CalibrationLedgerRef == nil || run.SuppressionLedgerRef == nil ||
		run.GovernedReportRef == nil {
		return comparableReport{}, fmt.Errorf("formal comparison requires complete governed ledger refs")
	}
	candidateData, err := reader.ReadArtifact(*run.CandidateSetRef)
	if err != nil {
		return comparableReport{}, err
	}
	candidateSet, err := contractsv1alpha1.DecodeGovernedCandidateSet(candidateData)
	if err != nil {
		return comparableReport{}, err
	}
	verificationData, err := reader.ReadArtifact(*run.VerificationLedgerRef)
	if err != nil {
		return comparableReport{}, err
	}
	verification, err := contractsv1alpha1.DecodeCandidateVerificationLedger(verificationData)
	if err != nil {
		return comparableReport{}, err
	}
	if err := candidateSet.ValidateAgainstVerificationLedger(verification); err != nil {
		return comparableReport{}, err
	}
	reportData, err := reader.ReadArtifact(*run.GovernedReportRef)
	if err != nil {
		return comparableReport{}, err
	}
	report, err := contractsv1alpha1.DecodeGovernedReviewReport(reportData)
	if err != nil {
		return comparableReport{}, err
	}
	if err := report.ValidateAgainstVerificationLedger(verification); err != nil {
		return comparableReport{}, err
	}
	calibrationData, err := reader.ReadArtifact(*run.CalibrationLedgerRef)
	if err != nil {
		return comparableReport{}, err
	}
	calibration, err := contractsv1alpha1.DecodeFindingCalibrationLedger(calibrationData)
	if err != nil {
		return comparableReport{}, err
	}
	if err := calibration.ValidateAgainst(report, verification); err != nil {
		return comparableReport{}, err
	}
	suppressionData, err := reader.ReadArtifact(*run.SuppressionLedgerRef)
	if err != nil {
		return comparableReport{}, err
	}
	suppression, err := contractsv1alpha1.DecodeFindingSuppressionLedger(suppressionData)
	if err != nil {
		return comparableReport{}, err
	}
	if err := suppression.ValidateAgainst(report, calibration); err != nil {
		return comparableReport{}, err
	}
	if candidateSet.ReviewRunID != run.RunID || report.ReviewRunID != run.RunID ||
		candidateSet.ExecutionID != report.ExecutionID ||
		candidateSet.HypothesisSetID != report.HypothesisSetID ||
		candidateSet.TargetDigest != report.TargetDigest ||
		!reflect.DeepEqual(candidateSet.Candidates, report.Candidates) {
		return comparableReport{}, fmt.Errorf(
			"formal candidate and report artifacts do not form one exact run closure",
		)
	}
	comparable := comparableReport{TargetDigest: report.TargetDigest}
	for _, finding := range report.Findings {
		comparable.Findings = append(comparable.Findings, comparableFinding{
			Fingerprint: finding.Fingerprint, Title: finding.Title,
			Path: finding.Anchor.Path, Line: int(finding.Anchor.StartLine),
		})
	}
	for _, candidate := range candidateSet.Candidates {
		hypothesis := candidate.Hypothesis
		comparable.Candidates = append(comparable.Candidates, comparableCandidate{
			MatchKey: hypothesis.ClusterFingerprint, CandidateID: candidate.CandidateID,
			Fingerprint: hypothesis.ClusterFingerprint, RuleID: hypothesis.Dimension.ID,
			Path: hypothesis.Anchor.Path, Line: int(hypothesis.Anchor.StartLine),
		})
	}
	for _, decision := range report.Decisions {
		fingerprint := ""
		for _, finding := range report.Findings {
			if finding.FindingID == decision.FindingID {
				fingerprint = finding.Fingerprint
				break
			}
		}
		comparable.Decisions = append(comparable.Decisions, comparableDecision{
			FindingID: decision.FindingID, Fingerprint: fingerprint,
			Action: string(decision.Action),
		})
	}
	return comparable, nil
}

func loadStageResult(
	ctx context.Context,
	reader RunReader,
	run runmodel.ReviewRun,
	stage reviewcore.StageName,
) (reviewcore.StageResult, error) {
	if err := ctx.Err(); err != nil {
		return reviewcore.StageResult{}, err
	}
	snapshot, err := reader.LoadExecutionSnapshot(run.ExecutionSnapshotID)
	if err != nil {
		return reviewcore.StageResult{}, err
	}
	for _, ref := range snapshot.ReplayInputRefs {
		data, err := reader.ReadArtifact(ref)
		if err != nil {
			return reviewcore.StageResult{}, err
		}
		result, err := reviewcore.DecodeStageResult(data)
		if err != nil {
			return reviewcore.StageResult{}, err
		}
		if result.Stage == stage {
			return result, nil
		}
	}
	var latest *runmodel.StageAttempt
	for index := range run.StageAttempts {
		attempt := &run.StageAttempts[index]
		if attempt.StageID != string(stage) ||
			attempt.Status != runmodel.StageStatusSucceeded ||
			attempt.OutputRef == nil {
			continue
		}
		if latest == nil || attempt.Generation > latest.Generation ||
			attempt.Generation == latest.Generation && attempt.Attempt > latest.Attempt {
			latest = attempt
		}
	}
	if latest == nil {
		return reviewcore.StageResult{}, fmt.Errorf(
			"run %q has no authoritative %s output", run.RunID, stage,
		)
	}
	data, err := reader.ReadArtifact(*latest.OutputRef)
	if err != nil {
		return reviewcore.StageResult{}, err
	}
	return reviewcore.DecodeStageResult(data)
}

func compareDecisions(
	baseline comparableReport,
	variant comparableReport,
) []runmodel.DecisionChange {
	type projected struct {
		fingerprint string
		action      string
	}
	project := func(report comparableReport) map[string]projected {
		latest := make(map[string]comparableDecision)
		for _, decision := range report.Decisions {
			latest[decision.FindingID] = decision
		}
		result := make(map[string]projected, len(latest))
		for findingID, decision := range latest {
			result[findingID] = projected{
				fingerprint: decision.Fingerprint,
				action:      decision.Action,
			}
		}
		return result
	}
	baselineDecisions := project(baseline)
	variantDecisions := project(variant)
	ids := make(map[string]struct{}, len(baselineDecisions)+len(variantDecisions))
	for id := range baselineDecisions {
		ids[id] = struct{}{}
	}
	for id := range variantDecisions {
		ids[id] = struct{}{}
	}
	changes := make([]runmodel.DecisionChange, 0)
	for id := range ids {
		before := baselineDecisions[id]
		after := variantDecisions[id]
		if before.action == after.action {
			continue
		}
		fingerprint := before.fingerprint
		if fingerprint == "" {
			fingerprint = after.fingerprint
		}
		changes = append(changes, runmodel.DecisionChange{
			FindingID:      id,
			Fingerprint:    fingerprint,
			BaselineAction: before.action,
			VariantAction:  after.action,
		})
	}
	sort.Slice(changes, func(left, right int) bool {
		return changes[left].FindingID < changes[right].FindingID
	})
	return changes
}

func compareStageLatency(
	reader RunReader,
	baseline runmodel.ReviewRun,
	variant runmodel.ReviewRun,
) ([]runmodel.StageLatencyDelta, error) {
	type latency struct {
		duration int64
		reused   bool
	}
	project := func(run runmodel.ReviewRun) (map[string]latency, error) {
		result := make(map[string]latency)
		for _, attempt := range run.StageAttempts {
			value := result[attempt.StageID]
			value.duration += attempt.DurationMS
			result[attempt.StageID] = value
		}
		snapshot, err := reader.LoadExecutionSnapshot(run.ExecutionSnapshotID)
		if err != nil {
			return nil, err
		}
		for _, ref := range snapshot.ReplayInputRefs {
			data, err := reader.ReadArtifact(ref)
			if err != nil {
				return nil, err
			}
			stage, err := reviewcore.DecodeStageResult(data)
			if err != nil {
				return nil, err
			}
			value := result[string(stage.Stage)]
			value.reused = true
			result[string(stage.Stage)] = value
		}
		return result, nil
	}
	baselineLatency, err := project(baseline)
	if err != nil {
		return nil, fmt.Errorf("load baseline latency: %w", err)
	}
	variantLatency, err := project(variant)
	if err != nil {
		return nil, fmt.Errorf("load variant latency: %w", err)
	}
	stageOrder := []reviewcore.StageName{
		reviewcore.StageMaterializeTarget,
		reviewcore.StagePlanContext,
		reviewcore.StageDetect,
		reviewcore.StageNormalize,
		reviewcore.StageVerify,
		reviewcore.StageAdjudicate,
		reviewcore.StageReport,
		reviewcore.StagePublish,
		reviewcore.StageCaptureFeedback,
		reviewcore.StageExportEvaluation,
	}
	result := make([]runmodel.StageLatencyDelta, 0, len(stageOrder)+1)
	for _, stage := range stageOrder {
		before := baselineLatency[string(stage)]
		after := variantLatency[string(stage)]
		result = append(result, runmodel.StageLatencyDelta{
			StageID:            string(stage),
			BaselineDurationMS: before.duration,
			VariantDurationMS:  after.duration,
			DeltaMS:            after.duration - before.duration,
			BaselineReused:     before.reused,
			VariantReused:      after.reused,
		})
	}
	const formalStage = "agent_hypothesize"
	if _, baselineExists := baselineLatency[formalStage]; baselineExists {
		before := baselineLatency[formalStage]
		after := variantLatency[formalStage]
		result = append(result, runmodel.StageLatencyDelta{
			StageID: formalStage, BaselineDurationMS: before.duration,
			VariantDurationMS: after.duration, DeltaMS: after.duration - before.duration,
			BaselineReused: before.reused, VariantReused: after.reused,
		})
	} else if after, variantExists := variantLatency[formalStage]; variantExists {
		result = append(result, runmodel.StageLatencyDelta{
			StageID: formalStage, VariantDurationMS: after.duration,
			DeltaMS: after.duration, VariantReused: after.reused,
		})
	}
	return result, nil
}
