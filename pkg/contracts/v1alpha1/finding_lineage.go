package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

const FindingLineageSchemaVersion = "argus.finding_lineage.v1alpha1"

type FindingLineageRelationType string

const (
	FindingLineageContinued  FindingLineageRelationType = "continued"
	FindingLineageSplit      FindingLineageRelationType = "split"
	FindingLineageMerged     FindingLineageRelationType = "merged"
	FindingLineageIntroduced FindingLineageRelationType = "introduced"
	FindingLineageResolved   FindingLineageRelationType = "resolved"
)

type FindingLineageMatchMethod string

const (
	FindingLineageExactFingerprint FindingLineageMatchMethod = "exact_fingerprint"
	FindingLineageStableFamily     FindingLineageMatchMethod = "stable_family"
	FindingLineageGitRenameFamily  FindingLineageMatchMethod = "git_rename_family"
	FindingLineageUnmatched        FindingLineageMatchMethod = "unmatched"
)

// FindingLineagePolicy is frozen into every lineage artifact. The policy is
// deliberately conservative: it has no fuzzy text, embedding, or LLM match.
type FindingLineagePolicy struct {
	PolicyID           string   `json:"policy_id"`
	Revision           string   `json:"revision"`
	SHA256             string   `json:"sha256"`
	FamilyFields       []string `json:"family_fields"`
	ManyToManyAction   string   `json:"many_to_many_action"`
	AncestryAuthority  string   `json:"ancestry_authority"`
	RenameThresholdBPS uint16   `json:"rename_threshold_bps,omitempty"`
}

type FindingLineageGitAncestry struct {
	Authority                string `json:"authority"`
	RepositoryIdentitySHA256 string `json:"repository_identity_sha256"`
	ObjectFormat             string `json:"object_format"`
	BaselineHeadOID          string `json:"baseline_head_oid"`
	VariantHeadOID           string `json:"variant_head_oid"`
	MergeBaseOID             string `json:"merge_base_oid"`
	BaselineIsAncestor       bool   `json:"baseline_is_ancestor"`
	EvidenceSHA256           string `json:"evidence_sha256"`
}

type FindingLineagePathMapping struct {
	BaselinePath  string `json:"baseline_path"`
	VariantPath   string `json:"variant_path"`
	SimilarityBPS uint16 `json:"similarity_bps"`
}

type FindingLineageRunBinding struct {
	RunID                    string        `json:"run_id"`
	RunSHA256                string        `json:"run_sha256"`
	TenantID                 string        `json:"tenant_id"`
	WorkspaceID              string        `json:"workspace_id"`
	Repository               RepositoryRef `json:"repository"`
	TargetMode               ReviewMode    `json:"target_mode"`
	BaseRevision             string        `json:"base_revision"`
	HeadRevision             string        `json:"head_revision"`
	TargetDigest             string        `json:"target_digest"`
	ReviewSpecSHA256         string        `json:"review_spec_sha256"`
	TargetSnapshotSHA256     string        `json:"target_snapshot_sha256"`
	CandidateSetSHA256       string        `json:"candidate_set_sha256"`
	VerificationLedgerSHA256 string        `json:"verification_ledger_sha256"`
	CalibrationLedgerSHA256  string        `json:"calibration_ledger_sha256"`
	SuppressionLedgerSHA256  string        `json:"suppression_ledger_sha256"`
	GovernedReportSHA256     string        `json:"governed_report_sha256"`
}

type FindingLineageFinding struct {
	RunID       string                 `json:"run_id"`
	FindingID   string                 `json:"finding_id"`
	Fingerprint string                 `json:"fingerprint"`
	FamilyKey   string                 `json:"family_key"`
	CandidateID string                 `json:"candidate_id"`
	Dimension   VersionedRef           `json:"dimension"`
	Category    string                 `json:"category"`
	Severity    HypothesisSeverity     `json:"severity"`
	Title       string                 `json:"title"`
	Anchor      HypothesisSourceAnchor `json:"anchor"`
}

