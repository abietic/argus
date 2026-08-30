package application

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"sort"

	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/source/gitadapter"
	"argus.local/argus/internal/targetmodel"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

type indexReplayMaterialization struct {
	targetRef   runmodel.ArtifactRef
	input       reviewcore.ReviewInput
	inputRef    runmodel.ArtifactRef
	receiptRefs []runmodel.ArtifactRef
}

type IndexReplayMaterializationRequest struct {
	SourceRun      runmodel.ReviewRun
	SourceSnapshot runmodel.ExecutionSnapshot
	SourceBundle   reviewconfig.ConfigBundle
	VariantBundle  reviewconfig.ConfigBundle
}

type IndexReplayMaterialization struct {
	TargetRef                  runmodel.ArtifactRef
	ReviewInput                reviewcore.ReviewInput
	ReviewInputRef             runmodel.ArtifactRef
	ContextProviderReceiptRefs []runmodel.ArtifactRef
}

type IndexReplayMaterializer interface {
	MaterializeIndexReplay(context.Context, IndexReplayMaterializationRequest) (IndexReplayMaterialization, error)
	ValidateIndexReplayMaterialization(context.Context, IndexReplayMaterializationRequest, IndexReplayMaterialization) error
}

func (service *Service) MaterializeIndexReplay(
	ctx context.Context,
	request IndexReplayMaterializationRequest,
) (IndexReplayMaterialization, error) {
	if service == nil || service.repository == nil || service.source == nil {
		return IndexReplayMaterialization{}, fmt.Errorf("index replay materializer is not initialized")
	}
	if _, err := reviewconfig.DiffReplayIndexPolicy(
		request.SourceBundle.Execution, request.VariantBundle.Execution,
	); err != nil {
		return IndexReplayMaterialization{}, err
	}
	data, err := service.repository.ReadArtifact(request.SourceSnapshot.ReviewInputRef)
	if err != nil {
		return IndexReplayMaterialization{}, fmt.Errorf("read index replay source ReviewInput: %w", err)
	}
	input, err := reviewcore.DecodeReviewInput(data)
	if err != nil {
		return IndexReplayMaterialization{}, fmt.Errorf("decode index replay source ReviewInput: %w", err)
	}
	materialized, err := service.materializeIndexReplay(
		ctx, request.SourceRun, request.SourceSnapshot, request.SourceBundle,
		request.VariantBundle, input,
	)
	if err != nil {
		return IndexReplayMaterialization{}, err
	}
	result := IndexReplayMaterialization{
		TargetRef: materialized.targetRef, ReviewInput: materialized.input,
		ReviewInputRef:             materialized.inputRef,
		ContextProviderReceiptRefs: slices.Clone(materialized.receiptRefs),
	}
	if err := service.ValidateIndexReplayMaterialization(ctx, request, result); err != nil {
		return IndexReplayMaterialization{}, err
	}
	return result, nil
}

