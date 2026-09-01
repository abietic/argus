package analyticsadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/abietic/argus/internal/analytics"
	feedbackdomain "github.com/abietic/argus/internal/feedback"
	"github.com/abietic/argus/internal/publication"
	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/reviewcore"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/source/gitadapter"
	"github.com/abietic/argus/internal/targetmodel"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

type findingSetArtifact struct {
	SchemaVersion string                       `json:"schema_version"`
	TargetDigest  string                       `json:"target_digest"`
	Findings      []reviewcore.Finding         `json:"findings"`
	Decisions     []reviewcore.FindingDecision `json:"decisions"`
}

type frozenRun struct {
	run                runmodel.ReviewRun
	finalRef           runmodel.ArtifactRef
	execution          runmodel.ExecutionSnapshot
	spec               contractsv1alpha1.ReviewSpec
	config             reviewconfig.ConfigBundle
	target             targetmodel.MaterializedTarget
	findingSet         *findingSetArtifact
	detect             *reviewcore.StageResult
	detectRef          *runmodel.ArtifactRef
	hypothesisSet      *contractsv1alpha1.ReviewHypothesisSet
	candidateSet       *contractsv1alpha1.GovernedCandidateSet
	verificationLedger *contractsv1alpha1.CandidateVerificationLedger
	calibrationLedger  *contractsv1alpha1.FindingCalibrationLedger
	suppressionLedger  *contractsv1alpha1.FindingSuppressionLedger
	governedReport     *contractsv1alpha1.GovernedReviewReport
}

type buildState struct {
	runCoverageReasons []string
	factReasons        []string
}

// Rebuild scans authoritative repositories once, validates every source
// closure, and persists one immutable projection. Queries never call this path.
func (adapter *Adapter) Rebuild(
	ctx context.Context,
	request RebuildRequest,
) (ProjectionSnapshot, error) {
	if adapter == nil || adapter.runs == nil || adapter.store == nil {
		return ProjectionSnapshot{}, fmt.Errorf(
			"projection adapter is not configured for rebuild",
		)
	}
	request.GroupBy = canonicalGroupBy(request.GroupBy)
	if err := request.Validate(); err != nil {
		return ProjectionSnapshot{}, err
	}
	if err := contextErr(ctx); err != nil {
		return ProjectionSnapshot{}, err
	}

	facts, bindings, coverage, err := adapter.buildFacts(ctx, request)
	if err != nil {
		return ProjectionSnapshot{}, err
	}
	dashboard, err := analytics.ProjectDashboard(facts, request.GroupBy)
	if err != nil {
		return ProjectionSnapshot{}, fmt.Errorf("project dashboard: %w", err)
	}
	snapshot := ProjectionSnapshot{
		SchemaVersion:  ProjectionSnapshotSchemaVersion,
		SnapshotID:     request.SnapshotID,
		PolicyRevision: ProjectionPolicyRevision,
		Scope:          request.Scope,
		Window:         request.Window,
		GroupBy:        slices.Clone(request.GroupBy),
		BuiltAt:        request.BuiltAt,
		Coverage:       coverage,
		RunBindings:    bindings,
		Facts:          facts,
		Dashboard:      dashboard,
	}
	return adapter.persist(ctx, snapshot)
}

func (adapter *Adapter) buildFacts(
	ctx context.Context,
	request RebuildRequest,
) (analytics.FactSet, []RunSourceBinding, []SourceCoverage, error) {
	history, err := adapter.runs.History(0)
	if err != nil {
		return analytics.FactSet{}, nil, nil, fmt.Errorf("read run history: %w", err)
	}
	state := buildState{
		runCoverageReasons: []string{},
		factReasons:        []string{},
	}
	reviewRuns := make([]analytics.ReviewRunFact, 0)
	stages := make([]analytics.StageFact, 0)
	contextProviders := make([]analytics.ContextProviderFact, 0)
	findings := make([]analytics.FindingFunnelFact, 0)
	feedbackOutcomes := make([]analytics.FeedbackOutcomeFact, 0)
	bindings := make([]RunSourceBinding, 0)

	for _, entry := range history {
		if err := contextErr(ctx); err != nil {
			return analytics.FactSet{}, nil, nil, err
		}
		switch entry.Status {
		case runmodel.RunStatusSucceeded, runmodel.RunStatusFailed,
			runmodel.RunStatusCanceled:
		default:
			if request.WindowContains(entry.LastEventTime) {
				state.runCoverageReasons = appendReason(
					state.runCoverageReasons,
					"nonterminal_run_scope_unresolved",
				)
				state.factReasons = appendReason(
					state.factReasons,
					"nonterminal_runs_not_projected",
				)
			}
			continue
		}
		run, loadErr := adapter.runs.LoadRun(entry.RunID)
		if loadErr != nil {
			return analytics.FactSet{}, nil, nil, fmt.Errorf(
				"load committed run %q: %w",
				entry.RunID,
				loadErr,
			)
		}
		if run.CompletedAt == nil || !request.WindowContains(*run.CompletedAt) {
			continue
		}
		frozen, loadErr := adapter.loadFrozenRun(run)
		if loadErr != nil {
			return analytics.FactSet{}, nil, nil, fmt.Errorf(
				"load frozen run %q: %w",
				run.RunID,
				loadErr,
			)
		}
		if !frozen.matches(request.Scope) {
			continue
		}
		runFact, stageFacts, findingFacts, binding, buildErr :=
			buildRunFacts(frozen)
		if buildErr != nil {
			return analytics.FactSet{}, nil, nil, fmt.Errorf(
				"build run %q facts: %w",
				run.RunID,
				buildErr,
			)
		}
		providerFacts, buildErr := adapter.buildContextProviderFacts(frozen, runFact.Dimensions)
		if buildErr != nil {
			return analytics.FactSet{}, nil, nil, fmt.Errorf(
				"build run %q context provider facts: %w",
				run.RunID,
				buildErr,
			)
		}
		findingFacts, publicationReasons, buildErr :=
			adapter.projectPublicationFacts(request, frozen, findingFacts)
		if buildErr != nil {
			return analytics.FactSet{}, nil, nil, fmt.Errorf(
				"build run %q publication projection: %w",
				run.RunID,
				buildErr,
			)
		}
		if len(publicationReasons) > 0 {
			runFact.ResultCompleteness = lessComplete(
				runFact.ResultCompleteness,
				analytics.CompletenessPartial,
			)
			runFact.IncompleteReasons = appendReasons(
				runFact.IncompleteReasons,
				publicationReasons...,
			)
			state.factReasons = appendReasons(
				state.factReasons,
				publicationReasons...,
			)
		}
		published := publishedFindingCount(findingFacts)
		if runFact.Funnel != nil {
			runFact.Funnel.Published = published
		}
		binding.Published = published
		for _, stage := range stageFacts {
			if !request.WindowContains(stage.OccurredAt) {
				state.factReasons = appendReason(
					state.factReasons,
					"selected_run_stage_outside_window",
				)
				continue
			}
			stages = append(stages, stage)
		}
		runFeedback, feedbackReasons, buildErr := adapter.buildFeedbackFacts(
			ctx,
			request,
			frozen,
			findingFacts,
		)
		if buildErr != nil {
			return analytics.FactSet{}, nil, nil, fmt.Errorf(
				"build run %q feedback projection: %w",
				run.RunID,
				buildErr,
			)
		}
		state.factReasons = appendReasons(state.factReasons, feedbackReasons...)
		reviewRuns = append(reviewRuns, runFact)
		contextProviders = append(contextProviders, providerFacts...)
		findings = append(findings, findingFacts...)
		feedbackOutcomes = append(feedbackOutcomes, runFeedback...)
		bindings = append(bindings, binding)
	}

	experiments := []analytics.ExperimentFact{}
	repeatability := []analytics.RepeatabilityFact{}
	evaluationCoverage := SourceCoverage{
		Source:            CoverageEvaluation,
		Completeness:      analytics.CompletenessUnknown,
		IncompleteReasons: []string{"evaluation_source_not_configured"},
	}

	lineageFacts := []analytics.FindingLineageFact{}
	lineageCoverage := SourceCoverage{
		Source: CoverageFindingLineage, Completeness: analytics.CompletenessUnknown,
		IncompleteReasons: []string{"finding_lineage_source_not_configured"},
	}
	if adapter.lineages != nil {
		batch, batchErr := adapter.lineages.FindingLineageFacts(ctx, request.Scope, request.Window)
		if batchErr != nil {
			return analytics.FactSet{}, nil, nil, fmt.Errorf("load finding lineage facts: %w", batchErr)
		}
		if err := batch.Validate(); err != nil {
			return analytics.FactSet{}, nil, nil, fmt.Errorf("validate finding lineage facts: %w", err)
		}
		lineageFacts = slices.Clone(batch.Facts)
		lineageCoverage = SourceCoverage{Source: CoverageFindingLineage, Completeness: batch.Completeness, IncompleteReasons: slices.Clone(batch.IncompleteReasons)}
		if batch.Completeness != analytics.CompletenessComplete {
			state.factReasons = appendReasons(state.factReasons, prefixReasons("finding_lineage", batch.IncompleteReasons)...)
		}
	}
	if adapter.evaluations != nil {
		batch, batchErr := adapter.evaluations.EvaluationFacts(
			ctx,
			request.Scope,
			request.Window,
		)
		if batchErr != nil {
			return analytics.FactSet{}, nil, nil, fmt.Errorf(
				"load evaluation facts: %w",
				batchErr,
			)
		}
		if err := batch.Validate(); err != nil {
			return analytics.FactSet{}, nil, nil, fmt.Errorf(
				"validate evaluation facts: %w",
				err,
			)
		}
		experiments = slices.Clone(batch.Facts)
		repeatability = slices.Clone(batch.RepeatabilityFacts)
		evaluationCoverage = SourceCoverage{
			Source:            CoverageEvaluation,
			Completeness:      batch.Completeness,
			IncompleteReasons: slices.Clone(batch.IncompleteReasons),
		}
		if batch.Completeness != analytics.CompletenessComplete {
			state.factReasons = appendReasons(
				state.factReasons,
				prefixReasons("evaluation", batch.IncompleteReasons)...,
			)
		}
	}

	sort.Slice(reviewRuns, func(left, right int) bool {
		return reviewRuns[left].RunID < reviewRuns[right].RunID
	})
	sort.Slice(stages, func(left, right int) bool {
		if stages[left].RunID != stages[right].RunID {
			return stages[left].RunID < stages[right].RunID
		}
		if stages[left].StageID != stages[right].StageID {
			return stages[left].StageID < stages[right].StageID
		}
		if stages[left].Attempt != stages[right].Attempt {
			return stages[left].Attempt < stages[right].Attempt
		}
		return stages[left].FactID < stages[right].FactID
	})
	sort.Slice(contextProviders, func(left, right int) bool {
		if contextProviders[left].RunID != contextProviders[right].RunID {
			return contextProviders[left].RunID < contextProviders[right].RunID
		}
		return contextProviders[left].ReceiptID < contextProviders[right].ReceiptID
	})
	sort.Slice(findings, func(left, right int) bool {
		if findings[left].RunID != findings[right].RunID {
			return findings[left].RunID < findings[right].RunID
		}
		return findings[left].CandidateID < findings[right].CandidateID
	})
	sort.Slice(feedbackOutcomes, func(left, right int) bool {
		if feedbackOutcomes[left].RunID != feedbackOutcomes[right].RunID {
			return feedbackOutcomes[left].RunID < feedbackOutcomes[right].RunID
		}
		return feedbackOutcomes[left].FindingID < feedbackOutcomes[right].FindingID
	})
	sort.Slice(experiments, func(left, right int) bool {
		return experiments[left].FactID < experiments[right].FactID
	})
	sort.Slice(repeatability, func(left, right int) bool {
		return repeatability[left].FactID < repeatability[right].FactID
	})
	sort.Slice(lineageFacts, func(left, right int) bool { return lineageFacts[left].FactID < lineageFacts[right].FactID })
	sort.Slice(bindings, func(left, right int) bool {
		return bindings[left].RunID < bindings[right].RunID
	})

	completeness := analytics.CompletenessComplete
	incompleteReasons := []string{}
	if len(state.factReasons) > 0 {
		completeness = analytics.CompletenessPartial
		incompleteReasons = slices.Clone(state.factReasons)
	}
	facts := analytics.FactSet{
		SchemaVersion:     analytics.FactSetSchemaVersion,
		Window:            request.Window,
		Completeness:      completeness,
		IncompleteReasons: incompleteReasons,
		ReviewRuns:        reviewRuns,
		Stages:            stages,
		ContextProviders:  contextProviders,
		Findings:          findings,
		FeedbackOutcomes:  feedbackOutcomes,
		FindingLineages:   lineageFacts,
		Experiments:       experiments,
		Repeatability:     repeatability,
		ValueObservations: []analytics.ValueObservation{},
	}
	if err := facts.Validate(); err != nil {
		return analytics.FactSet{}, nil, nil, fmt.Errorf(
			"validate rebuilt fact set: %w",
			err,
		)
	}

	runCoverage := completeCoverage(CoverageRunLedger)
	if len(state.runCoverageReasons) > 0 {
		runCoverage = SourceCoverage{
			Source:            CoverageRunLedger,
			Completeness:      analytics.CompletenessPartial,
			IncompleteReasons: slices.Clone(state.runCoverageReasons),
		}
	}
	feedbackCoverage := completeCoverage(CoverageFeedbackOutcome)
	if adapter.feedback == nil {
		feedbackCoverage = SourceCoverage{
			Source:            CoverageFeedbackOutcome,
			Completeness:      analytics.CompletenessUnknown,
			IncompleteReasons: []string{"feedback_repository_not_configured"},
		}
	}
	publicationCoverage := completeCoverage(CoveragePublication)
	if adapter.publication == nil {
		publicationCoverage = SourceCoverage{
			Source:            CoveragePublication,
			Completeness:      analytics.CompletenessUnknown,
			IncompleteReasons: []string{"publication_repository_not_configured"},
		}
	}
	coverage := []SourceCoverage{
		{
			Source:            CoverageCost,
			Completeness:      analytics.CompletenessUnknown,
			IncompleteReasons: []string{"cost_ledger_not_configured"},
		},
		evaluationCoverage,
		feedbackCoverage,
		completeCoverage(CoverageFindingDecision),
		lineageCoverage,
		publicationCoverage,
		runCoverage,
		{
			Source:            CoverageTrace,
			Completeness:      analytics.CompletenessUnknown,
			IncompleteReasons: []string{"authoritative_trace_not_available"},
		},
	}
	return facts, bindings, coverage, nil
}

