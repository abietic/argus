package v1alpha1

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestAgentReviewShadowContractsStrictRoundTripAndBindings(t *testing.T) {
	plan := validAgentReviewPlan()
	set := validReviewHypothesisSet(t)
	collection := validAgentExecutionReceiptCollection()
	summary, status, err := SummarizeAgentReviewResult(set, collection.Receipts)
	if err != nil {
		t.Fatal(err)
	}
	manifest := validAgentReviewResultManifest(plan, set, summary, status)
	observation := validAgentReviewObservation(plan, manifest)

	tests := []struct {
		name   string
		value  any
		decode func([]byte) error
	}{
		{
			name:  "plan",
			value: plan,
			decode: func(data []byte) error {
				_, err := DecodeAgentReviewPlan(data)
				return err
			},
		},
		{
			name:  "hypothesis set",
			value: set,
			decode: func(data []byte) error {
				_, err := DecodeReviewHypothesisSet(data)
				return err
			},
		},
		{
			name:  "receipt",
			value: collection.Receipts[0],
			decode: func(data []byte) error {
				_, err := DecodeAgentExecutionReceipt(data)
				return err
			},
		},
		{
			name:  "receipt collection",
			value: collection,
			decode: func(data []byte) error {
				_, err := DecodeAgentExecutionReceiptCollection(data)
				return err
			},
		},
		{
			name:  "manifest",
			value: manifest,
			decode: func(data []byte) error {
				_, err := DecodeAgentReviewResultManifest(data)
				return err
			},
		},
		{
			name:  "observation",
			value: observation,
			decode: func(data []byte) error {
				_, err := DecodeAgentReviewObservation(data)
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data, err := json.Marshal(test.value)
			if err != nil {
				t.Fatal(err)
			}
			if err := test.decode(data); err != nil {
				t.Fatalf("strict decode error = %v", err)
			}
			withUnknown := append(data[:len(data)-1], []byte(`,"raw_prompt":"secret"}`)...)
			if err := test.decode(withUnknown); err == nil ||
				!strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("strict decoder accepted raw/unknown field: %v", err)
			}
		})
	}
	setData, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	withNull := strings.Replace(
		string(setData),
		`"verification_observations":`,
		`"suggestion":null,"verification_observations":`,
		1,
	)
	if _, err := DecodeReviewHypothesisSet([]byte(withNull)); err == nil ||
		!strings.Contains(err.Error(), "null") {
		t.Fatalf("strict decoder accepted explicit null: %v", err)
	}
	if err := ValidateAgentReviewResultManifestBindings(manifest, plan, set, collection); err != nil {
		t.Fatalf("ValidateAgentReviewResultManifestBindings() error = %v", err)
	}
	if err := ValidateAgentReviewObservationBinding(observation, manifest, plan); err != nil {
		t.Fatalf("ValidateAgentReviewObservationBinding() error = %v", err)
	}
}

func TestAgentReviewPlanFailsClosed(t *testing.T) {
	plan := validAgentReviewPlan()
	plan.ToolPolicy.ToolNetwork = "allow"
	if err := plan.Validate(); err == nil || !strings.Contains(err.Error(), "must be denied") {
		t.Fatalf("Validate() network error = %v", err)
	}

	plan = validAgentReviewPlan()
	plan.Attestation = "attested"
	if err := plan.Validate(); err == nil || !strings.Contains(err.Error(), "non_attested") {
		t.Fatalf("Validate() attestation error = %v", err)
	}

	plan = validAgentReviewPlan()
	plan.VerificationRequired = false
	if err := plan.Validate(); err == nil || !strings.Contains(err.Error(), "must be true") {
		t.Fatalf("Validate() verification error = %v", err)
	}

	plan = validAgentReviewPlan()
	plan.Budget.MaxTargetBytes = plan.Budget.MaxGroupBytes - 1
	if err := plan.Validate(); err == nil || !strings.Contains(err.Error(), "max_target_bytes") {
		t.Fatalf("Validate() target byte budget error = %v", err)
	}

	plan = validAgentReviewPlan()
	plan.ExecutionSnapshotRef.Contract = "argus.unknown_snapshot.v1alpha1"
	if err := plan.Validate(); err == nil || !strings.Contains(err.Error(), AgentReviewExecutionSnapshotContract) {
		t.Fatalf("Validate() execution snapshot contract error = %v", err)
	}

	plan = validAgentReviewPlan()
	plan.ToolPolicy.AllowedTools = []string{"submit_context"}
	if err := plan.Validate(); err == nil || !strings.Contains(err.Error(), "terminal submit tool") {
		t.Fatalf("Validate() terminal tool policy error = %v", err)
	}
}