func (service *Service) materializeIndexReplay(
	ctx context.Context,
	sourceRun runmodel.ReviewRun,
	sourceSnapshot runmodel.ExecutionSnapshot,
	sourceBundle reviewconfig.ConfigBundle,
	variantBundle reviewconfig.ConfigBundle,
	sourceInput reviewcore.ReviewInput,
) (indexReplayMaterialization, error) {
	if service.contextProviders == nil && len(variantBundle.Execution.ContextProviders) != 0 {
		return indexReplayMaterialization{}, fmt.Errorf(
			"index replay requires the version-pinned context provider executor",
		)
	}
	config, err := contextMaterializationConfigFromBundle(variantBundle, service.configSource)
	if err != nil {
		return indexReplayMaterialization{}, err
	}
	var target MaterializedTarget
	if err := service.repository.ReadJSONArtifact(sourceSnapshot.TargetSnapshotRef, &target); err != nil {
		return indexReplayMaterialization{}, fmt.Errorf("load source materialized target: %w", err)
	}
	if err := target.Validate(); err != nil {
		return indexReplayMaterialization{}, fmt.Errorf("validate source materialized target: %w", err)
	}
	if target.Snapshot.TargetSnapshotID != sourceInput.TargetID ||
		target.Snapshot.Repository.RepositoryID != sourceBundle.Context.RepositoryID {
		return indexReplayMaterialization{}, fmt.Errorf(
			"source target, ReviewInput, and ConfigBundle context do not match",
		)
	}

	targetPaths, err := service.indexReplayTargetPaths(target)
	if err != nil {
		return indexReplayMaterialization{}, err
	}
	targetRanges, err := indexReplayTargetRanges(target)
	if err != nil {
		return indexReplayMaterialization{}, err
	}
	capture, err := service.source.CaptureRevision(ctx, gitadapter.RevisionRequest{
		RepositoryPath: sourceRun.RepositoryPath,
		Revision:       target.Snapshot.Head.CommitOID,
	})
	if err != nil {
		return indexReplayMaterialization{}, fmt.Errorf("capture index replay revision: %w", err)
	}
	if err := validateRevisionCapture(
		capture,
		sourceRun.RepositoryPath,
		target.Snapshot.Head.CommitOID,
	); err != nil {
		return indexReplayMaterialization{}, fmt.Errorf("validate index replay revision: %w", err)
	}
	if capture.Repository.RepositoryID != target.Snapshot.Repository.RepositoryID ||
		capture.Repository.ObjectFormat != target.Snapshot.Repository.ObjectFormat ||
		capture.Revision.CommitOID != target.Snapshot.Head.CommitOID {
		return indexReplayMaterialization{}, fmt.Errorf(
			"current repository does not match the exact frozen replay target",
		)
	}

	providerContextIDs, err := service.sourceProviderContextIDs(
		sourceSnapshot.ContextProviderReceiptRefs,
		sourceBundle.Execution.ContextProviders,
		target,
	)
	if err != nil {
		return indexReplayMaterialization{}, err
	}
	preserved, err := removeConfiguredContextBindings(sourceInput.Contexts, providerContextIDs)
	if err != nil {
		return indexReplayMaterialization{}, err
	}
	if len(preserved)+len(variantBundle.Execution.ContextProviders) >
		contractsv1alpha1.AgentReviewWorkerMaxContextCount {
		return indexReplayMaterialization{}, fmt.Errorf(
			"index replay contexts exceed maximum %d",
			contractsv1alpha1.AgentReviewWorkerMaxContextCount,
		)
	}

	requests := make(
		[]ContextProviderExecutionRequest,
		len(variantBundle.Execution.ContextProviders),
	)
	for index, definition := range variantBundle.Execution.ContextProviders {
		requests[index] = ContextProviderExecutionRequest{
			Definition: definition, RepositoryRoot: capture.RepositoryRoot,
			RepositoryID:  target.Snapshot.Repository.RepositoryID,
			CommitOID:     target.Snapshot.Head.CommitOID,
			TargetPaths:   slices.Clone(targetPaths),
			TargetRanges:  slices.Clone(targetRanges),
			PolicyInclude: slices.Clone(config.TargetInclude),
			PolicyExclude: slices.Clone(config.TargetExclude),
			MaxFiles:      config.MaxFiles, MaxFileBytes: config.MaxFileContentBytes,
			MaxArtifactBytes: contractsv1alpha1.AgentReviewContextArtifactMaxBytes,
		}
	}
	results, err := service.executeConfiguredContextProviders(
		ctx,
		requests,
		variantBundle.Budget.StageTimeoutMS,
		variantBundle.Execution.ContextProviderMaxConcurrency,
		false,
	)
	if err != nil {
		return indexReplayMaterialization{}, err
	}
	revalidation, err := service.source.RevalidateRevision(
		ctx,
		capture.RepositoryRoot,
		capture.Revision.Requested,
		capture.Revision.CommitOID,
	)
	if err != nil {
		return indexReplayMaterialization{}, fmt.Errorf("revalidate index replay revision: %w", err)
	}
	if err := validateRevisionRevalidation(revalidation, capture); err != nil {
		return indexReplayMaterialization{}, fmt.Errorf("validate index replay revalidation: %w", err)
	}
	if revalidation.State != gitadapter.RevisionStateUnchanged {
		return indexReplayMaterialization{}, fmt.Errorf(
			"index replay target changed during context capture",
		)
	}

	bindings := cloneContextBindings(preserved)
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
			return indexReplayMaterialization{}, fmt.Errorf(
				"index replay produced duplicate context ID %q",
				bindings[index].ContextID(),
			)
		}
	}
	contextBytes, err := frozenContextBytes(bindings, config.MaxMaterializedBytes)
	if err != nil {
		return indexReplayMaterialization{}, err
	}
	baseBytes := int64(0)
	if target.PatchRef != nil {
		baseBytes += target.PatchRef.SizeBytes
	}
	for _, file := range target.FileRefs {
		if file.ContentRef != nil {
			baseBytes += file.ContentRef.SizeBytes
		}
	}
	if baseBytes > config.MaxMaterializedBytes-contextBytes {
		return indexReplayMaterialization{}, fmt.Errorf(
			"index replay target plus context exceeds max_materialized_bytes=%d",
			config.MaxMaterializedBytes,
		)
	}

	variantInput := sourceInput
	variantInput.Contexts = cloneContextBindings(bindings)
	if err := variantInput.Validate(); err != nil {
		return indexReplayMaterialization{}, fmt.Errorf("validate index replay ReviewInput: %w", err)
	}
	variantTarget := target
	variantTarget.Contexts = cloneContextBindings(bindings)
	if err := variantTarget.Validate(); err != nil {
		return indexReplayMaterialization{}, fmt.Errorf("validate index replay target: %w", err)
	}
	inputRef, err := service.repository.PutJSONArtifact(runmodel.ContractReviewInput, variantInput)
	if err != nil {
		return indexReplayMaterialization{}, fmt.Errorf("persist index replay ReviewInput: %w", err)
	}
	targetRef, err := service.repository.PutJSONArtifact(
		runmodel.ContractMaterializedTarget,
		variantTarget,
	)
	if err != nil {
		return indexReplayMaterialization{}, fmt.Errorf("persist index replay target: %w", err)
	}
	return indexReplayMaterialization{
		targetRef: targetRef, input: variantInput, inputRef: inputRef,
		receiptRefs: receiptRefs,
	}, nil
}

