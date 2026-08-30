// Package contextprovider contains local adapters for the application context
// provider port. It does not own ContextRef or configuration semantics.
package contextprovider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"argus.local/argus/internal/application"
	"argus.local/argus/internal/contextcapture"
	goastcontext "argus.local/argus/internal/contextcapture/goast"
	compilecontext "argus.local/argus/internal/contextcapture/gocompile"
	depscontext "argus.local/argus/internal/contextcapture/godeps"
	searchcontext "argus.local/argus/internal/contextcapture/reposearch"
	"argus.local/argus/internal/reviewconfig"
)

const (
	goASTAdapterArtifact            = `{"schema_version":"argus.context_provider_adapter.v1alpha1","id":"argus-go-ast","revision":"2","kind":"go_ast","authority":"local_host"}`
	goDependenciesAdapterArtifact   = `{"schema_version":"argus.context_provider_adapter.v1alpha1","id":"argus-go-dependencies","revision":"1","kind":"dependency","authority":"local_host"}`
	goCompileAdapterArtifact        = `{"schema_version":"argus.context_provider_adapter.v1alpha1","id":"argus-go-compile","revision":"1","kind":"compile","authority":"local_host"}`
	repositorySearchAdapterArtifact = `{"schema_version":"argus.context_provider_adapter.v1alpha1","id":"argus-repository-search","revision":"2","kind":"repository_search","authority":"local_host"}`
)

// LocalAdapterArtifact returns the exact publishable component bytes for one
// built-in host adapter. Its semantic adapter ref and component artifact share
// one content digest, so formal planning never relies on an invented binding.
func LocalAdapterArtifact(definition reviewconfig.ContextProviderDefinition) ([]byte, error) {
	var content string
	switch {
	case definition.Kind == "go_ast" && definition.Adapter.ID == goastcontext.ProviderID &&
		definition.Adapter.Revision == goastcontext.ProviderRevision &&
		definition.Adapter.SHA256 == goastcontext.AdapterSHA256:
		content = goASTAdapterArtifact
	case definition.Kind == "dependency" && definition.Adapter.ID == depscontext.ProviderID &&
		definition.Adapter.Revision == depscontext.ProviderRevision &&
		definition.Adapter.SHA256 == depscontext.AdapterSHA256:
		content = goDependenciesAdapterArtifact
	case definition.Kind == "compile" && definition.Adapter.ID == compilecontext.ProviderID &&
		definition.Adapter.Revision == compilecontext.ProviderRevision &&
		definition.Adapter.SHA256 == compilecontext.AdapterSHA256:
		content = goCompileAdapterArtifact
	case definition.Kind == "repository_search" && definition.Adapter.ID == searchcontext.ProviderID &&
		definition.Adapter.Revision == searchcontext.ProviderRevision &&
		definition.Adapter.SHA256 == searchcontext.AdapterSHA256:
		content = repositorySearchAdapterArtifact
	default:
		return nil, fmt.Errorf("context provider adapter is not a publishable local built-in")
	}
	digest := sha256.Sum256([]byte(content))
	if hex.EncodeToString(digest[:]) != definition.Adapter.SHA256 {
		return nil, fmt.Errorf("built-in context provider adapter descriptor digest drifted")
	}
	return []byte(content), nil
}

func adapterDigestMatches(actual, current, legacy string) bool {
	return actual == current || actual == legacy
}

type LocalExecutor struct {
	goAST            *goastcontext.Provider
	goDependencies   *depscontext.Provider
	goCompile        *compilecontext.Provider
	goCompileError   error
	repositorySearch *searchcontext.Provider
}

func NewLocalExecutor(source goastcontext.Source) (*LocalExecutor, error) {
	provider, err := goastcontext.New(source)
	if err != nil {
		return nil, err
	}
	dependencies, err := depscontext.New(source)
	if err != nil {
		return nil, err
	}
	search, err := searchcontext.New(source)
	if err != nil {
		return nil, err
	}
	executor := &LocalExecutor{goAST: provider, goDependencies: dependencies, repositorySearch: search}
	runner, runnerErr := compilecontext.NewLocalRunner()
	if runnerErr == nil {
		executor.goCompile, runnerErr = compilecontext.New(source, runner)
	}
	executor.goCompileError = runnerErr
	return executor, nil
}

