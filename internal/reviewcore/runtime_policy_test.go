package reviewcore

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestDefaultRuntimePolicyPreservesExecuteStageBehavior(t *testing.T) {
	input := testInput("default.go", "package sample", "// TODO default")
	var legacyUpstream StageResult
	var policyUpstream StageResult
	for _, stage := range orderedStages {
		legacy, err := ExecuteStage(context.Background(), input, stage, legacyUpstream)
		if err != nil {
			t.Fatalf("ExecuteStage(%s) error = %v", stage, err)
		}
		configured, err := ExecuteStageWithPolicy(
			context.Background(),
			input,
			stage,
			policyUpstream,
			DefaultRuntimePolicy(),
		)
		if err != nil {
			t.Fatalf("ExecuteStageWithPolicy(%s) error = %v", stage, err)
		}
		if !reflect.DeepEqual(configured, legacy) {
			t.Fatalf("%s artifact differs under default policy", stage)
		}
		legacyUpstream = legacy
		policyUpstream = configured
	}
}

func TestRuntimePolicyDisablesRuleInDetectAndNormalize(t *testing.T) {
	input := testInput("disabled.go", "package sample", "// TODO disabled")
	policy := DefaultRuntimePolicy()
	setRuntimeRule(t, &policy, RuleUnfinishedWork, func(rule *RuntimeRule) {
		rule.Enabled = false
	})

	detected, err := ExecuteStageWithPolicy(
		context.Background(), input, StageDetect, detectUpstream(t, input, policy), policy,
	)
	if err != nil {
		t.Fatalf("configured detect error = %v", err)
	}
	if len(detected.Output.CandidateFindings) != 0 {
		t.Fatalf("disabled detect candidates = %+v", detected.Output.CandidateFindings)
	}

	defaultDetected, err := Detect(context.Background(), input)
	if err != nil {
		t.Fatalf("default Detect() error = %v", err)
	}
	normalized, err := ExecuteStageWithPolicy(
		context.Background(), input, StageNormalize, defaultDetected, policy,
	)
	if err != nil {
		t.Fatalf("configured normalize error = %v", err)
	}
	if len(normalized.Output.Findings) != 0 {
		t.Fatalf("disabled normalize findings = %+v", normalized.Output.Findings)
	}
}

func TestRuntimePolicyOverridesFindingSeverity(t *testing.T) {
	policy := DefaultRuntimePolicy()
	setRuntimeRule(t, &policy, RuleExplicitBugMarker, func(rule *RuntimeRule) {
		rule.Severity = "critical"
	})
	stages := runStagesWithPolicy(
		t,
		testInput("severity.go", "package sample", "// ARGUS_BUG severity"),
		policy,
	)
	finding := stages[1].Output.Findings[0]
	if finding.Severity != "critical" {
		t.Fatalf("configured severity = %q, want critical", finding.Severity)
	}
}

func TestRuntimePolicyAppliesLanguageAndPathPrefixes(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		languages []string
		prefixes  []string
		want      int
	}{
		{
			name: "language matches", path: "internal/review.go",
			languages: []string{"go"}, prefixes: []string{}, want: 1,
		},
		{
			name: "language excluded", path: "internal/review.go",
			languages: []string{"java"}, prefixes: []string{}, want: 0,
		},
		{
			name: "path matches", path: "internal/review.go",
			languages: []string{"go"}, prefixes: []string{"internal"}, want: 1,
		},
		{
			name: "path excluded", path: "cmd/review.go",
			languages: []string{"go"}, prefixes: []string{"internal"}, want: 0,
		},
		{
			name: "segment boundary enforced", path: "internalized/review.go",
			languages: []string{"go"}, prefixes: []string{"internal"}, want: 0,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := DefaultRuntimePolicy()
			setRuntimeRule(t, &policy, RuleUnfinishedWork, func(rule *RuntimeRule) {
				rule.Languages = test.languages
				rule.PathPrefixes = test.prefixes
			})
			detected, err := ExecuteStageWithPolicy(
				context.Background(),
				testInput(test.path, "package sample", "// TODO applicable"),
				StageDetect,
				detectUpstream(
					t,
					testInput(test.path, "package sample", "// TODO applicable"),
					policy,
				),
				policy,
			)
			if err != nil {
				t.Fatalf("configured detect error = %v", err)
			}
			if got := len(detected.Output.CandidateFindings); got != test.want {
				t.Fatalf("candidates = %d, want %d", got, test.want)
			}

			defaultDetected, err := Detect(
				context.Background(),
				testInput(test.path, "package sample", "// TODO applicable"),
			)
			if err != nil {
				t.Fatalf("default Detect() error = %v", err)
			}
			normalized, err := ExecuteStageWithPolicy(
				context.Background(),
				testInput(test.path, "package sample", "// TODO applicable"),
				StageNormalize,
				defaultDetected,
				policy,
			)
			if err != nil {
				t.Fatalf("configured normalize error = %v", err)
			}
			if got := len(normalized.Output.Findings); got != test.want {
				t.Fatalf("normalized findings = %d, want %d", got, test.want)
			}
		})
	}
}

