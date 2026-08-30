package calibrationpromotion

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/abietic/argus/internal/calibration"
	"github.com/abietic/argus/internal/configrepo"
	"github.com/abietic/argus/internal/evaluation"
	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/store/local"
)

func TestPrepareIsResumableAndNeverPublishes(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	fixture.config.failValidationOnce = true
	service, err := New(fixture.store, fixture.calibration, fixture.config, fixture.evaluation, fixture.runs)
	if err != nil {
		t.Fatal(err)
	}
	request, mutation := fixture.request, fixture.mutation
	if _, err := service.Prepare(context.Background(), request, mutation); err == nil || !errors.Is(err, errInjected) {
		t.Fatalf("first Prepare() error = %v, want injected failure", err)
	}
	partial, err := service.Get(request.PlanID, evaluation.Access{Actor: "reader", Roles: []evaluation.Role{evaluation.RolePromotionOperator}})
	if err != nil || partial.Status != StatusPreparing {
		t.Fatalf("partial plan = %+v, err=%v", partial, err)
	}
	plan, err := service.Prepare(context.Background(), request, mutation)
	if err != nil {
		t.Fatalf("retry Prepare() error = %v", err)
	}
	if plan.Status != StatusPrepared || plan.ConfigStatus != configrepo.StatusValidated || plan.PromotionStatus != evaluation.PromotionRegistered {
		t.Fatalf("prepared plan = %+v", plan)
	}
	if fixture.config.publishCalls != 0 || plan.ProfileCandidateSHA256 != fixture.calibration.run.ProfileCandidate.SHA256 || plan.VariantBundleSHA256 == plan.BaselineBundleSHA256 {
		t.Fatalf("prepare published or lost exact binding: plan=%+v publish_calls=%d", plan, fixture.config.publishCalls)
	}
	if plan.PromotionVariant.ManagedBinding == nil || plan.PromotionVariant.ManagedBinding.ConfigRevisionSHA256 != plan.VariantConfigSHA256 {
		t.Fatalf("managed binding = %+v", plan.PromotionVariant.ManagedBinding)
	}
	if fixture.config.createCalls != 2 || fixture.config.validateCalls != 2 || fixture.evaluation.registerCalls != 1 {
		t.Fatalf("resume calls create=%d validate=%d register=%d", fixture.config.createCalls, fixture.config.validateCalls, fixture.evaluation.registerCalls)
	}
}

func TestTargetedRegressionRejectsConfigSubstitution(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	service, _ := New(fixture.store, fixture.calibration, fixture.config, fixture.evaluation, fixture.runs)
	plan, err := service.Prepare(context.Background(), fixture.request, fixture.mutation)
	if err != nil {
		t.Fatal(err)
	}
	schemaMutation := fixture.mutation
	schemaMutation.IdempotencyKey, schemaMutation.At = "gate-schema", fixture.mutation.At.Add(time.Minute)
	schema := validGate(plan, evaluation.GateSchemaContract)
	if _, err := service.RecordGate(context.Background(), GateRequest{SchemaVersion: GateRequestSchemaVersion, PlanID: plan.Request.PlanID, Result: schema}, schemaMutation); err != nil {
		t.Fatal(err)
	}
	fixture.evaluation.experiment = evaluation.ExperimentRun{
		ExperimentRunID: "experiment-1", Variable: runmodel.ReplayVariableFilterPolicy,
		Comparisons: []evaluation.ExperimentCaseComparison{{BaselineConfigSHA256: plan.BaselineBundleSHA256, VariantConfigSHA256: digest("substituted")}},
		Summary:     evaluation.ExperimentRunSummary{Cases: 1, Improved: 1},
	}
	targetMutation := fixture.mutation
	targetMutation.IdempotencyKey, targetMutation.At = "gate-targeted", fixture.mutation.At.Add(2*time.Minute)
	target := validGate(plan, evaluation.GateTargetedRegression)
	_, err = service.RecordGate(context.Background(), GateRequest{SchemaVersion: GateRequestSchemaVersion, PlanID: plan.Request.PlanID, Result: target, ExperimentRunID: "experiment-1"}, targetMutation)
	if !errors.Is(err, ErrEvidenceMismatch) {
		t.Fatalf("RecordGate() error = %v", err)
	}
	if fixture.evaluation.gateCalls != 1 {
		t.Fatalf("managed gate was recorded despite substituted config")
	}
}

