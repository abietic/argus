// Package findinglineage owns durable, cross-revision Finding relationship
// facts. It consumes only committed formal ReviewRun closures and never
// mutates either source run.
package findinglineage

import (
	"errors"
	"time"

	"argus.local/argus/internal/runmodel"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

const (
	BuildRequestSchemaVersion = "argus.finding_lineage_build_request.v1alpha1"
	LedgerEventSchemaVersion  = "argus.finding_lineage_ledger_event.v1alpha1"
)

var (
	ErrNotFound = errors.New("finding lineage not found")
	ErrConflict = errors.New("finding lineage conflict")
	ErrCorrupt  = errors.New("corrupt finding lineage ledger")
)

type BuildRequest struct {
	SchemaVersion  string                                 `json:"schema_version"`
	IdempotencyKey string                                 `json:"idempotency_key"`
	BaselineRunID  string                                 `json:"baseline_run_id"`
	VariantRunID   string                                 `json:"variant_run_id"`
	Policy         contractsv1alpha1.FindingLineagePolicy `json:"policy"`
}

type Record struct {
	Request    BuildRequest                     `json:"request"`
	LineageRef runmodel.ArtifactRef             `json:"lineage_ref"`
	Lineage    contractsv1alpha1.FindingLineage `json:"lineage"`
	RecordedAt time.Time                        `json:"recorded_at"`
}

type ListFilter struct {
	RunID        string
	RepositoryID string
}
