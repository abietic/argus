package application

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/runrepo"
	"argus.local/argus/internal/source/gitadapter"
	"argus.local/argus/internal/store/local"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

type contextProviderExecutorFunc func(
	context.Context,
	ContextProviderExecutionRequest,
) (ContextProviderCapture, error)

func (execute contextProviderExecutorFunc) Execute(
	ctx context.Context,
	request ContextProviderExecutionRequest,
) (ContextProviderCapture, error) {
	return execute(ctx, request)
}

func TestConfiguredContextProvidersRunWithBoundedConcurrencyAndStableResults(t *testing.T) {
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := runrepo.New(store)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan string, 3)
	release := make(chan struct{})
	var active atomic.Int32
	var maximum atomic.Int32
	executor := contextProviderExecutorFunc(func(
		ctx context.Context,
		request ContextProviderExecutionRequest,
	) (ContextProviderCapture, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			observed := maximum.Load()
			if current <= observed || maximum.CompareAndSwap(observed, current) {
				break
			}
		}
		started <- request.Definition.ID
		select {
		case <-release:
		case <-ctx.Done():
			return ContextProviderCapture{}, ctx.Err()
		}
		return ContextProviderCapture{
			Contract: "argus.context.fixture.v1alpha1",
			Content:  []byte(`{"facts":[]}`),
			Revision: request.CommitOID,
			Coverage: reviewcore.ContextCoverage{
				Spans: []reviewcore.ContextSpan{{
					Path: "target.go", StartLine: 1, EndLine: 1,
				}},
				Symbols: []string{},
			},
		}, nil
	})
	now := time.Date(2026, time.August, 25, 3, 0, 0, 0, time.UTC)
	service := &Service{
		repository: repository, contextProviders: executor,
		now: func() time.Time {
			now = now.Add(time.Millisecond)
			return now
		},
	}
	requests := make([]ContextProviderExecutionRequest, 3)
	for index := range requests {
		requests[index] = testContextProviderExecutionRequest(
			fmt.Sprintf("provider-%d", index+1),
		)
	}
	resultChannel := make(chan struct {
		results []configuredContextProviderResult
		err     error
	}, 1)
	go func() {
		results, executeErr := service.executeConfiguredContextProviders(
			context.Background(), requests, 5_000, 2, false,
		)
		resultChannel <- struct {
			results []configuredContextProviderResult
			err     error
		}{results: results, err: executeErr}
	}()
	for range 2 {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("two providers did not overlap")
		}
	}
	select {
	case third := <-started:
		t.Fatalf("provider %q exceeded concurrency bound before release", third)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	outcome := <-resultChannel
	if outcome.err != nil {
		t.Fatal(outcome.err)
	}
	if maximum.Load() != 2 || len(outcome.results) != len(requests) {
		t.Fatalf("maximum=%d results=%d", maximum.Load(), len(outcome.results))
	}
	for index, result := range outcome.results {
		if result.binding.Ref == nil ||
			!strings.HasPrefix(result.binding.Ref.ContextID, requests[index].Definition.ID+"-") {
			t.Fatalf("result %d lost config order: %+v", index, result)
		}
		data, err := repository.ReadArtifact(result.receiptRef)
		if err != nil {
			t.Fatal(err)
		}
		receipt, err := contractsv1alpha1.DecodeContextProviderExecutionReceipt(data)
		if err != nil {
			t.Fatal(err)
		}
		if receipt.ProviderID != requests[index].Definition.ID {
			t.Fatalf("receipt %d provider = %q", index, receipt.ProviderID)
		}
	}
}

func TestConfiguredContextProvidersPropagateParentCancellation(t *testing.T) {
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := runrepo.New(store)
	if err != nil {
		t.Fatal(err)
	}
	executor := contextProviderExecutorFunc(func(
		ctx context.Context,
		_ ContextProviderExecutionRequest,
	) (ContextProviderCapture, error) {
		<-ctx.Done()
		return ContextProviderCapture{}, ctx.Err()
	})
	service := &Service{repository: repository, contextProviders: executor, now: time.Now}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = service.executeConfiguredContextProviders(
		ctx,
		[]ContextProviderExecutionRequest{testContextProviderExecutionRequest("provider-1")},
		5_000,
		1,
		false,
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled provider batch error = %v", err)
	}
}

