package reviewcore

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestGoASTDetectorFindsDiscardedContextCancelAndVerifies(t *testing.T) {
	tests := []struct {
		name   string
		call   string
		signal string
	}{
		{
			name:   "with cancel",
			call:   "ctx, _ := context.WithCancel(parent)",
			signal: "context.WithCancel",
		},
		{
			name:   "with timeout",
			call:   "ctx, _ := context.WithTimeout(parent, 0)",
			signal: "context.WithTimeout",
		},
		{
			name:   "with deadline",
			call:   "ctx, _ := context.WithDeadline(parent, time.Time{})",
			signal: "context.WithDeadline",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := testInput(
				"leak.go",
				"package sample",
				`import ("context"; "time")`,
				"func leak(parent context.Context) context.Context {",
				"\t"+test.call,
				"\treturn ctx",
				"}",
			)
			result, err := Run(context.Background(), input, RunOptions{})
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if len(result.Report.Findings) != 1 {
				t.Fatalf("findings = %+v, want one AST finding", result.Report.Findings)
			}
			finding := result.Report.Findings[0]
			if finding.RuleID != RuleGoContextCancelDiscarded ||
				finding.Verification.Status != VerificationVerified ||
				len(finding.Signals) != 1 ||
				finding.Signals[0] != test.signal {
				t.Fatalf("AST finding = %+v", finding)
			}
			if result.Report.Decisions[0].Action != DecisionPublish {
				t.Fatalf("decision = %+v, want publish", result.Report.Decisions[0])
			}
			detected := result.Stages[2]
			if len(detected.Output.DetectionGaps) != 0 ||
				len(detected.Output.CandidateFindings) != 1 {
				t.Fatalf("detect output = %+v", detected.Output)
			}
			candidate := detected.Output.CandidateFindings[0]
			if candidate.DetectorID != DetectorGoASTID ||
				candidate.DetectorRevision != DetectorGoASTRevision ||
				candidate.SignalKind != DetectionSignalGoASTPattern ||
				candidate.StartLine != 4 ||
				candidate.Evidence[0].Line != candidate.StartLine ||
				candidate.Evidence[0].SourceDigest != digestString(input.CanonicalPatch) {
				t.Fatalf("candidate provenance = %+v", candidate)
			}
		})
	}
}

func TestGoASTDetectorCleanCasesDoNotProduceCandidates(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
	}{
		{
			name: "cancel retained and invoked",
			lines: []string{
				"package sample",
				`import "context"`,
				"func clean(parent context.Context) context.Context {",
				"\tctx, cancel := context.WithCancel(parent)",
				"\tdefer cancel()",
				"\treturn ctx",
				"}",
			},
		},
		{
			name: "selector only occurs in string",
			lines: []string{
				"package sample",
				`const example = "ctx, _ := context.WithCancel(parent)"`,
			},
		},
		{
			name: "local context value shadows package",
			lines: []string{
				"package sample",
				`import "context"`,
				"type factory struct{}",
				"func (factory) WithCancel(any) (any, func()) { return nil, func() {} }",
				"func clean(context factory) {",
				"\t_, _ = context.WithCancel(nil)",
				"}",
			},
		},
		{
			name: "different tuple result discarded",
			lines: []string{
				"package sample",
				`import "context"`,
				"func clean(parent context.Context) {",
				"\t_, _ = other(parent)",
				"}",
				"func other(context.Context) (context.Context, func()) { return nil, nil }",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			detected, err := Detect(context.Background(), testInput("clean.go", test.lines...))
			if err != nil {
				t.Fatalf("Detect() error = %v", err)
			}
			if len(detected.Output.CandidateFindings) != 0 ||
				len(detected.Output.DetectionGaps) != 0 {
				t.Fatalf("clean detect output = %+v", detected.Output)
			}
		})
	}
}

func TestGoASTDetectorNeverEscapesFrozenTarget(t *testing.T) {
	finalContent := strings.Join([]string{
		"package sample",
		`import "context"`,
		"func changed() int { return 2 }",
		"func leak(parent context.Context) context.Context {",
		"\tctx, _ := context.WithCancel(parent)",
		"\treturn ctx",
		"}",
	}, "\n") + "\n"
	diffInput := ReviewInput{
		SchemaVersion: ReviewInputSchemaVersion,
		TargetID:      "target-existing-diff",
		TargetMode:    TargetModeDiff,
		CanonicalPatch: strings.Join([]string{
			"diff --git a/boundary.go b/boundary.go",
			"--- a/boundary.go",
			"+++ b/boundary.go",
			"@@ -1,7 +1,7 @@",
			" package sample",
			` import "context"`,
			"-func changed() int { return 1 }",
			"+func changed() int { return 2 }",
			" func leak(parent context.Context) context.Context {",
			" \tctx, _ := context.WithCancel(parent)",
			" \treturn ctx",
			" }",
		}, "\n") + "\n",
		Regions: []ReviewRegion{},
		Files: []FileManifestEntry{{
			Path: "boundary.go", SHA256: digestString(finalContent),
			SizeBytes: int64(len(finalContent)), Content: &finalContent,
		}},
		Contexts: []ContextBinding{},
	}

	tests := []struct {
		name  string
		input ReviewInput
	}{
		{name: "diff", input: diffInput},
		{
			name: "selection",
			input: regionInput(
				TargetModeSelection,
				"boundary.go",
				finalContent,
				3,
				3,
			),
		},
		{
			name:  "scope",
			input: regionInput(TargetModeScope, "boundary.go", finalContent, 3, 3),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			detected, err := Detect(context.Background(), test.input)
			if err != nil {
				t.Fatalf("Detect() error = %v", err)
			}
			if len(detected.Output.CandidateFindings) != 0 {
				t.Fatalf("out-of-target candidates = %+v", detected.Output.CandidateFindings)
			}
		})
	}

	authorized := regionInput(TargetModeSelection, "boundary.go", finalContent, 5, 5)
	detected, err := Detect(context.Background(), authorized)
	if err != nil {
		t.Fatalf("Detect(authorized selection) error = %v", err)
	}
	if len(detected.Output.CandidateFindings) != 1 ||
		detected.Output.CandidateFindings[0].StartLine != 5 ||
		detected.Output.CandidateFindings[0].Evidence[0].Kind != EvidenceTargetLine ||
		detected.Output.CandidateFindings[0].Evidence[0].SourceDigest !=
			authorized.Files[0].SHA256 {
		t.Fatalf("authorized selection candidate = %+v", detected.Output.CandidateFindings)
	}
}

