package controlplane

import (
	"fmt"
	"slices"
	"sort"

	"github.com/abietic/argus/internal/evaluation"
	"github.com/abietic/argus/internal/feedback"
	"github.com/abietic/argus/internal/findingdecision"
	"github.com/abietic/argus/internal/findinglineage"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

type evaluationFindingFacts struct {
	fingerprint string
	category    string
	severity    string
	anchors     []evaluation.LabelAnchor
}

// DeriveEvaluationCandidate transforms immutable review facts into a pending
// candidate-pool case. It never creates gold/active data and never infers
// license, consent, classification, owner, or label-policy authority.
func (service *Service) DeriveEvaluationCandidate(
	request evaluation.CandidateDerivationRequest,
) (evaluation.EvaluationCase, error) {
	if err := request.Validate(); err != nil {
		return evaluation.EvaluationCase{}, err
	}
	if request.Source == evaluation.CandidateFromMissedDefectIncident {
		return service.deriveMissedDefectIncidentCandidate(request)
	}
	if request.Source == evaluation.CandidateFromMutationProbe ||
		request.Source == evaluation.CandidateFromSyntheticProbe ||
		request.Source == evaluation.CandidateFromWorkflowProbe {
		return service.deriveEvaluationProbeCandidate(request)
	}
	detail, err := service.Finding(request.RunID, request.FindingID)
	if err != nil {
		return evaluation.EvaluationCase{}, err
	}
	if detail.Run.Kind != runmodel.RunKindReview || detail.Run.Status != runmodel.RunStatusSucceeded {
		return evaluation.EvaluationCase{}, fmt.Errorf("only a succeeded non-replay ReviewRun may seed evaluation candidates")
	}
	root, err := decisionRoot(detail)
	if err != nil {
		return evaluation.EvaluationCase{}, err
	}
	sourceRef, err := publicationSourceRef(detail.Run, root)
	if err != nil {
		return evaluation.EvaluationCase{}, err
	}
	snapshot, err := service.runs.LoadExecutionSnapshot(detail.Run.ExecutionSnapshotID)
	if err != nil {
		return evaluation.EvaluationCase{}, fmt.Errorf("load candidate execution snapshot: %w", err)
	}
	for _, ref := range []runmodel.ArtifactRef{
		snapshot.ReviewSpecRef,
		snapshot.TargetSnapshotRef,
		publicationRunArtifactRef(sourceRef, root.SourceContract),
	} {
		if err := service.runs.CheckArtifactEligibility(ref, runrepo.ArtifactUseCandidatePool); err != nil {
			return evaluation.EvaluationCase{}, fmt.Errorf("candidate source artifact is ineligible: %w", err)
		}
	}
	specData, err := service.runs.ReadArtifact(snapshot.ReviewSpecRef)
	if err != nil {
		return evaluation.EvaluationCase{}, fmt.Errorf("read candidate ReviewSpec: %w", err)
	}
	spec, err := contractsv1alpha1.DecodeReviewSpec(specData)
	if err != nil {
		return evaluation.EvaluationCase{}, fmt.Errorf("decode candidate ReviewSpec: %w", err)
	}
	facts, err := projectEvaluationFinding(detail)
	if err != nil {
		return evaluation.EvaluationCase{}, err
	}
	if len(facts.fingerprint) < 24 {
		return evaluation.EvaluationCase{}, fmt.Errorf("finding fingerprint is not canonical")
	}

	caseType := evaluation.CasePositiveLocalized
	expected := evaluation.OutcomeDefectPresent
	provenanceKind := evaluation.SourceHumanConfirmedFinding
	observedAt := request.CollectedAt
	inputSnapshotRef := snapshot.TargetSnapshotRef
	extraEvidenceRefs := []runmodel.ArtifactRef{}
	switch request.Source {
	case evaluation.CandidateFromHumanDecision:
		decision, err := exactLatestPublishDecision(detail.HumanDecisions, request.SourceID)
		if err != nil {
			return evaluation.EvaluationCase{}, err
		}
		observedAt = decision.OccurredAt
		if request.CollectedAt.Before(decision.RecordedAt) {
			return evaluation.EvaluationCase{}, fmt.Errorf("candidate was collected before its decision was recorded")
		}
	case evaluation.CandidateFromProductionFeedback:
		fact, err := exactUnsupersededFeedback(detail.Feedback, request.SourceID)
		if err != nil {
			return evaluation.EvaluationCase{}, err
		}
		observedAt = fact.OccurredAt
		if request.CollectedAt.Before(fact.RecordedAt) {
			return evaluation.EvaluationCase{}, fmt.Errorf("candidate was collected before its feedback was recorded")
		}
		provenanceKind = evaluation.SourceProductionFeedback
		switch fact.Action {
		case feedback.FeedbackAccept:
		case feedback.FeedbackDismiss:
			caseType = evaluation.CaseFalsePositiveRegression
			expected = evaluation.OutcomeFalsePositive
		default:
			return evaluation.EvaluationCase{}, fmt.Errorf(
				"feedback action %q is ambiguous and cannot derive a provisional label", fact.Action,
			)
		}
	case evaluation.CandidateFromReviewedBugFixPair:
		outcome, err := exactUnsupersededFixedOutcome(detail.Outcomes, request.SourceID)
		if err != nil {
			return evaluation.EvaluationCase{}, err
		}
		if request.CollectedAt.Before(outcome.RecordedAt) {
			return evaluation.EvaluationCase{}, fmt.Errorf("candidate was collected before its outcome was recorded")
		}
		fixRun, fixSnapshot, fixSpec, reportRef, err := service.reviewedFixRun(request.FixRunID)
		if err != nil {
			return evaluation.EvaluationCase{}, err
		}
		if fixSpec.Repository.RepositoryID != spec.Repository.RepositoryID {
			return evaluation.EvaluationCase{}, fmt.Errorf("fix run belongs to a different repository")
		}
		if fixRun.TargetMode != runmodel.TargetModeDiff || fixRun.BaseRevision != detail.Run.HeadRevision {
			return evaluation.EvaluationCase{}, fmt.Errorf("fix run must be a diff based exactly on the defect run head revision")
		}
		if fixRun.CompletedAt == nil || outcome.OccurredAt.Before(*fixRun.CompletedAt) {
			return evaluation.EvaluationCase{}, fmt.Errorf("fixed outcome must occur after the fix review completed")
		}
		if err := validateFixedOutcomeBindings(outcome, fixRun.HeadRevision); err != nil {
			return evaluation.EvaluationCase{}, err
		}
		if service.lineages != nil {
			lineageRef, err := service.fixedFindingLineageEvidence(request.RunID, request.FixRunID, request.FindingID)
			if err != nil {
				return evaluation.EvaluationCase{}, err
			}
			extraEvidenceRefs = append(extraEvidenceRefs, lineageRef)
		}
		caseType = evaluation.CaseFixValidation
		expected = evaluation.OutcomeFixValid
		provenanceKind = evaluation.SourceReviewedBugFixPair
		observedAt = outcome.OccurredAt
		inputSnapshotRef = fixSnapshot.TargetSnapshotRef
		extraEvidenceRefs = append(extraEvidenceRefs, fixSnapshot.TargetSnapshotRef, reportRef)
	}

	label := evaluation.Label{
		ExpectedOutcome: expected, Category: facts.category, Severity: facts.severity,
		Anchors: []evaluation.LabelAnchor{}, AnchorRefs: []string{},
		SuppressionTargets: []evaluation.SuppressionTarget{},
	}
	if caseType == evaluation.CasePositiveLocalized {
		if len(facts.anchors) == 0 {
			return evaluation.EvaluationCase{}, fmt.Errorf("finding has no independently bound source anchor")
		}
		label.Anchors = facts.anchors
		label.AnchorRefs = []string{sourceRef.URI}
	} else if caseType == evaluation.CaseFalsePositiveRegression {
		label.SuppressionTargets = []evaluation.SuppressionTarget{{ClusterFingerprint: facts.fingerprint}}
	}
	evidenceRefs := []string{sourceRef.URI, snapshot.TargetSnapshotRef.URI}
	for _, ref := range extraEvidenceRefs {
		evidenceRefs = append(evidenceRefs, ref.URI)
	}
	sort.Strings(evidenceRefs)
	evidenceRefs = slices.Compact(evidenceRefs)
	result := evaluation.EvaluationCase{
		SchemaVersion: evaluation.EvaluationCaseSchemaVersion,
		CaseID:        request.CaseID, Type: caseType,
		Provenance: evaluation.SourceProvenance{
			Kind: provenanceKind, RepositoryID: spec.Repository.RepositoryID,
			SourceID: request.SourceID, SourceRunID: request.RunID, FindingID: request.FindingID,
			ObservedAt: observedAt, CollectedAt: request.CollectedAt, EvidenceRefs: evidenceRefs,
		},
		LicenseConsent: evaluation.LicenseConsent{
			LicenseID: request.LicenseID, Consent: request.Consent,
			AllowedUses:  []evaluation.UseScope{evaluation.UseCandidatePool},
			Restrictions: slices.Clone(request.Restrictions),
		},
		Classification: request.Classification, Owner: request.Owner,
		InputSnapshotRef: inputSnapshotRef.URI,
		Label:            label, LabelPolicyRevision: request.LabelPolicyRevision,
		ReviewState: evaluation.ReviewPending, DatasetState: evaluation.DatasetCandidatePool,
		Split:        evaluation.SplitUnassigned,
		CloneGroupID: "finding-" + facts.fingerprint[:24], Eligibility: evaluation.Eligibility{},
		CreatedAt: request.CollectedAt,
	}
	if err := result.Validate(); err != nil {
		return evaluation.EvaluationCase{}, fmt.Errorf("validate derived evaluation candidate: %w", err)
	}
	return result, nil
}

func (service *Service) fixedFindingLineageEvidence(
	defectRunID, fixRunID, findingID string,
) (runmodel.ArtifactRef, error) {
	records, err := service.lineages.List(findinglineage.ListFilter{RunID: defectRunID})
	if err != nil {
		return runmodel.ArtifactRef{}, fmt.Errorf("list bug-fix finding lineage: %w", err)
	}
	var selected *findinglineage.Record
	for index := range records {
		record := records[index]
		if record.Lineage.Baseline.RunID != defectRunID || record.Lineage.Variant.RunID != fixRunID ||
			record.Lineage.Policy.AncestryAuthority != "local_git_object_graph" {
			continue
		}
		resolved := false
		for _, relation := range record.Lineage.Relations {
			if relation.Type == contractsv1alpha1.FindingLineageResolved &&
				slices.Equal(relation.BaselineFindingIDs, []string{findingID}) {
				resolved = true
				break
			}
		}
		if !resolved {
			continue
		}
		if selected != nil {
			return runmodel.ArtifactRef{}, fmt.Errorf("multiple Git-aware lineages resolve the source Finding")
		}
		copy := record
		selected = &copy
	}
	if selected == nil {
		return runmodel.ArtifactRef{}, fmt.Errorf("reviewed bug-fix pair requires Git-aware lineage resolving the source Finding")
	}
	return selected.LineageRef, nil
}

func exactLatestPublishDecision(
	decisions []findingdecision.Decision,
	id string,
) (findingdecision.Decision, error) {
	if len(decisions) == 0 || decisions[len(decisions)-1].DecisionID != id {
		return findingdecision.Decision{}, fmt.Errorf("source decision must be the latest decision for the Finding")
	}
	decision := decisions[len(decisions)-1]
	if decision.Action != findingdecision.ActionPublish {
		return findingdecision.Decision{}, fmt.Errorf("source decision is not an explicit publish decision")
	}
	return decision, nil
}

func exactUnsupersededFeedback(
	facts []feedback.Feedback,
	id string,
) (feedback.Feedback, error) {
	var selected *feedback.Feedback
	for index := range facts {
		if facts[index].PriorFeedbackID == id {
			return feedback.Feedback{}, fmt.Errorf("source feedback has been superseded")
		}
		if facts[index].FeedbackID == id {
			copy := facts[index]
			selected = &copy
		}
	}
	if selected == nil {
		return feedback.Feedback{}, fmt.Errorf("source feedback %q was not found on the Finding", id)
	}
	return *selected, nil
}

func exactUnsupersededFixedOutcome(
	facts []feedback.Outcome,
	id string,
) (feedback.Outcome, error) {
	if len(facts) == 0 || facts[len(facts)-1].OutcomeID != id {
		return feedback.Outcome{}, fmt.Errorf("source outcome must be the latest outcome for the Finding")
	}
	var selected *feedback.Outcome
	for index := range facts {
		if facts[index].PriorOutcomeID == id {
			return feedback.Outcome{}, fmt.Errorf("source outcome has been superseded")
		}
		if facts[index].OutcomeID == id {
			copy := facts[index]
			selected = &copy
		}
	}
	if selected == nil {
		return feedback.Outcome{}, fmt.Errorf("source outcome %q was not found on the Finding", id)
	}
	if selected.State != feedback.OutcomeFixed {
		return feedback.Outcome{}, fmt.Errorf("source outcome must assert fixed")
	}
	return *selected, nil
}

func (service *Service) reviewedFixRun(
	runID string,
) (runmodel.ReviewRun, runmodel.ExecutionSnapshot, contractsv1alpha1.ReviewSpec, runmodel.ArtifactRef, error) {
	run, err := service.runs.LoadRun(runID)
	if err != nil {
		return runmodel.ReviewRun{}, runmodel.ExecutionSnapshot{}, contractsv1alpha1.ReviewSpec{}, runmodel.ArtifactRef{},
			fmt.Errorf("load fix review run: %w", err)
	}
	if run.Kind != runmodel.RunKindReview || run.Status != runmodel.RunStatusSucceeded {
		return runmodel.ReviewRun{}, runmodel.ExecutionSnapshot{}, contractsv1alpha1.ReviewSpec{}, runmodel.ArtifactRef{},
			fmt.Errorf("fix run must be a succeeded non-replay ReviewRun")
	}
	var reportRef runmodel.ArtifactRef
	switch {
	case run.GovernedReportRef != nil:
		reportRef = *run.GovernedReportRef
	case run.JSONReportRef != nil:
		reportRef = *run.JSONReportRef
	default:
		return runmodel.ReviewRun{}, runmodel.ExecutionSnapshot{}, contractsv1alpha1.ReviewSpec{}, runmodel.ArtifactRef{},
			fmt.Errorf("fix run has no authoritative review report")
	}
	snapshot, err := service.runs.LoadExecutionSnapshot(run.ExecutionSnapshotID)
	if err != nil {
		return runmodel.ReviewRun{}, runmodel.ExecutionSnapshot{}, contractsv1alpha1.ReviewSpec{}, runmodel.ArtifactRef{},
			fmt.Errorf("load fix execution snapshot: %w", err)
	}
	for _, ref := range []runmodel.ArtifactRef{snapshot.ReviewSpecRef, snapshot.TargetSnapshotRef, reportRef} {
		if err := service.runs.CheckArtifactEligibility(ref, runrepo.ArtifactUseCandidatePool); err != nil {
			return runmodel.ReviewRun{}, runmodel.ExecutionSnapshot{}, contractsv1alpha1.ReviewSpec{}, runmodel.ArtifactRef{},
				fmt.Errorf("fix source artifact is ineligible: %w", err)
		}
	}
	data, err := service.runs.ReadArtifact(snapshot.ReviewSpecRef)
	if err != nil {
		return runmodel.ReviewRun{}, runmodel.ExecutionSnapshot{}, contractsv1alpha1.ReviewSpec{}, runmodel.ArtifactRef{},
			fmt.Errorf("read fix ReviewSpec: %w", err)
	}
	spec, err := contractsv1alpha1.DecodeReviewSpec(data)
	if err != nil {
		return runmodel.ReviewRun{}, runmodel.ExecutionSnapshot{}, contractsv1alpha1.ReviewSpec{}, runmodel.ArtifactRef{},
			fmt.Errorf("decode fix ReviewSpec: %w", err)
	}
	return run, snapshot, spec, reportRef, nil
}

func validateFixedOutcomeBindings(outcome feedback.Outcome, fixRevision string) error {
	changeBound := false
	ciBound := false
	for _, ref := range outcome.SourceRefs {
		if ref.Revision != fixRevision {
			continue
		}
		switch ref.Kind {
		case feedback.SourceRefChange:
			changeBound = true
		case feedback.SourceRefCIRun:
			ciBound = true
		}
	}
	if !changeBound || !ciBound {
		return fmt.Errorf("fixed outcome must bind both change and CI facts to fix head revision %q", fixRevision)
	}
	return nil
}

func projectEvaluationFinding(detail FindingDetail) (evaluationFindingFacts, error) {
	if detail.GovernedFinding != nil {
		finding := detail.GovernedFinding
		return evaluationFindingFacts{
			fingerprint: finding.Fingerprint, category: finding.Category,
			severity: string(finding.Severity),
			anchors: []evaluation.LabelAnchor{{
				Path: finding.Anchor.Path, Side: string(finding.Anchor.Side),
				StartLine: finding.Anchor.StartLine, EndLine: finding.Anchor.EndLine,
				SourceDigest: finding.Anchor.SourceDigest,
			}},
		}, nil
	}
	if detail.Finding == nil {
		return evaluationFindingFacts{}, fmt.Errorf("finding projection is unavailable")
	}
	finding := detail.Finding
	side := "file"
	if detail.Run.TargetMode == runmodel.TargetModeDiff {
		side = "new"
	}
	anchors := make([]evaluation.LabelAnchor, 0, len(finding.Evidence))
	for _, evidence := range finding.Evidence {
		if evidence.Path != finding.Path || evidence.SourceDigest == "" || evidence.Line == 0 {
			continue
		}
		anchors = append(anchors, evaluation.LabelAnchor{
			Path: evidence.Path, Side: side, StartLine: evidence.Line, EndLine: evidence.Line,
			SourceDigest: evidence.SourceDigest,
		})
	}
	sort.Slice(anchors, func(left, right int) bool {
		l, r := anchors[left], anchors[right]
		if l.Path != r.Path {
			return l.Path < r.Path
		}
		if l.Side != r.Side {
			return l.Side < r.Side
		}
		if l.StartLine != r.StartLine {
			return l.StartLine < r.StartLine
		}
		if l.EndLine != r.EndLine {
			return l.EndLine < r.EndLine
		}
		return l.SourceDigest < r.SourceDigest
	})
	anchors = slices.Compact(anchors)
	return evaluationFindingFacts{
		fingerprint: finding.Fingerprint, category: finding.RuleID,
		severity: finding.Severity, anchors: anchors,
	}, nil
}
