package reviewcore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

var stageRevisions = map[StageName]string{
	StageMaterializeTarget: "1",
	StagePlanContext:       "1",
	StageDetect:            "1",
	StageNormalize:         "1",
	StageVerify:            "1",
	StageAdjudicate:        "1",
	StageReport:            "1",
	StagePublish:           "1",
	StageCaptureFeedback:   "1",
	StageExportEvaluation:  "1",
}

var contextArtifactURI = regexp.MustCompile(
	`^artifact://[A-Za-z0-9]([A-Za-z0-9.-]{0,126}[A-Za-z0-9])?/[A-Za-z0-9._~-]+(/[A-Za-z0-9._~-]+)*$`,
)

func (input ReviewInput) Validate() error {
	return input.validateContext(context.Background())
}

func (input ReviewInput) validateContext(ctx context.Context) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if input.SchemaVersion != ReviewInputSchemaVersion {
		return fmt.Errorf("unsupported review input schema %q", input.SchemaVersion)
	}
	if err := validateTextID("target_id", input.TargetID); err != nil {
		return err
	}
	if input.Files == nil {
		return fmt.Errorf("files must be an explicit array")
	}
	files := make(map[string]FileManifestEntry, len(input.Files))
	previous := ""
	for index, file := range input.Files {
		if err := checkContext(ctx); err != nil {
			return err
		}
		if err := file.validate(index); err != nil {
			return err
		}
		if index > 0 && file.Path <= previous {
			return fmt.Errorf("files must be uniquely sorted by path")
		}
		previous = file.Path
		files[file.Path] = file
	}
	if input.Contexts == nil {
		return fmt.Errorf("contexts must be an explicit array")
	}
	previousContextID := ""
	for index, binding := range input.Contexts {
		if err := binding.validate(index); err != nil {
			return err
		}
		contextID := binding.contextID()
		if index > 0 && contextID <= previousContextID {
			return fmt.Errorf("contexts must be uniquely sorted by context_id")
		}
		previousContextID = contextID
	}
	if input.Regions == nil {
		return fmt.Errorf("regions must be an explicit array")
	}
	switch input.TargetMode {
	case TargetModeDiff:
		if input.CanonicalPatch == "" {
			return fmt.Errorf("diff input requires canonical_patch")
		}
		if len(input.Regions) != 0 {
			return fmt.Errorf("diff input derives its regions from canonical_patch")
		}
		if !utf8.ValidString(input.CanonicalPatch) ||
			strings.ContainsRune(input.CanonicalPatch, '\x00') {
			return fmt.Errorf("canonical_patch must be valid UTF-8 without NUL bytes")
		}
		if strings.ContainsRune(input.CanonicalPatch, '\r') ||
			!strings.HasSuffix(input.CanonicalPatch, "\n") {
			return fmt.Errorf("canonical_patch must use LF and end with a newline")
		}
		if _, err := parseUnifiedDiffContext(ctx, input.CanonicalPatch); err != nil {
			return fmt.Errorf("parse canonical_patch: %w", err)
		}
	case TargetModeSelection:
		if input.CanonicalPatch != "" {
			return fmt.Errorf("selection input must not synthesize a Git patch")
		}
		if len(input.Regions) == 0 {
			return fmt.Errorf("selection input requires at least one review region")
		}
	case TargetModeScope:
		if input.CanonicalPatch != "" {
			return fmt.Errorf("scope input must not synthesize a Git patch")
		}
	default:
		return fmt.Errorf("unsupported target_mode %q", input.TargetMode)
	}
	previousPath := ""
	var previousEnd uint32
	for index, region := range input.Regions {
		if err := checkContext(ctx); err != nil {
			return err
		}
		name := fmt.Sprintf("regions[%d]", index)
		if err := validateRepositoryPath(name+".path", region.Path); err != nil {
			return err
		}
		if region.StartLine == 0 || region.EndLine < region.StartLine {
			return fmt.Errorf("%s has an invalid line range", name)
		}
		if err := validateDigest(name+".sha256", region.SHA256); err != nil {
			return err
		}
		file, exists := files[region.Path]
		if !exists || file.SHA256 != region.SHA256 {
			return fmt.Errorf("%s does not bind an exact file manifest entry", name)
		}
		if file.Content == nil {
			return fmt.Errorf("%s requires frozen file content", name)
		}
		if _, exists := lineAt(*file.Content, region.EndLine); !exists {
			return fmt.Errorf("%s extends beyond frozen file content", name)
		}
		if index > 0 {
			if region.Path < previousPath ||
				region.Path == previousPath && region.StartLine <= previousEnd {
				return fmt.Errorf("regions must be uniquely sorted and non-overlapping")
			}
		}
		previousPath = region.Path
		previousEnd = region.EndLine
	}
	return nil
}

