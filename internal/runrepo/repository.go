package runrepo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/source/gitadapter"
	"argus.local/argus/internal/store/local"
	"argus.local/argus/internal/targetmodel"
	"argus.local/argus/internal/workflow"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

const (
	RunEventSchemaVersion = "argus.run_event.v1alpha1"

	EventRunCreated       = "run.created"
	EventRunStarted       = "run.started"
	EventStageStarted     = "stage.started"
	EventStageSucceeded   = "stage.succeeded"
	EventStageFailed      = "stage.failed"
	EventStageCanceled    = "stage.canceled"
	EventBindingRecorded  = "binding.recorded"
	EventEvidenceRecorded = "evidence.recorded"
	EventRunSucceeded     = "run.succeeded"
	EventRunFailed        = "run.failed"
	EventRunCanceled      = "run.canceled"
)

type Repository struct {
	store *local.Store
	mu    sync.Mutex
	now   func() time.Time
}

// RunEvent is the append-only run ledger fact. Binding and Evidence are copied
// into the ledger so a crash before terminal projection does not erase the
// execution admission or its result provenance.
type RunEvent struct {
	RunID               string                             `json:"run_id"`
	Kind                runmodel.RunKind                   `json:"kind"`
	Status              runmodel.RunStatus                 `json:"status"`
	EventType           string                             `json:"event_type"`
	ExecutionSnapshotID string                             `json:"execution_snapshot_id,omitempty"`
	StageID             string                             `json:"stage_id,omitempty"`
	Attempt             int                                `json:"attempt,omitempty"`
	Generation          int                                `json:"generation,omitempty"`
	Artifact            *runmodel.ArtifactRef              `json:"artifact_ref,omitempty"`
	Binding             *runmodel.PlatformExecutionBinding `json:"platform_execution_binding,omitempty"`
	Evidence            *runmodel.RunEvidence              `json:"run_evidence,omitempty"`
	FinalRunRef         *runmodel.ArtifactRef              `json:"final_run_ref,omitempty"`
	Failure             *runmodel.Failure                  `json:"failure,omitempty"`
}

type HistoryEntry struct {
	RunID         string             `json:"run_id"`
	Kind          runmodel.RunKind   `json:"kind"`
	Status        runmodel.RunStatus `json:"status"`
	LastSequence  uint64             `json:"last_sequence"`
	LastEventTime time.Time          `json:"last_event_time"`
	LastEventType string             `json:"last_event_type"`
}

// HistorySnapshot is a point-in-time projection of the append-only run index.
// Watermark is the highest run-index sequence included in Entries. Supplying
// that watermark to HistoryAt reconstructs the same projection even after new
// lifecycle events are appended.
type HistorySnapshot struct {
	Watermark uint64         `json:"watermark"`
	Entries   []HistoryEntry `json:"entries"`
}

// CommittedRunResult is the verified operator-facing result family selected by
// a committed ReviewRun. It intentionally excludes Markdown and raw agent task
// evidence; callers needing those artifacts must request their exact governed
// references separately.
type CommittedRunResult struct {
	Run                runmodel.ReviewRun
	Report             *reviewcore.Report
	CandidateSet       *contractsv1alpha1.GovernedCandidateSet
	VerificationLedger *contractsv1alpha1.CandidateVerificationLedger
	CalibrationLedger  *contractsv1alpha1.FindingCalibrationLedger
	SuppressionLedger  *contractsv1alpha1.FindingSuppressionLedger
	GovernedReport     *contractsv1alpha1.GovernedReviewReport
}

type findingSetProjection struct {
	SchemaVersion string                       `json:"schema_version"`
	TargetDigest  string                       `json:"target_digest"`
	Findings      []reviewcore.Finding         `json:"findings"`
	Decisions     []reviewcore.FindingDecision `json:"decisions"`
}

type shardManifestProjection struct {
	SchemaVersion  string               `json:"schema_version"`
	RunID          string               `json:"run_id"`
	TargetDigest   string               `json:"target_digest"`
	ReviewInputRef runmodel.ArtifactRef `json:"review_input_ref"`
	Limits         json.RawMessage      `json:"limits"`
	Shards         []json.RawMessage    `json:"shards"`
	Gaps           []shardGapProjection `json:"gaps"`
	Coverage       json.RawMessage      `json:"coverage"`
	CreatedAt      time.Time            `json:"created_at"`
}

type shardGapProjection struct {
	Path       string `json:"path"`
	SHA256     string `json:"sha256,omitempty"`
	SizeBytes  int64  `json:"size_bytes"`
	ReasonCode string `json:"reason_code"`
}

func verifyShardManifestProjection(
	data []byte,
	runID string,
	inputRef runmodel.ArtifactRef,
	targetDigest string,
) ([]string, error) {
	var manifest shardManifestProjection
	if err := decodeStrictJSON(data, &manifest); err != nil {
		return nil, fmt.Errorf("decode review shard manifest: %w", err)
	}
	if manifest.SchemaVersion != runmodel.ContractReviewShardManifest ||
		manifest.RunID != runID || manifest.TargetDigest != targetDigest ||
		manifest.ReviewInputRef != inputRef || manifest.Shards == nil || manifest.Gaps == nil ||
		len(manifest.Limits) == 0 || len(manifest.Coverage) == 0 || manifest.CreatedAt.IsZero() {
		return nil, fmt.Errorf("review shard manifest does not bind exact run and ReviewInput")
	}
	notes := make([]string, 0, len(manifest.Gaps))
	previousPath := ""
	for index, gap := range manifest.Gaps {
		if gap.Path == "" || gap.ReasonCode == "" || gap.SizeBytes < 0 ||
			index > 0 && gap.Path <= previousPath {
			return nil, fmt.Errorf("review shard manifest gaps are not canonical")
		}
		notes = append(notes, "scope shard gap: "+gap.Path+":"+gap.ReasonCode)
		previousPath = gap.Path
	}
	return notes, nil
}

func New(store *local.Store) (*Repository, error) {
	if store == nil {
		return nil, fmt.Errorf("local store is required")
	}
	return &Repository{store: store, now: time.Now}, nil
}

func (repository *Repository) PutArtifact(
	contract string,
	content []byte,
) (runmodel.ArtifactRef, error) {
	if strings.TrimSpace(contract) == "" {
		return runmodel.ArtifactRef{}, fmt.Errorf("artifact contract is required")
	}
	ref, err := repository.store.PutArtifact(content)
	if err != nil {
		return runmodel.ArtifactRef{}, err
	}
	return runmodel.ArtifactRef{
		URI:       ref.URI,
		SHA256:    ref.SHA256,
		SizeBytes: ref.SizeBytes,
		Contract:  contract,
	}, nil
}

func (repository *Repository) PutJSONArtifact(
	contract string,
	value any,
) (runmodel.ArtifactRef, error) {
	content, err := json.Marshal(value)
	if err != nil {
		return runmodel.ArtifactRef{}, fmt.Errorf("marshal %s artifact: %w", contract, err)
	}
	return repository.PutArtifact(contract, content)
}

func (repository *Repository) ReadArtifact(ref runmodel.ArtifactRef) ([]byte, error) {
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	if err := repository.CheckArtifactEligibility(ref, ArtifactUseRead); err != nil {
		return nil, err
	}
	content, err := repository.store.ReadArtifact(local.ArtifactRef{
		URI:       ref.URI,
		SHA256:    ref.SHA256,
		SizeBytes: ref.SizeBytes,
	})
	if err == nil {
		return content, nil
	}
	if quarantineErr := repository.autoQuarantineArtifact(ref); quarantineErr != nil {
		return nil, fmt.Errorf(
			"read artifact: %v; persist integrity quarantine: %w",
			err,
			quarantineErr,
		)
	}
	return nil, fmt.Errorf("%w: content verification failed: %w", ErrArtifactQuarantined, err)
}

func (repository *Repository) ReadJSONArtifact(ref runmodel.ArtifactRef, out any) error {
	if out == nil {
		return fmt.Errorf("artifact output is required")
	}
	content, err := repository.ReadArtifact(ref)
	if err != nil {
		return err
	}
	if err := decodeStrictJSON(content, out); err != nil {
		return fmt.Errorf("decode %s artifact: %w", ref.Contract, err)
	}
	return nil
}

func (repository *Repository) SaveExecutionSnapshot(snapshot runmodel.ExecutionSnapshot) error {
	if err := snapshot.Validate(); err != nil {
		return err
	}
	return repository.store.PutJSON(
		"runs/"+snapshot.ExecutionSnapshotID+"/execution-snapshot",
		snapshot,
	)
}

func (repository *Repository) LoadExecutionSnapshot(
	executionSnapshotID string,
) (runmodel.ExecutionSnapshot, error) {
	var snapshot runmodel.ExecutionSnapshot
	if err := repository.store.GetJSON(
		"runs/"+executionSnapshotID+"/execution-snapshot",
		&snapshot,
	); err != nil {
		return runmodel.ExecutionSnapshot{}, err
	}
	if err := snapshot.Validate(); err != nil {
		return runmodel.ExecutionSnapshot{}, fmt.Errorf("validate execution snapshot: %w", err)
	}
	return snapshot, nil
}

// TerminalEventID is the only accepted event ID for a run's commit record.
// Making it deterministic lets the local store enforce uniqueness across
// retries and across Repository instances.
func TerminalEventID(runID string) string {
	return runID + "-terminal"
}

// FinalizeRun verifies the complete persisted closure, writes the final run as
// a content-addressed artifact, and commits it by appending the unique terminal
// event. An orphan final artifact is harmless if the append fails.
func (repository *Repository) FinalizeRun(
	eventID string,
	eventTime time.Time,
	run runmodel.ReviewRun,
) (runmodel.ArtifactRef, error) {
	repository.mu.Lock()
	defer repository.mu.Unlock()

	if !isTerminalStatus(run.Status) {
		return runmodel.ArtifactRef{}, fmt.Errorf("only terminal runs can be finalized")
	}
	if eventID != TerminalEventID(run.RunID) {
		return runmodel.ArtifactRef{}, fmt.Errorf(
			"terminal event id is %q, want %q", eventID, TerminalEventID(run.RunID),
		)
	}
	if eventTime.IsZero() {
		return runmodel.ArtifactRef{}, fmt.Errorf("terminal event time is required")
	}
	if err := run.Validate(); err != nil {
		return runmodel.ArtifactRef{}, err
	}
	if err := repository.verifyRunClosure(run); err != nil {
		return runmodel.ArtifactRef{}, fmt.Errorf("verify final run closure: %w", err)
	}
	finalRef, err := repository.PutJSONArtifact(runmodel.ContractReviewRun, run)
	if err != nil {
		return runmodel.ArtifactRef{}, fmt.Errorf("persist final run artifact: %w", err)
	}
	event := RunEvent{
		RunID:               run.RunID,
		Kind:                run.Kind,
		Status:              run.Status,
		EventType:           terminalEventType(run.Status),
		ExecutionSnapshotID: run.ExecutionSnapshotID,
		FinalRunRef:         &finalRef,
		Failure:             run.Failure,
	}
	if err := repository.appendEventLocked(eventID, eventTime, event); err != nil {
		return runmodel.ArtifactRef{}, err
	}
	return finalRef, nil
}

func (repository *Repository) LoadRun(runID string) (runmodel.ReviewRun, error) {
	events, err := repository.readRunEvents(runID)
	if err != nil {
		return runmodel.ReviewRun{}, err
	}
	terminal, ok, err := firstTerminalEvent(events)
	if err != nil {
		return runmodel.ReviewRun{}, err
	}
	if !ok {
		return runmodel.ReviewRun{}, fmt.Errorf("run %q has no committed terminal event: %w", runID, os.ErrNotExist)
	}
	var run runmodel.ReviewRun
	if err := repository.ReadJSONArtifact(*terminal.Event.FinalRunRef, &run); err != nil {
		return runmodel.ReviewRun{}, fmt.Errorf("read committed final run: %w", err)
	}
	if err := run.Validate(); err != nil {
		return runmodel.ReviewRun{}, fmt.Errorf("validate committed final run: %w", err)
	}
	if run.RunID != runID || run.Kind != terminal.Event.Kind ||
		run.Status != terminal.Event.Status ||
		run.ExecutionSnapshotID != terminal.Event.ExecutionSnapshotID {
		return runmodel.ReviewRun{}, fmt.Errorf("committed final run does not match terminal event")
	}
	if err := repository.verifyRunClosure(run); err != nil {
		return runmodel.ReviewRun{}, fmt.Errorf("verify committed final run closure: %w", err)
	}
	return run, nil
}

