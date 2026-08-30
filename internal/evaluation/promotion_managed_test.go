package evaluation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"argus.local/argus/internal/store/local"
)

func TestManagedPromotionCannotUseGenericMutationPaths(t *testing.T) {
	t.Parallel()
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	binding := PromotionManagementBinding{Kind: "calibration_config", CalibrationRunID: "calibration-1", CalibrationRunSHA256: strings.Repeat("1", 64), ProfileCandidateID: "candidate-1", ProfileCandidateSHA256: strings.Repeat("2", 64), CalibrationReportSHA256: strings.Repeat("3", 64), BaselineBundleSHA256: strings.Repeat("4", 64), ConfigRevisionID: "config-calibration", ConfigRevision: "config-calibration:2", ConfigRevisionSHA256: strings.Repeat("5", 64)}
	variant := PromotionVariant{SchemaVersion: PromotionVariantSchemaVersion, VariantID: "managed-1", Component: PromotionFilter, Revision: binding.ConfigRevision, RollbackRevision: "config-baseline:1", PolicyRevision: "policy-1", Origin: PromotionExperiment, Owner: "owner", CreatedAt: now, ManagedBinding: &binding}
	mutation := Mutation{IdempotencyKey: "register", Actor: "operator", Roles: []Role{RolePromotionOperator}, Audit: "register managed", At: now}
	if _, err := repository.RegisterPromotion(context.Background(), variant, mutation); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("generic RegisterPromotion() error = %v", err)
	}
	if _, err := repository.RegisterManagedPromotion(context.Background(), variant, mutation); err != nil {
		t.Fatal(err)
	}
	gate := GateResult{SchemaVersion: GateResultSchemaVersion, VariantID: variant.VariantID, Gate: GateSchemaContract, Outcome: GatePass, Evidence: GateEvidence{Refs: []string{"artifact://test/schema"}, Basis: EvidenceDeterministic, ChecksPassed: true, HoldoutCaseIDs: []string{}}, Summary: "checked"}
	mutation.IdempotencyKey, mutation.At = "gate", now.Add(time.Minute)
	if _, err := repository.RecordGate(context.Background(), gate, mutation); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("generic RecordGate() error = %v", err)
	}
	if _, err := repository.RecordManagedGate(context.Background(), gate, binding, mutation); err != nil {
		t.Fatal(err)
	}
}
