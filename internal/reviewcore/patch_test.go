package reviewcore

import (
	"context"
	"strings"
	"testing"
)

func TestInspectCanonicalPatchConsumesExactHunkBodyCounts(t *testing.T) {
	patch := testCanonicalPatch(
		"@@ -1,3 +1,4 @@",
		" context",
		"--- old content",
		noNewlineMarker,
		"+++ new content",
		noNewlineMarker,
		"+extra",
		" tail",
		"@@ -10,0 +12 @@ optional function context",
		"+inserted",
	)

	inspection, err := InspectCanonicalPatch(context.Background(), patch)
	if err != nil {
		t.Fatalf("InspectCanonicalPatch() error = %v", err)
	}
	if len(inspection.Files) != 1 ||
		inspection.Files[0].Path != "sample.go" ||
		inspection.Files[0].Hunks != 2 ||
		inspection.TotalHunks != 2 {
		t.Fatalf("inspection = %+v", inspection)
	}

	parsed, err := parseUnifiedDiffContext(context.Background(), patch)
	if err != nil {
		t.Fatalf("parseUnifiedDiffContext() error = %v", err)
	}
	if len(parsed.Added) != 3 {
		t.Fatalf("added lines = %+v, want 3", parsed.Added)
	}
	if parsed.Added[0].NewLine != 2 ||
		parsed.Added[0].Text != "++ new content" ||
		parsed.Added[1].NewLine != 3 ||
		parsed.Added[1].Text != "extra" ||
		parsed.Added[2].NewLine != 12 ||
		parsed.Added[2].Text != "inserted" {
		t.Fatalf("added lines = %+v", parsed.Added)
	}
}

func TestDeletionOnlyHunkAuthorizesOnlyItsSurvivingTargetContext(t *testing.T) {
	patch := testCanonicalPatch(
		"@@ -1,4 +1,2 @@",
		" before",
		"-guard",
		"-fallback",
		" after",
	)
	parsed, err := parseUnifiedDiffContext(context.Background(), patch)
	if err != nil {
		t.Fatalf("parseUnifiedDiffContext() error = %v", err)
	}
	if len(parsed.Added) != 0 || len(parsed.DeletionContextTargets) != 2 ||
		parsed.DeletionContextTargets[0].NewLine != 1 ||
		parsed.DeletionContextTargets[1].NewLine != 2 {
		t.Fatalf("parsed deletion context = %+v", parsed)
	}

	content := "before\nafter\n"
	input := ReviewInput{
		SchemaVersion:  ReviewInputSchemaVersion,
		TargetID:       "target-deletion-only",
		TargetMode:     TargetModeDiff,
		CanonicalPatch: patch,
		Regions:        []ReviewRegion{},
		Files: []FileManifestEntry{{
			Path: "sample.go", SHA256: digestString(content),
			SizeBytes: int64(len(content)), Content: &content,
		}},
		Contexts: []ContextBinding{},
	}
	for _, span := range [][2]uint32{{1, 1}, {2, 2}, {1, 2}} {
		if !InputAuthorizesAnchor(
			context.Background(), input, "sample.go", span[0], span[1],
		) {
			t.Fatalf("deletion-only target context %d-%d was not authorized", span[0], span[1])
		}
	}
	if InputAuthorizesAnchor(context.Background(), input, "sample.go", 3, 3) {
		t.Fatal("line outside deletion-only hunk context was authorized")
	}
}

func TestReplacementHunkDoesNotAuthorizeUnchangedTargetContext(t *testing.T) {
	patch := testCanonicalPatch(
		"@@ -1,3 +1,3 @@",
		" before",
		"-old",
		"+new",
		" after",
	)
	parsed, err := parseUnifiedDiffContext(context.Background(), patch)
	if err != nil {
		t.Fatalf("parseUnifiedDiffContext() error = %v", err)
	}
	if len(parsed.Added) != 1 || len(parsed.DeletionContextTargets) != 0 {
		t.Fatalf("replacement authorization = %+v", parsed)
	}
}