// LoadCommittedRunResult verifies the terminal ReviewRun closure and decodes
// the one applicable deterministic or formal result family.
func (repository *Repository) LoadCommittedRunResult(runID string) (CommittedRunResult, error) {
	run, err := repository.LoadRun(runID)
	if err != nil {
		return CommittedRunResult{}, err
	}
	result := CommittedRunResult{Run: run}
	if run.JSONReportRef != nil {
		data, err := repository.ReadArtifact(*run.JSONReportRef)
		if err != nil {
			return CommittedRunResult{}, fmt.Errorf("read deterministic review report: %w", err)
		}
		report, err := reviewcore.DecodeReport(data)
		if err != nil {
			return CommittedRunResult{}, fmt.Errorf("decode deterministic review report: %w", err)
		}
		result.Report = &report
	}
	if run.CandidateSetRef != nil {
		data, err := repository.ReadArtifact(*run.CandidateSetRef)
		if err != nil {
			return CommittedRunResult{}, fmt.Errorf("read governed candidate set: %w", err)
		}
		candidateSet, err := contractsv1alpha1.DecodeGovernedCandidateSet(data)
		if err != nil {
			return CommittedRunResult{}, fmt.Errorf("decode governed candidate set: %w", err)
		}
		result.CandidateSet = &candidateSet
	}
	if run.VerificationLedgerRef != nil {
		data, err := repository.ReadArtifact(*run.VerificationLedgerRef)
		if err != nil {
			return CommittedRunResult{}, fmt.Errorf("read candidate verification ledger: %w", err)
		}
		ledger, err := contractsv1alpha1.DecodeCandidateVerificationLedger(data)
		if err != nil {
			return CommittedRunResult{}, fmt.Errorf("decode candidate verification ledger: %w", err)
		}
		result.VerificationLedger = &ledger
	}
	if run.CalibrationLedgerRef != nil {
		data, err := repository.ReadArtifact(*run.CalibrationLedgerRef)
		if err != nil {
			return CommittedRunResult{}, fmt.Errorf("read finding calibration ledger: %w", err)
		}
		ledger, err := contractsv1alpha1.DecodeFindingCalibrationLedger(data)
		if err != nil {
			return CommittedRunResult{}, fmt.Errorf("decode finding calibration ledger: %w", err)
		}
		result.CalibrationLedger = &ledger
	}
	if run.SuppressionLedgerRef != nil {
		data, err := repository.ReadArtifact(*run.SuppressionLedgerRef)
		if err != nil {
			return CommittedRunResult{}, fmt.Errorf("read finding suppression ledger: %w", err)
		}
		ledger, err := contractsv1alpha1.DecodeFindingSuppressionLedger(data)
		if err != nil {
			return CommittedRunResult{}, fmt.Errorf("decode finding suppression ledger: %w", err)
		}
		result.SuppressionLedger = &ledger
	}
	if run.GovernedReportRef != nil {
		data, err := repository.ReadArtifact(*run.GovernedReportRef)
		if err != nil {
			return CommittedRunResult{}, fmt.Errorf("read governed review report: %w", err)
		}
		report, err := contractsv1alpha1.DecodeGovernedReviewReport(data)
		if err != nil {
			return CommittedRunResult{}, fmt.Errorf("decode governed review report: %w", err)
		}
		result.GovernedReport = &report
	}
	return result, nil
}

// CommittedRunRef returns the immutable final-run artifact selected by the
// first authoritative terminal event. Replay snapshots freeze this reference
// so lineage does not depend on a mutable source_run_id lookup alone.
func (repository *Repository) CommittedRunRef(runID string) (runmodel.ArtifactRef, error) {
	events, err := repository.readRunEvents(runID)
	if err != nil {
		return runmodel.ArtifactRef{}, err
	}
	terminal, ok, err := firstTerminalEvent(events)
	if err != nil {
		return runmodel.ArtifactRef{}, err
	}
	if !ok {
		return runmodel.ArtifactRef{}, fmt.Errorf(
			"run %q has no committed terminal event: %w",
			runID,
			os.ErrNotExist,
		)
	}
	if terminal.Event.FinalRunRef == nil {
		return runmodel.ArtifactRef{}, fmt.Errorf("run %q terminal event has no final artifact", runID)
	}
	ref := *terminal.Event.FinalRunRef
	if err := ref.Validate(); err != nil {
		return runmodel.ArtifactRef{}, fmt.Errorf("run %q final artifact: %w", runID, err)
	}
	if ref.Contract != runmodel.ContractReviewRun {
		return runmodel.ArtifactRef{}, fmt.Errorf(
			"run %q final artifact contract is %q",
			runID,
			ref.Contract,
		)
	}
	return ref, nil
}

func (repository *Repository) AppendEvent(
	eventID string,
	eventTime time.Time,
	event RunEvent,
) error {
	if isTerminalStatus(event.Status) {
		return fmt.Errorf("terminal run events must be committed through FinalizeRun")
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	return repository.appendEventLocked(eventID, eventTime, event)
}

func (repository *Repository) appendEventLocked(
	eventID string,
	eventTime time.Time,
	event RunEvent,
) error {
	if eventTime.IsZero() {
		return fmt.Errorf("run event time is required")
	}
	if !strings.HasPrefix(eventID, event.RunID+"-") {
		return fmt.Errorf("event id %q is not namespaced by run %q", eventID, event.RunID)
	}
	if isTerminalStatus(event.Status) && eventID != TerminalEventID(event.RunID) {
		return fmt.Errorf("terminal event id is %q, want %q", eventID, TerminalEventID(event.RunID))
	}
	if err := event.Validate(); err != nil {
		return err
	}
	existing, err := repository.readRunEvents(event.RunID)
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		for _, persisted := range existing {
			if persisted.Event.Kind != event.Kind {
				return fmt.Errorf("run %q event kind changed from %q to %q",
					event.RunID, persisted.Event.Kind, event.Kind)
			}
		}
		terminal, ok, terminalErr := firstTerminalEvent(existing)
		if terminalErr != nil {
			return terminalErr
		}
		if ok && terminal.Envelope.ID != eventID {
			return fmt.Errorf("run %q is already terminal", event.RunID)
		}
		if ok && terminal.Envelope.ID == eventID && !isTerminalStatus(event.Status) {
			return fmt.Errorf("terminal event id cannot be reused by a non-terminal event")
		}
		if !ok && event.Status == runmodel.RunStatusPending {
			for _, persisted := range existing {
				if persisted.Event.Status == runmodel.RunStatusRunning {
					return fmt.Errorf("run %q cannot regress from running to pending", event.RunID)
				}
			}
		}
	}
	storeEvent := local.Event{
		ID:      eventID,
		Schema:  RunEventSchemaVersion,
		Time:    eventTime,
		Payload: event,
	}
	// The reverse-index fact is written first. If the process stops before the
	// authoritative run event append, the orphan fact is ignored by ImpactAt;
	// retrying the same run is idempotent, while a changed snapshot conflicts
	// before it can enter run-index.
	if (event.EventType == EventRunCreated || isTerminalStatus(event.Status)) &&
		event.ExecutionSnapshotID != "" {
		if fact, indexedAt, complete := repository.opportunisticImpactIndexFact(
			event.RunID, event.ExecutionSnapshotID,
		); complete {
			if err := repository.appendImpactIndexFact(fact, indexedAt); err != nil {
				return err
			}
		}
	}
	if _, err := repository.store.AppendJSONL("run-index", storeEvent); err != nil {
		return fmt.Errorf("append authoritative run event: %w", err)
	}
	return nil
}

func (repository *Repository) Events(runID string) ([]local.Envelope, error) {
	events, err := repository.readRunEvents(runID)
	if err != nil {
		return nil, err
	}
	envelopes := make([]local.Envelope, 0, len(events))
	for _, event := range events {
		envelopes = append(envelopes, event.Envelope)
	}
	return envelopes, nil
}

func (repository *Repository) History(limit int) ([]HistoryEntry, error) {
	snapshot, err := repository.HistoryAt(0)
	if err != nil {
		return nil, err
	}
	history := snapshot.Entries
	if limit > 0 && len(history) > limit {
		history = history[:limit]
	}
	return history, nil
}

// HistoryAt projects the run index at an exact sequence watermark. A zero
// watermark selects the latest complete stream snapshot. A non-zero watermark
// must still exist in the append-only stream; callers can therefore carry it
// across pagination without allowing later events to reorder prior pages.
func (repository *Repository) HistoryAt(watermark uint64) (HistorySnapshot, error) {
	envelopes, err := repository.store.ReadJSONL("run-index")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if watermark != 0 {
				return HistorySnapshot{}, fmt.Errorf(
					"run history watermark %d is unavailable: %w", watermark, os.ErrNotExist,
				)
			}
			return HistorySnapshot{Entries: []HistoryEntry{}}, nil
		}
		return HistorySnapshot{}, err
	}
	if len(envelopes) == 0 {
		if watermark != 0 {
			return HistorySnapshot{}, fmt.Errorf(
				"run history watermark %d is unavailable: %w", watermark, os.ErrNotExist,
			)
		}
		return HistorySnapshot{Entries: []HistoryEntry{}}, nil
	}
	latestWatermark := envelopes[len(envelopes)-1].Sequence
	if watermark == 0 {
		watermark = latestWatermark
	} else if watermark > latestWatermark {
		return HistorySnapshot{}, fmt.Errorf(
			"run history watermark %d exceeds latest sequence %d: %w",
			watermark, latestWatermark, os.ErrNotExist,
		)
	}
	latest := make(map[string]HistoryEntry)
	terminal := make(map[string]bool)
	kinds := make(map[string]runmodel.RunKind)
	for _, envelope := range envelopes {
		if envelope.Sequence > watermark {
			break
		}
		event, err := decodeRunEvent(envelope)
		if err != nil {
			return HistorySnapshot{}, err
		}
		if prior, ok := kinds[event.RunID]; ok && prior != event.Kind {
			return HistorySnapshot{}, fmt.Errorf("run %q event kind changed from %q to %q",
				event.RunID, prior, event.Kind)
		}
		kinds[event.RunID] = event.Kind
		if terminal[event.RunID] {
			// Preserve the first terminal commit as authority even if a stream
			// produced by an older writer contains a later status event.
			continue
		}
		latest[event.RunID] = HistoryEntry{
			RunID: event.RunID, Kind: event.Kind, Status: event.Status,
			LastSequence: envelope.Sequence, LastEventTime: envelope.Time,
			LastEventType: event.EventType,
		}
		if isTerminalStatus(event.Status) {
			terminal[event.RunID] = true
		}
	}
	history := make([]HistoryEntry, 0, len(latest))
	for _, entry := range latest {
		history = append(history, entry)
	}
	sort.Slice(history, func(left, right int) bool {
		if history[left].LastEventTime.Equal(history[right].LastEventTime) {
			return history[left].RunID > history[right].RunID
		}
		return history[left].LastEventTime.After(history[right].LastEventTime)
	})
	return HistorySnapshot{Watermark: watermark, Entries: history}, nil
}

func (repository *Repository) verifyRunClosure(run runmodel.ReviewRun) error {
	return repository.verifyRunClosureSeen(run, make(map[string]struct{}))
}

