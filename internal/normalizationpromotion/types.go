// Package normalizationpromotion owns the typed bridge from independently
// governed normalization quality evidence into the shared promotion ledger.
package normalizationpromotion

import (
	"errors"
	"time"

	"github.com/abietic/argus/internal/configrepo"
	"github.com/abietic/argus/internal/evaluation"
	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/runmodel"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

const (
	PolicySchemaVersion                 = "argus.normalization_promotion_policy.v1alpha1"
	PolicyContract                      = PolicySchemaVersion + "+json"
	PrepareRequestSchemaVersion         = "argus.normalization_promotion_prepare_request.v1alpha1"
	GateRequestSchemaVersion            = "argus.normalization_promotion_gate_request.v1alpha1"
	DecisionSchemaVersion               = "argus.normalization_promotion_gate_decision.v1alpha1"
	DecisionContract                    = DecisionSchemaVersion + "+json"
	OperationalGateRequestSchemaVersion = "argus.normalization_promotion_operational_gate_request.v1alpha1"
	OperationalGateRequestContract      = OperationalGateRequestSchemaVersion + "+json"
)

var (
	ErrEvidenceMismatch = errors.New("normalization promotion evidence mismatch")
	ErrUnauthorized     = errors.New("normalization promotion unauthorized")
)

type Thresholds struct {
	MinimumCases                uint32 `json:"minimum_cases"`
	MinimumEligibleCandidates   uint32 `json:"minimum_eligible_candidates"`
	MinimumOracleDuplicatePairs uint64 `json:"minimum_oracle_duplicate_pairs"`
	MinimumOracleDistinctPairs  uint64 `json:"minimum_oracle_distinct_pairs"`
	MinimumPairwisePrecisionPPM uint32 `json:"minimum_pairwise_precision_ppm"`
	MinimumPairwiseRecallPPM    uint32 `json:"minimum_pairwise_recall_ppm"`
	MaximumFalseMergeRatePPM    uint32 `json:"maximum_false_merge_rate_ppm"`
	MinimumExactPartitionPPM    uint32 `json:"minimum_exact_partition_ppm"`
}

type Policy struct {
	SchemaVersion     string     `json:"schema_version"`
	PolicyID          string     `json:"policy_id"`
	Revision          string     `json:"revision"`
	Test              Thresholds `json:"test"`
	Holdout           Thresholds `json:"holdout"`
	MinimumShadowRuns uint32     `json:"minimum_shadow_runs"`
	MinimumCanaryRuns uint32     `json:"minimum_canary_runs"`
	CanaryPercentage  int        `json:"canary_percentage"`
	CreatedAt         time.Time  `json:"created_at"`
}

type PrepareRequest struct {
	SchemaVersion            string                         `json:"schema_version"`
	PromotionVariantID       string                         `json:"promotion_variant_id"`
	BaselineReviewRunID      string                         `json:"baseline_review_run_id"`
	BaselineConfigRevisionID string                         `json:"baseline_config_revision_id"`
	BaselineConfigRevision   string                         `json:"baseline_config_revision"`
	VariantConfigRevisionID  string                         `json:"variant_config_revision_id"`
	VariantConfigRevision    string                         `json:"variant_config_revision"`
	VariantPolicyRevision    string                         `json:"variant_policy_revision"`
	RollbackPolicyRevision   string                         `json:"rollback_policy_revision"`
	VariantImplementation    contractsv1alpha1.VersionedRef `json:"variant_implementation"`
	RollbackImplementation   contractsv1alpha1.VersionedRef `json:"rollback_implementation"`
	PromotionPolicyRevision  string                         `json:"promotion_policy_revision"`
	Owner                    string                         `json:"owner"`
	Policy                   Policy                         `json:"policy"`
	CreatedAt                time.Time                      `json:"created_at"`
}

type Preparation struct {
	PolicyRef            runmodel.ArtifactRef       `json:"policy_ref"`
	BaselineBundleSHA256 string                     `json:"baseline_bundle_sha256"`
	VariantBundleSHA256  string                     `json:"variant_bundle_sha256"`
	BaselineConfigSHA256 string                     `json:"baseline_config_revision_sha256"`
	VariantConfig        reviewconfig.Revision      `json:"variant_config_revision"`
	VariantConfigSHA256  string                     `json:"variant_config_revision_sha256"`
	ConfigStatus         configrepo.Status          `json:"config_status"`
	Promotion            evaluation.PromotionRecord `json:"promotion"`
}

type GateRequest struct {
	SchemaVersion string                   `json:"schema_version"`
	VariantID     string                   `json:"variant_id"`
	Gate          evaluation.PromotionGate `json:"gate"`
	PolicyRef     runmodel.ArtifactRef     `json:"policy_ref"`
	QualityRunRef runmodel.ArtifactRef     `json:"quality_run_ref"`
	EvaluatedAt   time.Time                `json:"evaluated_at"`
}

type GateDecision struct {
	SchemaVersion string                                 `json:"schema_version"`
	VariantID     string                                 `json:"variant_id"`
	Gate          evaluation.PromotionGate               `json:"gate"`
	PolicyRef     runmodel.ArtifactRef                   `json:"policy_ref"`
	QualityRunRef runmodel.ArtifactRef                   `json:"quality_run_ref"`
	QualityRunID  string                                 `json:"quality_run_id"`
	Split         evaluation.Split                       `json:"split"`
	Outcome       evaluation.GateOutcome                 `json:"outcome"`
	ReasonCodes   []string                               `json:"reason_codes"`
	Measured      evaluation.NormalizationQualitySummary `json:"measured"`
	Thresholds    Thresholds                             `json:"thresholds"`
	EvaluatedAt   time.Time                              `json:"evaluated_at"`
}

type GateOutput struct {
	Decision    GateDecision               `json:"decision"`
	DecisionRef runmodel.ArtifactRef       `json:"decision_ref"`
	Promotion   evaluation.PromotionRecord `json:"promotion"`
}

type OperationalGateRequest struct {
	SchemaVersion    string                             `json:"schema_version"`
	VariantID        string                             `json:"variant_id"`
	Gate             evaluation.PromotionGate           `json:"gate"`
	PolicyRef        runmodel.ArtifactRef               `json:"policy_ref"`
	ReviewRunIDs     []string                           `json:"review_run_ids"`
	SafetyEventCount int                                `json:"safety_event_count"`
	Authorization    *evaluation.PromotionAuthorization `json:"authorization,omitempty"`
	EvaluatedAt      time.Time                          `json:"evaluated_at"`
}

type LifecycleOutput struct {
	Config    configrepo.Record          `json:"config"`
	Rollout   *configrepo.Rollout        `json:"rollout,omitempty"`
	Promotion evaluation.PromotionRecord `json:"promotion"`
}

type OperationalGateOutput struct {
	Result      evaluation.GateResult      `json:"result"`
	EvidenceRef runmodel.ArtifactRef       `json:"evidence_ref"`
	Promotion   evaluation.PromotionRecord `json:"promotion"`
}