func (binding ContextBinding) contextID() string {
	if binding.Ref != nil {
		return binding.Ref.ContextID
	}
	if binding.Gap != nil {
		return binding.Gap.ContextID
	}
	return ""
}

func (binding ContextBinding) Validate() error {
	return binding.validate(0)
}

func (binding ContextBinding) ContextID() string {
	return binding.contextID()
}

func (binding ContextBinding) validate(index int) error {
	name := fmt.Sprintf("contexts[%d]", index)
	payloads := 0
	if binding.Ref != nil {
		payloads++
	}
	if binding.Gap != nil {
		payloads++
	}
	if payloads != 1 {
		return fmt.Errorf("%s must contain exactly one ref or gap", name)
	}
	if binding.Ref != nil {
		return binding.Ref.validate(name + ".ref")
	}
	return binding.Gap.validate(name + ".gap")
}

func (ref ContextRef) validate(name string) error {
	if err := validateContextIdentity(
		name,
		ref.ContextID,
		ref.Kind,
		ref.Revision,
		ref.Digest,
		ref.Coverage,
		ref.Provenance,
	); err != nil {
		return err
	}
	if !contextArtifactURI.MatchString(ref.ArtifactURI) {
		return fmt.Errorf("%s.artifact_uri must be a platform-issued artifact URI", name)
	}
	artifactPath := strings.SplitN(
		strings.TrimPrefix(ref.ArtifactURI, "artifact://"),
		"/",
		2,
	)[1]
	for _, segment := range strings.Split(artifactPath, "/") {
		if segment == "." || segment == ".." {
			return fmt.Errorf("%s.artifact_uri must not traverse artifact namespaces", name)
		}
	}
	if err := validateTextID(name+".contract", ref.Contract); err != nil {
		return err
	}
	if ref.SizeBytes < 0 {
		return fmt.Errorf("%s.size_bytes must not be negative", name)
	}
	return nil
}

func (gap ContextGap) validate(name string) error {
	if err := validateContextIdentity(
		name,
		gap.ContextID,
		gap.Kind,
		gap.Revision,
		gap.Digest,
		gap.Coverage,
		gap.Provenance,
	); err != nil {
		return err
	}
	return validateTextID(name+".reason_code", gap.ReasonCode)
}

func validateContextIdentity(
	name string,
	contextID string,
	kind string,
	revision string,
	digest string,
	coverage ContextCoverage,
	provenance ContextProvenance,
) error {
	for field, value := range map[string]string{
		name + ".context_id": contextID,
		name + ".kind":       kind,
		name + ".revision":   revision,
	} {
		if err := validateTextID(field, value); err != nil {
			return err
		}
	}
	if err := validateDigest(name+".digest", digest); err != nil {
		return err
	}
	if err := coverage.validate(name + ".coverage"); err != nil {
		return err
	}
	return provenance.validate(name + ".provenance")
}

func (coverage ContextCoverage) validate(name string) error {
	if coverage.Spans == nil || coverage.Symbols == nil {
		return fmt.Errorf("%s spans and symbols must be explicit arrays", name)
	}
	if len(coverage.Spans) == 0 && len(coverage.Symbols) == 0 {
		return fmt.Errorf("%s must identify at least one span or symbol", name)
	}
	previousPath := ""
	var previousEnd uint32
	for index, span := range coverage.Spans {
		spanName := fmt.Sprintf("%s.spans[%d]", name, index)
		if err := validateRepositoryPath(spanName+".path", span.Path); err != nil {
			return err
		}
		if span.StartLine == 0 || span.EndLine < span.StartLine {
			return fmt.Errorf("%s has an invalid line range", spanName)
		}
		if index > 0 &&
			(span.Path < previousPath ||
				span.Path == previousPath && span.StartLine <= previousEnd) {
			return fmt.Errorf("%s.spans must be sorted and non-overlapping", name)
		}
		previousPath = span.Path
		previousEnd = span.EndLine
	}
	if !slices.IsSorted(coverage.Symbols) {
		return fmt.Errorf("%s.symbols must be sorted", name)
	}
	for index, symbol := range coverage.Symbols {
		if err := validateTextID(
			fmt.Sprintf("%s.symbols[%d]", name, index),
			symbol,
		); err != nil {
			return err
		}
		if index > 0 && symbol == coverage.Symbols[index-1] {
			return fmt.Errorf("%s.symbols contains duplicate %q", name, symbol)
		}
	}
	return nil
}

