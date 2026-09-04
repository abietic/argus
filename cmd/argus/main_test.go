package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/abietic/argus/internal/analytics"
	"github.com/abietic/argus/internal/application"
	"github.com/abietic/argus/internal/configdefaults"
	goastcontext "github.com/abietic/argus/internal/contextcapture/goast"
	compilecontext "github.com/abietic/argus/internal/contextcapture/gocompile"
	depscontext "github.com/abietic/argus/internal/contextcapture/godeps"
	searchcontext "github.com/abietic/argus/internal/contextcapture/reposearch"
	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/reviewcore"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/store/local"
	"github.com/abietic/argus/internal/workflow"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

func TestParseReviewFlags(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "repository")
	contextFile := filepath.Join(t.TempDir(), "call-graph.json")
	options, err := parseReviewFlags([]string{
		"--head", "HEAD",
		"--repo", repository,
		"--json",
		"--base", "HEAD~1",
		"--store", "/tmp/argus-store",
		"--context-file", "codegraph@Review=" + contextFile,
		"--context-provider", "go_ast",
	})
	if err != nil {
		t.Fatalf("parseReviewFlags() error = %v", err)
	}
	if options.repository != repository ||
		options.mode != "diff" ||
		options.base != "HEAD~1" ||
		options.head != "HEAD" ||
		options.store != "/tmp/argus-store" ||
		len(options.contextFiles) != 1 ||
		len(options.contextProviders) != 1 || options.contextProviders[0] != "go_ast" ||
		options.contextFiles[0] != (contextFileFlag{
			Kind: "codegraph", CoverageSymbol: "Review", Path: contextFile,
		}) ||
		!options.json {
		t.Fatalf("parseReviewFlags() = %+v", options)
	}
}

func TestParseReviewFlagsSelectionAndScope(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "repository")
	overlay := filepath.Join(t.TempDir(), "overlay.go")
	selection, err := parseReviewFlags([]string{
		"--repo", repository,
		"--mode", "selection",
		"--revision", "HEAD",
		"--path", "main.go",
		"--start-line", "2",
		"--end-line", "4",
		"--overlay-file", overlay,
	})
	if err != nil {
		t.Fatalf("parseReviewFlags(selection) error = %v", err)
	}
	if selection.mode != "selection" || selection.revision != "HEAD" ||
		selection.path != "main.go" || selection.startLine != 2 ||
		selection.endLine != 4 || selection.overlay != overlay {
		t.Fatalf("selection flags = %+v", selection)
	}

	multiRange, err := parseReviewFlags([]string{
		"--repo", repository,
		"--mode", "selection",
		"--revision", "HEAD",
		"--path", "main.go",
		"--range", "2:4",
		"--range", "8:9",
	})
	if err != nil {
		t.Fatalf("parseReviewFlags(multi-range) error = %v", err)
	}
	if len(multiRange.ranges) != 2 ||
		multiRange.ranges[0] !=
			(application.SelectionRange{StartLine: 2, EndLine: 4}) ||
		multiRange.ranges[1] !=
			(application.SelectionRange{StartLine: 8, EndLine: 9}) {
		t.Fatalf("multi-range flags = %+v", multiRange)
	}

	symbol, err := parseReviewFlags([]string{
		"--repo", repository,
		"--mode", "selection",
		"--revision", "HEAD",
		"--path", "main.go",
		"--symbol-language", "go",
		"--symbol-kind", "method",
		"--symbol-qualified-name", "Service.Review",
	})
	if err != nil {
		t.Fatalf("parseReviewFlags(symbol) error = %v", err)
	}
	if symbol.symbolLang != "go" || symbol.symbolKind != "method" ||
		symbol.symbolName != "Service.Review" {
		t.Fatalf("symbol flags = %+v", symbol)
	}

	scope, err := parseReviewFlags([]string{
		"--repo", repository,
		"--mode", "scope",
		"--revision", "main",
		"--include", "internal/**",
		"--include", "pkg/**",
		"--exclude", "**/*_test.go",
	})
	if err != nil {
		t.Fatalf("parseReviewFlags(scope) error = %v", err)
	}
	if scope.mode != "scope" || scope.revision != "main" ||
		strings.Join(scope.include, ",") != "internal/**,pkg/**" ||
		strings.Join(scope.exclude, ",") != "**/*_test.go" {
		t.Fatalf("scope flags = %+v", scope)
	}
}

func TestParseReviewFlagsRejectsMissingAndRelativeRepository(t *testing.T) {
	for name, arguments := range map[string][]string{
		"missing":  {"--repo", "/tmp/repository", "--base", "HEAD~1"},
		"relative": {"--repo", "repository", "--base", "HEAD~1", "--head", "HEAD"},
		"argument": {"--repo", "/tmp/repository", "--base", "a", "--head", "b", "extra"},
		"mixed modes": {
			"--repo", "/tmp/repository", "--mode", "selection",
			"--revision", "HEAD", "--path", "main.go", "--start-line", "1",
			"--end-line", "1", "--head", "HEAD",
		},
		"scope missing include": {
			"--repo", "/tmp/repository", "--mode", "scope", "--revision", "HEAD",
		},
		"ambiguous legacy and range selectors": {
			"--repo", "/tmp/repository", "--mode", "selection",
			"--revision", "HEAD", "--path", "main.go",
			"--start-line", "1", "--end-line", "1", "--range", "3:3",
		},
		"partial symbol selector": {
			"--repo", "/tmp/repository", "--mode", "selection",
			"--revision", "HEAD", "--path", "main.go",
			"--symbol-language", "go", "--symbol-kind", "method",
		},
		"invalid range syntax": {
			"--repo", "/tmp/repository", "--mode", "selection",
			"--revision", "HEAD", "--path", "main.go", "--range", "1-2",
		},
		"invalid symbol kind": {
			"--repo", "/tmp/repository", "--mode", "selection",
			"--revision", "HEAD", "--path", "main.go",
			"--symbol-language", "go", "--symbol-kind", "field",
			"--symbol-qualified-name", "Service.Value",
		},
		"relative context file": {
			"--repo", "/tmp/repository", "--base", "a", "--head", "b",
			"--context-file", "codegraph@Review=relative.json",
		},
		"unsupported context kind": {
			"--repo", "/tmp/repository", "--base", "a", "--head", "b",
			"--context-file", "shell@Review=/tmp/context.txt",
		},
		"sensitive context file": {
			"--repo", "/tmp/repository", "--base", "a", "--head", "b",
			"--context-file", "artifact@Review=/tmp/.env.production",
		},
		"unsupported automatic provider": {
			"--repo", "/tmp/repository", "--base", "a", "--head", "b",
			"--context-provider", "ambient_shell",
		},
		"automatic provider with governed config": {
			"--repo", "/tmp/repository", "--base", "a", "--head", "b",
			"--context-provider", "go_ast", "--config-state-dir", "/tmp/config",
		},
		"automatic provider with overlay": {
			"--repo", "/tmp/repository", "--mode", "selection",
			"--revision", "HEAD", "--path", "main.go",
			"--start-line", "1", "--end-line", "1",
			"--overlay-file", "/tmp/main.go", "--context-provider", "go_ast",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseReviewFlags(arguments); err == nil {
				t.Fatalf("parseReviewFlags(%q) succeeded", arguments)
			}
		})
	}
}