func TestRuntimePolicySupportsClosedSeverityScale(t *testing.T) {
	for _, severity := range []string{"info", "low", "medium", "high", "critical"} {
		t.Run(severity, func(t *testing.T) {
			policy := DefaultRuntimePolicy()
			policy.MinimumSeverity = severity
			setRuntimeRule(t, &policy, RuleExplicitBugMarker, func(rule *RuntimeRule) {
				rule.Severity = severity
			})
			if err := policy.Validate(); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			stages := runStagesWithPolicy(
				t,
				testInput("scale.go", "package sample", "// ARGUS_BUG scale"),
				policy,
			)
			if got := stages[1].Output.Findings[0].Severity; got != severity {
				t.Fatalf("finding severity = %q, want %q", got, severity)
			}
			if got := stages[3].Output.Decisions[0].Action; got != DecisionPublish {
				t.Fatalf("decision = %q, want publish", got)
			}
		})
	}
}

func TestRuntimePolicyMinimumSeverityRejectsWithoutDroppingFinding(t *testing.T) {
	policy := DefaultRuntimePolicy()
	policy.MinimumSeverity = "high"
	stages := runStagesWithPolicy(
		t,
		testInput("minimum.go", "package sample", "// TODO retained"),
		policy,
	)
	adjudicated := stages[3]
	if len(adjudicated.Output.Findings) != 1 ||
		len(adjudicated.Output.Decisions) != 1 {
		t.Fatalf("adjudicated output dropped facts: %+v", adjudicated.Output)
	}
	finding := adjudicated.Output.Findings[0]
	decision := adjudicated.Output.Decisions[0]
	if finding.Verification.Status != VerificationVerified ||
		decision.Action != DecisionReject ||
		decision.ReasonCode != "below_minimum_severity" {
		t.Fatalf("minimum severity adjudication = finding %+v decision %+v", finding, decision)
	}
	report := stages[4].Output.Report
	if report.Summary.Findings != 1 ||
		report.Summary.Verified != 1 ||
		report.Summary.Publish != 0 {
		t.Fatalf("minimum severity report = %+v", report)
	}
}

func TestRuntimePolicyCountsOnlyDistinctRecognizedEvidenceSources(t *testing.T) {
	input := testInput("evidence.go", "package sample", "// TODO TODO duplicate evidence")
	tests := []struct {
		name       string
		kinds      []EvidenceKind
		minimum    int
		wantStatus VerificationStatus
	}{
		{
			name:  "duplicate same-kind source counts once",
			kinds: []EvidenceKind{EvidencePatchLine}, minimum: 2,
			wantStatus: VerificationInconclusive,
		},
		{
			name:  "different recognized kinds count independently",
			kinds: []EvidenceKind{EvidenceFileContent, EvidencePatchLine}, minimum: 2,
			wantStatus: VerificationVerified,
		},
		{
			name:  "unrecognized-by-policy evidence does not count",
			kinds: []EvidenceKind{EvidenceTargetLine}, minimum: 1,
			wantStatus: VerificationInconclusive,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := DefaultRuntimePolicy()
			policy.RequiredEvidenceKinds = test.kinds
			policy.MinimumIndependentEvidence = test.minimum
			stages := runStagesWithPolicy(t, input, policy)
			finding := stages[2].Output.Findings[0]
			if finding.Verification.Status != test.wantStatus {
				t.Fatalf("verification = %+v, want status %q", finding.Verification, test.wantStatus)
			}
			if test.wantStatus == VerificationInconclusive &&
				finding.Verification.ReasonCode != "insufficient_independent_evidence" {
				t.Fatalf("inconclusive reason = %q", finding.Verification.ReasonCode)
			}
		})
	}
}

func TestRuntimePolicyEvidenceIndependenceUsesKindAndSourceDigest(t *testing.T) {
	content := "package sample\n// TODO selected\n"
	digest := digestString(content)
	input := ReviewInput{
		SchemaVersion:  ReviewInputSchemaVersion,
		TargetID:       "target-evidence-independence",
		TargetMode:     TargetModeSelection,
		CanonicalPatch: "",
		Regions: []ReviewRegion{{
			Path: "selected.go", StartLine: 2, EndLine: 2, SHA256: digest,
		}},
		Files: []FileManifestEntry{{
			Path: "selected.go", SHA256: digest,
			SizeBytes: int64(len(content)), Content: &content,
		}},
		Contexts: []ContextBinding{},
	}
	policy := DefaultRuntimePolicy()
	policy.RequiredEvidenceKinds = []EvidenceKind{
		EvidenceFileContent,
		EvidenceTargetLine,
	}
	policy.MinimumIndependentEvidence = 2
	stages := runStagesWithPolicy(t, input, policy)
	finding := stages[2].Output.Findings[0]
	if finding.Verification.Status != VerificationVerified {
		t.Fatalf(
			"same source digest with distinct evidence kinds must count independently: %+v",
			finding.Verification,
		)
	}
}

