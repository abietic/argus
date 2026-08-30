package evaluation

import (
	"fmt"
	"reflect"
	"slices"
	"sort"
	"time"

	"argus.local/argus/internal/runmodel"
)

const (
	NormalizationOracleSchemaVersion            = "argus.normalization_oracle.v1alpha1"
	NormalizationOracleContract                 = NormalizationOracleSchemaVersion + "+json"
	NormalizationQualityRunRequestSchemaVersion = "argus.normalization_quality_run_request.v1alpha1"
	NormalizationQualityRunSchemaVersion        = "argus.normalization_quality_run.v1alpha1"
	NormalizationQualityRunContract             = NormalizationQualityRunSchemaVersion + "+json"
	NormalizationQualityAuthority               = "independent_oracle_evaluation"
)

// NormalizationOracle freezes an independently adjudicated equivalence
// relation over one committed raw-candidate collection. It supplements an
// existing governed EvaluationCase; it never replaces that case's defect
// label and cannot be generated from Argus normalization output.
type NormalizationOracle struct {
	SchemaVersion             string                          `json:"schema_version"`
	OracleID                  string                          `json:"oracle_id"`
	CorpusSnapshotRef         runmodel.ArtifactRef            `json:"corpus_snapshot_ref"`
	CaseID                    string                          `json:"case_id"`
	Split                     Split                           `json:"split"`
	LabelRevision             uint64                          `json:"label_revision"`
	SourceReviewRunID         string                          `json:"source_review_run_id"`
	SourceReviewRunRef        runmodel.ArtifactRef            `json:"source_review_run_ref"`
	RawCandidateCollectionRef runmodel.ArtifactRef            `json:"raw_candidate_collection_ref"`
	TargetDigest              string                          `json:"target_digest"`
	EligibleRawCandidateIDs   []string                        `json:"eligible_raw_candidate_ids"`
	EquivalenceClasses        []NormalizationEquivalenceClass `json:"equivalence_classes"`
	Adjudication              NormalizationOracleAdjudication `json:"adjudication"`
	CreatedAt                 time.Time                       `json:"created_at"`
}

type NormalizationEquivalenceClass struct {
	ClassID         string   `json:"class_id"`
	RawCandidateIDs []string `json:"raw_candidate_ids"`
}

// ObservedPolicyRevisions is contamination evidence, not a permission. A
// quality run remains measurable when exposed, but is explicitly ineligible
// as independent evidence for that policy revision.
type NormalizationOracleAdjudication struct {
	Authority               string                 `json:"authority"`
	Revision                string                 `json:"revision"`
	ReviewerIDs             []string               `json:"reviewer_ids"`
	AdjudicatorID           string                 `json:"adjudicator_id"`
	EvidenceRefs            []runmodel.ArtifactRef `json:"evidence_refs"`
	ObservedPolicyRevisions []string               `json:"observed_policy_revisions"`
	AdjudicatedAt           time.Time              `json:"adjudicated_at"`
}

type NormalizationPrediction struct {
	CaseID                      string                          `json:"case_id"`
	OracleRef                   runmodel.ArtifactRef            `json:"oracle_ref"`
	PolicyRevision              string                          `json:"policy_revision"`
	PredictedEquivalenceClasses []NormalizationEquivalenceClass `json:"predicted_equivalence_classes"`
}

type RatioMetric struct {
	Available   bool   `json:"available"`
	Numerator   uint64 `json:"numerator"`
	Denominator uint64 `json:"denominator"`
	ValuePPM    uint32 `json:"value_ppm"`
	ReasonCode  string `json:"reason_code,omitempty"`
}