func TestRunReviewWithGoASTContextProviderFreezesAndPersistsExactFacts(t *testing.T) {
	repositoryPath := newCLITargetRepository(t)
	writeCLITargetFile(t, repositoryPath, "go.mod", "module example.com/go-ast-context\n\ngo 1.26\n")
	writeCLITargetFile(t, repositoryPath, "review/service.go", `package review
type Service struct{}
func validate(input string) bool { return input != "" }
func (Service) Review(input string) bool { return validate(input) }
`)
	base := commitCLITarget(t, repositoryPath, "base")
	writeCLITargetFile(t, repositoryPath, "review/handler.go", `package review
func Handler(service Service, input string) bool { return service.Review(input) }
`)
	head := commitCLITarget(t, repositoryPath, "head")
	storePath := t.TempDir()
	var output bytes.Buffer
	if err := runWithIO(context.Background(), []string{
		"review", "--repo", repositoryPath, "--mode", "diff",
		"--base", base, "--head", "main", "--context-provider", "go_ast",
		"--store", storePath, "--json",
	}, &output); err != nil {
		t.Fatalf("run review with go_ast: %v", err)
	}
	var decoded runOutput
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatalf("decode review output: %v\n%s", err, output.String())
	}
	if decoded.Run.BaseRevision != base || decoded.Run.HeadRevision != head {
		t.Fatalf("run revisions = %s..%s, want %s..%s", decoded.Run.BaseRevision, decoded.Run.HeadRevision, base, head)
	}
	runRepository, _, _, err := openRuntime(storePath, false)
	if err != nil {
		t.Fatal(err)
	}
	var target application.MaterializedTarget
	if err := runRepository.ReadJSONArtifact(decoded.Run.TargetSnapshotRef, &target); err != nil {
		t.Fatal(err)
	}
	if len(target.Contexts) != 1 || target.Contexts[0].Ref == nil {
		t.Fatalf("materialized contexts = %+v", target.Contexts)
	}
	contextRef := target.Contexts[0].Ref
	if contextRef.Kind != "go_ast" || contextRef.Revision != head ||
		contextRef.Contract != goastcontext.SchemaVersion {
		t.Fatalf("go_ast context ref = %+v", contextRef)
	}
	sourceSnapshot, err := runRepository.LoadExecutionSnapshot(decoded.Run.ExecutionSnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sourceSnapshot.ContextProviderReceiptRefs) != 1 {
		t.Fatalf("provider receipt refs = %+v", sourceSnapshot.ContextProviderReceiptRefs)
	}
	receiptData, err := runRepository.ReadArtifact(sourceSnapshot.ContextProviderReceiptRefs[0])
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := contractsv1alpha1.DecodeContextProviderExecutionReceipt(receiptData)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != contractsv1alpha1.ContextProviderReceiptSucceeded ||
		receipt.ContextID != contextRef.ContextID || receipt.ContextDigest != contextRef.Digest ||
		receipt.CommitOID != head || receipt.Authority != "local_host_observation" {
		t.Fatalf("provider receipt = %+v", receipt)
	}
	data, err := runRepository.ReadArtifact(runmodel.ArtifactRef{
		URI: contextRef.ArtifactURI, SHA256: contextRef.Digest,
		SizeBytes: contextRef.SizeBytes, Contract: contextRef.Contract,
	})
	if err != nil {
		t.Fatal(err)
	}
	var artifact goastcontext.Artifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		t.Fatal(err)
	}
	if artifact.CommitOID != head ||
		!slices.Equal(artifact.TargetPaths, []string{"review/handler.go"}) {
		t.Fatalf("go_ast artifact identity = %+v", artifact)
	}
	foundCall := false
	for _, call := range artifact.Calls {
		if strings.HasSuffix(call.Caller, ".Handler") &&
			strings.HasSuffix(call.Callee, ".Service.Review") &&
			call.Resolution == "go_types_exact" {
			foundCall = true
		}
	}
	if !foundCall {
		t.Fatalf("exact Handler -> Service.Review call missing: %+v", artifact.Calls)
	}
	foundPath := false
	for _, callPath := range artifact.CallPaths {
		if callPath.Direction == "downstream" && len(callPath.Symbols) == 3 &&
			strings.HasSuffix(callPath.Symbols[0], ".Handler") &&
			strings.HasSuffix(callPath.Symbols[1], ".Service.Review") &&
			strings.HasSuffix(callPath.Symbols[2], ".validate") && len(callPath.Sites) == 2 {
			foundPath = true
		}
	}
	if !foundPath {
		t.Fatalf("exact Handler -> Service.Review -> validate path missing: %+v", artifact.CallPaths)
	}
	var replayOutput bytes.Buffer
	if err := runWithIO(context.Background(), []string{
		"replay", "--run", decoded.Run.RunID, "--from", "detect",
		"--store", storePath, "--json",
	}, &replayOutput); err != nil {
		t.Fatalf("replay review with frozen go_ast context: %v", err)
	}
	var replayed runOutput
	if err := json.Unmarshal(replayOutput.Bytes(), &replayed); err != nil {
		t.Fatal(err)
	}
	if replayed.Run.Status != runmodel.RunStatusSucceeded ||
		replayed.Run.TargetSnapshotRef != decoded.Run.TargetSnapshotRef ||
		replayed.Run.HeadRevision != head {
		t.Fatalf("go_ast replay = %+v", replayed.Run)
	}
	replaySnapshot, err := runRepository.LoadExecutionSnapshot(replayed.Run.ExecutionSnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(replaySnapshot.ContextProviderReceiptRefs, sourceSnapshot.ContextProviderReceiptRefs) {
		t.Fatalf("replay changed provider receipts: %+v != %+v", replaySnapshot.ContextProviderReceiptRefs, sourceSnapshot.ContextProviderReceiptRefs)
	}
	var spec contractsv1alpha1.ReviewSpec
	if err := runRepository.ReadJSONArtifact(sourceSnapshot.ReviewSpecRef, &spec); err != nil {
		t.Fatal(err)
	}
	var bundle reviewconfig.ConfigBundle
	if err := runRepository.ReadJSONArtifact(sourceSnapshot.ConfigBundleRef, &bundle); err != nil {
		t.Fatal(err)
	}
	if decoded.Run.StartedAt == nil || replayed.Run.CompletedAt == nil {
		t.Fatalf("provider analytics runs lack timestamps: source=%+v replay=%+v", decoded.Run, replayed.Run)
	}
	windowStart := decoded.Run.StartedAt.Add(-time.Second).UTC()
	windowEnd := replayed.Run.CompletedAt.Add(time.Second).UTC()
	var dashboardOutput bytes.Buffer
	if err := runWithIO(context.Background(), []string{
		"dashboard", "rebuild", "--store", storePath,
		"--snapshot", "provider-dashboard", "--tenant", spec.TenantID,
		"--organization", bundle.Context.OrganizationID,
		"--repository", spec.Repository.RepositoryID,
		"--start", windowStart.Format(time.RFC3339Nano),
		"--end", windowEnd.Format(time.RFC3339Nano),
		"--built-at", windowEnd.Add(time.Second).Format(time.RFC3339Nano), "--json",
	}, &dashboardOutput); err != nil {
		t.Fatalf("rebuild provider dashboard: %v", err)
	}
	var dashboard dashboardSnapshotOutput
	if err := json.Unmarshal(dashboardOutput.Bytes(), &dashboard); err != nil {
		t.Fatal(err)
	}
	bindingModes := map[analytics.ContextProviderBindingMode]int{}
	for _, fact := range dashboard.Snapshot.Facts.ContextProviders {
		bindingModes[fact.BindingMode]++
	}
	if len(dashboard.Snapshot.Facts.ContextProviders) != 2 ||
		bindingModes[analytics.ContextProviderExecuted] != 1 ||
		bindingModes[analytics.ContextProviderReused] != 1 {
		t.Fatalf("provider analytics bindings = %+v", dashboard.Snapshot.Facts.ContextProviders)
	}
	providerMetrics := map[string]int64{
		"context_provider.attempt.count": 1,
		"context_provider.reused.count":  1,
	}
	for _, tile := range dashboard.Snapshot.Dashboard.Tiles {
		want, exists := providerMetrics[tile.MetricID]
		if !exists || tile.Value == nil {
			continue
		}
		if tile.Value.Amount != want {
			t.Fatalf("provider metric %s = %+v, want %d", tile.MetricID, tile.Value, want)
		}
		delete(providerMetrics, tile.MetricID)
	}
	if len(providerMetrics) != 0 {
		t.Fatalf("provider dashboard metrics missing: %+v", providerMetrics)
	}
}

