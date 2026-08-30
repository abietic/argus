package agentplan

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/abietic/argus/internal/configdefaults"
	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/reviewcore"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/source/gitadapter"
	"github.com/abietic/argus/internal/targetmodel"
	"github.com/abietic/argus/internal/workflow"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

func TestCompileProducesCanonicalPlanWithoutSensitiveOrFindingState(t *testing.T) {
	input := newCompileFixture(t, fixtureOptions{})
	first, err := Compile(input)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	secondInput := input
	secondInput.Components.Skills = slices.Clone(input.Components.Skills)
	secondInput.Components.Knowledge = slices.Clone(input.Components.Knowledge)
	secondInput.Components.ContextProviders = slices.Clone(input.Components.ContextProviders)
	second, err := Compile(secondInput)
	if err != nil {
		t.Fatalf("Compile(reordered closure) error = %v", err)
	}
	firstJSON := mustJSON(t, first)
	secondJSON := mustJSON(t, second)
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("same frozen closure was not byte-identical\nfirst:  %s\nsecond: %s", firstJSON, secondJSON)
	}
	if err := first.Validate(); err != nil {
		t.Fatalf("compiled plan Validate() error = %v", err)
	}
	if first.ConfigResolutionReceiptRef != input.ConfigResolutionReceiptProjection.Governed ||
		first.OutputContract != contractsv1alpha1.ReviewHypothesisSetSchemaVersion ||
		first.Disposition != contractsv1alpha1.AgentStageDispositionHypothesisOnly {
		t.Fatalf("compiled plan lost S1 closure/output semantics: %+v", first)
	}
	if first.Normalization.ID != input.ConfigBundle.AgentReview.Normalization.ID ||
		first.Normalization.Revision != input.ConfigBundle.AgentReview.Normalization.Revision ||
		first.Normalization.SHA256 != input.ConfigBundle.AgentReview.Normalization.SHA256 {
		t.Fatalf("compiled plan lost exact normalization selector: %+v", first.Normalization)
	}
	for name, binding := range map[string]contractsv1alpha1.ArtifactBinding{
		"execution snapshot": first.ExecutionSnapshot,
		"config bundle":      first.ConfigBundle,
		"config receipt":     first.ConfigResolutionReceiptRef,
		"workflow":           first.Workflow,
		"review spec":        first.ReviewSpec,
		"review input":       first.ReviewInput,
	} {
		if strings.HasPrefix(binding.Ref.URI, "artifact://local/") {
			t.Fatalf("compiled plan %s retained local-only URI %q", name, binding.Ref.URI)
		}
	}
	wire := strings.ToLower(string(firstJSON))
	for _, forbidden := range []string{
		"secret://", "deepseek-anthropic-key", "candidate_id", "finding", "raw_prompt", "endpoint",
	} {
		if strings.Contains(wire, forbidden) {
			t.Fatalf("compiled plan leaked forbidden semantic %q: %s", forbidden, firstJSON)
		}
	}
	if input.Components.Skills[0].ID != "group-changes" {
		t.Fatal("Compile mutated caller-owned component order")
	}
	reordered := input
	reordered.Components.Skills = slices.Clone(input.Components.Skills)
	slices.Reverse(reordered.Components.Skills)
	if _, err := Compile(reordered); err == nil || !strings.Contains(err.Error(), "policy order") {
		t.Fatalf("Compile(reordered skills) error = %v, want policy order rejection", err)
	}
}