func (adapter *Adapter) buildContextProviderFacts(
	frozen frozenRun,
	dimensions analytics.Dimensions,
) ([]analytics.ContextProviderFact, error) {
	refs := frozen.execution.ContextProviderReceiptRefs
	facts := make([]analytics.ContextProviderFact, 0, len(refs))
	mode := analytics.ContextProviderExecuted
	if frozen.run.Kind == runmodel.RunKindReplay {
		mode = analytics.ContextProviderReused
	}
	for index, ref := range refs {
		data, err := adapter.runs.ReadArtifact(ref)
		if err != nil {
			return nil, fmt.Errorf("read receipt %d: %w", index, err)
		}
		receipt, err := contractsv1alpha1.DecodeContextProviderExecutionReceipt(data)
		if err != nil {
			return nil, fmt.Errorf("decode receipt %d: %w", index, err)
		}
		if receipt.TimeoutMS <= 0 || uint64(receipt.TimeoutMS) > math.MaxUint64/1000 ||
			receipt.DurationMS < 0 || uint64(receipt.DurationMS) > math.MaxUint64/1000 {
			return nil, fmt.Errorf("receipt %q duration exceeds analytics range", receipt.ReceiptID)
		}
		status := analytics.ContextProviderSucceeded
		if receipt.Status == contractsv1alpha1.ContextProviderReceiptGap {
			status = analytics.ContextProviderGap
		}
		facts = append(facts, analytics.ContextProviderFact{
			SchemaVersion: analytics.ContextProviderFactSchemaVersion,
			FactID: stableFactID(
				"context-provider-fact",
				frozen.run.RunID,
				receipt.ReceiptID,
			),
			RunID: frozen.run.RunID, ReceiptID: receipt.ReceiptID,
			ReceiptSHA256: receipt.SHA256, ReceiptArtifactSHA256: ref.SHA256,
			ProviderID: receipt.ProviderID, ProviderRevision: receipt.ProviderRevision,
			Kind: receipt.Kind, AdapterID: receipt.Adapter.ID,
			AdapterRevision: receipt.Adapter.Revision, AdapterSHA256: receipt.Adapter.SHA256,
			RequestSHA256: receipt.RequestSHA256, BindingMode: mode, Status: status,
			ReasonCode: receipt.ReasonCode, ContextID: receipt.ContextID,
			ContextDigest: receipt.ContextDigest, ContextContract: receipt.ContextContract,
			TargetPathCount: uint64(len(receipt.TargetPaths)),
			TimeoutMicros:   uint64(receipt.TimeoutMS) * 1000,
			DurationMicros:  uint64(receipt.DurationMS) * 1000,
			Authority:       receipt.Authority, Dimensions: dimensions,
			OccurredAt: frozen.run.CompletedAt.UTC(),
		})
	}
	return facts, nil
}

