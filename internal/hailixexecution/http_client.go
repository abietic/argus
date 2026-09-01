package hailixexecution

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

const (
	// HTTPContractSchemaVersion identifies the public consumer contract Argus
	// expects Hailix to implement. It is deliberately independent from Hailix'
	// current DirectTaskInput endpoint.
	HTTPContractSchemaVersion = "hailix.platform_execution_http.v1alpha1"

	httpCapabilityPath = "v1/platform-executions/capabilities:resolve"
	httpEnsurePath     = "v1/platform-executions:ensure"
	httpLookupPath     = "v1/platform-executions:lookup"
	httpCancelPath     = "v1/platform-executions:cancel"
	httpAwaitPath      = "v1/platform-executions:await"
	httpVerifyPath     = "v1/platform-executions/callbacks:verify"

	defaultControlTimeout = 15 * time.Second
	defaultAwaitTimeout   = 30 * time.Minute
	maxHTTPRequestBytes   = 4 << 20
	maxHTTPResponseBytes  = int64(4 << 20)
	maxCallbackProofBytes = 64 << 10
	maxCredentialBytes    = 16 << 10
)

var (
	ErrHTTPOutcomeUnknown = errors.New(
		"Hailix platform-execution HTTP mutation outcome is unknown",
	)
	ErrHTTPRemoteRejected = errors.New("Hailix platform-execution request was rejected")
	ErrHTTPUnavailable    = errors.New("Hailix platform-execution service is unavailable")
)

// Credential is resolved at request time and must never be persisted in an
// Argus execution command, intent, binding, artifact, or error string.
type Credential struct {
	BearerToken string
	Revision    string
}

func (credential Credential) String() string {
	return fmt.Sprintf("Credential{BearerToken:[REDACTED] Revision:%q}", credential.Revision)
}

func (credential Credential) GoString() string { return credential.String() }

type CredentialSource interface {
	Resolve(context.Context, Subject) (Credential, error)
}

type HTTPClientConfig struct {
	BaseURL        string
	Client         *http.Client
	Credentials    CredentialSource
	ControlTimeout time.Duration
	AwaitTimeout   time.Duration
}

// HTTPClient is a strict transport implementation of Client. It owns only
// HTTP/session concerns; Adapter remains responsible for semantic receipt and
// subject verification.
type HTTPClient struct {
	baseURL        *url.URL
	client         *http.Client
	credentials    CredentialSource
	controlTimeout time.Duration
	awaitTimeout   time.Duration
}

var _ Client = (*HTTPClient)(nil)

func NewHTTPClient(config HTTPClientConfig) (*HTTPClient, error) {
	if config.Credentials == nil {
		return nil, fmt.Errorf("Hailix HTTP credential source is required")
	}
	baseURL, err := parseHTTPBaseURL(config.BaseURL)
	if err != nil {
		return nil, err
	}
	client := config.Client
	if client == nil {
		client = &http.Client{}
	}
	clientCopy := *client
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	controlTimeout := config.ControlTimeout
	if controlTimeout == 0 {
		controlTimeout = defaultControlTimeout
	}
	if controlTimeout < time.Millisecond || controlTimeout > 5*time.Minute {
		return nil, fmt.Errorf("Hailix HTTP control timeout must be between 1ms and 5m")
	}
	awaitTimeout := config.AwaitTimeout
	if awaitTimeout == 0 {
		awaitTimeout = defaultAwaitTimeout
	}
	if awaitTimeout < time.Millisecond || awaitTimeout > 24*time.Hour {
		return nil, fmt.Errorf("Hailix HTTP await timeout must be between 1ms and 24h")
	}
	return &HTTPClient{
		baseURL: baseURL, client: &clientCopy, credentials: config.Credentials,
		controlTimeout: controlTimeout, awaitTimeout: awaitTimeout,
	}, nil
}

