package formalreview

import (
	"context"
	"fmt"

	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

type FindingGovernanceReplayResult struct {
	Run            runmodel.ReviewRun
	Ref            runmodel.ArtifactRef
	Hypotheses     contractsv1alpha1.ReviewHypothesisSet
	Report         contractsv1alpha1.GovernedReviewReport
	Calibration    contractsv1alpha1.FindingCalibrationLedger
	CalibrationRef runmodel.ArtifactRef
	Suppression    contractsv1alpha1.FindingSuppressionLedger
	SuppressionRef runmodel.ArtifactRef
	Recovered      bool
}

type FilterPolicyReplayResult = FindingGovernanceReplayResult

func FinalizeFilterPolicyReplay(
	ctx context.Context,
	repository *runrepo.Repository,
	initialized InitializedRun,
	policy reviewconfig.FindingGovernancePolicy,
) (FilterPolicyReplayResult, error) {
	return finalizeFilterPolicyReplay(ctx, repository, initialized, policy)
}

// FinalizeFindingGovernanceReplay performs no Agent or provider execution. It
// reuses the exact committed upstream review facts and derives only the two
// policy-dependent ledgers under the variant ConfigBundle.
func FinalizeFindingGovernanceReplay(
	ctx context.Context,
	repository *runrepo.Repository,
	initialized InitializedRun,
	policy reviewconfig.FindingGovernancePolicy,
) (FindingGovernanceReplayResult, error) {
	return finalizeFilterPolicyReplay(ctx, repository, initialized, policy)
}

func finalizeFilterPolicyReplay(
	ctx context.Context,
	repository *runrepo.Repository,
	initialized InitializedRun,
	policy reviewconfig.FindingGovernancePolicy,
) (FindingGovernanceReplayResult, error) {
	if ctx == nil {
		return FindingGovernanceReplayResult{}, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return FindingGovernanceReplayResult{}, err
	}
	if repository == nil || !initialized.ReplayVariable.IsFilterPolicy() ||
		initialized.ReplayFromStage != "finding_governance" {
		return FindingGovernanceReplayResult{}, fmt.Errorf("initialized finding governance replay is required")
	}
	if err := policy.Validate(); err != nil {
		return FindingGovernanceReplayResult{}, err
	}
	if existing, err := repository.LoadRun(initialized.RunID); err == nil {
		return recoverFindingGovernanceReplay(repository, initialized, policy, existing)
	}
	source, err := repository.LoadRun(initialized.SourceRunID)
	if err != nil {
		return FindingGovernanceReplayResult{}, fmt.Errorf("load finding governance replay source: %w", err)
	}
	if source.Status != runmodel.RunStatusSucceeded || source.HypothesisSetRef == nil ||
		source.CandidateSetRef == nil || source.VerificationLedgerRef == nil ||
		source.GovernedReportRef == nil || source.GovernedMarkdownRef == nil {
		return FindingGovernanceReplayResult{}, fmt.Errorf("finding governance replay source lacks complete formal facts")
	}
	var hypotheses contractsv1alpha1.ReviewHypothesisSet
	if err := repository.ReadJSONArtifact(*source.HypothesisSetRef, &hypotheses); err != nil {
		return FindingGovernanceReplayResult{}, err
	}
	var verification contractsv1alpha1.CandidateVerificationLedger
	if err := repository.ReadJSONArtifact(*source.VerificationLedgerRef, &verification); err != nil {
		return FindingGovernanceReplayResult{}, err
	}
	var report contractsv1alpha1.GovernedReviewReport
	if err := repository.ReadJSONArtifact(*source.GovernedReportRef, &report); err != nil {
		return FindingGovernanceReplayResult{}, err
	}
	if report.ReviewRunID != hypotheses.ReviewRunID ||
		verification.ReviewRunID != hypotheses.ReviewRunID {
		return FindingGovernanceReplayResult{}, fmt.Errorf("source formal facts have different evidence owners")
	}
	calibration, err := BuildFindingCalibrationLedgerWithPolicy(report, verification, &policy)
	if err != nil {
		return FindingGovernanceReplayResult{}, err
	}
	calibrationRef, err := repository.PutJSONArtifact(runmodel.ContractFindingCalibrationLedger, calibration)
	if err != nil {
		return FindingGovernanceReplayResult{}, err
	}
	suppression, err := BuildFindingSuppressionLedgerWithPolicy(report, calibration, &policy)
	if err != nil {
		return FindingGovernanceReplayResult{}, err
	}
	suppressionRef, err := repository.PutJSONArtifact(runmodel.ContractFindingSuppressionLedger, suppression)
	if err != nil {
		return FindingGovernanceReplayResult{}, err
	}
	at := initialized.Snapshot.CreatedAt.UTC()
	startedAt, completedAt := at, at
	run := runmodel.ReviewRun{
		SchemaVersion: runmodel.RunSchemaVersion, RunID: initialized.RunID,
		Kind: runmodel.RunKindReplay, SourceRunID: source.RunID,
		ReplayFromStage: initialized.ReplayFromStage, ReplayRootRunID: initialized.ReplayRootRunID,
		ReplayNamespace: initialized.ReplayNamespace, ReplayVariable: initialized.ReplayVariable,
		RepositoryPath: source.RepositoryPath, TargetMode: source.TargetMode,
		BaseRevision: source.BaseRevision, HeadRevision: source.HeadRevision,
		Status: runmodel.RunStatusSucceeded, ExecutionSnapshotID: initialized.Snapshot.ExecutionSnapshotID,
		TargetSnapshotRef: source.TargetSnapshotRef,
		StageAttempts:     []runmodel.StageAttempt{}, Bindings: []runmodel.PlatformExecutionBinding{},
		Evidence: []runmodel.RunEvidence{}, HypothesisSetRef: source.HypothesisSetRef,
		CandidateSetRef: source.CandidateSetRef, VerificationLedgerRef: source.VerificationLedgerRef,
		CalibrationLedgerRef: &calibrationRef, SuppressionLedgerRef: &suppressionRef,
		GovernedReportRef: source.GovernedReportRef, GovernedMarkdownRef: source.GovernedMarkdownRef,
		CreatedAt: at, StartedAt: &startedAt, CompletedAt: &completedAt,
	}
	ref, err := repository.FinalizeRun(runrepo.TerminalEventID(run.RunID), completedAt, run)
	if err != nil {
		return FindingGovernanceReplayResult{}, fmt.Errorf("commit finding governance replay: %w", err)
	}
	return FindingGovernanceReplayResult{
		Run: run, Ref: ref, Hypotheses: hypotheses, Report: report,
		Calibration: calibration, CalibrationRef: calibrationRef,
		Suppression: suppression, SuppressionRef: suppressionRef,
	}, nil
}

func recoverFindingGovernanceReplay(
	repository *runrepo.Repository,
	initialized InitializedRun,
	policy reviewconfig.FindingGovernancePolicy,
	run runmodel.ReviewRun,
) (FindingGovernanceReplayResult, error) {
	if run.ExecutionSnapshotID != initialized.Snapshot.ExecutionSnapshotID ||
		run.SourceRunID != initialized.SourceRunID ||
		run.ReplayVariable != initialized.ReplayVariable ||
		run.CalibrationLedgerRef == nil || run.SuppressionLedgerRef == nil ||
		run.HypothesisSetRef == nil || run.GovernedReportRef == nil {
		return FindingGovernanceReplayResult{}, fmt.Errorf("existing finding governance replay changed immutable closure")
	}
	var hypotheses contractsv1alpha1.ReviewHypothesisSet
	var report contractsv1alpha1.GovernedReviewReport
	var calibration contractsv1alpha1.FindingCalibrationLedger
	var suppression contractsv1alpha1.FindingSuppressionLedger
	if err := repository.ReadJSONArtifact(*run.HypothesisSetRef, &hypotheses); err != nil {
		return FindingGovernanceReplayResult{}, err
	}
	if err := repository.ReadJSONArtifact(*run.GovernedReportRef, &report); err != nil {
		return FindingGovernanceReplayResult{}, err
	}
	if err := repository.ReadJSONArtifact(*run.CalibrationLedgerRef, &calibration); err != nil {
		return FindingGovernanceReplayResult{}, err
	}
	if err := repository.ReadJSONArtifact(*run.SuppressionLedgerRef, &suppression); err != nil {
		return FindingGovernanceReplayResult{}, err
	}
	if suppression.Policy == nil || suppression.Policy.SHA256 != policy.SHA256 {
		return FindingGovernanceReplayResult{}, fmt.Errorf("existing finding governance replay changed policy")
	}
	ref, err := repository.CommittedRunRef(run.RunID)
	if err != nil {
		return FindingGovernanceReplayResult{}, err
	}
	return FindingGovernanceReplayResult{
		Run: run, Ref: ref, Hypotheses: hypotheses, Report: report,
		Calibration: calibration, CalibrationRef: *run.CalibrationLedgerRef,
		Suppression: suppression, SuppressionRef: *run.SuppressionLedgerRef,
		Recovered: true,
	}, nil
}
