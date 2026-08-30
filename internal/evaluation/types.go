// Package evaluation owns Argus evaluation dataset governance, contamination
// controls, holdout exposure evidence, and the promotion ledger.
package evaluation

import (
	"errors"
	"time"
)

const (
	EvaluationCaseSchemaVersion       = "argus.evaluation_case.v1alpha1"
	LabelCorrectionSchemaVersion      = "argus.evaluation_label_correction.v1alpha1"
	CaseAnnotationSchemaVersion       = "argus.evaluation_case_annotation.v1alpha1"
	CaseAdjudicationSchemaVersion     = "argus.evaluation_case_adjudication.v1alpha1"
	CaseActivationSchemaVersion       = "argus.evaluation_case_activation.v1alpha1"
	CaseReviewAssignmentSchemaVersion = "argus.evaluation_case_review_assignment.v1alpha1"
	CaseReopenSchemaVersion           = "argus.evaluation_case_reopen.v1alpha1"
	ExposureSchemaVersion             = "argus.evaluation_exposure.v1alpha1"
	PromotionVariantSchemaVersion     = "argus.promotion_variant.v1alpha1"
	GateResultSchemaVersion           = "argus.promotion_gate_result.v1alpha1"

	datasetEventSchemaVersion   = "argus.evaluation_dataset_event.v1alpha1"
	promotionEventSchemaVersion = "argus.promotion_event.v1alpha1"
	datasetStream               = "evaluation/dataset"
	promotionStream             = "evaluation/promotion"
)

var (
	ErrNotFound          = errors.New("evaluation record not found")
	ErrConflict          = errors.New("evaluation mutation conflict")
	ErrUnauthorized      = errors.New("evaluation access unauthorized")
	ErrInvalidTransition = errors.New("invalid evaluation transition")
	ErrContaminated      = errors.New("evaluation dataset is contaminated")
	ErrCorrupt           = errors.New("corrupt evaluation repository")
)

type CaseType string

const (
	CasePositiveLocalized       CaseType = "positive_localized"
	CaseNegativeClean           CaseType = "negative_clean"
	CaseFalsePositiveRegression CaseType = "false_positive_regression"
	CaseMissedDefectRegression  CaseType = "missed_defect_regression"
	CaseFixValidation           CaseType = "fix_validation"
	CaseMutationDiagnostic      CaseType = "mutation_diagnostic"
	CaseWorkflowInvariant       CaseType = "workflow_invariant"
)

type SourceKind string

const (
	SourceHumanConfirmedFinding SourceKind = "human_confirmed_finding"
	SourceRejectedFalsePositive SourceKind = "rejected_false_positive"
	SourceIncidentMissedDefect  SourceKind = "incident_missed_defect"
	SourceReviewedBugFixPair    SourceKind = "reviewed_bug_fix_pair"
	SourceMutation              SourceKind = "mutation"
	SourceSynthetic             SourceKind = "synthetic"
	SourceProductionFeedback    SourceKind = "production_feedback"
)

type Classification string

const (
	ClassificationPublic       Classification = "public"
	ClassificationInternal     Classification = "internal"
	ClassificationConfidential Classification = "confidential"
	ClassificationRestricted   Classification = "restricted"
)

type ConsentBasis string

const (
	ConsentExplicit           ConsentBasis = "explicit"
	ConsentContractual        ConsentBasis = "contractual"
	ConsentAuthorizedInternal ConsentBasis = "authorized_internal"
	ConsentPublicLicense      ConsentBasis = "public_license"
	ConsentSynthetic          ConsentBasis = "synthetic"
)

type UseScope string

const (
	UseCandidatePool UseScope = "candidate_pool"
	UseEvaluation    UseScope = "evaluation"
	UseTraining      UseScope = "training"
	UsePromotion     UseScope = "promotion"
)

type ReviewState string

const (
	ReviewPending  ReviewState = "pending"
	ReviewInReview ReviewState = "in_review"
	ReviewApproved ReviewState = "approved"
	ReviewRejected ReviewState = "rejected"
)

type DatasetState string

const (
	DatasetCandidatePool DatasetState = "candidate_pool"
	DatasetGold          DatasetState = "gold"
	DatasetActive        DatasetState = "active"
	DatasetRetired       DatasetState = "retired"
)

type Split string

const (
	SplitUnassigned Split = "unassigned"
	SplitTrain      Split = "train"
	SplitDev        Split = "dev"
	SplitTest       Split = "test"
	SplitHoldout    Split = "holdout"
)

