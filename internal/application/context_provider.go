package application

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"argus.local/argus/internal/contextcapture"
	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/source/gitadapter"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

type ContextProviderExecutionRequest struct {
	Definition       reviewconfig.ContextProviderDefinition
	RepositoryRoot   string
	RepositoryID     string
	CommitOID        string
	TargetPaths      []string
	TargetRanges     []ContextProviderTargetRange
	PolicyInclude    []string
	PolicyExclude    []string
	MaxFiles         int
	MaxFileBytes     int64
	MaxArtifactBytes int64
}

type ContextProviderTargetRange struct {
	Path      string `json:"path"`
	StartLine uint32 `json:"start_line"`
	EndLine   uint32 `json:"end_line"`
}

type ContextProviderCapture struct {
	Contract string
	Content  []byte
	Revision string
	Coverage reviewcore.ContextCoverage
}

// ContextProviderExecutor is the application port for version-pinned context
// adapters. The application owns exact target freezing, artifact publication,
// ContextRef/Gap identity, budgets, and ordering.
type ContextProviderExecutor interface {
	Execute(context.Context, ContextProviderExecutionRequest) (ContextProviderCapture, error)
}

type ContextProviderError struct {
	Code             string
	ObservedRevision string
	Err              error
}

func (failure *ContextProviderError) Error() string {
	if failure == nil {
		return "<nil>"
	}
	if failure.Err == nil {
		return failure.Code
	}
	return failure.Code + ": " + failure.Err.Error()
}

func (failure *ContextProviderError) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.Err
}

type frozenProviderTarget struct {
	repositoryRoot string
	repositoryID   string
	commitOID      string
	targetPaths    []string
	targetRanges   []ContextProviderTargetRange
}

type configuredContextProviderResult struct {
	binding          reviewcore.ContextBinding
	receiptRef       runmodel.ArtifactRef
	observedRevision string
	err              error
}

type contextProviderExecutionOutcome struct {
	binding          reviewcore.ContextBinding
	observedRevision string
}

func (service *Service) prepareConfiguredContexts(
	ctx context.Context,
	request ReviewRequest,
	bundle reviewconfig.ConfigBundle,
	config LocalConfig,
) (ReviewRequest, []runmodel.ArtifactRef, error) {
	providers := bundle.Execution.ContextProviders
	if len(providers) == 0 {
		return request, []runmodel.ArtifactRef{}, nil
	}
	if err := request.Validate(config); err != nil {
		return ReviewRequest{}, nil, err
	}
	if len(request.Contexts)+len(providers) >
		contractsv1alpha1.AgentReviewWorkerMaxContextCount {
		return ReviewRequest{}, nil, fmt.Errorf(
			"configured and frozen contexts exceed maximum %d",
			contractsv1alpha1.AgentReviewWorkerMaxContextCount,
		)
	}
	frozen, err := service.freezeContextProviderTarget(ctx, &request, config)
	if err != nil {
		return ReviewRequest{}, nil, err
	}
	executionRequests := make([]ContextProviderExecutionRequest, len(providers))
	for index, definition := range providers {
		executionRequests[index] = ContextProviderExecutionRequest{
			Definition: definition, RepositoryRoot: frozen.repositoryRoot,
			RepositoryID: frozen.repositoryID, CommitOID: frozen.commitOID,
			TargetPaths:   slices.Clone(frozen.targetPaths),
			TargetRanges:  slices.Clone(frozen.targetRanges),
			PolicyInclude: slices.Clone(config.TargetInclude),
			PolicyExclude: slices.Clone(config.TargetExclude),
			MaxFiles:      config.MaxFiles, MaxFileBytes: config.MaxFileContentBytes,
			MaxArtifactBytes: contractsv1alpha1.AgentReviewContextArtifactMaxBytes,
		}
	}
	results, err := service.executeConfiguredContextProviders(
		ctx,
		executionRequests,
		bundle.Budget.StageTimeoutMS,
		bundle.Execution.ContextProviderMaxConcurrency,
		request.OverlayContent != nil,
	)
	if err != nil {
		return ReviewRequest{}, nil, err
	}
	bindings := slices.Clone(request.Contexts)
	receiptRefs := make([]runmodel.ArtifactRef, len(results))
	for index, result := range results {
		bindings = append(bindings, result.binding)
		receiptRefs[index] = result.receiptRef
	}
	sort.Slice(bindings, func(left, right int) bool {
		return bindings[left].ContextID() < bindings[right].ContextID()
	})
	for index := 1; index < len(bindings); index++ {
		if bindings[index-1].ContextID() == bindings[index].ContextID() {
			return ReviewRequest{}, nil, fmt.Errorf(
				"configured contexts contain duplicate context ID %q",
				bindings[index].ContextID(),
			)
		}
	}
	request.Contexts = bindings
	return request, receiptRefs, nil
}

