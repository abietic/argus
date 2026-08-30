package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"argus.local/argus/internal/evaluation"
	"argus.local/argus/internal/platformapi"
)

func TestAPIServeCommandStartsAuthenticatedLoopbackServerAndStops(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "store")
	principalPath := filepath.Join(t.TempDir(), "principal.json")
	principal := platformapi.Principal{
		SchemaVersion: platformapi.PrincipalSchemaVersion,
		Actor:         "cli-curator",
		Roles:         []evaluation.Role{evaluation.RoleDatasetCurator},
		Permissions: []platformapi.Permission{
			platformapi.PermissionConfigRead,
			platformapi.PermissionDashboardRead,
			platformapi.PermissionEvaluationRead,
			platformapi.PermissionReviewRead,
		},
		ProfileRevision: "cli-curator-v1",
	}
	data, _ := json.Marshal(principal)
	if err := os.WriteFile(principalPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	token := "cli-local-api-token-00000000000000000000"
	t.Setenv(localAPITokenEnvironment, token)

	ctx, cancel := context.WithCancel(context.Background())
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- runAPI(ctx, []string{
			"serve", "--store", storePath, "--principal", principalPath,
			"--config-state-dir", filepath.Join(t.TempDir(), "config-state"),
			"--listen", "127.0.0.1:0",
		}, writer)
		_ = writer.Close()
	}()
	line, err := bufio.NewReader(reader).ReadString('\n')
	if err != nil {
		cancel()
		t.Fatalf("read ready line: %v", err)
	}
	if strings.Contains(line, token) {
		cancel()
		t.Fatal("ready line exposed bearer token")
	}
	start := strings.Index(line, "http://")
	end := strings.Index(line, " profile_revision=")
	if start < 0 || end <= start {
		cancel()
		t.Fatalf("ready line = %q", line)
	}
	request, _ := http.NewRequest(http.MethodGet, line[start:end]+"/v1/health", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		cancel()
		t.Fatalf("GET health: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("health status = %d", response.StatusCode)
	}
	for _, path := range []string{"/v1/config/revisions", "/v1/dashboard/snapshots", "/v1/review-jobs", "/v1/review-runs"} {
		request, _ := http.NewRequest(http.MethodGet, line[start:end]+path, nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			cancel()
			t.Fatalf("GET %s: %v", path, err)
		}
		pressureURL := line[start:end] + "/v1/workloads/pressure?at=" +
			url.QueryEscape(time.Now().UTC().Format(time.RFC3339Nano))
		request, _ = http.NewRequest(http.MethodGet, pressureURL, nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response, err = http.DefaultClient.Do(request)
		if err != nil {
			cancel()
			t.Fatalf("GET workload pressure: %v", err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			cancel()
			t.Fatalf("GET workload pressure status = %d", response.StatusCode)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			cancel()
			t.Fatalf("GET %s status = %d", path, response.StatusCode)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runAPI() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("api serve did not stop after cancellation")
	}
}

func TestAPIServeFlagsRejectUnsafeAddressAndMissingToken(t *testing.T) {
	if _, err := parseAPIServeFlags([]string{
		"--store", filepath.Join(t.TempDir(), "store"),
		"--principal", filepath.Join(t.TempDir(), "principal.json"),
	}); err == nil || !strings.Contains(err.Error(), "--config-state-dir") {
		t.Fatalf("missing config state error = %v", err)
	}
	if _, err := parseAPIServeFlags([]string{
		"--store", filepath.Join(t.TempDir(), "store"),
		"--config-state-dir", filepath.Join(t.TempDir(), "config-state"),
		"--principal", filepath.Join(t.TempDir(), "principal.json"),
		"--listen", "0.0.0.0:7788",
	}); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("unsafe listen error = %v", err)
	}
	principalPath := filepath.Join(t.TempDir(), "principal.json")
	data, _ := json.Marshal(platformapi.Principal{
		SchemaVersion: platformapi.PrincipalSchemaVersion,
		Actor:         "cli-curator", Roles: []evaluation.Role{evaluation.RoleDatasetCurator},
		Permissions:     []platformapi.Permission{platformapi.PermissionEvaluationRead},
		ProfileRevision: "cli-curator-v1",
	})
	if err := os.WriteFile(principalPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(localAPITokenEnvironment, "")
	if err := runAPI(context.Background(), []string{
		"serve", "--store", filepath.Join(t.TempDir(), "store"),
		"--config-state-dir", filepath.Join(t.TempDir(), "config-state"),
		"--principal", principalPath, "--listen", "127.0.0.1:0",
	}, io.Discard); err == nil || !strings.Contains(err.Error(), "between 32 and 4096") {
		t.Fatalf("empty token error = %v", err)
	}
}

func TestAPIServeFlagsEnableFormalProfileOnlyAsCompleteSet(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := filepath.Abs(filepath.Join("..", "..", "runtime", "pi-review", "dist", "worker.js"))
	if err != nil {
		t.Fatal(err)
	}
	base := []string{
		"--store", filepath.Join(t.TempDir(), "store"),
		"--config-state-dir", filepath.Join(t.TempDir(), "config"),
		"--principal", filepath.Join(t.TempDir(), "principal.json"),
	}
	if _, err := parseAPIServeFlags(append(slices.Clone(base), "--formal-node", node)); err == nil || !strings.Contains(err.Error(), "required together") {
		t.Fatalf("partial formal profile error = %v", err)
	}
	options, err := parseAPIServeFlags(append(slices.Clone(base),
		"--formal-node", node,
		"--formal-worker-script", worker,
		"--formal-provider-profile", "deepseek-anthropic-env",
		"--formal-model", "deepseek-chat",
		"--formal-input-micros-per-million", "1",
		"--formal-output-micros-per-million", "2",
		"--formal-max-bytes-per-input-token", "4",
	))
	if err != nil {
		t.Fatal(err)
	}
	if !options.formalOn || options.formal.options.NodePath != node ||
		options.formal.options.WorkerScript != worker ||
		options.formal.pricing.OutputMicrosPerMillionTokens != 2 {
		t.Fatalf("formal flags were not bound: %+v", options.formal)
	}
}

func TestAPIServeFlagsFreezeCompleteHailixTransportForFormalBatchesOnly(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := filepath.Abs(filepath.Join("..", "..", "runtime", "pi-review", "dist", "worker.js"))
	if err != nil {
		t.Fatal(err)
	}
	base := []string{
		"--store", filepath.Join(t.TempDir(), "store"),
		"--config-state-dir", filepath.Join(t.TempDir(), "config"),
		"--principal", filepath.Join(t.TempDir(), "principal.json"),
	}
	transport := []string{
		"--formal-batch-execution-backend", "hailix-http",
		"--formal-batch-hailix-base-url", "https://hailix.example.test/platform/",
		"--formal-batch-hailix-capability-verifier-id", "capability-verifier",
		"--formal-batch-hailix-capability-verifier-revision", "v1",
		"--formal-batch-hailix-capability-verifier-sha256", strings.Repeat("c", 64),
		"--formal-batch-hailix-callback-verifier-id", "callback-verifier",
		"--formal-batch-hailix-callback-verifier-revision", "v1",
		"--formal-batch-hailix-callback-verifier-sha256", strings.Repeat("d", 64),
	}
	if _, err := parseAPIServeFlags(append(slices.Clone(base), transport...)); err == nil ||
		!strings.Contains(err.Error(), "requires the complete formal runtime/model/pricing profile") {
		t.Fatalf("Hailix batch without formal profile error = %v", err)
	}
	formal := []string{
		"--formal-node", node, "--formal-worker-script", worker,
		"--formal-provider-profile", "deepseek-anthropic-env",
		"--formal-model", "deepseek-chat",
		"--formal-input-micros-per-million", "1",
		"--formal-output-micros-per-million", "2",
		"--formal-max-bytes-per-input-token", "4",
	}
	arguments := append(append(slices.Clone(base), formal...), transport...)
	options, err := parseAPIServeFlags(arguments)
	if err != nil {
		t.Fatal(err)
	}
	if options.batchTransport.Backend != formalExecutionBackendHailixHTTP ||
		options.batchTransport.HailixBaseURL != "https://hailix.example.test/platform/" ||
		options.formal.transport.Backend != "" {
		t.Fatalf("API formal/batch transport = formal:%+v batch:%+v", options.formal.transport, options.batchTransport)
	}

	incomplete := arguments[:len(arguments)-2]
	if _, err := parseAPIServeFlags(incomplete); err == nil ||
		!strings.Contains(err.Error(), "--formal-batch-hailix-callback-verifier-sha256 is required") {
		t.Fatalf("incomplete Hailix batch transport error = %v", err)
	}
}