func (repository *Repository) verifyRunClosureSeen(
	run runmodel.ReviewRun,
	ancestors map[string]struct{},
) error {
	if _, cycle := ancestors[run.RunID]; cycle {
		return fmt.Errorf("replay lineage contains cycle at run %q", run.RunID)
	}
	ancestors[run.RunID] = struct{}{}
	defer delete(ancestors, run.RunID)

	snapshot, err := repository.LoadExecutionSnapshot(run.ExecutionSnapshotID)
	if err != nil {
		return fmt.Errorf("load execution snapshot %q: %w", run.ExecutionSnapshotID, err)
	}
	if snapshot.ExecutionSnapshotID != run.ExecutionSnapshotID {
		return fmt.Errorf("execution snapshot identity mismatch")
	}
	if snapshot.TargetSnapshotRef != run.TargetSnapshotRef {
		return fmt.Errorf("target snapshot reference does not match execution snapshot")
	}
	artifacts := make(map[string][]byte)
	for name, ref := range map[string]runmodel.ArtifactRef{
		"review spec":         snapshot.ReviewSpecRef,
		"target snapshot":     snapshot.TargetSnapshotRef,
		"review input":        snapshot.ReviewInputRef,
		"workflow definition": snapshot.WorkflowDefinitionRef,
		"config bundle":       snapshot.ConfigBundleRef,
	} {
		content, err := repository.ReadArtifact(ref)
		if err != nil {
			return fmt.Errorf("%s artifact: %w", name, err)
		}
		artifacts[name] = content
	}
	if snapshot.ReviewShardManifestRef != nil {
		content, err := repository.ReadArtifact(*snapshot.ReviewShardManifestRef)
		if err != nil {
			return fmt.Errorf("review shard manifest artifact: %w", err)
		}
		artifacts["review shard manifest"] = content
	}
	var definition workflow.Definition
	if err := decodeStrictJSON(artifacts["workflow definition"], &definition); err != nil {
		return fmt.Errorf("decode workflow definition: %w", err)
	}
	if err := definition.Validate(); err != nil {
		return fmt.Errorf("validate workflow definition: %w", err)
	}
	if definition.ID != snapshot.Workflow.ID || definition.Revision != snapshot.Workflow.Revision {
		return fmt.Errorf("workflow definition identity does not match execution snapshot")
	}
	configBundle, err := reviewconfig.DecodeBundle(artifacts["config bundle"])
	if err != nil {
		return fmt.Errorf("decode config bundle: %w", err)
	}
	if configBundle.BundleID != snapshot.Config.ID ||
		configBundle.SHA256 != snapshot.Config.SHA256 ||
		snapshot.Config.Revision != configBundle.SHA256[:16] {
		return fmt.Errorf("config bundle identity does not match execution snapshot")
	}
	if configBundle.Workflow.Definition.ID != snapshot.Workflow.ID ||
		configBundle.Workflow.Definition.Revision != snapshot.Workflow.Revision ||
		configBundle.Workflow.Definition.SHA256 != snapshot.Workflow.SHA256 ||
		!reflect.DeepEqual(
			configBundle.Execution.AllowedTools,
			snapshot.ToolPolicy.AllowedTools,
		) ||
		configBundle.Budget.StageTimeoutMS != snapshot.ToolPolicy.PerCallTimeoutMS ||
		configBundle.Budget.MaxOutputBytes != snapshot.ToolPolicy.MaxOutputBytes ||
		configBundle.Budget.MaxConcurrency != snapshot.ToolPolicy.MaxConcurrency ||
		string(configBundle.Publication.RemoteWrites) != snapshot.RemoteWrites ||
		string(configBundle.Publication.RemoteWrites) != snapshot.ToolPolicy.RemoteWrites ||
		snapshot.RuntimeProfile != configBundle.Execution.AgentProfile.ID+"@"+
			configBundle.Execution.AgentProfile.Revision {
		return fmt.Errorf("config bundle is not exactly projected into execution policy")
	}
	workflowOrder, err := definition.TopologicalOrder()
	if err != nil {
		return fmt.Errorf("order workflow definition: %w", err)
	}
	reviewInput, err := reviewcore.DecodeReviewInput(artifacts["review input"])
	if err != nil {
		return fmt.Errorf("decode review input: %w", err)
	}
	if snapshot.ReviewShardManifestRef != nil && reviewInput.TargetMode != reviewcore.TargetModeScope {
		return fmt.Errorf("review shard manifest may only bind a scope ReviewInput")
	}
	targetDigest, err := reviewcore.DigestReviewInput(reviewInput)
	if err != nil {
		return fmt.Errorf("digest review input: %w", err)
	}
	spec, err := contractsv1alpha1.DecodeReviewSpec(artifacts["review spec"])
	if err != nil {
		return fmt.Errorf("decode review spec: %w", err)
	}
	if spec.RequestID != run.RunID ||
		spec.WorkflowRef.ID != snapshot.Workflow.ID ||
		spec.WorkflowRef.Revision != snapshot.Workflow.Revision ||
		spec.WorkflowRef.SHA256 != snapshot.Workflow.SHA256 ||
		spec.ConfigBundleRef.ID != snapshot.Config.ID ||
		spec.ConfigBundleRef.Revision != snapshot.Config.Revision ||
		spec.ConfigBundleRef.SHA256 != snapshot.ConfigBundleRef.SHA256 {
		return fmt.Errorf("review spec identity and versioned refs do not match final run snapshot")
	}
	if configBundle.Context.TenantID != spec.TenantID ||
		configBundle.Context.OrganizationID != "local" ||
		configBundle.Context.RepositoryID != spec.Repository.RepositoryID {
		return fmt.Errorf("config bundle context does not match review authorization")
	}
	if run.Kind == runmodel.RunKindReview &&
		configBundle.Context.InvocationID != run.RunID {
		return fmt.Errorf("config bundle invocation does not match review run")
	}
	if spec.Target.Mode == contractsv1alpha1.ReviewModeSelection &&
		configBundle.Context.Path != spec.Target.Selection.Path {
		return fmt.Errorf("config bundle path does not match selection target")
	}
	if spec.Target.Mode != contractsv1alpha1.ReviewModeSelection &&
		configBundle.Context.Path != "" {
		return fmt.Errorf("repository-wide target requires an empty config context path")
	}
	var target targetmodel.MaterializedTarget
	if err := decodeStrictJSON(artifacts["target snapshot"], &target); err != nil {
		return fmt.Errorf("decode materialized target: %w", err)
	}
	if err := target.Validate(); err != nil {
		return fmt.Errorf("validate materialized target: %w", err)
	}
	if len(snapshot.ContextProviderReceiptRefs) != len(configBundle.Execution.ContextProviders) {
		return fmt.Errorf("context provider receipt count does not match frozen config")
	}
	contextByID := make(map[string]reviewcore.ContextBinding, len(target.Contexts))
	for _, binding := range target.Contexts {
		contextByID[binding.ContextID()] = binding
	}
	for index, ref := range snapshot.ContextProviderReceiptRefs {
		data, err := repository.ReadArtifact(ref)
		if err != nil {
			return fmt.Errorf("read context provider receipt %d: %w", index, err)
		}
		receipt, err := contractsv1alpha1.DecodeContextProviderExecutionReceipt(data)
		if err != nil {
			return fmt.Errorf("decode context provider receipt %d: %w", index, err)
		}
		definition := configBundle.Execution.ContextProviders[index]
		if receipt.ProviderID != definition.ID || receipt.ProviderRevision != definition.Revision ||
			receipt.Kind != definition.Kind || receipt.Adapter.ID != definition.Adapter.ID ||
			receipt.Adapter.Revision != definition.Adapter.Revision || receipt.Adapter.SHA256 != definition.Adapter.SHA256 ||
			receipt.RepositoryID != target.Snapshot.Repository.RepositoryID ||
			receipt.CommitOID != target.Snapshot.Head.CommitOID {
			return fmt.Errorf("context provider receipt %d does not match config or target", index)
		}
		binding, exists := contextByID[receipt.ContextID]
		if !exists {
			return fmt.Errorf("context provider receipt %d has no materialized binding", index)
		}
		switch receipt.Status {
		case contractsv1alpha1.ContextProviderReceiptSucceeded:
			if binding.Ref == nil || binding.Ref.Digest != receipt.ContextDigest || binding.Ref.Contract != receipt.ContextContract {
				return fmt.Errorf("context provider receipt %d success does not match ContextRef", index)
			}
		case contractsv1alpha1.ContextProviderReceiptGap:
			if binding.Gap == nil || binding.Gap.Digest != receipt.ContextDigest || binding.Gap.ReasonCode != receipt.ReasonCode {
				return fmt.Errorf("context provider receipt %d gap does not match ContextGap", index)
			}
		}
	}
	if err := repository.verifyMaterializedTargetClosure(
		target,
		spec,
		run,
		reviewInput,
		configBundle,
	); err != nil {
		return err
	}
	if string(run.TargetMode) != string(spec.Target.Mode) ||
		string(run.TargetMode) != string(reviewInput.TargetMode) {
		return fmt.Errorf("review target mode does not match run and frozen input")
	}
	switch run.TargetMode {
	case runmodel.TargetModeDiff:
		if spec.Target.Diff == nil ||
			spec.Target.Diff.BaseRevision != run.BaseRevision ||
			spec.Target.Diff.HeadRevision != run.HeadRevision {
			return fmt.Errorf("review spec diff target does not match final run revisions")
		}
		patchRef := runmodel.ArtifactRef{
			URI:       spec.Target.Diff.Patch.URI,
			SHA256:    spec.Target.Diff.Patch.SHA256,
			SizeBytes: spec.Target.Diff.Patch.SizeBytes,
			Contract:  runmodel.ContractCanonicalPatch,
		}
		patch, err := repository.ReadArtifact(patchRef)
		if err != nil {
			return fmt.Errorf("review spec canonical patch: %w", err)
		}
		if !bytes.Equal(patch, []byte(reviewInput.CanonicalPatch)) {
			return fmt.Errorf("review spec canonical patch does not match frozen review input")
		}
		if target.PatchRef == nil || *target.PatchRef != patchRef {
			return fmt.Errorf("review spec canonical patch does not match materialized target")
		}
	case runmodel.TargetModeSelection:
		selection := spec.Target.Selection
		targetSelection := target.Snapshot.Selection
		if selection == nil || selection.Revision != run.HeadRevision ||
			run.BaseRevision != run.HeadRevision ||
			targetSelection == nil ||
			targetSelection.Path != selection.Path ||
			!selectionSelectorMatchesSnapshot(*selection, *targetSelection) ||
			!selectionRegionsMatchSnapshot(reviewInput.Regions, *targetSelection) {
			return fmt.Errorf("review spec selection does not match final run target")
		}
		contentRef := runmodel.ArtifactRef{
			URI: selection.Content.URI, SHA256: selection.Content.SHA256,
			SizeBytes: selection.Content.SizeBytes,
			Contract:  runmodel.ContractSelectionContent,
		}
		selected, err := repository.ReadArtifact(contentRef)
		if err != nil {
			return fmt.Errorf("review spec selection content: %w", err)
		}
		expected, err := selectedRegionsContent(reviewInput, reviewInput.Regions)
		if err != nil {
			return fmt.Errorf("reconstruct frozen selection: %w", err)
		}
		if !bytes.Equal(selected, expected) {
			return fmt.Errorf("review spec selection content does not match frozen review input")
		}
		if target.SelectionContentRef == nil || *target.SelectionContentRef != contentRef {
			return fmt.Errorf("review spec selection content does not match materialized target")
		}
	case runmodel.TargetModeScope:
		scope := spec.Target.Scope
		if scope == nil || scope.Revision != run.HeadRevision ||
			run.BaseRevision != run.HeadRevision {
			return fmt.Errorf("review spec scope does not match final run revision")
		}
		if target.Snapshot.Scope == nil ||
			!reflect.DeepEqual(scope.Include, target.Snapshot.Scope.Include) ||
			!reflect.DeepEqual(scope.Exclude, target.Snapshot.Scope.Exclude) {
			return fmt.Errorf("review spec scope patterns do not match materialized target")
		}
	default:
		return fmt.Errorf("unsupported run target mode %q", run.TargetMode)
	}
	if isFormalAgentWorkflow(definition) {
		return repository.verifyFormalAgentRunClosure(
			run, snapshot, spec, target, reviewInput, targetDigest, configBundle, ancestors,
		)
	}

	knownInputs := map[runmodel.ArtifactRef]struct{}{snapshot.ReviewInputRef: {}}
	completedStages := make(map[string]struct{})
	reusedResults := make(map[string]reviewcore.StageResult)
	stageResultsByRef := make(map[runmodel.ArtifactRef]reviewcore.StageResult)
	for index, ref := range snapshot.ReplayInputRefs {
		content, err := repository.ReadArtifact(ref)
		if err != nil {
			return fmt.Errorf("replay_input_refs[%d]: %w", index, err)
		}
		stageResult, err := reviewcore.DecodeStageResult(content)
		if err != nil {
			return fmt.Errorf("decode replay_input_refs[%d]: %w", index, err)
		}
		if stageResult.TargetDigest != targetDigest {
			return fmt.Errorf("replay_input_refs[%d] targets another review input", index)
		}
		stageID := string(stageResult.Stage)
		if _, duplicate := completedStages[stageID]; duplicate {
			return fmt.Errorf("replay_input_refs contains duplicate stage %q", stageID)
		}
		completedStages[stageID] = struct{}{}
		reusedResults[stageID] = stageResult
		stageResultsByRef[ref] = stageResult
		knownInputs[ref] = struct{}{}
	}
	if run.Kind == runmodel.RunKindReview && len(snapshot.ReplayInputRefs) != 0 {
		return fmt.Errorf("review run cannot contain replay input refs")
	}
	if run.Kind == runmodel.RunKindReview && snapshot.ReplaySourceRunRef != nil {
		return fmt.Errorf("review run cannot contain replay source lineage")
	}
	if run.Kind == runmodel.RunKindReview && snapshot.ReplayChangeSetRef != nil {
		return fmt.Errorf("review run cannot contain a replay change set")
	}
	if run.Kind == runmodel.RunKindReplay {
		if err := repository.verifyReplayLineage(
			run,
			snapshot,
			spec,
			configBundle,
			ancestors,
		); err != nil {
			return err
		}
		replayStart := -1
		for index, stageID := range workflowOrder {
			if stageID == run.ReplayFromStage {
				replayStart = index
				break
			}
		}
		if replayStart < 0 {
			return fmt.Errorf("replay_from_stage %q is outside workflow", run.ReplayFromStage)
		}
		if len(snapshot.ReplayInputRefs) != replayStart {
			return fmt.Errorf("replay input refs do not exactly cover the workflow prefix")
		}
		for index := 0; index < replayStart; index++ {
			result, exists := stageResultsByRef[snapshot.ReplayInputRefs[index]]
			if !exists || string(result.Stage) != workflowOrder[index] {
				return fmt.Errorf("replay input refs are not the ordered workflow prefix")
			}
		}
	}
	workflowStages := make(map[string]struct{}, len(definition.Stages))
	workflowStageDefinitions := make(map[string]workflow.Stage, len(definition.Stages))
	for _, stage := range definition.Stages {
		workflowStages[stage.ID] = struct{}{}
		workflowStageDefinitions[stage.ID] = stage
	}
	latestAttempts := make(map[string]runmodel.StageAttempt)
	resultsByBinding := make(map[string]reviewcore.StageResult)
	for index, attempt := range run.StageAttempts {
		if _, exists := workflowStages[attempt.StageID]; !exists {
			return fmt.Errorf("stage_attempts[%d] references stage %q outside workflow",
				index, attempt.StageID)
		}
		if _, reused := completedStages[attempt.StageID]; reused {
			return fmt.Errorf("stage_attempts[%d] re-executes reused stage %q",
				index, attempt.StageID)
		}
		for inputIndex, ref := range attempt.InputRefs {
			if _, exists := knownInputs[ref]; !exists {
				return fmt.Errorf("stage_attempts[%d].input_refs[%d] is not in the execution closure",
					index, inputIndex)
			}
			if _, err := repository.ReadArtifact(ref); err != nil {
				return fmt.Errorf("stage_attempts[%d].input_refs[%d]: %w", index, inputIndex, err)
			}
		}
		if attempt.OutputRef != nil {
			content, err := repository.ReadArtifact(*attempt.OutputRef)
			if err != nil {
				return fmt.Errorf("stage_attempts[%d].output_ref: %w", index, err)
			}
			stageResult, err := reviewcore.DecodeStageResult(content)
			if err != nil {
				return fmt.Errorf("decode stage_attempts[%d].output_ref: %w", index, err)
			}
			if string(stageResult.Stage) != attempt.StageID ||
				stageResult.TargetDigest != targetDigest {
				return fmt.Errorf("stage_attempts[%d].output_ref does not match stage and target", index)
			}
			if attempt.Status == runmodel.StageStatusSucceeded {
				if attempt.StageID == string(reviewcore.StageMaterializeTarget) {
					if stageResult.InputDigest != targetDigest ||
						len(attempt.InputRefs) != 1 ||
						attempt.InputRefs[0] != snapshot.ReviewInputRef {
						return fmt.Errorf(
							"stage_attempts[%d] materialize_target input binding is not exact",
							index,
						)
					}
				} else {
					var predecessor reviewcore.StageResult
					foundPredecessor := false
					for inputIndex := len(attempt.InputRefs) - 1; inputIndex >= 0; inputIndex-- {
						candidate, exists := stageResultsByRef[attempt.InputRefs[inputIndex]]
						if exists {
							predecessor = candidate
							foundPredecessor = true
							break
						}
					}
					if !foundPredecessor ||
						stageResult.InputDigest != predecessor.ArtifactDigest ||
						!containsString(
							workflowStageDefinitions[attempt.StageID].DependsOn,
							string(predecessor.Stage),
						) {
						return fmt.Errorf(
							"stage_attempts[%d] output is not bound to its exact workflow predecessor",
							index,
						)
					}
				}
			}
			resultsByBinding[attempt.BindingID] = stageResult
			stageResultsByRef[*attempt.OutputRef] = stageResult
			knownInputs[*attempt.OutputRef] = struct{}{}
		}
		latest, exists := latestAttempts[attempt.StageID]
		if !exists || attempt.Generation > latest.Generation ||
			(attempt.Generation == latest.Generation && attempt.Attempt > latest.Attempt) {
			latestAttempts[attempt.StageID] = attempt
		}
	}
	if run.Status == runmodel.RunStatusSucceeded {
		priorDigest := targetDigest
		for _, stageID := range workflowOrder {
			if reused, exists := reusedResults[stageID]; exists {
				if reused.InputDigest != priorDigest {
					return fmt.Errorf("reused stage %q breaks the workflow artifact chain", stageID)
				}
				priorDigest = reused.ArtifactDigest
				continue
			}
			attempt, exists := latestAttempts[stageID]
			if !exists || attempt.Status != runmodel.StageStatusSucceeded {
				return fmt.Errorf("succeeded run has no final succeeded result for workflow stage %q",
					stageID)
			}
			result, exists := resultsByBinding[attempt.BindingID]
			if !exists {
				return fmt.Errorf("succeeded stage %q has no decoded output", stageID)
			}
			if result.InputDigest != priorDigest {
				return fmt.Errorf("stage %q breaks the workflow artifact chain", stageID)
			}
			priorDigest = result.ArtifactDigest
		}
	}
	expectedCompleteness, expectedCompletenessNotes := targetEvidenceCompleteness(target)
	if manifestData, exists := artifacts["review shard manifest"]; exists {
		shardNotes, err := verifyShardManifestProjection(
			manifestData, run.RunID, snapshot.ReviewInputRef, targetDigest,
		)
		if err != nil {
			return err
		}
		if len(shardNotes) != 0 {
			expectedCompleteness = runmodel.CompletenessPartial
			expectedCompletenessNotes = append(expectedCompletenessNotes, shardNotes...)
			sort.Strings(expectedCompletenessNotes)
			expectedCompletenessNotes = slices.Compact(expectedCompletenessNotes)
		}
	}
	for index, evidence := range run.Evidence {
		if _, err := repository.ReadArtifact(evidence.ArtifactRef); err != nil {
			return fmt.Errorf("run_evidence[%d].artifact_ref: %w", index, err)
		}
		if evidence.Completeness != expectedCompleteness ||
			!reflect.DeepEqual(evidence.CompletenessNotes, expectedCompletenessNotes) {
			return fmt.Errorf(
				"run_evidence[%d] completeness does not match the frozen target",
				index,
			)
		}
	}
	if run.Status == runmodel.RunStatusSucceeded {
		jsonReport, err := repository.ReadArtifact(*run.JSONReportRef)
		if err != nil {
			return fmt.Errorf("JSON report artifact: %w", err)
		}
		report, err := reviewcore.DecodeReport(jsonReport)
		if err != nil {
			return fmt.Errorf("decode JSON report artifact: %w", err)
		}
		if report.TargetDigest != targetDigest {
			return fmt.Errorf("JSON report targets another review input")
		}
		reportAttempt, exists := latestAttempts[string(reviewcore.StageReport)]
		if !exists {
			return fmt.Errorf("successful run has no report stage attempt")
		}
		reportStage, exists := resultsByBinding[reportAttempt.BindingID]
		if !exists || reportStage.Output.Report == nil ||
			!reflect.DeepEqual(*reportStage.Output.Report, report) {
			return fmt.Errorf("JSON report does not exactly match report stage output")
		}
		canonicalReport, err := reviewcore.MarshalReportJSON(report)
		if err != nil {
			return fmt.Errorf("marshal canonical JSON report: %w", err)
		}
		if !bytes.Equal(jsonReport, canonicalReport) {
			return fmt.Errorf("JSON report artifact is not canonical")
		}
		var findingSet findingSetProjection
		if err := repository.ReadJSONArtifact(*run.FindingSetRef, &findingSet); err != nil {
			return fmt.Errorf("decode finding set artifact: %w", err)
		}
		if findingSet.SchemaVersion != runmodel.ContractFindingSet ||
			findingSet.TargetDigest != targetDigest {
			return fmt.Errorf("finding set schema or target does not match final run")
		}
		for index, finding := range findingSet.Findings {
			if err := finding.Validate(); err != nil {
				return fmt.Errorf("finding set findings[%d]: %w", index, err)
			}
		}
		for index, decision := range findingSet.Decisions {
			if err := decision.Validate(); err != nil {
				return fmt.Errorf("finding set decisions[%d]: %w", index, err)
			}
		}
		if !reflect.DeepEqual(findingSet.Findings, report.Findings) ||
			!reflect.DeepEqual(findingSet.Decisions, report.Decisions) {
			return fmt.Errorf("finding set does not exactly match JSON report")
		}
		markdown, err := repository.ReadArtifact(*run.MarkdownReportRef)
		if err != nil {
			return fmt.Errorf("Markdown report artifact: %w", err)
		}
		expectedMarkdown, err := reviewcore.RenderReportMarkdown(context.Background(), report)
		if err != nil {
			return fmt.Errorf("render canonical Markdown report: %w", err)
		}
		if !bytes.Equal(markdown, []byte(expectedMarkdown)) {
			return fmt.Errorf("Markdown report does not exactly match JSON report")
		}
	} else {
		for name, ref := range map[string]*runmodel.ArtifactRef{
			"finding set":     run.FindingSetRef,
			"JSON report":     run.JSONReportRef,
			"Markdown report": run.MarkdownReportRef,
		} {
			if ref == nil {
				continue
			}
			if _, err := repository.ReadArtifact(*ref); err != nil {
				return fmt.Errorf("%s artifact: %w", name, err)
			}
		}
	}
	if err := repository.verifyLedgerProjection(run); err != nil {
		return err
	}
	return nil
}

