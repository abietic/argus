package application

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"argus.local/argus/internal/agentplan"
	"argus.local/argus/internal/configdefaults"
	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/source/gitadapter"
	"argus.local/argus/internal/targetmodel"
	"argus.local/argus/internal/workflow"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

func TestPrepareGovernedAgentStageCommitsExactPlanAndReusesAdmission(t *testing.T) {
	fixture := newAgentPreparationFixture(t, preparationContextsEmpty)
	preparer, err := NewAgentStagePreparer(
		fixture.provider,
		fixture.resolver,
		fixture.repository,
		fixture.repository,
		func() time.Time { return fixture.recordedAt },
	)
	if err != nil {
		t.Fatal(err)
	}
	first, err := preparer.PrepareGovernedAgentStage(
		context.Background(), fixture.subject, fixture.command,
	)
	if err != nil {
		t.Fatalf("PrepareGovernedAgentStage() error = %v", err)
	}
	if err := first.Plan.Validate(); err != nil {
		t.Fatalf("prepared Plan.Validate() error = %v", err)
	}
	if err := first.Admission.Validate(); err != nil {
		t.Fatalf("prepared Admission.Validate() error = %v", err)
	}
	if first.Admission.PlanSemanticSHA256 != first.Plan.SHA256 ||
		first.Admission.Plan.Governed.SHA256 == first.Plan.SHA256 ||
		first.Admission.ReceiptSemanticSHA256 != fixture.provider.receipt.SHA256 ||
		first.Admission.Sources.ConfigResolutionReceipt.Local.SHA256 ==
			fixture.provider.receipt.SHA256 {
		t.Fatalf("semantic/artifact digest classes were conflated: %+v", first.Admission)
	}
	for name, binding := range map[string]contractsv1alpha1.ArtifactBinding{
		"snapshot": first.Plan.ExecutionSnapshot,
		"config":   first.Plan.ConfigBundle,
		"receipt":  first.Plan.ConfigResolutionReceiptRef,
		"workflow": first.Plan.Workflow,
		"spec":     first.Plan.ReviewSpec,
		"input":    first.Plan.ReviewInput,
	} {
		if strings.HasPrefix(binding.Ref.URI, "artifact://local/") {
			t.Fatalf("prepared plan retained local %s URI %q", name, binding.Ref.URI)
		}
	}
	if fixture.provider.governedCalls != 1 || fixture.provider.legacyCalls != 0 ||
		fixture.resolver.calls != 1 || fixture.repository.projectionWrites != 7 ||
		fixture.repository.admissionWrites != 1 {
		t.Fatalf(
			"calls provider=%d/%d resolver=%d projections=%d admissions=%d",
			fixture.provider.governedCalls,
			fixture.provider.legacyCalls,
			fixture.resolver.calls,
			fixture.repository.projectionWrites,
			fixture.repository.admissionWrites,
		)
	}
	if got := fixture.repository.actions[len(fixture.repository.actions)-1]; got != "admission" {
		t.Fatalf("last commit action = %q, want admission", got)
	}

	providerCalls := fixture.provider.governedCalls
	resolverCalls := fixture.resolver.calls
	projectionWrites := fixture.repository.projectionWrites
	localWrites := fixture.repository.localWrites
	second, err := preparer.PrepareGovernedAgentStage(
		context.Background(), fixture.subject, fixture.command,
	)
	if err != nil {
		t.Fatalf("PrepareGovernedAgentStage(retry) error = %v", err)
	}
	if !reflect.DeepEqual(second, first) {
		t.Fatalf("idempotent retry changed prepared stage\nfirst=%+v\nsecond=%+v", first, second)
	}
	if fixture.provider.governedCalls != providerCalls ||
		fixture.resolver.calls != resolverCalls ||
		fixture.repository.projectionWrites != projectionWrites ||
		fixture.repository.localWrites != localWrites ||
		fixture.repository.admissionWrites != 1 {
		t.Fatalf("admitted retry performed mutable resolution or writes")
	}
}

