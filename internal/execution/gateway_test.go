package execution

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abietic/argus/internal/artifactrepo"
	"github.com/abietic/argus/internal/store/local"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

type allowClaimedEnsure struct{}

func (allowClaimedEnsure) AuthorizeClaimedEnsure(
	context.Context,
	contractsv1alpha1.StageExecutionRequest,
) error {
	return nil
}

func (allowClaimedEnsure) AuthorizeTerminalCancel(context.Context, CancelRequest) error {
	return nil
}

type denyClaimedEnsure struct{}

func (denyClaimedEnsure) AuthorizeClaimedEnsure(
	context.Context,
	contractsv1alpha1.StageExecutionRequest,
) error {
	return errors.New("durable dispatch claim is absent")
}

func (denyClaimedEnsure) AuthorizeTerminalCancel(context.Context, CancelRequest) error {
	return errors.New("durable terminal cancellation is absent")
}

func cancelThroughGateway(
	ctx context.Context,
	gateway *Gateway,
	handle Handle,
	key string,
) error {
	prepared, err := gateway.PrepareCancel(ctx, handle, terminalCancelAuthority(key))
	if err != nil {
		return err
	}
	return gateway.CancelPrepared(ctx, prepared)
}

func terminalCancelAuthority(key string) TerminalCancelAuthority {
	return TerminalCancelAuthority{
		TenantID: "tenant-1", WorkspaceID: "workspace-1", ReviewRunID: "run-1",
		StageID: "detect", StageRevision: "1", StageSHA256: strings.Repeat("a", 64),
		IntentID: "intent-1", IntentSHA256: strings.Repeat("b", 64),
		TerminalGateID: "terminal-gate-1", TerminalGateSHA256: strings.Repeat("c", 64),
		CancelIdempotencyKey: key,
	}
}

// Dispatch exists only in the test binary. Production callers must cross the
// application write-ahead claim before they can consume an opaque
// PreparedEnsure; exposing this convenience method in gateway.go would create
// a direct bypass around that authority.
func (gateway *Gateway) Dispatch(
	ctx context.Context,
	request contractsv1alpha1.StageExecutionRequest,
) (Handle, error) {
	prepared, err := gateway.Preflight(ctx, request)
	if err != nil {
		return Handle{}, err
	}
	return gateway.EnsurePrepared(ctx, prepared)
}

func TestGatewayDispatchResolvesEveryInputBeforeCreate(t *testing.T) {
	request := validPlatformRequest(t)
	request.Upstream = []contractsv1alpha1.ArtifactBinding{
		testArtifactBinding("upstream", "argus.stage-result.v1alpha1"),
	}
	request = resealPlatformRequest(t, request)
	port := &fakePlatformPort{handle: handleFor(request)}
	resolver := &fakeArtifactResolver{}
	gateway, err := NewGateway(port, resolver, allowClaimedEnsure{})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := gateway.Dispatch(context.Background(), request)
	if err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}
	if handle != port.handle || port.creates != 1 {
		t.Fatalf("handle = %+v, creates = %d", handle, port.creates)
	}
	want := []BindingPurpose{
		PurposeAgentStagePlan,
		PurposeExecutionSnapshot,
		PurposeReviewInput,
		PurposeUpstream,
	}
	if fmt.Sprint(resolver.purposes) != fmt.Sprint(want) {
		t.Fatalf("resolver purposes = %v, want %v", resolver.purposes, want)
	}

	resolver.failAt = PurposeReviewInput
	port.creates = 0
	if _, err := gateway.Dispatch(
		context.Background(),
		request,
	); err == nil || port.creates != 0 {
		t.Fatalf("failed resolution error = %v, creates = %d", err, port.creates)
	}
}