func TestInspectCanonicalPatchRejectsInvalidHunkStructure(t *testing.T) {
	tests := []struct {
		name string
		body []string
		want string
	}{
		{
			name: "old and new counts remain at EOF",
			body: []string{
				"@@ -1,2 +1,2 @@",
				" unchanged",
			},
			want: "reaches EOF before its declared counts are consumed",
		},
		{
			name: "new count remains at file boundary",
			body: []string{
				"@@ -0,0 +1,2 @@",
				"+first",
				"diff --git a/next.go b/next.go",
			},
			want: "starts a file boundary before hunk",
		},
		{
			name: "old count remains at next hunk",
			body: []string{
				"@@ -1,2 +1 @@",
				"-removed",
				"+replacement",
				"@@ -5 +5 @@",
			},
			want: "starts a new hunk before hunk",
		},
		{
			name: "addition exceeds declared counts",
			body: []string{
				"@@ -0,0 +1 @@",
				"+first",
				"+second",
			},
			want: "exceeds the declared counts",
		},
		{
			name: "deletion exceeds declared counts",
			body: []string{
				"@@ -1 +0,0 @@",
				"-first",
				"-second",
			},
			want: "exceeds the declared counts",
		},
		{
			name: "context consumes unavailable old line",
			body: []string{
				"@@ -1,0 +1 @@",
				" context",
			},
			want: "exceeds the declared context-line count",
		},
		{
			name: "body line has no unified diff prefix",
			body: []string{
				"@@ -1 +1 @@",
				"invalid body",
			},
			want: "is invalid inside hunk",
		},
		{
			name: "marker immediately follows header",
			body: []string{
				"@@ -1 +1 @@",
				noNewlineMarker,
			},
			want: "without a preceding hunk body line",
		},
		{
			name: "marker is repeated",
			body: []string{
				"@@ -0,0 +1 @@",
				"+line",
				noNewlineMarker,
				noNewlineMarker,
			},
			want: "without a preceding hunk body line",
		},
		{
			name: "malformed hunk header",
			body: []string{
				"@@ -1 +1 @",
			},
			want: "invalid hunk header",
		},
		{
			name: "empty hunk",
			body: []string{
				"@@ -1,0 +1,0 @@",
			},
			want: "declares an empty hunk",
		},
		{
			name: "old range overflows supported coordinates",
			body: []string{
				"@@ -4294967295,2 +1 @@",
			},
			want: "overflowing old-file range",
		},
		{
			name: "non-empty new range starts at zero",
			body: []string{
				"@@ -0,0 +0 @@",
			},
			want: "zero new-file start with a non-zero count",
		},
		{
			name: "body content appears before a hunk",
			body: []string{
				"+orphan",
			},
			want: "hunk body content outside a hunk",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := InspectCanonicalPatch(
				context.Background(),
				testCanonicalPatch(test.body...),
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("InspectCanonicalPatch() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestInspectCanonicalPatchDecodesQuotedGitPath(t *testing.T) {
	patch := strings.Join([]string{
		`diff --git "a/quo\"te.go" "b/quo\"te.go"`,
		"old mode 100644",
		"new mode 100755",
		"",
	}, "\n")

	inspection, err := InspectCanonicalPatch(context.Background(), patch)
	if err != nil {
		t.Fatalf("InspectCanonicalPatch() error = %v", err)
	}
	if len(inspection.Files) != 1 ||
		!inspection.Files[0].PathKnown ||
		inspection.Files[0].Path != `quo"te.go` ||
		inspection.Files[0].Hunks != 0 {
		t.Fatalf("inspection = %+v", inspection)
	}
}

func TestInspectCanonicalPatchDisambiguatesEmbeddedBPrefix(t *testing.T) {
	patch := strings.Join([]string{
		"diff --git a/x b/y.go b/x b/y.go",
		"old mode 100644",
		"new mode 100755",
		"",
	}, "\n")

	inspection, err := InspectCanonicalPatch(context.Background(), patch)
	if err != nil {
		t.Fatalf("InspectCanonicalPatch() error = %v", err)
	}
	if len(inspection.Files) != 1 ||
		!inspection.Files[0].PathKnown ||
		inspection.Files[0].Path != "x b/y.go" {
		t.Fatalf("inspection = %+v", inspection)
	}
}

func testCanonicalPatch(body ...string) string {
	lines := []string{
		"diff --git a/sample.go b/sample.go",
		"--- a/sample.go",
		"+++ b/sample.go",
	}
	lines = append(lines, body...)
	lines = append(lines, "")
	return strings.Join(lines, "\n")
}
