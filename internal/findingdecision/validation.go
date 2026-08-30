package findingdecision

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"argus.local/argus/internal/runmodel"
)

func (request Request) Validate() error {
	if request.SchemaVersion != RequestSchemaVersion {
		return fmt.Errorf("unsupported finding decision request schema %q", request.SchemaVersion)
	}
	if err := validateID("run_id", request.RunID); err != nil {
		return err
	}
	if err := validateID("finding_id", request.FindingID); err != nil {
		return err
	}
	if err := validateAction(request.Action); err != nil {
		return err
	}
	if err := validateID("reason_code", request.ReasonCode); err != nil {
		return err
	}
	if err := validateEvidenceRefs(request.EvidenceRefs); err != nil {
		return err
	}
	return validateUTC("occurred_at", request.OccurredAt)
}

func (mutation Mutation) Validate() error {
	if mutation.SchemaVersion != MutationSchemaVersion {
		return fmt.Errorf("unsupported finding decision mutation schema %q", mutation.SchemaVersion)
	}
	if err := validateID("idempotency_key", mutation.IdempotencyKey); err != nil {
		return err
	}
	if err := mutation.Actor.Validate(); err != nil {
		return fmt.Errorf("actor: %w", err)
	}
	if len(mutation.Roles) == 0 || !slices.IsSorted(mutation.Roles) {
		return fmt.Errorf("roles must be a non-empty canonical sorted array")
	}
	for index, role := range mutation.Roles {
		switch role {
		case RoleFindingReviewer, RolePublicationApprover:
		default:
			return fmt.Errorf("unsupported role %q", role)
		}
		if index > 0 && role == mutation.Roles[index-1] {
			return fmt.Errorf("roles contains duplicates")
		}
	}
	if err := validateText("audit", mutation.Audit, 4096); err != nil {
		return err
	}
	return validateUTC("at", mutation.At)
}

func (actor Actor) Validate() error {
	switch actor.Kind {
	case ActorHuman, ActorService:
	default:
		return fmt.Errorf("unsupported actor kind %q", actor.Kind)
	}
	return validateID("actor.id", actor.ID)
}

