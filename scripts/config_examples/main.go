// Command config_examples emits the normative ConfigRevision and ConfigBundle
// examples from the production default constructor and resolver. It writes
// only to stdout so repository updates remain explicit and reviewable.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"argus.local/argus/internal/configdefaults"
	"argus.local/argus/internal/formalreview"
	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/workflow"
)

func main() {
	contract := flag.String(
		"contract",
		"",
		"contract to emit: config-revision, config-bundle, config-revision-agent-review, config-bundle-agent-review, config-resolution-receipt, or config-resolution-receipt-replay-budget",
	)
	flag.Parse()

	revision, bundle, agentRevision, agentBundle, err := exampleValues()
	if err != nil {
		fail(err)
	}
	receipt, err := exampleResolutionReceipt(agentRevision, agentBundle)
	if err != nil {
		fail(err)
	}
	budgetReceipt, err := exampleBudgetReceipt(agentBundle, receipt)
	if err != nil {
		fail(err)
	}

	var value any
	switch *contract {
	case "config-revision":
		value = revision
	case "config-bundle":
		value = bundle
	case "config-revision-agent-review":
		value = agentRevision
	case "config-bundle-agent-review":
		value = agentBundle
	case "config-resolution-receipt":
		value = receipt
	case "config-resolution-receipt-replay-budget":
		value = budgetReceipt
	default:
		fail(fmt.Errorf(
			"unsupported -contract %q",
			*contract,
		))
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		fail(err)
	}
}

func exampleBudgetReceipt(
	bundle reviewconfig.ConfigBundle,
	receipt reviewconfig.ConfigResolutionReceipt,
) (reviewconfig.ConfigResolutionReceipt, error) {
	_, budgetReceipt, err := formalreview.BuildBudgetReplayConfig(bundle, receipt, 15_000)
	return budgetReceipt, err
}

func exampleResolutionReceipt(
	revision reviewconfig.Revision,
	bundle reviewconfig.ConfigBundle,
) (reviewconfig.ConfigResolutionReceipt, error) {
	revisionDigest, err := reviewconfig.DigestRevision(revision)
	if err != nil {
		return reviewconfig.ConfigResolutionReceipt{}, err
	}
	assignment := sha256.Sum256([]byte("argus-config-resolution-example-assignment"))
	return reviewconfig.NewConfigResolutionReceipt(
		bundle,
		[]reviewconfig.PublishedRevisionBinding{
			{
				Source: bundle.AppliedRevisions[0], RevisionSHA256: revisionDigest,
				PublishEventID: "publish-event-example-1", PublishSequence: 1,
				PublishedAt:      time.Date(2026, 8, 24, 1, 2, 3, 0, time.UTC),
				AssignmentSHA256: hex.EncodeToString(assignment[:]),
			},
		},
	)
}

func exampleValues() (
	reviewconfig.Revision,
	reviewconfig.ConfigBundle,
	reviewconfig.Revision,
	reviewconfig.ConfigBundle,
	error,
) {
	options := configdefaults.Options{
		ID:             "example-platform-default",
		Revision:       "1",
		MaxFiles:       200,
		MaxPatchBytes:  16 << 20,
		MaxInputBytes:  16 << 20,
		MaxOutputBytes: 16 << 20,
		MaxAttempts:    2,
		AllowedModes:   []string{"diff", "scope", "selection"},
		TargetInclude:  []string{"**"},
		TargetExclude:  []string{},
		Context: reviewconfig.ResolutionContext{
			TenantID:       "tenant-example",
			OrganizationID: "organization-example",
			RepositoryID:   "repository-example",
			Path:           "internal/reviewcore/pipeline.go",
			InvocationID:   "review-example-1",
		},
	}
	definition := workflow.DefaultReviewDefinition()
	revision, err := configdefaults.Revision(options, definition)
	if err != nil {
		return reviewconfig.Revision{}, reviewconfig.ConfigBundle{},
			reviewconfig.Revision{}, reviewconfig.ConfigBundle{}, err
	}
	bundle, err := reviewconfig.Resolve(
		options.Context,
		[]reviewconfig.Revision{revision},
	)
	if err != nil {
		return reviewconfig.Revision{}, reviewconfig.ConfigBundle{},
			reviewconfig.Revision{}, reviewconfig.ConfigBundle{}, err
	}
	agentRevision, err := configdefaults.Revision(options, definition)
	if err != nil {
		return reviewconfig.Revision{}, reviewconfig.ConfigBundle{},
			reviewconfig.Revision{}, reviewconfig.ConfigBundle{}, err
	}
	agentRevision.ID = "example-platform-agent-review"
	agentRevision.Patch.Execution.AllowedTools = &reviewconfig.SetPatch{
		Add: []string{"codegraph"}, Remove: []string{},
	}
	agentRevision.Patch.AgentReview = exampleAgentReviewPatch()
	agentBundle, err := reviewconfig.Resolve(
		options.Context,
		[]reviewconfig.Revision{agentRevision},
	)
	if err != nil {
		return reviewconfig.Revision{}, reviewconfig.ConfigBundle{},
			reviewconfig.Revision{}, reviewconfig.ConfigBundle{}, err
	}
	return revision, bundle, agentRevision, agentBundle, nil
}