type ExpectedOutcome string

const (
	OutcomeDefectPresent ExpectedOutcome = "defect_present"
	OutcomeClean         ExpectedOutcome = "clean"
	OutcomeFalsePositive ExpectedOutcome = "false_positive"
	OutcomeMissedDefect  ExpectedOutcome = "missed_defect"
	OutcomeFixValid      ExpectedOutcome = "fix_valid"
	OutcomeFixInvalid    ExpectedOutcome = "fix_invalid"
	OutcomeInvariantPass ExpectedOutcome = "invariant_pass"
	OutcomeInvariantFail ExpectedOutcome = "invariant_fail"
)

// SourceProvenance points to immutable source evidence rather than copying
// source-system mutable state into an EvaluationCase.
type SourceProvenance struct {
	Kind         SourceKind `json:"kind"`
	RepositoryID string     `json:"repository_id"`
	SourceID     string     `json:"source_id"`
	SourceRunID  string     `json:"source_run_id,omitempty"`
	FindingID    string     `json:"finding_id,omitempty"`
	ObservedAt   time.Time  `json:"observed_at"`
	CollectedAt  time.Time  `json:"collected_at"`
	EvidenceRefs []string   `json:"evidence_refs"`
}

type LicenseConsent struct {
	LicenseID    string       `json:"license_id"`
	Consent      ConsentBasis `json:"consent"`
	AllowedUses  []UseScope   `json:"allowed_uses"`
	Restrictions []string     `json:"restrictions"`
}

type Eligibility struct {
	Evaluation             bool `json:"evaluation"`
	Training               bool `json:"training"`
	Promotion              bool `json:"promotion"`
	ProductionDistribution bool `json:"production_distribution"`
}

type Label struct {
	ExpectedOutcome    ExpectedOutcome     `json:"expected_outcome"`
	Category           string              `json:"category,omitempty"`
	Severity           string              `json:"severity,omitempty"`
	Anchors            []LabelAnchor       `json:"anchors"`
	AnchorRefs         []string            `json:"anchor_refs"`
	SuppressionTargets []SuppressionTarget `json:"suppression_targets"`
}

type LabelAnchor struct {
	Path         string `json:"path"`
	Side         string `json:"side"`
	StartLine    uint32 `json:"start_line"`
	EndLine      uint32 `json:"end_line"`
	SourceDigest string `json:"source_digest"`
}

// SuppressionTarget freezes the exact canonical candidate identity that a
// false-positive regression is expected to exercise. Absence of this
// fingerprint from a report is not evidence that a filter rejected it.
type SuppressionTarget struct {
	ClusterFingerprint string `json:"cluster_fingerprint"`
}

// EvaluationCase is immutable after creation. Label corrections are separate
// append-only entries; CurrentLabel on CaseRecord is only a projection.
type EvaluationCase struct {
	SchemaVersion       string           `json:"schema_version"`
	CaseID              string           `json:"case_id"`
	Type                CaseType         `json:"type"`
	Provenance          SourceProvenance `json:"provenance"`
	LicenseConsent      LicenseConsent   `json:"license_consent"`
	Classification      Classification   `json:"classification"`
	Owner               string           `json:"owner"`
	InputSnapshotRef    string           `json:"input_snapshot_ref"`
	Label               Label            `json:"label"`
	LabelPolicyRevision string           `json:"label_policy_revision"`
	ReviewState         ReviewState      `json:"review_state"`
	DatasetState        DatasetState     `json:"dataset_state"`
	Split               Split            `json:"split"`
	CloneGroupID        string           `json:"clone_group_id"`
	Eligibility         Eligibility      `json:"eligibility"`
	CreatedAt           time.Time        `json:"created_at"`
}

type LabelCorrection struct {
	SchemaVersion         string   `json:"schema_version"`
	CaseID                string   `json:"case_id"`
	ExpectedLabelRevision uint64   `json:"expected_label_revision"`
	Label                 Label    `json:"label"`
	LabelPolicyRevision   string   `json:"label_policy_revision"`
	Reason                string   `json:"reason"`
	AffectedExperimentIDs []string `json:"affected_experiment_ids"`
}

type LabelEntry struct {
	Revision              uint64    `json:"revision"`
	EventID               string    `json:"event_id"`
	Label                 Label     `json:"label"`
	LabelPolicyRevision   string    `json:"label_policy_revision"`
	Reason                string    `json:"reason"`
	AffectedExperimentIDs []string  `json:"affected_experiment_ids"`
	Actor                 string    `json:"actor"`
	At                    time.Time `json:"at"`
}