func TestAgentReviewPlanMatchesConcretePiWorkerNumericLimits(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*AgentReviewPlan)
	}{
		{"max_files", func(plan *AgentReviewPlan) { plan.Budget.MaxFiles = agentReviewMaxFiles + 1 }},
		{"max_groups", func(plan *AgentReviewPlan) { plan.Budget.MaxGroups = agentReviewMaxGroups + 1 }},
		{"max_candidates", func(plan *AgentReviewPlan) { plan.Budget.MaxCandidates = agentReviewMaxCandidates + 1 }},
		{"max_model_calls", func(plan *AgentReviewPlan) { plan.Budget.MaxModelCalls = agentReviewMaxModelCalls + 1 }},
		{"max_tool_calls", func(plan *AgentReviewPlan) { plan.Budget.MaxToolCalls = agentReviewMaxToolCalls + 1 }},
		{"max_output_tokens", func(plan *AgentReviewPlan) { plan.Budget.MaxOutputTokens = agentReviewMaxOutputTokens + 1 }},
		{"max_group_bytes", func(plan *AgentReviewPlan) { plan.Budget.MaxGroupBytes = agentReviewMaxGroupBytes + 1 }},
		{"max_target_bytes", func(plan *AgentReviewPlan) { plan.Budget.MaxTargetBytes = agentReviewMaxTargetBytes + 1 }},
		{"timeout_ms", func(plan *AgentReviewPlan) { plan.Budget.TimeoutMS = agentReviewMaxTimeoutMS + 1 }},
		{"max_concurrency", func(plan *AgentReviewPlan) { plan.Budget.MaxConcurrency = agentReviewMaxConcurrency + 1 }},
		{"execution snapshot size", func(plan *AgentReviewPlan) {
			plan.ExecutionSnapshotRef.Ref.SizeBytes = agentReviewMaxArtifactBytes + 1
		}},
		{"review input size", func(plan *AgentReviewPlan) {
			plan.ReviewInputRef.Ref.SizeBytes = agentReviewMaxArtifactBytes + 1
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := validAgentReviewPlan()
			test.mutate(&plan)
			if err := plan.Validate(); err == nil ||
				!strings.Contains(err.Error(), "Pi worker maximum") {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}

	boundary := validAgentReviewPlan()
	boundary.Budget = AgentReviewBudget{
		MaxFiles:        agentReviewMaxFiles,
		MaxGroups:       agentReviewMaxGroups,
		MaxCandidates:   agentReviewMaxCandidates,
		MaxModelCalls:   agentReviewMaxModelCalls,
		MaxToolCalls:    agentReviewMaxToolCalls,
		MaxOutputTokens: agentReviewMaxOutputTokens,
		MaxGroupBytes:   agentReviewMaxGroupBytes,
		MaxTargetBytes:  agentReviewMaxTargetBytes,
		TimeoutMS:       agentReviewMaxTimeoutMS,
		MaxConcurrency:  agentReviewMaxConcurrency,
	}
	boundary.ExecutionSnapshotRef.Ref.SizeBytes = agentReviewMaxArtifactBytes
	boundary.ReviewInputRef.Ref.SizeBytes = agentReviewMaxArtifactBytes
	if err := boundary.Validate(); err != nil {
		t.Fatalf("Validate(maximum Pi worker limits) error = %v", err)
	}
}

func TestAgentReviewPlanMatchesConcretePiWorkerShape(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*AgentReviewPlan)
		error  string
	}{
		{
			name: "multiple context dimensions",
			mutate: func(plan *AgentReviewPlan) {
				plan.ContextDimensions = append(
					plan.ContextDimensions,
					testVersionedRef("type-context"),
				)
			},
			error: "exactly one",
		},
		{
			name: "too many review dimensions",
			mutate: func(plan *AgentReviewPlan) {
				plan.ReviewDimensions = make([]VersionedRef, agentReviewMaxReviewDimensions+1)
				for index := range plan.ReviewDimensions {
					plan.ReviewDimensions[index] = testVersionedRef(
						fmt.Sprintf("review-%02d", index),
					)
				}
			},
			error: "Pi worker maximum",
		},
		{
			name: "too many knowledge refs",
			mutate: func(plan *AgentReviewPlan) {
				plan.Knowledge = make([]VersionedRef, AgentReviewWorkerMaxKnowledgeCount+1)
				for index := range plan.Knowledge {
					plan.Knowledge[index] = testVersionedRef(fmt.Sprintf("knowledge-%d", index))
				}
			},
			error: "knowledge exceeds",
		},
		{
			name: "missing repository tool",
			mutate: func(plan *AgentReviewPlan) {
				plan.ToolPolicy.AllowedTools = []string{"list_files", "read_file"}
			},
			error: "requires tool_policy.allowed_tools exactly",
		},
		{
			name: "extra repository tool",
			mutate: func(plan *AgentReviewPlan) {
				plan.ToolPolicy.AllowedTools = []string{
					"list_files", "query_codegraph", "read_file", "search_code",
				}
			},
			error: "requires tool_policy.allowed_tools exactly",
		},
		{
			name: "reordered repository tools",
			mutate: func(plan *AgentReviewPlan) {
				plan.ToolPolicy.AllowedTools = []string{"read_file", "list_files", "search_code"}
			},
			error: "requires tool_policy.allowed_tools exactly",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := validAgentReviewPlan()
			test.mutate(&plan)
			if err := plan.Validate(); err == nil || !strings.Contains(err.Error(), test.error) {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}

	boundary := validAgentReviewPlan()
	boundary.ReviewDimensions = make([]VersionedRef, agentReviewMaxReviewDimensions)
	for index := range boundary.ReviewDimensions {
		boundary.ReviewDimensions[index] = testVersionedRef(fmt.Sprintf("review-%02d", index))
	}
	if err := boundary.Validate(); err != nil {
		t.Fatalf("Validate(maximum Pi worker review dimensions) error = %v", err)
	}
}

func TestReviewHypothesisSetRetainsExactEvidenceAndDedupLineage(t *testing.T) {
	set := validReviewHypothesisSet(t)
	set.Hypotheses[0].Evidence[0].Excerpt = "tampered"
	if err := set.Validate(); err == nil || !strings.Contains(err.Error(), "exact evidence") {
		t.Fatalf("Validate() evidence tamper error = %v", err)
	}

	set = validReviewHypothesisSet(t)
	set.Hypotheses[0].Evidence[0].Excerpt = "a b c d e f g"
	digest, err := DigestHypothesisEvidence(set.Hypotheses[0].Evidence[0])
	if err != nil {
		t.Fatal(err)
	}
	set.Hypotheses[0].Evidence[0].EvidenceDigest = digest
	if err := set.Validate(); err == nil || !strings.Contains(err.Error(), "at least 8 non-whitespace") {
		t.Fatalf("Validate() accepted weak exact evidence excerpt: %v", err)
	}

	set = validReviewHypothesisSet(t)
	set.DedupClusters[0].OccurrenceIDs = []string{"missing"}
	set.DedupClusters[0].CanonicalOccurrenceID = "missing"
	if err := set.Validate(); err == nil || !strings.Contains(err.Error(), "unknown occurrence") {
		t.Fatalf("Validate() missing lineage error = %v", err)
	}

	set = validReviewHypothesisSet(t)
	set.Hypotheses[0].Verification[0].EvidenceIDs = []string{}
	if err := set.Validate(); err == nil || !strings.Contains(err.Error(), "requires exact evidence") {
		t.Fatalf("Validate() confirmed without evidence error = %v", err)
	}

	set = validReviewHypothesisSet(t)
	set.NormalizationDecisions = []HypothesisNormalizationDecision{}
	if err := set.Validate(); err == nil || !strings.Contains(err.Error(), "normalization decision") {
		t.Fatalf("Validate() missing normalization lineage error = %v", err)
	}

	set = validReviewHypothesisSet(t)
	occurrenceID := "occurrence-1"
	set.NormalizationDecisions = append(
		set.NormalizationDecisions,
		HypothesisNormalizationDecision{
			RawCandidateID: "raw-candidate-2", ClaimDigest: testDigest,
			Action: HypothesisNormalizationRejectedInvalid, ReasonCode: "invalid_anchor",
			OccurrenceID: &occurrenceID,
		},
	)
	if err := set.Validate(); err == nil || !strings.Contains(err.Error(), "forbids occurrence") {
		t.Fatalf("Validate() invalid candidate lineage error = %v", err)
	}

	set = validReviewHypothesisSet(t)
	canonicalOccurrenceID := "occurrence-1"
	set.NormalizationDecisions = append(
		set.NormalizationDecisions,
		HypothesisNormalizationDecision{
			RawCandidateID: "raw-candidate-2", ClaimDigest: testDigest,
			Action:                HypothesisNormalizationMergedDuplicate,
			ReasonCode:            "same_claim",
			CanonicalOccurrenceID: &canonicalOccurrenceID,
		},
	)
	if err := set.Validate(); err != nil {
		t.Fatalf("Validate() rejected duplicate-to-canonical lineage: %v", err)
	}
	summary, _, err := SummarizeAgentReviewResult(set, []AgentExecutionReceipt{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.RawCandidates != 2 || summary.NormalizationRetained != 1 ||
		summary.MergedDuplicates != 1 || summary.HypothesisOccurrences != 1 {
		t.Fatalf("merged normalization summary = %+v", summary)
	}

	set.NormalizationDecisions[1].OccurrenceID = stringPointer("occurrence-2")
	if err := set.Validate(); err == nil || !strings.Contains(err.Error(), "canonical_occurrence_id only") {
		t.Fatalf("Validate() accepted a materialized duplicate occurrence: %v", err)
	}

	set = validReviewHypothesisSet(t)
	set.NormalizationDecisions = append(
		set.NormalizationDecisions,
		HypothesisNormalizationDecision{
			RawCandidateID: "raw-candidate-2", ClaimDigest: testDigest,
			Action: HypothesisNormalizationExcludedBudget, ReasonCode: "candidate_budget_exhausted",
		},
	)
	if err := set.Validate(); err != nil {
		t.Fatalf("Validate() rejected explicit budget exclusion: %v", err)
	}
	summary, _, err = SummarizeAgentReviewResult(set, []AgentExecutionReceipt{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.ExcludedBudget != 1 || summary.RawCandidates != 2 ||
		summary.HypothesisOccurrences != 1 {
		t.Fatalf("budget exclusion summary = %+v", summary)
	}

	set = validReviewHypothesisSet(t)
	duplicate := set.Hypotheses[0]
	duplicate.OccurrenceID = "occurrence-2"
	set.Hypotheses = append(set.Hypotheses, duplicate)
	set.NormalizationDecisions = append(
		set.NormalizationDecisions,
		HypothesisNormalizationDecision{
			RawCandidateID: "raw-candidate-2", ClaimDigest: testDigest,
			Action: HypothesisNormalizationRetained, ReasonCode: "valid_candidate",
			OccurrenceID: stringPointer("occurrence-2"),
		},
	)
	set.DedupClusters = append(
		set.DedupClusters,
		HypothesisDedupCluster{
			ClusterID: "cluster-2", Fingerprint: testDigest,
			CanonicalOccurrenceID: "occurrence-2", OccurrenceIDs: []string{"occurrence-2"},
		},
	)
	if err := set.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate canonical fingerprint") {
		t.Fatalf("Validate() accepted duplicate materialized hypotheses: %v", err)
	}
}

func TestAgentExecutionReceiptIsDiagnosticSelfReport(t *testing.T) {
	receipt := validAgentExecutionReceipt()
	receipt.ProvenanceClass = "platform_attested"
	if err := receipt.Validate(); err == nil || !strings.Contains(err.Error(), "worker_self_report") {
		t.Fatalf("Validate() provenance error = %v", err)
	}

	receipt = validAgentExecutionReceipt()
	receipt.Authority = "billing_authoritative"
	if err := receipt.Validate(); err == nil || !strings.Contains(err.Error(), "diagnostic_only") {
		t.Fatalf("Validate() authority error = %v", err)
	}

	receipt = validAgentExecutionReceipt()
	receipt.ToolCalls++
	if err := receipt.Validate(); err == nil || !strings.Contains(err.Error(), "sum") {
		t.Fatalf("Validate() tool count error = %v", err)
	}

	receipt = validAgentExecutionReceipt()
	receipt.Usage.Completeness = AgentTokenUsageUnavailable
	if err := receipt.Validate(); err == nil || !strings.Contains(err.Error(), "unavailable_reason_code") {
		t.Fatalf("Validate() unavailable usage error = %v", err)
	}

	receipt = validAgentExecutionReceipt()
	receipt.Usage.TotalTokens++
	if err := receipt.Validate(); err == nil || !strings.Contains(err.Error(), "total_tokens") {
		t.Fatalf("Validate() token total error = %v", err)
	}

	receipt = validAgentExecutionReceipt()
	receipt.ModelTurnsCompleted = receipt.ModelTurnsStarted + 1
	if err := receipt.Validate(); err == nil || !strings.Contains(err.Error(), "must not exceed") {
		t.Fatalf("Validate() provider turn lifecycle error = %v", err)
	}

	receipt = validAgentExecutionReceipt()
	receipt.ModelTurnsStarted++
	if err := receipt.Validate(); err == nil || !strings.Contains(err.Error(), "partial or unavailable") {
		t.Fatalf("Validate() incomplete lifecycle usage error = %v", err)
	}

	reason := "last_turn_usage_missing"
	receipt.Usage.Completeness = AgentTokenUsagePartial
	receipt.Usage.UnavailableReasonCode = &reason
	if err := receipt.Validate(); err != nil {
		t.Fatalf("Validate() rejected partial observed usage: %v", err)
	}

	receipt.Usage.UnavailableReasonCode = nil
	if err := receipt.Validate(); err == nil || !strings.Contains(err.Error(), "requires unavailable_reason_code") {
		t.Fatalf("Validate() accepted partial usage without reason: %v", err)
	}

	receipt.Usage = AgentTokenUsage{
		Completeness: AgentTokenUsagePartial, UnavailableReasonCode: &reason,
	}
	if err := receipt.Validate(); err == nil || !strings.Contains(err.Error(), "positive observed") {
		t.Fatalf("Validate() accepted partial usage without observed counters: %v", err)
	}
}

func TestAgentExecutionReceiptTerminalSubmitIsRoleBounded(t *testing.T) {
	receipt := validAgentExecutionReceipt()
	receipt.ToolUsage[1].ToolID = "submit_context"
	if err := receipt.Validate(); err == nil || !strings.Contains(err.Error(), "does not match task_role") {
		t.Fatalf("Validate() mismatched terminal error = %v", err)
	}

	receipt = validAgentExecutionReceipt()
	receipt.ToolUsage[1].InvocationCount = 2
	receipt.ToolCalls = 3
	if err := receipt.Validate(); err == nil || !strings.Contains(err.Error(), "successful terminal") {
		t.Fatalf("Validate() repeated successful terminal error = %v", err)
	}

	receipt = validAgentExecutionReceipt()
	receipt.ToolUsage[1].InvocationCount = 2
	receipt.ToolUsage[1].FailureCount = 1
	receipt.ToolCalls = 3
	if err := receipt.Validate(); err != nil {
		t.Fatalf("Validate() rejected one failed terminal attempt followed by one success: %v", err)
	}

	receipt = validAgentExecutionReceipt()
	receipt.ToolUsage = receipt.ToolUsage[:1]
	receipt.ToolCalls = 1
	if err := receipt.Validate(); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("Validate() missing terminal error = %v", err)
	}
}

func TestAgentExecutionReceiptCollectionClosesLineageAndOrdering(t *testing.T) {
	collection := singleAgentExecutionReceiptCollection()
	second := collection.Receipts[0]
	second.TaskID = "task-2"
	second.ReceiptID = "receipt-2"
	collection.Receipts = append(collection.Receipts, second)
	if err := collection.Validate(); err != nil {
		t.Fatalf("valid collection error = %v", err)
	}

	collection.Receipts[1].TaskID = "task-1"
	if err := collection.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate task_id") {
		t.Fatalf("duplicate task error = %v", err)
	}

	collection = singleAgentExecutionReceiptCollection()
	second = collection.Receipts[0]
	second.TaskID = "task-0"
	second.ReceiptID = "receipt-2"
	collection.Receipts = append(collection.Receipts, second)
	if err := collection.Validate(); err == nil || !strings.Contains(err.Error(), "sorted") {
		t.Fatalf("ordering error = %v", err)
	}

	collection = singleAgentExecutionReceiptCollection()
	second = collection.Receipts[0]
	second.TaskID = "task-2"
	collection.Receipts = append(collection.Receipts, second)
	if err := collection.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate receipt_id") {
		t.Fatalf("duplicate receipt error = %v", err)
	}

	collection = singleAgentExecutionReceiptCollection()
	collection.Receipts[0].ReviewRunID = "other-run"
	if err := collection.Validate(); err == nil || !strings.Contains(err.Error(), "collection lineage") {
		t.Fatalf("lineage error = %v", err)
	}
}

func TestAgentReviewUsageKeepsUnavailableDistinctFromZero(t *testing.T) {
	set := validReviewHypothesisSet(t)
	reported := validAgentExecutionReceipt()
	unavailable := validAgentExecutionReceipt()
	unavailable.ReceiptID = "receipt-3"
	unavailable.TaskID = "task-3"
	reason := "provider_usage_missing"
	unavailable.Usage = AgentTokenUsage{
		Completeness:          AgentTokenUsageUnavailable,
		UnavailableReasonCode: &reason,
	}
	summary, _, err := SummarizeAgentReviewResult(
		set,
		[]AgentExecutionReceipt{reported, unavailable},
	)
	if err != nil {
		t.Fatal(err)
	}
	if summary.UsageCompleteness != AgentTokenUsagePartial ||
		summary.UsageReportedReceipts != 1 || summary.UsagePartialReceipts != 0 ||
		summary.UsageUnavailableReceipts != 1 ||
		summary.TotalTokens != 150 || summary.ReasoningReportedReceipts != 1 {
		t.Fatalf("mixed usage summary = %+v", summary)
	}

	partial := validAgentExecutionReceipt()
	partial.ModelTurnsStarted = 2
	partial.ModelTurnsCompleted = 1
	reason = "last_turn_usage_missing"
	partial.Usage.Completeness = AgentTokenUsagePartial
	partial.Usage.UnavailableReasonCode = &reason
	summary, _, err = SummarizeAgentReviewResult(set, []AgentExecutionReceipt{partial})
	if err != nil {
		t.Fatal(err)
	}
	if summary.UsageCompleteness != AgentTokenUsagePartial ||
		summary.UsagePartialReceipts != 1 || summary.TotalTokens != 150 ||
		summary.ModelTurnsStarted != 2 || summary.ModelTurnsCompleted != 1 {
		t.Fatalf("partial turn usage summary = %+v", summary)
	}

	summary, _, err = SummarizeAgentReviewResult(set, []AgentExecutionReceipt{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.UsageCompleteness != AgentTokenUsageUnavailable || summary.TotalTokens != 0 {
		t.Fatalf("empty usage summary = %+v", summary)
	}
}

func TestAgentReviewManifestRequiresHostRecomputedSummary(t *testing.T) {
	plan := validAgentReviewPlan()
	set := validReviewHypothesisSet(t)
	collection := validAgentExecutionReceiptCollection()
	summary, status, err := SummarizeAgentReviewResult(set, collection.Receipts)
	if err != nil {
		t.Fatal(err)
	}
	manifest := validAgentReviewResultManifest(plan, set, summary, status)
	manifest.Summary.Confirmed--
	manifest.Summary.Unverified++
	if err := ValidateAgentReviewResultManifestBindings(manifest, plan, set, collection); err == nil ||
		!strings.Contains(err.Error(), "host recomputation") {
		t.Fatalf("binding accepted forged summary: %v", err)
	}

	manifest = validAgentReviewResultManifest(plan, set, summary, status)
	manifest.Disposition = "publishable"
	if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), "shadow_only") {
		t.Fatalf("Validate() disposition error = %v", err)
	}

	manifest = validAgentReviewResultManifest(plan, set, summary, status)
	manifest.ReviewInputRef.Contract = "argus.unknown_input.v1alpha1"
	if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), AgentReviewInputContract) {
		t.Fatalf("Validate() review input contract error = %v", err)
	}

	missing := "missing-occurrence"
	collection.Receipts[2].HypothesisOccurrenceID = &missing
	summary, status, err = SummarizeAgentReviewResult(set, collection.Receipts)
	if err != nil {
		t.Fatal(err)
	}
	manifest = validAgentReviewResultManifest(plan, set, summary, status)
	if err := ValidateAgentReviewResultManifestBindings(manifest, plan, set, collection); err == nil ||
		!strings.Contains(err.Error(), "exact hypothesis occurrence") {
		t.Fatalf("binding accepted unknown verification occurrence: %v", err)
	}
}

