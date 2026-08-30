// Package platformapi exposes the authenticated, loopback-only HTTP surface
// used by Argus' local platform profile. It deliberately does not implement
// identity management: one immutable principal is injected at process start.
package platformapi

import (
	"encoding/base64"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"argus.local/argus/internal/analytics"
	"argus.local/argus/internal/analyticsadapter"
	"argus.local/argus/internal/calibration"
	"argus.local/argus/internal/calibrationpromotion"
	"argus.local/argus/internal/configrepo"
	"argus.local/argus/internal/controlplane"
	"argus.local/argus/internal/evaluation"
	"argus.local/argus/internal/feedback"
	"argus.local/argus/internal/findingdecision"
	"argus.local/argus/internal/normalizationpromotion"
	"argus.local/argus/internal/promotionmonitor"
	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/reviewjob"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/runrepo"
	"argus.local/argus/internal/training"
	"argus.local/argus/internal/workflow"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

const (
	PrincipalSchemaVersion                                    = "argus.local_api_principal.v1alpha1"
	MutationInputSchemaVersion                                = "argus.local_api_mutation.v1alpha1"
	GovernanceBatchCommandSchemaVersion                       = "argus.local_api_governance_batch_command.v1alpha1"
	CaseImportCommandSchemaVersion                            = "argus.local_api_case_import_command.v1alpha1"
	TrustKeyCommandSchemaVersion                              = "argus.local_api_trust_key_command.v1alpha1"
	TrustKeyRevocationCommandSchemaVersion                    = "argus.local_api_trust_key_revocation_command.v1alpha1"
	ConfigCreateCommandSchemaVersion                          = "argus.local_api_config_create_command.v1alpha1"
	ConfigTransitionCommandSchemaVersion                      = "argus.local_api_config_transition_command.v1alpha1"
	ConfigResolutionQuerySchemaVersion                        = "argus.local_api_config_resolution_query.v1alpha1"
	ReviewJobSubmitCommandSchemaVersion                       = "argus.local_api_review_job_submit_command.v1alpha1"
	ReviewJobCancelCommandSchemaVersion                       = "argus.local_api_review_job_cancel_command.v1alpha1"
	EvaluationBatchResumeCommandSchemaVersion                 = "argus.local_api_evaluation_batch_resume_command.v1alpha1"
	ExperimentBatchSubmitCommandSchemaVersion                 = "argus.local_api_experiment_batch_submit_command.v1alpha1"
	RepeatabilityBatchSubmitCommandSchemaVersion              = "argus.local_api_repeatability_batch_submit_command.v1alpha1"
	AgentComponentPublishCommandSchemaVersion                 = "argus.local_api_agent_component_publish_command.v1alpha1"
	AgentComponentPublicationRecordSchemaVersion              = "argus.agent_component_publication_record.v1alpha1"
	FindingDecisionWriteCommandSchemaVersion                  = "argus.local_api_finding_decision_write_command.v1alpha1"
	FeedbackWriteCommandSchemaVersion                         = "argus.local_api_feedback_write_command.v1alpha1"
	OutcomeWriteCommandSchemaVersion                          = "argus.local_api_outcome_write_command.v1alpha1"
	CalibrationFitCommandSchemaVersion                        = "argus.local_api_calibration_fit_command.v1alpha1"
	TrainingMaterializeCommandSchemaVersion                   = "argus.local_api_training_materialize_command.v1alpha1"
	TrainingExportBuildCommandSchemaVersion                   = "argus.local_api_training_export_build_command.v1alpha1"
	TrainingJobPrepareCommandSchemaVersion                    = "argus.local_api_training_job_prepare_command.v1alpha1"
	TrainingJobObserveCommandSchemaVersion                    = "argus.local_api_training_job_observe_command.v1alpha1"
	CalibrationPromotionPrepareCommandSchemaVersion           = "argus.local_api_calibration_promotion_prepare_command.v1alpha1"
	CalibrationPromotionGateCommandSchemaVersion              = "argus.local_api_calibration_promotion_gate_command.v1alpha1"
	CalibrationPromotionActivateCommandSchemaVersion          = "argus.local_api_calibration_promotion_activate_command.v1alpha1"
	CalibrationPromotionRollbackCommandSchemaVersion          = "argus.local_api_calibration_promotion_rollback_command.v1alpha1"
	CalibrationPromotionObserveCommandSchemaVersion           = "argus.local_api_calibration_promotion_observe_command.v1alpha1"
	NormalizationPromotionPrepareCommandSchemaVersion         = "argus.local_api_normalization_promotion_prepare_command.v1alpha1"
	NormalizationPromotionGateCommandSchemaVersion            = "argus.local_api_normalization_promotion_gate_command.v1alpha1"
	NormalizationPromotionOperationalGateCommandSchemaVersion = "argus.local_api_normalization_promotion_operational_gate_command.v1alpha1"
	NormalizationPromotionCanaryCommandSchemaVersion          = "argus.local_api_normalization_promotion_canary_command.v1alpha1"
	NormalizationPromotionLifecycleCommandSchemaVersion       = "argus.local_api_normalization_promotion_lifecycle_command.v1alpha1"
	ResponseSchemaVersion                                     = "argus.local_api_response.v1alpha1"
	ErrorSchemaVersion                                        = "argus.local_api_error.v1alpha1"
)

type NormalizationPromotionPrepareCommand struct {
	SchemaVersion string                                `json:"schema_version"`
	Request       normalizationpromotion.PrepareRequest `json:"request"`
	Mutation      MutationInput                         `json:"mutation"`
}

func (command NormalizationPromotionPrepareCommand) Validate() error {
	if command.SchemaVersion != NormalizationPromotionPrepareCommandSchemaVersion {
		return fmt.Errorf("unsupported normalization promotion prepare command schema %q", command.SchemaVersion)
	}
	if err := command.Request.Validate(); err != nil {
		return fmt.Errorf("request: %w", err)
	}
	if err := command.Mutation.Validate(); err != nil {
		return fmt.Errorf("mutation: %w", err)
	}
	if !command.Request.CreatedAt.Equal(command.Mutation.At.UTC()) {
		return fmt.Errorf("request created_at must equal mutation at")
	}
	return nil
}

func DecodeNormalizationPromotionPrepareCommand(data []byte) (NormalizationPromotionPrepareCommand, error) {
	return decodeAndValidate(data, func(value NormalizationPromotionPrepareCommand) error { return value.Validate() })
}

type NormalizationPromotionGateCommand struct {
	SchemaVersion string                             `json:"schema_version"`
	Request       normalizationpromotion.GateRequest `json:"request"`
	Mutation      MutationInput                      `json:"mutation"`
}

type NormalizationPromotionOperationalGateCommand struct {
	SchemaVersion string                                        `json:"schema_version"`
	Request       normalizationpromotion.OperationalGateRequest `json:"request"`
	Mutation      MutationInput                                 `json:"mutation"`
}

func (command NormalizationPromotionOperationalGateCommand) Validate() error {
	if command.SchemaVersion != NormalizationPromotionOperationalGateCommandSchemaVersion {
		return fmt.Errorf("unsupported normalization promotion operational gate command schema %q", command.SchemaVersion)
	}
	if err := command.Request.Validate(); err != nil {
		return fmt.Errorf("request: %w", err)
	}
	if err := command.Mutation.Validate(); err != nil {
		return fmt.Errorf("mutation: %w", err)
	}
	if !command.Request.EvaluatedAt.Equal(command.Mutation.At.UTC()) {
		return fmt.Errorf("request evaluated_at must equal mutation at")
	}
	return nil
}

func DecodeNormalizationPromotionOperationalGateCommand(data []byte) (NormalizationPromotionOperationalGateCommand, error) {
	return decodeAndValidate(data, func(value NormalizationPromotionOperationalGateCommand) error { return value.Validate() })
}

type NormalizationPromotionCanaryCommand struct {
	SchemaVersion string               `json:"schema_version"`
	VariantID     string               `json:"variant_id"`
	PolicyRef     runmodel.ArtifactRef `json:"policy_ref"`
	Rollout       configrepo.Rollout   `json:"rollout"`
	Mutation      MutationInput        `json:"mutation"`
}

func (command NormalizationPromotionCanaryCommand) Validate() error {
	if command.SchemaVersion != NormalizationPromotionCanaryCommandSchemaVersion || command.VariantID == "" {
		return fmt.Errorf("normalization promotion canary command identity is invalid")
	}
	if err := command.PolicyRef.Validate(); err != nil || command.PolicyRef.Contract != normalizationpromotion.PolicyContract {
		return fmt.Errorf("policy_ref is invalid")
	}
	if err := command.Rollout.Validate(); err != nil || command.Rollout.Percentage >= 100 {
		return fmt.Errorf("rollout must be a percentage canary")
	}
	return command.Mutation.Validate()
}

func DecodeNormalizationPromotionCanaryCommand(data []byte) (NormalizationPromotionCanaryCommand, error) {
	return decodeAndValidate(data, func(value NormalizationPromotionCanaryCommand) error { return value.Validate() })
}

type NormalizationPromotionLifecycleCommand struct {
	SchemaVersion string        `json:"schema_version"`
	VariantID     string        `json:"variant_id"`
	Mutation      MutationInput `json:"mutation"`
}

func (command NormalizationPromotionLifecycleCommand) Validate() error {
	if command.SchemaVersion != NormalizationPromotionLifecycleCommandSchemaVersion || command.VariantID == "" {
		return fmt.Errorf("normalization promotion lifecycle command identity is invalid")
	}
	return command.Mutation.Validate()
}

func DecodeNormalizationPromotionLifecycleCommand(data []byte) (NormalizationPromotionLifecycleCommand, error) {
	return decodeAndValidate(data, func(value NormalizationPromotionLifecycleCommand) error { return value.Validate() })
}

func (command NormalizationPromotionGateCommand) Validate() error {
	if command.SchemaVersion != NormalizationPromotionGateCommandSchemaVersion {
		return fmt.Errorf("unsupported normalization promotion gate command schema %q", command.SchemaVersion)
	}
	if err := command.Request.Validate(); err != nil {
		return fmt.Errorf("request: %w", err)
	}
	if err := command.Mutation.Validate(); err != nil {
		return fmt.Errorf("mutation: %w", err)
	}
	if !command.Request.EvaluatedAt.Equal(command.Mutation.At.UTC()) {
		return fmt.Errorf("request evaluated_at must equal mutation at")
	}
	return nil
}

func DecodeNormalizationPromotionGateCommand(data []byte) (NormalizationPromotionGateCommand, error) {
	return decodeAndValidate(data, func(value NormalizationPromotionGateCommand) error { return value.Validate() })
}

type CalibrationPromotionPrepareCommand struct {
	SchemaVersion string                              `json:"schema_version"`
	Request       calibrationpromotion.PrepareRequest `json:"request"`
	Mutation      MutationInput                       `json:"mutation"`
}

func (command CalibrationPromotionPrepareCommand) Validate() error {
	if command.SchemaVersion != CalibrationPromotionPrepareCommandSchemaVersion {
		return fmt.Errorf("unsupported calibration promotion prepare command schema %q", command.SchemaVersion)
	}
	if err := command.Request.Validate(); err != nil {
		return fmt.Errorf("request: %w", err)
	}
	if err := command.Mutation.Validate(); err != nil {
		return fmt.Errorf("mutation: %w", err)
	}
	if !command.Request.CreatedAt.Equal(command.Mutation.At.UTC()) {
		return fmt.Errorf("request created_at must equal mutation at")
	}
	return nil
}
func DecodeCalibrationPromotionPrepareCommand(data []byte) (CalibrationPromotionPrepareCommand, error) {
	var value CalibrationPromotionPrepareCommand
	if err := decodeStrictInto(data, &value); err != nil {
		return value, err
	}
	return value, value.Validate()
}

type CalibrationPromotionGateCommand struct {
	SchemaVersion string                           `json:"schema_version"`
	Request       calibrationpromotion.GateRequest `json:"request"`
	Mutation      MutationInput                    `json:"mutation"`
}

func (command CalibrationPromotionGateCommand) Validate() error {
	if command.SchemaVersion != CalibrationPromotionGateCommandSchemaVersion {
		return fmt.Errorf("unsupported calibration promotion gate command schema %q", command.SchemaVersion)
	}
	if err := command.Request.Validate(); err != nil {
		return err
	}
	return command.Mutation.Validate()
}
func DecodeCalibrationPromotionGateCommand(data []byte) (CalibrationPromotionGateCommand, error) {
	var value CalibrationPromotionGateCommand
	if err := decodeStrictInto(data, &value); err != nil {
		return value, err
	}
	return value, value.Validate()
}

type CalibrationPromotionActivateCommand struct {
	SchemaVersion string             `json:"schema_version"`
	PlanID        string             `json:"plan_id"`
	Rollout       configrepo.Rollout `json:"rollout"`
	Mutation      MutationInput      `json:"mutation"`
}

func (command CalibrationPromotionActivateCommand) Validate() error {
	if command.SchemaVersion != CalibrationPromotionActivateCommandSchemaVersion {
		return fmt.Errorf("unsupported calibration promotion activate command schema %q", command.SchemaVersion)
	}
	if strings.TrimSpace(command.PlanID) == "" {
		return fmt.Errorf("plan_id is required")
	}
	if err := command.Rollout.Validate(); err != nil {
		return err
	}
	return command.Mutation.Validate()
}
func DecodeCalibrationPromotionActivateCommand(data []byte) (CalibrationPromotionActivateCommand, error) {
	var value CalibrationPromotionActivateCommand
	if err := decodeStrictInto(data, &value); err != nil {
		return value, err
	}
	return value, value.Validate()
}

type CalibrationPromotionRollbackCommand struct {
	SchemaVersion string        `json:"schema_version"`
	PlanID        string        `json:"plan_id"`
	Mutation      MutationInput `json:"mutation"`
}

type CalibrationPromotionObserveCommand struct {
	SchemaVersion string                        `json:"schema_version"`
	Request       promotionmonitor.BuildRequest `json:"request"`
	Mutation      MutationInput                 `json:"mutation"`
}

func (command CalibrationPromotionObserveCommand) Validate() error {
	if command.SchemaVersion != CalibrationPromotionObserveCommandSchemaVersion {
		return fmt.Errorf("unsupported calibration promotion observe command schema %q", command.SchemaVersion)
	}
	if err := command.Request.Validate(); err != nil {
		return fmt.Errorf("request: %w", err)
	}
	if err := command.Mutation.Validate(); err != nil {
		return fmt.Errorf("mutation: %w", err)
	}
	if !command.Request.ObservedAt.Equal(command.Mutation.At.UTC()) {
		return fmt.Errorf("request observed_at must equal mutation at")
	}
	return nil
}

func DecodeCalibrationPromotionObserveCommand(data []byte) (CalibrationPromotionObserveCommand, error) {
	var value CalibrationPromotionObserveCommand
	if err := decodeStrictInto(data, &value); err != nil {
		return value, err
	}
	return value, value.Validate()
}

func (command CalibrationPromotionRollbackCommand) Validate() error {
	if command.SchemaVersion != CalibrationPromotionRollbackCommandSchemaVersion {
		return fmt.Errorf("unsupported calibration promotion rollback command schema %q", command.SchemaVersion)
	}
	if strings.TrimSpace(command.PlanID) == "" {
		return fmt.Errorf("plan_id is required")
	}
	return command.Mutation.Validate()
}
func DecodeCalibrationPromotionRollbackCommand(data []byte) (CalibrationPromotionRollbackCommand, error) {
	var value CalibrationPromotionRollbackCommand
	if err := decodeStrictInto(data, &value); err != nil {
		return value, err
	}
	return value, value.Validate()
}

type CalibrationFitCommand struct {
	SchemaVersion string                 `json:"schema_version"`
	Request       calibration.FitRequest `json:"request"`
	Mutation      MutationInput          `json:"mutation"`
}

type TrainingMaterializeCommand struct {
	SchemaVersion string                          `json:"schema_version"`
	Request       training.MaterializationRequest `json:"request"`
	Mutation      MutationInput                   `json:"mutation"`
}

func (command TrainingMaterializeCommand) Validate() error {
	if command.SchemaVersion != TrainingMaterializeCommandSchemaVersion {
		return fmt.Errorf("unsupported training materialize command schema %q", command.SchemaVersion)
	}
	if err := command.Request.Validate(); err != nil {
		return fmt.Errorf("request: %w", err)
	}
	if err := command.Mutation.Validate(); err != nil {
		return fmt.Errorf("mutation: %w", err)
	}
	if !command.Request.CreatedAt.Equal(command.Mutation.At.UTC()) {
		return fmt.Errorf("request created_at must equal mutation at")
	}
	return nil
}

func DecodeTrainingMaterializeCommand(data []byte) (TrainingMaterializeCommand, error) {
	var command TrainingMaterializeCommand
	if err := decodeStrictInto(data, &command); err != nil {
		return command, fmt.Errorf("decode TrainingMaterializeCommand: %w", err)
	}
	if err := command.Validate(); err != nil {
		return command, fmt.Errorf("validate TrainingMaterializeCommand: %w", err)
	}
	return command, nil
}

type TrainingExportBuildCommand struct {
	SchemaVersion string                 `json:"schema_version"`
	Request       training.ExportRequest `json:"request"`
	Mutation      MutationInput          `json:"mutation"`
}

func (command TrainingExportBuildCommand) Validate() error {
	if command.SchemaVersion != TrainingExportBuildCommandSchemaVersion {
		return fmt.Errorf("unsupported training export build command schema %q", command.SchemaVersion)
	}
	if err := command.Request.Validate(); err != nil {
		return fmt.Errorf("request: %w", err)
	}
	if err := command.Mutation.Validate(); err != nil {
		return fmt.Errorf("mutation: %w", err)
	}
	if !command.Request.CreatedAt.Equal(command.Mutation.At.UTC()) {
		return fmt.Errorf("request created_at must equal mutation at")
	}
	return nil
}

func DecodeTrainingExportBuildCommand(data []byte) (TrainingExportBuildCommand, error) {
	var command TrainingExportBuildCommand
	if err := decodeStrictInto(data, &command); err != nil {
		return command, fmt.Errorf("decode TrainingExportBuildCommand: %w", err)
	}
	if err := command.Validate(); err != nil {
		return command, fmt.Errorf("validate TrainingExportBuildCommand: %w", err)
	}
	return command, nil
}

type TrainingJobPrepareCommand struct {
	SchemaVersion string                     `json:"schema_version"`
	Request       training.JobPrepareRequest `json:"request"`
	Mutation      MutationInput              `json:"mutation"`
}

func (command TrainingJobPrepareCommand) Validate() error {
	if command.SchemaVersion != TrainingJobPrepareCommandSchemaVersion {
		return fmt.Errorf("unsupported training job prepare command schema %q", command.SchemaVersion)
	}
	if err := command.Request.Validate(); err != nil {
		return fmt.Errorf("request: %w", err)
	}
	if err := command.Mutation.Validate(); err != nil {
		return fmt.Errorf("mutation: %w", err)
	}
	if !command.Request.CreatedAt.Equal(command.Mutation.At.UTC()) {
		return fmt.Errorf("request created_at must equal mutation at")
	}
	return nil
}

func DecodeTrainingJobPrepareCommand(data []byte) (TrainingJobPrepareCommand, error) {
	var command TrainingJobPrepareCommand
	if err := decodeStrictInto(data, &command); err != nil {
		return command, fmt.Errorf("decode TrainingJobPrepareCommand: %w", err)
	}
	if err := command.Validate(); err != nil {
		return command, fmt.Errorf("validate TrainingJobPrepareCommand: %w", err)
	}
	return command, nil
}

type TrainingJobObserveCommand struct {
	SchemaVersion string                         `json:"schema_version"`
	Request       training.JobObservationRequest `json:"request"`
	Mutation      MutationInput                  `json:"mutation"`
}

func (command TrainingJobObserveCommand) Validate() error {
	if command.SchemaVersion != TrainingJobObserveCommandSchemaVersion {
		return fmt.Errorf("unsupported training job observe command schema %q", command.SchemaVersion)
	}
	if err := command.Request.Validate(); err != nil {
		return fmt.Errorf("request: %w", err)
	}
	if err := command.Mutation.Validate(); err != nil {
		return fmt.Errorf("mutation: %w", err)
	}
	if !command.Request.ObservedAt.Equal(command.Mutation.At.UTC()) {
		return fmt.Errorf("request observed_at must equal mutation at")
	}
	return nil
}

func DecodeTrainingJobObserveCommand(data []byte) (TrainingJobObserveCommand, error) {
	var command TrainingJobObserveCommand
	if err := decodeStrictInto(data, &command); err != nil {
		return command, fmt.Errorf("decode TrainingJobObserveCommand: %w", err)
	}
	if err := command.Validate(); err != nil {
		return command, fmt.Errorf("validate TrainingJobObserveCommand: %w", err)
	}
	return command, nil
}

func (command CalibrationFitCommand) Validate() error {
	if command.SchemaVersion != CalibrationFitCommandSchemaVersion {
		return fmt.Errorf("unsupported calibration fit command schema %q", command.SchemaVersion)
	}
	if err := command.Request.Validate(); err != nil {
		return fmt.Errorf("request: %w", err)
	}
	if err := command.Mutation.Validate(); err != nil {
		return fmt.Errorf("mutation: %w", err)
	}
	if !command.Request.CreatedAt.Equal(command.Mutation.At.UTC()) {
		return fmt.Errorf("request created_at must equal mutation at")
	}
	return nil
}

func DecodeCalibrationFitCommand(data []byte) (CalibrationFitCommand, error) {
	var command CalibrationFitCommand
	if err := decodeStrictInto(data, &command); err != nil {
		return CalibrationFitCommand{}, fmt.Errorf("decode CalibrationFitCommand: %w", err)
	}
	if err := command.Validate(); err != nil {
		return CalibrationFitCommand{}, fmt.Errorf("validate CalibrationFitCommand: %w", err)
	}
	return command, nil
}

// AgentComponentPublishCommand publishes exact behavior-bearing bytes into
// the subject derived from one committed baseline ReviewRun. It intentionally
// contains neither caller-declared subject/actor nor a local filesystem path.
type AgentComponentPublishCommand struct {
	SchemaVersion       string                    `json:"schema_version"`
	BaselineReviewRunID string                    `json:"baseline_review_run_id"`
	Contract            string                    `json:"contract"`
	Ref                 reviewconfig.VersionedRef `json:"ref"`
	ContentBase64       string                    `json:"content_base64"`
	Mutation            MutationInput             `json:"mutation"`
}

type AgentComponentPublicationRequest struct {
	BaselineReviewRunID string
	Contract            string
	Ref                 reviewconfig.VersionedRef
	Content             []byte
}

type AgentComponentPublicationMutation struct {
	IdempotencyKey string
	Actor          string
	Audit          string
	At             time.Time
}

type AgentComponentPublicationRecord struct {
	SchemaVersion       string                                       `json:"schema_version"`
	BaselineReviewRunID string                                       `json:"baseline_review_run_id"`
	Contract            string                                       `json:"contract"`
	Binding             contractsv1alpha1.AgentStageComponentBinding `json:"binding"`
	PublishedBy         string                                       `json:"published_by"`
	PublishedAt         time.Time                                    `json:"published_at"`
}

func (command AgentComponentPublishCommand) Validate() error {
	if command.SchemaVersion != AgentComponentPublishCommandSchemaVersion {
		return fmt.Errorf("unsupported agent component publish command schema %q", command.SchemaVersion)
	}
	if err := validateIdentifier("baseline_review_run_id", command.BaselineReviewRunID, 256); err != nil {
		return err
	}
	switch command.Contract {
	case contractsv1alpha1.AgentStagePlanPromptContract,
		contractsv1alpha1.AgentStagePlanSkillContract,
		contractsv1alpha1.AgentStagePlanKnowledgeContract:
	default:
		return fmt.Errorf("platform publication only accepts prompt, skill, or knowledge contracts")
	}
	if err := command.Ref.Validate(); err != nil {
		return fmt.Errorf("ref: %w", err)
	}
	content, err := command.content()
	if err != nil {
		return err
	}
	ref := contractsv1alpha1.VersionedRef{
		ID: command.Ref.ID, Revision: command.Ref.Revision, SHA256: command.Ref.SHA256,
	}
	artifact := contractsv1alpha1.ArtifactBinding{
		Ref: contractsv1alpha1.ContentRef{
			URI:    "artifact://publication-preflight/sha256/" + command.Ref.SHA256,
			SHA256: command.Ref.SHA256, SizeBytes: int64(len(content)),
		},
		Contract: command.Contract,
	}
	switch command.Contract {
	case contractsv1alpha1.AgentStagePlanPromptContract:
		if _, err := contractsv1alpha1.NewAgentReviewWorkerPromptBundle(ref, artifact, content); err != nil {
			return fmt.Errorf("prompt content: %w", err)
		}
	case contractsv1alpha1.AgentStagePlanSkillContract:
		if _, err := contractsv1alpha1.NewAgentReviewWorkerSkill(ref, artifact, content); err != nil {
			return fmt.Errorf("skill content: %w", err)
		}
	case contractsv1alpha1.AgentStagePlanKnowledgeContract:
		if _, err := contractsv1alpha1.NewAgentReviewWorkerKnowledge(ref, artifact, content); err != nil {
			return fmt.Errorf("knowledge content: %w", err)
		}
	}
	if err := command.Mutation.Validate(); err != nil {
		return fmt.Errorf("mutation: %w", err)
	}
	return nil
}

func (command AgentComponentPublishCommand) content() ([]byte, error) {
	if command.ContentBase64 == "" {
		return nil, fmt.Errorf("content_base64 is required")
	}
	content, err := base64.StdEncoding.DecodeString(command.ContentBase64)
	if err != nil || base64.StdEncoding.EncodeToString(content) != command.ContentBase64 {
		return nil, fmt.Errorf("content_base64 must use canonical standard base64")
	}
	return content, nil
}

func (command AgentComponentPublishCommand) publication(
	principal Principal,
) (AgentComponentPublicationRequest, AgentComponentPublicationMutation, error) {
	if err := command.Validate(); err != nil {
		return AgentComponentPublicationRequest{}, AgentComponentPublicationMutation{}, err
	}
	content, _ := command.content()
	return AgentComponentPublicationRequest{
			BaselineReviewRunID: command.BaselineReviewRunID,
			Contract:            command.Contract, Ref: command.Ref, Content: content,
		}, AgentComponentPublicationMutation{
			IdempotencyKey: command.Mutation.IdempotencyKey,
			Actor:          principal.Actor, Audit: command.Mutation.Audit, At: command.Mutation.At.UTC(),
		}, nil
}

const maximumPlatformBudgetTimeoutMS int64 = 24 * 60 * 60 * 1000

// ExperimentBatchSubmitCommand admits a self-contained budget or configured-
// provider model variant, or exact references to already-published governed
// components. The local adapter supplies the credential-free executor
// template; a caller cannot smuggle local paths, provider credentials, or a
// pre-existing template.
type ExperimentBatchSubmitCommand struct {
	SchemaVersion      string                                    `json:"schema_version"`
	Request            evaluation.ExperimentBatchRequest         `json:"request"`
	BudgetTimeoutMS    *int64                                    `json:"budget_timeout_ms,omitempty"`
	ComponentRefs      []reviewconfig.VersionedRef               `json:"component_refs,omitempty"`
	ContextProviders   *[]reviewconfig.ContextProviderDefinition `json:"context_providers,omitempty"`
	RulePack           *reviewconfig.RulePack                    `json:"rule_pack,omitempty"`
	WorkflowDefinition *workflow.Definition                      `json:"workflow_definition,omitempty"`
	Model              string                                    `json:"model,omitempty"`
	FindingGovernance  *reviewconfig.FindingGovernancePolicy     `json:"finding_governance,omitempty"`
	Mutation           MutationInput                             `json:"mutation"`
}

type ExperimentBatchExecutionVariant struct {
	Variable           runmodel.ReplayVariable
	BudgetTimeoutMS    int64
	ComponentRefs      []reviewconfig.VersionedRef
	ContextProviders   []reviewconfig.ContextProviderDefinition
	RulePack           *reviewconfig.RulePack
	WorkflowDefinition *workflow.Definition
	Model              string
	FindingGovernance  *reviewconfig.FindingGovernancePolicy
}

func (command ExperimentBatchSubmitCommand) Validate() error {
	if command.SchemaVersion != ExperimentBatchSubmitCommandSchemaVersion {
		return fmt.Errorf("unsupported experiment batch submit command schema %q", command.SchemaVersion)
	}
	if err := command.Request.Validate(); err != nil {
		return fmt.Errorf("request: %w", err)
	}
	if command.Request.ExecutorTemplateRef != nil {
		return fmt.Errorf("request executor_template_ref must be omitted; the platform freezes it")
	}
	if _, err := command.executionVariant(); err != nil {
		return err
	}
	if err := command.Mutation.Validate(); err != nil {
		return fmt.Errorf("mutation: %w", err)
	}
	if !command.Request.CreatedAt.Equal(command.Mutation.At.UTC()) {
		return fmt.Errorf("request created_at must equal mutation at")
	}
	if command.Request.CreatedAt.Location() != time.UTC {
		return fmt.Errorf("request created_at must use UTC")
	}
	return nil
}

func (command ExperimentBatchSubmitCommand) executionVariant() (ExperimentBatchExecutionVariant, error) {
	variant := ExperimentBatchExecutionVariant{Variable: command.Request.Variable}
	switch command.Request.Variable {
	case runmodel.ReplayVariableBudget:
		if command.BudgetTimeoutMS == nil || *command.BudgetTimeoutMS < 1 ||
			*command.BudgetTimeoutMS > maximumPlatformBudgetTimeoutMS || command.ComponentRefs != nil ||
			command.ContextProviders != nil || command.RulePack != nil ||
			command.WorkflowDefinition != nil || command.Model != "" || command.FindingGovernance != nil {
			return ExperimentBatchExecutionVariant{}, fmt.Errorf(
				"budget experiment requires only budget_timeout_ms between 1 and %d",
				maximumPlatformBudgetTimeoutMS,
			)
		}
		variant.BudgetTimeoutMS = *command.BudgetTimeoutMS
	case runmodel.ReplayVariableModel:
		if command.BudgetTimeoutMS != nil || command.ComponentRefs != nil ||
			command.ContextProviders != nil || command.RulePack != nil ||
			command.WorkflowDefinition != nil || command.FindingGovernance != nil {
			return ExperimentBatchExecutionVariant{}, fmt.Errorf("model experiment must omit budget_timeout_ms and component_refs")
		}
		if err := validateIdentifier("model", command.Model, 256); err != nil {
			return ExperimentBatchExecutionVariant{}, err
		}
		variant.Model = command.Model
	case runmodel.ReplayVariablePrompt, runmodel.ReplayVariableSkillPack,
		runmodel.ReplayVariableKnowledgePack:
		if command.BudgetTimeoutMS != nil || command.ContextProviders != nil ||
			command.RulePack != nil || command.WorkflowDefinition != nil ||
			command.Model != "" || command.FindingGovernance != nil {
			return ExperimentBatchExecutionVariant{}, fmt.Errorf("component experiment must omit budget_timeout_ms and model")
		}
		maximum := 1
		if command.Request.Variable == runmodel.ReplayVariableSkillPack {
			maximum = contractsv1alpha1.AgentReviewWorkerMaxSkillCount
		} else if command.Request.Variable == runmodel.ReplayVariableKnowledgePack {
			maximum = contractsv1alpha1.AgentReviewWorkerMaxKnowledgeCount
		}
		if len(command.ComponentRefs) == 0 || len(command.ComponentRefs) > maximum ||
			(command.Request.Variable == runmodel.ReplayVariablePrompt && len(command.ComponentRefs) != 1) {
			return ExperimentBatchExecutionVariant{}, fmt.Errorf(
				"component_refs has an invalid count for %s", command.Request.Variable,
			)
		}
		previous := ""
		for index, ref := range command.ComponentRefs {
			if err := ref.Validate(); err != nil {
				return ExperimentBatchExecutionVariant{}, fmt.Errorf("component_refs[%d]: %w", index, err)
			}
			if index > 0 && ref.ID <= previous {
				return ExperimentBatchExecutionVariant{}, fmt.Errorf("component_refs must be uniquely sorted by id")
			}
			previous = ref.ID
		}
		variant.ComponentRefs = slices.Clone(command.ComponentRefs)
	case runmodel.ReplayVariableIndex:
		if command.BudgetTimeoutMS != nil || command.ComponentRefs != nil ||
			command.ContextProviders == nil || command.Model != "" ||
			command.RulePack != nil || command.WorkflowDefinition != nil || command.FindingGovernance != nil {
			return ExperimentBatchExecutionVariant{}, fmt.Errorf(
				"index experiment requires only a non-empty ordered context_providers array",
			)
		}
		providers := *command.ContextProviders
		if len(providers) == 0 || len(providers) > contractsv1alpha1.AgentReviewWorkerMaxContextCount {
			return ExperimentBatchExecutionVariant{}, fmt.Errorf(
				"context_providers count must be between 1 and %d",
				contractsv1alpha1.AgentReviewWorkerMaxContextCount,
			)
		}
		seen := make(map[string]struct{}, len(providers))
		for index, provider := range providers {
			if err := provider.Validate(); err != nil {
				return ExperimentBatchExecutionVariant{}, fmt.Errorf(
					"context_providers[%d]: %w", index, err,
				)
			}
			if _, duplicate := seen[provider.ID]; duplicate {
				return ExperimentBatchExecutionVariant{}, fmt.Errorf(
					"context_providers contains duplicate id %q", provider.ID,
				)
			}
			seen[provider.ID] = struct{}{}
		}
		variant.ContextProviders = slices.Clone(providers)
	case runmodel.ReplayVariableRulePack:
		if command.BudgetTimeoutMS != nil || command.ComponentRefs != nil ||
			command.ContextProviders != nil || command.Model != "" ||
			command.FindingGovernance != nil || command.WorkflowDefinition != nil || command.RulePack == nil {
			return ExperimentBatchExecutionVariant{}, fmt.Errorf(
				"rule_pack experiment requires only one sealed rule_pack",
			)
		}
		if err := command.RulePack.Validate(); err != nil {
			return ExperimentBatchExecutionVariant{}, fmt.Errorf("rule_pack: %w", err)
		}
		pack := clonePlatformRulePack(*command.RulePack)
		variant.RulePack = &pack
	case runmodel.ReplayVariableWorkflow:
		if command.BudgetTimeoutMS != nil || command.ComponentRefs != nil ||
			command.ContextProviders != nil || command.Model != "" || command.RulePack != nil ||
			command.FindingGovernance != nil || command.WorkflowDefinition == nil {
			return ExperimentBatchExecutionVariant{}, fmt.Errorf(
				"workflow experiment requires only one exact workflow_definition",
			)
		}
		if err := command.WorkflowDefinition.Validate(); err != nil {
			return ExperimentBatchExecutionVariant{}, fmt.Errorf("workflow_definition: %w", err)
		}
		definition := clonePlatformWorkflow(*command.WorkflowDefinition)
		variant.WorkflowDefinition = &definition
	case runmodel.ReplayVariableFilterPolicy, runmodel.ReplayVariableFindingGovernance:
		if command.BudgetTimeoutMS != nil || command.ComponentRefs != nil || command.Model != "" ||
			command.ContextProviders != nil || command.RulePack != nil ||
			command.WorkflowDefinition != nil || command.FindingGovernance == nil {
			return ExperimentBatchExecutionVariant{}, fmt.Errorf(
				"finding_governance experiment requires only finding_governance",
			)
		}
		if err := command.FindingGovernance.Validate(); err != nil {
			return ExperimentBatchExecutionVariant{}, fmt.Errorf("finding_governance: %w", err)
		}
		policy := *command.FindingGovernance
		policy.CalibrationProfile.Points = slices.Clone(policy.CalibrationProfile.Points)
		variant.FindingGovernance = &policy
	default:
		return ExperimentBatchExecutionVariant{}, fmt.Errorf(
			"platform experiment submission supports budget, model, prompt, skill_pack, knowledge_pack, rule_pack, workflow, index, or filter_policy",
		)
	}
	return variant, nil
}

func clonePlatformWorkflow(definition workflow.Definition) workflow.Definition {
	cloned := definition
	cloned.Stages = slices.Clone(definition.Stages)
	for index := range cloned.Stages {
		cloned.Stages[index].DependsOn = slices.Clone(definition.Stages[index].DependsOn)
		cloned.Stages[index].RequiredCapabilities = slices.Clone(definition.Stages[index].RequiredCapabilities)
		cloned.Stages[index].Retry.RetryableCodes = slices.Clone(definition.Stages[index].Retry.RetryableCodes)
		if definition.Stages[index].AuthorityCeiling != nil {
			ceiling := *definition.Stages[index].AuthorityCeiling
			ceiling.AllowedTools = slices.Clone(definition.Stages[index].AuthorityCeiling.AllowedTools)
			cloned.Stages[index].AuthorityCeiling = &ceiling
		}
	}
	return cloned
}

func clonePlatformRulePack(pack reviewconfig.RulePack) reviewconfig.RulePack {
	cloned := pack
	cloned.Rules = slices.Clone(pack.Rules)
	for index := range cloned.Rules {
		cloned.Rules[index].Languages = slices.Clone(pack.Rules[index].Languages)
		cloned.Rules[index].PathPrefixes = slices.Clone(pack.Rules[index].PathPrefixes)
		cloned.Rules[index].EvidenceKinds = slices.Clone(pack.Rules[index].EvidenceKinds)
	}
	return cloned
}

func (command ExperimentBatchSubmitCommand) mutation(principal Principal) (evaluation.Mutation, error) {
	if err := command.Validate(); err != nil {
		return evaluation.Mutation{}, err
	}
	return command.Mutation.mutation(principal)
}

// RepeatabilityBatchSubmitCommand admits exact replay only. There is no
// caller-controlled variant, path, or executor template in this surface.
type RepeatabilityBatchSubmitCommand struct {
	SchemaVersion string                               `json:"schema_version"`
	Request       evaluation.RepeatabilityBatchRequest `json:"request"`
	Mutation      MutationInput                        `json:"mutation"`
}

func (command RepeatabilityBatchSubmitCommand) Validate() error {
	if command.SchemaVersion != RepeatabilityBatchSubmitCommandSchemaVersion {
		return fmt.Errorf("unsupported repeatability batch submit command schema %q", command.SchemaVersion)
	}
	if err := command.Request.Validate(); err != nil {
		return fmt.Errorf("request: %w", err)
	}
	if command.Request.ExecutorTemplateRef != nil {
		return fmt.Errorf("request executor_template_ref must be omitted; the platform freezes it")
	}
	if err := command.Mutation.Validate(); err != nil {
		return fmt.Errorf("mutation: %w", err)
	}
	if !command.Request.CreatedAt.Equal(command.Mutation.At.UTC()) {
		return fmt.Errorf("request created_at must equal mutation at")
	}
	if command.Request.CreatedAt.Location() != time.UTC {
		return fmt.Errorf("request created_at must use UTC")
	}
	return nil
}

func (command RepeatabilityBatchSubmitCommand) mutation(principal Principal) (evaluation.Mutation, error) {
	if err := command.Validate(); err != nil {
		return evaluation.Mutation{}, err
	}
	return command.Mutation.mutation(principal)
}

type EvaluationBatchResumeCommand struct {
	SchemaVersion string        `json:"schema_version"`
	Mutation      MutationInput `json:"mutation"`
}

func (command EvaluationBatchResumeCommand) Validate() error {
	if command.SchemaVersion != EvaluationBatchResumeCommandSchemaVersion {
		return fmt.Errorf(
			"unsupported evaluation batch resume command schema %q", command.SchemaVersion,
		)
	}
	return command.Mutation.Validate()
}

func (command EvaluationBatchResumeCommand) mutation(principal Principal) (evaluation.Mutation, error) {
	if err := command.Validate(); err != nil {
		return evaluation.Mutation{}, err
	}
	return command.Mutation.mutation(principal)
}

type Permission string

const (
	PermissionComponentWrite  Permission = "component_write"
	PermissionConfigRead      Permission = "config_read"
	PermissionConfigWrite     Permission = "config_write"
	PermissionDashboardRead   Permission = "dashboard_read"
	PermissionEvaluationRead  Permission = "evaluation_read"
	PermissionEvaluationWrite Permission = "evaluation_write"
	PermissionReviewExecute   Permission = "review_execute"
	PermissionReviewRead      Permission = "review_read"
	PermissionReviewWrite     Permission = "review_write"
)

type Principal struct {
	SchemaVersion   string                    `json:"schema_version"`
	Actor           string                    `json:"actor"`
	ActorKind       findingdecision.ActorKind `json:"actor_kind,omitempty"`
	Roles           []evaluation.Role         `json:"roles"`
	FindingRoles    []findingdecision.Role    `json:"finding_roles,omitempty"`
	Permissions     []Permission              `json:"permissions"`
	ProfileRevision string                    `json:"profile_revision"`
}

func (principal Principal) Validate() error {
	if principal.SchemaVersion != PrincipalSchemaVersion {
		return fmt.Errorf("unsupported local API principal schema %q", principal.SchemaVersion)
	}
	if err := validateIdentifier("actor", principal.Actor, 256); err != nil {
		return err
	}
	if principal.Roles == nil {
		return fmt.Errorf("roles must be an explicit array")
	}
	if len(principal.Roles) > 0 {
		if err := (evaluation.Access{Actor: principal.Actor, Roles: principal.Roles}).Validate(); err != nil {
			return err
		}
	}
	if len(principal.Permissions) == 0 {
		return fmt.Errorf("permissions must be a non-empty array")
	}
	for index, permission := range principal.Permissions {
		switch permission {
		case PermissionComponentWrite, PermissionConfigRead, PermissionConfigWrite, PermissionDashboardRead,
			PermissionEvaluationRead, PermissionEvaluationWrite, PermissionReviewExecute,
			PermissionReviewRead, PermissionReviewWrite:
		default:
			return fmt.Errorf("permissions[%d] contains unsupported permission %q", index, permission)
		}
		if index > 0 && principal.Permissions[index-1] >= permission {
			return fmt.Errorf("permissions must be sorted and unique")
		}
	}
	if (principal.HasPermission(PermissionEvaluationRead) ||
		principal.HasPermission(PermissionEvaluationWrite)) && len(principal.Roles) == 0 {
		return fmt.Errorf("evaluation permissions require at least one evaluation role")
	}
	if principal.HasPermission(PermissionReviewWrite) {
		if principal.ActorKind != findingdecision.ActorHuman &&
			principal.ActorKind != findingdecision.ActorService {
			return fmt.Errorf("review_write requires actor_kind human or service")
		}
		if len(principal.FindingRoles) == 0 {
			return fmt.Errorf("review_write requires finding_roles")
		}
	}
	previousFindingRole := findingdecision.Role("")
	for index, role := range principal.FindingRoles {
		if role != findingdecision.RoleFindingReviewer &&
			role != findingdecision.RolePublicationApprover {
			return fmt.Errorf("finding_roles[%d] contains unsupported role %q", index, role)
		}
		if index > 0 && previousFindingRole >= role {
			return fmt.Errorf("finding_roles must be sorted and unique")
		}
		previousFindingRole = role
	}
	if err := validateIdentifier("profile_revision", principal.ProfileRevision, 256); err != nil {
		return err
	}
	return nil
}

type FindingDecisionWriteCommand struct {
	SchemaVersion string                        `json:"schema_version"`
	Action        findingdecision.Action        `json:"action"`
	ReasonCode    string                        `json:"reason_code"`
	EvidenceRefs  []findingdecision.EvidenceRef `json:"evidence_refs"`
	OccurredAt    time.Time                     `json:"occurred_at"`
	Mutation      MutationInput                 `json:"mutation"`
}

func (command FindingDecisionWriteCommand) Validate(runID, findingID string, principal Principal) error {
	if command.SchemaVersion != FindingDecisionWriteCommandSchemaVersion {
		return fmt.Errorf("unsupported finding decision write command schema %q", command.SchemaVersion)
	}
	request := findingdecision.Request{
		SchemaVersion: findingdecision.RequestSchemaVersion,
		RunID:         runID, FindingID: findingID, Action: command.Action,
		ReasonCode: command.ReasonCode, EvidenceRefs: slices.Clone(command.EvidenceRefs),
		OccurredAt: command.OccurredAt,
	}
	if err := request.Validate(); err != nil {
		return fmt.Errorf("request: %w", err)
	}
	if err := command.Mutation.Validate(); err != nil {
		return fmt.Errorf("mutation: %w", err)
	}
	return findingdecision.Mutation{
		SchemaVersion:  findingdecision.MutationSchemaVersion,
		IdempotencyKey: command.Mutation.IdempotencyKey,
		Actor:          findingdecision.Actor{Kind: principal.ActorKind, ID: principal.Actor},
		Roles:          slices.Clone(principal.FindingRoles), Audit: command.Mutation.Audit,
		At: command.Mutation.At,
	}.Validate()
}

type FeedbackWriteCommand struct {
	SchemaVersion   string                  `json:"schema_version"`
	FeedbackID      string                  `json:"feedback_id"`
	Action          feedback.FeedbackAction `json:"action"`
	Source          feedback.Source         `json:"source"`
	OccurredAt      time.Time               `json:"occurred_at"`
	SourceRefs      []feedback.SourceRef    `json:"source_refs"`
	PriorFeedbackID string                  `json:"prior_feedback_id,omitempty"`
	Mutation        MutationInput           `json:"mutation"`
}

func (command FeedbackWriteCommand) Fact(
	runID, findingID string,
	principal Principal,
) (feedback.Feedback, error) {
	if command.SchemaVersion != FeedbackWriteCommandSchemaVersion {
		return feedback.Feedback{}, fmt.Errorf(
			"unsupported feedback write command schema %q", command.SchemaVersion,
		)
	}
	if err := command.Mutation.Validate(); err != nil {
		return feedback.Feedback{}, fmt.Errorf("mutation: %w", err)
	}
	fact := feedback.Feedback{
		SchemaVersion: feedback.FeedbackSchemaVersion,
		FeedbackID:    command.FeedbackID, FindingID: findingID, RunID: runID,
		Action: command.Action,
		Actor:  feedback.ActorRef{Kind: feedback.ActorKind(principal.ActorKind), ID: principal.Actor},
		Source: command.Source, OccurredAt: command.OccurredAt,
		RecordedAt: command.Mutation.At, SourceRefs: slices.Clone(command.SourceRefs),
		PriorFeedbackID: command.PriorFeedbackID,
		IdempotencyKey:  command.Mutation.IdempotencyKey,
	}
	if err := fact.Validate(); err != nil {
		return feedback.Feedback{}, err
	}
	return fact, nil
}

type OutcomeWriteCommand struct {
	SchemaVersion  string                     `json:"schema_version"`
	OutcomeID      string                     `json:"outcome_id"`
	State          feedback.OutcomeState      `json:"state"`
	Source         feedback.Source            `json:"source"`
	OccurredAt     time.Time                  `json:"occurred_at"`
	Window         feedback.AttributionWindow `json:"attribution_window"`
	SourceRefs     []feedback.SourceRef       `json:"source_refs"`
	PriorOutcomeID string                     `json:"prior_outcome_id,omitempty"`
	Mutation       MutationInput              `json:"mutation"`
}

func (command OutcomeWriteCommand) Fact(
	runID, findingID string,
	principal Principal,
) (feedback.Outcome, error) {
	if command.SchemaVersion != OutcomeWriteCommandSchemaVersion {
		return feedback.Outcome{}, fmt.Errorf(
			"unsupported outcome write command schema %q", command.SchemaVersion,
		)
	}
	if err := command.Mutation.Validate(); err != nil {
		return feedback.Outcome{}, fmt.Errorf("mutation: %w", err)
	}
	fact := feedback.Outcome{
		SchemaVersion: feedback.OutcomeSchemaVersion,
		OutcomeID:     command.OutcomeID, FindingID: findingID, RunID: runID,
		State:  command.State,
		Actor:  feedback.ActorRef{Kind: feedback.ActorKind(principal.ActorKind), ID: principal.Actor},
		Source: command.Source, OccurredAt: command.OccurredAt,
		RecordedAt: command.Mutation.At, Window: command.Window,
		SourceRefs: slices.Clone(command.SourceRefs), PriorOutcomeID: command.PriorOutcomeID,
		IdempotencyKey: command.Mutation.IdempotencyKey,
	}
	if err := fact.Validate(); err != nil {
		return feedback.Outcome{}, err
	}
	return fact, nil
}

type ReviewJobSubmitCommand struct {
	SchemaVersion string            `json:"schema_version"`
	Request       reviewjob.Request `json:"request"`
	Mutation      MutationInput     `json:"mutation"`
}

func (command ReviewJobSubmitCommand) Validate() error {
	if command.SchemaVersion != ReviewJobSubmitCommandSchemaVersion {
		return fmt.Errorf("unsupported review job submit command schema %q", command.SchemaVersion)
	}
	if err := command.Request.Validate(); err != nil {
		return fmt.Errorf("request: %w", err)
	}
	if err := command.Mutation.Validate(); err != nil {
		return fmt.Errorf("mutation: %w", err)
	}
	return nil
}

type ReviewJobCancelCommand struct {
	SchemaVersion string        `json:"schema_version"`
	Reason        string        `json:"reason"`
	Mutation      MutationInput `json:"mutation"`
}

func (command ReviewJobCancelCommand) Validate() error {
	if command.SchemaVersion != ReviewJobCancelCommandSchemaVersion {
		return fmt.Errorf("unsupported review job cancel command schema %q", command.SchemaVersion)
	}
	if err := command.Mutation.Validate(); err != nil {
		return fmt.Errorf("mutation: %w", err)
	}
	return reviewjob.CancelCommand{
		Reason: command.Reason,
		Mutation: reviewjob.Mutation{
			IdempotencyKey: command.Mutation.IdempotencyKey,
			Actor:          "local-api-contract-validator", Audit: command.Mutation.Audit,
			At: command.Mutation.At,
		},
	}.Validate()
}

func (principal Principal) HasPermission(permission Permission) bool {
	index := sort.Search(len(principal.Permissions), func(index int) bool {
		return principal.Permissions[index] >= permission
	})
	return index < len(principal.Permissions) && principal.Permissions[index] == permission
}

func (principal Principal) Access() evaluation.Access {
	return evaluation.Access{Actor: principal.Actor, Roles: slices.Clone(principal.Roles)}
}

type MutationInput struct {
	SchemaVersion  string    `json:"schema_version"`
	IdempotencyKey string    `json:"idempotency_key"`
	Audit          string    `json:"audit"`
	At             time.Time `json:"at"`
}

func (input MutationInput) Validate() error {
	if input.SchemaVersion != MutationInputSchemaVersion {
		return fmt.Errorf("unsupported local API mutation schema %q", input.SchemaVersion)
	}
	mutation := evaluation.Mutation{
		IdempotencyKey: input.IdempotencyKey,
		Actor:          "local-api-contract-validator",
		Roles:          []evaluation.Role{evaluation.RoleDatasetCurator},
		Audit:          input.Audit,
		At:             input.At,
	}
	if err := mutation.Validate(); err != nil {
		return err
	}
	if input.At.Location() != time.UTC {
		return fmt.Errorf("at must use UTC")
	}
	if input.Audit == "" || len(input.Audit) > 4096 || !utf8.ValidString(input.Audit) ||
		input.Audit != strings.TrimSpace(input.Audit) {
		return fmt.Errorf("audit must be non-empty, trimmed UTF-8 within 4096 bytes")
	}
	for _, character := range input.Audit {
		if unicode.IsControl(character) {
			return fmt.Errorf("audit contains unsafe characters")
		}
	}
	return nil
}

func (input MutationInput) mutation(principal Principal) (evaluation.Mutation, error) {
	if err := input.Validate(); err != nil {
		return evaluation.Mutation{}, err
	}
	mutation := evaluation.Mutation{
		IdempotencyKey: input.IdempotencyKey,
		Actor:          principal.Actor,
		Roles:          slices.Clone(principal.Roles),
		Audit:          input.Audit,
		At:             input.At,
	}
	if err := mutation.Validate(); err != nil {
		return evaluation.Mutation{}, err
	}
	return mutation, nil
}

type GovernanceBatchCommand struct {
	SchemaVersion string                            `json:"schema_version"`
	Request       evaluation.GovernanceBatchRequest `json:"request"`
	Mutation      MutationInput                     `json:"mutation"`
}

func (command GovernanceBatchCommand) Validate() error {
	if command.SchemaVersion != GovernanceBatchCommandSchemaVersion {
		return fmt.Errorf("unsupported governance batch command schema %q", command.SchemaVersion)
	}
	if err := command.Request.Validate(); err != nil {
		return fmt.Errorf("request: %w", err)
	}
	if err := command.Mutation.Validate(); err != nil {
		return fmt.Errorf("mutation: %w", err)
	}
	if !command.Request.SubmittedAt.Equal(command.Mutation.At.UTC()) {
		return fmt.Errorf("request submitted_at must equal mutation at")
	}
	return nil
}

type CaseImportCommand struct {
	SchemaVersion string                                `json:"schema_version"`
	Request       evaluation.ExternalGovernedCaseImport `json:"request"`
	Mutation      MutationInput                         `json:"mutation"`
}

func (command CaseImportCommand) Validate() error {
	if command.SchemaVersion != CaseImportCommandSchemaVersion {
		return fmt.Errorf("unsupported case import command schema %q", command.SchemaVersion)
	}
	if err := command.Request.Validate(); err != nil {
		return fmt.Errorf("request: %w", err)
	}
	if err := command.Mutation.Validate(); err != nil {
		return fmt.Errorf("mutation: %w", err)
	}
	if !command.Request.ImportedAt.Equal(command.Mutation.At.UTC()) {
		return fmt.Errorf("request imported_at must equal mutation at")
	}
	return nil
}

type TrustKeyCommand struct {
	SchemaVersion string                                    `json:"schema_version"`
	Registration  evaluation.GovernanceTrustKeyRegistration `json:"registration"`
	Mutation      MutationInput                             `json:"mutation"`
}

func (command TrustKeyCommand) Validate() error {
	if command.SchemaVersion != TrustKeyCommandSchemaVersion {
		return fmt.Errorf("unsupported trust key command schema %q", command.SchemaVersion)
	}
	if err := command.Registration.Validate(); err != nil {
		return fmt.Errorf("registration: %w", err)
	}
	if err := command.Mutation.Validate(); err != nil {
		return fmt.Errorf("mutation: %w", err)
	}
	if !command.Registration.RegisteredAt.Equal(command.Mutation.At.UTC()) {
		return fmt.Errorf("registration registered_at must equal mutation at")
	}
	return nil
}

type TrustKeyRevocationCommand struct {
	SchemaVersion string                                  `json:"schema_version"`
	Revocation    evaluation.GovernanceTrustKeyRevocation `json:"revocation"`
	Mutation      MutationInput                           `json:"mutation"`
}

func (command TrustKeyRevocationCommand) Validate() error {
	if command.SchemaVersion != TrustKeyRevocationCommandSchemaVersion {
		return fmt.Errorf("unsupported trust key revocation command schema %q", command.SchemaVersion)
	}
	if err := command.Revocation.Validate(); err != nil {
		return fmt.Errorf("revocation: %w", err)
	}
	if err := command.Mutation.Validate(); err != nil {
		return fmt.Errorf("mutation: %w", err)
	}
	if !command.Revocation.RevokedAt.Equal(command.Mutation.At.UTC()) {
		return fmt.Errorf("revocation revoked_at must equal mutation at")
	}
	return nil
}

type ConfigCreateCommand struct {
	SchemaVersion string                `json:"schema_version"`
	Revision      reviewconfig.Revision `json:"revision"`
	Mutation      MutationInput         `json:"mutation"`
}

func (command ConfigCreateCommand) Validate() error {
	if command.SchemaVersion != ConfigCreateCommandSchemaVersion {
		return fmt.Errorf("unsupported config create command schema %q", command.SchemaVersion)
	}
	if err := command.Revision.Validate(); err != nil {
		return fmt.Errorf("revision: %w", err)
	}
	if err := command.Mutation.Validate(); err != nil {
		return fmt.Errorf("mutation: %w", err)
	}
	return nil
}

type ConfigTransitionCommand struct {
	SchemaVersion string              `json:"schema_version"`
	Mutation      MutationInput       `json:"mutation"`
	Rollout       *configrepo.Rollout `json:"rollout,omitempty"`
}

// ConfigResolutionQuery is a read-only structured query. InvocationID is part
// of rollout assignment identity and therefore must be caller supplied rather
// than invented by the HTTP adapter.
type ConfigResolutionQuery struct {
	SchemaVersion string                         `json:"schema_version"`
	Context       reviewconfig.ResolutionContext `json:"context"`
}

func (query ConfigResolutionQuery) Validate() error {
	if query.SchemaVersion != ConfigResolutionQuerySchemaVersion {
		return fmt.Errorf("unsupported config resolution query schema %q", query.SchemaVersion)
	}
	return query.Context.Validate()
}

type ConfigResolutionView struct {
	Bundle  reviewconfig.ConfigBundle            `json:"bundle"`
	Receipt reviewconfig.ConfigResolutionReceipt `json:"receipt"`
}

func (command ConfigTransitionCommand) Validate() error {
	if command.SchemaVersion != ConfigTransitionCommandSchemaVersion {
		return fmt.Errorf("unsupported config transition command schema %q", command.SchemaVersion)
	}
	if err := command.Mutation.Validate(); err != nil {
		return fmt.Errorf("mutation: %w", err)
	}
	if command.Rollout != nil {
		if err := command.Rollout.Validate(); err != nil {
			return fmt.Errorf("rollout: %w", err)
		}
	}
	return nil
}

func (input MutationInput) configMutation(principal Principal) (configrepo.Mutation, error) {
	if err := input.Validate(); err != nil {
		return configrepo.Mutation{}, err
	}
	return configrepo.Mutation{
		IdempotencyKey: input.IdempotencyKey,
		Actor:          principal.Actor,
		Audit:          input.Audit,
		At:             input.At,
	}, nil
}

type DashboardSnapshotView struct {
	SchemaVersion  string                              `json:"schema_version"`
	SnapshotID     string                              `json:"snapshot_id"`
	PolicyRevision string                              `json:"policy_revision"`
	Scope          analyticsadapter.Scope              `json:"scope"`
	Window         analytics.TimeWindow                `json:"window"`
	GroupBy        []analytics.DimensionName           `json:"group_by"`
	BuiltAt        time.Time                           `json:"built_at"`
	Coverage       []analyticsadapter.SourceCoverage   `json:"source_coverage"`
	RunBindings    []analyticsadapter.RunSourceBinding `json:"run_source_bindings"`
	Dashboard      analytics.DashboardProjection       `json:"dashboard"`
}

func dashboardView(snapshot analyticsadapter.ProjectionSnapshot) DashboardSnapshotView {
	return DashboardSnapshotView{
		SchemaVersion: snapshot.SchemaVersion, SnapshotID: snapshot.SnapshotID,
		PolicyRevision: snapshot.PolicyRevision, Scope: snapshot.Scope, Window: snapshot.Window,
		GroupBy: slices.Clone(snapshot.GroupBy), BuiltAt: snapshot.BuiltAt,
		Coverage: slices.Clone(snapshot.Coverage), RunBindings: slices.Clone(snapshot.RunBindings),
		Dashboard: snapshot.Dashboard,
	}
}

type Response struct {
	SchemaVersion string `json:"schema_version"`
	Data          any    `json:"data"`
}

type ErrorResponse struct {
	SchemaVersion string `json:"schema_version"`
	Code          string `json:"code"`
	Message       string `json:"message"`
}

type Page struct {
	Items      any    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

type ReviewRunImpactPage struct {
	SchemaVersion string                 `json:"schema_version"`
	Watermark     uint64                 `json:"watermark"`
	Selector      runrepo.ImpactSelector `json:"selector"`
	Items         []runrepo.ImpactMatch  `json:"items"`
	Coverage      runrepo.ImpactCoverage `json:"coverage"`
	NextCursor    string                 `json:"next_cursor,omitempty"`
}

// ReviewRunDetail exposes the verified, committed result artifacts that an
// operator needs to inspect a review. Raw prompts, task evidence, receipts and
// Markdown artifacts remain available only through governed artifact access.
// Pending/running entries have Committed=false and omit Run and reports.
type ReviewRunDetail struct {
	History            runrepo.HistoryEntry                           `json:"history"`
	Committed          bool                                           `json:"committed"`
	Run                *runmodel.ReviewRun                            `json:"run,omitempty"`
	Report             *reviewcore.Report                             `json:"report,omitempty"`
	CandidateSet       *contractsv1alpha1.GovernedCandidateSet        `json:"candidate_set,omitempty"`
	VerificationLedger *contractsv1alpha1.CandidateVerificationLedger `json:"verification_ledger,omitempty"`
	CalibrationLedger  *contractsv1alpha1.FindingCalibrationLedger    `json:"calibration_ledger,omitempty"`
	SuppressionLedger  *contractsv1alpha1.FindingSuppressionLedger    `json:"suppression_ledger,omitempty"`
	GovernedReport     *contractsv1alpha1.GovernedReviewReport        `json:"governed_report,omitempty"`
}

// FindingView preserves the distinct Finding/Decision/Feedback/Outcome facts
// while excluding provider publication request/result payloads from the local
// HTTP surface.
type FindingView struct {
	Run               runmodel.ReviewRun                          `json:"run"`
	Finding           *reviewcore.Finding                         `json:"finding,omitempty"`
	Decisions         []reviewcore.FindingDecision                `json:"decisions"`
	GovernedFinding   *contractsv1alpha1.GovernedReviewFinding    `json:"governed_finding,omitempty"`
	GovernedDecisions []contractsv1alpha1.GovernedFindingDecision `json:"governed_decisions"`
	HumanDecisions    []findingdecision.Decision                  `json:"human_decisions"`
	Feedback          []feedback.Feedback                         `json:"feedback"`
	Outcomes          []feedback.Outcome                          `json:"outcomes"`
}

type FeedbackWriteResult struct {
	Feedback    feedback.Feedback                       `json:"feedback"`
	Eligibility feedback.EvaluationCandidateEligibility `json:"evaluation_candidate_eligibility"`
}

func findingView(detail controlplane.FindingDetail) FindingView {
	return FindingView{
		Run: detail.Run, Finding: detail.Finding, Decisions: slices.Clone(detail.Decisions),
		GovernedFinding:   detail.GovernedFinding,
		GovernedDecisions: slices.Clone(detail.GovernedDecisions),
		HumanDecisions:    slices.Clone(detail.HumanDecisions),
		Feedback:          slices.Clone(detail.Feedback), Outcomes: slices.Clone(detail.Outcomes),
	}
}

type CaseDetail struct {
	Record        evaluation.CaseRecord                  `json:"record"`
	Labels        []evaluation.LabelEntry                `json:"labels"`
	Assignments   []evaluation.CaseReviewAssignmentEntry `json:"assignments"`
	Annotations   []evaluation.CaseAnnotationEntry       `json:"annotations"`
	Adjudications []evaluation.CaseAdjudicationEntry     `json:"adjudications"`
	Agreement     evaluation.CaseReviewAgreement         `json:"agreement"`
	Exposures     []evaluation.ExposureEntry             `json:"exposures"`
}

func validateIdentifier(name, value string, maximum int) error {
	if value == "" || strings.TrimSpace(value) != value || len(value) > maximum {
		return fmt.Errorf("%s must be non-empty, trimmed, and at most %d bytes", name, maximum)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return fmt.Errorf("%s must not contain control characters", name)
		}
	}
	return nil
}