func (provenance ContextProvenance) validate(name string) error {
	for field, value := range map[string]string{
		name + ".provider":          provenance.Provider,
		name + ".producer_id":       provenance.ProducerID,
		name + ".producer_revision": provenance.ProducerRevision,
	} {
		if err := validateTextID(field, value); err != nil {
			return err
		}
	}
	return nil
}

func (file FileManifestEntry) validate(index int) error {
	name := fmt.Sprintf("files[%d]", index)
	if err := validateRepositoryPath(name+".path", file.Path); err != nil {
		return err
	}
	if err := validateDigest(name+".sha256", file.SHA256); err != nil {
		return err
	}
	if file.SizeBytes < 0 {
		return fmt.Errorf("%s.size_bytes must not be negative", name)
	}
	if file.Content == nil {
		return nil
	}
	if int64(len(*file.Content)) != file.SizeBytes {
		return fmt.Errorf("%s content size does not match size_bytes", name)
	}
	if digestString(*file.Content) != file.SHA256 {
		return fmt.Errorf("%s content digest does not match sha256", name)
	}
	return nil
}

func (evidence Evidence) Validate() error {
	if err := validateDigest("evidence.id", evidence.ID); err != nil {
		return err
	}
	switch evidence.Kind {
	case EvidencePatchLine:
		if evidence.Claim != "marker_occurs_on_added_line" &&
			evidence.Claim != "ast_pattern_occurs_on_added_line" {
			return fmt.Errorf("patch evidence has unsupported claim %q", evidence.Claim)
		}
	case EvidenceTargetLine:
		if evidence.Claim != "marker_occurs_on_target_line" &&
			evidence.Claim != "ast_pattern_occurs_on_target_line" {
			return fmt.Errorf("target-line evidence has unsupported claim %q", evidence.Claim)
		}
	case EvidenceFileContent:
		if evidence.Claim != "final_file_line_at_anchor" {
			return fmt.Errorf("file-content evidence has unsupported claim %q", evidence.Claim)
		}
	default:
		return fmt.Errorf("unsupported evidence kind %q", evidence.Kind)
	}
	if err := validateDigest("evidence.source_digest", evidence.SourceDigest); err != nil {
		return err
	}
	if err := validateRepositoryPath("evidence.path", evidence.Path); err != nil {
		return err
	}
	if evidence.Line == 0 {
		return fmt.Errorf("evidence.line must be positive")
	}
	if !utf8.ValidString(evidence.Excerpt) || strings.ContainsAny(evidence.Excerpt, "\r\n") {
		return fmt.Errorf("evidence.excerpt must be one UTF-8 line")
	}
	return validateTextID("evidence.claim", evidence.Claim)
}

func (candidate CandidateFinding) Validate() error {
	if err := validateDigest("candidate.id", candidate.ID); err != nil {
		return err
	}
	rule, registered := registeredRuleFor(candidate.RuleID)
	if !registered {
		return fmt.Errorf("unsupported rule %q", candidate.RuleID)
	}
	if candidate.DetectorID != rule.DetectorID ||
		candidate.DetectorRevision != rule.DetectorRevision {
		return fmt.Errorf("candidate detector identity does not match registered rule")
	}
	if err := validateDigest("candidate.target_digest", candidate.TargetDigest); err != nil {
		return err
	}
	if err := validateRepositoryPath("candidate.path", candidate.Path); err != nil {
		return err
	}
	if candidate.StartLine == 0 || candidate.EndLine != candidate.StartLine {
		return fmt.Errorf("candidate line range must identify one positive line")
	}
	if !validSignal(candidate.RuleID, candidate.SignalKind, candidate.Signal) {
		return fmt.Errorf(
			"candidate signal %q/%q is unsupported for rule %q",
			candidate.SignalKind,
			candidate.Signal,
			candidate.RuleID,
		)
	}
	if candidate.Excerpt == "" || strings.ContainsAny(candidate.Excerpt, "\r\n") {
		return fmt.Errorf("candidate excerpt must be one non-empty line")
	}
	signalStart := int(candidate.SignalOffset)
	signalEnd := signalStart + len(candidate.Signal)
	if signalEnd > len(candidate.Excerpt) ||
		candidate.Excerpt[signalStart:signalEnd] != candidate.Signal {
		return fmt.Errorf("candidate signal offset does not identify signal")
	}
	expectedID := stableID(
		"candidate", candidate.DetectorID, candidate.DetectorRevision,
		candidate.TargetDigest, candidate.RuleID, string(candidate.SignalKind),
		candidate.Path, fmt.Sprintf("%d", candidate.StartLine),
		fmt.Sprintf("%d", candidate.SignalOffset), candidate.Excerpt,
	)
	if candidate.ID != expectedID {
		return fmt.Errorf("candidate id does not match canonical fields")
	}
	if len(candidate.Evidence) != 1 {
		return fmt.Errorf("candidate must contain exactly one target evidence")
	}
	for _, evidence := range candidate.Evidence {
		if err := evidence.Validate(); err != nil {
			return err
		}
		if evidence.Kind != EvidencePatchLine && evidence.Kind != EvidenceTargetLine {
			return fmt.Errorf("candidate contains unsupported target evidence")
		}
		if evidence.Path != candidate.Path ||
			evidence.Line != candidate.StartLine || evidence.Excerpt != candidate.Excerpt ||
			evidence.ID != stableID("evidence", string(evidence.Kind), candidate.ID) {
			return fmt.Errorf("candidate contains evidence that does not prove its target location")
		}
		if evidence.Claim != targetEvidenceClaim(candidate.SignalKind, evidence.Kind) {
			return fmt.Errorf("candidate evidence claim does not match its signal kind")
		}
	}
	return nil
}