func TestFailedGateIsDurableAndCannotActivate(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	service, _ := New(fixture.store, fixture.calibration, fixture.config, fixture.evaluation, fixture.runs)
	plan, err := service.Prepare(context.Background(), fixture.request, fixture.mutation)
	if err != nil {
		t.Fatal(err)
	}
	result := validGate(plan, evaluation.GateSchemaContract)
	result.Outcome, result.Evidence.ChecksPassed = evaluation.GateFail, false
	mutation := fixture.mutation
	mutation.IdempotencyKey, mutation.At = "gate-failed", mutation.At.Add(time.Minute)
	failed, err := service.RecordGate(context.Background(), GateRequest{SchemaVersion: GateRequestSchemaVersion, PlanID: plan.Request.PlanID, Result: result}, mutation)
	if err != nil || failed.Status != StatusFailed || failed.PromotionStatus != evaluation.PromotionFailed || failed.NextGate != nil {
		t.Fatalf("failed gate plan=%+v err=%v", failed, err)
	}
	loaded, err := service.Get(plan.Request.PlanID, evaluation.Access{Actor: "reader", Roles: []evaluation.Role{evaluation.RolePromotionOperator}})
	if err != nil || loaded.Status != StatusFailed {
		t.Fatalf("loaded failed plan=%+v err=%v", loaded, err)
	}
	activation := fixture.mutation
	activation.IdempotencyKey, activation.At = "activate-failed", activation.At.Add(time.Hour)
	if _, err := service.Activate(context.Background(), plan.Request.PlanID, configrepo.Rollout{Percentage: 100}, activation); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("Activate failed plan error=%v", err)
	}
}

func TestHoldoutRejectsReviewRunWithDifferentConfig(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	service, _ := New(fixture.store, fixture.calibration, fixture.config, fixture.evaluation, fixture.runs)
	plan, err := service.Prepare(context.Background(), fixture.request, fixture.mutation)
	if err != nil {
		t.Fatal(err)
	}
	mutation := fixture.mutation
	mutation.IdempotencyKey, mutation.At = "schema-holdout", mutation.At.Add(time.Minute)
	if _, err := service.RecordGate(context.Background(), GateRequest{SchemaVersion: GateRequestSchemaVersion, PlanID: plan.Request.PlanID, Result: validGate(plan, evaluation.GateSchemaContract)}, mutation); err != nil {
		t.Fatal(err)
	}
	fixture.evaluation.experiment = evaluation.ExperimentRun{ExperimentRunID: "experiment-1", Variable: runmodel.ReplayVariableFilterPolicy, Comparisons: []evaluation.ExperimentCaseComparison{{BaselineConfigSHA256: plan.BaselineBundleSHA256, VariantConfigSHA256: plan.VariantBundleSHA256}}, Summary: evaluation.ExperimentRunSummary{Cases: 1, Improved: 1}}
	mutation.IdempotencyKey, mutation.At = "target-holdout", mutation.At.Add(time.Minute)
	if _, err := service.RecordGate(context.Background(), GateRequest{SchemaVersion: GateRequestSchemaVersion, PlanID: plan.Request.PlanID, Result: validGate(plan, evaluation.GateTargetedRegression), ExperimentRunID: "experiment-1"}, mutation); err != nil {
		t.Fatal(err)
	}
	fixture.evaluation.evaluationRun = evaluation.EvaluationRun{EvaluationRunID: "holdout-evaluation-1", Results: []evaluation.EvaluationCaseResult{{CaseID: "holdout-case-1", ReviewRunID: fixture.runs.run.RunID}}}
	holdout := validGate(plan, evaluation.GateFixedHoldout)
	holdout.Evidence.EvaluationRunID, holdout.Evidence.HoldoutCaseIDs = "holdout-evaluation-1", []string{"holdout-case-1"}
	mutation.IdempotencyKey, mutation.At, mutation.Actor = "fixed-holdout", mutation.At.Add(time.Minute), "independent-holdout-runner"
	mutation.Roles = []evaluation.Role{evaluation.RoleDatasetCurator, evaluation.RoleHoldoutRunner, evaluation.RolePromotionOperator}
	_, err = service.RecordGate(context.Background(), GateRequest{SchemaVersion: GateRequestSchemaVersion, PlanID: plan.Request.PlanID, Result: holdout}, mutation)
	if !errors.Is(err, ErrEvidenceMismatch) {
		t.Fatalf("fixed holdout error = %v", err)
	}
	if fixture.evaluation.gateCalls != 2 {
		t.Fatalf("invalid holdout reached promotion ledger")
	}
}