func TestGatewayRejectsMismatchedHandleWithoutRetry(t *testing.T) {
	request := validPlatformRequest(t)
	port := &fakePlatformPort{handle: handleFor(request)}
	port.handle.Generation++
	gateway, err := NewGateway(port, &fakeArtifactResolver{}, allowClaimedEnsure{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.Dispatch(
		context.Background(),
		request,
	); !errors.Is(err, ErrPlatformIdentityMismatch) {
		t.Fatalf("Dispatch() error = %v, want ErrPlatformIdentityMismatch", err)
	}
	if port.creates != 1 {
		t.Fatalf("Create calls = %d, want exactly one", port.creates)
	}
}

func TestGatewayProviderHandleValidationMatchesDurableBinding(t *testing.T) {
	request := validPlatformRequest(t)
	tests := []struct {
		name    string
		handle  string
		wantErr bool
	}{
		{name: "ordinary", handle: "provider/executions/opaque-1"},
		{name: "512 bytes", handle: strings.Repeat("x", 512)},
		{name: "empty", handle: "", wantErr: true},
		{name: "leading whitespace", handle: " leading", wantErr: true},
		{name: "trailing whitespace", handle: "trailing ", wantErr: true},
		{name: "control", handle: "provider\nhandle", wantErr: true},
		{name: "invalid utf8", handle: string([]byte{0xff}), wantErr: true},
		{name: "513 bytes", handle: strings.Repeat("x", 513), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handle := handleFor(request)
			handle.ProviderHandle = test.handle
			gateway, err := NewGateway(
				&fakePlatformPort{handle: handle},
				&fakeArtifactResolver{},
				allowClaimedEnsure{},
			)
			if err != nil {
				t.Fatal(err)
			}
			_, err = gateway.Dispatch(context.Background(), request)
			if test.wantErr && !errors.Is(err, ErrPlatformIdentityMismatch) {
				t.Fatalf("Dispatch() error = %v, want identity mismatch", err)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("Dispatch() error = %v", err)
			}
		})
	}
}

func TestGatewayRejectsUnsafeHandleCoordinates(t *testing.T) {
	request := validPlatformRequest(t)
	maxSafe := maxStageExecutionJSONInteger
	unsafeInt := int(maxSafe)
	if uint64(unsafeInt) != maxSafe {
		t.Skip("platform int cannot represent the JSON-safe boundary")
	}
	unsafeInt++
	for _, test := range []struct {
		name   string
		mutate func(*Handle)
	}{
		{"attempt", func(handle *Handle) { handle.Attempt = unsafeInt }},
		{"generation", func(handle *Handle) { handle.Generation = unsafeInt }},
		{"fencing token", func(handle *Handle) { handle.FencingToken = maxSafe + 1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			handle := handleFor(request)
			test.mutate(&handle)
			gateway, err := NewGateway(
				&fakePlatformPort{handle: handle},
				&fakeArtifactResolver{},
				allowClaimedEnsure{},
			)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := gateway.Dispatch(
				context.Background(), request,
			); !errors.Is(err, ErrPlatformIdentityMismatch) {
				t.Fatalf("Dispatch() unsafe handle error = %v", err)
			}
			if err := cancelThroughGateway(
				context.Background(), gateway, handle, "cancel-unsafe-coordinate",
			); !errors.Is(err, ErrPlatformIdentityMismatch) {
				t.Fatalf("Cancel() unsafe handle error = %v", err)
			}
		})
	}
}

func TestGatewayLookupNeverCallsCreateAndBindsExactRequest(t *testing.T) {
	request := validPlatformRequest(t)
	port := &fakePlatformPort{handle: handleFor(request)}
	resolver := &fakeArtifactResolver{failAt: PurposeAgentStagePlan}
	gateway, err := NewGateway(port, resolver, allowClaimedEnsure{})
	if err != nil {
		t.Fatal(err)
	}
	handle, found, err := gateway.Lookup(context.Background(), request)
	if err != nil {
		t.Fatalf("Lookup() error = %v", err)
	}
	if !found || handle != port.handle || port.lookups != 1 || port.creates != 0 {
		t.Fatalf(
			"Lookup() = %+v, %v; lookups=%d creates=%d",
			handle,
			found,
			port.lookups,
			port.creates,
		)
	}
	port.handle.RequestSHA256 = strings.Repeat("f", 64)
	if _, _, err := gateway.Lookup(
		context.Background(),
		request,
	); !errors.Is(err, ErrPlatformIdentityMismatch) {
		t.Fatalf("Lookup(mismatched request digest) error = %v", err)
	}
	if port.creates != 0 {
		t.Fatalf("Lookup called Create %d times", port.creates)
	}
	if len(resolver.purposes) != 0 {
		t.Fatalf("identity-only Lookup re-resolved immutable inputs: %v", resolver.purposes)
	}
}

func TestGatewayPreparedEnsureBindsExactPreflightRequest(t *testing.T) {
	request := mutablePlatformRequest(t)
	port := &fakePlatformPort{handle: handleFor(request)}
	gateway, err := NewGateway(port, &fakeArtifactResolver{}, allowClaimedEnsure{})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := gateway.Preflight(context.Background(), request)
	if err != nil {
		t.Fatalf("Preflight() error = %v", err)
	}
	if prepared.RequestSHA256() != request.RequestSHA256 {
		t.Fatalf("prepared request digest = %q, want %q", prepared.RequestSHA256(), request.RequestSHA256)
	}
	if _, err := gateway.EnsurePrepared(nil, prepared); err == nil ||
		!strings.Contains(err.Error(), "context is required") {
		t.Fatalf("EnsurePrepared(nil) error = %v", err)
	}
	if port.creates != 0 {
		t.Fatalf("EnsurePrepared(nil) called provider %d times", port.creates)
	}

	detached := prepared.Request()
	detached.Upstream[0].Contract = "argus.detached_mutation.v1alpha1"
	detached.Capability.Authority.AllowedTools[0] = "detached_mutation"
	if prepared.Request().Upstream[0].Contract == detached.Upstream[0].Contract ||
		prepared.Request().Capability.Authority.AllowedTools[0] ==
			detached.Capability.Authority.AllowedTools[0] {
		t.Fatal("PreparedEnsure.Request() exposed mutable token state")
	}

	request.Upstream[0].Contract = "argus.request_b.v1alpha1"
	request.Capability.Authority.AllowedTools[0] = "request_b_tool"
	if _, err := gateway.EnsurePrepared(context.Background(), prepared); err != nil {
		t.Fatalf("EnsurePrepared() error = %v", err)
	}
	if port.lastCreateRequest.RequestSHA256 != prepared.RequestSHA256() ||
		port.lastCreateRequest.Upstream[0].Contract == request.Upstream[0].Contract ||
		port.lastCreateRequest.Capability.Authority.AllowedTools[0] ==
			request.Capability.Authority.AllowedTools[0] {
		t.Fatalf("provider received substituted request: %+v", port.lastCreateRequest)
	}
	if _, err := gateway.EnsurePrepared(
		context.Background(), prepared,
	); !errors.Is(err, ErrPreparedEnsureConsumed) {
		t.Fatalf("second EnsurePrepared() error = %v", err)
	}

	otherGateway, err := NewGateway(
		&fakePlatformPort{}, &fakeArtifactResolver{}, allowClaimedEnsure{},
	)
	if err != nil {
		t.Fatal(err)
	}
	otherPrepared, err := gateway.Preflight(
		context.Background(), validPlatformRequest(t),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := otherGateway.EnsurePrepared(
		context.Background(), otherPrepared,
	); !errors.Is(err, ErrInvalidPreparedEnsure) {
		t.Fatalf("foreign EnsurePrepared() error = %v", err)
	}
	if _, err := gateway.EnsurePrepared(
		context.Background(), PreparedEnsure{},
	); !errors.Is(err, ErrInvalidPreparedEnsure) {
		t.Fatalf("zero EnsurePrepared() error = %v", err)
	}
}

func TestGatewayCannotEnsureWithoutDurableClaimProof(t *testing.T) {
	request := validPlatformRequest(t)
	port := &fakePlatformPort{handle: handleFor(request)}
	gateway, err := NewGateway(port, &fakeArtifactResolver{}, denyClaimedEnsure{})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := gateway.Preflight(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.EnsurePrepared(context.Background(), prepared); err == nil ||
		!strings.Contains(err.Error(), "durable dispatch claim is absent") {
		t.Fatalf("EnsurePrepared(unclaimed) error = %v", err)
	}
	if port.creates != 0 {
		t.Fatal("unclaimed preflight token reached provider Ensure")
	}
	if _, err := gateway.PrepareClaimedEnsure(context.Background(), request); err == nil ||
		!strings.Contains(err.Error(), "durable dispatch claim is absent") {
		t.Fatalf("PrepareClaimedEnsure(unclaimed) error = %v", err)
	}
	if port.creates != 0 {
		t.Fatal("raw unclaimed recovery reached provider Ensure")
	}
	if _, err := gateway.PrepareCancel(
		context.Background(), handleFor(request), terminalCancelAuthority("cancel-without-terminal"),
	); err == nil || !strings.Contains(err.Error(), "durable terminal cancellation is absent") {
		t.Fatalf("PrepareCancel(without terminal) error = %v", err)
	}
	if port.cancels != 0 {
		t.Fatal("cancel without terminal authority reached provider")
	}
}

func TestGatewayConcurrentExactEnsureStartsProviderExecutionOnce(t *testing.T) {
	request := validPlatformRequest(t)
	port := &exactIdempotentPlatformPort{}
	gateway, err := NewGateway(port, &fakeArtifactResolver{}, allowClaimedEnsure{})
	if err != nil {
		t.Fatal(err)
	}
	const callers = 32
	handles := make(chan Handle, callers)
	errs := make(chan error, callers)
	var wait sync.WaitGroup
	for index := 0; index < callers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			prepared, prepareErr := gateway.PrepareClaimedEnsure(
				context.Background(),
				request,
			)
			if prepareErr != nil {
				errs <- prepareErr
				return
			}
			handle, ensureErr := gateway.EnsureClaimed(context.Background(), prepared)
			if ensureErr != nil {
				errs <- ensureErr
				return
			}
			handles <- handle
		}()
	}
	wait.Wait()
	close(errs)
	close(handles)
	for err := range errs {
		t.Fatalf("concurrent Ensure error = %v", err)
	}
	want := handleFor(request)
	count := 0
	for handle := range handles {
		count++
		if handle != want {
			t.Fatalf("concurrent Ensure handle = %+v, want %+v", handle, want)
		}
	}
	if count != callers || port.starts != 1 || port.calls != callers {
		t.Fatalf("handles=%d starts=%d calls=%d", count, port.starts, port.calls)
	}
}

func TestGatewayExactEnsureRejectsSameKeyDifferentPayload(t *testing.T) {
	request := validPlatformRequest(t)
	port := &exactIdempotentPlatformPort{}
	gateway, err := NewGateway(port, &fakeArtifactResolver{}, allowClaimedEnsure{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := gateway.Preflight(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.EnsurePrepared(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	changed := request
	changed.Deadline = changed.Deadline.Add(time.Second)
	changed = resealPlatformRequest(t, changed)
	if changed.IdempotencyKey != request.IdempotencyKey ||
		changed.RequestSHA256 == request.RequestSHA256 {
		t.Fatal("test request did not preserve key while changing exact payload")
	}
	second, err := gateway.Preflight(context.Background(), changed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.EnsurePrepared(context.Background(), second); err == nil ||
		!strings.Contains(err.Error(), "idempotency key is already bound") {
		t.Fatalf("EnsurePrepared(same key, changed payload) error = %v", err)
	}
	if port.starts != 1 {
		t.Fatalf("provider starts = %d, want 1", port.starts)
	}
}

func TestGatewayCancelIsExactIdempotentAfterLostResponseAndConcurrentRetry(t *testing.T) {
	request := validPlatformRequest(t)
	port := &exactIdempotentPlatformPort{loseFirstCancelResponse: true}
	gateway, err := NewGateway(port, &fakeArtifactResolver{}, allowClaimedEnsure{})
	if err != nil {
		t.Fatal(err)
	}
	ensure, err := gateway.Preflight(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := gateway.EnsurePrepared(context.Background(), ensure)
	if err != nil {
		t.Fatal(err)
	}
	const cancelKey = "cancel-exact-idempotent"
	first, err := gateway.PrepareCancel(
		context.Background(), handle, terminalCancelAuthority(cancelKey),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.CancelPrepared(context.Background(), first); err == nil ||
		!strings.Contains(err.Error(), "response lost") {
		t.Fatalf("first CancelPrepared() error = %v", err)
	}
	port.mu.Lock()
	port.loseFirstCancelResponse = false
	port.mu.Unlock()
	const retries = 24
	var wait sync.WaitGroup
	errs := make(chan error, retries)
	for index := 0; index < retries; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			prepared, prepareErr := gateway.PrepareCancel(
				context.Background(), handle, terminalCancelAuthority(cancelKey),
			)
			if prepareErr != nil {
				errs <- prepareErr
				return
			}
			errs <- gateway.CancelPrepared(context.Background(), prepared)
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent exact Cancel retry error = %v", err)
		}
	}
	port.mu.Lock()
	defer port.mu.Unlock()
	if port.cancelEffects != 1 || port.cancelCalls != retries+1 || port.starts != 1 {
		t.Fatalf(
			"cancel effects=%d calls=%d starts=%d",
			port.cancelEffects,
			port.cancelCalls,
			port.starts,
		)
	}
}

func TestGatewayCancelRejectsSameKeyChangedTarget(t *testing.T) {
	request := validPlatformRequest(t)
	port := &exactIdempotentPlatformPort{}
	gateway, err := NewGateway(port, &fakeArtifactResolver{}, allowClaimedEnsure{})
	if err != nil {
		t.Fatal(err)
	}
	ensure, err := gateway.Preflight(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := gateway.EnsurePrepared(context.Background(), ensure)
	if err != nil {
		t.Fatal(err)
	}
	const cancelKey = "cancel-one-key"
	if err := cancelThroughGateway(context.Background(), gateway, handle, cancelKey); err != nil {
		t.Fatal(err)
	}
	changed := handle
	changed.Generation++
	if err := cancelThroughGateway(
		context.Background(), gateway, changed, cancelKey,
	); err == nil || !strings.Contains(err.Error(), "cancel idempotency key is already bound") {
		t.Fatalf("changed-target cancel error = %v", err)
	}
	port.mu.Lock()
	defer port.mu.Unlock()
	if port.cancelEffects != 1 {
		t.Fatalf("cancel effects = %d, want 1", port.cancelEffects)
	}
}

func TestGatewayConcurrentCancelAndExactEnsureDoNotDuplicateEffects(t *testing.T) {
	request := validPlatformRequest(t)
	port := &exactIdempotentPlatformPort{}
	gateway, err := NewGateway(port, &fakeArtifactResolver{}, allowClaimedEnsure{})
	if err != nil {
		t.Fatal(err)
	}
	initial, err := gateway.Preflight(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := gateway.EnsurePrepared(context.Background(), initial)
	if err != nil {
		t.Fatal(err)
	}
	const operations = 32
	errs := make(chan error, operations)
	var wait sync.WaitGroup
	for index := 0; index < operations; index++ {
		wait.Add(1)
		go func(cancelOperation bool) {
			defer wait.Done()
			if cancelOperation {
				errs <- cancelThroughGateway(
					context.Background(), gateway, handle, "cancel-racing-exact-ensure",
				)
				return
			}
			prepared, prepareErr := gateway.PrepareClaimedEnsure(
				context.Background(), request,
			)
			if prepareErr != nil {
				errs <- prepareErr
				return
			}
			_, ensureErr := gateway.EnsureClaimed(context.Background(), prepared)
			errs <- ensureErr
		}(index%2 == 0)
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("racing cancel/ensure error = %v", err)
		}
	}
	port.mu.Lock()
	defer port.mu.Unlock()
	if port.starts != 1 || port.cancelEffects != 1 {
		t.Fatalf("provider starts=%d cancel effects=%d", port.starts, port.cancelEffects)
	}
}

func TestEnsurePreparedRejectsCanceledContextBeforeConsumingToken(t *testing.T) {
	request := validPlatformRequest(t)
	port := &fakePlatformPort{handle: handleFor(request)}
	gateway, err := NewGateway(port, &fakeArtifactResolver{}, allowClaimedEnsure{})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := gateway.Preflight(context.Background(), request)
	if err != nil {
		t.Fatalf("Preflight() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := gateway.EnsurePrepared(ctx, prepared); !errors.Is(err, context.Canceled) {
		t.Fatalf("EnsurePrepared(canceled) error = %v", err)
	}
	if port.creates != 0 {
		t.Fatal("canceled EnsurePrepared reached provider")
	}
	if _, err := gateway.EnsurePrepared(context.Background(), prepared); err != nil {
		t.Fatalf("EnsurePrepared(retry) error = %v", err)
	}
	if port.creates != 1 {
		t.Fatalf("provider creates = %d, want 1", port.creates)
	}
}

func TestGatewayReadSideOperationsFailFastOnCanceledContext(t *testing.T) {
	request := validPlatformRequest(t)
	result := validPlatformResult(request)
	port := &fakePlatformPort{handle: handleFor(request)}
	resolver := &fakeArtifactResolver{}
	gateway, err := NewGateway(port, resolver, allowClaimedEnsure{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := gateway.Preflight(ctx, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("Preflight(canceled) error = %v", err)
	}
	if _, found, err := gateway.Lookup(ctx, request); !errors.Is(err, context.Canceled) || found {
		t.Fatalf("Lookup(canceled) = found %v, error %v", found, err)
	}
	if err := gateway.AdmitResult(ctx, request, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("AdmitResult(canceled) error = %v", err)
	}
	if port.creates != 0 || port.lookups != 0 || port.cancels != 0 ||
		len(resolver.purposes) != 0 {
		t.Fatalf(
			"canceled operations reached dependencies: creates=%d lookups=%d cancels=%d resolves=%v",
			port.creates,
			port.lookups,
			port.cancels,
			resolver.purposes,
		)
	}
}

func TestGatewayDetachesRequestSlicesFromMaliciousPort(t *testing.T) {
	for _, operation := range []string{"create", "lookup"} {
		t.Run(operation, func(t *testing.T) {
			request := mutablePlatformRequest(t)
			originalUpstream := request.Upstream[0]
			originalTool := request.Capability.Authority.AllowedTools[0]
			port := &fakePlatformPort{handle: handleFor(request)}
			port.mutateRequest = func(candidate *contractsv1alpha1.StageExecutionRequest) {
				candidate.Upstream[0].Contract = "argus.provider_mutation.v1alpha1"
				candidate.Capability.Authority.AllowedTools[0] = "provider_mutation"
			}
			gateway, err := NewGateway(port, &fakeArtifactResolver{}, allowClaimedEnsure{})
			if err != nil {
				t.Fatal(err)
			}
			switch operation {
			case "create":
				if _, err := gateway.Dispatch(context.Background(), request); err != nil {
					t.Fatalf("Dispatch() error = %v", err)
				}
			case "lookup":
				if _, found, err := gateway.Lookup(context.Background(), request); err != nil || !found {
					t.Fatalf("Lookup() found = %v, error = %v", found, err)
				}
			}
			if request.Upstream[0] != originalUpstream ||
				request.Capability.Authority.AllowedTools[0] != originalTool {
				t.Fatal("malicious platform port mutated the caller request")
			}
			if err := request.Validate(); err != nil {
				t.Fatalf("caller request became invalid after provider call: %v", err)
			}
		})
	}
}

func TestGatewayRequestSliceIsolationUnderProviderMutation(t *testing.T) {
	for _, operation := range []string{"create", "lookup"} {
		t.Run(operation, func(t *testing.T) {
			request := mutablePlatformRequest(t)
			port := &racingMutationPort{
				handle:  handleFor(request),
				started: make(chan struct{}),
				release: make(chan struct{}),
			}
			gateway, err := NewGateway(port, &fakeArtifactResolver{}, allowClaimedEnsure{})
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				if operation == "create" {
					_, dispatchErr := gateway.Dispatch(context.Background(), request)
					done <- dispatchErr
					return
				}
				_, found, lookupErr := gateway.Lookup(context.Background(), request)
				if lookupErr == nil && !found {
					lookupErr = errors.New("lookup did not find provider handle")
				}
				done <- lookupErr
			}()
			<-port.started
			close(port.release)
			for {
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
					if request.Upstream[0].Contract != "argus.stage-result.v1alpha1" ||
						request.Capability.Authority.AllowedTools[0] != "read_file" {
						t.Fatal("concurrent provider mutation escaped request isolation")
					}
					return
				default:
					_ = request.Upstream[0].Contract
					_ = request.Capability.Authority.AllowedTools[0]
				}
			}
		})
	}
}

func TestGatewayFencesResultBeforeResolvingOutputAndTrace(t *testing.T) {
	request := validPlatformRequest(t)
	gateway, err := NewGateway(
		&fakePlatformPort{handle: handleFor(request)},
		&fakeArtifactResolver{},
		allowClaimedEnsure{},
	)
	if err != nil {
		t.Fatal(err)
	}
	result := validPlatformResult(request)
	resolver := gateway.resolver.(*fakeArtifactResolver)
	stale := result
	stale.FencingToken++
	if err := gateway.AdmitResult(
		context.Background(),
		request,
		stale,
	); !errors.Is(err, ErrPlatformIdentityMismatch) {
		t.Fatalf("stale AdmitResult() error = %v", err)
	}
	if len(resolver.purposes) != 0 {
		t.Fatalf("stale result resolved artifacts: %v", resolver.purposes)
	}
	if err := gateway.AdmitResult(
		context.Background(),
		request,
		result,
	); err != nil {
		t.Fatalf("AdmitResult() error = %v", err)
	}
	want := []BindingPurpose{PurposeStageOutput, PurposeTraceManifest}
	if fmt.Sprint(resolver.purposes) != fmt.Sprint(want) {
		t.Fatalf("resolver purposes = %v, want %v", resolver.purposes, want)
	}
}

func TestGatewayCancelUsesExactHandle(t *testing.T) {
	request := validPlatformRequest(t)
	port := &fakePlatformPort{handle: handleFor(request)}
	gateway, err := NewGateway(port, &fakeArtifactResolver{}, allowClaimedEnsure{})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := gateway.PrepareCancel(
		context.Background(), port.handle,
		terminalCancelAuthority("cancel-execution-1-detect-1"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.CancelPrepared(context.Background(), prepared); err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}
	if err := gateway.CancelPrepared(
		context.Background(), prepared,
	); !errors.Is(err, ErrPreparedCancelConsumed) {
		t.Fatalf("second CancelPrepared() error = %v", err)
	}
	if port.cancels != 1 || port.lastCancel.ExecutionID != request.ExecutionID ||
		port.lastCancel.Generation != request.Generation ||
		port.lastCancel.FencingToken != request.FencingToken {
		t.Fatalf("cancel = %+v, calls = %d", port.lastCancel, port.cancels)
	}
	if err := cancelThroughGateway(
		context.Background(), gateway, Handle{}, "cancel-1",
	); !errors.Is(
		err,
		ErrPlatformIdentityMismatch,
	) {
		t.Fatalf("incomplete Cancel() error = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cancelThroughGateway(
		canceled,
		gateway,
		port.handle,
		"cancel-execution-1-detect-canceled",
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("Cancel(canceled context) error = %v", err)
	}
	if port.cancels != 1 {
		t.Fatalf("canceled context reached provider Cancel: calls=%d", port.cancels)
	}
	otherGateway, err := NewGateway(
		&fakePlatformPort{}, &fakeArtifactResolver{}, allowClaimedEnsure{},
	)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := gateway.PrepareCancel(
		context.Background(), port.handle, terminalCancelAuthority("cancel-foreign-token"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := otherGateway.CancelPrepared(
		context.Background(), foreign,
	); !errors.Is(err, ErrInvalidPreparedCancel) {
		t.Fatalf("foreign CancelPrepared() error = %v", err)
	}
	if err := gateway.CancelPrepared(
		context.Background(), PreparedCancel{},
	); !errors.Is(err, ErrInvalidPreparedCancel) {
		t.Fatalf("zero CancelPrepared() error = %v", err)
	}
}

func TestGovernedArtifactResolverEnforcesCurrentTenantWorkspaceAndContract(t *testing.T) {
	store, err := local.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := artifactrepo.Open(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	subject := artifactrepo.Subject{
		TenantID: "tenant-1", WorkspaceID: "workspace-1", Roles: []string{},
	}
	put := func(id string, contract string) contractsv1alpha1.ArtifactBinding {
		t.Helper()
		ref, _, putErr := artifacts.Put(
			context.Background(),
			subject,
			artifactrepo.PutRequest{
				Authority: "argus-local", TenantID: subject.TenantID,
				WorkspaceID: subject.WorkspaceID, Contract: contract,
				AllowedUses: []artifactrepo.Use{artifactrepo.UseRead},
				Content:     []byte(id),
				Mutation: artifactrepo.Mutation{
					IdempotencyKey: "put-" + id,
					Actor:          "execution-test",
					Audit:          "bind typed platform input",
					At:             time.Date(2026, 7, 27, 1, 0, 0, 0, time.UTC),
				},
			},
		)
		if putErr != nil {
			t.Fatal(putErr)
		}
		return contractsv1alpha1.ArtifactBinding{
			Ref: contractsv1alpha1.ContentRef{
				URI: ref.URI, SHA256: ref.SHA256, SizeBytes: ref.SizeBytes,
			},
			Contract: ref.Contract,
		}
	}
	request := validPlatformRequest(t)
	request.ExecutionSnapshot = put(
		"snapshot",
		"argus.execution_snapshot.v1alpha1",
	)
	request.ReviewInput = put("input", "argus.review_input.v1alpha1")
	request.Plan = put("plan", contractsv1alpha1.AgentStagePlanSchemaVersion)
	request.Upstream = []contractsv1alpha1.ArtifactBinding{
		put("upstream", "argus.reviewcore_artifact.v1alpha1"),
	}
	request = resealPlatformRequest(t, request)
	port := &fakePlatformPort{handle: handleFor(request)}
	gateway, err := NewGateway(
		port,
		GovernedArtifactResolver{Repository: artifacts},
		allowClaimedEnsure{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.Dispatch(context.Background(), request); err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}

	crossTenant := request
	crossTenant.TenantID = "tenant-2"
	crossTenant = resealPlatformRequest(t, crossTenant)
	port.handle = handleFor(crossTenant)
	if _, err := gateway.Dispatch(
		context.Background(),
		crossTenant,
	); !errors.Is(err, artifactrepo.ErrUnauthorized) {
		t.Fatalf("cross-tenant Dispatch() error = %v", err)
	}
	forgedContract := request
	forgedContract.ReviewInput.Contract = "argus.other.v1alpha1"
	port.handle = handleFor(forgedContract)
	if _, err := gateway.Dispatch(
		context.Background(),
		forgedContract,
	); err == nil {
		t.Fatal("Dispatch() accepted a forged artifact contract")
	}
}

type fakePlatformPort struct {
	handle            Handle
	createErr         error
	cancelErr         error
	creates           int
	lookups           int
	cancels           int
	lastCancel        CancelRequest
	lastCreateRequest contractsv1alpha1.StageExecutionRequest
	lastLookupRequest contractsv1alpha1.StageExecutionRequest
	mutateRequest     func(*contractsv1alpha1.StageExecutionRequest)
}

type exactIdempotentPlatformPort struct {
	mu                      sync.Mutex
	request                 *contractsv1alpha1.StageExecutionRequest
	handle                  Handle
	cancel                  *CancelRequest
	calls                   int
	starts                  int
	cancelCalls             int
	cancelEffects           int
	loseFirstCancelResponse bool
}

func (port *exactIdempotentPlatformPort) Ensure(
	_ context.Context,
	request contractsv1alpha1.StageExecutionRequest,
) (Handle, error) {
	port.mu.Lock()
	defer port.mu.Unlock()
	port.calls++
	if port.request != nil {
		if port.request.IdempotencyKey != request.IdempotencyKey ||
			port.request.RequestSHA256 != request.RequestSHA256 {
			return Handle{}, errors.New("idempotency key is already bound to another exact request")
		}
		return port.handle, nil
	}
	copy := cloneStageExecutionRequest(request)
	port.request = &copy
	port.handle = handleFor(request)
	port.starts++
	return port.handle, nil
}

func (port *exactIdempotentPlatformPort) Lookup(
	_ context.Context,
	request contractsv1alpha1.StageExecutionRequest,
) (Handle, bool, error) {
	port.mu.Lock()
	defer port.mu.Unlock()
	if port.request == nil || port.request.IdempotencyKey != request.IdempotencyKey ||
		port.request.RequestSHA256 != request.RequestSHA256 {
		return Handle{}, false, nil
	}
	return port.handle, true, nil
}

func (port *exactIdempotentPlatformPort) Cancel(
	_ context.Context,
	request CancelRequest,
) error {
	port.mu.Lock()
	defer port.mu.Unlock()
	port.cancelCalls++
	if port.cancel != nil {
		if *port.cancel != request {
			return errors.New("cancel idempotency key is already bound to another exact target")
		}
		return nil
	}
	copy := request
	port.cancel = &copy
	port.cancelEffects++
	if port.loseFirstCancelResponse {
		return errors.New("cancel response lost after provider accepted request")
	}
	return nil
}

func (port *fakePlatformPort) Lookup(
	_ context.Context,
	request contractsv1alpha1.StageExecutionRequest,
) (Handle, bool, error) {
	port.lookups++
	port.lastLookupRequest = request
	if port.mutateRequest != nil {
		port.mutateRequest(&request)
	}
	if port.createErr != nil {
		return Handle{}, false, port.createErr
	}
	return port.handle, true, nil
}

func (port *fakePlatformPort) Ensure(
	_ context.Context,
	request contractsv1alpha1.StageExecutionRequest,
) (Handle, error) {
	port.creates++
	port.lastCreateRequest = request
	if port.mutateRequest != nil {
		port.mutateRequest(&request)
	}
	return port.handle, port.createErr
}

func (port *fakePlatformPort) Cancel(
	_ context.Context,
	request CancelRequest,
) error {
	port.cancels++
	port.lastCancel = request
	return port.cancelErr
}

type racingMutationPort struct {
	handle  Handle
	started chan struct{}
	release chan struct{}
}

func (port *racingMutationPort) mutate(
	request contractsv1alpha1.StageExecutionRequest,
) {
	close(port.started)
	<-port.release
	for index := 0; index < 10_000; index++ {
		request.Upstream[0].Contract = fmt.Sprintf("argus.provider-mutation-%d", index%2)
		request.Capability.Authority.AllowedTools[0] = fmt.Sprintf(
			"provider_mutation_%d",
			index%2,
		)
	}
}

func (port *racingMutationPort) Ensure(
	_ context.Context,
	request contractsv1alpha1.StageExecutionRequest,
) (Handle, error) {
	port.mutate(request)
	return port.handle, nil
}

func (port *racingMutationPort) Lookup(
	_ context.Context,
	request contractsv1alpha1.StageExecutionRequest,
) (Handle, bool, error) {
	port.mutate(request)
	return port.handle, true, nil
}

func (*racingMutationPort) Cancel(context.Context, CancelRequest) error {
	return nil
}

type fakeArtifactResolver struct {
	purposes []BindingPurpose
	failAt   BindingPurpose
}

func (resolver *fakeArtifactResolver) Verify(
	_ context.Context,
	_ string,
	_ string,
	_ contractsv1alpha1.ArtifactBinding,
	purpose BindingPurpose,
) error {
	resolver.purposes = append(resolver.purposes, purpose)
	if resolver.failAt == purpose {
		return errors.New("resolver rejected binding")
	}
	return nil
}

func handleFor(request contractsv1alpha1.StageExecutionRequest) Handle {
	return Handle{
		ProviderHandle: "provider-" + request.ExecutionID,
		ExecutionID:    request.ExecutionID, Attempt: request.Attempt,
		Generation: request.Generation, FencingToken: request.FencingToken,
		IdempotencyKey:   request.IdempotencyKey,
		RequestSHA256:    request.RequestSHA256,
		CapabilitySHA256: request.Capability.SHA256,
	}
}

func validPlatformResult(
	request contractsv1alpha1.StageExecutionRequest,
) contractsv1alpha1.StageExecutionResult {
	return contractsv1alpha1.StageExecutionResult{
		SchemaVersion:  contractsv1alpha1.StageExecutionResultSchemaVersion,
		ExecutionID:    request.ExecutionID,
		Attempt:        request.Attempt,
		Generation:     request.Generation,
		FencingToken:   request.FencingToken,
		IdempotencyKey: request.IdempotencyKey,
		RequestSHA256:  request.RequestSHA256,
		Status:         contractsv1alpha1.StageExecutionSucceeded,
		Output: &contractsv1alpha1.ArtifactBinding{
			Ref: contractsv1alpha1.ContentRef{
				URI:       "artifact://local/stage-output",
				SHA256:    strings.Repeat("b", 64),
				SizeBytes: 2,
			},
			Contract: request.OutputContract,
		},
		TraceManifest: &contractsv1alpha1.ArtifactBinding{
			Ref: contractsv1alpha1.ContentRef{
				URI:       "artifact://hailix/trace-manifest",
				SHA256:    strings.Repeat("c", 64),
				SizeBytes: 3,
			},
			Contract: "hailix.trace_manifest.v1alpha1",
		},
		Completeness:      "complete",
		CompletenessNotes: []string{},
		CapabilitySHA256:  request.Capability.SHA256,
		RecordedAt:        time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC),
	}
}

func resealPlatformRequest(
	t *testing.T,
	request contractsv1alpha1.StageExecutionRequest,
) contractsv1alpha1.StageExecutionRequest {
	t.Helper()
	sealed, err := contractsv1alpha1.SealStageExecutionRequest(request)
	if err != nil {
		t.Fatalf("SealStageExecutionRequest() error = %v", err)
	}
	return sealed
}

func mutablePlatformRequest(t *testing.T) contractsv1alpha1.StageExecutionRequest {
	t.Helper()
	request := validPlatformRequest(t)
	request.Upstream = []contractsv1alpha1.ArtifactBinding{
		testArtifactBinding("mutable-upstream", "argus.stage-result.v1alpha1"),
	}
	request.Capability.Authority.AllowedTools = []string{"read_file"}
	digest, err := contractsv1alpha1.DigestExecutorCapability(request.Capability)
	if err != nil {
		t.Fatal(err)
	}
	request.Capability.SHA256 = digest
	return resealPlatformRequest(t, request)
}

func testArtifactBinding(
	name string,
	contract string,
) contractsv1alpha1.ArtifactBinding {
	return contractsv1alpha1.ArtifactBinding{
		Ref: contractsv1alpha1.ContentRef{
			URI:       "artifact://local/" + name,
			SHA256:    strings.Repeat("d", 64),
			SizeBytes: 1,
		},
		Contract: contract,
	}
}
