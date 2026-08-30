package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"argus.local/argus/internal/runmodel"
)

func TestRunWithIOReviewSelectionAndScopeEndToEnd(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		content     string
		arguments   func(repositoryPath string, revision string, store string) []string
		wantMode    runmodel.TargetMode
		wantFinding string
	}{
		{
			name:        "selection",
			path:        "selected.go",
			content:     "package fixture\n// ARGUS_BUG selected\n",
			wantMode:    runmodel.TargetModeSelection,
			wantFinding: "selected.go",
			arguments: func(repositoryPath string, revision string, store string) []string {
				return []string{
					"review",
					"--repo", repositoryPath,
					"--mode", "selection",
					"--revision", revision,
					"--path", "selected.go",
					"--start-line", "2",
					"--end-line", "2",
					"--store", store,
					"--json",
				}
			},
		},
		{
			name:        "scope",
			path:        "pkg/scoped.go",
			content:     "package pkg\n// TODO scoped\n",
			wantMode:    runmodel.TargetModeScope,
			wantFinding: "pkg/scoped.go",
			arguments: func(repositoryPath string, revision string, store string) []string {
				return []string{
					"review",
					"--repo", repositoryPath,
					"--mode", "scope",
					"--revision", revision,
					"--include", "pkg/**",
					"--exclude", "**/*_test.go",
					"--store", store,
					"--json",
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repositoryPath := newCLITargetRepository(t)
			writeCLITargetFile(t, repositoryPath, test.path, test.content)
			revision := commitCLITarget(t, repositoryPath, "review target")
			writeCLITargetFile(t, repositoryPath, test.path, "package fixture\n")
			store := t.TempDir()
			var output bytes.Buffer

			err := runWithIO(
				context.Background(),
				test.arguments(repositoryPath, revision, store),
				&output,
			)
			if err != nil {
				t.Fatalf("runWithIO(review %s) error = %v", test.name, err)
			}
			var decoded runOutput
			decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&decoded); err != nil {
				t.Fatalf("decode review output: %v\n%s", err, output.String())
			}
			if decoded.Run.Status != runmodel.RunStatusSucceeded ||
				decoded.Run.TargetMode != test.wantMode ||
				decoded.Run.BaseRevision != revision ||
				decoded.Run.HeadRevision != revision ||
				decoded.Report == nil ||
				decoded.Report.Summary.Verified != 1 ||
				len(decoded.Report.Findings) != 1 ||
				decoded.Report.Findings[0].Path != test.wantFinding {
				t.Fatalf("review output = %+v", decoded)
			}
			if test.wantMode == runmodel.TargetModeSelection &&
				(len(decoded.Coverage.EffectiveRanges) != 1 ||
					decoded.Coverage.EffectiveRanges[0].StartLine != 2 ||
					decoded.Coverage.EffectiveRanges[0].EndLine != 2) {
				t.Fatalf("legacy selection coverage = %+v", decoded.Coverage)
			}
		})
	}
}

