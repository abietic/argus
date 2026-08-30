# Status

**Last verified:** 2026-08-27
**Milestone:** formal local Pi execution + governed report + unified ReviewRun terminal

## 当前已存在

- 产品、需求、技术设计、核心协议、集成边界、指标/评测/收益和旧系统证据文档。
- Go 1.26 module `argus.local/argus`；远端 module path 尚未决定。
- `ReviewSpec v1alpha1` Go 类型、strict decoder、validator 和 canonical struct digest。
- `diff`、`selection`、`scope` 三种严格样例及 JSON Schema。
- M0 CLI：
  - `argus version`
  - `argus validate review-spec <file>`
- 最小 ReviewWorkflow DAG 校验：
  - stage/dependency 唯一性；
  - unknown dependency/self dependency/cycle 拒绝；
  - replayable stage 禁止 remote side effect。
- ReviewSpec/Workflow admission：
  - workflow id/revision/digest 绑定；
  - ReviewSpec remote-write policy 与 stage side effect 联合校验；
  - 平台 attested executor capability 超权拒绝；
  - replay 对 non-replayable/side-effecting executor 失败关闭。
- `make verify` 工程门禁：Go/TypeScript format、typecheck、vet、test、JSON Schema/Go
  正反例 parity、文档链接和双 runtime build。
- 本地 Git 已初始化为 `master`，无 commit、无 remote。

## 本地平台 API

- `argus api serve` 已提供仅 literal loopback 可绑定的 authenticated HTTP adapter；除 credential-free embedded
  `/ui/` 静态壳外，所有 data/API 端点（含 health）强制 bearer，token 仅来自
  `ARGUS_LOCAL_API_TOKEN`，不写入 store 或响应。
- 服务启动时从 clean absolute strict JSON 文件冻结单一 principal；transport permissions 与 Evaluation roles
  分离，read/write actor 均由服务注入，
  请求尝试携带 actor/roles 会作为 unknown field 拒绝。它是 single-user/local authority，不是
  Hailix/IAM、tenant 或 Workspace attestation。
- API 覆盖 Evaluation Case list/show/external import、GovernanceBatch run/list/show、governance
  trust-key register/list/revoke；ConfigRevision create/list/show/validate/publish/rollback；以及 immutable
  Dashboard snapshot list/show 和 ReviewRun list/show。Config state 通过独立必填 `--config-state-dir` 绑定现有
  lifecycle repository；Dashboard handler 是 query-only，不返回 raw facts、不执行 rebuild。ReviewRun 列表使用
  run-index sequence watermark 保持跨页一致；show 对未终态 run 只返回 lifecycle summary，对终态 run 重验完整
  closure 并只返回结构化 deterministic/formal result，不返回 raw agent task evidence/receipt 或 Markdown。
  `review-runs/{run}/findings/{finding}` 通过既有 control-plane join 重验 exact run/finding 归属，分开返回
  model/governed/human Decision、Feedback、Outcome，并排除 publication provider request/result payload。
  同路径新增 Decision/Feedback/Outcome POST，使用独立 `review_write`、process-fixed actor kind/ID 与
  finding roles；request 自报 actor/roles/recorded time 会被 strict decoder 拒绝，Feedback 仍只回流
  candidate-only eligibility。embedded operator UI 的 Finding 详情页已接入这三类 append-only 写入，
  页面不提供 actor/role 输入；冲突、越权、无效 correction 与 ledger 损坏分别映射为稳定 HTTP 语义。
- `GET /v1/review-run-impacts` 已支持按 applied config revision、ConfigBundle、RulePack、Workflow、
  Model 的 exact ID/revision/optional SHA 反查 ReviewRun；cursor 绑定 selector 与 run-index watermark，
  append-only typed reverse-index row 在正常 run 写入时增量生成，API 启动显式 rebuild 旧 ledger 并报告
  complete/appended/gaps，GET 不隐式写盘。索引先筛选，实际命中的 terminal run 仍重验完整 closure，命中的
  nonterminal run 重验 authoritative created lineage；缺 snapshot 或未 rebuild 旧 row 进入不同 coverage gap。
  其他 list 有默认 50、最大 200 的 opaque marker cursor 分页。
- API 新增 `review_execute` 与 ReviewJob submit/list/show/cancel。submit 在返回 202 前冻结 published
  ConfigBundle/receipt 与 immutable command，再复用 scheduling admission/lease/heartbeat/generation/fencing/
  callback/reconcile；request disconnect 不取消，显式 cancel 建立永久 run fence。`deterministic_review_v1` 的
  orphaned nonterminal run 失败关闭避免重复；`formal_pi_review_v1` 已接受 committed source run，冻结 exact config、
  Pi runtime/component bytes 与 pricing ceiling，并消费 coordinator 已认领的同一 workload lease。fake provider E2E 已验证
  formal job 的单一 job/run/workload 与失败终态；跨进程 physical acceptance 进一步验证 generation 1 worker 在
  provider runner 内持久化 Pi group checkpoint 后被 SIGKILL，lease expiry reconcile 后 generation 2 接管并收到
  exact checkpoint，且 generation 1 迟到 callback 被 durable `stale_generation` fence 拒绝。2026-08-27
  revision 1+ 的累计 candidate-verification checkpoint 已完成：成功 verifier 可复用，失败 verifier 必须重跑，
  candidate/revision substitution 在模型前拒绝，并发乱序 callback 收敛。当天 opt-in live canary 使用环境中的
  真实 DeepSeek，在一个 selection group 的 context 与六个 review dimension 全部成功且首个 verifier revision 1
  fsync 后物理终止 generation 1 lease owner；generation 2 复用 revision 0/1、无需重跑已完成 verifier、提交
  succeeded ReviewRun，并以 durable `stale_generation` 拒绝旧代 callback；最终通过用时 254.98 秒。Node 可能在
  最后一个 progress event 后先退出，因此该证据证明的是持有 lease/callback authority 的 ReviewJob worker crash，
  不额外声称在同一时刻杀死仍存活的 Node 进程。
  该验收仍是 local direct-provider/non-attested 证据，不是 Hailix worker/Trace attestation。claimed formal executor
  复用 coordinator 的 exact scheduling repository，避免以另一 policy digest 重开同一 ledger。真实 provider canary 另列。
- formal ReviewJob 的跨进程 cancel propagation 已做 physical acceptance：不持有 worker active context 的父进程
  追加永久 cancel，子进程下一次 lease heartbeat 被 fence 并取消 provider runner context；workload 保持 canceled、
  active lease 清除，且 cancel 后没有 callback 被接受。当前传播延迟受本地 heartbeat interval 约束，尚不是
  Hailix push signal 或远端 worker attestation。
- ReviewJob 新增 permission-gated workload timeline API/UI；它直接投影 scheduling append-only sequence，保留
  admission、lease/heartbeat、generation/fencing、reconcile、callback/cancel reason，不复制为第二套 trace 真相。
- scheduling 已验证四类独立 pool、global/class/tenant bounded queue、tenant quota、priority bias、
  fairness/aging 与显式 reject/throttle。新增 strict `WorkloadPressureSnapshot`，从同一 ledger 按固定
  policy revision/SHA、sequence 和 UTC observation 对账 global/class/tenant capacity、oldest wait、
  admission/state/stale counters；expired work 只报告 requires-reconcile，不由查询改写。
  `argus workload pressure` 是不创建 missing store 的只读 CLI；authenticated
  `GET /v1/workloads/pressure?at=...` 复用 API process 已持有的 repository 与 `review_read`，operator
  overview 只呈现该快照而不把调度容量混入 ROI Dashboard。
  focused API/CLI、strict decoder、reconciliation、class isolation、tenant fairness 与 bounded concurrency
  tests 已通过；production Hailix 多实例资源池和平台指标 attestation 尚未实现。
- `/ui/` 已提供 ReviewJob submit/list/show/cancel、ReviewRun/Finding、Config、Dashboard、Evaluation 浏览和 Config create/validate/publish/rollback；
  静态壳不含运行数据或凭据，self-only CSP 下由用户输入 token，token 仅保存在 JS 内存且断开/pagehide 清除，
  不写 Web Storage/cookie/URL。真实浏览器 smoke 已完成 connect、overview、config create/validate/publish，
  console 无 warning/error。
- Evaluation Case/GovernanceBatch 增加 singular exact query show，解决领域合法 `/`、`:` ID 无法由 unencoded
  path segment 寻址的问题；path show 继续作为 safe-ID convenience route。
- HTTP adapter 有 strict JSON/Content-Type/query/path、4 MiB body、header/timeouts、稳定 error envelope 与
  graceful shutdown；focused tests 已覆盖未认证、错误 token、自报权限、ACL、普通与 watermarked 分页、loopback、
  CLI live health、review/finding service composition、Finding not-found/敏感字段边界、UI CSP/HEAD/method/token
  persistence rejection、exact complex-ID lookup、cancel 和 full-store token leak scan。
- principal/mutation/六类写命令均有 Go strict decoder/Validate、JSON Schema、正例和 docs-check parity。

## M1.2 已实现的实验性 Agentic Review CLI

- `runtime/pi-review` 是隔离的 TypeScript package，不改写现有 Go application service
  或 Hailix/Eino-Agent ownership：
  - 使用固定版本 `@earendil-works/pi-agent-core@0.84.1`；
  - 支持 working changes、exact commit diff 和显式 files；
  - 对确定性 change group 并行执行 context collector 与六类正交 Skill reviewer，再对
    Candidate 并发执行独立 verifier；
  - 额外 Skill 和业务知识只能通过显式文件参数加载，不自动执行目标仓库配置。
- Agent 只获得有界 `read_file`、literal `search_code`、`list_files` 和受限 CodeGraph
  查询；没有 shell、写文件、测试、网络或远程副作用工具。敏感路径、父目录 symlink
  越界、超大文件和子进程凭据继承均失败关闭或作为 coverage gap 留痕。
- Candidate、verdict 和 Finding 分离；Candidate anchor 必须覆盖变更行，confirmed
  Finding 必须携带宿主从冻结源码回读验证过的结构化证据。运行结束会重新捕获目标，
  新增变更路径、HEAD 或读取过的 context 漂移都会标记 stale/partial。
- 默认硬准入为 32 文件、4 MiB 冻结源码、8 分组、32 Candidate、全局 96
  provider/model turns、每 turn 8192 output tokens；文件数量和源文件字节在构造完整
  patch 前准入，且 context/review base task 之外会为最多 32 个 retained Candidate
  各预留一次独立 verification task 的最低 turn；`--plan` 在调用模型前分开展示
  Agent task 与 provider turn 最坏值、目标、跳过项、分组和准入结果。
- builtin review pack 现按 correctness、concurrency-data、error-contract、
  resource-lifecycle、security-contract、transaction-state 六个职责切分。前两个因
  去除错误/资源/事务重叠升为 `builtin-v2`，其余维度保持各自 revision；Markdown
  revision 元数据与 exact SHA-256 同时进入 standalone/shadow/formal snapshot。
  Go/TS/JSON Schema 的 worker/Plan 上限同步为 16 个 review dimensions，默认维度之外
  为受治理的仓库/业务线 Skill 保留 10 个扩展槽。
- 原始 Candidate 不再因 validation/dedup 被物理丢弃：报告分开保存 raw candidate、
  retained/invalid/merged-duplicate/excluded-budget 决策和 normalized candidate。存在
  Candidate 但禁用
  或未完成独立验证时，状态为 `partial`，不再返回误导性的 `complete`。
- Go shadow bridge 现将每个有界 raw claim（包括 `rejected_invalid` 和
  `excluded_budget`）规范化为独立 `AgentReviewRawCandidateCollection` 内容寻址 Artifact；
  claim digest、normalization fate、plan/target identity、ImportRecord、Manifest、查询和
  pre-authorized crash reconciliation 必须精确闭合。它仍是 `worker_self_report/shadow_only`，
  不是可信源码证据、Finding 或 gold label。
- Go shadow bridge 还将每个实际执行的 Pi task 的 exact system/user prompt、结构化
  context/review/verifier output，以及按执行顺序记录的 typed tool arguments/results 保存为
  独立内容寻址 `AgentReviewTaskEvidenceCollection`。总 payload 和单项均有硬上限；预算不足时
  删除正文但保留 SHA-256/字节数并显式标为 `partial/evidence_budget_exceeded`，依赖失败产生的
  synthetic task 则标为 `task_evidence_unavailable`。Go host 会把 task identity/status、
  prompt/output digest 和 tool invocation/failure counter 与 receipt 交叉复算后才允许入库。
  该 Artifact 固定为 `exact_local_sensitive/worker_self_report/diagnostic_only/shadow_only`，
  不保存 assistant reasoning，也不等于 provider transcript、Hailix Trace 或 attestation。