func (repository *Repository) verifyReplayLineage(
	run runmodel.ReviewRun,
	snapshot runmodel.ExecutionSnapshot,
	spec contractsv1alpha1.ReviewSpec,
	config reviewconfig.ConfigBundle,
	ancestors map[string]struct{},
) error {
	if run.SourceRunID == run.RunID {
		return fmt.Errorf("replay source run cannot be itself")
	}
	if snapshot.ReplaySourceRunRef == nil {
		return fmt.Errorf("replay execution snapshot requires immutable source run reference")
	}
	if snapshot.ReplayChangeSetRef == nil {
		return fmt.Errorf("replay execution snapshot requires an immutable change set")
	}
	changeData, err := repository.ReadArtifact(*snapshot.ReplayChangeSetRef)
	if err != nil {
		return fmt.Errorf("read replay change set: %w", err)
	}
	var change runmodel.ReplayChangeSet
	if err := decodeStrictJSON(changeData, &change); err != nil {
		return fmt.Errorf("decode replay change set: %w", err)
	}
	if err := change.Validate(); err != nil {
		return fmt.Errorf("validate replay change set: %w", err)
	}
	if change.Namespace != run.ReplayNamespace ||
		change.SourceRunID != run.SourceRunID ||
		change.RootRunID != run.ReplayRootRunID ||
		change.StartStage != run.ReplayFromStage ||
		change.Variable != run.ReplayVariable {
		return fmt.Errorf("replay change set does not match run lineage")
	}
	committedRef, err := repository.CommittedRunRef(run.SourceRunID)
	if err != nil {
		return fmt.Errorf("load replay source commit: %w", err)
	}
	if committedRef != *snapshot.ReplaySourceRunRef {
		return fmt.Errorf("replay source reference does not match authoritative terminal event")
	}
	content, err := repository.ReadArtifact(committedRef)
	if err != nil {
		return fmt.Errorf("read replay source run: %w", err)
	}
	var source runmodel.ReviewRun
	if err := decodeStrictJSON(content, &source); err != nil {
		return fmt.Errorf("decode replay source run: %w", err)
	}
	if err := source.Validate(); err != nil {
		return fmt.Errorf("validate replay source run: %w", err)
	}
	if source.RunID != run.SourceRunID || source.Status != runmodel.RunStatusSucceeded {
		return fmt.Errorf("replay source must be the exact succeeded committed run")
	}
	expectedRoot := source.RunID
	expectedParent := ""
	if source.Kind == runmodel.RunKindReplay {
		expectedRoot = source.ReplayRootRunID
		expectedParent = source.RunID
	}
	if run.ReplayRootRunID != expectedRoot ||
		change.ParentReplayRunID != expectedParent {
		return fmt.Errorf("replay root or parent lineage does not match source run")
	}
	if err := repository.verifyRunClosureSeen(source, ancestors); err != nil {
		return fmt.Errorf("verify replay source closure: %w", err)
	}
	if source.RepositoryPath != run.RepositoryPath ||
		source.TargetMode != run.TargetMode ||
		source.BaseRevision != run.BaseRevision ||
		source.HeadRevision != run.HeadRevision ||
		(source.TargetSnapshotRef != run.TargetSnapshotRef &&
			change.Variable != runmodel.ReplayVariableIndex) {
		return fmt.Errorf("replay source target does not match replay run")
	}
	sourceSnapshot, err := repository.LoadExecutionSnapshot(source.ExecutionSnapshotID)
	if err != nil {
		return fmt.Errorf("load replay source execution snapshot: %w", err)
	}
	if (sourceSnapshot.TargetSnapshotRef != snapshot.TargetSnapshotRef ||
		sourceSnapshot.ReviewInputRef != snapshot.ReviewInputRef) &&
		change.Variable != runmodel.ReplayVariableIndex {
		return fmt.Errorf("replay source snapshot does not bind the same target and input")
	}
	if sourceSnapshot.BuildIdentity != snapshot.BuildIdentity &&
		change.Variable != runmodel.ReplayVariableModel &&
		change.Variable != runmodel.ReplayVariablePrompt &&
		change.Variable != runmodel.ReplayVariableSkillPack &&
		change.Variable != runmodel.ReplayVariableKnowledgePack {
		return fmt.Errorf("replay changed build identity without declaration")
	}
	workflowChanged := sourceSnapshot.Workflow != snapshot.Workflow ||
		sourceSnapshot.WorkflowDefinitionRef != snapshot.WorkflowDefinitionRef
	if workflowChanged && change.Variable != runmodel.ReplayVariableWorkflow {
		return fmt.Errorf("replay changed workflow without declaration")
	}
	if !workflowChanged && change.Variable == runmodel.ReplayVariableWorkflow {
		return fmt.Errorf("workflow replay did not freeze a changed workflow")
	}
	if sourceSnapshot.RuntimeProfile != snapshot.RuntimeProfile ||
		!reflect.DeepEqual(sourceSnapshot.RuntimeEvidenceRefs, snapshot.RuntimeEvidenceRefs) {
		return fmt.Errorf("replay changed runtime profile or evidence without declaration")
	}
	contextReceiptsChanged := !reflect.DeepEqual(
		sourceSnapshot.ContextProviderReceiptRefs,
		snapshot.ContextProviderReceiptRefs,
	)
	if contextReceiptsChanged && change.Variable != runmodel.ReplayVariableIndex {
		return fmt.Errorf("replay changed context provider evidence without declaration")
	}
	if !contextReceiptsChanged && change.Variable == runmodel.ReplayVariableIndex {
		return fmt.Errorf("index replay did not freeze new context provider evidence")
	}
	sourceConfigData, err := repository.ReadArtifact(sourceSnapshot.ConfigBundleRef)
	if err != nil {
		return fmt.Errorf("read replay source ConfigBundle: %w", err)
	}
	sourceConfig, err := reviewconfig.DecodeBundle(sourceConfigData)
	if err != nil {
		return fmt.Errorf("decode replay source ConfigBundle: %w", err)
	}
	if err := validateDeclaredReplayConfigChange(
		sourceSnapshot,
		snapshot,
		sourceConfig,
		config,
		change,
	); err != nil {
		return err
	}
	if change.Variable == runmodel.ReplayVariableWorkflow {
		if err := repository.verifyWorkflowReplayChange(
			sourceSnapshot,
			snapshot,
			change,
			config,
		); err != nil {
			return err
		}
	}
	if change.Variable == runmodel.ReplayVariableIndex {
		if err := repository.verifyIndexReplayChange(
			sourceSnapshot,
			snapshot,
			sourceConfig,
			config,
			change,
		); err != nil {
			return err
		}
	}
	sourceSpecData, err := repository.ReadArtifact(sourceSnapshot.ReviewSpecRef)
	if err != nil {
		return fmt.Errorf("read replay source ReviewSpec: %w", err)
	}
	sourceSpec, err := contractsv1alpha1.DecodeReviewSpec(sourceSpecData)
	if err != nil {
		return fmt.Errorf("decode replay source ReviewSpec: %w", err)
	}
	sourceSpec.RequestID = ""
	sourceSpec.IdempotencyKey = ""
	spec.RequestID = ""
	spec.IdempotencyKey = ""
	if change.Variable != runmodel.ReplayVariableNone {
		sourceSpec.ConfigBundleRef = spec.ConfigBundleRef
	}
	if change.Variable == runmodel.ReplayVariableWorkflow {
		sourceSpec.WorkflowRef = spec.WorkflowRef
	}
	if !reflect.DeepEqual(sourceSpec, spec) {
		return fmt.Errorf("replay changed ReviewSpec authorization or intent")
	}
	authoritativeOutputs := make(map[string]runmodel.ArtifactRef)
	for index, ref := range sourceSnapshot.ReplayInputRefs {
		data, err := repository.ReadArtifact(ref)
		if err != nil {
			return fmt.Errorf("read source replay checkpoint %d: %w", index, err)
		}
		result, err := reviewcore.DecodeStageResult(data)
		if err != nil {
			return fmt.Errorf("decode source replay checkpoint %d: %w", index, err)
		}
		authoritativeOutputs[string(result.Stage)] = ref
	}
	latestAttempts := make(map[string]runmodel.StageAttempt)
	for _, attempt := range source.StageAttempts {
		latest, exists := latestAttempts[attempt.StageID]
		if !exists || attempt.Generation > latest.Generation ||
			attempt.Generation == latest.Generation && attempt.Attempt > latest.Attempt {
			latestAttempts[attempt.StageID] = attempt
		}
	}
	for stageID, attempt := range latestAttempts {
		if attempt.Status == runmodel.StageStatusSucceeded && attempt.OutputRef != nil {
			authoritativeOutputs[stageID] = *attempt.OutputRef
		}
	}
	for index, ref := range snapshot.ReplayInputRefs {
		data, err := repository.ReadArtifact(ref)
		if err != nil {
			return fmt.Errorf("read replay checkpoint %d: %w", index, err)
		}
		result, err := reviewcore.DecodeStageResult(data)
		if err != nil {
			return fmt.Errorf("decode replay checkpoint %d: %w", index, err)
		}
		if authoritative, exists := authoritativeOutputs[string(result.Stage)]; !exists ||
			authoritative != ref {
			return fmt.Errorf(
				"replay_input_refs[%d] is not the authoritative source-run checkpoint",
				index,
			)
		}
	}
	return nil
}

