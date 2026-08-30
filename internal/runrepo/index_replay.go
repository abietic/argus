package runrepo

import (
	"fmt"
	"reflect"

	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/reviewcore"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/targetmodel"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

func (repository *Repository) verifyIndexReplayChange(
	sourceSnapshot runmodel.ExecutionSnapshot,
	variantSnapshot runmodel.ExecutionSnapshot,
	sourceConfig reviewconfig.ConfigBundle,
	variantConfig reviewconfig.ConfigBundle,
	change runmodel.ReplayChangeSet,
) error {
	if change.StartStage != string(reviewcore.StageMaterializeTarget) ||
		len(variantSnapshot.ReplayInputRefs) != 0 {
		return fmt.Errorf(
			"index replay must restart at materialize_target without reused checkpoints",
		)
	}
	fields, err := reviewconfig.DiffReplayIndexPolicy(
		sourceConfig.Execution,
		variantConfig.Execution,
	)
	if err != nil {
		return fmt.Errorf("validate index replay policy: %w", err)
	}
	if !reflect.DeepEqual(fields, change.ChangedFields) {
		return fmt.Errorf("index replay changed_fields do not match exact ConfigBundles")
	}

	var sourceTarget targetmodel.MaterializedTarget
	if err := repository.ReadJSONArtifact(sourceSnapshot.TargetSnapshotRef, &sourceTarget); err != nil {
		return fmt.Errorf("read index replay source target: %w", err)
	}
	var variantTarget targetmodel.MaterializedTarget
	if err := repository.ReadJSONArtifact(variantSnapshot.TargetSnapshotRef, &variantTarget); err != nil {
		return fmt.Errorf("read index replay variant target: %w", err)
	}
	sourceTargetWithoutContexts := sourceTarget
	variantTargetWithoutContexts := variantTarget
	sourceTargetWithoutContexts.Contexts = nil
	variantTargetWithoutContexts.Contexts = nil
	if !reflect.DeepEqual(sourceTargetWithoutContexts, variantTargetWithoutContexts) {
		return fmt.Errorf("index replay changed frozen target bytes outside contexts")
	}

	sourceInputBytes, err := repository.ReadArtifact(sourceSnapshot.ReviewInputRef)
	if err != nil {
		return fmt.Errorf("read index replay source ReviewInput: %w", err)
	}
	sourceInput, err := reviewcore.DecodeReviewInput(sourceInputBytes)
	if err != nil {
		return fmt.Errorf("decode index replay source ReviewInput: %w", err)
	}
	variantInputBytes, err := repository.ReadArtifact(variantSnapshot.ReviewInputRef)
	if err != nil {
		return fmt.Errorf("read index replay variant ReviewInput: %w", err)
	}
	variantInput, err := reviewcore.DecodeReviewInput(variantInputBytes)
	if err != nil {
		return fmt.Errorf("decode index replay variant ReviewInput: %w", err)
	}
	sourceInputWithoutContexts := sourceInput
	variantInputWithoutContexts := variantInput
	sourceInputWithoutContexts.Contexts = nil
	variantInputWithoutContexts.Contexts = nil
	if !reflect.DeepEqual(sourceInputWithoutContexts, variantInputWithoutContexts) {
		return fmt.Errorf("index replay changed ReviewInput outside contexts")
	}

	sourcePreserved, err := repository.unconfiguredReplayContexts(
		sourceSnapshot.ContextProviderReceiptRefs,
		sourceConfig.Execution.ContextProviders,
		sourceInput.Contexts,
	)
	if err != nil {
		return fmt.Errorf("validate index replay source contexts: %w", err)
	}
	variantPreserved, err := repository.unconfiguredReplayContexts(
		variantSnapshot.ContextProviderReceiptRefs,
		variantConfig.Execution.ContextProviders,
		variantInput.Contexts,
	)
	if err != nil {
		return fmt.Errorf("validate index replay variant contexts: %w", err)
	}
	if !reflect.DeepEqual(sourcePreserved, variantPreserved) {
		return fmt.Errorf("index replay changed caller-supplied frozen contexts")
	}
	return nil
}

func (repository *Repository) unconfiguredReplayContexts(
	refs []runmodel.ArtifactRef,
	providers []reviewconfig.ContextProviderDefinition,
	contexts []reviewcore.ContextBinding,
) ([]reviewcore.ContextBinding, error) {
	if len(refs) != len(providers) {
		return nil, fmt.Errorf("context provider receipt count does not match config")
	}
	configured := make(map[string]struct{}, len(refs))
	for index, ref := range refs {
		data, err := repository.ReadArtifact(ref)
		if err != nil {
			return nil, fmt.Errorf("read receipt %d: %w", index, err)
		}
		receipt, err := contractsv1alpha1.DecodeContextProviderExecutionReceipt(data)
		if err != nil {
			return nil, fmt.Errorf("decode receipt %d: %w", index, err)
		}
		provider := providers[index]
		if receipt.ProviderID != provider.ID || receipt.ProviderRevision != provider.Revision ||
			receipt.Kind != provider.Kind || receipt.Adapter.ID != provider.Adapter.ID ||
			receipt.Adapter.Revision != provider.Adapter.Revision ||
			receipt.Adapter.SHA256 != provider.Adapter.SHA256 {
			return nil, fmt.Errorf("receipt %d does not match exact provider definition", index)
		}
		if _, duplicate := configured[receipt.ContextID]; duplicate {
			return nil, fmt.Errorf("receipts alias context %q", receipt.ContextID)
		}
		configured[receipt.ContextID] = struct{}{}
	}
	preserved := make([]reviewcore.ContextBinding, 0, len(contexts)-len(configured))
	seen := make(map[string]struct{}, len(configured))
	for _, binding := range contexts {
		if _, exists := configured[binding.ContextID()]; exists {
			seen[binding.ContextID()] = struct{}{}
			continue
		}
		preserved = append(preserved, binding)
	}
	if !reflect.DeepEqual(seen, configured) {
		return nil, fmt.Errorf("receipt has no matching frozen context")
	}
	return preserved, nil
}