- `exact_local_sensitive` 现使用独立的 `sensitive_process` / `sensitive_read` Artifact use 与
  role，既不能通过普通 `read` 旁路，也不允许 `export`。通用 `agent-review run/show --json`
  只序列化 ref、状态与治理摘要，Go `Result` 的 exact collection 固定 `json:"-"`；显式
  `agent-review evidence read` 必须同时提供 actor、闭集 purpose、唯一 request ID、UTC 时间、
  `--acknowledge-sensitive-output` 和 `--json`，且在返回正文前先提交不可变访问回执。同一
  request exact retry 复用回执，替换 artifact/purpose/actor/time 会冲突失败。
- 当前本地保留策略明确为 `until_explicit_revocation`：`agent-review evidence revoke` 通过
  integrity-operator tombstone 永久拒绝后续 processing/disclosure，但 manifest、receipt、
  去敏历史和 analytics 仍可查询。由于底层按内容寻址且 bytes 可能被多个逻辑对象引用，MVP
  不做无引用证明的物理 unlink；原始 bytes 等待未来 reference-aware GC。CLI 不提供敏感
  export。该边界是本地 authority adapter，不等于 hosted IAM 或 OS 文件权限隔离。
- Candidate/verifier evidence 限制为至多 20 行，并要求 excerpt 与冻结源码完整引用
  范围精确一致；单字符、局部 substring 和跨整文件形式证据会被宿主拒绝。敏感输入
  策略同时用于目标源码和显式 Skill/Knowledge，覆盖 env、secret config、service
  account、Kubernetes 与 Terraform 常见凭据/状态文件。
- 每份 CLI 报告包含 diagnostic-only Pi execution snapshot 和逐 Agent task receipt：
  冻结 target/group/Skill/Knowledge/prompt/runtime/provider/budget digest，并汇总
  provider turn、只读 tool、token、耗时、terminal status 与稳定错误码。当前 receipt
  明确是 `worker_self_report`，snapshot 明确是 `non_replayable`，不会冒充 Hailix
  Trace、platform attestation 或正式 Finding。
- package 的 72 个测试通过，包含 Pi Agent + fake provider 的真实 tool-submit loop、
  provider profile/auth fail-closed、hard abort、Git target、证据验证、候选 lineage、
  敏感输入、执行快照/receipt、预算和 partial 语义；根 `make verify` 已纳入
  typecheck、测试和 build。
- 新增显式 `deepseek-anthropic-env@1` profile。它仅接受 HTTPS 的
  `api.deepseek.com/anthropic`，拒绝 redirect，关闭 Pi ambient credential fallback，
  只通过 `ANTHROPIC_API_KEY` 注入认证。2026-08-20 的真实 smoke 已通过 Pi 的
  context -> review -> independent verification 多轮工具链路，在临时 Git fixture 的
  可空对象直接解引用上产出 1 个 high Candidate 和 1 个带精确源码证据的 confirmed
  verdict；另一个带空值保护的 clean fixture 产出 0 Candidate。两次都是刻意压低
  tool budget 的受限运行，因 context gaps 正确返回 `partial`，不能扩张为完整覆盖或
  precision/recall 验收。

## M1.3.3 已实现的本地 Pi shadow execution

- 新增严格的 `AgentReviewPlan`、`ReviewHypothesisSet`、
  `AgentReviewRawCandidateCollection`、
  `AgentExecutionReceipt`/Collection、`AgentReviewResultManifest` 和
  `AgentReviewObservation` v1alpha1 契约；Go 类型、strict decoder、Validate、JSON
  Schema、canonical examples、正反测试和协议文档已纳入 `make verify`。
- 契约保留每个 raw candidate 的有界 claim Artifact、digest 与 normalization fate，区分 Hypothesis、验证
  observation 和正式 Finding；完整结果必须闭合 context/review/verification receipt、
  coverage、tool/model budget、版本引用、时间区间和宿主复算 summary。
- `internal/agentshadow` 已提供本地 strict import/query：先通过同一 run
  repository 回读已提交的 ReviewRun，并闭合校验 MaterializedTarget、
  ExecutionSnapshot、ReviewInput、ReviewSpec 和 exact artifact refs；随后校验
  target digest、目标 anchor 和完整源码 evidence，再由 Go host 生成
  Manifest/Observation。ContextRef-only 与 old-side evidence 在当前冻结输入切片失败关闭；
  deletion-only hunk 仅允许以删除相邻的存活 target context 行作为 Candidate 主 anchor，
  Go evidence 授权也只扩展到该 deletion-only hunk 的存活 target context，replacement hunk
  不获得这一例外。
- 导入使用稳定 host intent time、内容寻址的只读 governed artifacts、append-only
  observation 和 immutable ImportRecord commit point；同输入重试/并发幂等，不同输入
  复用 key 冲突，scope 越权、future worker time、伪造引用和 artifact 篡改被拒绝或
  quarantine。它不写正式 ReviewRun/Finding/Feedback/Value ledger。
- `runtime/pi-review` 新增严格 stdio worker：stdin 恰一份
  `AgentReviewWorkerRequest`，stdout 恰一份 bounded result，stderr 只承载有界并持续
  drain 的进度。request 绑定 work item、attempt/generation/fence/idempotency、deadline、
  capability digest、Plan 和 exact base64 ReviewInput；duplicate/unknown/null/trailing、
  非 JS-safe integer、超限与 stale binding 全部拒绝。
- Go `LocalBridge` 自动从 committed succeeded ReviewRun 回读 ExecutionSnapshot 与
  canonical ReviewInput，构造 Plan 并启动无 shell 子进程。worker 在冻结内存目标上只
  使用 `list_files/read_file/search_code`，不读取 Git/working tree/CodeGraph；Go host
  复算 target/file/group patch digest、skipped/context closure、anchor/evidence、task 与
  coverage counter 后再映射 Hypothesis/Receipt 并提交 shadow result。
- 通用 ReviewRun Artifact 已增加 append-only integrity lifecycle：内容缺失、size 或 SHA-256
  漂移会自动 quarantine；人工 release 必须前后两次通过原始 bytes 校验；不可逆 tombstone
  保留共享 content-addressed bytes 和历史 lineage。CLI 提供 inspect/quarantine/release/tombstone，
  lifecycle ledger 损坏失败关闭。统一 gate 覆盖 read/candidate_pool/publication/evaluation/training/export，
  Publication 与 EvaluationRun 已逐项接入 exact input refs。该证据只覆盖同一 local store，
  不包含跨存储对账或 Hailix Trace integrity。
- execution intent 在 spawn 前持久化；相同语义 + key 只复用 terminal result，不同语义
  冲突，orphan intent 返回 unknown outcome 而不自动重跑。completion 为 v1alpha2；严格
  `ExecutionAttempt` 已升为 v1alpha3，并可绑定 immutable、4 KiB 上限、固定 taxonomy 的
  host-failure observation。execution 区分为 `succeeded`、`failed`、
  `canceled` 和 `unknown_outcome`：成功态
  必须重验完整 shadow result closure；失败/取消态只暴露 immutable completion 与脱敏
  failure；unknown outcome 只暴露 intent 证据，不能解释为失败或安全重试。worker
  `completed_at` 与 host-owned `recorded_at` 分离；succeeded 不得越过 deadline，
  failed/canceled 可在取消收敛后完成，但所有 terminal `completed_at` 都不得晚于
  `recorded_at`，terminal Attempt `ObservedAt` 只取 `recorded_at`。
- `LocalBridge` 在 intent 已接受后的 runner、协议解码/binding、mapping、encoding、import 和
  completion 错误路径都会返回当前 Attempt，同时保留原错误语义；intent 前的准入失败不
  伪造 Attempt。host failure 只保存 stage/reason，不保存 raw error/stderr/URL/credential，
  且不伪造 completion。`QueryExecution`/`QueryExecutions`/`ListExecutions` 提供 scope-bound
  查询、单批 execution lookup 和 host-observed UTC 半开时间窗口，扫描时拒绝 symlink、
  异常目录项、伪造路径和损坏 closure。LocalBridge 在 strict worker binding/mapping 后、
  `Service.Import` 前写入 immutable execution import authorization，绑定 intent/runtime digests、
  canonical payload refs、combined input digest 与 expected manifest；`ReconcileExecution` 只在
  该 authorization 与 exact committed import 完整闭合且 completion host time 不早于 import
  commit 时补写 succeeded completion，不调用 worker/provider；legacy/direct Import、只有
  authorization 而没有 committed result 的情况均保持 unknown，并发调用只有实际创建
  completion 者返回 reconciled。
- 子进程只接受 absolute regular non-symlink Node/worker，环境变量使用 allowlist；stdout
  hard limit、stderr 截断不阻塞 drain，Unix cancel 终止整个 process group。worker
  failure message 在持久化与回显前固定脱敏。
- runtime identity 绑定 Node binary、worker entrypoint、排序有界的
  `dist/*.js + package.json + package-lock.json`、production `node_modules` 实体、builtin
  Skill、provider/profile/model、budget 与 frozen input。shadow direct path 已 artifactize
  bounded exact prompt/context/output/tool evidence；但这些内容和 token usage 仍是 worker
  self-report。formal local Pi 现也发布同一 contract 的 governed Artifact，
  `StageExecutionResult.agent_task_evidence` 绑定 governed ref；host admission strict decode 后
  落本地同内容 ref，terminal result bytes、whole-run closure 与 `ReviewRun.agent_task_evidence_ref`
  共同闭合。formal PlatformPort 还会解析 Plan 绑定的 exact governed prompt Artifact，将
  `ref + artifact + canonical bytes` 注入 worker；Go/TypeScript 双侧复算 digest/size/revision，
  Pi runtime 的 context/review/verification/finalizer prompt 实际消费该 bundle，snapshot 与
  exact task evidence 回显同一 identity。代码所有的只读/凭据/repository-untrusted 安全基线
  仍不可由配置覆盖。但仍没有 provider/Hailix attestation，因此 Plan 保持 `non_attested`，
  snapshot 保持 `non_replayable`。
- CLI 已提供 `argus agent-review run/show`、`argus agent-review evidence read/revoke` 和
  `argus agent-review execution list/show/reconcile`。partial 会先打印已提交 manifest 再默认返回
  非零，只有显式 `--allow-partial` 才放宽退出码；post-intent 错误会先打印 Attempt 再返回
  非零。plain error 输出包含状态、execution/source-run ID、failure code、reused、
  `outcome_acknowledged` 和可选 `unconfirmed_manifest`；JSON 输出显式绑定 Attempt、
  acknowledgement、reused、store path 与可选
  `unconfirmed_committed_result`。后者覆盖 import 已提交但 completion 失败的 crash window，
  manifest 可用 `agent-review show --manifest-id` 查询。若 completion 已 rename 可见但目录
  fsync acknowledgement 失败，Attempt 可能已是 succeeded，但 acknowledgement 必须为 false；
  自动化不能只看状态确认本次写入。所有
  命令组的嵌套 `-h`/`--help` 都以 0 退出并打印完整 command-group usage。
- `internal/agentanalytics` 独立生成 AgentExecution/Task/Tool facts 和
  `agent_execution.*` 等 v1alpha3 诊断投影；facts/projection/snapshot/manifest/metric/
  policy/export 契约均为 v1alpha3，保存 intent/completion/可选 host-failure source
  bindings 与 immutable snapshot；host-failure binding 保存完整 canonical observation、
  重算 SHA 并与父 Attempt/Fact 闭合。CLI 提供
  `argus agent-review analytics rebuild/show/export`；导出支持 canonical JSON/CSV。成功
  execution 从完整 result closure 生成 task/tool/usage；failed/canceled/unknown-outcome
  只生成稀疏 host execution fact，不伪造 receipt、task、tool 或 token usage，投影分别
  提供对应计数。单次 current-projection rebuild 内按 Attempt `ObservedAt` 唯一归属；没有
  execution intent 的 legacy direct Import 保留 observation 归属，有 attempt 但当前属于
  其他窗口的 import 会被过滤。reconcile 后 current 归属可从旧 unknown 移到 terminal，旧
  immutable snapshot 保持不变，跨构建时点不能直接求和；稳定历史仍缺 transition/as-of
  ledger。attempt-aware rebuild 通过单批 execution lookup 构建索引，不再逐 result 重扫
  intent store。duration p50/p95 只统计带 manifest 的 worker-report 样本，不把稀疏 attempt 的
  host duration 当成同一口径。旧版 projection snapshot/manifest 显式不兼容，重建
  必须使用新的 v1alpha3 snapshot ID，避免同版本语义漂移。
  该包不 import 正式 `internal/analytics`，不会生成 Finding funnel、ValueObservation、
  成本或 ROI。CLI show/export 会重验固定 `local/local` scope；export 只允许 store 外
  目录，磁盘 manifest 绑定 snapshot ID/SHA-256、scope、target 与 dataset manifest，
  exact retry 幂等，不同内容拒绝，stdout acknowledgement 失败有 unknown-outcome 标记。
- 根 `make verify` 已包含 65 个 Pi tests、worker build，以及一个真实 built
  TypeScript worker 与 Go strict decoder 的 canceled round-trip；该 smoke 在 deadline
  admission 阶段结束，不调用 provider。focused Go test/vet/race 覆盖 bridge、mapper、
  runner、execution history、committed List/index repair、包含失败/取消/unknown outcome 的
  diagnostic projection/export 与 CLI lifecycle。
