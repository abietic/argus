package v1alpha1

import (
	"strings"
	"testing"
)

const testDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestReviewSpecValidateModes(t *testing.T) {
	tests := []struct {
		name   string
		target ReviewTarget
	}{
		{
			name: "diff",
			target: ReviewTarget{
				Mode: ReviewModeDiff,
				Diff: &DiffTarget{
					BaseRevision: "base", HeadRevision: "head",
					Patch: ContentRef{
						URI: "artifact://patch/1", SHA256: testDigest, SizeBytes: 1,
					},
				},
			},
		},
		{
			name: "legacy selection",
			target: ReviewTarget{
				Mode: ReviewModeSelection,
				Selection: &SelectionTarget{
					Revision: "head", Path: "internal/review.go",
					StartLine: 10, EndLine: 20,
					Content: ContentRef{
						URI: "artifact://selection/1", SHA256: testDigest, SizeBytes: 1,
					},
				},
			},
		},
		{
			name: "multi-range selection",
			target: ReviewTarget{
				Mode: ReviewModeSelection,
				Selection: &SelectionTarget{
					Revision: "head", Path: "internal/review.go",
					Ranges: []SelectionRange{
						{StartLine: 10, EndLine: 12},
						{StartLine: 20, EndLine: 21},
					},
					Content: ContentRef{
						URI: "artifact://selection/multi", SHA256: testDigest, SizeBytes: 1,
					},
				},
			},
		},
		{
			name: "symbol selection",
			target: ReviewTarget{
				Mode: ReviewModeSelection,
				Selection: &SelectionTarget{
					Revision: "head", Path: "internal/review.go",
					Symbol: &SymbolSelector{
						Language: "go", Kind: "method", QualifiedName: "Service.Review",
					},
					Content: ContentRef{
						URI: "artifact://selection/symbol", SHA256: testDigest, SizeBytes: 1,
					},
				},
			},
		},
		{
			name: "scope",
			target: ReviewTarget{
				Mode: ReviewModeScope,
				Scope: &ScopeTarget{
					Revision: "head", Include: []string{"internal/**"},
					Exclude: []string{"**/*_test.go"},
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := validReviewSpec(test.target)
			if err := spec.Validate(); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestSelectionTargetRejectsAmbiguousOrUnstableSelectors(t *testing.T) {
	tests := []struct {
		name      string
		selection SelectionTarget
	}{
		{
			name: "legacy and ranges",
			selection: SelectionTarget{
				Revision: "head", Path: "review.go", StartLine: 1, EndLine: 2,
				Ranges: []SelectionRange{{StartLine: 4, EndLine: 5}},
			},
		},
		{
			name: "overlapping ranges",
			selection: SelectionTarget{
				Revision: "head", Path: "review.go",
				Ranges: []SelectionRange{
					{StartLine: 1, EndLine: 3},
					{StartLine: 3, EndLine: 5},
				},
			},
		},
		{
			name: "non portable symbol",
			selection: SelectionTarget{
				Revision: "head", Path: "review.go",
				Symbol: &SymbolSelector{
					Language: "go", Kind: "function", QualifiedName: "pkg.Review",
				},
			},
		},
		{
			name: "unsupported language",
			selection: SelectionTarget{
				Revision: "head", Path: "review.go",
				Symbol: &SymbolSelector{
					Language: "python", Kind: "function", QualifiedName: "review",
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.selection.Content = ContentRef{
				URI: "artifact://selection/invalid", SHA256: testDigest, SizeBytes: 1,
			}
			spec := validReviewSpec(ReviewTarget{
				Mode: ReviewModeSelection, Selection: &test.selection,
			})
			if err := spec.Validate(); err == nil {
				t.Fatal("Validate() accepted an ambiguous or unstable selector")
			}
		})
	}
}

func TestReviewSpecRejectsAmbiguousTarget(t *testing.T) {
	spec := validReviewSpec(ReviewTarget{
		Mode: ReviewModeDiff,
		Diff: &DiffTarget{
			BaseRevision: "base", HeadRevision: "head",
			Patch: ContentRef{URI: "artifact://patch/1", SHA256: testDigest, SizeBytes: 1},
		},
		Scope: &ScopeTarget{Revision: "head", Include: []string{"**"}, Exclude: []string{}},
	})
	if err := spec.Validate(); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("Validate() error = %v, want ambiguous target rejection", err)
	}
}

func TestDecodeReviewSpecRejectsUnknownAndTrailingData(t *testing.T) {
	base := `{
		"schema_version":"argus.review_spec.v1alpha1",
		"request_id":"request-1",
		"idempotency_key":"key-1",
		"tenant_id":"tenant-1",
		"workspace_id":"workspace-1",
		"repository":{"provider":"local","repository_id":"argus"},
		"target":{"mode":"scope","scope":{"revision":"head","include":["**"],"exclude":[]}},
		"config_bundle_ref":{"id":"default","revision":"1","sha256":"` + testDigest + `"},
		"workflow_ref":{"id":"default","revision":"1","sha256":"` + testDigest + `"},
		"requested_outputs":["findings"],
		"remote_writes":"deny"`
	if _, err := DecodeReviewSpec([]byte(base + `,"unknown":true}`)); err == nil {
		t.Fatal("DecodeReviewSpec() accepted unknown field")
	}
	if _, err := DecodeReviewSpec([]byte(base + `} {}`)); err == nil {
		t.Fatal("DecodeReviewSpec() accepted trailing JSON")
	}
	if _, err := DecodeReviewSpec([]byte(base + `,"request_id":"request-2"}`)); err == nil ||
		!strings.Contains(err.Error(), "duplicate field") {
		t.Fatalf("DecodeReviewSpec() duplicate error = %v", err)
	}
}

func TestReviewSpecRejectsUnsafeInputs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ReviewSpec)
	}{
		{name: "remote writes", mutate: func(spec *ReviewSpec) { spec.RemoteWrites = "allow" }},
		{name: "credential URI", mutate: func(spec *ReviewSpec) {
			spec.Target.Diff.Patch.URI = "https://user:secret@example.com/patch"
		}},
		{name: "local file URI", mutate: func(spec *ReviewSpec) {
			spec.Target.Diff.Patch.URI = "file:///etc/passwd"
		}},
		{name: "network URI", mutate: func(spec *ReviewSpec) {
			spec.Target.Diff.Patch.URI = "http://127.0.0.1/internal"
		}},
		{name: "artifact query", mutate: func(spec *ReviewSpec) {
			spec.Target.Diff.Patch.URI = "artifact://argus/patch?token=secret"
		}},
		{name: "artifact port", mutate: func(spec *ReviewSpec) {
			spec.Target.Diff.Patch.URI = "artifact://argus:8080/patch"
		}},
		{name: "artifact traversal", mutate: func(spec *ReviewSpec) {
			spec.Target.Diff.Patch.URI = "artifact://argus/../secret"
		}},
		{name: "parent selection path", mutate: func(spec *ReviewSpec) {
			spec.Target = ReviewTarget{
				Mode: ReviewModeSelection,
				Selection: &SelectionTarget{
					Revision: "head", Path: "../secret", StartLine: 1, EndLine: 1,
					Content: ContentRef{URI: "artifact://selection/1", SHA256: testDigest, SizeBytes: 1},
				},
			}
		}},
		{name: "windows parent selection path", mutate: func(spec *ReviewSpec) {
			spec.Target = ReviewTarget{
				Mode: ReviewModeSelection,
				Selection: &SelectionTarget{
					Revision: "head", Path: `..\secret`, StartLine: 1, EndLine: 1,
					Content: ContentRef{URI: "artifact://selection/1", SHA256: testDigest, SizeBytes: 1},
				},
			}
		}},
		{name: "parent scope pattern", mutate: func(spec *ReviewSpec) {
			spec.Target = ReviewTarget{
				Mode:  ReviewModeScope,
				Scope: &ScopeTarget{Revision: "head", Include: []string{"../**"}, Exclude: []string{}},
			}
		}},
		{name: "windows parent scope pattern", mutate: func(spec *ReviewSpec) {
			spec.Target = ReviewTarget{
				Mode:  ReviewModeScope,
				Scope: &ScopeTarget{Revision: "head", Include: []string{`..\**`}, Exclude: []string{}},
			}
		}},
		{name: "ambiguous triple star scope pattern", mutate: func(spec *ReviewSpec) {
			spec.Target = ReviewTarget{
				Mode:  ReviewModeScope,
				Scope: &ScopeTarget{Revision: "head", Include: []string{"src/***"}, Exclude: []string{}},
			}
		}},
		{name: "unsupported question scope pattern", mutate: func(spec *ReviewSpec) {
			spec.Target = ReviewTarget{
				Mode:  ReviewModeScope,
				Scope: &ScopeTarget{Revision: "head", Include: []string{"src/?.go"}, Exclude: []string{}},
			}
		}},
		{name: "oversize scope pattern", mutate: func(spec *ReviewSpec) {
			spec.Target = ReviewTarget{
				Mode: ReviewModeScope,
				Scope: &ScopeTarget{
					Revision: "head", Include: []string{strings.Repeat("a", 1025)},
					Exclude: []string{},
				},
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := validReviewSpec(ReviewTarget{
				Mode: ReviewModeDiff,
				Diff: &DiffTarget{
					BaseRevision: "base", HeadRevision: "head",
					Patch: ContentRef{URI: "artifact://patch/1", SHA256: testDigest, SizeBytes: 1},
				},
			})
			test.mutate(&spec)
			if err := spec.Validate(); err == nil {
				t.Fatal("Validate() accepted unsafe input")
			}
		})
	}
}

func TestDigestReviewSpecIsStableAndSensitive(t *testing.T) {
	spec := validReviewSpec(ReviewTarget{
		Mode:  ReviewModeScope,
		Scope: &ScopeTarget{Revision: "head", Include: []string{"internal/**"}, Exclude: []string{}},
	})
	first, err := DigestReviewSpec(spec)
	if err != nil {
		t.Fatalf("DigestReviewSpec() error = %v", err)
	}
	second, err := DigestReviewSpec(spec)
	if err != nil {
		t.Fatalf("DigestReviewSpec() error = %v", err)
	}
	if first != second {
		t.Fatalf("digest changed: %q != %q", first, second)
	}
	spec.Target.Scope.Include = []string{"pkg/**"}
	changed, err := DigestReviewSpec(spec)
	if err != nil {
		t.Fatalf("DigestReviewSpec() error = %v", err)
	}
	if changed == first {
		t.Fatal("digest did not change with ReviewSpec")
	}
}

func validReviewSpec(target ReviewTarget) ReviewSpec {
	return ReviewSpec{
		SchemaVersion:  ReviewSpecSchemaVersion,
		RequestID:      "request-1",
		IdempotencyKey: "key-1",
		TenantID:       "tenant-1",
		WorkspaceID:    "workspace-1",
		Repository:     RepositoryRef{Provider: "local", RepositoryID: "argus"},
		Target:         target,
		ConfigBundleRef: VersionedRef{
			ID: "default", Revision: "1", SHA256: testDigest,
		},
		WorkflowRef: VersionedRef{
			ID: "default", Revision: "1", SHA256: testDigest,
		},
		RequestedOutput: []string{"findings", "report"},
		RemoteWrites:    "deny",
	}
}
