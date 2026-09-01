package agentshadow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abietic/argus/internal/application"
	"github.com/abietic/argus/internal/artifactrepo"
	"github.com/abietic/argus/internal/reviewcore"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/source/gitadapter"
	"github.com/abietic/argus/internal/store/local"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

var shadowTestDigest = strings.Repeat("a", 64)

type shadowFixture struct {
	store        *local.Store
	runs         *runrepo.Repository
	repository   *Repository
	service      *Service
	now          *time.Time
	scope        Scope
	plan         contractsv1alpha1.AgentReviewPlan
	set          contractsv1alpha1.ReviewHypothesisSet
	raw          contractsv1alpha1.AgentReviewRawCandidateCollection
	taskEvidence contractsv1alpha1.AgentReviewTaskEvidenceCollection
	collection   contractsv1alpha1.AgentExecutionReceiptCollection
	snapshot     runmodel.ExecutionSnapshot
}

func TestImportQueryIsIdempotentAndLeavesFormalLedgersUntouched(t *testing.T) {
	fixture := newShadowFixture(t)
	request := fixture.request(t, "import-1")
	formalRunBefore, err := fixture.runs.LoadRun(fixture.plan.SourceRunID)
	if err != nil {
		t.Fatal(err)
	}
	formalRefBefore, err := fixture.runs.CommittedRunRef(fixture.plan.SourceRunID)
	if err != nil {
		t.Fatal(err)
	}
	formalHistoryBefore, err := fixture.runs.History(0)
	if err != nil {
		t.Fatal(err)
	}
	first, err := fixture.service.Import(t.Context(), request)
	if err != nil {
		t.Fatalf("Import() error = %v", err)
	}
	if first.Manifest.ProducerClass != "argus_go_host" ||
		first.Manifest.Disposition != "shadow_only" ||
		first.Observation.ProducerClass != "argus_go_host" ||
		first.Observation.Summary.UsageCompleteness !=
			contractsv1alpha1.AgentTokenUsageProviderReported ||
		first.Observation.Summary.Receipts != 3 ||
		!first.Manifest.RecordedAt.Equal(first.Record.AcceptedAt) ||
		!first.Observation.RecordedAt.Equal(first.Record.AcceptedAt) {
		t.Fatalf("host result = %+v", first)
	}
	if !strings.HasPrefix(
		first.Record.AgentReviewPlanRef.Ref.URI,
		"artifact://argus-local/tenants/",
	) {
		t.Fatalf("plan ref is not governed: %+v", first.Record.AgentReviewPlanRef)
	}
	originalAcceptedAt := first.Record.AcceptedAt
	*fixture.now = fixture.now.Add(time.Hour)
	replayed, err := fixture.service.Import(t.Context(), request)
	if err != nil {
		t.Fatalf("idempotent Import() error = %v", err)
	}
	if !reflect.DeepEqual(replayed, first) ||
		!replayed.Record.AcceptedAt.Equal(originalAcceptedAt) {
		t.Fatalf("retry changed committed result\nfirst=%+v\nreplayed=%+v", first, replayed)
	}
	queried, err := fixture.service.Query(t.Context(), QueryRequest{
		Scope: fixture.scope, ManifestID: first.Manifest.ManifestID,
	})
	if err != nil || !reflect.DeepEqual(queried, first) {
		t.Fatalf("Query() = %+v, %v", queried, err)
	}
	for name, binding := range map[string]contractsv1alpha1.ArtifactBinding{
		"plan":               first.Record.AgentReviewPlanRef,
		"hypothesis set":     first.Record.HypothesisSetRef,
		"raw candidates":     first.Record.RawCandidateCollectionRef,
		"receipt collection": first.Record.ReceiptCollectionRef,
		"manifest":           first.Record.ManifestRef,
	} {
		artifactRecord, err := fixture.repository.artifacts.Inspect(
			t.Context(),
			artifactSubject(fixture.scope),
			artifactRef(binding),
		)
		if err != nil {
			t.Fatal(err)
		}
		if !artifactRecord.Metadata.CreatedAt.Equal(originalAcceptedAt) ||
			!artifactRecord.StateChangedAt.Equal(originalAcceptedAt) {
			t.Fatalf(
				"%s host audit times = (%s, %s), want stable intent time %s",
				name,
				artifactRecord.Metadata.CreatedAt,
				artifactRecord.StateChangedAt,
				originalAcceptedAt,
			)
		}
	}
	formalRunAfter, err := fixture.runs.LoadRun(fixture.plan.SourceRunID)
	if err != nil {
		t.Fatal(err)
	}
	formalRefAfter, err := fixture.runs.CommittedRunRef(fixture.plan.SourceRunID)
	if err != nil {
		t.Fatal(err)
	}
	formalHistoryAfter, err := fixture.runs.History(0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(formalRunAfter, formalRunBefore) ||
		formalRefAfter != formalRefBefore ||
		!reflect.DeepEqual(formalHistoryAfter, formalHistoryBefore) {
		t.Fatalf("shadow import changed the formal source run or history")
	}
	feedbackLedger := filepath.Join(
		fixture.store.Root(),
		"streams",
		"feedback-outcome-ledger.jsonl",
	)
	if _, err := os.Stat(feedbackLedger); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("shadow import wrote formal feedback state %q: %v", feedbackLedger, err)
	}
}

