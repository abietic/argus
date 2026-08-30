package findinglineage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"unicode"
	"unicode/utf8"

	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

func DefaultPolicy() contractsv1alpha1.FindingLineagePolicy {
	policy, err := contractsv1alpha1.SealFindingLineagePolicy(
		contractsv1alpha1.FindingLineagePolicy{
			PolicyID:          "argus-conservative-lineage-matcher",
			Revision:          "1",
			FamilyFields:      []string{"category", "dimension", "path", "title_normalized"},
			ManyToManyAction:  "leave_unmatched",
			AncestryAuthority: "caller_order_unverified",
		},
	)
	if err != nil {
		panic(err)
	}
	return policy
}

func GitAwarePolicy() contractsv1alpha1.FindingLineagePolicy {
	policy, err := contractsv1alpha1.SealFindingLineagePolicy(
		contractsv1alpha1.FindingLineagePolicy{
			PolicyID: "argus-git-aware-conservative-lineage-matcher", Revision: "1",
			FamilyFields:     []string{"category", "dimension", "path", "title_normalized"},
			ManyToManyAction: "leave_unmatched", AncestryAuthority: "local_git_object_graph",
			RenameThresholdBPS: 5000,
		},
	)
	if err != nil {
		panic(err)
	}
	return policy
}

func DefaultIdempotencyKey(baselineRunID, variantRunID string, policy contractsv1alpha1.FindingLineagePolicy) string {
	data, _ := json.Marshal([]string{baselineRunID, variantRunID, policy.SHA256})
	digest := sha256.Sum256(data)
	return "finding-lineage-build-" + hex.EncodeToString(digest[:])[:24]
}

func (request BuildRequest) Validate() error {
	if request.SchemaVersion != BuildRequestSchemaVersion {
		return fmt.Errorf("unsupported finding lineage build request schema %q", request.SchemaVersion)
	}
	for name, value := range map[string]string{
		"idempotency_key": request.IdempotencyKey,
		"baseline_run_id": request.BaselineRunID,
		"variant_run_id":  request.VariantRunID,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if request.BaselineRunID == request.VariantRunID {
		return fmt.Errorf("baseline and variant runs must be distinct")
	}
	if err := request.Policy.Validate(); err != nil {
		return fmt.Errorf("policy: %w", err)
	}
	if !reflect.DeepEqual(request.Policy, DefaultPolicy()) && !reflect.DeepEqual(request.Policy, GitAwarePolicy()) {
		return fmt.Errorf("only built-in conservative lineage policies are supported")
	}
	return nil
}

func validateID(name, value string) error {
	if value == "" || len(value) > 256 || !utf8.ValidString(value) || value != strings.TrimSpace(value) {
		return fmt.Errorf("%s is invalid", name)
	}
	if strings.ContainsAny(value, `/\\`) {
		return fmt.Errorf("%s must not contain path separators", name)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return fmt.Errorf("%s contains control characters", name)
		}
	}
	return nil
}
