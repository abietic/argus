package platformapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"argus.local/argus/internal/configrepo"
	"argus.local/argus/internal/evaluation"
	"argus.local/argus/internal/normalizationpromotion"
	"argus.local/argus/internal/pireviewmap"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/store/local"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

type normalizationPromotionStub struct {
	records  []evaluation.PromotionRecord
	prepared normalizationpromotion.PrepareRequest
	gated    normalizationpromotion.GateRequest
}

func (stub *normalizationPromotionStub) Prepare(_ context.Context, request normalizationpromotion.PrepareRequest, _ evaluation.Mutation) (normalizationpromotion.Preparation, error) {
	stub.prepared = request
	return normalizationpromotion.Preparation{Promotion: stub.records[0]}, nil
}

func (stub *normalizationPromotionStub) RecordQualityGate(_ context.Context, request normalizationpromotion.GateRequest, _ evaluation.Mutation) (normalizationpromotion.GateOutput, error) {
	stub.gated = request
	return normalizationpromotion.GateOutput{Promotion: stub.records[0]}, nil
}

func (stub *normalizationPromotionStub) RecordOperationalGate(_ context.Context, _ normalizationpromotion.OperationalGateRequest, _ evaluation.Mutation) (normalizationpromotion.OperationalGateOutput, error) {
	return normalizationpromotion.OperationalGateOutput{Promotion: stub.records[0]}, nil
}

func (stub *normalizationPromotionStub) StartCanary(_ context.Context, _ string, _ runmodel.ArtifactRef, _ configrepo.Rollout, _ evaluation.Mutation) (normalizationpromotion.LifecycleOutput, error) {
	return normalizationpromotion.LifecycleOutput{Promotion: stub.records[0]}, nil
}

func (stub *normalizationPromotionStub) Activate(_ context.Context, _ string, _ evaluation.Mutation) (normalizationpromotion.LifecycleOutput, error) {
	return normalizationpromotion.LifecycleOutput{Promotion: stub.records[0]}, nil
}

func (stub *normalizationPromotionStub) Rollback(_ context.Context, _ string, _ evaluation.Mutation) (normalizationpromotion.LifecycleOutput, error) {
	return normalizationpromotion.LifecycleOutput{Promotion: stub.records[0]}, nil
}

func (stub *normalizationPromotionStub) Get(id string, _ evaluation.Access) (evaluation.PromotionRecord, error) {
	for _, record := range stub.records {
		if record.Variant.VariantID == id {
			return record, nil
		}
	}
	return evaluation.PromotionRecord{}, evaluation.ErrNotFound
}

func (stub *normalizationPromotionStub) List(evaluation.Access) ([]evaluation.PromotionRecord, error) {
	return stub.records, nil
}

