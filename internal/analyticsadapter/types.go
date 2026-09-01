// Package analyticsadapter materializes versioned analytics projections from
// authoritative local repositories. Rebuild is the only operation that reads
// source ledgers; query and export read immutable projection snapshots only.
package analyticsadapter

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/abietic/argus/internal/analytics"
)

const (
	ProjectionSnapshotSchemaVersion = "argus.analytics_projection_snapshot.v1alpha1"
	ProjectionManifestSchemaVersion = "argus.analytics_projection_manifest.v1alpha1"
	ProjectionPolicyRevision        = "argus.analytics_projection_policy.v1alpha1"

	projectionManifestRoot = "analytics/projection-manifests/"
)

const (
	CoverageCost            = "cost"
	CoverageEvaluation      = "evaluation"
	CoverageFeedbackOutcome = "feedback_outcome"
	CoverageFindingDecision = "finding_decision"
	CoverageFindingLineage  = "finding_lineage"
	CoveragePublication     = "publication"
	CoverageRunLedger       = "run_ledger"
	CoverageTrace           = "trace"
)

type Scope struct {
	TenantID       string `json:"tenant_id"`
	OrganizationID string `json:"organization_id"`
	RepositoryID   string `json:"repository_id"`
}

type RebuildRequest struct {
	SnapshotID string                    `json:"snapshot_id"`
	Scope      Scope                     `json:"scope"`
	Window     analytics.TimeWindow      `json:"window"`
	GroupBy    []analytics.DimensionName `json:"group_by"`
	BuiltAt    time.Time                 `json:"built_at"`
}

// EvaluationSource is intentionally narrower than evaluation.Repository.
// Evaluation ownership remains outside this adapter; an authorized caller may
// supply already-governed, versioned experiment facts.
type EvaluationSource interface {
	EvaluationFacts(
		context.Context,
		Scope,
		analytics.TimeWindow,
	) (EvaluationBatch, error)
}

type EvaluationBatch struct {
	Completeness       analytics.Completeness        `json:"completeness"`
	IncompleteReasons  []string                      `json:"incomplete_reasons"`
	Facts              []analytics.ExperimentFact    `json:"facts"`
	RepeatabilityFacts []analytics.RepeatabilityFact `json:"repeatability_facts"`
}

type FindingLineageSource interface {
	FindingLineageFacts(context.Context, Scope, analytics.TimeWindow) (FindingLineageBatch, error)
}

type FindingLineageBatch struct {
	Completeness      analytics.Completeness         `json:"completeness"`
	IncompleteReasons []string                       `json:"incomplete_reasons"`
	Facts             []analytics.FindingLineageFact `json:"facts"`
}

type SourceCoverage struct {
	Source            string                 `json:"source"`
	Completeness      analytics.Completeness `json:"completeness"`
	IncompleteReasons []string               `json:"incomplete_reasons"`
}

// RunSourceBinding preserves the exact frozen inputs from which analytical
// facts were derived. Counts are reconciliation evidence, not mutable totals.
type RunSourceBinding struct {
	RunID                    string `json:"run_id"`
	FinalRunSHA256           string `json:"final_run_sha256"`
	ExecutionSnapshotID      string `json:"execution_snapshot_id"`
	ReviewSpecSHA256         string `json:"review_spec_sha256"`
	TargetSnapshotSHA256     string `json:"target_snapshot_sha256"`
	ConfigID                 string `json:"config_id"`
	ConfigRevision           string `json:"config_revision"`
	ConfigSHA256             string `json:"config_sha256"`
	WorkflowID               string `json:"workflow_id"`
	WorkflowRevision         string `json:"workflow_revision"`
	WorkflowSHA256           string `json:"workflow_sha256"`
	FindingSetSHA256         string `json:"finding_set_sha256,omitempty"`
	DetectStageSHA256        string `json:"detect_stage_sha256,omitempty"`
	HypothesisSetSHA256      string `json:"hypothesis_set_sha256,omitempty"`
	CandidateSetSHA256       string `json:"candidate_set_sha256,omitempty"`
	VerificationLedgerSHA256 string `json:"verification_ledger_sha256,omitempty"`
	CalibrationLedgerSHA256  string `json:"calibration_ledger_sha256,omitempty"`
	SuppressionLedgerSHA256  string `json:"suppression_ledger_sha256,omitempty"`
	GovernedReportSHA256     string `json:"governed_report_sha256,omitempty"`
	Candidates               uint64 `json:"candidates"`
	Normalized               uint64 `json:"normalized"`
	Verified                 uint64 `json:"verified"`
	Decisions                uint64 `json:"decisions"`
	Published                uint64 `json:"published"`
}