func TestVerifyFrozenContextProviderReceiptsBindsConfigTargetAndContext(t *testing.T) {
	fixture := newAgentPreparationFixture(t, preparationContextsEmpty)
	var target targetmodel.MaterializedTarget
	if err := json.Unmarshal(
		fixture.repository.local[fixture.repository.snapshot.TargetSnapshotRef.URI].data,
		&target,
	); err != nil {
		t.Fatal(err)
	}
	var input reviewcore.ReviewInput
	if err := json.Unmarshal(
		fixture.repository.local[fixture.repository.snapshot.ReviewInputRef.URI].data,
		&input,
	); err != nil {
		t.Fatal(err)
	}
	definition := reviewconfig.ContextProviderDefinition{
		ID: "go-ast", Revision: "1", Kind: "go_ast",
		Adapter: reviewconfig.VersionedRef{
			ID: "argus-go-ast", Revision: "2", SHA256: preparationDigest("go-ast-adapter"),
		},
	}
	digest := preparationDigest("go-ast-gap-request")
	binding := reviewcore.ContextBinding{Gap: &reviewcore.ContextGap{
		ContextID: "go-ast-gap-123456789abc", Kind: definition.Kind,
		Revision: target.Snapshot.Head.CommitOID, Digest: digest,
		Coverage: reviewcore.ContextCoverage{
			Spans: []reviewcore.ContextSpan{}, Symbols: []string{"provider:go-ast"},
		},
		Provenance: reviewcore.ContextProvenance{
			Provider: definition.Kind, ProducerID: definition.Adapter.ID,
			ProducerRevision: definition.Adapter.Revision,
		},
		ReasonCode: "provider_unavailable",
	}}
	target.Contexts = []reviewcore.ContextBinding{binding}
	input.Contexts = []reviewcore.ContextBinding{binding}
	startedAt := time.Date(2026, 8, 24, 2, 1, 0, 0, time.UTC)
	receipt, err := contractsv1alpha1.SealContextProviderExecutionReceipt(
		contractsv1alpha1.ContextProviderExecutionReceipt{
			ProviderID: definition.ID, ProviderRevision: definition.Revision,
			Kind: definition.Kind,
			Adapter: contractsv1alpha1.VersionedRef{
				ID: definition.Adapter.ID, Revision: definition.Adapter.Revision,
				SHA256: definition.Adapter.SHA256,
			},
			RequestSHA256: preparationDigest("request"),
			RepositoryID:  target.Snapshot.Repository.RepositoryID,
			CommitOID:     target.Snapshot.Head.CommitOID,
			TargetPaths:   []string{"pkg/p.go"}, Status: contractsv1alpha1.ContextProviderReceiptGap,
			TargetRanges: []contractsv1alpha1.ContextProviderTargetRange{},
			ReasonCode:   binding.Gap.ReasonCode, ContextID: binding.Gap.ContextID,
			ContextDigest: binding.Gap.Digest, TimeoutMS: 1000,
			StartedAt: startedAt, CompletedAt: startedAt.Add(time.Millisecond), DurationMS: 1,
			Authority: "local_host_observation",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	receiptRef := fixture.repository.addJSON(
		t, runmodel.ContractContextProviderExecutionReceipt, receipt,
	)
	snapshot := fixture.repository.snapshot
	snapshot.ContextProviderReceiptRefs = []runmodel.ArtifactRef{receiptRef}
	bundle := fixture.provider.bundle
	bundle.Execution.ContextProviders = []reviewconfig.ContextProviderDefinition{definition}
	preparer := &AgentStagePreparer{artifacts: fixture.repository}
	if err := preparer.verifyFrozenContextProviderReceipts(
		context.Background(), snapshot, bundle, target, input,
	); err != nil {
		t.Fatalf("verifyFrozenContextProviderReceipts() error = %v", err)
	}

	wrongDefinition := definition
	wrongDefinition.Adapter.ID = "other-adapter"
	bundle.Execution.ContextProviders = []reviewconfig.ContextProviderDefinition{wrongDefinition}
	if err := preparer.verifyFrozenContextProviderReceipts(
		context.Background(), snapshot, bundle, target, input,
	); err == nil || !strings.Contains(err.Error(), "does not match governed config or target") {
		t.Fatalf("mismatched context provider receipt error = %v", err)
	}
	aliased := snapshot
	aliased.ContextProviderReceiptRefs = []runmodel.ArtifactRef{receiptRef, receiptRef}
	bundle.Execution.ContextProviders = []reviewconfig.ContextProviderDefinition{definition, definition}
	if err := preparer.verifyFrozenContextProviderReceipts(
		context.Background(), aliased, bundle, target, input,
	); err == nil || !strings.Contains(err.Error(), "alias context") {
		t.Fatalf("aliased context provider receipt error = %v", err)
	}
}

func TestPrepareGovernedAgentStageRejectsArtifactIncompatibleSubjectBeforeReads(t *testing.T) {
	fixture := newAgentPreparationFixture(t, preparationContextsEmpty)
	preparer, err := NewAgentStagePreparer(
		fixture.provider, fixture.resolver, fixture.repository, fixture.repository, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	subject := fixture.subject
	subject.TenantID = "租户"
	_, err = preparer.PrepareGovernedAgentStage(
		context.Background(), subject, fixture.command,
	)
	if err == nil || !strings.Contains(err.Error(), "artifact-compatible") {
		t.Fatalf("PrepareGovernedAgentStage() error = %v", err)
	}
	if fixture.repository.snapshotReads != 0 || fixture.provider.governedCalls != 0 ||
		fixture.resolver.calls != 0 || fixture.repository.projectionWrites != 0 {
		t.Fatal("artifact-incompatible subject reached a repository or provider")
	}
}

func TestPrepareGovernedAgentStageAcceptsExactLocalFrozenContext(t *testing.T) {
	fixture := newAgentPreparationFixture(t, preparationContextsLocalRef)
	preparer, err := NewAgentStagePreparer(
		fixture.provider,
		fixture.resolver,
		fixture.repository,
		fixture.repository,
		func() time.Time { return fixture.recordedAt },
	)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := preparer.PrepareGovernedAgentStage(
		context.Background(), fixture.subject, fixture.command,
	)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Plan.PlanID == "" || fixture.provider.governedCalls != 1 ||
		fixture.resolver.calls != 1 || fixture.repository.projectionWrites == 0 ||
		fixture.repository.admissionWrites != 1 {
		t.Fatalf(
			"prepared=%+v provider=%d resolver=%d projections=%d admissions=%d",
			prepared,
			fixture.provider.governedCalls,
			fixture.resolver.calls,
			fixture.repository.projectionWrites,
			fixture.repository.admissionWrites,
		)
	}
}

func TestPrepareGovernedAgentStageRejectsRolloutDriftBeforeComponentResolution(t *testing.T) {
	fixture := newAgentPreparationFixture(t, preparationContextsEmpty)
	drifted := fixture.provider.bundle
	drifted.Context.InvocationID = "another-run"
	fixture.provider.bundle = drifted
	preparer, err := NewAgentStagePreparer(
		fixture.provider, fixture.resolver, fixture.repository, fixture.repository, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = preparer.PrepareGovernedAgentStage(
		context.Background(), fixture.subject, fixture.command,
	)
	if err == nil {
		t.Fatal("PrepareGovernedAgentStage() accepted config rollout drift")
	}
	if fixture.resolver.calls != 0 || fixture.repository.projectionWrites != 0 ||
		fixture.repository.admissionWrites != 0 {
		t.Fatal("rollout drift reached component resolution or projection writes")
	}
}

func TestPrepareGovernedAgentStageRejectsSelfSealedAdmittedPlanOutsideFrozenClosure(
	t *testing.T,
) {
	fixture := newAgentPreparationFixture(t, preparationContextsEmpty)
	preparer, err := NewAgentStagePreparer(
		fixture.provider,
		fixture.resolver,
		fixture.repository,
		fixture.repository,
		func() time.Time { return fixture.recordedAt },
	)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := preparer.PrepareGovernedAgentStage(
		context.Background(), fixture.subject, fixture.command,
	)
	if err != nil {
		t.Fatalf("PrepareGovernedAgentStage() error = %v", err)
	}

	forged := prepared.Plan
	forged.Budget.MaxHypotheses++
	forged, err = contractsv1alpha1.SealAgentStagePlan(forged)
	if err != nil {
		t.Fatalf("SealAgentStagePlan(forged) error = %v", err)
	}
	forgedBytes, err := json.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	forgedLocal := fixture.repository.addLocal(runmodel.ContractAgentStagePlan, forgedBytes)
	forgedGoverned := fixture.repository.addGoverned(
		fixture.subject,
		runmodel.ContractAgentStagePlan,
		forgedBytes,
	)
	forgedAdmission := prepared.Admission
	forgedAdmission.PlanID = forged.PlanID
	forgedAdmission.PlanSemanticSHA256 = forged.SHA256
	forgedAdmission.PlanBehaviorSHA256 = forged.BehaviorSHA256
	forgedAdmission.Plan = runmodel.AgentArtifactProjection{
		Local: forgedLocal,
		Governed: runmodel.GovernedArtifactBinding{
			URI: forgedGoverned.Ref.URI, SHA256: forgedGoverned.Ref.SHA256,
			SizeBytes: forgedGoverned.Ref.SizeBytes, Contract: forgedGoverned.Contract,
		},
	}
	forgedAdmission, err = runmodel.SealAgentStagePlanAdmission(forgedAdmission)
	if err != nil {
		t.Fatalf("SealAgentStagePlanAdmission(forged) error = %v", err)
	}
	fixture.repository.admission = &forgedAdmission
	fixture.repository.resetObservations()

	_, err = preparer.PrepareGovernedAgentStage(
		context.Background(), fixture.subject, fixture.command,
	)
	if err == nil || !strings.Contains(err.Error(), "canonical frozen-closure compilation") {
		t.Fatalf("PrepareGovernedAgentStage(forged admission) error = %v", err)
	}
	if fixture.provider.governedCalls != 1 || fixture.resolver.calls != 1 ||
		fixture.repository.projectionWrites != 0 || fixture.repository.localWrites != 0 ||
		fixture.repository.admissionWrites != 0 {
		t.Fatal("forged admitted plan triggered mutable resolution or writes")
	}
}

type preparationContextMode int

const (
	preparationContextsEmpty preparationContextMode = iota
	preparationContextsLocalRef
)

type agentPreparationFixture struct {
	repository *agentPreparationRepositoryStub
	provider   *governedConfigProviderStub
	resolver   *agentComponentResolverStub
	subject    AgentPlanningSubject
	command    PrepareAgentStageCommand
	recordedAt time.Time
}

func newAgentPreparationFixture(
	t *testing.T,
	contextMode preparationContextMode,
) agentPreparationFixture {
	t.Helper()
	subject := AgentPlanningSubject{
		TenantID: "tenant-1", OrganizationID: "org-1",
		WorkspaceID: "workspace-1", RepositoryID: "repo-1",
	}
	repository := newAgentPreparationRepositoryStub(subject)
	definition := preparationWorkflow()
	componentData := preparationComponentData()
	resolutionContext := reviewconfig.ResolutionContext{
		TenantID: subject.TenantID, OrganizationID: subject.OrganizationID,
		RepositoryID: subject.RepositoryID, Path: "pkg/p.go", InvocationID: "run-1",
	}
	revision, err := configdefaults.Revision(configdefaults.Options{
		ID: "platform-default", Revision: "1", MaxFiles: 20,
		MaxPatchBytes: 1 << 20, MaxInputBytes: 1 << 20,
		MaxOutputBytes: 1 << 20, MaxAttempts: 1,
	}, definition)
	if err != nil {
		t.Fatal(err)
	}
	revision.Patch.Execution.AgentProfile = preparationConfigRef(
		"formal-agent-runtime", "1", componentData["runtime"],
	)
	revision.Patch.Execution.AllowedTools = &reviewconfig.SetPatch{
		Add: []string{"codegraph"}, Remove: []string{},
	}
	revision.Patch.AgentReview = preparationAgentPolicy(componentData)
	if err := revision.Validate(); err != nil {
		t.Fatalf("fixture revision Validate() error = %v", err)
	}
	bundle, err := reviewconfig.Resolve(
		resolutionContext,
		[]reviewconfig.Revision{revision},
	)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := reviewconfig.NewConfigResolutionReceipt(
		bundle,
		[]reviewconfig.PublishedRevisionBinding{{
			Source: bundle.AppliedRevisions[0], RevisionSHA256: preparationDigest("revision"),
			PublishEventID: "publish-event-1", PublishSequence: 1,
			PublishedAt:      time.Date(2026, 8, 24, 1, 2, 3, 0, time.UTC),
			AssignmentSHA256: preparationDigest("assignment"),
		}},
	)
	if err != nil {
		t.Fatal(err)
	}

	content := "package p\n\nfunc F() {}\n"
	selected := "package p\n"
	contentRef := repository.addLocal(targetmodel.ContractFileContent, []byte(content))
	selectedRef := repository.addLocal(targetmodel.ContractSelectionContent, []byte(selected))
	manifestRef := repository.addLocal(
		targetmodel.ContractSelectionManifest,
		[]byte(`{"schema_version":"argus.selection_manifest.fixture.v1"}`),
	)
	contexts := []reviewcore.ContextBinding{}
	if contextMode == preparationContextsLocalRef {
		contextData := []byte("local context")
		contextRef := repository.addLocal(
			"argus.context.repository_search.v1alpha1",
			contextData,
		)
		contexts = []reviewcore.ContextBinding{{
			Ref: &reviewcore.ContextRef{
				ContextID: "context-1", Kind: "repository_search", Revision: "head",
				Digest: contextRef.SHA256,
				Coverage: reviewcore.ContextCoverage{
					Spans: []reviewcore.ContextSpan{{
						Path: "pkg/related.go", StartLine: 1, EndLine: 2,
					}},
					Symbols: []string{},
				},
				Provenance: reviewcore.ContextProvenance{
					Provider: "local-test", ProducerID: "fixture", ProducerRevision: "1",
				},
				ArtifactURI: contextRef.URI, Contract: contextRef.Contract,
				SizeBytes: contextRef.SizeBytes,
			},
		}}
	}
	revisionSnapshot := gitadapter.RevisionSnapshot{
		Requested: "HEAD", CommitOID: strings.Repeat("a", 40),
	}
	targetSnapshot, err := targetmodel.SealTargetSnapshot(targetmodel.TargetSnapshot{
		SchemaVersion: targetmodel.TargetSnapshotSchemaVersion,
		Mode:          reviewcore.TargetModeSelection,
		Repository: gitadapter.RepositorySnapshot{
			Kind: "local_git", RepositoryID: subject.RepositoryID, ObjectFormat: "sha1",
		},
		Base: revisionSnapshot, Head: revisionSnapshot,
		ManifestSHA256: manifestRef.SHA256,
		Selection: &targetmodel.SelectionSnapshot{
			Path: "pkg/p.go", StartLine: 1, EndLine: 1,
			EffectiveRanges: []targetmodel.SelectionRange{{StartLine: 1, EndLine: 1}},
			SourceKind:      targetmodel.SelectionSourceCommit,
			FileSHA256:      contentRef.SHA256, FileSizeBytes: contentRef.SizeBytes,
			SelectionContentSHA256: selectedRef.SHA256,
			SelectionSizeBytes:     selectedRef.SizeBytes,
		},
		DirtyState: gitadapter.DirtyStateClean, Completeness: gitadapter.CompletenessComplete,
		CompletenessReason: []gitadapter.Reason{},
		CapturedAt:         time.Date(2026, 8, 24, 2, 0, 0, 0, time.UTC),
		CapturedBy:         "argus-test", GitVersion: "git version test",
	})
	if err != nil {
		t.Fatal(err)
	}
	target := targetmodel.MaterializedTarget{
		SchemaVersion: targetmodel.MaterializedTargetSchemaVersion,
		Snapshot:      targetSnapshot, ManifestRef: manifestRef,
		SelectionContentRef: &selectedRef,
		FileRefs: []targetmodel.TargetFileRef{{
			Path: "pkg/p.go", SHA256: contentRef.SHA256, SizeBytes: contentRef.SizeBytes,
			ContentRef: &contentRef, Completeness: gitadapter.CompletenessComplete,
			Reasons: []gitadapter.Reason{},
		}},
		Contexts: preparationCloneContexts(contexts),
	}
	if err := target.Validate(); err != nil {
		t.Fatal(err)
	}
	reviewInput := reviewcore.ReviewInput{
		SchemaVersion: reviewcore.ReviewInputSchemaVersion,
		TargetID:      targetSnapshot.TargetSnapshotID, TargetMode: reviewcore.TargetModeSelection,
		CanonicalPatch: "",
		Regions: []reviewcore.ReviewRegion{{
			Path: "pkg/p.go", StartLine: 1, EndLine: 1, SHA256: contentRef.SHA256,
		}},
		Files: []reviewcore.FileManifestEntry{{
			Path: "pkg/p.go", SHA256: contentRef.SHA256,
			SizeBytes: contentRef.SizeBytes, Content: &content,
		}},
		Contexts: preparationCloneContexts(contexts),
	}
	if err := reviewInput.Validate(); err != nil {
		t.Fatal(err)
	}
	workflowDigest, err := workflow.DigestDefinition(definition)
	if err != nil {
		t.Fatal(err)
	}
	workflowRef := repository.addJSON(t, runmodel.ContractWorkflowDefinition, definition)
	configRef := repository.addJSON(t, runmodel.ContractConfigBundle, bundle)
	inputRef := repository.addJSON(t, runmodel.ContractReviewInput, reviewInput)
	reviewSpec := contractsv1alpha1.ReviewSpec{
		SchemaVersion: contractsv1alpha1.ReviewSpecSchemaVersion,
		RequestID:     "run-1", IdempotencyKey: "run-1",
		TenantID: subject.TenantID, WorkspaceID: subject.WorkspaceID,
		Repository: contractsv1alpha1.RepositoryRef{
			Provider: "local-git", RepositoryID: subject.RepositoryID,
		},
		Target: contractsv1alpha1.ReviewTarget{
			Mode: contractsv1alpha1.ReviewModeSelection,
			Selection: &contractsv1alpha1.SelectionTarget{
				Revision: revisionSnapshot.CommitOID, Path: "pkg/p.go", StartLine: 1, EndLine: 1,
				Content: contractsv1alpha1.ContentRef{
					URI: selectedRef.URI, SHA256: selectedRef.SHA256,
					SizeBytes: selectedRef.SizeBytes,
				},
			},
		},
		ConfigBundleRef: contractsv1alpha1.VersionedRef{
			ID: bundle.BundleID, Revision: bundle.SHA256[:16], SHA256: configRef.SHA256,
		},
		WorkflowRef: contractsv1alpha1.VersionedRef{
			ID: definition.ID, Revision: definition.Revision, SHA256: workflowDigest,
		},
		RequestedOutput: []string{"findings", "report", "trace"}, RemoteWrites: "deny",
	}
	if err := reviewSpec.Validate(); err != nil {
		t.Fatal(err)
	}
	specRef := repository.addJSON(t, runmodel.ContractReviewSpec, reviewSpec)
	targetRef := repository.addJSON(t, runmodel.ContractMaterializedTarget, target)
	snapshot := runmodel.ExecutionSnapshot{
		SchemaVersion: runmodel.SnapshotSchemaVersion, ExecutionSnapshotID: "snapshot-1",
		ReviewSpecSHA256: specRef.SHA256, ReviewSpecRef: specRef,
		TargetSnapshotRef: targetRef, ReviewInputRef: inputRef,
		ReplayInputRefs:       []runmodel.ArtifactRef{},
		WorkflowDefinitionRef: workflowRef, ConfigBundleRef: configRef,
		Workflow: runmodel.WorkflowRef{
			ID: definition.ID, Revision: definition.Revision, SHA256: workflowDigest,
		},
		Config: runmodel.PolicyRef{
			ID: bundle.BundleID, Revision: bundle.SHA256[:16], SHA256: bundle.SHA256,
		},
		RuntimeProfile: bundle.Execution.AgentProfile.ID + "@" +
			bundle.Execution.AgentProfile.Revision,
		BuildIdentity: "argus-test-build",
		ToolPolicy: runmodel.ToolInvocationPolicy{
			AllowedTools: slices.Clone(bundle.Execution.AllowedTools),
			Network:      "deny", WorkspaceWrites: "deny", RemoteWrites: "deny",
			PerCallTimeoutMS: bundle.Budget.StageTimeoutMS,
			MaxOutputBytes:   bundle.Budget.MaxOutputBytes,
			MaxConcurrency:   bundle.Budget.MaxConcurrency, MaxDelegationDepth: 0,
		},
		RemoteWrites: "deny",
		CreatedAt:    time.Date(2026, 8, 24, 2, 3, 4, 0, time.UTC),
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	repository.snapshot = snapshot
	components := preparationResolvedComponents(
		repository,
		subject,
		*bundle.AgentReview,
		bundle.Execution,
		componentData,
	)
	repository.resetObservations()
	return agentPreparationFixture{
		repository: repository,
		provider:   &governedConfigProviderStub{bundle: bundle, receipt: receipt},
		resolver:   &agentComponentResolverStub{components: components},
		subject:    subject,
		command: PrepareAgentStageCommand{
			ReviewRunID: "run-1", StageID: "agent_hypothesize",
		},
		recordedAt: time.Date(2026, 8, 24, 3, 4, 5, 0, time.UTC),
	}
}

func preparationWorkflow() workflow.Definition {
	return workflow.Definition{
		SchemaVersion: workflow.DefinitionSchemaVersion,
		ID:            "formal-agent-review", Revision: "1",
		Stages: []workflow.Stage{{
			ID: "agent_hypothesize", Kind: agentplan.AgentHypothesizeStageKind,
			ImplementationRevision: "1",
			InputContract:          contractsv1alpha1.AgentStagePlanReviewInputContract,
			OutputContract:         contractsv1alpha1.AgentStagePlanOutputContract,
			DependsOn:              []string{}, Executor: "formal-agent-runtime",
			RequiredCapabilities: []string{"brokered-model", "frozen-review-input"},
			AuthorityCeiling: &workflow.StageAuthorityCeiling{
				ModelEgress: "provider_broker_only", AllowedTools: []string{"codegraph"},
				ToolNetwork: "deny", WorkspaceReads: "frozen_input_only",
				WorkspaceWrites: "deny", RemoteWrites: "deny", MaxDelegationDepth: 0,
				MaxModelCalls: 16, MaxToolCalls: 16,
			},
			Budget: workflow.StageBudget{
				TimeoutMS: 30_000, MaxInputBytes: 1 << 20,
				MaxOutputBytes: 1 << 20, MaxConcurrency: 1,
			},
			Retry: workflow.RetryPolicy{
				MaxAttempts: 1, BackoffMS: 0, Jitter: false,
				RetryableCodes: []string{}, UnknownOutcome: workflow.UnknownOutcomeFail,
			},
			FailurePolicy: workflow.FailurePolicyFailRun,
			SideEffect:    workflow.SideEffectNone, ReplayPolicy: workflow.ReplayPolicyExact,
		}},
	}
}

func preparationComponentData() map[string][]byte {
	return map[string][]byte{
		"agent":     []byte(`{"id":"pi-agent","revision":"1"}`),
		"provider":  []byte(`{"id":"anthropic-compatible","revision":"1"}`),
		"model":     []byte(`{"id":"deepseek-chat","revision":"1"}`),
		"runtime":   []byte(`{"id":"formal-agent-runtime","revision":"1"}`),
		"prompt":    []byte(`{"id":"review-prompt","revision":"1"}`),
		"api":       []byte(`{"id":"anthropic-messages","revision":"2023-06-01"}`),
		"skill":     []byte(`{"id":"review-core","revision":"1"}`),
		"knowledge": []byte(`{"id":"repository-standards","revision":"1"}`),
	}
}

func preparationAgentPolicy(
	components map[string][]byte,
) *reviewconfig.AgentReviewPatch {
	modelEgress := reviewconfig.AgentModelEgressProviderBrokerOnly
	workspaceReads := reviewconfig.AgentWorkspaceReadsFrozenInputOnly
	delegation := 0
	maxFiles, maxGroups, maxHypotheses := 10, 4, 16
	maxModelCalls, maxToolCalls := 8, 8
	maxTargetBytes, maxGroupBytes := int64(64<<10), int64(16<<10)
	maxOutputBytes, maxOutputTokens := int64(64<<10), int64(4096)
	maxCostMicros, timeoutMS := int64(500_000), int64(10_000)
	maxConcurrency := 1
	return &reviewconfig.AgentReviewPatch{
		Agent: preparationConfigRef("pi-agent", "1", components["agent"]),
		Normalization: preparationConfigRef(
			"candidate-normalization", "v2", components["agent"],
		),
		Provider: preparationConfigRef(
			"anthropic-compatible", "1", components["provider"],
		),
		Model:  preparationConfigRef("deepseek-chat", "1", components["model"]),
		Prompt: preparationConfigRef("review-prompt", "1", components["prompt"]),
		SkillPacks: &reviewconfig.OrderedAgentSkillPackPatch{
			Upsert: []reviewconfig.AgentSkillPackDefinition{{
				ID: "review-core", Phase: reviewconfig.AgentSkillPhaseReview,
				Ref: *preparationConfigRef("skill-review-core", "1", components["skill"]),
			}},
			Remove: []string{},
		},
		KnowledgePacks: &reviewconfig.OrderedAgentKnowledgePackPatch{
			Upsert: []reviewconfig.AgentKnowledgePackDefinition{{
				ID: "repository-standards",
				Ref: *preparationConfigRef(
					"knowledge-repository-standards", "1", components["knowledge"],
				),
			}},
			Remove: []string{},
		},
		APIProtocol: preparationConfigRef(
			"anthropic-messages", "2023-06-01", components["api"],
		),
		Authority: &reviewconfig.AgentAuthorityPatch{
			ModelEgress:        &modelEgress,
			ToolNetwork:        preparationPermission(reviewconfig.PermissionDeny),
			WorkspaceReads:     &workspaceReads,
			WorkspaceWrites:    preparationPermission(reviewconfig.PermissionDeny),
			RemoteWrites:       preparationPermission(reviewconfig.PermissionDeny),
			MaxDelegationDepth: &delegation,
			Tools:              &reviewconfig.SetPatch{Add: []string{"codegraph"}, Remove: []string{}},
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

func preparationResolvedComponents(
	repository *agentPreparationRepositoryStub,
	subject AgentPlanningSubject,
	policy reviewconfig.AgentReviewPolicy,
	execution reviewconfig.ExecutionPolicy,
	data map[string][]byte,
) agentplan.ResolvedComponents {
	component := func(
		ref reviewconfig.VersionedRef,
		contract string,
		content []byte,
	) contractsv1alpha1.AgentStageComponentBinding {
		binding := repository.addGoverned(subject, contract, content)
		return contractsv1alpha1.AgentStageComponentBinding{
			Ref: contractsv1alpha1.VersionedRef{
				ID: ref.ID, Revision: ref.Revision, SHA256: ref.SHA256,
			},
			Artifact: binding,
		}
	}
	result := agentplan.ResolvedComponents{
		Agent: component(policy.Agent, contractsv1alpha1.AgentStagePlanAgentContract, data["agent"]),
		Provider: component(
			policy.Provider, contractsv1alpha1.AgentStagePlanProviderContract, data["provider"],
		),
		Model: component(policy.Model, contractsv1alpha1.AgentStagePlanModelContract, data["model"]),
		Runtime: component(
			execution.AgentProfile, contractsv1alpha1.AgentStagePlanRuntimeContract, data["runtime"],
		),
		Prompt: component(
			policy.Prompt, contractsv1alpha1.AgentStagePlanPromptContract, data["prompt"],
		),
		APIProtocol: component(
			policy.APIProtocol, contractsv1alpha1.AgentStagePlanAPIProtocolContract, data["api"],
		),
		Skills:           []contractsv1alpha1.AgentStageSkillBinding{},
		Knowledge:        []contractsv1alpha1.AgentStageKnowledgeBinding{},
		ContextProviders: []contractsv1alpha1.AgentStageContextProviderBinding{},
	}
	for _, skill := range policy.SkillPacks {
		binding := component(
			skill.Ref, contractsv1alpha1.AgentStagePlanSkillContract, data["skill"],
		)
		result.Skills = append(result.Skills, contractsv1alpha1.AgentStageSkillBinding{
			ID: skill.ID, Phase: contractsv1alpha1.AgentStageSkillPhase(skill.Phase),
			Ref: binding.Ref, Artifact: binding.Artifact,
		})
	}
	for _, knowledge := range policy.KnowledgePacks {
		binding := component(
			knowledge.Ref, contractsv1alpha1.AgentStagePlanKnowledgeContract, data["knowledge"],
		)
		result.Knowledge = append(
			result.Knowledge,
			contractsv1alpha1.AgentStageKnowledgeBinding{
				ID: knowledge.ID, Ref: binding.Ref, Artifact: binding.Artifact,
			},
		)
	}
	return result
}

func preparationConfigRef(id, revision string, content []byte) *reviewconfig.VersionedRef {
	return &reviewconfig.VersionedRef{
		ID: id, Revision: revision, SHA256: preparationSHA(content),
	}
}

func preparationPermission(value reviewconfig.Permission) *reviewconfig.Permission {
	copy := value
	return &copy
}

func preparationDigest(label string) string {
	return preparationSHA([]byte(label))
}

func preparationSHA(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func preparationCloneContexts(
	contexts []reviewcore.ContextBinding,
) []reviewcore.ContextBinding {
	data, err := json.Marshal(contexts)
	if err != nil {
		panic(err)
	}
	var cloned []reviewcore.ContextBinding
	if err := json.Unmarshal(data, &cloned); err != nil {
		panic(err)
	}
	return cloned
}

type preparationStoredArtifact struct {
	binding contractsv1alpha1.ArtifactBinding
	local   runmodel.ArtifactRef
	data    []byte
	subject AgentPlanningSubject
}

type agentPreparationRepositoryStub struct {
	subject   AgentPlanningSubject
	snapshot  runmodel.ExecutionSnapshot
	local     map[string]preparationStoredArtifact
	governed  map[string]preparationStoredArtifact
	admission *runmodel.AgentStagePlanAdmission

	snapshotReads         int
	localWrites           int
	projectionWrites      int
	admissionWrites       int
	rejectCanceledContext bool
	actions               []string
}

func newAgentPreparationRepositoryStub(
	subject AgentPlanningSubject,
) *agentPreparationRepositoryStub {
	return &agentPreparationRepositoryStub{
		subject:  subject,
		local:    make(map[string]preparationStoredArtifact),
		governed: make(map[string]preparationStoredArtifact),
	}
}

func (repository *agentPreparationRepositoryStub) resetObservations() {
	repository.snapshotReads = 0
	repository.localWrites = 0
	repository.projectionWrites = 0
	repository.admissionWrites = 0
	repository.actions = nil
}

func (repository *agentPreparationRepositoryStub) addLocal(
	contract string,
	data []byte,
) runmodel.ArtifactRef {
	ref := localRefForBytes(contract, data)
	repository.local[ref.URI] = preparationStoredArtifact{
		local: ref, data: bytes.Clone(data),
	}
	return ref
}

func (repository *agentPreparationRepositoryStub) addJSON(
	t *testing.T,
	contract string,
	value any,
) runmodel.ArtifactRef {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return repository.addLocal(contract, data)
}

func (repository *agentPreparationRepositoryStub) addGoverned(
	subject AgentPlanningSubject,
	contract string,
	data []byte,
) contractsv1alpha1.ArtifactBinding {
	digest := preparationSHA(data)
	object := preparationDigest(
		subject.TenantID + "\x00" + subject.WorkspaceID + "\x00" + contract + "\x00" + digest,
	)
	binding := contractsv1alpha1.ArtifactBinding{
		Ref: contractsv1alpha1.ContentRef{
			URI: "artifact://argus-test/tenants/" + subject.TenantID +
				"/workspaces/" + subject.WorkspaceID + "/objects/" + object,
			SHA256: digest, SizeBytes: int64(len(data)),
		},
		Contract: contract,
	}
	repository.governed[binding.Ref.URI] = preparationStoredArtifact{
		binding: binding, data: bytes.Clone(data), subject: subject,
	}
	return binding
}

func (repository *agentPreparationRepositoryStub) ExecutionSnapshotForRun(
	_ context.Context,
	runID string,
) (runmodel.ExecutionSnapshot, error) {
	repository.snapshotReads++
	if runID != "run-1" {
		return runmodel.ExecutionSnapshot{}, errors.New("run not found")
	}
	return repository.snapshot, nil
}

func (repository *agentPreparationRepositoryStub) CommittedRunRef(
	_ context.Context,
	_ string,
) (runmodel.ArtifactRef, error) {
	return runmodel.ArtifactRef{}, errors.New("run commit not found")
}

func (repository *agentPreparationRepositoryStub) ReadLocalArtifact(
	ctx context.Context,
	ref runmodel.ArtifactRef,
) ([]byte, error) {
	if repository.rejectCanceledContext && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	stored, ok := repository.local[ref.URI]
	if !ok || stored.local != ref {
		return nil, errors.New("local artifact not found")
	}
	return bytes.Clone(stored.data), nil
}

func (repository *agentPreparationRepositoryStub) PutLocalArtifact(
	ctx context.Context,
	contract string,
	data []byte,
) (runmodel.ArtifactRef, error) {
	if repository.rejectCanceledContext && ctx.Err() != nil {
		return runmodel.ArtifactRef{}, ctx.Err()
	}
	repository.localWrites++
	repository.actions = append(repository.actions, "local:"+contract)
	return repository.addLocal(contract, data), nil
}

func (repository *agentPreparationRepositoryStub) ProjectReadOnly(
	_ context.Context,
	subject AgentPlanningSubject,
	request AgentArtifactProjectionRequest,
) (contractsv1alpha1.ArtifactBinding, error) {
	repository.projectionWrites++
	repository.actions = append(repository.actions, "projection:"+request.Role)
	if request.Source != localRefForBytes(request.Source.Contract, request.ExactContent) {
		return contractsv1alpha1.ArtifactBinding{}, errors.New("source mismatch")
	}
	return repository.addGoverned(subject, request.Source.Contract, request.ExactContent), nil
}

func (repository *agentPreparationRepositoryStub) VerifyRead(
	_ context.Context,
	subject AgentPlanningSubject,
	binding contractsv1alpha1.ArtifactBinding,
) ([]byte, error) {
	parsed, err := url.Parse(binding.Ref.URI)
	if err != nil || parsed.Scheme != "artifact" || parsed.Host != "argus-test" {
		return nil, errors.New("not a governed artifact URI")
	}
	stored, ok := repository.governed[binding.Ref.URI]
	if !ok || stored.binding != binding || stored.subject != subject ||
		preparationSHA(stored.data) != binding.Ref.SHA256 ||
		int64(len(stored.data)) != binding.Ref.SizeBytes {
		return nil, errors.New("governed artifact is unavailable to subject")
	}
	return bytes.Clone(stored.data), nil
}

func (repository *agentPreparationRepositoryStub) LookupAgentStagePlanAdmission(
	_ context.Context,
	runID string,
	stageID string,
	subject runmodel.AgentPlanningSubject,
) (runmodel.AgentStagePlanAdmission, bool, error) {
	if repository.admission == nil {
		return runmodel.AgentStagePlanAdmission{}, false, nil
	}
	if repository.admission.ReviewRunID != runID ||
		repository.admission.Stage.ID != stageID || repository.admission.Subject != subject {
		return runmodel.AgentStagePlanAdmission{}, false, errors.New("admission mismatch")
	}
	return *repository.admission, true, nil
}

func (repository *agentPreparationRepositoryStub) AppendAgentStagePlanAdmission(
	_ context.Context,
	admission runmodel.AgentStagePlanAdmission,
) error {
	repository.admissionWrites++
	repository.actions = append(repository.actions, "admission")
	if repository.admission != nil {
		if *repository.admission == admission {
			return nil
		}
		return fmt.Errorf("admission conflict")
	}
	copy := admission
	repository.admission = &copy
	return nil
}