func TestConfiguredContextProviderPersistsExactRevisionMismatchEvidence(t *testing.T) {
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := runrepo.New(store)
	if err != nil {
		t.Fatal(err)
	}
	request := testContextProviderExecutionRequest("provider-1")
	observedRevision := "2222222222222222222222222222222222222222"
	executor := contextProviderExecutorFunc(func(
		context.Context,
		ContextProviderExecutionRequest,
	) (ContextProviderCapture, error) {
		return ContextProviderCapture{
			Contract: "argus.context.fixture.v1alpha1",
			Content:  []byte(`{"facts":[]}`),
			Revision: observedRevision,
			Coverage: reviewcore.ContextCoverage{
				Spans:   []reviewcore.ContextSpan{{Path: "target.go", StartLine: 1, EndLine: 1}},
				Symbols: []string{},
			},
		}, nil
	})
	now := time.Date(2026, time.August, 25, 3, 0, 0, 0, time.UTC)
	service := &Service{
		repository:       repository,
		contextProviders: executor,
		now: func() time.Time {
			now = now.Add(time.Millisecond)
			return now
		},
	}
	result := service.executeConfiguredContextProviderWithReceipt(
		context.Background(), request, 5_000, false,
	)
	if result.err != nil {
		t.Fatal(result.err)
	}
	if result.binding.Gap == nil ||
		result.binding.Gap.ReasonCode != contractsv1alpha1.ContextProviderRevisionMismatch ||
		result.observedRevision != observedRevision {
		t.Fatalf("revision mismatch result = %+v", result)
	}
	data, err := repository.ReadArtifact(result.receiptRef)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := contractsv1alpha1.DecodeContextProviderExecutionReceipt(data)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.CommitOID != request.CommitOID ||
		receipt.ObservedRevision != observedRevision ||
		receipt.ReasonCode != contractsv1alpha1.ContextProviderRevisionMismatch ||
		receipt.ContextContract != "" {
		t.Fatalf("revision mismatch receipt = %+v", receipt)
	}
}

func testContextProviderExecutionRequest(id string) ContextProviderExecutionRequest {
	return ContextProviderExecutionRequest{
		Definition: reviewconfig.ContextProviderDefinition{
			ID: id, Revision: "1", Kind: "go_ast",
			Adapter: reviewconfig.VersionedRef{
				ID: "adapter", Revision: "1",
				SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			},
		},
		RepositoryRoot: "/repo", RepositoryID: "repository",
		CommitOID:   "1111111111111111111111111111111111111111",
		TargetPaths: []string{"target.go"}, PolicyInclude: []string{"**"},
		PolicyExclude: []string{}, MaxFiles: 10, MaxFileBytes: 1024,
		MaxArtifactBytes: 4096,
	}
}

func TestConfiguredContextProviderProjectsSuccessAndTypedGaps(t *testing.T) {
	request := ContextProviderExecutionRequest{
		Definition: reviewconfig.ContextProviderDefinition{
			ID: "go-ast-exact", Revision: "1", Kind: "go_ast",
			Adapter: reviewconfig.VersionedRef{
				ID: "argus-go-ast", Revision: "2",
				SHA256: "d89854750360b6c97fcfbdcb4d1b8f22c0a63dd616efc7d95b5f2606ca937ac9",
			},
		},
		RepositoryRoot: "/repo", RepositoryID: "local-repository",
		CommitOID:   "1111111111111111111111111111111111111111",
		TargetPaths: []string{"target.go"}, PolicyInclude: []string{"**"},
		PolicyExclude: []string{}, MaxFiles: 10, MaxFileBytes: 1024,
		MaxArtifactBytes: 4096,
	}
	newService := func(t *testing.T, executor ContextProviderExecutor) *Service {
		t.Helper()
		store, err := local.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		repository, err := runrepo.New(store)
		if err != nil {
			t.Fatal(err)
		}
		return &Service{repository: repository, contextProviders: executor}
	}
	tests := []struct {
		name      string
		executor  ContextProviderExecutor
		timeoutMS int64
		wantGap   string
		wantRef   bool
	}{
		{name: "missing executor", timeoutMS: 100, wantGap: "provider_executor_unavailable"},
		{
			name: "classified failure", timeoutMS: 100, wantGap: "adapter_identity_mismatch",
			executor: contextProviderExecutorFunc(func(context.Context, ContextProviderExecutionRequest) (ContextProviderCapture, error) {
				return ContextProviderCapture{}, &ContextProviderError{Code: "adapter_identity_mismatch", Err: errors.New("drift")}
			}),
		},
		{
			name: "timeout", timeoutMS: 1, wantGap: "provider_timeout",
			executor: contextProviderExecutorFunc(func(ctx context.Context, _ ContextProviderExecutionRequest) (ContextProviderCapture, error) {
				<-ctx.Done()
				return ContextProviderCapture{}, ctx.Err()
			}),
		},
		{
			name: "invalid output", timeoutMS: 100, wantGap: "provider_output_invalid",
			executor: contextProviderExecutorFunc(func(context.Context, ContextProviderExecutionRequest) (ContextProviderCapture, error) {
				return ContextProviderCapture{Contract: "argus.context.invalid.v1alpha1", Content: nil}, nil
			}),
		},
		{
			name: "success", timeoutMS: 100, wantRef: true,
			executor: contextProviderExecutorFunc(func(_ context.Context, request ContextProviderExecutionRequest) (ContextProviderCapture, error) {
				return ContextProviderCapture{
					Contract: "argus.context.fixture.v1alpha1", Content: []byte(`{"facts":[]}`),
					Revision: request.CommitOID,
					Coverage: reviewcore.ContextCoverage{
						Spans:   []reviewcore.ContextSpan{{Path: "target.go", StartLine: 1, EndLine: 1}},
						Symbols: []string{},
					},
				}, nil
			}),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := newService(t, test.executor)
			binding, err := service.executeConfiguredContextProvider(
				context.Background(), request, test.timeoutMS, false,
			)
			if err != nil {
				t.Fatal(err)
			}
			if test.wantRef {
				if binding.Ref == nil || binding.Gap != nil || binding.Ref.Revision != request.CommitOID {
					t.Fatalf("success binding = %+v", binding)
				}
				return
			}
			if binding.Gap == nil || binding.Ref != nil || binding.Gap.ReasonCode != test.wantGap {
				t.Fatalf("gap binding = %+v, want %s", binding, test.wantGap)
			}
		})
	}
}