func TestNormalizationPromotionRoutesAreStrictAndPermissionScoped(t *testing.T) {
	now := time.Date(2026, time.August, 27, 8, 0, 0, 0, time.UTC)
	record := evaluation.PromotionRecord{Variant: evaluation.PromotionVariant{VariantID: "normalization-policy-v2"}}
	stub := &normalizationPromotionStub{records: []evaluation.PromotionRecord{record}}
	configStore, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	configs, err := configrepo.New(configStore)
	if err != nil {
		t.Fatal(err)
	}
	principal := Principal{
		SchemaVersion: PrincipalSchemaVersion, Actor: "normalization-operator",
		Roles:           []evaluation.Role{evaluation.RoleDatasetCurator, evaluation.RolePromotionOperator},
		Permissions:     []Permission{PermissionConfigRead, PermissionConfigWrite, PermissionEvaluationRead, PermissionEvaluationWrite},
		ProfileRevision: "normalization-promotion-v1",
	}
	handler, err := NewHandler(Services{NormalizationPromotion: stub, Config: configs}, principal, testToken)
	if err != nil {
		t.Fatal(err)
	}

	list := httptest.NewRecorder()
	handler.ServeHTTP(list, authenticatedRequest(http.MethodGet, "/v1/evaluation/normalization/promotions?limit=10", nil))
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), record.Variant.VariantID) {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}
	prepare := normalizationpromotion.PrepareRequest{
		SchemaVersion:            normalizationpromotion.PrepareRequestSchemaVersion,
		PromotionVariantID:       record.Variant.VariantID,
		BaselineReviewRunID:      "normalization-baseline-run",
		BaselineConfigRevisionID: "normalization-config",
		BaselineConfigRevision:   "v1",
		VariantConfigRevisionID:  "normalization-config",
		VariantConfigRevision:    "v2",
		VariantPolicyRevision:    pireviewmap.NormalizationPreviewPolicyRevision,
		RollbackPolicyRevision:   "argus-pi-review-workflow-v1",
		VariantImplementation: contractsv1alpha1.VersionedRef{
			ID:       contractsv1alpha1.CandidateNormalizationImplementationID,
			Revision: contractsv1alpha1.CandidateNormalizationRevisionV2,
			SHA256:   strings.Repeat("a", 64),
		},
		RollbackImplementation: contractsv1alpha1.VersionedRef{
			ID:       contractsv1alpha1.CandidateNormalizationImplementationID,
			Revision: contractsv1alpha1.CandidateNormalizationRevisionV1,
			SHA256:   strings.Repeat("a", 64),
		},
		PromotionPolicyRevision: "normalization-promotion-v1", Owner: "normalization-owner",
		Policy: normalizationpromotion.Policy{
			SchemaVersion: normalizationpromotion.PolicySchemaVersion,
			PolicyID:      "normalization-gate", Revision: "1",
			Test: testPromotionThresholds(), Holdout: testPromotionThresholds(), CreatedAt: now,
			MinimumShadowRuns: 1, MinimumCanaryRuns: 1, CanaryPercentage: 10,
		},
		CreatedAt: now,
	}
	prepareBody := mustJSON(t, NormalizationPromotionPrepareCommand{
		SchemaVersion: NormalizationPromotionPrepareCommandSchemaVersion,
		Request:       prepare,
		Mutation:      MutationInput{SchemaVersion: MutationInputSchemaVersion, IdempotencyKey: "prepare-normalization", Audit: "prepare governed normalization promotion", At: now},
	})
	prepared := httptest.NewRecorder()
	handler.ServeHTTP(prepared, authenticatedRequest(http.MethodPost, "/v1/evaluation/normalization/promotions", strings.NewReader(string(prepareBody))))
	if prepared.Code != http.StatusCreated || stub.prepared.PromotionVariantID != record.Variant.VariantID {
		t.Fatalf("prepare status=%d body=%s request=%+v", prepared.Code, prepared.Body.String(), stub.prepared)
	}

	ref := testNormalizationPromotionRef(normalizationpromotion.PolicyContract)
	qualityRef := testNormalizationPromotionRef(evaluation.NormalizationQualityRunContract)
	gateRequest := normalizationpromotion.GateRequest{
		SchemaVersion: normalizationpromotion.GateRequestSchemaVersion,
		VariantID:     record.Variant.VariantID, Gate: evaluation.GateTargetedRegression,
		PolicyRef: ref, QualityRunRef: qualityRef, EvaluatedAt: now.Add(time.Minute),
	}
	gateBody := mustJSON(t, NormalizationPromotionGateCommand{
		SchemaVersion: NormalizationPromotionGateCommandSchemaVersion,
		Request:       gateRequest,
		Mutation:      MutationInput{SchemaVersion: MutationInputSchemaVersion, IdempotencyKey: "gate-normalization", Audit: "record exact normalization quality gate", At: gateRequest.EvaluatedAt},
	})
	gated := httptest.NewRecorder()
	handler.ServeHTTP(gated, authenticatedRequest(http.MethodPost, "/v1/evaluation/normalization/promotion/gates", strings.NewReader(string(gateBody))))
	if gated.Code != http.StatusOK || stub.gated.VariantID != record.Variant.VariantID {
		t.Fatalf("gate status=%d body=%s request=%+v", gated.Code, gated.Body.String(), stub.gated)
	}

	operationalAt := gateRequest.EvaluatedAt.Add(time.Minute)
	operationalBody := mustJSON(t, NormalizationPromotionOperationalGateCommand{
		SchemaVersion: NormalizationPromotionOperationalGateCommandSchemaVersion,
		Request: normalizationpromotion.OperationalGateRequest{
			SchemaVersion: normalizationpromotion.OperationalGateRequestSchemaVersion,
			VariantID:     record.Variant.VariantID, Gate: evaluation.GateShadowTraffic,
			PolicyRef: ref, ReviewRunIDs: []string{"shadow-run-1"}, SafetyEventCount: 0,
			EvaluatedAt: operationalAt,
		},
		Mutation: MutationInput{SchemaVersion: MutationInputSchemaVersion, IdempotencyKey: "shadow-normalization", Audit: "verify shadow runs", At: operationalAt},
	})
	operational := httptest.NewRecorder()
	handler.ServeHTTP(operational, authenticatedRequest(http.MethodPost, "/v1/evaluation/normalization/promotion/operational-gates", strings.NewReader(string(operationalBody))))
	if operational.Code != http.StatusOK {
		t.Fatalf("operational gate status=%d body=%s", operational.Code, operational.Body.String())
	}

	canaryBody := mustJSON(t, NormalizationPromotionCanaryCommand{
		SchemaVersion: NormalizationPromotionCanaryCommandSchemaVersion,
		VariantID:     record.Variant.VariantID, PolicyRef: ref,
		Rollout:  configrepo.Rollout{Percentage: 10, Seed: "normalization-canary"},
		Mutation: MutationInput{SchemaVersion: MutationInputSchemaVersion, IdempotencyKey: "canary-normalization", Audit: "start canary", At: operationalAt.Add(time.Minute)},
	})
	canary := httptest.NewRecorder()
	handler.ServeHTTP(canary, authenticatedRequest(http.MethodPost, "/v1/evaluation/normalization/promotion/canary", strings.NewReader(string(canaryBody))))
	if canary.Code != http.StatusOK {
		t.Fatalf("canary status=%d body=%s", canary.Code, canary.Body.String())
	}

	lifecycleBody := mustJSON(t, NormalizationPromotionLifecycleCommand{
		SchemaVersion: NormalizationPromotionLifecycleCommandSchemaVersion,
		VariantID:     record.Variant.VariantID,
		Mutation:      MutationInput{SchemaVersion: MutationInputSchemaVersion, IdempotencyKey: "activate-normalization", Audit: "activate normalization", At: operationalAt.Add(2 * time.Minute)},
	})
	activated := httptest.NewRecorder()
	handler.ServeHTTP(activated, authenticatedRequest(http.MethodPost, "/v1/evaluation/normalization/promotion/activate", strings.NewReader(string(lifecycleBody))))
	if activated.Code != http.StatusOK {
		t.Fatalf("activate status=%d body=%s", activated.Code, activated.Body.String())
	}

	strict := httptest.NewRecorder()
	handler.ServeHTTP(strict, authenticatedRequest(http.MethodPost, "/v1/evaluation/normalization/promotions", strings.NewReader(`{"schema_version":"argus.local_api_normalization_promotion_prepare_command.v1alpha1","unexpected":true}`)))
	if strict.Code != http.StatusBadRequest {
		t.Fatalf("strict status=%d body=%s", strict.Code, strict.Body.String())
	}

	readOnly := principal
	readOnly.Permissions = []Permission{PermissionEvaluationRead}
	readHandler, err := NewHandler(Services{NormalizationPromotion: stub, Config: configs}, readOnly, testToken)
	if err != nil {
		t.Fatal(err)
	}
	denied := httptest.NewRecorder()
	readHandler.ServeHTTP(denied, authenticatedRequest(http.MethodPost, "/v1/evaluation/normalization/promotions", strings.NewReader(string(prepareBody))))
	if denied.Code != http.StatusForbidden {
		t.Fatalf("write without permission status=%d body=%s", denied.Code, denied.Body.String())
	}

	evaluationWriter := principal
	evaluationWriter.Permissions = []Permission{PermissionConfigRead, PermissionEvaluationRead, PermissionEvaluationWrite}
	evaluationWriterHandler, err := NewHandler(Services{NormalizationPromotion: stub, Config: configs}, evaluationWriter, testToken)
	if err != nil {
		t.Fatal(err)
	}
	configDenied := httptest.NewRecorder()
	evaluationWriterHandler.ServeHTTP(configDenied, authenticatedRequest(http.MethodPost, "/v1/evaluation/normalization/promotions", strings.NewReader(string(prepareBody))))
	if configDenied.Code != http.StatusForbidden {
		t.Fatalf("prepare without config_write status=%d body=%s", configDenied.Code, configDenied.Body.String())
	}
}

func testPromotionThresholds() normalizationpromotion.Thresholds {
	return normalizationpromotion.Thresholds{
		MinimumCases: 1, MinimumEligibleCandidates: 2,
		MinimumOracleDuplicatePairs: 1, MinimumOracleDistinctPairs: 1,
		MinimumPairwisePrecisionPPM: 900_000, MinimumPairwiseRecallPPM: 900_000,
		MaximumFalseMergeRatePPM: 100_000, MinimumExactPartitionPPM: 900_000,
	}
}

func testNormalizationPromotionRef(contract string) runmodel.ArtifactRef {
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	return runmodel.ArtifactRef{URI: "artifact://local/sha256/" + digest, SHA256: digest, SizeBytes: 1, Contract: contract}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