func (service *Service) executeConfiguredContextProviders(
	ctx context.Context,
	requests []ContextProviderExecutionRequest,
	timeoutMS int64,
	maxConcurrency int,
	overlay bool,
) ([]configuredContextProviderResult, error) {
	if len(requests) == 0 {
		return []configuredContextProviderResult{}, nil
	}
	if maxConcurrency <= 0 {
		return nil, fmt.Errorf("context provider max concurrency must be positive")
	}
	workerCount := min(maxConcurrency, len(requests))
	workerContext, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int)
	results := make([]configuredContextProviderResult, len(requests))
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for index := range jobs {
				result := service.executeConfiguredContextProviderWithReceipt(
					workerContext,
					requests[index],
					timeoutMS,
					overlay,
				)
				results[index] = result
				if result.err != nil {
					cancel()
				}
			}
		}()
	}
	scheduled := len(requests)
schedule:
	for index := range requests {
		select {
		case jobs <- index:
		case <-workerContext.Done():
			scheduled = index
			break schedule
		}
	}
	close(jobs)
	workers.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for index := range scheduled {
		if results[index].err != nil {
			return nil, fmt.Errorf(
				"execute context provider %s@%s: %w",
				requests[index].Definition.ID,
				requests[index].Definition.Revision,
				results[index].err,
			)
		}
	}
	if scheduled != len(requests) {
		return nil, fmt.Errorf("context provider execution canceled before all providers were scheduled")
	}
	return results, nil
}

func (service *Service) executeConfiguredContextProviderWithReceipt(
	ctx context.Context,
	request ContextProviderExecutionRequest,
	timeoutMS int64,
	overlay bool,
) configuredContextProviderResult {
	startedAt, err := service.timestamp()
	if err != nil {
		return configuredContextProviderResult{err: err}
	}
	outcome, err := service.executeConfiguredContextProviderDetailed(
		ctx,
		request,
		timeoutMS,
		overlay,
	)
	if err != nil {
		return configuredContextProviderResult{err: err}
	}
	completedAt, err := service.timestamp()
	if err != nil {
		return configuredContextProviderResult{err: err}
	}
	receiptRef, err := service.persistContextProviderReceipt(
		request,
		outcome.binding,
		outcome.observedRevision,
		timeoutMS,
		startedAt,
		completedAt,
	)
	return configuredContextProviderResult{
		binding:          outcome.binding,
		receiptRef:       receiptRef,
		observedRevision: outcome.observedRevision,
		err:              err,
	}
}