func (executor *LocalExecutor) Execute(
	ctx context.Context,
	request application.ContextProviderExecutionRequest,
) (application.ContextProviderCapture, error) {
	if executor == nil || executor.goAST == nil {
		return application.ContextProviderCapture{}, &application.ContextProviderError{
			Code: "provider_executor_unavailable", Err: fmt.Errorf("local executor is not initialized"),
		}
	}
	switch request.Definition.Kind {
	case "repository_search":
		adapter := request.Definition.Adapter
		if executor.repositorySearch == nil || adapter.ID != searchcontext.ProviderID ||
			adapter.Revision != searchcontext.ProviderRevision ||
			!adapterDigestMatches(adapter.SHA256, searchcontext.AdapterSHA256, searchcontext.LegacyAdapterSHA256) {
			return application.ContextProviderCapture{}, &application.ContextProviderError{
				Code: "adapter_identity_mismatch", Err: fmt.Errorf("repository search adapter identity is not executable"),
			}
		}
		captured, err := executor.repositorySearch.Capture(ctx, searchcontext.Request{
			RepositoryRoot: request.RepositoryRoot, CommitOID: request.CommitOID,
			TargetPaths: request.TargetPaths, PolicyInclude: request.PolicyInclude,
			TargetRanges:  applicationTargetRangesToRepositorySearch(request.TargetRanges),
			PolicyExclude: request.PolicyExclude, MaxFiles: request.MaxFiles,
			MaxFileBytes: request.MaxFileBytes, MaxArtifactBytes: request.MaxArtifactBytes,
		})
		if err != nil {
			return application.ContextProviderCapture{}, classifyCaptureError("search_capture_failed", err)
		}
		return application.ContextProviderCapture{
			Contract: searchcontext.SchemaVersion, Content: captured.Bytes,
			Revision: captured.Artifact.CommitOID, Coverage: captured.Coverage,
		}, nil
	case "go_ast":
		adapter := request.Definition.Adapter
		if adapter.ID != goastcontext.ProviderID ||
			adapter.Revision != goastcontext.ProviderRevision ||
			!adapterDigestMatches(adapter.SHA256, goastcontext.AdapterSHA256, goastcontext.LegacyAdapterSHA256) {
			return application.ContextProviderCapture{}, &application.ContextProviderError{
				Code: "adapter_identity_mismatch", Err: fmt.Errorf("go_ast adapter identity is not executable"),
			}
		}
		captured, err := executor.goAST.Capture(ctx, goastcontext.Request{
			RepositoryRoot:   request.RepositoryRoot,
			CommitOID:        request.CommitOID,
			TargetPaths:      request.TargetPaths,
			PolicyInclude:    request.PolicyInclude,
			PolicyExclude:    request.PolicyExclude,
			MaxFiles:         request.MaxFiles,
			MaxFileBytes:     request.MaxFileBytes,
			MaxArtifactBytes: request.MaxArtifactBytes,
		})
		if err != nil {
			return application.ContextProviderCapture{}, classifyCaptureError("capture_failed", err)
		}
		return application.ContextProviderCapture{
			Contract: goastcontext.SchemaVersion,
			Content:  captured.Bytes,
			Revision: captured.Artifact.CommitOID,
			Coverage: captured.Coverage,
		}, nil
	case "dependency":
		adapter := request.Definition.Adapter
		if executor.goDependencies == nil || adapter.ID != depscontext.ProviderID ||
			adapter.Revision != depscontext.ProviderRevision ||
			!adapterDigestMatches(adapter.SHA256, depscontext.AdapterSHA256, depscontext.LegacyAdapterSHA256) {
			return application.ContextProviderCapture{}, &application.ContextProviderError{
				Code: "adapter_identity_mismatch", Err: fmt.Errorf("Go dependency adapter identity is not executable"),
			}
		}
		captured, err := executor.goDependencies.Capture(ctx, depscontext.Request{
			RepositoryRoot: request.RepositoryRoot, CommitOID: request.CommitOID,
			TargetPaths: request.TargetPaths, PolicyInclude: request.PolicyInclude,
			PolicyExclude: request.PolicyExclude, MaxFiles: request.MaxFiles,
			MaxFileBytes: request.MaxFileBytes, MaxArtifactBytes: request.MaxArtifactBytes,
		})
		if err != nil {
			return application.ContextProviderCapture{}, classifyCaptureError("capture_failed", err)
		}
		return application.ContextProviderCapture{
			Contract: depscontext.SchemaVersion, Content: captured.Bytes,
			Revision: captured.Artifact.CommitOID, Coverage: captured.Coverage,
		}, nil
	case "compile":
		adapter := request.Definition.Adapter
		if executor.goCompile == nil || adapter.ID != compilecontext.ProviderID ||
			adapter.Revision != compilecontext.ProviderRevision ||
			!adapterDigestMatches(adapter.SHA256, compilecontext.AdapterSHA256, compilecontext.LegacyAdapterSHA256) {
			return application.ContextProviderCapture{}, &application.ContextProviderError{
				Code: "adapter_identity_mismatch", Err: fmt.Errorf(
					"Go compile adapter identity is not executable: %v", executor.goCompileError,
				),
			}
		}
		captured, err := executor.goCompile.Capture(ctx, compilecontext.Request{
			RepositoryRoot: request.RepositoryRoot, CommitOID: request.CommitOID,
			TargetPaths: request.TargetPaths, PolicyInclude: request.PolicyInclude,
			PolicyExclude: request.PolicyExclude, MaxFiles: request.MaxFiles,
			MaxFileBytes: request.MaxFileBytes, MaxArtifactBytes: request.MaxArtifactBytes,
		})
		if err != nil {
			return application.ContextProviderCapture{}, classifyCaptureError("compile_capture_failed", err)
		}
		return application.ContextProviderCapture{
			Contract: compilecontext.SchemaVersion, Content: captured.Bytes,
			Revision: captured.Artifact.CommitOID, Coverage: captured.Coverage,
		}, nil
	default:
		return application.ContextProviderCapture{}, &application.ContextProviderError{
			Code: "provider_kind_unsupported", Err: fmt.Errorf("unsupported provider kind %q", request.Definition.Kind),
		}
	}
}

func applicationTargetRangesToRepositorySearch(
	values []application.ContextProviderTargetRange,
) []searchcontext.TargetRange {
	result := make([]searchcontext.TargetRange, len(values))
	for index, value := range values {
		result[index] = searchcontext.TargetRange{
			Path: value.Path, StartLine: value.StartLine, EndLine: value.EndLine,
		}
	}
	return result
}

func classifyCaptureError(defaultCode string, err error) *application.ContextProviderError {
	var mismatch *contextcapture.RevisionMismatchError
	if errors.As(err, &mismatch) {
		return &application.ContextProviderError{
			Code:             contextcapture.RevisionMismatchReasonCode,
			ObservedRevision: mismatch.Observed,
			Err:              err,
		}
	}
	return &application.ContextProviderError{Code: defaultCode, Err: err}
}
