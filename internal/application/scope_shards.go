package application

import (
	"context"
	"time"

	"github.com/abietic/argus/internal/reviewcore"
	"github.com/abietic/argus/internal/runmodel"
)

// ScopeShardPort is the Argus-owned full-scan boundary. Implementations own
// immutable shard inputs/checkpoints/fan-in while their generation authority
// is supplied by the outer scheduler dispatch.
type ScopeShardPort interface {
	PlanScope(context.Context, ScopeShardPlanRequest) (ScopeShardPlan, error)
	ScopeCoverage(context.Context, string, runmodel.ArtifactRef) (ScopeShardCoverage, error)
	DetectScope(context.Context, ScopeShardDetectRequest) (reviewcore.StageResult, error)
	CancelScope(context.Context, string, string, time.Time) error
}

type ScopeShardPlan struct {
	ManifestRef runmodel.ArtifactRef
	Coverage    ScopeShardCoverage
}

type ScopeShardCoverage struct {
	Complete    bool
	ReasonCodes []string
}

type ScopeShardPlanRequest struct {
	RunID         string
	Input         reviewcore.ReviewInput
	InputRef      runmodel.ArtifactRef
	MaxInputBytes int64
	CreatedAt     time.Time
}

type ScopeShardDetectRequest struct {
	RunID          string
	Input          reviewcore.ReviewInput
	InputRef       runmodel.ArtifactRef
	ManifestRef    runmodel.ArtifactRef
	Upstream       reviewcore.StageResult
	Policy         reviewcore.RuntimePolicy
	MaxInputBytes  int64
	MaxOutputBytes int64
	MaxConcurrency int
}