func (service *Service) persistContextProviderReceipt(
	request ContextProviderExecutionRequest,
	binding reviewcore.ContextBinding,
	observedRevision string,
	timeoutMS int64,
	startedAt time.Time,
	completedAt time.Time,
) (runmodel.ArtifactRef, error) {
	requestDigest, err := digestContextProviderRequest(request)
	if err != nil {
		return runmodel.ArtifactRef{}, err
	}
	receipt := contractsv1alpha1.ContextProviderExecutionReceipt{
		ProviderID: request.Definition.ID, ProviderRevision: request.Definition.Revision,
		Kind: request.Definition.Kind,
		Adapter: contractsv1alpha1.VersionedRef{
			ID: request.Definition.Adapter.ID, Revision: request.Definition.Adapter.Revision,
			SHA256: request.Definition.Adapter.SHA256,
		},
		RequestSHA256: requestDigest, RepositoryID: request.RepositoryID,
		CommitOID: request.CommitOID, TargetPaths: slices.Clone(request.TargetPaths),
		TargetRanges:     make([]contractsv1alpha1.ContextProviderTargetRange, len(request.TargetRanges)),
		ObservedRevision: observedRevision,
		TimeoutMS:        timeoutMS, StartedAt: startedAt, CompletedAt: completedAt,
		DurationMS: completedAt.Sub(startedAt).Milliseconds(),
		Authority:  "local_host_observation",
	}
	for index, targetRange := range request.TargetRanges {
		receipt.TargetRanges[index] = contractsv1alpha1.ContextProviderTargetRange{
			Path: targetRange.Path, StartLine: targetRange.StartLine, EndLine: targetRange.EndLine,
		}
	}
	if binding.Ref != nil {
		receipt.Status = contractsv1alpha1.ContextProviderReceiptSucceeded
		receipt.ContextID = binding.Ref.ContextID
		receipt.ContextDigest = binding.Ref.Digest
		receipt.ContextContract = binding.Ref.Contract
	} else if binding.Gap != nil {
		receipt.Status = contractsv1alpha1.ContextProviderReceiptGap
		receipt.ReasonCode = binding.Gap.ReasonCode
		receipt.ContextID = binding.Gap.ContextID
		receipt.ContextDigest = binding.Gap.Digest
	} else {
		return runmodel.ArtifactRef{}, fmt.Errorf("context provider binding is empty")
	}
	receipt, err = contractsv1alpha1.SealContextProviderExecutionReceipt(receipt)
	if err != nil {
		return runmodel.ArtifactRef{}, err
	}
	ref, err := service.repository.PutJSONArtifact(
		runmodel.ContractContextProviderExecutionReceipt,
		receipt,
	)
	if err != nil {
		return runmodel.ArtifactRef{}, err
	}
	return ref, nil
}

func (service *Service) executeConfiguredContextProvider(
	ctx context.Context,
	request ContextProviderExecutionRequest,
	timeoutMS int64,
	overlay bool,
) (reviewcore.ContextBinding, error) {
	outcome, err := service.executeConfiguredContextProviderDetailed(ctx, request, timeoutMS, overlay)
	return outcome.binding, err
}

