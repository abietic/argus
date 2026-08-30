package runmodel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const AgentStageHypothesisEvidenceSchemaVersion = "argus.agent_stage_hypothesis_evidence.v1alpha1"

// AgentStageHypothesisEvidence is the append-only admission fact for one
// formal agent-stage hypothesis artifact. It records provenance only: this
// fact is deliberately not a Finding, Decision, Feedback, evaluation label,
// or value/ROI observation.
//
// ResultRef addresses the complete StageExecutionResult bytes received from
// the executor. Output closes the governed result binding over the exact local
// bytes that were strictly decoded as a ReviewHypothesisSet by the application
// admission service. Digest classes remain explicit and non-interchangeable.
type AgentStageHypothesisEvidence struct {
	SchemaVersion string `json:"schema_version"`
	EvidenceID    string `json:"evidence_id"`
	SHA256        string `json:"sha256"`

	Subject     AgentPlanningSubject `json:"subject"`
	ReviewRunID string               `json:"review_run_id"`
	Stage       AgentStageRef        `json:"stage"`

	AdmissionID        string `json:"agent_stage_plan_admission_id"`
	AdmissionSHA256    string `json:"agent_stage_plan_admission_sha256"`
	IntentID           string `json:"dispatch_intent_id"`
	IntentSHA256       string `json:"dispatch_intent_sha256"`
	BindingID          string `json:"execution_binding_id"`
	BindingSHA256      string `json:"execution_binding_sha256"`
	TerminalGateID     string `json:"agent_stage_terminal_gate_id"`
	TerminalGateSHA256 string `json:"agent_stage_terminal_gate_sha256"`

	RequestRef            ArtifactRef             `json:"stage_execution_request_ref"`
	RequestSemanticSHA256 string                  `json:"request_semantic_sha256"`
	ResultRef             ArtifactRef             `json:"stage_execution_result_ref"`
	Output                AgentArtifactProjection `json:"output"`

	PlanID             string `json:"plan_id"`
	PlanSemanticSHA256 string `json:"plan_semantic_sha256"`
	TargetDigest       string `json:"target_digest"`
	HypothesisSetID    string `json:"hypothesis_set_id"`

	AdmittedAt time.Time `json:"admitted_at"`
}

// SealAgentStageHypothesisEvidence derives the sole evidence authority for an
// execution binding and the self digest. AdmittedAt is caller-observed and is
// never silently rewritten, preserving exact retry semantics.
func SealAgentStageHypothesisEvidence(
	evidence AgentStageHypothesisEvidence,
) (AgentStageHypothesisEvidence, error) {
	if evidence.SchemaVersion != "" &&
		evidence.SchemaVersion != AgentStageHypothesisEvidenceSchemaVersion {
		return AgentStageHypothesisEvidence{}, fmt.Errorf(
			"unsupported AgentStageHypothesisEvidence schema %q",
			evidence.SchemaVersion,
		)
	}
	evidence.SchemaVersion = AgentStageHypothesisEvidenceSchemaVersion
	evidence.EvidenceID = ""
	evidence.SHA256 = ""
	if err := evidence.validateContent(); err != nil {
		return AgentStageHypothesisEvidence{}, err
	}
	evidenceID, err := AgentStageHypothesisEvidenceID(
		evidence.ReviewRunID,
		evidence.BindingID,
	)
	if err != nil {
		return AgentStageHypothesisEvidence{}, err
	}
	evidence.EvidenceID = evidenceID
	digest, err := DigestAgentStageHypothesisEvidence(evidence)
	if err != nil {
		return AgentStageHypothesisEvidence{}, err
	}
	evidence.SHA256 = digest
	if err := evidence.Validate(); err != nil {
		return AgentStageHypothesisEvidence{}, fmt.Errorf(
			"validate sealed AgentStageHypothesisEvidence: %w",
			err,
		)
	}
	return evidence, nil
}

// AgentStageHypothesisEvidenceID returns the deterministic single admission
// key for an execution binding. A changed result for the same binding must
// conflict instead of becoming a second evidence authority.
func AgentStageHypothesisEvidenceID(runID, bindingID string) (string, error) {
	if err := validateAgentIdentity("review_run_id", runID); err != nil {
		return "", err
	}
	if err := validateAgentStageIdentifier("execution_binding_id", bindingID); err != nil {
		return "", err
	}
	if !strings.HasPrefix(
		bindingID,
		runID+"-agent-stage-execution-binding-",
	) {
		return "", fmt.Errorf(
			"execution_binding_id is not namespaced by review_run_id",
		)
	}
	payload, err := json.Marshal(struct {
		ReviewRunID string `json:"review_run_id"`
		BindingID   string `json:"execution_binding_id"`
	}{ReviewRunID: runID, BindingID: bindingID})
	if err != nil {
		return "", fmt.Errorf("marshal agent-stage hypothesis evidence identity: %w", err)
	}
	digest := sha256.Sum256(payload)
	return runID + "-agent-stage-hypothesis-evidence-" +
		hex.EncodeToString(digest[:12]), nil
}