func TestActivateAndRollbackResumeAcrossPartialWrites(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t)
	service, _ := New(fixture.store, fixture.calibration, fixture.config, fixture.evaluation, fixture.runs)
	plan, err := service.Prepare(context.Background(), fixture.request, fixture.mutation)
	if err != nil {
		t.Fatal(err)
	}
	plan.Status, plan.PromotionStatus, plan.NextGate = StatusGatesPassed, evaluation.PromotionActive, nil
	fixture.evaluation.promotion.Status, fixture.evaluation.promotion.NextGate = evaluation.PromotionActive, nil
	gateMutation := fixture.mutation
	gateMutation.IdempotencyKey, gateMutation.At = "all-gates-test", gateMutation.At.Add(time.Hour)
	plan.UpdatedAt, plan.UpdatedBy = gateMutation.At, gateMutation.Actor
	if err := service.appendPlan("all-gates-test:event", "gate_recorded", plan, gateMutation); err != nil {
		t.Fatal(err)
	}
	fixture.config.failPublishOnce = true
	activation := fixture.mutation
	activation.IdempotencyKey, activation.At = "activate-1", activation.At.Add(2*time.Hour)
	rollout := configrepo.Rollout{Percentage: 10, Seed: "stable-seed"}
	if _, err := service.Activate(context.Background(), plan.Request.PlanID, rollout, activation); !errors.Is(err, errInjected) {
		t.Fatalf("first Activate() error = %v", err)
	}
	partial, _ := service.Get(plan.Request.PlanID, evaluation.Access{Actor: "reader", Roles: []evaluation.Role{evaluation.RolePromotionOperator}})
	if partial.Status != StatusActivating {
		t.Fatalf("activation partial status = %s", partial.Status)
	}
	active, err := service.Activate(context.Background(), plan.Request.PlanID, rollout, activation)
	if err != nil || active.Status != StatusActive {
		t.Fatalf("Activate retry = %+v, %v", active, err)
	}
	fixture.evaluation.failRollbackOnce = true
	rollback := fixture.mutation
	rollback.IdempotencyKey, rollback.At = "rollback-1", rollback.At.Add(3*time.Hour)
	if _, err := service.Rollback(context.Background(), plan.Request.PlanID, rollback); !errors.Is(err, errInjected) {
		t.Fatalf("first Rollback() error = %v", err)
	}
	partial, _ = service.Get(plan.Request.PlanID, evaluation.Access{Actor: "reader", Roles: []evaluation.Role{evaluation.RolePromotionOperator}})
	if partial.Status != StatusRollbackPending {
		t.Fatalf("rollback partial status = %s", partial.Status)
	}
	rolledBack, err := service.Rollback(context.Background(), plan.Request.PlanID, rollback)
	if err != nil || rolledBack.Status != StatusRolledBack || rolledBack.ConfigStatus != configrepo.StatusRolledBack || rolledBack.PromotionStatus != evaluation.PromotionRolledBack {
		t.Fatalf("Rollback retry = %+v, %v", rolledBack, err)
	}
}