- 这仍是 local/local、single-user、direct-provider shadow slice，不是 Hailix Task/Worker、
  ACP、Trace、credential service 或多租户 API。2026-08-26 已使用环境中的 DeepSeek
  Anthropic-compatible endpoint/model 完成 Go host -> strict worker -> live provider ->
  shadow import：known-defect execution
  `agent-review-execution-b14735655e2141fe6fe5793b` 在删除 nil guard 的 fixture 上保留并独立
  确认 1 个 high finding，3/3 task 成功、7/7 provider turn 完成；clean execution
  `agent-review-execution-62f52aa0502ba5816d911a46` 在恢复 guard 的 fixture 上返回 0 raw
  candidate/0 finding，2/2 task 成功、4/4 provider turn 完成。两者因 fixture 缺调用方或测试
  而诚实保持 partial；execution authority 仍为 `local_direct_provider_shadow/non_attested`，
  side effects/remote writes 均 deny。

## 已实现的 formal Agent Review 本地执行切片

- `ConfigRevision`/`ConfigBundle` 新增可选 typed `AgentReviewPolicy`：闭合 exact
  agent/provider/model/prompt/API protocol refs、按 stable ID 保持治理顺序的
  grouping/context/review/verification Skill、Knowledge、工具权限和完整 Agent budget；至少
  一个 review Skill。当前固定 provider-broker-only model egress、tool network deny、
  frozen-input-only reads、workspace/remote writes deny 和 delegation depth 0。
- config lifecycle repository 新增 `ResolvePublishedWithReceipt`：从同一原子 projection
  返回 exact `ConfigBundle` 与 `ConfigResolutionReceipt`。receipt 绑定 context、bundle 和
  applied published revisions 的 revision/publish/assignment provenance；它是本地治理记录，
  不是签名或远端 attestation，不能脱离受信 repository 自证 publication。
- 本地平台新增 strict `POST /v1/config/resolutions` 与 Operator UI 生效配置解析表单；只读
  `config_read` principal 可获得同一次原子解析的 ConfigBundle + ConfigResolutionReceipt，并查看逐字段
  explain。未知字段或 caller 自报 actor/roles 失败关闭；本地 context 仍不等于 hosted scope authorization。
- application 新增更强的 `GovernedConfigProvider` 与 `AgentComponentResolver` port。
  `CompileGovernedAgentStage` 要求 authenticated host adapter 注入独立的
  `AgentPlanningSubject`，在访问两类 provider 前先校验其 tenant/organization/workspace/
  repository 与 ReviewRun/ReviewSpec/resolution context、repository provider、invocation 及
  selection/non-selection path scope 全部匹配，并把完整已准入 subject 绑定到 component
  request；随后只接受同次
  config provider 调用的 bundle + receipt，并用 policy identity 解析 exact agent/provider/
  model/runtime/prompt/API protocol/Skill/Knowledge/context-provider artifacts。resolver request
  与输出 Plan 均不包含 model credential。
- workflow `StageAuthorityCeiling` 已能冻结 model egress、tool allowlist/network、workspace/
  remote write、delegation、model/tool call ceiling；formal Agent policy 超出 workflow、
  ExecutionSnapshot 或 ConfigBundle 任一权限/预算时失败关闭，不做 silent clamp。
- `internal/agentplan` 提供无 filesystem/network/env/clock/random/secret/provider I/O 的纯
  compiler。它重验 ExecutionSnapshot、MaterializedTarget、ConfigBundle、receipt、workflow、
  ReviewSpec、ReviewInput 的 exact artifact binding，并闭合 diff/selection/scope、配置
  include/exclude、文件/context、ordered components、runtime/build、authority 和 byte/call
  budget。
- 新增 `AgentStagePlan v1alpha1` Go contract、strict decoder/validator、JSON Schema、规范
  example 与正反测试。Plan 有 full/behavior digest；exact runtime component 与
  ExecutionSnapshot build identity 都进入 behavior closure。输出固定
  `ReviewHypothesisSet`、`disposition=hypothesis_only`、`side_effects=deny`，不含 credential、
  endpoint、raw prompt、Candidate 或 Finding。
- application/runrepo 已把 canonical Plan 和全部冻结来源作为 immutable admission closure
	持久化；正式 dispatch 使用 canonical request、attempt/generation/fence、唯一 claim 和 exact
	execution binding。caller cancellation 后的已 claim 操作使用有界 detached context；已 claim
	unknown outcome 只允许 exact recovery，不会开启不同请求。dispatch claim 与 terminal admission
	现在先竞争同一条跨进程 append-only generation decision stream：terminal completion 先赢会
	阻止更高 generation claim，更高 claim 先赢会拒绝旧 completion；exact retry 复用同一 decision。
- succeeded callback 不能只靠 echo 字段自证：必须通过 host callback verifier，登记绑定 exact
	binding/result/verifier/proof digest/time 的 proof-redacted receipt；receipt registry 可在
	receipt-to-terminal crash window 后恢复一次性验证结果。result 与 cancellation 竞争同一
	deterministic terminal GateID，成功 winner 才能追加 formal `ReviewHypothesisSet` evidence；
	output/trace 同时保留 governed 与 local exact projection。
- executor trust 已进入 capability/request immutable identity：authority、capability verifier 和 callback
	verifier 任一变化都会改变 digest。fresh callback、receipt-to-terminal recovery、terminal/evidence
	exact retry 都从 dispatch intent 绑定的 persisted request 取得 trust，并要求实际/已登记 verifier exact
	match；本地 Pi 与 Hailix consumer adapter 还会与各自 pinned trust root 再比较。该闭包防止 unknown-outcome
	recovery 跨信任域复用旧 identity，但仍不是 provider/Hailix attestation。
- historical reconciler 枚举全部 claimed intent，并由 authoritative scheduling workload 判定
	canceled/terminal/expired/reassigned；它的 gateway 类型只暴露 Lookup/Cancel，单项失败不会
	阻塞后续 intent，actor 由 constructor 注入。
- `internal/piexecution` 已实现本地 formal Pi PlatformPort：从冻结 Plan/Request/ReviewInput
	降低为 strict worker request，使用 exact-idempotent Ensure/completion ledger，输出 typed
	succeeded/failed/canceled `StageExecutionResult`，并由本地 immutable-ledger callback proof
	闭合 result admission。证据 mapping 失败会成为有界 typed operational failure，不伪造空
	HypothesisSet。
- `internal/formalreview` 与 `argus agent-review formal bootstrap/run/show` 已接通真实 composition：
	bootstrap 检查 Node/worker bytes、加载六项 builtin review skills、发布 15 个 subject-bound
	components 和 lifecycle-governed config；run 初始化独立 formal ExecutionSnapshot、调度并执行
	Pi stage；show 只读终态。DeepSeek profile 已区分 provider ID `deepseek-anthropic` 与
	profile `deepseek-anthropic-env`，credential 只保留 env SecretRef。`ModelProfile` artifact 进一步把
	安全的内部 component ID 与 exact provider wire model 分离，已验证 `deepseek-v4-pro[1m]` 不被改写。
	runtime/agent/worker-owned skill revision 绑定 build digest；bootstrap 复用已发布 exact component，完整
	config semantics 独立生成 config revision，因此 worker 或预算演进不会再争用旧 identity。
- terminal workload 重入会从 exact dispatch coordinate、terminal gate、canonical result 与
	hypothesis evidence 恢复，不再 claim 已结束 workload，也不会二次调用 provider。CLI E2E
	分别覆盖成功与失败链路，并验证 exact retry 的 runner 调用次数保持 1。
- 2026-08-26 real DeepSeek formal acceptance 已闭合：known-defect run
	`formal-cf7757b7f805b472cab09d55` 成功提交 2 个独立 verifier-confirmed Finding；zero-candidate run
	`formal-6ab794536eceed67f4ac7390` 成功提交 0 Candidate/0 Finding，但因 collector 明确报告调用方/测试
	evidence gap 保持 partial，未冒充 proven-clean；exact retry 在 0.4 秒内复用 dispatch/admission/final run。
	无效凭证 run `formal-af591844ca1755ef937b21ba` 以非零 CLI、failed ReviewRun 和
	`pi_no_review_completed` 收敛。正式 success 现在要求有 target group 时至少一个 group 完成全部 review
	dimensions；provider 全失败不能再以 partial success fail-open。上述均为 local worker/provider evidence，
	不是 Hailix/provider attestation。
- real DeepSeek physical recovery 暴露并闭合两个只在跨代/真实输出上出现的 contract bug：Pi checkpoint ledger
	现在保存每代 host-authored `generation_bound`，恢复 worker 的逻辑时间窗口只可下调到实际被复用 checkpoint
	对应的最早 host binding，避免把 generation 1 task 错判为早于 generation 2 execution；frozen target 的
	provider-facing `new/file` target-side alias 在 fingerprint、checkpoint、evidence 与 host mapping 前统一为
	diff=`new`、selection/scope=`file`，`old` 仍拒绝。确定性 lowering/mapper 与 Go/TS 对照测试覆盖拒绝和接受路径。
- 新增 `evaluation corpus snapshot build/show` 与 `run/resume/show` 本地 baseline corpus runner。模型执行前
	先把完整 Case 语义 digest、governance/label revision、split/clone group、committed source ReviewRun ref、
	TargetSnapshot ref 与 source ExecutionSnapshot digest 冻结成 content-addressed `CorpusSnapshot`；formal
	request 必须引用它，任一 Case/source closure 漂移都在 provider 前失败。请求只接受已经通过独立治理的
	active/evaluation-eligible Case，冻结四类 exposure 和可选 dimension scope；全部 Case 在 provider 前完成
	snapshot/remote-deny/权限 preflight 并先记录 exposure。
	CLI 将 exact runtime/component/config path/pricing 冻结为 credential-free content-addressed template，
	每 Case 用稳定 formal key 执行并在 host 重验 committed ReviewRun/ExecutionSnapshot/governed ledgers 后
	checkpoint；finalizing commit point 固定 EvaluationRun/terminal 时间。两 source CLI E2E 已证明同一 published
	ConfigRevision 可复用、第二次 run 与 resume 均不增加 provider runner 调用；preflight failure 0 provider
	调用且 durable failure 不保存 raw cause。它是 local corpus orchestration，不是 Hailix worker lease 或真实
	多仓质量结论。
- 2026-08-27 corpus admission 进一步要求单一 purpose/split：development=train/dev、quality gate=test、
	promotion gate=holdout；snapshot 内重复 clone group、split 漂移及 holdout 任意历史 `seen` 均失败关闭，
	formal preflight 会再次检查快照生成后的 exposure，避免陈旧快照绕过污染门禁。
- 2026-08-27 repository search adapter/provider revision 2 将 selection 的 exact target ranges 纳入
	provider request digest、execution receipt 与 index replay；检索词只来自圈选范围前后 32 行的合并窗口，
	artifact 保存 `target_ranges/query_halo_lines`，coverage 只声明精确圈选范围。focused Go tests 已覆盖范围外
	头尾噪声不进入 query；尚未用新的未污染 train/dev corpus 证明真实模型召回收益。
- 2026-08-27 使用 Hailix 历史修复 `b651675` 构建了一个本地临时、外部 Ed25519 多人治理的
	`positive_localized` Case，并以 DeepSeek Anthropic-compatible endpoint 运行真实 formal corpus：无自动
	context 的 run `formal-d354c5953bdc91c6fd69a6d0` 成功提交，但 0 Candidate 且因 4 个 context gap 得到
	`inconclusive/review_coverage_partial`，不能计为质量通过。该 run 报告 7 个 provider-reported receipt、
	137104 input / 31025 output / 409409 total tokens，authority 仍是 worker self-report diagnostic。
	同时发现 `repository_search` 对 1153-file 仓库逐文件串行 Git 读取会耗尽 30 秒 stage budget；现仅对明确
	声明并发安全的 Git source 启用最多 16 路读取，同一 Hailix commit 已在约 10 秒内成功冻结 66401-byte
	context artifact。随后 context-rich diagnostic 因 `pi_no_review_completed` 失败，且该 test clone 已参与
	调试，不能再冒充 blind quality evidence。以上均不是生产信任或多仓基线。
- 新增 `CandidateVerificationLedger v1alpha1`、`GovernedCandidateSet v1alpha1` 与
	`GovernedReviewReport v1alpha1`：每个 verifier observation 先成为独立、内容寻址且可严格回连
	Candidate/occurrence/sequence/evidence 的 Verification fact；ReviewRun 单独引用 ledger，Candidate/Finding
	中的 verdict 仅为受 ledger 校验的只读投影。每个 admitted
	Hypothesis 先成为 content-addressed、由 terminal ReviewRun 单独引用并可通过
	`candidate list/show` 查询的 Candidate 与 Verification fact；只有
	最新 verifier observation 为 `confirmed` 的 Candidate 才生成 Finding；rejected/inconclusive
	Candidate 继续保留但不制造 Finding。MVP Decision 固定为 `queued_for_human`，并显式记录
	`confidence_available=false/not_calibrated`，不授予远程发布或反馈/evaluation 真值权限。