func TestCompileBehaviorIdentityTracksEffectiveInputsButNotLineageURI(t *testing.T) {
	baseInput := newCompileFixture(t, fixtureOptions{})
	base, err := Compile(baseInput)
	if err != nil {
		t.Fatal(err)
	}
	behaviorVariants := []struct {
		name   string
		mutate func(*reviewconfig.Revision)
	}{
		{
			name: "prompt",
			mutate: func(revision *reviewconfig.Revision) {
				revision.Patch.AgentReview.Prompt = configRef(
					"review-prompt",
					"2",
					"prompt-v2",
				)
			},
		},
		{
			name: "normalization",
			mutate: func(revision *reviewconfig.Revision) {
				revision.Patch.AgentReview.Normalization = configRef(
					"candidate-normalization",
					"v1",
					"pi-agent",
				)
			},
		},
		{
			name: "model",
			mutate: func(revision *reviewconfig.Revision) {
				revision.Patch.AgentReview.Model = configRef(
					"deepseek-chat",
					"2",
					"model-v2",
				)
			},
		},
		{
			name: "runtime",
			mutate: func(revision *reviewconfig.Revision) {
				revision.Patch.Execution.AgentProfile = configRef(
					"formal-agent-runtime",
					"2",
					"formal-agent-runtime-v2",
				)
			},
		},
		{
			name: "skill",
			mutate: func(revision *reviewconfig.Revision) {
				revision.Patch.AgentReview.SkillPacks.Upsert[0].Ref = reviewconfig.VersionedRef{
					ID: "skill-group-changes", Revision: "2", SHA256: digestLabel("skill-group-v2"),
				}
			},
		},
		{
			name: "budget",
			mutate: func(revision *reviewconfig.Revision) {
				value := *revision.Patch.AgentReview.Budget.MaxModelCalls - 1
				revision.Patch.AgentReview.Budget.MaxModelCalls = &value
			},
		},
	}
	for _, variant := range behaviorVariants {
		t.Run(variant.name, func(t *testing.T) {
			changedInput := newCompileFixture(t, fixtureOptions{mutateRevision: variant.mutate})
			changed, err := Compile(changedInput)
			if err != nil {
				t.Fatalf("Compile(%s) error = %v", variant.name, err)
			}
			if changed.BehaviorSHA256 == base.BehaviorSHA256 {
				t.Fatalf("%s change did not alter behavior identity", variant.name)
			}
		})
	}
	buildInput := baseInput
	buildInput.ExecutionSnapshot.BuildIdentity = "argus-test-build-v2"
	buildInput.ExecutionSnapshotRef = jsonArtifact(
		t,
		"execution-snapshot-build-v2",
		runmodel.ContractExecutionSnapshot,
		buildInput.ExecutionSnapshot,
	)
	buildInput.ExecutionSnapshotProjection = governedProjection(buildInput.ExecutionSnapshotRef)
	buildChanged, err := Compile(buildInput)
	if err != nil {
		t.Fatalf("Compile(build identity) error = %v", err)
	}
	if buildChanged.BehaviorSHA256 == base.BehaviorSHA256 {
		t.Fatal("build identity change did not alter behavior identity")
	}

	lineageInput := baseInput
	lineageBindings := slices.Clone(baseInput.ConfigResolutionReceipt.Revisions)
	lineageBindings[0].PublishedAt = lineageBindings[0].PublishedAt.Add(time.Second)
	lineageReceipt, err := reviewconfig.NewConfigResolutionReceipt(
		baseInput.ConfigBundle,
		lineageBindings,
	)
	if err != nil {
		t.Fatalf("NewConfigResolutionReceipt(lineage-only) error = %v", err)
	}
	lineageInput.ConfigResolutionReceipt = lineageReceipt
	lineageInput.ConfigResolutionReceiptRef = jsonArtifact(
		t,
		"lineage-config-resolution-receipt",
		runmodel.ContractConfigResolutionReceipt,
		lineageReceipt,
	)
	lineageInput.ConfigResolutionReceiptProjection = governedProjection(
		lineageInput.ConfigResolutionReceiptRef,
	)
	lineage, err := Compile(lineageInput)
	if err != nil {
		t.Fatalf("Compile(lineage-only) error = %v", err)
	}
	if lineage.BehaviorSHA256 != base.BehaviorSHA256 {
		t.Fatal("artifact lineage URI changed behavior identity")
	}
	if lineage.SHA256 == base.SHA256 || lineage.PlanID == base.PlanID {
		t.Fatal("artifact lineage URI did not change full plan identity")
	}
}

