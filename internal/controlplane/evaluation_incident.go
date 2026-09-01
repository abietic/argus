package controlplane

import (
	"fmt"
	"slices"
	"sort"

	"github.com/abietic/argus/internal/evaluation"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/targetmodel"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

func (service *Service) deriveMissedDefectIncidentCandidate(request evaluation.CandidateDerivationRequest) (evaluation.EvaluationCase, error) {
	if service.evaluationSources == nil {
		return evaluation.EvaluationCase{}, fmt.Errorf("evaluation source reader is not configured")
	}
	incident, err := service.evaluationSources.ResolveMissedDefectIncident(request.SourceID)
	if err != nil {
		return evaluation.EvaluationCase{}, fmt.Errorf("resolve missed-defect incident: %w", err)
	}
	if incident.ReviewRunID != request.RunID {
		return evaluation.EvaluationCase{}, fmt.Errorf("incident is bound to another review run")
	}
	if request.CollectedAt.Before(incident.RecordedAt) {
		return evaluation.EvaluationCase{}, fmt.Errorf("candidate was collected before the incident was recorded")
	}
	run, err := service.runs.LoadRun(request.RunID)
	if err != nil {
		return evaluation.EvaluationCase{}, fmt.Errorf("load incident review run: %w", err)
	}
	if run.Kind != runmodel.RunKindReview || run.Status != runmodel.RunStatusSucceeded || run.GovernedReportRef == nil {
		return evaluation.EvaluationCase{}, fmt.Errorf("incident source requires a succeeded formal non-replay ReviewRun")
	}
	if run.CompletedAt == nil || incident.OccurredAt.Before(*run.CompletedAt) {
		return evaluation.EvaluationCase{}, fmt.Errorf("incident must occur after the source review completed")
	}
	snapshot, err := service.runs.LoadExecutionSnapshot(run.ExecutionSnapshotID)
	if err != nil {
		return evaluation.EvaluationCase{}, fmt.Errorf("load incident execution snapshot: %w", err)
	}
	refs := []runmodel.ArtifactRef{snapshot.ReviewSpecRef, snapshot.TargetSnapshotRef, *run.GovernedReportRef}
	refs = append(refs, incident.EvidenceArtifacts...)
	for _, ref := range refs {
		if err := service.runs.CheckArtifactEligibility(ref, runrepo.ArtifactUseCandidatePool); err != nil {
			return evaluation.EvaluationCase{}, fmt.Errorf("incident source artifact is ineligible: %w", err)
		}
	}
	specBytes, err := service.runs.ReadArtifact(snapshot.ReviewSpecRef)
	if err != nil {
		return evaluation.EvaluationCase{}, fmt.Errorf("read incident ReviewSpec: %w", err)
	}
	spec, err := contractsv1alpha1.DecodeReviewSpec(specBytes)
	if err != nil {
		return evaluation.EvaluationCase{}, fmt.Errorf("decode incident ReviewSpec: %w", err)
	}
	targetBytes, err := service.runs.ReadArtifact(snapshot.TargetSnapshotRef)
	if err != nil {
		return evaluation.EvaluationCase{}, fmt.Errorf("read incident TargetSnapshot: %w", err)
	}
	target, err := targetmodel.DecodeMaterializedTarget(targetBytes)
	if err != nil {
		return evaluation.EvaluationCase{}, fmt.Errorf("decode incident TargetSnapshot: %w", err)
	}
	if err := validateIncidentAnchors(target, incident.Anchors); err != nil {
		return evaluation.EvaluationCase{}, err
	}
	reportBytes, err := service.runs.ReadArtifact(*run.GovernedReportRef)
	if err != nil {
		return evaluation.EvaluationCase{}, fmt.Errorf("read incident governed report: %w", err)
	}
	report, err := contractsv1alpha1.DecodeGovernedReviewReport(reportBytes)
	if err != nil {
		return evaluation.EvaluationCase{}, fmt.Errorf("decode incident governed report: %w", err)
	}
	if report.Completeness != contractsv1alpha1.AgentReviewComplete {
		return evaluation.EvaluationCase{}, fmt.Errorf("partial review cannot prove a missed defect")
	}
	if reportContainsIncident(report, incident) {
		return evaluation.EvaluationCase{}, fmt.Errorf("incident defect was present in the authoritative review report")
	}
	evidenceRefs := []string{snapshot.TargetSnapshotRef.URI, run.GovernedReportRef.URI}
	anchorRefs := make([]string, 0, len(incident.EvidenceArtifacts))
	for _, ref := range incident.EvidenceArtifacts {
		evidenceRefs = append(evidenceRefs, ref.URI)
		anchorRefs = append(anchorRefs, ref.URI)
	}
	sort.Strings(evidenceRefs)
	evidenceRefs = slices.Compact(evidenceRefs)
	sort.Strings(anchorRefs)
	anchorRefs = slices.Compact(anchorRefs)
	result := evaluation.EvaluationCase{
		SchemaVersion: evaluation.EvaluationCaseSchemaVersion, CaseID: request.CaseID,
		Type: evaluation.CaseMissedDefectRegression,
		Provenance: evaluation.SourceProvenance{
			Kind: evaluation.SourceIncidentMissedDefect, RepositoryID: spec.Repository.RepositoryID,
			SourceID: incident.IncidentID, SourceRunID: run.RunID,
			ObservedAt: incident.OccurredAt, CollectedAt: request.CollectedAt, EvidenceRefs: evidenceRefs,
		},
		LicenseConsent: evaluation.LicenseConsent{
			LicenseID: request.LicenseID, Consent: request.Consent,
			AllowedUses: []evaluation.UseScope{evaluation.UseCandidatePool}, Restrictions: slices.Clone(request.Restrictions),
		},
		Classification: request.Classification, Owner: request.Owner,
		InputSnapshotRef: snapshot.TargetSnapshotRef.URI,
		Label: evaluation.Label{
			ExpectedOutcome: evaluation.OutcomeMissedDefect, Category: incident.Category,
			Severity: incident.Severity, Anchors: slices.Clone(incident.Anchors),
			AnchorRefs: anchorRefs, SuppressionTargets: []evaluation.SuppressionTarget{},
		},
		LabelPolicyRevision: request.LabelPolicyRevision, ReviewState: evaluation.ReviewPending,
		DatasetState: evaluation.DatasetCandidatePool, Split: evaluation.SplitUnassigned,
		CloneGroupID: "incident-" + incident.DefectFingerprint[:24], Eligibility: evaluation.Eligibility{},
		CreatedAt: request.CollectedAt,
	}
	if err := result.Validate(); err != nil {
		return evaluation.EvaluationCase{}, fmt.Errorf("validate incident-derived candidate: %w", err)
	}
	return result, nil
}

func validateIncidentAnchors(target targetmodel.MaterializedTarget, anchors []evaluation.LabelAnchor) error {
	for _, anchor := range anchors {
		matched := false
		for _, file := range target.FileRefs {
			if file.Path == anchor.Path && file.SHA256 == anchor.SourceDigest {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("incident anchor %s:%d-%d is not bound to the frozen target", anchor.Path, anchor.StartLine, anchor.EndLine)
		}
		if target.Snapshot.Selection != nil {
			withinSelection := false
			for _, lineRange := range target.Snapshot.Selection.EffectiveRanges {
				if lineRange.StartLine <= anchor.StartLine && anchor.EndLine <= lineRange.EndLine {
					withinSelection = true
					break
				}
			}
			if !withinSelection {
				return fmt.Errorf("incident anchor %s:%d-%d is outside the frozen selection",
					anchor.Path, anchor.StartLine, anchor.EndLine)
			}
		}
	}
	return nil
}

func reportContainsIncident(report contractsv1alpha1.GovernedReviewReport, incident evaluation.MissedDefectIncident) bool {
	for _, candidate := range report.Candidates {
		if candidate.Hypothesis.ClusterFingerprint == incident.DefectFingerprint || incidentAnchorOverlaps(candidate.Hypothesis.Anchor, incident.Anchors) {
			return true
		}
	}
	return false
}

func incidentAnchorOverlaps(anchor contractsv1alpha1.HypothesisSourceAnchor, expected []evaluation.LabelAnchor) bool {
	for _, candidate := range expected {
		if anchor.Path == candidate.Path && string(anchor.Side) == candidate.Side &&
			anchor.SourceDigest == candidate.SourceDigest && anchor.StartLine <= candidate.EndLine && candidate.StartLine <= anchor.EndLine {
			return true
		}
	}
	return false
}