func (service *Service) executeConfiguredContextProviderDetailed(
	ctx context.Context,
	request ContextProviderExecutionRequest,
	timeoutMS int64,
	overlay bool,
) (contextProviderExecutionOutcome, error) {
	requestDigest, err := digestContextProviderRequest(request)
	if err != nil {
		return contextProviderExecutionOutcome{}, err
	}
	provenance := reviewcore.ContextProvenance{
		Provider:         request.Definition.Kind,
		ProducerID:       request.Definition.Adapter.ID,
		ProducerRevision: request.Definition.Adapter.Revision,
	}
	coverageMarker := reviewcore.ContextCoverage{
		Spans:   []reviewcore.ContextSpan{},
		Symbols: []string{"provider:" + request.Definition.ID},
	}
	if overlay {
		binding, gapErr := contextProviderGap(request, requestDigest, provenance, coverageMarker, "overlay_not_supported")
		return contextProviderExecutionOutcome{binding: binding}, gapErr
	}
	if service.contextProviders == nil {
		binding, gapErr := contextProviderGap(request, requestDigest, provenance, coverageMarker, "provider_executor_unavailable")
		return contextProviderExecutionOutcome{binding: binding}, gapErr
	}
	if timeoutMS <= 0 {
		return contextProviderExecutionOutcome{}, fmt.Errorf("context provider timeout must be positive")
	}
	providerContext, cancel := context.WithTimeout(ctx, time.Duration(timeoutMS)*time.Millisecond)
	defer cancel()
	capture, executeErr := service.contextProviders.Execute(providerContext, request)
	if executeErr != nil {
		if ctx.Err() != nil {
			return contextProviderExecutionOutcome{}, ctx.Err()
		}
		code := "provider_failed"
		if errors.Is(executeErr, context.DeadlineExceeded) || errors.Is(providerContext.Err(), context.DeadlineExceeded) {
			code = "provider_timeout"
		}
		var classified *ContextProviderError
		observedRevision := ""
		if errors.As(executeErr, &classified) && classified.Code != "" {
			code = classified.Code
			observedRevision = classified.ObservedRevision
		}
		binding, gapErr := contextProviderGap(request, requestDigest, provenance, coverageMarker, code)
		return contextProviderExecutionOutcome{
			binding: binding, observedRevision: observedRevision,
		}, gapErr
	}
	if capture.Contract == "" || len(capture.Content) == 0 ||
		int64(len(capture.Content)) > request.MaxArtifactBytes ||
		!utf8.Valid(capture.Content) || bytes.IndexByte(capture.Content, 0) >= 0 ||
		!contextcapture.IsExactRevision(capture.Revision) {
		binding, gapErr := contextProviderGap(request, requestDigest, provenance, coverageMarker, "provider_output_invalid")
		return contextProviderExecutionOutcome{binding: binding}, gapErr
	}
	if capture.Revision != request.CommitOID {
		binding, gapErr := contextProviderGap(
			request, requestDigest, provenance, coverageMarker, contextcapture.RevisionMismatchReasonCode,
		)
		return contextProviderExecutionOutcome{
			binding: binding, observedRevision: capture.Revision,
		}, gapErr
	}
	probe := runmodel.ArtifactRef{
		URI:    "artifact://local/sha256/" + strings.Repeat("0", 64),
		SHA256: strings.Repeat("0", 64), SizeBytes: int64(len(capture.Content)),
		Contract: capture.Contract,
	}
	if err := probe.Validate(); err != nil {
		binding, gapErr := contextProviderGap(request, requestDigest, provenance, coverageMarker, "provider_output_invalid")
		return contextProviderExecutionOutcome{binding: binding}, gapErr
	}
	ref, err := service.repository.PutArtifact(capture.Contract, capture.Content)
	if err != nil {
		return contextProviderExecutionOutcome{}, fmt.Errorf("persist context provider output: %w", err)
	}
	binding := reviewcore.ContextBinding{Ref: &reviewcore.ContextRef{
		ContextID: request.Definition.ID + "-" + ref.SHA256[:16],
		Kind:      request.Definition.Kind, Revision: request.CommitOID, Digest: ref.SHA256,
		Coverage: capture.Coverage, Provenance: provenance,
		ArtifactURI: ref.URI, Contract: ref.Contract, SizeBytes: ref.SizeBytes,
	}}
	if err := binding.Validate(); err != nil {
		gap, gapErr := contextProviderGap(request, requestDigest, provenance, coverageMarker, "provider_output_invalid")
		return contextProviderExecutionOutcome{binding: gap}, gapErr
	}
	return contextProviderExecutionOutcome{binding: binding, observedRevision: capture.Revision}, nil
}

func contextProviderGap(
	request ContextProviderExecutionRequest,
	requestDigest string,
	provenance reviewcore.ContextProvenance,
	coverage reviewcore.ContextCoverage,
	reasonCode string,
) (reviewcore.ContextBinding, error) {
	binding := reviewcore.ContextBinding{Gap: &reviewcore.ContextGap{
		ContextID: request.Definition.ID + "-gap-" + requestDigest[:12],
		Kind:      request.Definition.Kind, Revision: request.CommitOID,
		Digest: requestDigest, Coverage: coverage, Provenance: provenance,
		ReasonCode: reasonCode,
	}}
	if err := binding.Validate(); err != nil {
		return reviewcore.ContextBinding{}, fmt.Errorf("validate context provider gap: %w", err)
	}
	return binding, nil
}