func TestCompileFailsClosedOnMissingOrMismatchedClosure(t *testing.T) {
	tests := []struct {
		name    string
		options fixtureOptions
		mutate  func(*CompileInput)
		want    string
	}{
		{
			name: "static bundle without published receipt",
			mutate: func(input *CompileInput) {
				input.ConfigResolutionReceipt = reviewconfig.ConfigResolutionReceipt{}
			},
			want: "published config resolution receipt",
		},
		{
			name: "receipt artifact mismatch",
			mutate: func(input *CompileInput) {
				input.ConfigResolutionReceiptRef.SHA256 = digestLabel("other-receipt")
				input.ConfigResolutionReceiptRef.URI = "artifact://local/sha256/" +
					input.ConfigResolutionReceiptRef.SHA256
			},
			want: "receipt artifact ref does not bind",
		},
		{
			name: "review input artifact mismatch",
			mutate: func(input *CompileInput) {
				input.ReviewInput.TargetID = "other-target"
			},
			want: "review input artifact ref does not bind",
		},
		{
			name: "materialized target artifact mismatch",
			mutate: func(input *CompileInput) {
				input.ExecutionSnapshot.TargetSnapshotRef = contentAddressedRef(
					runmodel.ContractMaterializedTarget,
					digestLabel("other-materialized-target"),
					128,
				)
				input.ExecutionSnapshotRef = jsonArtifact(
					t,
					"execution-snapshot-other-target",
					runmodel.ContractExecutionSnapshot,
					input.ExecutionSnapshot,
				)
			},
			want: "materialized target artifact ref does not bind",
		},
		{
			name: "prompt component mismatch",
			mutate: func(input *CompileInput) {
				input.Components.Prompt.Ref.Revision = "2"
			},
			want: "prompt component does not match",
		},
		{
			name: "component closure exceeds input budget",
			mutate: func(input *CompileInput) {
				input.Components.Prompt.Artifact.Ref.SizeBytes = 2 << 20
			},
			want: "component/input closure exceeds",
		},
		{
			name: "snapshot authority drift",
			mutate: func(input *CompileInput) {
				input.ExecutionSnapshot.ToolPolicy.AllowedTools = []string{}
				input.ExecutionSnapshotRef = jsonArtifact(
					t,
					"execution-snapshot-drifted",
					runmodel.ContractExecutionSnapshot,
					input.ExecutionSnapshot,
				)
				input.ExecutionSnapshotProjection = governedProjection(
					input.ExecutionSnapshotRef,
				)
			},
			want: "not exactly projected",
		},
		{
			name: "workflow call ceiling",
			options: fixtureOptions{mutateWorkflow: func(definition *workflow.Definition) {
				definition.Stages[0].AuthorityCeiling.MaxModelCalls = 7
			}},
			want: "call budget exceeds",
		},
		{
			name: "non exact replay",
			options: fixtureOptions{mutateWorkflow: func(definition *workflow.Definition) {
				definition.Stages[0].ReplayPolicy = workflow.ReplayPolicyCheckpoint
			}},
			want: "must use exact replay",
		},
		{
			name: "unbound upstream dependency",
			options: fixtureOptions{mutateWorkflow: func(definition *workflow.Definition) {
				agent := definition.Stages[0]
				agent.DependsOn = []string{"prepare-agent-input"}
				definition.Stages = []workflow.Stage{fixturePreparationStage(), agent}
			}},
			want: "must not depend on unbound upstream",
		},
		{
			name: "unbound replay input",
			mutate: func(input *CompileInput) {
				input.ExecutionSnapshot.ReplayInputRefs = []runmodel.ArtifactRef{
					contentAddressedRef(
						runmodel.ContractStageResult,
						digestLabel("replay-input"),
						128,
					),
				}
				input.ExecutionSnapshotRef = jsonArtifact(
					t,
					"execution-snapshot-replay",
					runmodel.ContractExecutionSnapshot,
					input.ExecutionSnapshot,
				)
				input.ExecutionSnapshotProjection = governedProjection(
					input.ExecutionSnapshotRef,
				)
			},
			want: "replay requires a formal replay closure",
		},
		{
			name: "target path denied by frozen config",
			options: fixtureOptions{mutateRevision: func(revision *reviewconfig.Revision) {
				revision.Patch.Target.Exclude = &reviewconfig.SetPatch{
					Add: []string{"pkg/**"}, Remove: []string{},
				}
			}},
			want: "denied by frozen target policy",
		},
		{
			name: "frozen input exceeds target budget",
			options: fixtureOptions{mutateRevision: func(revision *reviewconfig.Revision) {
				maxTargetBytes := int64(256)
				maxGroupBytes := int64(128)
				revision.Patch.AgentReview.Budget.MaxTargetBytes = &maxTargetBytes
				revision.Patch.AgentReview.Budget.MaxGroupBytes = &maxGroupBytes
			}},
			want: "target byte budget",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := newCompileFixture(t, test.options)
			if test.mutate != nil {
				test.mutate(&input)
			}
			if _, err := Compile(input); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Compile() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestAuthorityBudgetCountsAllMaterializedTargetFiles(t *testing.T) {
	input := newCompileFixture(t, fixtureOptions{})
	policy := *input.ConfigBundle.AgentReview
	policy.Budget.MaxFiles = 1
	target := input.Target
	target.FileRefs = append(
		slices.Clone(target.FileRefs),
		targetmodel.TargetFileRef{Path: "pkg/deleted.go"},
	)
	err := validateAuthorityAndBudget(
		policy,
		input.ConfigBundle,
		input.Workflow.Stages[0],
		input.ExecutionSnapshot,
		target,
		input.ReviewInput,
		input.Components,
	)
	if err == nil || !strings.Contains(err.Error(), "file count") {
		t.Fatalf("validateAuthorityAndBudget() error = %v, want materialized file count rejection", err)
	}
}

func TestCompileLowersWorkflowStageBudgetIntoSealedPlan(t *testing.T) {
	input := newCompileFixture(t, fixtureOptions{mutateWorkflow: func(definition *workflow.Definition) {
		definition.Stages[0].Budget.TimeoutMS = 5_000
		definition.Stages[0].Budget.MaxOutputBytes = 32 << 10
		definition.Stages[0].Budget.MaxConcurrency = 1
	}})
	plan, err := Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Budget.TimeoutMS != 5_000 || plan.Budget.MaxOutputBytes != 32<<10 ||
		plan.Budget.MaxConcurrency != 1 {
		t.Fatalf("effective workflow budget = %+v", plan.Budget)
	}
	if plan.Budget.MaxFiles != input.ConfigBundle.AgentReview.Budget.MaxFiles ||
		plan.Budget.MaxModelCalls != input.ConfigBundle.AgentReview.Budget.MaxModelCalls {
		t.Fatalf("workflow replay changed non-stage budget fields: %+v", plan.Budget)
	}
}

func TestCompileLowersEffectiveRetryPolicyIntoSealedPlan(t *testing.T) {
	input := newCompileFixture(t, fixtureOptions{
		mutateWorkflow: func(definition *workflow.Definition) {
			definition.Stages[0].Retry = workflow.RetryPolicy{
				MaxAttempts: 3, BackoffMS: 25, Jitter: false,
				RetryableCodes: []string{"deadline_exceeded", "provider_error"},
				UnknownOutcome: workflow.UnknownOutcomeReconcile,
			}
		},
		mutateRevision: func(revision *reviewconfig.Revision) {
			maxAttempts := 2
			revision.Patch.Budget.MaxAttempts = &maxAttempts
		},
	})
	plan, err := Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Retry.MaxAttempts != 2 || plan.Retry.BackoffMS != 25 ||
		plan.Retry.UnknownOutcome != "reconcile" ||
		!slices.Equal(plan.Retry.RetryableCodes, []string{"deadline_exceeded", "provider_error"}) {
		t.Fatalf("effective retry policy = %+v", plan.Retry)
	}
}

type fixtureOptions struct {
	mutateWorkflow func(*workflow.Definition)
	mutateRevision func(*reviewconfig.Revision)
}

func newCompileFixture(t *testing.T, options fixtureOptions) CompileInput {
	t.Helper()
	definition := formalAgentWorkflow()
	if options.mutateWorkflow != nil {
		options.mutateWorkflow(&definition)
	}
	if err := definition.Validate(); err != nil {
		t.Fatalf("fixture workflow Validate() error = %v", err)
	}
	resolutionContext := reviewconfig.ResolutionContext{
		TenantID:       "tenant-1",
		OrganizationID: "org-1",
		RepositoryID:   "repo-1",
		Path:           "pkg/p.go",
		InvocationID:   "run-1",
	}
	revision, err := configdefaults.Revision(configdefaults.Options{
		ID:             "platform-default",
		Revision:       "1",
		MaxFiles:       20,
		MaxPatchBytes:  1 << 20,
		MaxInputBytes:  1 << 20,
		MaxOutputBytes: 1 << 20,
		MaxAttempts:    1,
	}, definition)
	if err != nil {
		t.Fatalf("configdefaults.Revision() error = %v", err)
	}
	revision.Patch.Execution.AllowedTools = &reviewconfig.SetPatch{
		Add: []string{"codegraph"}, Remove: []string{},
	}
	revision.Patch.Execution.ContextProviders = &reviewconfig.OrderedContextProviderPatch{
		Upsert: []reviewconfig.ContextProviderDefinition{
			{
				ID: "repo-codegraph", Revision: "1", Kind: "codegraph",
				Adapter: reviewconfig.VersionedRef{
					ID: "codegraph-adapter", Revision: "1", SHA256: digestLabel("codegraph-adapter"),
				},
			},
		},
		Remove: []string{},
	}
	revision.Patch.AgentReview = completeAgentPolicyPatch()
	if options.mutateRevision != nil {
		options.mutateRevision(&revision)
	}
	if err := revision.Validate(); err != nil {
		t.Fatalf("fixture revision Validate() error = %v", err)
	}
	bundle, err := reviewconfig.Resolve(resolutionContext, []reviewconfig.Revision{revision})
	if err != nil {
		t.Fatalf("reviewconfig.Resolve() error = %v", err)
	}

	bindings := make([]reviewconfig.PublishedRevisionBinding, len(bundle.AppliedRevisions))
	for index, source := range bundle.AppliedRevisions {
		bindings[index] = reviewconfig.PublishedRevisionBinding{
			Source:           source,
			RevisionSHA256:   digestLabel("revision-" + source.ID + "-" + source.Revision),
			PublishEventID:   "publish-event-1",
			PublishSequence:  uint64(index + 1),
			PublishedAt:      time.Date(2026, 8, 24, 1, 2, 3+index, 0, time.UTC),
			AssignmentSHA256: digestLabel("assignment-1"),
		}
	}
	receipt, err := reviewconfig.NewConfigResolutionReceipt(bundle, bindings)
	if err != nil {
		t.Fatalf("NewConfigResolutionReceipt() error = %v", err)
	}

	content := "package p\n\nfunc F() {}\n"
	contentDigest := sha256Hex([]byte(content))
	selectedContent := "package p\n"
	target := selectionTargetFixture(t, "repo-1", content, selectedContent)
	reviewInput := reviewcore.ReviewInput{
		SchemaVersion:  reviewcore.ReviewInputSchemaVersion,
		TargetID:       target.Snapshot.TargetSnapshotID,
		TargetMode:     reviewcore.TargetModeSelection,
		CanonicalPatch: "",
		Regions: []reviewcore.ReviewRegion{
			{Path: "pkg/p.go", StartLine: 1, EndLine: 1, SHA256: contentDigest},
		},
		Files: []reviewcore.FileManifestEntry{
			{
				Path: "pkg/p.go", SHA256: contentDigest, SizeBytes: int64(len(content)), Content: &content,
			},
		},
		Contexts: []reviewcore.ContextBinding{},
	}
	if err := reviewInput.Validate(); err != nil {
		t.Fatalf("fixture ReviewInput Validate() error = %v", err)
	}
	workflowDigest, err := workflow.DigestDefinition(definition)
	if err != nil {
		t.Fatal(err)
	}
	workflowRef := jsonArtifact(
		t,
		"workflow",
		runmodel.ContractWorkflowDefinition,
		definition,
	)
	configRef := jsonArtifact(t, "config-bundle", runmodel.ContractConfigBundle, bundle)
	inputRef := jsonArtifact(t, "review-input", runmodel.ContractReviewInput, reviewInput)
	reviewSpec := contractsv1alpha1.ReviewSpec{
		SchemaVersion:  contractsv1alpha1.ReviewSpecSchemaVersion,
		RequestID:      "run-1",
		IdempotencyKey: "run-1",
		TenantID:       "tenant-1",
		WorkspaceID:    "workspace-1",
		Repository: contractsv1alpha1.RepositoryRef{
			Provider: "local-git", RepositoryID: "repo-1",
		},
		Target: contractsv1alpha1.ReviewTarget{
			Mode: contractsv1alpha1.ReviewModeSelection,
			Selection: &contractsv1alpha1.SelectionTarget{
				Revision: target.Snapshot.Head.CommitOID,
				Path:     "pkg/p.go", StartLine: 1, EndLine: 1,
				Content: contractsv1alpha1.ContentRef{
					URI:       target.SelectionContentRef.URI,
					SHA256:    target.SelectionContentRef.SHA256,
					SizeBytes: target.SelectionContentRef.SizeBytes,
				},
			},
		},
		ConfigBundleRef: contractsv1alpha1.VersionedRef{
			ID: bundle.BundleID, Revision: bundle.SHA256[:16], SHA256: configRef.SHA256,
		},
		WorkflowRef: contractsv1alpha1.VersionedRef{
			ID: definition.ID, Revision: definition.Revision, SHA256: workflowDigest,
		},
		RequestedOutput: []string{"findings", "report", "trace"},
		RemoteWrites:    "deny",
	}
	if err := reviewSpec.Validate(); err != nil {
		t.Fatalf("fixture ReviewSpec Validate() error = %v", err)
	}
	specRef := jsonArtifact(t, "review-spec", runmodel.ContractReviewSpec, reviewSpec)
	snapshot := runmodel.ExecutionSnapshot{
		SchemaVersion:         runmodel.SnapshotSchemaVersion,
		ExecutionSnapshotID:   "snapshot-1",
		ReviewSpecSHA256:      specRef.SHA256,
		ReviewSpecRef:         specRef,
		TargetSnapshotRef:     jsonArtifact(t, "materialized-target", runmodel.ContractMaterializedTarget, target),
		ReviewInputRef:        inputRef,
		ReplayInputRefs:       []runmodel.ArtifactRef{},
		WorkflowDefinitionRef: workflowRef,
		ConfigBundleRef:       configRef,
		Workflow: runmodel.WorkflowRef{
			ID: definition.ID, Revision: definition.Revision, SHA256: workflowDigest,
		},
		Config: runmodel.PolicyRef{
			ID: bundle.BundleID, Revision: bundle.SHA256[:16], SHA256: bundle.SHA256,
		},
		RuntimeProfile: bundle.Execution.AgentProfile.ID + "@" + bundle.Execution.AgentProfile.Revision,
		BuildIdentity:  "argus-test-build",
		ToolPolicy: runmodel.ToolInvocationPolicy{
			AllowedTools:       slices.Clone(bundle.Execution.AllowedTools),
			Network:            "deny",
			WorkspaceWrites:    "deny",
			RemoteWrites:       "deny",
			PerCallTimeoutMS:   bundle.Budget.StageTimeoutMS,
			MaxOutputBytes:     bundle.Budget.MaxOutputBytes,
			MaxConcurrency:     bundle.Budget.MaxConcurrency,
			MaxDelegationDepth: 0,
		},
		RemoteWrites: "deny",
		CreatedAt:    time.Date(2026, 8, 24, 2, 3, 4, 0, time.UTC),
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("fixture ExecutionSnapshot Validate() error = %v", err)
	}
	input := CompileInput{
		ReviewRunID: "run-1",
		StageID:     "agent_hypothesize",

		ExecutionSnapshot:    snapshot,
		ExecutionSnapshotRef: jsonArtifact(t, "execution-snapshot", runmodel.ContractExecutionSnapshot, snapshot),

		ConfigBundle:               bundle,
		ConfigResolutionReceipt:    receipt,
		ConfigResolutionReceiptRef: jsonArtifact(t, "config-resolution-receipt", runmodel.ContractConfigResolutionReceipt, receipt),

		Workflow:    definition,
		ReviewSpec:  reviewSpec,
		Target:      target,
		ReviewInput: reviewInput,
		Components:  resolvedComponents(*bundle.AgentReview, bundle.Execution),
	}
	input.ExecutionSnapshotProjection = governedProjection(input.ExecutionSnapshotRef)
	input.ConfigBundleProjection = governedProjection(input.ExecutionSnapshot.ConfigBundleRef)
	input.ConfigResolutionReceiptProjection = governedProjection(
		input.ConfigResolutionReceiptRef,
	)
	input.WorkflowProjection = governedProjection(input.ExecutionSnapshot.WorkflowDefinitionRef)
	input.ReviewSpecProjection = governedProjection(input.ExecutionSnapshot.ReviewSpecRef)
	input.ReviewInputProjection = governedProjection(input.ExecutionSnapshot.ReviewInputRef)
	return input
}

func selectionTargetFixture(
	t *testing.T,
	repositoryID string,
	fileContent string,
	selectedContent string,
) targetmodel.MaterializedTarget {
	t.Helper()
	fileDigest := sha256Hex([]byte(fileContent))
	selectionDigest := sha256Hex([]byte(selectedContent))
	manifestDigest := digestLabel("selection-manifest")
	commitOID := strings.Repeat("a", 40)
	revision := gitadapter.RevisionSnapshot{Requested: "HEAD", CommitOID: commitOID}
	snapshot, err := targetmodel.SealTargetSnapshot(targetmodel.TargetSnapshot{
		SchemaVersion: targetmodel.TargetSnapshotSchemaVersion,
		Mode:          reviewcore.TargetModeSelection,
		Repository: gitadapter.RepositorySnapshot{
			Kind: "local_git", RepositoryID: repositoryID, ObjectFormat: "sha1",
		},
		Base:           revision,
		Head:           revision,
		ManifestSHA256: manifestDigest,
		Selection: &targetmodel.SelectionSnapshot{
			Path:                   "pkg/p.go",
			StartLine:              1,
			EndLine:                1,
			EffectiveRanges:        []targetmodel.SelectionRange{{StartLine: 1, EndLine: 1}},
			SourceKind:             targetmodel.SelectionSourceCommit,
			FileSHA256:             fileDigest,
			FileSizeBytes:          int64(len(fileContent)),
			SelectionContentSHA256: selectionDigest,
			SelectionSizeBytes:     int64(len(selectedContent)),
		},
		DirtyState:         gitadapter.DirtyStateClean,
		Completeness:       gitadapter.CompletenessComplete,
		CompletenessReason: []gitadapter.Reason{},
		CapturedAt:         time.Date(2026, 8, 24, 0, 1, 2, 0, time.UTC),
		CapturedBy:         "argus-test",
		GitVersion:         "git version test",
	})
	if err != nil {
		t.Fatalf("SealTargetSnapshot() error = %v", err)
	}
	fileRef := contentAddressedRef(
		targetmodel.ContractFileContent,
		fileDigest,
		int64(len(fileContent)),
	)
	selectionRef := contentAddressedRef(
		targetmodel.ContractSelectionContent,
		selectionDigest,
		int64(len(selectedContent)),
	)
	target := targetmodel.MaterializedTarget{
		SchemaVersion: targetmodel.MaterializedTargetSchemaVersion,
		Snapshot:      snapshot,
		ManifestRef: contentAddressedRef(
			targetmodel.ContractSelectionManifest,
			manifestDigest,
			128,
		),
		SelectionContentRef: &selectionRef,
		FileRefs: []targetmodel.TargetFileRef{
			{
				Path:         "pkg/p.go",
				SHA256:       fileDigest,
				SizeBytes:    int64(len(fileContent)),
				ContentRef:   &fileRef,
				Completeness: gitadapter.CompletenessComplete,
				Reasons:      []gitadapter.Reason{},
			},
		},
		Contexts: []reviewcore.ContextBinding{},
	}
	if err := target.Validate(); err != nil {
		t.Fatalf("fixture materialized target Validate() error = %v", err)
	}
	return target
}

func formalAgentWorkflow() workflow.Definition {
	return workflow.Definition{
		SchemaVersion: workflow.DefinitionSchemaVersion,
		ID:            "formal-agent-review",
		Revision:      "1",
		Stages: []workflow.Stage{
			{
				ID:                     "agent_hypothesize",
				Kind:                   AgentHypothesizeStageKind,
				ImplementationRevision: "1",
				InputContract:          contractsv1alpha1.AgentStagePlanReviewInputContract,
				OutputContract:         contractsv1alpha1.AgentStagePlanOutputContract,
				DependsOn:              []string{},
				Executor:               "formal-agent-runtime",
				RequiredCapabilities:   []string{"brokered-model", "frozen-review-input"},
				AuthorityCeiling: &workflow.StageAuthorityCeiling{
					ModelEgress:        "provider_broker_only",
					AllowedTools:       []string{"codegraph"},
					ToolNetwork:        "deny",
					WorkspaceReads:     "frozen_input_only",
					WorkspaceWrites:    "deny",
					RemoteWrites:       "deny",
					MaxDelegationDepth: 0,
					MaxModelCalls:      16,
					MaxToolCalls:       16,
				},
				Budget: workflow.StageBudget{
					TimeoutMS:      30_000,
					MaxInputBytes:  1 << 20,
					MaxOutputBytes: 1 << 20,
					MaxConcurrency: 1,
				},
				Retry: workflow.RetryPolicy{
					MaxAttempts: 1, BackoffMS: 0, Jitter: false,
					RetryableCodes: []string{}, UnknownOutcome: workflow.UnknownOutcomeFail,
				},
				FailurePolicy: workflow.FailurePolicyFailRun,
				SideEffect:    workflow.SideEffectNone,
				ReplayPolicy:  workflow.ReplayPolicyExact,
			},
		},
	}
}

func fixturePreparationStage() workflow.Stage {
	return workflow.Stage{
		ID:                     "prepare-agent-input",
		Kind:                   "prepare_agent_input",
		ImplementationRevision: "1",
		InputContract:          contractsv1alpha1.AgentStagePlanReviewInputContract,
		OutputContract:         "argus.prepared_agent_input.v1alpha1",
		DependsOn:              []string{},
		Executor:               "deterministic-local",
		RequiredCapabilities:   []string{},
		Budget: workflow.StageBudget{
			TimeoutMS: 1_000, MaxInputBytes: 1 << 20,
			MaxOutputBytes: 1 << 20, MaxConcurrency: 1,
		},
		Retry: workflow.RetryPolicy{
			MaxAttempts: 1, BackoffMS: 0, Jitter: false,
			RetryableCodes: []string{}, UnknownOutcome: workflow.UnknownOutcomeFail,
		},
		FailurePolicy: workflow.FailurePolicyFailRun,
		SideEffect:    workflow.SideEffectNone,
		ReplayPolicy:  workflow.ReplayPolicyExact,
	}
}

func completeAgentPolicyPatch() *reviewconfig.AgentReviewPatch {
	modelEgress := reviewconfig.AgentModelEgressProviderBrokerOnly
	workspaceReads := reviewconfig.AgentWorkspaceReadsFrozenInputOnly
	delegation := 0
	maxFiles := 10
	maxGroups := 4
	maxHypotheses := 16
	maxModelCalls := 8
	maxToolCalls := 8
	maxTargetBytes := int64(64 << 10)
	maxGroupBytes := int64(16 << 10)
	maxOutputBytes := int64(64 << 10)
	maxOutputTokens := int64(4096)
	maxCostMicros := int64(500_000)
	timeoutMS := int64(10_000)
	maxConcurrency := 1
	return &reviewconfig.AgentReviewPatch{
		Agent:           configRef("pi-agent", "1", "pi-agent"),
		Normalization:   configRef("candidate-normalization", "v2", "pi-agent"),
		Provider:        configRef("anthropic-compatible", "1", "anthropic-compatible"),
		Model:           configRef("deepseek-chat", "1", "deepseek-chat"),
		Prompt:          configRef("review-prompt", "1", "review-prompt"),
		ModelCredential: &reviewconfig.SecretRef{URI: "secret://env/deepseek-anthropic-key"},
		SkillPacks: &reviewconfig.OrderedAgentSkillPackPatch{
			Upsert: []reviewconfig.AgentSkillPackDefinition{
				{
					ID: "group-changes", Phase: reviewconfig.AgentSkillPhaseGrouping,
					Ref: *configRef("skill-group-changes", "1", "skill-group-changes"),
				},
				{
					ID: "review-core", Phase: reviewconfig.AgentSkillPhaseReview,
					Ref: *configRef("skill-review-core", "1", "skill-review-core"),
				},
				{
					ID: "verify-hypotheses", Phase: reviewconfig.AgentSkillPhaseVerification,
					Ref: *configRef("skill-verify-hypotheses", "1", "skill-verify-hypotheses"),
				},
			},
			Remove: []string{},
		},
		KnowledgePacks: &reviewconfig.OrderedAgentKnowledgePackPatch{
			Upsert: []reviewconfig.AgentKnowledgePackDefinition{
				{
					ID:  "repository-standards",
					Ref: *configRef("knowledge-repository-standards", "1", "repository-standards"),
				},
			},
			Remove: []string{},
		},
		APIProtocol: configRef("anthropic-messages", "2023-06-01", "anthropic-messages"),
		Authority: &reviewconfig.AgentAuthorityPatch{
			ModelEgress:        &modelEgress,
			ToolNetwork:        permission(reviewconfig.PermissionDeny),
			WorkspaceReads:     &workspaceReads,
			WorkspaceWrites:    permission(reviewconfig.PermissionDeny),
			RemoteWrites:       permission(reviewconfig.PermissionDeny),
			MaxDelegationDepth: &delegation,
			Tools:              &reviewconfig.SetPatch{Add: []string{"codegraph"}, Remove: []string{}},
		},
		Budget: &reviewconfig.AgentBudgetPatch{
			MaxFiles:        &maxFiles,
			MaxGroups:       &maxGroups,
			MaxHypotheses:   &maxHypotheses,
			MaxModelCalls:   &maxModelCalls,
			MaxToolCalls:    &maxToolCalls,
			MaxTargetBytes:  &maxTargetBytes,
			MaxGroupBytes:   &maxGroupBytes,
			MaxOutputBytes:  &maxOutputBytes,
			MaxOutputTokens: &maxOutputTokens,
			MaxCostMicros:   &maxCostMicros,
			TimeoutMS:       &timeoutMS,
			MaxConcurrency:  &maxConcurrency,
		},
	}
}

func resolvedComponents(
	policy reviewconfig.AgentReviewPolicy,
	execution reviewconfig.ExecutionPolicy,
) ResolvedComponents {
	providers := execution.ContextProviders
	components := ResolvedComponents{
		Agent:       componentBinding(policy.Agent, contractsv1alpha1.AgentStagePlanAgentContract),
		Provider:    componentBinding(policy.Provider, contractsv1alpha1.AgentStagePlanProviderContract),
		Model:       componentBinding(policy.Model, contractsv1alpha1.AgentStagePlanModelContract),
		Runtime:     componentBinding(execution.AgentProfile, contractsv1alpha1.AgentStagePlanRuntimeContract),
		Prompt:      componentBinding(policy.Prompt, contractsv1alpha1.AgentStagePlanPromptContract),
		APIProtocol: componentBinding(policy.APIProtocol, contractsv1alpha1.AgentStagePlanAPIProtocolContract),
		Skills:      make([]contractsv1alpha1.AgentStageSkillBinding, len(policy.SkillPacks)),
		Knowledge:   make([]contractsv1alpha1.AgentStageKnowledgeBinding, len(policy.KnowledgePacks)),
		ContextProviders: make(
			[]contractsv1alpha1.AgentStageContextProviderBinding,
			len(providers),
		),
	}
	for index, pack := range policy.SkillPacks {
		binding := componentBinding(pack.Ref, contractsv1alpha1.AgentStagePlanSkillContract)
		components.Skills[index] = contractsv1alpha1.AgentStageSkillBinding{
			ID: pack.ID, Phase: contractsv1alpha1.AgentStageSkillPhase(pack.Phase),
			Ref: binding.Ref, Artifact: binding.Artifact,
		}
	}
	for index, pack := range policy.KnowledgePacks {
		binding := componentBinding(pack.Ref, contractsv1alpha1.AgentStagePlanKnowledgeContract)
		components.Knowledge[index] = contractsv1alpha1.AgentStageKnowledgeBinding{
			ID: pack.ID, Ref: binding.Ref, Artifact: binding.Artifact,
		}
	}
	for index, provider := range providers {
		components.ContextProviders[index] = contractsv1alpha1.AgentStageContextProviderBinding{
			ID: provider.ID, Revision: provider.Revision, Kind: provider.Kind,
			Adapter: componentBinding(
				provider.Adapter,
				contractsv1alpha1.AgentStagePlanContextProviderAdapterContract,
			),
		}
	}
	return components
}

func componentBinding(
	ref reviewconfig.VersionedRef,
	contract string,
) contractsv1alpha1.AgentStageComponentBinding {
	return contractsv1alpha1.AgentStageComponentBinding{
		Ref: versionedRef(ref.ID, ref.Revision, ref.SHA256),
		Artifact: contractsv1alpha1.ArtifactBinding{
			Ref: contractsv1alpha1.ContentRef{
				URI:       "artifact://local/component-" + ref.ID + "-" + ref.Revision,
				SHA256:    ref.SHA256,
				SizeBytes: int64(len(ref.ID) + len(ref.Revision)),
			},
			Contract: contract,
		},
	}
}

func configRef(id, revision, content string) *reviewconfig.VersionedRef {
	return &reviewconfig.VersionedRef{ID: id, Revision: revision, SHA256: digestLabel(content)}
}

func permission(value reviewconfig.Permission) *reviewconfig.Permission {
	return &value
}

func jsonArtifact(
	t *testing.T,
	_ string,
	contract string,
	value any,
) runmodel.ArtifactRef {
	t.Helper()
	data := mustJSON(t, value)
	digest := sha256Hex(data)
	return runmodel.ArtifactRef{
		URI:       "artifact://local/sha256/" + digest,
		SHA256:    digest,
		SizeBytes: int64(len(data)),
		Contract:  contract,
	}
}

func contentAddressedRef(contract, digest string, size int64) runmodel.ArtifactRef {
	return runmodel.ArtifactRef{
		URI:       "artifact://local/sha256/" + digest,
		SHA256:    digest,
		SizeBytes: size,
		Contract:  contract,
	}
}

func governedProjection(local runmodel.ArtifactRef) GovernedArtifactProjection {
	objectID := digestLabel(
		"governed\x00" + local.Contract + "\x00" + local.SHA256 + "\x00" +
			fmt.Sprintf("%d", local.SizeBytes),
	)
	return GovernedArtifactProjection{
		Local: local,
		Governed: contractsv1alpha1.ArtifactBinding{
			Ref: contractsv1alpha1.ContentRef{
				URI: "artifact://argus-local/tenants/tenant-1/workspaces/workspace-1/objects/" +
					objectID,
				SHA256:    local.SHA256,
				SizeBytes: local.SizeBytes,
			},
			Contract: local.Contract,
		},
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func digestLabel(value string) string {
	return sha256Hex([]byte(value))
}