func (repository *Repository) verifyWorkflowReplayChange(
	sourceSnapshot runmodel.ExecutionSnapshot,
	variantSnapshot runmodel.ExecutionSnapshot,
	change runmodel.ReplayChangeSet,
	config reviewconfig.ConfigBundle,
) error {
	budget := config.Budget
	sourceBytes, err := repository.ReadArtifact(sourceSnapshot.WorkflowDefinitionRef)
	if err != nil {
		return fmt.Errorf("read replay source WorkflowDefinition: %w", err)
	}
	variantBytes, err := repository.ReadArtifact(variantSnapshot.WorkflowDefinitionRef)
	if err != nil {
		return fmt.Errorf("read replay variant WorkflowDefinition: %w", err)
	}
	source, err := workflow.DecodeDefinition(sourceBytes)
	if err != nil {
		return fmt.Errorf("decode replay source WorkflowDefinition: %w", err)
	}
	variant, err := workflow.DecodeDefinition(variantBytes)
	if err != nil {
		return fmt.Errorf("decode replay variant WorkflowDefinition: %w", err)
	}
	sourceDigest, err := workflow.DigestDefinition(source)
	if err != nil {
		return err
	}
	variantDigest, err := workflow.DigestDefinition(variant)
	if err != nil {
		return err
	}
	if sourceSnapshot.Workflow != (runmodel.WorkflowRef{
		ID: source.ID, Revision: source.Revision, SHA256: sourceDigest,
	}) || variantSnapshot.Workflow != (runmodel.WorkflowRef{
		ID: variant.ID, Revision: variant.Revision, SHA256: variantDigest,
	}) {
		return fmt.Errorf("replay workflow snapshot identity does not match exact definition")
	}
	fields, _, err := workflow.DiffReplayExecutionPolicy(source, variant)
	if err != nil {
		return fmt.Errorf("validate declared workflow replay: %w", err)
	}
	if !reflect.DeepEqual(fields, change.ChangedFields) {
		return fmt.Errorf("workflow replay changed_fields do not match exact definitions")
	}
	limits := workflow.RuntimeLimits{
		TimeoutMS:      budget.StageTimeoutMS,
		MaxInputBytes:  budget.MaxInputBytes,
		MaxOutputBytes: budget.MaxOutputBytes,
		MaxAttempts:    budget.MaxAttempts,
		MaxConcurrency: budget.MaxConcurrency,
	}
	if variant.ID == workflow.FormalAgentReviewDefinition().ID {
		if config.AgentReview == nil {
			return fmt.Errorf("formal workflow replay has no AgentReviewPolicy")
		}
		for _, field := range fields {
			if !strings.HasPrefix(field, "workflow.stages.agent_hypothesize.budget.") &&
				!strings.HasPrefix(field, "workflow.stages.agent_hypothesize.retry.") {
				return fmt.Errorf("formal workflow replay changed a non-budget/retry execution field")
			}
		}
		agentBudget := config.AgentReview.Budget
		limits = workflow.RuntimeLimits{
			TimeoutMS:      agentBudget.TimeoutMS,
			MaxInputBytes:  agentBudget.MaxTargetBytes,
			MaxOutputBytes: agentBudget.MaxOutputBytes,
			MaxAttempts:    config.Budget.MaxAttempts,
			MaxConcurrency: agentBudget.MaxConcurrency,
		}
	}
	if !workflow.ReplayChangesEffectivePolicy(
		source,
		variant,
		limits,
	) {
		return fmt.Errorf(
			"workflow replay does not change effective scheduler behavior under frozen runtime limits",
		)
	}
	return nil
}

