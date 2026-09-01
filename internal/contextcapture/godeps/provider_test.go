package godeps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/abietic/argus/internal/contextcapture"
	"github.com/abietic/argus/internal/source/gitadapter"
)

const dependencyTestCommit = "1111111111111111111111111111111111111111"

type dependencySource struct {
	order         []string
	files         map[string][]byte
	unavailable   map[string]gitadapter.ReasonCode
	listingCommit string
}

func (source *dependencySource) ListFilesAtCommitWithAdmission(
	_ context.Context,
	_ string,
	commit string,
	_ gitadapter.ScopeAdmission,
	_ int,
) (gitadapter.RevisionFileList, error) {
	entries := make([]gitadapter.RevisionFileEntry, 0, len(source.order))
	for _, file := range source.order {
		entries = append(entries, gitadapter.RevisionFileEntry{Path: file, Type: "blob", Mode: "100644"})
	}
	if source.listingCommit != "" {
		commit = source.listingCommit
	}
	return gitadapter.RevisionFileList{
		SchemaVersion: gitadapter.RevisionFileListSchemaVersion,
		CommitOID:     commit, Include: []string{"**/*.go", "go.mod"}, Exclude: []string{},
		PolicyInclude: []string{"**"}, PolicyExclude: []string{}, Files: entries,
		Coverage: gitadapter.RevisionFileCoverage{
			ScannedFiles: len(entries), MatchedFiles: len(entries), RetainedFiles: len(entries),
		},
		Completeness: gitadapter.CompletenessComplete, Reasons: []gitadapter.Reason{},
	}, nil
}

func TestCaptureRejectsDependencyListingRevisionMismatch(t *testing.T) {
	observed := strings.Repeat("2", 40)
	source := &dependencySource{
		order: []string{"go.mod"}, files: map[string][]byte{"go.mod": []byte("module example.com/project\n")},
		listingCommit: observed,
	}
	provider, _ := New(source)
	_, err := provider.Capture(t.Context(), Request{
		RepositoryRoot: "/repo", CommitOID: dependencyTestCommit, TargetPaths: []string{"main.go"},
	})
	var mismatch *contextcapture.RevisionMismatchError
	if !errors.As(err, &mismatch) || mismatch.Expected != dependencyTestCommit || mismatch.Observed != observed {
		t.Fatalf("revision mismatch err=%v mismatch=%+v", err, mismatch)
	}
}

func (source *dependencySource) ReadFileAtCommit(
	_ context.Context,
	_ string,
	commit string,
	file string,
	_ int64,
) (gitadapter.FileContent, error) {
	data := source.files[file]
	if reason := source.unavailable[file]; reason != "" {
		return gitadapter.FileContent{
			SchemaVersion: gitadapter.FileContentSchemaVersion,
			CommitOID:     commit, Path: file, Mode: "100644", BlobOID: strings.Repeat("a", 40),
			Included: false, Completeness: gitadapter.CompletenessSkipped,
			Reasons: []gitadapter.Reason{{Code: reason}},
		}, nil
	}
	return gitadapter.FileContent{
		SchemaVersion: gitadapter.FileContentSchemaVersion,
		CommitOID:     commit, Path: file, Mode: "100644", BlobOID: strings.Repeat("a", 40),
		SHA256: strings.Repeat("b", 64), SizeBytes: int64(len(data)), Included: true,
		Completeness: gitadapter.CompletenessComplete, Reasons: []gitadapter.Reason{}, Data: data,
	}, nil
}