type NormalizationQualityCaseResult struct {
	CaseID                    string                     `json:"case_id"`
	OracleID                  string                     `json:"oracle_id"`
	OracleBinding             NormalizationOracleBinding `json:"oracle_binding"`
	SourceReviewRunID         string                     `json:"source_review_run_id"`
	SourceReviewRunRef        runmodel.ArtifactRef       `json:"source_review_run_ref"`
	RawCandidateCollectionRef runmodel.ArtifactRef       `json:"raw_candidate_collection_ref"`
	Split                     Split                      `json:"split"`
	EligibleRawCandidates     uint32                     `json:"eligible_raw_candidates"`
	OracleUniqueClaims        uint32                     `json:"oracle_unique_claims"`
	PredictedUniqueClaims     uint32                     `json:"predicted_unique_claims"`
	UniqueClaimDelta          int64                      `json:"unique_claim_delta"`
	TrueDuplicatePairs        uint64                     `json:"true_duplicate_pairs"`
	FalseDuplicatePairs       uint64                     `json:"false_duplicate_pairs"`
	MissedDuplicatePairs      uint64                     `json:"missed_duplicate_pairs"`
	TrueDistinctPairs         uint64                     `json:"true_distinct_pairs"`
	PairwisePrecision         RatioMetric                `json:"pairwise_precision"`
	PairwiseRecall            RatioMetric                `json:"pairwise_recall"`
	FalseMergeRate            RatioMetric                `json:"false_merge_rate"`
	ExactPartitionMatch       bool                       `json:"exact_partition_match"`
	PolicyExposed             bool                       `json:"policy_exposed"`
}

type NormalizationQualitySummary struct {
	Cases                    uint32      `json:"cases"`
	EligibleRawCandidates    uint32      `json:"eligible_raw_candidates"`
	OracleUniqueClaims       uint32      `json:"oracle_unique_claims"`
	PredictedUniqueClaims    uint32      `json:"predicted_unique_claims"`
	UniqueClaimDelta         int64       `json:"unique_claim_delta"`
	TrueDuplicatePairs       uint64      `json:"true_duplicate_pairs"`
	FalseDuplicatePairs      uint64      `json:"false_duplicate_pairs"`
	MissedDuplicatePairs     uint64      `json:"missed_duplicate_pairs"`
	TrueDistinctPairs        uint64      `json:"true_distinct_pairs"`
	PairwisePrecision        RatioMetric `json:"pairwise_precision"`
	PairwiseRecall           RatioMetric `json:"pairwise_recall"`
	FalseMergeRate           RatioMetric `json:"false_merge_rate"`
	ExactPartitionMatches    uint32      `json:"exact_partition_matches"`
	PolicyExposedCases       uint32      `json:"policy_exposed_cases"`
	IndependentEvidenceCases uint32      `json:"independent_evidence_cases"`
}

type NormalizationQualityRun struct {
	SchemaVersion  string                           `json:"schema_version"`
	QualityRunID   string                           `json:"quality_run_id"`
	PolicyRevision string                           `json:"policy_revision"`
	Authority      string                           `json:"authority"`
	Cases          []NormalizationQualityCaseResult `json:"cases"`
	Summary        NormalizationQualitySummary      `json:"summary"`
	CreatedAt      time.Time                        `json:"created_at"`
}

// ValidateAggregate checks the self-contained invariants of a quality summary.
// Whole-run validation additionally recomputes this summary from case facts.
func (summary NormalizationQualitySummary) ValidateAggregate() error {
	if summary.Cases == 0 || summary.EligibleRawCandidates == 0 ||
		summary.OracleUniqueClaims == 0 || summary.PredictedUniqueClaims == 0 ||
		summary.OracleUniqueClaims > summary.EligibleRawCandidates ||
		summary.PredictedUniqueClaims > summary.EligibleRawCandidates {
		return fmt.Errorf("normalization quality summary contains invalid case/candidate/class counts")
	}
	if summary.UniqueClaimDelta != int64(summary.PredictedUniqueClaims)-int64(summary.OracleUniqueClaims) {
		return fmt.Errorf("normalization quality summary contains invalid unique claim delta")
	}
	if summary.PairwisePrecision != ratioMetric(summary.TrueDuplicatePairs, summary.TrueDuplicatePairs+summary.FalseDuplicatePairs, "no_predicted_duplicate_pairs") ||
		summary.PairwiseRecall != ratioMetric(summary.TrueDuplicatePairs, summary.TrueDuplicatePairs+summary.MissedDuplicatePairs, "no_oracle_duplicate_pairs") ||
		summary.FalseMergeRate != ratioMetric(summary.FalseDuplicatePairs, summary.TrueDuplicatePairs+summary.FalseDuplicatePairs, "no_predicted_duplicate_pairs") {
		return fmt.Errorf("normalization quality summary ratio metrics are inconsistent")
	}
	if summary.ExactPartitionMatches > summary.Cases ||
		summary.PolicyExposedCases+summary.IndependentEvidenceCases != summary.Cases {
		return fmt.Errorf("normalization quality summary contains invalid case classifications")
	}
	return nil
}

