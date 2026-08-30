// Package reviewjob implements the bounded local asynchronous adapter for
// Argus review commands. Durable dispatch remains owned by scheduling; this
// package only freezes review intent and maps it to one scheduling workload.
package reviewjob

import (
	"context"
	"errors"
	"time"

	"github.com/abietic/argus/internal/application"
	"github.com/abietic/argus/internal/formalreview"
	"github.com/abietic/argus/internal/piexecution"
	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/scheduling"
	"github.com/abietic/argus/internal/store/local"
)

const (
	RequestSchemaVersion          = "argus.review_job_request.v1alpha1"
	CommandSchemaVersion          = "argus.review_job_command.v1alpha1"
	EventSchemaVersion            = "argus.review_job_event.v1alpha1"
	RecordSchemaVersion           = "argus.review_job.v1alpha1"
	DeterministicExecutionProfile = "deterministic_review_v1"
	FormalPiExecutionProfile      = "formal_pi_review_v1"

	jobStream = "review/jobs"
)

var (
	ErrNotFound = errors.New("review job not found")
	ErrConflict = errors.New("review job mutation conflict")
	ErrCorrupt  = errors.New("corrupt review job ledger")
)

type Request struct {
	SchemaVersion    string `json:"schema_version"`
	ExecutionProfile string `json:"execution_profile"`
	RepositoryPath   string `json:"repository_path"`
	Mode             string `json:"mode"`
	SourceRunID      string `json:"source_run_id,omitempty"`

	BaseRevision string `json:"base_revision,omitempty"`
	HeadRevision string `json:"head_revision,omitempty"`

	Revision        string                       `json:"revision,omitempty"`
	SelectionPath   string                       `json:"selection_path,omitempty"`
	StartLine       uint32                       `json:"start_line,omitempty"`
	EndLine         uint32                       `json:"end_line,omitempty"`
	SelectionRanges []application.SelectionRange `json:"selection_ranges"`
	SelectionSymbol *application.SymbolSelector  `json:"selection_symbol,omitempty"`

	Include []string `json:"include"`
	Exclude []string `json:"exclude"`

	ExecutionTimeoutSeconds uint32 `json:"execution_timeout_seconds"`
}

// FormalRuntimeBinding freezes every non-secret local Pi input used to admit a
// formal job. Credential values remain environment-only. At execution the
// configured files are rebuilt and exact-compared with Bootstrap before the
// provider subprocess can start.
type FormalRuntimeBinding struct {
	Options   formalreview.LocalPiBootstrapOptions `json:"options"`
	Bootstrap formalreview.LocalPiBootstrap        `json:"bootstrap"`
	Pricing   piexecution.PricingCeiling           `json:"pricing"`
}

type Mutation struct {
	IdempotencyKey string    `json:"idempotency_key"`
	Actor          string    `json:"actor"`
	Audit          string    `json:"audit"`
	At             time.Time `json:"at"`
}

type CancelCommand struct {
	Reason   string   `json:"reason"`
	Mutation Mutation `json:"mutation"`
}

type Command struct {
	SchemaVersion    string                               `json:"schema_version"`
	JobID            string                               `json:"job_id"`
	RunID            string                               `json:"run_id"`
	ExecutionProfile string                               `json:"execution_profile"`
	Request          Request                              `json:"request"`
	ConfigBundle     reviewconfig.ConfigBundle            `json:"config_bundle"`
	ConfigReceipt    reviewconfig.ConfigResolutionReceipt `json:"config_resolution_receipt"`
	FormalRuntime    *FormalRuntimeBinding                `json:"formal_runtime,omitempty"`
	Actor            string                               `json:"actor"`
	IdempotencyKey   string                               `json:"idempotency_key"`
	Audit            string                               `json:"audit"`
	SubmittedAt      time.Time                            `json:"submitted_at"`
}