func (adapter *Adapter) loadFrozenRun(run runmodel.ReviewRun) (frozenRun, error) {
	finalRef, err := adapter.runs.CommittedRunRef(run.RunID)
	if err != nil {
		return frozenRun{}, err
	}
	execution, err := adapter.runs.LoadExecutionSnapshot(run.ExecutionSnapshotID)
	if err != nil {
		return frozenRun{}, err
	}
	specData, err := adapter.runs.ReadArtifact(execution.ReviewSpecRef)
	if err != nil {
		return frozenRun{}, fmt.Errorf("read ReviewSpec: %w", err)
	}
	spec, err := contractsv1alpha1.DecodeReviewSpec(specData)
	if err != nil {
		return frozenRun{}, err
	}
	configData, err := adapter.runs.ReadArtifact(execution.ConfigBundleRef)
	if err != nil {
		return frozenRun{}, fmt.Errorf("read ConfigBundle: %w", err)
	}
	config, err := reviewconfig.DecodeBundle(configData)
	if err != nil {
		return frozenRun{}, err
	}
	var target targetmodel.MaterializedTarget
	if err := adapter.runs.ReadJSONArtifact(execution.TargetSnapshotRef, &target); err != nil {
		return frozenRun{}, fmt.Errorf("read materialized target: %w", err)
	}
	if err := target.Validate(); err != nil {
		return frozenRun{}, fmt.Errorf("validate materialized target: %w", err)
	}
	result := frozenRun{
		run:       run,
		finalRef:  finalRef,
		execution: execution,
		spec:      spec,
		config:    config,
		target:    target,
	}
	if run.FindingSetRef == nil {
		if run.CandidateSetRef == nil && run.VerificationLedgerRef == nil &&
			run.CalibrationLedgerRef == nil && run.SuppressionLedgerRef == nil && run.GovernedReportRef == nil &&
			run.HypothesisSetRef == nil {
			return result, nil
		}
		if run.CandidateSetRef == nil || run.VerificationLedgerRef == nil ||
			run.CalibrationLedgerRef == nil || run.SuppressionLedgerRef == nil || run.GovernedReportRef == nil ||
			run.HypothesisSetRef == nil {
			return frozenRun{}, fmt.Errorf("formal run has incomplete governed review bindings")
		}
		hypothesisData, err := adapter.runs.ReadArtifact(*run.HypothesisSetRef)
		if err != nil {
			return frozenRun{}, fmt.Errorf("read formal hypothesis set: %w", err)
		}
		hypotheses, err := contractsv1alpha1.DecodeReviewHypothesisSet(hypothesisData)
		if err != nil {
			return frozenRun{}, fmt.Errorf("decode formal hypothesis set: %w", err)
		}
		candidateData, err := adapter.runs.ReadArtifact(*run.CandidateSetRef)
		if err != nil {
			return frozenRun{}, fmt.Errorf("read governed candidate set: %w", err)
		}
		candidates, err := contractsv1alpha1.DecodeGovernedCandidateSet(candidateData)
		if err != nil {
			return frozenRun{}, fmt.Errorf("decode governed candidate set: %w", err)
		}
		if err := candidates.ValidateAgainstHypothesisSet(hypotheses); err != nil {
			return frozenRun{}, fmt.Errorf("validate governed candidate lineage: %w", err)
		}
		verificationData, err := adapter.runs.ReadArtifact(*run.VerificationLedgerRef)
		if err != nil {
			return frozenRun{}, fmt.Errorf("read candidate verification ledger: %w", err)
		}
		verification, err := contractsv1alpha1.DecodeCandidateVerificationLedger(verificationData)
		if err != nil {
			return frozenRun{}, fmt.Errorf("decode candidate verification ledger: %w", err)
		}
		if err := verification.ValidateAgainstHypothesisSet(hypotheses); err != nil {
			return frozenRun{}, fmt.Errorf("validate candidate verification lineage: %w", err)
		}
		if err := candidates.ValidateAgainstVerificationLedger(verification); err != nil {
			return frozenRun{}, fmt.Errorf("validate candidate verification projection: %w", err)
		}
		reportData, err := adapter.runs.ReadArtifact(*run.GovernedReportRef)
		if err != nil {
			return frozenRun{}, fmt.Errorf("read governed review report: %w", err)
		}
		report, err := contractsv1alpha1.DecodeGovernedReviewReport(reportData)
		if err != nil {
			return frozenRun{}, fmt.Errorf("decode governed review report: %w", err)
		}
		if err := report.ValidateAgainstHypothesisSet(hypotheses); err != nil {
			return frozenRun{}, fmt.Errorf("validate governed report lineage: %w", err)
		}
		if err := report.ValidateAgainstVerificationLedger(verification); err != nil {
			return frozenRun{}, fmt.Errorf("validate governed report verification projection: %w", err)
		}
		calibrationData, err := adapter.runs.ReadArtifact(*run.CalibrationLedgerRef)
		if err != nil {
			return frozenRun{}, fmt.Errorf("read finding calibration ledger: %w", err)
		}
		calibration, err := contractsv1alpha1.DecodeFindingCalibrationLedger(calibrationData)
		if err != nil {
			return frozenRun{}, fmt.Errorf("decode finding calibration ledger: %w", err)
		}
		if err := calibration.ValidateAgainst(report, verification); err != nil {
			return frozenRun{}, fmt.Errorf("validate finding calibration lineage: %w", err)
		}
		suppressionData, err := adapter.runs.ReadArtifact(*run.SuppressionLedgerRef)
		if err != nil {
			return frozenRun{}, fmt.Errorf("read finding suppression ledger: %w", err)
		}
		suppression, err := contractsv1alpha1.DecodeFindingSuppressionLedger(suppressionData)
		if err != nil {
			return frozenRun{}, fmt.Errorf("decode finding suppression ledger: %w", err)
		}
		if err := suppression.ValidateAgainst(report, calibration); err != nil {
			return frozenRun{}, fmt.Errorf("validate finding suppression lineage: %w", err)
		}
		if !reflect.DeepEqual(report.Candidates, candidates.Candidates) ||
			report.Summary.Candidates != candidates.Summary.Candidates ||
			report.Summary.Confirmed != candidates.Summary.Confirmed ||
			report.Summary.Rejected != candidates.Summary.Rejected ||
			report.Summary.Inconclusive != candidates.Summary.Inconclusive {
			return frozenRun{}, fmt.Errorf("governed report changed the candidate ledger")
		}
		result.hypothesisSet = &hypotheses
		result.candidateSet = &candidates
		result.verificationLedger = &verification
		result.calibrationLedger = &calibration
		result.suppressionLedger = &suppression
		result.governedReport = &report
		return result, nil
	}
	var findingSet findingSetArtifact
	if err := adapter.runs.ReadJSONArtifact(*run.FindingSetRef, &findingSet); err != nil {
		return frozenRun{}, fmt.Errorf("read finding set: %w", err)
	}
	if err := validateFindingSet(findingSet); err != nil {
		return frozenRun{}, err
	}
	detect, detectRef, err := loadDetectStage(adapter, run)
	if err != nil {
		return frozenRun{}, err
	}
	result.findingSet = &findingSet
	result.detect = &detect
	result.detectRef = &detectRef
	return result, nil
}

func loadDetectStage(
	adapter *Adapter,
	run runmodel.ReviewRun,
) (reviewcore.StageResult, runmodel.ArtifactRef, error) {
	var selected *runmodel.StageAttempt
	for index := range run.StageAttempts {
		attempt := run.StageAttempts[index]
		if attempt.StageID != string(reviewcore.StageDetect) ||
			attempt.Status != runmodel.StageStatusSucceeded {
			continue
		}
		if selected == nil || attempt.Generation > selected.Generation ||
			attempt.Generation == selected.Generation &&
				attempt.Attempt > selected.Attempt {
			copy := attempt
			selected = &copy
		}
	}
	if selected == nil || selected.OutputRef == nil {
		return reviewcore.StageResult{}, runmodel.ArtifactRef{},
			fmt.Errorf("finding set has no successful detect stage")
	}
	data, err := adapter.runs.ReadArtifact(*selected.OutputRef)
	if err != nil {
		return reviewcore.StageResult{}, runmodel.ArtifactRef{},
			fmt.Errorf("read detect stage: %w", err)
	}
	result, err := reviewcore.DecodeStageResult(data)
	if err != nil {
		return reviewcore.StageResult{}, runmodel.ArtifactRef{}, err
	}
	if result.Stage != reviewcore.StageDetect {
		return reviewcore.StageResult{}, runmodel.ArtifactRef{},
			fmt.Errorf("selected detect artifact is %q", result.Stage)
	}
	return result, *selected.OutputRef, nil
}

func (run frozenRun) matches(scope Scope) bool {
	return run.spec.TenantID == scope.TenantID &&
		run.config.Context.TenantID == scope.TenantID &&
		run.config.Context.OrganizationID == scope.OrganizationID &&
		run.spec.Repository.RepositoryID == scope.RepositoryID &&
		run.config.Context.RepositoryID == scope.RepositoryID
}

func buildRunFacts(
	frozen frozenRun,
) (
	analytics.ReviewRunFact,
	[]analytics.StageFact,
	[]analytics.FindingFunnelFact,
	RunSourceBinding,
	error,
) {
	run := frozen.run
	if run.StartedAt == nil || run.CompletedAt == nil {
		return analytics.ReviewRunFact{}, nil, nil, RunSourceBinding{},
			fmt.Errorf("committed terminal run lacks timestamps")
	}
	dimensions := analytics.Dimensions{
		TenantID:         frozen.spec.TenantID,
		OrganizationID:   frozen.config.Context.OrganizationID,
		RepositoryID:     frozen.spec.Repository.RepositoryID,
		WorkflowRevision: frozen.execution.Workflow.Revision,
		ConfigRevision:   frozen.execution.Config.Revision,
	}
	resultCompleteness, resultReasons := runCompleteness(frozen)
	stageFacts, err := buildStageFacts(run, dimensions)
	if err != nil {
		return analytics.ReviewRunFact{}, nil, nil, RunSourceBinding{}, err
	}
	findingFacts := []analytics.FindingFunnelFact{}
	summary := (*analytics.FunnelSummary)(nil)
	binding := RunSourceBinding{
		RunID:                run.RunID,
		FinalRunSHA256:       frozen.finalRef.SHA256,
		ExecutionSnapshotID:  frozen.execution.ExecutionSnapshotID,
		ReviewSpecSHA256:     frozen.execution.ReviewSpecSHA256,
		TargetSnapshotSHA256: frozen.execution.TargetSnapshotRef.SHA256,
		ConfigID:             frozen.execution.Config.ID,
		ConfigRevision:       frozen.execution.Config.Revision,
		ConfigSHA256:         frozen.execution.ConfigBundleRef.SHA256,
		WorkflowID:           frozen.execution.Workflow.ID,
		WorkflowRevision:     frozen.execution.Workflow.Revision,
		WorkflowSHA256:       frozen.execution.Workflow.SHA256,
	}
	if frozen.findingSet != nil {
		var buildErr error
		findingFacts, summary, binding, buildErr = buildFindingFacts(
			frozen,
			dimensions,
			resultCompleteness,
			resultReasons,
			binding,
		)
		if buildErr != nil {
			return analytics.ReviewRunFact{}, nil, nil, RunSourceBinding{}, buildErr
		}
	} else if frozen.governedReport != nil {
		var buildErr error
		findingFacts, summary, binding, buildErr = buildGovernedFindingFacts(
			frozen,
			dimensions,
			resultCompleteness,
			resultReasons,
			binding,
		)
		if buildErr != nil {
			return analytics.ReviewRunFact{}, nil, nil, RunSourceBinding{}, buildErr
		}
	}
	status, err := mapRunStatus(run.Status)
	if err != nil {
		return analytics.ReviewRunFact{}, nil, nil, RunSourceBinding{}, err
	}
	runFact := analytics.ReviewRunFact{
		SchemaVersion:      analytics.ReviewRunFactSchemaVersion,
		FactID:             stableFactID("run-fact", run.RunID),
		RunID:              run.RunID,
		Dimensions:         dimensions,
		Status:             status,
		ResultCompleteness: resultCompleteness,
		IncompleteReasons:  slices.Clone(resultReasons),
		Funnel:             summary,
		StartedAt:          run.StartedAt.UTC(),
		FinishedAt:         timePointer(run.CompletedAt.UTC()),
		OccurredAt:         run.CompletedAt.UTC(),
	}
	return runFact, stageFacts, findingFacts, binding, nil
}