func TestRuntimePolicyPartialTargetBlocksPublishWithoutDroppingFinding(t *testing.T) {
	tests := []struct {
		name        string
		allow       bool
		humanReview bool
		wantAction  DecisionAction
		wantReason  string
	}{
		{
			name:  "partial evidence requires human review",
			allow: false, humanReview: true,
			wantAction: DecisionHumanReview,
			wantReason: "partial_target_requires_human_review",
		},
		{
			name:  "partial evidence rejected when human review disabled",
			allow: false, humanReview: false,
			wantAction: DecisionReject,
			wantReason: "partial_target_evidence_disallowed",
		},
		{
			name:  "explicit partial evidence allowance publishes",
			allow: true, humanReview: true,
			wantAction: DecisionPublish,
			wantReason: "patch_and_content_confirmed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := DefaultRuntimePolicy()
			policy.TargetComplete = false
			policy.AllowPartialEvidence = test.allow
			policy.HumanReviewInconclusive = test.humanReview
			stages := runStagesWithPolicy(
				t,
				testInput("partial.go", "package sample", "// TODO retained"),
				policy,
			)
			adjudicated := stages[3]
			if len(adjudicated.Output.Findings) != 1 ||
				len(adjudicated.Output.Decisions) != 1 {
				t.Fatalf("partial target adjudication dropped facts: %+v", adjudicated.Output)
			}
			if adjudicated.Output.Findings[0].Verification.Status != VerificationVerified {
				t.Fatalf("partial target changed verification fact: %+v", adjudicated.Output.Findings[0])
			}
			decision := adjudicated.Output.Decisions[0]
			if decision.Action != test.wantAction || decision.ReasonCode != test.wantReason {
				t.Fatalf("decision = %+v, want %s/%s", decision, test.wantAction, test.wantReason)
			}
		})
	}
}

func TestRuntimePolicyPartialTargetDoesNotOverrideRejectedVerification(t *testing.T) {
	policy := DefaultRuntimePolicy()
	policy.TargetComplete = false
	input := testInput(
		"rejected-partial.go",
		"package sample",
		`const marker = "// TODO is not a comment"`,
	)
	stages := runStagesWithPolicy(t, input, policy)
	decision := stages[3].Output.Decisions[0]
	if decision.Action != DecisionReject || decision.ReasonCode != "marker_not_in_comment" {
		t.Fatalf("rejected verification decision = %+v", decision)
	}
}

func TestRuntimePolicyRoutesInconclusiveFindingExplicitly(t *testing.T) {
	input := testInput("inconclusive.go", "package sample", "// TODO unavailable")
	input.Files[0].Content = nil
	tests := []struct {
		name        string
		humanReview bool
		wantAction  DecisionAction
		wantReason  string
	}{
		{
			name: "human review enabled", humanReview: true,
			wantAction: DecisionHumanReview, wantReason: "content_unavailable",
		},
		{
			name: "human review disabled", humanReview: false,
			wantAction: DecisionReject, wantReason: "inconclusive_human_review_disabled",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := DefaultRuntimePolicy()
			policy.HumanReviewInconclusive = test.humanReview
			stages := runStagesWithPolicy(t, input, policy)
			adjudicated := stages[3]
			if len(adjudicated.Output.Findings) != 1 ||
				len(adjudicated.Output.Decisions) != 1 {
				t.Fatalf("inconclusive adjudication dropped facts: %+v", adjudicated.Output)
			}
			decision := adjudicated.Output.Decisions[0]
			if decision.Action != test.wantAction || decision.ReasonCode != test.wantReason {
				t.Fatalf("decision = %+v, want %s/%s", decision, test.wantAction, test.wantReason)
			}
		})
	}
}

func TestRuntimePolicyRejectsUnknownEnabledRule(t *testing.T) {
	policy := DefaultRuntimePolicy()
	policy.Rules = append(policy.Rules, RuntimeRule{
		RuleID: "unknown-rule", DetectorID: detectorID, DetectorRevision: detectorRevision,
		Enabled: true, Severity: "high",
		Languages: []string{"go"}, PathPrefixes: []string{},
	})
	_, err := ExecuteStageWithPolicy(
		context.Background(),
		testInput("unknown.go", "package sample", "// TODO unknown"),
		StageDetect,
		detectUpstream(
			t,
			testInput("unknown.go", "package sample", "// TODO unknown"),
			DefaultRuntimePolicy(),
		),
		policy,
	)
	if err == nil || !strings.Contains(err.Error(), `enabled runtime rule "unknown-rule" is unsupported`) {
		t.Fatalf("unknown enabled rule error = %v", err)
	}

	policy.Rules[len(policy.Rules)-1].Enabled = false
	if err := policy.Validate(); err != nil {
		t.Fatalf("disabled unknown rule must be inert, Validate() error = %v", err)
	}
}