func TestCapturePreservesNonCanonicalLineEndingGap(t *testing.T) {
	source := &dependencySource{
		order: []string{"go.mod", "main.go", "crlf.go"},
		files: map[string][]byte{
			"go.mod":  []byte("module example.com/project\n"),
			"main.go": []byte("package main\nfunc Main() {}\n"),
			"crlf.go": []byte("package main\r\n"),
		},
		unavailable: map[string]gitadapter.ReasonCode{
			"crlf.go": gitadapter.ReasonNonCanonicalLineEndings,
		},
	}
	provider, _ := New(source)
	result, err := provider.Capture(t.Context(), Request{
		RepositoryRoot: "/repo", CommitOID: dependencyTestCommit,
		TargetPaths: []string{"main.go"}, PolicyInclude: []string{"**"}, PolicyExclude: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasGap(result.Artifact.Gaps, "file_non_canonical_line_endings") ||
		result.Artifact.Coverage.GoFilesFailed != 1 {
		t.Fatalf("noncanonical dependency gap = %+v coverage=%+v", result.Artifact.Gaps, result.Artifact.Coverage)
	}
}

func TestCaptureProducesDeterministicExactDependencyFacts(t *testing.T) {
	files := map[string][]byte{
		"go.mod":                      []byte("module example.com/project\n\ngo 1.23\n"),
		"cmd/app/main.go":             []byte("package main\nimport (\"fmt\"; \"example.com/project/internal/service\")\nfunc main(){fmt.Println(service.Name)}\n"),
		"internal/service/service.go": []byte("package service\nimport (\"context\"; \"external.example/lib\")\nvar _ = context.Background\nconst Name = \"service\"\n"),
	}
	firstSource := &dependencySource{
		order: []string{"internal/service/service.go", "go.mod", "cmd/app/main.go"}, files: files,
	}
	secondSource := &dependencySource{
		order: []string{"cmd/app/main.go", "go.mod", "internal/service/service.go"}, files: files,
	}
	request := Request{
		RepositoryRoot: "/repo", CommitOID: dependencyTestCommit,
		TargetPaths: []string{"cmd/app/main.go"}, PolicyInclude: []string{"**"},
		PolicyExclude: []string{}, MaxFiles: 10, MaxFileBytes: 4096,
		MaxArtifactBytes: DefaultMaxArtifactBytes,
	}
	firstProvider, _ := New(firstSource)
	secondProvider, _ := New(secondSource)
	first, err := firstProvider.Capture(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := secondProvider.Capture(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes, second.Bytes) {
		t.Fatalf("listing order changed artifact:\n%s\n%s", first.Bytes, second.Bytes)
	}
	if first.Artifact.ModulePath != "example.com/project" ||
		first.Artifact.Coverage.GoFilesParsed != 2 ||
		first.Artifact.Coverage.TargetGoFilesFound != 1 {
		t.Fatalf("dependency artifact = %+v", first.Artifact)
	}
	internalFound := false
	classes := []string{}
	for _, dependency := range first.Artifact.Imports {
		classes = append(classes, dependency.Resolution)
		if dependency.ImportPath == "example.com/project/internal/service" &&
			dependency.Resolution == "internal_exact" &&
			dependency.ResolvedDirectory == "internal/service" && dependency.Target {
			internalFound = true
		}
	}
	if !internalFound || !slices.Contains(classes, "stdlib") || !slices.Contains(classes, "external") {
		t.Fatalf("dependency resolutions = %+v", first.Artifact.Imports)
	}
	if len(first.Coverage.Spans) == 0 || first.Coverage.Spans[0].Path != "cmd/app/main.go" {
		t.Fatalf("review coverage = %+v", first.Coverage)
	}
	decoded, err := DecodeArtifact(first.Bytes)
	if err != nil || decoded.CommitOID != dependencyTestCommit {
		t.Fatalf("DecodeArtifact() = %+v, %v", decoded, err)
	}
}

func TestCaptureDeclaresArtifactBudgetTruncation(t *testing.T) {
	imports := make([]string, 0, 100)
	for index := range 100 {
		imports = append(imports, `"external.example/dependency`+string(rune('a'+index%26))+`"`)
	}
	data := []byte("package main\nimport (" + strings.Join(imports, "\n") + ")\n")
	source := &dependencySource{
		order: []string{"go.mod", "main.go"},
		files: map[string][]byte{"go.mod": []byte("module example.com/project\n"), "main.go": data},
	}
	provider, _ := New(source)
	result, err := provider.Capture(context.Background(), Request{
		RepositoryRoot: "/repo", CommitOID: dependencyTestCommit,
		TargetPaths: []string{"main.go"}, PolicyInclude: []string{"**"},
		PolicyExclude: []string{}, MaxFiles: 200, MaxFileBytes: 1 << 20,
		MaxArtifactBytes: 1_500,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Artifact.Coverage.Truncated || !hasGap(result.Artifact.Gaps, "artifact_budget_exceeded") ||
		len(result.Bytes) > 1_500 {
		t.Fatalf("bounded dependency artifact = %+v bytes=%d", result.Artifact.Coverage, len(result.Bytes))
	}
}

func TestDecodeArtifactRejectsUnknownFields(t *testing.T) {
	artifact := Artifact{
		SchemaVersion: SchemaVersion, ProviderID: ProviderID, ProviderRevision: ProviderRevision,
		CommitOID: dependencyTestCommit, ModulePath: "", TargetPaths: []string{},
		Packages: []PackageFact{}, Imports: []ImportFact{}, Coverage: Coverage{},
		Gaps: []Gap{{Code: "module_path_unavailable", Path: "go.mod"}},
	}
	data, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data[:len(data)-1], []byte(`,"unknown":true}`)...)
	if _, err := DecodeArtifact(data); err == nil {
		t.Fatal("DecodeArtifact accepted an unknown field")
	}
}
