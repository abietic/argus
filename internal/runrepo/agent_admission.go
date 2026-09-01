package runrepo

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/store/local"
)

var (
	// ErrAgentStageAdmissionConflict is exported at the repository boundary so
	// callers do not need to depend on the local-store adapter to recognize a
	// concurrent winner.
	ErrAgentStageAdmissionConflict        = local.ErrEventConflict
	ErrAgentStageAdmissionSubjectMismatch = errors.New(
		"agent-stage admission belongs to another planning subject",
	)
)

// ExecutionSnapshotForRun resolves the snapshot exclusively through the
// authoritative run.created lineage. Caller-supplied snapshot IDs and refs
// are intentionally absent from this API.
func (repository *Repository) ExecutionSnapshotForRun(
	runID string,
) (runmodel.ExecutionSnapshot, error) {
	if _, err := runmodel.AgentStagePlanAdmissionID(runID, "namespace-check"); err != nil {
		return runmodel.ExecutionSnapshot{}, err
	}
	events, err := repository.readRunEvents(runID)
	if err != nil {
		return runmodel.ExecutionSnapshot{}, err
	}
	if len(events) == 0 {
		return runmodel.ExecutionSnapshot{}, fmt.Errorf(
			"run %q has no authoritative events: %w",
			runID,
			os.ErrNotExist,
		)
	}
	createdIndex := -1
	var created RunEvent
	for index, persisted := range events {
		if persisted.Event.EventType != EventRunCreated {
			continue
		}
		if createdIndex >= 0 {
			return runmodel.ExecutionSnapshot{}, fmt.Errorf(
				"run %q contains multiple run.created events",
				runID,
			)
		}
		createdIndex = index
		created = persisted.Event
	}
	if createdIndex < 0 {
		return runmodel.ExecutionSnapshot{}, fmt.Errorf(
			"run %q has no authoritative run.created event: %w",
			runID, os.ErrNotExist,
		)
	}
	if createdIndex != 0 {
		return runmodel.ExecutionSnapshot{}, fmt.Errorf(
			"run %q run.created event is not the first run fact",
			runID,
		)
	}
	if created.ExecutionSnapshotID == "" {
		return runmodel.ExecutionSnapshot{}, fmt.Errorf(
			"run %q run.created event has no execution_snapshot_id: %w",
			runID, os.ErrNotExist,
		)
	}
	for _, persisted := range events {
		event := persisted.Event
		if event.Kind != created.Kind {
			return runmodel.ExecutionSnapshot{}, fmt.Errorf(
				"run %q kind drifted after run.created",
				runID,
			)
		}
		if event.ExecutionSnapshotID != "" &&
			event.ExecutionSnapshotID != created.ExecutionSnapshotID {
			return runmodel.ExecutionSnapshot{}, fmt.Errorf(
				"run %q execution snapshot lineage drifted from %q to %q",
				runID,
				created.ExecutionSnapshotID,
				event.ExecutionSnapshotID,
			)
		}
	}
	snapshot, err := repository.LoadExecutionSnapshot(created.ExecutionSnapshotID)
	if err != nil {
		return runmodel.ExecutionSnapshot{}, fmt.Errorf(
			"load run %q authoritative execution snapshot: %w",
			runID,
			err,
		)
	}
	if snapshot.ExecutionSnapshotID != created.ExecutionSnapshotID {
		return runmodel.ExecutionSnapshot{}, fmt.Errorf(
			"run %q loaded execution snapshot does not match run.created",
			runID,
		)
	}
	return snapshot, nil
}

// AppendAgentStagePlanAdmission commits the sole authoritative formal plan for
// a run+stage coordinate. The local store supplies cross-Repository-instance
// locking: an exact retry is idempotent, while a changed timestamp, subject,
// projection, or plan returns an error wrapping local.ErrEventConflict.
func (repository *Repository) AppendAgentStagePlanAdmission(
	admission runmodel.AgentStagePlanAdmission,
) error {
	if err := admission.Validate(); err != nil {
		return fmt.Errorf("validate AgentStagePlanAdmission: %w", err)
	}
	stream, err := agentStageAdmissionStream(admission.ReviewRunID)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(admission.AdmissionID, admission.ReviewRunID+"-") {
		return fmt.Errorf(
			"agent-stage admission %q is not namespaced by run %q",
			admission.AdmissionID,
			admission.ReviewRunID,
		)
	}
	// Fail closed on pre-existing corrupt or cross-run facts before extending
	// the stream. AppendJSONL performs the authoritative duplicate-ID check
	// while holding its cross-instance file lock.
	if _, err := repository.readAgentStagePlanAdmissions(admission.ReviewRunID); err != nil {
		return err
	}
	envelope, err := repository.store.AppendJSONL(stream, local.Event{
		ID:      admission.AdmissionID,
		Schema:  runmodel.AgentStagePlanAdmissionSchemaVersion,
		Time:    admission.RecordedAt,
		Payload: admission,
	})
	if err != nil {
		return fmt.Errorf("append authoritative agent-stage admission: %w", err)
	}
	persisted, err := decodeAgentStagePlanAdmission(envelope, admission.ReviewRunID)
	if err != nil {
		return fmt.Errorf("verify appended agent-stage admission: %w", err)
	}
	if persisted != admission {
		return fmt.Errorf("appended agent-stage admission differs from request")
	}
	return nil
}