func buildStageFacts(
	run runmodel.ReviewRun,
	dimensions analytics.Dimensions,
) ([]analytics.StageFact, error) {
	evidenceByBinding := make(map[string]runmodel.RunEvidence, len(run.Evidence))
	for _, evidence := range run.Evidence {
		evidenceByBinding[evidence.BindingID] = evidence
	}
	facts := make([]analytics.StageFact, 0, len(run.StageAttempts))
	for _, attempt := range run.StageAttempts {
		if attempt.Attempt < 1 || uint64(attempt.Attempt) > math.MaxUint32 {
			return nil, fmt.Errorf("stage attempt is outside analytics uint32 range")
		}
		if attempt.FinishedAt == nil || attempt.FinishedAt.Before(attempt.StartedAt) {
			return nil, fmt.Errorf("stage %q has invalid persisted timestamps", attempt.StageID)
		}
		expectedMS := attempt.FinishedAt.Sub(attempt.StartedAt).Milliseconds()
		if expectedMS < 0 {
			expectedMS = 0
		}
		if attempt.DurationMS != expectedMS || attempt.DurationMS < 0 ||
			uint64(attempt.DurationMS) > math.MaxUint64/1000 {
			return nil, fmt.Errorf("stage %q persisted duration is inconsistent", attempt.StageID)
		}
		status, err := mapStageStatus(attempt.Status)
		if err != nil {
			return nil, err
		}
		completeness := analytics.CompletenessPartial
		reasons := []string{reasonCode("stage", string(attempt.Status))}
		if attempt.Status == runmodel.StageStatusSucceeded {
			evidence, exists := evidenceByBinding[attempt.BindingID]
			if !exists {
				return nil, fmt.Errorf("succeeded stage %q lacks evidence", attempt.StageID)
			}
			if evidence.Completeness == runmodel.CompletenessComplete {
				completeness = analytics.CompletenessComplete
				reasons = []string{}
			} else {
				reasons = prefixReasons(
					"evidence",
					evidence.CompletenessNotes,
				)
			}
		} else if attempt.ErrorCode != "" {
			reasons = []string{reasonCode("stage", attempt.ErrorCode)}
		}
		facts = append(facts, analytics.StageFact{
			SchemaVersion:      analytics.StageFactSchemaVersion,
			FactID:             stableFactID("stage-fact", run.RunID, attempt.BindingID),
			RunID:              run.RunID,
			StageID:            attempt.StageID,
			StageRevision:      stageRevision(run, attempt),
			Attempt:            uint32(attempt.Attempt),
			Dimensions:         dimensions,
			Status:             status,
			ResultCompleteness: completeness,
			IncompleteReasons:  reasons,
			DurationMicros:     uint64(attempt.DurationMS) * 1000,
			OccurredAt:         attempt.FinishedAt.UTC(),
		})
	}
	return facts, nil
}

func stageRevision(run runmodel.ReviewRun, attempt runmodel.StageAttempt) string {
	if attempt.OutputRef == nil {
		return "unknown"
	}
	for _, evidence := range run.Evidence {
		if evidence.BindingID == attempt.BindingID {
			// The immutable artifact digest is the executable output revision
			// when a typed stage revision cannot be loaded without another read.
			return "artifact-" + evidence.ArtifactRef.SHA256[:16]
		}
	}
	return "unknown"
}

func buildFindingFacts(
	frozen frozenRun,
	dimensions analytics.Dimensions,
	completeness analytics.Completeness,
	reasons []string,
	binding RunSourceBinding,
) (
	[]analytics.FindingFunnelFact,
	*analytics.FunnelSummary,
	RunSourceBinding,
	error,
) {
	set := *frozen.findingSet
	detected := frozen.detect.Output.CandidateFindings
	candidates := make(map[string]reviewcore.CandidateFinding, len(detected))
	for _, candidate := range detected {
		if _, duplicate := candidates[candidate.ID]; duplicate {
			return nil, nil, binding, fmt.Errorf("detect stage contains duplicate candidate")
		}
		candidates[candidate.ID] = candidate
	}
	type findingProjection struct {
		finding                reviewcore.Finding
		decision               reviewcore.FindingDecision
		verification           analytics.VerificationState
		publicationEligibility analytics.PublicationEligibility
	}
	byCandidate := make(map[string]findingProjection)
	latestDecision := make(map[string]reviewcore.FindingDecision)
	for _, decision := range set.Decisions {
		current, exists := latestDecision[decision.FindingID]
		if !exists || decision.Sequence > current.Sequence {
			latestDecision[decision.FindingID] = decision
		}
	}
	verified := uint64(0)
	for _, finding := range set.Findings {
		decision, exists := latestDecision[finding.ID]
		if !exists {
			return nil, nil, binding, fmt.Errorf(
				"finding %q lacks a decision",
				finding.ID,
			)
		}
		verification, err := mapVerification(finding.Verification.Status)
		if err != nil {
			return nil, nil, binding, err
		}
		publicationEligibility, err := mapDecision(decision.Action)
		if err != nil {
			return nil, nil, binding, err
		}
		if verification == analytics.VerificationVerified {
			verified++
		}
		projection := findingProjection{
			finding:                finding,
			decision:               decision,
			verification:           verification,
			publicationEligibility: publicationEligibility,
		}
		for _, lineage := range finding.Lineage {
			if _, exists := candidates[lineage.CandidateID]; !exists {
				return nil, nil, binding, fmt.Errorf(
					"finding %q references candidate absent from detect stage",
					finding.ID,
				)
			}
			if _, duplicate := byCandidate[lineage.CandidateID]; duplicate {
				return nil, nil, binding, fmt.Errorf(
					"candidate %q maps to multiple findings",
					lineage.CandidateID,
				)
			}
			byCandidate[lineage.CandidateID] = projection
		}
	}
	facts := make([]analytics.FindingFunnelFact, 0, len(detected))
	for _, candidate := range detected {
		factDimensions := dimensions
		factDimensions.Language = gitadapter.DetectLanguage(candidate.Path)
		factDimensions.RuleID = candidate.RuleID
		factDimensions.Path = candidate.Path
		fact := analytics.FindingFunnelFact{
			SchemaVersion:          analytics.FindingFunnelFactSchemaVersion,
			FactID:                 stableFactID("finding-funnel", frozen.run.RunID, candidate.ID),
			RunID:                  frozen.run.RunID,
			CandidateID:            candidate.ID,
			Normalized:             false,
			Verification:           analytics.VerificationNotReached,
			PublicationEligibility: analytics.EligibilityNotReached,
			Publication:            analytics.PublicationNotReached,
			ResultCompleteness:     completeness,
			IncompleteReasons:      slices.Clone(reasons),
			Dimensions:             factDimensions,
			OccurredAt:             frozen.run.CompletedAt.UTC(),
		}
		if projection, exists := byCandidate[candidate.ID]; exists {
			fact.FindingID = projection.finding.ID
			fact.Normalized = true
			fact.Verification = projection.verification
			fact.PublicationEligibility = projection.publicationEligibility
			fact.Publication = analytics.PublicationUnknown
		}
		facts = append(facts, fact)
	}
	summary := &analytics.FunnelSummary{
		Candidates: uint64(len(detected)),
		Normalized: uint64(len(set.Findings)),
		Verified:   verified,
		Published:  0,
	}
	if err := summary.Validate(); err != nil {
		return nil, nil, binding, err
	}
	binding.FindingSetSHA256 = frozen.run.FindingSetRef.SHA256
	if frozen.detectRef != nil {
		binding.DetectStageSHA256 = frozen.detectRef.SHA256
	}
	if binding.DetectStageSHA256 == "" {
		return nil, nil, binding, fmt.Errorf("detect stage digest does not reconcile")
	}
	binding.Candidates = summary.Candidates
	binding.Normalized = summary.Normalized
	binding.Verified = summary.Verified
	binding.Decisions = uint64(len(set.Decisions))
	binding.Published = summary.Published
	return facts, summary, binding, nil
}

