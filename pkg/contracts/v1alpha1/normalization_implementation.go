package v1alpha1

import "fmt"

const (
	CandidateNormalizationImplementationID = "candidate-normalization"

	CandidateNormalizationRevisionV0 = "v0"
	CandidateNormalizationRevisionV1 = "v1"
	CandidateNormalizationRevisionV2 = "v2"

	CandidateNormalizationCurrentRevision = CandidateNormalizationRevisionV2
)

// ValidateCandidateNormalizationImplementation validates the closed selector
// understood by both the Pi worker and the host-side recomputation. The digest
// is intentionally the worker implementation digest: changing normalization
// code therefore requires publishing and selecting new immutable worker bytes.
func ValidateCandidateNormalizationImplementation(ref VersionedRef, name string) error {
	if err := ref.validate(name); err != nil {
		return err
	}
	if ref.ID != CandidateNormalizationImplementationID {
		return fmt.Errorf("%s.id must be %q", name, CandidateNormalizationImplementationID)
	}
	switch ref.Revision {
	case CandidateNormalizationRevisionV0,
		CandidateNormalizationRevisionV1,
		CandidateNormalizationRevisionV2:
		return nil
	default:
		return fmt.Errorf("%s.revision has unsupported value %q", name, ref.Revision)
	}
}

func CandidateNormalizationWorkflowRevision(revision string) (string, error) {
	switch revision {
	case CandidateNormalizationRevisionV0,
		CandidateNormalizationRevisionV1,
		CandidateNormalizationRevisionV2:
		return "argus-pi-review-workflow-" + revision, nil
	default:
		return "", fmt.Errorf("unsupported candidate normalization revision %q", revision)
	}
}