func TestAgentReviewBindingBudgetsOnlyFrozenRepositoryTools(t *testing.T) {
	plan := validAgentReviewPlan()
	plan.Budget.MaxToolCalls = 1
	set := validReviewHypothesisSet(t)
	collection := validAgentExecutionReceiptCollection()
	manifest := agentReviewManifestFor(t, plan, set, collection)
	if err := ValidateAgentReviewResultManifestBindings(manifest, plan, set, collection); err != nil {
		t.Fatalf("terminal submit was incorrectly charged to max_tool_calls: %v", err)
	}

	collection.Receipts[1].ToolUsage[0].InvocationCount = 2
	collection.Receipts[1].ToolCalls = 3
	manifest = agentReviewManifestFor(t, plan, set, collection)
	if err := ValidateAgentReviewResultManifestBindings(manifest, plan, set, collection); err == nil ||
		!strings.Contains(err.Error(), "repository-tool budget") {
		t.Fatalf("binding accepted repository calls above max_tool_calls: %v", err)
	}

	collection = validAgentExecutionReceiptCollection()
	collection.Receipts[1].ToolUsage[0].ToolID = "run_shell"
	manifest = agentReviewManifestFor(t, plan, set, collection)
	if err := ValidateAgentReviewResultManifestBindings(manifest, plan, set, collection); err == nil ||
		!strings.Contains(err.Error(), "outside the frozen plan") {
		t.Fatalf("binding accepted an unplanned tool: %v", err)
	}
}