// buildGovernedFindingFacts adapts the formal Pi review ledger into the same
// provider-neutral funnel used by the dashboard. Rejected and inconclusive
// candidates remain candidate facts; only independently confirmed candidates
// have a governed Finding. The initial formal decision is human_queue, so it
// cannot be confused with publication authority.
func buildGovernedFindingFacts(
	frozen frozenRun,
	dimensions analytics.Dimensions,
	completeness analytics.Completeness,
	reasons []string,
	binding RunSourceBinding,
) (
	[]analytics.FindingFunnelFact,
	*analytics.FunnelSummary,
	RunSourceBinding,
	error,
) {
	if frozen.hypothesisSet == nil || frozen.candidateSet == nil ||
		frozen.verificationLedger == nil ||
		frozen.calibrationLedger == nil || frozen.suppressionLedger == nil ||
		frozen.governedReport == nil || frozen.run.HypothesisSetRef == nil ||
		frozen.run.CandidateSetRef == nil || frozen.run.VerificationLedgerRef == nil ||
		frozen.run.CalibrationLedgerRef == nil || frozen.run.SuppressionLedgerRef == nil ||
		frozen.run.GovernedReportRef == nil {
		return nil, nil, binding, fmt.Errorf("formal governed review closure is incomplete")
	}
	report := *frozen.governedReport
	findings := make(map[string]contractsv1alpha1.GovernedReviewFinding, len(report.Findings))
	for _, finding := range report.Findings {
		if _, duplicate := findings[finding.CandidateID]; duplicate {
			return nil, nil, binding, fmt.Errorf(
				"formal candidate %q maps to multiple findings", finding.CandidateID,
			)
		}
		findings[finding.CandidateID] = finding
	}
	decisions := make(map[string]contractsv1alpha1.GovernedFindingDecision, len(report.Decisions))
	for _, decision := range report.Decisions {
		if _, duplicate := decisions[decision.FindingID]; duplicate {
			return nil, nil, binding, fmt.Errorf(
				"formal finding %q has multiple initial decisions", decision.FindingID,
			)
		}
		decisions[decision.FindingID] = decision
	}
	suppressions := make(map[string]contractsv1alpha1.FindingSuppressionFact, len(frozen.suppressionLedger.Facts))
	for _, fact := range frozen.suppressionLedger.Facts {
		suppressions[fact.FindingID] = fact
	}

	facts := make([]analytics.FindingFunnelFact, 0, len(report.Candidates))
	for _, candidate := range report.Candidates {
		factDimensions := dimensions
		factDimensions.Language = gitadapter.DetectLanguage(candidate.Hypothesis.Anchor.Path)
		factDimensions.RuleID = candidate.Hypothesis.Dimension.ID
		factDimensions.Path = candidate.Hypothesis.Anchor.Path
		fact := analytics.FindingFunnelFact{
			SchemaVersion:          analytics.FindingFunnelFactSchemaVersion,
			FactID:                 stableFactID("formal-finding-funnel", frozen.run.RunID, candidate.CandidateID),
			RunID:                  frozen.run.RunID,
			CandidateID:            candidate.CandidateID,
			Normalized:             false,
			Verification:           analytics.VerificationNotReached,
			PublicationEligibility: analytics.EligibilityNotReached,
			Publication:            analytics.PublicationNotReached,
			ResultCompleteness:     completeness,
			IncompleteReasons:      slices.Clone(reasons),
			Dimensions:             factDimensions,
			OccurredAt:             frozen.run.CompletedAt.UTC(),
		}
		if finding, exists := findings[candidate.CandidateID]; exists {
			decision, decided := decisions[finding.FindingID]
			suppression, governed := suppressions[finding.FindingID]
			if !decided || decision.Action != contractsv1alpha1.GovernedFindingQueuedForHuman {
				return nil, nil, binding, fmt.Errorf(
					"formal finding %q lacks its governed human-queue decision", finding.FindingID,
				)
			}
			if !governed {
				return nil, nil, binding, fmt.Errorf(
					"formal finding %q lacks its suppression decision", finding.FindingID,
				)
			}
			fact.FindingID = finding.FindingID
			fact.Normalized = true
			fact.Verification = analytics.VerificationVerified
			if suppression.Action == contractsv1alpha1.FindingSuppressed {
				fact.PublicationEligibility = analytics.EligibilitySuppressed
			} else {
				fact.PublicationEligibility = analytics.EligibilityHumanQueue
			}
		}
		facts = append(facts, fact)
	}
	summary := &analytics.FunnelSummary{
		Candidates: uint64(report.Summary.Candidates),
		Normalized: uint64(report.Summary.Findings),
		Verified:   uint64(report.Summary.Confirmed),
		Published:  0,
	}
	if err := summary.Validate(); err != nil {
		return nil, nil, binding, err
	}
	binding.HypothesisSetSHA256 = frozen.run.HypothesisSetRef.SHA256
	binding.CandidateSetSHA256 = frozen.run.CandidateSetRef.SHA256
	binding.VerificationLedgerSHA256 = frozen.run.VerificationLedgerRef.SHA256
	binding.CalibrationLedgerSHA256 = frozen.run.CalibrationLedgerRef.SHA256
	binding.SuppressionLedgerSHA256 = frozen.run.SuppressionLedgerRef.SHA256
	binding.GovernedReportSHA256 = frozen.run.GovernedReportRef.SHA256
	binding.Candidates = summary.Candidates
	binding.Normalized = summary.Normalized
	binding.Verified = summary.Verified
	binding.Decisions = uint64(len(report.Decisions))
	binding.Published = summary.Published
	return facts, summary, binding, nil
}

type publicationObservation struct {
	state       analytics.PublicationState
	ref         *analytics.SourceRef
	publishedAt *time.Time
}

// Publication is an as-of join for the selected run cohort: the run must occur
// inside request.Window, while channel events up to EndExclusive define the
// state visible at that cutoff. StartInclusive does not discard earlier state
// transitions for a run that belongs to the cohort.
func (adapter *Adapter) projectPublicationFacts(
	request RebuildRequest,
	frozen frozenRun,
	facts []analytics.FindingFunnelFact,
) ([]analytics.FindingFunnelFact, []string, error) {
	if len(facts) == 0 {
		return facts, nil, nil
	}
	// Formal governed findings are intentionally queued for human review and
	// grant no publication authority. Their immutable initial decision therefore
	// proves publication_not_reached without consulting the legacy publication
	// request contract, which is bound to FindingSet rather than CandidateSet.
	if frozen.governedReport != nil {
		return slices.Clone(facts), nil, nil
	}
	projected := slices.Clone(facts)
	if adapter.publication == nil {
		reason := "publication_repository_not_configured"
		for index := range projected {
			if !projected[index].Normalized {
				continue
			}
			projected[index].Publication = analytics.PublicationUnknown
			projected[index].PublicationRef = nil
			projected[index].PublicationAt = nil
			projected[index].ResultCompleteness = lessComplete(
				projected[index].ResultCompleteness,
				analytics.CompletenessPartial,
			)
			projected[index].IncompleteReasons = appendReason(
				projected[index].IncompleteReasons,
				reason,
			)
		}
		return projected, []string{reason}, nil
	}

	findings := make(map[string]reviewcore.Finding, len(frozen.findingSet.Findings))
	for _, finding := range frozen.findingSet.Findings {
		findings[finding.ID] = finding
	}
	decisions := make(map[string]reviewcore.FindingDecision)
	for _, decision := range frozen.findingSet.Decisions {
		current, exists := decisions[decision.FindingID]
		if !exists || decision.Sequence > current.Sequence {
			decisions[decision.FindingID] = decision
		}
	}
	observations := make(map[string]publicationObservation)
	for _, fact := range projected {
		if !fact.Normalized {
			continue
		}
		if _, exists := observations[fact.FindingID]; exists {
			continue
		}
		finding, exists := findings[fact.FindingID]
		if !exists {
			return nil, nil, fmt.Errorf(
				"finding fact %q is absent from the frozen finding set",
				fact.FindingID,
			)
		}
		decision, exists := decisions[fact.FindingID]
		if !exists {
			return nil, nil, fmt.Errorf(
				"finding %q lacks a latest decision",
				fact.FindingID,
			)
		}
		records, err := adapter.publication.ByFinding(
			frozen.run.RunID,
			fact.FindingID,
		)
		if err != nil {
			return nil, nil, err
		}
		observation, err := observePublication(
			records,
			frozen,
			finding,
			decision,
			request.Window.EndExclusive,
		)
		if err != nil {
			return nil, nil, err
		}
		observations[fact.FindingID] = observation
	}
	for index := range projected {
		if !projected[index].Normalized {
			continue
		}
		observation := observations[projected[index].FindingID]
		projected[index].Publication = observation.state
		projected[index].PublicationRef = cloneAnalyticsSourceRef(observation.ref)
		projected[index].PublicationAt = timePointerClone(observation.publishedAt)
	}
	return projected, nil, nil
}

func observePublication(
	records []publication.Record,
	frozen frozenRun,
	finding reviewcore.Finding,
	decision reviewcore.FindingDecision,
	endExclusive time.Time,
) (publicationObservation, error) {
	if len(records) == 0 {
		return publicationObservation{state: analytics.PublicationNotReached}, nil
	}
	records = slices.Clone(records)
	slices.SortFunc(records, func(left, right publication.Record) int {
		return strings.Compare(
			left.Request.PublicationID,
			right.Request.PublicationID,
		)
	})
	best := publicationObservation{state: analytics.PublicationNotReached}
	bestRank := publicationStateRank(best.state)
	for index, record := range records {
		if index > 0 &&
			record.Request.PublicationID == records[index-1].Request.PublicationID {
			return publicationObservation{}, fmt.Errorf(
				"publication source returned duplicate record %q",
				record.Request.PublicationID,
			)
		}
		if err := validatePublicationBinding(
			record,
			frozen,
			finding,
			decision,
		); err != nil {
			return publicationObservation{}, fmt.Errorf(
				"publication %q: %w",
				record.Request.PublicationID,
				err,
			)
		}
		observation, err := publicationObservationAt(record, endExclusive)
		if err != nil {
			return publicationObservation{}, fmt.Errorf(
				"publication %q: %w",
				record.Request.PublicationID,
				err,
			)
		}
		rank := publicationStateRank(observation.state)
		if rank > bestRank {
			best = observation
			bestRank = rank
		}
	}
	return best, nil
}