func (client *HTTPClient) GetCapabilitySnapshot(
	ctx context.Context,
	command CapabilityCommand,
) (CapabilityReceipt, error) {
	request := capabilityHTTPRequest{
		SchemaVersion: HTTPContractSchemaVersion,
		Subject:       toHTTPSubject(command.Subject),
		PlanID:        command.PlanID,
		PlanSHA256:    command.PlanSHA256,
		CanonicalPlan: cloneRawMessage(command.CanonicalPlanJSON),
	}
	var response capabilityHTTPResponse
	if err := client.doControlJSON(
		ctx, command.Subject, httpCapabilityPath, command.PlanSHA256, request, &response,
	); err != nil {
		return CapabilityReceipt{}, err
	}
	if response.SchemaVersion != HTTPContractSchemaVersion {
		return CapabilityReceipt{}, contractViolation(
			"capability response schema is %q", response.SchemaVersion,
		)
	}
	return CapabilityReceipt{
		Subject:    response.Receipt.Subject.toSubject(),
		PlanID:     response.Receipt.PlanID,
		PlanSHA256: response.Receipt.PlanSHA256,
		Capability: cloneCapability(response.Receipt.Capability),
		Verifier:   response.Receipt.Verifier,
	}, nil
}

func (client *HTTPClient) EnsureExecution(
	ctx context.Context,
	command EnsureCommand,
) (EnsureReceipt, error) {
	request, err := contractsv1alpha1.DecodeStageExecutionRequest(
		command.CanonicalRequestJSON,
	)
	if err != nil {
		return EnsureReceipt{}, fmt.Errorf("decode exact ensure request: %w", err)
	}
	payload := executionHTTPRequest{
		SchemaVersion:    HTTPContractSchemaVersion,
		Subject:          toHTTPSubject(command.Subject),
		CanonicalRequest: cloneRawMessage(command.CanonicalRequestJSON),
	}
	var response ensureHTTPResponse
	if err := client.doMutationJSON(
		ctx, command.Subject, httpEnsurePath, request.IdempotencyKey, payload, &response,
	); err != nil {
		return EnsureReceipt{}, err
	}
	return decodeEnsureHTTPResponse(response)
}

func (client *HTTPClient) LookupExecution(
	ctx context.Context,
	command LookupCommand,
) (EnsureReceipt, bool, error) {
	request, err := contractsv1alpha1.DecodeStageExecutionRequest(
		command.CanonicalRequestJSON,
	)
	if err != nil {
		return EnsureReceipt{}, false, fmt.Errorf("decode exact lookup request: %w", err)
	}
	payload := executionHTTPRequest{
		SchemaVersion:    HTTPContractSchemaVersion,
		Subject:          toHTTPSubject(command.Subject),
		CanonicalRequest: cloneRawMessage(command.CanonicalRequestJSON),
	}
	var response ensureHTTPResponse
	status, err := client.doJSON(
		ctx, command.Subject, httpLookupPath, request.RequestSHA256, payload, &response,
		false, true,
	)
	if status == http.StatusNotFound && err == nil {
		return EnsureReceipt{}, false, nil
	}
	if err != nil {
		return EnsureReceipt{}, false, err
	}
	receipt, err := decodeEnsureHTTPResponse(response)
	if err != nil {
		return EnsureReceipt{}, false, err
	}
	return receipt, true, nil
}

func (client *HTTPClient) CancelExecution(
	ctx context.Context,
	command CancelCommand,
) error {
	payload := cancelHTTPRequest{
		SchemaVersion:        HTTPContractSchemaVersion,
		Subject:              toHTTPSubject(command.Subject),
		ReviewRunID:          command.ReviewRunID,
		StageID:              command.StageID,
		StageRevision:        command.StageRevision,
		StageSHA256:          command.StageSHA256,
		IntentID:             command.IntentID,
		IntentSHA256:         command.IntentSHA256,
		TerminalGateID:       command.TerminalGateID,
		TerminalGateSHA256:   command.TerminalGateSHA256,
		ProviderHandle:       command.ProviderHandle,
		ExecutionID:          command.ExecutionID,
		Attempt:              command.Attempt,
		Generation:           command.Generation,
		FencingToken:         command.FencingToken,
		CreateIdempotencyKey: command.CreateIdempotencyKey,
		CancelIdempotencyKey: command.CancelIdempotencyKey,
		RequestSHA256:        command.RequestSHA256,
		CapabilitySHA256:     command.CapabilitySHA256,
	}
	status, err := client.doJSON(
		ctx, command.Subject, httpCancelPath, command.CancelIdempotencyKey, payload, nil,
		true, false,
	)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return fmt.Errorf(
			"%w: cancel response status %d must be 204",
			ErrHTTPOutcomeUnknown,
			status,
		)
	}
	return nil
}