// CaseGovernance is the mutable projection of append-only governance events.
// CaseRecord.Case remains the exact immutable ingress object.
type CaseGovernance struct {
	Revision       uint64         `json:"revision"`
	ReviewState    ReviewState    `json:"review_state"`
	DatasetState   DatasetState   `json:"dataset_state"`
	Split          Split          `json:"split"`
	Eligibility    Eligibility    `json:"eligibility"`
	LicenseConsent LicenseConsent `json:"license_consent"`
	UpdatedAt      time.Time      `json:"updated_at"`
	UpdatedBy      string         `json:"updated_by"`
}

type CaseRecord struct {
	Case                       EvaluationCase                  `json:"case"`
	CurrentGovernance          CaseGovernance                  `json:"current_governance"`
	CurrentLabel               Label                           `json:"current_label"`
	CurrentLabelPolicyRevision string                          `json:"current_label_policy_revision"`
	CurrentLabelRevision       uint64                          `json:"current_label_revision"`
	UpdatedAt                  time.Time                       `json:"updated_at"`
	UpdatedBy                  string                          `json:"updated_by"`
	ExternalGovernance         *ExternalGovernanceImportRecord `json:"external_governance,omitempty"`
}

// CurrentCase materializes the immutable case plus its current append-only
// governance and label projections for admission and contamination checks.
func (record CaseRecord) CurrentCase() EvaluationCase {
	current := cloneValue(record.Case)
	current.ReviewState = record.CurrentGovernance.ReviewState
	current.DatasetState = record.CurrentGovernance.DatasetState
	current.Split = record.CurrentGovernance.Split
	current.Eligibility = record.CurrentGovernance.Eligibility
	current.LicenseConsent = cloneValue(record.CurrentGovernance.LicenseConsent)
	current.Label = cloneValue(record.CurrentLabel)
	current.LabelPolicyRevision = record.CurrentLabelPolicyRevision
	return current
}

type AnnotationVerdict string

const (
	AnnotationApprove AnnotationVerdict = "approve"
	AnnotationReject  AnnotationVerdict = "reject"
)

// CaseAnnotation is one independent human review. It never changes dataset
// truth by itself; only an adjudication over multiple annotations can do that.
type CaseAnnotation struct {
	SchemaVersion              string            `json:"schema_version"`
	CaseID                     string            `json:"case_id"`
	ExpectedGovernanceRevision uint64            `json:"expected_governance_revision"`
	ExpectedLabelRevision      uint64            `json:"expected_label_revision"`
	Verdict                    AnnotationVerdict `json:"verdict"`
	ProposedLabel              *Label            `json:"proposed_label,omitempty"`
	LabelPolicyRevision        string            `json:"label_policy_revision,omitempty"`
	Rationale                  string            `json:"rationale"`
	EvidenceRefs               []string          `json:"evidence_refs"`
	ReviewedAt                 time.Time         `json:"reviewed_at"`
}

type CaseAnnotationEntry struct {
	EventID    string         `json:"event_id"`
	Annotation CaseAnnotation `json:"annotation"`
	Reviewer   string         `json:"reviewer"`
	At         time.Time      `json:"at"`
}

// CaseReviewAssignment creates an explicit blind review round. Assigned
// reviewers may annotate that exact governance/label revision, while reviewer
// reads remain filtered to their own annotations.
type CaseReviewAssignment struct {
	SchemaVersion              string    `json:"schema_version"`
	CaseID                     string    `json:"case_id"`
	ExpectedGovernanceRevision uint64    `json:"expected_governance_revision"`
	ExpectedLabelRevision      uint64    `json:"expected_label_revision"`
	ReviewerIDs                []string  `json:"reviewer_ids"`
	Blind                      bool      `json:"blind"`
	Reason                     string    `json:"reason"`
	AssignedAt                 time.Time `json:"assigned_at"`
}

type CaseReviewAssignmentEntry struct {
	EventID    string               `json:"event_id"`
	Assignment CaseReviewAssignment `json:"assignment"`
	AssignedBy string               `json:"assigned_by"`
	At         time.Time            `json:"at"`
}

type ReviewAgreementState string

const (
	ReviewAgreementUnavailable  ReviewAgreementState = "unavailable"
	ReviewAgreementIncomplete   ReviewAgreementState = "incomplete"
	ReviewAgreementUnanimous    ReviewAgreementState = "unanimous"
	ReviewAgreementDisagreement ReviewAgreementState = "disagreement"
)

