package normalizationpromotion

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/abietic/argus/internal/configrepo"
	"github.com/abietic/argus/internal/evaluation"
	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/store/local"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

type promotionTestRuns struct {
	run       runmodel.ReviewRun
	snapshot  runmodel.ExecutionSnapshot
	artifacts map[string][]byte
}

func (runs *promotionTestRuns) PutJSONArtifact(contract string, value any) (runmodel.ArtifactRef, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return runmodel.ArtifactRef{}, err
	}
	return runs.put(contract, data), nil
}

func (runs *promotionTestRuns) put(contract string, data []byte) runmodel.ArtifactRef {
	digest := sha256.Sum256(data)
	encoded := hex.EncodeToString(digest[:])
	ref := runmodel.ArtifactRef{
		URI: "artifact://local/sha256/" + encoded, SHA256: encoded,
		SizeBytes: int64(len(data)), Contract: contract,
	}
	if runs.artifacts == nil {
		runs.artifacts = make(map[string][]byte)
	}
	runs.artifacts[ref.URI] = append([]byte(nil), data...)
	return ref
}

func (runs *promotionTestRuns) ReadArtifact(ref runmodel.ArtifactRef) ([]byte, error) {
	data, ok := runs.artifacts[ref.URI]
	if !ok {
		return nil, fmt.Errorf("artifact %q not found", ref.URI)
	}
	return append([]byte(nil), data...), nil
}

func (runs *promotionTestRuns) LoadRun(id string) (runmodel.ReviewRun, error) {
	if id != runs.run.RunID {
		return runmodel.ReviewRun{}, fmt.Errorf("run %q not found", id)
	}
	return runs.run, nil
}

func (runs *promotionTestRuns) LoadExecutionSnapshot(id string) (runmodel.ExecutionSnapshot, error) {
	if id != runs.snapshot.ExecutionSnapshotID {
		return runmodel.ExecutionSnapshot{}, fmt.Errorf("snapshot %q not found", id)
	}
	return runs.snapshot, nil
}

type unusedQualityVerifier struct{}

func (unusedQualityVerifier) VerifyQuality(context.Context, runmodel.ArtifactRef, evaluation.Access) (evaluation.NormalizationQualityRun, error) {
	return evaluation.NormalizationQualityRun{}, fmt.Errorf("quality verification is not expected")
}

