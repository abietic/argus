const state = {
  token: "",
  page: "overview",
};

const content = document.querySelector("#content");
const notice = document.querySelector("#notice");
const title = document.querySelector("#page-title");
const tokenInput = document.querySelector("#token-input");
const connectButton = document.querySelector("#connect-button");
const disconnectButton = document.querySelector("#disconnect-button");
const connectionDot = document.querySelector("#connection-dot");
const connectionLabel = document.querySelector("#connection-label");

const pageTitles = {
  overview: "运行概览",
  jobs: "评审任务",
  reviews: "评审运行",
  config: "配置治理",
  dashboard: "收益看板",
  evaluation: "评测数据",
  training: "训练治理",
};

class ApiError extends Error {
  constructor(status, code, message) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

function element(tag, options = {}, children = []) {
  const value = document.createElement(tag);
  if (options.className) value.className = options.className;
  if (options.text !== undefined) value.textContent = String(options.text);
  if (options.type) value.type = options.type;
  if (options.placeholder) value.placeholder = options.placeholder;
  if (options.value !== undefined) value.value = String(options.value);
  if (options.hidden) value.hidden = true;
  if (options.disabled) value.disabled = true;
  if (options.title) value.title = options.title;
  if (options.min !== undefined) value.min = String(options.min);
  if (options.max !== undefined) value.max = String(options.max);
  if (options.step !== undefined) value.step = String(options.step);
  if (options.onClick) value.addEventListener("click", options.onClick);
  for (const child of children) {
    if (child !== null && child !== undefined) value.append(child);
  }
  return value;
}

function clearContent(...children) {
  content.replaceChildren(...children);
}

function showNotice(message, success = false) {
  notice.textContent = message;
  notice.className = success ? "notice success" : "notice";
  notice.hidden = false;
}

function hideNotice() {
  notice.hidden = true;
  notice.textContent = "";
}

function setConnected(connected, profileRevision = "") {
  connectionDot.classList.toggle("connected", connected);
  connectionLabel.textContent = connected
    ? `已连接 · ${profileRevision || "local"}`
    : "未连接";
  connectButton.hidden = connected;
  disconnectButton.hidden = !connected;
  tokenInput.hidden = connected;
}

async function api(path, options = {}, tokenOverride = "") {
  const headers = new Headers(options.headers || {});
  headers.set("Authorization", `Bearer ${tokenOverride || state.token}`);
  if (options.body !== undefined) headers.set("Content-Type", "application/json");
  const response = await fetch(path, {
    method: options.method || "GET",
    headers,
    body: options.body === undefined ? undefined : JSON.stringify(options.body),
    credentials: "omit",
    cache: "no-store",
  });
  let payload;
  try {
    payload = await response.json();
  } catch {
    throw new ApiError(response.status, "invalid_response", "服务返回了无法解析的响应");
  }
  if (!response.ok) {
    if (response.status === 401 && !tokenOverride) disconnect("认证已失效，请重新连接");
    throw new ApiError(response.status, payload.code || "request_failed", payload.message || "请求失败");
  }
  return payload.data;
}

async function safeApi(path) {
  try {
    return { ok: true, data: await api(path) };
  } catch (error) {
    return { ok: false, error };
  }
}

async function connect() {
  hideNotice();
  const candidate = tokenInput.value;
  tokenInput.value = "";
  if (candidate.length < 32) {
    showNotice("token 至少需要 32 字节");
    return;
  }
  connectButton.disabled = true;
  try {
    const health = await api("/v1/health", {}, candidate);
    state.token = candidate;
    setConnected(true, health.profile_revision);
    await loadPage(state.page);
  } catch (error) {
    state.token = "";
    showNotice(error instanceof ApiError ? error.message : "无法连接本地 API");
  } finally {
    connectButton.disabled = false;
  }
}

function disconnect(message = "") {
  state.token = "";
  setConnected(false);
  clearContent(emptyState("连接本地 Argus API", "输入 bearer token 后读取受权限保护的平台数据。"));
  if (message) showNotice(message);
}

function emptyState(heading, message) {
  return element("div", { className: "empty-state" }, [
    element("span", { className: "empty-icon", text: "◎" }),
    element("h2", { text: heading }),
    element("p", { text: message }),
  ]);
}

function compactEmptyState(heading, message) {
	const state = emptyState(heading, message);
	state.classList.add("compact");
	return state;
}

function loadingState() {
  return emptyState("正在读取权威投影", "请稍候…");
}

function errorCard(label, error) {
  const message = error instanceof ApiError ? `${error.status} · ${error.message}` : "读取失败";
  return element("article", { className: "card" }, [
    element("h3", { text: label }),
    element("p", { className: "muted", text: message }),
  ]);
}

function metricCard(label, value, note) {
  return element("article", { className: "card" }, [
    element("h3", { text: label }),
    element("p", { className: "metric-value", text: value }),
    element("p", { className: "metric-note", text: note }),
  ]);
}

function statusBadge(value) {
  return element("span", { className: `badge ${String(value || "").toLowerCase()}`, text: value || "unknown" });
}

function formatTime(value) {
  if (!value) return "—";
  const date = new Date(value);
  return Number.isNaN(date.valueOf()) ? String(value) : date.toLocaleString("zh-CN", { hour12: false });
}

function shortID(value, maximum = 34) {
  const text = String(value || "—");
  return text.length <= maximum ? text : `${text.slice(0, maximum - 8)}…${text.slice(-7)}`;
}

function jsonView(value) {
  return element("pre", { className: "json-view", text: JSON.stringify(value, null, 2) });
}

function sectionHeader(label, action = null) {
  return element("div", { className: "section-head" }, [
    element("h2", { text: label }),
    action,
  ]);
}

function dataTable(columns, rows, onSelect = null) {
  const head = element("thead", {}, [
    element("tr", {}, columns.map((column) => element("th", { text: column.label }))),
  ]);
  const body = element("tbody");
  for (const row of rows) {
    const tr = element("tr");
    if (onSelect) {
      tr.dataset.action = "select";
      tr.tabIndex = 0;
      tr.addEventListener("click", () => onSelect(row));
      tr.addEventListener("keydown", (event) => {
        if (event.key === "Enter" || event.key === " ") onSelect(row);
      });
    }
    for (const column of columns) {
      const rendered = column.render(row);
      tr.append(element("td", {}, [rendered instanceof Node ? rendered : document.createTextNode(String(rendered))]));
    }
    body.append(tr);
  }
  return element("div", { className: "card" }, [element("table", {}, [head, body])]);
}

async function loadPage(page) {
  state.page = page;
  title.textContent = pageTitles[page] || "Argus";
  document.querySelectorAll(".nav-item").forEach((item) => {
    item.classList.toggle("active", item.dataset.page === page);
  });
  hideNotice();
  if (!state.token) {
    clearContent(emptyState("连接本地 Argus API", "输入 bearer token 后读取受权限保护的平台数据。"));
    return;
  }
  clearContent(loadingState());
  try {
    if (page === "overview") await renderOverview();
    if (page === "jobs") await renderJobs();
    if (page === "reviews") await renderReviews();
    if (page === "config") await renderConfig();
    if (page === "dashboard") await renderDashboards();
    if (page === "evaluation") await renderEvaluation();
    if (page === "training") await renderTraining();
  } catch (error) {
    clearContent(errorCard(pageTitles[page], error));
  }
}

async function renderOverview() {
  const observedAt = new Date().toISOString();
  const [jobs, runs, configs, dashboards, cases, pressure] = await Promise.all([
    safeApi("/v1/review-jobs?limit=5"),
    safeApi("/v1/review-runs?limit=5"),
    safeApi("/v1/config/revisions?limit=5"),
    safeApi("/v1/dashboard/snapshots?limit=5"),
    safeApi("/v1/evaluation/cases?limit=5"),
    safeApi(`/v1/workloads/pressure?at=${encodeURIComponent(observedAt)}`),
  ]);
  const sources = [
    ["评审任务", jobs, "个近期异步任务"],
    ["近期评审", runs, "个可见运行"],
    ["配置修订", configs, "个近期修订"],
    ["看板快照", dashboards, "个不可变快照"],
    ["评测 Case", cases, "个近期 Case"],
  ];
  const metricGrid = element("div", { className: "grid metrics" }, sources.map(([label, result, note]) => {
    if (!result.ok) return metricCard(label, "—", result.error.status === 403 ? "当前 principal 无权限" : "服务不可用");
    return metricCard(label, result.data.items.length, note);
  }));
  const recent = runs.ok
    ? reviewTable(runs.data.items, (entry) => showReviewRun(entry.run_id))
    : errorCard("近期评审", runs.error);
  const pressureContent = pressure.ok
    ? workloadPressureView(pressure.data)
    : errorCard("Workload Pressure", pressure.error);
  const refresh = element("button", { className: "ghost", text: "刷新", onClick: () => renderOverview() });
  clearContent(metricGrid, sectionHeader("调度背压", refresh), pressureContent,
    sectionHeader("近期评审运行"), recent);
}

function workloadPressureView(snapshot) {
  const global = snapshot.global || {};
  const stale = snapshot.stale || {};
  const metrics = element("div", { className: "grid metrics" }, [
    metricCard("容量状态", global.state || "unknown", `policy ${shortID(snapshot.policy_revision, 24)}`),
    metricCard("排队", `${global.queue_depth || 0} / ${global.queue_limit || 0}`, `最久等待 ${global.oldest_pending_wait_ms || 0} ms`),
    metricCard("执行中", `${global.active_depth || 0} / ${global.active_limit || 0}`, `ledger sequence ${snapshot.ledger_sequence || 0}`),
    metricCard("待协调", (stale.pending_requires_reconcile || 0) + (stale.active_requires_reconcile || 0), "查询不会隐式 reconcile"),
  ]);
  const classes = dataTable([
    { label: "Workload Class", render: (item) => element("span", { className: "mono", text: item.class }) },
    { label: "状态", render: (item) => statusBadge(item.capacity?.state) },
    { label: "Queue", render: (item) => `${item.capacity?.queue_depth || 0} / ${item.capacity?.queue_limit || 0}` },
    { label: "Active", render: (item) => `${item.capacity?.active_depth || 0} / ${item.capacity?.active_limit || 0}` },
    { label: "Priority", render: (item) => item.base_priority },
  ], snapshot.classes || []);
  return element("div", {}, [metrics, classes]);
}

function jobTable(items, onSelect) {
  if (!items.length) return emptyState("暂无评审任务", "提交不可变目标后，任务会脱离 HTTP 请求持续执行。");
  return dataTable([
    { label: "Job", render: (item) => element("span", { className: "mono", text: shortID(item.job_id) }) },
    { label: "模式", render: (item) => item.request?.mode || "—" },
    { label: "状态", render: (item) => statusBadge(item.workload?.state) },
    { label: "原因", render: (item) => item.workload?.state_reason || "—" },
    { label: "提交时间", render: (item) => formatTime(item.submitted_at) },
  ], items, onSelect);
}

async function renderJobs() {
  const page = await api("/v1/review-jobs?limit=50");
  const create = element("button", { className: "primary", text: "提交评审", onClick: showJobCreate });
  clearContent(sectionHeader("Durable Review Jobs", create), jobTable(page.items, (record) => showJob(record.job_id)));
  if (page.next_cursor) {
    content.append(element("div", { className: "action-row" }, [
      element("button", {
        className: "action", text: "读取下一页", onClick: async () => {
          const next = await api(`/v1/review-jobs?limit=50&cursor=${encodeURIComponent(page.next_cursor)}`);
          content.append(jobTable(next.items, (record) => showJob(record.job_id)));
        },
      }),
    ]));
  }
}

function showJobCreate() {
  const deterministicRequest = {
    schema_version: "argus.review_job_request.v1alpha1",
    execution_profile: "deterministic_review_v1",
    repository_path: "/absolute/path/to/repository",
    mode: "diff",
    base_revision: "main",
    head_revision: "HEAD",
    selection_ranges: null,
    include: null,
    exclude: null,
    execution_timeout_seconds: 1800,
  };
  const formalRequest = {
    schema_version: "argus.review_job_request.v1alpha1",
    execution_profile: "formal_pi_review_v1",
    source_run_id: "run-replace-with-succeeded-source",
    execution_timeout_seconds: 1800,
  };
  const editor = element("textarea", { value: JSON.stringify(deterministicRequest, null, 2) });
  const auditInput = element("input", { value: "submit immutable review job via local UI" });
  const submit = element("button", {
    className: "primary", text: "提交 Job", onClick: async () => {
      try {
        const request = JSON.parse(editor.value);
        const record = await api("/v1/review-jobs", {
          method: "POST",
          body: {
            schema_version: "argus.local_api_review_job_submit_command.v1alpha1",
            request,
            mutation: mutation("ui-review-job-submit", auditInput.value),
          },
        });
        showNotice(`${record.job_id} 已持久化并进入调度`, true);
        await showJob(record.job_id);
      } catch (error) {
        showNotice(error.message || "评审请求 JSON 无效");
      }
    },
  });
  const back = element("button", { className: "ghost", text: "返回任务", onClick: () => renderJobs() });
  clearContent(sectionHeader("提交异步评审", back), element("article", { className: "card" }, [
    element("p", { className: "muted", text: "deterministic_review_v1 直接采集目标；formal_pi_review_v1 只接受已成功 source_run_id。两者均在提交时冻结配置，formal 还冻结 Pi runtime/component 摘要与定价上限。" }),
    element("div", { className: "action-row" }, [
      element("button", { className: "action", text: "目标评审模板", onClick: () => { editor.value = JSON.stringify(deterministicRequest, null, 2); } }),
      element("button", { className: "action", text: "Formal Pi 模板", onClick: () => { editor.value = JSON.stringify(formalRequest, null, 2); } }),
    ]),
    field("ReviewJob Request JSON", editor),
    element("div", { className: "form-grid" }, [field("审计说明", auditInput, "wide")]),
    element("div", { className: "action-row" }, [submit]),
  ]));
}

async function showJob(jobID) {
  clearContent(loadingState());
  const [record, timeline] = await Promise.all([
    api(`/v1/review-jobs/${jobID}`),
    api(`/v1/review-jobs/${jobID}/timeline?limit=200`),
  ]);
  const stateValue = record.workload?.state || "unknown";
  const cancelable = ["pending", "leased", "unknown"].includes(stateValue);
  const cancelJob = async () => {
    try {
      await api(`/v1/review-jobs/${jobID}/cancel`, {
        method: "POST",
        body: {
          schema_version: "argus.local_api_review_job_cancel_command.v1alpha1",
          reason: "operator canceled through local UI",
          mutation: mutation("ui-review-job-cancel", "cancel local review job via operator UI"),
        },
      });
      showNotice(`${jobID} 已取消并建立永久 fence`, true);
      await showJob(jobID);
    } catch (error) {
      showNotice(error.message || "取消任务失败");
    }
  };
  const actions = element("div", { className: "action-row" }, [
    element("button", { className: "action", text: "刷新", onClick: () => showJob(jobID) }),
    element("button", { className: "action", text: "取消", disabled: !cancelable, onClick: cancelJob }),
    element("button", { className: "primary", text: "查看 Run", disabled: !record.run, onClick: () => showReviewRun(record.run_id) }),
  ]);
  const back = element("button", { className: "ghost", text: "返回任务", onClick: () => renderJobs() });
  const timelineTable = dataTable([
    { label: "Seq", render: (item) => element("span", { className: "mono", text: item.sequence }) },
    { label: "事件", render: (item) => statusBadge(item.type) },
    { label: "执行坐标 / 原因", render: (item) => timelineEventSummary(item) },
    { label: "时间", render: (item) => formatTime(item.occurred_at) },
  ], timeline.items || []);
  clearContent(sectionHeader(`Job · ${shortID(jobID, 60)}`, back), element("div", { className: "grid metrics" }, [
    metricCard("状态", stateValue, record.workload?.state_reason || "—"),
    metricCard("Generation", record.workload?.generation || 0, "fencing generation"),
    metricCard("Run", shortID(record.run_id, 24), record.run ? record.run.status : "尚无 terminal authority"),
    metricCard("Profile", record.execution_profile, "冻结执行 profile"),
  ]), actions, sectionHeader("调度生命周期"), timelineTable,
  sectionHeader("不可变命令投影"), jsonView(record));
}

function timelineEventSummary(item) {
  if (item.admission) return `${item.admission.decision} · ${item.admission.reason}`;
  if (item.lease) return `attempt ${item.lease.attempt} / generation ${item.lease.generation} · ${item.lease.worker_id}`;
  if (item.heartbeat) return `generation ${item.heartbeat.heartbeat.generation} · lease → ${formatTime(item.heartbeat.expires_at)}`;
  if (item.callback) return `${item.callback.callback.status} · ${item.callback.reason || item.callback.callback.failure_code || "accepted"}`;
  if (item.cancellation) return `cancel · ${item.cancellation.reason}`;
  if (item.reconciled?.length) return item.reconciled.map((action) => `${action.type} g${action.generation}`).join(", ");
  return item.audit || "—";
}

function reviewTable(items, onSelect) {
  if (!items.length) return emptyState("暂无评审运行", "通过 CLI 或后续 durable execution API 创建运行。 ");
  return dataTable([
    { label: "Run", render: (item) => element("span", { className: "mono", text: shortID(item.run_id) }) },
    { label: "类型", render: (item) => item.kind },
    { label: "状态", render: (item) => statusBadge(item.status) },
    { label: "最后事件", render: (item) => item.last_event_type },
    { label: "时间", render: (item) => formatTime(item.last_event_time) },
  ], items, onSelect);
}

async function renderReviews() {
  const page = await api("/v1/review-runs?limit=50");
  const refresh = element("button", { className: "ghost", text: "刷新", onClick: () => renderReviews() });
  clearContent(sectionHeader("权威运行历史", refresh), impactLookupPanel(), reviewTable(page.items, (entry) => showReviewRun(entry.run_id)));
  if (page.next_cursor) {
    content.append(element("div", { className: "action-row" }, [
      element("button", {
        className: "action", text: "读取下一页", onClick: async () => {
          const next = await api(`/v1/review-runs?limit=50&cursor=${encodeURIComponent(page.next_cursor)}`);
          content.append(reviewTable(next.items, (entry) => showReviewRun(entry.run_id)));
        },
      }),
    ]));
  }
}

function impactLookupPanel() {
  const kind = selectControl(["config_revision", "config_bundle", "rule_pack", "workflow", "model"], "model");
  const id = element("input", { placeholder: "组件 ID" });
  const revision = element("input", { placeholder: "精确 revision" });
  const sha256 = element("input", { placeholder: "可选 SHA-256" });
  const results = element("div", { className: "impact-results" });
  const lookup = element("button", {
    className: "action", text: "反查 ReviewRun", onClick: async () => {
      const query = new URLSearchParams({ kind: kind.value, id: id.value, revision: revision.value, limit: "50" });
      if (sha256.value) query.set("sha256", sha256.value);
      try {
        const page = await api(`/v1/review-run-impacts?${query.toString()}`);
        const coverage = element("p", {
          className: "muted",
          text: `watermark ${page.watermark} · resolved ${page.coverage.runs_resolved}/${page.coverage.history_entries} · gaps ${page.coverage.gaps.length}`,
        });
        const table = page.items.length ? reviewTable(page.items.map((item) => item.history), (entry) => showReviewRun(entry.run_id)) :
          emptyState("没有受影响的 ReviewRun", page.coverage.complete ? "冻结绑定中没有精确匹配。" : "存在未闭合 run，请检查 coverage gaps。");
        results.replaceChildren(coverage, table, jsonView(page.coverage));
      } catch (error) {
        showNotice(error.message || "反向影响查询失败");
      }
    },
  });
  return element("article", { className: "card impact-lookup" }, [
    element("h3", { text: "组件影响反查" }),
    element("p", { className: "muted", text: "按冻结的 exact ID/revision 查询；可选 SHA-256。缺失 ExecutionSnapshot 会显式进入 coverage gaps。" }),
    element("div", { className: "form-grid impact-fields" }, [
      field("组件类型", kind), field("组件 ID", id), field("Revision", revision), field("SHA-256", sha256),
    ]),
    element("div", { className: "action-row" }, [lookup]),
    results,
  ]);
}

async function showReviewRun(runID) {
  clearContent(loadingState());
  const detail = await api(`/v1/review-runs/${runID}`);
  const run = detail.run;
  const summary = element("article", { className: "card" }, [
    element("h2", { text: shortID(detail.history.run_id, 70) }),
    element("dl", { className: "detail-list" }, [
      element("dt", { text: "状态" }), statusBadge(detail.history.status),
      element("dt", { text: "是否已提交" }), element("dd", { text: detail.committed ? "是" : "否" }),
      element("dt", { text: "最后事件" }), element("dd", { text: detail.history.last_event_type }),
      element("dt", { text: "最后时间" }), element("dd", { text: formatTime(detail.history.last_event_time) }),
      element("dt", { text: "目标模式" }), element("dd", { text: run?.target_mode || "—" }),
      element("dt", { text: "仓库" }), element("dd", { className: "mono", text: run?.repository_path || "—" }),
      element("dt", { text: "Revision" }), element("dd", { className: "mono", text: run ? `${run.base_revision} → ${run.head_revision}` : "—" }),
    ]),
  ]);
  const back = element("button", { className: "ghost", text: "返回列表", onClick: () => renderReviews() });
  const findings = detail.report?.findings || detail.governed_report?.findings || [];
  clearContent(sectionHeader("评审详情", back), summary);
  if (findings.length) {
    content.append(sectionHeader(`Findings · ${findings.length}`));
    content.append(dataTable([
      { label: "Finding", render: (item) => element("span", { className: "mono", text: shortID(item.id || item.finding_id) }) },
      { label: "位置", render: (item) => `${item.path || "—"}:${item.start_line || item.line || "—"}` },
      { label: "规则", render: (item) => item.rule_id || item.category || "—" },
      { label: "说明", render: (item) => item.message || item.title || item.summary || "—" },
    ], findings, (item) => showFinding(runID, item.id || item.finding_id)));
  } else {
    content.append(emptyState(detail.committed ? "没有 Finding" : "运行尚未提交", detail.committed ? "当前结果未产生可发布 Finding。" : "完成后可查看结构化结果。"));
  }
}

async function showFinding(runID, findingID) {
  clearContent(loadingState());
  const detail = await api(`/v1/review-runs/${runID}/findings/${findingID}`);
  const finding = detail.finding || detail.governed_finding || {};
  const counts = element("div", { className: "grid metrics" }, [
    metricCard("初始决策", detail.decisions.length + detail.governed_decisions.length, "model / governed facts"),
    metricCard("人类决策", detail.human_decisions.length, "append-only decisions"),
    metricCard("反馈", detail.feedback.length, "operator / code-host facts"),
    metricCard("结果", detail.outcomes.length, "fixed / recurred / escaped"),
  ]);
  const back = element("button", { className: "ghost", text: "返回运行", onClick: () => showReviewRun(runID) });
  const writeFacts = findingFactWriter(runID, findingID);
  clearContent(sectionHeader(`Finding · ${shortID(findingID, 60)}`, back), counts,
    sectionHeader("追加评审事实"), writeFacts,
    sectionHeader("结构化事实"), jsonView({
      finding,
      decisions: detail.decisions,
      governed_decisions: detail.governed_decisions,
      human_decisions: detail.human_decisions,
      feedback: detail.feedback,
      outcomes: detail.outcomes,
    }));
}

function selectControl(values, selected = "") {
  const select = element("select");
  for (const value of values) {
    const option = element("option", { value, text: value });
    option.selected = value === selected;
    select.append(option);
  }
  return select;
}

function findingFactWriter(runID, findingID) {
  const decisionAction = selectControl(["human_review", "reject", "publish"], "human_review");
  const decisionReason = element("input", { value: "operator_reviewed_evidence" });
  const decisionEvidence = element("input", { value: `finding-${findingID}` });
  const decisionAudit = element("input", { value: "append finding decision via local operator UI" });

  const feedbackAction = selectControl(["accept", "dismiss", "wont_fix", "outdated", "needs_discussion"], "needs_discussion");
  const feedbackEvidence = element("input", { value: `feedback-observation-${findingID}` });
  const feedbackPrior = element("input", { placeholder: "修正既有反馈时填写" });
  const feedbackAudit = element("input", { value: "append feedback fact via local operator UI" });

  const outcomeState = selectControl(["fixed", "recurred", "escaped", "unknown"], "unknown");
  const outcomeEvidence = element("input", { value: `outcome-observation-${findingID}` });
  const outcomePrior = element("input", { placeholder: "修正既有结果时填写" });
  const outcomeAudit = element("input", { value: "append outcome fact via local operator UI" });

  const append = async (kind) => {
    const occurredAt = new Date();
    let path;
    let body;
    if (kind === "decision") {
      path = `/v1/review-runs/${runID}/findings/${findingID}/decisions`;
      body = {
        schema_version: "argus.local_api_finding_decision_write_command.v1alpha1",
        action: decisionAction.value,
        reason_code: decisionReason.value,
        evidence_refs: [{ authority: "argus-local-ui", id: decisionEvidence.value }],
        occurred_at: occurredAt.toISOString(),
        mutation: mutation("ui-finding-decision", decisionAudit.value),
      };
    } else if (kind === "feedback") {
      path = `/v1/review-runs/${runID}/findings/${findingID}/feedback`;
      body = {
        schema_version: "argus.local_api_feedback_write_command.v1alpha1",
        feedback_id: `feedback-ui-${crypto.randomUUID()}`,
        action: feedbackAction.value,
        source: { kind: "user_interface", id: "argus-local-operator" },
        occurred_at: occurredAt.toISOString(),
        source_refs: [{ kind: "manual_observation", authority: "argus-local-ui", id: feedbackEvidence.value }],
        mutation: mutation("ui-finding-feedback", feedbackAudit.value),
      };
      if (feedbackPrior.value) body.prior_feedback_id = feedbackPrior.value;
    } else {
      const windowStart = new Date(occurredAt.valueOf() - 24 * 60 * 60 * 1000);
      const windowEnd = new Date(occurredAt.valueOf() + 1000);
      path = `/v1/review-runs/${runID}/findings/${findingID}/outcomes`;
      body = {
        schema_version: "argus.local_api_outcome_write_command.v1alpha1",
        outcome_id: `outcome-ui-${crypto.randomUUID()}`,
        state: outcomeState.value,
        source: { kind: "user_interface", id: "argus-local-operator" },
        occurred_at: occurredAt.toISOString(),
        attribution_window: { start: windowStart.toISOString(), end: windowEnd.toISOString() },
        source_refs: [{ kind: "manual_observation", authority: "argus-local-ui", id: outcomeEvidence.value }],
        mutation: mutation("ui-finding-outcome", outcomeAudit.value),
      };
      if (outcomePrior.value) body.prior_outcome_id = outcomePrior.value;
    }
    try {
      await api(path, { method: "POST", body });
      showNotice(`${kind} 事实已追加`, true);
      await showFinding(runID, findingID);
    } catch (error) {
      showNotice(error.message || `${kind} 写入失败`);
    }
  };

  return element("div", { className: "grid three" }, [
    element("article", { className: "card fact-writer" }, [
      element("h3", { text: "Decision" }),
      field("行动", decisionAction),
      field("原因代码", decisionReason),
      field("证据 ID", decisionEvidence),
      field("审计说明", decisionAudit),
      element("div", { className: "action-row" }, [
        element("button", { className: "action", text: "追加 Decision", onClick: () => append("decision") }),
      ]),
    ]),
    element("article", { className: "card fact-writer" }, [
      element("h3", { text: "Feedback" }),
      field("动作", feedbackAction),
      field("观察证据 ID", feedbackEvidence),
      field("Prior Feedback ID", feedbackPrior),
      field("审计说明", feedbackAudit),
      element("div", { className: "action-row" }, [
        element("button", { className: "action", text: "追加 Feedback", onClick: () => append("feedback") }),
      ]),
    ]),
    element("article", { className: "card fact-writer" }, [
      element("h3", { text: "Outcome" }),
      field("状态", outcomeState),
      field("观察证据 ID", outcomeEvidence),
      field("Prior Outcome ID", outcomePrior),
      field("审计说明", outcomeAudit),
      element("div", { className: "action-row" }, [
        element("button", { className: "action", text: "追加 Outcome", onClick: () => append("outcome") }),
      ]),
    ]),
  ]);
}

function configTable(items, onSelect) {
  if (!items.length) return emptyState("暂无配置修订", "创建 draft 后再进行 validate 与 publish。 ");
  return dataTable([
    { label: "配置", render: (item) => element("span", { className: "mono", text: `${item.revision.id}@${item.revision.revision}` }) },
    { label: "Scope", render: (item) => item.revision.scope },
    { label: "状态", render: (item) => statusBadge(item.status) },
    { label: "更新人", render: (item) => item.updated_by },
    { label: "更新时间", render: (item) => formatTime(item.updated_at) },
  ], items, onSelect);
}

async function renderConfig() {
  const page = await api("/v1/config/revisions?limit=50");
  const create = element("button", { className: "primary", text: "创建修订", onClick: showConfigCreate });
	const resolve = element("button", { className: "action", text: "解析生效配置", onClick: showConfigResolution });
	const publishComponent = element("button", { className: "action", text: "发布 Agent 组件", onClick: showAgentComponentPublish });
	const actions = element("div", { className: "action-row" }, [publishComponent, resolve, create]);
  clearContent(sectionHeader("配置修订", actions), configTable(page.items, (record) => showConfigRevision(record.revision.id, record.revision.revision)));
}

async function canonicalBase64AndSHA256(text) {
	const bytes = new TextEncoder().encode(text);
	let binary = "";
	for (let offset = 0; offset < bytes.length; offset += 0x8000) {
		binary += String.fromCharCode(...bytes.subarray(offset, offset + 0x8000));
	}
	const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", bytes));
	return {
		content_base64: btoa(binary),
		sha256: [...digest].map((value) => value.toString(16).padStart(2, "0")).join(""),
	};
}

function showAgentComponentPublish() {
	const baseline = element("input", { value: "review-baseline-replace-me" });
	const contract = selectControl([
		"argus.prompt_bundle.v1alpha1",
		"argus.skill_pack.v1alpha1",
		"argus.knowledge_pack.v1alpha1",
	], "argus.knowledge_pack.v1alpha1");
	const componentID = element("input", { value: "payments-domain" });
	const revision = element("input", { value: "refund-invariants-v1" });
	const componentContent = element("textarea", {
		value: "# Payments\n\nRefunds require a committed ledger transition.\n",
	});
	const audit = element("input", { value: "publish exact governed Agent component via local UI" });
	const publish = element("button", {
		className: "primary", text: "发布不可变组件", onClick: async () => {
			try {
				const encoded = await canonicalBase64AndSHA256(componentContent.value);
				const result = await api("/v1/agent-components", {
					method: "POST",
					body: {
						schema_version: "argus.local_api_agent_component_publish_command.v1alpha1",
						baseline_review_run_id: baseline.value,
						contract: contract.value,
						ref: { id: componentID.value, revision: revision.value, sha256: encoded.sha256 },
						content_base64: encoded.content_base64,
						mutation: mutation("ui-agent-component-publish", audit.value),
					},
				});
				showNotice(`${result.binding.ref.id}@${result.binding.ref.revision} 已发布`, true);
				clearContent(
					sectionHeader("Agent 组件已发布", element("button", { className: "ghost", text: "返回配置", onClick: () => renderConfig() })),
					jsonView(result),
				);
			} catch (error) {
				showNotice(error.message || "Agent 组件发布失败");
			}
		},
	});
	const back = element("button", { className: "ghost", text: "返回配置", onClick: () => renderConfig() });
	clearContent(sectionHeader("发布 Agent 组件", back), element("article", { className: "card" }, [
		element("p", { className: "muted", text: "subject 只从已提交 baseline ReviewRun 推导；页面上传 UTF-8 内容并在内存中计算 exact SHA-256/base64，不接受 tenant、repository 或本地文件路径。prompt 内容必须是严格 PromptBundle JSON，且 ref revision 与 bundle revision 一致。" }),
		element("div", { className: "form-grid" }, [
			field("Baseline ReviewRun", baseline, "wide"), field("Contract", contract),
			field("组件 ID", componentID), field("Revision", revision),
			field("UTF-8 组件内容", componentContent, "wide"), field("审计说明", audit, "wide"),
		]),
		element("div", { className: "action-row" }, [publish]),
	]));
}

function showConfigResolution() {
	const tenant = element("input", { value: "local" });
	const organization = element("input", { value: "local" });
	const repository = element("input", { value: "repository-local" });
	const path = element("input", { value: "" });
	const invocation = element("input", { value: `ui-resolution-${crypto.randomUUID()}` });
	const resolve = element("button", {
		className: "primary", text: "解析", onClick: async () => {
			try {
				const result = await api("/v1/config/resolutions", {
					method: "POST",
					body: {
						schema_version: "argus.local_api_config_resolution_query.v1alpha1",
						context: {
							tenant_id: tenant.value, organization_id: organization.value,
							repository_id: repository.value, path: path.value,
							invocation_id: invocation.value,
						},
					},
				});
				clearContent(sectionHeader("生效配置", element("button", { className: "ghost", text: "返回配置", onClick: () => renderConfig() })), jsonView(result));
			} catch (error) {
				showNotice(error.message || "配置解析失败");
			}
		},
	});
	const back = element("button", { className: "ghost", text: "返回配置", onClick: () => renderConfig() });
	clearContent(sectionHeader("解析生效配置", back), element("article", { className: "card" }, [
		element("div", { className: "form-grid" }, [
			field("Tenant", tenant), field("Organization", organization),
			field("Repository", repository), field("Path", path),
			field("Invocation ID", invocation, "wide"),
		]),
		element("div", { className: "action-row" }, [resolve]),
	]));
}

async function showConfigRevision(id, revision) {
  clearContent(loadingState());
  const detail = await api(`/v1/config/revisions/${id}/${revision}`);
  const status = detail.record.status;
  const auditInput = element("input", { value: "operator lifecycle transition via local UI" });
  const percentageInput = element("input", { type: "number", value: 100, min: 1, max: 100 });
  const seedInput = element("input", { placeholder: "灰度低于 100% 时必填" });
  const runTransition = async (action) => {
    const command = {
      schema_version: "argus.local_api_config_transition_command.v1alpha1",
      mutation: mutation(`ui-config-${action}`, auditInput.value),
    };
    if (action === "publish") {
      const percentage = Number(percentageInput.value);
      command.rollout = { percentage };
      if (percentage < 100) command.rollout.seed = seedInput.value;
    }
    try {
      await api(`/v1/config/revisions/${id}/${revision}/${action}`, { method: "POST", body: command });
      showNotice(`${id}@${revision} 已执行 ${action}`, true);
      await showConfigRevision(id, revision);
    } catch (error) {
      showNotice(error.message || "配置转换失败");
    }
  };
  const controls = element("article", { className: "card" }, [
    element("h2", { text: `${id}@${revision}` }),
    element("div", { className: "form-grid" }, [
      field("审计说明", auditInput, "wide"),
      field("发布百分比", percentageInput),
      field("灰度 seed", seedInput),
    ]),
    element("div", { className: "action-row" }, [
      element("button", { className: "action", text: "Validate", disabled: status !== "draft", onClick: () => runTransition("validate") }),
      element("button", { className: "primary", text: "Publish", disabled: status !== "validated", onClick: () => runTransition("publish") }),
      element("button", { className: "action", text: "Rollback", disabled: status !== "published", onClick: () => runTransition("rollback") }),
    ]),
  ]);
  const back = element("button", { className: "ghost", text: "返回配置", onClick: () => renderConfig() });
  clearContent(sectionHeader("配置详情", back), element("div", { className: "grid two" }, [controls, jsonView(detail)]));
}

function field(label, control, extraClass = "") {
  return element("div", { className: `field ${extraClass}`.trim() }, [element("label", { text: label }), control]);
}

function mutation(prefix, audit) {
  return {
    schema_version: "argus.local_api_mutation.v1alpha1",
    idempotency_key: `${prefix}-${crypto.randomUUID()}`,
    audit,
    at: new Date().toISOString(),
  };
}

function showConfigCreate() {
  const defaultRevision = {
    schema_version: "argus.config_revision.v1alpha1",
    id: "platform-default",
    revision: "1",
    scope: "platform",
    selector: {},
    patch: { target: { max_files: 200 } },
  };
  const editor = element("textarea", { value: JSON.stringify(defaultRevision, null, 2) });
  const auditInput = element("input", { value: "create governed configuration revision via local UI" });
  const submit = element("button", {
    className: "primary", text: "创建 Draft", onClick: async () => {
      try {
        const revision = JSON.parse(editor.value);
        await api("/v1/config/revisions", {
          method: "POST",
          body: {
            schema_version: "argus.local_api_config_create_command.v1alpha1",
            revision,
            mutation: mutation("ui-config-create", auditInput.value),
          },
        });
        showNotice(`${revision.id}@${revision.revision} 已创建`, true);
        await renderConfig();
      } catch (error) {
        showNotice(error.message || "配置 JSON 无效");
      }
    },
  });
  const back = element("button", { className: "ghost", text: "返回配置", onClick: () => renderConfig() });
  clearContent(sectionHeader("创建配置修订", back), element("article", { className: "card" }, [
    field("Revision JSON", editor),
    element("div", { className: "form-grid" }, [field("审计说明", auditInput, "wide")]),
    element("div", { className: "action-row" }, [submit]),
  ]));
}

async function renderDashboards() {
  const page = await api("/v1/dashboard/snapshots?limit=50");
  const rows = page.items;
  clearContent(sectionHeader("不可变 Dashboard 快照"), rows.length ? dataTable([
    { label: "Snapshot", render: (item) => element("span", { className: "mono", text: shortID(item.snapshot_id) }) },
    { label: "Policy", render: (item) => item.policy_revision },
    { label: "窗口", render: (item) => `${formatTime(item.window?.start_inclusive)} → ${formatTime(item.window?.end_exclusive)}` },
    { label: "构建时间", render: (item) => formatTime(item.built_at) },
  ], rows, (item) => showDashboard(item.snapshot_id)) : emptyState("暂无看板快照", "先通过 dashboard rebuild 生成不可变 projection。"));
}

async function showDashboard(snapshotID) {
  clearContent(loadingState());
  const snapshot = await api(`/v1/dashboard/snapshots/${snapshotID}`);
  const back = element("button", { className: "ghost", text: "返回看板", onClick: () => renderDashboards() });
  clearContent(sectionHeader(`Snapshot · ${shortID(snapshotID, 60)}`, back), jsonView(snapshot));
}

const strictTrainingRedactionPolicy = {
  schema_version: "argus.training_strict_redaction_policy.v1alpha1",
  id: "argus-strict-text",
  revision: "1",
  rule_ids: [
    "assignment_secret", "aws_access_key", "bearer_credential", "email_address",
    "github_token", "ip_address", "jwt", "openai_anthropic_key", "pem_private_key",
    "slack_token", "url_userinfo", "user_home_path",
  ],
  replacement: "[ARGUS_REDACTED]",
  max_artifact_bytes: 1048576,
  sha256: "ca57b580c56c10e6a04cd2cf648dfc3b9a09a86cb24810cb2ff9922f83dad87c",
};

async function renderTraining() {
  const [manifestResult, exportResult, jobResult] = await Promise.all([
    safeApi("/v1/training/manifests?limit=50"),
    safeApi("/v1/training/exports?limit=50"),
    safeApi("/v1/training/jobs?limit=50"),
  ]);
  const manifests = manifestResult.ok ? manifestResult.data.items : [];
  const exports = exportResult.ok ? exportResult.data.items : [];
  const jobs = jobResult.ok ? jobResult.data.items : [];
  const terminalJobs = jobs.filter((item) => ["succeeded", "failed", "canceled"].includes(item.plan?.status)).length;
  const metrics = element("div", { className: "grid metrics" }, [
    metricCard("Dataset Manifests", manifestResult.ok ? manifests.length : "—", "独立治理标签与 exact case revision"),
    metricCard("Redacted Exports", exportResult.ok ? exports.length : "—", "严格内建策略与逐 artifact receipt"),
    metricCard("Training Jobs", jobResult.ok ? jobs.length : "—", "external_manual，不执行 provider 请求"),
    metricCard("Terminal Jobs", jobResult.ok ? terminalJobs : "—", "operator 记录，不能自动晋升"),
  ]);
  const create = element("button", { className: "primary", text: "物化训练清单", onClick: showTrainingMaterialize });
  const manifestsView = manifestResult.ok
    ? (manifests.length ? dataTable([
      { label: "Manifest", render: (item) => element("span", { className: "mono", text: shortID(item.manifest?.manifest_id) }) },
      { label: "Dataset", render: (item) => `${item.manifest?.dataset_id || "—"}@${item.manifest?.dataset_revision || "—"}` },
      { label: "Samples", render: (item) => item.manifest?.samples?.length || 0 },
      { label: "Content", render: (item) => item.manifest?.contains_source_bytes ? statusBadge("unsafe") : statusBadge("reference_only") },
      { label: "Created", render: (item) => formatTime(item.manifest?.created_at) },
    ], manifests, (item) => showTrainingManifest(item.manifest.manifest_id))
      : compactEmptyState("暂无训练清单", "仅 active + approved + train split + 独立裁决的 Case 才能物化。"))
    : errorCard("Training Manifests", manifestResult.error);
  const exportsView = exportResult.ok
    ? (exports.length ? dataTable([
      { label: "Export", render: (item) => element("span", { className: "mono", text: shortID(item.bundle?.export_id) }) },
      { label: "Manifest", render: (item) => element("span", { className: "mono", text: shortID(item.bundle?.manifest_id) }) },
      { label: "Samples", render: (item) => item.bundle?.samples?.length || 0 },
      { label: "Receipts", render: (item) => item.bundle?.receipts?.length || 0 },
      { label: "Policy", render: (item) => `${item.bundle?.policy?.id || "—"}@${item.bundle?.policy?.revision || "—"}` },
    ], exports, (item) => showTrainingExport(item.bundle.export_id))
      : compactEmptyState("暂无脱敏导出", "从 exact manifest 构建 content-addressed redacted bundle。"))
    : errorCard("Training Exports", exportResult.error);
  const jobsView = jobResult.ok
    ? (jobs.length ? dataTable([
      { label: "Job", render: (item) => element("span", { className: "mono", text: shortID(item.plan?.request?.job_id) }) },
      { label: "Export", render: (item) => element("span", { className: "mono", text: shortID(item.plan?.request?.export_id) }) },
      { label: "Provider / Model", render: (item) => `${item.plan?.request?.provider_id || "—"} / ${item.plan?.request?.base_model?.id || "—"}` },
      { label: "状态", render: (item) => statusBadge(item.plan?.status) },
      { label: "Authority", render: (item) => item.plan?.receipt_authority || "—" },
    ], jobs, (item) => showTrainingJob(item.plan.request.job_id))
      : compactEmptyState("暂无训练作业", "作业计划只冻结外部执行 intent，不会调用训练 provider。"))
    : errorCard("Training Jobs", jobResult.error);
  clearContent(
    metrics,
    element("article", { className: "card" }, [
      element("h2", { text: "安全边界" }),
      element("p", { className: "muted", text: "API 只构建脱敏 bundle；publish 仅允许 CLI 写到 store 外的显式目录。训练计划 remote side effects 固定 deny，回执 authority 固定 operator_recorded_unattested，promotion_eligible=false；本页面不会调用训练 provider。" }),
    ]),
    sectionHeader("训练数据清单", create), manifestsView,
    sectionHeader("脱敏导出历史"), exportsView,
    sectionHeader("外部训练作业"), jobsView,
  );
}

function showTrainingMaterialize() {
  const digestA = "a".repeat(64);
  const digestB = "b".repeat(64);
  const digestC = "c".repeat(64);
  const digestD = "d".repeat(64);
  const request = {
    schema_version: "argus.training_materialization_request.v1alpha1",
    dataset_id: "dataset-review-quality",
    dataset_revision: "revision-replace-me",
    repository_id: "repository-replace-me",
    config_bundle_ref: { uri: `artifact://local/sha256/${digestD}`, sha256: digestD, size_bytes: 1, contract: "argus.config_bundle.v1alpha1" },
    cases: [{
      case_id: "case-replace-me",
      expected_governance_revision: 1,
      expected_label_revision: 1,
      artifact_refs: [
        { uri: `artifact://local/sha256/${digestA}`, sha256: digestA, size_bytes: 1, contract: "argus.training_input_snapshot.v1alpha1" },
        { uri: `artifact://local/sha256/${digestB}`, sha256: digestB, size_bytes: 1, contract: "argus.training_source_evidence.v1alpha1" },
        { uri: `artifact://local/sha256/${digestC}`, sha256: digestC, size_bytes: 1, contract: "argus.training_adjudication_evidence.v1alpha1" },
      ],
      authority: {
        kind: "external_governance",
        reviewer_ids: ["reviewer-replace-me"],
        adjudicator_id: "adjudicator-replace-me",
        evidence_refs: [`artifact://local/sha256/${digestC}`],
        statement: "independently_adjudicated_not_argus_output",
      },
    }],
    created_at: new Date().toISOString(),
  };
  const editor = element("textarea", { value: JSON.stringify(request, null, 2) });
  const auditInput = element("input", { value: "materialize independently governed training dataset" });
  const submit = element("button", { className: "primary", text: "物化训练清单", onClick: async () => {
    try {
      const submitted = JSON.parse(editor.value);
      const submittedMutation = mutation("ui-training-materialize", auditInput.value);
      submitted.created_at = submittedMutation.at;
      const record = await api("/v1/training/manifests", { method: "POST", body: {
        schema_version: "argus.local_api_training_materialize_command.v1alpha1",
        request: submitted,
        mutation: submittedMutation,
      } });
      showNotice(`${record.manifest.manifest_id} 已物化`, true);
      await showTrainingManifest(record.manifest.manifest_id);
    } catch (error) {
      showNotice(error.message || "训练清单请求 JSON 无效");
    }
  } });
  const back = element("button", { className: "ghost", text: "返回训练治理", onClick: renderTraining });
  clearContent(sectionHeader("物化训练数据清单", back), element("article", { className: "card" }, [
    element("p", { className: "muted", text: "必须绑定已落盘的 exact ConfigBundle、Case revision 与 artifact refs。holdout、Argus 自标注、未授权训练用途或缺少独立裁决的 Case 都会被服务拒绝。created_at 在提交时绑定 mutation 时间。" }),
    field("Materialization Request JSON", editor),
    field("审计说明", auditInput),
    element("div", { className: "action-row" }, [submit]),
  ]));
}

async function showTrainingManifest(manifestID) {
  clearContent(loadingState());
  const record = await api(`/v1/training/manifest?manifest_id=${encodeURIComponent(manifestID)}`);
  const actions = element("div", { className: "action-row" }, [
    element("button", { className: "ghost", text: "返回训练治理", onClick: renderTraining }),
    element("button", { className: "primary", text: "构建脱敏导出", onClick: () => showTrainingExportBuild(record) }),
  ]);
  const summary = element("div", { className: "grid metrics" }, [
    metricCard("Dataset", `${record.manifest.dataset_id}@${record.manifest.dataset_revision}`, record.manifest.repository_id),
    metricCard("Samples", record.manifest.samples?.length || 0, record.manifest.content_mode),
    metricCard("Source Bytes", String(record.manifest.contains_source_bytes), "必须为 false"),
    metricCard("Self Labels", String(record.manifest.self_labels_allowed), "必须为 false"),
  ]);
  clearContent(sectionHeader(`Manifest · ${shortID(manifestID, 60)}`, actions), summary, jsonView(record));
}

function showTrainingExportBuild(manifestRecord) {
  const manifest = manifestRecord.manifest;
  const request = {
    schema_version: "argus.training_export_request.v1alpha1",
    export_id: `training-export-${crypto.randomUUID()}`,
    manifest_id: manifest.manifest_id,
    manifest_ref: manifestRecord.manifest_ref,
    policy: strictTrainingRedactionPolicy,
    created_at: new Date().toISOString(),
  };
  const editor = element("textarea", { value: JSON.stringify(request, null, 2) });
  const auditInput = element("input", { value: "build strict redacted training export bundle" });
  const submit = element("button", { className: "primary", text: "构建脱敏 Bundle", onClick: async () => {
    try {
      const submitted = JSON.parse(editor.value);
      const submittedMutation = mutation("ui-training-export-build", auditInput.value);
      submitted.created_at = submittedMutation.at;
      const record = await api("/v1/training/exports", { method: "POST", body: {
        schema_version: "argus.local_api_training_export_build_command.v1alpha1",
        request: submitted,
        mutation: submittedMutation,
      } });
      showNotice(`${record.bundle.export_id} 已构建；尚未发布到文件系统`, true);
      await showTrainingExport(record.bundle.export_id);
    } catch (error) {
      showNotice(error.message || "训练导出请求 JSON 无效");
    }
  } });
  const back = element("button", { className: "ghost", text: "返回清单", onClick: () => showTrainingManifest(manifest.manifest_id) });
  clearContent(sectionHeader("构建严格脱敏导出", back), element("article", { className: "card" }, [
    element("p", { className: "muted", text: "策略是封闭且版本化的内建 detector 集，服务会重读每个源 artifact、执行 residual check 并保存 receipt。API 只构建；publish 仅允许 CLI 显式写到 Argus store 之外，本页面不接收 output path。" }),
    field("Export Request JSON", editor),
    field("审计说明", auditInput),
    element("div", { className: "action-row" }, [submit]),
  ]));
}

async function showTrainingExport(exportID) {
  clearContent(loadingState());
  const record = await api(`/v1/training/export?export_id=${encodeURIComponent(exportID)}`);
  const actions = element("div", { className: "action-row" }, [
    element("button", { className: "ghost", text: "返回训练治理", onClick: renderTraining }),
    element("button", { className: "primary", text: "准备训练作业", onClick: () => showTrainingJobPrepare(record) }),
  ]);
  const summary = element("div", { className: "grid metrics" }, [
    metricCard("Samples", record.bundle.samples?.length || 0, record.bundle.portable_format),
    metricCard("Receipts", record.bundle.receipts?.length || 0, `policy ${record.bundle.policy.revision}`),
    metricCard("Unredacted Bytes", String(record.bundle.contains_unredacted_source_bytes), "必须为 false"),
    metricCard("Bundle SHA", shortID(record.bundle.sha256, 22), "content-addressed"),
  ]);
  clearContent(sectionHeader(`Export · ${shortID(exportID, 60)}`, actions), summary, jsonView(record));
}

function showTrainingJobPrepare(exportRecord) {
  const bundle = exportRecord.bundle;
  const request = {
    schema_version: "argus.training_job_prepare_request.v1alpha1",
    job_id: `training-job-${crypto.randomUUID()}`,
    export_id: bundle.export_id,
    export_bundle_ref: exportRecord.bundle_ref,
    provider_id: "provider-replace-me",
    provider_profile_revision: "profile-replace-me",
    base_model: { id: "base-model-replace-me", revision: "revision-replace-me", sha256: "a".repeat(64) },
    objective: "supervised_fine_tuning",
    hyperparameters: { epochs: 3, batch_size: 8, learning_rate_micros: 2000, seed: 42 },
    created_at: new Date().toISOString(),
  };
  const editor = element("textarea", { value: JSON.stringify(request, null, 2) });
  const auditInput = element("input", { value: "prepare frozen external manual training job" });
  const submit = element("button", { className: "primary", text: "准备作业", onClick: async () => {
    try {
      const submitted = JSON.parse(editor.value);
      const submittedMutation = mutation("ui-training-job-prepare", auditInput.value);
      submitted.created_at = submittedMutation.at;
      const record = await api("/v1/training/jobs", { method: "POST", body: {
        schema_version: "argus.local_api_training_job_prepare_command.v1alpha1",
        request: submitted,
        mutation: submittedMutation,
      } });
      showNotice(`${record.plan.request.job_id} 已准备；未调用 provider`, true);
      await showTrainingJob(record.plan.request.job_id);
    } catch (error) {
      showNotice(error.message || "训练作业请求 JSON 无效");
    }
  } });
  const back = element("button", { className: "ghost", text: "返回导出", onClick: () => showTrainingExport(bundle.export_id) });
  clearContent(sectionHeader("准备外部训练作业", back), element("article", { className: "card" }, [
    element("p", { className: "muted", text: "只冻结 exact export、provider profile、base model 与整数超参数。execution_mode=external_manual，remote side effects 固定 deny；不会调用训练 provider，也不会读取 provider 凭据。" }),
    field("Job Prepare Request JSON", editor),
    field("审计说明", auditInput),
    element("div", { className: "action-row" }, [submit]),
  ]));
}

async function sha256Hex(value) {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(value));
  return [...new Uint8Array(digest)].map((byte) => byte.toString(16).padStart(2, "0")).join("");
}