func validatePublicationBinding(
	record publication.Record,
	frozen frozenRun,
	finding reviewcore.Finding,
	decision reviewcore.FindingDecision,
) error {
	if err := record.Request.Validate(); err != nil {
		return fmt.Errorf("invalid request: %w", err)
	}
	digest, err := publication.DigestRequest(record.Request)
	if err != nil {
		return err
	}
	if digest != record.RequestSHA256 {
		return fmt.Errorf("request digest does not match the immutable request")
	}
	if frozen.run.FindingSetRef == nil {
		return fmt.Errorf("frozen run has no finding set")
	}
	request := record.Request
	switch {
	case request.RunID != frozen.run.RunID:
		return fmt.Errorf("request binds a different run")
	case request.TenantID != frozen.spec.TenantID:
		return fmt.Errorf("request binds a different tenant")
	case request.WorkspaceID != frozen.spec.WorkspaceID:
		return fmt.Errorf("request binds a different workspace")
	case request.Provider != frozen.spec.Repository.Provider:
		return fmt.Errorf("request binds a different provider")
	case request.RepositoryID != frozen.spec.Repository.RepositoryID:
		return fmt.Errorf("request binds a different repository")
	case request.BaseRevision != frozen.run.BaseRevision:
		return fmt.Errorf("request binds a different base revision")
	case request.ExpectedHeadRevision != frozen.run.HeadRevision:
		return fmt.Errorf("request binds a different head revision")
	case request.TargetMode != publication.TargetModeDiff ||
		frozen.run.TargetMode != runmodel.TargetModeDiff ||
		frozen.spec.Target.Mode != contractsv1alpha1.ReviewModeDiff:
		return fmt.Errorf("request does not bind the frozen diff target mode")
	case !publicationContentRefMatches(
		request.TargetSnapshotRef,
		frozen.execution.TargetSnapshotRef,
	):
		return fmt.Errorf("request binds a different target snapshot")
	case !publicationContentRefMatches(
		request.ConfigBundleRef,
		frozen.execution.ConfigBundleRef,
	):
		return fmt.Errorf("request binds a different config bundle")
	case !publicationContentRefMatches(
		request.FindingSourceRef,
		*frozen.run.FindingSetRef,
	):
		return fmt.Errorf("request binds a different finding set")
	case request.FindingSourceContract != publication.FindingSourceFindingSet:
		return fmt.Errorf("request does not bind the legacy finding-set contract")
	case request.FindingID != finding.ID:
		return fmt.Errorf("request binds a different finding")
	case request.Fingerprint != finding.Fingerprint:
		return fmt.Errorf("request binds a different fingerprint")
	case request.DecisionID != decision.ID:
		return fmt.Errorf("request does not bind the latest decision")
	case decision.Action != reviewcore.DecisionPublish:
		return fmt.Errorf("bound decision is not publish-eligible")
	case request.TargetDigest != finding.TargetDigest:
		return fmt.Errorf("request binds a different target digest")
	case request.Anchor.Path != finding.Path ||
		request.Anchor.StartLine != finding.StartLine ||
		request.Anchor.EndLine != finding.EndLine ||
		request.Anchor.TargetDigest != finding.TargetDigest:
		return fmt.Errorf("request binds a different stable anchor")
	}
	return validatePublicationRecord(record)
}

func validatePublicationRecord(record publication.Record) error {
	if len(record.Events) == 0 {
		return fmt.Errorf("record has no immutable ledger events")
	}
	state := publication.State("")
	var revalidation *publication.Revalidation
	var providerRequest *publication.ProviderRequest
	var providerResult *publication.ProviderResult
	var previous time.Time
	for index, event := range record.Events {
		if err := event.Validate(); err != nil {
			return fmt.Errorf("event[%d]: %w", index, err)
		}
		if event.PublicationID != record.Request.PublicationID ||
			event.RequestSHA256 != record.RequestSHA256 {
			return fmt.Errorf("event[%d] does not bind the record", index)
		}
		if index > 0 && event.OccurredAt.Before(previous) {
			return fmt.Errorf("ledger events are not time ordered")
		}
		previous = event.OccurredAt
		switch event.Type {
		case publication.EventRequested:
			if index != 0 || state != "" ||
				!reflect.DeepEqual(event.Request, &record.Request) {
				return fmt.Errorf("requested event does not initialize the record")
			}
			if event.OccurredAt.Before(record.Request.CreatedAt) {
				return fmt.Errorf("requested event predates its immutable request")
			}
			state = publication.StateRequested
		case publication.EventDispatchStarted:
			if state != publication.StateRequested {
				return fmt.Errorf("dispatch event has invalid prior state %q", state)
			}
			if event.ProviderRequest.RequestSHA256 != record.RequestSHA256 ||
				event.ProviderRequest.IdempotencyKey != record.Request.IdempotencyKey ||
				event.ProviderRequest.PublicationID != record.Request.PublicationID ||
				event.ProviderRequest.Provider != record.Request.Provider ||
				event.ProviderRequest.RepositoryID != record.Request.RepositoryID ||
				event.ProviderRequest.HeadRevision !=
					record.Request.ExpectedHeadRevision ||
				event.ProviderRequest.FindingID != record.Request.FindingID ||
				event.ProviderRequest.Fingerprint != record.Request.Fingerprint ||
				event.ProviderRequest.DecisionID != record.Request.DecisionID ||
				event.ProviderRequest.Anchor != record.Request.Anchor ||
				event.ProviderRequest.Channel != record.Request.Channel ||
				event.ProviderRequest.Message != record.Request.Message {
				return fmt.Errorf("dispatch event does not bind the request")
			}
			if err := validatePublicationRevalidation(
				record.Request,
				*event.Revalidation,
			); err != nil {
				return err
			}
			if event.OccurredAt.Before(event.Revalidation.CheckedAt) {
				return fmt.Errorf("dispatch event predates provider revalidation")
			}
			revalidation = event.Revalidation
			providerRequest = event.ProviderRequest
			state = publication.StateDispatching
		case publication.EventPublished:
			if state != publication.StateDispatching {
				return fmt.Errorf("published event has invalid prior state %q", state)
			}
			if err := validatePublicationProviderResult(
				record.Request,
				event,
				publication.ProviderResultPublished,
			); err != nil {
				return err
			}
			providerResult = event.ProviderResult
			state = publication.StatePublished
		case publication.EventRejected:
			if state != publication.StateRequested &&
				state != publication.StateDispatching {
				return fmt.Errorf("rejected event has invalid prior state %q", state)
			}
			if err := validatePublicationProviderResult(
				record.Request,
				event,
				publication.ProviderResultRejected,
			); err != nil {
				return err
			}
			providerResult = event.ProviderResult
			state = publication.StateRejected
		case publication.EventOutcomeUnknown:
			if state != publication.StateDispatching {
				return fmt.Errorf("unknown event has invalid prior state %q", state)
			}
			if err := validatePublicationProviderResult(
				record.Request,
				event,
				publication.ProviderResultUnknown,
			); err != nil {
				return err
			}
			providerResult = event.ProviderResult
			state = publication.StateUnknown
		case publication.EventReconciledPublished:
			if state != publication.StateDispatching &&
				state != publication.StateUnknown {
				return fmt.Errorf(
					"reconciled publication has invalid prior state %q",
					state,
				)
			}
			if err := validatePublicationProviderResult(
				record.Request,
				event,
				publication.ProviderResultPublished,
			); err != nil {
				return err
			}
			providerResult = event.ProviderResult
			state = publication.StatePublished
		case publication.EventReconciledNotFound:
			if state != publication.StateDispatching &&
				state != publication.StateUnknown {
				return fmt.Errorf(
					"reconciled absence has invalid prior state %q",
					state,
				)
			}
			if err := validatePublicationProviderResult(
				record.Request,
				event,
				publication.ProviderResultNotFound,
			); err != nil {
				return err
			}
			providerResult = event.ProviderResult
			state = publication.StateNotFound
		default:
			return fmt.Errorf("unsupported publication event %q", event.Type)
		}
	}
	if state != record.State ||
		!reflect.DeepEqual(record.Revalidation, revalidation) ||
		!reflect.DeepEqual(record.ProviderRequest, providerRequest) ||
		!reflect.DeepEqual(record.ProviderResult, providerResult) {
		return fmt.Errorf("record summary does not match its immutable events")
	}
	return nil
}

func validatePublicationRevalidation(
	request publication.Request,
	revalidation publication.Revalidation,
) error {
	if err := revalidation.Validate(); err != nil {
		return err
	}
	switch {
	case revalidation.ProviderHeadRevision != request.ExpectedHeadRevision:
		return fmt.Errorf("revalidation head does not match the request")
	case revalidation.CheckedAt.Before(request.CreatedAt):
		return fmt.Errorf("revalidation predates the request")
	case !revalidation.PermissionGranted:
		return fmt.Errorf("revalidation denies publication permission")
	case revalidation.PermissionRevision != request.ExpectedPermissionRevision:
		return fmt.Errorf("revalidation permission revision changed")
	case revalidation.ConfigBundleSHA256 != request.ConfigBundleRef.SHA256:
		return fmt.Errorf("revalidation config digest changed")
	case revalidation.FindingSourceSHA256 != request.FindingSourceRef.SHA256:
		return fmt.Errorf("revalidation finding-set digest changed")
	case !revalidation.FindingCurrent || !revalidation.AnchorCurrent:
		return fmt.Errorf("revalidation does not prove a current finding and anchor")
	default:
		return nil
	}
}