func TestRunReviewWithParallelExactContextProviders(t *testing.T) {
	repositoryPath := newCLITargetRepository(t)
	writeCLITargetFile(t, repositoryPath, "go.mod", "module example.com/parallel\n\ngo 1.23\n")
	writeCLITargetFile(t, repositoryPath, "internal/service/service.go", `package service
const Name = "service"
`)
	writeCLITargetFile(t, repositoryPath, "cmd/app/main.go", `package main
import "fmt"
func main() { fmt.Println("base") }
`)
	base := commitCLITarget(t, repositoryPath, "base")
	writeCLITargetFile(t, repositoryPath, "cmd/app/main.go", `package main
import (
    "fmt"
    "example.com/parallel/internal/service"
)
func main() { fmt.Println(service.Name) }
`)
	head := commitCLITarget(t, repositoryPath, "head")
	storePath := t.TempDir()
	var output bytes.Buffer
	if err := runWithIO(context.Background(), []string{
		"review", "--repo", repositoryPath, "--mode", "diff",
		"--base", base, "--head", "main",
		"--context-provider", "go_dependencies",
		"--context-provider", "go_ast",
		"--store", storePath, "--json",
	}, &output); err != nil {
		t.Fatalf("run review with parallel providers: %v", err)
	}
	var decoded runOutput
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	runRepository, _, _, err := openRuntime(storePath, false)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := runRepository.LoadExecutionSnapshot(decoded.Run.ExecutionSnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.ContextProviderReceiptRefs) != 2 {
		t.Fatalf("provider receipt refs = %+v", snapshot.ContextProviderReceiptRefs)
	}
	var target application.MaterializedTarget
	if err := runRepository.ReadJSONArtifact(decoded.Run.TargetSnapshotRef, &target); err != nil {
		t.Fatal(err)
	}
	if len(target.Contexts) != 2 {
		t.Fatalf("parallel provider contexts = %+v", target.Contexts)
	}
	kinds := []string{}
	dependencyFound := false
	for _, binding := range target.Contexts {
		if binding.Ref == nil {
			t.Fatalf("parallel provider produced gap: %+v", binding)
		}
		kinds = append(kinds, binding.Ref.Kind)
		if binding.Ref.Kind != "dependency" {
			continue
		}
		data, err := runRepository.ReadArtifact(runmodel.ArtifactRef{
			URI: binding.Ref.ArtifactURI, SHA256: binding.Ref.Digest,
			SizeBytes: binding.Ref.SizeBytes, Contract: binding.Ref.Contract,
		})
		if err != nil {
			t.Fatal(err)
		}
		artifact, err := depscontext.DecodeArtifact(data)
		if err != nil {
			t.Fatal(err)
		}
		for _, dependency := range artifact.Imports {
			if dependency.ImportPath == "example.com/parallel/internal/service" &&
				dependency.Resolution == "internal_exact" && dependency.Target {
				dependencyFound = true
			}
		}
	}
	if !slices.Equal(kinds, []string{"go_ast", "dependency"}) || !dependencyFound {
		t.Fatalf("parallel provider kinds/dependencies = %v found=%v", kinds, dependencyFound)
	}
	var bundle reviewconfig.ConfigBundle
	if err := runRepository.ReadJSONArtifact(snapshot.ConfigBundleRef, &bundle); err != nil {
		t.Fatal(err)
	}
	if bundle.Execution.ContextProviderMaxConcurrency != 4 ||
		len(bundle.Execution.ContextProviders) != 2 || decoded.Run.HeadRevision != head {
		t.Fatalf("frozen parallel provider config = %+v", bundle.Execution)
	}
}

