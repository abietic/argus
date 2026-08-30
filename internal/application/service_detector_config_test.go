package application

import (
	"context"
	"strings"
	"testing"

	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/reviewcore"
	"github.com/abietic/argus/internal/workflow"
)

func TestServiceExecutesConfiguredGoASTDetectorEndToEnd(t *testing.T) {
	repositoryPath := t.TempDir()
	runGit(t, repositoryPath, "init", "-q", "-b", "main")
	runGit(t, repositoryPath, "config", "user.name", "Argus Test")
	runGit(t, repositoryPath, "config", "user.email", "argus@example.invalid")
	runGit(t, repositoryPath, "config", "commit.gpgsign", "false")
	writeFixture(
		t,
		repositoryPath,
		"review.go",
		"package fixture\n\nimport \"context\"\n\n"+
			"func Review(parent context.Context) context.Context { return parent }\n",
	)
	base := commitFixture(t, repositoryPath, "base")
	writeFixture(
		t,
		repositoryPath,
		"review.go",
		"package fixture\n\nimport \"context\"\n\n"+
			"func Review(parent context.Context) context.Context {\n"+
			"\tresult, _ := context.WithCancel(parent)\n"+
			"\treturn result\n"+
			"}\n",
	)
	head := commitFixture(t, repositoryPath, "head")

	service, repository := newTestService(t, ServiceOptions{})
	outcome, err := service.Review(context.Background(), ReviewRequest{
		RepositoryPath: repositoryPath,
		BaseRevision:   base,
		HeadRevision:   head,
	})
	if err != nil {
		t.Fatalf("Review() error = %v", err)
	}
	if outcome.Report == nil || len(outcome.Report.Findings) != 1 ||
		outcome.Report.Findings[0].RuleID != reviewcore.RuleGoContextCancelDiscarded ||
		outcome.Report.Findings[0].Verification.Status != reviewcore.VerificationVerified ||
		outcome.Report.Summary.Publish != 1 {
		t.Fatalf("AST review outcome = %+v", outcome)
	}
	detected, err := loadStageResult(
		context.Background(),
		repository,
		outcome.Run,
		reviewcore.StageDetect,
	)
	if err != nil {
		t.Fatalf("load detect checkpoint: %v", err)
	}
	if len(detected.Output.CandidateFindings) != 1 ||
		detected.Output.CandidateFindings[0].DetectorID != reviewcore.DetectorGoASTID ||
		len(detected.Output.DetectionGaps) != 0 {
		t.Fatalf("detect checkpoint = %+v", detected.Output)
	}
}

func TestDefaultConfigBundleEnablesVersionedGoASTRule(t *testing.T) {
	bundle, err := DefaultConfigBundle(
		DefaultLocalConfig(),
		workflow.DefaultReviewDefinition(),
	)
	if err != nil {
		t.Fatalf("DefaultConfigBundle() error = %v", err)
	}
	var configured *reviewconfig.RuleDefinition
	for index := range bundle.RulePack.Rules {
		if bundle.RulePack.Rules[index].ID == reviewcore.RuleGoContextCancelDiscarded {
			configured = &bundle.RulePack.Rules[index]
			break
		}
	}
	if configured == nil {
		t.Fatal("default RulePack omitted Go AST rule")
	}
	if configured.Revision != "1" || !configured.Enabled ||
		configured.Detector.ID != reviewcore.DetectorGoASTID ||
		configured.Detector.Revision != reviewcore.DetectorGoASTRevision ||
		configured.Detector.SHA256 != descriptorDigest(
			reviewcore.DetectorGoASTID+"@"+reviewcore.DetectorGoASTRevision,
		) {
		t.Fatalf("Go AST rule descriptor = %+v", configured)
	}
	policy, err := runtimePolicyFromBundle(bundle)
	if err != nil {
		t.Fatalf("runtimePolicyFromBundle() error = %v", err)
	}
	found := false
	for _, rule := range policy.Rules {
		if rule.RuleID == reviewcore.RuleGoContextCancelDiscarded {
			found = true
			if !rule.Enabled || rule.DetectorID != reviewcore.DetectorGoASTID ||
				rule.DetectorRevision != reviewcore.DetectorGoASTRevision {
				t.Fatalf("runtime Go AST rule = %+v", rule)
			}
		}
	}
	if !found {
		t.Fatal("runtime policy omitted configured Go AST rule")
	}
}

func TestRuntimePolicyFromBundleRejectsUnknownEnabledRule(t *testing.T) {
	bundle, err := DefaultConfigBundle(
		DefaultLocalConfig(),
		workflow.DefaultReviewDefinition(),
	)
	if err != nil {
		t.Fatal(err)
	}
	rules := append([]reviewconfig.RuleDefinition(nil), bundle.RulePack.Rules...)
	rules = append(rules, reviewconfig.RuleDefinition{
		ID:       "unknown-rule",
		Revision: "1",
		Kind:     "deterministic",
		Detector: reviewconfig.VersionedRef{
			ID:       reviewcore.DetectorGoASTID,
			Revision: reviewcore.DetectorGoASTRevision,
			SHA256: descriptorDigest(
				reviewcore.DetectorGoASTID + "@" + reviewcore.DetectorGoASTRevision,
			),
		},
		Languages: []string{"go"}, PathPrefixes: []string{},
		EvidenceKinds: []string{"file_content", "patch_line", "target_line"},
		Severity:      "high", Enabled: true,
	})
	bundle.RulePack, err = reviewconfig.SealRulePack(
		bundle.RulePack.ID,
		"unknown-enabled",
		rules,
	)
	if err != nil {
		t.Fatal(err)
	}
	resealConfigBundle(t, &bundle)
	if _, err := runtimePolicyFromBundle(bundle); err == nil ||
		!strings.Contains(err.Error(), `enabled local rule "unknown-rule" is unsupported`) {
		t.Fatalf("unknown enabled rule error = %v", err)
	}
}

func TestRuntimePolicyFromBundlePreservesExplicitASTDisable(t *testing.T) {
	bundle, err := DefaultConfigBundle(
		DefaultLocalConfig(),
		workflow.DefaultReviewDefinition(),
	)
	if err != nil {
		t.Fatal(err)
	}
	rules := append([]reviewconfig.RuleDefinition(nil), bundle.RulePack.Rules...)
	for index := range rules {
		if rules[index].ID == reviewcore.RuleGoContextCancelDiscarded {
			rules[index].Enabled = false
		}
	}
	bundle.RulePack, err = reviewconfig.SealRulePack(
		bundle.RulePack.ID,
		"ast-disabled",
		rules,
	)
	if err != nil {
		t.Fatal(err)
	}
	resealConfigBundle(t, &bundle)
	policy, err := runtimePolicyFromBundle(bundle)
	if err != nil {
		t.Fatalf("runtimePolicyFromBundle() error = %v", err)
	}
	for _, rule := range policy.Rules {
		if rule.RuleID == reviewcore.RuleGoContextCancelDiscarded {
			if rule.Enabled {
				t.Fatalf("runtime AST rule ignored explicit disable: %+v", rule)
			}
			return
		}
	}
	t.Fatal("runtime policy dropped disabled AST rule provenance")
}