// CaseReviewAgreement is a deterministic projection, never a mutable score.
// Counts and exact-label agreement can be recomputed from assignment and
// annotation events.
type CaseReviewAgreement struct {
	CaseID              string               `json:"case_id"`
	GovernanceRevision  uint64               `json:"governance_revision"`
	LabelRevision       uint64               `json:"label_revision"`
	AssignedReviewers   int                  `json:"assigned_reviewers"`
	CompletedReviews    int                  `json:"completed_reviews"`
	ApproveCount        int                  `json:"approve_count"`
	RejectCount         int                  `json:"reject_count"`
	VerdictAgreement    ReviewAgreementState `json:"verdict_agreement"`
	ExactLabelAgreement ReviewAgreementState `json:"exact_label_agreement"`
}

type AdjudicationOutcome string

const (
	AdjudicationApprove AdjudicationOutcome = "approve"
	AdjudicationReject  AdjudicationOutcome = "reject"
)

// CaseAdjudication freezes the exact annotation events used to accept or
// reject a candidate. An approved label must have been proposed by a reviewer.
type CaseAdjudication struct {
	SchemaVersion              string              `json:"schema_version"`
	CaseID                     string              `json:"case_id"`
	ExpectedGovernanceRevision uint64              `json:"expected_governance_revision"`
	ExpectedLabelRevision      uint64              `json:"expected_label_revision"`
	AnnotationEventIDs         []string            `json:"annotation_event_ids"`
	Outcome                    AdjudicationOutcome `json:"outcome"`
	SelectedLabel              *Label              `json:"selected_label,omitempty"`
	LabelPolicyRevision        string              `json:"label_policy_revision,omitempty"`
	Rationale                  string              `json:"rationale"`
	EvidenceRefs               []string            `json:"evidence_refs"`
	AdjudicatedAt              time.Time           `json:"adjudicated_at"`
}

type CaseAdjudicationEntry struct {
	EventID      string           `json:"event_id"`
	Adjudication CaseAdjudication `json:"adjudication"`
	Adjudicator  string           `json:"adjudicator"`
	At           time.Time        `json:"at"`
}

// CaseActivation is a separate governed transition from approved gold to an
// active split. It cannot be folded into adjudication, so holdout assignment
// and expanded data-use consent remain independently auditable.
type CaseActivation struct {
	SchemaVersion              string         `json:"schema_version"`
	CaseID                     string         `json:"case_id"`
	ExpectedGovernanceRevision uint64         `json:"expected_governance_revision"`
	Split                      Split          `json:"split"`
	Eligibility                Eligibility    `json:"eligibility"`
	LicenseConsent             LicenseConsent `json:"license_consent"`
	Reason                     string         `json:"reason"`
	ActivatedAt                time.Time      `json:"activated_at"`
}

// CaseReopen invalidates the current governed projection for future use while
// preserving all historical labels, annotations, adjudications, exposures and
// experiments. A new review round starts from candidate-only ingress rights.
type CaseReopen struct {
	SchemaVersion              string    `json:"schema_version"`
	CaseID                     string    `json:"case_id"`
	ExpectedGovernanceRevision uint64    `json:"expected_governance_revision"`
	Reason                     string    `json:"reason"`
	EvidenceRefs               []string  `json:"evidence_refs"`
	AffectedExperimentIDs      []string  `json:"affected_experiment_ids"`
	ReopenedAt                 time.Time `json:"reopened_at"`
}

type ExposureComponent string

const (
	ExposurePrompt ExposureComponent = "prompt"
	ExposureRule   ExposureComponent = "rule"
	ExposureModel  ExposureComponent = "model"
	ExposureIndex  ExposureComponent = "index"
)

type ExposureStatus string

const (
	ExposureSeen    ExposureStatus = "seen"
	ExposureNotSeen ExposureStatus = "not_seen"
)

type ExposureObservation struct {
	Component ExposureComponent `json:"component"`
	Status    ExposureStatus    `json:"status"`
	Revision  string            `json:"revision"`
}

// Exposure records whether each mutable evaluation input had already seen a
// case before the run. All four components are mandatory so omission cannot be
// mistaken for not-seen.
type Exposure struct {
	SchemaVersion   string                `json:"schema_version"`
	EvaluationRunID string                `json:"evaluation_run_id"`
	CaseID          string                `json:"case_id"`
	Observations    []ExposureObservation `json:"observations"`
	ObservedAt      time.Time             `json:"observed_at"`
}

