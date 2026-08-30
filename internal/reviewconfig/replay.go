package reviewconfig

import (
	"fmt"
	"reflect"
	"slices"
)

// DiffReplayIndexPolicy admits the ContextProvider set as one atomic index
// variable while freezing every other execution-policy field. Provider order
// is part of the frozen execution contract because receipts are positional.
func DiffReplayIndexPolicy(
	baseline ExecutionPolicy,
	variant ExecutionPolicy,
) ([]string, error) {
	left := baseline
	right := variant
	left.ContextProviders = nil
	right.ContextProviders = nil
	if !reflect.DeepEqual(left, right) {
		return nil, fmt.Errorf(
			"index replay may change context_providers only; agent, model, credential, tool, and concurrency policy must remain frozen",
		)
	}
	if reflect.DeepEqual(baseline.ContextProviders, variant.ContextProviders) {
		return nil, fmt.Errorf("index replay does not change context provider policy")
	}

	fields := make([]string, 0)
	baselineByID := make(map[string]ContextProviderDefinition, len(baseline.ContextProviders))
	variantByID := make(map[string]ContextProviderDefinition, len(variant.ContextProviders))
	baselineOrder := make([]string, len(baseline.ContextProviders))
	variantOrder := make([]string, len(variant.ContextProviders))
	for index, provider := range baseline.ContextProviders {
		baselineByID[provider.ID] = provider
		baselineOrder[index] = provider.ID
	}
	for index, provider := range variant.ContextProviders {
		variantByID[provider.ID] = provider
		variantOrder[index] = provider.ID
	}
	if !slices.Equal(baselineOrder, variantOrder) {
		fields = append(fields, "execution.context_providers.order")
	}
	for _, provider := range baseline.ContextProviders {
		other, exists := variantByID[provider.ID]
		prefix := "execution.context_providers." + provider.ID
		if !exists {
			fields = append(fields, prefix+".removed")
			continue
		}
		if provider.Revision != other.Revision {
			fields = append(fields, prefix+".revision")
		}
		if provider.Kind != other.Kind {
			fields = append(fields, prefix+".kind")
		}
		if provider.Adapter.ID != other.Adapter.ID {
			fields = append(fields, prefix+".adapter.id")
		}
		if provider.Adapter.Revision != other.Adapter.Revision {
			fields = append(fields, prefix+".adapter.revision")
		}
		if provider.Adapter.SHA256 != other.Adapter.SHA256 {
			fields = append(fields, prefix+".adapter.sha256")
		}
	}
	for _, provider := range variant.ContextProviders {
		if _, exists := baselineByID[provider.ID]; !exists {
			fields = append(fields, "execution.context_providers."+provider.ID+".added")
		}
	}
	slices.Sort(fields)
	if len(fields) == 0 {
		return nil, fmt.Errorf("index replay changes only unsupported identity metadata")
	}
	return fields, nil
}
