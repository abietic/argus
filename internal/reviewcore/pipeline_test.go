package reviewcore

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestRunVerifiedFindingAndDualReport(t *testing.T) {
	input := testInput("review.go",
		"package sample",
		"// TODO: replace the fixture",
		"func review() {}",
	)

	result, err := Run(context.Background(), input, RunOptions{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(result.Stages) != len(orderedStages) {
		t.Fatalf("Run() stages = %d, want %d", len(result.Stages), len(orderedStages))
	}
	if result.Report.Summary != (ReportSummary{
		Findings: 1, Verified: 1, Publish: 1,
	}) {
		t.Fatalf("Run() summary = %+v", result.Report.Summary)
	}
	finding := result.Report.Findings[0]
	if finding.Verification.Status != VerificationVerified {
		t.Fatalf("verification = %q", finding.Verification.Status)
	}
	if result.Report.Decisions[0].Action != DecisionPublish {
		t.Fatalf("decision = %q", result.Report.Decisions[0].Action)
	}
	if !strings.Contains(string(result.ReportJSON), `"status": "verified"`) {
		t.Fatalf("JSON report does not contain verified finding:\n%s", result.ReportJSON)
	}
	if !strings.Contains(result.ReportMarkdown, "| verified | publish |") {
		t.Fatalf("Markdown report does not contain verified finding:\n%s", result.ReportMarkdown)
	}
	if _, err := DecodeReport(result.ReportJSON); err != nil {
		t.Fatalf("DecodeReport(report JSON) error = %v", err)
	}
}

func TestDetectsSupportedMarkers(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		wantRule string
	}{
		{name: "todo", line: "// TODO later", wantRule: "unfinished-work"},
		{name: "fixme", line: "// FIXME later", wantRule: "unfinished-work"},
		{name: "explicit bug", line: "// ARGUS_BUG: broken", wantRule: "explicit-bug-marker"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			detected, err := Detect(
				context.Background(),
				testInput("marker.go", "package sample", test.line),
			)
			if err != nil {
				t.Fatalf("Detect() error = %v", err)
			}
			if len(detected.Output.CandidateFindings) != 1 {
				t.Fatalf("candidates = %d, want 1", len(detected.Output.CandidateFindings))
			}
			if got := detected.Output.CandidateFindings[0].RuleID; got != test.wantRule {
				t.Fatalf("rule = %q, want %q", got, test.wantRule)
			}
		})
	}
}