- `FindingCalibrationLedger` 与 `FindingSuppressionLedger` 已成为独立 formal terminal Artifact：Pi reviewer
	可输出不进入 blind verifier 的 raw confidence；可选 `FindingGovernancePolicy` 与单调分段线性
	`CalibrationProfile` 通过 ConfigRevision scope resolve、FieldSource explain、ConfigBundle digest 和
	ExecutionSnapshot 冻结。formal terminal 从 exact bundle 做整数 PPM 校准；只有全部 Finding 均可校准才按
	confidence desc/FindingID asc 排名并执行 minimum-confidence 与 max-findings，阈值/限额 loser 均保留为
	可查询 suppression fact。缺 raw/profile 时整体 fail-open 到 human queue，不伪造分数。ReviewRun whole-closure、
	exact retry、通用 show、authenticated ReviewRun API、EvaluationRun 与 analytics source binding 已同时
	绑定两份 ledger；analytics 会把 suppressed 投影为非发布资格，Evaluation filter efficacy 会消费该 ledger。
	尚未宣称示例 profile 具有生产统计校准质量。formal replay 已新增独立
	`filter_policy` 单变量：只替换已有 provenance 的 exact `finding_governance` policy，保持 Pi build/tool/runtime
	authority 不变，保存 derived receipt/ChangeSet 并支持 exact retry。它从 `finding_governance` 直接
	开始，复用 source Hypothesis/Candidate/Verification/Report/Markdown refs，只生成新 Calibration/
	Suppression ledger；不创建 workload、StageAttempt、binding、task/usage evidence，也不调用 provider。
	链式 replay、show、同键异值冲突、伪造执行/report/calibration 拒绝及 durable ExperimentBatch+
	Evaluation/ExperimentRun 已有 E2E；post-processing variant 的 provider latency 显式 unavailable。
	这是 `v1alpha1` formal report family 的 fail-closed breaking closure：缺少两份新 ref 的旧本地 formal
	terminal 不再被当前 decoder/whole-closure 接受，需要从冻结 source replay 重建；没有静默补造迁移。
- `internal/calibration` 已新增受治理 profile fitting closure。每条 observation 必须同时回连 exact
	active EvaluationCase governance/label revision、committed ReviewRun/Candidate/raw confidence、
	repository/TargetSnapshot 与既有双人 review + 独立 adjudication 或 signed external governance；
	valid-defect 需匹配 label category/source anchor，false-positive 需由 clean snapshot 或 exact
	suppression fingerprint 建立。train 与 dev/test 的 case/clone/candidate 隔离、Artifact
	training/evaluation gate、正负类覆盖和 operator/labeler independence 均失败关闭，holdout 不参与。
	fitter 使用整数 PAV 生成单调 profile，并保存 overall/repository/dimension 的 Brier/ECE 与
	label-rate/mean-raw drift；gate 失败候选也不可变保存。CLI `calibration fit/show/list` 与 authenticated
	API `GET|POST /v1/calibration/runs` 已接通，输出固定 `auto_published=false`，不绕过 ConfigRevision
	publish/activate/rollback。
- `internal/training` 已新增受治理、reference-only 的训练数据 materialization closure。它要求 exact
	ConfigBundle 同时允许 training/export 且 strict redaction，只接纳 approved active train Case 与独立
	adjudication/external governance；请求完整 ArtifactRefs 必须精确覆盖 input/source/label/authority evidence，
	逐项通过 training/export integrity gate 并实际重读校验。内容寻址 manifest 冻结 governance/label/license/
	consent/classification/provenance 和 refs，明确 `contains_source_bytes=false`、`self_labels_allowed=false`；
	append-only ledger 支持 exact retry、冲突检测与 restore-time fail closed。CLI `training materialize/show/list`
	和 authenticated API `/v1/training/{manifests,manifest}` 已接通。
- `internal/training` 已在该 manifest 上新增受控正文 export closure：closed/versioned strict-text policy 固定
	12 类 secret/identity/path detector、1 MiB UTF-8 输入上限与 policy digest；每个 artifact 生成内容寻址
	redacted ref 和不含敏感正文的 receipt。Exporter 重验 manifest/source eligibility 与 bytes，restore 重放脱敏
	并比较输出，append-only export ledger 支持 exact retry、冲突与损坏失败关闭。CLI
	`training export build/show/list/publish` 已接通，publish 只在 store 外原子产生 `manifest.json + records.jsonl`；
	authenticated API `/v1/training/{exports,export}` 只 build/query，不接受文件路径。leakage corpus、schema/examples、
	API auth/strict decode 与发布边界已有测试。该本地规则集不等于通用 DLP；生产 ObjectStore/IAM、retention/delete
	传播、provider executor 与签名 provider receipt 仍未实现。
- `internal/training.JobService` 已新增 provider-neutral 外部训练作业记录闭环。Prepare 冻结 exact export bundle、
	provider/profile/base model、SFT 整数参数并固定 remote deny；Observation 只允许 plan-SHA CAS 下的
	`submitted -> terminal`，receipt 内容寻址并在 restore 重读验证。CLI `training job prepare/observe/show/list`
	与 authenticated API `/v1/training/{jobs,job,job/observations}` 已接通。所有 receipt 明确是
	`operator_recorded_unattested`，plan 固定 `promotion_eligible=false`；当前没有 provider 调用、签名 attestation、
	billing truth、模型注册/评测/promotion bridge。
- embedded Operator UI 已新增“训练治理”页面，复用上述 authenticated API 查看 manifest/export/job 历史，
	并提交 materialize、export build、job prepare 与 provider observation。UI 不接收文件系统 output path，
	不暴露 export publish，不读取 provider credential，也不调用训练 provider；浏览器仅为不含 secret 的
	operator receipt metadata 生成与 Go semantic digest 一致的 SHA-256。该页面仍是 single-process local authority，
	不等于 hosted IAM、provider attestation 或训练执行平台。
- `internal/calibrationpromotion` 已闭合 passed profile 到受控配置发布的显式操作链：prepare 只创建并
	validate 精确 ConfigRevision，typed managed binding 阻止通用 promotion 入口绕过；targeted regression
	重验单变量 ExperimentRun 的 exact baseline/variant bundle，fixed holdout 重验独立 actor、case population、
	每个 ReviewRun 的 variant bundle、fit manifest 隔离与既有 not-seen exposure。全部 gate 后仍需显式
	activate；跨 config/evaluation ledger 的 prepare/activate/rollback 使用 durable intent 和派生幂等键恢复。
	CLI、authenticated API 与 Operator UI 已接通，读写同时要求 evaluation/config 权限。
- `internal/promotionmonitor` 已闭合 activation 后的本地质量窗口观测：每条不可变 observation 绑定 exact
	managed Plan SHA、activation、两个 immutable Dashboard snapshot SHA 和版本化整数 policy，只选择
	baseline/variant bundle 的 exact ReviewRun cohort。run success/complete、published Outcome coverage、fixed 与
	adverse rate 保留原始分子分母；partial 与不足样本分别为 unavailable/insufficient_data，只有 evaluated
	regression 生成非自动执行的 rollback recommendation。CLI、authenticated API 与 Operator UI 已接通；API
	额外要求 dashboard read，服务没有配置写端口。focused test 已覆盖混合 config 过滤、窗口/基线替换拒绝、
	weak-signal 排除、partial/不足样本不误报、immutable exact retry/conflict 和严格 decoder。
- Verification/Calibration/Suppression ledger、CandidateSet 与 governed JSON/Markdown 报告使用 content-addressed artifact 保存，formal `run/show` 均返回
	exact refs。`formalreview.Finalizer` 只从冻结 snapshot 与 authoritative formal ledgers 重建
	StageAttempt、PlatformExecutionBinding 和 RunEvidence；Repository 对 Hypothesis、Verification ledger、治理报告、
	目标和 ledger 做 whole-closure 校验后提交统一 terminal `ReviewRun`。成功/失败 exact retry
	复用同一 final artifact，通用 `history/show` 已有 CLI E2E。Argus 侧 production-shaped Hailix
	PlatformPort adapter、严格 HTTP consumer、request-time environment credential、正式 CLI backend
	composition、machine schema 和 formal
	provider-commit/response-loss recovery E2E 已完成；当前仍没有 Hailix public server、远端
	credential/IAM attestation、通用 current-cancel service、runtime attestation 或
	provider-authoritative atomic create-or-return-one-handle/outbox。formal same-input exact replay 与 budget/model/prompt/skill_pack/knowledge_pack/rule_pack/workflow/index/filter_policy
	variant 已接通：variant 以 derived config receipt 绑定 baseline/source receipt；budget 只原子改变
	stage/Pi timeout，model 只改变 subject-bound model component 与对应 manifest build identity，
	prompt 只改变 exact governed prompt component 与对应 build identity；skill_pack 只允许保持完整
	ordered skill ID/phase 不变并替换 exact governed Artifact，未覆盖 builtin 保持原 ref；runtime-file evidence、tool policy、
	authority/budget 保持冻结。同键异值在 provider 前冲突，remote deny 保持不变。formal compare 已按
	cluster fingerprint 对齐 run-local Candidate ID。workflow variant 已只开放单阶段有效资源预算收紧：exact
	definition/config/receipt/change-set 同时绑定，compiler 将 policy/stage 逐项最小值写入 Plan，build/tool/runtime
	保持冻结，identity-only、retry/authority/graph 变化失败关闭。新增/删除技能与任意 stage
	checkpoint recovery 仍未开放；formal Pi 已实现受限的 group/candidate-verification checkpoint，恢复后仍重跑全局
	normalization/dedup，只复用 candidate digest 与 frozen evidence 重验一致的 completed verifier。prompt Artifact 的执行消费、prompt-only derived ConfigResolutionReceipt、variant
	admission/CLI 和 baseline-vs-variant E2E 已闭合，不能只替换 caller-supplied config ref。
	formal worker 的每个 review skill 现在也由 host 从 subject-bound Artifact 解析 exact bytes，
	Go/TS 双侧校验顺序/ref/contract/size/base64/UTF-8/SHA 后由 Pi runtime 实际消费；skill_pack
	replay 的 CLI、derived receipt、initializer/compiler/repository admission、exact retry 与同键异值
	provider-before-dispatch conflict E2E 已闭合。
- formal run/bootstrap 已接受 repeatable clean-absolute `--knowledge`，把 repository/business-line
  KnowledgePack 发布为 subject-bound component 并进入 config/manifest/build identity。PlatformPort
  从 Plan Artifact 解析 exact bytes，worker Go/TS admission 校验顺序/ref/contract/size/base64/UTF-8/SHA，
  并作为 untrusted reference data 实际注入 context/review/verifier；snapshot 回显同一 ID/digest/bytes。
  contract、bootstrap、formal mapper round-trip 和 TS runtime consumption 已验证。knowledge_pack-only
  replay 也已闭合 derived receipt、initializer/compiler/repository admission、CLI、variant component
  publication 和真实 formal E2E：Pi 消费 variant marker，build identity 改变，runtime evidence/tool
  policy/remote deny 冻结，exact retry 不重跑，同键异值在 dispatch 前冲突。
- rule_pack-only replay 已闭合 sealed RulePack 的 derived receipt、initializer/compiler/repository、
  CLI/API/UI/Batch/Evaluation 输入链路。AgentStagePlan 同时绑定语义 ref 与 exact base64 bytes，Go/TS
  双侧重算 semantic digest；Pi context/review/verifier 实际消费同一受治理判据，且不能扩大 authority、
  output contract 或 side effects。RulePack 变化不伪造 runtime/component build identity。
- workflow-only replay 已闭合 exact WorkflowDefinition、derived receipt、initializer/compiler/repository、
  CLI/API/UI/Batch/Evaluation 输入链路。仅 stage timeout/input/output/concurrency 可变，且必须改变 sealed
  AgentStagePlan 的有效预算；runtime/build/ToolPolicy/authority/target 保持冻结，exact retry 与链式 replay
  均重验持久 definition bytes。
- `review --context-file KIND@COVERAGE_SYMBOL=ABSOLUTE_FILE` 已能把外部 CodeGraph/LSP/repository/dependency 输出
  发布为 content-addressed frozen ContextRef Artifact。formal PlatformPort 按 ReviewInput exact
  顺序解析 bytes，worker 复核 ID/kind/revision/URI/contract/digest/size/base64 并由 Pi context
  collector 实际消费；成功 ref 不再伪报 unavailable，ContextGap 继续进入 coverage，且上下文
  不能扩大 anchor 或提升权限。Go contract/mapper/CLI 与 TS consumption/substitution tests 已覆盖。