async function sealProviderJobReceipt(draft) {
  const receipt = {
    schema_version: "argus.training_provider_job_receipt.v1alpha1",
    provider_id: draft.provider_id,
    external_job_id: draft.external_job_id,
    status: draft.status,
  };
  if (draft.status === "succeeded") receipt.output_model_id = draft.output_model_id;
  receipt.authority = "operator_recorded_unattested";
  receipt.contains_secret = false;
  receipt.observed_at = draft.observed_at;
  receipt.sha256 = "";
  receipt.sha256 = await sha256Hex(JSON.stringify(receipt));
  return receipt;
}

async function showTrainingJob(jobID) {
  clearContent(loadingState());
  const record = await api(`/v1/training/job?job_id=${encodeURIComponent(jobID)}`);
  const actions = [element("button", { className: "ghost", text: "返回训练治理", onClick: renderTraining })];
  if (["prepared", "submitted"].includes(record.plan.status)) {
    actions.push(element("button", { className: "primary", text: "记录外部状态", onClick: () => showTrainingJobObserve(record) }));
  }
  const summary = element("div", { className: "grid metrics" }, [
    metricCard("状态", record.plan.status, record.plan.execution_mode),
    metricCard("Remote Effects", record.plan.remote_side_effects, "必须为 deny"),
    metricCard("Authority", record.plan.receipt_authority, "非 provider attestation"),
    metricCard("Promotion Eligible", String(record.plan.promotion_eligible), "固定 false"),
  ]);
  clearContent(sectionHeader(`Training Job · ${shortID(jobID, 60)}`, element("div", { className: "action-row" }, actions)), summary, jsonView(record));
}