func validateDeclaredReplayConfigChange(
	sourceSnapshot runmodel.ExecutionSnapshot,
	variantSnapshot runmodel.ExecutionSnapshot,
	source reviewconfig.ConfigBundle,
	variant reviewconfig.ConfigBundle,
	change runmodel.ReplayChangeSet,
) error {
	if change.BaselineSHA256 != source.SHA256 ||
		change.VariantSHA256 != variant.SHA256 ||
		sourceSnapshot.Config.SHA256 != source.SHA256 ||
		variantSnapshot.Config.SHA256 != variant.SHA256 {
		return fmt.Errorf("replay change set digests do not match frozen ConfigBundles")
	}
	if source.Context != variant.Context {
		return fmt.Errorf("replay variant changed the config resolution context")
	}
	if source.SchemaVersion != variant.SchemaVersion ||
		!reflect.DeepEqual(source.AppliedRevisions, variant.AppliedRevisions) ||
		!reflect.DeepEqual(source.FieldSources, variant.FieldSources) ||
		!reflect.DeepEqual(source.Explain, variant.Explain) {
		return fmt.Errorf("replay variant changed config provenance or explanation")
	}
	if !reflect.DeepEqual(source.Target, variant.Target) ||
		(!reflect.DeepEqual(source.Execution, variant.Execution) &&
			change.Variable != runmodel.ReplayVariableIndex) ||
		!reflect.DeepEqual(source.Publication, variant.Publication) ||
		!reflect.DeepEqual(source.Data, variant.Data) {
		return fmt.Errorf("replay variant changed authorization, workflow, execution, publication, or data policy")
	}
	if !change.Variable.IsFilterPolicy() &&
		!reflect.DeepEqual(source.FindingGovernance, variant.FindingGovernance) {
		return fmt.Errorf("replay variant changed finding governance without declaration")
	}

	sourceTools := sourceSnapshot.ToolPolicy
	variantTools := variantSnapshot.ToolPolicy
	switch change.Variable {
	case runmodel.ReplayVariableNone:
		if sourceSnapshot.Config != variantSnapshot.Config ||
			sourceSnapshot.ConfigBundleRef != variantSnapshot.ConfigBundleRef ||
			!reflect.DeepEqual(source, variant) ||
			!reflect.DeepEqual(sourceTools, variantTools) {
			return fmt.Errorf("exact replay changed config or runtime policy")
		}
	case runmodel.ReplayVariableRulePack:
		if !reflect.DeepEqual(source.Workflow, variant.Workflow) {
			return fmt.Errorf("rule_pack replay changed workflow")
		}
		if reflect.DeepEqual(source.RulePack, variant.RulePack) ||
			!reflect.DeepEqual(source.AgentReview, variant.AgentReview) ||
			!reflect.DeepEqual(source.Budget, variant.Budget) ||
			!reflect.DeepEqual(source.Verification, variant.Verification) ||
			!reflect.DeepEqual(source.Adjudication, variant.Adjudication) ||
			!reflect.DeepEqual(change.ChangedFields, []string{"rule_pack"}) ||
			!reflect.DeepEqual(sourceTools, variantTools) {
			return fmt.Errorf("rule_pack replay contains an undeclared or missing change")
		}
	case runmodel.ReplayVariableFilterPolicy:
		if !reflect.DeepEqual(source.FindingGovernance, variant.FindingGovernance) {
			if source.FindingGovernance == nil || variant.FindingGovernance == nil ||
				reflect.DeepEqual(source.FindingGovernance, variant.FindingGovernance) ||
				!reflect.DeepEqual(source.Workflow, variant.Workflow) ||
				!reflect.DeepEqual(source.RulePack, variant.RulePack) ||
				!reflect.DeepEqual(source.Budget, variant.Budget) ||
				!reflect.DeepEqual(source.Verification, variant.Verification) ||
				!reflect.DeepEqual(source.Adjudication, variant.Adjudication) ||
				!reflect.DeepEqual(source.AgentReview, variant.AgentReview) ||
				!reflect.DeepEqual(sourceTools, variantTools) ||
				!reflect.DeepEqual(change.ChangedFields, []string{"finding_governance"}) {
				return fmt.Errorf("formal filter_policy replay contains an undeclared or missing change")
			}
			break
		}
		if !reflect.DeepEqual(source.Workflow, variant.Workflow) ||
			!reflect.DeepEqual(source.RulePack, variant.RulePack) ||
			!reflect.DeepEqual(source.Budget, variant.Budget) ||
			!reflect.DeepEqual(source.AgentReview, variant.AgentReview) ||
			!reflect.DeepEqual(sourceTools, variantTools) {
			return fmt.Errorf("filter replay contains an undeclared config change")
		}
		expected := make([]string, 0, 2)
		if !reflect.DeepEqual(source.Adjudication, variant.Adjudication) {
			expected = append(expected, "adjudication")
		}
		if !reflect.DeepEqual(source.Verification, variant.Verification) {
			expected = append(expected, "verification")
		}
		if len(expected) == 0 || !reflect.DeepEqual(change.ChangedFields, expected) {
			return fmt.Errorf("filter replay change fields do not match frozen policy")
		}
	case runmodel.ReplayVariableFindingGovernance:
		if source.FindingGovernance == nil || variant.FindingGovernance == nil ||
			reflect.DeepEqual(source.FindingGovernance, variant.FindingGovernance) ||
			!reflect.DeepEqual(source.Workflow, variant.Workflow) ||
			!reflect.DeepEqual(source.RulePack, variant.RulePack) ||
			!reflect.DeepEqual(source.Budget, variant.Budget) ||
			!reflect.DeepEqual(source.Verification, variant.Verification) ||
			!reflect.DeepEqual(source.Adjudication, variant.Adjudication) ||
			!reflect.DeepEqual(source.AgentReview, variant.AgentReview) ||
			!reflect.DeepEqual(sourceTools, variantTools) ||
			!reflect.DeepEqual(change.ChangedFields, []string{"finding_governance"}) {
			return fmt.Errorf("finding_governance replay contains an undeclared or missing change")
		}
	case runmodel.ReplayVariableBudget:
		if !reflect.DeepEqual(source.Workflow, variant.Workflow) ||
			!reflect.DeepEqual(source.RulePack, variant.RulePack) ||
			!reflect.DeepEqual(source.Verification, variant.Verification) ||
			!reflect.DeepEqual(source.Adjudication, variant.Adjudication) {
			return fmt.Errorf("budget replay contains an undeclared config change")
		}
		expectedFields := []string{"budget.stage_timeout_ms"}
		switch {
		case source.AgentReview == nil && variant.AgentReview == nil:
		case source.AgentReview == nil || variant.AgentReview == nil:
			return fmt.Errorf("budget replay changed agent review presence")
		default:
			sourcePolicy, variantPolicy := *source.AgentReview, *variant.AgentReview
			sourcePolicy.SHA256, variantPolicy.SHA256 = "", ""
			sourcePolicy.Budget.TimeoutMS, variantPolicy.Budget.TimeoutMS = 0, 0
			if source.AgentReview.Budget.TimeoutMS == variant.AgentReview.Budget.TimeoutMS ||
				!reflect.DeepEqual(sourcePolicy, variantPolicy) ||
				variant.AgentReview.Budget.TimeoutMS != variant.Budget.StageTimeoutMS {
				return fmt.Errorf("budget replay changed an undeclared agent review field")
			}
			expectedFields = []string{
				"agent_review.budget.timeout_ms", "budget.stage_timeout_ms",
			}
		}
		if !reflect.DeepEqual(change.ChangedFields, expectedFields) {
			return fmt.Errorf("budget replay changed_fields do not match frozen policy")
		}
		sourceBudget := source.Budget
		variantBudget := variant.Budget
		sourceBudget.StageTimeoutMS = 0
		variantBudget.StageTimeoutMS = 0
		if source.Budget.StageTimeoutMS == variant.Budget.StageTimeoutMS ||
			!reflect.DeepEqual(sourceBudget, variantBudget) {
			return fmt.Errorf("local budget replay may change stage_timeout_ms only")
		}
		sourceTools.PerCallTimeoutMS = 0
		variantTools.PerCallTimeoutMS = 0
		if !reflect.DeepEqual(sourceTools, variantTools) {
			return fmt.Errorf("budget replay changed another tool policy field")
		}
	case runmodel.ReplayVariableModel:
		if !reflect.DeepEqual(source.Workflow, variant.Workflow) ||
			!reflect.DeepEqual(source.RulePack, variant.RulePack) ||
			!reflect.DeepEqual(source.Budget, variant.Budget) ||
			!reflect.DeepEqual(source.Verification, variant.Verification) ||
			!reflect.DeepEqual(source.Adjudication, variant.Adjudication) ||
			source.AgentReview == nil || variant.AgentReview == nil ||
			!reflect.DeepEqual(change.ChangedFields, []string{"agent_review.model"}) ||
			!reflect.DeepEqual(sourceTools, variantTools) ||
			sourceSnapshot.BuildIdentity == variantSnapshot.BuildIdentity {
			return fmt.Errorf("model replay contains an undeclared or missing change")
		}
		sourcePolicy, variantPolicy := *source.AgentReview, *variant.AgentReview
		sourceModel, variantModel := sourcePolicy.Model, variantPolicy.Model
		sourcePolicy.Model, variantPolicy.Model = reviewconfig.VersionedRef{}, reviewconfig.VersionedRef{}
		sourcePolicy.SHA256, variantPolicy.SHA256 = "", ""
		if sourceModel == variantModel || !reflect.DeepEqual(sourcePolicy, variantPolicy) {
			return fmt.Errorf("model replay changed an undeclared agent review field")
		}
	case runmodel.ReplayVariablePrompt:
		if !reflect.DeepEqual(source.Workflow, variant.Workflow) ||
			!reflect.DeepEqual(source.RulePack, variant.RulePack) ||
			!reflect.DeepEqual(source.Budget, variant.Budget) ||
			!reflect.DeepEqual(source.Verification, variant.Verification) ||
			!reflect.DeepEqual(source.Adjudication, variant.Adjudication) ||
			source.AgentReview == nil || variant.AgentReview == nil ||
			!reflect.DeepEqual(change.ChangedFields, []string{"agent_review.prompt"}) ||
			!reflect.DeepEqual(sourceTools, variantTools) ||
			sourceSnapshot.BuildIdentity == variantSnapshot.BuildIdentity {
			return fmt.Errorf("prompt replay contains an undeclared or missing change")
		}
		sourcePolicy, variantPolicy := *source.AgentReview, *variant.AgentReview
		sourcePrompt, variantPrompt := sourcePolicy.Prompt, variantPolicy.Prompt
		sourcePolicy.Prompt, variantPolicy.Prompt = reviewconfig.VersionedRef{}, reviewconfig.VersionedRef{}
		sourcePolicy.SHA256, variantPolicy.SHA256 = "", ""
		if sourcePrompt == variantPrompt || !reflect.DeepEqual(sourcePolicy, variantPolicy) {
			return fmt.Errorf("prompt replay changed an undeclared agent review field")
		}
	case runmodel.ReplayVariableSkillPack:
		if !reflect.DeepEqual(source.Workflow, variant.Workflow) ||
			!reflect.DeepEqual(source.RulePack, variant.RulePack) ||
			!reflect.DeepEqual(source.Budget, variant.Budget) ||
			!reflect.DeepEqual(source.Verification, variant.Verification) ||
			!reflect.DeepEqual(source.Adjudication, variant.Adjudication) ||
			source.AgentReview == nil || variant.AgentReview == nil ||
			!reflect.DeepEqual(change.ChangedFields, []string{"agent_review.skill_packs"}) ||
			!reflect.DeepEqual(sourceTools, variantTools) ||
			sourceSnapshot.BuildIdentity == variantSnapshot.BuildIdentity {
			return fmt.Errorf("skill_pack replay contains an undeclared or missing change")
		}
		sourcePolicy, variantPolicy := *source.AgentReview, *variant.AgentReview
		sourceSkills, variantSkills := sourcePolicy.SkillPacks, variantPolicy.SkillPacks
		sourcePolicy.SkillPacks, variantPolicy.SkillPacks = nil, nil
		sourcePolicy.SHA256, variantPolicy.SHA256 = "", ""
		if len(variantSkills) == 0 || reflect.DeepEqual(sourceSkills, variantSkills) ||
			!reflect.DeepEqual(sourcePolicy, variantPolicy) {
			return fmt.Errorf("skill_pack replay changed an undeclared agent review field")
		}
	case runmodel.ReplayVariableKnowledgePack:
		if !reflect.DeepEqual(source.Workflow, variant.Workflow) ||
			!reflect.DeepEqual(source.RulePack, variant.RulePack) ||
			!reflect.DeepEqual(source.Budget, variant.Budget) ||
			!reflect.DeepEqual(source.Verification, variant.Verification) ||
			!reflect.DeepEqual(source.Adjudication, variant.Adjudication) ||
			source.AgentReview == nil || variant.AgentReview == nil ||
			!reflect.DeepEqual(change.ChangedFields, []string{"agent_review.knowledge_packs"}) ||
			!reflect.DeepEqual(sourceTools, variantTools) ||
			sourceSnapshot.BuildIdentity == variantSnapshot.BuildIdentity {
			return fmt.Errorf("knowledge_pack replay contains an undeclared or missing change")
		}
		sourcePolicy, variantPolicy := *source.AgentReview, *variant.AgentReview
		sourceKnowledge, variantKnowledge := sourcePolicy.KnowledgePacks, variantPolicy.KnowledgePacks
		sourcePolicy.KnowledgePacks, variantPolicy.KnowledgePacks = nil, nil
		sourcePolicy.SHA256, variantPolicy.SHA256 = "", ""
		if len(variantKnowledge) == 0 || reflect.DeepEqual(sourceKnowledge, variantKnowledge) ||
			!reflect.DeepEqual(sourcePolicy, variantPolicy) {
			return fmt.Errorf("knowledge_pack replay changed an undeclared agent review field")
		}
	case runmodel.ReplayVariableWorkflow:
		if reflect.DeepEqual(source.Workflow, variant.Workflow) ||
			!reflect.DeepEqual(source.RulePack, variant.RulePack) ||
			!reflect.DeepEqual(source.Budget, variant.Budget) ||
			!reflect.DeepEqual(source.Verification, variant.Verification) ||
			!reflect.DeepEqual(source.Adjudication, variant.Adjudication) ||
			!reflect.DeepEqual(source.AgentReview, variant.AgentReview) ||
			!reflect.DeepEqual(sourceTools, variantTools) {
			return fmt.Errorf("workflow replay contains an undeclared or missing change")
		}
		if source.Workflow.Definition.ID != variant.Workflow.Definition.ID ||
			source.Workflow.Definition == variant.Workflow.Definition ||
			variant.Workflow.Definition.ID != variantSnapshot.Workflow.ID ||
			variant.Workflow.Definition.Revision != variantSnapshot.Workflow.Revision ||
			variant.Workflow.Definition.SHA256 != variantSnapshot.Workflow.SHA256 {
			return fmt.Errorf("workflow replay ConfigBundle does not bind exact variant workflow")
		}
	case runmodel.ReplayVariableIndex:
		if !reflect.DeepEqual(source.Workflow, variant.Workflow) ||
			!reflect.DeepEqual(source.RulePack, variant.RulePack) ||
			!reflect.DeepEqual(source.Budget, variant.Budget) ||
			!reflect.DeepEqual(source.Verification, variant.Verification) ||
			!reflect.DeepEqual(source.Adjudication, variant.Adjudication) ||
			!reflect.DeepEqual(source.AgentReview, variant.AgentReview) ||
			!reflect.DeepEqual(sourceTools, variantTools) {
			return fmt.Errorf("index replay contains an undeclared non-index change")
		}
		fields, err := reviewconfig.DiffReplayIndexPolicy(source.Execution, variant.Execution)
		if err != nil {
			return fmt.Errorf("validate declared index replay: %w", err)
		}
		if !reflect.DeepEqual(fields, change.ChangedFields) {
			return fmt.Errorf("index replay changed_fields do not match frozen provider policy")
		}
	default:
		return fmt.Errorf("replay variable %q is not executable by the local runtime",
			change.Variable)
	}
	return nil
}