func TestImportIdempotencyConflictAndQueryScopeIsolation(t *testing.T) {
	fixture := newShadowFixture(t)
	request := fixture.request(t, "import-conflict")
	result, err := fixture.service.Import(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	fixture.set.Hypotheses[0].Title = "a different validated title"
	conflicting := fixture.request(t, "import-conflict")
	if _, err := fixture.service.Import(
		t.Context(),
		conflicting,
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting Import() error = %v, want ErrConflict", err)
	}
	if _, err := fixture.service.Query(t.Context(), QueryRequest{
		Scope:      Scope{TenantID: fixture.scope.TenantID, WorkspaceID: "workspace-other"},
		ManifestID: result.Manifest.ManifestID,
	}); !errors.Is(err, artifactrepo.ErrUnauthorized) {
		t.Fatalf("cross-scope Query() error = %v, want ErrUnauthorized", err)
	}
	envelopes, err := fixture.store.ReadJSONL(result.Record.ObservationStream)
	if err != nil || len(envelopes) != 1 {
		t.Fatalf("observation stream after conflict = %d, %v", len(envelopes), err)
	}
}

func TestImportRejectsForgedFrozenInputsAndEvidenceBeforeIntent(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *shadowFixture)
		want   string
	}{
		{
			name: "review input ref",
			mutate: func(_ *testing.T, fixture *shadowFixture) {
				fixture.plan.ReviewInputRef.Ref.SizeBytes++
			},
			want: "exact plan review input",
		},
		{
			name: "target digest",
			mutate: func(_ *testing.T, fixture *shadowFixture) {
				forged := strings.Repeat("b", 64)
				fixture.plan.TargetDigest = forged
				fixture.set.TargetDigest = forged
				fixture.raw.TargetDigest = forged
			},
			want: "target_digest",
		},
		{
			name: "anchor path",
			mutate: func(_ *testing.T, fixture *shadowFixture) {
				fixture.set.Hypotheses[0].Anchor.Path = "other.go"
			},
			want: "frozen file content",
		},
		{
			name: "raw candidate claim",
			mutate: func(_ *testing.T, fixture *shadowFixture) {
				fixture.raw.RawCandidates[0].Claim.Title = "tampered raw claim"
			},
			want: "claim_digest",
		},
		{
			name: "raw normalization fate",
			mutate: func(_ *testing.T, fixture *shadowFixture) {
				fixture.raw.RawCandidates[0].Action =
					contractsv1alpha1.HypothesisNormalizationRejectedInvalid
				fixture.raw.RawCandidates[0].ReasonCode = "invalid_candidate"
			},
			want: "normalization decision",
		},
		{
			name: "source digest",
			mutate: func(_ *testing.T, fixture *shadowFixture) {
				fixture.set.Hypotheses[0].Anchor.SourceDigest = strings.Repeat("b", 64)
			},
			want: "source_digest",
		},
		{
			name: "excerpt",
			mutate: func(t *testing.T, fixture *shadowFixture) {
				fixture.set.Hypotheses[0].Evidence[0].Excerpt = "forged.Excerpt()"
				recomputeEvidenceDigest(t, &fixture.set.Hypotheses[0].Evidence[0])
			},
			want: "frozen source range",
		},
		{
			name: "old side",
			mutate: func(t *testing.T, fixture *shadowFixture) {
				fixture.set.Hypotheses[0].Anchor.Side = contractsv1alpha1.HypothesisAnchorOld
				fixture.set.Hypotheses[0].Evidence[0].Anchor.Side =
					contractsv1alpha1.HypothesisAnchorOld
				recomputeEvidenceDigest(t, &fixture.set.Hypotheses[0].Evidence[0])
			},
			want: "old-side source is not trusted",
		},
		{
			name: "nonexistent committed source run",
			mutate: func(_ *testing.T, fixture *shadowFixture) {
				missing := "missing-source-run"
				fixture.plan.SourceRunID = missing
				fixture.set.SourceRunID = missing
				fixture.raw.SourceRunID = missing
				fixture.collection.SourceRunID = missing
				for index := range fixture.collection.Receipts {
					fixture.collection.Receipts[index].SourceRunID = missing
				}
			},
			want: "load committed source run",
		},
		{
			name: "mismatched committed snapshot",
			mutate: func(t *testing.T, fixture *shadowFixture) {
				forged := fixture.snapshot
				forged.BuildIdentity += "-forged"
				ref, err := fixture.runs.PutJSONArtifact(runmodel.SnapshotSchemaVersion, forged)
				if err != nil {
					t.Fatal(err)
				}
				fixture.plan.ExecutionSnapshotRef = bindingFromRunArtifact(ref)
			},
			want: "does not exactly match the committed snapshot",
		},
		{
			name: "mismatched committed target",
			mutate: func(t *testing.T, fixture *shadowFixture) {
				forged := fixture.snapshot
				forged.TargetSnapshotRef = fixture.snapshot.ReviewInputRef
				forged.TargetSnapshotRef.Contract = runmodel.ContractMaterializedTarget
				ref, err := fixture.runs.PutJSONArtifact(runmodel.SnapshotSchemaVersion, forged)
				if err != nil {
					t.Fatal(err)
				}
				fixture.plan.ExecutionSnapshotRef = bindingFromRunArtifact(ref)
			},
			want: "target_snapshot_ref does not match",
		},
		{
			name: "unknown execution snapshot field",
			mutate: func(t *testing.T, fixture *shadowFixture) {
				data, err := json.Marshal(fixture.snapshot)
				if err != nil {
					t.Fatal(err)
				}
				data = append(data[:len(data)-1], []byte(`,"unknown":true}`)...)
				ref, err := fixture.runs.PutArtifact(runmodel.SnapshotSchemaVersion, data)
				if err != nil {
					t.Fatal(err)
				}
				fixture.plan.ExecutionSnapshotRef = bindingFromRunArtifact(ref)
			},
			want: "unknown field",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newShadowFixture(t)
			test.mutate(t, fixture)
			request := fixture.request(t, "reject-"+strings.ReplaceAll(test.name, " ", "-"))
			if _, err := fixture.service.Import(t.Context(), request); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("Import() error = %v, want %q", err, test.want)
			}
			var intent importIntent
			if err := fixture.store.GetJSON(
				intentObjectID(fixture.scope, request.IdempotencyKey),
				&intent,
			); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected import persisted intent: %+v, %v", intent, err)
			}
		})
	}
}