func (gap DetectionGap) Validate() error {
	if err := validateDigest("detection_gap.id", gap.ID); err != nil {
		return err
	}
	rule, registered := registeredRuleFor(gap.RuleID)
	if !registered {
		return fmt.Errorf("unsupported rule %q", gap.RuleID)
	}
	if gap.DetectorID != rule.DetectorID ||
		gap.DetectorRevision != rule.DetectorRevision {
		return fmt.Errorf("detection gap detector identity does not match registered rule")
	}
	if err := validateDigest("detection_gap.target_digest", gap.TargetDigest); err != nil {
		return err
	}
	if err := validateDigest("detection_gap.source_digest", gap.SourceDigest); err != nil {
		return err
	}
	if err := validateRepositoryPath("detection_gap.path", gap.Path); err != nil {
		return err
	}
	if gap.Line == 0 {
		return fmt.Errorf("detection_gap.line must be positive")
	}
	switch gap.ReasonCode {
	case DetectionGapContentUnavailable, DetectionGapGoParseFailed:
	default:
		return fmt.Errorf("unsupported detection gap reason %q", gap.ReasonCode)
	}
	expectedID := stableID(
		"detection-gap",
		gap.DetectorID,
		gap.DetectorRevision,
		gap.RuleID,
		gap.TargetDigest,
		gap.SourceDigest,
		gap.Path,
		fmt.Sprintf("%d", gap.Line),
		string(gap.ReasonCode),
	)
	if gap.ID != expectedID {
		return fmt.Errorf("detection gap id does not match canonical fields")
	}
	return nil
}