func indexReplayTargetRanges(target MaterializedTarget) ([]ContextProviderTargetRange, error) {
	if target.Snapshot.Mode != reviewcore.TargetModeSelection {
		return []ContextProviderTargetRange{}, nil
	}
	if target.Snapshot.Selection == nil || len(target.Snapshot.Selection.EffectiveRanges) == 0 {
		return nil, fmt.Errorf("selection replay target is missing effective ranges")
	}
	ranges := make([]ContextProviderTargetRange, len(target.Snapshot.Selection.EffectiveRanges))
	for index, lineRange := range target.Snapshot.Selection.EffectiveRanges {
		ranges[index] = ContextProviderTargetRange{
			Path:      target.Snapshot.Selection.Path,
			StartLine: lineRange.StartLine, EndLine: lineRange.EndLine,
		}
	}
	return ranges, nil
}

func (service *Service) ValidateIndexReplayMaterialization(
	ctx context.Context,
	request IndexReplayMaterializationRequest,
	materialized IndexReplayMaterialization,
) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if service == nil || service.repository == nil {
		return fmt.Errorf("index replay materializer is not initialized")
	}
	if materialized.TargetRef.Contract != runmodel.ContractMaterializedTarget ||
		materialized.ReviewInputRef.Contract != runmodel.ContractReviewInput ||
		len(materialized.ContextProviderReceiptRefs) != len(request.VariantBundle.Execution.ContextProviders) {
		return fmt.Errorf("index replay materialization refs do not match variant config")
	}
	var sourceTarget MaterializedTarget
	if err := service.repository.ReadJSONArtifact(request.SourceSnapshot.TargetSnapshotRef, &sourceTarget); err != nil {
		return fmt.Errorf("read index replay source target: %w", err)
	}
	var variantTarget MaterializedTarget
	if err := service.repository.ReadJSONArtifact(materialized.TargetRef, &variantTarget); err != nil {
		return fmt.Errorf("read index replay variant target: %w", err)
	}
	sourceTargetWithoutContexts := sourceTarget
	variantTargetWithoutContexts := variantTarget
	sourceTargetWithoutContexts.Contexts = nil
	variantTargetWithoutContexts.Contexts = nil
	if !reflect.DeepEqual(sourceTargetWithoutContexts, variantTargetWithoutContexts) {
		return fmt.Errorf("index replay changed frozen target outside contexts")
	}
	sourceInputData, err := service.repository.ReadArtifact(request.SourceSnapshot.ReviewInputRef)
	if err != nil {
		return fmt.Errorf("read index replay source ReviewInput: %w", err)
	}
	sourceInput, err := reviewcore.DecodeReviewInput(sourceInputData)
	if err != nil {
		return fmt.Errorf("decode index replay source ReviewInput: %w", err)
	}
	variantInputData, err := service.repository.ReadArtifact(materialized.ReviewInputRef)
	if err != nil {
		return fmt.Errorf("read index replay variant ReviewInput: %w", err)
	}
	variantInput, err := reviewcore.DecodeReviewInput(variantInputData)
	if err != nil {
		return fmt.Errorf("decode index replay variant ReviewInput: %w", err)
	}
	if !reflect.DeepEqual(variantInput, materialized.ReviewInput) ||
		!reflect.DeepEqual(variantTarget.Contexts, variantInput.Contexts) {
		return fmt.Errorf("index replay target and ReviewInput contexts do not match")
	}
	sourceInputWithoutContexts := sourceInput
	variantInputWithoutContexts := variantInput
	sourceInputWithoutContexts.Contexts = nil
	variantInputWithoutContexts.Contexts = nil
	if !reflect.DeepEqual(sourceInputWithoutContexts, variantInputWithoutContexts) {
		return fmt.Errorf("index replay changed ReviewInput outside contexts")
	}
	sourceIDs, err := service.sourceProviderContextIDs(
		request.SourceSnapshot.ContextProviderReceiptRefs,
		request.SourceBundle.Execution.ContextProviders,
		sourceTarget,
	)
	if err != nil {
		return fmt.Errorf("validate index replay source receipts: %w", err)
	}
	variantIDs, err := service.sourceProviderContextIDs(
		materialized.ContextProviderReceiptRefs,
		request.VariantBundle.Execution.ContextProviders,
		variantTarget,
	)
	if err != nil {
		return fmt.Errorf("validate index replay variant receipts: %w", err)
	}
	sourcePreserved, err := removeConfiguredContextBindings(sourceInput.Contexts, sourceIDs)
	if err != nil {
		return fmt.Errorf("validate index replay source contexts: %w", err)
	}
	variantPreserved, err := removeConfiguredContextBindings(variantInput.Contexts, variantIDs)
	if err != nil {
		return fmt.Errorf("validate index replay variant contexts: %w", err)
	}
	if !reflect.DeepEqual(sourcePreserved, variantPreserved) {
		return fmt.Errorf("index replay changed caller-supplied frozen contexts")
	}
	return nil
}

