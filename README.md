# Argus

[![CI](https://github.com/abietic/argus/actions/workflows/ci.yml/badge.svg)](https://github.com/abietic/argus/actions/workflows/ci.yml)

Argus 是一个面向真实软件仓库的 AI Code Review 产品，而不是一组临时
subagent prompt。它把变更评审、圈选代码评审和仓库/目录扫描统一为可配置、
可追踪、可重放、可评测、可归因的平台能力。

当前 Go 平台链路已经在 M0/M1 的产品边界和本地可追踪闭环上接通 exact Git diff、
圈选行/overlay 和仓库/目录 scope。三种目标使用同一套评审、重放、比较和历史实现；
其 formal detector 当前是封闭、版本化的 deterministic registry：包含用于平台语义
验收的 marker baseline 和一条基于冻结源码的 Go AST context-cancel rule。它仍不代表
完整生产评审质量。

`runtime/pi-review` 提供真实 Agentic Code Review runtime：直接嵌入 Pi Agent SDK，
对 working changes、exact commit diff 或显式文件执行分组上下文收集、多 Skill 并行
评审和独立候选验证。默认 pack 已按 correctness、concurrency/data、error/outcome、
resource lifecycle、security/contract、transaction/durable state 拆成六个 versioned
dimension；最多 16 个 review dimensions，额外仓库/业务 Skill 仍需显式治理配置。M1.3.3
已在严格 frozen-input stdio worker 之上，由 Go host 自动
构造计划、启动 worker、复算目标与证据、映射 Hypothesis/Receipt、提交 shadow-only
结果。Go host 另以 immutable intent、v1alpha2 completion、bounded host-failure
observation、strict task-evidence binding，以及 v1alpha3 `ExecutionAttempt` 区分
`succeeded`、`failed`、`canceled` 与 `unknown_outcome`；独立 `agent_execution.*`
诊断快照覆盖这些 host-observed 状态并可查询、导出 JSON/CSV。冻结 diff 不允许 old-side
anchor；deletion-only 缺陷只能锚定删除相邻的存活 target context，replacement hunk 不获得
该例外。2026-08-26 的本地 DeepSeek canary 已分别闭合 confirmed known-defect、
zero-candidate clean、auth-failure import，以及完整组 checkpoint 后物理 SIGKILL、generation 2
复用 context/review 并重跑 verifier 的 succeeded ReviewRun。2026-08-27 又完成 candidate-verification
checkpoint、单调 revision、失败 verifier 重跑与 tamper rejection，并以真实 DeepSeek 在 revision 1 fsync 后
物理终止 generation 1 lease owner；generation 2 复用已完成 verifier、正式成功并拒绝旧代 callback。
这些仍是本地、single-user、direct-provider、
non-attested、shadow-only 证据，不代表 Hailix/ACP production adapter 或真实 corpus 质量验收。

formal Agent Review 另建立了**规划与本地执行边界**：版本化 `AgentReviewPolicy`、由配置
lifecycle repository 同次解析返回的 `ConfigResolutionReceipt`、workflow authority
ceiling，以及纯函数编译生成的 `AgentStagePlan`。Plan 精确绑定
ExecutionSnapshot/ConfigBundle/receipt/workflow/spec/input、agent/provider/model/runtime/
prompt/API protocol、按配置顺序排列的 Skill/Knowledge/context provider、build identity、
权限和预算；输出固定为 `ReviewHypothesisSet`，`disposition=hypothesis_only`、
	`side_effects=deny`。执行链把 Plan 及其冻结来源落入 append-only local ledger，并建立
	`StageExecutionRequest -> dispatch intent/claim -> execution binding -> authenticated callback
	receipt -> result/cancel terminal gate -> ReviewHypothesisSet evidence` 的严格闭包；exact retry、
	unknown outcome、旧 generation 回收和取消竞态都有独立事实。dispatch claim 与 terminal
	admission 还通过同一 append-only generation CAS 串行化，后代 dispatch 和旧 completion 的
	并发只允许一个赢家。`agent-review formal` 已把
	governed component/config bootstrap、本地 Pi PlatformPort、调度、执行、终态准入与查询接成
	CLI；成功结果先生成独立 content-addressed CandidateSet，再只把独立 verifier 确认的项提升为 Finding，并固定生成
	`queued_for_human` Decision，不授予自动发布权限。formal attempt、binding、evidence 与治理报告
	随后提交为统一的 terminal `ReviewRun`，因此通用 `history/show` 可直接消费，精确重试不会
	重复执行 provider。本地 runtime component 进一步冻结 Node executable、全部 `dist/*.js`、
	package metadata/lockfile 和 lockfile production dependency 实体文件；dispatch 前和进程启动前
	都会重新散列，漂移时不调用 provider。当前已实现 Hailix anti-corruption adapter 的
	capability/ensure/lookup/cancel/result/callback consumer contract、正式 `hailix-http` CLI backend
	composition 与 unknown-outcome recovery E2E；请求凭据固定在每次调用时从环境解析，不进入持久化命令。
	但 Hailix 尚无匹配的 public platform-execution server，因此仍没有 production Hailix wiring、
	远端身份/credential broker 或 runtime attestation；未绑定的 upstream、replay input 和多
	attempt retry继续失败关闭。

## 三个项目的职责

| 项目 | 核心职责 |
|---|---|
| Argus | Code Review 领域控制面：目标、规则、工作流、Finding、决策、反馈、评测和收益 |
| Hailix | 通用 Agent 执行平台：身份/Workspace、Task/Worker Runtime、ACP、Trace/Artifact 和模型执行证据 |
| Eino-Agent | 可被 ACP 控制的 Coding Agent Runtime：仓库理解、工具调用、subagent 和执行能力 |

Argus 不复制 Hailix 的 Worker Runtime，也不把 Eino-Agent 的 session 当成业务
事实源。Argus 通过版本化契约绑定两者，并保留独立的领域状态和证据链。

本地 Pi shadow 链路会把 normalize 前的有界 raw candidate（包括
`rejected_invalid/excluded_budget`）保存为独立内容寻址 Artifact，并由 manifest、查询和
crash reconciliation 精确绑定；它仍是 `worker_self_report/shadow_only`，不会自动变成
Finding、评测真值或可发布评论。

## 核心闭环

```text
ReviewSpec
  -> TargetSnapshot + immutable ExecutionSnapshot
  -> versioned ReviewWorkflow
  -> PlatformExecutionBinding + RunEvidence
  -> CandidateFinding (全量保留)
  -> Evidence / Verification / Adjudication
  -> PublicationDecision
  -> Feedback / Outcome
  -> EvaluationCase / Replay / Experiment
  -> guarded promotion
```

## 快速开始

```bash
make pi-install
make verify
make build
./build/argus version
./build/argus validate review-spec examples/review-spec.diff.json
./build/argus review --help

# deterministic workflow-only replay 需要 source-derived ConfigBundle 将 workflow ref
# 指向同一 workflow_id 的新 revision，并同时提供该 exact WorkflowDefinition。
# 只允许改变 scheduler 实际消费的 stage budget/retry policy；stage graph、executor、
# contracts、authority、failure/side-effect/replay semantics 保持冻结。
./build/argus replay \
  --run <committed-succeeded-local-run> \
  --from <earliest-affected-stage> \
  --change workflow \
  --variant-config-bundle /absolute/source-derived-variant-config-bundle.json \
  --variant-workflow /absolute/variant-workflow-definition.json \
  --store /absolute/argus-store \
  --json

# deterministic index-only replay 从 source-derived ConfigBundle 修改
# execution.context_providers，并从 materialize_target 重跑。它会在同一 exact
# repository/commit/target paths 上重新执行 version-pinned provider，冻结新的
# ContextRef/Gap、receipt、ReviewInput 与 MaterializedTarget；其他 execution policy、
# caller-supplied context 和所有非 context target bytes 保持不变。
./build/argus replay \
  --run <committed-succeeded-local-run> \
  --from materialize_target \
  --change index \
  --variant-config-bundle /absolute/source-derived-index-variant.json \
  --store /absolute/argus-store \
  --json

# 可选：在默认本地配置下，并行采集 repository lexical matches、Go AST/go/types/call path、
# module/import dependency 与 compile-only facts。四类采集都只读取 exact Git object，
# 不读取漂移的 working tree；repository_search 在读取 blob 前排除凭据敏感路径。
# go_ast revision 2 额外保留带 line/column call site 的最多三跳 exact local-module
# upstream/downstream path；name match、external 和 unresolved call 不会升级成精确调用链。
# go_compile 使用 go test -c 编译测试源码但不执行 Test/TestMain/init；模块网络、cgo、
# ambient workspace/toolchain fallback 均关闭，不能替代 Hailix sandbox 中的真实 test run。
./build/argus review \
  --repo /absolute/repository \
  --mode diff --base main --head HEAD \
  --context-provider repository_search \
  --context-provider go_ast \
  --context-provider go_dependencies \
  --context-provider go_compile \
  --store /absolute/argus-store

# 可选：把 CodeGraph/LSP/依赖分析的既有输出作为冻结上下文输入。
# 文件会以 content-addressed Artifact 落盘，后续 formal Pi 执行校验 exact bytes；
# 它只能提供证据，不能扩大可评论行范围或提升工具权限。
./build/argus review \
  --repo /absolute/repository \
  --mode diff --base main --head HEAD \
  --context-file codegraph@Handler=/absolute/context/call-graph.json \
  --context-file lsp@Handler=/absolute/context/type-facts.json \
  --store /absolute/argus-store

# 只生成 Pi 评审计划，不调用模型
make pi-review ARGS='changes --repo /absolute/repository --plan'

# 对已经完成的 formal review/replay 做受治理的 EvaluationRun 评分。
# 需要先为同一 evaluation_run_id 和每个 case 记录 exposure；record 本身只读已提交
# ReviewRun，不会触发模型调用、评论或 Apply。
./build/argus evaluation run record \
  --store /absolute/argus-store \
  --input /absolute/evaluation-run-request.json \
  --mutation /absolute/evaluation-mutation.json --json

# 在模型看到语料前，先把 active Case 语义、治理/标签 revision、committed source ReviewRun、
# TargetSnapshot 与 source ExecutionSnapshot digest 冻结成 content-addressed CorpusSnapshot。
./build/argus evaluation corpus snapshot build \
  --store /absolute/argus-store \
  --input /absolute/corpus-snapshot-request.json \
  --access /absolute/evaluation-access.json --json

# 首次在 governed active corpus 上执行 formal Pi，并自动生成 EvaluationRun。
# request 必须引用上一步的 exact CorpusSnapshot；每个 case 仍显式绑定 label revision、source
# ReviewRun 和 exposure。runner 会在任何模型调用前拒绝 corpus/case/source execution 漂移。
# `--` 后复用 formal run flags，但不允许覆盖
# store/source/idempotency。API key 只从当前环境读取，不进入 durable executor template。
./build/argus evaluation corpus run \
  --store /absolute/argus-store \
  --input /absolute/formal-corpus-batch-request.json \
  --mutation /absolute/evaluation-mutation.json --json -- \
  --config-state-dir /absolute/argus-config-state \
  --node /absolute/node --worker-script /absolute/runtime/pi-review/dist/worker.js \
  --provider-profile deepseek-anthropic-env --model "$ANTHROPIC_MODEL" \
  --input-micros-per-million 1 --output-micros-per-million 1 \
  --max-bytes-per-input-token 4

# exact retry 可再次使用原 request/mutation/formal flags；也可只凭 batch ID 与授权恢复。
./build/argus evaluation corpus resume \
  --store /absolute/argus-store --batch <formal-corpus-batch-id> \
  --access /absolute/evaluation-access.json --json

# 持久化批量 prompt variant 实验。`--` 后复用 formal replay 的 variant 与 execution-backend flags；
# batch 自己拥有 store/source/idempotency，不允许模板覆盖。hailix-http 只冻结 endpoint/trust identity，
# ARGUS_HAILIX_BEARER_TOKEN / ARGUS_HAILIX_CREDENTIAL_REVISION 始终按请求从环境读取。
./build/argus evaluation batch run \
  --store /absolute/argus-store \
  --input /absolute/experiment-batch-request.json \
  --mutation /absolute/evaluation-mutation.json --json -- \
  --node /absolute/node --worker-script /absolute/runtime/pi-review/dist/worker.js \
  --provider-profile deepseek-anthropic-env --model "$ANTHROPIC_MODEL" \
  --input-micros-per-million 1 --output-micros-per-million 1 \
  --max-bytes-per-input-token 4 \
  --change prompt --prompt-bundle /absolute/prompt-bundle.json \
  --at 2026-08-25T10:00:00Z

# 批次 intent 会绑定 credential-free、content-addressed 的 exact Pi/transport 执行模板。
# 进程退出后只需 batch ID 和授权上下文即可恢复，不应再次传模型、prompt 或 skill 参数。
./build/argus evaluation batch resume \
  --store /absolute/argus-store --batch <experiment-batch-id> \
  --access /absolute/evaluation-access.json --json

./build/argus evaluation repeatability batch resume \
  --store /absolute/argus-store --batch <repeatability-batch-id> \
  --access /absolute/evaluation-access.json --json

# 在同一 store 重建不可变产品看板；formal governed funnel 与已提交实验 token delta
# 都从 authoritative local ledgers 重算，不需要手工拼 analytics facts。
./build/argus dashboard rebuild \
  --store /absolute/argus-store --snapshot <dashboard-snapshot-id> \
  --tenant local --organization local --repository <repository-id> \
  --start 2026-08-25T00:00:00Z --end 2026-08-26T00:00:00Z \
  --built-at 2026-08-26T00:01:00Z --json

# 调用 Anthropic；凭据只从环境读取
export ANTHROPIC_API_KEY='...'
make pi-review ARGS='changes --repo /absolute/repository'

# Go host -> frozen-input Pi worker -> shadow evidence
export ANTHROPIC_BASE_URL='https://api.deepseek.com/anthropic'
export ANTHROPIC_MODEL='your-deepseek-model-id'

# 显式花费 provider 预算的本地 physical canary；默认 make verify 不会运行。
# 它在完整 checkpoint fsync 后同时杀掉 ReviewJob Go worker 与 Node Pi 子进程，
# 等待 lease 过期后要求 generation 2 复用并成功闭合。
ARGUS_LIVE_DEEPSEEK_CHECKPOINT_RESTART=1 \
  go test ./cmd/argus \
  -run '^TestLiveFormalReviewJobRecoversDeepSeekCheckpointAfterPhysicalKill$' \
  -count=1 -v

./build/argus agent-review run \
  --store /absolute/argus-store \
  --source-run <committed-succeeded-review-run> \
  --idempotency-key <stable-execution-key> \
  --node /absolute/regular/non-symlink/node \
  --worker-script "$PWD/runtime/pi-review/dist/worker.js" \
  --provider-profile deepseek-anthropic-env \
  --model "$ANTHROPIC_MODEL" \
  --allow-partial

# 查询成功、失败、取消和 unknown-outcome execution；时间窗口为 [start, end)
./build/argus agent-review execution list \
  --store /absolute/argus-store \
  --start 2026-08-23T00:00:00Z \
  --end 2026-08-24T00:00:00Z
./build/argus agent-review execution show \
  --store /absolute/argus-store \
  --execution <agent-review-execution-id>

# 只在 exact committed shadow result 已存在时收敛 crash window；不会重跑 provider
./build/argus agent-review execution reconcile \
  --store /absolute/argus-store \
  --execution <agent-review-execution-id>

# 通用 run/show JSON 只输出敏感证据 ref 与治理摘要；显式读取必须先写审计回执。
./build/argus agent-review evidence read \
  --store /absolute/argus-store \
  --manifest-id <shadow-manifest-id> \
  --request-id <unique-access-id> \
  --actor <human-or-service-id> \
  --purpose local_debug \
  --at 2026-08-25T01:02:03Z \
  --acknowledge-sensitive-output \
  --json

# 本地保留策略为 until_explicit_revocation；tombstone 后正文不可再读取，历史仍可查询。
./build/argus agent-review evidence revoke \
  --store /absolute/argus-store \
  --manifest-id <shadow-manifest-id> \
  --idempotency-key <stable-revocation-key> \
  --actor <retention-operator-id> \
  --reason 'local retention period ended' \
  --at 2026-09-25T01:02:03Z

# 通用 ReviewRun Artifact 生命周期使用独立的 ref/change JSON；不会物理删除共享内容寻址 bytes。
./build/argus artifact integrity inspect \
  --store /absolute/argus-store \
  --ref /absolute/artifact-ref.json \
  --json
./build/argus artifact integrity quarantine \
  --store /absolute/argus-store \
  --input /absolute/artifact-integrity-change.json
# 修复底层 bytes 后，release 会先重新验证 SHA-256/size；tombstone 不可逆。
./build/argus artifact integrity release --store /absolute/argus-store --input /absolute/release.json
./build/argus artifact integrity tombstone --store /absolute/argus-store --input /absolute/tombstone.json

# 对已提交 shadow executions 构建不可变诊断快照
./build/argus agent-review analytics rebuild \
  --store /absolute/argus-store \
  --snapshot <snapshot-id> \
  --start 2026-08-23T00:00:00Z \
  --end 2026-08-24T00:00:00Z \
  --built-at 2026-08-24T00:00:00Z

# 正式本地 Pi 链路：先对一个已提交 source ReviewRun 发布 exact components/config。
# --at 是幂等发布事实的一部分；相同 bootstrap key 重试时必须保持不变。
# 可重复传 --knowledge /absolute/repository-invariants.md；bootstrap 与 run 必须使用同一组 exact 文件。
# provider model 名称按 opaque wire value 原样保存；内部 registry 使用安全的派生 component ID。
# runtime/worker-owned component revision 绑定 build digest；相同 exact component 会直接复用。
NODE_PATH="$(command -v node)"
WORKER_PATH="$PWD/runtime/pi-review/dist/worker.js"

# 推荐的首次本地 formal Pi 评审入口：`--` 前是受治理的 formal/runtime 参数，
# `--` 后原样使用 review 的 diff/selection/scope 目标参数。它会依次提交 source ReviewRun、
# exact component/config bootstrap 和 formal ReviewRun，并输出 source_run_id/formal_run_id。
./build/argus agent-review quick \
  --store /absolute/argus-store \
  --config-state-dir /absolute/argus-config-state \
  --idempotency-key <stable-quick-key> \
  --at 2026-09-04T01:02:03Z \
  --node "$NODE_PATH" \
  --worker-script "$WORKER_PATH" \
  --provider-profile deepseek-anthropic-env \
  --model "$ANTHROPIC_MODEL" \
  --knowledge /absolute/repository-invariants.md \
  --input-micros-per-million <governed-input-ceiling> \
  --output-micros-per-million <governed-output-ceiling> \
  --max-bytes-per-input-token 4 \
  --json -- \
  --repo /absolute/repository --mode diff --base main --head HEAD \
  --context-provider repository_search --context-provider go_ast

# 若 bootstrap/formal 阶段失败，输出仍包含已提交的 source_run_id。使用相同 quick key、
# --at 和 formal 参数恢复；此路径精确复用已提交 source 与 formal terminal，不重复调用 provider。
# 不带 --source-run 重跑首次命令会按设计创建一次新的 source review，不属于精确重试。
./build/argus agent-review quick \
  --store /absolute/argus-store \
  --config-state-dir /absolute/argus-config-state \
  --source-run <source_run_id-from-prior-output> \
  --idempotency-key <same-stable-quick-key> \
  --at 2026-09-04T01:02:03Z \
  --node "$NODE_PATH" --worker-script "$WORKER_PATH" \
  --provider-profile deepseek-anthropic-env --model "$ANTHROPIC_MODEL" \
  --knowledge /absolute/repository-invariants.md \
  --input-micros-per-million <same-governed-input-ceiling> \
  --output-micros-per-million <same-governed-output-ceiling> \
  --max-bytes-per-input-token 4 --json

# 分阶段入口仍保留，用于调试、平台集成或显式控制 bootstrap/run 生命周期。
./build/argus agent-review formal bootstrap \
  --store /absolute/argus-store \
  --config-state-dir /absolute/argus-config-state \
  --source-run <committed-succeeded-review-run> \
  --idempotency-key <stable-bootstrap-key> \
  --at 2026-08-25T01:02:03Z \
  --node "$NODE_PATH" \
  --worker-script "$WORKER_PATH" \
  --provider-profile deepseek-anthropic-env \
  --model "$ANTHROPIC_MODEL" \
  --knowledge /absolute/repository-invariants.md

# 价格参数是调用方治理的最坏情况 ceiling，不从 provider 响应或非版本化 latest 猜测。
# formal success 至少要求一个 change group 完成全部 review dimensions；0 reviewed group 必须失败。
./build/argus agent-review formal run \
  --store /absolute/argus-store \
  --config-state-dir /absolute/argus-config-state \
  --source-run <committed-succeeded-review-run> \
  --idempotency-key <stable-formal-run-key> \
  --node "$NODE_PATH" \
  --worker-script "$WORKER_PATH" \
  --provider-profile deepseek-anthropic-env \
  --model "$ANTHROPIC_MODEL" \
  --knowledge /absolute/repository-invariants.md \
  --input-micros-per-million <governed-input-ceiling> \
  --output-micros-per-million <governed-output-ceiling> \
  --max-bytes-per-input-token 4 \
  --json

# 可选的 Hailix HTTP consumer 路径。token/revision 只从环境读取；其余 trust root 必须显式 pin。
# 当前 Hailix checkout 尚无对应 public server，这段命令用于实现该契约后的正式联调，不可改接 DirectTaskInput。
export ARGUS_HAILIX_BEARER_TOKEN='<request-time-secret>'
export ARGUS_HAILIX_CREDENTIAL_REVISION='<broker-revision>'
# 在上面的 formal run/replay 参数后追加：
#   --execution-backend hailix-http \
#   --hailix-base-url https://hailix.example/api/ \
#   --hailix-capability-verifier-id <id> \
#   --hailix-capability-verifier-revision <revision> \
#   --hailix-capability-verifier-sha256 <64-lower-hex> \
#   --hailix-callback-verifier-id <id> \
#   --hailix-callback-verifier-revision <revision> \
#   --hailix-callback-verifier-sha256 <64-lower-hex>

# 对已成功提交的 formal ReviewRun 做 same-input exact replay。
# 不读取 config latest；runtime/component/config/workflow 必须与源闭包一致，仍 deny remote writes。
./build/argus agent-review formal replay \
  --store /absolute/argus-store \
  --source-formal-run <committed-succeeded-formal-run> \
  --idempotency-key <stable-exact-replay-key> \
  --node "$NODE_PATH" \
  --worker-script "$WORKER_PATH" \
  --provider-profile deepseek-anthropic-env \
  --model "$ANTHROPIC_MODEL" \
  --input-micros-per-million <governed-input-ceiling> \
  --output-micros-per-million <governed-output-ceiling> \
  --max-bytes-per-input-token 4 \
  --json

# 对同一冻结输入做 budget-only variant；两个 timeout 字段原子变化，其他行为身份保持冻结。
./build/argus agent-review formal replay \
  --store /absolute/argus-store \
  --source-formal-run <committed-succeeded-formal-run> \
  --idempotency-key <stable-budget-variant-key> \
  --node "$NODE_PATH" \
  --worker-script "$WORKER_PATH" \
  --provider-profile deepseek-anthropic-env \
  --model "$ANTHROPIC_MODEL" \
  --input-micros-per-million <governed-input-ceiling> \
  --output-micros-per-million <governed-output-ceiling> \
  --max-bytes-per-input-token 4 \
  --change budget \
  --timeout-ms 120000 \
  --json

# model-only variant 会发布 exact model component，改变 manifest build identity，但复用同一 runtime bytes。
./build/argus agent-review formal replay \
  --store /absolute/argus-store \
  --source-formal-run <committed-succeeded-formal-run> \
  --idempotency-key <stable-model-variant-key> \
  --node "$NODE_PATH" \
  --worker-script "$WORKER_PATH" \
  --provider-profile deepseek-anthropic-env \
  --model <variant-model-id> \
  --input-micros-per-million <governed-input-ceiling> \
  --output-micros-per-million <governed-output-ceiling> \
  --max-bytes-per-input-token 4 \
  --change model \
  --at 2026-08-25T03:04:05Z \
  --json

# prompt-only variant 从严格 JSON 文件发布 exact prompt component；模型、工具权限和预算保持冻结。
./build/argus agent-review formal replay \
  --store /absolute/argus-store \
  --source-formal-run <committed-succeeded-formal-run> \
  --idempotency-key <stable-prompt-variant-key> \
  --node "$NODE_PATH" \
  --worker-script "$WORKER_PATH" \
  --provider-profile deepseek-anthropic-env \
  --model "$ANTHROPIC_MODEL" \
  --input-micros-per-million <governed-input-ceiling> \
  --output-micros-per-million <governed-output-ceiling> \
  --max-bytes-per-input-token 4 \
  --change prompt \
  --prompt-bundle /absolute/prompt-bundle.json \
  --at 2026-08-25T03:04:05Z \
  --json

# skill_pack-only variant 用同名 Markdown 替换已有 review skill 的 exact Artifact。
# 可重复传多个 --review-skill；新增/删除维度必须走正常配置发布，不能伪装成原子 replay。
./build/argus agent-review formal replay \
  --store /absolute/argus-store \
  --source-formal-run <committed-succeeded-formal-run> \
  --idempotency-key <stable-skill-variant-key> \
  --node "$NODE_PATH" \
  --worker-script "$WORKER_PATH" \
  --provider-profile deepseek-anthropic-env \
  --model "$ANTHROPIC_MODEL" \
  --input-micros-per-million <governed-input-ceiling> \
  --output-micros-per-million <governed-output-ceiling> \
  --max-bytes-per-input-token 4 \
  --change skill_pack \
  --review-skill /absolute/correctness.md \
  --at 2026-08-25T03:04:05Z \
  --json

# knowledge_pack-only variant 要求文件名 stem 与 baseline knowledge ID 一致。
# 当前需传入完整 ordered knowledge set；新增/删除知识仍走正常配置发布。
./build/argus agent-review formal replay \
  --store /absolute/argus-store \
  --source-formal-run <committed-succeeded-formal-run-with-knowledge> \
  --idempotency-key <stable-knowledge-variant-key> \
  --node "$NODE_PATH" \
  --worker-script "$WORKER_PATH" \
  --provider-profile deepseek-anthropic-env \
  --model "$ANTHROPIC_MODEL" \
  --input-micros-per-million <governed-input-ceiling> \
  --output-micros-per-million <governed-output-ceiling> \
  --max-bytes-per-input-token 4 \
  --change knowledge_pack \
  --knowledge /absolute/repository-invariants.md \
  --at 2026-08-25T03:05:05Z \
  --json

# rule_pack-only variant 接受一个已 seal 的严格 JSON；它会成为 Pi 三个推理阶段的受治理判据，
# 但不能改变工具、凭据、输出 contract 或副作用。
./build/argus agent-review formal replay \
  --store /absolute/argus-store \
  --source-formal-run <committed-succeeded-formal-run> \
  --idempotency-key <stable-rule-pack-variant-key> \
  --node "$NODE_PATH" \
  --worker-script "$WORKER_PATH" \
  --provider-profile deepseek-anthropic-env \
  --model "$ANTHROPIC_MODEL" \
  --input-micros-per-million <governed-input-ceiling> \
  --output-micros-per-million <governed-output-ceiling> \
  --max-bytes-per-input-token 4 \
  --change rule_pack \
  --rule-pack /absolute/rule-pack.json \
  --at 2026-08-25T03:05:20Z \
  --json

# index-only variant 从 source 的 exact repository/commit/target paths 重新采集有序
# built-in context providers，冻结新的 ContextRef/Gap、receipt、ReviewInput 和
# MaterializedTarget 后重跑 Pi；caller-supplied context 原样保留。
./build/argus agent-review formal replay \
  --store /absolute/argus-store \
  --source-formal-run <committed-succeeded-formal-run> \
  --idempotency-key <stable-index-variant-key> \
  --node "$NODE_PATH" \
  --worker-script "$WORKER_PATH" \
  --provider-profile deepseek-anthropic-env \
  --model "$ANTHROPIC_MODEL" \
  --input-micros-per-million <governed-input-ceiling> \
  --output-micros-per-million <governed-output-ceiling> \
  --max-bytes-per-input-token 4 \
  --change index \
  --context-provider repository_search \
  --context-provider go_dependencies \
  --at 2026-08-25T03:05:35Z \
  --json

# filter_policy-only variant 不调用 provider：它复用 source 的 exact
# Hypothesis/Candidate/Verification/Report，仅重算 Calibration/Suppression。
# baseline 必须先通过正常 ConfigRevision publication 启用 finding_governance。
./build/argus agent-review formal replay \
  --store /absolute/argus-store \
  --source-formal-run <committed-succeeded-formal-run> \
  --idempotency-key <stable-governance-variant-key> \
  --node "$NODE_PATH" \
  --worker-script "$WORKER_PATH" \
  --provider-profile deepseek-anthropic-env \
  --model "$ANTHROPIC_MODEL" \
  --input-micros-per-million <governed-input-ceiling> \
  --output-micros-per-million <governed-output-ceiling> \
  --max-bytes-per-input-token 4 \
  --change filter_policy \
  --finding-governance /absolute/finding-governance-policy.json \
  --at 2026-08-25T03:06:05Z \
  --json

# formal baseline/variant 使用 governed report family 比较；Candidate 按稳定 fingerprint 对齐。
./build/argus compare \
  --store /absolute/argus-store \
  --baseline <source-formal-run-id> \
  --variant <budget-variant-run-id> \
  --json

# 跨 revision lineage 与 compare 不同：两侧必须是同 tenant/workspace/repository、同 target mode、
# 不同 target digest 的 committed formal governed run。两侧 run head 必须是同一 local Git object graph
# 中的 exact commit OID；build 证明 baseline 是 variant 的严格祖先，并冻结 50% Git rename evidence。
./build/argus lineage build \
  --store /absolute/argus-store \
  --baseline <older-formal-run-id> \
  --variant <newer-formal-run-id> \
  --json
./build/argus lineage list --store /absolute/argus-store --run <formal-run-id> --json
./build/argus lineage show --store /absolute/argus-store --lineage <finding-lineage-id> --json

# authenticated local API 同样提供 GET/POST /v1/finding-lineages 和
# GET /v1/finding-lineages/{lineage_id}；POST 使用 BuildRequestSchemaVersion。

# 只读已准入终态、Hypothesis、Finding/Decision 报告；不会调用 provider。
./build/argus agent-review formal show \
  --store /absolute/argus-store \
  --formal-run <formal-run-id> \
  --json

# 用当前 deterministic normalization policy 重新聚类 committed raw candidates；
# 只输出 ID/维度/决策/计数，不调用 provider、不写新 Run/Finding，也不等于正式 replay。
./build/argus agent-review formal normalize-preview \
  --store /absolute/argus-store \
  --formal-run <succeeded-formal-run-id> \
  --json

# 对一批 committed raw-candidate closure 做 recorded policy -> 当前 policy 的成对比较；
# 结果是 content-addressed diagnostic artifact，不调用 provider、不改源 Run，也不进入 gold/promotion。
./build/argus agent-review formal normalize-compare \
  --store /absolute/argus-store \
  --comparison-id <stable-id> \
  --case <case-id>=<succeeded-formal-run-id> \
  --case <case-id>=<another-succeeded-formal-run-id> \
  --at <RFC3339-UTC> \
  --json

# unique-claim 去重质量不能从收缩数量推断。先将外部 reviewer/adjudicator 对 committed raw claims
# 的 equivalence classes 绑定到 governed CorpusSnapshot 并封存。seal 只创建 immutable artifact；
# 该 artifact 还必须由预注册的 scoped Ed25519 trust key 签名并写入 append-only oracle registry。
./build/argus evaluation normalization oracle seal \
  --store /absolute/argus-store \
  --input /absolute/normalization-oracle.json \
  --access /absolute/evaluation-access.json \
  --json
./build/argus evaluation normalization oracle register \
  --store /absolute/argus-store \
  --input /absolute/normalization-oracle-registration.json \
  --mutation /absolute/normalization-oracle-registration-mutation.json \
  --json
# register 输出的 exact binding 必须写入 quality request；不能只传一个可漂移的 oracle ref。
./build/argus evaluation normalization run \
  --store /absolute/argus-store \
  --input /absolute/normalization-quality-run-request.json \
  --access /absolute/evaluation-access.json \
  --json
./build/argus evaluation normalization oracle list \
  --store /absolute/argus-store --access /absolute/evaluation-access.json --json
./build/argus evaluation normalization oracle show \
  --store /absolute/argus-store --oracle <oracle-id> --access /absolute/evaluation-access.json --json
./build/argus evaluation normalization oracle revoke \
  --store /absolute/argus-store \
  --input /absolute/normalization-oracle-revocation.json \
  --mutation /absolute/normalization-oracle-revocation-mutation.json \
  --json

# 撤销、trust-key 撤销、case label/governance 漂移或非当前 CAS revision 都会令后续 run/show 失败关闭。
./build/argus evaluation normalization show \
  --store /absolute/argus-store \
  --ref /absolute/normalization-quality-run-ref.json \
  --access /absolute/evaluation-access.json \
  --json

# 归一化策略只能通过独立 test/holdout quality artifact 晋级。prepare 从 exact active baseline
# ReviewRun/ConfigResolutionReceipt 派生并 validate 只替换 normalization selector 的 ConfigRevision，
# 同时密封质量阈值、shadow/canary 最小运行数和 canary percentage。
./build/argus evaluation normalization promotion prepare \
  --store /absolute/argus-store \
  --config-state-dir /absolute/config-state \
  --input "$PWD/examples/normalization-promotion-prepare-request.json" \
  --mutation /absolute/normalization-promotion-prepare-mutation.json \
  --json
./build/argus evaluation normalization promotion gate \
  --store /absolute/argus-store \
  --input /absolute/normalization-promotion-gate-request.json \
  --mutation /absolute/normalization-promotion-gate-mutation.json \
  --json

# test/holdout 通过后，shadow gate 只接受 exact variant bundle 的成功 ReviewRun。
./build/argus evaluation normalization promotion operational-gate \
  --store /absolute/argus-store \
  --config-state-dir /absolute/config-state \
  --input "$PWD/examples/normalization-promotion-operational-gate-request.json" \
  --mutation /absolute/normalization-shadow-mutation.json \
  --json

# shadow 通过后，以 policy 固定 percentage 和稳定 seed 发布 canary；policy-ref 必须使用 prepare
# 输出的完整 ArtifactRef（含正确 size_bytes）。真实命中 canary 的 ReviewRun 再提交 canary gate，
# 随后依次提交 promotion_authorization 与 rollback_monitor operational gate。
./build/argus evaluation normalization promotion canary \
  --store /absolute/argus-store \
  --config-state-dir /absolute/config-state \
  --variant <variant-id> \
  --policy-ref /absolute/normalization-policy-ref.json \
  --percentage 10 --seed <stable-seed> \
  --mutation /absolute/normalization-canary-mutation.json \
  --json

# 七个 gate 全绿后仍需显式将同一 seed 的 canary 单调扩到 100%；任一阶段可显式回滚/中止。
./build/argus evaluation normalization promotion activate \
  --store /absolute/argus-store --config-state-dir /absolute/config-state \
  --variant <variant-id> --mutation /absolute/normalization-activate-mutation.json --json
./build/argus evaluation normalization promotion rollback \
  --store /absolute/argus-store --config-state-dir /absolute/config-state \
  --variant <variant-id> --mutation /absolute/normalization-rollback-mutation.json --json
./build/argus evaluation normalization promotion show \
  --store /absolute/argus-store --variant <variant-id> \
  --access /absolute/evaluation-access.json --json

# 本地平台提供同一领域服务的 prepare/quality gate/operational gate/canary/activate/rollback API；
# embedded UI 可查看 shared ledger 并执行显式生命周期操作。涉及配置变更的 API 同时要求
# evaluation 与 config 权限；不会自动激活。

# Candidate 与 Verification 是独立事实集合；list 返回全部 verification facts，show 返回
# 该 Candidate 的 latest fact。rejected/inconclusive 不会因没有 Finding 而消失。
./build/argus candidate list --store /absolute/argus-store --run <formal-run-id> --json
./build/argus candidate show \
  --store /absolute/argus-store --run <formal-run-id> --candidate <candidate-id> --json

# 通用 show/API detail 同时返回独立 calibration/suppression ledger。未配置策略或 reviewer 未提供
# raw confidence 时保持 unavailable/human queue；exact ConfigBundle 启用 finding_governance 后执行
# 整数分段校准、minimum-confidence 与 max-findings，所有 suppressed loser 仍可查询。
# 可发布的 repository-scope patch 样例见 examples/config-revision.finding-governance.json；其中曲线仅为
# 机制示例，生产 profile 应由隔离标注集拟合和验收。
# 受治理拟合要求每条 observation 回连 active EvaluationCase 和 committed Candidate；train 与
# dev/test 隔离，holdout 不参与。输出只是不可变 candidate/report，不会自动发布配置。
./build/argus calibration fit \
  --store /absolute/argus-store \
  --input "$PWD/examples/calibration-fit-request.json" \
  --mutation /absolute/calibration-fit-mutation.json \
  --json
./build/argus calibration show \
  --store /absolute/argus-store --run <calibration-run-id> --access /absolute/evaluation-access.json --json
# authenticated local API 同步提供 GET/POST /v1/calibration/runs 与
# GET /v1/calibration/run?run_id=...；POST 样例见 examples/local-api-calibration-fit-command.json。
# 训练数据物化只输出受治理的 reference-only manifest，不复制源码正文。请求必须提供 ConfigBundle 和
# Case input/source/label/authority evidence 的完整 ArtifactRef，服务同时重验 training/export gate、
# strict redaction、license/consent、独立裁决和内容摘要。
./build/argus training materialize \
  --store /absolute/argus-store \
  --input "$PWD/examples/training-materialization-request.json" \
  --mutation /absolute/training-materialization-mutation.json \
  --json
./build/argus training show \
  --store /absolute/argus-store --manifest <training-manifest-id> \
  --access /absolute/evaluation-access.json --json
# authenticated API 同步提供 GET/POST /v1/training/manifests 与
# GET /v1/training/manifest?manifest_id=...；POST command 样例见
# examples/local-api-training-materialize-command.json。
# 使用 materialize 返回的 exact manifest_id/ref 构造 export request。build 会重新验证全部治理和 artifact，
# 使用固定版本 strict-text policy 产出内容寻址 redacted artifacts、receipts 与 bundle。
./build/argus training export build \
  --store /absolute/argus-store \
  --input "$PWD/examples/training-export-request.json" \
  --mutation /absolute/training-export-mutation.json \
  --json
./build/argus training export publish \
  --store /absolute/argus-store --export <training-export-id> \
  --access /absolute/evaluation-access.json \
  --output /absolute/new-directory-outside-store --json
# publish 只写 manifest.json 和仅含脱敏正文的 records.jsonl；同路径仅允许 exact retry。
# authenticated API 提供 GET/POST /v1/training/exports 与
# GET /v1/training/export?export_id=...，但不会接受服务端 output path 或执行 publish。
# 当前 strict-text policy 是有明确规则覆盖范围的 deterministic detector，不是通用 DLP。
# 在 exact export 上可以准备 provider-neutral 外部训练计划；Argus 不调用 provider，remote side effects 固定 deny。
./build/argus training job prepare \
  --store /absolute/argus-store \
  --input "$PWD/examples/training-job-prepare-request.json" \
  --mutation /absolute/training-job-prepare-mutation.json --json
# 外部人工/独立执行后，只能用当前 plan SHA 追加 submitted，再追加 succeeded|failed|canceled。
./build/argus training job observe \
  --store /absolute/argus-store \
  --input "$PWD/examples/training-job-observation-request.json" \
  --mutation /absolute/training-job-observation-mutation.json --json
# receipt 固定为 operator_recorded_unattested，plan 固定 promotion_eligible=false；记录成功不等于 provider 证明。
# API 同步提供 GET/POST /v1/training/jobs、GET /v1/training/job?job_id=... 与
# POST /v1/training/job/observations，但不持有 provider credential 或提交远端训练。
# 启动 `argus api serve` 后，Operator UI 的“训练治理”复用同一组认证端点查看三类历史并执行
# materialize/build/prepare/observe。UI 不提供 export publish/output path，也不会调用训练 provider；
# portable 文件发布仍只能通过上面的 CLI 明确执行。
# passed candidate 不再需要手写普通 config/promotion 操作。下面只准备并 validate 候选，不发布；随后按
# next_gate 依次记录证据，全部通过后仍需显式 activate。managed variant 无法从通用 promotion CLI 绕过。
./build/argus calibration promotion prepare \
  --store /absolute/argus-store --config-state-dir /absolute/config-state \
  --input "$PWD/examples/calibration-promotion-prepare-request.json" \
  --mutation /absolute/calibration-promotion-prepare-mutation.json --json
./build/argus calibration promotion gate \
  --store /absolute/argus-store --config-state-dir /absolute/config-state \
  --input "$PWD/examples/calibration-promotion-gate-request.json" \
  --mutation /absolute/calibration-promotion-gate-mutation.json --json
# 全部 gate 通过后：calibration promotion activate --plan <id> --percentage 10 --seed <stable-seed> ...
# active 后可用 calibration promotion rollback --plan <id> ... 恢复两个 ledger projection。
# authenticated API 对应 /v1/calibration/promotion/{plans,plan,gates,activate,rollback}；Operator UI 的
# “评测数据 / Calibration Promotion”可查看计划、提交 gate、激活与回滚。
./build/argus config create \
  --state-dir /absolute/config-state \
  --file "$PWD/examples/config-revision.finding-governance.json" \
  --idempotency-key governance-create-1 --actor local-operator \
  --audit 'create repository finding governance' --at 2026-08-26T00:00:00Z --json
# 随后对 repository-finding-governance@1 执行 config validate/publish；formal run 必须使用同一
# --config-state-dir。已提交 run/replay 始终读取自身 ExecutionSnapshot 的 exact ConfigBundle，不读 latest。
./build/argus show --store /absolute/argus-store --run <formal-run-id> --json

# Finding 查询保持 source-typed；formal 结果返回 governed_finding/governed_decisions。
./build/argus finding show \
  --store /absolute/argus-store --run <formal-run-id> --finding <finding-id> --json

# 后续人工 Decision 是独立 append-only ledger。control-plane 自行解析并冻结源报告
# artifact/初始 Decision；publish 需要 publication_approver，replay 永远不能 publish。
./build/argus decision record \
  --store /absolute/argus-store \
  --input /absolute/finding-decision-request.json \
  --mutation /absolute/finding-decision-mutation.json \
  --json

# 评审执行快照始终保持 remote write deny。发布审批人另行签发一次性、最长 24h、
# 精确绑定 Decision/source/diff/config/publication identity 的 PublicationGrant。
./build/argus publication grant record \
  --store /absolute/argus-store \
  --input /absolute/publication-grant-request.json \
  --mutation /absolute/publication-grant-mutation.json \
  --json

# Intent 只含 grant_id 和创建时间。构建 provider-neutral PublicationRequest 不调用远端；
# dispatch authorizer 才以 append-only reservation 原子消费 Grant。
./build/argus publication request build \
  --store /absolute/argus-store \
  --input /absolute/publication-intent.json \
  --json

# single-user-local 是显式本地 profile，不冒充 Hailix/IAM。Grant 的
# expected_permission_revision 必须为 single-user-local-v1；凭据只从三个固定环境变量读取。
export ARGUS_GITHUB_TOKEN='<secret>'
export ARGUS_GITHUB_PRINCIPAL_ID='<github-user-or-installation-id>'
export ARGUS_GITHUB_CREDENTIAL_REVISION='<operator-managed-revision>'
./build/argus publication github dispatch \
  --store /absolute/argus-store \
  --input /absolute/publication-intent.json \
  --single-user-local \
  --json

# create outcome unknown 时只允许 reconcile；它不会启动一个尚未越过 provider boundary 的写入。
./build/argus publication github reconcile \
  --store /absolute/argus-store \
  --publication <publication-id> \
  --single-user-local \
  --json

# 本地 CLI 已验证 deny review -> Decision -> Grant -> dispatch/unknown -> lookup reconcile。
# 未提供 env、Grant、exact revision 或显式 profile 时全部失败关闭；生产仍需 Hailix/IAM broker。

# Feedback/Outcome 不产生或修改 Decision。Argus UI/API 的人工复核可直接对 human queue
# 记录 Feedback/Outcome；code_host 来源
# 仍必须绑定 publication ledger 中实际 published comment。输入使用 strict JSON contract。
./build/argus feedback record \
  --store /absolute/argus-store --input /absolute/feedback.json --json
./build/argus outcome record \
  --store /absolute/argus-store --input /absolute/outcome.json --json

# 只从最新 human publish Decision，或未被后续事实更正的 accept/dismiss Feedback 派生评测候选。
# repository/snapshot/finding source/anchor/fingerprint 由控制面回读；结果强制为
# pending + unassigned + candidate_pool-only + eligibility=false，绝不会直接成为 gold。
./build/argus evaluation case derive \
  --store /absolute/argus-store \
  --input /absolute/evaluation-candidate-derivation-request.json \
  --mutation /absolute/evaluation-mutation.json \
  --json

# reviewed_bug_fix_pair 使用同一 derive 命令，并额外提供 fix_run_id；source_id 是 latest fixed Outcome。
# fix run 必须同仓且 base=defect head，Outcome 的 change/ci_run revision 必须都等于 fix head。

# 独立记录漏检事故；它不能由已有 Finding/escaped Outcome 代替。
./build/argus evaluation incident record \
  --store /absolute/argus-store \
  --input /absolute/missed-defect-incident.json \
  --mutation /absolute/incident-ingest-mutation.json \
  --json

# incident_missed_defect derive 不设置 finding_id。控制面要求完整 formal report，并证明
# incident fingerprint/anchor 没有出现在当时的任何 canonical Candidate 中。

# 记录 mutation/synthetic defect/clean/workflow invariant 的独立 source fact。
# Probe 的 source/oracle/executor authority 必须互异；receipt artifact 需先存在于同一 run store，
# exact 绑定 TargetSnapshot、oracle digest、method revision、actual observation、evidence 与 remote deny。
./build/argus evaluation probe record \
  --store /absolute/argus-store \
  --input /absolute/evaluation-probe.json \
  --mutation /absolute/probe-ingest-mutation.json \
  --json

# 使用 mutation_probe、synthetic_probe 或 workflow_invariant_probe 调用同一 case derive；不设置 finding_id。
# mutation receipt 还必须绑定同仓 distinct baseline；synthetic/clean/workflow 强制 consent=synthetic。

# evaluation case create 只接受 pending + unassigned + candidate_pool + eligibility=false，不能直接
# 导入 gold/active。外部 key 必须先由独立 governance_trust_admin 注册；registered_at 与 mutation.at
# 必须相同，input 见 examples/governance-trust-key-registration.json。
./build/argus evaluation trust-key register \
  --store /absolute/argus-store \
  --input /absolute/governance-trust-key-registration.json \
  --mutation /absolute/trust-key-registration-mutation.json \
  --json

# 外部已经完成多人治理的 Case 必须使用签名 import；key 必须 exact 匹配 active registry revision，
# import operator 不能是 trust admin/reviewer/adjudicator。mutation.at 必须与 imported_at 完全一致。
./build/argus evaluation case import \
  --store /absolute/argus-store \
  --input /absolute/external-governed-case-import.json \
  --mutation /absolute/external-import-mutation.json \
  --json

# 撤销只阻止后续 import，不追溯删除已导入 Case；list 需要 governance_trust_admin access。
./build/argus evaluation trust-key revoke \
  --store /absolute/argus-store \
  --input /absolute/governance-trust-key-revocation.json \
  --mutation /absolute/trust-key-revocation-mutation.json \
  --json
./build/argus evaluation trust-key list \
  --store /absolute/argus-store --access /absolute/trust-admin-access.json --json

# registry/key/import 都是本地 append-only 治理事实，不等于 Hailix/IAM 已认证 actor 或 key authority。

# curator 先冻结 blind assignment；未分配 reviewer 不能提交，reviewer 读取看不到同轮其他意见。
./build/argus evaluation case assign \
  --store /absolute/argus-store \
  --input /absolute/evaluation-case-review-assignment.json \
  --mutation /absolute/assignment-mutation.json \
  --json

# 两名不同 assigned reviewer 只能追加独立 annotation；annotation 本身不能修改数据集真值。
./build/argus evaluation case annotate \
  --store /absolute/argus-store \
  --input /absolute/evaluation-case-annotation.json \
  --mutation /absolute/reviewer-mutation.json \
  --json

# 第三名独立 adjudicator 冻结至少两条 annotation event；通过后只进入 unassigned gold。
./build/argus evaluation case adjudicate \
  --store /absolute/argus-store \
  --input /absolute/evaluation-case-adjudication.json \
  --mutation /absolute/adjudicator-mutation.json \
  --json

# curator 单独扩展许可、分配 split；holdout 必须由 holdout_maintainer 操作。
./build/argus evaluation case activate \
  --store /absolute/argus-store \
  --input /absolute/evaluation-case-activation.json \
  --mutation /absolute/curator-mutation.json \
  --json

# 新证据可把 gold/active/retired append-only reopen 回 candidate-only；历史与受影响 experiments 保留。
./build/argus evaluation case reopen \
  --store /absolute/argus-store \
  --input /absolute/evaluation-case-reopen.json \
  --mutation /absolute/reopen-mutation.json \
  --json

# 1..500 个同类治理操作可组成 recoverable batch。intent 先落账，item event_id 是 checkpoint；
# partial failure 会显式返回 succeeded/failed/not_started，不承诺跨 item 原子回滚。
./build/argus evaluation governance-batch run \
  --store /absolute/argus-store \
  --input /absolute/governance-batch-request.json \
  --mutation /absolute/governance-batch-mutation.json \
  --json
./build/argus evaluation governance-batch show \
  --store /absolute/argus-store --batch <batch-id> \
  --access /absolute/evaluation-access.json --json
./build/argus evaluation governance-batch list \
  --store /absolute/argus-store --access /absolute/evaluation-access.json --json

# 启动本地平台 API。principal 是进程固定身份，HTTP 请求不能声明或覆盖 actor/roles；
# token 只从环境读取。listen 必须是 literal loopback IP，不能使用 localhost、0.0.0.0 或局域网地址。
export ARGUS_LOCAL_API_TOKEN='<at-least-32-byte-random-secret>'
./build/argus api serve \
  --store /absolute/argus-store \
  --config-state-dir /absolute/argus-config-state \
  --principal /absolute/local-api-principal.json \
  --listen 127.0.0.1:7788

# 可选：让同一 API process 接受 formal_pi_review_v1。credential 仍只从当前进程环境读取，
# 不进入 request、command 或 ledger；下面的 runtime/model/pricing flags 必须成组提供。
./build/argus api serve \
  --store /absolute/argus-store \
  --config-state-dir /absolute/argus-config-state \
  --principal /absolute/local-api-principal.json \
  --formal-node "$(command -v node)" \
  --formal-worker-script "$PWD/runtime/pi-review/dist/worker.js" \
  --formal-provider-profile deepseek-anthropic-env \
  --formal-model deepseek-chat \
  --formal-input-micros-per-million 1 \
  --formal-output-micros-per-million 1 \
  --formal-max-bytes-per-input-token 4

# 打开 credential-free 静态 UI 壳；它不含 principal/token/store 数据。页面内输入 token 后，所有数据请求
# （包括 health）仍需要 bearer。列表默认 50、最大 200，next_cursor 是不透明 cursor。
open http://127.0.0.1:7788/ui/

curl --fail-with-body \
  -H "Authorization: Bearer $ARGUS_LOCAL_API_TOKEN" \
  'http://127.0.0.1:7788/v1/evaluation/cases?limit=50'

# ReviewRun history 使用 run-index sequence watermark 冻结一次分页遍历；详情对 pending/running
# 只返回 lifecycle summary，对 terminal run 先校验完整 closure 再返回结构化 result family。
curl --fail-with-body \
  -H "Authorization: Bearer $ARGUS_LOCAL_API_TOKEN" \
  'http://127.0.0.1:7788/v1/review-runs?limit=50'

# durable review job 在返回 202 前先冻结 exact published ConfigBundle/receipt 和 immutable command，
# 随后复用 scheduling ledger 异步 claim/lease/heartbeat/fencing/callback；客户端断开不会取消。
curl --fail-with-body -X POST \
  -H "Authorization: Bearer $ARGUS_LOCAL_API_TOKEN" \
  -H 'Content-Type: application/json' \
  --data-binary @examples/local-api-review-job-submit-command.json \
  http://127.0.0.1:7788/v1/review-jobs

# formal job 必须引用一个已成功且 committed 的 deterministic source ReviewRun，并且此前已用
# `argus agent-review formal bootstrap` 向同一 store/config-state 发布 subject-bound components/config。
curl --fail-with-body -X POST \
  -H "Authorization: Bearer $ARGUS_LOCAL_API_TOKEN" \
  -H 'Content-Type: application/json' \
  --data-binary @examples/local-api-formal-review-job-submit-command.json \
  http://127.0.0.1:7788/v1/review-jobs

# 只有显式 cancel command 会永久 fence 该 job/run，并取消本进程中仍活跃的 execution context。
curl --fail-with-body -X POST \
  -H "Authorization: Bearer $ARGUS_LOCAL_API_TOKEN" \
  -H 'Content-Type: application/json' \
  --data-binary @examples/local-api-review-job-cancel-command.json \
  http://127.0.0.1:7788/v1/review-jobs/<job-id>/cancel

# timeline 是 scheduling append-only ledger 的 workload-scoped 只读投影，保留原 sequence、
# admission reason、attempt/generation/fencing、heartbeat、reconcile、callback/cancel 证据。
curl --fail-with-body \
  -H "Authorization: Bearer $ARGUS_LOCAL_API_TOKEN" \
  'http://127.0.0.1:7788/v1/review-jobs/<job-id>/timeline?limit=50'

# 查询 scheduling ledger 的只读背压快照。`at` 必须是显式 UTC；快照绑定 policy
# revision/SHA 与 ledger sequence，并按 global/class/tenant 给出 queue/active depth、
# oldest pending wait、admission/state/stale counters。查询不会隐式 reconcile 过期工作。
./build/argus workload pressure \
  --store /absolute/argus-store --at 2026-08-26T02:00:00Z --json
curl --fail-with-body \
  -H "Authorization: Bearer $ARGUS_LOCAL_API_TOKEN" \
  'http://127.0.0.1:7788/v1/workloads/pressure?at=2026-08-26T02%3A00%3A00Z'

# Finding drill-down 保留模型/治理/人类 Decision、Feedback、Outcome 的独立事实，不合并成可变状态。
curl --fail-with-body \
  -H "Authorization: Bearer $ARGUS_LOCAL_API_TOKEN" \
  'http://127.0.0.1:7788/v1/review-runs/<run-id>/findings/<finding-id>'

# 用 exact id/revision（可选 SHA-256）反查冻结了某个配置修订、ConfigBundle、
# RulePack、Workflow 或 Model 的 ReviewRun。API 启动会显式增量 rebuild 旧 ledger，
# 正常新 run 在权威 event 前写 typed reverse-index row；GET 本身不写盘。实际命中的
# terminal/nonterminal run 仍分别重验 whole closure/created lineage；缺 snapshot 或
# 尚未 rebuild 的 row 都进入带 reason_code 的 coverage.gaps，不会被静默省略。
curl --fail-with-body \
  -H "Authorization: Bearer $ARGUS_LOCAL_API_TOKEN" \
  'http://127.0.0.1:7788/v1/review-run-impacts?kind=model&id=<model-id>&revision=<revision>&limit=50'

# `review_write` 使用 process-fixed actor_kind/actor/finding_roles；body 无法自报身份。
# embedded operator UI 的 Finding 详情页调用同一组端点，只允许填写行动、证据、
# correction lineage 与审计说明；身份和角色仍只由启动时 principal 注入。
curl --fail-with-body -X POST \
  -H "Authorization: Bearer $ARGUS_LOCAL_API_TOKEN" \
  -H 'Content-Type: application/json' \
  --data-binary @examples/local-api-finding-decision-write-command.json \
  'http://127.0.0.1:7788/v1/review-runs/<run-id>/findings/<finding-id>/decisions'
curl --fail-with-body -X POST \
  -H "Authorization: Bearer $ARGUS_LOCAL_API_TOKEN" \
  -H 'Content-Type: application/json' \
  --data-binary @examples/local-api-feedback-write-command.json \
  'http://127.0.0.1:7788/v1/review-runs/<run-id>/findings/<finding-id>/feedback'
curl --fail-with-body -X POST \
  -H "Authorization: Bearer $ARGUS_LOCAL_API_TOKEN" \
  -H 'Content-Type: application/json' \
  --data-binary @examples/local-api-outcome-write-command.json \
  'http://127.0.0.1:7788/v1/review-runs/<run-id>/findings/<finding-id>/outcomes'

# 写请求只提交业务 contract 和 local mutation；示例不会携带 actor/roles。
curl --fail-with-body -X POST \
  -H "Authorization: Bearer $ARGUS_LOCAL_API_TOKEN" \
  -H 'Content-Type: application/json' \
  --data-binary @examples/local-api-governance-batch-command.json \
  http://127.0.0.1:7788/v1/evaluation/governance-batches

# 当前还提供 case show/import、governance batch show/list、trust-key register/list/revoke。
# domain-valid Case/Batch ID 可能含 `/` 或 `:`，此时使用 singular exact query endpoint：
# `/v1/evaluation/case?case_id=...`、`/v1/evaluation/governance-batch?batch_id=...`。
# 评测运行历史保持事实类型分离，并使用领域层 Case ACL：
# `/v1/evaluation/evaluation-runs` + `/v1/evaluation/evaluation-run?evaluation_run_id=...`
# `/v1/evaluation/experiment-runs` + `/v1/evaluation/experiment-run?experiment_run_id=...`
# `/v1/evaluation/repeatability-runs` + `/v1/evaluation/repeatability-run?repeatability_run_id=...`
# 可恢复执行状态另由 experiment/repeatability-batches 列表及 singular `?batch_id=...` 查询。
# 当 `api serve` 启用了完整 formal Pi profile 时，`evaluation_write` 可直接提交不含本地
# 文件路径的 budget、exact model ID、已发布 prompt/skill/knowledge、已 seal RulePack 或
# ordered built-in context provider 单变量 ExperimentBatch，以及
# exact RepeatabilityBatch。
# adapter 会从服务启动配置冻结 credential-free executor template，先持久化 intent，再异步执行；
# 调用方必须省略 executor_template_ref。组件实验只接收 exact、排序唯一的 component_refs；
# 引用按 baseline ReviewRun 的 subject 解析，所有 subject 都在 batch admission/provider 前闭合。
curl --fail-with-body -X POST \
  -H "Authorization: Bearer $ARGUS_LOCAL_API_TOKEN" \
  -H 'Content-Type: application/json' \
  --data-binary @examples/local-api-experiment-batch-submit-command.json \
  'http://127.0.0.1:7788/v1/evaluation/experiment-batches'
# prompt 组件引用实验使用同一 endpoint/schema：
# examples/local-api-prompt-experiment-batch-submit-command.json
# model-only 使用同一 endpoint/schema，但只携带 configured provider 下的 exact model ID：
# examples/local-api-model-experiment-batch-submit-command.json
# index-only 使用同一 endpoint/schema，context_providers 必须是有序、唯一、可发布的本地内置定义：
# examples/local-api-index-experiment-batch-submit-command.json
# rule_pack 使用同一 endpoint/schema，exact sealed bytes 会进入 Pi 的 context/review/verifier 输入：
# examples/local-api-rule-pack-experiment-batch-submit-command.json

# component_write 可先把 exact UTF-8 prompt/skill/knowledge 发布到 baseline ReviewRun 派生的
# subject；body 不接受 subject/actor/path，ref SHA-256 必须绑定 canonical base64 解码后的字节。
curl --fail-with-body -X POST \
  -H "Authorization: Bearer $ARGUS_LOCAL_API_TOKEN" \
  -H 'Content-Type: application/json' \
  --data-binary @examples/local-api-agent-component-publish-command.json \
  'http://127.0.0.1:7788/v1/agent-components'
curl --fail-with-body -X POST \
  -H "Authorization: Bearer $ARGUS_LOCAL_API_TOKEN" \
  -H 'Content-Type: application/json' \
  --data-binary @examples/local-api-repeatability-batch-submit-command.json \
  'http://127.0.0.1:7788/v1/evaluation/repeatability-batches'
# `evaluation_write` 还可异步恢复已经具备 exact executor template 的 running batch；恢复 command
# 只携带 mutation，actor/roles 仍由 principal 注入。202 表示恢复请求已审计并被本地 launcher 接受，
# 不表示 batch 已成功；随后查询 batch status/checkpoints/active_lease/last_failure。
curl --fail-with-body -X POST \
  -H "Authorization: Bearer $ARGUS_LOCAL_API_TOKEN" \
  -H 'Content-Type: application/json' \
  --data-binary @examples/local-api-evaluation-batch-resume-command.json \
  'http://127.0.0.1:7788/v1/evaluation/experiment-batch/resume?batch_id=<batch-id>'
# Repeatability 使用同一 command schema：
# /v1/evaluation/repeatability-batch/resume?batch_id=<batch-id>
# Config lifecycle 提供 revision create/list/show、validate/publish/rollback；config state 与运行 store
# 显式分离，必须指向 review/formal execution 实际使用的同一个 --config-state-dir。
# `POST /v1/config/resolutions` 接收 strict ResolutionContext，并原子返回 exact ConfigBundle 与
# ConfigResolutionReceipt；样例见 examples/local-api-config-resolution-query.json。该查询不接受 actor/roles，
# UI 的“解析生效配置”可查看 applied revision 和逐字段 explain。
# Dashboard API 只 list/show 已经由 `dashboard rebuild` 创建的 immutable snapshot；show 返回 dashboard、
# coverage 和 source bindings，不返回底层 raw facts，也不会在请求路径现场扫描 authoritative ledgers。
# principal 的 `permissions` 分开控制 config_read/config_write/dashboard_read/evaluation_read/
# evaluation_write/review_execute/review_read；workload pressure 复用只读 `review_read`，
# `roles` 只用于 Evaluation 的领域授权。
# ReviewRun detail 不返回 raw prompt、agent task evidence/receipt 或 Markdown artifact bytes；Finding
# detail 也不返回 publication provider request/result payload。
# `/ui/` 只提供同源 HTML/CSS/JS，使用 strict CSP；token 仅在当前 JS 内存变量中存在，断开或 pagehide 清除，
# 不写 localStorage/sessionStorage/cookie，不进入 URL。UI 已支持 workload pressure 概览、durable ReviewJob submit/list/show/cancel/timeline、
# ReviewRun/Finding、Config、Dashboard、Evaluation Case、三类评测运行和两类 replay batch 浏览/提交/恢复，
# 以及 Config create/validate/publish/rollback。
# 当前 job execution_profile 包含 deterministic_review_v1 与 formal_pi_review_v1；后者不自动 bootstrap，
# 也不把本地 API process 伪称为 Hailix production worker。
# 这是 single-process local authority，不是 hosted、多租户或 Hailix/IAM 身份证明。

# formal evidence ref/scope 只从 committed terminal whole-closure 推导。
./build/argus agent-review formal evidence read \
  --store /absolute/argus-store \
  --formal-run <formal-run-id> \
  --request-id <unique-access-id> \
  --actor <human-or-service-id> \
  --purpose evaluation_replay \
  --at 2026-08-25T02:03:04Z \
  --acknowledge-sensitive-output \
  --json

./build/argus agent-review formal evidence revoke \
  --store /absolute/argus-store \
  --formal-run <formal-run-id> \
  --idempotency-key <stable-revocation-key> \
  --actor <retention-operator-id> \
  --reason 'local retention period ended' \
  --at 2026-09-25T02:03:04Z
```

DeepSeek Anthropic-compatible profile 只从进程环境读取 `ANTHROPIC_API_KEY`、
`ANTHROPIC_BASE_URL` 和 `ANTHROPIC_MODEL`。bootstrap 只保存固定的 env SecretRef，不读取、
打印或持久化 secret/endpoint。同一 formal `run` 在终态后重试会重验 immutable runtime/config
identity，再从 terminal gate 与 evidence ledger 恢复结果，绝不二次调用模型。failed/canceled
终态会先提交可查询的 failed/canceled `ReviewRun`、打印 JSON/文本，再返回非零。通用
`argus history` 和 `argus show --run <formal-run-id>` 也能读取该终态。formal `replay` 目前开放
`variable=none` 的 same-input exact replay，以及 `budget`、`model`、`prompt`、`skill_pack`、
`knowledge_pack`、`rule_pack`、`workflow`、`index`、`filter_policy` 单变量 replay；budget 只允许
外层 stage 和 Pi agent timeout 原子变化，model 只允许 governed model component 与 manifest
build identity 变化，prompt 只允许 exact governed prompt component 与对应 build identity 变化；
skill_pack 只允许在保持技能 ID、phase 和顺序不变时替换 exact governed skill Artifact。
knowledge_pack 同样保持知识 ID 和顺序，仅替换 exact Artifact。rule_pack 只替换一个已 seal 的
缺陷判据集合，exact JSON bytes 和语义 digest 同时进入 AgentStagePlan/worker envelope，并由 Pi 的
context/review/verifier 实际消费；它不能改变权限、输出 contract 或副作用。index 只改变 ordered
`execution.context_providers`，从 source exact commit 重新物化上下文并重跑 Pi；provider adapter 以
content-addressed component 发布，receipt 在 plan admission 和 terminal 两次闭合。filter_policy 复用 source 的 provider
产物且只重算 calibration/suppression；旧 `finding_governance` 变量只保留兼容。新增/删除技能或知识需要
正常配置 revision。workflow 只允许 exact 单阶段定义的新 revision 改变 timeout/input/output/concurrency
预算，并把与 AgentReviewPolicy 逐项取最小后的有效值写入 sealed AgentStagePlan；graph、executor、contract、
authority、retry、side effect 和 build identity 保持冻结。deterministic
`argus replay` 已另行支持受限 workflow policy variant，不会被冒充为 formal Pi 行为变化。

`agent-review run` 只有在 worker 成功且 Go host 完整闭合校验后才提交 shadow result；
随后再按 manifest 覆盖状态决定退出码。受限上下文运行常会得到 `partial`；只有明确接受
这种覆盖语义时才传 `--allow-partial`，否则命令在结果已经提交后返回非零，便于自动化
默认失败关闭。intent 落盘后的失败、取消或无法确认 completion 的错误路径会先输出可查询
的 `ExecutionAttempt` 再返回非零；plain 输出包含状态、execution/source-run ID、failure
code、bounded host diagnosis、reused、`outcome_acknowledged` 和 `unconfirmed_manifest`，JSON
输出绑定 Attempt、acknowledgement、reused、store path 与可选
的 `unconfirmed_committed_result`。后者只出现在 shadow import 已提交、但 completion 未能
确认的 crash window；可以用 `agent-review show --manifest-id <id>` 检查该 manifest，或
用 `agent-review execution reconcile` 在完整重验 LocalBridge 于 import 前写入的 immutable
authorization、intent/runtime digests、三份 canonical import payload、expected manifest、
ImportRecord 和 artifact closure 后补写幂等 succeeded completion。legacy/direct Import 没有
该 authorization，不能嫁接到 execution；authorization 已落盘但 exact import 尚未 committed
时也不能伪造成功。没有完整且绑定的 committed result 时 reconcile 保持
`unknown_outcome`，绝不调用 worker/provider。

`Attempt.Status` 表示调用结束后可严格重验的当前状态，`outcome_acknowledged` 表示本次 mutation
是否获得成功确认。completion 文件已 rename、随后目录 fsync 失败时，前者可能已经是
`succeeded` 而后者仍为 `false`；自动化必须同时要求命令成功和 acknowledgement，不能仅凭
stdout/JSON 中的状态确认本次写入。

v1alpha2 completion 与 v1alpha3 Attempt 将 worker 报告的 `completed_at` 和 host 写入的
`recorded_at` 分开。
`succeeded` 的 `completed_at` 不得越过执行 deadline；`failed`/`canceled` 可以在取消
收敛后完成，但所有 terminal `completed_at` 都不得晚于 `recorded_at`。terminal Attempt
的 `observed_at` 只取 `recorded_at`，作为 history 与 analytics 的唯一窗口归属时间。

所有命令组的嵌套 `-h`/`--help` 都按只读成功请求处理：退出码为 0，并打印该命令组的
完整 usage。

本地 diff 评审、重放、比较与历史查询见
[M1 本地纵向链路](docs/roadmap/M1_LOCAL_VERTICAL_SLICE.md)；selection、overlay 和
scope 命令及边界见
[M1.1 统一本地目标](docs/roadmap/M1_1_UNIFIED_TARGETS.md)。实验性 Pi CLI 的命令、
安全边界和输出语义见 [Pi Review CLI](runtime/pi-review/README.md)。

## 文档入口

- [需求基线](REQUIREMENTS.md)
- [产品方向](PROJECT_DIRECTION.md)
- [技术设计](TECHNICAL_DESIGN.md)
- [核心协议](docs/contracts/core-protocols.md)
- [上下文边界](docs/architecture/context-map.md)
- [Hailix / Eino-Agent 集成](docs/integration/hailix-eino-agent.md)
- [指标、评测与收益](docs/product/metrics-evaluation-value.md)
- [当前状态](docs/roadmap/STATUS.md)
- [执行计划](docs/roadmap/PLAN.md)
- [未决事项](docs/roadmap/REMAINING.md)
- [M1 本地纵向链路](docs/roadmap/M1_LOCAL_VERTICAL_SLICE.md)
- [M1.1 统一本地目标](docs/roadmap/M1_1_UNIFIED_TARGETS.md)
- [MVP 验收矩阵](docs/roadmap/MVP_ACCEPTANCE_MATRIX.md)

## 当前边界

- 没有远端仓库和生产部署。
- 本地 scheduling repository 已实际执行四类独立 pool、global/class/tenant bounded queue、
  tenant active/queued quota、priority bias、fairness/aging 和显式 admitted/queued/rejected/throttled
  事实；`argus workload pressure` 与 authenticated `GET /v1/workloads/pressure?at=...` 从同一
  ledger 生成 strict `argus.workload_pressure_snapshot.v1alpha1`。快照固定 policy digest、
  sequence 和 observation time，按 global/class/tenant 对账容量与 stale work，且绝不在读取时
  隐式 reconcile。它仍是 single-process local authority，不是 Hailix 分布式资源或平台指标证明。
- 本地纵向链路已接受 exact commit-to-commit diff、单文件行 selection、
  selection overlay、multi-range/Go symbol selection 和 exact-revision scope；
  显式 `--context-provider repository_search`、`go_ast`、`go_dependencies` 与 compile-only
  `go_compile` 已能对 diff/selection/scope 从 exact Git revision 生成严格、内容寻址的
  repository lexical match、symbol/type/direct-call/bounded-call-path、module/package/import dependency 与 compile
  diagnostic ContextRef；它们记录 sensitive/file/search/parse/type/budget gap，且 formal Pi
  worker 已能消费同一 exact bytes。默认调用会先把 provider 写入本次
  ConfigBundle；已发布 lifecycle config 中的 `execution.context_providers` 也能驱动同一个
  bounded-concurrency exact-revision executor；并发上限由冻结配置
  `execution.context_provider_max_concurrency` 控制。每次成功或 Gap 都有
  request/config/target/context-bound 的本地执行
  receipt 与 latency，并由 ExecutionSnapshot 引用；replay 复用 receipt，不重跑 provider。
  `repository_search` 只搜索目标中的高区分度标识符，结果是 lexical evidence，不冒充
  CodeGraph/LSP semantic resolution；`.env`、credential/key/state 路径在 blob read 前拒绝。
  `go_ast` 的多跳 path 只使用同一 exact module 中从冻结源码递归 type-check 得到的
  `go_types_exact` 边并限制为三跳；interface/dynamic、module 外 external/name-match/unresolved
  仍保持 direct/partial evidence，不读取 module cache 或伪装成完整 call graph。
  `go_compile` 关闭 module network/cgo/ambient workspace，仅编译测试代码而不运行；embed、local replace、
  precompiled object 和不完整输入保守 unavailable。真实 test execution、CodeGraph/LSP adapter、
  多 attempt/recovery 与远端 attestation 尚未实现。
- formal deterministic registry 目前包含 marker baseline（授权目标内 Go 注释中的
  `TODO`、`FIXME`、`ARGUS_BUG`）和 Go AST discarded-context-cancel detector；后者对不可读
  内容或 parse failure 记录 coverage gap。它们用于验证配置、证据、重放和失败语义，
  不能外推为完整代码评审质量。
- `evaluation run record` 已把 active/approved/eligible EvaluationCase、exact label revision、
  exposure、TargetSnapshot 和已提交 formal GovernedReviewReport 闭合为 immutable EvaluationRun。
  当前提供确定性的 defect/category presence，以及结构化 localization 评分：Label 冻结
  path/side/line-range/source-digest，只有 digest-bound range overlap 才算命中；partial coverage 或
  无结构化 anchor 真值时 localization 显式 unavailable。`false_positive_regression` Label 还必须
  冻结排序唯一的 candidate cluster fingerprint；完整 GovernedReport 中对应 canonical Candidate
  实际出现后，rejected/confirmed/inconclusive 才分别计为 suppressed/escaped/inconclusive target。
  target 未出现或 coverage partial 时 filter efficacy 显式 unavailable，不能把“没有 Finding”解释成
  过滤成功。`fix_validation` 不再按 Finding 数量评分：binding 必须带 ApplyTrial，绑定 exact
  Finding/suggestion/snapshot/report/edit script，并读取、重算 dry-run/compile/test evidence artifact；
  conclusive trial 才产生 apply fidelity 和 fix verdict，partial 保持 inconclusive。当前 authority 明确为
  `local_host_unattested`，不是 Hailix/沙箱 attestation。它尚不负责批量
  调度 formal replay。`evaluation experiment record` 已能在两个 EvaluationRun case/label/evaluator
  完全一致时，逐 case 校验 variant 是 baseline 的 exact declared-variable replay，并记录
  improved/regressed/unchanged/inconclusive。每个 comparison 还会分别汇总两侧 committed
  ReviewRun 的全部 StageAttempt `duration_ms`，记录成对 latency delta，并明确标记 authority 为
  `review_run_stage_attempts`；它是本地主机执行台账，不是 provider/Hailix attestation。formal Pi
  的严格 `AgentExecutionReceiptCollection` 现已由 StageExecutionResult 贯通到 committed
  `ReviewRun.agent_execution_receipt_ref`；EvaluationRun 汇总 provider-reported/partial/unavailable
  receipt 数和 token counter，只有两侧全部为 provider-reported 时 Experiment 才输出 paired
  input/output/total token delta，authority 固定为 `worker_self_report_diagnostic`。真实账单 cost 仍保持
  unavailable，不能从预算 ceiling 或 worker receipt 推算。独立 `evaluation repeatability record/list/show`
  已能对同一 baseline 的 direct `variable=none` exact replays 计算 Finding-set pairwise Jaccard、exact-set/
  presence/anchor-hit 与 verdict-flip PPM；partial report 明确让 Finding-set stability unavailable，不能
  冒充稳定空结果。若 EvaluationRun 冻结了 dimension scope，结果还会按 exact dimension ref 分别保存
  Finding-set/verdict 稳定性，以及 Candidate/raw Candidate/context gap/failed task、累计耗时和 token 的
  样本区间；缺 execution/usage evidence 时对应比较显式 unavailable。ExperimentRun 自身的 instability
  字段仍不从非重复样本推断。comparison 同时保留 paired
  suppression/escape 整数差值和 paired Apply passed/failed check 差值；任一侧对应 metric unavailable
  时 paired delta 也 unavailable。
  `dashboard rebuild` 现已通过独立 anti-corruption adapter 读取受治理 Evaluation/Experiment ledger，
  按 scope/window 生成 baseline/variant 的 input/output/total token ExperimentFact 和 exact delta tile；
  partial usage 在看板保持 unknown，且该路径不生成 cost fact。
  RepeatabilityRun 使用独立 `RepeatabilityFact` 投影 Finding-set/anchor/verdict stability 以及逐 dimension
  Candidate/context-gap/task-failure/duration/token range；它不会伪装成 Experiment quality delta。
  dashboard、canonical JSON/CSV 和 repeatability typed Parquet 表都保留 repeatability/case/review_dimension 与
  source refs。旧 projection snapshot 需用新 snapshot ID 重建。
  Evaluation binding 还可显式声明 exact `dimension_scope`；每维只从 committed Candidate/Finding 和
  可回连的 review/verification receipt 计算 execution coverage、verdict、localization、turn/tool、
  cumulative task duration 与 attributable token，共享 context 不强行分摊。Experiment 按相同 dimension
  ID 保留两侧 exact revision 并生成差值，Dashboard 以受控 `review_dimension` 输出独立 token/duration
  facts。新 formal Pi run 已将 exact raw candidate collection 纳入 terminal ReviewRun，Evaluation 复算
  retained/merged-duplicate/rejected-invalid/excluded-budget，Experiment 在两侧证据都可用时生成 paired
  integer delta；旧 run 继续显式 unavailable。committed coverage 与 task/group receipt 还会生成逐维
  context-gap count/delta，Dashboard 已消费 duplicate/invalid/budget/context-gap 整数 facts，并按
  `sum(fate)/sum(raw)`、scale=6 生成 duplicate/invalid/budget rate；零分母或缺 closure 时保持 partial。
  `evaluation batch run` 已提供第一条 durable orchestration：
  intent 先落账、逐 case exposure 在 provider 前落账、最多 16 路受控并发、稳定 replay 幂等键、
  exact change-set/config digest revalidation，以及 EvaluationRun/ExperimentRun/Batch terminal 自动闭合；
  每个完成 case 在 host revalidation 后独立 checkpoint，checkpoint 后失败的重试只执行缺失 case，
  terminal 后重试不再调用 executor。批次执行还使用 sequence-CAS 的 durable claim、heartbeat、
	  generation/fencing token：并发 worker 不能重复调度，lease 过期后新 worker 可接管，旧 token
	  不能 checkpoint 或 terminal。CLI 在 intent 前把 exact runtime/component bytes、非 secret options、
	  pricing ceiling 和单变量参数冻结为 content-addressed executor template；持久化 request 必须绑定该
	  ArtifactRef，`evaluation batch resume` 从模板恢复，文件漂移或 ref 不一致会在 provider 前失败。
	  API key 只在执行时从环境读取，不进入模板。当前 CLI adapter 支持 formal replay 已开放的
	  budget/model/prompt/skill_pack/knowledge_pack/rule_pack/workflow/index/filter_policy variant；旧
	  `finding_governance` 变量仅保留历史事实与调用兼容。
	  `evaluation repeatability batch run/resume/show` 复用同一 durable 协议执行预先冻结的 sample×case 矩阵，
  只接受 direct exact replay；每个样本生成独立 EvaluationRun，最终自动闭合 RepeatabilityRun，恢复时
  只补缺失的 sample×case checkpoint。该 runner 是 Argus 评测编排，不复制 Hailix Worker Runtime。
- Pi runtime 已接到 Go 本地 shadow evidence 链路；只接受已提交 ReviewRun 的冻结
  ReviewInput，worker 运行期间不读取 Git/working tree，且不写正式 Finding、Feedback、
  Value 或 promotion ledger。standalone Pi CLI 仍保留用于本地能力实验。
- formal Agent Stage 已完成受治理的配置解析、exact component closure、Plan admission、
	durable request/dispatch/binding、callback receipt、result/cancel terminal gate 与 hypothesis
	evidence ledger。`ConfigResolutionReceipt` 是本地治理记录，不是签名或远端
	attestation；其可信性来自同一次 `GovernedConfigProvider` 调用背后的配置 lifecycle
	repository，而不是 receipt 自证。入口还要求 host 注入独立 `AgentPlanningSubject` 并闭合
	tenant/organization/workspace/repository；该内存投影本身不是 Hailix/IAM attestation。
	本地 Pi PlatformPort、bootstrap/run/show CLI、成功/失败终态 E2E 和终态幂等恢复已接通；
	正式准入的 Hypothesis 会生成独立 Candidate/Finding/Decision 报告 artifact，但 Finding
	只会 `queued_for_human`，不会自动发布。formal attempt/binding/evidence/report 已由仓储层
	重新验证完整闭包并提交为统一 terminal `ReviewRun`，通用 history/show 不再依赖 formal
	旁路查询。Hailix PlatformPort consumer adapter 已接到显式 `hailix-http` CLI backend，并以固定环境
	变量按请求解析 credential；尚无匹配的 Hailix 公共 server endpoint、远端
		credential broker、runtime attestation、通用 current-cancel service；formal execution
		的本地主机 runtime-file manifest 只证明本机观测字节，不冒充远端 attestation。实际 prompt/
		context/tool transcript 已作为 bounded `exact_local_sensitive` Artifact 进入 shadow 和 formal
		closure，并拒绝普通 read/export；两条路径都有 purpose-bound audited read 与 logical revoke，
		formal ref/scope 由 committed terminal whole-closure 推导。formal PlatformPort 还会解析
		Plan 绑定的 governed prompt、review-skill、exact sealed RulePack，以及 ReviewInput 引用的 frozen context Artifact；
		worker 双侧校验 ref/artifact/bytes 后由 Pi context collector 实际消费。context evidence 不扩大
		ReviewInput region/patch anchor，也不能授予工具、凭据或副作用；prompt/skill 在 snapshot/task evidence
		中回显相同 identity。但内容仍是 worker self-report，且
		缺 reference-aware physical GC；formal 已支持 exact 与 budget/model/prompt/skill_pack/knowledge_pack/rule_pack/workflow/index/filter_policy-only variant replay，
		并支持由不可变 AgentStagePlan 约束的有界 multi-attempt retry；远程发布仍不支持，deterministic workflow policy replay 是独立路径。
- 本地 v1alpha3 `agent_execution.*` facts/projection 只表示 host-observed execution 状态与
  worker-self-reported 任务/工具/usage 诊断事实。failed/canceled/unknown-outcome 没有
  result receipt 时不会伪造 task、tool 或 token 事实；它们都不是 Hailix attestation、
  账单、成本、ROI 或模型质量证明。单次 rebuild 的 current projection 中，每个 execution
  只按当时 Attempt 的 `observed_at` 归属一个半开窗口；没有 execution intent 的 legacy
  direct Import 仍按其 observation 计入，有 attempt 但当前归属其他窗口的 import 会被过滤。
  reconcile 会把 current 状态从旧 unknown 观察移动到新的 terminal 观察；既有 immutable
  snapshot 不会被改写，因此不同时间构建的 snapshot 不能直接跨窗口求和。稳定事件历史仍
  需要 immutable transition facts/as-of 语义。duration p50/p95 只统计带
  manifest 的 worker-report 样本。旧版 diagnostic snapshot/manifest 会被显式拒绝，
  重建时必须使用新的 v1alpha3 snapshot ID，不能让同一版本工件静默改变语义。
- 正式本地 dashboard facts 已包含 `context_providers`：从严格执行回执投影 Provider
  attempt/succeeded/gap/reuse、typed gap reason 和 duration p50/p95。Replay reuse 不会重复
  计为执行或延迟样本；所有 tile 都明确标记 `local_host_observation` 不是平台 attestation，
  facts 可随其余六类表一起导出 JSON/CSV/Parquet。
- intent 已落盘但 host 在 timeout、协议解码、binding、mapping、import 或 completion
  阶段失败且没有合法 completion 时，会保留 immutable、4 KiB 上限、固定 taxonomy 的
  脱敏诊断 observation；它只解释 `unknown_outcome`，不伪造 terminal completion。若 exact
  LocalBridge pre-authorized exact committed import 已存在，可显式 reconcile；否则仍禁止自动
  重跑。成功
  completion 的 host `recorded_at` 还必须不早于 import `accepted_at`，回拨时钟失败关闭。
- Hailix production transport/wiring、Eino-Agent production adapter、数据库、Web/API、hosted dashboard 和
  分布式/真实流量 evaluation executor 尚未实现；本地 durable formal replay batch、EvaluationRun/
  ExperimentRun、formal dashboard CLI 与隔离的 AgentExecution diagnostic projection 已存在。
- 核心 `pkg/contracts/v1alpha1` 契约仍允许不兼容调整，但调整必须更新样例、测试和变更
  说明；execution history 与 AgentExecution diagnostic analytics 已独立升级为 v1alpha3。
- 旧 Smart CR 资料只作为问题与失败模式证据；不会复制内部链接、凭据、专有
  标识或未经验证的历史实现。