type ProjectionSnapshot struct {
	SchemaVersion  string                        `json:"schema_version"`
	SnapshotID     string                        `json:"snapshot_id"`
	PolicyRevision string                        `json:"policy_revision"`
	Scope          Scope                         `json:"scope"`
	Window         analytics.TimeWindow          `json:"window"`
	GroupBy        []analytics.DimensionName     `json:"group_by"`
	BuiltAt        time.Time                     `json:"built_at"`
	Coverage       []SourceCoverage              `json:"source_coverage"`
	RunBindings    []RunSourceBinding            `json:"run_source_bindings"`
	Facts          analytics.FactSet             `json:"facts"`
	Dashboard      analytics.DashboardProjection `json:"dashboard"`
}

// SnapshotSummary is the immutable manifest projection exposed by list APIs.
// It intentionally excludes fact and dashboard payload bytes.
type SnapshotSummary struct {
	SnapshotID string                    `json:"snapshot_id"`
	Scope      Scope                     `json:"scope"`
	Window     analytics.TimeWindow      `json:"window"`
	GroupBy    []analytics.DimensionName `json:"group_by"`
	BuiltAt    time.Time                 `json:"built_at"`
}

type Selector struct {
	Scope   Scope
	Window  analytics.TimeWindow
	GroupBy []analytics.DimensionName
}

type ExportTarget string

const (
	ExportFacts     ExportTarget = "facts"
	ExportDashboard ExportTarget = "dashboard"
)

func (scope Scope) Validate() error {
	dimensions := analytics.Dimensions{
		TenantID:         scope.TenantID,
		OrganizationID:   scope.OrganizationID,
		RepositoryID:     scope.RepositoryID,
		WorkflowRevision: "scope",
		ConfigRevision:   "scope",
	}
	return dimensions.Validate()
}

func (request RebuildRequest) Validate() error {
	if err := validateID("snapshot_id", request.SnapshotID); err != nil {
		return err
	}
	if err := request.Scope.Validate(); err != nil {
		return fmt.Errorf("scope: %w", err)
	}
	if err := request.Window.Validate(); err != nil {
		return fmt.Errorf("window: %w", err)
	}
	if err := validateGroupBy(request.GroupBy); err != nil {
		return err
	}
	if err := validateUTC("built_at", request.BuiltAt); err != nil {
		return err
	}
	if request.BuiltAt.Before(request.Window.EndExclusive) {
		return fmt.Errorf("built_at must not precede the closed observation window")
	}
	return nil
}

func (batch EvaluationBatch) Validate() error {
	if err := validateCompleteness(
		"evaluation batch",
		batch.Completeness,
		batch.IncompleteReasons,
	); err != nil {
		return err
	}
	if batch.Facts == nil {
		return fmt.Errorf("evaluation facts must be an explicit array")
	}
	if batch.RepeatabilityFacts == nil {
		return fmt.Errorf("repeatability facts must be an explicit array")
	}
	for index, fact := range batch.Facts {
		if err := fact.Validate(); err != nil {
			return fmt.Errorf("evaluation facts[%d]: %w", index, err)
		}
	}
	for index, fact := range batch.RepeatabilityFacts {
		if err := fact.Validate(); err != nil {
			return fmt.Errorf("repeatability facts[%d]: %w", index, err)
		}
	}
	return nil
}

func (batch FindingLineageBatch) Validate() error {
	if err := validateCompleteness("finding lineage batch", batch.Completeness, batch.IncompleteReasons); err != nil {
		return err
	}
	if batch.Facts == nil {
		return fmt.Errorf("finding lineage facts must be an explicit array")
	}
	for index, fact := range batch.Facts {
		if err := fact.Validate(); err != nil {
			return fmt.Errorf("finding lineage facts[%d]: %w", index, err)
		}
	}
	return nil
}