func (service *Service) indexReplayTargetPaths(
	target MaterializedTarget,
) ([]string, error) {
	paths := make([]string, 0)
	switch target.Snapshot.Mode {
	case reviewcore.TargetModeDiff:
		var manifest gitadapter.ChangeManifest
		if err := service.repository.ReadJSONArtifact(target.ManifestRef, &manifest); err != nil {
			return nil, fmt.Errorf("read index replay change manifest: %w", err)
		}
		if err := targetmodel.ValidateDiffManifest(manifest, target.Snapshot); err != nil {
			return nil, fmt.Errorf("validate index replay change manifest: %w", err)
		}
		for _, file := range manifest.Files {
			if file.Included && file.Status != gitadapter.ChangeDeleted {
				paths = append(paths, file.Path)
			}
		}
	case reviewcore.TargetModeSelection:
		if target.Snapshot.Selection == nil {
			return nil, fmt.Errorf("selection target is missing exact path")
		}
		paths = append(paths, target.Snapshot.Selection.Path)
	case reviewcore.TargetModeScope:
		var manifest ScopeManifest
		if err := service.repository.ReadJSONArtifact(target.ManifestRef, &manifest); err != nil {
			return nil, fmt.Errorf("read index replay scope manifest: %w", err)
		}
		if err := manifest.ValidateAgainst(target.Snapshot); err != nil {
			return nil, fmt.Errorf("validate index replay scope manifest: %w", err)
		}
		for _, file := range manifest.Files {
			paths = append(paths, file.Path)
		}
	default:
		return nil, fmt.Errorf("unsupported index replay target mode %q", target.Snapshot.Mode)
	}
	sort.Strings(paths)
	return paths, nil
}