func (repository *Repository) verifyMaterializedTargetClosure(
	target targetmodel.MaterializedTarget,
	spec contractsv1alpha1.ReviewSpec,
	run runmodel.ReviewRun,
	input reviewcore.ReviewInput,
	config reviewconfig.ConfigBundle,
) error {
	if target.Snapshot.TargetSnapshotID != input.TargetID ||
		string(target.Snapshot.Mode) != string(run.TargetMode) ||
		target.Snapshot.Base.CommitOID != run.BaseRevision ||
		target.Snapshot.Head.CommitOID != run.HeadRevision {
		return fmt.Errorf("materialized target identity does not match run and frozen input")
	}
	if !reflect.DeepEqual(target.Contexts, input.Contexts) {
		return fmt.Errorf("materialized contexts do not exactly match frozen review input")
	}
	for _, binding := range target.Contexts {
		if binding.Ref == nil {
			continue
		}
		ref := runmodel.ArtifactRef{
			URI:       binding.Ref.ArtifactURI,
			SHA256:    binding.Ref.Digest,
			SizeBytes: binding.Ref.SizeBytes,
			Contract:  binding.Ref.Contract,
		}
		if _, err := repository.ReadArtifact(ref); err != nil {
			return fmt.Errorf("read frozen context %q: %w", binding.Ref.ContextID, err)
		}
	}
	if spec.Repository.Provider != "local-git" ||
		target.Snapshot.Repository.Kind != "local_git" ||
		spec.Repository.RepositoryID != target.Snapshot.Repository.RepositoryID {
		return fmt.Errorf("materialized target repository does not match review spec")
	}
	if !containsString(config.Target.AllowedModes, string(target.Snapshot.Mode)) {
		return fmt.Errorf("materialized target mode is denied by frozen config")
	}
	if len(target.FileRefs) > config.Target.MaxFiles {
		return fmt.Errorf("materialized target exceeds frozen config max_files")
	}

	manifestData, err := repository.ReadArtifact(target.ManifestRef)
	if err != nil {
		return fmt.Errorf("materialized target manifest: %w", err)
	}
	switch target.Snapshot.Mode {
	case reviewcore.TargetModeDiff:
		var manifest gitadapter.ChangeManifest
		if err := decodeStrictJSON(manifestData, &manifest); err != nil {
			return fmt.Errorf("decode change manifest: %w", err)
		}
		if err := targetmodel.ValidateDiffManifest(manifest, target.Snapshot); err != nil {
			return fmt.Errorf("validate change manifest: %w", err)
		}
		if len(manifest.Files) != len(target.FileRefs) {
			return fmt.Errorf("change manifest does not exactly cover materialized file refs")
		}
		changes := make(map[string]gitadapter.FileChange, len(manifest.Files))
		for _, change := range manifest.Files {
			changes[change.Path] = change
		}
		for _, file := range target.FileRefs {
			change, exists := changes[file.Path]
			if !exists || change.Status != file.Status {
				return fmt.Errorf("materialized file_ref %q is not bound to change manifest", file.Path)
			}
		}
		patch, err := repository.ReadArtifact(*target.PatchRef)
		if err != nil {
			return fmt.Errorf("read materialized canonical patch: %w", err)
		}
		inspection, err := reviewcore.InspectCanonicalPatch(context.Background(), string(patch))
		if err != nil {
			return fmt.Errorf("inspect materialized canonical patch: %w", err)
		}
		if len(inspection.Files) != len(manifest.Files) ||
			len(inspection.Files) != manifest.Coverage.IncludedFiles ||
			inspection.TotalHunks != manifest.Coverage.IncludedHunks {
			return fmt.Errorf("canonical patch coverage does not exactly match change manifest")
		}
		for index, change := range manifest.Files {
			patchFile := inspection.Files[index]
			if !patchFile.PathKnown || patchFile.Path != change.Path ||
				patchFile.Hunks != change.HunkCount || !change.Included {
				return fmt.Errorf(
					"canonical patch file %d does not exactly match change manifest",
					index,
				)
			}
			allowed, err := gitadapter.ScopeIncludesPath(
				change.Path,
				config.Target.Include,
				config.Target.Exclude,
			)
			if err != nil {
				return fmt.Errorf("evaluate diff target policy for %q: %w", change.Path, err)
			}
			if !allowed {
				return fmt.Errorf("diff path %q is denied by frozen config", change.Path)
			}
		}
	case reviewcore.TargetModeSelection:
		var manifest targetmodel.SelectionManifest
		if err := decodeStrictJSON(manifestData, &manifest); err != nil {
			return fmt.Errorf("decode selection manifest: %w", err)
		}
		if err := manifest.ValidateAgainst(target.Snapshot); err != nil {
			return fmt.Errorf("validate selection manifest: %w", err)
		}
		allowed, err := gitadapter.ScopeIncludesPath(
			manifest.Path,
			config.Target.Include,
			config.Target.Exclude,
		)
		if err != nil {
			return fmt.Errorf("evaluate selection target policy: %w", err)
		}
		if !allowed {
			return fmt.Errorf("selection path %q is denied by frozen config", manifest.Path)
		}
	case reviewcore.TargetModeScope:
		var manifest targetmodel.ScopeManifest
		if err := decodeStrictJSON(manifestData, &manifest); err != nil {
			return fmt.Errorf("decode scope manifest: %w", err)
		}
		if err := manifest.ValidateAgainst(target.Snapshot); err != nil {
			return fmt.Errorf("validate scope manifest: %w", err)
		}
		if len(manifest.Files) != len(target.FileRefs) {
			return fmt.Errorf("scope manifest does not exactly cover materialized file refs")
		}
		for index, file := range target.FileRefs {
			manifestFile := manifest.Files[index]
			authorized, err := gitadapter.ScopeIncludesPath(
				manifestFile.Path,
				manifest.Include,
				manifest.Exclude,
			)
			if err != nil {
				return fmt.Errorf("validate scope path %q: %w", manifestFile.Path, err)
			}
			if !authorized {
				return fmt.Errorf("scope manifest file %q is outside authorized patterns", manifestFile.Path)
			}
			policyAllowed, err := gitadapter.ScopeIncludesPath(
				manifestFile.Path,
				config.Target.Include,
				config.Target.Exclude,
			)
			if err != nil {
				return fmt.Errorf("evaluate scope target policy for %q: %w", manifestFile.Path, err)
			}
			if !policyAllowed {
				if file.ContentRef != nil ||
					file.Completeness != gitadapter.CompletenessSkipped ||
					!targetReasonExists(
						file.Reasons,
						gitadapter.ReasonTargetPolicyExcluded,
					) {
					return fmt.Errorf(
						"scope path %q bypasses frozen config target policy",
						manifestFile.Path,
					)
				}
			}
			if manifestFile.Path != file.Path ||
				manifestFile.SHA256 != file.SHA256 ||
				manifestFile.SizeBytes != file.SizeBytes ||
				manifestFile.Completeness != file.Completeness ||
				!reflect.DeepEqual(manifestFile.Reasons, file.Reasons) {
				return fmt.Errorf("scope file_ref %q does not match scope manifest", file.Path)
			}
		}
	default:
		return fmt.Errorf("unsupported materialized target mode %q", target.Snapshot.Mode)
	}

	targetFiles := make(map[string]targetmodel.TargetFileRef, len(target.FileRefs))
	expectedInputFiles := 0
	for _, file := range target.FileRefs {
		targetFiles[file.Path] = file
		switch target.Snapshot.Mode {
		case reviewcore.TargetModeDiff:
			if file.SHA256 != "" {
				expectedInputFiles++
			}
		case reviewcore.TargetModeSelection, reviewcore.TargetModeScope:
			if file.ContentRef != nil {
				expectedInputFiles++
			}
		}
	}
	if len(input.Files) != expectedInputFiles {
		return fmt.Errorf("frozen review input does not exactly cover materialized file identities")
	}
	inputFiles := make(map[string]reviewcore.FileManifestEntry, len(input.Files))
	for _, file := range input.Files {
		targetFile, exists := targetFiles[file.Path]
		if !exists || targetFile.SHA256 != file.SHA256 ||
			targetFile.SizeBytes != file.SizeBytes {
			return fmt.Errorf("frozen review input file %q is not bound to materialized target", file.Path)
		}
		if file.Content == nil {
			if targetFile.ContentRef != nil {
				return fmt.Errorf("frozen review input omits retained content for %q", file.Path)
			}
		} else {
			if targetFile.ContentRef == nil {
				return fmt.Errorf("frozen review input contains unretained content for %q", file.Path)
			}
			content, err := repository.ReadArtifact(*targetFile.ContentRef)
			if err != nil {
				return fmt.Errorf("materialized file content %q: %w", file.Path, err)
			}
			if !bytes.Equal(content, []byte(*file.Content)) {
				return fmt.Errorf("frozen review input content differs for %q", file.Path)
			}
		}
		inputFiles[file.Path] = file
	}
	for _, file := range target.FileRefs {
		if file.ContentRef == nil {
			continue
		}
		inputFile, exists := inputFiles[file.Path]
		if !exists || inputFile.Content == nil {
			return fmt.Errorf("retained materialized content %q is absent from frozen input", file.Path)
		}
	}
	if target.Snapshot.Mode == reviewcore.TargetModeScope {
		regions := make(map[string]reviewcore.ReviewRegion, len(input.Regions))
		for _, region := range input.Regions {
			if _, duplicate := regions[region.Path]; duplicate {
				return fmt.Errorf("scope frozen input has multiple regions for %q", region.Path)
			}
			regions[region.Path] = region
		}
		expectedRegions := 0
		for _, file := range input.Files {
			if file.Content == nil {
				continue
			}
			lines := frozenLineCount(*file.Content)
			region, exists := regions[file.Path]
			if lines == 0 {
				if exists {
					return fmt.Errorf("scope frozen input authorizes lines in empty file %q", file.Path)
				}
				continue
			}
			expectedRegions++
			if !exists || region.StartLine != 1 || uint64(region.EndLine) != uint64(lines) ||
				region.SHA256 != file.SHA256 {
				return fmt.Errorf("scope frozen input does not fully authorize retained file %q", file.Path)
			}
		}
		if len(input.Regions) != expectedRegions {
			return fmt.Errorf("scope frozen input regions do not exactly cover retained files")
		}
	}
	materializedBytes := int64(len(input.CanonicalPatch))
	for _, file := range input.Files {
		if file.Content != nil {
			materializedBytes += int64(len(*file.Content))
		}
	}
	for _, binding := range input.Contexts {
		if binding.Ref == nil {
			continue
		}
		if materializedBytes > config.Budget.MaxInputBytes ||
			binding.Ref.SizeBytes > config.Budget.MaxInputBytes-materializedBytes {
			return fmt.Errorf(
				"frozen review input and contexts exceed config max_input_bytes=%d",
				config.Budget.MaxInputBytes,
			)
		}
		materializedBytes += binding.Ref.SizeBytes
	}
	if materializedBytes > config.Budget.MaxInputBytes {
		return fmt.Errorf(
			"frozen review input uses %d bytes, exceeds config max_input_bytes=%d",
			materializedBytes,
			config.Budget.MaxInputBytes,
		)
	}
	return nil
}

func targetEvidenceCompleteness(
	target targetmodel.MaterializedTarget,
) (runmodel.Completeness, []string) {
	if target.Snapshot.Completeness == gitadapter.CompletenessComplete {
		return runmodel.CompletenessComplete, []string{}
	}
	notes := make([]string, 0, len(target.Snapshot.CompletenessReason))
	for _, reason := range target.Snapshot.CompletenessReason {
		value := string(reason.Code)
		if reason.Detail != "" {
			value += ": " + reason.Detail
		}
		notes = append(notes, value)
	}
	return runmodel.CompletenessPartial, notes
}

func targetReasonExists(
	reasons []gitadapter.Reason,
	code gitadapter.ReasonCode,
) bool {
	for _, reason := range reasons {
		if reason.Code == code {
			return true
		}
	}
	return false
}