func exampleAgentReviewPatch() *reviewconfig.AgentReviewPatch {
	const digest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	ref := func(id, revision string) *reviewconfig.VersionedRef {
		return &reviewconfig.VersionedRef{ID: id, Revision: revision, SHA256: digest}
	}
	modelEgress := reviewconfig.AgentModelEgressProviderBrokerOnly
	workspaceReads := reviewconfig.AgentWorkspaceReadsFrozenInputOnly
	deny := reviewconfig.PermissionDeny
	delegationDepth := 0
	maxFiles, maxGroups, maxHypotheses := 100, 16, 64
	maxModelCalls, maxToolCalls, maxConcurrency := 32, 64, 1
	maxTargetBytes, maxGroupBytes := int64(512<<10), int64(128<<10)
	maxOutputBytes, maxOutputTokens := int64(128<<10), int64(10_000)
	maxCostMicros, timeoutMS := int64(500_000), int64(20_000)
	return &reviewconfig.AgentReviewPatch{
		Agent:         ref("pi-agent", "1"),
		Normalization: ref("candidate-normalization", "v2"),
		Provider:      ref("anthropic-compatible", "1"),
		Model:         ref("deepseek-chat", "2026-08-01"),
		Prompt:        ref("review-prompt", "1"),
		ModelCredential: &reviewconfig.SecretRef{
			URI: "secret://env/deepseek-anthropic-key",
		},
		SkillPacks: &reviewconfig.OrderedAgentSkillPackPatch{
			Upsert: []reviewconfig.AgentSkillPackDefinition{
				{ID: "group-changes", Phase: reviewconfig.AgentSkillPhaseGrouping, Ref: *ref("skill-group-changes", "1")},
				{ID: "review-core", Phase: reviewconfig.AgentSkillPhaseReview, Ref: *ref("skill-review-core", "1")},
				{ID: "verify-candidates", Phase: reviewconfig.AgentSkillPhaseVerification, Ref: *ref("skill-verify-candidates", "1")},
			},
			Remove: []string{},
		},
		KnowledgePacks: &reviewconfig.OrderedAgentKnowledgePackPatch{
			Upsert: []reviewconfig.AgentKnowledgePackDefinition{
				{ID: "business-default", Ref: *ref("knowledge-business-default", "1")},
			},
			Remove: []string{},
		},
		APIProtocol: ref("anthropic-messages", "2023-06-01"),
		Authority: &reviewconfig.AgentAuthorityPatch{
			ModelEgress: &modelEgress, ToolNetwork: &deny,
			WorkspaceReads: &workspaceReads, WorkspaceWrites: &deny,
			RemoteWrites: &deny, MaxDelegationDepth: &delegationDepth,
			Tools: &reviewconfig.SetPatch{Add: []string{"codegraph"}, Remove: []string{}},
		},
		Budget: &reviewconfig.AgentBudgetPatch{
			MaxFiles: &maxFiles, MaxGroups: &maxGroups, MaxHypotheses: &maxHypotheses,
			MaxModelCalls: &maxModelCalls, MaxToolCalls: &maxToolCalls,
			MaxTargetBytes: &maxTargetBytes, MaxGroupBytes: &maxGroupBytes,
			MaxOutputBytes: &maxOutputBytes, MaxOutputTokens: &maxOutputTokens,
			MaxCostMicros: &maxCostMicros, TimeoutMS: &timeoutMS,
			MaxConcurrency: &maxConcurrency,
		},
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "config-examples:", err)
	os.Exit(1)
}
