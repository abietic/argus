package application

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/reviewcore"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
)

type reviewInputPreparation struct {
	SchemaVersion              string                 `json:"schema_version"`
	RunID                      string                 `json:"run_id"`
	RequestSHA256              string                 `json:"request_sha256"`
	ConfigBundleSHA256         string                 `json:"config_bundle_sha256"`
	BuildIdentity              string                 `json:"build_identity"`
	RepositoryRoot             string                 `json:"repository_root"`
	TargetRef                  runmodel.ArtifactRef   `json:"target_ref"`
	InputRef                   runmodel.ArtifactRef   `json:"input_ref"`
	ContextProviderReceiptRefs []runmodel.ArtifactRef `json:"context_provider_receipt_refs"`
}

func (service *Service) prepareReviewInput(
	ctx context.Context,
	runID string,
	request ReviewRequest,
	bundle reviewconfig.ConfigBundle,
	config LocalConfig,
) (Materialization, []runmodel.ArtifactRef, error) {
	requestData, err := json.Marshal(request)
	if err != nil {
		return Materialization{}, nil, err
	}
	requestSum := sha256.Sum256(requestData)
	requestDigest := hex.EncodeToString(requestSum[:])
	configDigest, err := reviewconfig.DigestBundleArtifact(bundle)
	if err != nil {
		return Materialization{}, nil, err
	}
	if service.executionDispatch != nil {
		if ref, err := service.repository.ReviewPreparation(runID); err == nil {
			return service.loadPreparedReviewInput(ref, runID, requestDigest, configDigest, bundle)
		} else if !os.IsNotExist(err) {
			return Materialization{}, nil, err
		}
	}
	preparedRequest, receiptRefs, err := service.prepareConfiguredContexts(ctx, request, bundle, config)
	if err != nil {
		return Materialization{}, nil, err
	}
	materialized, err := Materialize(ctx, service.source, service.repository, preparedRequest, config)
	if err != nil {
		return Materialization{}, nil, err
	}
	if service.executionDispatch == nil {
		return materialized, receiptRefs, nil
	}
	preparation := reviewInputPreparation{
		SchemaVersion: runrepo.ReviewPreparationContract, RunID: runID,
		RequestSHA256: requestDigest, ConfigBundleSHA256: configDigest, BuildIdentity: service.buildIdentity,
		RepositoryRoot: materialized.RepositoryRoot, TargetRef: materialized.TargetRef, InputRef: materialized.InputRef,
		ContextProviderReceiptRefs: slices.Clone(receiptRefs),
	}
	if err := preparation.Validate(); err != nil {
		return Materialization{}, nil, err
	}
	ref, err := service.repository.PutJSONArtifact(runrepo.ReviewPreparationContract, preparation)
	if err != nil {
		return Materialization{}, nil, err
	}
	if err := ctx.Err(); err != nil {
		return Materialization{}, nil, err
	}
	if err := service.verifyExecutionAuthority(ctx, runID); err != nil {
		return Materialization{}, nil, err
	}
	at, err := service.timestamp()
	if err != nil {
		return Materialization{}, nil, err
	}
	if err := service.repository.BindReviewPreparation(runID, ref, at); err != nil {
		return Materialization{}, nil, err
	}
	return materialized, receiptRefs, nil
}