func (finding Finding) Validate() error {
	for name, value := range map[string]string{
		"finding.id":            finding.ID,
		"finding.fingerprint":   finding.Fingerprint,
		"finding.target_digest": finding.TargetDigest,
	} {
		if err := validateDigest(name, value); err != nil {
			return err
		}
	}
	if err := validateTextID("finding.anchor", finding.Anchor); err != nil {
		return err
	}
	rule, registered := registeredRuleFor(finding.RuleID)
	if !registered {
		return fmt.Errorf("unsupported rule %q", finding.RuleID)
	}
	if err := validateTextID("finding.title", finding.Title); err != nil {
		return err
	}
	switch finding.Severity {
	case "info", "low", "medium", "high", "critical":
	default:
		return fmt.Errorf("finding severity %q is unsupported", finding.Severity)
	}
	if err := validateTextID("finding.message", finding.Message); err != nil {
		return err
	}
	if err := validateRepositoryPath("finding.path", finding.Path); err != nil {
		return err
	}
	if finding.StartLine == 0 || finding.EndLine != finding.StartLine {
		return fmt.Errorf("finding line range must identify one positive line")
	}
	if finding.Excerpt == "" || strings.ContainsAny(finding.Excerpt, "\r\n") {
		return fmt.Errorf("finding excerpt must be one non-empty line")
	}
	expectedFingerprint := stableID(
		"fingerprint", finding.RuleID, finding.Anchor, normalizeExcerpt(finding.Excerpt),
	)
	if finding.Fingerprint != expectedFingerprint ||
		finding.ID != stableID("finding", finding.Fingerprint) {
		return fmt.Errorf("finding identity does not match canonical semantic fields")
	}
	if len(finding.Signals) == 0 || !slices.IsSorted(finding.Signals) {
		return fmt.Errorf("finding signals must be a non-empty sorted array")
	}
	for index, signal := range finding.Signals {
		if !validSignal(finding.RuleID, rule.SignalKind, signal) ||
			index > 0 && signal == finding.Signals[index-1] {
			return fmt.Errorf("finding signals contain an invalid or duplicate value")
		}
		if !strings.Contains(finding.Excerpt, signal) {
			return fmt.Errorf("finding signal %q is absent from excerpt", signal)
		}
	}
	if finding.Evidence == nil || len(finding.Evidence) == 0 {
		return fmt.Errorf("finding evidence must be a non-empty array")
	}
	evidenceIDs := make(map[string]struct{}, len(finding.Evidence))
	for _, evidence := range finding.Evidence {
		if err := evidence.Validate(); err != nil {
			return err
		}
		if evidence.Path != finding.Path || evidence.Line != finding.StartLine {
			return fmt.Errorf("finding evidence does not match its anchor location")
		}
		if _, duplicate := evidenceIDs[evidence.ID]; duplicate {
			return fmt.Errorf("finding contains duplicate evidence %q", evidence.ID)
		}
		evidenceIDs[evidence.ID] = struct{}{}
	}
	if finding.Lineage == nil || len(finding.Lineage) == 0 {
		return fmt.Errorf("finding lineage must be a non-empty array")
	}
	winners := 0
	seenCandidates := make(map[string]struct{}, len(finding.Lineage))
	for _, lineage := range finding.Lineage {
		if err := validateDigest("lineage.candidate_id", lineage.CandidateID); err != nil {
			return err
		}
		if _, duplicate := seenCandidates[lineage.CandidateID]; duplicate {
			return fmt.Errorf("finding lineage contains duplicate candidate %q", lineage.CandidateID)
		}
		seenCandidates[lineage.CandidateID] = struct{}{}
		switch lineage.Disposition {
		case LineageWinner:
			winners++
			if lineage.ReasonCode != "canonical_candidate" {
				return fmt.Errorf("winner lineage has invalid reason")
			}
		case LineageDuplicate:
			if lineage.ReasonCode != "semantic_duplicate" {
				return fmt.Errorf("duplicate lineage has invalid reason")
			}
		default:
			return fmt.Errorf("unsupported lineage disposition %q", lineage.Disposition)
		}
	}
	if winners != 1 {
		return fmt.Errorf("finding lineage must contain exactly one winner")
	}
	expectedTargetEvidence := make(map[string]string, len(finding.Lineage)*2)
	for _, lineage := range finding.Lineage {
		expectedTargetEvidence[stableID(
			"evidence", string(EvidencePatchLine), lineage.CandidateID,
		)] = lineage.CandidateID
		expectedTargetEvidence[stableID(
			"evidence", string(EvidenceTargetLine), lineage.CandidateID,
		)] = lineage.CandidateID
	}
	contentEvidence := 0
	coveredCandidates := make(map[string]struct{}, len(finding.Lineage))
	for _, evidence := range finding.Evidence {
		switch evidence.Kind {
		case EvidencePatchLine, EvidenceTargetLine:
			candidateID, expected := expectedTargetEvidence[evidence.ID]
			if !expected {
				return fmt.Errorf("target evidence is not backed by candidate lineage")
			}
			if _, duplicate := coveredCandidates[candidateID]; duplicate {
				return fmt.Errorf("candidate lineage has multiple target evidence records")
			}
			coveredCandidates[candidateID] = struct{}{}
		case EvidenceFileContent:
			contentEvidence++
			if evidence.ID != stableID("evidence", "content", finding.ID, evidence.SourceDigest) {
				return fmt.Errorf("file-content evidence id does not match finding and source")
			}
		}
	}
	if len(coveredCandidates) != len(finding.Lineage) {
		return fmt.Errorf("finding dropped target evidence from candidate lineage")
	}
	if contentEvidence > 1 {
		return fmt.Errorf("finding contains multiple final-file evidence records")
	}
	if err := finding.Verification.validate(); err != nil {
		return err
	}
	for _, evidenceID := range finding.Verification.EvidenceIDs {
		if _, exists := evidenceIDs[evidenceID]; !exists {
			return fmt.Errorf("verification references evidence absent from finding")
		}
	}
	return nil
}