function showTrainingJobObserve(jobRecord) {
  const plan = jobRecord.plan;
  const submittedReceipt = plan.observations?.[0]?.receipt;
  const statuses = plan.status === "prepared" ? ["submitted"] : ["succeeded", "failed", "canceled"];
  const statusInput = selectControl(statuses, statuses[0]);
  const externalJobInput = element("input", { value: submittedReceipt?.external_job_id || "external-job-replace-me", disabled: Boolean(submittedReceipt) });
  const outputModelInput = element("input", { value: "output-model-replace-me" });
  const outputModelField = field("Output Model ID（仅 succeeded）", outputModelInput);
  outputModelField.hidden = statuses[0] !== "succeeded";
  statusInput.addEventListener("change", () => { outputModelField.hidden = statusInput.value !== "succeeded"; });
  const auditInput = element("input", { value: plan.status === "prepared" ? "record external training submission" : "record terminal external training status" });
  const submit = element("button", { className: "primary", text: "密封并记录回执", onClick: async () => {
    try {
      const submittedMutation = mutation("ui-training-job-observe", auditInput.value);
      const receipt = await sealProviderJobReceipt({
        provider_id: plan.request.provider_id,
        external_job_id: externalJobInput.value,
        status: statusInput.value,
        output_model_id: outputModelInput.value,
        observed_at: submittedMutation.at,
      });
      const request = {
        schema_version: "argus.training_job_observation_request.v1alpha1",
        job_id: plan.request.job_id,
        expected_plan_sha256: plan.sha256,
        receipt,
        observed_at: submittedMutation.at,
      };
      const record = await api("/v1/training/job/observations", { method: "POST", body: {
        schema_version: "argus.local_api_training_job_observe_command.v1alpha1",
        request,
        mutation: submittedMutation,
      } });
      showNotice(`${record.plan.request.job_id} 已记录 ${record.plan.status}；回执未经 provider 证明`, true);
      await showTrainingJob(record.plan.request.job_id);
    } catch (error) {
      showNotice(error.message || "训练作业回执无效");
    }
  } });
  const back = element("button", { className: "ghost", text: "返回作业", onClick: () => showTrainingJob(plan.request.job_id) });
  clearContent(sectionHeader("记录外部训练状态", back), element("article", { className: "card" }, [
    element("p", { className: "muted", text: "本页只记录操作员从外部系统观察到的事实。回执在浏览器内按 canonical 字段顺序计算 SHA-256，并以 expected plan SHA 做 CAS；authority 固定 operator_recorded_unattested，promotion_eligible=false。" }),
    element("div", { className: "form-grid" }, [
      field("状态", statusInput),
      field("External Job ID", externalJobInput),
      outputModelField,
      field("审计说明", auditInput, "wide"),
    ]),
    element("div", { className: "action-row" }, [submit]),
  ]));
}