- `review --context-provider repository_search`、`go_ast`、`go_dependencies` 与 `go_compile` 已为
  diff/selection/scope 接通四条自动 exact-revision provider：mutable ref 先冻结为 commit OID，
  所有源码只经 Git object adapter 读取；输出严格 `GoASTContext` 与 `GoDependenciesContext`
  以及独立 `RepositorySearchContext`、`GoCompileContext` contract，分别包含高区分度标识符的
  lexical repository matches、declaration/type/direct caller-callee/bounded call path、module/package/import resolution 和
  compile facts，
  coverage 与 typed gaps，并以 frozen ContextRef 进入同一 formal Pi transport/consumption E2E。CLI provider 先进入
  invocation ConfigBundle；published lifecycle config 可直接驱动同一 application executor，且拒绝
  invocation 旁路。executor missing、adapter mismatch、timeout、overlay、capture/output failure
  已投影为 canonical-request-bound ContextGap；exact replay 复用 frozen context，不重新读 Git。
  application port 按冻结的 `context_provider_max_concurrency` 运行有界 worker pool，保持配置顺序的
  结果与 receipt，一个 provider 的 gap 不抹除其他结果，parent/fatal cancellation 会传播。
  每次执行已有 strict `ContextProviderExecutionReceipt`，绑定 config/request/exact target/context
  outcome、timeout/latency 和 local authority，由 ExecutionSnapshot 引用并在 replay 复用。application
  在 artifact publication 前重验 executor observed revision；四个本地 provider 也重验 source listing
  和逐 file revision。漂移内容不会被消费，而是形成带 requested/observed exact OID 的
  `context_revision_mismatch` Gap receipt，并可按 typed reason 进入 analytics。
  正式 analytics/dashboard 已从该 receipt 构建 versioned `context_provider_fact`，区分
  executed/reused，输出 attempt/success/gap、typed gap reason、duration p50/p95，并支持
  JSON/CSV/typed Parquet 表；Replay 不重复贡献执行和延迟样本。
  `go_ast` adapter revision 2 已修复同一行重复调用缺少 column 导致 fact 冲突的问题，并以
  `go.mod` module path 建立 exact-revision local importer，递归 type-check 同仓 package 后生成
  target-rooted 最多三跳 upstream/downstream path；path
  symbol/site closure、simple-path、排序、2048 path/artifact budget 和 strict schema 均失败关闭。
  module 外 external import 不读取 ambient module cache；external/name-match/unresolved/dynamic edge
  不会被提升为精确多跳语义。缺失/非法 module 与 package 冲突进入 typed gap。
  `go_compile` 只从 exact Git object 物化普通源码，以隔离环境变量和 process group 执行
  `go test -c`；真实测试代码不执行，编译诊断、toolchain identity、输出 digest/size 和
  passed/failed/unavailable 进入 `GoCompileContext`。真实 Go toolchain 测试已证明 TestMain 未执行、
  module network 关闭、取消/输出上限生效；embed/cgo/local replace/`.syso`/不完整输入均显式 unavailable。
  formal Pi E2E 已消费该 exact context kind，replay 在 working tree 漂移后仍复用原 ref。
  `repository_search` 在 blob read 前拒绝 environment/credential/state/key 路径，保留 listing/file/
  target/query/match/artifact budget gap 和对账 coverage；它是 lexical evidence，不冒充 CodeGraph/LSP
  semantic resolution。CLI E2E 已证明 exact match、working-tree 漂移后 replay 复用，以及 formal Pi
  worker 同时消费 compile 与 repository_search kind。
  尚未实现的是受 Hailix sandbox 隔离的 repository test execution、CodeGraph/LSP semantic adapter 和远端
  platform attestation。formal stage 已支持 plan-bound multi-attempt/generation；失败历史与最终 attempt
  分别保留 dispatch/binding/callback/terminal 事实，unknown outcome 不消耗新 attempt。正式 CLI E2E 已证明
  合法 worker report 的全部 review task 首次以 Plan 白名单 `provider_error` 失败后第二 attempt 成功、外层
  workload lease 坐标保持不变，终态 exact
  retry 从最新内层 dispatch 恢复且不再次调用 runner；未授权 worker failure code 失败关闭为不可重试。
- formal runtime component 现在是详细 `LocalRuntimeFileManifest`：绑定 Node executable、
	`dist/*.js`、package metadata/lockfile 以及 lockfile production packages 中所有实体普通文件；
	同一 canonical bytes 同时保存为 governed component 与 ExecutionSnapshot local evidence。
	PlatformPort 在 dispatch 前和 process start 前重新计算，任何漂移都阻止 worker/provider。
	该 authority 明确为 `local_host_observation`，不等于 Hailix/IAM attestation；shadow direct
	与 formal local Pi 的 task evidence 均已独立 artifactize，后者通过 StageExecutionResult 和
  统一 ReviewRun 引用闭合。
- Pi task evidence 只保留已通过 runtime admission 的 tool executions；预算拒绝的 tool request 不进入
	执行 transcript/receipt counter。terminal submit 仍计入观察但不占 repository tool budget，失败的已准入
	tool 同时进入 invocation/failure counter。真实 DeepSeek 触发过预算拒绝路径，Go host 已验证 transcript、
	receipt 和 failure counter 守恒。
- formal local Pi 发布的 task evidence governed source 同样只允许 sensitive process/read，
  因而不能通过普通 Artifact read/export。`formal evidence read/revoke` 不接受 caller-supplied
  artifact/scope，而是从 committed ReviewRun -> exact attempt/intent/terminal result whole-closure
  解析 governed source；读取沿用 purpose-bound receipt，撤销后 `formal show` 仍返回 tombstoned
  摘要和 immutable terminal history。host-localized exact copy 仅供 terminal closure 重验，物理
  bytes 仍等待 reference-aware GC；因此 logical revoke 不能扩张为磁盘级删除证明。
- compiler 只接受具有 authoritative source/change-set closure 的 formal same-input exact replay
	或 budget/model/prompt/skill_pack/knowledge_pack/rule_pack/workflow/index/filter_policy-only variant；继续拒绝非空 upstream dependency、任意 stage checkpoint reuse、其他 variant 与未绑定 retry 变化，不能把本地 ledger baseline 描述为已部署 executor。
	retry max/backoff/error-code/unknown-outcome 已进入 exact AgentStagePlan behavior identity。

## M1 已实现的本地闭环

- Git diff source adapter：
  - 将用户 revision 解析为 exact base/head commit OID 后再执行 diff；
  - 生成 canonical patch、文件/语言/hunk coverage manifest；
  - patch/file 限额、unsafe path、stale revision、binary/symlink/submodule 等情况
    以 partial/skipped reason 留痕，不返回静默截断；
  - 从 exact head commit 安全读取普通文本文件，不读取可变 working tree 内容。
- 本地持久化：
  - content-addressed artifact 使用
    `artifact://local/sha256/<digest>`；
  - JSONL run ledger append + fsync，事件 ID 幂等且冲突失败；
  - ExecutionSnapshot 冻结 ReviewSpec、TargetSnapshot、ReviewInput、Workflow、
    Config 和 replay input refs；
  - terminal event 引用 content-addressed final ReviewRun，加载时校验证据闭包。
- 独立执行证据：
  - StageAttempt、PlatformExecutionBinding 和 RunEvidence 分开记录；
  - binding 包含 idempotency key、attempt/generation、fencing token 和 runtime
    identity；
  - final projection 必须与 append-only ledger 和 artifact digest 对齐。
- 同一 deterministic implementation 跑通
  `materialize_target -> plan_context -> detect -> normalize -> verify -> adjudicate ->
  report -> publish -> capture_feedback -> export_evaluation`：
  - 最后三个领域 stage 当前只落 `remote_disabled`、`awaiting_feedback`、
    `candidate_only` lifecycle fact，不冒充真实远程评论、反馈或 gold label；
  - 封闭 detector registry 包含 marker baseline（新增 Go 注释中的 `TODO`、`FIXME`、
    `ARGUS_BUG`）和冻结源码 Go AST discarded-context-cancel rule；AST 内容不可用或 parse
    failure 会生成 coverage gap，RulePack 可启停已注册规则；
  - verify 使用 frozen exact-commit file content；上下文不足时保留
    inconclusive/human-review，不伪装成 verified；
  - Candidate、Finding、Decision、report 和 stage checkpoint 分开保存。
- application service 已实现 review、按 stage checkpoint replay、same-target
  finding fingerprint compare、history、分类 retry 和 context cancellation；
  默认最多两次 attempt，所有本地 ToolPolicy 继续 deny network/workspace/remote
  write。
- 本地 CLI 已接入同一 application service：
  - `argus review`
  - `argus replay`
  - `argus compare`
  - `argus history`
  - `argus show`
  - 默认 Markdown 输出，并支持 `--json` 供脚本消费。
- focused tests 已覆盖 exact diff/file capture、store 幂等与损坏拒绝、证据闭包、
  review/replay/compare/history、retry、cancellation、partial context、duplicate 和
  marker 真假例。

初始 2026-07-26 已用临时本地 Git 仓库完成零 Finding 与 verified Finding 两类 CLI
smoke；当时的五阶段 attempt 数是历史基线，随后已扩展为上面的十阶段 artifact chain：

- 当时 review `succeeded`，记录 5 个 stage attempt、5 个 binding 和 5 个 evidence；
- 从 `verify` replay `succeeded`，复用 `detect`、`normalize`，执行 3 个 stage；
- verified Finding 路径产出 1 条 `explicit-bug-marker`，compare 得到
  added 0、removed 0、unchanged 1；
- history 返回两个 succeeded run，show 可重新加载 report；
- `make verify` 通过 format、vet、全仓 test、docs/schema/example 校验和 build；
  application/run ledger 的 race/shuffle focused tests 也通过。

操作与验收说明见 [M1 本地纵向链路](M1_LOCAL_VERTICAL_SLICE.md)。

## 相邻项目已验证事实

### Hailix

- 已实现 Task admission、Codex ACP Worker、Worker identity、Trace/Artifact、
  Outbox/Projection 和 Langfuse 执行链路。
- production `internal/localproduct/app.go` 已把 Gateway、Frontier、Artifact content、
  Outbox 和可选 Worker Runtime 接到真实入口；`StartWorkerRun` 的 permit/session/runtime
  generation 与事务性 Task/WorkerRun/AgentTurn/Runtime/Outbox 仍是 Hailix 内部协议，
  不是 Argus 可调用的公共 API。在线 migration/deployment 与当前 checkout 是否一致未验证。
- 当前外部入口接收收窄的 DirectTaskInput，由服务端生成 TaskSpec identity/readiness，
  Argus 不应自行伪造 TaskSpec。
- Argus `internal/hailixexecution` 已实现 consumer adapter，并已接到正式 `agent-review formal run/replay`
  的显式 `hailix-http` backend：exact plan capability、
  完整 Subject 绑定的 Ensure/Lookup/Await/Cancel、typed terminal 和 pinned callback verifier，并有
  unknown-outcome、跨 repository receipt substitution、persisted-request trust drift 等 fake contract tests；
  CLI 只从固定环境变量按请求读取 credential，且 pinned verifier/base URL 必须显式配置。Hailix 尚未提供
  匹配的 public server endpoint，因此不能写成 production wiring。
- 当前为 loopback single-user profile；动态 tenant/IAM、通用 analytics、Evolution/
  Truth 和共享模型网关不是已完成能力。
- TaskSpec 当前只接受 `codex-acp`，ArtifactRef 当前只接受 `report`/`patch`；production
  ACP permission 默认 cancel、terminal operations unsupported，初始化仍声明文件写能力，
  不满足 Argus review-safe capability。

### Eino-Agent

- 以下能力来自 2026-07-26 的既有源码审计；2026-08-25 当前 `../eino-agent` 只含
  `.yhc` 会话数据，没有可复核源码，canonical checkout 路径尚未恢复。
- ACP stdio、session、prompt stream、permission、scoped cancel 和完整 QueryEngine 可用。
- 不提供 ACP-native review DAG、结构化 review result、durable replay cursor 或强文件
  系统沙箱。
- 目标仓库的 hooks/MCP/skills/plugins 可能在评审 prompt 前加载或执行；当前不能安全
  地把不可信 checkout 直接作为 ACP CWD。

Hailix 条目是 2026-08-25 本地 checkout 事实；Eino-Agent 条目仍是 2026-07-26 历史
证据且当前不可复核。两者都不是未来兼容保证。

### Deterministic scope shard / crash recovery

- `internal/reviewshard` 已实现 immutable `ReviewShardManifest`、exact content-addressed
  shard ReviewInput、coverage gap、append-only generation/checkpoint/aggregate ledger、
  CAS、per-shard attempt 与旧 generation fencing。serialized shard input 自身也受 byte
  budget 约束，不用源文件 bytes 冒充真实输入大小。
- 异步 API 的 `deterministic_review_v1` scope 已接线：复用外层 ReviewJob dispatch authority，
  按冻结 budget 并行执行 pending shard，不创建会与 `full_scan` pool 自锁的嵌套 workload；
  fan-in 重新生成 full-target Candidate/DetectionGap identity，再复用既有下游 stages。
- ExecutionSnapshot 现在冻结 shard manifest ref；run closure 回读并校验 manifest 与 exact
  run/input/target binding。manifest gap 会把全部 Stage `RunEvidence.completeness` 降为 partial，
  保留逐 path reason。
