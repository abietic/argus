package evaluation

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestPromotionRequiresSevenOrderedEvidenceBackedGatesAndSupportsRollback(t *testing.T) {
	repository, store := newEvaluationRepository(t)
	holdout := testActiveCase("promotion-holdout", "repo-promotion-holdout", SplitHoldout, testEpoch)
	createCase(t, repository, holdout, RoleHoldoutMaintainer)
	exposure := testExposure(
		"holdout-run", holdout.CaseID, holdout.CreatedAt.Add(1), ExposureNotSeen,
	)
	if _, err := repository.RecordExposure(
		context.Background(),
		exposure,
		testMutation("promotion-exposure", holdout.CreatedAt.Add(2), RoleHoldoutRunner),
	); err != nil {
		t.Fatalf("RecordExposure() error = %v", err)
	}

	invalid := testPromotionVariant("invalid", holdout.CreatedAt.Add(3))
	invalid.RollbackRevision = ""
	if _, err := repository.RegisterPromotion(
		context.Background(),
		invalid,
		testMutation("register-no-rollback", invalid.CreatedAt, RolePromotionOperator),
	); err == nil {
		t.Fatal("RegisterPromotion(without rollback) unexpectedly succeeded")
	}
	feedback := testPromotionVariant("feedback", holdout.CreatedAt.Add(3))
	feedback.Origin = PromotionProductionFeedback
	if _, err := repository.RegisterPromotion(
		context.Background(),
		feedback,
		testMutation("register-feedback", feedback.CreatedAt, RolePromotionOperator),
	); err == nil {
		t.Fatal("RegisterPromotion(production feedback) unexpectedly succeeded")
	}

	variant := testPromotionVariant("variant-1", holdout.CreatedAt.Add(3))
	registerMutation := testMutation(
		"register-variant-1", variant.CreatedAt, RolePromotionOperator,
	)
	record, err := repository.RegisterPromotion(
		context.Background(), variant, registerMutation,
	)
	if err != nil {
		t.Fatalf("RegisterPromotion() error = %v", err)
	}
	if record.Status != PromotionRegistered ||
		record.NextGate == nil || *record.NextGate != GateSchemaContract {
		t.Fatalf("registered projection = %+v", record)
	}
	if _, err := repository.RegisterPromotion(
		context.Background(), variant, registerMutation,
	); err != nil {
		t.Fatalf("RegisterPromotion(idempotent) error = %v", err)
	}

	skipped := testGateResult(variant.VariantID, GateTargetedRegression)
	if _, err := repository.RecordGate(
		context.Background(),
		skipped,
		testMutation(
			"skip-schema", variant.CreatedAt.Add(1), RolePromotionOperator,
		),
	); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("RecordGate(skip) error = %v", err)
	}
	missingEvidence := testGateResult(variant.VariantID, GateSchemaContract)
	missingEvidence.Evidence.Refs = []string{}
	if _, err := repository.RecordGate(
		context.Background(),
		missingEvidence,
		testMutation(
			"missing-evidence", variant.CreatedAt.Add(1), RolePromotionOperator,
		),
	); err == nil {
		t.Fatal("RecordGate(missing evidence) unexpectedly succeeded")
	}
	record, err = repository.GetPromotion(
		variant.VariantID,
		Access{Actor: "operator", Roles: []Role{RolePromotionOperator}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Gates) != 0 {
		t.Fatalf("rejected gates changed ledger: %+v", record.Gates)
	}

	for index, gate := range OrderedPromotionGates() {
		result := testGateResult(variant.VariantID, gate)
		roles := []Role{RolePromotionOperator}
		switch gate {
		case GateFixedHoldout:
			result.Evidence.EvaluationRunID = exposure.EvaluationRunID
			result.Evidence.HoldoutCaseIDs = []string{holdout.CaseID}
			roles = []Role{RoleHoldoutRunner, RolePromotionOperator}
		case GateAuthorization:
			result.Evidence.Authorization = &PromotionAuthorization{
				Kind:         "human",
				AuthorizedBy: "test-actor",
			}
			roles = []Role{RolePromotionApprover}
		case GateRollbackMonitor:
			result.Evidence.RollbackVerified = true
		}
		gateMutation := testMutation(
			fmt.Sprintf("gate-%d", index+1),
			variant.CreatedAt.Add(timeStep(index+1)),
			roles...,
		)
		record, err = repository.RecordGate(
			context.Background(),
			result,
			gateMutation,
		)
		if err != nil {
			t.Fatalf("RecordGate(%s) error = %v", gate, err)
		}
		if index == 0 {
			if _, err := repository.RecordGate(
				context.Background(), result, gateMutation,
			); err != nil {
				t.Fatalf("RecordGate(idempotent) error = %v", err)
			}
			conflict := result
			conflict.Summary = "different retry payload"
			if _, err := repository.RecordGate(
				context.Background(), conflict, gateMutation,
			); !errors.Is(err, ErrConflict) {
				t.Fatalf("RecordGate(conflicting idempotency key) error = %v", err)
			}
		}
		if index < len(OrderedPromotionGates())-1 &&
			record.Status != PromotionInProgress {
			t.Fatalf("status after %s = %q", gate, record.Status)
		}
	}
	if record.Status != PromotionActive || record.NextGate != nil ||
		len(record.Gates) != len(OrderedPromotionGates()) {
		t.Fatalf("active promotion projection = %+v", record)
	}
	if !record.Gates[2].EvidenceRedacted ||
		len(record.Gates[2].Result.Evidence.HoldoutCaseIDs) != 0 {
		t.Fatalf("operator-only projection leaked holdout evidence: %+v", record.Gates[2])
	}
	fullRecord, err := repository.GetPromotion(
		variant.VariantID,
		Access{
			Actor: "operator",
			Roles: []Role{RoleHoldoutRunner, RolePromotionOperator},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if fullRecord.Gates[2].EvidenceRedacted ||
		len(fullRecord.Gates[2].Result.Evidence.HoldoutCaseIDs) != 1 {
		t.Fatalf("authorized projection did not expose holdout evidence: %+v",
			fullRecord.Gates[2])
	}
	active, err := repository.ActiveRevision(PromotionRulePack)
	if err != nil {
		t.Fatal(err)
	}
	if active != variant.Revision {
		t.Fatalf("active revision = %q, want %q", active, variant.Revision)
	}

	restarted, err := New(store)
	if err != nil {
		t.Fatalf("New(restart) error = %v", err)
	}
	restartedRecord, err := restarted.GetPromotion(
		variant.VariantID,
		Access{
			Actor: "operator",
			Roles: []Role{RoleHoldoutRunner, RolePromotionOperator},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if restartedRecord.Status != PromotionActive ||
		len(restartedRecord.Gates) != len(OrderedPromotionGates()) {
		t.Fatalf("restart promotion projection = %+v", restartedRecord)
	}
	rollbackMutation := testMutation(
		"rollback-variant-1",
		variant.CreatedAt.Add(timeStep(20)),
		RolePromotionOperator,
	)
	rolledBack, err := restarted.RollbackPromotion(
		context.Background(), variant.VariantID, rollbackMutation,
	)
	if err != nil {
		t.Fatalf("RollbackPromotion() error = %v", err)
	}
	if rolledBack.Status != PromotionRolledBack {
		t.Fatalf("rollback status = %q", rolledBack.Status)
	}
	active, err = restarted.ActiveRevision(PromotionRulePack)
	if err != nil {
		t.Fatal(err)
	}
	if active != variant.RollbackRevision {
		t.Fatalf("rollback active revision = %q, want %q",
			active, variant.RollbackRevision)
	}
	if _, err := restarted.RollbackPromotion(
		context.Background(), variant.VariantID, rollbackMutation,
	); err != nil {
		t.Fatalf("RollbackPromotion(idempotent) error = %v", err)
	}
}

func TestPromotionFailAndInconclusiveAreTerminalLedgerResults(t *testing.T) {
	tests := []struct {
		outcome GateOutcome
		status  PromotionStatus
	}{
		{outcome: GateFail, status: PromotionFailed},
		{outcome: GateInconclusive, status: PromotionInconclusive},
	}
	for index, test := range tests {
		t.Run(string(test.outcome), func(t *testing.T) {
			repository, _ := newEvaluationRepository(t)
			variant := testPromotionVariant(
				fmt.Sprintf("variant-terminal-%d", index),
				testEpoch.Add(timeStep(index+1)),
			)
			if _, err := repository.RegisterPromotion(
				context.Background(),
				variant,
				testMutation(
					"register-"+variant.VariantID,
					variant.CreatedAt,
					RolePromotionOperator,
				),
			); err != nil {
				t.Fatal(err)
			}
			result := testGateResult(variant.VariantID, GateSchemaContract)
			result.Outcome = test.outcome
			result.Evidence.ChecksPassed = false
			record, err := repository.RecordGate(
				context.Background(),
				result,
				testMutation(
					"terminal-"+variant.VariantID,
					variant.CreatedAt.Add(1),
					RolePromotionOperator,
				),
			)
			if err != nil {
				t.Fatalf("RecordGate(%s) error = %v", test.outcome, err)
			}
			if record.Status != test.status || record.NextGate != nil ||
				len(record.Gates) != 1 {
				t.Fatalf("terminal projection = %+v", record)
			}
			next := testGateResult(variant.VariantID, GateTargetedRegression)
			if _, err := repository.RecordGate(
				context.Background(),
				next,
				testMutation(
					"after-terminal-"+variant.VariantID,
					variant.CreatedAt.Add(2),
					RolePromotionOperator,
				),
			); !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("RecordGate(after terminal) error = %v", err)
			}
			if _, err := repository.ActiveRevision(variant.Component); !errors.Is(err, ErrNotFound) {
				t.Fatalf("ActiveRevision(terminal variant) error = %v", err)
			}
		})
	}
}

func TestFixedHoldoutGateFailsClosedOnContamination(t *testing.T) {
	repository, _ := newEvaluationRepository(t)
	holdout := testActiveCase("seen-holdout", "repo-seen-holdout", SplitHoldout, testEpoch)
	createCase(t, repository, holdout, RoleHoldoutMaintainer)
	seen := testExposure(
		"seen-run", holdout.CaseID, holdout.CreatedAt.Add(1), ExposureSeen,
	)
	if _, err := repository.RecordExposure(
		context.Background(),
		seen,
		testMutation("record-seen", holdout.CreatedAt.Add(2), RoleHoldoutRunner),
	); err != nil {
		t.Fatal(err)
	}
	variant := testPromotionVariant("contaminated-variant", holdout.CreatedAt.Add(3))
	variant.Component = PromotionPrompt
	if _, err := repository.RegisterPromotion(
		context.Background(),
		variant,
		testMutation("register-contaminated", variant.CreatedAt, RolePromotionOperator),
	); err != nil {
		t.Fatal(err)
	}
	for index, gate := range []PromotionGate{GateSchemaContract, GateTargetedRegression} {
		if _, err := repository.RecordGate(
			context.Background(),
			testGateResult(variant.VariantID, gate),
			testMutation(
				fmt.Sprintf("pre-holdout-%d", index),
				variant.CreatedAt.Add(timeStep(index+1)),
				RolePromotionOperator,
			),
		); err != nil {
			t.Fatal(err)
		}
	}
	result := testGateResult(variant.VariantID, GateFixedHoldout)
	result.Evidence.EvaluationRunID = seen.EvaluationRunID
	result.Evidence.HoldoutCaseIDs = []string{holdout.CaseID}
	if _, err := repository.RecordGate(
		context.Background(),
		result,
		testMutation(
			"contaminated-holdout-gate",
			variant.CreatedAt.Add(timeStep(3)),
			RoleHoldoutRunner,
			RolePromotionOperator,
		),
	); !errors.Is(err, ErrContaminated) {
		t.Fatalf("RecordGate(contaminated holdout) error = %v", err)
	}
	record, err := repository.GetPromotion(
		variant.VariantID,
		Access{Actor: "operator", Roles: []Role{RolePromotionOperator}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if record.NextGate == nil || *record.NextGate != GateFixedHoldout ||
		len(record.Gates) != 2 {
		t.Fatalf("contaminated gate changed ledger = %+v", record)
	}
}

func testPromotionVariant(id string, at time.Time) PromotionVariant {
	return PromotionVariant{
		SchemaVersion:    PromotionVariantSchemaVersion,
		VariantID:        id,
		Component:        PromotionRulePack,
		Revision:         "revision-" + id,
		RollbackRevision: "baseline-1",
		PolicyRevision:   "promotion-policy-1",
		Origin:           PromotionExperiment,
		Owner:            "quality-team",
		CreatedAt:        at,
	}
}

func testGateResult(variantID string, gate PromotionGate) GateResult {
	return GateResult{
		SchemaVersion: GateResultSchemaVersion,
		VariantID:     variantID,
		Gate:          gate,
		Outcome:       GatePass,
		Evidence: GateEvidence{
			Refs:             []string{testArtifact("evidence-" + string(gate))},
			Basis:            EvidenceDeterministic,
			ChecksPassed:     true,
			HoldoutCaseIDs:   []string{},
			SafetyEventCount: 0,
		},
		Summary: "gate evidence accepted",
	}
}

func timeStep(value int) time.Duration {
	return time.Duration(value) * time.Minute
}