func (verification Verification) validate() error {
	switch verification.Status {
	case VerificationPending:
		if verification.ReasonCode != "not_verified" || verification.EvidenceIDs == nil {
			return fmt.Errorf("pending verification is malformed")
		}
	case VerificationVerified, VerificationRejected, VerificationInconclusive:
		if err := validateTextID("verification.reason_code", verification.ReasonCode); err != nil {
			return err
		}
		if verification.EvidenceIDs == nil || len(verification.EvidenceIDs) == 0 {
			return fmt.Errorf("completed verification requires evidence_ids")
		}
	default:
		return fmt.Errorf("unsupported verification status %q", verification.Status)
	}
	for _, evidenceID := range verification.EvidenceIDs {
		if err := validateDigest("verification.evidence_id", evidenceID); err != nil {
			return err
		}
	}
	if !slices.IsSorted(verification.EvidenceIDs) {
		return fmt.Errorf("verification.evidence_ids must be sorted")
	}
	for index := 1; index < len(verification.EvidenceIDs); index++ {
		if verification.EvidenceIDs[index] == verification.EvidenceIDs[index-1] {
			return fmt.Errorf("verification.evidence_ids contains duplicates")
		}
	}
	return nil
}

func (decision FindingDecision) Validate() error {
	if err := validateDigest("decision.id", decision.ID); err != nil {
		return err
	}
	if err := validateDigest("decision.finding_id", decision.FindingID); err != nil {
		return err
	}
	if decision.Sequence == 0 {
		return fmt.Errorf("decision.sequence must be positive")
	}
	if decision.Sequence == 1 && decision.PriorDecisionID != "" {
		return fmt.Errorf("first decision must not have a prior_decision_id")
	}
	if decision.Sequence > 1 {
		if err := validateDigest("decision.prior_decision_id", decision.PriorDecisionID); err != nil {
			return err
		}
	}
	switch decision.Action {
	case DecisionPublish, DecisionReject, DecisionHumanReview:
	default:
		return fmt.Errorf("unsupported decision action %q", decision.Action)
	}
	if err := validateTextID("decision.reason_code", decision.ReasonCode); err != nil {
		return err
	}
	if decision.EvidenceIDs == nil || len(decision.EvidenceIDs) == 0 {
		return fmt.Errorf("decision.evidence_ids must be a non-empty array")
	}
	for _, evidenceID := range decision.EvidenceIDs {
		if err := validateDigest("decision.evidence_id", evidenceID); err != nil {
			return err
		}
	}
	if !slices.IsSorted(decision.EvidenceIDs) {
		return fmt.Errorf("decision.evidence_ids must be sorted")
	}
	for index := 1; index < len(decision.EvidenceIDs); index++ {
		if decision.EvidenceIDs[index] == decision.EvidenceIDs[index-1] {
			return fmt.Errorf("decision.evidence_ids contains duplicates")
		}
	}
	expectedID := stableID(
		"decision", decision.FindingID, fmt.Sprintf("%d", decision.Sequence),
		string(decision.Action), decision.ReasonCode, strings.Join(decision.EvidenceIDs, ","),
	)
	if decision.ID != expectedID {
		return fmt.Errorf("decision id does not match canonical fields")
	}
	return nil
}

func (report Report) Validate() error {
	if report.SchemaVersion != ReportSchemaVersion {
		return fmt.Errorf("unsupported report schema %q", report.SchemaVersion)
	}
	if err := validateDigest("report.target_digest", report.TargetDigest); err != nil {
		return err
	}
	if report.Findings == nil || report.Decisions == nil {
		return fmt.Errorf("report findings and decisions must be explicit arrays")
	}
	for _, finding := range report.Findings {
		if err := finding.Validate(); err != nil {
			return err
		}
		if finding.TargetDigest != report.TargetDigest {
			return fmt.Errorf("finding target digest does not match report")
		}
	}
	for _, decision := range report.Decisions {
		if err := decision.Validate(); err != nil {
			return err
		}
	}
	expected, err := summarize(report.Findings, report.Decisions)
	if err != nil {
		return err
	}
	if expected != report.Summary {
		return fmt.Errorf("report summary does not match findings and decisions")
	}
	return nil
}