func TestAgentReviewBindingRecomputesCoverageFromTaskReceipts(t *testing.T) {
	plan := validAgentReviewPlan()
	set := validReviewHypothesisSet(t)

	collection := validAgentExecutionReceiptCollection()
	collection.Receipts = append(
		collection.Receipts,
		validContextAgentExecutionReceipt(),
	)
	collection.Receipts[3].ReceiptID = "receipt-4"
	collection.Receipts[3].TaskID = "task-4"
	manifest := agentReviewManifestFor(t, plan, set, collection)
	if err := ValidateAgentReviewResultManifestBindings(manifest, plan, set, collection); err == nil ||
		!strings.Contains(err.Error(), "duplicates role/group/dimension/occurrence") {
		t.Fatalf("binding accepted duplicate logical task: %v", err)
	}

	collection = validAgentExecutionReceiptCollection()
	collection.Receipts = append(collection.Receipts[:1], collection.Receipts[2:]...)
	manifest = agentReviewManifestFor(t, plan, set, collection)
	if err := ValidateAgentReviewResultManifestBindings(manifest, plan, set, collection); err == nil ||
		!strings.Contains(err.Error(), "review receipt count") {
		t.Fatalf("binding accepted a missing review receipt: %v", err)
	}

	collection = validAgentExecutionReceiptCollection()
	collection.Receipts = collection.Receipts[1:]
	manifest = agentReviewManifestFor(t, plan, set, collection)
	if err := ValidateAgentReviewResultManifestBindings(manifest, plan, set, collection); err == nil ||
		!strings.Contains(err.Error(), "context receipt count") {
		t.Fatalf("binding accepted a missing context receipt: %v", err)
	}

	collection = validAgentExecutionReceiptCollection()
	collection.Receipts[1].GroupID = "group-2"
	manifest = agentReviewManifestFor(t, plan, set, collection)
	if err := ValidateAgentReviewResultManifestBindings(manifest, plan, set, collection); err == nil ||
		!strings.Contains(err.Error(), "outside the context receipt groups") {
		t.Fatalf("binding accepted a review-only group: %v", err)
	}

	collection = validAgentExecutionReceiptCollection()
	set = validReviewHypothesisSet(t)
	set.Completeness = AgentReviewPartial
	set.CompletenessReasons = []string{"review_partial"}
	set.Coverage.GroupsReviewed = 0
	set.Coverage.ReviewTasksSucceeded = 0
	manifest = agentReviewManifestFor(t, plan, set, collection)
	if err := ValidateAgentReviewResultManifestBindings(manifest, plan, set, collection); err == nil ||
		!strings.Contains(err.Error(), "review_tasks_succeeded") {
		t.Fatalf("binding accepted forged succeeded review coverage: %v", err)
	}

	set = validReviewHypothesisSet(t)
	collection = validAgentExecutionReceiptCollection()
	failureReason := "context_failed"
	collection.Receipts[0].Status = AgentTaskFailed
	collection.Receipts[0].FailureReasonCode = &failureReason
	collection.Receipts[0].OutputDigest = nil
	manifest = agentReviewManifestFor(t, plan, set, collection)
	if err := ValidateAgentReviewResultManifestBindings(manifest, plan, set, collection); err == nil ||
		!strings.Contains(err.Error(), "every context and review receipt to succeed") {
		t.Fatalf("binding accepted complete output with failed context: %v", err)
	}

	set = validReviewHypothesisSet(t)
	plan = validAgentReviewPlan()
	plan.Budget.MaxGroups = agentReviewMaxGroups
	plan.ReviewDimensions = []VersionedRef{
		testVersionedRef("correctness"),
		testVersionedRef("security"),
	}
	set.Coverage.GroupsTotal = ^uint32(0)
	set.Coverage.GroupsReviewed = ^uint32(0)
	set.Coverage.ReviewTasksTotal = ^uint32(0)
	set.Coverage.ReviewTasksSucceeded = ^uint32(0)
	collection = validAgentExecutionReceiptCollection()
	manifest = agentReviewManifestFor(t, plan, set, collection)
	if err := ValidateAgentReviewResultManifestBindings(manifest, plan, set, collection); err == nil ||
		!strings.Contains(err.Error(), "exceeds the frozen plan budget") {
		t.Fatalf("binding accepted an unrepresentable review task count: %v", err)
	}
}