func validatePublicationProviderResult(
	request publication.Request,
	event publication.LedgerEvent,
	expected publication.ProviderResultStatus,
) error {
	if event.ProviderResult == nil {
		return fmt.Errorf("terminal event has no provider result")
	}
	if err := event.ProviderResult.Validate(); err != nil {
		return err
	}
	if event.ProviderResult.Status != expected ||
		event.ProviderResult.IdempotencyKey != request.IdempotencyKey {
		return fmt.Errorf("provider result does not bind the requested outcome")
	}
	if !event.OccurredAt.Equal(event.ProviderResult.ObservedAt) {
		return fmt.Errorf("provider result time does not bind the ledger event")
	}
	return nil
}

func publicationObservationAt(
	record publication.Record,
	endExclusive time.Time,
) (publicationObservation, error) {
	observation := publicationObservation{state: analytics.PublicationNotReached}
	for _, event := range record.Events {
		if !event.OccurredAt.Before(endExclusive) {
			break
		}
		ref, err := publicationEventRef(event)
		if err != nil {
			return publicationObservation{}, err
		}
		observation.ref = &ref
		observation.publishedAt = nil
		switch event.Type {
		case publication.EventRequested:
			observation.state = analytics.PublicationRequested
		case publication.EventDispatchStarted:
			observation.state = analytics.PublicationDispatching
		case publication.EventPublished, publication.EventReconciledPublished:
			if record.State != publication.StatePublished ||
				event.ProviderResult == nil ||
				event.ProviderResult.Status != publication.ProviderResultPublished ||
				event.ProviderResult.ProviderRequestID == "" ||
				event.ProviderResult.CommentID == "" {
				return publicationObservation{}, fmt.Errorf(
					"published state lacks provider comment evidence",
				)
			}
			observation.state = analytics.PublicationPublished
			observation.publishedAt = timePointer(
				event.ProviderResult.ObservedAt.UTC(),
			)
		case publication.EventRejected:
			observation.state = analytics.PublicationRejected
		case publication.EventOutcomeUnknown:
			observation.state = analytics.PublicationUnknown
		case publication.EventReconciledNotFound:
			observation.state = analytics.PublicationNotFound
		default:
			return publicationObservation{}, fmt.Errorf(
				"unsupported publication event %q",
				event.Type,
			)
		}
	}
	return observation, nil
}

func publicationEventRef(
	event publication.LedgerEvent,
) (analytics.SourceRef, error) {
	data, err := json.Marshal(event)
	if err != nil {
		return analytics.SourceRef{}, err
	}
	return analytics.SourceRef{
		Kind:     analytics.SourcePublication,
		ID:       event.PublicationID,
		Revision: string(event.Type),
		SHA256:   digestBytes(data),
	}, nil
}

func publicationContentRefMatches(
	left publication.ContentRef,
	right runmodel.ArtifactRef,
) bool {
	return left.URI == right.URI &&
		left.SHA256 == right.SHA256 &&
		left.SizeBytes == right.SizeBytes
}

func publicationStateRank(state analytics.PublicationState) int {
	switch state {
	case analytics.PublicationPublished:
		return 7
	case analytics.PublicationUnknown:
		return 6
	case analytics.PublicationDispatching:
		return 5
	case analytics.PublicationRequested:
		return 4
	case analytics.PublicationRejected:
		return 3
	case analytics.PublicationNotFound:
		return 2
	case analytics.PublicationNotReached:
		return 1
	default:
		return 0
	}
}

func publishedFindingCount(facts []analytics.FindingFunnelFact) uint64 {
	published := make(map[string]struct{})
	for _, fact := range facts {
		if fact.Normalized && fact.Publication == analytics.PublicationPublished {
			published[fact.FindingID] = struct{}{}
		}
	}
	return uint64(len(published))
}

func cloneAnalyticsSourceRef(ref *analytics.SourceRef) *analytics.SourceRef {
	if ref == nil {
		return nil
	}
	copy := *ref
	return &copy
}

func timePointerClone(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	return timePointer(value.UTC())
}

func lessComplete(
	current analytics.Completeness,
	candidate analytics.Completeness,
) analytics.Completeness {
	rank := map[analytics.Completeness]int{
		analytics.CompletenessComplete: 2,
		analytics.CompletenessPartial:  1,
		analytics.CompletenessUnknown:  0,
	}
	if rank[candidate] < rank[current] {
		return candidate
	}
	return current
}