func (result StageResult) Validate() error {
	if result.SchemaVersion != ArtifactSchemaVersion {
		return fmt.Errorf("unsupported stage artifact schema %q", result.SchemaVersion)
	}
	revision, ok := stageRevisions[result.Stage]
	if !ok || result.StageRevision != revision {
		return fmt.Errorf("unsupported stage or revision %q@%q", result.Stage, result.StageRevision)
	}
	if err := validateDigest("stage.input_digest", result.InputDigest); err != nil {
		return err
	}
	if err := validateDigest("stage.target_digest", result.TargetDigest); err != nil {
		return err
	}
	if err := validateDigest("stage.artifact_digest", result.ArtifactDigest); err != nil {
		return err
	}
	if err := result.validateOutput(); err != nil {
		return err
	}
	expected, err := digestStageResult(result)
	if err != nil {
		return err
	}
	if expected != result.ArtifactDigest {
		return fmt.Errorf("stage artifact digest mismatch")
	}
	return nil
}

func (result StageResult) validateOutput() error {
	output := result.Output
	switch result.Stage {
	case StageMaterializeTarget, StagePlanContext, StagePublish,
		StageCaptureFeedback, StageExportEvaluation:
		if output.CandidateFindings != nil || output.DetectionGaps != nil ||
			output.Findings != nil ||
			output.Decisions != nil || output.Report != nil ||
			output.Lifecycle == nil {
			return fmt.Errorf("%s output discriminator is invalid", result.Stage)
		}
		if err := output.Lifecycle.validateForStage(result.Stage); err != nil {
			return err
		}
	case StageDetect:
		if output.CandidateFindings == nil || output.DetectionGaps == nil ||
			output.Findings != nil ||
			output.Decisions != nil || output.Report != nil ||
			output.Lifecycle != nil {
			return fmt.Errorf("detect output discriminator is invalid")
		}
	case StageNormalize, StageVerify:
		if output.CandidateFindings != nil || output.DetectionGaps != nil ||
			output.Findings == nil ||
			output.Decisions != nil || output.Report != nil ||
			output.Lifecycle != nil {
			return fmt.Errorf("%s output discriminator is invalid", result.Stage)
		}
	case StageAdjudicate:
		if output.CandidateFindings != nil || output.DetectionGaps != nil ||
			output.Findings == nil ||
			output.Decisions == nil || output.Report != nil ||
			output.Lifecycle != nil {
			return fmt.Errorf("adjudicate output discriminator is invalid")
		}
	case StageReport:
		if output.CandidateFindings != nil || output.DetectionGaps != nil ||
			output.Findings != nil ||
			output.Decisions != nil || output.Report == nil ||
			output.Lifecycle != nil {
			return fmt.Errorf("report output discriminator is invalid")
		}
	}
	if result.Stage == StageMaterializeTarget &&
		result.InputDigest != result.TargetDigest {
		return fmt.Errorf("materialize_target artifact must bind directly to its target digest")
	}
	for _, candidate := range output.CandidateFindings {
		if err := candidate.Validate(); err != nil {
			return err
		}
		if candidate.TargetDigest != result.TargetDigest {
			return fmt.Errorf("candidate target digest does not match stage artifact")
		}
	}
	for _, gap := range output.DetectionGaps {
		if err := gap.Validate(); err != nil {
			return err
		}
		if gap.TargetDigest != result.TargetDigest {
			return fmt.Errorf("detection gap target digest does not match stage artifact")
		}
	}
	for _, finding := range output.Findings {
		if err := finding.Validate(); err != nil {
			return err
		}
		if finding.TargetDigest != result.TargetDigest {
			return fmt.Errorf("finding target digest does not match stage artifact")
		}
		if result.Stage == StageNormalize && finding.Verification.Status != VerificationPending {
			return fmt.Errorf("normalize output must have pending verification")
		}
		if result.Stage != StageNormalize && finding.Verification.Status == VerificationPending {
			return fmt.Errorf("%s output must have completed verification", result.Stage)
		}
	}
	for _, decision := range output.Decisions {
		if err := decision.Validate(); err != nil {
			return err
		}
	}
	if result.Stage == StageAdjudicate {
		if _, err := summarize(output.Findings, output.Decisions); err != nil {
			return err
		}
	}
	if output.Report != nil {
		if output.Report.TargetDigest != result.TargetDigest {
			return fmt.Errorf("report target digest does not match stage artifact")
		}
		return output.Report.Validate()
	}
	return nil
}

