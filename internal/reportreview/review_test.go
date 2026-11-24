package reportreview

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abietic/argus/internal/store/local"
	contract "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

func TestReportReviewBothDeliveriesAndTerminalRetry(t *testing.T) {
	dir, store, first := makeReportFixture(t, false)
	firstBytes, err := Run(t.Context(), store, first, dir)
	if err != nil {
		t.Fatal(err)
	}
	firstResult, err := contract.DecodeReportReviewResult(firstBytes)
	if err != nil {
		t.Fatal(err)
	}
	var codes []string
	for _, finding := range firstResult.Findings {
		codes = append(codes, finding.Code)
	}
	if firstResult.Verdict != contract.ReportReviewNeedsRework || !slices.Equal(codes, []string{
		"unregistered-evidence-link", "omitted-overrides-high-friction", "omitted-overrides-delayed-execution",
	}) {
		t.Fatalf("unexpected first result: %+v", firstResult)
	}
	secondDir, _, second := makeReportFixture(t, true)
	second.IdempotencyKey, second.TaskRevision = "delivery-2", 6
	second.WorkerRunID, second.ArtifactID = "worker-2", "report-2"
	secondBytes, err := Run(t.Context(), store, second, secondDir)
	if err != nil {
		t.Fatal(err)
	}
	secondResult, err := contract.DecodeReportReviewResult(secondBytes)
	if err != nil || secondResult.Verdict != contract.ReportReviewPassed || secondResult.ReviewRunID == firstResult.ReviewRunID {
		t.Fatalf("unexpected second result: %+v (%v)", secondResult, err)
	}
	for _, attempt := range []struct {
		request contract.ReportReviewRequest
		want    []byte
	}{{first, firstBytes}, {second, secondBytes}} {
		got, err := Run(t.Context(), store, attempt.request, filepath.Join(t.TempDir(), "absent"))
		if err != nil || !bytes.Equal(got, attempt.want) || !bytes.HasSuffix(got, []byte("\n")) {
			t.Fatalf("terminal retry failed: %v", err)
		}
	}
	second.ArtifactSHA256 = first.ArtifactSHA256
	if _, err := Run(t.Context(), store, second, dir); err == nil || !strings.Contains(err.Error(), "idempotency key") {
		t.Fatalf("same key accepted a different request: %v", err)
	}
}

func TestReportReviewResumesPreparedInputAndConcurrentRetry(t *testing.T) {
	dir, store, request := makeReportFixture(t, true)
	sha, err := contract.DigestReportReviewRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := freeze(t.Context(), store, request, sha, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutJSON("report-review-"+digest([]byte(request.IdempotencyKey)), prepared); err != nil {
		t.Fatal(err)
	}
	// Simulate interruption after committing inputs but before committing a
	// verdict, followed by two retrying owners with no staging files.
	var results [2][]byte
	var errors [2]error
	var workers sync.WaitGroup
	for index := range results {
		workers.Go(func() {
			results[index], errors[index] = Run(t.Context(), store, request, filepath.Join(dir, "absent"))
		})
	}
	workers.Wait()
	if errors[0] != nil || errors[1] != nil || !bytes.Equal(results[0], results[1]) {
		t.Fatalf("concurrent prepared retries diverged: %v, %v", errors[0], errors[1])
	}
}

func TestReportReviewRejectsUnfrozenAndInvalidEvidence(t *testing.T) {
	dir, store, request := makeReportFixture(t, true)
	file := filepath.Join(dir, "configured-costs.csv")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	modified := append(data, []byte("modified")...)
	if err := os.WriteFile(file, modified, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(t.Context(), store, request, dir); err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("modified staged bytes accepted: %v", err)
	}
	request.EvidenceSHA256["configured-costs.csv"] = digest(modified)
	if _, err := Run(t.Context(), store, request, dir); err == nil || !strings.Contains(err.Error(), "facts digest") {
		t.Fatalf("conflicting facts digest accepted: %v", err)
	}
	request.IdempotencyKey = "symlink-request"
	if err := os.Rename(file, file+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(file+".original", file); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(t.Context(), store, request, dir); err == nil || !strings.Contains(err.Error(), "read frozen input") {
		t.Fatalf("symlink input accepted: %v", err)
	}
}

func makeReportFixture(t *testing.T, repaired bool) (string, *local.Store, contract.ReportReviewRequest) {
	t.Helper()
	dir := t.TempDir()
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, data []byte) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
		return digest(data)
	}
	request := contract.ReportReviewRequest{
		SchemaVersion: contract.ReportReviewRequestSchema, IdempotencyKey: "delivery-1", TaskID: "task-1", TaskRevision: 4,
		WorkerRunID: "worker-1", ArtifactID: "report-1", Profile: contract.LetsQuantReportProfile, EvidenceSHA256: map[string]string{},
	}
	caseFacts := make(map[string]any)
	for i, name := range caseNames {
		var csv strings.Builder
		csv.WriteString("date,nav,cash,drawdown,risk_frozen,positions\n")
		date := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
		for j := 0; j < 260; j++ {
			nav := 100
			if j == 259 {
				date, nav = time.Date(2024, 12, 27, 0, 0, 0, 0, time.UTC), 110-i
			}
			fmt.Fprintf(&csv, "%s,%d,0,0,false,{}\n", date.Format(time.DateOnly), nav)
			date = date.AddDate(0, 0, 1)
		}
		sha := write(name+".csv", []byte(csv.String()))
		request.EvidenceSHA256[name+".csv"] = sha
		caseFacts[name] = map[string]any{"label": name, "filled_trade_count": 2, "source_sha256": sha}
	}
	inputID, resultSHA := strings.Repeat("a", 64), strings.Repeat("b", 64)
	facts, err := json.Marshal(map[string]any{
		"experiment_input_id": inputID, "result_sha256": resultSHA, "case_count": 3, "cases": caseFacts,
		"execution_scenarios": []map[string]any{
			{"name": "configured-costs"},
			{"name": "high-friction", "commission_rate": 0.001, "minimum_commission": 8, "sell_tax_rate": 0.001, "slippage_bps": 20},
			{"name": "delayed-execution", "execution_delay_trading_days": 3, "slippage_bps": 10},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	request.EvidenceSHA256["facts.json"] = write("facts.json", facts)
	report := fmt.Sprintf("%s %s\n| configured-costs | 10.000%% | 0.000%% | 2 |\n| high-friction | 9.000%% | 0.000%% | 2 |\n| delayed-execution | 8.000%% | 0.000%% | 2 |\n低 100.00 和 200.00 个基点\n", inputID, resultSHA)
	if repaired {
		report += "| high-friction | `commission_rate=0.001`, `minimum_commission=8`, `sell_tax_rate=0.001`, `slippage_bps=20` |\n"
		report += "| delayed-execution | `execution_delay_trading_days=3`, `slippage_bps=10` |\n"
	} else {
		report += "[清单](delivery-manifest.json)\n"
	}
	request.ArtifactSHA256 = write(reportName, []byte(report))
	return dir, store, request
}