func (client *HTTPClient) AwaitExecution(
	ctx context.Context,
	command AwaitCommand,
) (TerminalEnvelope, error) {
	payload := awaitHTTPRequest{
		SchemaVersion:    HTTPContractSchemaVersion,
		Subject:          toHTTPSubject(command.Subject),
		ProviderHandle:   command.ProviderHandle,
		ExecutionID:      command.ExecutionID,
		Attempt:          command.Attempt,
		Generation:       command.Generation,
		FencingToken:     command.FencingToken,
		IdempotencyKey:   command.IdempotencyKey,
		RequestSHA256:    command.RequestSHA256,
		CapabilitySHA256: command.CapabilitySHA256,
	}
	var response terminalHTTPResponse
	awaitContext, cancel, err := client.boundedContext(ctx, client.awaitTimeout)
	if err != nil {
		return TerminalEnvelope{}, err
	}
	defer cancel()
	if _, err := client.doJSON(
		awaitContext, command.Subject, httpAwaitPath, command.RequestSHA256, payload, &response,
		false, false,
	); err != nil {
		return TerminalEnvelope{}, err
	}
	if response.SchemaVersion != HTTPContractSchemaVersion {
		return TerminalEnvelope{}, contractViolation(
			"terminal response schema is %q", response.SchemaVersion,
		)
	}
	proof, err := decodeCanonicalBase64(response.CallbackProofBase64)
	if err != nil {
		return TerminalEnvelope{}, contractViolation("decode callback proof: %v", err)
	}
	if len(proof) == 0 || len(proof) > maxCallbackProofBytes {
		return TerminalEnvelope{}, contractViolation(
			"callback proof must be between 1 and %d bytes", maxCallbackProofBytes,
		)
	}
	return TerminalEnvelope{
		CanonicalResultJSON: bytes.Clone(response.CanonicalResult),
		CallbackProof:       proof,
	}, nil
}

func (client *HTTPClient) VerifyResultCallback(
	ctx context.Context,
	command VerifyCallbackCommand,
) (CallbackVerificationReceipt, error) {
	if len(command.CallbackProof) == 0 || len(command.CallbackProof) > maxCallbackProofBytes {
		return CallbackVerificationReceipt{}, fmt.Errorf(
			"callback proof must be between 1 and %d bytes", maxCallbackProofBytes,
		)
	}
	payload := verifyCallbackHTTPRequest{
		SchemaVersion:       HTTPContractSchemaVersion,
		Subject:             toHTTPSubject(command.Subject),
		Binding:             command.Binding,
		CanonicalResult:     cloneRawMessage(command.CanonicalResultJSON),
		ResultSHA256:        command.ResultSHA256,
		CallbackProofBase64: base64.StdEncoding.EncodeToString(command.CallbackProof),
		ProofSHA256:         command.ProofSHA256,
	}
	var response callbackHTTPResponse
	if err := client.doControlJSON(
		ctx, command.Subject, httpVerifyPath, command.ResultSHA256, payload, &response,
	); err != nil {
		return CallbackVerificationReceipt{}, err
	}
	if response.SchemaVersion != HTTPContractSchemaVersion {
		return CallbackVerificationReceipt{}, contractViolation(
			"callback response schema is %q", response.SchemaVersion,
		)
	}
	receipt := response.Receipt
	return CallbackVerificationReceipt{
		Subject: receipt.Subject.toSubject(), BindingID: receipt.BindingID,
		BindingSHA256: receipt.BindingSHA256, ProviderHandle: receipt.ProviderHandle,
		ResultSHA256: receipt.ResultSHA256, ProofSHA256: receipt.ProofSHA256,
		VerifierID: receipt.VerifierID, VerifierRevision: receipt.VerifierRevision,
		VerifierSHA256: receipt.VerifierSHA256,
	}, nil
}