func TestPrepareDerivesValidatedCandidateFromExactActiveBaseline(t *testing.T) {
	now := time.Date(2026, time.August, 27, 9, 0, 0, 0, time.UTC)
	state, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	evaluations, err := evaluation.New(state)
	if err != nil {
		t.Fatal(err)
	}
	configState, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	configs, err := configrepo.New(configState)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", "config-revision.agent-review.json"))
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := reviewconfig.DecodeRevision(data)
	if err != nil {
		t.Fatal(err)
	}
	baseline.ID = "normalization-config"
	baseline.Revision = "v0"
	baseline.Patch.AgentReview.Normalization.Revision = contractsv1alpha1.CandidateNormalizationRevisionV0
	configMutation := func(key string, offset time.Duration) configrepo.Mutation {
		return configrepo.Mutation{IdempotencyKey: key, Actor: "config-operator", Audit: key, At: now.Add(offset)}
	}
	if _, err := configs.Create(t.Context(), baseline, configMutation("create-baseline", -3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := configs.ValidateRevision(t.Context(), baseline.ID, baseline.Revision, configMutation("validate-baseline", -2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := configs.Publish(t.Context(), baseline.ID, baseline.Revision, configrepo.Rollout{Percentage: 100}, configMutation("publish-baseline", -time.Minute)); err != nil {
		t.Fatal(err)
	}
	context := reviewconfig.ResolutionContext{
		TenantID: "tenant-1", OrganizationID: "organization-1",
		RepositoryID: "repository-1", Path: "internal/review.go", InvocationID: "invocation-1",
	}
	bundle, _, err := configs.ResolvePublishedWithReceipt(t.Context(), context)
	if err != nil {
		t.Fatal(err)
	}
	bundleData, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	runs := &promotionTestRuns{}
	bundleRef := runs.put(runmodel.ContractConfigBundle, bundleData)
	runs.run = runmodel.ReviewRun{
		RunID: "baseline-review-run", Status: runmodel.RunStatusSucceeded,
		ExecutionSnapshotID: "baseline-execution-snapshot",
	}
	runs.snapshot = runmodel.ExecutionSnapshot{
		ExecutionSnapshotID: runs.run.ExecutionSnapshotID, ConfigBundleRef: bundleRef,
	}
	service, err := newService(evaluations, runs, unusedQualityVerifier{}, configs)
	if err != nil {
		t.Fatal(err)
	}
	request := validPrepareRequest(now)
	request.BaselineReviewRunID = runs.run.RunID
	request.BaselineConfigRevisionID = baseline.ID
	request.BaselineConfigRevision = baseline.Revision
	request.VariantConfigRevisionID = baseline.ID
	request.VariantConfigRevision = "v2"
	request.RollbackImplementation.SHA256 = baseline.Patch.AgentReview.Agent.SHA256
	request.VariantImplementation.SHA256 = baseline.Patch.AgentReview.Agent.SHA256
	preparation, err := service.Prepare(t.Context(), request, evaluation.Mutation{
		IdempotencyKey: "prepare-normalization-v2", Actor: "promotion-operator",
		Roles: []evaluation.Role{evaluation.RolePromotionOperator}, Audit: "prepare exact normalization candidate", At: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := preparation.Validate(); err != nil {
		t.Fatal(err)
	}
	if preparation.ConfigStatus != configrepo.StatusValidated ||
		preparation.VariantConfig.Patch.AgentReview.Normalization.Revision != contractsv1alpha1.CandidateNormalizationRevisionV1 {
		t.Fatalf("prepared config = %+v", preparation.VariantConfig)
	}
	if preparation.Promotion.NextGate == nil || *preparation.Promotion.NextGate != evaluation.GateTargetedRegression {
		t.Fatalf("prepared promotion = %+v", preparation.Promotion)
	}
}

func TestOperationalPromotionRunsCanaryActivationAndRollbackAgainstConfigLedger(t *testing.T) {
	now := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	state, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	evaluations, err := evaluation.New(state)
	if err != nil {
		t.Fatal(err)
	}
	configState, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	configs, err := configrepo.New(configState)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", "config-revision.agent-review.json"))
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := reviewconfig.DecodeRevision(data)
	if err != nil {
		t.Fatal(err)
	}
	baseline.ID, baseline.Revision = "normalization-lifecycle", "v0"
	baseline.Patch.AgentReview.Normalization.Revision = contractsv1alpha1.CandidateNormalizationRevisionV0
	configMutation := func(key string, at time.Time) configrepo.Mutation {
		return configrepo.Mutation{IdempotencyKey: key, Actor: "config-operator", Audit: key, At: at}
	}
	if _, err = configs.Create(t.Context(), baseline, configMutation("lifecycle-create", now.Add(-3*time.Minute))); err != nil {
		t.Fatal(err)
	}
	if _, err = configs.ValidateRevision(t.Context(), baseline.ID, baseline.Revision, configMutation("lifecycle-validate", now.Add(-2*time.Minute))); err != nil {
		t.Fatal(err)
	}
	if _, err = configs.Publish(t.Context(), baseline.ID, baseline.Revision, configrepo.Rollout{Percentage: 100}, configMutation("lifecycle-publish", now.Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	resolutionContext := reviewconfig.ResolutionContext{TenantID: "tenant-1", OrganizationID: "organization-1", RepositoryID: "repository-1", Path: "internal/review.go", InvocationID: "invocation-1"}
	baselineBundle, _, err := configs.ResolvePublishedWithReceipt(t.Context(), resolutionContext)
	if err != nil {
		t.Fatal(err)
	}
	baselineData, _ := json.Marshal(baselineBundle)
	runs := &promotionTestRuns{}
	baselineRef := runs.put(runmodel.ContractConfigBundle, baselineData)
	runs.run = runmodel.ReviewRun{RunID: "lifecycle-baseline-run", Status: runmodel.RunStatusSucceeded, ExecutionSnapshotID: "lifecycle-baseline-snapshot"}
	runs.snapshot = runmodel.ExecutionSnapshot{ExecutionSnapshotID: runs.run.ExecutionSnapshotID, ConfigBundleRef: baselineRef}
	service, err := newService(evaluations, runs, unusedQualityVerifier{}, configs)
	if err != nil {
		t.Fatal(err)
	}
	request := validPrepareRequest(now)
	request.PromotionVariantID = "normalization-lifecycle-promotion"
	request.BaselineReviewRunID = runs.run.RunID
	request.BaselineConfigRevisionID, request.BaselineConfigRevision = baseline.ID, baseline.Revision
	request.VariantConfigRevisionID, request.VariantConfigRevision = baseline.ID, "v1"
	request.RollbackImplementation.SHA256 = baseline.Patch.AgentReview.Agent.SHA256
	request.VariantImplementation.SHA256 = baseline.Patch.AgentReview.Agent.SHA256
	preparation, err := service.Prepare(t.Context(), request, evaluation.Mutation{IdempotencyKey: "lifecycle-prepare", Actor: "operator", Roles: []evaluation.Role{evaluation.RolePromotionOperator}, Audit: "prepare lifecycle", At: now})
	if err != nil {
		t.Fatal(err)
	}
	binding := *preparation.Promotion.Variant.ManagedBinding
	passQuality := func(gate evaluation.PromotionGate, at time.Time, roles []evaluation.Role) {
		evidence := evaluation.GateEvidence{Refs: []string{preparation.PolicyRef.URI}, Basis: evaluation.EvidenceHumanCalibrated, ChecksPassed: true, NormalizationQualityRunID: "quality-" + string(gate), HoldoutCaseIDs: []string{}}
		if gate == evaluation.GateFixedHoldout {
			evidence.HoldoutCaseIDs = []string{"holdout-case-1"}
		}
		_, gateErr := evaluations.RecordManagedGate(t.Context(), evaluation.GateResult{SchemaVersion: evaluation.GateResultSchemaVersion, VariantID: request.PromotionVariantID, Gate: gate, Outcome: evaluation.GatePass, Evidence: evidence, Summary: "quality passed"}, binding, evaluation.Mutation{IdempotencyKey: "lifecycle-" + string(gate), Actor: "operator", Roles: roles, Audit: "quality gate", At: at})
		if gateErr != nil {
			t.Fatal(gateErr)
		}
	}
	passQuality(evaluation.GateTargetedRegression, now.Add(time.Minute), []evaluation.Role{evaluation.RolePromotionOperator})
	passQuality(evaluation.GateFixedHoldout, now.Add(2*time.Minute), []evaluation.Role{evaluation.RoleHoldoutRunner, evaluation.RolePromotionOperator})
	variantBundle, err := reviewconfig.Resolve(resolutionContext, []reviewconfig.Revision{preparation.VariantConfig})
	if err != nil || variantBundle.SHA256 != preparation.VariantBundleSHA256 {
		t.Fatalf("variant bundle: %v %+v", err, variantBundle)
	}
	variantData, _ := json.Marshal(variantBundle)
	variantRef := runs.put(runmodel.ContractConfigBundle, variantData)
	shadowCompleted := now.Add(3 * time.Minute)
	runs.run = runmodel.ReviewRun{RunID: "shadow-run-1", Status: runmodel.RunStatusSucceeded, ExecutionSnapshotID: "shadow-snapshot", CompletedAt: &shadowCompleted}
	runs.snapshot = runmodel.ExecutionSnapshot{ExecutionSnapshotID: runs.run.ExecutionSnapshotID, ConfigBundleRef: variantRef}
	shadow, err := service.RecordOperationalGate(t.Context(), OperationalGateRequest{SchemaVersion: OperationalGateRequestSchemaVersion, VariantID: request.PromotionVariantID, Gate: evaluation.GateShadowTraffic, PolicyRef: preparation.PolicyRef, ReviewRunIDs: []string{runs.run.RunID}, SafetyEventCount: 0, EvaluatedAt: now.Add(4 * time.Minute)}, evaluation.Mutation{IdempotencyKey: "lifecycle-shadow", Actor: "operator", Roles: []evaluation.Role{evaluation.RolePromotionOperator}, Audit: "shadow", At: now.Add(4 * time.Minute)})
	if err != nil || shadow.Result.Outcome != evaluation.GatePass {
		t.Fatalf("shadow = %+v err=%v", shadow, err)
	}
	seed := canarySeedForContext(resolutionContext, preparation.VariantConfig, request.Policy.CanaryPercentage)
	canaryAt := now.Add(5 * time.Minute)
	canaryRollout := configrepo.Rollout{Percentage: request.Policy.CanaryPercentage, Seed: seed}
	canaryMutation := evaluation.Mutation{IdempotencyKey: "lifecycle-canary-start", Actor: "operator", Roles: []evaluation.Role{evaluation.RolePromotionOperator}, Audit: "start canary", At: canaryAt}
	canary, err := service.StartCanary(t.Context(), request.PromotionVariantID, preparation.PolicyRef, canaryRollout, canaryMutation)
	if err != nil || canary.Config.Status != configrepo.StatusPublished {
		t.Fatalf("start canary = %+v err=%v", canary, err)
	}
	if _, err := service.StartCanary(t.Context(), request.PromotionVariantID, preparation.PolicyRef, canaryRollout, canaryMutation); err != nil {
		t.Fatalf("retry canary after committed outcome: %v", err)
	}
	canaryCompleted := canaryAt.Add(time.Minute)
	runs.run = runmodel.ReviewRun{RunID: "canary-run-1", Status: runmodel.RunStatusSucceeded, ExecutionSnapshotID: "canary-snapshot", CompletedAt: &canaryCompleted}
	runs.snapshot = runmodel.ExecutionSnapshot{ExecutionSnapshotID: runs.run.ExecutionSnapshotID, ConfigBundleRef: variantRef}
	canaryGate, err := service.RecordOperationalGate(t.Context(), OperationalGateRequest{SchemaVersion: OperationalGateRequestSchemaVersion, VariantID: request.PromotionVariantID, Gate: evaluation.GateCanary, PolicyRef: preparation.PolicyRef, ReviewRunIDs: []string{runs.run.RunID}, SafetyEventCount: 0, EvaluatedAt: canaryAt.Add(2 * time.Minute)}, evaluation.Mutation{IdempotencyKey: "lifecycle-canary-gate", Actor: "operator", Roles: []evaluation.Role{evaluation.RolePromotionOperator}, Audit: "canary gate", At: canaryAt.Add(2 * time.Minute)})
	if err != nil || canaryGate.Result.Outcome != evaluation.GatePass {
		t.Fatalf("canary gate = %+v err=%v", canaryGate, err)
	}
	authorizationAt := canaryAt.Add(3 * time.Minute)
	authorization := &evaluation.PromotionAuthorization{Kind: "human", AuthorizedBy: "approver"}
	_, err = service.RecordOperationalGate(t.Context(), OperationalGateRequest{SchemaVersion: OperationalGateRequestSchemaVersion, VariantID: request.PromotionVariantID, Gate: evaluation.GateAuthorization, PolicyRef: preparation.PolicyRef, ReviewRunIDs: []string{}, Authorization: authorization, EvaluatedAt: authorizationAt}, evaluation.Mutation{IdempotencyKey: "lifecycle-authorize", Actor: "approver", Roles: []evaluation.Role{evaluation.RolePromotionApprover}, Audit: "authorize", At: authorizationAt})
	if err != nil {
		t.Fatal(err)
	}
	monitorAt := canaryAt.Add(4 * time.Minute)
	monitor, err := service.RecordOperationalGate(t.Context(), OperationalGateRequest{SchemaVersion: OperationalGateRequestSchemaVersion, VariantID: request.PromotionVariantID, Gate: evaluation.GateRollbackMonitor, PolicyRef: preparation.PolicyRef, ReviewRunIDs: []string{}, EvaluatedAt: monitorAt}, evaluation.Mutation{IdempotencyKey: "lifecycle-monitor", Actor: "operator", Roles: []evaluation.Role{evaluation.RolePromotionOperator}, Audit: "verify rollback", At: monitorAt})
	if err != nil || monitor.Promotion.Status != evaluation.PromotionActive {
		t.Fatalf("monitor = %+v err=%v", monitor, err)
	}
	activationMutation := evaluation.Mutation{IdempotencyKey: "lifecycle-activate", Actor: "operator", Roles: []evaluation.Role{evaluation.RolePromotionOperator}, Audit: "activate", At: canaryAt.Add(5 * time.Minute)}
	activated, err := service.Activate(t.Context(), request.PromotionVariantID, activationMutation)
	if err != nil || activated.Rollout == nil || activated.Rollout.Percentage != 100 {
		t.Fatalf("activate = %+v err=%v", activated, err)
	}
	if _, err := service.Activate(t.Context(), request.PromotionVariantID, activationMutation); err != nil {
		t.Fatalf("retry activation after committed outcome: %v", err)
	}
	rolledBack, err := service.Rollback(t.Context(), request.PromotionVariantID, evaluation.Mutation{IdempotencyKey: "lifecycle-rollback", Actor: "operator", Roles: []evaluation.Role{evaluation.RolePromotionOperator}, Audit: "rollback", At: canaryAt.Add(6 * time.Minute)})
	if err != nil || rolledBack.Config.Status != configrepo.StatusRolledBack || rolledBack.Promotion.Status != evaluation.PromotionRolledBack {
		t.Fatalf("rollback = %+v err=%v", rolledBack, err)
	}
}

func canarySeedForContext(context reviewconfig.ResolutionContext, revision reviewconfig.Revision, percentage int) string {
	selector := string(revision.Scope) + "\x00*"
	for index := 0; ; index++ {
		seed := fmt.Sprintf("seed-%d", index)
		value := strings.Join([]string{context.TenantID, context.OrganizationID, context.RepositoryID, context.Path, context.InvocationID, selector, revision.ID, revision.Revision, seed}, "\x00")
		digest := sha256.Sum256([]byte(value))
		if int(binary.BigEndian.Uint64(digest[:8])%100) < percentage {
			return seed
		}
	}
}

func TestPrepareRequestAcceptsAnyImplementedExactNormalizationSelector(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 27, 7, 0, 0, 0, time.UTC)
	request := validPrepareRequest(now)
	if err := request.Validate(); err != nil {
		t.Fatalf("valid v1 selector request = %v", err)
	}
}

func TestPrepareRequestRejectsNormalizationSelectorDrift(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 27, 7, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		mutate func(*PrepareRequest)
	}{
		{name: "policy revision", mutate: func(request *PrepareRequest) {
			request.VariantPolicyRevision = "argus-pi-review-workflow-v2"
		}},
		{name: "worker digest", mutate: func(request *PrepareRequest) {
			request.RollbackImplementation.SHA256 = strings.Repeat("b", 64)
		}},
		{name: "same implementation", mutate: func(request *PrepareRequest) {
			request.RollbackImplementation.Revision = request.VariantImplementation.Revision
			request.RollbackPolicyRevision = request.VariantPolicyRevision
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := validPrepareRequest(now)
			test.mutate(&request)
			if err := request.Validate(); err == nil {
				t.Fatal("expected selector drift rejection")
			}
		})
	}
}

func validPrepareRequest(now time.Time) PrepareRequest {
	sha := strings.Repeat("a", 64)
	thresholds := Thresholds{
		MinimumCases: 1, MinimumEligibleCandidates: 1,
		MinimumOracleDuplicatePairs: 1, MinimumOracleDistinctPairs: 1,
	}
	return PrepareRequest{
		SchemaVersion: PrepareRequestSchemaVersion, PromotionVariantID: "normalization-v1",
		BaselineReviewRunID:      "baseline-review-run",
		BaselineConfigRevisionID: "normalization-config",
		BaselineConfigRevision:   "v0",
		VariantConfigRevisionID:  "normalization-config",
		VariantConfigRevision:    "v1",
		VariantPolicyRevision:    "argus-pi-review-workflow-v1",
		RollbackPolicyRevision:   "argus-pi-review-workflow-v0",
		VariantImplementation: contractsv1alpha1.VersionedRef{
			ID:       contractsv1alpha1.CandidateNormalizationImplementationID,
			Revision: contractsv1alpha1.CandidateNormalizationRevisionV1, SHA256: sha,
		},
		RollbackImplementation: contractsv1alpha1.VersionedRef{
			ID:       contractsv1alpha1.CandidateNormalizationImplementationID,
			Revision: contractsv1alpha1.CandidateNormalizationRevisionV0, SHA256: sha,
		},
		PromotionPolicyRevision: "normalization-policy-1", Owner: "normalization-owner",
		Policy: Policy{
			SchemaVersion: PolicySchemaVersion, PolicyID: "normalization-quality-gate",
			Revision: "1", Test: thresholds, Holdout: thresholds, CreatedAt: now,
			MinimumShadowRuns: 1, MinimumCanaryRuns: 1, CanaryPercentage: 10,
		},
		CreatedAt: now,
	}
}

func TestEvaluateGateDecisionFailsClosedAcrossPassFailAndInconclusive(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 27, 6, 0, 0, 0, time.UTC)
	request := GateRequest{
		SchemaVersion: GateRequestSchemaVersion,
		VariantID:     "normalization-v1",
		Gate:          evaluation.GateTargetedRegression,
		PolicyRef:     testArtifactRef(PolicyContract, "policy"),
		QualityRunRef: testArtifactRef(evaluation.NormalizationQualityRunContract, "quality"),
		EvaluatedAt:   now,
	}
	thresholds := Thresholds{
		MinimumCases: 2, MinimumEligibleCandidates: 4,
		MinimumOracleDuplicatePairs: 2, MinimumOracleDistinctPairs: 2,
		MinimumPairwisePrecisionPPM: 900_000, MinimumPairwiseRecallPPM: 900_000,
		MaximumFalseMergeRatePPM: 100_000, MinimumExactPartitionPPM: 900_000,
	}
	passing := evaluation.NormalizationQualityRun{
		QualityRunID: "quality-run-1",
		Summary: evaluation.NormalizationQualitySummary{
			Cases: 2, EligibleRawCandidates: 4,
			TrueDuplicatePairs: 2, TrueDistinctPairs: 2,
			PairwisePrecision:     ratio(2, 2, 1_000_000),
			PairwiseRecall:        ratio(2, 2, 1_000_000),
			FalseMergeRate:        ratio(0, 2, 0),
			ExactPartitionMatches: 2,
		},
	}

	tests := []struct {
		name    string
		mutate  func(*evaluation.NormalizationQualityRun)
		outcome evaluation.GateOutcome
		reason  string
	}{
		{name: "pass", outcome: evaluation.GatePass, reason: "quality_thresholds_passed"},
		{name: "threshold failure", mutate: func(run *evaluation.NormalizationQualityRun) {
			run.Summary.FalseDuplicatePairs = 1
			run.Summary.TrueDistinctPairs = 1
			run.Summary.PairwisePrecision = ratio(2, 3, 666_667)
			run.Summary.FalseMergeRate = ratio(1, 2, 500_000)
		}, outcome: evaluation.GateFail, reason: "quality_threshold_failed"},
		{name: "insufficient sample", mutate: func(run *evaluation.NormalizationQualityRun) {
			run.Summary.Cases = 1
			run.Summary.ExactPartitionMatches = 1
		}, outcome: evaluation.GateInconclusive, reason: "insufficient_sample"},
		{name: "unavailable metric", mutate: func(run *evaluation.NormalizationQualityRun) {
			run.Summary.PairwiseRecall = evaluation.RatioMetric{ReasonCode: "no_oracle_duplicates"}
		}, outcome: evaluation.GateInconclusive, reason: "required_metric_unavailable"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			run := passing
			if test.mutate != nil {
				test.mutate(&run)
			}
			decision := evaluateGateDecision(request, run, evaluation.SplitTest, thresholds)
			if decision.Outcome != test.outcome {
				t.Fatalf("outcome = %q, want %q", decision.Outcome, test.outcome)
			}
			if len(decision.ReasonCodes) != 1 || decision.ReasonCodes[0] != test.reason {
				t.Fatalf("reason_codes = %#v, want %q", decision.ReasonCodes, test.reason)
			}
		})
	}
}

func TestThresholdsRejectZeroSampleAndOutOfRangeRatio(t *testing.T) {
	t.Parallel()
	valid := Thresholds{
		MinimumCases: 1, MinimumEligibleCandidates: 1,
		MinimumOracleDuplicatePairs: 1, MinimumOracleDistinctPairs: 1,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid thresholds: %v", err)
	}
	invalid := valid
	invalid.MinimumCases = 0
	if err := invalid.Validate(); err == nil {
		t.Fatal("expected zero minimum cases to fail")
	}
	invalid = valid
	invalid.MinimumPairwisePrecisionPPM = 1_000_001
	if err := invalid.Validate(); err == nil {
		t.Fatal("expected out-of-range ratio to fail")
	}
}

func ratio(numerator, denominator uint64, ppm uint32) evaluation.RatioMetric {
	return evaluation.RatioMetric{Available: true, Numerator: numerator, Denominator: denominator, ValuePPM: ppm}
}

func testArtifactRef(contract, seed string) runmodel.ArtifactRef {
	return runmodel.ArtifactRef{
		URI:       "argus://artifacts/sha256/" + seed,
		SHA256:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SizeBytes: 1,
		Contract:  contract,
	}
}