- focused recovery 已验证：generation 1 三个 shard 中只完成一个后，generation 2 从同一
  store 只补两个 pending shard；最终 aggregate 混合保留 1/2 代 provenance，65 个完整 target
  Finding 闭合，generation 1 的迟到 checkpoint 被 durable fence 拒绝。另已覆盖 aggregate
  已提交但 detect stage fact 未提交的重放窗口、checkpoint 时间早于 generation 的拒绝与 race。
- physical acceptance 已在真实子进程完成一个 shard 后执行 SIGKILL；lease 过期后 generation 2
  只执行剩余两个 shard，最终闭合 65 个 Finding、混合 generation provenance，并拒绝 generation 1
  迟到 checkpoint/callback。这仍是 local ReviewJob/scheduling authority，不是 Hailix attestation。

## 尚未实现

- deterministic scope shard 已完成物理 worker SIGKILL 恢复验收；仍缺远端 Hailix worker 与
  CodeGraph/LSP provider 的多 attempt/recovery。selection、multi-range/Go symbol、exact-revision scope、配置驱动的
  本地 `repository_search`/`go_ast`/`go_dependencies`/compile-only `go_compile` provider、
  有界并发、typed ContextGap 和 local execution receipt 已有实现。
- 代码平台事件/评论 adapter、远程发布和 unknown-outcome recovery；Feedback/Outcome
  当前只有本地 ledger/CLI，ValueObservation 只有本地 analytics fact/projection。
- Config 的 hosted 多租户 Web/API 管理面、IAM 签名发布、远程分发/缓存失效和跨实例一致性；
  本地 ConfigRevision/ConfigBundle/RulePack resolve、publish、percentage rollout advance、rollback、
  explain、bearer API 和 embedded UI 已实现。
- formal 本地 Pi detector/verifier、typed result sink 与 governed Candidate/Finding/Decision
	报告和统一 ReviewRun terminal projection 已接通；尚缺可重放下游 stage、production
  Hailix server integration 和 publisher。MVP Finding 只排队人工复核，不自动发布。
- Hailix public platform-execution server/IAM、Eino-Agent review-safe adapter、代码平台 adapter；
  Argus 侧 Hailix consumer/HTTP contract 与正式 CLI backend 已通过 loopback formal 联调，但尚未与真实
  Hailix public service 联调。
- 生产数据库/ObjectStore、hosted dashboard 和多租户 Web/API；正式本地九表
  Parquet/dashboard CLI、bearer API 与 embedded UI 已实现，AgentExecution 诊断投影虽覆盖四类
  execution 状态，部分导出当前仍只有 JSON/CSV。
- Agentic EvaluationRun/Experiment runner 和真实 shadow/canary executor；本地
  EvaluationCase、exposure、holdout 与 promotion governance ledger 已实现。EvaluationRun 的
  真实 Finding 入口也已增加 candidate derivation：最新 human publish Decision 或未被更正的
  accept/dismiss Feedback 可派生 pending/unassigned/candidate-only Case，repository/snapshot/source/
  anchor/fingerprint 由控制面回读，含糊反馈拒绝，且绝不直接升级 gold/active。派生 candidate 现已
  接入 append-only review governance：同一治理轮次至少两个 distinct reviewer annotation，第三个
  independent adjudicator 冻结 exact annotation events；approve 只能选已复核 label，并先进入
  unassigned/ineligible gold。独立 activation 才能扩展 license/consent、分配 split/eligibility，holdout
  另需 maintainer 权限，stale revision/同 actor 重复 review/越权与 split contamination 均拒绝；
  immutable ingress Case、current governance、annotation/adjudication/label history 可同时查询。
  `reviewed_bug_fix_pair` source builder 也已接通：latest unsuperseded fixed Outcome 必须以 change+CI
  refs exact 绑定同仓后继 fix ReviewRun head，fix base 必须等于 defect head，四组 defect/fix artifact
  通过 candidate-pool gate 后只产生 provisional fix_validation Case。
  `incident_missed_defect` 也已实现独立 append-only Incident ledger 与 source builder：source revision、
  fingerprint、anchors、exact evidence refs 可修订不可覆盖/分叉；完整 formal report、frozen target
  anchor coverage 和全部 canonical Candidate negative search 闭合后才产生 provisional missed-defect
  Case，partial/已检测/越界/quarantine 均失败关闭。
  mutation/synthetic/workflow source 也已接入独立 append-only Evaluation Probe ledger：source、oracle、
  executor authority 分离，receipt exact 绑定 formal TargetSnapshot、oracle digest、版本化 method、实际
  observation、evidence 与 remote deny；mutation 额外绑定同仓 distinct baseline。builder 会重验全部
  artifact、target/selection anchor 与 chronology，并拒绝以任何 Argus review output 作为 oracle evidence。
  四类正向 E2E 已分别产生 mutation diagnostic、synthetic defect、negative clean 与 workflow invariant
  candidate；它们仍是 pending/unassigned/ineligible，synthetic consent 与 no-production-distribution 保持强制。
  candidate governance 已加入 mandatory blind assignment：2..8 个 assigned reviewer 才能在 exact round
  annotation，reviewer read 隔离同轮其他身份/意见，adjudication 必须覆盖每个 assigned reviewer；agreement
  从事件重算 counts、verdict/exact-label unanimous/disagreement/incomplete。gold/active/retired 可 append-only
  reopen 到 pending candidate-only，清空未来 split/eligibility、冻结 affected experiments，并要求新一轮 assignment。
  direct `CreateCase` 已收窄为 pending/unassigned/ineligible candidate_pool ingress，不能再直接写入
  gold/active；外部已治理 Case 使用独立 signed import，Ed25519 attestation 绑定 exact Case digest、
  input/provenance evidence、policy、2..8 reviewers 与 independent adjudicator，frozen key 还限制 repository/
  classification/validity。导入 operator 必须独立且有 curator/holdout 权限，event restore 会重验签名，
  `CaseRecord.external_governance` 保留完整 lineage，exact retry 幂等。key 必须先由独立
  `governance_trust_admin` 写入 append-only registry，import 逐字段匹配 active revision；注册/撤销有严格
  contract、CLI、角色授权、幂等与 restart E2E，撤销不追溯破坏已导入 Case。当前 actor/role 仍是本地
  authority，不是 Hailix/IAM identity 或生产 trust root。
  assignment/annotation/adjudication/activation/reopen 现可通过同一 homogeneous GovernanceBatch service/CLI
  批量执行：1..500 items、intent-before-transition、stable item checkpoint、exact retry/crash resume、
  succeeded/failed/not_started terminal 与 exact dataset-event restore revalidation 已由领域和 CLI E2E 覆盖；
  reviewer batch 仍保持 blind read isolation，holdout 仍走原 ACL。该能力不是跨 item 原子事务、hosted API
  或分布式 worker。
  EvaluationRun 的
  只读评分/不可变落账核心也已接通：它闭合 exact label revision、exposure、TargetSnapshot、
  committed formal report 与 remote deny，并在 label 竞态时失败关闭。ExperimentRun recorder 也已
  校验相同 case/label/evaluator 和逐 case direct replay/declared variable lineage，并冻结两侧
  committed run ref、ReplayChangeSet ref 与 config digests 后输出质量转移；它同时从两侧 committed
  ReviewRun 的全部 StageAttempt 台账复算 baseline/variant duration 与 exact delta，缺失 timing 时
  失败关闭，并明确标记 `review_run_stage_attempts` 本地 authority；
	本地 durable ExperimentBatchRunner 已完成 intent-before-provider、exposure-before-execution、
	受控并发、稳定逐 case 幂等、committed run/change-set/config digest revalidation、append-only
	逐 case checkpoint、恢复时只执行缺失 case、自动 EvaluationRun/ExperimentRun/terminal 与
	terminal retry reuse；batch stream 已用 expected-sequence CAS 接入 durable claim/heartbeat、
	generation/fencing 和过期接管，checkpoint/terminal 必须绑定当前 lease；CLI 同进程复用 formal Pi replay。
	真实 formal replay CLI E2E 已验证 ConfigBundle artifact digest 与内嵌语义 digest 分离、variant
	change-set closure 和 terminal exact retry 不二次调用 runner。
	故障测试还验证 case checkpoint 后、EvaluationRun 前失败可以恢复且不再次调用 executor。
	Evaluation Label 已新增排序的 path/side/line-range/source-digest 真值，EvaluationRun 对完整报告
	执行 digest-bound range-overlap localization，输出 expected/matched anchor 与 localized finding 计数；
	ExperimentRun 输出 paired matched-anchor delta，partial coverage 保持 localization unavailable。
	false-positive regression Label 还必须冻结排序唯一的 suppression cluster fingerprint；EvaluationRun
	只在完整 GovernedReport 中 target Candidate 实际出现时，按 rejected/confirmed/inconclusive 输出
	observed/suppressed/escaped/inconclusive target 整数事实，target 未出现和 partial coverage 都保持
	filter efficacy unavailable；ExperimentRun 输出 evidence-bound paired suppressed/escaped delta。
	fix_validation 已新增 exact-bound ApplyTrial：重新读取并校验 edit script 与 dry-run/compile/test
	evidence artifact，conclusive trial 驱动 fix verdict，partial 保持 inconclusive；ExperimentRun 输出
	paired passed/failed check delta。当前只具备 `local_host_unattested` recorder/evaluator，仍缺隔离 Apply
	executor/Hailix attestation。formal Pi receipt 已从 StageExecutionResult 严格贯通到 committed
	ReviewRun；EvaluationRun 复算 worker-self-reported token facts，ExperimentRun 只在两侧完整
	provider-reported 时输出 input/output/total token delta。账单成本仍明确 unavailable，不从
	PricingCeiling 或 receipt 推算。独立 RepeatabilityRun 已要求 succeeded non-replay baseline 与其 direct
	`variable=none` exact replays，重验 committed refs、ExecutionSnapshot/ChangeSet 的相同 target/input/config/
	workflow/runtime/tool policy，按稳定 FindingID 输出 pairwise Jaccard、exact-set/presence/anchor-hit 与
	verdict-flip PPM；partial report 令 Finding-set stability unavailable。dimension scope 还会输出逐维
	Finding/verdict 稳定性与 Candidate/raw/context-gap/task-failure/duration/token 区间，并分别标注 execution/
	usage availability。append-only restore、幂等、授权 show/list、严格 Schema/example 与 CLI 已闭合。
	新增 durable RepeatabilityBatchRunner：provider 前冻结 sample×case 矩阵，使用稳定幂等、逐单元 checkpoint、
	缺失单元恢复、host exact replay revalidation、sample EvaluationRun 与最终 RepeatabilityRun 自动闭合，
	并具备 claim/heartbeat/generation/fencing/过期接管及 `evaluation repeatability batch run/resume/show`。
	Experiment/Repeatability batch 的 intent 现都冻结并绑定 credential-free、content-addressed exact Pi/
	transport executor template；除 runtime/component bytes 外，还保存 `local-pi` 或 `hailix-http` endpoint 与
	pinned verifier identity。CLI 直接复用 formal replay backend flags；local API 使用独立 `formal-batch-*`
	启动 profile，避免普通 formal ReviewJob 隐式切换 backend。resume 只需 batch/access，模板 ref 不一致或
	漂移会在 provider 前失败；Hailix/provider credential value 不落盘并按请求解析。终态 resume 已验证不再次
	调用 executor。还缺真实 Hailix server 和权威成本/provider 时延 attestation。

- deterministic `workflow` 单变量 replay 已接通 CLI 和 whole-closure：variant ConfigBundle 必须绑定
	独立 strict WorkflowDefinition 的 exact ID/revision/SHA，且同 workflow ID 使用新 revision；只允许改变
	实际 scheduler 消费的 stage budget 和 retry max-attempt/backoff/retryable-code。graph、executor、contract、
	authority、failure/side-effect/unknown-outcome/replay policy 冻结；最早受影响 stage 控制 checkpoint reuse。
	ExecutionSnapshot 保存 variant bytes，Repository terminal validation 重读 source/variant artifacts 并重算
	changed fields。测试证明 retryable-code 变化会实际改变 attempt terminal 行为，identity-only/缺 definition/
	结构漂移失败关闭。formal Pi workflow variant 采用同一 exact diff 基础，只允许改变单 stage 资源预算
	或 retry policy，并由 AgentStagePlan 和 formal orchestrator 实际消费。

