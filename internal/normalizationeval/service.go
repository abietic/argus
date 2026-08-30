// Package normalizationeval owns the exact evidence-closure evaluation for
// normalization policies. Interfaces such as CLI, HTTP, and promotion must use
// this service instead of trusting a stored quality artifact directly.
package normalizationeval

import (
	"context"
	"fmt"
	"reflect"
	"slices"

	"argus.local/argus/internal/evaluation"
	"argus.local/argus/internal/pireviewmap"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/runrepo"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

type Service struct {
	evaluation *evaluation.Repository
	runs       *runrepo.Repository
}

func New(repository *evaluation.Repository, runs *runrepo.Repository) (*Service, error) {
	if repository == nil || runs == nil {
		return nil, fmt.Errorf("evaluation and run repositories are required")
	}
	return &Service{evaluation: repository, runs: runs}, nil
}

func (service *Service) ValidateOracleClosure(
	ctx context.Context,
	oracle evaluation.NormalizationOracle,
	access evaluation.Access,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := oracle.Validate(); err != nil {
		return err
	}
	snapshotData, err := service.runs.ReadArtifact(oracle.CorpusSnapshotRef)
	if err != nil {
		return fmt.Errorf("read oracle CorpusSnapshot: %w", err)
	}
	snapshot, err := evaluation.DecodeCorpusSnapshot(snapshotData)
	if err != nil {
		return err
	}
	builder, err := evaluation.NewCorpusSnapshotBuilder(service.evaluation, service.runs)
	if err != nil {
		return err
	}
	if err := builder.Authorize(snapshot, access); err != nil {
		return err
	}
	var snapshotCase *evaluation.CorpusSnapshotCase
	for index := range snapshot.Cases {
		if snapshot.Cases[index].CaseID == oracle.CaseID {
			snapshotCase = &snapshot.Cases[index]
			break
		}
	}
	if snapshotCase == nil || snapshotCase.Split != oracle.Split || snapshotCase.LabelRevision != oracle.LabelRevision {
		return fmt.Errorf("normalization oracle does not bind the CorpusSnapshot case revision")
	}
	run, err := service.runs.LoadRun(oracle.SourceReviewRunID)
	if err != nil {
		return fmt.Errorf("load normalization source ReviewRun: %w", err)
	}
	if run.Status != runmodel.RunStatusSucceeded || run.RawCandidateCollectionRef == nil ||
		*run.RawCandidateCollectionRef != oracle.RawCandidateCollectionRef {
		return fmt.Errorf("normalization oracle requires a succeeded ReviewRun with the exact raw collection")
	}
	runRef, err := service.runs.CommittedRunRef(run.RunID)
	if err != nil || runRef != oracle.SourceReviewRunRef {
		return fmt.Errorf("normalization oracle source ReviewRun ref changed")
	}
	root, err := service.replayRoot(run)
	if err != nil {
		return err
	}
	if root.TargetSnapshotRef != snapshotCase.TargetSnapshotRef {
		return fmt.Errorf("normalization source run does not descend from the CorpusSnapshot target")
	}
	rawData, err := service.runs.ReadArtifact(oracle.RawCandidateCollectionRef)
	if err != nil {
		return err
	}
	raw, err := contractsv1alpha1.DecodeAgentReviewRawCandidateCollection(rawData)
	if err != nil {
		return err
	}
	if raw.ReviewRunID != run.RunID || raw.TargetDigest != oracle.TargetDigest {
		return fmt.Errorf("normalization oracle raw collection identity changed")
	}
	eligible := make([]string, 0, len(raw.RawCandidates))
	for _, candidate := range raw.RawCandidates {
		if candidate.Action == contractsv1alpha1.HypothesisNormalizationRetained ||
			candidate.Action == contractsv1alpha1.HypothesisNormalizationMergedDuplicate {
			eligible = append(eligible, candidate.RawCandidateID)
		}
	}
	slices.Sort(eligible)
	if !slices.Equal(eligible, oracle.EligibleRawCandidateIDs) {
		return fmt.Errorf("normalization oracle eligible candidate set differs from committed raw facts")
	}
	for _, evidenceRef := range oracle.Adjudication.EvidenceRefs {
		if _, err := service.runs.ReadArtifact(evidenceRef); err != nil {
			return fmt.Errorf("read normalization oracle evidence %s: %w", evidenceRef.SHA256, err)
		}
	}
	if run.CompletedAt == nil || oracle.Adjudication.AdjudicatedAt.Before(*run.CompletedAt) {
		return fmt.Errorf("normalization oracle adjudication predates the committed candidate output")
	}
	return nil
}

func (service *Service) BuildQuality(
	ctx context.Context,
	request evaluation.NormalizationQualityRunRequest,
	access evaluation.Access,
) (evaluation.NormalizationQualityRun, error) {
	if err := request.Validate(); err != nil {
		return evaluation.NormalizationQualityRun{}, err
	}
	if _, err := service.evaluation.AuthorizeNormalizationOracleBindings(request.OracleBindings, access); err != nil {
		return evaluation.NormalizationQualityRun{}, err
	}
	oracles := make([]evaluation.NormalizationOracle, 0, len(request.OracleBindings))
	predictions := make([]evaluation.NormalizationPrediction, 0, len(request.OracleBindings))
	for _, binding := range request.OracleBindings {
		if err := ctx.Err(); err != nil {
			return evaluation.NormalizationQualityRun{}, err
		}
		oracleData, err := service.runs.ReadArtifact(binding.OracleRef)
		if err != nil {
			return evaluation.NormalizationQualityRun{}, fmt.Errorf("read normalization oracle %s: %w", binding.OracleRef.SHA256, err)
		}
		oracle, err := evaluation.DecodeNormalizationOracle(oracleData)
		if err != nil {
			return evaluation.NormalizationQualityRun{}, err
		}
		if oracle.OracleID != binding.OracleID || oracle.CaseID != binding.CaseID {
			return evaluation.NormalizationQualityRun{}, fmt.Errorf("normalization oracle artifact does not match registry binding")
		}
		if err := service.ValidateOracleClosure(ctx, oracle, access); err != nil {
			return evaluation.NormalizationQualityRun{}, err
		}
		rawData, err := service.runs.ReadArtifact(oracle.RawCandidateCollectionRef)
		if err != nil {
			return evaluation.NormalizationQualityRun{}, err
		}
		raw, err := contractsv1alpha1.DecodeAgentReviewRawCandidateCollection(rawData)
		if err != nil {
			return evaluation.NormalizationQualityRun{}, err
		}
		preview, err := pireviewmap.PreviewNormalizationForRevision(raw, request.PolicyRevision)
		if err != nil {
			return evaluation.NormalizationQualityRun{}, err
		}
		classes := make([]evaluation.NormalizationEquivalenceClass, 0, len(preview.Clusters))
		for _, cluster := range preview.Clusters {
			classes = append(classes, evaluation.NormalizationEquivalenceClass{
				ClassID: cluster.CanonicalRawCandidateID, RawCandidateIDs: slices.Clone(cluster.RawCandidateIDs),
			})
		}
		oracles = append(oracles, oracle)
		predictions = append(predictions, evaluation.NormalizationPrediction{
			CaseID: oracle.CaseID, OracleRef: binding.OracleRef, PolicyRevision: request.PolicyRevision,
			PredictedEquivalenceClasses: classes,
		})
	}
	return evaluation.EvaluateNormalizationQuality(
		request.QualityRunID, request.PolicyRevision, oracles, request.OracleBindings, predictions, request.CreatedAt,
	)
}

func (service *Service) VerifyQuality(
	ctx context.Context,
	ref runmodel.ArtifactRef,
	access evaluation.Access,
) (evaluation.NormalizationQualityRun, error) {
	if err := ref.Validate(); err != nil {
		return evaluation.NormalizationQualityRun{}, err
	}
	if ref.Contract != evaluation.NormalizationQualityRunContract {
		return evaluation.NormalizationQualityRun{}, fmt.Errorf("normalization quality ref has contract %q", ref.Contract)
	}
	data, err := service.runs.ReadArtifact(ref)
	if err != nil {
		return evaluation.NormalizationQualityRun{}, err
	}
	quality, err := evaluation.DecodeNormalizationQualityRun(data)
	if err != nil {
		return evaluation.NormalizationQualityRun{}, err
	}
	bindings := make([]evaluation.NormalizationOracleBinding, 0, len(quality.Cases))
	for _, result := range quality.Cases {
		bindings = append(bindings, result.OracleBinding)
	}
	slices.SortFunc(bindings, func(left, right evaluation.NormalizationOracleBinding) int {
		if left.OracleRef.SHA256 < right.OracleRef.SHA256 {
			return -1
		}
		if left.OracleRef.SHA256 > right.OracleRef.SHA256 {
			return 1
		}
		return 0
	})
	recomputed, err := service.BuildQuality(ctx, evaluation.NormalizationQualityRunRequest{
		SchemaVersion: evaluation.NormalizationQualityRunRequestSchemaVersion,
		QualityRunID:  quality.QualityRunID, PolicyRevision: quality.PolicyRevision,
		OracleBindings: bindings, CreatedAt: quality.CreatedAt,
	}, access)
	if err != nil {
		return evaluation.NormalizationQualityRun{}, err
	}
	if !reflect.DeepEqual(recomputed, quality) {
		return evaluation.NormalizationQualityRun{}, fmt.Errorf("normalization quality artifact differs from exact oracle/raw recomputation")
	}
	return quality, nil
}

func (service *Service) replayRoot(run runmodel.ReviewRun) (runmodel.ReviewRun, error) {
	seen := make(map[string]struct{})
	for run.Kind == runmodel.RunKindReplay {
		if run.SourceRunID == "" {
			return runmodel.ReviewRun{}, fmt.Errorf("replay ReviewRun has no source_run_id")
		}
		if _, duplicate := seen[run.RunID]; duplicate || len(seen) >= 64 {
			return runmodel.ReviewRun{}, fmt.Errorf("replay ReviewRun source chain is cyclic or too deep")
		}
		seen[run.RunID] = struct{}{}
		next, err := service.runs.LoadRun(run.SourceRunID)
		if err != nil {
			return runmodel.ReviewRun{}, fmt.Errorf("load replay source ReviewRun %q: %w", run.SourceRunID, err)
		}
		run = next
	}
	return run, nil
}