type FindingLineageRelation struct {
	RelationID         string                     `json:"relation_id"`
	Type               FindingLineageRelationType `json:"type"`
	Method             FindingLineageMatchMethod  `json:"method"`
	ReasonCode         string                     `json:"reason_code"`
	FamilyKey          string                     `json:"family_key"`
	BaselineFindingIDs []string                   `json:"baseline_finding_ids"`
	VariantFindingIDs  []string                   `json:"variant_finding_ids"`
}

type FindingLineageSummary struct {
	BaselineFindings uint32 `json:"baseline_findings"`
	VariantFindings  uint32 `json:"variant_findings"`
	Continued        uint32 `json:"continued"`
	Split            uint32 `json:"split"`
	Merged           uint32 `json:"merged"`
	Introduced       uint32 `json:"introduced"`
	Resolved         uint32 `json:"resolved"`
}

// FindingLineage is an immutable, evidence-bound interpretation between two
// committed formal review runs. Its policy declares whether baseline ->
// variant order is merely caller-selected or proven by a local Git object
// graph.
type FindingLineage struct {
	SchemaVersion    string                      `json:"schema_version"`
	LineageID        string                      `json:"lineage_id"`
	Policy           FindingLineagePolicy        `json:"policy"`
	Baseline         FindingLineageRunBinding    `json:"baseline"`
	Variant          FindingLineageRunBinding    `json:"variant"`
	Ancestry         *FindingLineageGitAncestry  `json:"ancestry,omitempty"`
	PathMappings     []FindingLineagePathMapping `json:"path_mappings"`
	BaselineFindings []FindingLineageFinding     `json:"baseline_findings"`
	VariantFindings  []FindingLineageFinding     `json:"variant_findings"`
	Relations        []FindingLineageRelation    `json:"relations"`
	Summary          FindingLineageSummary       `json:"summary"`
	GeneratedAt      time.Time                   `json:"generated_at"`
}

func SealFindingLineage(lineage FindingLineage) (FindingLineage, error) {
	lineage.SchemaVersion = FindingLineageSchemaVersion
	lineage.LineageID = ""
	digest, err := DigestFindingLineage(lineage)
	if err != nil {
		return FindingLineage{}, err
	}
	lineage.LineageID = "finding-lineage-" + digest[:24]
	if err := lineage.Validate(); err != nil {
		return FindingLineage{}, err
	}
	return lineage, nil
}

