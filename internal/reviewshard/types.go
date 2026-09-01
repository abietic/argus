// Package reviewshard owns Argus full-scan shard identity, immutable inputs,
// checkpoints, and fan-in facts. It deliberately does not implement worker
// leasing: scheduling/Hailix remains the execution authority and supplies the
// exact generation/fencing binding recorded here.
package reviewshard

import (
	"errors"
	"time"

	"github.com/abietic/argus/internal/runmodel"
)

const (
	ManifestSchemaVersion   = "argus.review_shard_manifest.v1alpha1"
	CheckpointSchemaVersion = "argus.review_shard_checkpoint.v1alpha1"
	AggregateSchemaVersion  = "argus.review_shard_aggregate.v1alpha1"
	GenerationSchemaVersion = "argus.review_shard_generation.v1alpha1"
	eventSchemaVersion      = "argus.review_shard_event.v1alpha1"
	eventStream             = "review/shards"
)

var (
	ErrNotFound          = errors.New("review shard plan not found")
	ErrConflict          = errors.New("review shard mutation conflict")
	ErrFenced            = errors.New("review shard generation fenced")
	ErrCanceled          = errors.New("review shard plan canceled")
	ErrIncomplete        = errors.New("review shard plan incomplete")
	ErrInvalidTransition = errors.New("invalid review shard transition")
)

type Limits struct {
	MaxFilesPerShard      int   `json:"max_files_per_shard"`
	MaxBytesPerShard      int64 `json:"max_bytes_per_shard"`
	MaxInputBytesPerShard int64 `json:"max_input_bytes_per_shard"`
	MaxShards             int   `json:"max_shards"`
}

type ShardFile struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

type CoverageGap struct {
	Path       string `json:"path"`
	SHA256     string `json:"sha256,omitempty"`
	SizeBytes  int64  `json:"size_bytes"`
	ReasonCode string `json:"reason_code"`
}

type Coverage struct {
	TotalFiles      int   `json:"total_files"`
	ExecutableFiles int   `json:"executable_files"`
	SkippedFiles    int   `json:"skipped_files"`
	TotalBytes      int64 `json:"total_bytes"`
	ExecutableBytes int64 `json:"executable_bytes"`
}

type Shard struct {
	ShardID     string               `json:"shard_id"`
	Ordinal     int                  `json:"ordinal"`
	InputRef    runmodel.ArtifactRef `json:"input_ref"`
	InputSHA256 string               `json:"input_sha256"`
	Files       []ShardFile          `json:"files"`
	FileBytes   int64                `json:"file_bytes"`
	InputBytes  int64                `json:"input_bytes"`
}

type Manifest struct {
	SchemaVersion  string               `json:"schema_version"`
	RunID          string               `json:"run_id"`
	TargetDigest   string               `json:"target_digest"`
	ReviewInputRef runmodel.ArtifactRef `json:"review_input_ref"`
	Limits         Limits               `json:"limits"`
	Shards         []Shard              `json:"shards"`
	Gaps           []CoverageGap        `json:"gaps"`
	Coverage       Coverage             `json:"coverage"`
	CreatedAt      time.Time            `json:"created_at"`
}

// GenerationBinding is a copied, provider-neutral scheduling authority fact.
// The adapter must derive it from an authenticated lease; it is never treated
// as a credential or as Hailix attestation by itself.
type GenerationBinding struct {
	SchemaVersion string    `json:"schema_version"`
	RunID         string    `json:"run_id"`
	WorkloadID    string    `json:"workload_id"`
	LeaseID       string    `json:"lease_id"`
	WorkerID      string    `json:"worker_id"`
	Attempt       int       `json:"attempt"`
	Generation    int       `json:"generation"`
	FencingToken  uint64    `json:"fencing_token"`
	BoundAt       time.Time `json:"bound_at"`
}

type CheckpointStatus string