func TestRunReviewWithGoCompileContextFreezesFailureAndReplayDoesNotRecompile(t *testing.T) {
	repositoryPath := newCLITargetRepository(t)
	writeCLITargetFile(t, repositoryPath, "go.mod", "module example.com/compile-context\n\ngo 1.26\n")
	writeCLITargetFile(t, repositoryPath, "review/review.go", `package review
func Review() int { return 1 }
`)
	base := commitCLITarget(t, repositoryPath, "base")
	writeCLITargetFile(t, repositoryPath, "review/review.go", `package review
func Review() int { return missingValue }
`)
	head := commitCLITarget(t, repositoryPath, "head")
	storePath := t.TempDir()
	var output bytes.Buffer
	if err := runWithIO(context.Background(), []string{
		"review", "--repo", repositoryPath, "--mode", "diff",
		"--base", base, "--head", "main", "--context-provider", "go_compile",
		"--store", storePath, "--json",
	}, &output); err != nil {
		t.Fatalf("run review with go_compile: %v\n%s", err, output.String())
	}
	var decoded runOutput
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	runRepository, _, _, err := openRuntime(storePath, false)
	if err != nil {
		t.Fatal(err)
	}
	var target application.MaterializedTarget
	if err := runRepository.ReadJSONArtifact(decoded.Run.TargetSnapshotRef, &target); err != nil {
		t.Fatal(err)
	}
	if len(target.Contexts) != 1 || target.Contexts[0].Ref == nil ||
		target.Contexts[0].Ref.Kind != "compile" ||
		target.Contexts[0].Ref.Contract != compilecontext.SchemaVersion {
		t.Fatalf("Go compile context binding = %+v", target.Contexts)
	}
	contextRef := target.Contexts[0].Ref
	data, err := runRepository.ReadArtifact(runmodel.ArtifactRef{
		URI: contextRef.ArtifactURI, SHA256: contextRef.Digest,
		SizeBytes: contextRef.SizeBytes, Contract: contextRef.Contract,
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := compilecontext.DecodeArtifact(data)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.CommitOID != head || len(artifact.Packages) != 1 ||
		artifact.Packages[0].Status != "failed" || len(artifact.Packages[0].Diagnostics) != 1 ||
		artifact.Packages[0].Diagnostics[0].Path != "review/review.go" ||
		!strings.Contains(artifact.Packages[0].Diagnostics[0].Message, "undefined: missingValue") ||
		artifact.Toolchain.RepositoryCodeExecute || artifact.Toolchain.ModuleNetwork != "disabled_goproxy_off" {
		t.Fatalf("Go compile artifact = %+v", artifact)
	}
	snapshot, err := runRepository.LoadExecutionSnapshot(decoded.Run.ExecutionSnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.ContextProviderReceiptRefs) != 1 {
		t.Fatalf("compile provider receipts = %+v", snapshot.ContextProviderReceiptRefs)
	}

	// A replay must use the frozen context bytes even after the source checkout changes.
	writeCLITargetFile(t, repositoryPath, "review/review.go", "package review\nfunc Review() int { return 2 }\n")
	var replayOutput bytes.Buffer
	if err := runWithIO(context.Background(), []string{
		"replay", "--run", decoded.Run.RunID, "--from", "detect",
		"--store", storePath, "--json",
	}, &replayOutput); err != nil {
		t.Fatalf("replay frozen compile evidence: %v", err)
	}
	var replayed runOutput
	if err := json.Unmarshal(replayOutput.Bytes(), &replayed); err != nil {
		t.Fatal(err)
	}
	replaySnapshot, err := runRepository.LoadExecutionSnapshot(replayed.Run.ExecutionSnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Run.TargetSnapshotRef != decoded.Run.TargetSnapshotRef ||
		!slices.Equal(replaySnapshot.ContextProviderReceiptRefs, snapshot.ContextProviderReceiptRefs) {
		t.Fatalf("replay changed frozen compile evidence: run=%+v receipts=%+v", replayed.Run, replaySnapshot.ContextProviderReceiptRefs)
	}
}

func TestRunReviewWithRepositorySearchFreezesExactMatchesAndReplayDoesNotSearchAgain(t *testing.T) {
	repositoryPath := newCLITargetRepository(t)
	writeCLITargetFile(t, repositoryPath, "review/handler.go", `package review
func HandleOrder(request OrderRequest) error { return nil }
`)
	writeCLITargetFile(t, repositoryPath, "review/service.go", `package review
type OrderRequest struct { AccountID string }
func ValidateOrder(request OrderRequest) error { return nil }
`)
	base := commitCLITarget(t, repositoryPath, "base")
	writeCLITargetFile(t, repositoryPath, "review/handler.go", `package review
func HandleOrder(request OrderRequest) error { return ValidateOrder(request) }
`)
	head := commitCLITarget(t, repositoryPath, "head")
	storePath := t.TempDir()
	var output bytes.Buffer
	if err := runWithIO(context.Background(), []string{
		"review", "--repo", repositoryPath, "--mode", "diff",
		"--base", base, "--head", "main", "--context-provider", "repository_search",
		"--store", storePath, "--json",
	}, &output); err != nil {
		t.Fatalf("run review with repository_search: %v\n%s", err, output.String())
	}
	var decoded runOutput
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	runRepository, _, _, err := openRuntime(storePath, false)
	if err != nil {
		t.Fatal(err)
	}
	var target application.MaterializedTarget
	if err := runRepository.ReadJSONArtifact(decoded.Run.TargetSnapshotRef, &target); err != nil {
		t.Fatal(err)
	}
	if len(target.Contexts) != 1 || target.Contexts[0].Ref == nil ||
		target.Contexts[0].Ref.Kind != "repository_search" ||
		target.Contexts[0].Ref.Contract != searchcontext.SchemaVersion {
		t.Fatalf("repository search context binding = %+v", target.Contexts)
	}
	contextRef := target.Contexts[0].Ref
	data, err := runRepository.ReadArtifact(runmodel.ArtifactRef{
		URI: contextRef.ArtifactURI, SHA256: contextRef.Digest,
		SizeBytes: contextRef.SizeBytes, Contract: contextRef.Contract,
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := searchcontext.DecodeArtifact(data)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, match := range artifact.Matches {
		if match.Term == "ValidateOrder" && match.Path == "review/service.go" {
			found = true
		}
	}
	if artifact.CommitOID != head || !found || artifact.Coverage.TargetFilesFound != 1 {
		t.Fatalf("repository search artifact = %+v", artifact)
	}
	snapshot, err := runRepository.LoadExecutionSnapshot(decoded.Run.ExecutionSnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.ContextProviderReceiptRefs) != 1 {
		t.Fatalf("repository search receipts = %+v", snapshot.ContextProviderReceiptRefs)
	}

	// Replay must retain exact search evidence after the mutable checkout changes.
	writeCLITargetFile(t, repositoryPath, "review/service.go", "package review\n")
	var replayOutput bytes.Buffer
	if err := runWithIO(context.Background(), []string{
		"replay", "--run", decoded.Run.RunID, "--from", "detect",
		"--store", storePath, "--json",
	}, &replayOutput); err != nil {
		t.Fatalf("replay frozen repository search evidence: %v", err)
	}
	var replayed runOutput
	if err := json.Unmarshal(replayOutput.Bytes(), &replayed); err != nil {
		t.Fatal(err)
	}
	replaySnapshot, err := runRepository.LoadExecutionSnapshot(replayed.Run.ExecutionSnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Run.TargetSnapshotRef != decoded.Run.TargetSnapshotRef ||
		!slices.Equal(replaySnapshot.ContextProviderReceiptRefs, snapshot.ContextProviderReceiptRefs) {
		t.Fatalf("replay changed frozen repository search evidence: run=%+v receipts=%+v", replayed.Run, replaySnapshot.ContextProviderReceiptRefs)
	}
}

func TestAutomaticGoASTContextTargetsSelectionAndScopeExactly(t *testing.T) {
	repositoryPath := newCLITargetRepository(t)
	writeCLITargetFile(t, repositoryPath, "selected.go", "package fixture\nfunc Selected() {}\n")
	writeCLITargetFile(t, repositoryPath, "pkg/a.go", "package pkg\nfunc A() {}\n")
	writeCLITargetFile(t, repositoryPath, "other/b.go", "package other\nfunc B() {}\n")
	revision := commitCLITarget(t, repositoryPath, "targets")
	tests := []struct {
		name      string
		arguments []string
		want      []string
	}{
		{
			name: "selection",
			arguments: []string{
				"review", "--repo", repositoryPath, "--mode", "selection",
				"--revision", "main", "--path", "selected.go",
				"--start-line", "1", "--end-line", "1", "--context-provider", "go_ast",
			},
			want: []string{"selected.go"},
		},
		{
			name: "scope",
			arguments: []string{
				"review", "--repo", repositoryPath, "--mode", "scope",
				"--revision", "main", "--include", "pkg/**", "--context-provider", "go_ast",
			},
			want: []string{"pkg/a.go"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			storePath := t.TempDir()
			arguments := append(slices.Clone(test.arguments), "--store", storePath, "--json")
			var output bytes.Buffer
			if err := runWithIO(context.Background(), arguments, &output); err != nil {
				t.Fatal(err)
			}
			var decoded runOutput
			if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.Run.HeadRevision != revision {
				t.Fatalf("head revision = %s, want %s", decoded.Run.HeadRevision, revision)
			}
			store, err := local.Open(storePath)
			if err != nil {
				t.Fatal(err)
			}
			repository, err := runrepo.New(store)
			if err != nil {
				t.Fatal(err)
			}
			var target application.MaterializedTarget
			if err := repository.ReadJSONArtifact(decoded.Run.TargetSnapshotRef, &target); err != nil {
				t.Fatal(err)
			}
			if len(target.Contexts) != 1 || target.Contexts[0].Ref == nil {
				t.Fatalf("contexts = %+v", target.Contexts)
			}
			ref := target.Contexts[0].Ref
			data, err := repository.ReadArtifact(runmodel.ArtifactRef{
				URI: ref.ArtifactURI, SHA256: ref.Digest, SizeBytes: ref.SizeBytes, Contract: ref.Contract,
			})
			if err != nil {
				t.Fatal(err)
			}
			var artifact goastcontext.Artifact
			if err := json.Unmarshal(data, &artifact); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(artifact.TargetPaths, test.want) {
				t.Fatalf("target paths = %v, want %v", artifact.TargetPaths, test.want)
			}
		})
	}
}

func TestPublishedConfigDrivesGoASTProviderWithoutCLIBypass(t *testing.T) {
	repositoryPath := newCLITargetRepository(t)
	writeCLITargetFile(t, repositoryPath, "selected.go", "package fixture\nfunc Selected() {}\n")
	revision := commitCLITarget(t, repositoryPath, "governed context")
	stateDir := t.TempDir()
	config := application.DefaultLocalConfig()
	definition := workflow.DefaultReviewDefinition()
	allowedModes := make([]string, len(config.AllowedModes))
	for index, mode := range config.AllowedModes {
		allowedModes[index] = string(mode)
	}
	governed, err := configdefaults.Revision(configdefaults.Options{
		ID: "platform-go-ast", Revision: "1", MaxFiles: config.MaxFiles,
		MaxPatchBytes: config.MaxPatchBytes, MaxInputBytes: config.MaxMaterializedBytes,
		MaxOutputBytes: definition.Stages[0].Budget.MaxOutputBytes,
		MaxAttempts:    config.MaxAttempts, AllowedModes: allowedModes,
		TargetInclude: config.TargetInclude, TargetExclude: config.TargetExclude,
		ContextProviders: []reviewconfig.ContextProviderDefinition{{
			ID: "go-ast-exact", Revision: "1", Kind: "go_ast",
			Adapter: reviewconfig.VersionedRef{
				ID: goastcontext.ProviderID, Revision: goastcontext.ProviderRevision,
				SHA256: goastcontext.AdapterSHA256,
			},
		}},
	}, definition)
	if err != nil {
		t.Fatal(err)
	}
	revisionPath := filepath.Join(t.TempDir(), "governed.json")
	data, err := json.Marshal(governed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(revisionPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	runConfigCommand(t, []string{
		"config", "create", "--state-dir", stateDir, "--file", revisionPath,
		"--idempotency-key", "create-go-ast", "--actor", "tester",
		"--audit", "create provider", "--at", "2026-08-25T02:00:00Z", "--json",
	})
	runConfigCommand(t, []string{
		"config", "validate", "--state-dir", stateDir,
		"--id", governed.ID, "--revision", governed.Revision,
		"--idempotency-key", "validate-go-ast", "--actor", "tester",
		"--audit", "validate provider", "--at", "2026-08-25T02:00:01Z", "--json",
	})
	runConfigCommand(t, []string{
		"config", "publish", "--state-dir", stateDir,
		"--id", governed.ID, "--revision", governed.Revision, "--percentage", "100",
		"--idempotency-key", "publish-go-ast", "--actor", "tester",
		"--audit", "publish provider", "--at", "2026-08-25T02:00:02Z", "--json",
	})

	storePath := t.TempDir()
	var output bytes.Buffer
	if err := runWithIO(context.Background(), []string{
		"review", "--repo", repositoryPath, "--mode", "selection",
		"--revision", "main", "--path", "selected.go", "--start-line", "1", "--end-line", "1",
		"--config-state-dir", stateDir, "--store", storePath, "--json",
	}, &output); err != nil {
		t.Fatal(err)
	}
	var decoded runOutput
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Run.HeadRevision != revision {
		t.Fatalf("head revision = %s", decoded.Run.HeadRevision)
	}
	repository, _, _, err := openRuntime(storePath, false)
	if err != nil {
		t.Fatal(err)
	}
	var target application.MaterializedTarget
	if err := repository.ReadJSONArtifact(decoded.Run.TargetSnapshotRef, &target); err != nil {
		t.Fatal(err)
	}
	if len(target.Contexts) != 1 || target.Contexts[0].Ref == nil || target.Contexts[0].Ref.Kind != "go_ast" {
		t.Fatalf("governed contexts = %+v", target.Contexts)
	}
	snapshot, err := repository.LoadExecutionSnapshot(decoded.Run.ExecutionSnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	bundleData, err := repository.ReadArtifact(snapshot.ConfigBundleRef)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := reviewconfig.DecodeBundle(bundleData)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Execution.ContextProviders) != 1 || bundle.Execution.ContextProviders[0] != governed.Patch.Execution.ContextProviders.Upsert[0] {
		t.Fatalf("frozen configured providers = %+v", bundle.Execution.ContextProviders)
	}
}

func TestPublishReviewContextFilesCreatesExactReadableBinding(t *testing.T) {
	storePath := t.TempDir()
	contextPath := filepath.Join(t.TempDir(), "call-graph.json")
	content := `{"calls":[{"caller":"Serve","callee":"Handle"}],"types":["Handle func(Request) Response"]}`
	if err := os.WriteFile(contextPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	bindings, err := publishReviewContextFiles(
		context.Background(),
		storePath,
		[]contextFileFlag{{Kind: "codegraph", CoverageSymbol: "Handle", Path: contextPath}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings[0].Ref == nil ||
		bindings[0].Ref.Kind != "codegraph" ||
		bindings[0].Ref.Contract != "argus.context.codegraph.v1alpha1" {
		t.Fatalf("context bindings = %+v", bindings)
	}
	store, err := local.Open(storePath)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := runrepo.New(store)
	if err != nil {
		t.Fatal(err)
	}
	ref := bindings[0].Ref
	resolved, err := repository.ReadArtifact(runmodel.ArtifactRef{
		URI: ref.ArtifactURI, SHA256: ref.Digest,
		SizeBytes: ref.SizeBytes, Contract: ref.Contract,
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(resolved) != content {
		t.Fatalf("resolved context = %q", resolved)
	}
}

func TestSelectionRangeListEnforcesMaximumCount(t *testing.T) {
	var ranges selectionRangeList
	for line := 1; line <= 128; line++ {
		if err := ranges.Set(fmt.Sprintf("%d:%d", line, line)); err != nil {
			t.Fatalf("range %d rejected: %v", line, err)
		}
	}
	if err := ranges.Set("129:129"); err == nil ||
		!strings.Contains(err.Error(), "at most 128") {
		t.Fatalf("129th range error = %v", err)
	}
}

func TestParseReplayCompareHistoryAndShowFlags(t *testing.T) {
	replay, err := parseReplayFlags([]string{"--run", "run-1", "--from", "verify", "--json"})
	if err != nil || replay.run != "run-1" || replay.from != "verify" || !replay.json {
		t.Fatalf("parseReplayFlags() = %+v, %v", replay, err)
	}
	variantPath := filepath.Join(t.TempDir(), "variant.json")
	workflowPath := filepath.Join(t.TempDir(), "workflow.json")
	replay, err = parseReplayFlags([]string{
		"--run", "run-1",
		"--from", "detect",
		"--change", "rule_pack",
		"--variant-config-bundle", variantPath,
	})
	if err != nil ||
		replay.change != string(runmodel.ReplayVariableRulePack) ||
		replay.variantConfigBundle != variantPath {
		t.Fatalf("parseReplayFlags(variant) = %+v, %v", replay, err)
	}
	replay, err = parseReplayFlags([]string{
		"--run", "run-1", "--from", "report", "--change", "workflow",
		"--variant-config-bundle", variantPath, "--variant-workflow", workflowPath,
	})
	if err != nil || replay.variantWorkflow != workflowPath {
		t.Fatalf("parseReplayFlags(workflow variant) = %+v, %v", replay, err)
	}
	if _, err := parseReplayFlags([]string{"--run", "run-1", "--from", "unknown"}); err == nil {
		t.Fatal("parseReplayFlags() accepted an unknown stage")
	}
	for name, arguments := range map[string][]string{
		"change without bundle": {
			"--run", "run-1", "--from", "detect", "--change", "rule_pack",
		},
		"bundle without change": {
			"--run", "run-1", "--from", "detect",
			"--variant-config-bundle", variantPath,
		},
		"none as change": {
			"--run", "run-1", "--from", "detect", "--change", "none",
			"--variant-config-bundle", variantPath,
		},
		"unknown change": {
			"--run", "run-1", "--from", "detect", "--change", "everything",
			"--variant-config-bundle", variantPath,
		},
		"relative bundle": {
			"--run", "run-1", "--from", "detect", "--change", "rule_pack",
			"--variant-config-bundle", "variant.json",
		},
		"workflow without definition": {
			"--run", "run-1", "--from", "report", "--change", "workflow",
			"--variant-config-bundle", variantPath,
		},
		"definition for another variable": {
			"--run", "run-1", "--from", "detect", "--change", "rule_pack",
			"--variant-config-bundle", variantPath, "--variant-workflow", workflowPath,
		},
		"relative workflow": {
			"--run", "run-1", "--from", "report", "--change", "workflow",
			"--variant-config-bundle", variantPath, "--variant-workflow", "workflow.json",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseReplayFlags(arguments); err == nil {
				t.Fatalf("parseReplayFlags(%q) unexpectedly succeeded", arguments)
			}
		})
	}

	comparison, err := parseCompareFlags([]string{
		"--variant", "run-2", "--baseline", "run-1", "--store", "/tmp/store",
	})
	if err != nil || comparison.baseline != "run-1" ||
		comparison.variant != "run-2" || comparison.store != "/tmp/store" {
		t.Fatalf("parseCompareFlags() = %+v, %v", comparison, err)
	}
	if _, err := parseCompareFlags([]string{
		"--baseline", "same", "--variant", "same",
	}); err == nil {
		t.Fatal("parseCompareFlags() accepted identical runs")
	}

	history, err := parseHistoryFlags([]string{"--limit", "0", "--json"})
	if err != nil || history.limit != 0 || !history.json {
		t.Fatalf("parseHistoryFlags() = %+v, %v", history, err)
	}
	if _, err := parseHistoryFlags([]string{"--limit", "-1"}); err == nil {
		t.Fatal("parseHistoryFlags() accepted a negative limit")
	}

	show, err := parseShowFlags([]string{"--run", "run-1"})
	if err != nil || show.run != "run-1" {
		t.Fatalf("parseShowFlags() = %+v, %v", show, err)
	}
	if _, err := parseShowFlags(nil); err == nil {
		t.Fatal("parseShowFlags() accepted a missing run")
	}
}

func TestWriteComparisonMarkdownIncludesCandidateDecisionLatencyAndAvailability(
	t *testing.T,
) {
	var output bytes.Buffer
	comparison := runmodel.RunComparison{
		SchemaVersion: runmodel.ComparisonSchemaVersion,
		BaselineRunID: "baseline",
		VariantRunID:  "variant",
		Added: []runmodel.FindingChange{{
			Fingerprint: "finding-fingerprint",
			Title:       "new finding",
			Path:        "internal/a.go",
			Line:        12,
		}},
		Removed:   []runmodel.FindingChange{},
		Unchanged: []runmodel.FindingChange{},
		CandidatesAdded: []runmodel.CandidateChange{{
			CandidateID: "candidate-new",
			RuleID:      "rule-new",
			Path:        "internal/a.go",
			Line:        12,
		}},
		CandidatesRemoved: []runmodel.CandidateChange{},
		CandidatesUnchanged: []runmodel.CandidateChange{{
			CandidateID: "candidate-stable",
			RuleID:      "rule-stable",
			Path:        "internal/b.go",
			Line:        8,
		}},
		DecisionChanges: []runmodel.DecisionChange{{
			FindingID:      "finding-1",
			Fingerprint:    "finding-fingerprint",
			BaselineAction: "suppress",
			VariantAction:  "publish",
		}},
		StageLatency: []runmodel.StageLatencyDelta{{
			StageID:            "detect",
			BaselineDurationMS: 20,
			VariantDurationMS:  8,
			DeltaMS:            -12,
			VariantReused:      true,
		}},
		Quality: runmodel.MetricAvailability{
			Available: false, ReasonCode: "evaluation_labels_required",
		},
		Cost: runmodel.MetricAvailability{
			Available: false, ReasonCode: "cost_evidence_not_recorded",
		},
	}
	if err := writeComparisonMarkdown(&output, comparison, "/tmp/store"); err != nil {
		t.Fatalf("writeComparisonMarkdown() error = %v", err)
	}
	for _, want := range []string{
		"Findings: added 1, removed 0, unchanged 0",
		"Candidates: added 1, removed 0, unchanged 1",
		"## Candidates added",
		"`candidate-new`",
		"## Decision changes",
		"`suppress`",
		"`publish`",
		"## Stage latency",
		"| `detect` | 20 | 8 | -12 | false | true |",
		"`evaluation_labels_required`",
		"`cost_evidence_not_recorded`",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("comparison output does not contain %q:\n%s", want, output.String())
		}
	}
}

func TestReplayRejectsInvalidVariantBeforeOpeningState(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "not-created-store")
	descriptor := filepath.Join(root, "invalid-variant.json")
	if err := os.WriteFile(
		descriptor,
		[]byte(`{"schema_version":"first","schema_version":"duplicate"}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	err := runWithIO(
		context.Background(),
		[]string{
			"replay",
			"--run", "source-run",
			"--from", "detect",
			"--change", "rule_pack",
			"--variant-config-bundle", descriptor,
			"--store", store,
		},
		&bytes.Buffer{},
	)
	if err == nil || !strings.Contains(err.Error(), `duplicate JSON field "schema_version"`) {
		t.Fatalf("invalid variant error = %v", err)
	}
	if _, statErr := os.Stat(store); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("invalid variant opened store before validation: stat error = %v", statErr)
	}
}

func TestReplayVariantCLIWiresAtomicChangeAndStrictConfigBundle(t *testing.T) {
	repositoryPath := newCLITargetRepository(t)
	writeCLITargetFile(
		t,
		repositoryPath,
		"variant.go",
		"package fixture\n// ARGUS_BUG replay variant\n",
	)
	revision := commitCLITarget(t, repositoryPath, "variant replay target")
	store := t.TempDir()
	var reviewOutput bytes.Buffer
	if err := runWithIO(
		context.Background(),
		[]string{
			"review",
			"--repo", repositoryPath,
			"--mode", "selection",
			"--revision", revision,
			"--path", "variant.go",
			"--start-line", "2",
			"--end-line", "2",
			"--store", store,
			"--json",
		},
		&reviewOutput,
	); err != nil {
		t.Fatalf("review error = %v", err)
	}
	var reviewed runOutput
	if err := json.Unmarshal(reviewOutput.Bytes(), &reviewed); err != nil {
		t.Fatalf("decode review output: %v\n%s", err, reviewOutput.String())
	}
	if reviewed.Report == nil || len(reviewed.Report.Findings) != 1 {
		t.Fatalf("review output = %+v", reviewed)
	}

	runRepository, _, _, err := openRuntime(store, false)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := runRepository.LoadExecutionSnapshot(
		reviewed.Run.ExecutionSnapshotID,
	)
	if err != nil {
		t.Fatal(err)
	}
	bundleData, err := runRepository.ReadArtifact(snapshot.ConfigBundleRef)
	if err != nil {
		t.Fatal(err)
	}
	sourceBundle, err := reviewconfig.DecodeBundle(bundleData)
	if err != nil {
		t.Fatal(err)
	}
	variant := sourceBundle
	rules := append(
		[]reviewconfig.RuleDefinition(nil),
		sourceBundle.RulePack.Rules...,
	)
	changed := false
	for index := range rules {
		if rules[index].ID == reviewcore.RuleExplicitBugMarker {
			rules[index].Enabled = false
			changed = true
		}
	}
	if !changed {
		t.Fatal("source bundle has no explicit-bug rule")
	}
	variant.RulePack, err = reviewconfig.SealRulePack(
		sourceBundle.RulePack.ID,
		"cli-variant-1",
		rules,
	)
	if err != nil {
		t.Fatal(err)
	}
	variant.BundleID = ""
	variant.SHA256 = ""
	variant.SHA256, err = reviewconfig.DigestBundle(variant)
	if err != nil {
		t.Fatal(err)
	}
	variant.BundleID = "bundle-" + variant.SHA256[:24]
	if err := variant.Validate(); err != nil {
		t.Fatalf("variant bundle is invalid: %v", err)
	}
	variantPath := writeCLIJSONDescriptor(t, "variant-bundle.json", variant)

	var replayOutput bytes.Buffer
	if err := runWithIO(
		context.Background(),
		[]string{
			"replay",
			"--run", reviewed.Run.RunID,
			"--from", "detect",
			"--change", "rule_pack",
			"--variant-config-bundle", variantPath,
			"--store", store,
			"--json",
		},
		&replayOutput,
	); err != nil {
		t.Fatalf("variant replay error = %v", err)
	}
	var replayed runOutput
	if err := json.Unmarshal(replayOutput.Bytes(), &replayed); err != nil {
		t.Fatalf("decode replay output: %v\n%s", err, replayOutput.String())
	}
	if replayed.Run.ReplayVariable != runmodel.ReplayVariableRulePack ||
		replayed.Run.SourceRunID != reviewed.Run.RunID ||
		replayed.Report == nil ||
		len(replayed.Report.Findings) != 0 {
		t.Fatalf("variant replay output = %+v", replayed)
	}

	variantWorkflow := workflow.DefaultReviewDefinition()
	variantWorkflow.Revision = "cli-workflow-variant-1"
	for index := range variantWorkflow.Stages {
		if variantWorkflow.Stages[index].ID == string(reviewcore.StageReport) {
			variantWorkflow.Stages[index].Retry.BackoffMS++
		}
	}
	workflowDigest, err := workflow.DigestDefinition(variantWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	workflowBundle := sourceBundle
	workflowBundle.Workflow.Definition = reviewconfig.VersionedRef{
		ID: variantWorkflow.ID, Revision: variantWorkflow.Revision, SHA256: workflowDigest,
	}
	workflowBundle.BundleID, workflowBundle.SHA256 = "", ""
	workflowBundle.SHA256, err = reviewconfig.DigestBundle(workflowBundle)
	if err != nil {
		t.Fatal(err)
	}
	workflowBundle.BundleID = "bundle-" + workflowBundle.SHA256[:24]
	if err := workflowBundle.Validate(); err != nil {
		t.Fatal(err)
	}
	workflowBundlePath := writeCLIJSONDescriptor(t, "workflow-variant-bundle.json", workflowBundle)
	workflowPath := writeCLIJSONDescriptor(t, "workflow-variant.json", variantWorkflow)
	var workflowReplayOutput bytes.Buffer
	if err := runWithIO(
		context.Background(),
		[]string{
			"replay", "--run", reviewed.Run.RunID, "--from", "report",
			"--change", "workflow", "--variant-config-bundle", workflowBundlePath,
			"--variant-workflow", workflowPath, "--store", store, "--json",
		},
		&workflowReplayOutput,
	); err != nil {
		t.Fatalf("workflow variant replay error = %v", err)
	}
	var workflowReplayed runOutput
	if err := json.Unmarshal(workflowReplayOutput.Bytes(), &workflowReplayed); err != nil {
		t.Fatal(err)
	}
	if workflowReplayed.Run.ReplayVariable != runmodel.ReplayVariableWorkflow ||
		workflowReplayed.Report == nil {
		t.Fatalf("workflow replay output = %+v", workflowReplayed)
	}
	workflowSnapshot, err := runRepository.LoadExecutionSnapshot(
		workflowReplayed.Run.ExecutionSnapshotID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if workflowSnapshot.Workflow.SHA256 != workflowDigest ||
		workflowSnapshot.WorkflowDefinitionRef.SHA256 != workflowDigest {
		t.Fatalf("workflow replay snapshot = %+v", workflowSnapshot)
	}

	indexBundle := sourceBundle
	indexBundle.Execution.ContextProviders = []reviewconfig.ContextProviderDefinition{{
		ID: "go-ast-exact", Revision: "1", Kind: "go_ast",
		Adapter: reviewconfig.VersionedRef{
			ID: goastcontext.ProviderID, Revision: goastcontext.ProviderRevision,
			SHA256: goastcontext.AdapterSHA256,
		},
	}}
	indexBundle.BundleID, indexBundle.SHA256 = "", ""
	indexBundle.SHA256, err = reviewconfig.DigestBundle(indexBundle)
	if err != nil {
		t.Fatal(err)
	}
	indexBundle.BundleID = "bundle-" + indexBundle.SHA256[:24]
	if err := indexBundle.Validate(); err != nil {
		t.Fatal(err)
	}
	indexBundlePath := writeCLIJSONDescriptor(t, "index-variant-bundle.json", indexBundle)
	var indexReplayOutput bytes.Buffer
	if err := runWithIO(
		context.Background(),
		[]string{
			"replay", "--run", reviewed.Run.RunID, "--from", "materialize_target",
			"--change", "index", "--variant-config-bundle", indexBundlePath,
			"--store", store, "--json",
		},
		&indexReplayOutput,
	); err != nil {
		t.Fatalf("index variant replay error = %v", err)
	}
	var indexReplayed runOutput
	if err := json.Unmarshal(indexReplayOutput.Bytes(), &indexReplayed); err != nil {
		t.Fatal(err)
	}
	if indexReplayed.Run.ReplayVariable != runmodel.ReplayVariableIndex ||
		indexReplayed.Report == nil {
		t.Fatalf("index replay output = %+v", indexReplayed)
	}
	indexSnapshot, err := runRepository.LoadExecutionSnapshot(
		indexReplayed.Run.ExecutionSnapshotID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(indexSnapshot.ContextProviderReceiptRefs) != 1 ||
		indexSnapshot.TargetSnapshotRef == snapshot.TargetSnapshotRef ||
		indexSnapshot.ReviewInputRef == snapshot.ReviewInputRef {
		t.Fatalf("index replay snapshot = %+v", indexSnapshot)
	}
	var indexChange runmodel.ReplayChangeSet
	if indexSnapshot.ReplayChangeSetRef == nil {
		t.Fatal("index replay has no change set")
	}
	if err := runRepository.ReadJSONArtifact(*indexSnapshot.ReplayChangeSetRef, &indexChange); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(indexChange.ChangedFields, []string{
		"execution.context_providers.go-ast-exact.added",
		"execution.context_providers.order",
	}) {
		t.Fatalf("index changed fields = %v", indexChange.ChangedFields)
	}
}

func TestResolveDefaultStoreOutsideCurrentWorkingDirectory(t *testing.T) {
	current := t.TempDir()
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(current); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(original); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})

	path, err := resolveStorePath("")
	if err != nil {
		t.Fatalf("resolveStorePath() error = %v", err)
	}
	configDirectory, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(configDirectory, "argus", "local-store")
	if path != want {
		t.Fatalf("resolveStorePath() = %q, want %q", path, want)
	}
	relative, err := filepath.Rel(current, path)
	if err != nil {
		t.Fatal(err)
	}
	if relative == "." || !strings.HasPrefix(relative, "..") {
		t.Fatalf("default store %q is inside current working directory %q", path, current)
	}
}

func TestRejectStoreInsideRepositoryBeforeCreation(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(repository, ".argus")
	err := rejectStoreInsideRepository(inside, repository)
	if err == nil || !strings.Contains(err.Error(), "outside reviewed repository") {
		t.Fatalf("rejectStoreInsideRepository() error = %v", err)
	}
	if _, statErr := os.Stat(inside); !os.IsNotExist(statErr) {
		t.Fatalf("store path was created before rejection: %v", statErr)
	}
	outside := filepath.Join(t.TempDir(), "store")
	if err := rejectStoreInsideRepository(outside, repository); err != nil {
		t.Fatalf("outside store rejected: %v", err)
	}
}

func TestRejectStoreInsideRepositoryThroughSymlink(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	inside := filepath.Join(repository, "state")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "store-link")
	if err := os.Symlink(inside, link); err != nil {
		t.Fatal(err)
	}
	if err := rejectStoreInsideRepository(link, repository); err == nil {
		t.Fatal("symlinked in-repository store was accepted")
	}
}

func TestReadSelectionOverlayAtByteLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overlay.go")
	if err := os.WriteFile(path, []byte("four"), 0o600); err != nil {
		t.Fatal(err)
	}
	overlay, err := readSelectionOverlayWithLimit(path, 4)
	if err != nil {
		t.Fatalf("readSelectionOverlayWithLimit() error = %v", err)
	}
	if overlay != "four" {
		t.Fatalf("readSelectionOverlayWithLimit() = %q", overlay)
	}
}

func TestReadSelectionOverlayRejectsLimitPlusOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overlay.go")
	if err := os.WriteFile(path, []byte("123456"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := readSelectionOverlayWithLimit(path, 5)
	if err == nil || !strings.Contains(err.Error(), "exceeds local limit of 5 bytes") {
		t.Fatalf("readSelectionOverlayWithLimit() error = %v", err)
	}
}

func TestReadSelectionOverlayRejectsNonRegularFile(t *testing.T) {
	_, err := readSelectionOverlayWithLimit(t.TempDir(), 16)
	if err == nil || !strings.Contains(err.Error(), "must be a regular file") {
		t.Fatalf("readSelectionOverlayWithLimit() error = %v", err)
	}
}

func TestReadSelectionOverlayRejectsFIFOWithoutWaitingForWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overlay.pipe")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := readSelectionOverlayWithLimit(path, 16)
	if err == nil || !strings.Contains(err.Error(), "must be a regular file") {
		t.Fatalf("readSelectionOverlayWithLimit() error = %v", err)
	}
}

func TestReadRegularFileLimitRejectsFIFOWithoutWaitingForWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "descriptor.pipe")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readRegularFileLimit(path, 32); err == nil ||
		!strings.Contains(err.Error(), "input must be a regular file") {
		t.Fatalf("readRegularFileLimit(FIFO) error = %v", err)
	}
}

func TestReadSelectionOverlayFollowsSymlinkToRegularFile(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "overlay.go")
	if err := os.WriteFile(target, []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "overlay-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	overlay, err := readSelectionOverlayWithLimit(link, 32)
	if err != nil {
		t.Fatalf("readSelectionOverlayWithLimit() error = %v", err)
	}
	if overlay != "package main\n" {
		t.Fatalf("readSelectionOverlayWithLimit() = %q", overlay)
	}
}

func TestRunWithIOVersionHelpAndUsage(t *testing.T) {
	var output bytes.Buffer
	if err := runWithIO(context.Background(), []string{"version"}, &output); err != nil {
		t.Fatalf("version error = %v", err)
	}
	if got := output.String(); got != "argus "+version+"\n" {
		t.Fatalf("version output = %q", got)
	}

	output.Reset()
	if err := runWithIO(
		context.Background(),
		[]string{"review", "--help"},
		&output,
	); err != nil {
		t.Fatalf("review help error = %v", err)
	}
	if !strings.Contains(output.String(), "--repo <absolute-path>") {
		t.Fatalf("review help = %q", output.String())
	}
	if !strings.Contains(output.String(), "--range") {
		t.Fatalf("review help does not disclose selection ranges: %q", output.String())
	}

	for _, test := range []struct {
		arguments []string
		want      string
	}{
		{[]string{"version", "--help"}, "usage: argus version"},
		{[]string{"validate", "review-spec", "--help"}, "usage: argus validate review-spec <file>"},
		{[]string{"agent-review", "run", "--help"}, "--allow-partial"},
		{[]string{"agent-review", "quick", "--help"}, "--source-run"},
		{[]string{"agent-review", "formal", "run", "--help"}, "--input-micros-per-million"},
		{[]string{"agent-review", "execution", "list", "--help"}, "--start"},
		{[]string{"agent-review", "analytics", "rebuild", "--help"}, "--group-by"},
		{[]string{"dashboard", "rebuild", "--help"}, "--repository"},
	} {
		output.Reset()
		if err := runWithIO(context.Background(), test.arguments, &output); err != nil {
			t.Fatalf("%v help error = %v", test.arguments, err)
		}
		if !strings.Contains(output.String(), test.want) {
			t.Fatalf("%v help does not disclose %q: %q", test.arguments, test.want, output.String())
		}
	}

	output.Reset()
	if err := runWithIO(context.Background(), []string{"help"}, &output); err != nil {
		t.Fatalf("help error = %v", err)
	}
	defaultStore, err := resolveStorePath("")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), defaultStore) {
		t.Fatalf("help does not expose resolved default store: %q", output.String())
	}

	err = runWithIO(context.Background(), []string{"unknown"}, &output)
	if err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("unknown command error = %v", err)
	}
}

func TestWriteRunOutcomeJSONIsMachineReadable(t *testing.T) {
	var output bytes.Buffer
	outcome := application.RunOutcome{
		Run: runmodel.ReviewRun{
			SchemaVersion: runmodel.RunSchemaVersion,
			RunID:         "run-1",
			Kind:          runmodel.RunKindReview,
			TargetMode:    runmodel.TargetModeDiff,
			Status:        runmodel.RunStatusSucceeded,
		},
		FinalRef: runmodel.ArtifactRef{
			URI:      "artifact://local/sha256/" + strings.Repeat("a", 64),
			SHA256:   strings.Repeat("a", 64),
			Contract: runmodel.ContractReviewRun,
		},
	}
	if err := writeRunOutcome(&output, outcome, nil, "/tmp/store", true); err != nil {
		t.Fatalf("writeRunOutcome() error = %v", err)
	}
	var decoded struct {
		Run       runmodel.ReviewRun     `json:"run"`
		FinalRef  runmodel.ArtifactRef   `json:"final_ref"`
		Report    *reviewcore.Report     `json:"report,omitempty"`
		Reused    []reviewcore.StageName `json:"reused_stages"`
		Coverage  runCoverageSummary     `json:"coverage"`
		StorePath string                 `json:"store_path"`
	}
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatalf("decode output: %v\n%s", err, output.String())
	}
	if decoded.Run.RunID != "run-1" || decoded.StorePath != "/tmp/store" ||
		decoded.Reused == nil || len(decoded.Reused) != 0 ||
		decoded.Coverage.State != coverageUnknown {
		t.Fatalf("decoded output = %+v", decoded)
	}
}

func TestWriteRunOutcomeMakesPartialNoFindingsExplicitlyNonClean(t *testing.T) {
	var output bytes.Buffer
	outcome := application.RunOutcome{
		Run: runmodel.ReviewRun{
			RunID:      "run-partial",
			Kind:       runmodel.RunKindReview,
			TargetMode: runmodel.TargetModeScope,
			Status:     runmodel.RunStatusSucceeded,
			Evidence: []runmodel.RunEvidence{{
				Completeness:      runmodel.CompletenessPartial,
				CompletenessNotes: []string{"no_matching_files: no files matched"},
			}},
		},
		Markdown: "# Argus deterministic review\n\nNo findings.\n",
	}
	if err := writeRunOutcome(
		&output,
		outcome,
		nil,
		"/tmp/store",
		false,
	); err != nil {
		t.Fatalf("writeRunOutcome() error = %v", err)
	}
	for _, want := range []string{
		"Coverage: `partial`",
		`INCOMPLETE; "No findings" is not a clean verdict`,
		"no_matching_files: no files matched",
		"No findings.",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("output does not contain %q:\n%s", want, output.String())
		}
	}
}