async function renderEvaluation() {
	const [casePage, evaluationPage, experimentPage, repeatabilityPage, experimentBatchPage, repeatabilityBatchPage, calibrationPage, promotionResult, observationResult, normalizationPromotionResult] = await Promise.all([
		api("/v1/evaluation/cases?limit=50"),
		api("/v1/evaluation/evaluation-runs?limit=50"),
		api("/v1/evaluation/experiment-runs?limit=50"),
		api("/v1/evaluation/repeatability-runs?limit=50"),
		api("/v1/evaluation/experiment-batches?limit=50"),
		api("/v1/evaluation/repeatability-batches?limit=50"),
		api("/v1/calibration/runs?limit=50"),
		safeApi("/v1/calibration/promotion/plans?limit=50"),
		safeApi("/v1/calibration/promotion/observations?limit=50"),
		safeApi("/v1/evaluation/normalization/promotions?limit=50"),
	]);
	const promotionPlans = promotionResult.ok ? promotionResult.data.items : [];
	const promotionObservations = observationResult.ok ? observationResult.data.items : [];
	const normalizationPromotions = normalizationPromotionResult.ok ? normalizationPromotionResult.data.items : [];
	const runMetrics = element("div", { className: "grid metrics" }, [
		metricCard("Evaluation Runs", evaluationPage.items.length, "case 结果事实"),
		metricCard("Experiment Runs", experimentPage.items.length, "单变量质量/成本比较"),
		metricCard("Repeatability Runs", repeatabilityPage.items.length, "稳定性事实，不等同正确性"),
		metricCard("Replay Batches", experimentBatchPage.items.length + repeatabilityBatchPage.items.length, "可恢复执行状态"),
		metricCard("Calibration Runs", calibrationPage.items.length, "候选 profile，不自动发布"),
		metricCard("Promotion Plans", promotionResult.ok ? promotionPlans.length : "—", promotionResult.ok ? "受控配置生命周期" : "需要 evaluation + config 权限"),
		metricCard("Quality Windows", observationResult.ok ? promotionObservations.length : "—", observationResult.ok ? "上线前后 exact snapshot 比较" : "需要 dashboard + evaluation + config 权限"),
		metricCard("Normalization Gates", normalizationPromotionResult.ok ? normalizationPromotions.length : "—", normalizationPromotionResult.ok ? "独立 oracle 的 test/holdout 门禁" : "需要 evaluation 权限"),
	]);
	const normalizationPromotionRows = normalizationPromotionResult.ok && normalizationPromotions.length ? dataTable([
		{ label: "Variant", render: (item) => element("span", { className: "mono", text: shortID(item.variant?.variant_id) }) },
		{ label: "Policy", render: (item) => item.variant?.managed_binding?.normalization_policy_revision || "—" },
		{ label: "状态", render: (item) => statusBadge(item.status) },
		{ label: "Next gate", render: (item) => item.next_gate || "—" },
		{ label: "Gates", render: (item) => item.gates?.length || 0 },
	], normalizationPromotions, (item) => showNormalizationPromotion(item.variant.variant_id))
		: (normalizationPromotionResult.ok ? compactEmptyState("暂无 Normalization Promotion", "先用独立 test corpus 生成 exact quality artifact，再准备受治理门禁。") : errorCard("Normalization Promotion", normalizationPromotionResult.error));
	const calibrationRuns = calibrationPage.items.length ? dataTable([
		{ label: "Calibration Run", render: (item) => element("span", { className: "mono", text: shortID(item.run_id) }) },
		{ label: "Profile", render: (item) => `${item.profile_candidate?.profile?.id || "—"}@${item.profile_candidate?.profile?.revision || "—"}` },
		{ label: "Gate", render: (item) => statusBadge(item.profile_candidate?.status) },
		{ label: "Validation ECE", render: (item) => calibrationOverallMetric(item, "validation")?.ece_ppm ?? "—" },
		{ label: "Created", render: (item) => formatTime(item.created_at) },
	], calibrationPage.items, (item) => showCalibrationRun(item.run_id)) : compactEmptyState("暂无 Calibration Run", "拟合只接受独立治理的 candidate-level truth，且不会自动发布 profile。");
	const promotionRows = promotionResult.ok && promotionPlans.length ? dataTable([
		{ label: "Plan", render: (item) => element("span", { className: "mono", text: shortID(item.request?.plan_id) }) },
		{ label: "状态", render: (item) => statusBadge(item.status) },
		{ label: "Config", render: (item) => `${item.variant_config_revision?.id || "—"}@${item.variant_config_revision?.revision || "—"}` },
		{ label: "Promotion", render: (item) => statusBadge(item.promotion_status) },
		{ label: "Updated", render: (item) => formatTime(item.updated_at) },
	], promotionPlans, (item) => showCalibrationPromotionPlan(item.request.plan_id))
		: (promotionResult.ok ? compactEmptyState("暂无 Promotion Plan", "passed profile 必须显式准备、过实验与独立 holdout 后才能发布。") : errorCard("Calibration Promotion", promotionResult.error));
	const observationRows = observationResult.ok && promotionObservations.length ? dataTable([
		{ label: "Observation", render: (item) => element("span", { className: "mono", text: shortID(item.observation_id) }) },
		{ label: "Plan", render: (item) => element("span", { className: "mono", text: shortID(item.plan_id) }) },
		{ label: "状态", render: (item) => statusBadge(item.status) },
		{ label: "Rollback", render: (item) => item.rollback_recommendation ? statusBadge("recommended") : "—" },
		{ label: "Observed", render: (item) => formatTime(item.observed_at) },
	], promotionObservations, (item) => showCalibrationPromotionObservation(item.observation_id))
		: (observationResult.ok ? compactEmptyState("暂无质量窗口观测", "激活后，用两个不可变 Dashboard snapshot 构建受治理比较。") : errorCard("Promotion Quality Windows", observationResult.error));
	const evaluationRuns = evaluationPage.items.length ? dataTable([
		{ label: "Evaluation Run", render: (item) => element("span", { className: "mono", text: shortID(item.evaluation_run_id) }) },
		{ label: "Cases", render: (item) => item.summary?.cases || 0 },
		{ label: "Passed", render: (item) => item.summary?.passed || 0 },
		{ label: "Failed", render: (item) => item.summary?.failed || 0 },
		{ label: "Recorded", render: (item) => formatTime(item.recorded_at) },
	], evaluationPage.items, (item) => showEvaluationRun("evaluation", item.evaluation_run_id)) : compactEmptyState("暂无 Evaluation Run", "先将 committed ReviewRun 与治理后的 Case 绑定评测。");
	const experimentRuns = experimentPage.items.length ? dataTable([
		{ label: "Experiment Run", render: (item) => element("span", { className: "mono", text: shortID(item.experiment_run_id) }) },
		{ label: "变量", render: (item) => item.variable || "—" },
		{ label: "Improved", render: (item) => item.summary?.improved || 0 },
		{ label: "Regressed", render: (item) => item.summary?.regressed || 0 },
		{ label: "Recorded", render: (item) => formatTime(item.recorded_at) },
	], experimentPage.items, (item) => showEvaluationRun("experiment", item.experiment_run_id)) : compactEmptyState("暂无 Experiment Run", "单变量 replay 完成后会保留 baseline/variant 比较事实。");
	const repeatabilityRuns = repeatabilityPage.items.length ? dataTable([
		{ label: "Repeatability Run", render: (item) => element("span", { className: "mono", text: shortID(item.repeatability_run_id) }) },
		{ label: "Samples/case", render: (item) => item.summary?.samples_per_case || 0 },
		{ label: "Stable", render: (item) => item.summary?.stable_cases || 0 },
		{ label: "Unstable", render: (item) => item.summary?.unstable_cases || 0 },
		{ label: "Recorded", render: (item) => formatTime(item.recorded_at) },
	], repeatabilityPage.items, (item) => showEvaluationRun("repeatability", item.repeatability_run_id)) : compactEmptyState("暂无 Repeatability Run", "exact replay 样本完成后会独立记录稳定性，而不是伪装成质量提升。");
	const batches = [...experimentBatchPage.items.map((item) => ({ ...item, batch_kind: "experiment" })),
		...repeatabilityBatchPage.items.map((item) => ({ ...item, batch_kind: "repeatability" }))];
	const batchRows = batches.length ? dataTable([
		{ label: "Batch", render: (item) => element("span", { className: "mono", text: shortID(item.request?.batch_id) }) },
		{ label: "类型", render: (item) => item.batch_kind },
		{ label: "状态", render: (item) => statusBadge(item.status) },
		{ label: "Checkpoints", render: (item) => `${item.completed_cases?.length || 0}` },
		{ label: "Updated", render: (item) => formatTime(item.updated_at) },
	], batches, (item) => showEvaluationBatch(item.batch_kind, item.request.batch_id)) : compactEmptyState("暂无 Replay Batch", "批执行 intent、lease、checkpoint 与 terminal 会在这里保留。");
	const rows = casePage.items;
	const cases = rows.length ? dataTable([
    { label: "Case", render: (item) => element("span", { className: "mono", text: shortID(item.case.case_id) }) },
    { label: "类型", render: (item) => item.case.case_type },
    { label: "Dataset", render: (item) => statusBadge(item.current_governance?.dataset_state) },
    { label: "Split", render: (item) => item.current_governance?.split || "—" },
    { label: "Label rev", render: (item) => item.current_label_revision },
  ], rows, (item) => showEvaluationCase(item.case.case_id)) : compactEmptyState("暂无 Evaluation Case", "生产反馈只能进入 candidate pool，不能自动成为 gold。");
	clearContent(runMetrics,
		sectionHeader("Calibration Profiles"), calibrationRuns,
		sectionHeader("Calibration Promotion", promotionResult.ok ? element("button", { className: "primary", text: "准备发布计划", onClick: showCalibrationPromotionPrepare }) : null), promotionRows,
		sectionHeader("Promotion Quality Windows"), observationRows,
		sectionHeader("Normalization Promotion", normalizationPromotionResult.ok ? element("button", { className: "primary", text: "准备归一化晋级", onClick: showNormalizationPromotionPrepare }) : null), normalizationPromotionRows,
		sectionHeader("Evaluation Runs"), evaluationRuns,
		sectionHeader("Experiment Runs"), experimentRuns,
		sectionHeader("Repeatability Runs"), repeatabilityRuns,
		sectionHeader("Replay Batches", element("div", { className: "action-row" }, [
			element("button", { className: "primary", text: "提交单变量实验", onClick: () => showEvaluationBatchSubmit("experiment") }),
			element("button", { className: "action", text: "提交重复性批次", onClick: () => showEvaluationBatchSubmit("repeatability") }),
		])), batchRows,
		sectionHeader("Evaluation Cases"), cases);
}

