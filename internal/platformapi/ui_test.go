package platformapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"argus.local/argus/internal/evaluation"
)

func TestOperatorUIServesCredentialFreeShellWhileAPIRemainsAuthenticated(t *testing.T) {
	repository, _ := newTestRepository(t)
	principal := Principal{
		SchemaVersion: PrincipalSchemaVersion, Actor: "ui-reader",
		Roles:           []evaluation.Role{evaluation.RoleDatasetCurator},
		Permissions:     []Permission{PermissionEvaluationRead},
		ProfileRevision: "ui-reader-v1",
	}
	handler, err := NewHandler(Services{Evaluation: repository}, principal, testToken)
	if err != nil {
		t.Fatal(err)
	}

	root := httptest.NewRecorder()
	handler.ServeHTTP(root, httptest.NewRequest(http.MethodGet, "/", nil))
	if root.Code != http.StatusTemporaryRedirect || root.Header().Get("Location") != "/ui/" {
		t.Fatalf("root status=%d location=%q", root.Code, root.Header().Get("Location"))
	}

	index := httptest.NewRecorder()
	handler.ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/ui/", nil))
	if index.Code != http.StatusOK || !strings.Contains(index.Body.String(), "Argus Operator") {
		t.Fatalf("index status=%d body=%s", index.Code, index.Body.String())
	}
	for name, want := range map[string]string{
		"Cache-Control":                "no-store",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Referrer-Policy":              "no-referrer",
		"X-Frame-Options":              "DENY",
	} {
		if got := index.Header().Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	csp := index.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") ||
		!strings.Contains(csp, "script-src 'self'") ||
		strings.Contains(csp, "unsafe-inline") {
		t.Fatalf("Content-Security-Policy = %q", csp)
	}
	if strings.Contains(index.Body.String(), testToken) ||
		strings.Contains(index.Body.String(), "ARGUS_LOCAL_API_TOKEN") {
		t.Fatal("static UI shell contains a bearer secret or environment name")
	}

	script := httptest.NewRecorder()
	handler.ServeHTTP(script, httptest.NewRequest(http.MethodGet, "/ui/app.js", nil))
	if script.Code != http.StatusOK || script.Header().Get("Content-Type") != "text/javascript; charset=utf-8" {
		t.Fatalf("script status=%d content-type=%q", script.Code, script.Header().Get("Content-Type"))
	}
	for _, forbidden := range []string{"localStorage", "sessionStorage", "document.cookie"} {
		if strings.Contains(script.Body.String(), forbidden) {
			t.Fatalf("operator UI persists credentials through %s", forbidden)
		}
	}
	if !strings.Contains(script.Body.String(), "/v1/workloads/pressure?at=") ||
		!strings.Contains(script.Body.String(), "查询不会隐式 reconcile") {
		t.Fatal("operator overview does not expose the read-only workload pressure projection")
	}
	for _, endpoint := range []string{
		"/decisions`", "/feedback`", "/outcomes`", "/v1/review-run-impacts?",
		"/v1/evaluation/evaluation-runs?", "/v1/evaluation/experiment-runs?",
		"/v1/evaluation/repeatability-runs?", "/v1/evaluation/experiment-batches?",
		"/v1/evaluation/repeatability-batches?",
		"/v1/calibration/runs?",
		"/v1/calibration/run?run_id=",
		"/v1/calibration/promotion/plans?",
		"/v1/calibration/promotion/plan?plan_id=",
		"/v1/calibration/promotion/gates",
		"/v1/calibration/promotion/activate",
		"/v1/calibration/promotion/rollback",
		"/v1/calibration/promotion/observations?",
		"/v1/calibration/promotion/observation?observation_id=",
		"argus.local_api_calibration_promotion_observe_command.v1alpha1",
		"/v1/evaluation/experiment-batches\"",
		"/v1/evaluation/repeatability-batches\"",
		"argus.local_api_experiment_batch_submit_command.v1alpha1",
		"argus.local_api_repeatability_batch_submit_command.v1alpha1",
		"/v1/evaluation/experiment-batch/resume?batch_id=",
		"/v1/evaluation/repeatability-batch/resume?batch_id=",
		"/v1/agent-components",
		"argus.local_api_agent_component_publish_command.v1alpha1",
		"/v1/config/resolutions",
		"/v1/training/manifests?",
		"/v1/training/manifest?manifest_id=",
		"/v1/training/exports?",
		"/v1/training/export?export_id=",
		"/v1/training/jobs?",
		"/v1/training/job?job_id=",
		"/v1/training/job/observations",
		"argus.local_api_training_materialize_command.v1alpha1",
		"argus.local_api_training_export_build_command.v1alpha1",
		"argus.local_api_training_job_prepare_command.v1alpha1",
		"argus.local_api_training_job_observe_command.v1alpha1",
	} {
		if !strings.Contains(script.Body.String(), endpoint) {
			t.Fatalf("operator finding UI does not expose write endpoint %q", endpoint)
		}
	}
	for _, required := range []string{
		"提交单变量实验", "提交重复性批次",
		"请求不接收本地 prompt、skill 或 knowledge 路径",
		"component_refs", "published-prompt-v2", "knowledge_pack", "rule_pack", "workflow_definition",
		"受治理缺陷判据被 Pi context/review/verifier 实际消费",
		"AgentReviewPolicy 与 stage budget 的逐项最小值",
		"filter_policy", "只重算 calibration/suppression",
		"deepseek-reasoner", "exact model ID",
		"准备发布计划", "验证并记录门禁", "managed variant",
		"创建质量窗口观测", "监控不会自动回滚", "integer PPM thresholds",
		"发布 Agent 组件", "subject 只从已提交 baseline ReviewRun 推导",
		"API 只构建", "publish 仅允许 CLI", "本页面不接收 output path",
		"remote side effects 固定 deny", "operator_recorded_unattested",
		"promotion_eligible=false", "不会调用训练 provider",
		"expected plan SHA 做 CAS", "independently_adjudicated_not_argus_output",
	} {
		if !strings.Contains(script.Body.String(), required) {
			t.Fatalf("operator evaluation UI omitted platform batch boundary %q", required)
		}
	}
	for _, forbidden := range []string{"actor:", "actor_kind:", "finding_roles:"} {
		if strings.Contains(script.Body.String(), forbidden) {
			t.Fatalf("operator finding UI allows caller-controlled authority through %q", forbidden)
		}
	}
	if strings.Contains(script.Body.String(), "/v1/training/export/publish") {
		t.Fatal("operator UI exposes filesystem publication through the authenticated API")
	}

	head := httptest.NewRecorder()
	handler.ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/ui/app.css", nil))
	if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Length") == "" {
		t.Fatalf("HEAD status=%d length=%q body=%d", head.Code, head.Header().Get("Content-Length"), head.Body.Len())
	}
	style := httptest.NewRecorder()
	handler.ServeHTTP(style, httptest.NewRequest(http.MethodGet, "/ui/app.css", nil))
	if style.Code != http.StatusOK || !strings.Contains(style.Body.String(), "[hidden]") ||
		!strings.Contains(style.Body.String(), "display: none !important") {
		t.Fatal("operator UI does not reliably hide inactive variant controls")
	}
	post := httptest.NewRecorder()
	handler.ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/ui/", nil))
	if post.Code != http.StatusMethodNotAllowed || post.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST UI status=%d allow=%q", post.Code, post.Header().Get("Allow"))
	}

	api := httptest.NewRecorder()
	handler.ServeHTTP(api, httptest.NewRequest(http.MethodGet, "/v1/health", nil))
	if api.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated API status=%d body=%s", api.Code, api.Body.String())
	}
}
