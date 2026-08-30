package controlplane

import (
	"fmt"
	"slices"
	"sort"

	"argus.local/argus/internal/evaluation"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/runrepo"
	"argus.local/argus/internal/targetmodel"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

func (service *Service) deriveEvaluationProbeCandidate(
	request evaluation.CandidateDerivationRequest,
) (evaluation.EvaluationCase, error) {
	if service.evaluationSources == nil {
		return evaluation.EvaluationCase{}, fmt.Errorf("evaluation source reader is not configured")
	}
	probe, err := service.evaluationSources.ResolveEvaluationProbe(request.SourceID)
	if err != nil {
		return evaluation.EvaluationCase{}, fmt.Errorf("resolve evaluation probe: %w", err)
	}
	if probe.ReviewRunID != request.RunID {
		return evaluation.EvaluationCase{}, fmt.Errorf("probe is bound to another review run")
	}
	if request.CollectedAt.Before(probe.RecordedAt) {
		return evaluation.EvaluationCase{}, fmt.Errorf("candidate was collected before the probe was recorded")
	}
	if err := validateProbeRequestKind(request.Source, probe.Kind); err != nil {
		return evaluation.EvaluationCase{}, err
	}
	if (probe.Kind == evaluation.ProbeSyntheticDefect ||
		probe.Kind == evaluation.ProbeSyntheticClean ||
		probe.Kind == evaluation.ProbeWorkflowInvariant) && request.Consent != evaluation.ConsentSynthetic {
		return evaluation.EvaluationCase{}, fmt.Errorf("synthetic probe requires synthetic consent")
	}
	run, err := service.runs.LoadRun(request.RunID)
	if err != nil {
		return evaluation.EvaluationCase{}, fmt.Errorf("load probe review run: %w", err)
	}
	if run.Kind != runmodel.RunKindReview || run.Status != runmodel.RunStatusSucceeded ||
		run.GovernedReportRef == nil || run.CompletedAt == nil {
		return evaluation.EvaluationCase{}, fmt.Errorf("probe source requires a succeeded formal non-replay ReviewRun")
	}
	snapshot, err := service.runs.LoadExecutionSnapshot(run.ExecutionSnapshotID)
	if err != nil {
		return evaluation.EvaluationCase{}, fmt.Errorf("load probe execution snapshot: %w", err)
	}
	refs := []runmodel.ArtifactRef{snapshot.ReviewSpecRef, snapshot.TargetSnapshotRef, probe.ReceiptRef}
	refs = append(refs, probe.Oracle.EvidenceArtifacts...)
	for _, ref := range refs {
		if err := service.runs.CheckArtifactEligibility(ref, runrepo.ArtifactUseCandidatePool); err != nil {
			return evaluation.EvaluationCase{}, fmt.Errorf("probe source artifact is ineligible: %w", err)
		}
	}
	for _, ref := range probe.Oracle.EvidenceArtifacts {
		if isReviewGeneratedOracleEvidence(ref.Contract) {
			return evaluation.EvaluationCase{}, fmt.Errorf("probe oracle cannot use Argus review output contract %q as source truth", ref.Contract)
		}
	}
	receiptBytes, err := service.runs.ReadArtifact(probe.ReceiptRef)
	if err != nil {
		return evaluation.EvaluationCase{}, fmt.Errorf("read evaluation probe receipt: %w", err)
	}
	receipt, err := evaluation.DecodeEvaluationProbeReceipt(receiptBytes)
	if err != nil {
		return evaluation.EvaluationCase{}, fmt.Errorf("decode evaluation probe receipt: %w", err)
	}
	oracleSHA256, err := probe.Oracle.SHA256()
	if err != nil {
		return evaluation.EvaluationCase{}, fmt.Errorf("digest probe oracle: %w", err)
	}
	if receipt.ProbeID != probe.ProbeID || receipt.Kind != probe.Kind ||
		receipt.ReviewRunID != probe.ReviewRunID || receipt.OracleSHA256 != oracleSHA256 ||
		receipt.ObservedOutcome != probe.Oracle.ExpectedOutcome {
		return evaluation.EvaluationCase{}, fmt.Errorf("probe receipt does not bind the exact probe oracle and observation")
	}
	if receipt.ExecutorAuthority == probe.Oracle.Authority ||
		receipt.ExecutorAuthority == probe.SourceAuthority {
		return evaluation.EvaluationCase{}, fmt.Errorf("probe executor authority must be independent from source and oracle authorities")
	}
	if receipt.InputTargetRef != snapshot.TargetSnapshotRef {
		return evaluation.EvaluationCase{}, fmt.Errorf("probe receipt input does not equal the run TargetSnapshot")
	}
	if receipt.CompletedAt.After(probe.RecordedAt) || receipt.CompletedAt.Before(*run.CompletedAt) {
		return evaluation.EvaluationCase{}, fmt.Errorf("probe receipt chronology is invalid")
	}
	for _, ref := range receipt.EvidenceArtifacts {
		if err := service.runs.CheckArtifactEligibility(ref, runrepo.ArtifactUseCandidatePool); err != nil {
			return evaluation.EvaluationCase{}, fmt.Errorf("probe receipt evidence is ineligible: %w", err)
		}
	}
	specBytes, err := service.runs.ReadArtifact(snapshot.ReviewSpecRef)
	if err != nil {
		return evaluation.EvaluationCase{}, fmt.Errorf("read probe ReviewSpec: %w", err)
	}
	spec, err := contractsv1alpha1.DecodeReviewSpec(specBytes)
	if err != nil {
		return evaluation.EvaluationCase{}, fmt.Errorf("decode probe ReviewSpec: %w", err)
	}
	targetBytes, err := service.runs.ReadArtifact(snapshot.TargetSnapshotRef)
	if err != nil {
		return evaluation.EvaluationCase{}, fmt.Errorf("read probe TargetSnapshot: %w", err)
	}
	target, err := targetmodel.DecodeMaterializedTarget(targetBytes)
	if err != nil {
		return evaluation.EvaluationCase{}, fmt.Errorf("decode probe TargetSnapshot: %w", err)
	}
	if target.Snapshot.Repository.RepositoryID != spec.Repository.RepositoryID {
		return evaluation.EvaluationCase{}, fmt.Errorf("probe ReviewSpec and TargetSnapshot repository mismatch")
	}
	if len(probe.Oracle.Anchors) != 0 {
		if err := validateIncidentAnchors(target, probe.Oracle.Anchors); err != nil {
			return evaluation.EvaluationCase{}, fmt.Errorf("validate probe oracle anchors: %w", err)
		}
	}
	if receipt.BaselineTargetRef != nil {
		if err := service.runs.CheckArtifactEligibility(*receipt.BaselineTargetRef, runrepo.ArtifactUseCandidatePool); err != nil {
			return evaluation.EvaluationCase{}, fmt.Errorf("probe baseline artifact is ineligible: %w", err)
		}
		baselineBytes, err := service.runs.ReadArtifact(*receipt.BaselineTargetRef)
		if err != nil {
			return evaluation.EvaluationCase{}, fmt.Errorf("read mutation baseline TargetSnapshot: %w", err)
		}
		baseline, err := targetmodel.DecodeMaterializedTarget(baselineBytes)
		if err != nil {
			return evaluation.EvaluationCase{}, fmt.Errorf("decode mutation baseline TargetSnapshot: %w", err)
		}
		if baseline.Snapshot.Repository.RepositoryID != target.Snapshot.Repository.RepositoryID ||
			baseline.Snapshot.SHA256 == target.Snapshot.SHA256 {
			return evaluation.EvaluationCase{}, fmt.Errorf("mutation baseline must be a distinct target in the same repository")
		}
	}

	caseType := evaluation.CaseMutationDiagnostic
	provenanceKind := evaluation.SourceSynthetic
	switch probe.Kind {
	case evaluation.ProbeMutationDefect:
		provenanceKind = evaluation.SourceMutation
	case evaluation.ProbeSyntheticDefect:
	case evaluation.ProbeSyntheticClean:
		caseType = evaluation.CaseNegativeClean
	case evaluation.ProbeWorkflowInvariant:
		caseType = evaluation.CaseWorkflowInvariant
	}
	evidenceRefs := []string{snapshot.TargetSnapshotRef.URI, probe.ReceiptRef.URI}
	if receipt.BaselineTargetRef != nil {
		evidenceRefs = append(evidenceRefs, receipt.BaselineTargetRef.URI)
	}
	anchorRefs := make([]string, 0, len(probe.Oracle.EvidenceArtifacts))
	for _, ref := range probe.Oracle.EvidenceArtifacts {
		evidenceRefs = append(evidenceRefs, ref.URI)
		anchorRefs = append(anchorRefs, ref.URI)
	}
	for _, ref := range receipt.EvidenceArtifacts {
		evidenceRefs = append(evidenceRefs, ref.URI)
	}
	sort.Strings(evidenceRefs)
	evidenceRefs = slices.Compact(evidenceRefs)
	sort.Strings(anchorRefs)
	anchorRefs = slices.Compact(anchorRefs)
	result := evaluation.EvaluationCase{
		SchemaVersion: evaluation.EvaluationCaseSchemaVersion,
		CaseID:        request.CaseID, Type: caseType,
		Provenance: evaluation.SourceProvenance{
			Kind: provenanceKind, RepositoryID: spec.Repository.RepositoryID,
			SourceID: probe.ProbeID, SourceRunID: run.RunID,
			ObservedAt: receipt.CompletedAt, CollectedAt: request.CollectedAt,
			EvidenceRefs: evidenceRefs,
		},
		LicenseConsent: evaluation.LicenseConsent{
			LicenseID: request.LicenseID, Consent: request.Consent,
			AllowedUses:  []evaluation.UseScope{evaluation.UseCandidatePool},
			Restrictions: slices.Clone(request.Restrictions),
		},
		Classification: request.Classification, Owner: request.Owner,
		InputSnapshotRef: snapshot.TargetSnapshotRef.URI,
		Label: evaluation.Label{
			ExpectedOutcome: probe.Oracle.ExpectedOutcome,
			Category:        probe.Oracle.Category, Severity: probe.Oracle.Severity,
			Anchors: slices.Clone(probe.Oracle.Anchors), AnchorRefs: anchorRefs,
			SuppressionTargets: []evaluation.SuppressionTarget{},
		},
		LabelPolicyRevision: request.LabelPolicyRevision,
		ReviewState:         evaluation.ReviewPending, DatasetState: evaluation.DatasetCandidatePool,
		Split: evaluation.SplitUnassigned, CloneGroupID: "probe-" + oracleSHA256[:24],
		Eligibility: evaluation.Eligibility{}, CreatedAt: request.CollectedAt,
	}
	if err := result.Validate(); err != nil {
		return evaluation.EvaluationCase{}, fmt.Errorf("validate probe-derived candidate: %w", err)
	}
	return result, nil
}

func validateProbeRequestKind(source evaluation.CandidateDerivationSource, kind evaluation.ProbeKind) error {
	switch source {
	case evaluation.CandidateFromMutationProbe:
		if kind != evaluation.ProbeMutationDefect {
			return fmt.Errorf("mutation_probe source requires mutation_defect probe")
		}
	case evaluation.CandidateFromSyntheticProbe:
		if kind != evaluation.ProbeSyntheticDefect && kind != evaluation.ProbeSyntheticClean {
			return fmt.Errorf("synthetic_probe source requires synthetic_defect or synthetic_clean probe")
		}
	case evaluation.CandidateFromWorkflowProbe:
		if kind != evaluation.ProbeWorkflowInvariant {
			return fmt.Errorf("workflow_invariant_probe source requires workflow_invariant probe")
		}
	default:
		return fmt.Errorf("unsupported evaluation probe source %q", source)
	}
	return nil
}

func isReviewGeneratedOracleEvidence(contract string) bool {
	switch contract {
	case runmodel.ContractFindingSet, runmodel.ContractJSONReport,
		runmodel.ContractReviewHypothesisSet, runmodel.ContractGovernedCandidateSet,
		runmodel.ContractGovernedReviewReport, runmodel.ContractGovernedReviewMarkdown,
		runmodel.ContractAgentReviewRawCandidates, runmodel.ContractAgentReviewTaskEvidence,
		runmodel.ContractAgentExecutionReceipts:
		return true
	default:
		return false
	}
}
