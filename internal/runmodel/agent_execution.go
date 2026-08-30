package runmodel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	AgentStagePlanAdmissionSchemaVersion = "argus.agent_stage_plan_admission.v1alpha1"
	ContractAgentStagePlan               = "argus.agent_stage_plan.v1alpha1"
)

// AgentPlanningSubject is the authenticated scope copied into an agent-stage
// admission. It is a provenance fact, not an authentication credential. A
// repository lookup must still compare it with the host-authenticated subject.
type AgentPlanningSubject struct {
	TenantID       string `json:"tenant_id"`
	OrganizationID string `json:"organization_id"`
	WorkspaceID    string `json:"workspace_id"`
	RepositoryID   string `json:"repository_id"`
}

// AgentStageRef is the exact workflow-stage identity bound into a plan.
type AgentStageRef struct {
	ID       string `json:"id"`
	Revision string `json:"revision"`
	SHA256   string `json:"sha256"`
}

// GovernedArtifactBinding is an execution-facing, tenant/workspace-scoped
// artifact alias. Its SHA256 is the digest of the stored canonical bytes. It
// is deliberately distinct from an artifact's embedded semantic/self digest.
type GovernedArtifactBinding struct {
	URI       string `json:"uri"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	Contract  string `json:"contract"`
}

// AgentArtifactProjection proves that one local immutable artifact and one
// governed execution-facing alias identify the exact same canonical bytes.
type AgentArtifactProjection struct {
	Local    ArtifactRef             `json:"local_ref"`
	Governed GovernedArtifactBinding `json:"governed_binding"`
}

// AgentStageSourceProjections closes all execution-facing source artifacts of
// AgentStagePlan. ConfigResolutionReceipt remains separate from ConfigBundle:
// its embedded semantic digest is recorded on AgentStagePlanAdmission.
type AgentStageSourceProjections struct {
	ExecutionSnapshot       AgentArtifactProjection `json:"execution_snapshot"`
	ConfigBundle            AgentArtifactProjection `json:"config_bundle"`
	ConfigResolutionReceipt AgentArtifactProjection `json:"config_resolution_receipt"`
	Workflow                AgentArtifactProjection `json:"workflow"`
	ReviewSpec              AgentArtifactProjection `json:"review_spec"`
	ReviewInput             AgentArtifactProjection `json:"review_input"`
}

// AgentStagePlanAdmission is the append-only commit point between governed
// planning and dispatch. PlanSemanticSHA256 and ReceiptSemanticSHA256 bind the
// embedded domain identities; Plan/Source projection refs bind the canonical
// artifact bytes. These digest classes are not interchangeable.
type AgentStagePlanAdmission struct {
	SchemaVersion string `json:"schema_version"`
	AdmissionID   string `json:"admission_id"`
	SHA256        string `json:"sha256"`

	Subject     AgentPlanningSubject `json:"subject"`
	ReviewRunID string               `json:"review_run_id"`
	Stage       AgentStageRef        `json:"stage"`

	PlanID             string                  `json:"plan_id"`
	PlanSemanticSHA256 string                  `json:"plan_semantic_sha256"`
	PlanBehaviorSHA256 string                  `json:"plan_behavior_sha256"`
	Plan               AgentArtifactProjection `json:"plan"`

	ReceiptID             string                      `json:"config_resolution_receipt_id"`
	ReceiptSemanticSHA256 string                      `json:"config_resolution_receipt_semantic_sha256"`
	Sources               AgentStageSourceProjections `json:"source_projections"`

	RecordedAt time.Time `json:"recorded_at"`
}

// SealAgentStagePlanAdmission derives the unique run+stage admission identity
// and its self digest. RecordedAt must already be a non-zero UTC observation;
// silently rewriting a caller timestamp would make retries ambiguous.
func SealAgentStagePlanAdmission(
	admission AgentStagePlanAdmission,
) (AgentStagePlanAdmission, error) {
	if admission.SchemaVersion != "" &&
		admission.SchemaVersion != AgentStagePlanAdmissionSchemaVersion {
		return AgentStagePlanAdmission{}, fmt.Errorf(
			"unsupported AgentStagePlanAdmission schema %q",
			admission.SchemaVersion,
		)
	}
	admission.SchemaVersion = AgentStagePlanAdmissionSchemaVersion
	admission.AdmissionID = ""
	admission.SHA256 = ""
	if err := admission.validateContent(); err != nil {
		return AgentStagePlanAdmission{}, err
	}
	admissionID, err := AgentStagePlanAdmissionID(
		admission.ReviewRunID,
		admission.Stage.ID,
	)
	if err != nil {
		return AgentStagePlanAdmission{}, err
	}
	admission.AdmissionID = admissionID
	digest, err := DigestAgentStagePlanAdmission(admission)
	if err != nil {
		return AgentStagePlanAdmission{}, err
	}
	admission.SHA256 = digest
	if err := admission.Validate(); err != nil {
		return AgentStagePlanAdmission{}, fmt.Errorf(
			"validate sealed AgentStagePlanAdmission: %w",
			err,
		)
	}
	return admission, nil
}

// AgentStagePlanAdmissionID returns the sole authoritative admission key for
// one run+stage coordinate. A different plan for the same coordinate must
// conflict at persistence instead of becoming a second authority.
func AgentStagePlanAdmissionID(runID, stageID string) (string, error) {
	if err := validateAgentIdentity("review_run_id", runID); err != nil {
		return "", err
	}
	if err := validateAgentStageIdentifier("stage.id", stageID); err != nil {
		return "", err
	}
	payload, err := json.Marshal(struct {
		ReviewRunID string `json:"review_run_id"`
		StageID     string `json:"stage_id"`
	}{ReviewRunID: runID, StageID: stageID})
	if err != nil {
		return "", fmt.Errorf("marshal agent-stage admission identity: %w", err)
	}
	digest := sha256.Sum256(payload)
	return runID + "-agent-stage-admission-" + hex.EncodeToString(digest[:12]), nil
}

// DigestAgentStagePlanAdmission returns the record self digest. AdmissionID is
// included; only the SHA256 field is cleared.
func DigestAgentStagePlanAdmission(admission AgentStagePlanAdmission) (string, error) {
	copy := admission
	copy.SHA256 = ""
	data, err := json.Marshal(copy)
	if err != nil {
		return "", fmt.Errorf("marshal AgentStagePlanAdmission: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func (admission AgentStagePlanAdmission) Validate() error {
	if err := admission.validateContent(); err != nil {
		return err
	}
	wantID, err := AgentStagePlanAdmissionID(
		admission.ReviewRunID,
		admission.Stage.ID,
	)
	if err != nil {
		return err
	}
	if admission.AdmissionID != wantID {
		return fmt.Errorf("admission_id must be %q for the exact run and stage", wantID)
	}
	if err := requireSHA256("sha256", admission.SHA256); err != nil {
		return err
	}
	digest, err := DigestAgentStagePlanAdmission(admission)
	if err != nil {
		return err
	}
	if admission.SHA256 != digest {
		return fmt.Errorf("sha256 does not match AgentStagePlanAdmission content")
	}
	return nil
}

func (admission AgentStagePlanAdmission) validateContent() error {
	if admission.SchemaVersion != AgentStagePlanAdmissionSchemaVersion {
		return fmt.Errorf(
			"unsupported AgentStagePlanAdmission schema %q",
			admission.SchemaVersion,
		)
	}
	if err := admission.Subject.Validate(); err != nil {
		return fmt.Errorf("subject: %w", err)
	}
	if err := validateAgentIdentity("review_run_id", admission.ReviewRunID); err != nil {
		return err
	}
	if err := admission.Stage.Validate(); err != nil {
		return err
	}
	if err := requireSHA256("plan_semantic_sha256", admission.PlanSemanticSHA256); err != nil {
		return err
	}
	if admission.PlanID != "agent-stage-plan-"+admission.PlanSemanticSHA256[:24] {
		return fmt.Errorf("plan_id does not match plan_semantic_sha256")
	}
	if err := requireSHA256("plan_behavior_sha256", admission.PlanBehaviorSHA256); err != nil {
		return err
	}
	if err := admission.Plan.validate(
		"plan",
		ContractAgentStagePlan,
		admission.Subject,
	); err != nil {
		return err
	}
	if err := requireSHA256(
		"config_resolution_receipt_semantic_sha256",
		admission.ReceiptSemanticSHA256,
	); err != nil {
		return err
	}
	if admission.ReceiptID != "config-resolution-"+admission.ReceiptSemanticSHA256[:24] {
		return fmt.Errorf(
			"config_resolution_receipt_id does not match its semantic SHA-256",
		)
	}
	if err := admission.Sources.validate(admission.Subject); err != nil {
		return err
	}
	if admission.RecordedAt.IsZero() || admission.RecordedAt.Location() != time.UTC {
		return fmt.Errorf("recorded_at must be a non-zero UTC timestamp")
	}
	return nil
}

func (subject AgentPlanningSubject) Validate() error {
	for name, value := range map[string]string{
		"tenant_id":       subject.TenantID,
		"organization_id": subject.OrganizationID,
		"workspace_id":    subject.WorkspaceID,
		"repository_id":   subject.RepositoryID,
	} {
		if err := validateAgentIdentity(name, value); err != nil {
			return err
		}
	}
	return nil
}

func (stage AgentStageRef) Validate() error {
	if err := validateAgentStageIdentifier("stage.id", stage.ID); err != nil {
		return err
	}
	if err := validateAgentStageIdentifier("stage.revision", stage.Revision); err != nil {
		return err
	}
	if strings.EqualFold(stage.ID, "latest") || strings.EqualFold(stage.Revision, "latest") {
		return fmt.Errorf("stage must not use latest")
	}
	return requireSHA256("stage.sha256", stage.SHA256)
}

func (sources AgentStageSourceProjections) validate(subject AgentPlanningSubject) error {
	for _, item := range []struct {
		name       string
		projection AgentArtifactProjection
		contract   string
	}{
		{"source_projections.execution_snapshot", sources.ExecutionSnapshot, ContractExecutionSnapshot},
		{"source_projections.config_bundle", sources.ConfigBundle, ContractConfigBundle},
		{
			"source_projections.config_resolution_receipt",
			sources.ConfigResolutionReceipt,
			ContractConfigResolutionReceipt,
		},
		{"source_projections.workflow", sources.Workflow, ContractWorkflowDefinition},
		{"source_projections.review_spec", sources.ReviewSpec, ContractReviewSpec},
		{"source_projections.review_input", sources.ReviewInput, ContractReviewInput},
	} {
		if err := item.projection.validate(item.name, item.contract, subject); err != nil {
			return err
		}
	}
	return nil
}

func (projection AgentArtifactProjection) validate(
	name string,
	contract string,
	subject AgentPlanningSubject,
) error {
	if err := projection.Local.Validate(); err != nil {
		return fmt.Errorf("%s.local_ref: %w", name, err)
	}
	if projection.Local.SizeBytes == 0 {
		return fmt.Errorf("%s.local_ref must be non-empty", name)
	}
	if projection.Local.Contract != contract {
		return fmt.Errorf("%s.local_ref contract must be %q", name, contract)
	}
	if err := projection.Governed.validateForSubject(subject); err != nil {
		return fmt.Errorf("%s.governed_binding: %w", name, err)
	}
	if projection.Governed.Contract != contract {
		return fmt.Errorf("%s.governed_binding contract must be %q", name, contract)
	}
	if projection.Local.SHA256 != projection.Governed.SHA256 ||
		projection.Local.SizeBytes != projection.Governed.SizeBytes ||
		projection.Local.Contract != projection.Governed.Contract {
		return fmt.Errorf("%s local and governed refs must bind the exact same bytes", name)
	}
	return nil
}

func (binding GovernedArtifactBinding) validateForSubject(
	subject AgentPlanningSubject,
) error {
	if err := requireSHA256("sha256", binding.SHA256); err != nil {
		return err
	}
	if binding.SizeBytes <= 0 {
		return fmt.Errorf("size_bytes must be positive")
	}
	if binding.Contract == "" || binding.Contract != strings.TrimSpace(binding.Contract) {
		return fmt.Errorf("contract is required")
	}
	parsed, err := url.Parse(binding.URI)
	if err != nil {
		return fmt.Errorf("parse governed artifact URI: %w", err)
	}
	if parsed.Scheme != "artifact" || parsed.Host == "" ||
		parsed.Opaque != "" || parsed.User != nil || parsed.RawQuery != "" ||
		parsed.Fragment != "" || parsed.ForceQuery || parsed.RawPath != "" {
		return fmt.Errorf("URI must be a canonical governed artifact URI")
	}
	if err := validateAgentIdentity("authority", parsed.Host); err != nil ||
		parsed.Host != strings.ToLower(parsed.Host) {
		return fmt.Errorf("URI authority is invalid")
	}
	segments := strings.Split(strings.TrimPrefix(parsed.Path, "/"), "/")
	if len(segments) != 6 || segments[0] != "tenants" ||
		segments[2] != "workspaces" || segments[4] != "objects" {
		return fmt.Errorf("URI is not in a governed tenant/workspace object namespace")
	}
	if segments[1] != subject.TenantID || segments[3] != subject.WorkspaceID {
		return fmt.Errorf("URI belongs to another planning subject")
	}
	if err := requireSHA256("object_id", segments[5]); err != nil {
		return fmt.Errorf("governed artifact URI object identity: %w", err)
	}
	wantURI := "artifact://" + parsed.Host + "/tenants/" + subject.TenantID +
		"/workspaces/" + subject.WorkspaceID + "/objects/" + segments[5]
	if binding.URI != wantURI {
		return fmt.Errorf("URI is not in canonical form")
	}
	return nil
}

func validateAgentIdentity(name, value string) error {
	if value == "" || len(value) > 128 || value != strings.TrimSpace(value) ||
		!utf8.ValidString(value) || value == "." || value == ".." {
		return fmt.Errorf("%s must be a non-empty safe identifier", name)
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			strings.ContainsRune("._~-", character) {
			continue
		}
		return fmt.Errorf("%s must be a non-empty safe identifier", name)
	}
	return nil
}

// validateAgentStageIdentifier mirrors the public AgentStagePlan identifier
// envelope. Stage values are hashed rather than interpolated into store paths,
// so repository-path restrictions do not belong here.
func validateAgentStageIdentifier(name, value string) error {
	if value == "" || len(value) > 256 || value != strings.TrimSpace(value) ||
		!utf8.ValidString(value) {
		return fmt.Errorf("%s must be a non-empty safe identifier", name)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return fmt.Errorf("%s must be a non-empty safe identifier", name)
		}
	}
	return nil
}