func (coverage SourceCoverage) Validate() error {
	switch coverage.Source {
	case CoverageCost, CoverageEvaluation, CoverageFeedbackOutcome,
		CoverageFindingDecision, CoveragePublication, CoverageRunLedger,
		CoverageFindingLineage, CoverageTrace:
	default:
		return fmt.Errorf("unsupported source coverage %q", coverage.Source)
	}
	return validateCompleteness(
		"source coverage "+coverage.Source,
		coverage.Completeness,
		coverage.IncompleteReasons,
	)
}

func (binding RunSourceBinding) Validate() error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{"run_id", binding.RunID},
		{"execution_snapshot_id", binding.ExecutionSnapshotID},
		{"config_id", binding.ConfigID},
		{"config_revision", binding.ConfigRevision},
		{"workflow_id", binding.WorkflowID},
		{"workflow_revision", binding.WorkflowRevision},
	} {
		if err := validateID(field.name, field.value); err != nil {
			return err
		}
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"final_run_sha256", binding.FinalRunSHA256},
		{"review_spec_sha256", binding.ReviewSpecSHA256},
		{"target_snapshot_sha256", binding.TargetSnapshotSHA256},
		{"config_sha256", binding.ConfigSHA256},
		{"workflow_sha256", binding.WorkflowSHA256},
	} {
		if err := validateSHA256(field.name, field.value); err != nil {
			return err
		}
	}
	legacy := binding.FindingSetSHA256 != "" || binding.DetectStageSHA256 != ""
	formal := binding.HypothesisSetSHA256 != "" || binding.CandidateSetSHA256 != "" ||
		binding.VerificationLedgerSHA256 != "" ||
		binding.CalibrationLedgerSHA256 != "" || binding.SuppressionLedgerSHA256 != "" ||
		binding.GovernedReportSHA256 != ""
	if legacy && formal {
		return fmt.Errorf("run binding cannot mix legacy and formal finding sources")
	}
	if !legacy && !formal {
		if binding.Candidates != 0 || binding.Normalized != 0 ||
			binding.Verified != 0 || binding.Decisions != 0 ||
			binding.Published != 0 {
			return fmt.Errorf("run without finding sources cannot claim finding projections")
		}
		return nil
	}
	if legacy {
		if err := validateSHA256("finding_set_sha256", binding.FindingSetSHA256); err != nil {
			return err
		}
		if err := validateSHA256("detect_stage_sha256", binding.DetectStageSHA256); err != nil {
			return err
		}
	}
	if formal {
		for _, field := range []struct {
			name  string
			value string
		}{
			{"hypothesis_set_sha256", binding.HypothesisSetSHA256},
			{"candidate_set_sha256", binding.CandidateSetSHA256},
			{"verification_ledger_sha256", binding.VerificationLedgerSHA256},
			{"calibration_ledger_sha256", binding.CalibrationLedgerSHA256},
			{"suppression_ledger_sha256", binding.SuppressionLedgerSHA256},
			{"governed_report_sha256", binding.GovernedReportSHA256},
		} {
			if err := validateSHA256(field.name, field.value); err != nil {
				return err
			}
		}
	}
	if binding.Normalized > binding.Candidates ||
		binding.Verified > binding.Normalized ||
		binding.Published > binding.Verified ||
		binding.Decisions < binding.Normalized {
		return fmt.Errorf("run binding funnel counts are inconsistent")
	}
	return nil
}