func TestGoASTDetectorRecordsConservativeCoverageGaps(t *testing.T) {
	unparseable := testInput(
		"broken.go",
		"package sample",
		`import "context"`,
		"func broken( {",
		"\tctx, _ := context.WithCancel(parent)",
	)
	detected, err := Detect(context.Background(), unparseable)
	if err != nil {
		t.Fatalf("Detect(unparseable) error = %v", err)
	}
	if len(detected.Output.CandidateFindings) != 0 ||
		len(detected.Output.DetectionGaps) != 1 ||
		detected.Output.DetectionGaps[0].ReasonCode != DetectionGapGoParseFailed ||
		detected.Output.DetectionGaps[0].SourceDigest != unparseable.Files[0].SHA256 {
		t.Fatalf("unparseable detect output = %+v", detected.Output)
	}

	unavailable := testInput(
		"unavailable.go",
		"package sample",
		`import "context"`,
		"func leak(parent context.Context) { _, _ = context.WithCancel(parent) }",
	)
	unavailable.Files[0].Content = nil
	detected, err = Detect(context.Background(), unavailable)
	if err != nil {
		t.Fatalf("Detect(unavailable) error = %v", err)
	}
	if len(detected.Output.DetectionGaps) != 1 ||
		detected.Output.DetectionGaps[0].ReasonCode != DetectionGapContentUnavailable {
		t.Fatalf("unavailable gaps = %+v", detected.Output.DetectionGaps)
	}
}

func TestGoASTDetectorCanBeDisabledWithoutDisablingMarkerBaseline(t *testing.T) {
	input := testInput(
		"disabled.go",
		"package sample",
		`import "context"`,
		"func leak(parent context.Context) {",
		"\t_, _ = context.WithCancel(parent)",
		"}",
		"// TODO retained baseline",
	)
	policy := DefaultRuntimePolicy()
	setRuntimeRule(t, &policy, RuleGoContextCancelDiscarded, func(rule *RuntimeRule) {
		rule.Enabled = false
	})
	detected, err := detectWithPolicy(context.Background(), input, policy)
	if err != nil {
		t.Fatalf("detectWithPolicy() error = %v", err)
	}
	if len(detected.Output.CandidateFindings) != 1 ||
		detected.Output.CandidateFindings[0].RuleID != RuleUnfinishedWork {
		t.Fatalf("disabled AST detector candidates = %+v", detected.Output.CandidateFindings)
	}
}

func TestCandidateAndDetectionGapStrictValidation(t *testing.T) {
	input := testInput(
		"strict.go",
		"package sample",
		`import "context"`,
		"func leak(parent context.Context) { _, _ = context.WithCancel(parent) }",
	)
	detected, err := Detect(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	candidate := detected.Output.CandidateFindings[0]
	encoded, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	unknown := []byte(strings.TrimSuffix(string(encoded), "}") + `,"unknown":true}`)
	if _, err := DecodeCandidateFinding(unknown); err == nil ||
		!strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown candidate field error = %v", err)
	}
	invalidKind := []byte(strings.Replace(
		string(encoded),
		`"signal_kind":"go_ast_pattern"`,
		`"signal_kind":"model_guess"`,
		1,
	))
	if _, err := DecodeCandidateFinding(invalidKind); err == nil ||
		!strings.Contains(err.Error(), "signal") {
		t.Fatalf("invalid signal kind error = %v", err)
	}
	forged := candidate
	forged.DetectorID = DetectorFixtureMarkerID
	if err := forged.Validate(); err == nil ||
		!strings.Contains(err.Error(), "detector identity") {
		t.Fatalf("forged detector identity error = %v", err)
	}

	broken := testInput("gap.go", "package sample", "func broken( {")
	withGap, err := Detect(context.Background(), broken)
	if err != nil {
		t.Fatal(err)
	}
	gapJSON, err := json.Marshal(withGap.Output.DetectionGaps[0])
	if err != nil {
		t.Fatal(err)
	}
	unknownGap := []byte(strings.TrimSuffix(string(gapJSON), "}") + `,"unknown":true}`)
	if _, err := DecodeDetectionGap(unknownGap); err == nil ||
		!strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown detection gap field error = %v", err)
	}
}

func regionInput(
	mode TargetMode,
	repositoryPath string,
	content string,
	startLine uint32,
	endLine uint32,
) ReviewInput {
	digest := digestString(content)
	return ReviewInput{
		SchemaVersion: ReviewInputSchemaVersion,
		TargetID:      "target-region",
		TargetMode:    mode,
		Regions: []ReviewRegion{{
			Path: repositoryPath, StartLine: startLine, EndLine: endLine, SHA256: digest,
		}},
		Files: []FileManifestEntry{{
			Path: repositoryPath, SHA256: digest,
			SizeBytes: int64(len(content)), Content: &content,
		}},
		Contexts: []ContextBinding{},
	}
}