func (adapter *Adapter) buildFeedbackFacts(
	ctx context.Context,
	request RebuildRequest,
	frozen frozenRun,
	findingFacts []analytics.FindingFunnelFact,
) ([]analytics.FeedbackOutcomeFact, []string, error) {
	type observableFinding struct {
		findingID  string
		dimensions analytics.Dimensions
		published  bool
	}
	observable := make(map[string]observableFinding)
	for _, fact := range findingFacts {
		if !fact.Normalized {
			continue
		}
		observable[fact.FindingID] = observableFinding{
			findingID:  fact.FindingID,
			dimensions: fact.Dimensions,
			published:  fact.Publication == analytics.PublicationPublished,
		}
	}
	keys := make([]string, 0, len(observable))
	for key := range observable {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	facts := make([]analytics.FeedbackOutcomeFact, 0, len(keys))
	reasons := []string{}
	for _, findingID := range keys {
		if err := contextErr(ctx); err != nil {
			return nil, nil, err
		}
		projected := observable[findingID]
		if adapter.feedback == nil {
			if !projected.published {
				continue
			}
			facts = append(facts, analytics.FeedbackOutcomeFact{
				SchemaVersion: analytics.FeedbackOutcomeFactSchemaVersion,
				FactID: stableFactID(
					"feedback-outcome",
					frozen.run.RunID,
					findingID,
				),
				RunID:              frozen.run.RunID,
				FindingID:          findingID,
				Feedback:           analytics.FeedbackUnknown,
				Outcome:            analytics.OutcomeUnknown,
				ResultCompleteness: analytics.CompletenessUnknown,
				IncompleteReasons:  []string{"feedback_repository_not_configured"},
				Dimensions:         projected.dimensions,
				OccurredAt:         frozen.run.CompletedAt.UTC(),
			})
			reasons = appendReason(reasons, "feedback_repository_not_configured")
			continue
		}
		entries, err := adapter.feedback.ByFinding(findingID)
		if err != nil {
			return nil, nil, err
		}
		var latestFeedback *feedbackdomain.Feedback
		var latestFeedbackSequence uint64
		var latestOutcome *feedbackdomain.Outcome
		var latestOutcomeSequence uint64
		for _, entry := range entries {
			switch {
			case entry.Feedback != nil &&
				entry.Feedback.RunID == frozen.run.RunID &&
				entry.Feedback.RecordedAt.Before(request.Window.EndExclusive):
				copy := *entry.Feedback
				latestFeedback = &copy
				latestFeedbackSequence = entry.Sequence
			case entry.Outcome != nil &&
				entry.Outcome.RunID == frozen.run.RunID &&
				entry.Outcome.RecordedAt.Before(request.Window.EndExclusive):
				copy := *entry.Outcome
				latestOutcome = &copy
				latestOutcomeSequence = entry.Sequence
			}
		}
		// A remotely published Finding is observable even before any response,
		// so it contributes explicit no_feedback/no_outcome. A formal
		// human-queue Finding only enters this projection after the platform has
		// an actual Feedback or Outcome fact; absence must not be mistaken for
		// having shown the Finding to a user.
		if !projected.published && latestFeedback == nil && latestOutcome == nil {
			continue
		}
		fact, factReasons, err := projectFeedbackOutcomeForFinding(
			frozen.run,
			findingID,
			projected.dimensions,
			latestFeedback,
			latestFeedbackSequence,
			latestOutcome,
			latestOutcomeSequence,
		)
		if err != nil {
			return nil, nil, err
		}
		reasons = appendReasons(reasons, factReasons...)
		facts = append(facts, fact)
	}
	return facts, reasons, nil
}

func projectFeedbackOutcomeForFinding(
	run runmodel.ReviewRun,
	findingID string,
	dimensions analytics.Dimensions,
	latestFeedback *feedbackdomain.Feedback,
	feedbackSequence uint64,
	latestOutcome *feedbackdomain.Outcome,
	outcomeSequence uint64,
) (analytics.FeedbackOutcomeFact, []string, error) {
	reasons := []string{}
	fact := analytics.FeedbackOutcomeFact{
		SchemaVersion:      analytics.FeedbackOutcomeFactSchemaVersion,
		FactID:             stableFactID("feedback-outcome", run.RunID, findingID),
		RunID:              run.RunID,
		FindingID:          findingID,
		Feedback:           analytics.FeedbackNoFeedback,
		Outcome:            analytics.OutcomeNoOutcome,
		ResultCompleteness: analytics.CompletenessComplete,
		IncompleteReasons:  []string{},
		Dimensions:         dimensions,
		OccurredAt:         run.CompletedAt.UTC(),
	}
	if latestFeedback != nil {
		state, complete := mapFeedback(latestFeedback.Action)
		fact.Feedback = state
		if !complete {
			reasons = appendReason(reasons, "feedback_needs_discussion")
		}
		fact.FeedbackAt = timePointer(latestFeedback.OccurredAt.UTC())
		ref, err := feedbackSourceRef(
			analytics.SourceFeedback,
			latestFeedback.FeedbackID,
			feedbackSequence,
			*latestFeedback,
		)
		if err != nil {
			return analytics.FeedbackOutcomeFact{}, nil, err
		}
		fact.FeedbackRef = &ref
		if latestFeedback.RecordedAt.After(fact.OccurredAt) {
			fact.OccurredAt = latestFeedback.RecordedAt.UTC()
		}
	}
	if latestOutcome != nil {
		state, complete := mapOutcome(latestOutcome.State)
		fact.Outcome = state
		if !complete {
			reasons = appendReason(reasons, "outcome_unknown")
		}
		fact.OutcomeAt = timePointer(latestOutcome.OccurredAt.UTC())
		ref, err := feedbackSourceRef(
			analytics.SourceOutcome,
			latestOutcome.OutcomeID,
			outcomeSequence,
			*latestOutcome,
		)
		if err != nil {
			return analytics.FeedbackOutcomeFact{}, nil, err
		}
		fact.OutcomeRef = &ref
		if latestOutcome.RecordedAt.After(fact.OccurredAt) {
			fact.OccurredAt = latestOutcome.RecordedAt.UTC()
		}
	}
	if len(reasons) > 0 {
		fact.ResultCompleteness = analytics.CompletenessPartial
		fact.IncompleteReasons = slices.Clone(reasons)
	}
	if err := fact.Validate(); err != nil {
		return analytics.FeedbackOutcomeFact{}, nil, err
	}
	return fact, reasons, nil
}

func validateFindingSet(set findingSetArtifact) error {
	if set.SchemaVersion != runmodel.ContractFindingSet {
		return fmt.Errorf("unsupported finding set schema %q", set.SchemaVersion)
	}
	if set.Findings == nil || set.Decisions == nil {
		return fmt.Errorf("finding set arrays must be explicit")
	}
	findingIDs := make(map[string]reviewcore.Finding, len(set.Findings))
	for index, finding := range set.Findings {
		if err := finding.Validate(); err != nil {
			return fmt.Errorf("findings[%d]: %w", index, err)
		}
		if finding.TargetDigest != set.TargetDigest {
			return fmt.Errorf("finding target digest does not match finding set")
		}
		if _, duplicate := findingIDs[finding.ID]; duplicate {
			return fmt.Errorf("duplicate finding %q", finding.ID)
		}
		findingIDs[finding.ID] = finding
	}
	decisions := make(map[string][]reviewcore.FindingDecision)
	for index, decision := range set.Decisions {
		if err := decision.Validate(); err != nil {
			return fmt.Errorf("decisions[%d]: %w", index, err)
		}
		if _, exists := findingIDs[decision.FindingID]; !exists {
			return fmt.Errorf("decision references unknown finding %q", decision.FindingID)
		}
		decisions[decision.FindingID] = append(
			decisions[decision.FindingID],
			decision,
		)
	}
	for findingID := range findingIDs {
		history := decisions[findingID]
		if len(history) == 0 {
			return fmt.Errorf("finding %q has no decision", findingID)
		}
		sort.Slice(history, func(left, right int) bool {
			return history[left].Sequence < history[right].Sequence
		})
		for index, decision := range history {
			if decision.Sequence != uint32(index+1) ||
				index > 0 && decision.PriorDecisionID != history[index-1].ID {
				return fmt.Errorf("finding %q decision history is broken", findingID)
			}
		}
	}
	return nil
}

func runCompleteness(
	frozen frozenRun,
) (analytics.Completeness, []string) {
	reasons := []string{}
	switch frozen.run.Status {
	case runmodel.RunStatusFailed:
		reasons = appendReason(reasons, "run_failed")
	case runmodel.RunStatusCanceled:
		reasons = appendReason(reasons, "run_canceled")
	}
	if frozen.target.Snapshot.Completeness != gitadapter.CompletenessComplete {
		for _, reason := range frozen.target.Snapshot.CompletenessReason {
			reasons = appendReason(
				reasons,
				reasonCode("target", string(reason.Code)),
			)
		}
		if len(frozen.target.Snapshot.CompletenessReason) == 0 {
			reasons = appendReason(reasons, "target_incomplete")
		}
	}
	for _, evidence := range frozen.run.Evidence {
		if evidence.Completeness != runmodel.CompletenessComplete {
			reasons = appendReasons(
				reasons,
				prefixReasons("evidence_"+evidence.StageID, evidence.CompletenessNotes)...,
			)
		}
	}
	if frozen.governedReport != nil &&
		frozen.governedReport.Completeness != contractsv1alpha1.AgentReviewComplete {
		reasons = appendReason(reasons, "formal_review_partial")
	}
	if len(reasons) == 0 {
		return analytics.CompletenessComplete, []string{}
	}
	return analytics.CompletenessPartial, reasons
}

func mapRunStatus(status runmodel.RunStatus) (analytics.RunStatus, error) {
	switch status {
	case runmodel.RunStatusSucceeded:
		return analytics.RunStatusSucceeded, nil
	case runmodel.RunStatusFailed:
		return analytics.RunStatusFailed, nil
	case runmodel.RunStatusCanceled:
		return analytics.RunStatusCanceled, nil
	default:
		return "", fmt.Errorf("unsupported terminal run status %q", status)
	}
}

func mapStageStatus(status runmodel.StageStatus) (analytics.StageStatus, error) {
	switch status {
	case runmodel.StageStatusSucceeded:
		return analytics.StageStatusSucceeded, nil
	case runmodel.StageStatusFailed:
		return analytics.StageStatusFailed, nil
	case runmodel.StageStatusCanceled:
		return analytics.StageStatusCanceled, nil
	default:
		return "", fmt.Errorf("unsupported terminal stage status %q", status)
	}
}

func mapVerification(
	status reviewcore.VerificationStatus,
) (analytics.VerificationState, error) {
	switch status {
	case reviewcore.VerificationVerified:
		return analytics.VerificationVerified, nil
	case reviewcore.VerificationRejected:
		return analytics.VerificationRejected, nil
	case reviewcore.VerificationInconclusive:
		return analytics.VerificationInconclusive, nil
	default:
		return "", fmt.Errorf("unsupported final verification %q", status)
	}
}

func mapDecision(
	action reviewcore.DecisionAction,
) (analytics.PublicationEligibility, error) {
	switch action {
	case reviewcore.DecisionPublish:
		return analytics.EligibilityPublishEligible, nil
	case reviewcore.DecisionReject:
		return analytics.EligibilitySuppressed, nil
	case reviewcore.DecisionHumanReview:
		return analytics.EligibilityHumanQueue, nil
	default:
		return "", fmt.Errorf("unsupported decision action %q", action)
	}
}

func mapFeedback(
	action feedbackdomain.FeedbackAction,
) (analytics.FeedbackState, bool) {
	switch action {
	case feedbackdomain.FeedbackAccept:
		return analytics.FeedbackAccepted, true
	case feedbackdomain.FeedbackDismiss:
		return analytics.FeedbackDismissed, true
	case feedbackdomain.FeedbackWontFix:
		return analytics.FeedbackWontFix, true
	case feedbackdomain.FeedbackOutdated:
		return analytics.FeedbackOutdated, true
	case feedbackdomain.FeedbackNeedsDiscussion:
		return analytics.FeedbackUnknown, false
	default:
		return analytics.FeedbackUnknown, false
	}
}

func mapOutcome(
	state feedbackdomain.OutcomeState,
) (analytics.OutcomeState, bool) {
	switch state {
	case feedbackdomain.OutcomeFixed:
		return analytics.OutcomeFixed, true
	case feedbackdomain.OutcomeRecurred:
		return analytics.OutcomeRecurred, true
	case feedbackdomain.OutcomeEscaped:
		return analytics.OutcomeEscaped, true
	case feedbackdomain.OutcomeUnknown:
		return analytics.OutcomeUnknown, false
	default:
		return analytics.OutcomeUnknown, false
	}
}

func feedbackSourceRef(
	kind analytics.SourceKind,
	id string,
	sequence uint64,
	value any,
) (analytics.SourceRef, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return analytics.SourceRef{}, err
	}
	return analytics.SourceRef{
		Kind:     kind,
		ID:       id,
		Revision: "sequence-" + strconv.FormatUint(sequence, 10),
		SHA256:   digestBytes(data),
	}, nil
}

func stableFactID(prefix string, parts ...string) string {
	return prefix + "-" + digestBytes([]byte(strings.Join(parts, "\x00")))[:24]
}

func prefixReasons(prefix string, reasons []string) []string {
	result := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		result = appendReason(result, reasonCode(prefix, reason))
	}
	return result
}

func appendReasons(existing []string, values ...string) []string {
	result := slices.Clone(existing)
	for _, value := range values {
		result = appendReason(result, value)
	}
	return result
}

func appendReason(existing []string, value string) []string {
	if value == "" {
		return existing
	}
	index, exists := slices.BinarySearch(existing, value)
	if exists {
		return existing
	}
	return slices.Insert(existing, index, value)
}

func reasonCode(prefix, value string) string {
	raw := strings.ToLower(strings.TrimSpace(prefix + "_" + value))
	var builder strings.Builder
	for _, character := range raw {
		switch {
		case unicode.IsLetter(character), unicode.IsDigit(character),
			strings.ContainsRune("._:@+-", character):
			builder.WriteRune(character)
		default:
			builder.WriteByte('_')
		}
	}
	result := strings.Trim(builder.String(), "_")
	if result == "" {
		result = "unknown_reason"
	}
	if len(result) > 240 {
		result = result[:215] + "_" + digestBytes([]byte(result))[:24]
	}
	return result
}

func completeCoverage(source string) SourceCoverage {
	return SourceCoverage{
		Source:            source,
		Completeness:      analytics.CompletenessComplete,
		IncompleteReasons: []string{},
	}
}

func timePointer(value time.Time) *time.Time {
	copy := value
	return &copy
}

func (request RebuildRequest) WindowContains(value time.Time) bool {
	return !value.Before(request.Window.StartInclusive) &&
		value.Before(request.Window.EndExclusive)
}
