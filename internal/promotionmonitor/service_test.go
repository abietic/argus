package promotionmonitor

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/abietic/argus/internal/analytics"
	"github.com/abietic/argus/internal/analyticsadapter"
	"github.com/abietic/argus/internal/calibrationpromotion"
	"github.com/abietic/argus/internal/evaluation"
	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/store/local"
)

func TestBuildObservationDetectsRegressionWithoutUsingFeedbackOrModelScores(t *testing.T) {
	activation := monitorTime(2, 0)
	plan := monitorPlan(activation)
	baseline := monitorSnapshot("baseline-snapshot", monitorTime(0, 0), activation, plan.BaselineBundleSHA256, 10, 10, 10, 10, 8, 8, 0)
	// A non-baseline run in the same immutable snapshot must not contaminate the
	// exact config cohort.
	baseline.RunBindings = append(baseline.RunBindings, analyticsadapter.RunSourceBinding{RunID: "foreign-run", ConfigSHA256: plan.VariantBundleSHA256})
	baseline.Facts.ReviewRuns = append(baseline.Facts.ReviewRuns, analytics.ReviewRunFact{RunID: "foreign-run", Status: analytics.RunStatusFailed, ResultCompleteness: analytics.CompletenessPartial})
	observed := monitorSnapshot("observation-snapshot", activation, monitorTime(4, 0), plan.VariantBundleSHA256, 10, 8, 9, 10, 8, 6, 2)

	result, err := buildObservation(monitorRequest(monitorTime(4, 30)), monitorMutation(monitorTime(4, 30)), plan, strings.Repeat("f", 64), baseline, strings.Repeat("1", 64), observed, strings.Repeat("2", 64))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusRegressed || !result.RollbackRecommendation {
		t.Fatalf("status=%s rollback=%t", result.Status, result.RollbackRecommendation)
	}
	if !slices.Contains(result.ReasonCodes, "published_fixed_rate:regression_exceeds_policy") || !slices.Contains(result.ReasonCodes, "review_run_success_rate:regression_exceeds_policy") {
		t.Fatalf("reason codes = %v", result.ReasonCodes)
	}
	for _, metric := range result.Metrics {
		if strings.Contains(string(metric.MetricID), "feedback") || strings.Contains(metric.Definition, "model") {
			t.Fatalf("weak signal entered quality gate: %+v", metric)
		}
	}
}

func TestBuildObservationSeparatesUnavailableAndInsufficientDataFromRegression(t *testing.T) {
	activation := monitorTime(2, 0)
	plan := monitorPlan(activation)
	baseline := monitorSnapshot("baseline-snapshot", monitorTime(0, 0), activation, plan.BaselineBundleSHA256, 1, 1, 1, 0, 0, 0, 0)
	observed := monitorSnapshot("observation-snapshot", activation, monitorTime(4, 0), plan.VariantBundleSHA256, 1, 0, 0, 0, 0, 0, 0)
	request := monitorRequest(monitorTime(4, 30))
	result, err := buildObservation(request, monitorMutation(request.ObservedAt), plan, strings.Repeat("f", 64), baseline, strings.Repeat("1", 64), observed, strings.Repeat("2", 64))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusInsufficientData || result.RollbackRecommendation {
		t.Fatalf("insufficient result = %+v", result)
	}
	for _, metric := range result.Metrics {
		if metric.Availability != MetricInsufficientData || metric.Regression {
			t.Fatalf("insufficient metric = %+v", metric)
		}
	}

	observed.Facts.Completeness = analytics.CompletenessPartial
	observed.Facts.IncompleteReasons = []string{"feedback_source_unavailable"}
	result, err = buildObservation(request, monitorMutation(request.ObservedAt), plan, strings.Repeat("f", 64), baseline, strings.Repeat("1", 64), observed, strings.Repeat("2", 64))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusUnavailable || result.RollbackRecommendation {
		t.Fatalf("unavailable result = %+v", result)
	}
}