func (service *Service) sourceProviderContextIDs(
	refs []runmodel.ArtifactRef,
	providers []reviewconfig.ContextProviderDefinition,
	target MaterializedTarget,
) (map[string]struct{}, error) {
	if len(refs) != len(providers) {
		return nil, fmt.Errorf("source context provider receipts do not match source config")
	}
	ids := make(map[string]struct{}, len(refs))
	for index, ref := range refs {
		data, err := service.repository.ReadArtifact(ref)
		if err != nil {
			return nil, fmt.Errorf("read source context provider receipt %d: %w", index, err)
		}
		receipt, err := contractsv1alpha1.DecodeContextProviderExecutionReceipt(data)
		if err != nil {
			return nil, fmt.Errorf("decode source context provider receipt %d: %w", index, err)
		}
		provider := providers[index]
		if receipt.ProviderID != provider.ID || receipt.ProviderRevision != provider.Revision ||
			receipt.Kind != provider.Kind || receipt.Adapter.ID != provider.Adapter.ID ||
			receipt.Adapter.Revision != provider.Adapter.Revision ||
			receipt.Adapter.SHA256 != provider.Adapter.SHA256 ||
			receipt.RepositoryID != target.Snapshot.Repository.RepositoryID ||
			receipt.CommitOID != target.Snapshot.Head.CommitOID {
			return nil, fmt.Errorf("source context provider receipt %d is not exact", index)
		}
		if _, duplicate := ids[receipt.ContextID]; duplicate {
			return nil, fmt.Errorf("source context provider receipts alias context %q", receipt.ContextID)
		}
		ids[receipt.ContextID] = struct{}{}
	}
	return ids, nil
}

func removeConfiguredContextBindings(
	bindings []reviewcore.ContextBinding,
	configuredIDs map[string]struct{},
) ([]reviewcore.ContextBinding, error) {
	preserved := make([]reviewcore.ContextBinding, 0, len(bindings)-len(configuredIDs))
	seen := make(map[string]struct{}, len(configuredIDs))
	for _, binding := range bindings {
		if _, configured := configuredIDs[binding.ContextID()]; configured {
			seen[binding.ContextID()] = struct{}{}
			continue
		}
		preserved = append(preserved, binding)
	}
	if !reflect.DeepEqual(seen, configuredIDs) {
		return nil, fmt.Errorf("source provider receipt has no exact frozen context binding")
	}
	return cloneContextBindings(preserved), nil
}