- deterministic `index` 单变量 replay 已接通 CLI、provider execution 与 whole-closure：唯一可变配置是
	ordered `execution.context_providers`，exact changed fields 覆盖 provider add/remove/order 及
	ID/revision/kind/adapter ref 差异；agent/model/credential/tool/concurrency 与其他配置冻结。replay 强制从
	`materialize_target` 开始且无 checkpoint reuse，在 source exact repository ID/head commit 和 immutable
	manifest target paths 上重新采集，写入新的 ContextRef/Gap、receipt、ReviewInput 与 MaterializedTarget，
	并保留 caller-supplied contexts。terminal closure 独立重读两侧 config/target/input/receipt，确认除 configured
	contexts 外目标和输入不变。CLI E2E 已证明从空 provider 集切到 exact Go AST provider。
	formal Pi `index` 现复用同一物化器，但以 host-precomputed provenance 进入 AgentStagePlan：built-in adapter
	descriptor 作为 exact subject-bound component 发布，Pi 只消费冻结 Context Artifact。plan admission 在模型前
	重验 receipt/config/adapter/repository/commit/binding/provenance，terminal 再做 whole-closure；exact retry 不重复
	provider。CLI 与本地 Platform ExperimentBatch E2E 已证明 fresh target/input/receipt、Pi 重跑、Case 对 target
	除 contexts 等价的安全绑定和终态幂等。authority 仍是 `local_host_observation`，不等于平台 attestation。
	RepeatabilityRun 已通过独立 RepeatabilityFact 接入 analytics anti-corruption adapter；dashboard 输出
	case/dimension stability 与 execution/usage range observation，partial availability 保持 unknown。
	canonical JSON/CSV 和 repeatability typed Parquet 表保留 repeatability/case/review_dimension/source refs；同时修复
	既有 dimension ExperimentFact 在 dashboard grouping、CSV 和 Parquet 丢失 review_dimension 的问题。
	本地平台 API 现以只读 port 和进程固定 principal 暴露 EvaluationRun、ExperimentRun、RepeatabilityRun
	以及 ExperimentBatch、RepeatabilityBatch 的授权 list/show；Repository batch list 会逐项重验 baseline
	Case ACL，缺失 baseline 视为 corruption。两类 running batch 现可通过 `evaluation_write` 的 strict resume
	command 异步恢复：请求另记 append-only audit、不替换 intent actor；adapter exact-load executor template，
	HTTP 断开不取消；后台失败只保存固定 code + lease/generation/fencing，不落 raw error。Operator UI 展示
	checkpoint/lease/last failure 并提供恢复动作。配置了 formal Pi profile 的 local API 还可通过 plural
	POST 异步提交 budget、exact configured-provider model ID、exact published prompt/skill/knowledge 或 ordered built-in
	context provider 单变量 ExperimentBatch 与 exact
	RepeatabilityBatch：caller 不能提供 template ref 或本地 component path；组件 ref 按每个 baseline
	ReviewRun 派生的 subject 解析并在 admission/provider 前逐 subject preflight。adapter 从启动配置冻结 exact
	credential-free template，intent-first 后执行；相同 command retry 不重复调用 provider。Operator UI 提供
	变量选择、component refs 与 exact repeatability 表单。三类运行和两类 batch 仍分开展示，稳定性不会被
	呈现成质量提升。列表 cursor 仍不是冻结 dataset snapshot，且该能力不等于 hosted IAM/多租户平台。
	独立 `component_write` POST/UI 现可发布 prompt/skill/knowledge：subject 从 committed baseline ReviewRun
	闭包推导，principal actor 注入，内容 contract/digest/revision、审计与幂等均在写入前验证；caller 不能提供
	subject 或路径。它仍不是签名供应链或 hosted component registry。
	model-only template 冻结新 model component/build identity，保持 provider/runtime/API protocol/预算/credential
	不变，并在每个 run intent 后、provider 前完成 subject publication；终态 retry 不重复 provider。
	EvaluationRun binding 现可显式冻结最多 16 个 exact dimension refs；每维结果从 committed
	GovernedReport 与 receipt 复算 Candidate disposition、Finding/localization、review/verification task、
	turn/tool、cumulative task duration 和 attributable token。verification 只能按 hypothesis occurrence
	回连原维度，共享 context 不任意分摊；没有成功 review receipt 时维度 verdict 为 inconclusive。
	ExperimentRun 只按相同 dimension ID 比较并保留两侧 exact revision/SHA，输出 Candidate/Finding、
	duration/token 与 verdict transition；analytics 增加受控 `review_dimension` 的独立 token/duration facts。
	新 formal Pi run 已从 StageExecutionResult admission 到 committed ReviewRun 引用 exact raw candidate
	collection，EvaluationRun 直接保存 hypothesis/raw refs 并复算各维 retained/merged/rejected/excluded
	整数事实，ExperimentRun 在两侧可用时输出 paired delta；旧 run 保持显式 unavailable。尚未把这些整数
	committed hypothesis coverage 与 task/group receipt 已形成 dimension-specific context-gap count/delta；
	analytics 对 duplicate/invalid/budget/context-gap 输出独立整数 facts，并以 sum(fate)/sum(raw)、scale=6
	生成三个 normalization rate。任一侧 evidence unavailable 或 arm raw 分母为零时保持 partial。
	dashboard rebuild 已接入受治理 Evaluation/Experiment ledger 的窄 source adapter，按 scope/window
	输出 input/output/total token baseline、variant 与 delta tile；partial usage 显示 unknown，且不生成
	账单 cost fact。同库 CLI E2E 已继续执行到 immutable Dashboard snapshot，验证 committed formal
	receipt 经 EvaluationRun/ExperimentRun 到 `experiment.total_tokens.delta` 的完整链路。正式 analytics
	也已识别 formal `ReviewHypothesisSet + GovernedCandidateSet + GovernedReviewReport`：重新校验三段
	artifact lineage、保留全部 Candidate、只把 confirmed Candidate 计为 verified Finding，并把
	`queued_for_human` 保持为 human queue/publication not reached；projection binding 冻结三份 exact digest。
	跨 revision `FindingLineage` 已作为独立 content-addressed artifact 和 append-only request ledger 实现：
	只接受同 tenant/workspace/repository、同 target mode、不同 target digest 的 committed formal closure，
	冻结 run/spec/target 及 Candidate/Verification/Calibration/Suppression/Report digest；exact fingerprint 与
	保守 stable family 可生成 continued/split/merged，多对多歧义失败关闭为带 reason 的
	resolved/introduced。local CLI/API 新 build 已固定使用 Git-aware policy：同一 canonical common-dir、exact
	commit OID、strict ancestor/merge-base proof 和 5000 BPS rename mappings 由隔离 object view 生成并共同摘要；
	只有显式 rename mapping 可产生 `git_rename_family`。reverse order、不同 repository、mutable ref、threshold
	drift 和 evidence tamper 均失败关闭；旧 caller-order policy 只保留读取兼容。lineage relation 已作为第九张
	独立 analytics fact table 进入 snapshot/JSON/CSV/Parquet，source coverage 单独报告；它只保存关系证据，不把
	resolved/introduced 改写成 Outcome 或 label。Dashboard 进一步按 relation type、match method 与 exact policy
	revision 输出 relation/baseline-ref/variant-ref count，并固定附带 relationship-only warning，因而可用于后续真实
	corpus 的 matcher stability/drift 分析但不会伪造 fixed/escaped/precision 指标。reviewed bug-fix evaluation candidate 在 local composition 中还要求
	exact Git-aware lineage 把 defect run/Finding 解析为 fix run 的 resolved relation，并与独立 fixed Outcome、change/CI
	evidence 一起入 provenance；lineage 本身不能单独生成 gold label。
	control-plane Finding resolver 也已识别 formal GovernedReviewReport，`finding show` 以互斥的
	`governed_finding/governed_decisions` 返回原生契约；Feedback/Outcome CLI 可在 exact run/finding
	校验后对 human queue 记录平台 UI/API 人工事实。analytics 仅在真实 Feedback/Outcome 已存在时投影
	未发布 human queue，且 Dashboard 的 published→accepted/fixed 指标仍只消费 published Finding。
	后续人工 FindingDecision 现由独立 append-only ledger 保存，control-plane 从 committed legacy/formal
	源报告解析 exact root，按 sequence/prior 连续追加；严格 request/mutation/decision Schema、角色授权、
	幂等冲突、重启恢复和损坏拒绝已有 focused test，同库 CLI E2E 已验证 `decision record -> finding show`。
	`publish` 要求 publication approver 且 replay 永久拒绝。最新 publish Decision 已接入只读
	`publication request build`：builder 从 frozen spec/snapshot/config 与 source-typed Finding 派生
	provider-neutral request，formal report 不再塞入 legacy finding_set 字段；publication service 也把
	control-plane `RequestAuthorizer` 设为必选并在 dispatch 前二次校验。独立 append-only
	PublicationGrant ledger 已实现最长 24h exact binding、strict restore、幂等签发与单次 reservation；
	同库 CLI E2E 验证默认 deny 的 local review 经人工 Decision/Grant 可构建唯一 request，而不扩大 Agent
	权限。GitHub adapter 已实现 PR open/base/head、changed-file anchor、mandatory credential/permission
	ports、inline/summary create、隐藏幂等 marker 和 lookup-only reconcile；HTTP fault E2E 已证明 create
	响应丢失后不重复 POST 并收敛同一 comment。当前没有生产 credential/IAM wiring、生产 dispatch API
	或 live GitHub canary，所以这些证据不能写成生产发布。显式 single-user-local dispatch/reconcile CLI
	已接通：只接受固定 env credential、固定 local permission revision 和显式 profile flag；同库 E2E
	验证 dispatch unknown 后 reconcile 只 lookup，token 全 store 扫描无泄漏。Hailix 当前源码尚无公开的
	代码平台 credential/RepositoryAccessGrant API，不能复用其仅面向模型 Worker 的 internal runtimesecret。
- stage/shard fan-out、多进程 worker 接管、进程崩溃后的 checkpoint resume 和
  Hailix runtime adapter。当前 CLI 已在同一 local store 接入 run-level durable
  workload admission/lease/heartbeat/fencing/terminal callback；它只协调一个完整
  run 的同步执行，不能冒充上述恢复与分布式能力。

## 已知风险

- JSON Schema/Go 目前通过选定正反例做 parity gate，尚未由同一模型生成；新增约束
  必须同步扩充 corpus。
- `v1alpha1` digest 基于 Go struct 的 JSON 编码；进入跨语言稳定版本前需要冻结
  canonical JSON 规范。
- 当前 marker + 单条 Go AST rule 只能证明平台语义、detector composition 和证据闭包可
  工作，不能用于推断完整代码评审 precision/recall 或收益。
- Pi CLI 与新 Go shadow bridge 已完成一个已知缺陷和一个 clean fixture 的 DeepSeek
  Anthropic-compatible 真实 smoke，并验证受限 context gap 会诚实返回 partial。真实
  timeout 路径已成功导入 partial 结果；无效 child-only token 的 auth canary
  `agent-review-execution-c9a8786a9d073353e191cd7f` 以非零 CLI 状态收敛为可查询的 failed
  manifest，保存 1 个 `provider_error` receipt、1 个 `dependency_failed` receipt，并对没有
  exact prompt transcript 的 synthetic canceled task 显式记录
  `partial/task_evidence_unavailable`。2026-08-27 opt-in DeepSeek canary 已在 candidate-verification revision 1
  fsync 后物理终止 generation 1 lease owner，证明 lease expiry 后 generation 2 复用 context/review 和已完成
  verifier、提交 succeeded ReviewRun，并拒绝旧代 callback。2026-08-27 另用独立 synthetic development
  repository 完成了一次 600 秒预算的 selection formal run：DeepSeek 经 6/6 review task 和独立 verifier
  确认 tenant-less permission cache 的跨租户授权缺陷，最终 `queued_for_human`，同幂等键在 host admission
  修复后复用已持久化 callback、未重复调用 provider。该运行实际暴露并修复了两项跨边界问题：terminal
  schema 拒绝后重试必须按“一次成功 + 可审计失败次数”闭合；worker/host 的 frozen excerpt 必须共同执行
  CRLF 归一化和 outer trim。`repository_search@v2` 的 index-only replay 也在同一 source 上执行成功，但
  4 个 raw candidates 全因模型移除了多行源码的公共缩进而被 deterministic normalization 拒绝，结果从
  1 个 confirmed Finding 退化为 0；这是 development 负向证据，不是索引收益结论，也未使用已曝光 dev
  case 冒充 blind comparison。随后已将 review/verification terminal evidence 的 exact path/range/excerpt
  校验前移：非法提交返回工具错误并保留 task evidence/receipt failure，模型可在同一 task 修正；Go host
  仍在 normalization 与 terminal admission 独立重验。新的未曝光 synthetic dev case
  `f3e95eee665dbd633721c54536162c4c6463c7be` 上，target-only formal run
  `formal-c92db7ebd6efed8d0ed07eb4` 完成 6/6 review task 但为 0 Candidate/0 Finding；同 source 的
  `repository_search@v2` index-only replay `formal-variant-ac475b1799e799f1b9d33be1` 产出 2 个 raw
  Candidate、2 个 confirmed/queued Finding。correctness task 的 terminal submit 为 3 次调用/2 次失败，
  error-contract 为 4/3，两个 verifier 中一个为 2/1；越权引用 `test/approval.test.ts` 等 context 文件和
  非 canonical excerpt 均被拒绝，最终 evidence 只引用冻结目标 `src/approval.ts`。这证明可纠错链路和
  index 上下文能在该 case 暴露缺陷，但 2 个 Finding 是同一状态迁移问题跨 dimension 的重复，不能把它
  计为 2 个 unique defect 或据此声称总体索引收益。仍缺足量预注册 oracle 的 dev/holdout 重复运行、跨
  dimension 语义去重、质量、稳定性和成本验收。当前按 `--no-renames`
  捕获 diff，纯 rename 可能表现为 delete + add，不能把 rename-only 内容当作新缺陷。