func TestBuildObservationRejectsWindowOrBaselineBindingSubstitution(t *testing.T) {
	activation := monitorTime(2, 0)
	plan := monitorPlan(activation)
	baseline := monitorSnapshot("baseline-snapshot", monitorTime(0, 0), activation.Add(time.Second), plan.BaselineBundleSHA256, 10, 10, 10, 10, 8, 8, 0)
	observed := monitorSnapshot("observation-snapshot", activation, monitorTime(4, 0), plan.VariantBundleSHA256, 10, 10, 10, 10, 8, 8, 0)
	request := monitorRequest(monitorTime(4, 30))
	if _, err := buildObservation(request, monitorMutation(request.ObservedAt), plan, strings.Repeat("f", 64), baseline, strings.Repeat("1", 64), observed, strings.Repeat("2", 64)); !errors.Is(err, ErrEvidenceMismatch) {
		t.Fatalf("window substitution error = %v", err)
	}
	baseline.Window.EndExclusive = activation
	baseline.RunBindings[0].ConfigSHA256 = plan.VariantBundleSHA256
	if _, err := buildObservation(request, monitorMutation(request.ObservedAt), plan, strings.Repeat("f", 64), baseline, strings.Repeat("1", 64), observed, strings.Repeat("2", 64)); !errors.Is(err, ErrEvidenceMismatch) {
		t.Fatalf("baseline substitution error = %v", err)
	}
}

func TestObservationRepositoryIsImmutableAndQueryOnly(t *testing.T) {
	activation := monitorTime(2, 0)
	plan := monitorPlan(activation)
	baseline := monitorSnapshot("baseline-snapshot", monitorTime(0, 0), activation, plan.BaselineBundleSHA256, 10, 10, 10, 10, 8, 8, 0)
	observed := monitorSnapshot("observation-snapshot", activation, monitorTime(4, 0), plan.VariantBundleSHA256, 10, 10, 10, 10, 8, 8, 0)
	request := monitorRequest(monitorTime(4, 30))
	observation, err := buildObservation(request, monitorMutation(request.ObservedAt), plan, strings.Repeat("f", 64), baseline, strings.Repeat("1", 64), observed, strings.Repeat("2", 64))
	if err != nil {
		t.Fatal(err)
	}
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service, err := Open(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.persist(observation); err != nil {
		t.Fatal(err)
	}
	if _, err := service.persist(observation); err != nil {
		t.Fatalf("exact retry: %v", err)
	}
	access := evaluation.Access{Actor: "monitor-reader", Roles: []evaluation.Role{evaluation.RolePromotionOperator}}
	loaded, err := service.Get(request.ObservationID, access)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != StatusHealthy {
		t.Fatalf("loaded = %+v", loaded)
	}
	list, err := service.List(access)
	if err != nil || len(list) != 1 || list[0].ObservationSHA256 == "" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	denied := evaluation.Access{Actor: "ordinary-reviewer", Roles: []evaluation.Role{evaluation.RoleDatasetReviewer}}
	if _, err := service.Get(request.ObservationID, denied); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("denied read = %v", err)
	}
	conflict := observation
	conflict.Audit = "different evidence"
	if _, err := service.persist(conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict = %v", err)
	}
}

func TestDecodeBuildRequestIsStrict(t *testing.T) {
	request := monitorRequest(monitorTime(4, 30))
	data, _ := json.Marshal(request)
	if _, err := DecodeBuildRequest(data); err != nil {
		t.Fatal(err)
	}
	unknown := append(data[:len(data)-1], []byte(`,"unknown":true}`)...)
	if _, err := DecodeBuildRequest(unknown); err == nil {
		t.Fatal("unknown field accepted")
	}
	duplicate := append(data[:len(data)-1], []byte(`,"plan_id":"other"}`)...)
	if _, err := DecodeBuildRequest(duplicate); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate field error = %v", err)
	}
}

func monitorPlan(activation time.Time) calibrationpromotion.Plan {
	return calibrationpromotion.Plan{
		Request: calibrationpromotion.PrepareRequest{PlanID: "promotion-plan-1", CalibrationRunID: "calibration-run-1", BaselineReviewRunID: "baseline-run-0", PromotionPolicyRevision: "promotion-policy-1"},
		Status:  calibrationpromotion.StatusActive, UpdatedAt: activation,
		CalibrationRunSHA256: strings.Repeat("a", 64), BaselineBundleSHA256: strings.Repeat("b", 64), VariantBundleSHA256: strings.Repeat("c", 64), VariantConfigSHA256: strings.Repeat("d", 64),
		VariantConfig: reviewconfig.Revision{ID: "config-calibration", Revision: "2"}, PromotionVariant: evaluation.PromotionVariant{VariantID: "variant-calibration-2"},
	}
}