func TestAgentReviewBindingClosesVerificationEvidence(t *testing.T) {
	plan := validAgentReviewPlan()
	set := validReviewHypothesisSet(t)

	collection := validAgentExecutionReceiptCollection()
	collection.Receipts = collection.Receipts[:2]
	manifest := agentReviewManifestFor(t, plan, set, collection)
	if err := ValidateAgentReviewResultManifestBindings(manifest, plan, set, collection); err == nil ||
		!strings.Contains(err.Error(), "require a succeeded receipt") {
		t.Fatalf("binding accepted an observation without verification receipt: %v", err)
	}

	collection = validAgentExecutionReceiptCollection()
	failureReason := "provider_failed"
	collection.Receipts[2].Status = AgentTaskFailed
	collection.Receipts[2].FailureReasonCode = &failureReason
	collection.Receipts[2].OutputDigest = nil
	manifest = agentReviewManifestFor(t, plan, set, collection)
	if err := ValidateAgentReviewResultManifestBindings(manifest, plan, set, collection); err == nil ||
		!strings.Contains(err.Error(), "require a succeeded receipt") {
		t.Fatalf("binding accepted observations from a failed verification task: %v", err)
	}

	set = validReviewHypothesisSet(t)
	set.Hypotheses[0].Verification = []HypothesisVerificationObservation{}
	set.Completeness = AgentReviewPartial
	set.CompletenessReasons = []string{"verification_incomplete"}
	set.Coverage.Gaps = []AgentReviewCoverageGap{
		{
			GapID: "gap-verification-1", Phase: AgentReviewCoverageVerification,
			SubjectID: "occurrence-1", ReasonCode: "verification_not_started",
		},
	}
	collection = validAgentExecutionReceiptCollection()
	collection.Receipts = collection.Receipts[:2]
	manifest = agentReviewManifestFor(t, plan, set, collection)
	if err := ValidateAgentReviewResultManifestBindings(manifest, plan, set, collection); err != nil {
		t.Fatalf("binding rejected explicit partial verification gap: %v", err)
	}

	set.Coverage.Gaps = []AgentReviewCoverageGap{}
	manifest = agentReviewManifestFor(t, plan, set, collection)
	if err := ValidateAgentReviewResultManifestBindings(manifest, plan, set, collection); err == nil ||
		!strings.Contains(err.Error(), "verification coverage gap") {
		t.Fatalf("binding accepted silently unverified partial output: %v", err)
	}

	set = validReviewHypothesisSet(t)
	set.Hypotheses[0].Verification = []HypothesisVerificationObservation{}
	collection = validAgentExecutionReceiptCollection()
	manifest = agentReviewManifestFor(t, plan, set, collection)
	if err := ValidateAgentReviewResultManifestBindings(manifest, plan, set, collection); err == nil ||
		!strings.Contains(err.Error(), "requires one succeeded verification receipt and an observation") {
		t.Fatalf("binding accepted complete but unverified output: %v", err)
	}
}