func (snapshot ProjectionSnapshot) Validate() error {
	if snapshot.SchemaVersion != ProjectionSnapshotSchemaVersion {
		return fmt.Errorf(
			"unsupported projection snapshot schema %q",
			snapshot.SchemaVersion,
		)
	}
	if err := validateID("snapshot_id", snapshot.SnapshotID); err != nil {
		return err
	}
	if snapshot.PolicyRevision != ProjectionPolicyRevision {
		return fmt.Errorf("unsupported projection policy %q", snapshot.PolicyRevision)
	}
	if err := snapshot.Scope.Validate(); err != nil {
		return fmt.Errorf("scope: %w", err)
	}
	if err := snapshot.Window.Validate(); err != nil {
		return fmt.Errorf("window: %w", err)
	}
	if err := validateGroupBy(snapshot.GroupBy); err != nil {
		return err
	}
	if err := validateUTC("built_at", snapshot.BuiltAt); err != nil {
		return err
	}
	if snapshot.BuiltAt.Before(snapshot.Window.EndExclusive) {
		return fmt.Errorf("built_at precedes observation window")
	}
	if snapshot.Coverage == nil || snapshot.RunBindings == nil {
		return fmt.Errorf("source coverage and run bindings must be explicit arrays")
	}
	expectedSources := []string{
		CoverageCost,
		CoverageEvaluation,
		CoverageFeedbackOutcome,
		CoverageFindingDecision,
		CoverageFindingLineage,
		CoveragePublication,
		CoverageRunLedger,
		CoverageTrace,
	}
	if len(snapshot.Coverage) != len(expectedSources) {
		return fmt.Errorf("source coverage must contain every governed source")
	}
	for index, coverage := range snapshot.Coverage {
		if err := coverage.Validate(); err != nil {
			return fmt.Errorf("source_coverage[%d]: %w", index, err)
		}
		if coverage.Source != expectedSources[index] {
			return fmt.Errorf("source coverage must be canonically ordered and complete")
		}
	}
	previousRun := ""
	for index, binding := range snapshot.RunBindings {
		if err := binding.Validate(); err != nil {
			return fmt.Errorf("run_source_bindings[%d]: %w", index, err)
		}
		if index > 0 && binding.RunID <= previousRun {
			return fmt.Errorf("run source bindings must be uniquely sorted")
		}
		previousRun = binding.RunID
	}
	if err := snapshot.Facts.Validate(); err != nil {
		return fmt.Errorf("facts: %w", err)
	}
	if snapshot.Facts.Window != snapshot.Window {
		return fmt.Errorf("fact window does not match projection snapshot")
	}
	if err := validateFactScope(snapshot.Facts, snapshot.Scope); err != nil {
		return err
	}
	if err := snapshot.Dashboard.Validate(); err != nil {
		return fmt.Errorf("dashboard: %w", err)
	}
	if snapshot.Dashboard.Window != snapshot.Window ||
		!slices.Equal(snapshot.Dashboard.GroupBy, snapshot.GroupBy) {
		return fmt.Errorf("dashboard selector does not match projection snapshot")
	}
	expected, err := analytics.ProjectDashboard(snapshot.Facts, snapshot.GroupBy)
	if err != nil {
		return fmt.Errorf("rebuild dashboard validation projection: %w", err)
	}
	if !reflect.DeepEqual(expected, snapshot.Dashboard) {
		return fmt.Errorf("dashboard does not exactly project the frozen facts")
	}
	if len(snapshot.RunBindings) != len(snapshot.Facts.ReviewRuns) {
		return fmt.Errorf("every review run fact requires one exact source binding")
	}
	for index := range snapshot.RunBindings {
		if snapshot.RunBindings[index].RunID != snapshot.Facts.ReviewRuns[index].RunID {
			return fmt.Errorf("run facts and source bindings do not reconcile")
		}
	}
	return nil
}

func (selector Selector) Validate() error {
	if err := selector.Scope.Validate(); err != nil {
		return fmt.Errorf("scope: %w", err)
	}
	if err := selector.Window.Validate(); err != nil {
		return fmt.Errorf("window: %w", err)
	}
	return validateGroupBy(selector.GroupBy)
}

func validateFactScope(facts analytics.FactSet, scope Scope) error {
	check := func(name string, dimensions analytics.Dimensions) error {
		if dimensions.TenantID != scope.TenantID ||
			dimensions.OrganizationID != scope.OrganizationID ||
			dimensions.RepositoryID != scope.RepositoryID {
			return fmt.Errorf("%s escapes projection scope", name)
		}
		return nil
	}
	for index, fact := range facts.ReviewRuns {
		if err := check(fmt.Sprintf("review_runs[%d]", index), fact.Dimensions); err != nil {
			return err
		}
	}
	for index, fact := range facts.Stages {
		if err := check(fmt.Sprintf("stages[%d]", index), fact.Dimensions); err != nil {
			return err
		}
	}
	for index, fact := range facts.Findings {
		if err := check(fmt.Sprintf("finding_funnel[%d]", index), fact.Dimensions); err != nil {
			return err
		}
	}
	for index, fact := range facts.FeedbackOutcomes {
		if err := check(fmt.Sprintf("feedback_outcomes[%d]", index), fact.Dimensions); err != nil {
			return err
		}
	}
	for index, fact := range facts.FindingLineages {
		if fact.TenantID != scope.TenantID || fact.OrganizationID != scope.OrganizationID || fact.RepositoryID != scope.RepositoryID {
			return fmt.Errorf("finding_lineages[%d] escapes projection scope", index)
		}
	}
	for index, fact := range facts.Experiments {
		if err := check(fmt.Sprintf("experiments[%d]", index), fact.Dimensions); err != nil {
			return err
		}
	}
	for index, fact := range facts.ValueObservations {
		if err := check(
			fmt.Sprintf("value_observations[%d]", index),
			fact.Dimensions,
		); err != nil {
			return err
		}
	}
	return nil
}