function normalizationThresholdTemplate() {
	return {
		minimum_cases: 20,
		minimum_eligible_candidates: 100,
		minimum_oracle_duplicate_pairs: 30,
		minimum_oracle_distinct_pairs: 30,
		minimum_pairwise_precision_ppm: 950000,
		minimum_pairwise_recall_ppm: 900000,
		maximum_false_merge_rate_ppm: 50000,
		minimum_exact_partition_ppm: 900000,
	};
}

function showNormalizationPromotionPrepare() {
	const now = new Date().toISOString();
	const request = {
		schema_version: "argus.normalization_promotion_prepare_request.v1alpha1",
		promotion_variant_id: "normalization-policy-replace-me",
		baseline_review_run_id: "replace-with-succeeded-review-run-id",
		baseline_config_revision_id: "replace-with-active-normalization-owner-id",
		baseline_config_revision: "replace-with-active-normalization-owner-revision",
		variant_config_revision_id: "normalization-policy-config",
		variant_config_revision: "v2",
		variant_policy_revision: "argus-pi-review-workflow-v2",
		rollback_policy_revision: "argus-pi-review-workflow-v1",
		variant_implementation: { id: "candidate-normalization", revision: "v2", sha256: "replace-with-exact-worker-sha256" },
		rollback_implementation: { id: "candidate-normalization", revision: "v1", sha256: "replace-with-same-exact-worker-sha256" },
		promotion_policy_revision: "normalization-promotion-policy-v1",
		owner: "normalization-promotion-owner",
		policy: {
			schema_version: "argus.normalization_promotion_policy.v1alpha1",
			policy_id: "normalization-quality-gate",
			revision: "1",
			test: normalizationThresholdTemplate(),
			holdout: normalizationThresholdTemplate(),
			minimum_shadow_runs: 20,
			minimum_canary_runs: 50,
			canary_percentage: 10,
			created_at: now,
		},
		created_at: now,
	};
	const editor = element("textarea", { value: JSON.stringify(request, null, 2) });
	const auditInput = element("input", { value: "prepare normalization policy promotion without activation" });
	const submit = element("button", { className: "primary", text: "创建门禁候选", onClick: async () => {
		try {
			const submitted = JSON.parse(editor.value);
			const mutationInput = mutation("ui-normalization-promotion-prepare", auditInput.value);
			submitted.created_at = mutationInput.at;
			submitted.policy.created_at = mutationInput.at;
			const prepared = await api("/v1/evaluation/normalization/promotions", { method: "POST", body: {
				schema_version: "argus.local_api_normalization_promotion_prepare_command.v1alpha1",
				request: submitted,
				mutation: mutationInput,
			} });
			showNotice("归一化晋级候选已注册；尚未激活", true);
			await showNormalizationPromotion(prepared.promotion.variant.variant_id);
		} catch (error) { showNotice(error.message || "归一化晋级请求无效"); }
	} });
	const back = element("button", { className: "ghost", text: "返回评测", onClick: renderEvaluation });
	clearContent(sectionHeader("准备 Normalization Promotion", back), element("article", { className: "card" }, [
		element("p", { className: "muted", text: "这里从成功基线运行和当前配置账本派生只修改 normalization selector 的 validated 候选配置，并密封阈值策略。test 与 holdout 必须分别提交由当前 oracle registry 重算的 exact quality artifact；不会调用 provider 或自动激活。" }),
		field("Prepare Request JSON", editor), field("审计说明", auditInput), element("div", { className: "action-row" }, [submit]),
	]));
}

