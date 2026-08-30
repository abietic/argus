package reviewshard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"time"

	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/runmodel"
)

type ArtifactWriter interface {
	PutArtifact(string, []byte) (runmodel.ArtifactRef, error)
}

// Plan freezes deterministic, non-overlapping scope inputs. Every source file
// appears exactly once as either executable shard coverage or an explicit gap.
// Context artifacts are copied as evidence-only bindings and never expand a
// shard's review regions.
func Plan(
	ctx context.Context,
	runID string,
	input reviewcore.ReviewInput,
	inputRef runmodel.ArtifactRef,
	limits Limits,
	createdAt time.Time,
	artifacts ArtifactWriter,
) (Manifest, error) {
	if ctx == nil || artifacts == nil {
		return Manifest{}, fmt.Errorf("context and artifact writer are required")
	}
	if err := validateID("run_id", runID); err != nil {
		return Manifest{}, err
	}
	if err := limits.Validate(); err != nil {
		return Manifest{}, err
	}
	if !isUTC(createdAt) {
		return Manifest{}, fmt.Errorf("created_at must be a non-zero UTC timestamp")
	}
	if input.TargetMode != reviewcore.TargetModeScope {
		return Manifest{}, fmt.Errorf("review sharding requires a scope ReviewInput")
	}
	if err := input.Validate(); err != nil {
		return Manifest{}, fmt.Errorf("validate scope ReviewInput: %w", err)
	}
	inputDigest, err := reviewcore.DigestReviewInput(input)
	if err != nil {
		return Manifest{}, err
	}
	if err := validateRef("review_input_ref", inputRef, runmodel.ContractReviewInput); err != nil {
		return Manifest{}, err
	}
	if inputRef.SHA256 != inputDigest {
		return Manifest{}, fmt.Errorf("review_input_ref does not bind exact ReviewInput bytes")
	}

	manifest := Manifest{
		SchemaVersion: ManifestSchemaVersion,
		RunID:         runID, TargetDigest: inputDigest, ReviewInputRef: inputRef,
		Limits: limits, Shards: []Shard{}, Gaps: []CoverageGap{},
		CreatedAt: createdAt,
	}
	current := make([]reviewcore.FileManifestEntry, 0, limits.MaxFilesPerShard)
	var currentBytes int64
	var freezeFiles func([]reviewcore.FileManifestEntry) error
	freezeFiles = func(files []reviewcore.FileManifestEntry) error {
		if len(files) == 0 {
			return nil
		}
		shardInput := reviewcore.ReviewInput{
			SchemaVersion: input.SchemaVersion,
			TargetID:      input.TargetID, TargetMode: input.TargetMode,
			CanonicalPatch: "",
			Regions:        regionsForFiles(input.Regions, files),
			Files:          slices.Clone(files), Contexts: slices.Clone(input.Contexts),
		}
		if err := shardInput.Validate(); err != nil {
			return fmt.Errorf("validate shard %d input: %w", len(manifest.Shards), err)
		}
		data, err := json.Marshal(shardInput)
		if err != nil {
			return fmt.Errorf("marshal shard input: %w", err)
		}
		ref, err := artifacts.PutArtifact(runmodel.ContractReviewInput, data)
		if err != nil {
			return fmt.Errorf("persist shard input: %w", err)
		}
		digest, err := reviewcore.DigestReviewInput(shardInput)
		if err != nil {
			return err
		}
		if ref.SHA256 != digest || ref.SizeBytes != int64(len(data)) {
			return fmt.Errorf("persisted shard input identity differs from exact bytes")
		}
		if ref.SizeBytes > limits.MaxInputBytesPerShard {
			if len(files) == 1 {
				manifest.Gaps = append(manifest.Gaps, gapFor(files[0], "shard_input_bytes_exceeded"))
				return nil
			}
			middle := len(files) / 2
			if err := freezeFiles(files[:middle]); err != nil {
				return err
			}
			return freezeFiles(files[middle:])
		}
		if len(manifest.Shards) >= limits.MaxShards {
			for _, file := range files {
				manifest.Gaps = append(manifest.Gaps, gapFor(file, "max_shards_exceeded"))
			}
			return nil
		}
		var fileBytes int64
		for _, file := range files {
			fileBytes += file.SizeBytes
		}
		ordinal := len(manifest.Shards)
		manifest.Shards = append(manifest.Shards, Shard{
			ShardID: shardID(runID, ordinal, digest), Ordinal: ordinal,
			InputRef: ref, InputSHA256: digest,
			Files: shardFiles(files), FileBytes: fileBytes,
			InputBytes: ref.SizeBytes,
		})
		return nil
	}
	flush := func() error {
		if err := freezeFiles(current); err != nil {
			return err
		}
		current = current[:0]
		currentBytes = 0
		return nil
	}

	for _, file := range input.Files {
		if err := ctx.Err(); err != nil {
			return Manifest{}, err
		}
		manifest.Coverage.TotalFiles++
		manifest.Coverage.TotalBytes += file.SizeBytes
		switch {
		case file.Content == nil:
			manifest.Gaps = append(manifest.Gaps, gapFor(file, "content_unavailable"))
		case file.SizeBytes > limits.MaxBytesPerShard:
			manifest.Gaps = append(manifest.Gaps, gapFor(file, "shard_file_bytes_exceeded"))
		default:
			wouldOverflow := len(current) == limits.MaxFilesPerShard ||
				len(current) > 0 && currentBytes+file.SizeBytes > limits.MaxBytesPerShard
			if wouldOverflow {
				if err := flush(); err != nil {
					return Manifest{}, err
				}
			}
			current = append(current, file)
			currentBytes += file.SizeBytes
		}
	}
	if err := flush(); err != nil {
		return Manifest{}, err
	}
	sort.Slice(manifest.Gaps, func(left, right int) bool {
		return manifest.Gaps[left].Path < manifest.Gaps[right].Path
	})
	for _, shard := range manifest.Shards {
		manifest.Coverage.ExecutableFiles += len(shard.Files)
		manifest.Coverage.ExecutableBytes += shard.FileBytes
	}
	manifest.Coverage.SkippedFiles = len(manifest.Gaps)
	if err := manifest.Validate(); err != nil {
		return Manifest{}, fmt.Errorf("validate planned shard manifest: %w", err)
	}
	return manifest, nil
}

func regionsForFiles(
	regions []reviewcore.ReviewRegion,
	files []reviewcore.FileManifestEntry,
) []reviewcore.ReviewRegion {
	paths := make(map[string]struct{}, len(files))
	for _, file := range files {
		paths[file.Path] = struct{}{}
	}
	result := make([]reviewcore.ReviewRegion, 0, len(regions))
	for _, region := range regions {
		if _, exists := paths[region.Path]; exists {
			result = append(result, region)
		}
	}
	return result
}

func shardFiles(files []reviewcore.FileManifestEntry) []ShardFile {
	result := make([]ShardFile, 0, len(files))
	for _, file := range files {
		result = append(result, ShardFile{
			Path: file.Path, SHA256: file.SHA256, SizeBytes: file.SizeBytes,
		})
	}
	return result
}

func gapFor(file reviewcore.FileManifestEntry, reason string) CoverageGap {
	return CoverageGap{
		Path: file.Path, SHA256: file.SHA256, SizeBytes: file.SizeBytes,
		ReasonCode: reason,
	}
}

func shardID(runID string, ordinal int, digest string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s", runID, ordinal, digest)))
	return fmt.Sprintf("shard-%04d-%s", ordinal, hex.EncodeToString(sum[:6]))
}