func DigestAgentStageHypothesisEvidence(
	evidence AgentStageHypothesisEvidence,
) (string, error) {
	copy := evidence
	copy.SHA256 = ""
	data, err := json.Marshal(copy)
	if err != nil {
		return "", fmt.Errorf("marshal AgentStageHypothesisEvidence: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func (evidence AgentStageHypothesisEvidence) Validate() error {
	if err := evidence.validateContent(); err != nil {
		return err
	}
	wantID, err := AgentStageHypothesisEvidenceID(
		evidence.ReviewRunID,
		evidence.BindingID,
	)
	if err != nil {
		return err
	}
	if evidence.EvidenceID != wantID {
		return fmt.Errorf(
			"evidence_id must be %q for the exact execution binding",
			wantID,
		)
	}
	if err := requireSHA256("sha256", evidence.SHA256); err != nil {
		return err
	}
	digest, err := DigestAgentStageHypothesisEvidence(evidence)
	if err != nil {
		return err
	}
	if evidence.SHA256 != digest {
		return fmt.Errorf(
			"sha256 does not match AgentStageHypothesisEvidence content",
		)
	}
	return nil
}

func (evidence AgentStageHypothesisEvidence) validateContent() error {
	if evidence.SchemaVersion != AgentStageHypothesisEvidenceSchemaVersion {
		return fmt.Errorf(
			"unsupported AgentStageHypothesisEvidence schema %q",
			evidence.SchemaVersion,
		)
	}
	if err := evidence.Subject.Validate(); err != nil {
		return fmt.Errorf("subject: %w", err)
	}
	if err := validateAgentIdentity("review_run_id", evidence.ReviewRunID); err != nil {
		return err
	}
	if err := evidence.Stage.Validate(); err != nil {
		return err
	}
	wantAdmissionID, err := AgentStagePlanAdmissionID(
		evidence.ReviewRunID,
		evidence.Stage.ID,
	)
	if err != nil {
		return err
	}
	if evidence.AdmissionID != wantAdmissionID {
		return fmt.Errorf(
			"agent_stage_plan_admission_id must be %q for the exact run and stage",
			wantAdmissionID,
		)
	}
	if err := requireSHA256(
		"agent_stage_plan_admission_sha256",
		evidence.AdmissionSHA256,
	); err != nil {
		return err
	}
	if _, err := AgentStageExecutionBindingID(
		evidence.ReviewRunID,
		evidence.IntentID,
	); err != nil {
		return err
	}
	if err := requireSHA256("dispatch_intent_sha256", evidence.IntentSHA256); err != nil {
		return err
	}
	if _, err := AgentStageHypothesisEvidenceID(
		evidence.ReviewRunID,
		evidence.BindingID,
	); err != nil {
		return err
	}
	if err := requireSHA256(
		"execution_binding_sha256",
		evidence.BindingSHA256,
	); err != nil {
		return err
	}
	wantTerminalGateID, err := AgentStageTerminalGateID(
		evidence.ReviewRunID,
		evidence.IntentID,
	)
	if err != nil {
		return err
	}
	if evidence.TerminalGateID != wantTerminalGateID {
		return fmt.Errorf(
			"agent_stage_terminal_gate_id must be %q for the exact dispatch intent",
			wantTerminalGateID,
		)
	}
	if err := requireSHA256(
		"agent_stage_terminal_gate_sha256",
		evidence.TerminalGateSHA256,
	); err != nil {
		return err
	}
	if err := evidence.RequestRef.Validate(); err != nil {
		return fmt.Errorf("stage_execution_request_ref: %w", err)
	}
	if evidence.RequestRef.SizeBytes <= 0 ||
		evidence.RequestRef.Contract != ContractStageExecutionRequest {
		return fmt.Errorf(
			"stage_execution_request_ref must be a non-empty %q artifact",
			ContractStageExecutionRequest,
		)
	}
	if err := requireSHA256(
		"request_semantic_sha256",
		evidence.RequestSemanticSHA256,
	); err != nil {
		return err
	}
	if err := evidence.ResultRef.Validate(); err != nil {
		return fmt.Errorf("stage_execution_result_ref: %w", err)
	}
	if evidence.ResultRef.SizeBytes <= 0 ||
		evidence.ResultRef.Contract != ContractStageExecutionResult {
		return fmt.Errorf(
			"stage_execution_result_ref must be a non-empty %q artifact",
			ContractStageExecutionResult,
		)
	}
	if err := evidence.Output.validate(
		"output",
		ContractReviewHypothesisSet,
		evidence.Subject,
	); err != nil {
		return err
	}
	if err := requireSHA256("plan_semantic_sha256", evidence.PlanSemanticSHA256); err != nil {
		return err
	}
	if evidence.PlanID != "agent-stage-plan-"+evidence.PlanSemanticSHA256[:24] {
		return fmt.Errorf("plan_id does not match plan_semantic_sha256")
	}
	if err := requireSHA256("target_digest", evidence.TargetDigest); err != nil {
		return err
	}
	if err := validateAgentStageIdentifier(
		"hypothesis_set_id",
		evidence.HypothesisSetID,
	); err != nil {
		return err
	}
	if evidence.AdmittedAt.IsZero() || evidence.AdmittedAt.Location() != time.UTC {
		return fmt.Errorf("admitted_at must be a non-zero UTC timestamp")
	}
	return nil
}

// ValidateAgainstClosure proves that the evidence is authorized by the exact
// admission -> dispatch intent -> execution binding -> accepted terminal gate
// lineage. Artifact content semantics are admitted separately by the
// application service before this fact is sealed.
func (evidence AgentStageHypothesisEvidence) ValidateAgainstClosure(
	admission AgentStagePlanAdmission,
	intent AgentStageDispatchIntent,
	binding AgentStageExecutionBinding,
	terminal AgentStageTerminalGate,
) error {
	if err := evidence.Validate(); err != nil {
		return fmt.Errorf("validate hypothesis evidence: %w", err)
	}
	if err := admission.Validate(); err != nil {
		return fmt.Errorf("validate agent-stage plan admission: %w", err)
	}
	if err := intent.ValidateAgainstAdmission(admission); err != nil {
		return fmt.Errorf("validate dispatch intent against admission: %w", err)
	}
	if err := binding.ValidateAgainstIntent(intent); err != nil {
		return fmt.Errorf("validate execution binding against intent: %w", err)
	}
	if err := terminal.ValidateAgainstAdmissionAndIntent(admission, intent); err != nil {
		return fmt.Errorf("validate terminal gate against admission and intent: %w", err)
	}
	if err := terminal.ValidateCompletionAgainstBinding(binding); err != nil {
		return fmt.Errorf("validate accepted terminal completion against binding: %w", err)
	}
	if evidence.Subject != admission.Subject ||
		evidence.Subject != intent.Subject ||
		evidence.Subject != binding.Subject ||
		evidence.Subject != terminal.Subject ||
		evidence.ReviewRunID != admission.ReviewRunID ||
		evidence.ReviewRunID != intent.ReviewRunID ||
		evidence.ReviewRunID != binding.ReviewRunID ||
		evidence.ReviewRunID != terminal.ReviewRunID ||
		evidence.Stage != admission.Stage ||
		evidence.Stage != intent.Stage ||
		evidence.Stage != binding.Stage ||
		evidence.Stage != terminal.Stage ||
		evidence.AdmissionID != admission.AdmissionID ||
		evidence.AdmissionSHA256 != admission.SHA256 ||
		evidence.IntentID != intent.IntentID ||
		evidence.IntentSHA256 != intent.SHA256 ||
		evidence.BindingID != binding.BindingID ||
		evidence.BindingSHA256 != binding.SHA256 ||
		evidence.TerminalGateID != terminal.GateID ||
		evidence.TerminalGateSHA256 != terminal.SHA256 ||
		evidence.RequestRef != intent.RequestRef ||
		evidence.RequestRef != binding.RequestRef ||
		evidence.RequestRef != terminal.RequestRef ||
		evidence.RequestSemanticSHA256 != intent.RequestSemanticSHA256 ||
		evidence.RequestSemanticSHA256 != binding.RequestSemanticSHA256 ||
		evidence.RequestSemanticSHA256 != terminal.RequestSemanticSHA256 ||
		terminal.Kind != AgentStageSucceededResultAccepted ||
		terminal.Completion == nil ||
		evidence.ResultRef != terminal.Completion.ResultRef ||
		evidence.Output != terminal.Completion.Output ||
		evidence.PlanID != admission.PlanID ||
		evidence.PlanID != intent.PlanID ||
		evidence.PlanSemanticSHA256 != admission.PlanSemanticSHA256 ||
		evidence.PlanSemanticSHA256 != intent.PlanSemanticSHA256 {
		return fmt.Errorf("hypothesis evidence does not match exact accepted terminal closure")
	}
	if evidence.AdmittedAt.Before(terminal.Completion.AcceptedAt) {
		return fmt.Errorf("hypothesis evidence admitted_at predates accepted terminal winner")
	}
	return nil
}