type Submission struct {
	JobID            string               `json:"job_id"`
	RunID            string               `json:"run_id"`
	WorkloadID       string               `json:"workload_id"`
	CommandRef       runmodel.ArtifactRef `json:"command_ref"`
	ConfigBundleRef  runmodel.ArtifactRef `json:"config_bundle_ref"`
	ConfigReceiptRef runmodel.ArtifactRef `json:"config_resolution_receipt_ref"`
	Request          Request              `json:"request"`
	Actor            string               `json:"actor"`
	Audit            string               `json:"audit"`
	SubmittedAt      time.Time            `json:"submitted_at"`
	IdempotencyKey   string               `json:"idempotency_key"`
	Sequence         uint64               `json:"sequence"`
}

type Record struct {
	SchemaVersion    string                    `json:"schema_version"`
	JobID            string                    `json:"job_id"`
	RunID            string                    `json:"run_id"`
	ExecutionProfile string                    `json:"execution_profile"`
	Request          Request                   `json:"request"`
	CommandRef       runmodel.ArtifactRef      `json:"command_ref"`
	ConfigBundleRef  runmodel.ArtifactRef      `json:"config_bundle_ref"`
	ConfigReceiptRef runmodel.ArtifactRef      `json:"config_resolution_receipt_ref"`
	Workload         scheduling.WorkloadRecord `json:"workload"`
	Run              *runmodel.ReviewRun       `json:"run,omitempty"`
	SubmittedBy      string                    `json:"submitted_by"`
	Audit            string                    `json:"audit"`
	SubmittedAt      time.Time                 `json:"submitted_at"`
}

type ConfigResolver interface {
	ResolvePublishedWithReceipt(
		context.Context,
		reviewconfig.ResolutionContext,
	) (reviewconfig.ConfigBundle, reviewconfig.ConfigResolutionReceipt, error)
}

type Scheduler interface {
	Submit(context.Context, scheduling.WorkloadSpec, scheduling.Mutation) (scheduling.WorkloadRecord, error)
	Claim(context.Context, scheduling.ClaimRequest) (scheduling.Dispatch, error)
	Heartbeat(context.Context, scheduling.Heartbeat) (scheduling.WorkloadRecord, error)
	Complete(context.Context, scheduling.Callback) (scheduling.WorkloadRecord, error)
	CancelRun(context.Context, string, string, scheduling.Mutation) ([]scheduling.WorkloadRecord, error)
	Reconcile(context.Context, scheduling.Mutation) ([]scheduling.WorkloadRecord, error)
	Get(string) (scheduling.WorkloadRecord, error)
	List() ([]scheduling.WorkloadRecord, error)
	Timeline(string) ([]scheduling.WorkloadTimelineEvent, error)
}

type Executor interface {
	Execute(context.Context, Command) (application.RunOutcome, error)
}

// ClaimedExecutor consumes the exact scheduling lease already owned by the
// ReviewJob coordinator. Formal Pi implements this interface so it does not
// create and claim a nested workload with the same concurrency class.
type ClaimedExecutor interface {
	ExecuteClaimed(context.Context, Command, scheduling.Dispatch) (application.RunOutcome, error)
	CanResumeNonterminal(Command) bool
}

type FormalProfile struct {
	Options   formalreview.LocalPiBootstrapOptions
	Pricing   piexecution.PricingCeiling
	Admission FormalAdmissionValidator
}

type FormalAdmissionValidator interface {
	ValidateFormalAdmission(
		context.Context,
		application.AgentPlanningSubject,
		formalreview.LocalPiBootstrap,
	) error
}

type ExecuteFunc func(context.Context, Command) (application.RunOutcome, error)

func (function ExecuteFunc) Execute(
	ctx context.Context,
	command Command,
) (application.RunOutcome, error) {
	return function(ctx, command)
}

type Store interface {
	PutArtifact([]byte) (local.ArtifactRef, error)
	ReadArtifact(local.ArtifactRef) ([]byte, error)
	AppendJSONLAtSequenceWithStatus(string, uint64, local.Event) (local.Envelope, bool, error)
	ReadJSONL(string) ([]local.Envelope, error)
}