func TestAgentReviewBindingBudgetsCanonicalHypothesesNotRawDuplicates(t *testing.T) {
	plan := validAgentReviewPlan()
	plan.Budget.MaxCandidates = 1
	collection := validAgentExecutionReceiptCollection()
	canonicalOccurrenceID := "occurrence-1"

	set := validReviewHypothesisSet(t)
	set.NormalizationDecisions = append(
		set.NormalizationDecisions,
		HypothesisNormalizationDecision{
			RawCandidateID: "raw-candidate-2", ClaimDigest: testDigest,
			Action:                HypothesisNormalizationMergedDuplicate,
			ReasonCode:            "same_claim",
			CanonicalOccurrenceID: &canonicalOccurrenceID,
		},
	)
	manifest := agentReviewManifestFor(t, plan, set, collection)
	if err := ValidateAgentReviewResultManifestBindings(manifest, plan, set, collection); err != nil {
		t.Fatalf("binding charged a merged raw duplicate to verifier budget: %v", err)
	}

	set = validReviewHypothesisSet(t)
	set.NormalizationDecisions = append(
		set.NormalizationDecisions,
		HypothesisNormalizationDecision{
			RawCandidateID: "raw-candidate-2", ClaimDigest: testDigest,
			Action: HypothesisNormalizationExcludedBudget, ReasonCode: "candidate_budget_exhausted",
		},
	)
	manifest = agentReviewManifestFor(t, plan, set, collection)
	if err := ValidateAgentReviewResultManifestBindings(manifest, plan, set, collection); err != nil {
		t.Fatalf("binding rejected explicit over-budget raw provenance: %v", err)
	}
	roomyPlan := validAgentReviewPlan()
	roomyPlan.Budget.MaxCandidates = 2
	manifest = agentReviewManifestFor(t, roomyPlan, set, collection)
	if err := ValidateAgentReviewResultManifestBindings(manifest, roomyPlan, set, collection); err == nil ||
		!strings.Contains(err.Error(), "budget to be exhausted") {
		t.Fatalf("binding accepted unjustified budget exclusion: %v", err)
	}

	set = validReviewHypothesisSet(t)
	alternateDigest := strings.Repeat("b", 64)
	second := set.Hypotheses[0]
	second.OccurrenceID = "occurrence-2"
	second.ClusterFingerprint = alternateDigest
	second.Verification[0].ObservationID = "verification-2"
	set.Hypotheses = append(set.Hypotheses, second)
	set.NormalizationDecisions = append(
		set.NormalizationDecisions,
		HypothesisNormalizationDecision{
			RawCandidateID: "raw-candidate-2", ClaimDigest: alternateDigest,
			Action: HypothesisNormalizationRetained, ReasonCode: "valid_candidate",
			OccurrenceID: stringPointer("occurrence-2"),
		},
	)
	set.DedupClusters = append(
		set.DedupClusters,
		HypothesisDedupCluster{
			ClusterID: "cluster-2", Fingerprint: alternateDigest,
			CanonicalOccurrenceID: "occurrence-2", OccurrenceIDs: []string{"occurrence-2"},
		},
	)
	manifest = agentReviewManifestFor(t, plan, set, collection)
	if err := ValidateAgentReviewResultManifestBindings(manifest, plan, set, collection); err == nil ||
		!strings.Contains(err.Error(), "exceed the frozen candidate budget") {
		t.Fatalf("binding accepted retained hypotheses above max_candidates: %v", err)
	}
}

func TestAgentReviewBindingRejectsUnfrozenDimensions(t *testing.T) {
	plan := validAgentReviewPlan()
	set := validReviewHypothesisSet(t)
	collection := validAgentExecutionReceiptCollection()

	set.Hypotheses[0].Dimension = testVersionedRef("security")
	manifest := agentReviewManifestFor(t, plan, set, collection)
	if err := ValidateAgentReviewResultManifestBindings(manifest, plan, set, collection); err == nil ||
		!strings.Contains(err.Error(), "review dimension is outside the plan") {
		t.Fatalf("binding accepted an unfrozen hypothesis dimension: %v", err)
	}

	set = validReviewHypothesisSet(t)
	set.Hypotheses[0].Verification[0].Verifier = testVersionedRef("other-verifier")
	manifest = agentReviewManifestFor(t, plan, set, collection)
	if err := ValidateAgentReviewResultManifestBindings(manifest, plan, set, collection); err == nil ||
		!strings.Contains(err.Error(), "verifier does not match the plan") {
		t.Fatalf("binding accepted an unfrozen verifier: %v", err)
	}
}