func TestConfiguredContextProviderGapIdentityIsExactIdempotent(t *testing.T) {
	request := ContextProviderExecutionRequest{
		Definition: reviewconfig.ContextProviderDefinition{
			ID: "provider", Revision: "1", Kind: "go_ast",
			Adapter: reviewconfig.VersionedRef{ID: "adapter", Revision: "1", SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		},
		RepositoryID: "repository", CommitOID: "1111111111111111111111111111111111111111",
		TargetPaths: []string{"target.go"}, PolicyInclude: []string{"**"}, PolicyExclude: []string{},
		MaxFiles: 1, MaxFileBytes: 1, MaxArtifactBytes: 1,
	}
	digest, err := digestContextProviderRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	provenance := reviewcore.ContextProvenance{Provider: "go_ast", ProducerID: "adapter", ProducerRevision: "1"}
	coverage := reviewcore.ContextCoverage{Spans: []reviewcore.ContextSpan{}, Symbols: []string{"provider:provider"}}
	first, err := contextProviderGap(request, digest, provenance, coverage, "provider_failed")
	if err != nil {
		t.Fatal(err)
	}
	second, err := contextProviderGap(request, digest, provenance, coverage, "provider_failed")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Gap, second.Gap) {
		t.Fatalf("same provider request changed gap identity: %+v != %+v", first, second)
	}
	request.CommitOID = "2222222222222222222222222222222222222222"
	changed, err := digestContextProviderRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if changed == digest {
		t.Fatal("commit substitution did not change provider request digest")
	}
	request.CommitOID = "1111111111111111111111111111111111111111"
	request.TargetRanges = []ContextProviderTargetRange{{Path: "target.go", StartLine: 10, EndLine: 12}}
	ranged, err := digestContextProviderRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	request.TargetRanges[0].EndLine = 13
	changedRange, err := digestContextProviderRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if ranged == changedRange {
		t.Fatal("target range substitution did not change provider request digest")
	}
}

func TestFreezeContextProviderSelectionRangesPreservesExactRanges(t *testing.T) {
	service := &Service{}
	ranges, err := service.freezeContextProviderSelectionRanges(
		context.Background(), gitadapter.RevisionCapture{}, ReviewRequest{
			SelectionPath: "pkg/review.go",
			SelectionRanges: []SelectionRange{
				{StartLine: 10, EndLine: 12},
				{StartLine: 20, EndLine: 21},
			},
		}, DefaultLocalConfig(),
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []ContextProviderTargetRange{
		{Path: "pkg/review.go", StartLine: 10, EndLine: 12},
		{Path: "pkg/review.go", StartLine: 20, EndLine: 21},
	}
	if !reflect.DeepEqual(ranges, want) {
		t.Fatalf("selection context ranges = %+v, want %+v", ranges, want)
	}
}