type NormalizationQualityRunRequest struct {
	SchemaVersion  string                       `json:"schema_version"`
	QualityRunID   string                       `json:"quality_run_id"`
	PolicyRevision string                       `json:"policy_revision"`
	OracleBindings []NormalizationOracleBinding `json:"oracle_bindings"`
	CreatedAt      time.Time                    `json:"created_at"`
}

func DecodeNormalizationOracle(data []byte) (NormalizationOracle, error) {
	return decodeStrict(data, "NormalizationOracle", func(value NormalizationOracle) error { return value.Validate() })
}

func DecodeNormalizationQualityRun(data []byte) (NormalizationQualityRun, error) {
	return decodeStrict(data, "NormalizationQualityRun", func(value NormalizationQualityRun) error { return value.Validate() })
}

func DecodeNormalizationQualityRunRequest(data []byte) (NormalizationQualityRunRequest, error) {
	return decodeStrict(data, "NormalizationQualityRunRequest", func(value NormalizationQualityRunRequest) error {
		return value.Validate()
	})
}

func (request NormalizationQualityRunRequest) Validate() error {
	if request.SchemaVersion != NormalizationQualityRunRequestSchemaVersion {
		return fmt.Errorf("unsupported normalization quality run request schema %q", request.SchemaVersion)
	}
	if err := validateID("quality_run_id", request.QualityRunID); err != nil {
		return err
	}
	if err := validateID("policy_revision", request.PolicyRevision); err != nil {
		return err
	}
	if len(request.OracleBindings) == 0 || len(request.OracleBindings) > 1024 {
		return fmt.Errorf("oracle_bindings must contain between 1 and 1024 entries")
	}
	previous := ""
	for index, binding := range request.OracleBindings {
		if err := binding.Validate(); err != nil {
			return fmt.Errorf("oracle_bindings[%d]: %w", index, err)
		}
		if index > 0 && binding.OracleRef.SHA256 <= previous {
			return fmt.Errorf("oracle_bindings must be uniquely sorted by oracle sha256")
		}
		previous = binding.OracleRef.SHA256
	}
	if request.CreatedAt.IsZero() || request.CreatedAt.Location() != time.UTC {
		return fmt.Errorf("created_at must be non-zero UTC")
	}
	return nil
}