func validGate(plan Plan, gate evaluation.PromotionGate) evaluation.GateResult {
	return evaluation.GateResult{SchemaVersion: evaluation.GateResultSchemaVersion, VariantID: plan.PromotionVariant.VariantID, Gate: gate, Outcome: evaluation.GatePass, Evidence: evaluation.GateEvidence{Refs: []string{"artifact://test/evidence"}, Basis: evaluation.EvidenceDeterministic, ChecksPassed: true, HoldoutCaseIDs: []string{}}, Summary: "verified"}
}

var errInjected = errors.New("injected validation failure")

type fixture struct {
	store       *local.Store
	calibration *fakeCalibration
	config      *fakeConfig
	evaluation  *fakeEvaluation
	runs        *fakeRuns
	request     PrepareRequest
	mutation    evaluation.Mutation
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	root := filepath.Clean(filepath.Join("..", "..", "examples"))
	baseData, err := os.ReadFile(filepath.Join(root, "config-revision.json"))
	if err != nil {
		t.Fatal(err)
	}
	base, err := reviewconfig.DecodeRevision(baseData)
	if err != nil {
		t.Fatal(err)
	}
	governanceData, err := os.ReadFile(filepath.Join(root, "config-revision.finding-governance.json"))
	if err != nil {
		t.Fatal(err)
	}
	baselineRevision, err := reviewconfig.DecodeRevision(governanceData)
	if err != nil {
		t.Fatal(err)
	}
	resolutionContext := reviewconfig.ResolutionContext{TenantID: "local", OrganizationID: "local", RepositoryID: "argus", Path: "internal/review.go", InvocationID: "review-1"}
	bundle, err := reviewconfig.Resolve(resolutionContext, []reviewconfig.Revision{base, baselineRevision})
	if err != nil {
		t.Fatal(err)
	}
	bundleData, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	baseSHA, _ := reviewconfig.DigestRevision(base)
	baselineSHA, _ := reviewconfig.DigestRevision(baselineRevision)
	receipt, err := reviewconfig.NewConfigResolutionReceipt(bundle, []reviewconfig.PublishedRevisionBinding{
		{Source: bundle.AppliedRevisions[0], RevisionSHA256: baseSHA, PublishEventID: "publish-base", PublishSequence: 1, PublishedAt: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), AssignmentSHA256: digest("assign-base")},
		{Source: bundle.AppliedRevisions[1], RevisionSHA256: baselineSHA, PublishEventID: "publish-governance", PublishSequence: 2, PublishedAt: time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC), AssignmentSHA256: digest("assign-governance")},
	})
	if err != nil {
		t.Fatal(err)
	}
	baselineBinding := receipt.Revisions[1]
	profile := bundle.FindingGovernance.CalibrationProfile
	points := append([]reviewconfig.CalibrationPoint(nil), profile.Points...)
	points[len(points)-1].ConfidencePPM--
	profile, err = reviewconfig.SealCalibrationProfile(profile.ID, "calibrated-2", points)
	if err != nil {
		t.Fatal(err)
	}
	calibrationRun := calibration.Run{RunID: "calibration-1", SHA256: digest("calibration-run"), Report: calibration.Report{Passed: true, SHA256: digest("report")}, ProfileCandidate: calibration.ProfileCandidate{CandidateID: "candidate-1", SHA256: digest("candidate"), Status: "gate_passed", AutoPublished: false, Profile: profile}, CreatedBy: "fit-operator"}
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config := &fakeConfig{bundle: bundle, receipt: receipt, records: map[string]configrepo.Record{configKey(baselineRevision.ID, baselineRevision.Revision): {Revision: baselineRevision, SHA256: baselineBinding.RevisionSHA256, Status: configrepo.StatusPublished}}}
	run := runmodel.ReviewRun{RunID: "review-1", Status: runmodel.RunStatusSucceeded, ExecutionSnapshotID: "snapshot-1"}
	sum := sha256.Sum256(bundleData)
	runs := &fakeRuns{run: run, snapshot: runmodel.ExecutionSnapshot{ExecutionSnapshotID: "snapshot-1", ConfigBundleRef: runmodel.ArtifactRef{URI: "artifact://test/config", SHA256: hex.EncodeToString(sum[:]), SizeBytes: int64(len(bundleData)), Contract: runmodel.ContractConfigBundle}}, artifacts: map[string][]byte{"artifact://test/config": bundleData}}
	now := time.Date(2026, 8, 26, 10, 0, 0, 0, time.UTC)
	request := PrepareRequest{SchemaVersion: PrepareRequestSchemaVersion, PlanID: "plan-1", CalibrationRunID: calibrationRun.RunID, ExpectedCalibrationRunSHA256: calibrationRun.SHA256, BaselineReviewRunID: run.RunID, BaselineConfigRevisionID: baselineRevision.ID, BaselineConfigRevision: baselineRevision.Revision, VariantConfigRevisionID: "calibration-filter", VariantConfigRevision: "2", PromotionVariantID: "promotion-calibration-2", PromotionPolicyRevision: "policy-1", Owner: "promotion-owner", CreatedAt: now}
	mutation := evaluation.Mutation{IdempotencyKey: "prepare-1", Actor: "prepare-operator", Roles: []evaluation.Role{evaluation.RoleDatasetCurator, evaluation.RolePromotionOperator}, Audit: "prepare calibration candidate", At: now}
	eval := &fakeEvaluation{}
	return fixture{store: store, calibration: &fakeCalibration{run: calibrationRun}, config: config, evaluation: eval, runs: runs, request: request, mutation: mutation}
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func configKey(id, revision string) string { return id + "\x00" + revision }