func digestContextProviderRequest(request ContextProviderExecutionRequest) (string, error) {
	canonical := struct {
		Definition       reviewconfig.ContextProviderDefinition `json:"definition"`
		RepositoryID     string                                 `json:"repository_id"`
		CommitOID        string                                 `json:"commit_oid"`
		TargetPaths      []string                               `json:"target_paths"`
		TargetRanges     []ContextProviderTargetRange           `json:"target_ranges"`
		PolicyInclude    []string                               `json:"policy_include"`
		PolicyExclude    []string                               `json:"policy_exclude"`
		MaxFiles         int                                    `json:"max_files"`
		MaxFileBytes     int64                                  `json:"max_file_bytes"`
		MaxArtifactBytes int64                                  `json:"max_artifact_bytes"`
	}{
		Definition: request.Definition, RepositoryID: request.RepositoryID,
		CommitOID: request.CommitOID, TargetPaths: slices.Clone(request.TargetPaths),
		TargetRanges:  slices.Clone(request.TargetRanges),
		PolicyInclude: slices.Clone(request.PolicyInclude),
		PolicyExclude: slices.Clone(request.PolicyExclude),
		MaxFiles:      request.MaxFiles, MaxFileBytes: request.MaxFileBytes,
		MaxArtifactBytes: request.MaxArtifactBytes,
	}
	data, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("marshal context provider request: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func (service *Service) freezeContextProviderTarget(
	ctx context.Context,
	request *ReviewRequest,
	config LocalConfig,
) (frozenProviderTarget, error) {
	switch request.normalizedMode() {
	case reviewcore.TargetModeDiff:
		result, err := service.source.MaterializeDiff(ctx, gitadapter.Request{
			RepositoryPath: request.RepositoryPath,
			BaseRevision:   request.BaseRevision, HeadRevision: request.HeadRevision,
			Limits: gitadapter.Limits{MaxPatchBytes: config.MaxPatchBytes, MaxFiles: config.MaxFiles},
		})
		if err != nil {
			return frozenProviderTarget{}, fmt.Errorf("freeze diff context target: %w", err)
		}
		if err := validateDiffResult(ctx, result, *request, config.MaxPatchBytes, config.MaxFiles); err != nil {
			return frozenProviderTarget{}, fmt.Errorf("validate diff context target: %w", err)
		}
		if result.Snapshot == nil || result.Manifest == nil {
			return frozenProviderTarget{}, fmt.Errorf("diff context target is missing snapshot or manifest")
		}
		_, manifest, err := admitDiffArtifacts(result)
		if err != nil {
			return frozenProviderTarget{}, err
		}
		if err := validateDiffTargetPolicy(manifest, config); err != nil {
			return frozenProviderTarget{}, err
		}
		paths := make([]string, 0, len(manifest.Files))
		for _, file := range manifest.Files {
			if file.Included && file.Status != gitadapter.ChangeDeleted {
				paths = append(paths, file.Path)
			}
		}
		sort.Strings(paths)
		request.BaseRevision = result.Snapshot.Base.CommitOID
		request.HeadRevision = result.Snapshot.Head.CommitOID
		return frozenProviderTarget{
			repositoryRoot: result.RepositoryRoot,
			repositoryID:   result.Snapshot.Repository.RepositoryID,
			commitOID:      result.Snapshot.Head.CommitOID, targetPaths: paths,
			targetRanges: []ContextProviderTargetRange{},
		}, nil
	case reviewcore.TargetModeSelection:
		allowed, err := targetPathAllowed(config, request.SelectionPath)
		if err != nil || !allowed {
			if err != nil {
				return frozenProviderTarget{}, err
			}
			return frozenProviderTarget{}, fmt.Errorf("selection path is denied by resolved target policy")
		}
		capture, err := service.source.CaptureRevision(ctx, gitadapter.RevisionRequest{
			RepositoryPath: request.RepositoryPath, Revision: request.Revision,
		})
		if err != nil {
			return frozenProviderTarget{}, fmt.Errorf("freeze selection context target: %w", err)
		}
		if err := validateRevisionCapture(capture, request.RepositoryPath, request.Revision); err != nil {
			return frozenProviderTarget{}, err
		}
		request.Revision = capture.Revision.CommitOID
		ranges, err := service.freezeContextProviderSelectionRanges(ctx, capture, *request, config)
		if err != nil {
			return frozenProviderTarget{}, err
		}
		return frozenProviderTarget{
			repositoryRoot: capture.RepositoryRoot,
			repositoryID:   capture.Repository.RepositoryID,
			commitOID:      capture.Revision.CommitOID,
			targetPaths:    []string{request.SelectionPath},
			targetRanges:   ranges,
		}, nil
	case reviewcore.TargetModeScope:
		capture, err := service.source.CaptureRevision(ctx, gitadapter.RevisionRequest{
			RepositoryPath: request.RepositoryPath, Revision: request.Revision,
		})
		if err != nil {
			return frozenProviderTarget{}, fmt.Errorf("freeze scope context target: %w", err)
		}
		if err := validateRevisionCapture(capture, request.RepositoryPath, request.Revision); err != nil {
			return frozenProviderTarget{}, err
		}
		scopeSource, ok := service.source.(scopeAdmissionTargetSource)
		if !ok {
			return frozenProviderTarget{}, fmt.Errorf("target source does not support scope provider admission")
		}
		listing, err := scopeSource.ListFilesAtCommitWithAdmission(
			ctx, capture.RepositoryRoot, capture.Revision.CommitOID,
			gitadapter.ScopeAdmission{
				Include: slices.Clone(request.Include), Exclude: slices.Clone(request.Exclude),
				PolicyInclude: slices.Clone(config.TargetInclude),
				PolicyExclude: slices.Clone(config.TargetExclude),
			},
			config.MaxFiles,
		)
		if err != nil {
			return frozenProviderTarget{}, fmt.Errorf("freeze scope context files: %w", err)
		}
		if err := validateRevisionFileList(
			listing, capture, request.Include, request.Exclude,
			config.TargetInclude, config.TargetExclude, config.MaxFiles,
		); err != nil {
			return frozenProviderTarget{}, err
		}
		paths := make([]string, len(listing.Files))
		for index, file := range listing.Files {
			paths[index] = file.Path
		}
		sort.Strings(paths)
		request.Revision = capture.Revision.CommitOID
		return frozenProviderTarget{
			repositoryRoot: capture.RepositoryRoot,
			repositoryID:   capture.Repository.RepositoryID,
			commitOID:      capture.Revision.CommitOID, targetPaths: paths,
			targetRanges: []ContextProviderTargetRange{},
		}, nil
	default:
		return frozenProviderTarget{}, fmt.Errorf("unsupported context provider target mode %q", request.Mode)
	}
}

func (service *Service) freezeContextProviderSelectionRanges(
	ctx context.Context,
	capture gitadapter.RevisionCapture,
	request ReviewRequest,
	config LocalConfig,
) ([]ContextProviderTargetRange, error) {
	var ranges []SelectionRange
	if request.StartLine != 0 || request.EndLine != 0 {
		ranges = []SelectionRange{{StartLine: request.StartLine, EndLine: request.EndLine}}
	} else if len(request.SelectionRanges) > 0 {
		ranges = slices.Clone(request.SelectionRanges)
	} else {
		var content string
		if request.OverlayContent != nil {
			content = *request.OverlayContent
		} else {
			file, err := service.source.ReadFileAtCommit(
				ctx, capture.RepositoryRoot, capture.Revision.CommitOID,
				request.SelectionPath, config.MaxFileContentBytes,
			)
			if err != nil {
				return nil, fmt.Errorf("read selection for context range: %w", err)
			}
			if err := validateFileContent(
				file, capture.Revision.CommitOID, capture.Repository.ObjectFormat,
				request.SelectionPath, nil, false, config.MaxFileContentBytes,
			); err != nil {
				return nil, fmt.Errorf("validate selection for context range: %w", err)
			}
			if !file.Included {
				return nil, fmt.Errorf("selection is unavailable for context range")
			}
			content = string(file.Data)
		}
		resolved, _, err := resolveSelectionSelector(content, request)
		if err != nil {
			return nil, fmt.Errorf("resolve selection context range: %w", err)
		}
		ranges = resolved
	}
	result := make([]ContextProviderTargetRange, len(ranges))
	for index, lineRange := range ranges {
		result[index] = ContextProviderTargetRange{
			Path: request.SelectionPath, StartLine: lineRange.StartLine, EndLine: lineRange.EndLine,
		}
	}
	return result, nil
}
