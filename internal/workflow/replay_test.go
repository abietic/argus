package workflow

import (
	"slices"
	"strings"
	"testing"
)

func TestDiffReplayExecutionPolicyReturnsExactFieldsAndEarliestStage(t *testing.T) {
	baseline := DefaultReviewDefinition()
	variant := DefaultReviewDefinition()
	variant.Revision = "3"
	for index := range variant.Stages {
		switch variant.Stages[index].ID {
		case "verify":
			variant.Stages[index].Retry.RetryableCodes = []string{"stage_timeout"}
		case "report":
			variant.Stages[index].Budget.TimeoutMS--
		}
	}
	fields, earliest, err := DiffReplayExecutionPolicy(baseline, variant)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"workflow.stages.report.budget.timeout_ms",
		"workflow.stages.verify.retry.retryable_codes",
	}
	if !slices.Equal(fields, want) || earliest != "verify" {
		t.Fatalf("DiffReplayExecutionPolicy() = (%v, %q), want (%v, verify)", fields, earliest, want)
	}
}

func TestDiffReplayExecutionPolicyRejectsIdentityOnlyAndStructuralChanges(t *testing.T) {
	baseline := DefaultReviewDefinition()
	tests := []struct {
		name   string
		mutate func(*Definition)
		want   string
	}{
		{
			name: "identity only",
			mutate: func(definition *Definition) {
				definition.Revision = "3"
			},
			want: "only identity",
		},
		{
			name: "same revision",
			mutate: func(definition *Definition) {
				definition.Stages[2].Budget.TimeoutMS--
			},
			want: "new revision",
		},
		{
			name: "changed workflow id",
			mutate: func(definition *Definition) {
				definition.ID = "other-workflow"
				definition.Revision = "3"
				definition.Stages[2].Budget.TimeoutMS--
			},
			want: "preserve workflow_id",
		},
		{
			name: "changed executor",
			mutate: func(definition *Definition) {
				definition.Revision = "3"
				definition.Stages[2].Executor = "other-executor"
			},
			want: "immutable graph",
		},
		{
			name: "changed graph",
			mutate: func(definition *Definition) {
				definition.Revision = "3"
				definition.Stages[3].DependsOn = []string{"plan_context"}
			},
			want: "immutable graph",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			variant := DefaultReviewDefinition()
			test.mutate(&variant)
			_, _, err := DiffReplayExecutionPolicy(baseline, variant)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("DiffReplayExecutionPolicy() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestReplayChangesEffectivePolicyRejectsInactiveCeilingOnly(t *testing.T) {
	baseline := DefaultReviewDefinition()
	variant := DefaultReviewDefinition()
	variant.Revision = "3"
	variant.Stages[0].Budget.TimeoutMS++

	if _, _, err := DiffReplayExecutionPolicy(baseline, variant); err != nil {
		t.Fatalf("DiffReplayExecutionPolicy() error = %v", err)
	}
	limits := RuntimeLimits{
		TimeoutMS:      baseline.Stages[0].Budget.TimeoutMS,
		MaxInputBytes:  baseline.Stages[0].Budget.MaxInputBytes,
		MaxOutputBytes: baseline.Stages[0].Budget.MaxOutputBytes,
		MaxAttempts:    baseline.Stages[0].Retry.MaxAttempts,
		MaxConcurrency: baseline.Stages[0].Budget.MaxConcurrency,
	}
	if ReplayChangesEffectivePolicy(baseline, variant, limits) {
		t.Fatal("inactive workflow ceiling was treated as an effective scheduler change")
	}

	variant.Stages[0].Retry.RetryableCodes = []string{"stage_timeout"}
	if !ReplayChangesEffectivePolicy(baseline, variant, limits) {
		t.Fatal("retry evidence policy change was not treated as effective")
	}
}