func (client *HTTPClient) doControlJSON(
	ctx context.Context,
	subject Subject,
	path string,
	requestIdentity string,
	payload any,
	target any,
) error {
	controlContext, cancel, err := client.boundedContext(ctx, client.controlTimeout)
	if err != nil {
		return err
	}
	defer cancel()
	_, err = client.doJSON(
		controlContext, subject, path, requestIdentity, payload, target, false, false,
	)
	return err
}

func (client *HTTPClient) doMutationJSON(
	ctx context.Context,
	subject Subject,
	path string,
	idempotencyKey string,
	payload any,
	target any,
) error {
	controlContext, cancel, err := client.boundedContext(ctx, client.controlTimeout)
	if err != nil {
		return err
	}
	defer cancel()
	_, err = client.doJSON(
		controlContext, subject, path, idempotencyKey, payload, target, true, false,
	)
	return err
}

func (client *HTTPClient) doJSON(
	ctx context.Context,
	subject Subject,
	path string,
	requestIdentity string,
	payload any,
	target any,
	mutation bool,
	allowNotFound bool,
) (int, error) {
	if ctx == nil {
		return 0, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if client == nil || client.baseURL == nil || client.client == nil || client.credentials == nil {
		return 0, fmt.Errorf("Hailix HTTP client is not initialized")
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("marshal Hailix HTTP request: %w", err)
	}
	if len(data) > maxHTTPRequestBytes {
		return 0, fmt.Errorf("Hailix HTTP request exceeds %d bytes", maxHTTPRequestBytes)
	}
	credential, err := client.credentials.Resolve(ctx, subject)
	if err != nil {
		return 0, fmt.Errorf("resolve Hailix HTTP credential: %w", err)
	}
	if err := validateCredential(credential); err != nil {
		return 0, err
	}
	endpoint, err := client.resolve(path)
	if err != nil {
		return 0, err
	}
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, endpoint.String(), bytes.NewReader(data),
	)
	if err != nil {
		return 0, fmt.Errorf("create Hailix HTTP request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+credential.BearerToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", requestIdentity)
	request.Header.Set("User-Agent", "argus-code-review")
	request.Header.Set("X-Hailix-Credential-Revision", credential.Revision)
	request.Header.Set("X-Hailix-Platform-Execution-Version", HTTPContractSchemaVersion)

	response, err := client.client.Do(request)
	if err != nil {
		return 0, classifyHTTPFailure(mutation, "send request", err)
	}
	defer response.Body.Close()
	responseData, err := io.ReadAll(io.LimitReader(response.Body, maxHTTPResponseBytes+1))
	if err != nil {
		return response.StatusCode, classifyHTTPFailure(mutation, "read response", err)
	}
	if int64(len(responseData)) > maxHTTPResponseBytes {
		return response.StatusCode, classifyHTTPFailure(
			mutation,
			"read response",
			fmt.Errorf("response exceeds %d bytes", maxHTTPResponseBytes),
		)
	}
	if allowNotFound && response.StatusCode == http.StatusNotFound {
		if len(bytes.TrimSpace(responseData)) != 0 {
			return response.StatusCode, fmt.Errorf(
				"%w: lookup 404 response must have an empty body",
				ErrClientContractViolation,
			)
		}
		return response.StatusCode, nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		remoteErr := decodeHTTPRemoteError(response.StatusCode, responseData)
		if mutation && response.StatusCode >= 500 {
			return response.StatusCode, fmt.Errorf("%w: %v", ErrHTTPOutcomeUnknown, remoteErr)
		}
		return response.StatusCode, remoteErr
	}
	if target == nil {
		if len(bytes.TrimSpace(responseData)) != 0 {
			return response.StatusCode, classifyHTTPFailure(
				mutation,
				"decode response",
				fmt.Errorf("successful empty response contains a body"),
			)
		}
		return response.StatusCode, nil
	}
	if err := decodeStrictJSON(responseData, target); err != nil {
		return response.StatusCode, classifyHTTPFailure(mutation, "decode response", err)
	}
	return response.StatusCode, nil
}

func (client *HTTPClient) boundedContext(
	ctx context.Context,
	timeout time.Duration,
) (context.Context, context.CancelFunc, error) {
	if ctx == nil {
		return nil, nil, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	bounded, cancel := context.WithTimeout(ctx, timeout)
	return bounded, cancel, nil
}

func (client *HTTPClient) resolve(endpointPath string) (*url.URL, error) {
	reference, err := url.Parse(endpointPath)
	if err != nil || reference.IsAbs() || reference.Host != "" || reference.RawQuery != "" ||
		reference.Fragment != "" || strings.HasPrefix(reference.Path, "/") {
		return nil, fmt.Errorf("resolve Hailix HTTP endpoint")
	}
	resolved := client.baseURL.ResolveReference(reference)
	if resolved.Scheme != client.baseURL.Scheme || resolved.Host != client.baseURL.Host ||
		!strings.HasPrefix(resolved.EscapedPath(), client.baseURL.EscapedPath()) {
		return nil, fmt.Errorf("resolved Hailix HTTP endpoint escaped configured base")
	}
	return resolved, nil
}

func parseHTTPBaseURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed == nil || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" {
		return nil, fmt.Errorf("Hailix HTTP base must be a clean HTTPS or literal-loopback HTTP URL")
	}
	if parsed.Scheme != "https" && !isLiteralLoopbackHTTP(parsed) {
		return nil, fmt.Errorf("Hailix HTTP base must be HTTPS or literal-loopback HTTP")
	}
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	if !strings.HasSuffix(parsed.Path, "/") || strings.Contains(parsed.Path, "//") ||
		strings.Contains(parsed.Path, "/./") || strings.Contains(parsed.Path, "/../") {
		return nil, fmt.Errorf("Hailix HTTP base path must be clean and end with slash")
	}
	return parsed, nil
}

