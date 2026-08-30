package reviewshard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
	"time"

	"argus.local/argus/internal/runmodel"
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`)
var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (limits Limits) Validate() error {
	if limits.MaxFilesPerShard < 1 || limits.MaxFilesPerShard > 1024 {
		return fmt.Errorf("max_files_per_shard must be between 1 and 1024")
	}
	if limits.MaxBytesPerShard < 1 || limits.MaxBytesPerShard > 1<<30 {
		return fmt.Errorf("max_bytes_per_shard must be between 1 and 1073741824")
	}
	if limits.MaxInputBytesPerShard < 1 || limits.MaxInputBytesPerShard > 1<<30 {
		return fmt.Errorf("max_input_bytes_per_shard must be between 1 and 1073741824")
	}
	if limits.MaxShards < 1 || limits.MaxShards > 4096 {
		return fmt.Errorf("max_shards must be between 1 and 4096")
	}
	return nil
}

func (manifest Manifest) Validate() error {
	if manifest.SchemaVersion != ManifestSchemaVersion {
		return fmt.Errorf("unsupported shard manifest schema %q", manifest.SchemaVersion)
	}
	if err := validateID("run_id", manifest.RunID); err != nil {
		return err
	}
	if err := validateDigest("target_digest", manifest.TargetDigest); err != nil {
		return err
	}
	if err := validateRef("review_input_ref", manifest.ReviewInputRef, runmodel.ContractReviewInput); err != nil {
		return err
	}
	if err := manifest.Limits.Validate(); err != nil {
		return err
	}
	if manifest.Shards == nil || manifest.Gaps == nil {
		return fmt.Errorf("shards and gaps must be explicit arrays")
	}
	seenPaths := make(map[string]struct{}, manifest.Coverage.TotalFiles)
	var executableFiles int
	var executableBytes int64
	previousExecutablePath := ""
	for index, shard := range manifest.Shards {
		if err := shard.validate(index, manifest.Limits); err != nil {
			return fmt.Errorf("shards[%d]: %w", index, err)
		}
		for _, file := range shard.Files {
			if previousExecutablePath != "" && file.Path <= previousExecutablePath {
				return fmt.Errorf("executable files must be globally and uniquely sorted by path")
			}
			if _, duplicate := seenPaths[file.Path]; duplicate {
				return fmt.Errorf("path %q appears more than once in shard closure", file.Path)
			}
			seenPaths[file.Path] = struct{}{}
			executableFiles++
			executableBytes += file.SizeBytes
			previousExecutablePath = file.Path
		}
	}
	var skippedBytes int64
	previousGap := ""
	for index, gap := range manifest.Gaps {
		if err := gap.validate(); err != nil {
			return fmt.Errorf("gaps[%d]: %w", index, err)
		}
		if index > 0 && gap.Path <= previousGap {
			return fmt.Errorf("gaps must be uniquely sorted by path")
		}
		if _, duplicate := seenPaths[gap.Path]; duplicate {
			return fmt.Errorf("path %q appears as both executable and gap", gap.Path)
		}
		seenPaths[gap.Path] = struct{}{}
		previousGap = gap.Path
		skippedBytes += gap.SizeBytes
	}
	if manifest.Coverage.TotalFiles != len(seenPaths) ||
		manifest.Coverage.ExecutableFiles != executableFiles ||
		manifest.Coverage.SkippedFiles != len(manifest.Gaps) ||
		manifest.Coverage.ExecutableBytes != executableBytes ||
		manifest.Coverage.TotalBytes != executableBytes+skippedBytes {
		return fmt.Errorf("coverage does not exactly match shard and gap closure")
	}
	if executableFiles > 0 && len(manifest.Shards) == 0 {
		return fmt.Errorf("executable files require at least one shard")
	}
	if executableFiles == 0 && len(manifest.Shards) != 0 {
		return fmt.Errorf("empty executable coverage cannot contain shards")
	}
	if !isUTC(manifest.CreatedAt) {
		return fmt.Errorf("created_at must be a non-zero UTC timestamp")
	}
	return nil
}

func (shard Shard) validate(index int, limits Limits) error {
	if err := validateID("shard_id", shard.ShardID); err != nil {
		return err
	}
	if shard.Ordinal != index {
		return fmt.Errorf("ordinal is %d, want %d", shard.Ordinal, index)
	}
	if err := validateRef("input_ref", shard.InputRef, runmodel.ContractReviewInput); err != nil {
		return err
	}
	if shard.InputSHA256 != shard.InputRef.SHA256 {
		return fmt.Errorf("input_sha256 does not match input_ref")
	}
	if shard.Files == nil || len(shard.Files) == 0 || len(shard.Files) > limits.MaxFilesPerShard {
		return fmt.Errorf("files must contain between 1 and %d entries", limits.MaxFilesPerShard)
	}
	var size int64
	previous := ""
	for fileIndex, file := range shard.Files {
		if err := file.validate(); err != nil {
			return fmt.Errorf("files[%d]: %w", fileIndex, err)
		}
		if fileIndex > 0 && file.Path <= previous {
			return fmt.Errorf("files must be uniquely sorted by path")
		}
		previous = file.Path
		size += file.SizeBytes
	}
	if shard.FileBytes != size || size > limits.MaxBytesPerShard {
		return fmt.Errorf("file_bytes does not match files or exceeds shard byte limit")
	}
	if shard.InputBytes != shard.InputRef.SizeBytes {
		return fmt.Errorf("input_bytes does not match exact input artifact size")
	}
	if shard.InputBytes > limits.MaxInputBytesPerShard {
		return fmt.Errorf("input_bytes exceeds exact shard artifact byte limit")
	}
	return nil
}

func (file ShardFile) validate() error {
	if err := validatePath(file.Path); err != nil {
		return err
	}
	if err := validateDigest("sha256", file.SHA256); err != nil {
		return err
	}
	if file.SizeBytes < 0 {
		return fmt.Errorf("size_bytes must not be negative")
	}
	return nil
}

func (gap CoverageGap) validate() error {
	if err := validatePath(gap.Path); err != nil {
		return err
	}
	if gap.SHA256 != "" {
		if err := validateDigest("sha256", gap.SHA256); err != nil {
			return err
		}
	}
	if gap.SizeBytes < 0 {
		return fmt.Errorf("size_bytes must not be negative")
	}
	if err := validateText("reason_code", gap.ReasonCode, 128, false); err != nil {
		return err
	}
	return nil
}

func (binding GenerationBinding) Validate() error {
	if binding.SchemaVersion != GenerationSchemaVersion {
		return fmt.Errorf("unsupported shard generation schema %q", binding.SchemaVersion)
	}
	for name, value := range map[string]string{
		"run_id": binding.RunID, "workload_id": binding.WorkloadID,
		"lease_id": binding.LeaseID, "worker_id": binding.WorkerID,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if binding.Attempt < 1 || binding.Generation < 1 || binding.FencingToken == 0 {
		return fmt.Errorf("attempt, generation, and fencing_token must be positive")
	}
	if !isUTC(binding.BoundAt) {
		return fmt.Errorf("bound_at must be a non-zero UTC timestamp")
	}
	return nil
}

func (checkpoint Checkpoint) Validate() error {
	if checkpoint.SchemaVersion != CheckpointSchemaVersion {
		return fmt.Errorf("unsupported shard checkpoint schema %q", checkpoint.SchemaVersion)
	}
	for name, value := range map[string]string{
		"run_id": checkpoint.RunID, "shard_id": checkpoint.ShardID,
		"worker_id": checkpoint.WorkerID,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if checkpoint.Attempt < 1 || checkpoint.Generation < 1 || checkpoint.FencingToken == 0 {
		return fmt.Errorf("attempt, generation, and fencing_token must be positive")
	}
	if checkpoint.ProcessedFiles < 0 || checkpoint.ProcessedBytes < 0 {
		return fmt.Errorf("processed coverage must not be negative")
	}
	if !isUTC(checkpoint.StartedAt) || !isUTC(checkpoint.CompletedAt) ||
		checkpoint.CompletedAt.Before(checkpoint.StartedAt) {
		return fmt.Errorf("checkpoint requires non-decreasing UTC execution timestamps")
	}
	switch checkpoint.Status {
	case CheckpointSucceeded:
		if checkpoint.OutputRef == nil || checkpoint.Failure != nil {
			return fmt.Errorf("succeeded checkpoint requires output_ref and no failure")
		}
		if err := validateRef("output_ref", *checkpoint.OutputRef, runmodel.ContractStageResult); err != nil {
			return err
		}
	case CheckpointFailed, CheckpointCanceled:
		if checkpoint.OutputRef != nil || checkpoint.Failure == nil {
			return fmt.Errorf("failed/canceled checkpoint requires failure and no output_ref")
		}
		if err := checkpoint.Failure.validate(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported checkpoint status %q", checkpoint.Status)
	}
	return nil
}

func (failure Failure) validate() error {
	if err := validateText("failure.code", failure.Code, 128, false); err != nil {
		return err
	}
	return validateText("failure.message", failure.Message, 4096, false)
}

func (aggregate Aggregate) Validate() error {
	if aggregate.SchemaVersion != AggregateSchemaVersion {
		return fmt.Errorf("unsupported shard aggregate schema %q", aggregate.SchemaVersion)
	}
	if err := validateID("run_id", aggregate.RunID); err != nil {
		return err
	}
	if err := validateRef("manifest_ref", aggregate.ManifestRef, runmodel.ContractReviewShardManifest); err != nil {
		return err
	}
	if aggregate.Completeness != "complete" && aggregate.Completeness != "partial" {
		return fmt.Errorf("completeness must be complete or partial")
	}
	if aggregate.Outputs == nil || aggregate.Gaps == nil {
		return fmt.Errorf("outputs and gaps must be explicit arrays")
	}
	for index, output := range aggregate.Outputs {
		if output.Ordinal != index {
			return fmt.Errorf("outputs[%d] has non-canonical ordinal", index)
		}
		if err := validateID("outputs.shard_id", output.ShardID); err != nil {
			return err
		}
		if err := validateRef("outputs.output_ref", output.OutputRef, runmodel.ContractStageResult); err != nil {
			return err
		}
		if output.Generation < 1 || output.FencingToken == 0 {
			return fmt.Errorf("output generation and fencing_token must be positive")
		}
	}
	if !isUTC(aggregate.CompletedAt) {
		return fmt.Errorf("completed_at must be a non-zero UTC timestamp")
	}
	return validateID("completed_by", aggregate.CompletedBy)
}

func (mutation Mutation) Validate() error {
	if err := validateID("idempotency_key", mutation.IdempotencyKey); err != nil {
		return err
	}
	if err := validateID("actor", mutation.Actor); err != nil {
		return err
	}
	if err := validateText("audit", mutation.Audit, 4096, false); err != nil {
		return err
	}
	if !isUTC(mutation.At) {
		return fmt.Errorf("mutation time must be a non-zero UTC timestamp")
	}
	return nil
}

func validateRef(name string, ref runmodel.ArtifactRef, contract string) error {
	if err := ref.Validate(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if ref.Contract != contract {
		return fmt.Errorf("%s contract is %q, want %q", name, ref.Contract, contract)
	}
	return nil
}

func validateID(name, value string) error {
	if !idPattern.MatchString(value) {
		return fmt.Errorf("%s is not a valid bounded identifier", name)
	}
	return nil
}

func validateDigest(name, value string) error {
	if !digestPattern.MatchString(value) {
		return fmt.Errorf("%s must be a lowercase SHA-256 digest", name)
	}
	return nil
}

func validatePath(value string) error {
	if value == "" || strings.Contains(value, "\\") || strings.ContainsRune(value, '\x00') ||
		strings.HasPrefix(value, "/") || path.Clean(value) != value || value == "." ||
		strings.HasPrefix(value, "../") {
		return fmt.Errorf("path %q is not a canonical repository-relative path", value)
	}
	return nil
}

func validateText(name, value string, maximum int, allowEmpty bool) error {
	if !allowEmpty && strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", name)
	}
	if len(value) > maximum || strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("%s exceeds its text boundary", name)
	}
	return nil
}

func isUTC(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC
}

func decodeStrict[T any](data []byte, validate func(T) error) (T, error) {
	var value T
	if err := rejectDuplicateJSONFields(data); err != nil {
		return value, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return value, fmt.Errorf("multiple JSON values are not allowed")
		}
		return value, fmt.Errorf("invalid trailing JSON: %w", err)
	}
	if err := validate(value); err != nil {
		return value, err
	}
	return value, nil
}

func rejectDuplicateJSONFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := inspectJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return fmt.Errorf("invalid JSON: %w", err)
		}
		return fmt.Errorf("unexpected trailing JSON token")
	}
	return nil
}

func inspectJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	delimiter, nested := token.(json.Delim)
	if !nested {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("JSON object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON field %q", key)
			}
			seen[key] = struct{}{}
			if err := inspectJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := inspectJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
}

func DecodeManifest(data []byte) (Manifest, error) {
	return decodeStrict(data, func(value Manifest) error { return value.Validate() })
}

func DecodeCheckpoint(data []byte) (Checkpoint, error) {
	return decodeStrict(data, func(value Checkpoint) error { return value.Validate() })
}

func DecodeAggregate(data []byte) (Aggregate, error) {
	return decodeStrict(data, func(value Aggregate) error { return value.Validate() })
}