func TestAgentReviewObservationAppendOnlySuccessor(t *testing.T) {
	plan := validAgentReviewPlan()
	set := validReviewHypothesisSet(t)
	collection := validAgentExecutionReceiptCollection()
	summary, status, err := SummarizeAgentReviewResult(set, collection.Receipts)
	if err != nil {
		t.Fatal(err)
	}
	manifest := validAgentReviewResultManifest(plan, set, summary, status)
	previous := validAgentReviewObservation(plan, manifest)
	next := previous
	next.ObservationID = "observation-2"
	next.Sequence = 2
	next.RecordedAt = next.RecordedAt.Add(time.Second)
	if err := ValidateAgentReviewObservationAppend(previous, next); err != nil {
		t.Fatalf("valid append error = %v", err)
	}
	next.Sequence = 3
	if err := ValidateAgentReviewObservationAppend(previous, next); err == nil {
		t.Fatal("append validator accepted a sequence gap")
	}
}

func validAgentReviewPlan() AgentReviewPlan {
	return AgentReviewPlan{
		SchemaVersion: AgentReviewPlanSchemaVersion,
		PlanID:        "plan-1", SourceRunID: "source-run-1",
		ExecutionID: "execution-1", ReviewRunID: "review-run-1",
		ExecutionSnapshotRef: testArtifactBinding(
			"artifact://local/execution-snapshot", "argus.execution_snapshot.v1alpha1",
		),
		ReviewInputRef: testArtifactBinding(
			"artifact://local/review-input", "argus.review_input.v1alpha1",
		),
		TargetDigest:   testDigest,
		RulePack:       testVersionedRef("review-rules"),
		Implementation: testVersionedRef("pi-review"),
		Normalization: VersionedRef{
			ID: CandidateNormalizationImplementationID, Revision: CandidateNormalizationCurrentRevision,
			SHA256: testDigest,
		},
		Grouping: testVersionedRef("path-grouping"),
		ContextDimensions: []VersionedRef{
			testVersionedRef("call-context"),
		},
		ReviewDimensions: []VersionedRef{
			testVersionedRef("correctness"),
		},
		Verifier:    testVersionedRef("independent-verifier"),
		Knowledge:   []VersionedRef{},
		Runtime:     testVersionedRef("node-runtime"),
		Profile:     testVersionedRef("local-shadow"),
		Agent:       testVersionedRef("pi-agent"),
		Provider:    testVersionedRef("deepseek-anthropic-env"),
		Model:       testVersionedRef("deepseek-v4-pro-1m"),
		APIProtocol: AgentAPIProtocolAnthropicMessages,
		ToolPolicy: AgentReviewToolPolicy{
			AllowedTools: []string{"list_files", "read_file", "search_code"},
			ToolNetwork:  "deny", WorkspaceWrites: "deny", RemoteWrites: "deny",
		},
		Budget: AgentReviewBudget{
			MaxFiles: 20, MaxGroups: 10, MaxCandidates: 20, MaxModelCalls: 50,
			MaxToolCalls: 50, MaxOutputTokens: 4096, MaxGroupBytes: 100000,
			MaxTargetBytes: 1000000,
			TimeoutMS:      60000, MaxConcurrency: 4,
		},
		VerificationRequired: true,
		ExecutionClass:       AgentReviewExecutionLocalDirectProviderShadow,
		Attestation:          AgentReviewAttestationNonAttested,
		SideEffects:          "deny",
		CreatedAt:            time.Date(2026, time.August, 20, 10, 0, 0, 0, time.UTC),
	}
}

func validReviewHypothesisSet(t *testing.T) ReviewHypothesisSet {
	t.Helper()
	anchor := HypothesisSourceAnchor{
		Path: "internal/review.go", Side: HypothesisAnchorNew,
		StartLine: 10, EndLine: 11, SourceDigest: testDigest,
	}
	evidence := HypothesisEvidence{
		EvidenceID: "evidence-1", Statement: "nil can reach this dereference",
		Anchor: anchor, Excerpt: "value.Field()",
	}
	digest, err := DigestHypothesisEvidence(evidence)
	if err != nil {
		t.Fatal(err)
	}
	evidence.EvidenceDigest = digest
	return ReviewHypothesisSet{
		SchemaVersion:   ReviewHypothesisSetSchemaVersion,
		HypothesisSetID: "hypothesis-set-1", PlanID: "plan-1",
		SourceRunID: "source-run-1", ExecutionID: "execution-1",
		ReviewRunID: "review-run-1", TargetDigest: testDigest,
		Completeness: AgentReviewComplete, CompletenessReasons: []string{},
		NormalizationDecisions: []HypothesisNormalizationDecision{
			{
				RawCandidateID: "raw-candidate-1", ClaimDigest: testDigest,
				Action: HypothesisNormalizationRetained, ReasonCode: "valid_candidate",
				OccurrenceID: stringPointer("occurrence-1"),
			},
		},
		Hypotheses: []ReviewHypothesis{
			{
				OccurrenceID: "occurrence-1", ClusterFingerprint: testDigest,
				GroupID: "group-1", Dimension: testVersionedRef("correctness"),
				Category: "correctness", Severity: HypothesisSeverityHigh,
				Title: "possible nil dereference", Description: "value may be nil",
				Impact: "request panic", Anchor: anchor,
				Evidence: []HypothesisEvidence{evidence},
				Verification: []HypothesisVerificationObservation{
					{
						ObservationID: "verification-1", Sequence: 1,
						Verifier: testVersionedRef("independent-verifier"),
						Verdict:  HypothesisVerificationConfirmed, ReasonCode: "path_reachable",
						Explanation: "the nil branch reaches the dereference",
						EvidenceIDs: []string{"evidence-1"},
					},
				},
			},
		},
		DedupClusters: []HypothesisDedupCluster{
			{
				ClusterID: "cluster-1", Fingerprint: testDigest,
				CanonicalOccurrenceID: "occurrence-1",
				OccurrenceIDs:         []string{"occurrence-1"},
			},
		},
		Coverage: AgentReviewCoverage{
			GroupsTotal: 1, GroupsReviewed: 1, ReviewTasksTotal: 1,
			ReviewTasksSucceeded: 1, FilesIncluded: 1, Gaps: []AgentReviewCoverageGap{},
		},
		GeneratedAt: time.Date(2026, time.August, 20, 10, 1, 0, 0, time.UTC),
	}
}