func TestRunWithIOReviewAdvancedSelectionSelectorsAndCoverageEndToEnd(t *testing.T) {
	tests := []struct {
		name              string
		content           string
		selector          []string
		wantRanges        string
		wantFindings      int
		wantSymbolKind    string
		wantQualifiedName string
	}{
		{
			name: "multi-range",
			content: "package fixture\n" +
				"// TODO first selected range\n" +
				"func untouched() {}\n" +
				"// ARGUS_BUG second selected range\n",
			selector:     []string{"--range", "2:2", "--range", "4:4"},
			wantRanges:   "2:2,4:4",
			wantFindings: 2,
		},
		{
			name: "method symbol",
			content: "package fixture\n" +
				"type Service struct{}\n" +
				"func (Service) Review() {\n" +
				"\t// ARGUS_BUG selected method\n" +
				"}\n",
			selector: []string{
				"--symbol-language", "go",
				"--symbol-kind", "method",
				"--symbol-qualified-name", "Service.Review",
			},
			wantRanges:        "3:5",
			wantFindings:      1,
			wantSymbolKind:    "method",
			wantQualifiedName: "Service.Review",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repositoryPath := newCLITargetRepository(t)
			writeCLITargetFile(t, repositoryPath, "review.go", test.content)
			revision := commitCLITarget(t, repositoryPath, "advanced selection")
			store := t.TempDir()

			arguments := []string{
				"review",
				"--repo", repositoryPath,
				"--mode", "selection",
				"--revision", revision,
				"--path", "review.go",
				"--store", store,
				"--json",
			}
			arguments = append(arguments, test.selector...)
			var reviewJSON bytes.Buffer
			if err := runWithIO(
				context.Background(),
				arguments,
				&reviewJSON,
			); err != nil {
				t.Fatalf("runWithIO(review) error = %v", err)
			}

			var reviewed runOutput
			decoder := json.NewDecoder(bytes.NewReader(reviewJSON.Bytes()))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&reviewed); err != nil {
				t.Fatalf("decode review output: %v\n%s", err, reviewJSON.String())
			}
			assertSelectionCoverage(
				t,
				reviewed.Coverage,
				test.wantRanges,
				test.wantSymbolKind,
				test.wantQualifiedName,
			)
			if reviewed.Report == nil ||
				len(reviewed.Report.Findings) != test.wantFindings {
				t.Fatalf("review findings = %+v", reviewed.Report)
			}
			for _, want := range []string{`"coverage"`, `"effective_ranges"`} {
				if !strings.Contains(reviewJSON.String(), want) {
					t.Fatalf("review JSON hides %q:\n%s", want, reviewJSON.String())
				}
			}
			if test.wantSymbolKind != "" &&
				!strings.Contains(reviewJSON.String(), `"symbol"`) {
				t.Fatalf("review JSON hides symbol:\n%s", reviewJSON.String())
			}

			var showJSON bytes.Buffer
			if err := runWithIO(
				context.Background(),
				[]string{
					"show",
					"--run", reviewed.Run.RunID,
					"--store", store,
					"--json",
				},
				&showJSON,
			); err != nil {
				t.Fatalf("runWithIO(show --json) error = %v", err)
			}
			var shown showOutput
			decoder = json.NewDecoder(bytes.NewReader(showJSON.Bytes()))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&shown); err != nil {
				t.Fatalf("decode show output: %v\n%s", err, showJSON.String())
			}
			assertSelectionCoverage(
				t,
				shown.Coverage,
				test.wantRanges,
				test.wantSymbolKind,
				test.wantQualifiedName,
			)

			var showMarkdown bytes.Buffer
			if err := runWithIO(
				context.Background(),
				[]string{"show", "--run", reviewed.Run.RunID, "--store", store},
				&showMarkdown,
			); err != nil {
				t.Fatalf("runWithIO(show) error = %v", err)
			}
			if !strings.Contains(
				showMarkdown.String(),
				"Effective ranges: `"+test.wantRanges+"`",
			) {
				t.Fatalf("show Markdown hides effective ranges:\n%s", showMarkdown.String())
			}
			if test.wantSymbolKind != "" {
				want := "Symbol: `language=go, kind=" + test.wantSymbolKind +
					", qualified_name=" + test.wantQualifiedName + "`"
				if !strings.Contains(showMarkdown.String(), want) {
					t.Fatalf("show Markdown hides symbol %q:\n%s", want, showMarkdown.String())
				}
			}
		})
	}
}