func TestRunSelectionReviewsOnlyFrozenRegion(t *testing.T) {
	content := "package sample\n// TODO outside selection\n// ARGUS_BUG inside selection\n"
	digest := digestString(content)
	input := ReviewInput{
		SchemaVersion:  ReviewInputSchemaVersion,
		TargetID:       "target-selection",
		TargetMode:     TargetModeSelection,
		CanonicalPatch: "",
		Regions: []ReviewRegion{{
			Path: "selected.go", StartLine: 3, EndLine: 3, SHA256: digest,
		}},
		Files: []FileManifestEntry{{
			Path: "selected.go", SHA256: digest, SizeBytes: int64(len(content)), Content: &content,
		}},
		Contexts: []ContextBinding{},
	}
	result, err := Run(context.Background(), input, RunOptions{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Report.Summary.Verified != 1 ||
		len(result.Report.Findings) != 1 ||
		result.Report.Findings[0].StartLine != 3 ||
		result.Report.Findings[0].RuleID != "explicit-bug-marker" ||
		result.Report.Findings[0].Evidence[0].Kind != EvidenceTargetLine {
		t.Fatalf("selection report = %+v", result.Report)
	}
}

func TestContextCoverageCannotExpandSelectionFindingAnchors(t *testing.T) {
	content := "package sample\n// TODO context-only line\n// ARGUS_BUG selected line\n"
	digest := digestString(content)
	contextDigest := digestString("frozen external context")
	input := ReviewInput{
		SchemaVersion:  ReviewInputSchemaVersion,
		TargetID:       "target-selection-with-context",
		TargetMode:     TargetModeSelection,
		CanonicalPatch: "",
		Regions: []ReviewRegion{{
			Path: "selected.go", StartLine: 3, EndLine: 3, SHA256: digest,
		}},
		Files: []FileManifestEntry{{
			Path: "selected.go", SHA256: digest, SizeBytes: int64(len(content)), Content: &content,
		}},
		Contexts: []ContextBinding{{
			Ref: &ContextRef{
				ContextID: "context-repository-search-1",
				Kind:      "repository_search",
				Revision:  "commit-1",
				Digest:    contextDigest,
				Coverage: ContextCoverage{
					Spans: []ContextSpan{{
						Path: "selected.go", StartLine: 2, EndLine: 2,
					}},
					Symbols: []string{},
				},
				Provenance: ContextProvenance{
					Provider: "local-test", ProducerID: "fixture",
					ProducerRevision: "1",
				},
				ArtifactURI: "artifact://argus/contexts/repository-search-1",
				Contract:    "argus.context.repository_search.v1alpha1",
				SizeBytes:   int64(len("frozen external context")),
			},
		}},
	}
	result, err := Run(context.Background(), input, RunOptions{})
	if err != nil {
		t.Fatalf("Run(selection with context) error = %v", err)
	}
	if len(result.Report.Findings) != 1 || result.Report.Findings[0].StartLine != 3 {
		t.Fatalf("context coverage expanded findings: %+v", result.Report.Findings)
	}

	targetDigest, err := DigestReviewInput(input)
	if err != nil {
		t.Fatalf("DigestReviewInput() error = %v", err)
	}
	excerpt := "// TODO context-only line"
	candidateID := stableID(
		"candidate", detectorID, detectorRevision, targetDigest, RuleUnfinishedWork,
		string(DetectionSignalLexicalMarker), "selected.go", "2", "3", excerpt,
	)
	forged := CandidateFinding{
		ID: candidateID, DetectorID: detectorID, DetectorRevision: detectorRevision,
		RuleID: RuleUnfinishedWork, TargetDigest: targetDigest, Path: "selected.go",
		StartLine: 2, EndLine: 2, SignalKind: DetectionSignalLexicalMarker,
		Signal: "TODO", SignalOffset: 3, Excerpt: excerpt,
		Evidence: []Evidence{{
			ID:   stableID("evidence", string(EvidenceTargetLine), candidateID),
			Kind: EvidenceTargetLine, SourceDigest: digest, Path: "selected.go",
			Line: 2, Excerpt: excerpt, Claim: "marker_occurs_on_target_line",
		}},
	}
	detected, err := newStageResult(
		StageDetect,
		targetDigest,
		targetDigest,
		StageOutput{
			CandidateFindings: []CandidateFinding{forged},
			DetectionGaps:     []DetectionGap{},
		},
	)
	if err != nil {
		t.Fatalf("newStageResult(forged context candidate) error = %v", err)
	}
	if _, err := ExecuteStage(
		context.Background(),
		input,
		StageNormalize,
		detected,
	); err == nil || !strings.Contains(err.Error(), "outside the frozen target") {
		t.Fatalf("ExecuteStage(normalize context anchor) error = %v", err)
	}
}

func TestReviewInputRejectsAmbiguousContextBinding(t *testing.T) {
	input := testInput("review.go", "package sample", "// TODO selected")
	digest := digestString("context request")
	ref := ContextRef{
		ContextID: "context-1", Kind: "repository_search", Revision: "commit-1",
		Digest: digest,
		Coverage: ContextCoverage{
			Spans:   []ContextSpan{{Path: "other.go", StartLine: 1, EndLine: 1}},
			Symbols: []string{},
		},
		Provenance: ContextProvenance{
			Provider: "local-test", ProducerID: "fixture", ProducerRevision: "1",
		},
		ArtifactURI: "artifact://argus/contexts/1",
		Contract:    "argus.context.repository_search.v1alpha1",
		SizeBytes:   1,
	}
	gap := ContextGap{
		ContextID: "context-1", Kind: "repository_search", Revision: "commit-1",
		Digest: digest, Coverage: ref.Coverage, Provenance: ref.Provenance,
		ReasonCode: "provider_unavailable",
	}
	input.Contexts = []ContextBinding{{Ref: &ref, Gap: &gap}}
	if err := input.Validate(); err == nil ||
		!strings.Contains(err.Error(), "exactly one ref or gap") {
		t.Fatalf("ReviewInput.Validate(ambiguous context) error = %v", err)
	}
}

func TestRunScopeReviewsEveryEnumeratedRegion(t *testing.T) {
	first := "package first\n// TODO first\n"
	second := "package second\n// ARGUS_BUG second\n"
	firstDigest := digestString(first)
	secondDigest := digestString(second)
	input := ReviewInput{
		SchemaVersion:  ReviewInputSchemaVersion,
		TargetID:       "target-scope",
		TargetMode:     TargetModeScope,
		CanonicalPatch: "",
		Regions: []ReviewRegion{
			{Path: "a.go", StartLine: 1, EndLine: 2, SHA256: firstDigest},
			{Path: "nested/b.go", StartLine: 1, EndLine: 2, SHA256: secondDigest},
		},
		Files: []FileManifestEntry{
			{Path: "a.go", SHA256: firstDigest, SizeBytes: int64(len(first)), Content: &first},
			{
				Path: "nested/b.go", SHA256: secondDigest,
				SizeBytes: int64(len(second)), Content: &second,
			},
		},
		Contexts: []ContextBinding{},
	}
	result, err := Run(context.Background(), input, RunOptions{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Report.Summary.Verified != 2 ||
		result.Report.Summary.Publish != 2 ||
		len(result.Report.Findings) != 2 {
		t.Fatalf("scope report = %+v", result.Report)
	}
}

func TestDetectScopeIndexesFrozenContentOnce(t *testing.T) {
	content := strings.Repeat("x\n", 5_000) + "// TODO bounded scan\n"
	digest := digestString(content)
	input := ReviewInput{
		SchemaVersion:  ReviewInputSchemaVersion,
		TargetID:       "target-large-scope",
		TargetMode:     TargetModeScope,
		CanonicalPatch: "",
		Regions: []ReviewRegion{{
			Path: "large.go", StartLine: 1, EndLine: 5_001, SHA256: digest,
		}},
		Files: []FileManifestEntry{{
			Path: "large.go", SHA256: digest,
			SizeBytes: int64(len(content)), Content: &content,
		}},
		Contexts: []ContextBinding{},
	}

	detected, err := Detect(context.Background(), input)
	if err != nil {
		t.Fatalf("Detect() error = %v", err)
	}
	if len(detected.Output.CandidateFindings) != 1 ||
		detected.Output.CandidateFindings[0].StartLine != 5_001 {
		t.Fatalf("candidates = %+v", detected.Output.CandidateFindings)
	}
}

func TestDetectSkipsNonPortablePatchPathButReviewsSafeFile(t *testing.T) {
	input := ReviewInput{
		SchemaVersion: ReviewInputSchemaVersion,
		TargetID:      "target-unsafe-path",
		TargetMode:    TargetModeDiff,
		CanonicalPatch: strings.Join([]string{
			"diff --git a/bad:name.go b/bad:name.go",
			"new file mode 100644",
			"--- /dev/null",
			"+++ b/bad:name.go",
			"@@ -0,0 +1,2 @@",
			"+package sample",
			"+// ARGUS_BUG must not escape source admission",
			"diff --git a/safe.go b/safe.go",
			"new file mode 100644",
			"--- /dev/null",
			"+++ b/safe.go",
			"@@ -0,0 +1,2 @@",
			"+package sample",
			"+// TODO review the admitted file",
			"",
		}, "\n"),
		Regions: []ReviewRegion{},
		Files: []FileManifestEntry{{
			Path: "safe.go", SHA256: digestString("package sample\n// TODO review the admitted file\n"),
			SizeBytes: int64(len("package sample\n// TODO review the admitted file\n")),
			Content:   stringPointer("package sample\n// TODO review the admitted file\n"),
		}},
		Contexts: []ContextBinding{},
	}
	detected, err := Detect(context.Background(), input)
	if err != nil {
		t.Fatalf("Detect() error = %v", err)
	}
	if len(detected.Output.CandidateFindings) != 1 ||
		detected.Output.CandidateFindings[0].Path != "safe.go" {
		t.Fatalf("candidates = %+v, want only safe.go", detected.Output.CandidateFindings)
	}
}

func TestVerifyRejectsMarkerInsideString(t *testing.T) {
	input := testInput("false_positive.go",
		"package sample",
		`const note = "// TODO is test data"`,
	)
	result, err := Run(context.Background(), input, RunOptions{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	finding := result.Report.Findings[0]
	if finding.Verification.Status != VerificationRejected ||
		finding.Verification.ReasonCode != "marker_not_in_comment" {
		t.Fatalf("verification = %+v", finding.Verification)
	}
	if result.Report.Decisions[0].Action != DecisionReject {
		t.Fatalf("decision = %q, want reject", result.Report.Decisions[0].Action)
	}
}

func TestVerifyRejectsPatchAndFinalContentMismatch(t *testing.T) {
	input := testInput("mismatch.go", "package sample", "// TODO from patch")
	content := "package sample\n// completed in final content\n"
	input.Files[0].Content = &content
	input.Files[0].SizeBytes = int64(len(content))
	input.Files[0].SHA256 = digestString(content)

	result, err := Run(context.Background(), input, RunOptions{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	finding := result.Report.Findings[0]
	if finding.Verification.Status != VerificationRejected ||
		finding.Verification.ReasonCode != "content_mismatch" {
		t.Fatalf("verification = %+v", finding.Verification)
	}
}

func TestVerifyIsInconclusiveWithoutFileContent(t *testing.T) {
	input := testInput("unknown.go", "package sample", "// FIXME: unresolved")
	input.Files[0].Content = nil

	result, err := Run(context.Background(), input, RunOptions{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	finding := result.Report.Findings[0]
	if finding.Verification.Status != VerificationInconclusive ||
		finding.Verification.ReasonCode != "content_unavailable" {
		t.Fatalf("verification = %+v", finding.Verification)
	}
	if result.Report.Decisions[0].Action != DecisionHumanReview {
		t.Fatalf("decision = %q, want human_review", result.Report.Decisions[0].Action)
	}
}

func TestRunNoFinding(t *testing.T) {
	input := testInput("clean.go", "package sample", "func clean() {}")
	result, err := Run(context.Background(), input, RunOptions{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Report.TargetDigest != result.InputDigest {
		t.Fatalf("empty report target = %q, want %q", result.Report.TargetDigest, result.InputDigest)
	}
	if result.Report.Summary != (ReportSummary{}) {
		t.Fatalf("summary = %+v, want zero", result.Report.Summary)
	}
	if result.Report.Findings == nil || result.Report.Decisions == nil {
		t.Fatal("empty report arrays must be explicit")
	}
	if !strings.Contains(result.ReportMarkdown, "No findings.") {
		t.Fatalf("Markdown report =\n%s", result.ReportMarkdown)
	}
}

func TestRunMetadataOnlyPatchProducesEmptyReport(t *testing.T) {
	input := ReviewInput{
		SchemaVersion: ReviewInputSchemaVersion,
		TargetID:      "target-rename",
		TargetMode:    TargetModeDiff,
		CanonicalPatch: strings.Join([]string{
			"diff --git a/old.go b/new.go",
			"similarity index 100%",
			"rename from old.go",
			"rename to new.go",
			"",
		}, "\n"),
		Regions:  []ReviewRegion{},
		Files:    []FileManifestEntry{},
		Contexts: []ContextBinding{},
	}
	result, err := Run(context.Background(), input, RunOptions{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Report.Summary != (ReportSummary{}) ||
		len(result.Report.Findings) != 0 ||
		len(result.Report.Decisions) != 0 {
		t.Fatalf("metadata-only report = %+v", result.Report)
	}
}

func TestInspectCanonicalPatchKeepsPortableSpaceAndUnicodePaths(t *testing.T) {
	tests := []struct {
		name  string
		patch string
		path  string
		hunks int
	}{
		{
			name: "space mode only",
			patch: strings.Join([]string{
				"diff --git a/foo bar.go b/foo bar.go",
				"old mode 100644",
				"new mode 100755",
				"",
			}, "\n"),
			path: "foo bar.go",
		},
		{
			name: "unicode text hunk",
			patch: strings.Join([]string{
				"diff --git a/你好.go b/你好.go",
				"--- a/你好.go",
				"+++ b/你好.go",
				"@@ -1 +1,2 @@",
				" package fixture",
				"+// changed",
				"",
			}, "\n"),
			path:  "你好.go",
			hunks: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inspection, err := InspectCanonicalPatch(context.Background(), test.patch)
			if err != nil {
				t.Fatalf("InspectCanonicalPatch() error = %v", err)
			}
			if len(inspection.Files) != 1 ||
				!inspection.Files[0].PathKnown ||
				inspection.Files[0].Path != test.path ||
				inspection.Files[0].Hunks != test.hunks {
				t.Fatalf("inspection = %+v", inspection)
			}
		})
	}
}

func TestNormalizeDeduplicatesButRetainsLineage(t *testing.T) {
	input := testInput("duplicate.go", "package sample", "// TODO TODO")
	detected, err := Detect(context.Background(), input)
	if err != nil {
		t.Fatalf("Detect() error = %v", err)
	}
	if len(detected.Output.CandidateFindings) != 2 {
		t.Fatalf("candidates = %d, want 2", len(detected.Output.CandidateFindings))
	}
	normalized, err := Normalize(context.Background(), detected)
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	if len(normalized.Output.Findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(normalized.Output.Findings))
	}
	lineage := normalized.Output.Findings[0].Lineage
	if len(lineage) != 2 {
		t.Fatalf("lineage = %d, want 2", len(lineage))
	}
	if lineage[0].Disposition != LineageWinner || lineage[1].Disposition != LineageDuplicate {
		t.Fatalf("lineage dispositions = %+v", lineage)
	}
}

func TestRunHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, testInput("cancel.go", "package sample", "// TODO later"), RunOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
}

func TestStableIDsAndArtifacts(t *testing.T) {
	input := testInput("stable.go",
		"package sample",
		"// ARGUS_BUG: stable fixture",
	)
	first, err := Run(context.Background(), input, RunOptions{})
	if err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
	second, err := Run(context.Background(), input, RunOptions{})
	if err != nil {
		t.Fatalf("second Run() error = %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("identical input produced different result")
	}
	for index := range first.Stages {
		if first.Stages[index].ArtifactDigest != second.Stages[index].ArtifactDigest {
			t.Fatalf("stage %d digest changed", index)
		}
	}
}

func TestFindingIdentitySurvivesUnrelatedLineShift(t *testing.T) {
	before := testInput("shift.go",
		"package sample",
		"// TODO: stable across line movement",
		"func stable() {}",
	)
	after := testInput("shift.go",
		"package sample",
		"const unrelated = true",
		"// TODO: stable across line movement",
		"func stable() {}",
	)
	first, err := Run(context.Background(), before, RunOptions{})
	if err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
	second, err := Run(context.Background(), after, RunOptions{})
	if err != nil {
		t.Fatalf("second Run() error = %v", err)
	}
	firstFinding := first.Report.Findings[0]
	secondFinding := second.Report.Findings[0]
	if firstFinding.ID != secondFinding.ID ||
		firstFinding.Fingerprint != secondFinding.Fingerprint ||
		firstFinding.Anchor != secondFinding.Anchor {
		t.Fatalf(
			"identity changed after line shift:\nfirst=%s %s %s\nsecond=%s %s %s",
			firstFinding.ID, firstFinding.Fingerprint, firstFinding.Anchor,
			secondFinding.ID, secondFinding.Fingerprint, secondFinding.Anchor,
		)
	}
	if firstFinding.TargetDigest == secondFinding.TargetDigest {
		t.Fatal("fixture inputs unexpectedly have the same target digest")
	}
}

func TestExecuteStageSupportsDurableCheckpoints(t *testing.T) {
	input := testInput("checkpoint.go", "package sample", "// TODO checkpoint")
	var upstream StageResult
	checkpoints := make([]StageResult, 0, len(orderedStages))
	for _, stage := range orderedStages {
		next, err := ExecuteStage(context.Background(), input, stage, upstream)
		if err != nil {
			t.Fatalf("ExecuteStage(%s) error = %v", stage, err)
		}
		encoded, err := json.Marshal(next)
		if err != nil {
			t.Fatalf("marshal checkpoint: %v", err)
		}
		decoded, err := DecodeStageResult(encoded)
		if err != nil {
			t.Fatalf("decode checkpoint: %v", err)
		}
		checkpoints = append(checkpoints, decoded)
		upstream = decoded
	}
	full, err := Run(context.Background(), input, RunOptions{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !reflect.DeepEqual(checkpoints, full.Stages) {
		t.Fatal("single-stage checkpoints differ from Run path")
	}
}

func TestReplayReusesExactUpstreamArtifacts(t *testing.T) {
	input := testInput("replay.go", "package sample", "// TODO replay")
	original, err := Run(context.Background(), input, RunOptions{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	replayed, err := Run(context.Background(), input, RunOptions{
		StartAt:  StageVerify,
		Upstream: append([]StageResult(nil), original.Stages[:4]...),
	})
	if err != nil {
		t.Fatalf("replayed Run() error = %v", err)
	}
	if !reflect.DeepEqual(replayed.Report, original.Report) ||
		!reflect.DeepEqual(replayed.Stages, original.Stages) {
		t.Fatal("replayed result differs from original")
	}
	if !reflect.DeepEqual(replayed.ReusedStages, []StageName{
		StageMaterializeTarget,
		StagePlanContext,
		StageDetect,
		StageNormalize,
	}) {
		t.Fatalf("reused stages = %v", replayed.ReusedStages)
	}
}

func TestReplayRejectsTamperedOrForeignArtifacts(t *testing.T) {
	input := testInput("replay.go", "package sample", "// TODO replay")
	original, err := Run(context.Background(), input, RunOptions{})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	tampered := append([]StageResult(nil), original.Stages[:4]...)
	tampered[3].Output.Findings = cloneFindings(tampered[3].Output.Findings)
	tampered[3].Output.Findings[0].Title = "tampered"
	if _, err := Run(context.Background(), input, RunOptions{
		StartAt: StageVerify, Upstream: tampered,
	}); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("tampered replay error = %v", err)
	}

	foreign := testInput("replay.go", "package sample", "// FIXME another input")
	if _, err := ExecuteStage(
		context.Background(), foreign, StageNormalize, original.Stages[2],
	); err == nil || !strings.Contains(err.Error(), "another review input") {
		t.Fatalf("foreign artifact error = %v", err)
	}
}

func TestReportPreservesAppendOnlyDecisionHistory(t *testing.T) {
	result, err := Run(
		context.Background(),
		testInput("history.go", "package sample", "// TODO history"),
		RunOptions{},
	)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	report := result.Report
	first := report.Decisions[0]
	second := FindingDecision{
		FindingID: first.FindingID, Sequence: 2, PriorDecisionID: first.ID,
		Action: DecisionReject, ReasonCode: "policy_suppressed",
		EvidenceIDs: append([]string(nil), first.EvidenceIDs...),
	}
	second.ID = stableID(
		"decision", second.FindingID, "2", string(second.Action),
		second.ReasonCode, strings.Join(second.EvidenceIDs, ","),
	)
	report.Decisions = append(report.Decisions, second)
	report.Summary.Publish = 0
	if err := report.Validate(); err != nil {
		t.Fatalf("append-only Report.Validate() error = %v", err)
	}
}

func testInput(path string, lines ...string) ReviewInput {
	content := strings.Join(lines, "\n") + "\n"
	patchLines := make([]string, 0, len(lines)+5)
	patchLines = append(
		patchLines,
		"diff --git a/"+path+" b/"+path,
		"new file mode 100644",
		"--- /dev/null",
		"+++ b/"+path,
		"@@ -0,0 +1,"+strconv.Itoa(len(lines))+" @@",
	)
	for _, line := range lines {
		patchLines = append(patchLines, "+"+line)
	}
	patch := strings.Join(patchLines, "\n") + "\n"
	return ReviewInput{
		SchemaVersion:  ReviewInputSchemaVersion,
		TargetID:       "target-fixture",
		TargetMode:     TargetModeDiff,
		CanonicalPatch: patch,
		Regions:        []ReviewRegion{},
		Files: []FileManifestEntry{{
			Path: path, SHA256: digestString(content), SizeBytes: int64(len(content)), Content: &content,
		}},
		Contexts: []ContextBinding{},
	}
}

func stringPointer(value string) *string {
	return &value
}