func TestEvidenceMayUseFrozenContextOutsideTargetButFindingAnchorMayNot(t *testing.T) {
	t.Run("frozen file evidence outside selection", func(t *testing.T) {
		fixture := newShadowFixture(t)
		evidence := &fixture.set.Hypotheses[0].Evidence[0]
		evidence.Anchor.StartLine = 1
		evidence.Anchor.EndLine = 1
		evidence.Excerpt = "package demo"
		recomputeEvidenceDigest(t, evidence)

		if _, err := fixture.service.Import(
			t.Context(),
			fixture.request(t, "context-evidence"),
		); err != nil {
			t.Fatalf("Import() rejected frozen context evidence: %v", err)
		}
	})

	t.Run("hypothesis anchor outside selection", func(t *testing.T) {
		fixture := newShadowFixture(t)
		fixture.set.Hypotheses[0].Anchor.StartLine = 1
		fixture.set.Hypotheses[0].Anchor.EndLine = 1

		if _, err := fixture.service.Import(
			t.Context(),
			fixture.request(t, "out-of-target-hypothesis"),
		); err == nil || !strings.Contains(err.Error(), "authorized frozen target") {
			t.Fatalf("Import() error = %v, want authorized target rejection", err)
		}
	})
}

func TestImportRejectsFutureWorkerTimestampBeforeIntent(t *testing.T) {
	fixture := newShadowFixture(t)
	future := (*fixture.now).Add(time.Minute)
	fixture.collection.Receipts[0].StartedAt = future
	fixture.collection.Receipts[0].FinishedAt = future.Add(time.Second)
	request := fixture.request(t, "future-worker-time")

	if _, err := fixture.service.Import(t.Context(), request); err == nil ||
		!strings.Contains(err.Error(), "timing is outside") {
		t.Fatalf("Import() error = %v, want future worker timestamp rejection", err)
	}
	var intent importIntent
	if err := fixture.store.GetJSON(
		intentObjectID(fixture.scope, request.IdempotencyKey),
		&intent,
	); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("future worker timestamp persisted intent: %+v, %v", intent, err)
	}
}

func TestQueryDetectsTamperedArtifactAndQuarantinesIt(t *testing.T) {
	fixture := newShadowFixture(t)
	result, err := fixture.service.Import(
		t.Context(),
		fixture.request(t, "tamper-1"),
	)
	if err != nil {
		t.Fatal(err)
	}
	ref := result.Record.HypothesisSetRef
	path := filepath.Join(
		fixture.store.Root(),
		"artifacts",
		"sha256",
		ref.Ref.SHA256[:2],
		ref.Ref.SHA256,
	)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Query(t.Context(), QueryRequest{
		Scope: fixture.scope, ManifestID: result.Manifest.ManifestID,
	}); !errors.Is(err, artifactrepo.ErrQuarantined) {
		t.Fatalf("Query() tamper error = %v, want ErrQuarantined", err)
	}
	record, err := fixture.repository.artifacts.Inspect(
		t.Context(),
		artifactSubject(fixture.scope),
		artifactRef(ref),
	)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != artifactrepo.StateQuarantined {
		t.Fatalf("tampered artifact state = %q", record.State)
	}
}