func TestRunWithIOReviewSelectionRejectsAmbiguousAndInvalidSelectorsEndToEnd(
	t *testing.T,
) {
	repositoryPath := newCLITargetRepository(t)
	writeCLITargetFile(
		t,
		repositoryPath,
		"review.go",
		"package fixture\nfunc Review() {}\n",
	)
	revision := commitCLITarget(t, repositoryPath, "selector rejection")

	tests := []struct {
		name     string
		selector []string
		want     string
	}{
		{
			name: "ambiguous legacy and multi-range",
			selector: []string{
				"--start-line", "1", "--end-line", "1", "--range", "2:2",
			},
			want: "exactly one selector",
		},
		{
			name:     "invalid range syntax",
			selector: []string{"--range", "1-2"},
			want:     "must use START:END",
		},
		{
			name: "unsorted ranges rejected by application",
			selector: []string{
				"--range", "2:2", "--range", "1:1",
			},
			want: "sorted and non-overlapping",
		},
		{
			name: "overlapping ranges rejected by application",
			selector: []string{
				"--range", "1:3", "--range", "3:4",
			},
			want: "sorted and non-overlapping",
		},
		{
			name: "partial symbol",
			selector: []string{
				"--symbol-language", "go",
				"--symbol-kind", "method",
			},
			want: "requires --symbol-language, --symbol-kind, and --symbol-qualified-name",
		},
		{
			name: "invalid symbol kind",
			selector: []string{
				"--symbol-language", "go",
				"--symbol-kind", "field",
				"--symbol-qualified-name", "Service.Value",
			},
			want: `symbol selector kind "field" is unsupported`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			arguments := []string{
				"review",
				"--repo", repositoryPath,
				"--mode", "selection",
				"--revision", revision,
				"--path", "review.go",
				"--store", filepath.Join(t.TempDir(), "store"),
			}
			arguments = append(arguments, test.selector...)
			err := runWithIO(context.Background(), arguments, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("runWithIO(review) error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func assertSelectionCoverage(
	t *testing.T,
	coverage runCoverageSummary,
	wantRanges string,
	wantSymbolKind string,
	wantQualifiedName string,
) {
	t.Helper()
	if got := formatEffectiveRanges(coverage.EffectiveRanges); got != wantRanges {
		t.Fatalf("effective ranges = %q, want %q; coverage=%+v", got, wantRanges, coverage)
	}
	if wantSymbolKind == "" {
		if coverage.Symbol != nil {
			t.Fatalf("range selection unexpectedly has symbol %+v", coverage.Symbol)
		}
		return
	}
	if coverage.Symbol == nil ||
		coverage.Symbol.Language != "go" ||
		coverage.Symbol.Kind != wantSymbolKind ||
		coverage.Symbol.QualifiedName != wantQualifiedName {
		t.Fatalf("symbol coverage = %+v", coverage.Symbol)
	}
}

func TestRunWithIOReviewScopeDisclosesIncompleteCoverage(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		content     string
		include     string
		wantState   string
		wantWarning string
		wantSummary string
		wantReason  string
	}{
		{
			name:        "no matching files",
			path:        "main.go",
			content:     "package main\n",
			include:     "docs/**",
			wantState:   "partial",
			wantWarning: "INCOMPLETE",
			wantSummary: "scope files scanned=1, matched=0, included=0, skipped=0, excluded=0",
			wantReason:  "no_matching_files",
		},
		{
			name:        "matched file skipped",
			path:        "binary.dat",
			content:     "prefix\x00suffix",
			include:     "binary.dat",
			wantState:   "skipped",
			wantWarning: "NOT REVIEWED",
			wantSummary: "scope files scanned=1, matched=1, included=0, skipped=1, excluded=0",
			wantReason:  "binary_file",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repositoryPath := newCLITargetRepository(t)
			writeCLITargetFile(t, repositoryPath, test.path, test.content)
			revision := commitCLITarget(t, repositoryPath, "incomplete scope target")
			var output bytes.Buffer
			if err := runWithIO(
				context.Background(),
				[]string{
					"review",
					"--repo", repositoryPath,
					"--mode", "scope",
					"--revision", revision,
					"--include", test.include,
					"--store", t.TempDir(),
				},
				&output,
			); err != nil {
				t.Fatalf("runWithIO(review scope) error = %v", err)
			}
			for _, want := range []string{
				"Coverage: `" + test.wantState + "`",
				test.wantWarning,
				`"No findings" is not a clean verdict`,
				test.wantSummary,
				test.wantReason,
				"No findings.",
			} {
				if !strings.Contains(output.String(), want) {
					t.Fatalf("output does not contain %q:\n%s", want, output.String())
				}
			}
		})
	}
}

func newCLITargetRepository(t *testing.T) string {
	t.Helper()
	repositoryPath := t.TempDir()
	runCLITargetGit(t, repositoryPath, "init", "-q", "-b", "main")
	runCLITargetGit(t, repositoryPath, "config", "user.name", "Argus Test")
	runCLITargetGit(t, repositoryPath, "config", "user.email", "argus@example.invalid")
	runCLITargetGit(t, repositoryPath, "config", "commit.gpgsign", "false")
	return repositoryPath
}

func writeCLITargetFile(
	t *testing.T,
	repositoryPath string,
	repositoryFile string,
	content string,
) {
	t.Helper()
	target := filepath.Join(repositoryPath, repositoryFile)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("create CLI target parent: %v", err)
	}
	if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
		t.Fatalf("write CLI target: %v", err)
	}
}

func commitCLITarget(t *testing.T, repositoryPath string, message string) string {
	t.Helper()
	runCLITargetGit(t, repositoryPath, "add", "--all")
	runCLITargetGit(t, repositoryPath, "commit", "-q", "-m", message)
	return strings.TrimSpace(runCLITargetGit(t, repositoryPath, "rev-parse", "HEAD"))
}

func runCLITargetGit(
	t *testing.T,
	repositoryPath string,
	arguments ...string,
) string {
	t.Helper()
	command := exec.CommandContext(context.Background(), "git", arguments...)
	command.Dir = repositoryPath
	command.Env = append(
		os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
	return string(output)
}