async function showNormalizationPromotion(variantID) {
	clearContent(loadingState());
	const record = await api(`/v1/evaluation/normalization/promotion?variant_id=${encodeURIComponent(variantID)}`);
	const back = element("button", { className: "ghost", text: "返回评测", onClick: renderEvaluation });
	const actions = [back];
	if (record.next_gate === "targeted_regression" || record.next_gate === "fixed_holdout") {
		actions.push(element("button", { className: "primary", text: `提交 ${record.next_gate}`, onClick: () => showNormalizationPromotionGate(record) }));
	}
	if (["shadow_traffic", "canary", "promotion_authorization", "rollback_monitor"].includes(record.next_gate)) {
		actions.push(element("button", { className: "primary", text: `提交 ${record.next_gate}`, onClick: () => showNormalizationOperationalGate(record) }));
	}
	if (record.next_gate === "canary") {
		actions.push(element("button", { className: "action", text: "启动 Canary", onClick: () => showNormalizationCanary(record) }));
	}
	if (record.status === "active") {
		actions.push(element("button", { className: "primary", text: "扩到 100%", onClick: () => runNormalizationLifecycle(record, "activate") }));
	}
	if (["canary", "promotion_authorization", "rollback_monitor"].includes(record.next_gate) || ["active", "failed", "inconclusive"].includes(record.status)) {
		actions.push(element("button", { className: "danger", text: "回滚/中止", onClick: () => runNormalizationLifecycle(record, "rollback") }));
	}
	const metrics = element("div", { className: "grid metrics" }, [
		metricCard("状态", record.status || "unknown", "shared append-only promotion ledger"),
		metricCard("Next gate", record.next_gate || "—", "只接受 exact ordered transition"),
		metricCard("Policy", record.variant?.managed_binding?.normalization_policy_revision || "—", "当前实现的 deterministic revision"),
		metricCard("Gate policy", record.variant?.managed_binding?.normalization_gate_policy_revision || "—", shortID(record.variant?.managed_binding?.normalization_gate_policy_sha256, 20)),
	]);
	clearContent(sectionHeader(`Normalization · ${shortID(variantID, 60)}`, element("div", { className: "action-row" }, actions)), metrics, jsonView(record));
}

function normalizationPolicyRefTemplate(record) {
	const sha = record.variant?.managed_binding?.normalization_gate_policy_sha256 || "a".repeat(64);
	const binding = record.variant?.managed_binding || {};
	return { uri: binding.normalization_gate_policy_uri || `artifact://local/sha256/${sha}`, sha256: sha, size_bytes: binding.normalization_gate_policy_size_bytes || 0, contract: "argus.normalization_promotion_policy.v1alpha1+json" };
}

function showNormalizationOperationalGate(record) {
	const gate = record.next_gate;
	const request = {
		schema_version: "argus.normalization_promotion_operational_gate_request.v1alpha1",
		variant_id: record.variant.variant_id,
		gate,
		policy_ref: normalizationPolicyRefTemplate(record),
		review_run_ids: ["replace-with-exact-succeeded-review-run-id"],
		safety_event_count: 0,
		evaluated_at: new Date().toISOString(),
	};
	if (gate === "promotion_authorization") {
		request.review_run_ids = [];
		request.authorization = { kind: "human", authorized_by: "replace-with-current-principal-actor" };
	} else if (gate === "rollback_monitor") {
		request.review_run_ids = [];
	}
	const editor = element("textarea", { value: JSON.stringify(request, null, 2) });
	const auditInput = element("input", { value: `verify exact ${gate} evidence` });
	const submit = element("button", { className: "primary", text: "验证并记录", onClick: async () => {
		try {
			const submitted = JSON.parse(editor.value);
			const mutationInput = mutation(`ui-normalization-${gate}`, auditInput.value);
			submitted.evaluated_at = mutationInput.at;
			const output = await api("/v1/evaluation/normalization/promotion/operational-gates", { method: "POST", body: {
				schema_version: "argus.local_api_normalization_promotion_operational_gate_command.v1alpha1",
				request: submitted, mutation: mutationInput,
			} });
			showNotice(`${gate}: ${output.result.outcome}`, output.result.outcome === "pass");
			await showNormalizationPromotion(record.variant.variant_id);
		} catch (error) { showNotice(error.message || "运营门禁证据无效"); }
	} });
	const back = element("button", { className: "ghost", text: "返回候选", onClick: () => showNormalizationPromotion(record.variant.variant_id) });
	clearContent(sectionHeader(`Operational gate · ${gate}`, back), element("article", { className: "card" }, [
		element("p", { className: "muted", text: "policy_ref 必须保留 prepare 返回的完整 size_bytes。shadow 运行必须绑定候选 bundle；canary 运行还必须由当前配置账本实际分配到候选。授权 actor 必须与当前 principal 一致。" }),
		field("Operational Gate Request JSON", editor), field("审计说明", auditInput), element("div", { className: "action-row" }, [submit]),
	]));
}

function showNormalizationCanary(record) {
	const body = {
		schema_version: "argus.local_api_normalization_promotion_canary_command.v1alpha1",
		variant_id: record.variant.variant_id,
		policy_ref: normalizationPolicyRefTemplate(record),
		rollout: { percentage: 10, seed: "replace-with-stable-canary-seed" },
		mutation: mutation("ui-normalization-canary", "start deterministic normalization canary"),
	};
	const editor = element("textarea", { value: JSON.stringify(body, null, 2) });
	const submit = element("button", { className: "primary", text: "发布 Canary", onClick: async () => {
		try {
			const submitted = JSON.parse(editor.value);
			submitted.mutation = mutation("ui-normalization-canary", submitted.mutation.audit);
			await api("/v1/evaluation/normalization/promotion/canary", { method: "POST", body: submitted });
			showNotice("候选配置已进入确定性 canary；请先生成真实命中运行再提交 canary gate", true);
			await showNormalizationPromotion(record.variant.variant_id);
		} catch (error) { showNotice(error.message || "Canary 发布失败"); }
	} });
	const back = element("button", { className: "ghost", text: "返回候选", onClick: () => showNormalizationPromotion(record.variant.variant_id) });
	clearContent(sectionHeader("启动 Normalization Canary", back), element("article", { className: "card" }, [
		element("p", { className: "muted", text: "percentage 必须与密封 policy 一致且小于 100；seed 会冻结确定性分流，后续只能使用同一 seed 单调扩容。" }),
		field("Canary Command JSON", editor), element("div", { className: "action-row" }, [submit]),
	]));
}

async function runNormalizationLifecycle(record, action) {
	try {
		await api(`/v1/evaluation/normalization/promotion/${action}`, { method: "POST", body: {
			schema_version: "argus.local_api_normalization_promotion_lifecycle_command.v1alpha1",
			variant_id: record.variant.variant_id,
			mutation: mutation(`ui-normalization-${action}`, `${action} governed normalization config`),
		} });
		showNotice(action === "activate" ? "Canary 已扩到 100%" : "配置与 promotion 已回滚", true);
		await showNormalizationPromotion(record.variant.variant_id);
	} catch (error) { showNotice(error.message || `${action} 失败`); }
}

function showNormalizationPromotionGate(record) {
	const now = new Date().toISOString();
	const request = {
		schema_version: "argus.normalization_promotion_gate_request.v1alpha1",
		variant_id: record.variant.variant_id,
		gate: record.next_gate,
		policy_ref: {
			uri: `artifact://local/sha256/${record.variant.managed_binding.normalization_gate_policy_sha256}`,
			sha256: record.variant.managed_binding.normalization_gate_policy_sha256,
			size_bytes: 0,
			contract: "argus.normalization_promotion_policy.v1alpha1+json",
		},
		quality_run_ref: {
			uri: `artifact://local/sha256/${"a".repeat(64)}`,
			sha256: "a".repeat(64),
			size_bytes: 0,
			contract: "argus.normalization_quality_run.v1alpha1+json",
		},
		evaluated_at: now,
	};
	const editor = element("textarea", { value: JSON.stringify(request, null, 2) });
	const auditInput = element("input", { value: `verify exact ${record.next_gate} normalization closure` });
	const submit = element("button", { className: "primary", text: "重算并记录门禁", onClick: async () => {
		try {
			const submitted = JSON.parse(editor.value);
			const mutationInput = mutation(`ui-normalization-${record.next_gate}`, auditInput.value);
			submitted.evaluated_at = mutationInput.at;
			const output = await api("/v1/evaluation/normalization/promotion/gates", { method: "POST", body: {
				schema_version: "argus.local_api_normalization_promotion_gate_command.v1alpha1",
				request: submitted,
				mutation: mutationInput,
			} });
			showNotice(`${record.next_gate}: ${output.decision.outcome}`, output.decision.outcome === "pass");
			await showNormalizationPromotion(record.variant.variant_id);
		} catch (error) { showNotice(error.message || "归一化门禁证据无效"); }
	} });
	const back = element("button", { className: "ghost", text: "返回候选", onClick: () => showNormalizationPromotion(record.variant.variant_id) });
	clearContent(sectionHeader(`Normalization gate · ${record.next_gate}`, back), element("article", { className: "card" }, [
		element("p", { className: "muted", text: "服务会从 content-addressed artifact 读取并重算完整 oracle closure；撤销、split 漂移、policy exposure、非独立证据或 digest 漂移都会拒绝。" }),
		field("Gate Request JSON", editor), field("审计说明", auditInput), element("div", { className: "action-row" }, [submit]),
	]));
}

function calibrationOverallMetric(run, dataset) {
	return (run.report?.metrics || []).find((item) => item.scope_kind === "overall" && item.scope_id === "all" && item.dataset === dataset);
}

async function showCalibrationRun(runID) {
	clearContent(loadingState());
	const run = await api(`/v1/calibration/run?run_id=${encodeURIComponent(runID)}`);
	const back = element("button", { className: "ghost", text: "返回评测数据", onClick: () => renderEvaluation() });
	const training = calibrationOverallMetric(run, "training") || {};
	const validation = calibrationOverallMetric(run, "validation") || {};
	const metrics = element("div", { className: "grid metrics" }, [
		metricCard("Gate", run.profile_candidate?.status || "—", "auto_published=false"),
		metricCard("Train samples", training.sample_count || 0, `ECE ${training.ece_ppm ?? "—"}`),
		metricCard("Validation samples", validation.sample_count || 0, `ECE ${validation.ece_ppm ?? "—"}`),
		metricCard("Validation Brier", validation.brier_score_ppm ?? "—", "integer PPM"),
	]);
	clearContent(sectionHeader(`Calibration · ${shortID(runID, 60)}`, back), metrics, jsonView(run));
}