type fakeCalibration struct{ run calibration.Run }

func (fake *fakeCalibration) Get(id string, _ evaluation.Access) (calibration.Run, error) {
	if id != fake.run.RunID {
		return calibration.Run{}, calibration.ErrNotFound
	}
	return fake.run, nil
}

type fakeConfig struct {
	bundle                                   reviewconfig.ConfigBundle
	receipt                                  reviewconfig.ConfigResolutionReceipt
	records                                  map[string]configrepo.Record
	failValidationOnce                       bool
	failPublishOnce                          bool
	createCalls, validateCalls, publishCalls int
}

func (fake *fakeConfig) Create(_ context.Context, revision reviewconfig.Revision, _ configrepo.Mutation) (configrepo.Record, error) {
	fake.createCalls++
	key := configKey(revision.ID, revision.Revision)
	if record, ok := fake.records[key]; ok {
		return record, nil
	}
	sha, _ := reviewconfig.DigestRevision(revision)
	record := configrepo.Record{Revision: revision, SHA256: sha, Status: configrepo.StatusDraft}
	fake.records[key] = record
	return record, nil
}
func (fake *fakeConfig) ValidateRevision(_ context.Context, id, revision string, _ configrepo.Mutation) (configrepo.Record, error) {
	fake.validateCalls++
	if fake.failValidationOnce {
		fake.failValidationOnce = false
		return configrepo.Record{}, errInjected
	}
	key := configKey(id, revision)
	record := fake.records[key]
	if record.Status == configrepo.StatusDraft {
		record.Status = configrepo.StatusValidated
		fake.records[key] = record
	}
	return record, nil
}
func (fake *fakeConfig) Publish(_ context.Context, id, revision string, _ configrepo.Rollout, _ configrepo.Mutation) (configrepo.Record, error) {
	fake.publishCalls++
	if fake.failPublishOnce {
		fake.failPublishOnce = false
		return configrepo.Record{}, errInjected
	}
	record := fake.records[configKey(id, revision)]
	record.Status = configrepo.StatusPublished
	fake.records[configKey(id, revision)] = record
	return record, nil
}
func (fake *fakeConfig) Rollback(_ context.Context, id, revision string, _ configrepo.Mutation) (configrepo.Record, error) {
	record := fake.records[configKey(id, revision)]
	record.Status = configrepo.StatusRolledBack
	return record, nil
}
func (fake *fakeConfig) Get(id, revision string) (configrepo.Record, error) {
	record, ok := fake.records[configKey(id, revision)]
	if !ok {
		return configrepo.Record{}, configrepo.ErrNotFound
	}
	return record, nil
}
func (fake *fakeConfig) ResolvePublishedWithReceipt(context.Context, reviewconfig.ResolutionContext) (reviewconfig.ConfigBundle, reviewconfig.ConfigResolutionReceipt, error) {
	return fake.bundle, fake.receipt, nil
}