- 为修复上述 duplicate inflation，Pi normalization 新增可版本化、保守的 `semantic_duplicate`：只允许同
  group/path、target-side、重叠 anchor 的不同 fingerprint Candidate 合并；exact duplicate 继续使用
  `duplicate_fingerprint`。第一版只接受固定 ASCII title-token Jaccard 至少 3/4。raw claim 与 reason
  均保留，canonical winner 只消耗一次 verifier。Go host 独立重算 relation，并拒绝 exact-as-semantic、
  不同 group/path/anchor 或低相似度的 worker 声明。Pi workflow 已覆盖真实 dev2 标题形态的跨 dimension
  合并，以及同 anchor 不同缺陷不得合并；Go formal mapper 已覆盖合法映射和伪造 semantic decision 拒绝。
  当时全量门禁为 74/74 Pi tests 与全部 Go/Schema/docs/build/cross-language smoke 通过。已曝光 dev2 case 的
  physical regression 首先因新 bootstrap 遗漏 600 秒预算，在 175.7 秒以可重试 `deadline_exceeded` 失败；
  修正为 600 秒后，target-only `formal-a2a45df5841b9b742d20e380` 成功并产出 1 个 confirmed Finding，
  与同 case 此前两次 target-only 0 Finding 不一致，证明单样本 provider 运行存在可见波动。随后 index-only
	`formal-variant-9d4f8230bf5bd3555ea56e34` 在 334.1 秒以非重试 `pi_no_review_completed` 失败，没有形成
	Hypothesis/Report，因此没有物理证明 semantic merge。失败源于 formal coverage gate，而非已观测到的
	semantic host-recomputation 拒绝。现在 failed/canceled StageExecutionResult 已允许严格配对的
	`agent_task_evidence + agent_execution_receipts`，mapper 在 coverage error 前保留已验证 diagnostics，
	host 将 governed/local projection 只写入 non-succeeded terminal gate；失败 ReviewRun 仍不引用这些
	artifact，也不生成 Hypothesis/Finding/Report。`formal show` 只显示
	`terminal_failure_diagnostics` 的 task role/status/failure-code 聚合，exact `formal evidence read` 仍需
	purpose-bound audited sensitive access，并同时返回 receipts；local evidence 缺失、单边 binding、终态
	篡改和重启恢复均失败关闭。诊断发布失败会收敛为
	`pi_failure_diagnostic_persistence_failed` 而不是静默丢失。命令层 E2E 已证明 coverage failure 可查询、
	可审计读取且不会污染失败 ReviewRun。当前仍未在足量
	oracle corpus 上证明误合并率或总体 precision。
- 新实现第一次真实 replay `formal-variant-c51ee6f241ab8550e9730471` 复用了同一 600 秒 source/input/index
  closure，运行 416.997 秒后以 `pi_evidence_mapping_rejected` 失败。immutable group checkpoint 证明 5 个
  review Candidate 和 5 个独立 verifier 均已完成；具体拒绝是 error-contract 标题包含
  `approved->rejected` 时，TypeScript `JSON.stringify` 保留 `>`，Go `encoding/json` 默认写成 `\u003e`，
  使同一 fingerprint 的 host recomputation 漂移。该失败也证明第一版 diagnostics 仍晚于 business candidate
  mapping：终态没有诊断 binding。现在 mapper 已先验证 report/receipt/task evidence 并形成 diagnostic
  artifacts，再执行 Candidate/Hypothesis mapping；业务 mapping 任意失败都保留 terminal-only diagnostics。
  Go fingerprint 编码已关闭 HTML escaping，并加入同时含 `< > &` 的 JSON.stringify 固定向量。修复后的
  DeepSeek replay `formal-variant-43fce30e598150022b72dd46` 已在同一 frozen source/index closure 上物理成功：
  321.929 秒完成 1 个 context、6 个 review 和 4 个 verifier task，11/11 task 成功，formal terminal、task
  evidence/receipt、Hypothesis/Report 全部闭合。这证明 fingerprint wire 修复有效，也给出新的负向质量证据：
  4 个 confirmed Finding 的 title/category 不同，但都指向 `applyDecision` 同一无 guard 状态覆写根因，第一版
  title-only 3/4 Jaccard 全部漏并。
- `semantic_duplicate` 第二版增加宿主可重算的受控同义归一和 relaxed path：除原结构约束外，必须共享相同
  camelCase/underscore 代码标识符、至少一段相交 source evidence、至少 4 个 canonical title token，且交集覆盖
  较短 title token 集合的一半；原 3/4 Jaccard 路径继续保留。该变化将 Pi execution/checkpoint
  `workflowRevision` 从 `argus-pi-review-workflow-v0` 升为 `v1`，旧 checkpoint 不会在新 normalize 语义下
  静默复用。Pi 新增上述 4 条真实标题归并为 1 个 Candidate/1 次 verifier 的回归测试，总计 75/75；同 anchor
  不同缺陷和 Go host 伪造 relation 仍拒绝。v1 runtime 先以全新 config lifecycle 建立 target-only baseline
  `formal-d6e36e980a27a6711c471f1b`：58.370 秒、7/7 task、0 Candidate；其 index replay
  `formal-variant-1552355cc7c7ab897ab8eb82` 在 412.145 秒完成，5 条 raw 中 2 条成功 merge，但仍保留 3 个
  confirmed Finding/3 次 verifier。三条分别以 correctness、error-contract、concurrency-data 描述同一
  `applyDecision` 无 guard 状态覆写，说明 title-only heuristic 仍不足。
- 基于上述 immutable physical output，v2 relation 在共享代码标识符和相交源码 evidence 已成立后，允许用
  root-cause `description` 的固定 canonical token 集合作为第二信号：双方至少 12 token、交集至少 12，且覆盖
  较短描述的 55%。真实三条描述的 measured overlap 为 56%–64%；同 anchor 不同缺陷 fixture 现在同时使用
  不同 title 和不同 description，避免自相矛盾测试。Pi/Go host 双侧重算与伪造拒绝测试通过，workflow/checkpoint
  revision 升为 `argus-pi-review-workflow-v2`。新的 target-only baseline
  `formal-082dea8fec412f40bbe0fc07` 在 132.607 秒完成 7/7 task、0 Candidate；同 snapshot 的 index replay
  `formal-variant-36318c6ff1916058ea1d61fc` 在 314.084 秒完成 8/8 task。后者保留 4 条 raw lineage，3 条以
  `semantic_duplicate` 指向同一 canonical occurrence，只执行 1 个 verifier，并生成 1 个 confirmed/
  queued Finding。这证明 v2 在该 exposed synthetic case 上物理闭合；它仍是 development heuristic，尚未在
  足量 oracle corpus 上证明误合并率或总体 unique-defect precision/recall。
- 新增 `agent-review formal normalize-preview`：从 committed succeeded formal ReviewRun 的 whole-closure
  读取 exact `AgentReviewRawCandidateCollection`，用当前 v2 policy 重新计算稳定 winner/duplicate cluster，
  只输出 raw ID、dimension、prior/preview decision、cluster 和整数计数。它固定
  `diagnostic_only/preview_only`，不回显 claim/excerpt，不调用 provider，不写 ReviewRun/Hypothesis/Finding/
  Decision/Evaluation，也不会把 prior invalid/budget-excluded claim 重新提升。对上述 v1 physical run 的真实
  预览得到 5 eligible raw -> 1 retained + 4 semantic merge，五个 dimension 同属一个 cluster；这是离线
  development evidence，不是 v2 physical execution 或 corpus quality 证明。
- 新增 `agent-review formal normalize-compare` 与
  `NormalizationPolicyComparison` schema/example：批量绑定 committed ReviewRun/raw refs，比较 recorded
  decision 与当前 policy，并持久化 content-addressed `diagnostic_only/comparison_only` artifact。真实 dev2
  v1+v2 comparison `cc0beabc...` 覆盖 2 case/9 raw：v1 由 3 retained 预览为 1（2 个 decision delta），v2
  recorded 与 preview 均为 1 retained/3 semantic merge（0 delta）。该 artifact 证明 policy 重算一致性，
  不含 oracle，不进入 Evaluation/Promotion。
- 新增独立 `NormalizationOracle` 与 `NormalizationQualityRun`：oracle 绑定 governed CorpusSnapshot、case/label
  revision、committed ReviewRun/raw ref、完整 eligible ID partition、2 reviewer + 独立 adjudicator、外部 evidence
  和 policy exposure；quality run 对当前 exact policy 重算 cluster，输出 pairwise TP/FP/FN/TN、整数 PPM
  precision/recall/false-merge、unique-count delta 和 exact partition。seal 只写 content-addressed artifact；
  新增外部 Ed25519 `NormalizationOracleAttestation`、复用 scoped governance trust-key registry，并通过
  append-only register/replace/revoke ledger、expected current revision/event CAS、operator/reviewer/adjudicator/
  trust-admin 职责分离和 restore-time 全事件重验形成活动授权。Quality request/result 现在绑定 exact oracle
  registration、attestation 与 trust-key registration event；oracle/key revoke、case label/governance 漂移或
  stale binding 后 run/show 失败关闭。focused CLI E2E 用一个 governed dev case + fake provider 正式 run 验证
  seal→sign/register→run→show→revoke-deny；domain test 覆盖篡改签名、stale CAS、幂等和 restart。这证明本地
  治理机制，不是足量真实 corpus 质量结论；actor 仍是本地声明身份，尚非 Hailix/IAM 平台认证。
- 新增 normalization policy 的 managed promotion bridge：不可变 policy 分别冻结 test/holdout 的样本覆盖、
  pairwise precision/recall、false-merge 和 exact-partition 整数 PPM 阈值；prepare 注册 workflow managed
  variant 并自动写 schema gate，targeted regression/fixed holdout 通过共享 exact verifier 重新授权和重算
  整个 oracle/raw closure。split 混用、policy exposure、非独立 evidence、revoke/drift 失败关闭；覆盖不足或
  metric unavailable 为 inconclusive，阈值未达为 fail。CLI、本地 bearer-authenticated API 和 embedded UI
  已接入，decision 与 quality/policy refs 写入 append-only ledger。policy 还冻结最小 shadow/canary run 数和
  canary percentage；operational gate 会重验 exact variant bundle、真实 lifecycle canary assignment、运行完成
  时间、安全事件、approver actor 与 rollback frame。当前没有足量真实 corpus，也没有执行真实 shadow/canary，
  因而示例阈值与机制级测试都不是生产质量结论。
- 已补齐 normalization execution selector：ConfigRevision/ConfigBundle 的 `AgentReviewPolicy.normalization`、
  canonical AgentStagePlan、stdio AgentReviewPlan、Pi ExecutionSnapshot/checkpoint 与 Go host recomputation 均绑定
  `candidate-normalization@v0|v1|v2 + exact worker SHA-256`；worker admission 拒绝未知 revision 或 digest 漂移，
  formal bootstrap/CLI 可选 v0/v1/v2，offline evaluator 也按 exact revision 重算。v0/v1/v2 的差异由 Go/TS
  focused tests 固定，跨 revision checkpoint 不可复用。promotion prepare 现从 exact active baseline
  ReviewRun/ConfigResolutionReceipt 派生并 validate 只替换 selector 的 ConfigRevision，binding 冻结 baseline/
  variant bundle 与两版 config revision/digest。服务级 E2E 已覆盖 shadow → 10% canary → authorization →
  rollback monitor → 100% activation → 双 ledger rollback；这只证明本地状态机和证据重验，不表示真实流量已运行。
- normalization managed promotion 现进一步绑定候选/回滚的 exact selector：两者必须是受支持的
  `candidate-normalization@v0|v1|v2`、使用同一 worker SHA-256，且 workflow policy revision 必须由 selector
  唯一推导；shared binding 会保存 implementation/rollback revision 与 digest，generic endpoint 不能绕过。
  通用 config lifecycle 新增 `rollout_advanced`：percentage 只能单调扩大且 seed 不可漂移，100% promotion 保留
  原 publish rollback frame；CLI、Local API 和拒绝路径测试已覆盖 10→40→100 与回滚。
- worker package identity、shadow/formal task evidence 与 formal receipt 已由本地主机内容寻址和重验，但仍没有
  provider/Hailix attestation；assistant/provider transcript、credential 使用证明和 authoritative
  billing cost 也未闭合；当前 committed token 仅是 worker self-report，不能据此声称 provider billing
  truth 或完整离线重放。
- host-failure observation 只保留固定 stage/reason taxonomy；它不会保存原始 provider
  payload，也不能替代 Hailix Trace、provider attestation 或完整根因诊断。
- Hailix/Eino-Agent 都有接入缺口；当前 local binding/runtime identity 不是生产
  platform attestation。