function showCalibrationPromotionPrepare() {
	const now = new Date().toISOString();
	const request = {
		schema_version: "argus.calibration_promotion_prepare_request.v1alpha1",
		plan_id: "calibration-promotion-plan-replace-me",
		calibration_run_id: "calibration-run-replace-me",
		expected_calibration_run_sha256: "a".repeat(64),
		baseline_review_run_id: "review-run-replace-me",
		baseline_config_revision_id: "repository-finding-governance",
		baseline_config_revision: "1",
		variant_config_revision_id: "repository-finding-governance",
		variant_config_revision: "calibrated-2",
		promotion_variant_id: "calibration-filter-calibrated-2",
		promotion_policy_revision: "calibration-promotion-policy-1",
		owner: "promotion-owner",
		created_at: now,
	};
	const editor = element("textarea", { value: JSON.stringify(request, null, 2) });
	const auditInput = element("input", { value: "prepare passed calibration candidate without publishing" });
	const submit = element("button", { className: "primary", text: "创建未发布候选", onClick: async () => {
		try {
			const submitted = JSON.parse(editor.value);
			const mutationInput = mutation("ui-calibration-promotion-prepare", auditInput.value);
			submitted.created_at = mutationInput.at;
			const plan = await api("/v1/calibration/promotion/plans", { method: "POST", body: { schema_version: "argus.local_api_calibration_promotion_prepare_command.v1alpha1", request: submitted, mutation: mutationInput } });
			showNotice(`${plan.request.plan_id} 已准备，但尚未发布`, true);
			await showCalibrationPromotionPlan(plan.request.plan_id);
		} catch (error) { showNotice(error.message || "发布计划请求无效"); }
	} });
	const back = element("button", { className: "ghost", text: "返回评测", onClick: renderEvaluation });
	clearContent(sectionHeader("准备 Calibration Promotion", back), element("article", { className: "card" }, [
		element("p", { className: "muted", text: "只生成并验证 ConfigRevision，同时注册强绑定 managed variant；不会发布。baseline 必须仍是 ReviewRun 使用的当前精确配置。" }),
		field("Prepare Request JSON", editor), field("审计说明", auditInput), element("div", { className: "action-row" }, [submit]),
	]));
}

async function showCalibrationPromotionPlan(planID) {
	clearContent(loadingState());
	const plan = await api(`/v1/calibration/promotion/plan?plan_id=${encodeURIComponent(planID)}`);
	const back = element("button", { className: "ghost", text: "返回评测", onClick: renderEvaluation });
	const actions = [back];
	if ((plan.status === "prepared" || plan.status === "gating") && plan.next_gate) actions.push(element("button", { className: "primary", text: `记录 ${plan.next_gate}`, onClick: () => showCalibrationPromotionGate(plan) }));
	if (plan.status === "gates_passed") actions.push(element("button", { className: "primary", text: "发布 100%", onClick: async () => {
		try { await api("/v1/calibration/promotion/activate", { method: "POST", body: { schema_version: "argus.local_api_calibration_promotion_activate_command.v1alpha1", plan_id: planID, rollout: { percentage: 100 }, mutation: mutation("ui-calibration-promotion-activate", "activate fully gated calibration config") } }); showNotice("配置已显式发布", true); await showCalibrationPromotionPlan(planID); } catch (error) { showNotice(error.message || "发布失败"); }
	} }));
	if (plan.status === "active") actions.push(element("button", { className: "action", text: "回滚", onClick: async () => {
		try { await api("/v1/calibration/promotion/rollback", { method: "POST", body: { schema_version: "argus.local_api_calibration_promotion_rollback_command.v1alpha1", plan_id: planID, mutation: mutation("ui-calibration-promotion-rollback", "rollback active calibration config") } }); showNotice("配置和 promotion projection 已回滚", true); await showCalibrationPromotionPlan(planID); } catch (error) { showNotice(error.message || "回滚失败"); }
	} }));
	if (plan.status === "active") actions.push(element("button", { className: "primary", text: "创建质量窗口观测", onClick: () => showCalibrationPromotionObserve(plan) }));
	const metrics = element("div", { className: "grid metrics" }, [
		metricCard("Plan", plan.status, "durable state machine"), metricCard("Config", plan.config_status, shortID(plan.variant_config_revision_sha256, 20)), metricCard("Promotion", plan.promotion_status, shortID(plan.promotion_variant?.variant_id, 26)), metricCard("Variant bundle", shortID(plan.variant_bundle_sha256, 18), "experiment + holdout exact binding"),
	]);
	clearContent(sectionHeader(`Promotion · ${shortID(planID, 60)}`, element("div", { className: "action-row" }, actions)), metrics, jsonView(plan));
}

function promotionMonitorRules() {
	return [
		{ metric_id: "published_adverse_rate", direction: "lower_is_better", minimum_baseline_sample_size: 30, minimum_observation_sample_size: 30, maximum_regression_ppm: 50000 },
		{ metric_id: "published_fixed_rate", direction: "higher_is_better", minimum_baseline_sample_size: 30, minimum_observation_sample_size: 30, maximum_regression_ppm: 50000 },
		{ metric_id: "published_outcome_coverage_rate", direction: "higher_is_better", minimum_baseline_sample_size: 50, minimum_observation_sample_size: 50, maximum_regression_ppm: 100000 },
		{ metric_id: "review_run_complete_rate", direction: "higher_is_better", minimum_baseline_sample_size: 100, minimum_observation_sample_size: 100, maximum_regression_ppm: 20000 },
		{ metric_id: "review_run_success_rate", direction: "higher_is_better", minimum_baseline_sample_size: 100, minimum_observation_sample_size: 100, maximum_regression_ppm: 20000 },
	];
}

function showCalibrationPromotionObserve(plan) {
	const now = new Date().toISOString();
	const request = {
		schema_version: "argus.calibration_promotion_observation_request.v1alpha1",
		observation_id: `${plan.request.plan_id}-observation-replace-me`,
		plan_id: plan.request.plan_id,
		baseline_snapshot_id: "dashboard-before-activation-replace-me",
		observation_snapshot_id: "dashboard-after-activation-replace-me",
		policy: { schema_version: "argus.calibration_promotion_monitor_policy.v1alpha1", policy_id: "calibration-promotion-monitor", revision: "1", rules: promotionMonitorRules() },
		observed_at: now,
	};
	const editor = element("textarea", { value: JSON.stringify(request, null, 2) });
	const auditInput = element("input", { value: "compare immutable pre and post activation quality windows" });
	const submit = element("button", { className: "primary", text: "构建不可变观测", onClick: async () => {
		try {
			const submitted = JSON.parse(editor.value);
			const mutationInput = mutation(`ui-promotion-observe-${plan.request.plan_id}`, auditInput.value);
			submitted.observed_at = mutationInput.at;
			const observation = await api("/v1/calibration/promotion/observations", { method: "POST", body: { schema_version: "argus.local_api_calibration_promotion_observe_command.v1alpha1", request: submitted, mutation: mutationInput } });
			showNotice(`${observation.request.observation_id} 已构建；监控不会自动回滚`, true);
			await showCalibrationPromotionObservation(observation.request.observation_id);
		} catch (error) { showNotice(error.message || "质量窗口观测无效"); }
	} });
	const back = element("button", { className: "ghost", text: "返回计划", onClick: () => showCalibrationPromotionPlan(plan.request.plan_id) });
	clearContent(sectionHeader("创建 Promotion 质量窗口观测", back), element("article", { className: "card" }, [
		element("p", { className: "muted", text: "baseline snapshot 必须完全位于 activation 前，observation snapshot 必须位于 activation 后。只比较 exact config cohort；partial 与不足样本不会触发回滚建议。" }),
		field("Observation Request JSON", editor), field("审计说明", auditInput), element("div", { className: "action-row" }, [submit]),
	]));
}

async function showCalibrationPromotionObservation(observationID) {
	clearContent(loadingState());
	const observation = await api(`/v1/calibration/promotion/observation?observation_id=${encodeURIComponent(observationID)}`);
	const back = element("button", { className: "ghost", text: "返回评测", onClick: renderEvaluation });
	const summary = element("div", { className: "grid metrics" }, [
		metricCard("状态", observation.status, observation.rollback_recommendation ? "建议人工评估回滚" : "不自动回滚"),
		metricCard("Baseline", shortID(observation.baseline_snapshot?.snapshot_id, 22), `${formatTime(observation.baseline_snapshot?.window?.start_inclusive)} → ${formatTime(observation.baseline_snapshot?.window?.end_exclusive)}`),
		metricCard("Observation", shortID(observation.observation_snapshot?.snapshot_id, 22), `${formatTime(observation.observation_snapshot?.window?.start_inclusive)} → ${formatTime(observation.observation_snapshot?.window?.end_exclusive)}`),
		metricCard("Policy", `${observation.request?.policy?.policy_id}@${observation.request?.policy?.revision}`, "integer PPM thresholds"),
	]);
	const metrics = dataTable([
		{ label: "Metric", render: (item) => element("span", { className: "mono", text: item.metric_id }) },
		{ label: "状态", render: (item) => statusBadge(item.availability) },
		{ label: "Baseline", render: (item) => item.baseline?.rate_ppm == null ? `— (${item.baseline?.denominator || 0})` : `${item.baseline.rate_ppm} ppm (${item.baseline.denominator})` },
		{ label: "Observation", render: (item) => item.observation?.rate_ppm == null ? `— (${item.observation?.denominator || 0})` : `${item.observation.rate_ppm} ppm (${item.observation.denominator})` },
		{ label: "Delta", render: (item) => item.delta_ppm == null ? "—" : `${item.delta_ppm} ppm` },
		{ label: "Regression", render: (item) => item.regression ? statusBadge("regressed") : "—" },
	], observation.metrics || []);
	clearContent(sectionHeader(`Quality Window · ${shortID(observationID, 60)}`, back), summary, metrics, jsonView(observation));
}

function showCalibrationPromotionGate(plan) {
	const gate = plan.next_gate;
	const evidence = { refs: ["artifact://replace-with-governed-evidence"], basis: "deterministic", checks_passed: true, holdout_case_ids: [], safety_event_count: 0, rollback_verified: gate === "rollback_monitor" };
	if (gate === "fixed_holdout") { evidence.evaluation_run_id = "evaluation-holdout-replace-me"; evidence.holdout_case_ids = ["holdout-case-replace-me"]; }
	if (gate === "promotion_authorization") evidence.authorization = { kind: "human", authorized_by: "must-match-api-principal-actor" };
	const request = { schema_version: "argus.calibration_promotion_gate_request.v1alpha1", plan_id: plan.request.plan_id, result: { schema_version: "argus.promotion_gate_result.v1alpha1", variant_id: plan.promotion_variant.variant_id, gate, outcome: "pass", evidence, summary: "replace with concise verified evidence summary" } };
	if (gate === "targeted_regression") request.experiment_run_id = "experiment-finding-governance-replace-me";
	const editor = element("textarea", { value: JSON.stringify(request, null, 2) });
	const auditInput = element("input", { value: `record governed ${gate} evidence` });
	const submit = element("button", { className: "primary", text: "验证并记录门禁", onClick: async () => {
		try { const body = { schema_version: "argus.local_api_calibration_promotion_gate_command.v1alpha1", request: JSON.parse(editor.value), mutation: mutation(`ui-calibration-promotion-${gate}`, auditInput.value) }; const updated = await api("/v1/calibration/promotion/gates", { method: "POST", body }); showNotice(`${gate} 已验证并记录`, true); await showCalibrationPromotionPlan(updated.request.plan_id); } catch (error) { showNotice(error.message || "门禁证据无效"); }
	} });
	const back = element("button", { className: "ghost", text: "返回计划", onClick: () => showCalibrationPromotionPlan(plan.request.plan_id) });
	clearContent(sectionHeader(`Gate · ${gate}`, back), element("article", { className: "card" }, [element("p", { className: "muted", text: "pass 会由服务重新读取绑定的 calibration、ConfigRevision、ExperimentRun 或 holdout EvaluationRun；仅提交 refs 不能绕过精确事实校验。" }), field("Gate Request JSON", editor), field("审计说明", auditInput), element("div", { className: "action-row" }, [submit])]));
}

function batchExposureTemplate() {
	return ["prompt", "rule", "model", "index"].map((component) => ({
		component,
		status: "not_seen",
		revision: `${component}-revision-1`,
	}));
}