func isLiteralLoopbackHTTP(reference *url.URL) bool {
	if reference == nil || reference.Scheme != "http" {
		return false
	}
	ip := net.ParseIP(reference.Hostname())
	return ip != nil && ip.IsLoopback()
}

func validateCredential(credential Credential) error {
	if credential.BearerToken == "" || credential.Revision == "" ||
		len(credential.BearerToken) > maxCredentialBytes ||
		len(credential.Revision) > 256 ||
		strings.TrimSpace(credential.BearerToken) != credential.BearerToken ||
		strings.TrimSpace(credential.Revision) != credential.Revision ||
		hasControl(credential.BearerToken) || hasControl(credential.Revision) {
		return fmt.Errorf("Hailix HTTP credential is incomplete")
	}
	return nil
}

func classifyHTTPFailure(mutation bool, operation string, err error) error {
	if mutation {
		return fmt.Errorf("%w: %s: %v", ErrHTTPOutcomeUnknown, operation, err)
	}
	return fmt.Errorf("%w: %s: %v", ErrHTTPUnavailable, operation, err)
}

func decodeHTTPRemoteError(status int, data []byte) error {
	var envelope httpErrorResponse
	if err := decodeStrictJSON(data, &envelope); err != nil ||
		envelope.SchemaVersion != HTTPContractSchemaVersion ||
		envelope.Error.Code == "" || envelope.Error.Message == "" ||
		len(envelope.Error.Code) > 128 || len(envelope.Error.Message) > 1024 {
		return fmt.Errorf("%w: HTTP %d returned an invalid error envelope", ErrHTTPUnavailable, status)
	}
	if envelope.Error.Retryable || status == http.StatusTooManyRequests || status >= 500 {
		return fmt.Errorf(
			"%w: HTTP %d code=%s retryable=true",
			ErrHTTPUnavailable,
			status,
			envelope.Error.Code,
		)
	}
	return fmt.Errorf(
		"%w: HTTP %d code=%s",
		ErrHTTPRemoteRejected,
		status,
		envelope.Error.Code,
	)
}

func decodeStrictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
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

func decodeEnsureHTTPResponse(response ensureHTTPResponse) (EnsureReceipt, error) {
	if response.SchemaVersion != HTTPContractSchemaVersion {
		return EnsureReceipt{}, contractViolation(
			"ensure response schema is %q", response.SchemaVersion,
		)
	}
	receipt := response.Receipt
	return EnsureReceipt{
		Subject: receipt.Subject.toSubject(), ProviderHandle: receipt.ProviderHandle,
		ExecutionID: receipt.ExecutionID, Attempt: receipt.Attempt,
		Generation: receipt.Generation, FencingToken: receipt.FencingToken,
		IdempotencyKey: receipt.IdempotencyKey, RequestSHA256: receipt.RequestSHA256,
		CapabilitySHA256: receipt.CapabilitySHA256,
	}, nil
}

func decodeCanonicalBase64(value string) ([]byte, error) {
	decoded, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil {
		return nil, err
	}
	if base64.StdEncoding.EncodeToString(decoded) != value {
		return nil, fmt.Errorf("base64 is not canonical")
	}
	return decoded, nil
}

func cloneRawMessage(value []byte) json.RawMessage { return bytes.Clone(value) }

func cloneCapability(
	value contractsv1alpha1.ExecutorCapabilitySnapshot,
) contractsv1alpha1.ExecutorCapabilitySnapshot {
	value.Authority.AllowedTools = append([]string(nil), value.Authority.AllowedTools...)
	return value
}

type httpSubject struct {
	TenantID       string `json:"tenant_id"`
	OrganizationID string `json:"organization_id"`
	WorkspaceID    string `json:"workspace_id"`
	RepositoryID   string `json:"repository_id"`
}

func toHTTPSubject(value Subject) httpSubject {
	return httpSubject{
		TenantID: value.TenantID, OrganizationID: value.OrganizationID,
		WorkspaceID: value.WorkspaceID, RepositoryID: value.RepositoryID,
	}
}

func (value httpSubject) toSubject() Subject {
	return Subject{
		TenantID: value.TenantID, OrganizationID: value.OrganizationID,
		WorkspaceID: value.WorkspaceID, RepositoryID: value.RepositoryID,
	}
}

type capabilityHTTPRequest struct {
	SchemaVersion string          `json:"schema_version"`
	Subject       httpSubject     `json:"subject"`
	PlanID        string          `json:"plan_id"`
	PlanSHA256    string          `json:"plan_sha256"`
	CanonicalPlan json.RawMessage `json:"canonical_plan"`
}

type capabilityHTTPResponse struct {
	SchemaVersion string                    `json:"schema_version"`
	Receipt       capabilityHTTPReceiptBody `json:"receipt"`
}

type capabilityHTTPReceiptBody struct {
	Subject    httpSubject                                  `json:"subject"`
	PlanID     string                                       `json:"plan_id"`
	PlanSHA256 string                                       `json:"plan_sha256"`
	Capability contractsv1alpha1.ExecutorCapabilitySnapshot `json:"capability"`
	Verifier   VerifierRef                                  `json:"verifier"`
}

type executionHTTPRequest struct {
	SchemaVersion    string          `json:"schema_version"`
	Subject          httpSubject     `json:"subject"`
	CanonicalRequest json.RawMessage `json:"canonical_request"`
}