func frozenLineCount(content string) int {
	if content == "" {
		return 0
	}
	lines := strings.Count(content, "\n")
	if !strings.HasSuffix(content, "\n") {
		lines++
	}
	return lines
}

func selectedRegionContent(
	input reviewcore.ReviewInput,
	region reviewcore.ReviewRegion,
) ([]byte, error) {
	for _, file := range input.Files {
		if file.Path != region.Path || file.SHA256 != region.SHA256 || file.Content == nil {
			continue
		}
		if *file.Content == "" {
			return nil, fmt.Errorf("selection region exceeds empty frozen file content")
		}
		lines := strings.Split(*file.Content, "\n")
		if strings.HasSuffix(*file.Content, "\n") {
			lines = lines[:len(lines)-1]
		}
		if region.StartLine == 0 || region.EndLine < region.StartLine ||
			uint64(region.EndLine) > uint64(len(lines)) {
			return nil, fmt.Errorf("selection region exceeds frozen file content")
		}
		return []byte(strings.Join(lines[region.StartLine-1:region.EndLine], "\n") + "\n"), nil
	}
	return nil, fmt.Errorf("selection region has no exact frozen file")
}

func selectedRegionsContent(
	input reviewcore.ReviewInput,
	regions []reviewcore.ReviewRegion,
) ([]byte, error) {
	var selected bytes.Buffer
	for _, region := range regions {
		fragment, err := selectedRegionContent(input, region)
		if err != nil {
			return nil, err
		}
		selected.Write(fragment)
	}
	return selected.Bytes(), nil
}

func selectionSelectorMatchesSnapshot(
	selection contractsv1alpha1.SelectionTarget,
	snapshot targetmodel.SelectionSnapshot,
) bool {
	switch {
	case selection.StartLine != 0 || selection.EndLine != 0:
		return snapshot.StartLine == selection.StartLine &&
			snapshot.EndLine == selection.EndLine &&
			snapshot.Symbol == nil &&
			len(snapshot.EffectiveRanges) == 1 &&
			snapshot.EffectiveRanges[0] == (targetmodel.SelectionRange{
				StartLine: selection.StartLine,
				EndLine:   selection.EndLine,
			})
	case selection.Symbol != nil:
		return snapshot.StartLine == 0 &&
			snapshot.EndLine == 0 &&
			snapshot.Symbol != nil &&
			snapshot.Symbol.Language == selection.Symbol.Language &&
			snapshot.Symbol.Kind == selection.Symbol.Kind &&
			snapshot.Symbol.QualifiedName == selection.Symbol.QualifiedName
	default:
		if snapshot.StartLine != 0 || snapshot.EndLine != 0 || snapshot.Symbol != nil ||
			len(selection.Ranges) != len(snapshot.EffectiveRanges) {
			return false
		}
		for index, lineRange := range selection.Ranges {
			if snapshot.EffectiveRanges[index] != (targetmodel.SelectionRange{
				StartLine: lineRange.StartLine,
				EndLine:   lineRange.EndLine,
			}) {
				return false
			}
		}
		return true
	}
}

func selectionRegionsMatchSnapshot(
	regions []reviewcore.ReviewRegion,
	snapshot targetmodel.SelectionSnapshot,
) bool {
	if len(regions) != len(snapshot.EffectiveRanges) {
		return false
	}
	for index, lineRange := range snapshot.EffectiveRanges {
		region := regions[index]
		if region.Path != snapshot.Path ||
			region.StartLine != lineRange.StartLine ||
			region.EndLine != lineRange.EndLine ||
			region.SHA256 != snapshot.FileSHA256 {
			return false
		}
	}
	return true
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func (repository *Repository) verifyLedgerProjection(run runmodel.ReviewRun) error {
	events, err := repository.readRunEvents(run.RunID)
	if err != nil {
		return fmt.Errorf("read run ledger: %w", err)
	}
	ledgerBindings := make(map[string]runmodel.PlatformExecutionBinding)
	ledgerEvidence := make(map[string]runmodel.RunEvidence)
	for _, persisted := range events {
		event := persisted.Event
		if event.EventType == EventBindingRecorded {
			binding := *event.Binding
			if _, duplicate := ledgerBindings[binding.BindingID]; duplicate {
				return fmt.Errorf("ledger contains duplicate binding fact %q", binding.BindingID)
			}
			ledgerBindings[binding.BindingID] = binding
		}
		if event.EventType == EventEvidenceRecorded {
			evidence := *event.Evidence
			if _, duplicate := ledgerEvidence[evidence.EvidenceID]; duplicate {
				return fmt.Errorf("ledger contains duplicate evidence fact %q", evidence.EvidenceID)
			}
			ledgerEvidence[evidence.EvidenceID] = evidence
		}
	}
	if len(ledgerBindings) != len(run.Bindings) {
		return fmt.Errorf("ledger binding facts do not exactly cover final run bindings")
	}
	for index, binding := range run.Bindings {
		persisted, exists := ledgerBindings[binding.BindingID]
		if !exists || !reflect.DeepEqual(persisted, binding) {
			return fmt.Errorf("platform_execution_bindings[%d] does not exactly match ledger", index)
		}
	}
	if len(ledgerEvidence) != len(run.Evidence) {
		return fmt.Errorf("ledger evidence facts do not exactly cover final run evidence")
	}
	for index, evidence := range run.Evidence {
		persisted, exists := ledgerEvidence[evidence.EvidenceID]
		if !exists || !reflect.DeepEqual(persisted, evidence) {
			return fmt.Errorf("run_evidence[%d] does not exactly match ledger", index)
		}
	}
	return nil
}

type persistedRunEvent struct {
	Envelope local.Envelope
	Event    RunEvent
}

func (repository *Repository) readRunEvents(runID string) ([]persistedRunEvent, error) {
	envelopes, err := repository.store.ReadJSONL("run-index")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []persistedRunEvent{}, nil
		}
		return nil, err
	}
	events := make([]persistedRunEvent, 0)
	for _, envelope := range envelopes {
		event, err := decodeRunEvent(envelope)
		if err != nil {
			return nil, err
		}
		if event.RunID == runID {
			events = append(events, persistedRunEvent{Envelope: envelope, Event: event})
		}
	}
	return events, nil
}

func decodeRunEvent(envelope local.Envelope) (RunEvent, error) {
	if envelope.Schema != RunEventSchemaVersion {
		return RunEvent{}, fmt.Errorf("unsupported run event schema %q", envelope.Schema)
	}
	var event RunEvent
	if err := decodeStrictJSON(envelope.Payload, &event); err != nil {
		return RunEvent{}, fmt.Errorf("decode run event %q: %w", envelope.ID, err)
	}
	if !strings.HasPrefix(envelope.ID, event.RunID+"-") {
		return RunEvent{}, fmt.Errorf("run event %q is not namespaced by run %q",
			envelope.ID, event.RunID)
	}
	if isTerminalStatus(event.Status) && envelope.ID != TerminalEventID(event.RunID) {
		return RunEvent{}, fmt.Errorf("terminal event %q has non-canonical id", envelope.ID)
	}
	if err := event.Validate(); err != nil {
		return RunEvent{}, fmt.Errorf("validate run event %q: %w", envelope.ID, err)
	}
	return event, nil
}

func firstTerminalEvent(
	events []persistedRunEvent,
) (persistedRunEvent, bool, error) {
	var terminal persistedRunEvent
	found := false
	for _, event := range events {
		if !isTerminalStatus(event.Event.Status) {
			continue
		}
		if found {
			return persistedRunEvent{}, false, fmt.Errorf(
				"run %q contains multiple terminal events", event.Event.RunID,
			)
		}
		terminal = event
		found = true
	}
	return terminal, found, nil
}

func (event RunEvent) Validate() error {
	if event.RunID == "" || event.EventType == "" {
		return fmt.Errorf("run event identity and type are required")
	}
	switch event.Kind {
	case runmodel.RunKindReview, runmodel.RunKindReplay:
	default:
		return fmt.Errorf("unsupported run event kind %q", event.Kind)
	}
	switch event.EventType {
	case EventRunCreated:
		if event.Status != runmodel.RunStatusPending {
			return fmt.Errorf("%s requires pending status", event.EventType)
		}
	case EventRunStarted, EventStageStarted, EventStageSucceeded, EventStageFailed,
		EventStageCanceled, EventBindingRecorded, EventEvidenceRecorded:
		if event.Status != runmodel.RunStatusRunning {
			return fmt.Errorf("%s requires running status", event.EventType)
		}
	case EventRunSucceeded:
		if event.Status != runmodel.RunStatusSucceeded {
			return fmt.Errorf("%s requires succeeded status", event.EventType)
		}
	case EventRunFailed:
		if event.Status != runmodel.RunStatusFailed {
			return fmt.Errorf("%s requires failed status", event.EventType)
		}
	case EventRunCanceled:
		if event.Status != runmodel.RunStatusCanceled {
			return fmt.Errorf("%s requires canceled status", event.EventType)
		}
	default:
		return fmt.Errorf("unsupported run event type %q", event.EventType)
	}

	terminal := isTerminalStatus(event.Status)
	if terminal {
		if event.FinalRunRef == nil {
			return fmt.Errorf("terminal run event requires final_run_ref")
		}
		if err := event.FinalRunRef.Validate(); err != nil {
			return fmt.Errorf("final_run_ref: %w", err)
		}
		if event.FinalRunRef.Contract != runmodel.ContractReviewRun {
			return fmt.Errorf("final_run_ref contract is %q, want %q",
				event.FinalRunRef.Contract, runmodel.ContractReviewRun)
		}
		if event.ExecutionSnapshotID == "" {
			return fmt.Errorf("terminal run event requires execution_snapshot_id")
		}
	} else if event.FinalRunRef != nil {
		return fmt.Errorf("non-terminal run event cannot contain final_run_ref")
	}
	if event.Artifact != nil {
		if err := event.Artifact.Validate(); err != nil {
			return fmt.Errorf("artifact_ref: %w", err)
		}
	}
	if event.Binding != nil {
		if err := event.Binding.Validate(); err != nil {
			return fmt.Errorf("platform_execution_binding: %w", err)
		}
		if event.Binding.RunID != event.RunID {
			return fmt.Errorf("platform execution binding belongs to another run")
		}
		if !eventStageMatches(event.StageID, event.Attempt, event.Generation,
			event.Binding.StageID, event.Binding.Attempt, event.Binding.Generation) {
			return fmt.Errorf("run event does not match platform execution binding")
		}
	}
	if event.Evidence != nil {
		if err := event.Evidence.Validate(); err != nil {
			return fmt.Errorf("run_evidence: %w", err)
		}
		if !eventStageMatches(event.StageID, event.Attempt, event.Generation,
			event.Evidence.StageID, event.Evidence.Attempt, event.Evidence.Generation) {
			return fmt.Errorf("run event does not match evidence")
		}
	}
	if event.EventType == EventBindingRecorded && event.Binding == nil {
		return fmt.Errorf("binding.recorded requires platform_execution_binding")
	}
	if event.EventType == EventEvidenceRecorded && event.Evidence == nil {
		return fmt.Errorf("evidence.recorded requires run_evidence")
	}
	if strings.HasPrefix(event.EventType, "stage.") {
		if event.StageID == "" || event.Attempt < 1 || event.Generation < 1 {
			return fmt.Errorf("stage event requires stage_id, attempt, and generation")
		}
	}
	if event.Status == runmodel.RunStatusFailed && event.Failure == nil {
		return fmt.Errorf("failed run event requires failure")
	}
	if event.Status == runmodel.RunStatusSucceeded && event.Failure != nil {
		return fmt.Errorf("succeeded run event cannot contain failure")
	}
	return nil
}

func eventStageMatches(
	eventStage string,
	eventAttempt int,
	eventGeneration int,
	factStage string,
	factAttempt int,
	factGeneration int,
) bool {
	return (eventStage == "" || eventStage == factStage) &&
		(eventAttempt == 0 || eventAttempt == factAttempt) &&
		(eventGeneration == 0 || eventGeneration == factGeneration)
}

func isTerminalStatus(status runmodel.RunStatus) bool {
	switch status {
	case runmodel.RunStatusSucceeded, runmodel.RunStatusFailed, runmodel.RunStatusCanceled:
		return true
	default:
		return false
	}
}

func terminalEventType(status runmodel.RunStatus) string {
	switch status {
	case runmodel.RunStatusSucceeded:
		return EventRunSucceeded
	case runmodel.RunStatusFailed:
		return EventRunFailed
	case runmodel.RunStatusCanceled:
		return EventRunCanceled
	default:
		return ""
	}
}
