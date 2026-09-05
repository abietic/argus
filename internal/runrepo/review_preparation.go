package runrepo

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/store/local"
)

const ReviewPreparationContract = "argus.review_preparation.v1alpha1"

const reviewPreparationBindingContract = "argus.review_preparation_binding.v1alpha1"

type reviewPreparationBinding struct {
	SchemaVersion string               `json:"schema_version"`
	RunID         string               `json:"run_id"`
	Preparation   runmodel.ArtifactRef `json:"preparation_ref"`
}

func (binding reviewPreparationBinding) Validate() error {
	if binding.SchemaVersion != reviewPreparationBindingContract || binding.Preparation.Contract != ReviewPreparationContract {
		return fmt.Errorf("unsupported review preparation binding schema or artifact contract")
	}
	if _, err := reviewPreparationStream(binding.RunID); err != nil {
		return err
	}
	return binding.Preparation.Validate()
}

func decodeReviewPreparationBinding(data []byte) (reviewPreparationBinding, error) {
	var binding reviewPreparationBinding
	if err := decodeStrictJSON(data, &binding); err != nil {
		return reviewPreparationBinding{}, err
	}
	if err := binding.Validate(); err != nil {
		return reviewPreparationBinding{}, err
	}
	return binding, nil
}

// BindReviewPreparation publishes the exact pre-lifecycle deterministic input.
// It precedes scope shard planning, whose manifest must reuse that input after
// a crash. The append-only binding is separate from run.created authority.
func (repository *Repository) BindReviewPreparation(runID string, ref runmodel.ArtifactRef, at time.Time) error {
	stream, err := reviewPreparationStream(runID)
	if err != nil {
		return err
	}
	if at.IsZero() || at.Location() != time.UTC {
		return fmt.Errorf("preparation binding requires a non-zero UTC time")
	}
	if err := ref.Validate(); err != nil {
		return err
	}
	if ref.Contract != ReviewPreparationContract {
		return fmt.Errorf("unsupported review preparation contract")
	}
	if _, err := repository.ReadArtifact(ref); err != nil {
		return err
	}
	if existing, err := repository.ReviewPreparation(runID); err == nil {
		if existing != ref {
			return fmt.Errorf("%w: review preparation already binds different input", local.ErrEventConflict)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	_, _, err = repository.store.AppendJSONLAtSequenceWithStatus(stream, 0, local.Event{
		ID: runID + "-prepared", Schema: reviewPreparationBindingContract, Time: at,
		Payload: reviewPreparationBinding{
			SchemaVersion: reviewPreparationBindingContract, RunID: runID, Preparation: ref,
		},
	})
	return err
}

func (repository *Repository) ReviewPreparation(runID string) (runmodel.ArtifactRef, error) {
	stream, err := reviewPreparationStream(runID)
	if err != nil {
		return runmodel.ArtifactRef{}, err
	}
	envelopes, err := repository.store.ReadJSONL(stream)
	if err != nil {
		return runmodel.ArtifactRef{}, err
	}
	if len(envelopes) == 0 {
		return runmodel.ArtifactRef{}, os.ErrNotExist
	}
	if len(envelopes) != 1 {
		return runmodel.ArtifactRef{}, fmt.Errorf("ambiguous review preparation binding")
	}
	envelope := envelopes[0]
	binding, err := decodeReviewPreparationBinding(envelope.Payload)
	if err != nil {
		return runmodel.ArtifactRef{}, err
	}
	if envelope.Schema != reviewPreparationBindingContract || envelope.ID != runID+"-prepared" ||
		binding.SchemaVersion != reviewPreparationBindingContract || binding.RunID != runID ||
		binding.Preparation.Contract != ReviewPreparationContract {
		return runmodel.ArtifactRef{}, fmt.Errorf("invalid review preparation binding")
	}
	if err := binding.Preparation.Validate(); err != nil {
		return runmodel.ArtifactRef{}, err
	}
	if _, err := repository.ReadArtifact(binding.Preparation); err != nil {
		return runmodel.ArtifactRef{}, err
	}
	return binding.Preparation, nil
}

func reviewPreparationStream(runID string) (string, error) {
	if _, err := runmodel.AgentStagePlanAdmissionID(runID, "preparation"); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(runID))
	return "review/preparations/" + hex.EncodeToString(sum[:]), nil
}