func monitorRequest(at time.Time) BuildRequest {
	rules := []MetricRule{
		{MetricID: MetricPublishedAdverseRate, Direction: analytics.LowerIsBetter, MinimumBaselineSampleSize: 5, MinimumObservationSampleSize: 5, MaximumRegressionPPM: 50_000},
		{MetricID: MetricPublishedFixedRate, Direction: analytics.HigherIsBetter, MinimumBaselineSampleSize: 5, MinimumObservationSampleSize: 5, MaximumRegressionPPM: 50_000},
		{MetricID: MetricPublishedOutcomeCoverage, Direction: analytics.HigherIsBetter, MinimumBaselineSampleSize: 5, MinimumObservationSampleSize: 5, MaximumRegressionPPM: 50_000},
		{MetricID: MetricRunCompleteRate, Direction: analytics.HigherIsBetter, MinimumBaselineSampleSize: 5, MinimumObservationSampleSize: 5, MaximumRegressionPPM: 50_000},
		{MetricID: MetricRunSuccessRate, Direction: analytics.HigherIsBetter, MinimumBaselineSampleSize: 5, MinimumObservationSampleSize: 5, MaximumRegressionPPM: 50_000},
	}
	return BuildRequest{SchemaVersion: RequestSchemaVersion, ObservationID: "promotion-observation-1", PlanID: "promotion-plan-1", BaselineSnapshotID: "baseline-snapshot", ObservationSnapshotID: "observation-snapshot", Policy: Policy{SchemaVersion: PolicySchemaVersion, PolicyID: "monitor-policy", Revision: "1", Rules: rules}, ObservedAt: at}
}

func monitorMutation(at time.Time) evaluation.Mutation {
	return evaluation.Mutation{IdempotencyKey: "observe-promotion-1", Actor: "promotion-operator", Roles: []evaluation.Role{evaluation.RolePromotionOperator, evaluation.RolePromotionApprover}, Audit: "compare immutable pre and post activation windows", At: at}
}

func monitorSnapshot(id string, start, end time.Time, configSHA string, runCount, succeeded, complete, published, known, fixed, adverse int) analyticsadapter.ProjectionSnapshot {
	snapshot := analyticsadapter.ProjectionSnapshot{SnapshotID: id, Scope: analyticsadapter.Scope{TenantID: "tenant-local", OrganizationID: "organization-local", RepositoryID: "repository-local"}, Window: analytics.TimeWindow{StartInclusive: start, EndExclusive: end}, GroupBy: []analytics.DimensionName{}, BuiltAt: end.Add(time.Minute), Facts: analytics.FactSet{Completeness: analytics.CompletenessComplete, IncompleteReasons: []string{}, ReviewRuns: []analytics.ReviewRunFact{}, Stages: []analytics.StageFact{}, ContextProviders: []analytics.ContextProviderFact{}, Findings: []analytics.FindingFunnelFact{}, FeedbackOutcomes: []analytics.FeedbackOutcomeFact{}, Experiments: []analytics.ExperimentFact{}, Repeatability: []analytics.RepeatabilityFact{}, ValueObservations: []analytics.ValueObservation{}, FindingLineages: []analytics.FindingLineageFact{}}, RunBindings: []analyticsadapter.RunSourceBinding{}}
	for index := 0; index < runCount; index++ {
		runID := "run-" + id + "-" + string(rune('a'+index))
		if id == "baseline-snapshot" && index == 0 {
			runID = "baseline-run-0"
		}
		status := analytics.RunStatusFailed
		if index < succeeded {
			status = analytics.RunStatusSucceeded
		}
		completeness := analytics.CompletenessPartial
		if index < complete {
			completeness = analytics.CompletenessComplete
		}
		snapshot.RunBindings = append(snapshot.RunBindings, analyticsadapter.RunSourceBinding{RunID: runID, ConfigSHA256: configSHA})
		snapshot.Facts.ReviewRuns = append(snapshot.Facts.ReviewRuns, analytics.ReviewRunFact{RunID: runID, Status: status, ResultCompleteness: completeness})
	}
	for index := 0; index < published; index++ {
		runID := snapshot.RunBindings[index%len(snapshot.RunBindings)].RunID
		findingID := "finding-" + string(rune('a'+index))
		snapshot.Facts.Findings = append(snapshot.Facts.Findings, analytics.FindingFunnelFact{RunID: runID, FindingID: findingID, Publication: analytics.PublicationPublished})
		outcome := analytics.OutcomeNoOutcome
		if index < fixed {
			outcome = analytics.OutcomeFixed
		} else if index < fixed+adverse {
			outcome = analytics.OutcomeEscaped
		} else if index < known {
			outcome = analytics.OutcomeRecurred
		}
		snapshot.Facts.FeedbackOutcomes = append(snapshot.Facts.FeedbackOutcomes, analytics.FeedbackOutcomeFact{RunID: runID, FindingID: findingID, Outcome: outcome})
	}
	return snapshot
}

func monitorTime(hour, minute int) time.Time {
	return time.Date(2026, 8, 26, hour, minute, 0, 0, time.UTC)
}