type ExposureEntry struct {
	EventID  string    `json:"event_id"`
	Exposure Exposure  `json:"exposure"`
	Actor    string    `json:"actor"`
	At       time.Time `json:"at"`
}

type Role string

const (
	RoleDatasetCurator       Role = "dataset_curator"
	RoleDatasetReviewer      Role = "dataset_reviewer"
	RoleDatasetAdjudicator   Role = "dataset_adjudicator"
	RoleIncidentIngest       Role = "incident_ingest"
	RoleProbeIngest          Role = "probe_ingest"
	RoleFeedbackIngest       Role = "feedback_ingest"
	RoleHoldoutMaintainer    Role = "holdout_maintainer"
	RoleHoldoutRunner        Role = "holdout_runner"
	RolePromotionOperator    Role = "promotion_operator"
	RolePromotionApprover    Role = "promotion_approver"
	RoleGovernanceTrustAdmin Role = "governance_trust_admin"
)

// Mutation is immutable audit and authorization context. A retry must reuse
// the same idempotency key, actor, roles, audit text, and timestamp.
type Mutation struct {
	IdempotencyKey string    `json:"idempotency_key"`
	Actor          string    `json:"actor"`
	Roles          []Role    `json:"roles"`
	Audit          string    `json:"audit"`
	At             time.Time `json:"at"`
}

type Access struct {
	Actor string `json:"actor"`
	Roles []Role `json:"roles"`
}

type PromotionComponent string

const (
	PromotionRulePack PromotionComponent = "rule_pack"
	PromotionPrompt   PromotionComponent = "prompt"
	PromotionModel    PromotionComponent = "model"
	PromotionWorkflow PromotionComponent = "workflow"
	PromotionFilter   PromotionComponent = "filter"
)

type PromotionOrigin string

const (
	PromotionConfigurationChange PromotionOrigin = "configuration_change"
	PromotionExperiment          PromotionOrigin = "experiment"
	PromotionProductionFeedback  PromotionOrigin = "production_feedback"
)

// PromotionManagementBinding makes a promotion variant non-generic. Such a
// variant may only be advanced by the owning domain service, which must
// revalidate the exact immutable source and configuration facts before it
// records a gate. This prevents a caller from registering a typed variant and
// then bypassing its domain checks through the generic promotion endpoint.
type PromotionManagementBinding struct {
	Kind                                string `json:"kind"`
	CalibrationRunID                    string `json:"calibration_run_id,omitempty"`
	CalibrationRunSHA256                string `json:"calibration_run_sha256,omitempty"`
	ProfileCandidateID                  string `json:"profile_candidate_id,omitempty"`
	ProfileCandidateSHA256              string `json:"profile_candidate_sha256,omitempty"`
	CalibrationReportSHA256             string `json:"calibration_report_sha256,omitempty"`
	BaselineBundleSHA256                string `json:"baseline_bundle_sha256,omitempty"`
	VariantBundleSHA256                 string `json:"variant_bundle_sha256,omitempty"`
	BaselineConfigRevisionID            string `json:"baseline_config_revision_id,omitempty"`
	BaselineConfigRevision              string `json:"baseline_config_revision,omitempty"`
	BaselineConfigRevisionSHA256        string `json:"baseline_config_revision_sha256,omitempty"`
	ConfigRevisionID                    string `json:"config_revision_id,omitempty"`
	ConfigRevision                      string `json:"config_revision,omitempty"`
	ConfigRevisionSHA256                string `json:"config_revision_sha256,omitempty"`
	NormalizationPolicyRevision         string `json:"normalization_policy_revision,omitempty"`
	NormalizationImplementationID       string `json:"normalization_implementation_id,omitempty"`
	NormalizationImplementationRevision string `json:"normalization_implementation_revision,omitempty"`
	NormalizationImplementationSHA256   string `json:"normalization_implementation_sha256,omitempty"`
	NormalizationRollbackRevision       string `json:"normalization_rollback_revision,omitempty"`
	NormalizationRollbackSHA256         string `json:"normalization_rollback_sha256,omitempty"`
	NormalizationGatePolicyID           string `json:"normalization_gate_policy_id,omitempty"`
	NormalizationGatePolicyRevision     string `json:"normalization_gate_policy_revision,omitempty"`
	NormalizationGatePolicyURI          string `json:"normalization_gate_policy_uri,omitempty"`
	NormalizationGatePolicySizeBytes    int64  `json:"normalization_gate_policy_size_bytes,omitempty"`
	NormalizationGatePolicySHA256       string `json:"normalization_gate_policy_sha256,omitempty"`
}