func TestConcurrentImportHasOneCommitAndObservation(t *testing.T) {
	fixture := newShadowFixture(t)
	request := fixture.request(t, "concurrent-1")
	const workers = 4
	results := make(chan Result, workers)
	errorsChannel := make(chan error, workers)
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := fixture.service.Import(context.Background(), request)
			if err != nil {
				errorsChannel <- err
				return
			}
			results <- result
		}()
	}
	wait.Wait()
	close(results)
	close(errorsChannel)
	for err := range errorsChannel {
		t.Errorf("concurrent Import() error = %v", err)
	}
	var manifestID string
	var observationStream string
	count := 0
	for result := range results {
		count++
		if manifestID == "" {
			manifestID = result.Manifest.ManifestID
			observationStream = result.Record.ObservationStream
		}
		if result.Manifest.ManifestID != manifestID {
			t.Fatalf("concurrent manifests differ: %q != %q", result.Manifest.ManifestID, manifestID)
		}
	}
	if count != workers {
		t.Fatalf("successful imports = %d, want %d", count, workers)
	}
	envelopes, err := fixture.store.ReadJSONL(observationStream)
	if err != nil || len(envelopes) != 1 {
		t.Fatalf("observation envelopes = %d, %v", len(envelopes), err)
	}
}

func TestUnavailableUsageRemainsUnknownWithDiagnosticReason(t *testing.T) {
	fixture := newShadowFixture(t)
	reason := "provider_usage_unavailable"
	for index := range fixture.collection.Receipts {
		fixture.collection.Receipts[index].Usage = contractsv1alpha1.AgentTokenUsage{
			Completeness:          contractsv1alpha1.AgentTokenUsageUnavailable,
			UnavailableReasonCode: &reason,
		}
	}
	result, err := fixture.service.Import(
		t.Context(),
		fixture.request(t, "usage-unavailable"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Manifest.Summary.UsageCompleteness !=
		contractsv1alpha1.AgentTokenUsageUnavailable ||
		result.Manifest.Summary.UsageReportedReceipts != 0 ||
		result.Manifest.Summary.InputTokens != 0 ||
		!reflect.DeepEqual(result.Observation.ReasonCodes, []string{reason}) {
		t.Fatalf("unavailable usage was fabricated or lost: %+v", result)
	}
}

func TestPartialUsageIsAggregatedAndDiagnosticReasonPersists(t *testing.T) {
	fixture := newShadowFixture(t)
	reason := "provider_omitted_cache_usage"
	fixture.collection.Receipts[1].Usage.Completeness =
		contractsv1alpha1.AgentTokenUsagePartial
	fixture.collection.Receipts[1].Usage.UnavailableReasonCode = &reason

	result, err := fixture.service.Import(
		t.Context(),
		fixture.request(t, "usage-partial"),
	)
	if err != nil {
		t.Fatal(err)
	}
	summary := result.Manifest.Summary
	if result.ReceiptCollection.Receipts[1].Usage.Completeness !=
		contractsv1alpha1.AgentTokenUsagePartial ||
		summary.UsageCompleteness != contractsv1alpha1.AgentTokenUsagePartial ||
		summary.UsagePartialReceipts != 1 ||
		summary.UsageReportedReceipts != 2 ||
		summary.UsageUnavailableReceipts != 0 ||
		result.Observation.Summary.UsagePartialReceipts != 1 ||
		!reflect.DeepEqual(result.Observation.ReasonCodes, []string{reason}) {
		t.Fatalf("partial usage was not preserved and aggregated: %+v", result)
	}
	queried, err := fixture.service.Query(t.Context(), QueryRequest{
		Scope: fixture.scope, ManifestID: result.Manifest.ManifestID,
	})
	if err != nil || !reflect.DeepEqual(queried, result) {
		t.Fatalf("Query() partial result = %+v, %v", queried, err)
	}
}

func newShadowFixture(t *testing.T) *shadowFixture {
	t.Helper()
	store, err := local.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runrepo.New(store)
	if err != nil {
		t.Fatal(err)
	}
	repositoryPath, revision, content := committedSelectionRepository(t)
	source, err := gitadapter.New()
	if err != nil {
		t.Fatal(err)
	}
	sourceClock := &shadowFixtureClock{
		value: time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC),
	}
	sourceService, err := application.NewService(source, runs, application.ServiceOptions{
		IDs:               &shadowFixtureIDs{},
		Now:               sourceClock.Now,
		DisableScheduling: true,
		BuildIdentity:     "agent-shadow-fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	sourceOutcome, err := sourceService.Review(t.Context(), application.ReviewRequest{
		RepositoryPath: repositoryPath,
		Mode:           reviewcore.TargetModeSelection,
		Revision:       revision,
		SelectionPath:  "internal/review.go",
		StartLine:      4,
		EndLine:        4,
	})
	if err != nil {
		t.Fatalf("create committed source review: %v", err)
	}
	if sourceOutcome.Run.Status != runmodel.RunStatusSucceeded {
		t.Fatalf("source review status = %q", sourceOutcome.Run.Status)
	}
	if _, err := runs.LoadRun(sourceOutcome.Run.RunID); err != nil {
		t.Fatalf("load committed source review: %v", err)
	}
	snapshot, err := runs.LoadExecutionSnapshot(sourceOutcome.Run.ExecutionSnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	inputData, err := runs.ReadArtifact(snapshot.ReviewInputRef)
	if err != nil {
		t.Fatal(err)
	}
	input, err := reviewcore.DecodeReviewInput(inputData)
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Files) != 1 || input.Files[0].Path != "internal/review.go" ||
		input.Files[0].Content == nil || *input.Files[0].Content != content {
		t.Fatalf("source ReviewInput did not freeze the selection file: %+v", input)
	}
	fileDigest := input.Files[0].SHA256
	targetDigest, err := reviewcore.DigestReviewInput(input)
	if err != nil {
		t.Fatal(err)
	}
	snapshotRef, err := runs.PutJSONArtifact(runmodel.SnapshotSchemaVersion, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	acceptedAt := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	repository, err := OpenRepository(store, func() time.Time { return acceptedAt })
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(repository)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{TenantID: "local", WorkspaceID: "local"}
	versioned := func(id string) contractsv1alpha1.VersionedRef {
		return contractsv1alpha1.VersionedRef{
			ID: id, Revision: "1", SHA256: shadowTestDigest,
		}
	}
	plan := contractsv1alpha1.AgentReviewPlan{
		SchemaVersion: contractsv1alpha1.AgentReviewPlanSchemaVersion,
		PlanID:        "plan-1", SourceRunID: sourceOutcome.Run.RunID,
		ExecutionID: "execution-1", ReviewRunID: "review-run-1",
		ExecutionSnapshotRef: bindingFromRunArtifact(snapshotRef),
		ReviewInputRef:       bindingFromRunArtifact(snapshot.ReviewInputRef),
		TargetDigest:         targetDigest,
		RulePack:             versioned("review-rules"),
		Implementation:       versioned("pi-review"), Grouping: versioned("path-grouping"),
		Normalization: contractsv1alpha1.VersionedRef{
			ID:       contractsv1alpha1.CandidateNormalizationImplementationID,
			Revision: contractsv1alpha1.CandidateNormalizationCurrentRevision,
			SHA256:   shadowTestDigest,
		},
		ContextDimensions: []contractsv1alpha1.VersionedRef{versioned("call-context")},
		ReviewDimensions:  []contractsv1alpha1.VersionedRef{versioned("correctness")},
		Verifier:          versioned("independent-verifier"),
		Knowledge:         []contractsv1alpha1.VersionedRef{},
		Runtime:           versioned("node-runtime"),
		Profile:           versioned("local-shadow"),
		Agent:             versioned("pi-agent"),
		Provider:          versioned("deepseek-anthropic-env"), Model: versioned("deepseek-v4-pro-1m"),
		APIProtocol: contractsv1alpha1.AgentAPIProtocolAnthropicMessages,
		ToolPolicy: contractsv1alpha1.AgentReviewToolPolicy{
			AllowedTools: []string{"list_files", "read_file", "search_code"},
			ToolNetwork:  "deny", WorkspaceWrites: "deny", RemoteWrites: "deny",
		},
		Budget: contractsv1alpha1.AgentReviewBudget{
			MaxFiles: 10, MaxGroups: 10, MaxCandidates: 10, MaxModelCalls: 20,
			MaxToolCalls: 5, MaxOutputTokens: 4096, MaxGroupBytes: 100000,
			MaxTargetBytes: 200000, TimeoutMS: 60000, MaxConcurrency: 2,
		},
		VerificationRequired: true,
		ExecutionClass:       contractsv1alpha1.AgentReviewExecutionLocalDirectProviderShadow,
		Attestation:          contractsv1alpha1.AgentReviewAttestationNonAttested,
		SideEffects:          "deny",
		CreatedAt:            time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC),
	}
	anchor := contractsv1alpha1.HypothesisSourceAnchor{
		Path: "internal/review.go", Side: contractsv1alpha1.HypothesisAnchorNew,
		StartLine: 4, EndLine: 4, SourceDigest: fileDigest,
	}
	evidence := contractsv1alpha1.HypothesisEvidence{
		EvidenceID: "evidence-1", Statement: "value reaches this dereference",
		Anchor: anchor, Excerpt: "value.Field()",
	}
	recomputeEvidenceDigest(t, &evidence)
	occurrenceID := "occurrence-1"
	set := contractsv1alpha1.ReviewHypothesisSet{
		SchemaVersion:   contractsv1alpha1.ReviewHypothesisSetSchemaVersion,
		HypothesisSetID: "hypothesis-set-1", PlanID: plan.PlanID,
		SourceRunID: plan.SourceRunID, ExecutionID: plan.ExecutionID,
		ReviewRunID: plan.ReviewRunID, TargetDigest: targetDigest,
		Completeness:        contractsv1alpha1.AgentReviewComplete,
		CompletenessReasons: []string{},
		NormalizationDecisions: []contractsv1alpha1.HypothesisNormalizationDecision{
			{
				RawCandidateID: "raw-candidate-1", ClaimDigest: shadowTestDigest,
				Action:     contractsv1alpha1.HypothesisNormalizationRetained,
				ReasonCode: "normalized_candidate_retained", OccurrenceID: &occurrenceID,
			},
		},
		Hypotheses: []contractsv1alpha1.ReviewHypothesis{
			{
				OccurrenceID: occurrenceID, ClusterFingerprint: shadowTestDigest,
				GroupID: "group-1", Dimension: plan.ReviewDimensions[0],
				Category: "correctness", Severity: contractsv1alpha1.HypothesisSeverityHigh,
				Title: "possible nil dereference", Description: "value may be nil",
				Impact: "request panic", Anchor: anchor,
				Evidence: []contractsv1alpha1.HypothesisEvidence{evidence},
				Verification: []contractsv1alpha1.HypothesisVerificationObservation{
					{
						ObservationID: "verification-1", Sequence: 1,
						Verifier:    plan.Verifier,
						Verdict:     contractsv1alpha1.HypothesisVerificationConfirmed,
						ReasonCode:  "path_reachable",
						Explanation: "the path reaches the dereference",
						EvidenceIDs: []string{"evidence-1"},
					},
				},
			},
		},
		DedupClusters: []contractsv1alpha1.HypothesisDedupCluster{
			{
				ClusterID: "cluster-1", Fingerprint: shadowTestDigest,
				CanonicalOccurrenceID: occurrenceID, OccurrenceIDs: []string{occurrenceID},
			},
		},
		Coverage: contractsv1alpha1.AgentReviewCoverage{
			GroupsTotal: 1, GroupsReviewed: 1, ReviewTasksTotal: 1,
			ReviewTasksSucceeded: 1, FilesIncluded: 1,
			Gaps: []contractsv1alpha1.AgentReviewCoverageGap{},
		},
		GeneratedAt: time.Date(2026, 8, 20, 10, 5, 0, 0, time.UTC),
	}
	rawClaim := contractsv1alpha1.AgentReviewRawCandidateClaim{
		Category: "correctness", Severity: contractsv1alpha1.HypothesisSeverityHigh,
		Title: "possible nil dereference", Description: "value may be nil",
		Impact: "request panic",
		Anchor: contractsv1alpha1.AgentReviewRawCandidateSourceAnchor{
			Path: "internal/review.go", Side: contractsv1alpha1.HypothesisAnchorNew,
			StartLine: 4, EndLine: 4,
		},
		Evidence: []contractsv1alpha1.AgentReviewRawCandidateEvidence{{
			Statement: "value reaches this dereference",
			Anchor: contractsv1alpha1.AgentReviewRawCandidateSourceAnchor{
				Path: "internal/review.go", Side: contractsv1alpha1.HypothesisAnchorNew,
				StartLine: 4, EndLine: 4,
			},
			Excerpt: "value.Field()",
		}},
	}
	rawDigest, err := contractsv1alpha1.DigestAgentReviewRawCandidateClaim(rawClaim)
	if err != nil {
		t.Fatal(err)
	}
	set.NormalizationDecisions[0].ClaimDigest = rawDigest
	raw := contractsv1alpha1.AgentReviewRawCandidateCollection{
		SchemaVersion: contractsv1alpha1.AgentReviewRawCandidateCollectionSchemaVersion,
		PlanID:        plan.PlanID, SourceRunID: plan.SourceRunID,
		ExecutionID: plan.ExecutionID, ReviewRunID: plan.ReviewRunID,
		TargetDigest: plan.TargetDigest,
		Authority:    contractsv1alpha1.AgentReviewRawCandidateAuthorityWorkerSelfReport,
		Disposition:  contractsv1alpha1.AgentReviewRawCandidateDispositionShadowOnly,
		RawCandidates: []contractsv1alpha1.AgentReviewRawCandidatePayload{{
			RawCandidateID: "raw-candidate-1", GroupID: "group-1",
			Dimension: plan.ReviewDimensions[0], ClaimDigest: rawDigest,
			Action:     contractsv1alpha1.HypothesisNormalizationRetained,
			ReasonCode: "normalized_candidate_retained", Claim: rawClaim,
		}},
	}
	contextReceipt := fixtureReceipt(plan, "receipt-context", "task-context", "group-1")
	contextReceipt.TaskRole = contractsv1alpha1.AgentTaskContext
	contextReceipt.Dimension = plan.ContextDimensions[0]
	contextReceipt.ToolCalls++
	contextReceipt.ToolUsage = append(contextReceipt.ToolUsage, contractsv1alpha1.AgentToolUsage{
		ToolID: "submit_context", InvocationCount: 1,
	})
	reviewReceipt := fixtureReceipt(plan, "receipt-review", "task-review", "group-1")
	reviewReceipt.TaskRole = contractsv1alpha1.AgentTaskReview
	reviewReceipt.Dimension = plan.ReviewDimensions[0]
	reviewReceipt.ToolCalls++
	reviewReceipt.ToolUsage = append(reviewReceipt.ToolUsage, contractsv1alpha1.AgentToolUsage{
		ToolID: "submit_candidates", InvocationCount: 1,
	})
	verificationReceipt := fixtureReceipt(
		plan,
		"receipt-verification",
		"task-verification",
		"group-1",
	)
	verificationReceipt.TaskRole = contractsv1alpha1.AgentTaskVerification
	verificationReceipt.Dimension = plan.Verifier
	verificationReceipt.HypothesisOccurrenceID = &occurrenceID
	verificationReceipt.ToolCalls++
	verificationReceipt.ToolUsage = append(
		verificationReceipt.ToolUsage,
		contractsv1alpha1.AgentToolUsage{ToolID: "submit_verdict", InvocationCount: 1},
	)
	collection := contractsv1alpha1.AgentExecutionReceiptCollection{
		SchemaVersion: contractsv1alpha1.AgentExecutionReceiptCollectionSchemaVersion,
		PlanID:        plan.PlanID, SourceRunID: plan.SourceRunID,
		ExecutionID: plan.ExecutionID, ReviewRunID: plan.ReviewRunID,
		Receipts: []contractsv1alpha1.AgentExecutionReceipt{
			contextReceipt,
			reviewReceipt,
			verificationReceipt,
		},
	}
	taskEvidence := contractsv1alpha1.AgentReviewTaskEvidenceCollection{
		SchemaVersion: contractsv1alpha1.AgentReviewTaskEvidenceCollectionSchemaVersion,
		PlanID:        plan.PlanID, SourceRunID: plan.SourceRunID,
		ExecutionID: plan.ExecutionID, ReviewRunID: plan.ReviewRunID,
		TargetDigest:   plan.TargetDigest,
		Authority:      contractsv1alpha1.AgentReviewTaskEvidenceAuthority,
		Provenance:     contractsv1alpha1.AgentReviewTaskEvidenceProvenance,
		Disposition:    contractsv1alpha1.AgentReviewTaskEvidenceDisposition,
		ContentPolicy:  contractsv1alpha1.AgentReviewTaskEvidenceContentPolicy,
		Completeness:   contractsv1alpha1.AgentReviewTaskEvidencePartial,
		ReasonCodes:    []string{contractsv1alpha1.AgentReviewTaskEvidenceUnavailable},
		TaskExecutions: []contractsv1alpha1.AgentReviewTaskExecutionEvidence{},
	}
	if err := plan.Validate(); err != nil {
		t.Fatalf("fixture plan: %v", err)
	}
	if err := set.Validate(); err != nil {
		t.Fatalf("fixture set: %v", err)
	}
	if err := raw.Validate(); err != nil {
		t.Fatalf("fixture raw candidates: %v", err)
	}
	if err := collection.Validate(); err != nil {
		t.Fatalf("fixture collection: %v", err)
	}
	if err := contractsv1alpha1.ValidateAgentReviewTaskEvidenceBindings(taskEvidence, plan, collection); err != nil {
		t.Fatalf("fixture task evidence: %v", err)
	}
	return &shadowFixture{
		store: store, runs: runs, repository: repository, service: service,
		now: &acceptedAt, scope: scope, plan: plan, set: set, raw: raw, taskEvidence: taskEvidence,
		collection: collection, snapshot: snapshot,
	}
}

func fixtureReceipt(
	plan contractsv1alpha1.AgentReviewPlan,
	receiptID string,
	taskID string,
	groupID string,
) contractsv1alpha1.AgentExecutionReceipt {
	reasoning := uint64(10)
	return contractsv1alpha1.AgentExecutionReceipt{
		SchemaVersion: contractsv1alpha1.AgentExecutionReceiptSchemaVersion,
		ReceiptID:     receiptID, PlanID: plan.PlanID, SourceRunID: plan.SourceRunID,
		ExecutionID: plan.ExecutionID, ReviewRunID: plan.ReviewRunID,
		TaskID: taskID, GroupID: groupID,
		Runtime: plan.Runtime,
		Profile: plan.Profile,
		Agent:   plan.Agent, Provider: plan.Provider, Model: plan.Model,
		APIProtocol:       plan.APIProtocol,
		ProvenanceClass:   contractsv1alpha1.AgentReceiptProvenanceWorkerSelfReport,
		Authority:         contractsv1alpha1.AgentReceiptAuthorityDiagnosticOnly,
		Status:            contractsv1alpha1.AgentTaskSucceeded,
		StartedAt:         time.Date(2026, 8, 20, 10, 1, 0, 0, time.UTC),
		FinishedAt:        time.Date(2026, 8, 20, 10, 2, 0, 0, time.UTC),
		ModelTurnsStarted: 1, ModelTurnsCompleted: 1, ToolCalls: 1,
		PromptDigest: &[]string{shadowTestDigest}[0],
		OutputDigest: &[]string{shadowTestDigest}[0],
		ToolUsage: []contractsv1alpha1.AgentToolUsage{
			{ToolID: "read_file", InvocationCount: 1},
		},
		Usage: contractsv1alpha1.AgentTokenUsage{
			Completeness: contractsv1alpha1.AgentTokenUsageProviderReported,
			InputTokens:  100, OutputTokens: 50, ReasoningTokens: &reasoning,
			TotalTokens: 150,
		},
	}
}

func (fixture *shadowFixture) request(t *testing.T, key string) ImportRequest {
	t.Helper()
	marshal := func(value any) []byte {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	return ImportRequest{
		Scope: fixture.scope, IdempotencyKey: key,
		Plan: marshal(fixture.plan), HypothesisSet: marshal(fixture.set),
		RawCandidateCollection: marshal(fixture.raw),
		TaskEvidenceCollection: marshal(fixture.taskEvidence),
		ReceiptCollection:      marshal(fixture.collection),
	}
}

func recomputeEvidenceDigest(
	t *testing.T,
	evidence *contractsv1alpha1.HypothesisEvidence,
) {
	t.Helper()
	digest, err := contractsv1alpha1.DigestHypothesisEvidence(*evidence)
	if err != nil {
		t.Fatal(err)
	}
	evidence.EvidenceDigest = digest
}

func bindingFromRunArtifact(ref runmodel.ArtifactRef) contractsv1alpha1.ArtifactBinding {
	return bindingFromRunRef(ref)
}

func artifactRef(binding contractsv1alpha1.ArtifactBinding) artifactrepo.Ref {
	return artifactrepo.Ref{
		URI: binding.Ref.URI, SHA256: binding.Ref.SHA256,
		SizeBytes: binding.Ref.SizeBytes, Contract: binding.Contract,
	}
}

type shadowFixtureIDs struct {
	mu     sync.Mutex
	counts map[string]int
}

func (ids *shadowFixtureIDs) New(prefix string) (string, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	if ids.counts == nil {
		ids.counts = make(map[string]int)
	}
	ids.counts[prefix]++
	if prefix == "run" && ids.counts[prefix] == 1 {
		return "source-run-1", nil
	}
	return fmt.Sprintf("%s-shadow-%04d", prefix, ids.counts[prefix]), nil
}

type shadowFixtureClock struct {
	mu    sync.Mutex
	value time.Time
}

func (clock *shadowFixtureClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.value = clock.value.Add(time.Millisecond)
	return clock.value
}

func committedSelectionRepository(t *testing.T) (string, string, string) {
	t.Helper()
	repositoryPath := t.TempDir()
	runShadowGit(t, repositoryPath, "init", "-q", "-b", "main")
	runShadowGit(t, repositoryPath, "config", "user.name", "Argus Test")
	runShadowGit(t, repositoryPath, "config", "user.email", "argus@example.invalid")
	runShadowGit(t, repositoryPath, "config", "commit.gpgsign", "false")
	content := "package demo\n\nfunc run(value interface{ Field() }) {\n    value.Field()\n}\n"
	path := filepath.Join(repositoryPath, "internal", "review.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runShadowGit(t, repositoryPath, "add", "--all")
	runShadowGit(t, repositoryPath, "commit", "-q", "-m", "selection fixture")
	revision := strings.TrimSpace(runShadowGit(t, repositoryPath, "rev-parse", "HEAD"))
	return repositoryPath, revision, content
}

func runShadowGit(t *testing.T, repositoryPath string, arguments ...string) string {
	t.Helper()
	command := exec.CommandContext(t.Context(), "git", arguments...)
	command.Dir = repositoryPath
	command.Env = append(
		os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
	return string(output)
}