func (root Root) Validate() error {
	if root.SchemaVersion != RootSchemaVersion {
		return fmt.Errorf("unsupported finding decision root schema %q", root.SchemaVersion)
	}
	for name, value := range map[string]string{
		"run_id": root.RunID, "finding_id": root.FindingID,
		"initial_decision_id": root.InitialDecisionID,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	switch root.SourceContract {
	case runmodel.ContractFindingSet, runmodel.ContractGovernedReviewReport:
	default:
		return fmt.Errorf("unsupported finding decision source contract %q", root.SourceContract)
	}
	return validateSHA256("source_sha256", root.SourceSHA256)
}

func (decision Decision) Validate() error {
	if decision.SchemaVersion != DecisionSchemaVersion {
		return fmt.Errorf("unsupported finding decision schema %q", decision.SchemaVersion)
	}
	root := decision.Root()
	if err := root.Validate(); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"decision_id": decision.DecisionID, "prior_decision_id": decision.PriorDecisionID,
		"reason_code": decision.ReasonCode, "idempotency_key": decision.IdempotencyKey,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if decision.Sequence < 2 {
		return fmt.Errorf("post-initial decision sequence must start at 2")
	}
	if err := validateAction(decision.Action); err != nil {
		return err
	}
	if err := validateEvidenceRefs(decision.EvidenceRefs); err != nil {
		return err
	}
	if err := validateUTC("occurred_at", decision.OccurredAt); err != nil {
		return err
	}
	if err := validateUTC("recorded_at", decision.RecordedAt); err != nil {
		return err
	}
	if decision.RecordedAt.Before(decision.OccurredAt) {
		return fmt.Errorf("recorded_at must not precede occurred_at")
	}
	if err := decision.RecordedBy.Validate(); err != nil {
		return fmt.Errorf("recorded_by: %w", err)
	}
	mutation := Mutation{
		SchemaVersion: MutationSchemaVersion, IdempotencyKey: decision.IdempotencyKey,
		Actor: decision.RecordedBy, Roles: decision.Roles, Audit: decision.Audit,
		At: decision.RecordedAt,
	}
	if err := mutation.Validate(); err != nil {
		return err
	}
	if err := authorize(decision.Action, decision.Roles); err != nil {
		return err
	}
	want, err := decisionID(decision)
	if err != nil {
		return err
	}
	if decision.DecisionID != want {
		return fmt.Errorf("decision_id does not match canonical decision facts")
	}
	return nil
}

func (decision Decision) Root() Root {
	return Root{
		SchemaVersion: RootSchemaVersion, RunID: decision.RunID, FindingID: decision.FindingID,
		InitialDecisionID: decision.InitialDecisionID, SourceContract: decision.SourceContract,
		SourceSHA256: decision.SourceSHA256,
	}
}

func authorize(action Action, roles []Role) error {
	if action == ActionPublish {
		if slices.Contains(roles, RolePublicationApprover) {
			return nil
		}
		return fmt.Errorf("publication_approver role is required for publish")
	}
	if slices.Contains(roles, RoleFindingReviewer) ||
		slices.Contains(roles, RolePublicationApprover) {
		return nil
	}
	return fmt.Errorf("finding reviewer role is required")
}

func decisionID(decision Decision) (string, error) {
	copy := decision
	copy.DecisionID = ""
	data, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return "finding-decision-" + hex.EncodeToString(digest[:])[:24], nil
}

func validateAction(action Action) error {
	switch action {
	case ActionPublish, ActionReject, ActionHumanReview:
		return nil
	default:
		return fmt.Errorf("unsupported finding decision action %q", action)
	}
}

func validateEvidenceRefs(refs []EvidenceRef) error {
	if len(refs) == 0 || len(refs) > 64 {
		return fmt.Errorf("evidence_refs must contain 1..64 entries")
	}
	keys := make([]string, len(refs))
	for index, ref := range refs {
		if err := validateID("evidence_ref.authority", ref.Authority); err != nil {
			return err
		}
		if err := validateText("evidence_ref.id", ref.ID, 2048); err != nil {
			return err
		}
		if ref.Revision != "" {
			if err := validateText("evidence_ref.revision", ref.Revision, 512); err != nil {
				return err
			}
		}
		if ref.SHA256 != "" {
			if err := validateSHA256("evidence_ref.sha256", ref.SHA256); err != nil {
				return err
			}
		}
		keys[index] = strings.Join([]string{ref.Authority, ref.ID, ref.Revision, ref.SHA256}, "\x00")
	}
	if !slices.IsSorted(keys) {
		return fmt.Errorf("evidence_refs must be sorted canonically")
	}
	for index := 1; index < len(keys); index++ {
		if keys[index] == keys[index-1] {
			return fmt.Errorf("evidence_refs contains duplicates")
		}
	}
	return nil
}

func validateID(name, value string) error {
	if err := validateText(name, value, 256); err != nil {
		return err
	}
	if strings.ContainsAny(value, `/\\`) {
		return fmt.Errorf("%s must not contain path separators", name)
	}
	return nil
}

func validateText(name, value string, max int) error {
	if value == "" || len(value) > max || !utf8.ValidString(value) || value != strings.TrimSpace(value) {
		return fmt.Errorf("%s must be non-empty safe text of at most %d bytes", name, max)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return fmt.Errorf("%s contains control characters", name)
		}
	}
	return nil
}

func validateSHA256(name, value string) error {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return fmt.Errorf("%s must be a lowercase SHA-256 digest", name)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return fmt.Errorf("%s must be hexadecimal", name)
	}
	return nil
}

func validateUTC(name string, value time.Time) error {
	if value.IsZero() {
		return fmt.Errorf("%s is required", name)
	}
	if value.Location() != time.UTC {
		return fmt.Errorf("%s must use UTC", name)
	}
	return nil
}