func (oracle NormalizationOracle) Validate() error {
	if oracle.SchemaVersion != NormalizationOracleSchemaVersion {
		return fmt.Errorf("unsupported normalization oracle schema %q", oracle.SchemaVersion)
	}
	for name, value := range map[string]string{
		"oracle_id": oracle.OracleID, "case_id": oracle.CaseID,
		"source_review_run_id": oracle.SourceReviewRunID,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if oracle.Split == SplitUnassigned {
		return fmt.Errorf("normalization oracle requires an assigned split")
	}
	if err := oracle.Split.Validate(); err != nil {
		return err
	}
	if oracle.LabelRevision == 0 {
		return fmt.Errorf("label_revision must be positive")
	}
	for name, ref := range map[string]runmodel.ArtifactRef{
		"corpus_snapshot_ref":          oracle.CorpusSnapshotRef,
		"source_review_run_ref":        oracle.SourceReviewRunRef,
		"raw_candidate_collection_ref": oracle.RawCandidateCollectionRef,
	} {
		if err := ref.Validate(); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if oracle.CorpusSnapshotRef.Contract != CorpusSnapshotContract ||
		oracle.SourceReviewRunRef.Contract != runmodel.ContractReviewRun ||
		oracle.RawCandidateCollectionRef.Contract != runmodel.ContractAgentReviewRawCandidates {
		return fmt.Errorf("normalization oracle contains an unsupported artifact contract")
	}
	if err := validateSHA256("target_digest", oracle.TargetDigest); err != nil {
		return err
	}
	if len(oracle.EligibleRawCandidateIDs) == 0 || len(oracle.EligibleRawCandidateIDs) > 1024 {
		return fmt.Errorf("eligible_raw_candidate_ids must contain between 1 and 1024 entries")
	}
	if !uniquelySortedIDs(oracle.EligibleRawCandidateIDs) {
		return fmt.Errorf("eligible_raw_candidate_ids must be uniquely sorted")
	}
	if len(oracle.EquivalenceClasses) == 0 || len(oracle.EquivalenceClasses) > len(oracle.EligibleRawCandidateIDs) {
		return fmt.Errorf("equivalence_classes must partition eligible raw candidates")
	}
	partition := make([]string, 0, len(oracle.EligibleRawCandidateIDs))
	previousClass := ""
	for index, class := range oracle.EquivalenceClasses {
		if err := validateID("class_id", class.ClassID); err != nil {
			return fmt.Errorf("equivalence_classes[%d]: %w", index, err)
		}
		if index > 0 && class.ClassID <= previousClass {
			return fmt.Errorf("equivalence_classes must be uniquely sorted by class_id")
		}
		if len(class.RawCandidateIDs) == 0 || !uniquelySortedIDs(class.RawCandidateIDs) {
			return fmt.Errorf("equivalence class %q candidate IDs must be non-empty and uniquely sorted", class.ClassID)
		}
		partition = append(partition, class.RawCandidateIDs...)
		previousClass = class.ClassID
	}
	sort.Strings(partition)
	if !slices.Equal(partition, oracle.EligibleRawCandidateIDs) {
		return fmt.Errorf("equivalence_classes do not exactly partition eligible_raw_candidate_ids")
	}
	if err := oracle.Adjudication.Validate(); err != nil {
		return err
	}
	if oracle.CreatedAt.IsZero() || oracle.CreatedAt.Location() != time.UTC || oracle.Adjudication.AdjudicatedAt.After(oracle.CreatedAt) {
		return fmt.Errorf("created_at must be UTC and not precede adjudication")
	}
	return nil
}

func (adjudication NormalizationOracleAdjudication) Validate() error {
	for name, value := range map[string]string{
		"adjudication.authority":      adjudication.Authority,
		"adjudication.revision":       adjudication.Revision,
		"adjudication.adjudicator_id": adjudication.AdjudicatorID,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if adjudication.Authority == NormalizationQualityAuthority || adjudication.Authority == "argus" {
		return fmt.Errorf("oracle authority must be independent from Argus evaluation")
	}
	if len(adjudication.ReviewerIDs) < 2 || !uniquelySortedIDs(adjudication.ReviewerIDs) {
		return fmt.Errorf("reviewer_ids requires at least two uniquely sorted reviewers")
	}
	if slices.Contains(adjudication.ReviewerIDs, adjudication.AdjudicatorID) {
		return fmt.Errorf("adjudicator must be independent from reviewers")
	}
	if len(adjudication.EvidenceRefs) == 0 {
		return fmt.Errorf("normalization oracle requires independent evidence refs")
	}
	for index, ref := range adjudication.EvidenceRefs {
		if err := ref.Validate(); err != nil {
			return fmt.Errorf("evidence_refs[%d]: %w", index, err)
		}
		if index > 0 && ref.SHA256 <= adjudication.EvidenceRefs[index-1].SHA256 {
			return fmt.Errorf("evidence_refs must be uniquely sorted by sha256")
		}
	}
	if adjudication.ObservedPolicyRevisions == nil || !uniquelySortedIDs(adjudication.ObservedPolicyRevisions) {
		return fmt.Errorf("observed_policy_revisions must be a uniquely sorted array")
	}
	if adjudication.AdjudicatedAt.IsZero() || adjudication.AdjudicatedAt.Location() != time.UTC {
		return fmt.Errorf("adjudicated_at must be non-zero UTC")
	}
	return nil
}

func EvaluateNormalizationQuality(
	qualityRunID string,
	policyRevision string,
	oracles []NormalizationOracle,
	oracleBindings []NormalizationOracleBinding,
	predictions []NormalizationPrediction,
	createdAt time.Time,
) (NormalizationQualityRun, error) {
	if err := validateID("quality_run_id", qualityRunID); err != nil {
		return NormalizationQualityRun{}, err
	}
	if err := validateID("policy_revision", policyRevision); err != nil {
		return NormalizationQualityRun{}, err
	}
	if len(oracles) == 0 || len(oracles) != len(oracleBindings) || len(oracles) != len(predictions) {
		return NormalizationQualityRun{}, fmt.Errorf("oracles, bindings, and predictions must be non-empty and aligned")
	}
	if createdAt.IsZero() || createdAt.Location() != time.UTC {
		return NormalizationQualityRun{}, fmt.Errorf("created_at must be non-zero UTC")
	}
	run := NormalizationQualityRun{
		SchemaVersion: NormalizationQualityRunSchemaVersion,
		QualityRunID:  qualityRunID, PolicyRevision: policyRevision,
		Authority: NormalizationQualityAuthority,
		Cases:     []NormalizationQualityCaseResult{}, CreatedAt: createdAt,
	}
	for index, oracle := range oracles {
		if err := oracle.Validate(); err != nil {
			return NormalizationQualityRun{}, fmt.Errorf("oracle[%d]: %w", index, err)
		}
		prediction := predictions[index]
		binding := oracleBindings[index]
		if err := binding.Validate(); err != nil {
			return NormalizationQualityRun{}, fmt.Errorf("oracle_bindings[%d]: %w", index, err)
		}
		if binding.OracleID != oracle.OracleID || binding.CaseID != oracle.CaseID ||
			prediction.CaseID != oracle.CaseID || prediction.PolicyRevision != policyRevision ||
			prediction.OracleRef != binding.OracleRef {
			return NormalizationQualityRun{}, fmt.Errorf("prediction[%d] does not bind oracle and policy", index)
		}
		result, err := evaluateNormalizationCase(oracle, binding, prediction)
		if err != nil {
			return NormalizationQualityRun{}, fmt.Errorf("case %q: %w", oracle.CaseID, err)
		}
		run.Cases = append(run.Cases, result)
		accumulateNormalizationQuality(&run.Summary, result)
	}
	slices.SortFunc(run.Cases, func(left, right NormalizationQualityCaseResult) int {
		if left.CaseID < right.CaseID {
			return -1
		}
		if left.CaseID > right.CaseID {
			return 1
		}
		return 0
	})
	finalizeNormalizationQualitySummary(&run.Summary)
	if err := run.Validate(); err != nil {
		return NormalizationQualityRun{}, err
	}
	return run, nil
}

func evaluateNormalizationCase(
	oracle NormalizationOracle,
	binding NormalizationOracleBinding,
	prediction NormalizationPrediction,
) (NormalizationQualityCaseResult, error) {
	predictedByID, err := partitionIndex(prediction.PredictedEquivalenceClasses, oracle.EligibleRawCandidateIDs)
	if err != nil {
		return NormalizationQualityCaseResult{}, fmt.Errorf("prediction partition: %w", err)
	}
	oracleByID, err := partitionIndex(oracle.EquivalenceClasses, oracle.EligibleRawCandidateIDs)
	if err != nil {
		return NormalizationQualityCaseResult{}, fmt.Errorf("oracle partition: %w", err)
	}
	result := NormalizationQualityCaseResult{
		CaseID: oracle.CaseID, OracleID: oracle.OracleID, OracleBinding: binding,
		SourceReviewRunID: oracle.SourceReviewRunID, SourceReviewRunRef: oracle.SourceReviewRunRef,
		RawCandidateCollectionRef: oracle.RawCandidateCollectionRef, Split: oracle.Split,
		EligibleRawCandidates: uint32(len(oracle.EligibleRawCandidateIDs)),
		OracleUniqueClaims:    uint32(len(oracle.EquivalenceClasses)),
		PredictedUniqueClaims: uint32(len(prediction.PredictedEquivalenceClasses)),
		UniqueClaimDelta:      int64(len(prediction.PredictedEquivalenceClasses) - len(oracle.EquivalenceClasses)),
		PolicyExposed:         slices.Contains(oracle.Adjudication.ObservedPolicyRevisions, prediction.PolicyRevision),
	}
	for left := 0; left < len(oracle.EligibleRawCandidateIDs); left++ {
		for right := left + 1; right < len(oracle.EligibleRawCandidateIDs); right++ {
			leftID, rightID := oracle.EligibleRawCandidateIDs[left], oracle.EligibleRawCandidateIDs[right]
			oracleSame := oracleByID[leftID] == oracleByID[rightID]
			predictedSame := predictedByID[leftID] == predictedByID[rightID]
			switch {
			case oracleSame && predictedSame:
				result.TrueDuplicatePairs++
			case !oracleSame && predictedSame:
				result.FalseDuplicatePairs++
			case oracleSame && !predictedSame:
				result.MissedDuplicatePairs++
			default:
				result.TrueDistinctPairs++
			}
		}
	}
	result.PairwisePrecision = ratioMetric(result.TrueDuplicatePairs, result.TrueDuplicatePairs+result.FalseDuplicatePairs, "no_predicted_duplicate_pairs")
	result.PairwiseRecall = ratioMetric(result.TrueDuplicatePairs, result.TrueDuplicatePairs+result.MissedDuplicatePairs, "no_oracle_duplicate_pairs")
	result.FalseMergeRate = ratioMetric(result.FalseDuplicatePairs, result.TrueDuplicatePairs+result.FalseDuplicatePairs, "no_predicted_duplicate_pairs")
	result.ExactPartitionMatch = result.FalseDuplicatePairs == 0 && result.MissedDuplicatePairs == 0
	return result, nil
}

func (run NormalizationQualityRun) Validate() error {
	if run.SchemaVersion != NormalizationQualityRunSchemaVersion || run.Authority != NormalizationQualityAuthority {
		return fmt.Errorf("normalization quality run has unsupported schema or authority")
	}
	if err := validateID("quality_run_id", run.QualityRunID); err != nil {
		return err
	}
	if err := validateID("policy_revision", run.PolicyRevision); err != nil {
		return err
	}
	if len(run.Cases) == 0 {
		return fmt.Errorf("normalization quality run requires cases")
	}
	if len(run.Cases) > 1024 {
		return fmt.Errorf("normalization quality run supports at most 1024 cases")
	}
	if run.CreatedAt.IsZero() || run.CreatedAt.Location() != time.UTC {
		return fmt.Errorf("created_at must be non-zero UTC")
	}
	computed := NormalizationQualitySummary{}
	previous := ""
	for index, item := range run.Cases {
		if err := item.validate(); err != nil {
			return err
		}
		if index > 0 && item.CaseID <= previous {
			return fmt.Errorf("cases must be uniquely sorted by case_id")
		}
		previous = item.CaseID
		accumulateNormalizationQuality(&computed, item)
	}
	finalizeNormalizationQualitySummary(&computed)
	if err := run.Summary.ValidateAggregate(); err != nil {
		return err
	}
	if !reflect.DeepEqual(computed, run.Summary) {
		return fmt.Errorf("normalization quality summary does not match cases")
	}
	return nil
}

func (item NormalizationQualityCaseResult) validate() error {
	for name, value := range map[string]string{
		"case_id": item.CaseID, "oracle_id": item.OracleID,
		"source_review_run_id": item.SourceReviewRunID,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if item.Split == SplitUnassigned {
		return fmt.Errorf("normalization quality case requires assigned split")
	}
	if err := item.Split.Validate(); err != nil {
		return err
	}
	for name, binding := range map[string]struct {
		ref      runmodel.ArtifactRef
		contract string
	}{
		"source_review_run_ref":        {item.SourceReviewRunRef, runmodel.ContractReviewRun},
		"raw_candidate_collection_ref": {item.RawCandidateCollectionRef, runmodel.ContractAgentReviewRawCandidates},
	} {
		if err := binding.ref.Validate(); err != nil || binding.ref.Contract != binding.contract {
			return fmt.Errorf("%s is invalid", name)
		}
	}
	if err := item.OracleBinding.Validate(); err != nil ||
		item.OracleBinding.OracleID != item.OracleID || item.OracleBinding.CaseID != item.CaseID {
		return fmt.Errorf("oracle_binding is invalid")
	}
	if item.EligibleRawCandidates == 0 || item.OracleUniqueClaims == 0 ||
		item.PredictedUniqueClaims == 0 || item.OracleUniqueClaims > item.EligibleRawCandidates ||
		item.PredictedUniqueClaims > item.EligibleRawCandidates {
		return fmt.Errorf("normalization quality case contains invalid candidate/class counts")
	}
	if item.UniqueClaimDelta != int64(item.PredictedUniqueClaims)-int64(item.OracleUniqueClaims) {
		return fmt.Errorf("normalization quality case contains invalid unique claim delta")
	}
	pairs := item.TrueDuplicatePairs + item.FalseDuplicatePairs + item.MissedDuplicatePairs + item.TrueDistinctPairs
	wantPairs := uint64(item.EligibleRawCandidates) * uint64(item.EligibleRawCandidates-1) / 2
	if pairs != wantPairs {
		return fmt.Errorf("normalization quality case pair counts are incomplete")
	}
	if item.PairwisePrecision != ratioMetric(item.TrueDuplicatePairs, item.TrueDuplicatePairs+item.FalseDuplicatePairs, "no_predicted_duplicate_pairs") ||
		item.PairwiseRecall != ratioMetric(item.TrueDuplicatePairs, item.TrueDuplicatePairs+item.MissedDuplicatePairs, "no_oracle_duplicate_pairs") ||
		item.FalseMergeRate != ratioMetric(item.FalseDuplicatePairs, item.TrueDuplicatePairs+item.FalseDuplicatePairs, "no_predicted_duplicate_pairs") {
		return fmt.Errorf("normalization quality case ratio metrics are inconsistent")
	}
	if item.ExactPartitionMatch != (item.FalseDuplicatePairs == 0 && item.MissedDuplicatePairs == 0) {
		return fmt.Errorf("normalization quality case exact match is inconsistent")
	}
	return nil
}

func partitionIndex(classes []NormalizationEquivalenceClass, eligible []string) (map[string]string, error) {
	if len(classes) == 0 {
		return nil, fmt.Errorf("equivalence classes are empty")
	}
	result := make(map[string]string, len(eligible))
	for _, class := range classes {
		if err := validateID("class_id", class.ClassID); err != nil {
			return nil, err
		}
		if len(class.RawCandidateIDs) == 0 {
			return nil, fmt.Errorf("class %q is empty", class.ClassID)
		}
		for _, id := range class.RawCandidateIDs {
			if _, exists := result[id]; exists {
				return nil, fmt.Errorf("raw candidate %q appears in multiple classes", id)
			}
			result[id] = class.ClassID
		}
	}
	if len(result) != len(eligible) {
		return nil, fmt.Errorf("partition size differs from eligible candidate set")
	}
	for _, id := range eligible {
		if _, exists := result[id]; !exists {
			return nil, fmt.Errorf("eligible raw candidate %q is missing", id)
		}
	}
	return result, nil
}

func ratioMetric(numerator, denominator uint64, unavailableReason string) RatioMetric {
	if denominator == 0 {
		return RatioMetric{ReasonCode: unavailableReason}
	}
	return RatioMetric{Available: true, Numerator: numerator, Denominator: denominator, ValuePPM: uint32((numerator*1_000_000 + denominator/2) / denominator)}
}

func accumulateNormalizationQuality(summary *NormalizationQualitySummary, item NormalizationQualityCaseResult) {
	summary.Cases++
	summary.EligibleRawCandidates += item.EligibleRawCandidates
	summary.OracleUniqueClaims += item.OracleUniqueClaims
	summary.PredictedUniqueClaims += item.PredictedUniqueClaims
	summary.TrueDuplicatePairs += item.TrueDuplicatePairs
	summary.FalseDuplicatePairs += item.FalseDuplicatePairs
	summary.MissedDuplicatePairs += item.MissedDuplicatePairs
	summary.TrueDistinctPairs += item.TrueDistinctPairs
	if item.ExactPartitionMatch {
		summary.ExactPartitionMatches++
	}
	if item.PolicyExposed {
		summary.PolicyExposedCases++
	} else {
		summary.IndependentEvidenceCases++
	}
}

func finalizeNormalizationQualitySummary(summary *NormalizationQualitySummary) {
	summary.UniqueClaimDelta = int64(summary.PredictedUniqueClaims) - int64(summary.OracleUniqueClaims)
	summary.PairwisePrecision = ratioMetric(summary.TrueDuplicatePairs, summary.TrueDuplicatePairs+summary.FalseDuplicatePairs, "no_predicted_duplicate_pairs")
	summary.PairwiseRecall = ratioMetric(summary.TrueDuplicatePairs, summary.TrueDuplicatePairs+summary.MissedDuplicatePairs, "no_oracle_duplicate_pairs")
	summary.FalseMergeRate = ratioMetric(summary.FalseDuplicatePairs, summary.TrueDuplicatePairs+summary.FalseDuplicatePairs, "no_predicted_duplicate_pairs")
}

func uniquelySortedIDs(values []string) bool {
	for index, value := range values {
		if validateID("id", value) != nil || (index > 0 && value <= values[index-1]) {
			return false
		}
	}
	return true
}