type fakeEvaluation struct {
	promotion                evaluation.PromotionRecord
	experiment               evaluation.ExperimentRun
	evaluationRun            evaluation.EvaluationRun
	failRollbackOnce         bool
	registerCalls, gateCalls int
}

func (fake *fakeEvaluation) RegisterManagedPromotion(_ context.Context, variant evaluation.PromotionVariant, _ evaluation.Mutation) (evaluation.PromotionRecord, error) {
	fake.registerCalls++
	if fake.promotion.Variant.VariantID == "" {
		next := evaluation.GateSchemaContract
		fake.promotion = evaluation.PromotionRecord{Variant: variant, Status: evaluation.PromotionRegistered, NextGate: &next}
	}
	return fake.promotion, nil
}
func (fake *fakeEvaluation) RecordManagedGate(_ context.Context, result evaluation.GateResult, _ evaluation.PromotionManagementBinding, _ evaluation.Mutation) (evaluation.PromotionRecord, error) {
	fake.gateCalls++
	if result.Outcome == evaluation.GateFail {
		fake.promotion.Status, fake.promotion.NextGate = evaluation.PromotionFailed, nil
		return fake.promotion, nil
	}
	if result.Outcome == evaluation.GateInconclusive {
		fake.promotion.Status, fake.promotion.NextGate = evaluation.PromotionInconclusive, nil
		return fake.promotion, nil
	}
	fake.promotion.Status = evaluation.PromotionInProgress
	next := evaluation.GateTargetedRegression
	if result.Gate == evaluation.GateTargetedRegression {
		next = evaluation.GateFixedHoldout
	}
	fake.promotion.NextGate = &next
	return fake.promotion, nil
}
func (fake *fakeEvaluation) RollbackManagedPromotion(context.Context, string, evaluation.PromotionManagementBinding, evaluation.Mutation) (evaluation.PromotionRecord, error) {
	if fake.failRollbackOnce {
		fake.failRollbackOnce = false
		return evaluation.PromotionRecord{}, errInjected
	}
	fake.promotion.Status = evaluation.PromotionRolledBack
	return fake.promotion, nil
}
func (fake *fakeEvaluation) GetPromotion(string, evaluation.Access) (evaluation.PromotionRecord, error) {
	return fake.promotion, nil
}
func (fake *fakeEvaluation) GetExperimentRun(string, evaluation.Access) (evaluation.ExperimentRun, error) {
	return fake.experiment, nil
}
func (fake *fakeEvaluation) GetEvaluationRun(string, evaluation.Access) (evaluation.EvaluationRun, error) {
	return fake.evaluationRun, nil
}

type fakeRuns struct {
	run       runmodel.ReviewRun
	snapshot  runmodel.ExecutionSnapshot
	artifacts map[string][]byte
}

func (fake *fakeRuns) LoadRun(id string) (runmodel.ReviewRun, error) {
	if id != fake.run.RunID {
		return runmodel.ReviewRun{}, os.ErrNotExist
	}
	return fake.run, nil
}
func (fake *fakeRuns) LoadExecutionSnapshot(id string) (runmodel.ExecutionSnapshot, error) {
	if id != fake.snapshot.ExecutionSnapshotID {
		return runmodel.ExecutionSnapshot{}, os.ErrNotExist
	}
	return fake.snapshot, nil
}
func (fake *fakeRuns) ReadArtifact(ref runmodel.ArtifactRef) ([]byte, error) {
	data, ok := fake.artifacts[ref.URI]
	if !ok {
		return nil, os.ErrNotExist
	}
	return data, nil
}