// LookupAgentStagePlanAdmission returns the exact admitted plan for one
// run+stage only when the complete host-authenticated subject matches. A
// missing admission returns (zero, false, nil); authorization and corruption
// failures remain errors.
func (repository *Repository) LookupAgentStagePlanAdmission(
	runID string,
	stageID string,
	subject runmodel.AgentPlanningSubject,
) (runmodel.AgentStagePlanAdmission, bool, error) {
	if err := subject.Validate(); err != nil {
		return runmodel.AgentStagePlanAdmission{}, false, fmt.Errorf(
			"validate planning subject: %w",
			err,
		)
	}
	admissionID, err := runmodel.AgentStagePlanAdmissionID(runID, stageID)
	if err != nil {
		return runmodel.AgentStagePlanAdmission{}, false, err
	}
	admission, err := repository.LoadAgentStagePlanAdmission(
		runID,
		admissionID,
		subject,
	)
	if errors.Is(err, os.ErrNotExist) {
		return runmodel.AgentStagePlanAdmission{}, false, nil
	}
	if err != nil {
		return runmodel.AgentStagePlanAdmission{}, false, err
	}
	if admission.Stage.ID != stageID {
		return runmodel.AgentStagePlanAdmission{}, false, fmt.Errorf(
			"agent-stage admission ID resolved a different stage",
		)
	}
	return admission, true, nil
}

// LoadAgentStagePlanAdmission loads one exact admission ID from the run-scoped
// stream and revalidates every fact in that stream before returning it.
func (repository *Repository) LoadAgentStagePlanAdmission(
	runID string,
	admissionID string,
	subject runmodel.AgentPlanningSubject,
) (runmodel.AgentStagePlanAdmission, error) {
	if err := subject.Validate(); err != nil {
		return runmodel.AgentStagePlanAdmission{}, fmt.Errorf(
			"validate planning subject: %w",
			err,
		)
	}
	if !strings.HasPrefix(admissionID, runID+"-") {
		return runmodel.AgentStagePlanAdmission{}, fmt.Errorf(
			"agent-stage admission %q is not namespaced by run %q",
			admissionID,
			runID,
		)
	}
	admissions, err := repository.readAgentStagePlanAdmissions(runID)
	if err != nil {
		return runmodel.AgentStagePlanAdmission{}, err
	}
	for _, admission := range admissions {
		if admission.AdmissionID != admissionID {
			continue
		}
		if admission.Subject != subject {
			return runmodel.AgentStagePlanAdmission{}, fmt.Errorf(
				"%w: run %q stage %q",
				ErrAgentStageAdmissionSubjectMismatch,
				admission.ReviewRunID,
				admission.Stage.ID,
			)
		}
		return admission, nil
	}
	return runmodel.AgentStagePlanAdmission{}, fmt.Errorf(
		"agent-stage admission %q in run %q: %w",
		admissionID,
		runID,
		os.ErrNotExist,
	)
}

func (repository *Repository) readAgentStagePlanAdmissions(
	runID string,
) ([]runmodel.AgentStagePlanAdmission, error) {
	stream, err := agentStageAdmissionStream(runID)
	if err != nil {
		return nil, err
	}
	envelopes, err := repository.store.ReadJSONL(stream)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []runmodel.AgentStagePlanAdmission{}, nil
		}
		return nil, fmt.Errorf("read agent-stage admission stream: %w", err)
	}
	admissions := make([]runmodel.AgentStagePlanAdmission, 0, len(envelopes))
	for _, envelope := range envelopes {
		admission, err := decodeAgentStagePlanAdmission(envelope, runID)
		if err != nil {
			return nil, err
		}
		admissions = append(admissions, admission)
	}
	return admissions, nil
}

func decodeAgentStagePlanAdmission(
	envelope local.Envelope,
	runID string,
) (runmodel.AgentStagePlanAdmission, error) {
	if envelope.Schema != runmodel.AgentStagePlanAdmissionSchemaVersion {
		return runmodel.AgentStagePlanAdmission{}, fmt.Errorf(
			"unsupported agent-stage admission event schema %q",
			envelope.Schema,
		)
	}
	var admission runmodel.AgentStagePlanAdmission
	if err := decodeStrictJSON(envelope.Payload, &admission); err != nil {
		return runmodel.AgentStagePlanAdmission{}, fmt.Errorf(
			"decode agent-stage admission %q: %w",
			envelope.ID,
			err,
		)
	}
	if admission.ReviewRunID != runID ||
		!strings.HasPrefix(envelope.ID, runID+"-") {
		return runmodel.AgentStagePlanAdmission{}, fmt.Errorf(
			"agent-stage admission event %q is not namespaced by run %q",
			envelope.ID,
			runID,
		)
	}
	if envelope.ID != admission.AdmissionID {
		return runmodel.AgentStagePlanAdmission{}, fmt.Errorf(
			"agent-stage admission event ID does not match its payload",
		)
	}
	if !envelope.Time.Equal(admission.RecordedAt) {
		return runmodel.AgentStagePlanAdmission{}, fmt.Errorf(
			"agent-stage admission event time does not match recorded_at",
		)
	}
	if err := admission.Validate(); err != nil {
		return runmodel.AgentStagePlanAdmission{}, fmt.Errorf(
			"validate agent-stage admission %q: %w",
			envelope.ID,
			err,
		)
	}
	return admission, nil
}

func agentStageAdmissionStream(runID string) (string, error) {
	if _, err := runmodel.AgentStagePlanAdmissionID(runID, "namespace-check"); err != nil {
		return "", err
	}
	return "runs/" + runID + "/agent-stage-plan-admissions", nil
}