const (
	PromotionManagementCalibrationConfig   = "calibration_config"
	PromotionManagementNormalizationPolicy = "normalization_policy"
)

type PromotionVariant struct {
	SchemaVersion    string                      `json:"schema_version"`
	VariantID        string                      `json:"variant_id"`
	Component        PromotionComponent          `json:"component"`
	Revision         string                      `json:"revision"`
	RollbackRevision string                      `json:"rollback_revision"`
	PolicyRevision   string                      `json:"policy_revision"`
	Origin           PromotionOrigin             `json:"origin"`
	Owner            string                      `json:"owner"`
	CreatedAt        time.Time                   `json:"created_at"`
	ManagedBinding   *PromotionManagementBinding `json:"managed_binding,omitempty"`
}

type PromotionGate string

const (
	GateSchemaContract     PromotionGate = "schema_contract_validation"
	GateTargetedRegression PromotionGate = "targeted_regression"
	GateFixedHoldout       PromotionGate = "fixed_holdout"
	GateShadowTraffic      PromotionGate = "shadow_traffic"
	GateCanary             PromotionGate = "canary"
	GateAuthorization      PromotionGate = "promotion_authorization"
	GateRollbackMonitor    PromotionGate = "rollback_monitor"
)

var promotionGateOrder = [...]PromotionGate{
	GateSchemaContract,
	GateTargetedRegression,
	GateFixedHoldout,
	GateShadowTraffic,
	GateCanary,
	GateAuthorization,
	GateRollbackMonitor,
}

func OrderedPromotionGates() []PromotionGate {
	return append([]PromotionGate(nil), promotionGateOrder[:]...)
}

type GateOutcome string

const (
	GatePass         GateOutcome = "pass"
	GateFail         GateOutcome = "fail"
	GateInconclusive GateOutcome = "inconclusive"
)

type EvidenceBasis string

const (
	EvidenceDeterministic   EvidenceBasis = "deterministic"
	EvidenceHumanCalibrated EvidenceBasis = "human_calibrated"
	EvidenceJudgeOnly       EvidenceBasis = "judge_only"
)

type PromotionAuthorization struct {
	Kind           string `json:"kind"`
	AuthorizedBy   string `json:"authorized_by"`
	PolicyRevision string `json:"policy_revision,omitempty"`
}

type GateEvidence struct {
	Refs                      []string                `json:"refs"`
	Basis                     EvidenceBasis           `json:"basis"`
	ChecksPassed              bool                    `json:"checks_passed"`
	EvaluationRunID           string                  `json:"evaluation_run_id,omitempty"`
	NormalizationQualityRunID string                  `json:"normalization_quality_run_id,omitempty"`
	HoldoutCaseIDs            []string                `json:"holdout_case_ids"`
	SafetyEventCount          int                     `json:"safety_event_count"`
	RollbackVerified          bool                    `json:"rollback_verified"`
	Authorization             *PromotionAuthorization `json:"authorization,omitempty"`
}

type GateResult struct {
	SchemaVersion string        `json:"schema_version"`
	VariantID     string        `json:"variant_id"`
	Gate          PromotionGate `json:"gate"`
	Outcome       GateOutcome   `json:"outcome"`
	Evidence      GateEvidence  `json:"evidence"`
	Summary       string        `json:"summary"`
}

type PromotionStatus string

const (
	PromotionRegistered   PromotionStatus = "registered"
	PromotionInProgress   PromotionStatus = "in_progress"
	PromotionFailed       PromotionStatus = "failed"
	PromotionInconclusive PromotionStatus = "inconclusive"
	PromotionActive       PromotionStatus = "active"
	PromotionRolledBack   PromotionStatus = "rolled_back"
)

type GateEntry struct {
	Sequence         uint64     `json:"sequence"`
	EventID          string     `json:"event_id"`
	Result           GateResult `json:"result"`
	EvidenceRedacted bool       `json:"evidence_redacted"`
	Actor            string     `json:"actor"`
	At               time.Time  `json:"at"`
}

type PromotionRecord struct {
	Variant   PromotionVariant `json:"variant"`
	Status    PromotionStatus  `json:"status"`
	NextGate  *PromotionGate   `json:"next_gate,omitempty"`
	Gates     []GateEntry      `json:"gates"`
	UpdatedAt time.Time        `json:"updated_at"`
	UpdatedBy string           `json:"updated_by"`
}