func TestRuntimePolicyValidationRejectsMalformedSetsAndSeverity(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*RuntimePolicy)
		match  string
	}{
		{
			name: "nil rules",
			mutate: func(policy *RuntimePolicy) {
				policy.Rules = nil
			},
			match: "explicit array",
		},
		{
			name: "unsupported minimum severity",
			mutate: func(policy *RuntimePolicy) {
				policy.MinimumSeverity = "urgent"
			},
			match: "minimum severity",
		},
		{
			name: "nil required evidence kinds",
			mutate: func(policy *RuntimePolicy) {
				policy.RequiredEvidenceKinds = nil
			},
			match: "required evidence kinds",
		},
		{
			name: "unsorted required evidence kinds",
			mutate: func(policy *RuntimePolicy) {
				policy.RequiredEvidenceKinds = []EvidenceKind{
					EvidenceTargetLine,
					EvidenceFileContent,
				}
			},
			match: "must be sorted",
		},
		{
			name: "duplicate required evidence kind",
			mutate: func(policy *RuntimePolicy) {
				policy.RequiredEvidenceKinds = []EvidenceKind{
					EvidenceFileContent,
					EvidenceFileContent,
				}
			},
			match: "contain duplicate",
		},
		{
			name: "unknown required evidence kind",
			mutate: func(policy *RuntimePolicy) {
				policy.RequiredEvidenceKinds = []EvidenceKind{"llm_claim"}
			},
			match: "unsupported",
		},
		{
			name: "non-positive independent evidence",
			mutate: func(policy *RuntimePolicy) {
				policy.MinimumIndependentEvidence = 0
			},
			match: "must be positive",
		},
		{
			name: "unsupported detector",
			mutate: func(policy *RuntimePolicy) {
				policy.Rules[0].DetectorID = "another-detector"
			},
			match: "unsupported detector",
		},
		{
			name: "nil languages",
			mutate: func(policy *RuntimePolicy) {
				policy.Rules[0].Languages = nil
			},
			match: "languages must be",
		},
		{
			name: "unsafe path prefix",
			mutate: func(policy *RuntimePolicy) {
				policy.Rules[0].PathPrefixes = []string{"../internal"}
			},
			match: "path prefix",
		},
		{
			name: "duplicate rule",
			mutate: func(policy *RuntimePolicy) {
				policy.Rules = append(policy.Rules, policy.Rules[0])
			},
			match: "duplicate rule",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := DefaultRuntimePolicy()
			test.mutate(&policy)
			if err := policy.Validate(); err == nil || !strings.Contains(err.Error(), test.match) {
				t.Fatalf("Validate() error = %v, want %q", err, test.match)
			}
		})
	}
}

func runStagesWithPolicy(
	t *testing.T,
	input ReviewInput,
	policy RuntimePolicy,
) []StageResult {
	t.Helper()
	var upstream StageResult
	stages := make([]StageResult, 0, 5)
	for _, stage := range orderedStages {
		result, err := ExecuteStageWithPolicy(
			context.Background(), input, stage, upstream, policy,
		)
		if err != nil {
			t.Fatalf("ExecuteStageWithPolicy(%s) error = %v", stage, err)
		}
		switch stage {
		case StageDetect, StageNormalize, StageVerify, StageAdjudicate, StageReport:
			stages = append(stages, result)
		}
		upstream = result
	}
	return stages
}

func detectUpstream(
	t *testing.T,
	input ReviewInput,
	policy RuntimePolicy,
) StageResult {
	t.Helper()
	var upstream StageResult
	for _, stage := range []StageName{StageMaterializeTarget, StagePlanContext} {
		result, err := ExecuteStageWithPolicy(
			context.Background(), input, stage, upstream, policy,
		)
		if err != nil {
			t.Fatalf("ExecuteStageWithPolicy(%s) error = %v", stage, err)
		}
		upstream = result
	}
	return upstream
}

func setRuntimeRule(
	t *testing.T,
	policy *RuntimePolicy,
	ruleID string,
	mutate func(*RuntimeRule),
) {
	t.Helper()
	for index := range policy.Rules {
		if policy.Rules[index].RuleID == ruleID {
			mutate(&policy.Rules[index])
			return
		}
	}
	t.Fatalf("default runtime policy is missing %q", ruleID)
}