const (
	CheckpointSucceeded CheckpointStatus = "succeeded"
	CheckpointFailed    CheckpointStatus = "failed"
	CheckpointCanceled  CheckpointStatus = "canceled"
)

type Failure struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type Checkpoint struct {
	SchemaVersion  string                `json:"schema_version"`
	RunID          string                `json:"run_id"`
	ShardID        string                `json:"shard_id"`
	Attempt        int                   `json:"attempt"`
	Generation     int                   `json:"generation"`
	FencingToken   uint64                `json:"fencing_token"`
	WorkerID       string                `json:"worker_id"`
	Status         CheckpointStatus      `json:"status"`
	OutputRef      *runmodel.ArtifactRef `json:"output_ref,omitempty"`
	ProcessedFiles int                   `json:"processed_files"`
	ProcessedBytes int64                 `json:"processed_bytes"`
	Failure        *Failure              `json:"failure,omitempty"`
	StartedAt      time.Time             `json:"started_at"`
	CompletedAt    time.Time             `json:"completed_at"`
}

type ShardOutput struct {
	ShardID      string               `json:"shard_id"`
	Ordinal      int                  `json:"ordinal"`
	OutputRef    runmodel.ArtifactRef `json:"output_ref"`
	Generation   int                  `json:"generation"`
	FencingToken uint64               `json:"fencing_token"`
}

type Aggregate struct {
	SchemaVersion string               `json:"schema_version"`
	RunID         string               `json:"run_id"`
	ManifestRef   runmodel.ArtifactRef `json:"manifest_ref"`
	Completeness  string               `json:"completeness"`
	Outputs       []ShardOutput        `json:"outputs"`
	Coverage      Coverage             `json:"coverage"`
	Gaps          []CoverageGap        `json:"gaps"`
	CompletedAt   time.Time            `json:"completed_at"`
	CompletedBy   string               `json:"completed_by"`
}

type Mutation struct {
	IdempotencyKey string    `json:"idempotency_key"`
	Actor          string    `json:"actor"`
	Audit          string    `json:"audit"`
	At             time.Time `json:"at"`
}

type Record struct {
	Manifest          Manifest              `json:"manifest"`
	ManifestRef       runmodel.ArtifactRef  `json:"manifest_ref"`
	ActiveGeneration  *GenerationBinding    `json:"active_generation,omitempty"`
	CheckpointHistory []Checkpoint          `json:"checkpoint_history"`
	Completed         []Checkpoint          `json:"completed"`
	PendingShardIDs   []string              `json:"pending_shard_ids"`
	Aggregate         *Aggregate            `json:"aggregate,omitempty"`
	AggregateRef      *runmodel.ArtifactRef `json:"aggregate_ref,omitempty"`
	Canceled          bool                  `json:"canceled"`
	CancelReason      string                `json:"cancel_reason,omitempty"`
	UpdatedAt         time.Time             `json:"updated_at"`
}

type eventType string

const (
	eventPlanRecorded       eventType = "plan_recorded"
	eventGenerationBound    eventType = "generation_bound"
	eventCheckpointRecorded eventType = "checkpoint_recorded"
	eventPlanCanceled       eventType = "plan_canceled"
	eventAggregateRecorded  eventType = "aggregate_recorded"
)

type shardEvent struct {
	SchemaVersion string                `json:"schema_version"`
	Type          eventType             `json:"type"`
	RunID         string                `json:"run_id"`
	ManifestRef   *runmodel.ArtifactRef `json:"manifest_ref,omitempty"`
	Generation    *GenerationBinding    `json:"generation,omitempty"`
	Checkpoint    *Checkpoint           `json:"checkpoint,omitempty"`
	AggregateRef  *runmodel.ArtifactRef `json:"aggregate_ref,omitempty"`
	CancelReason  string                `json:"cancel_reason,omitempty"`
	Actor         string                `json:"actor"`
	Audit         string                `json:"audit"`
	OccurredAt    time.Time             `json:"occurred_at"`
}