func (service *Service) loadPreparedReviewInput(
	ref runmodel.ArtifactRef,
	runID, requestDigest, configDigest string,
	bundle reviewconfig.ConfigBundle,
) (Materialization, []runmodel.ArtifactRef, error) {
	data, err := service.repository.ReadArtifact(ref)
	if err != nil {
		return Materialization{}, nil, err
	}
	prepared, err := decodeReviewInputPreparation(data)
	if err != nil {
		return Materialization{}, nil, err
	}
	if prepared.SchemaVersion != runrepo.ReviewPreparationContract || prepared.RunID != runID ||
		prepared.RequestSHA256 != requestDigest || prepared.ConfigBundleSHA256 != configDigest ||
		prepared.BuildIdentity != service.buildIdentity {
		return Materialization{}, nil, fmt.Errorf("prepared review differs from exact request, configuration, or build")
	}
	if len(prepared.ContextProviderReceiptRefs) != len(bundle.Execution.ContextProviders) {
		return Materialization{}, nil, fmt.Errorf("prepared review has invalid frozen input identity")
	}
	var target MaterializedTarget
	if err := service.repository.ReadJSONArtifact(prepared.TargetRef, &target); err != nil {
		return Materialization{}, nil, err
	}
	if err := target.Validate(); err != nil {
		return Materialization{}, nil, err
	}
	inputBytes, err := service.repository.ReadArtifact(prepared.InputRef)
	if err != nil {
		return Materialization{}, nil, err
	}
	input, err := reviewcore.DecodeReviewInput(inputBytes)
	if err != nil {
		return Materialization{}, nil, err
	}
	if input.TargetID != target.Snapshot.TargetSnapshotID || input.TargetMode != target.Snapshot.Mode {
		return Materialization{}, nil, fmt.Errorf("prepared review input does not bind its exact target")
	}
	for _, receipt := range prepared.ContextProviderReceiptRefs {
		if _, err := service.repository.ReadArtifact(receipt); err != nil {
			return Materialization{}, nil, err
		}
	}
	return Materialization{
		RepositoryRoot: prepared.RepositoryRoot, Target: target, TargetRef: prepared.TargetRef,
		Input: input, InputRef: prepared.InputRef,
	}, slices.Clone(prepared.ContextProviderReceiptRefs), nil
}

func (prepared reviewInputPreparation) Validate() error {
	if prepared.SchemaVersion != runrepo.ReviewPreparationContract {
		return fmt.Errorf("unsupported review preparation schema")
	}
	if _, err := runmodel.AgentStagePlanAdmissionID(prepared.RunID, "preparation"); err != nil {
		return err
	}
	for name, digest := range map[string]string{"request_sha256": prepared.RequestSHA256, "config_bundle_sha256": prepared.ConfigBundleSHA256} {
		data, err := hex.DecodeString(digest)
		if err != nil || len(data) != sha256.Size || strings.ToLower(digest) != digest {
			return fmt.Errorf("%s must be a lowercase SHA-256", name)
		}
	}
	if prepared.BuildIdentity == "" || strings.TrimSpace(prepared.BuildIdentity) != prepared.BuildIdentity || !utf8.ValidString(prepared.BuildIdentity) {
		return fmt.Errorf("build_identity must be a nonempty trimmed UTF-8 string")
	}
	if !filepath.IsAbs(prepared.RepositoryRoot) || filepath.Clean(prepared.RepositoryRoot) != prepared.RepositoryRoot {
		return fmt.Errorf("repository_root must be a clean absolute path")
	}
	if err := prepared.TargetRef.Validate(); err != nil {
		return err
	}
	if err := prepared.InputRef.Validate(); err != nil {
		return err
	}
	if prepared.TargetRef.Contract != runmodel.ContractMaterializedTarget || prepared.InputRef.Contract != runmodel.ContractReviewInput {
		return fmt.Errorf("prepared target or input contract is unsupported")
	}
	if prepared.ContextProviderReceiptRefs == nil {
		return fmt.Errorf("context_provider_receipt_refs must be an explicit array")
	}
	seen := make(map[runmodel.ArtifactRef]bool)
	for _, receipt := range prepared.ContextProviderReceiptRefs {
		if err := receipt.Validate(); err != nil {
			return err
		}
		if receipt.Contract != runmodel.ContractContextProviderExecutionReceipt || seen[receipt] {
			return fmt.Errorf("prepared context receipt contract is unsupported or duplicated")
		}
		seen[receipt] = true
	}
	return nil
}

func decodeReviewInputPreparation(data []byte) (reviewInputPreparation, error) {
	var prepared reviewInputPreparation
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&prepared); err != nil {
		return reviewInputPreparation{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return reviewInputPreparation{}, fmt.Errorf("review preparation must contain one JSON value")
	}
	if err := prepared.Validate(); err != nil {
		return reviewInputPreparation{}, err
	}
	return prepared, nil
}
