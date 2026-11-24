// Package reportreview reviews frozen delivery reports with versioned,
// deterministic criteria and retains the exact result for retry.
package reportreview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/abietic/argus/internal/store/local"
	contract "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

const reportName = "report.md"

type frozenInput struct {
	RequestSHA256 string                       `json:"request_sha256"`
	Request       contract.ReportReviewRequest `json:"request"`
	Files         map[string]local.ArtifactRef `json:"files"`
}

// Run returns the exact committed JSON bytes, including their final LF. Inputs
// are copied into the artifact store before evaluation. On a terminal retry the
// staging directory may already be gone.
func Run(ctx context.Context, store *local.Store, request contract.ReportReviewRequest, inputDir string) ([]byte, error) {
	requestDigest, err := contract.DigestReportReviewRequest(request)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	runID := "report-review-" + digest([]byte(request.IdempotencyKey))
	want := contract.ReportReviewResult{
		SchemaVersion: contract.ReportReviewResultSchema, RequestSHA256: requestDigest,
		ReviewRunID: runID, TaskID: request.TaskID, TaskRevision: request.TaskRevision,
		WorkerRunID: request.WorkerRunID, ArtifactID: request.ArtifactID,
		ArtifactSHA256: request.ArtifactSHA256, Profile: request.Profile,
	}
	var frozen frozenInput
	if err := store.GetJSON(runID, &frozen); errors.Is(err, os.ErrNotExist) {
		frozen, err = freeze(ctx, store, request, requestDigest, inputDir)
		if err != nil {
			return nil, err
		}
		if err := store.PutJSON(runID, frozen); err != nil && !errors.Is(err, local.ErrImmutableExists) {
			return nil, err
		}
		if err := store.GetJSON(runID, &frozen); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if frozen.RequestSHA256 != requestDigest {
		return nil, fmt.Errorf("idempotency key already binds a different report review request")
	}
	if storedDigest, err := contract.DigestReportReviewRequest(frozen.Request); err != nil || storedDigest != requestDigest {
		return nil, fmt.Errorf("stored request does not match the frozen review identity")
	}
	inputs, err := loadInputs(store, request, frozen)
	if err != nil {
		return nil, err
	}
	resultID := runID + "-result"
	var resultRef local.ArtifactRef
	if err := store.GetJSON(resultID, &resultRef); err == nil {
		return loadResult(store, resultRef, want)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	result := want
	result.Findings, err = auditLetsQuant(inputs)
	if err != nil {
		return nil, fmt.Errorf("evaluate frozen report: %w", err)
	}
	result.Verdict = contract.ReportReviewPassed
	if len(result.Findings) > 0 {
		result.Verdict = contract.ReportReviewNeedsRework
	}
	if err := result.Validate(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	expectedRef, err := store.PutArtifact(append(encoded, '\n'))
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := store.PutJSON(resultID, expectedRef); err != nil && !errors.Is(err, local.ErrImmutableExists) {
		return nil, err
	}
	if err := store.GetJSON(resultID, &resultRef); err != nil {
		return nil, err
	}
	if resultRef != expectedRef {
		return nil, fmt.Errorf("concurrent report review produced a different result")
	}
	return loadResult(store, resultRef, want)
}

func freeze(ctx context.Context, store *local.Store, request contract.ReportReviewRequest, requestDigest, inputDir string) (frozenInput, error) {
	root, err := os.OpenRoot(inputDir)
	if err != nil {
		return frozenInput{}, err
	}
	defer root.Close()
	frozen := frozenInput{RequestSHA256: requestDigest, Request: request, Files: make(map[string]local.ArtifactRef)}
	for name, expected := range inputDigests(request) {
		if err := ctx.Err(); err != nil {
			return frozenInput{}, err
		}
		data, err := readFrozenFile(root, name)
		if err != nil {
			return frozenInput{}, fmt.Errorf("read frozen input %s: %w", name, err)
		}
		if digest(data) != expected {
			return frozenInput{}, fmt.Errorf("frozen input %s SHA-256 mismatch", name)
		}
		ref, err := store.PutArtifact(data)
		if err != nil {
			return frozenInput{}, err
		}
		frozen.Files[name] = ref
	}
	return frozen, nil
}

func inputDigests(request contract.ReportReviewRequest) map[string]string {
	digests := make(map[string]string, len(request.EvidenceSHA256)+1)
	for name, digest := range request.EvidenceSHA256 {
		digests[name] = digest
	}
	digests[reportName] = request.ArtifactSHA256
	return digests
}

func loadInputs(store *local.Store, request contract.ReportReviewRequest, frozen frozenInput) (map[string][]byte, error) {
	digests := inputDigests(request)
	if len(frozen.Files) != len(digests) {
		return nil, fmt.Errorf("incomplete frozen input record")
	}
	inputs := make(map[string][]byte, len(digests))
	for name, expected := range digests {
		ref := frozen.Files[name]
		if ref.SHA256 != expected || ref.SizeBytes < 1 || ref.SizeBytes > maxFrozenFile {
			return nil, fmt.Errorf("invalid frozen artifact binding for %s", name)
		}
		data, err := store.ReadArtifact(ref)
		if err != nil {
			return nil, err
		}
		inputs[name] = data
	}
	return inputs, nil
}

func loadResult(store *local.Store, ref local.ArtifactRef, want contract.ReportReviewResult) ([]byte, error) {
	if ref.SizeBytes < 1 || ref.SizeBytes > 64<<10 {
		return nil, fmt.Errorf("invalid stored report result size")
	}
	data, err := store.ReadArtifact(ref)
	if err != nil {
		return nil, err
	}
	result, err := contract.DecodeReportReviewResult(data)
	if err != nil {
		return nil, err
	}
	if result.RequestSHA256 != want.RequestSHA256 || result.ReviewRunID != want.ReviewRunID ||
		result.TaskID != want.TaskID || result.TaskRevision != want.TaskRevision || result.WorkerRunID != want.WorkerRunID ||
		result.ArtifactID != want.ArtifactID || result.ArtifactSHA256 != want.ArtifactSHA256 || result.Profile != want.Profile {
		return nil, fmt.Errorf("stored report result does not match the frozen delivery")
	}
	return data, nil
}

const maxFrozenFile = 1 << 20

func readFrozenFile(root *os.Root, name string) ([]byte, error) {
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > maxFrozenFile {
		return nil, fmt.Errorf("expected a nonempty regular file of at most %d bytes", maxFrozenFile)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxFrozenFile+1))
	if err != nil {
		return nil, err
	}
	if len(data) < 1 || len(data) > maxFrozenFile {
		return nil, fmt.Errorf("frozen file size is outside the accepted limit")
	}
	return data, nil
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
