package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	ReportReviewRequestSchema = "argus.report_review_request.v1alpha1"
	ReportReviewResultSchema  = "argus.report_review_result.v1alpha1"
	LetsQuantReportProfile    = "lets-quant-report.v1"
	ReportReviewPassed        = "passed"
	ReportReviewNeedsRework   = "rework_required"
	MaxReportTaskRevision     = 1<<53 - 1
)

var reportReviewID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`)

type ReportReviewRequest struct {
	SchemaVersion  string            `json:"schema_version"`
	IdempotencyKey string            `json:"idempotency_key"`
	TaskID         string            `json:"task_id"`
	TaskRevision   uint64            `json:"task_revision"`
	WorkerRunID    string            `json:"worker_run_id"`
	ArtifactID     string            `json:"artifact_id"`
	ArtifactSHA256 string            `json:"artifact_sha256"`
	Profile        string            `json:"profile"`
	EvidenceSHA256 map[string]string `json:"evidence_sha256"`
}

type ReportReviewResult struct {
	SchemaVersion  string          `json:"schema_version"`
	RequestSHA256  string          `json:"request_sha256"`
	ReviewRunID    string          `json:"review_run_id"`
	TaskID         string          `json:"task_id"`
	TaskRevision   uint64          `json:"task_revision"`
	WorkerRunID    string          `json:"worker_run_id"`
	ArtifactID     string          `json:"artifact_id"`
	ArtifactSHA256 string          `json:"artifact_sha256"`
	Profile        string          `json:"profile"`
	Verdict        string          `json:"verdict"`
	Findings       []ReportFinding `json:"findings"`
}

type ReportFinding struct {
	Code    string `json:"code"`
	Summary string `json:"summary"`
	Line    uint64 `json:"line,omitempty"`
}

func DecodeReportReviewRequest(data []byte) (ReportReviewRequest, error) {
	var request ReportReviewRequest
	if err := decodeReportReviewJSON(data, &request, []string{
		"schema_version", "idempotency_key", "task_id", "task_revision", "worker_run_id", "artifact_id",
		"artifact_sha256", "profile", "evidence_sha256",
	}); err != nil {
		return ReportReviewRequest{}, err
	}
	return request, request.Validate()
}

func DecodeReportReviewResult(data []byte) (ReportReviewResult, error) {
	var result ReportReviewResult
	if err := decodeReportReviewJSON(data, &result, []string{
		"schema_version", "request_sha256", "review_run_id", "task_id", "task_revision", "worker_run_id", "artifact_id",
		"artifact_sha256", "profile", "verdict", "findings",
	}); err != nil {
		return ReportReviewResult{}, err
	}
	return result, result.Validate()
}

func decodeReportReviewJSON(data []byte, target any, keys []string) error {
	if !utf8.Valid(data) {
		return fmt.Errorf("report review JSON must be UTF-8")
	}
	if err := decodeStageExecutionJSON(data, target); err != nil {
		return err
	}
	// encoding/json matches struct fields case-insensitively. Require the
	// exact wire keys even when there is no exact-case duplicate.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if len(fields) != len(keys) {
		return fmt.Errorf("report review JSON fields do not match its contract")
	}
	for _, key := range keys {
		if _, ok := fields[key]; !ok {
			return fmt.Errorf("report review JSON requires field %s", key)
		}
	}
	return nil
}

func (finding *ReportFinding) UnmarshalJSON(data []byte) error {
	type wireFinding ReportFinding
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	keys := []string{"code", "summary"}
	if _, hasLine := fields["line"]; hasLine {
		keys = append(keys, "line")
	}
	var wire wireFinding
	if err := decodeReportReviewJSON(data, &wire, keys); err != nil {
		return err
	}
	if len(keys) == 3 && wire.Line == 0 {
		return fmt.Errorf("report finding line must be positive when supplied")
	}
	*finding = ReportFinding(wire)
	return nil
}

func (request ReportReviewRequest) Validate() error {
	if request.SchemaVersion != ReportReviewRequestSchema ||
		!reportReviewID.MatchString(request.IdempotencyKey) {
		return fmt.Errorf("invalid report review schema or idempotency_key")
	}
	if err := validateReportDelivery(request.TaskID, request.TaskRevision, request.WorkerRunID, request.ArtifactID, request.ArtifactSHA256, request.Profile); err != nil {
		return err
	}
	names := []string{"facts.json", "configured-costs.csv", "high-friction.csv", "delayed-execution.csv"}
	if len(request.EvidenceSHA256) != len(names) {
		return fmt.Errorf("profile requires four exact evidence files")
	}
	for _, name := range names {
		if err := requireSHA256("evidence_sha256."+name, request.EvidenceSHA256[name]); err != nil {
			return err
		}
	}
	return nil
}

func (result ReportReviewResult) Validate() error {
	if result.SchemaVersion != ReportReviewResultSchema || !reportReviewID.MatchString(result.ReviewRunID) {
		return fmt.Errorf("invalid report review result schema or run ID")
	}
	if err := requireSHA256("request_sha256", result.RequestSHA256); err != nil {
		return err
	}
	if err := validateReportDelivery(result.TaskID, result.TaskRevision, result.WorkerRunID, result.ArtifactID, result.ArtifactSHA256, result.Profile); err != nil {
		return err
	}
	if result.Findings == nil || len(result.Findings) > 20 ||
		(result.Verdict != ReportReviewPassed && result.Verdict != ReportReviewNeedsRework) ||
		(result.Verdict == ReportReviewPassed) != (len(result.Findings) == 0) {
		return fmt.Errorf("invalid report review verdict or findings")
	}
	for _, finding := range result.Findings {
		if !reportReviewID.MatchString(finding.Code) || strings.TrimSpace(finding.Summary) == "" ||
			!utf8.ValidString(finding.Summary) || len(finding.Summary) > 800 ||
			strings.IndexFunc(finding.Summary, unicode.IsControl) >= 0 || finding.Line > MaxReportTaskRevision {
			return fmt.Errorf("invalid report finding")
		}
	}
	return nil
}

func validateReportDelivery(task string, revision uint64, worker, artifact, digest, profile string) error {
	if !reportReviewID.MatchString(task) || !reportReviewID.MatchString(worker) || !reportReviewID.MatchString(artifact) ||
		revision == 0 || revision > MaxReportTaskRevision || profile != LetsQuantReportProfile {
		return fmt.Errorf("invalid report delivery identity or unsupported profile")
	}
	return requireSHA256("artifact_sha256", digest)
}

func DigestReportReviewRequest(request ReportReviewRequest) (string, error) {
	if err := request.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}