func validAgentExecutionReceipt() AgentExecutionReceipt {
	reasoningTokens := uint64(10)
	return AgentExecutionReceipt{
		SchemaVersion: AgentExecutionReceiptSchemaVersion,
		ReceiptID:     "receipt-2", PlanID: "plan-1", SourceRunID: "source-run-1",
		ExecutionID: "execution-1", ReviewRunID: "review-run-1",
		TaskID: "task-2", GroupID: "group-1", TaskRole: AgentTaskReview,
		Dimension: testVersionedRef("correctness"), Runtime: testVersionedRef("node-runtime"),
		Profile: testVersionedRef("local-shadow"), Agent: testVersionedRef("pi-agent"),
		Provider:          testVersionedRef("deepseek-anthropic-env"),
		Model:             testVersionedRef("deepseek-v4-pro-1m"),
		APIProtocol:       AgentAPIProtocolAnthropicMessages,
		ProvenanceClass:   AgentReceiptProvenanceWorkerSelfReport,
		Authority:         AgentReceiptAuthorityDiagnosticOnly,
		Status:            AgentTaskSucceeded,
		PromptDigest:      stringPointer(testDigest),
		OutputDigest:      stringPointer(testDigest),
		StartedAt:         time.Date(2026, time.August, 20, 10, 0, 0, 0, time.UTC),
		FinishedAt:        time.Date(2026, time.August, 20, 10, 0, 1, 0, time.UTC),
		ModelTurnsStarted: 1, ModelTurnsCompleted: 1, ToolCalls: 2,
		ToolUsage: []AgentToolUsage{
			{ToolID: "read_file", InvocationCount: 1, FailureCount: 0},
			{ToolID: "submit_candidates", InvocationCount: 1, FailureCount: 0},
		},
		Usage: AgentTokenUsage{
			Completeness: AgentTokenUsageProviderReported,
			InputTokens:  100, OutputTokens: 50, ReasoningTokens: &reasoningTokens,
			TotalTokens: 150,
		},
	}
}

func validContextAgentExecutionReceipt() AgentExecutionReceipt {
	receipt := validAgentExecutionReceipt()
	receipt.ReceiptID = "receipt-1"
	receipt.TaskID = "task-1"
	receipt.TaskRole = AgentTaskContext
	receipt.Dimension = testVersionedRef("call-context")
	receipt.ToolUsage[1].ToolID = "submit_context"
	return receipt
}

func validVerificationAgentExecutionReceipt() AgentExecutionReceipt {
	receipt := validAgentExecutionReceipt()
	receipt.ReceiptID = "receipt-3"
	receipt.TaskID = "task-3"
	receipt.TaskRole = AgentTaskVerification
	receipt.Dimension = testVersionedRef("independent-verifier")
	receipt.HypothesisOccurrenceID = stringPointer("occurrence-1")
	receipt.ToolUsage[1].ToolID = "submit_verdict"
	return receipt
}

func validAgentExecutionReceiptCollection() AgentExecutionReceiptCollection {
	return AgentExecutionReceiptCollection{
		SchemaVersion: AgentExecutionReceiptCollectionSchemaVersion,
		PlanID:        "plan-1", SourceRunID: "source-run-1",
		ExecutionID: "execution-1", ReviewRunID: "review-run-1",
		Receipts: []AgentExecutionReceipt{
			validContextAgentExecutionReceipt(),
			validAgentExecutionReceipt(),
			validVerificationAgentExecutionReceipt(),
		},
	}
}

func singleAgentExecutionReceiptCollection() AgentExecutionReceiptCollection {
	collection := validAgentExecutionReceiptCollection()
	collection.Receipts = collection.Receipts[:1]
	return collection
}

func validAgentReviewResultManifest(
	plan AgentReviewPlan,
	set ReviewHypothesisSet,
	summary AgentReviewResultSummary,
	status AgentReviewRunStatus,
) AgentReviewResultManifest {
	return AgentReviewResultManifest{
		SchemaVersion: AgentReviewResultManifestSchemaVersion,
		ManifestID:    "manifest-1", PlanID: plan.PlanID,
		HypothesisSetID: set.HypothesisSetID, SourceRunID: plan.SourceRunID,
		ExecutionID: plan.ExecutionID, ReviewRunID: plan.ReviewRunID,
		TargetDigest:         plan.TargetDigest,
		ExecutionSnapshotRef: plan.ExecutionSnapshotRef,
		ReviewInputRef:       plan.ReviewInputRef,
		AgentReviewPlanRef: testArtifactBinding(
			"artifact://local/agent-review-plan", AgentReviewPlanSchemaVersion,
		),
		HypothesisSetRef: testArtifactBinding(
			"artifact://local/hypothesis-set", ReviewHypothesisSetSchemaVersion,
		),
		RawCandidateCollectionRef: testArtifactBinding(
			"artifact://local/raw-candidates", AgentReviewRawCandidateCollectionSchemaVersion,
		),
		AgentTaskEvidenceRef: testArtifactBinding(
			"artifact://local/task-evidence", AgentReviewTaskEvidenceCollectionSchemaVersion,
		),
		AgentExecutionReceiptRef: testArtifactBinding(
			"artifact://local/agent-receipts", AgentExecutionReceiptCollectionContract,
		),
		ProducerClass: "argus_go_host", Disposition: "shadow_only",
		Status: status, Summary: summary,
		RecordedAt: time.Date(2026, time.August, 20, 10, 2, 0, 0, time.UTC),
	}
}

func agentReviewManifestFor(
	t *testing.T,
	plan AgentReviewPlan,
	set ReviewHypothesisSet,
	collection AgentExecutionReceiptCollection,
) AgentReviewResultManifest {
	t.Helper()
	summary, status, err := SummarizeAgentReviewResult(set, collection.Receipts)
	if err != nil {
		t.Fatal(err)
	}
	return validAgentReviewResultManifest(plan, set, summary, status)
}

func validAgentReviewObservation(
	plan AgentReviewPlan,
	manifest AgentReviewResultManifest,
) AgentReviewObservation {
	return AgentReviewObservation{
		SchemaVersion: AgentReviewObservationSchemaVersion,
		ObservationID: "observation-1", ManifestID: manifest.ManifestID,
		SourceRunID: manifest.SourceRunID, ExecutionID: manifest.ExecutionID,
		ReviewRunID: manifest.ReviewRunID, Sequence: 1,
		Kind:          AgentReviewObservationShadowResultRecorded,
		ProducerClass: "argus_go_host", Disposition: "shadow_only", Status: manifest.Status,
		Agent: plan.Agent, Provider: plan.Provider, Model: plan.Model,
		Summary: manifest.Summary, DurationMS: 120000, ReasonCodes: []string{},
		RecordedAt: time.Date(2026, time.August, 20, 10, 3, 0, 0, time.UTC),
	}
}

func testArtifactBinding(uri, contract string) ArtifactBinding {
	return ArtifactBinding{
		Ref:      ContentRef{URI: uri, SHA256: testDigest, SizeBytes: 10},
		Contract: contract,
	}
}

func testVersionedRef(id string) VersionedRef {
	return VersionedRef{ID: id, Revision: "1", SHA256: testDigest}
}

func stringPointer(value string) *string {
	return &value
}