function showEvaluationBatchSubmit(kind) {
	const experiment = kind === "experiment";
	const request = experiment ? {
		schema_version: "argus.experiment_batch_request.v1alpha1",
		batch_id: "experiment-batch-replace-me",
		baseline_evaluation_run_id: "evaluation-baseline-replace-me",
		variant_evaluation_run_id: "evaluation-budget-replace-me",
		experiment_run_id: "experiment-run-replace-me",
		experiment_revision: "budget-timeout-v1",
		variable: "budget",
		executor_revision: "argus-local-formal-pi-replay-1",
		max_concurrency: 2,
		cases: [{
			case_id: "case-replace-me",
			expected_label_revision: 1,
			baseline_review_run_id: "review-baseline-replace-me",
			expected_baseline_config_sha256: "e".repeat(64),
			expected_variant_config_sha256: "f".repeat(64),
			exposure_observations: batchExposureTemplate(),
		}],
		created_at: new Date().toISOString(),
	} : {
		schema_version: "argus.repeatability_batch_request.v1alpha1",
		batch_id: "repeatability-batch-replace-me",
		baseline_evaluation_run_id: "evaluation-baseline-replace-me",
		replay_evaluation_run_ids: ["evaluation-replay-1", "evaluation-replay-2"],
		repeatability_run_id: "repeatability-run-replace-me",
		repeatability_revision: "exact-replay-v1",
		executor_revision: "argus-local-formal-pi-replay-1",
		max_concurrency: 2,
		cases: [{
			case_id: "case-replace-me",
			expected_label_revision: 1,
			baseline_review_run_id: "review-baseline-replace-me",
			expected_baseline_config_sha256: "e".repeat(64),
			exposure_observations: batchExposureTemplate(),
		}],
		created_at: new Date().toISOString(),
	};
	const editor = element("textarea", { value: JSON.stringify(request, null, 2) });
	const auditInput = element("input", {
		value: experiment
			? "submit budget experiment through local operator UI"
			: "submit exact repeatability batch through local operator UI",
	});
	const timeoutInput = element("input", {
		type: "number", value: 120000, min: 1, max: 86400000, step: 1000,
	});
	const variantInput = selectControl(["budget", "model", "prompt", "skill_pack", "knowledge_pack", "rule_pack", "workflow", "index", "filter_policy"], "budget");
	const modelInput = element("input", { value: "deepseek-reasoner" });
	const componentRefsInput = element("textarea", {
		value: JSON.stringify([{
			id: "pi-review-prompts",
			revision: "published-prompt-v2",
			sha256: "a".repeat(64),
		}], null, 2),
	});
	const contextProvidersInput = element("textarea", {
		value: JSON.stringify([{
			id: "go-ast",
			revision: "1",
			kind: "go_ast",
			adapter: {
				id: "argus-go-ast",
				revision: "2",
				sha256: "f343930b884436b3cef6e35fda496e4780fa2edfc94285299f23db552821e961",
			},
		}], null, 2),
	});
	const rulePackInput = element("textarea", {
		value: JSON.stringify({
			schema_version: "argus.rule_pack.v1alpha1",
			id: "agent-review-rules",
			revision: "2",
			rules: [{
				id: "correctness",
				revision: "2",
				kind: "agent",
				detector: { id: "pi-review", revision: "1", sha256: "a".repeat(64) },
				languages: ["go"],
				path_prefixes: [],
				evidence_kinds: ["file_content", "target_line"],
				severity: "high",
				enabled: true,
			}],
			sha256: "8e1749c883ff09234357997d61675d8e82b4ea6f260a7ca74793b4f60540b820",
		}, null, 2),
	});
	const workflowInput = element("textarea", {
		value: JSON.stringify({
			schema_version: "argus.workflow.v1alpha1",
			workflow_id: "formal-pi-agent-review",
			revision: "stage-budget-v2",
			stages: [{
				stage_id: "agent_hypothesize",
				kind: "agent_hypothesize",
				implementation_revision: "1",
				input_contract: "argus.review_input.v1alpha1",
				output_contract: "argus.review_hypothesis_set.v1alpha1",
				depends_on: [],
				executor: "formal-local-pi",
				required_capabilities: ["brokered-model", "frozen-review-input"],
				authority_ceiling: {
					model_egress: "provider_broker_only",
					allowed_tools: ["list_files", "read_file", "search_code"],
					tool_network: "deny",
					workspace_reads: "frozen_input_only",
					workspace_writes: "deny",
					remote_writes: "deny",
					max_delegation_depth: 0,
					max_model_calls: 2000,
					max_tool_calls: 100,
				},
				budget: {
					timeout_ms: 60000,
					max_input_bytes: 67108864,
					max_output_bytes: 1048576,
					max_concurrency: 8,
				},
				retry: {
					max_attempts: 1,
					backoff_ms: 0,
					jitter: false,
					retryable_codes: [],
					unknown_outcome: "reconcile",
				},
				failure_policy: "fail_run",
				side_effect: "none",
				replay_policy: "exact",
			}],
		}, null, 2),
	});
	const findingGovernanceInput = element("textarea", {
		value: JSON.stringify({
			schema_version: "argus.finding_governance_policy.v1alpha1",
			id: "effective",
			revision: "resolved",
			calibration_profile: {
				schema_version: "argus.calibration_profile.v1alpha1",
				id: "review-confidence",
				revision: "1",
				points: [
					{ raw_ppm: 0, confidence_ppm: 0 },
					{ raw_ppm: 500000, confidence_ppm: 350000 },
					{ raw_ppm: 800000, confidence_ppm: 650000 },
					{ raw_ppm: 1000000, confidence_ppm: 900000 },
				],
				sha256: "cd65d7c017ea54737056f6da8d5d630dc9f77268e977fd846db4017b50b50af5",
			},
			minimum_confidence_ppm: 700000,
			max_findings: 5,
			sha256: "e05611b556736407a653e538bd0ba6737640f9664e0fc317d83e6f152d3dc8c3",
		}, null, 2),
	});
	const timeoutField = field("变更后 timeout（毫秒）", timeoutInput);
	const modelField = field("变更后 exact model ID", modelInput);
	const componentRefsField = field("已发布组件 VersionedRef JSON", componentRefsInput, "wide");
	const contextProvidersField = field("有序 ContextProviderDefinition JSON", contextProvidersInput, "wide");
	const rulePackField = field("已 seal 的 RulePack JSON", rulePackInput, "wide");
	const workflowField = field("Exact formal WorkflowDefinition JSON", workflowInput, "wide");
	const findingGovernanceField = field("已 seal 的 FindingGovernancePolicy JSON", findingGovernanceInput, "wide");
	modelField.hidden = true;
	componentRefsField.hidden = true;
	contextProvidersField.hidden = true;
	rulePackField.hidden = true;
	workflowField.hidden = true;
	findingGovernanceField.hidden = true;
	if (experiment) {
		variantInput.addEventListener("change", () => {
			let draft;
			try {
				draft = JSON.parse(editor.value);
			} catch {
				showNotice("先修正 ExperimentBatch Request JSON，再切换变量");
				variantInput.value = request.variable;
				return;
			}
			draft.variable = variantInput.value;
			draft.variant_evaluation_run_id = `evaluation-${variantInput.value}-replace-me`;
			draft.experiment_revision = `${variantInput.value}-variant-v1`;
			editor.value = JSON.stringify(draft, null, 2);
			timeoutField.hidden = variantInput.value !== "budget";
			modelField.hidden = variantInput.value !== "model";
			componentRefsField.hidden = variantInput.value === "budget" || variantInput.value === "model" || variantInput.value === "rule_pack" || variantInput.value === "workflow" || variantInput.value === "index" || variantInput.value === "filter_policy";
			contextProvidersField.hidden = variantInput.value !== "index";
			rulePackField.hidden = variantInput.value !== "rule_pack";
			workflowField.hidden = variantInput.value !== "workflow";
			findingGovernanceField.hidden = variantInput.value !== "filter_policy";
			const componentIDs = {
				prompt: "pi-review-prompts",
				skill_pack: "correctness",
				knowledge_pack: "payments-domain",
			};
			if (componentIDs[variantInput.value]) {
				componentRefsInput.value = JSON.stringify([{
					id: componentIDs[variantInput.value],
					revision: `published-${variantInput.value}-v2`,
					sha256: "a".repeat(64),
				}], null, 2);
			}
			auditInput.value = `submit ${variantInput.value} experiment through local operator UI`;
		});
	}
	const submit = element("button", {
		className: "primary",
		text: experiment ? "提交单变量实验" : "提交重复性批次",
		onClick: async () => {
			try {
				const submittedRequest = JSON.parse(editor.value);
				const submittedMutation = mutation(
					experiment ? "ui-experiment-batch-submit" : "ui-repeatability-batch-submit",
					auditInput.value,
				);
				submittedRequest.created_at = submittedMutation.at;
				const body = {
					schema_version: experiment
						? "argus.local_api_experiment_batch_submit_command.v1alpha1"
						: "argus.local_api_repeatability_batch_submit_command.v1alpha1",
					request: submittedRequest,
					mutation: submittedMutation,
				};
				if (experiment && submittedRequest.variable === "budget") {
					body.budget_timeout_ms = Number(timeoutInput.value);
				} else if (experiment && submittedRequest.variable === "model") {
					body.model = modelInput.value;
				} else if (experiment && submittedRequest.variable === "filter_policy") {
					body.finding_governance = JSON.parse(findingGovernanceInput.value);
				} else if (experiment && submittedRequest.variable === "rule_pack") {
					body.rule_pack = JSON.parse(rulePackInput.value);
				} else if (experiment && submittedRequest.variable === "workflow") {
					body.workflow_definition = JSON.parse(workflowInput.value);
				} else if (experiment && submittedRequest.variable === "index") {
					body.context_providers = JSON.parse(contextProvidersInput.value);
				} else if (experiment) {
					body.component_refs = JSON.parse(componentRefsInput.value);
				}
				const record = await api(
					experiment ? "/v1/evaluation/experiment-batches" : "/v1/evaluation/repeatability-batches",
					{ method: "POST", body },
				);
				showNotice(`${record.request.batch_id} 已持久化并异步执行`, true);
				await showEvaluationBatch(kind, record.request.batch_id);
			} catch (error) {
				showNotice(error.message || "批次请求 JSON 无效");
			}
		},
	});
	const back = element("button", { className: "ghost", text: "返回评测", onClick: () => renderEvaluation() });
	const fields = [field("审计说明", auditInput, "wide")];
	if (experiment) fields.unshift(field("实验变量", variantInput), timeoutField, modelField, componentRefsField, contextProvidersField, rulePackField, workflowField, findingGovernanceField);
	clearContent(sectionHeader(experiment ? "提交单变量实验" : "提交精确重复性批次", back),
		element("article", { className: "card" }, [
			element("p", { className: "muted", text: "平台接受 budget、exact model ID、已发布 exact prompt/skill/knowledge 引用、已 seal 的 RulePack、只收紧单阶段资源预算的 exact WorkflowDefinition、有序的本地内置 context provider 定义或已 seal 的 finding governance policy，以及 exact replay；执行器模板由 API formal profile 冻结。rule_pack 会作为受治理缺陷判据被 Pi context/review/verifier 实际消费；workflow 的有效预算会以 AgentReviewPolicy 与 stage budget 的逐项最小值进入 sealed AgentStagePlan；index 会从 baseline 的精确 repository/commit/target 重新采集上下文并重跑 Pi；filter_policy 只重算 calibration/suppression，不调用 provider。请求不接收本地 prompt、skill 或 knowledge 路径。created_at 会在提交时绑定本次 mutation 时间。" }),
			field(experiment ? "ExperimentBatch Request JSON" : "RepeatabilityBatch Request JSON", editor),
			element("div", { className: "form-grid" }, fields),
			element("div", { className: "action-row" }, [submit]),
		]));
}

async function showEvaluationRun(kind, runID) {
	clearContent(loadingState());
	const endpoints = {
		evaluation: `/v1/evaluation/evaluation-run?evaluation_run_id=${encodeURIComponent(runID)}`,
		experiment: `/v1/evaluation/experiment-run?experiment_run_id=${encodeURIComponent(runID)}`,
		repeatability: `/v1/evaluation/repeatability-run?repeatability_run_id=${encodeURIComponent(runID)}`,
	};
	const run = await api(endpoints[kind]);
	const back = element("button", { className: "ghost", text: "返回评测", onClick: () => renderEvaluation() });
	clearContent(sectionHeader(`${kind} run · ${shortID(runID, 60)}`, back), jsonView(run));
}

async function showEvaluationBatch(kind, batchID) {
	clearContent(loadingState());
	const endpoint = kind === "experiment"
		? `/v1/evaluation/experiment-batch?batch_id=${encodeURIComponent(batchID)}`
		: `/v1/evaluation/repeatability-batch?batch_id=${encodeURIComponent(batchID)}`;
	const batch = await api(endpoint);
	const back = element("button", { className: "ghost", text: "返回评测", onClick: () => renderEvaluation() });
	const actions = [back];
	if (batch.status === "running") {
		actions.push(element("button", {
			className: "primary", text: "恢复缺失任务", onClick: async () => {
				try {
					const resumeEndpoint = kind === "experiment"
						? `/v1/evaluation/experiment-batch/resume?batch_id=${encodeURIComponent(batchID)}`
						: `/v1/evaluation/repeatability-batch/resume?batch_id=${encodeURIComponent(batchID)}`;
					await api(resumeEndpoint, {
						method: "POST",
						body: {
							schema_version: "argus.local_api_evaluation_batch_resume_command.v1alpha1",
							mutation: mutation(`ui-${kind}-batch-resume`, `resume ${kind} batch from frozen executor template`),
						},
					});
					showNotice(`${kind} batch 恢复请求已持久化`, true);
					await showEvaluationBatch(kind, batchID);
				} catch (error) {
					showNotice(error.message || "批次恢复失败");
				}
			},
		}));
	}
	const summary = element("div", { className: "grid metrics" }, [
		metricCard("状态", batch.status || "unknown", "Evaluation 领域终态"),
		metricCard("Checkpoints", batch.completed_cases?.length || 0, "已通过 host revalidation"),
		metricCard("Lease generation", batch.active_lease?.generation || batch.last_lease?.generation || 0, "generation/fencing 可恢复执行"),
		metricCard("最近失败", batch.last_failure?.code || "—", batch.last_failure ? `worker ${shortID(batch.last_failure.worker_id)}` : "无 durable failure observation"),
	]);
	clearContent(
		sectionHeader(`${kind} batch · ${shortID(batchID, 60)}`, element("div", { className: "action-row" }, actions)),
		summary,
		jsonView(batch),
	);
}

async function showEvaluationCase(caseID) {
  clearContent(loadingState());
  const detail = await api(`/v1/evaluation/case?case_id=${encodeURIComponent(caseID)}`);
  const back = element("button", { className: "ghost", text: "返回评测", onClick: () => renderEvaluation() });
  clearContent(sectionHeader(`Case · ${shortID(caseID, 60)}`, back), jsonView(detail));
}

connectButton.addEventListener("click", connect);
disconnectButton.addEventListener("click", () => disconnect());
tokenInput.addEventListener("keydown", (event) => {
  if (event.key === "Enter") connect();
});
document.querySelectorAll(".nav-item").forEach((item) => {
  item.addEventListener("click", () => loadPage(item.dataset.page));
});
window.addEventListener("pagehide", () => {
  state.token = "";
});
