package hailixexecution

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

func TestHTTPClientAndAdapterRoundTripExactPublicContract(t *testing.T) {
	request := validRequest(t)
	plan := validPlan(t)
	result := validResult(t, request)
	resultJSON, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	capability := capabilityForPlan(t, plan)
	proof := []byte("signed-platform-callback-proof")
	credentialSource := &recordingCredentialSource{credential: Credential{
		BearerToken: "hailix-test-token", Revision: "credential-revision-7",
	}}

	var mutex sync.Mutex
	operationCalls := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
		if incoming.Method != http.MethodPost {
			t.Errorf("method = %s", incoming.Method)
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if incoming.Header.Get("Authorization") != "Bearer hailix-test-token" ||
			incoming.Header.Get("X-Hailix-Credential-Revision") != "credential-revision-7" ||
			incoming.Header.Get("X-Hailix-Platform-Execution-Version") != HTTPContractSchemaVersion {
			t.Errorf("transport headers = %+v", incoming.Header)
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		mutex.Lock()
		operationCalls[incoming.URL.Path]++
		mutex.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		switch incoming.URL.Path {
		case "/api/" + httpCapabilityPath:
			var command capabilityHTTPRequest
			if decodeErr := decodeStrictJSONReader(incoming, &command); decodeErr != nil {
				t.Errorf("decode capability request: %v", decodeErr)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			decoded, decodeErr := contractsv1alpha1.DecodeAgentStagePlan(command.CanonicalPlan)
			if decodeErr != nil || decoded.PlanID != plan.PlanID ||
				command.PlanSHA256 != plan.SHA256 ||
				incoming.Header.Get("Idempotency-Key") != plan.SHA256 {
				t.Errorf("capability command = %+v plan=%+v error=%v", command, decoded, decodeErr)
			}
			writeHTTPJSON(t, writer, capabilityHTTPResponse{
				SchemaVersion: HTTPContractSchemaVersion,
				Receipt: capabilityHTTPReceiptBody{
					Subject: command.Subject, PlanID: command.PlanID, PlanSHA256: command.PlanSHA256,
					Capability: capability,
					Verifier: VerifierRef{
						ID: "hailix-platform-capability-verifier", Revision: "v1",
						SHA256: strings.Repeat("c", 64),
					},
				},
			})
		case "/api/" + httpEnsurePath, "/api/" + httpLookupPath:
			var command executionHTTPRequest
			if decodeErr := decodeStrictJSONReader(incoming, &command); decodeErr != nil {
				t.Errorf("decode execution request: %v", decodeErr)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			decoded, decodeErr := contractsv1alpha1.DecodeStageExecutionRequest(
				command.CanonicalRequest,
			)
			if decodeErr != nil || decoded.RequestSHA256 != request.RequestSHA256 ||
				command.Subject.toSubject() != clientSubject(validSubject()) {
				t.Errorf("execution command = %+v decoded=%+v error=%v", command, decoded, decodeErr)
			}
			if incoming.URL.Path == "/api/"+httpEnsurePath &&
				incoming.Header.Get("Idempotency-Key") != request.IdempotencyKey {
				t.Errorf("ensure idempotency header = %q", incoming.Header.Get("Idempotency-Key"))
			}
			writeHTTPJSON(t, writer, ensureResponseForRequest(request, command.Subject))
		case "/api/" + httpAwaitPath:
			var command awaitHTTPRequest
			if decodeErr := decodeStrictJSONReader(incoming, &command); decodeErr != nil {
				t.Errorf("decode await request: %v", decodeErr)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			if command.ExecutionID != request.ExecutionID || command.RequestSHA256 != request.RequestSHA256 {
				t.Errorf("await command = %+v", command)
			}
			writeHTTPJSON(t, writer, terminalHTTPResponse{
				SchemaVersion: HTTPContractSchemaVersion, CanonicalResult: resultJSON,
				CallbackProofBase64: base64.StdEncoding.EncodeToString(proof),
			})
		case "/api/" + httpVerifyPath:
			var command verifyCallbackHTTPRequest
			if decodeErr := decodeStrictJSONReader(incoming, &command); decodeErr != nil {
				t.Errorf("decode verify request: %v", decodeErr)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			if !reflect.DeepEqual([]byte(command.CanonicalResult), resultJSON) ||
				command.CallbackProofBase64 != base64.StdEncoding.EncodeToString(proof) {
				t.Errorf("callback command = %+v", command)
			}
			writeHTTPJSON(t, writer, callbackHTTPResponse{
				SchemaVersion: HTTPContractSchemaVersion,
				Receipt: callbackHTTPReceiptBody{
					Subject: command.Subject, BindingID: command.Binding.BindingID,
					BindingSHA256:  command.Binding.BindingSHA256,
					ProviderHandle: command.Binding.ProviderHandle,
					ResultSHA256:   command.ResultSHA256, ProofSHA256: command.ProofSHA256,
					VerifierID: "hailix-platform-callback-verifier", VerifierRevision: "v1",
					VerifierSHA256: strings.Repeat("d", 64),
				},
			})
		case "/api/" + httpCancelPath:
			var command cancelHTTPRequest
			if decodeErr := decodeStrictJSONReader(incoming, &command); decodeErr != nil {
				t.Errorf("decode cancel request: %v", decodeErr)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			if command.ExecutionID != request.ExecutionID ||
				incoming.Header.Get("Idempotency-Key") != command.CancelIdempotencyKey {
				t.Errorf("cancel command = %+v", command)
			}
			writer.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected path %q", incoming.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	httpClient, err := NewHTTPClient(HTTPClientConfig{
		BaseURL: server.URL + "/api/", Credentials: credentialSource,
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := newAdapter(t, httpClient)
	resolved, err := adapter.ResolveAgentExecutorCapability(context.Background(), validSubject(), plan)
	if err != nil || !reflect.DeepEqual(resolved, capability) {
		t.Fatalf("ResolveAgentExecutorCapability() = (%+v, %v)", resolved, err)
	}
	handle, err := adapter.Ensure(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	lookedUp, found, err := adapter.Lookup(context.Background(), request)
	if err != nil || !found || lookedUp != handle {
		t.Fatalf("Lookup() = (%+v, %v, %v), want %+v", lookedUp, found, err, handle)
	}
	terminal, callbackProof, err := adapter.AwaitResult(context.Background(), handle)
	if err != nil || !reflect.DeepEqual(terminal, result) || !reflect.DeepEqual(callbackProof, proof) {
		t.Fatalf("AwaitResult() = (%+v, %q, %v)", terminal, callbackProof, err)
	}
	binding := validBinding(t, request, handle)
	verifier, err := adapter.VerifyAgentStageResultCallback(
		context.Background(), validSubject(), binding, request.Capability.Trust, resultJSON, proof,
	)
	if err != nil || verifier.ID != "hailix-platform-callback-verifier" {
		t.Fatalf("VerifyAgentStageResultCallback() = (%+v, %v)", verifier, err)
	}
	if err := adapter.Cancel(context.Background(), validCancel(request, handle)); err != nil {
		t.Fatal(err)
	}

	credentialSource.mutex.Lock()
	credentialCalls := credentialSource.calls
	credentialSubjects := append([]Subject(nil), credentialSource.subjects...)
	credentialSource.mutex.Unlock()
	if credentialCalls != 6 {
		t.Fatalf("credential Resolve calls = %d, want 6", credentialCalls)
	}
	for _, subject := range credentialSubjects {
		if subject != clientSubject(validSubject()) {
			t.Fatalf("credential resolved for escaped subject %+v", subject)
		}
	}
	for _, path := range []string{
		"/api/" + httpCapabilityPath, "/api/" + httpEnsurePath,
		"/api/" + httpLookupPath, "/api/" + httpAwaitPath,
		"/api/" + httpVerifyPath, "/api/" + httpCancelPath,
	} {
		if operationCalls[path] != 1 {
			t.Fatalf("operation %s calls = %d", path, operationCalls[path])
		}
	}
}

func TestHTTPClientClassifiesMutationUnknownOutcomeAndReadOnlyFailure(t *testing.T) {
	request := validRequest(t)
	canonical, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection reset after request write")
	})
	client, err := NewHTTPClient(HTTPClientConfig{
		BaseURL: "http://127.0.0.1:7789/",
		Client:  &http.Client{Transport: transport},
		Credentials: &recordingCredentialSource{credential: Credential{
			BearerToken: "secret-transport-token", Revision: "credential-1",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.EnsureExecution(context.Background(), EnsureCommand{
		Subject: clientSubject(validSubject()), CanonicalRequestJSON: canonical,
	})
	if !errors.Is(err, ErrHTTPOutcomeUnknown) || strings.Contains(err.Error(), "secret-transport-token") {
		t.Fatalf("EnsureExecution() error = %v", err)
	}
	_, _, err = client.LookupExecution(context.Background(), LookupCommand{
		Subject: clientSubject(validSubject()), CanonicalRequestJSON: canonical,
	})
	if !errors.Is(err, ErrHTTPUnavailable) || errors.Is(err, ErrHTTPOutcomeUnknown) {
		t.Fatalf("LookupExecution() error = %v", err)
	}
}

func TestHTTPClientExactLookupNotFoundAndStrictMutationResponse(t *testing.T) {
	request := validRequest(t)
	canonical, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
		switch incoming.URL.Path {
		case "/" + httpLookupPath:
			writer.WriteHeader(http.StatusNotFound)
		case "/" + httpEnsurePath:
			writer.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(
				writer,
				`{"schema_version":%q,"receipt":{},"unexpected":true}`,
				HTTPContractSchemaVersion,
			)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client, err := NewHTTPClient(HTTPClientConfig{
		BaseURL: server.URL + "/",
		Credentials: &recordingCredentialSource{credential: Credential{
			BearerToken: "token", Revision: "credential-1",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, found, lookupErr := client.LookupExecution(context.Background(), LookupCommand{
		Subject: clientSubject(validSubject()), CanonicalRequestJSON: canonical,
	}); lookupErr != nil || found {
		t.Fatalf("LookupExecution() = (found=%v, err=%v)", found, lookupErr)
	}
	if _, ensureErr := client.EnsureExecution(context.Background(), EnsureCommand{
		Subject: clientSubject(validSubject()), CanonicalRequestJSON: canonical,
	}); !errors.Is(ensureErr, ErrHTTPOutcomeUnknown) {
		t.Fatalf("EnsureExecution(strict invalid response) error = %v", ensureErr)
	}
}

func TestHTTPClientExactEnsureRecoversProviderCommittedUnknownOutcome(t *testing.T) {
	request := validRequest(t)
	canonical, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	starts := 0
	ensureCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
		if incoming.URL.Path != "/"+httpEnsurePath {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		var command executionHTTPRequest
		if decodeErr := decodeStrictJSONReader(incoming, &command); decodeErr != nil {
			t.Errorf("decode exact ensure: %v", decodeErr)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		ensureCalls++
		if starts == 0 {
			starts++
		}
		if ensureCalls == 1 {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusServiceUnavailable)
			writeHTTPJSON(t, writer, httpErrorResponse{
				SchemaVersion: HTTPContractSchemaVersion,
				Error: httpErrorBody{
					Code: "response_lost_after_commit", Message: "retry exact ensure",
					Retryable: true,
				},
			})
			return
		}
		writeHTTPJSON(t, writer, ensureResponseForRequest(request, command.Subject))
	}))
	defer server.Close()
	client, err := NewHTTPClient(HTTPClientConfig{
		BaseURL: server.URL + "/",
		Credentials: &recordingCredentialSource{credential: Credential{
			BearerToken: "token", Revision: "credential-1",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	command := EnsureCommand{
		Subject: clientSubject(validSubject()), CanonicalRequestJSON: canonical,
	}
	if _, firstErr := client.EnsureExecution(context.Background(), command); !errors.Is(
		firstErr,
		ErrHTTPOutcomeUnknown,
	) {
		t.Fatalf("first EnsureExecution() error = %v", firstErr)
	}
	receipt, err := client.EnsureExecution(context.Background(), command)
	if err != nil || receipt.ExecutionID != request.ExecutionID || ensureCalls != 2 || starts != 1 {
		t.Fatalf(
			"exact Ensure recovery receipt=%+v err=%v calls=%d starts=%d",
			receipt,
			err,
			ensureCalls,
			starts,
		)
	}
}

func TestHTTPClientRejectsUnsafeEndpointAndRedirect(t *testing.T) {
	credentials := &recordingCredentialSource{credential: Credential{
		BearerToken: "token", Revision: "credential-1",
	}}
	for _, endpoint := range []string{
		"http://example.com/", "http://localhost:7788/", "https://user@example.com/",
		"https://example.com/no-trailing-slash", "https://example.com/?token=secret",
	} {
		if _, err := NewHTTPClient(HTTPClientConfig{
			BaseURL: endpoint, Credentials: credentials,
		}); err == nil {
			t.Fatalf("NewHTTPClient(%q) unexpectedly succeeded", endpoint)
		}
	}

	request := validRequest(t)
	canonical, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	redirectHits := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, incoming *http.Request) {
		if incoming.URL.Path == "/redirected" {
			redirectHits++
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		http.Redirect(writer, incoming, "/redirected", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client, err := NewHTTPClient(HTTPClientConfig{
		BaseURL: server.URL + "/", Credentials: credentials,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, ensureErr := client.EnsureExecution(context.Background(), EnsureCommand{
		Subject: clientSubject(validSubject()), CanonicalRequestJSON: canonical,
	})
	if ensureErr == nil || redirectHits != 0 {
		t.Fatalf("redirect ensure error=%v redirected_hits=%d", ensureErr, redirectHits)
	}
}

func TestHTTPClientRejectsOversizedCallbackProofBeforeCredentialOrNetwork(t *testing.T) {
	credentials := &recordingCredentialSource{credential: Credential{
		BearerToken: "token", Revision: "credential-1",
	}}
	client, err := NewHTTPClient(HTTPClientConfig{
		BaseURL: "http://127.0.0.1:7789/", Credentials: credentials,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.VerifyResultCallback(context.Background(), VerifyCallbackCommand{
		CallbackProof: make([]byte, maxCallbackProofBytes+1),
	})
	if err == nil || !strings.Contains(err.Error(), "callback proof") {
		t.Fatalf("VerifyResultCallback(oversized proof) error = %v", err)
	}
	credentials.mutex.Lock()
	calls := credentials.calls
	credentials.mutex.Unlock()
	if calls != 0 {
		t.Fatalf("oversized proof resolved credentials %d times", calls)
	}
}

func TestHTTPContractExamplesStrictDecode(t *testing.T) {
	ensureData, err := os.ReadFile(
		"../../examples/hailix-platform-execution-http.ensure-request.json",
	)
	if err != nil {
		t.Fatal(err)
	}
	var ensure executionHTTPRequest
	if err := decodeStrictJSON(ensureData, &ensure); err != nil {
		t.Fatal(err)
	}
	request, err := contractsv1alpha1.DecodeStageExecutionRequest(ensure.CanonicalRequest)
	if err != nil {
		t.Fatal(err)
	}
	if ensure.SchemaVersion != HTTPContractSchemaVersion ||
		ensure.Subject.TenantID != request.TenantID ||
		ensure.Subject.WorkspaceID != request.WorkspaceID {
		t.Fatalf("ensure example subject/request mismatch: %+v / %+v", ensure.Subject, request)
	}

	terminalData, err := os.ReadFile(
		"../../examples/hailix-platform-execution-http.terminal-response.json",
	)
	if err != nil {
		t.Fatal(err)
	}
	var terminal terminalHTTPResponse
	if err := decodeStrictJSON(terminalData, &terminal); err != nil {
		t.Fatal(err)
	}
	result, err := contractsv1alpha1.DecodeStageExecutionResult(terminal.CanonicalResult)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeCanonicalBase64(terminal.CallbackProofBase64); err != nil {
		t.Fatal(err)
	}
	if terminal.SchemaVersion != HTTPContractSchemaVersion ||
		result.RequestSHA256 != request.RequestSHA256 ||
		result.CapabilitySHA256 != request.Capability.SHA256 {
		t.Fatalf("terminal example differs from exact request: %+v / %+v", result, request)
	}
}

func capabilityForPlan(
	t *testing.T,
	plan contractsv1alpha1.AgentStagePlan,
) contractsv1alpha1.ExecutorCapabilitySnapshot {
	t.Helper()
	capability := contractsv1alpha1.ExecutorCapabilitySnapshot{
		RuntimeKind: plan.Runtime.Ref.ID, RuntimeID: "hailix-runtime-1",
		RuntimeRevision: plan.Runtime.Ref.Revision,
		RuntimeSHA256:   plan.Runtime.Ref.SHA256, BuildIdentity: plan.BuildIdentity,
		Authority: contractsv1alpha1.ExecutionAuthority{
			AllowedTools:       append([]string(nil), plan.ToolAuthority.Tools...),
			ModelEgress:        plan.ModelAuthority.ModelEgress,
			ToolNetwork:        plan.ToolAuthority.ToolNetwork,
			WorkspaceReads:     plan.ToolAuthority.WorkspaceReads,
			WorkspaceWrites:    plan.ToolAuthority.WorkspaceWrites,
			RemoteWrites:       plan.ToolAuthority.RemoteWrites,
			MaxDelegationDepth: plan.ToolAuthority.MaxDelegationDepth,
		},
		Trust: testHailixExecutorTrust(),
	}
	var err error
	capability.SHA256, err = contractsv1alpha1.DigestExecutorCapability(capability)
	if err != nil {
		t.Fatal(err)
	}
	return capability
}

func ensureResponseForRequest(
	request contractsv1alpha1.StageExecutionRequest,
	subject httpSubject,
) ensureHTTPResponse {
	return ensureHTTPResponse{
		SchemaVersion: HTTPContractSchemaVersion,
		Receipt: ensureHTTPReceiptBody{
			Subject: subject, ProviderHandle: "hailix-execution-handle-1",
			ExecutionID: request.ExecutionID, Attempt: request.Attempt,
			Generation: request.Generation, FencingToken: request.FencingToken,
			IdempotencyKey: request.IdempotencyKey, RequestSHA256: request.RequestSHA256,
			CapabilitySHA256: request.Capability.SHA256,
		},
	}
}

type recordingCredentialSource struct {
	mutex      sync.Mutex
	credential Credential
	calls      int
	subjects   []Subject
}

func (source *recordingCredentialSource) Resolve(
	_ context.Context,
	subject Subject,
) (Credential, error) {
	source.mutex.Lock()
	defer source.mutex.Unlock()
	source.calls++
	source.subjects = append(source.subjects, subject)
	return source.credential, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func decodeStrictJSONReader(incoming *http.Request, target any) error {
	decoder := json.NewDecoder(incoming.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return err
	}
	return nil
}

func writeHTTPJSON(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Errorf("encode HTTP fixture response: %v", err)
	}
}