type ensureHTTPResponse struct {
	SchemaVersion string                `json:"schema_version"`
	Receipt       ensureHTTPReceiptBody `json:"receipt"`
}

type ensureHTTPReceiptBody struct {
	Subject          httpSubject `json:"subject"`
	ProviderHandle   string      `json:"provider_handle"`
	ExecutionID      string      `json:"execution_id"`
	Attempt          int         `json:"attempt"`
	Generation       int         `json:"generation"`
	FencingToken     uint64      `json:"fencing_token"`
	IdempotencyKey   string      `json:"idempotency_key"`
	RequestSHA256    string      `json:"request_sha256"`
	CapabilitySHA256 string      `json:"capability_sha256"`
}

type cancelHTTPRequest struct {
	SchemaVersion        string      `json:"schema_version"`
	Subject              httpSubject `json:"subject"`
	ReviewRunID          string      `json:"review_run_id"`
	StageID              string      `json:"stage_id"`
	StageRevision        string      `json:"stage_revision"`
	StageSHA256          string      `json:"stage_sha256"`
	IntentID             string      `json:"intent_id"`
	IntentSHA256         string      `json:"intent_sha256"`
	TerminalGateID       string      `json:"terminal_gate_id"`
	TerminalGateSHA256   string      `json:"terminal_gate_sha256"`
	ProviderHandle       string      `json:"provider_handle"`
	ExecutionID          string      `json:"execution_id"`
	Attempt              int         `json:"attempt"`
	Generation           int         `json:"generation"`
	FencingToken         uint64      `json:"fencing_token"`
	CreateIdempotencyKey string      `json:"create_idempotency_key"`
	CancelIdempotencyKey string      `json:"cancel_idempotency_key"`
	RequestSHA256        string      `json:"request_sha256"`
	CapabilitySHA256     string      `json:"capability_sha256"`
}

type awaitHTTPRequest struct {
	SchemaVersion    string      `json:"schema_version"`
	Subject          httpSubject `json:"subject"`
	ProviderHandle   string      `json:"provider_handle"`
	ExecutionID      string      `json:"execution_id"`
	Attempt          int         `json:"attempt"`
	Generation       int         `json:"generation"`
	FencingToken     uint64      `json:"fencing_token"`
	IdempotencyKey   string      `json:"idempotency_key"`
	RequestSHA256    string      `json:"request_sha256"`
	CapabilitySHA256 string      `json:"capability_sha256"`
}

type terminalHTTPResponse struct {
	SchemaVersion       string          `json:"schema_version"`
	CanonicalResult     json.RawMessage `json:"canonical_result"`
	CallbackProofBase64 string          `json:"callback_proof_base64"`
}

type verifyCallbackHTTPRequest struct {
	SchemaVersion       string          `json:"schema_version"`
	Subject             httpSubject     `json:"subject"`
	Binding             CallbackBinding `json:"binding"`
	CanonicalResult     json.RawMessage `json:"canonical_result"`
	ResultSHA256        string          `json:"result_sha256"`
	CallbackProofBase64 string          `json:"callback_proof_base64"`
	ProofSHA256         string          `json:"proof_sha256"`
}

type callbackHTTPResponse struct {
	SchemaVersion string                  `json:"schema_version"`
	Receipt       callbackHTTPReceiptBody `json:"receipt"`
}

type callbackHTTPReceiptBody struct {
	Subject          httpSubject `json:"subject"`
	BindingID        string      `json:"binding_id"`
	BindingSHA256    string      `json:"binding_sha256"`
	ProviderHandle   string      `json:"provider_handle"`
	ResultSHA256     string      `json:"result_sha256"`
	ProofSHA256      string      `json:"proof_sha256"`
	VerifierID       string      `json:"verifier_id"`
	VerifierRevision string      `json:"verifier_revision"`
	VerifierSHA256   string      `json:"verifier_sha256"`
}

type httpErrorResponse struct {
	SchemaVersion string        `json:"schema_version"`
	Error         httpErrorBody `json:"error"`
}

type httpErrorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}