func DigestFindingLineage(lineage FindingLineage) (string, error) {
	copy := lineage
	copy.LineageID = ""
	data, err := json.Marshal(copy)
	if err != nil {
		return "", fmt.Errorf("marshal FindingLineage digest input: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func SealFindingLineagePolicy(policy FindingLineagePolicy) (FindingLineagePolicy, error) {
	policy.SHA256 = ""
	data, err := json.Marshal(policy)
	if err != nil {
		return FindingLineagePolicy{}, err
	}
	digest := sha256.Sum256(data)
	policy.SHA256 = hex.EncodeToString(digest[:])
	if err := policy.Validate(); err != nil {
		return FindingLineagePolicy{}, err
	}
	return policy, nil
}

func SealFindingLineageGitAncestry(evidence FindingLineageGitAncestry, mappings []FindingLineagePathMapping) (FindingLineageGitAncestry, error) {
	evidence.EvidenceSHA256 = ""
	data, err := json.Marshal(struct {
		Evidence FindingLineageGitAncestry   `json:"evidence"`
		Mappings []FindingLineagePathMapping `json:"mappings"`
	}{evidence, mappings})
	if err != nil {
		return FindingLineageGitAncestry{}, err
	}
	digest := sha256.Sum256(data)
	evidence.EvidenceSHA256 = hex.EncodeToString(digest[:])
	return evidence, nil
}

func DecodeFindingLineage(data []byte) (FindingLineage, error) {
	var lineage FindingLineage
	if err := decodeAgentReviewShadowJSON(data, &lineage); err != nil {
		return FindingLineage{}, fmt.Errorf("decode FindingLineage: %w", err)
	}
	if err := lineage.Validate(); err != nil {
		return FindingLineage{}, err
	}
	return lineage, nil
}

func (policy FindingLineagePolicy) Validate() error {
	for name, value := range map[string]string{
		"policy_id": policy.PolicyID, "revision": policy.Revision,
	} {
		if err := requireIdentifier(name, value); err != nil {
			return err
		}
	}
	if err := requireSHA256("policy.sha256", policy.SHA256); err != nil {
		return err
	}
	wantFields := []string{"category", "dimension", "path", "title_normalized"}
	if !slices.Equal(policy.FamilyFields, wantFields) {
		return fmt.Errorf("policy.family_fields must equal the canonical v1alpha1 fields")
	}
	if policy.ManyToManyAction != "leave_unmatched" {
		return fmt.Errorf("policy.many_to_many_action must be leave_unmatched")
	}
	switch policy.AncestryAuthority {
	case "caller_order_unverified":
		if policy.RenameThresholdBPS != 0 {
			return fmt.Errorf("unverified policy cannot set rename threshold")
		}
	case "local_git_object_graph":
		if policy.RenameThresholdBPS != 5000 {
			return fmt.Errorf("local Git policy rename threshold must be 5000 BPS")
		}
	default:
		return fmt.Errorf("policy.ancestry_authority is unsupported")
	}
	copy := policy
	copy.SHA256 = ""
	data, err := json.Marshal(copy)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(data)
	if policy.SHA256 != hex.EncodeToString(digest[:]) {
		return fmt.Errorf("policy.sha256 does not match canonical policy")
	}
	return nil
}

func (lineage FindingLineage) Validate() error {
	if lineage.SchemaVersion != FindingLineageSchemaVersion {
		return fmt.Errorf("unsupported FindingLineage schema %q", lineage.SchemaVersion)
	}
	if err := requireIdentifier("lineage_id", lineage.LineageID); err != nil {
		return err
	}
	if err := lineage.Policy.Validate(); err != nil {
		return fmt.Errorf("policy: %w", err)
	}
	if err := lineage.Baseline.validate("baseline"); err != nil {
		return err
	}
	if err := lineage.Variant.validate("variant"); err != nil {
		return err
	}
	if lineage.Baseline.RunID == lineage.Variant.RunID ||
		lineage.Baseline.TargetDigest == lineage.Variant.TargetDigest {
		return fmt.Errorf("lineage requires distinct runs and target digests")
	}
	if lineage.Baseline.TenantID != lineage.Variant.TenantID ||
		lineage.Baseline.WorkspaceID != lineage.Variant.WorkspaceID ||
		lineage.Baseline.Repository != lineage.Variant.Repository {
		return fmt.Errorf("lineage runs must belong to the same tenant, workspace, and repository")
	}
	if lineage.Baseline.TargetMode != lineage.Variant.TargetMode {
		return fmt.Errorf("lineage runs must use the same target mode")
	}
	if lineage.PathMappings == nil {
		return fmt.Errorf("path_mappings must be an explicit array")
	}
	if lineage.Policy.AncestryAuthority == "local_git_object_graph" {
		if lineage.Ancestry == nil {
			return fmt.Errorf("local Git lineage requires ancestry evidence")
		}
		if err := lineage.Ancestry.validate(lineage.Policy, lineage.PathMappings); err != nil {
			return err
		}
		if lineage.Ancestry.BaselineHeadOID != lineage.Baseline.HeadRevision ||
			lineage.Ancestry.VariantHeadOID != lineage.Variant.HeadRevision {
			return fmt.Errorf("Git ancestry does not bind source run head revisions")
		}
	} else if lineage.Ancestry != nil || len(lineage.PathMappings) != 0 {
		return fmt.Errorf("caller-order lineage cannot claim Git ancestry or path mappings")
	}
	if lineage.BaselineFindings == nil || lineage.VariantFindings == nil || lineage.Relations == nil {
		return fmt.Errorf("finding and relation arrays must be explicit")
	}
	baseline, err := validateLineageFindings("baseline_findings", lineage.Baseline.RunID, lineage.BaselineFindings)
	if err != nil {
		return err
	}
	variant, err := validateLineageFindings("variant_findings", lineage.Variant.RunID, lineage.VariantFindings)
	if err != nil {
		return err
	}
	if err := validateLineageRelations(lineage.Relations, baseline, variant, lineage.PathMappings); err != nil {
		return err
	}
	wantSummary := summarizeFindingLineage(lineage)
	if lineage.Summary != wantSummary {
		return fmt.Errorf("summary does not match lineage facts")
	}
	if err := validateAgentReviewTime("generated_at", lineage.GeneratedAt); err != nil {
		return err
	}
	want, err := DigestFindingLineage(lineage)
	if err != nil {
		return err
	}
	if lineage.LineageID != "finding-lineage-"+want[:24] {
		return fmt.Errorf("lineage_id does not match canonical lineage facts")
	}
	return nil
}

func (binding FindingLineageRunBinding) validate(name string) error {
	for field, value := range map[string]string{
		name + ".run_id": binding.RunID, name + ".tenant_id": binding.TenantID,
		name + ".workspace_id":             binding.WorkspaceID,
		name + ".repository.provider":      binding.Repository.Provider,
		name + ".repository.repository_id": binding.Repository.RepositoryID,
		name + ".base_revision":            binding.BaseRevision, name + ".head_revision": binding.HeadRevision,
	} {
		if err := requireIdentifier(field, value); err != nil {
			return err
		}
	}
	if binding.TargetMode != ReviewModeDiff && binding.TargetMode != ReviewModeSelection && binding.TargetMode != ReviewModeScope {
		return fmt.Errorf("%s.target_mode is unsupported", name)
	}
	for field, value := range map[string]string{
		"run_sha256": binding.RunSHA256, "target_digest": binding.TargetDigest,
		"review_spec_sha256":         binding.ReviewSpecSHA256,
		"target_snapshot_sha256":     binding.TargetSnapshotSHA256,
		"candidate_set_sha256":       binding.CandidateSetSHA256,
		"verification_ledger_sha256": binding.VerificationLedgerSHA256,
		"calibration_ledger_sha256":  binding.CalibrationLedgerSHA256,
		"suppression_ledger_sha256":  binding.SuppressionLedgerSHA256,
		"governed_report_sha256":     binding.GovernedReportSHA256,
	} {
		if err := requireSHA256(name+"."+field, value); err != nil {
			return err
		}
	}
	return nil
}

func (evidence FindingLineageGitAncestry) validate(policy FindingLineagePolicy, mappings []FindingLineagePathMapping) error {
	if evidence.Authority != "local_git_object_graph" {
		return fmt.Errorf("ancestry authority is unsupported")
	}
	if err := requireSHA256("ancestry.repository_identity_sha256", evidence.RepositoryIdentitySHA256); err != nil {
		return err
	}
	if evidence.ObjectFormat != "sha1" && evidence.ObjectFormat != "sha256" {
		return fmt.Errorf("ancestry object_format is unsupported")
	}
	length := 40
	if evidence.ObjectFormat == "sha256" {
		length = 64
	}
	for name, value := range map[string]string{"baseline_head_oid": evidence.BaselineHeadOID, "variant_head_oid": evidence.VariantHeadOID, "merge_base_oid": evidence.MergeBaseOID} {
		if len(value) != length || !isLowercaseHex(value) {
			return fmt.Errorf("ancestry.%s does not match object format", name)
		}
	}
	if !evidence.BaselineIsAncestor || evidence.MergeBaseOID != evidence.BaselineHeadOID || evidence.BaselineHeadOID == evidence.VariantHeadOID {
		return fmt.Errorf("ancestry does not prove strict baseline-before-variant order")
	}
	if mappings == nil {
		return fmt.Errorf("path_mappings must be explicit")
	}
	seenBefore, seenAfter := map[string]struct{}{}, map[string]struct{}{}
	for index, mapping := range mappings {
		if err := requireRepositoryPath(fmt.Sprintf("path_mappings[%d].baseline_path", index), mapping.BaselinePath); err != nil {
			return err
		}
		if err := requireRepositoryPath(fmt.Sprintf("path_mappings[%d].variant_path", index), mapping.VariantPath); err != nil {
			return err
		}
		if mapping.BaselinePath == mapping.VariantPath || mapping.SimilarityBPS < policy.RenameThresholdBPS || mapping.SimilarityBPS > 10000 {
			return fmt.Errorf("path_mappings[%d] is not an admitted rename", index)
		}
		if index > 0 && (mappings[index-1].BaselinePath > mapping.BaselinePath || mappings[index-1].BaselinePath == mapping.BaselinePath && mappings[index-1].VariantPath >= mapping.VariantPath) {
			return fmt.Errorf("path_mappings must be sorted and unique")
		}
		if _, exists := seenBefore[mapping.BaselinePath]; exists {
			return fmt.Errorf("baseline path appears in multiple mappings")
		}
		seenBefore[mapping.BaselinePath] = struct{}{}
		if _, exists := seenAfter[mapping.VariantPath]; exists {
			return fmt.Errorf("variant path appears in multiple mappings")
		}
		seenAfter[mapping.VariantPath] = struct{}{}
	}
	sealed, err := SealFindingLineageGitAncestry(evidence, mappings)
	if err != nil || sealed.EvidenceSHA256 != evidence.EvidenceSHA256 {
		return fmt.Errorf("ancestry evidence_sha256 does not bind ancestry and mappings")
	}
	return nil
}

func isLowercaseHex(value string) bool {
	if value == "" || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded)*2 == len(value)
}

func validateLineageFindings(name, runID string, findings []FindingLineageFinding) (map[string]FindingLineageFinding, error) {
	index := make(map[string]FindingLineageFinding, len(findings))
	previous := ""
	for position, finding := range findings {
		for field, value := range map[string]string{
			"finding_id": finding.FindingID, "candidate_id": finding.CandidateID,
			"category": finding.Category,
		} {
			if err := requireIdentifier(fmt.Sprintf("%s[%d].%s", name, position, field), value); err != nil {
				return nil, err
			}
		}
		if finding.RunID != runID {
			return nil, fmt.Errorf("%s[%d] does not bind its run", name, position)
		}
		if err := requireSHA256(name+".fingerprint", finding.Fingerprint); err != nil {
			return nil, err
		}
		if err := requireSHA256(name+".family_key", finding.FamilyKey); err != nil {
			return nil, err
		}
		if err := finding.Dimension.validate(name + ".dimension"); err != nil {
			return nil, err
		}
		if err := finding.Anchor.validate(name + ".anchor"); err != nil {
			return nil, err
		}
		if err := requireBoundedAgentReviewText(name+".title", finding.Title, 512, true); err != nil {
			return nil, err
		}
		if finding.FamilyKey != findingLineageFamilyKey(
			finding.Category, finding.Dimension, finding.Anchor.Path, finding.Title,
		) {
			return nil, fmt.Errorf("%s[%d].family_key does not match canonical fields", name, position)
		}
		switch finding.Severity {
		case HypothesisSeverityLow, HypothesisSeverityMedium, HypothesisSeverityHigh, HypothesisSeverityCritical:
		default:
			return nil, fmt.Errorf("%s[%d].severity is unsupported", name, position)
		}
		if position > 0 && finding.FindingID <= previous {
			return nil, fmt.Errorf("%s must be uniquely sorted by finding_id", name)
		}
		previous = finding.FindingID
		index[finding.FindingID] = finding
	}
	return index, nil
}

func validateLineageRelations(relations []FindingLineageRelation, baseline, variant map[string]FindingLineageFinding, mappings []FindingLineagePathMapping) error {
	renames := map[string]string{}
	for _, mapping := range mappings {
		renames[mapping.BaselinePath] = mapping.VariantPath
	}
	seenBaseline := make(map[string]struct{}, len(baseline))
	seenVariant := make(map[string]struct{}, len(variant))
	previous := ""
	for index, relation := range relations {
		if err := requireIdentifier("relation.relation_id", relation.RelationID); err != nil {
			return err
		}
		if err := requireIdentifier("relation.reason_code", relation.ReasonCode); err != nil {
			return err
		}
		if err := requireSHA256("relation.family_key", relation.FamilyKey); err != nil {
			return err
		}
		if relation.BaselineFindingIDs == nil || relation.VariantFindingIDs == nil ||
			!slices.IsSorted(relation.BaselineFindingIDs) || !slices.IsSorted(relation.VariantFindingIDs) ||
			hasAdjacentStringDuplicate(relation.BaselineFindingIDs) || hasAdjacentStringDuplicate(relation.VariantFindingIDs) {
			return fmt.Errorf("relation %d finding IDs must be explicit sorted unique arrays", index)
		}
		if err := validateRelationCardinality(relation); err != nil {
			return fmt.Errorf("relation %d: %w", index, err)
		}
		for _, id := range relation.BaselineFindingIDs {
			finding, exists := baseline[id]
			familyMatches := finding.FamilyKey == relation.FamilyKey
			if relation.Method == FindingLineageGitRenameFamily {
				mapped, ok := renames[finding.Anchor.Path]
				familyMatches = ok && findingLineageFamilyKey(finding.Category, finding.Dimension, mapped, finding.Title) == relation.FamilyKey
			}
			if !exists || relation.Method != FindingLineageExactFingerprint && !familyMatches {
				return fmt.Errorf("relation %d baseline finding is missing or outside family", index)
			}
			if _, duplicate := seenBaseline[id]; duplicate {
				return fmt.Errorf("baseline finding %q appears in multiple relations", id)
			}
			seenBaseline[id] = struct{}{}
		}
		for _, id := range relation.VariantFindingIDs {
			finding, exists := variant[id]
			if !exists || relation.Method != FindingLineageExactFingerprint && finding.FamilyKey != relation.FamilyKey {
				return fmt.Errorf("relation %d variant finding is missing or outside family", index)
			}
			if _, duplicate := seenVariant[id]; duplicate {
				return fmt.Errorf("variant finding %q appears in multiple relations", id)
			}
			seenVariant[id] = struct{}{}
		}
		wantID := FindingLineageRelationID(relation)
		if relation.Method == FindingLineageExactFingerprint {
			before := baseline[relation.BaselineFindingIDs[0]]
			after := variant[relation.VariantFindingIDs[0]]
			if before.Fingerprint != after.Fingerprint || relation.FamilyKey != ExactFindingLineageKey(before.Fingerprint) {
				return fmt.Errorf("relation %d exact fingerprint evidence does not close", index)
			}
		}
		if relation.RelationID != wantID {
			return fmt.Errorf("relation %d identity does not match canonical fields", index)
		}
		if index > 0 && relation.RelationID <= previous {
			return fmt.Errorf("relations must be uniquely sorted by relation_id")
		}
		previous = relation.RelationID
	}
	if len(seenBaseline) != len(baseline) || len(seenVariant) != len(variant) {
		return fmt.Errorf("relations must cover every finding exactly once")
	}
	return nil
}

func validateRelationCardinality(relation FindingLineageRelation) error {
	b, v := len(relation.BaselineFindingIDs), len(relation.VariantFindingIDs)
	switch relation.Type {
	case FindingLineageContinued:
		if b != 1 || v != 1 || (relation.Method != FindingLineageExactFingerprint && relation.Method != FindingLineageStableFamily && relation.Method != FindingLineageGitRenameFamily) {
			return fmt.Errorf("continued requires one-to-one conservative evidence")
		}
	case FindingLineageSplit:
		if b != 1 || v < 2 || (relation.Method != FindingLineageStableFamily && relation.Method != FindingLineageGitRenameFamily) {
			return fmt.Errorf("split requires one-to-many stable_family")
		}
	case FindingLineageMerged:
		if b < 2 || v != 1 || (relation.Method != FindingLineageStableFamily && relation.Method != FindingLineageGitRenameFamily) {
			return fmt.Errorf("merged requires many-to-one stable_family")
		}
	case FindingLineageIntroduced:
		if b != 0 || v != 1 || relation.Method != FindingLineageUnmatched {
			return fmt.Errorf("introduced requires one unmatched variant")
		}
	case FindingLineageResolved:
		if b != 1 || v != 0 || relation.Method != FindingLineageUnmatched {
			return fmt.Errorf("resolved requires one unmatched baseline")
		}
	default:
		return fmt.Errorf("unsupported relation type %q", relation.Type)
	}
	if relation.Method == FindingLineageExactFingerprint {
		if relation.ReasonCode != "exact_canonical_fingerprint" {
			return fmt.Errorf("exact fingerprint relation has invalid reason")
		}
	} else if relation.Method == FindingLineageStableFamily {
		if relation.ReasonCode != "dimension_category_path_title_family" {
			return fmt.Errorf("stable family relation has invalid reason")
		}
	} else if relation.Method == FindingLineageGitRenameFamily {
		if relation.ReasonCode != "git_rename_dimension_category_title_family" {
			return fmt.Errorf("Git rename family relation has invalid reason")
		}
	} else if relation.ReasonCode != "no_conservative_match" && relation.ReasonCode != "ambiguous_many_to_many_family" {
		return fmt.Errorf("unmatched relation has invalid reason")
	}
	return nil
}

func FindingLineageRelationID(relation FindingLineageRelation) string {
	copy := relation
	copy.RelationID = ""
	data, _ := json.Marshal(copy)
	digest := sha256.Sum256(data)
	return "lineage-relation-" + hex.EncodeToString(digest[:])[:24]
}

func summarizeFindingLineage(lineage FindingLineage) FindingLineageSummary {
	summary := FindingLineageSummary{
		BaselineFindings: uint32(len(lineage.BaselineFindings)),
		VariantFindings:  uint32(len(lineage.VariantFindings)),
	}
	for _, relation := range lineage.Relations {
		switch relation.Type {
		case FindingLineageContinued:
			summary.Continued++
		case FindingLineageSplit:
			summary.Split++
		case FindingLineageMerged:
			summary.Merged++
		case FindingLineageIntroduced:
			summary.Introduced++
		case FindingLineageResolved:
			summary.Resolved++
		}
	}
	return summary
}

func CanonicalizeFindingLineage(lineage *FindingLineage) {
	sort.Slice(lineage.PathMappings, func(i, j int) bool {
		if lineage.PathMappings[i].BaselinePath != lineage.PathMappings[j].BaselinePath {
			return lineage.PathMappings[i].BaselinePath < lineage.PathMappings[j].BaselinePath
		}
		return lineage.PathMappings[i].VariantPath < lineage.PathMappings[j].VariantPath
	})
	sort.Slice(lineage.BaselineFindings, func(i, j int) bool {
		return lineage.BaselineFindings[i].FindingID < lineage.BaselineFindings[j].FindingID
	})
	sort.Slice(lineage.VariantFindings, func(i, j int) bool {
		return lineage.VariantFindings[i].FindingID < lineage.VariantFindings[j].FindingID
	})
	for index := range lineage.Relations {
		slices.Sort(lineage.Relations[index].BaselineFindingIDs)
		slices.Sort(lineage.Relations[index].VariantFindingIDs)
		lineage.Relations[index].RelationID = FindingLineageRelationID(lineage.Relations[index])
	}
	sort.Slice(lineage.Relations, func(i, j int) bool { return lineage.Relations[i].RelationID < lineage.Relations[j].RelationID })
	lineage.Summary = summarizeFindingLineage(*lineage)
}

func hasAdjacentStringDuplicate(values []string) bool {
	for index := 1; index < len(values); index++ {
		if values[index] == values[index-1] {
			return true
		}
	}
	return false
}

func FindingLineageFamilyKey(finding GovernedReviewFinding) string {
	return findingLineageFamilyKey(
		finding.Category, finding.Dimension, finding.Anchor.Path, finding.Title,
	)
}

func FindingLineageFamilyKeyForPath(finding FindingLineageFinding, path string) string {
	return findingLineageFamilyKey(finding.Category, finding.Dimension, path, finding.Title)
}

func findingLineageFamilyKey(category string, dimension VersionedRef, path, title string) string {
	data, _ := json.Marshal([]any{
		category,
		dimension,
		path,
		strings.Join(strings.Fields(strings.ToLower(title)), " "),
	})
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func ExactFindingLineageKey(fingerprint string) string {
	digest := sha256.Sum256([]byte("exact-fingerprint\x00" + fingerprint))
	return hex.EncodeToString(digest[:])
}