func (fact LifecycleFact) validateForStage(stage StageName) error {
	if fact.ReasonCodes == nil || !slices.IsSorted(fact.ReasonCodes) {
		return fmt.Errorf("lifecycle reason_codes must be an explicit sorted array")
	}
	for index, reason := range fact.ReasonCodes {
		if err := validateTextID("lifecycle.reason_code", reason); err != nil {
			return err
		}
		if index > 0 && reason == fact.ReasonCodes[index-1] {
			return fmt.Errorf("lifecycle reason_codes contain duplicate %q", reason)
		}
	}
	var expectedKind string
	var allowedStates []LifecycleState
	switch stage {
	case StageMaterializeTarget:
		expectedKind = "target_materialized"
		allowedStates = []LifecycleState{LifecycleComplete}
	case StagePlanContext:
		expectedKind = "context_planned"
		allowedStates = []LifecycleState{LifecycleComplete, LifecyclePartial}
	case StagePublish:
		expectedKind = "publication_projection"
		allowedStates = []LifecycleState{LifecycleRemoteDisabled}
	case StageCaptureFeedback:
		expectedKind = "feedback_registration"
		allowedStates = []LifecycleState{LifecycleAwaitingFeedback}
	case StageExportEvaluation:
		expectedKind = "evaluation_export_registration"
		allowedStates = []LifecycleState{LifecycleCandidateOnly}
	default:
		return fmt.Errorf("stage %q cannot contain a lifecycle fact", stage)
	}
	if fact.Kind != expectedKind || !slices.Contains(allowedStates, fact.State) {
		return fmt.Errorf("lifecycle fact does not match stage %q", stage)
	}
	switch stage {
	case StageMaterializeTarget:
		if len(fact.ReasonCodes) != 0 || fact.RecordCount == 0 {
			return fmt.Errorf("materialize_target lifecycle fact is incomplete")
		}
	case StagePlanContext:
		if fact.State == LifecycleComplete && len(fact.ReasonCodes) != 0 ||
			fact.State == LifecyclePartial && len(fact.ReasonCodes) == 0 {
			return fmt.Errorf("plan_context completeness and reasons disagree")
		}
	case StagePublish:
		if !slices.Equal(fact.ReasonCodes, []string{"remote_writes_denied"}) {
			return fmt.Errorf("local publish stage must record remote_writes_denied")
		}
	case StageCaptureFeedback:
		if !slices.Equal(fact.ReasonCodes, []string{"external_feedback_not_recorded"}) {
			return fmt.Errorf("capture_feedback must remain an awaiting registration")
		}
	case StageExportEvaluation:
		if !slices.Equal(fact.ReasonCodes, []string{"governance_review_required"}) {
			return fmt.Errorf("export_evaluation must remain candidate-only")
		}
	}
	return nil
}

func DigestReviewInput(input ReviewInput) (string, error) {
	return digestReviewInputContext(context.Background(), input)
}

func digestReviewInputContext(ctx context.Context, input ReviewInput) (string, error) {
	if err := input.validateContext(ctx); err != nil {
		return "", err
	}
	if err := checkContext(ctx); err != nil {
		return "", err
	}
	data, err := json.Marshal(input)
	if err != nil {
		return "", fmt.Errorf("marshal review input: %w", err)
	}
	if err := checkContext(ctx); err != nil {
		return "", err
	}
	return digestBytes(data), nil
}

func digestStageResult(result StageResult) (string, error) {
	copy := result
	copy.ArtifactDigest = ""
	data, err := json.Marshal(copy)
	if err != nil {
		return "", fmt.Errorf("marshal stage artifact: %w", err)
	}
	return digestBytes(data), nil
}

func digestBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func digestString(value string) string {
	return digestBytes([]byte(value))
}

func stableID(parts ...string) string {
	return digestString(strings.Join(parts, "\x00"))
}

func validateDigest(name, value string) error {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return fmt.Errorf("%s must be a lowercase SHA-256 digest", name)
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return fmt.Errorf("%s must be a lowercase SHA-256 digest", name)
	}
	return nil
}

func validateTextID(name, value string) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 512 ||
		!utf8.ValidString(value) || containsControl(value) {
		return fmt.Errorf("%s must be a non-empty trimmed UTF-8 string without controls", name)
	}
	return nil
}

func validateRepositoryPath(name, value string) error {
	if value == "" || !utf8.ValidString(value) || containsControl(value) ||
		strings.ContainsAny(value, `\:`) || value != path.Clean(value) ||
		path.IsAbs(value) || value == "." || value == ".." || strings.HasPrefix(value, "../") {
		return fmt.Errorf("%s must be a clean repository-relative path", name)
	}
	return nil
}

func containsControl(value string) bool {
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return true
		}
	}
	return false
}