func validateCompleteness(
	name string,
	completeness analytics.Completeness,
	reasons []string,
) error {
	switch completeness {
	case analytics.CompletenessComplete:
		if reasons == nil || len(reasons) != 0 {
			return fmt.Errorf("%s complete state requires an explicit empty reason array", name)
		}
	case analytics.CompletenessPartial, analytics.CompletenessUnknown:
		if len(reasons) == 0 {
			return fmt.Errorf("%s %s state requires reasons", name, completeness)
		}
	default:
		return fmt.Errorf("%s has unsupported completeness %q", name, completeness)
	}
	previous := ""
	for index, reason := range reasons {
		if err := validateID(fmt.Sprintf("%s.reasons[%d]", name, index), reason); err != nil {
			return err
		}
		if index > 0 && reason <= previous {
			return fmt.Errorf("%s reasons must be uniquely sorted", name)
		}
		previous = reason
	}
	return nil
}

func validateGroupBy(groupBy []analytics.DimensionName) error {
	if groupBy == nil {
		return fmt.Errorf("group_by must be an explicit array")
	}
	canonical := canonicalGroupBy(groupBy)
	if !slices.Equal(canonical, groupBy) {
		return fmt.Errorf("group_by must be uniquely sorted in canonical order")
	}
	seen := make(map[analytics.DimensionName]struct{})
	for _, name := range groupBy {
		switch name {
		case analytics.DimensionTenant,
			analytics.DimensionOrganization,
			analytics.DimensionRepository,
			analytics.DimensionLanguage,
			analytics.DimensionRule,
			analytics.DimensionPath,
			analytics.DimensionWorkflowRevision,
			analytics.DimensionConfigRevision:
		default:
			return fmt.Errorf("unsupported group_by dimension %q", name)
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("duplicate group_by dimension %q", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

func canonicalGroupBy(groupBy []analytics.DimensionName) []analytics.DimensionName {
	order := map[analytics.DimensionName]int{
		analytics.DimensionTenant:           0,
		analytics.DimensionOrganization:     1,
		analytics.DimensionRepository:       2,
		analytics.DimensionLanguage:         3,
		analytics.DimensionRule:             4,
		analytics.DimensionPath:             5,
		analytics.DimensionWorkflowRevision: 6,
		analytics.DimensionConfigRevision:   7,
	}
	result := append([]analytics.DimensionName{}, groupBy...)
	slices.SortFunc(result, func(left, right analytics.DimensionName) int {
		return order[left] - order[right]
	})
	return result
}

func validateID(name, value string) error {
	if value == "" || len(value) > 255 || value != strings.TrimSpace(value) ||
		!utf8.ValidString(value) {
		return fmt.Errorf("%s must be a non-empty safe identifier", name)
	}
	for _, character := range value {
		if unicode.IsLetter(character) || unicode.IsDigit(character) ||
			strings.ContainsRune("._:@+-", character) {
			continue
		}
		return fmt.Errorf("%s contains unsupported character %q", name, character)
	}
	return nil
}

func validateSHA256(name, value string) error {
	if len(value) != 64 {
		return fmt.Errorf("%s must be a lowercase SHA-256 digest", name)
	}
	for _, character := range value {
		if character < '0' || character > '9' && character < 'a' ||
			character > 'f' {
			return fmt.Errorf("%s must be a lowercase SHA-256 digest", name)
		}
	}
	return nil
}

func validateUTC(name string, value time.Time) error {
	if value.IsZero() {
		return fmt.Errorf("%s is required", name)
	}
	_, offset := value.Zone()
	if offset != 0 {
		return fmt.Errorf("%s must use UTC", name)
	}
	return nil
}
