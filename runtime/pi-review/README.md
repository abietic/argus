# Argus Pi Review CLI

这是 Argus 当前优先验证评审效果的本地 runtime。它直接嵌入
`@earendil-works/pi-agent-core`，使用独立 Pi Agent 完成上下文收集、多维评审和
候选缺陷验证；既可作为 standalone CLI 运行，也可由 Go host 通过严格 stdio worker
协议执行。两种模式都暂不依赖 Hailix、ACP、数据库或 Web 平台。

## 运行

```bash
cd runtime/pi-review
npm ci

# 官方 Anthropic
export ANTHROPIC_API_KEY='...'

# 或 DeepSeek 的 Anthropic-compatible profile
export ARGUS_PROVIDER_PROFILE='deepseek-anthropic-env'
export ANTHROPIC_BASE_URL='https://api.deepseek.com/anthropic'
export ANTHROPIC_API_KEY='...'
export ANTHROPIC_MODEL='deepseek-v4-pro[1m]'

# 当前工作区（含未跟踪文本文件）
npm --silent run review -- changes --repo /absolute/repository

# 精确提交差异
npm --silent run review -- commits \
  --repo /absolute/repository \
  --base <base-revision> \
  --head <head-revision>

# 显式完整文件
npm --silent run review -- files \
  --repo /absolute/repository \
  -- src/order.go internal/store.go
```

先检查目标和分组而不调用模型：

```bash
npm --silent run review -- changes --repo /absolute/repository --plan
```

JSON 输出与 CI 阈值：

```bash
npm --silent run review -- changes \
  --repo /absolute/repository \
  --format json \
  --fail-on high
```

默认使用 `anthropic-official` profile 和固定模型 `claude-sonnet-4-6`。可通过
`--provider-profile deepseek-anthropic-env` 使用 DeepSeek Anthropic-compatible API；
该 profile 从 `ANTHROPIC_BASE_URL`、`ANTHROPIC_API_KEY` 和 `ANTHROPIC_MODEL` 解析
endpoint、凭据和模型，也可用 `--model`/`ARGUS_MODEL` 覆盖模型。

DeepSeek profile 只接受 HTTPS、精确的 `api.deepseek.com/anthropic` endpoint，拒绝
userinfo、query、fragment 和 HTTP redirect；Pi 的 ambient credential resolver 被关闭，
每次请求只注入显式解析的 `ANTHROPIC_API_KEY`。CLI 没有 `--api-key` 参数，凭据不会
进入 prompt、报告或 Git/CodeGraph 子进程环境。`ANTHROPIC_AUTH_TOKEN` 即使存在也不
参与该 profile 的认证。

## Go frozen-input worker

根目录 `argus agent-review run` 不让 TypeScript worker 自己捕获仓库。Go host 只从
同一 local store 回读一个已提交且成功的 ReviewRun，加载并校验其 immutable
ExecutionSnapshot/ReviewInput，然后通过 stdin 发送一份有界、严格的 work item：

```text
Go committed ReviewRun + ReviewInput + exact frozen ContextRef Artifacts
  -> AgentReviewPlan + attempt/generation/fence/capability binding
  -> stdin: exactly one frozen ReviewInput work item
     + prompt_bundle/review_skills/knowledge_packs/context_artifacts exact bytes
  -> Pi context / multi-skill review / independent verifier
  -> stdout: exactly one bounded result; stderr: bounded progress
  -> Go strict decode + target/group/patch/evidence/counter recomputation
  -> succeeded: immutable shadow artifacts + append-only observation
  -> failed/canceled: redacted immutable completion
  -> no visible completion: unknown-outcome attempt
```

worker 仅使用冻结在内存中的 `list_files`、`read_file`、`search_code`；不调用 Git、
CodeGraph、shell 或目标仓库配置。Node executable 与 worker entrypoint 必须是 absolute
regular non-symlink file；Go host 将 `dist/*.js`、`package.json`、`package-lock.json`
的有界排序 manifest 绑定到 execution identity。`node_modules` 实体内容仍没有
attestation，因此整个模式固定为 `non_attested`。

```bash
cd /absolute/argus
make pi-build
make build

export ANTHROPIC_BASE_URL='https://api.deepseek.com/anthropic'
export ANTHROPIC_API_KEY='...'
export ANTHROPIC_MODEL='your-deepseek-model-id'

./build/argus agent-review run \
  --store /absolute/argus-store \
  --source-run <committed-succeeded-review-run> \
  --idempotency-key <stable-execution-key> \
  --node /absolute/regular/non-symlink/node \
  --worker-script /absolute/argus/runtime/pi-review/dist/worker.js \
  --provider-profile deepseek-anthropic-env \
  --model "$ANTHROPIC_MODEL"
```

同一 idempotency key + 相同语义只复用已提交结果；不同语义冲突。若执行 intent 已写入
但无法确认 worker 结果，host 返回 unknown outcome，绝不自动重跑可能已发生的 provider
调用。worker 失败消息在落盘和回显前固定脱敏；partial 默认返回非零，只有显式
`--allow-partial` 才放宽进程退出语义。

execution completion 当前为 v1alpha2，`ExecutionAttempt` 为 v1alpha3。worker 的非 attested
`completed_at` 和 host-owned `recorded_at` 是不同事实：succeeded 不得越过 deadline；
failed/canceled 可以在取消收敛后完成，但所有 terminal `completed_at` 都不得晚于
`recorded_at`。terminal Attempt 的 `ObservedAt` 只使用 `recorded_at` 作为查询与
analytics 窗口归属时间。没有 completion 时，若存在 host-failure observation，则使用该
host-owned `observed_at`；否则使用 intent `accepted_at`。

intent 接受后，无论 worker 返回 failed/canceled、子进程异常，还是协议、mapping/import
失败，Go bridge 都会在不覆盖原错误的前提下返回当前 `ExecutionAttempt`。可以用以下命令
按 host-observed 的 UTC 半开时间窗口查询四类状态；只有 `succeeded` attempt 才能引用
shadow result manifest：

```bash
./build/argus agent-review execution list \
  --store /absolute/argus-store \
  --start 2026-08-23T00:00:00Z \
  --end 2026-08-24T00:00:00Z
./build/argus agent-review execution show \
  --store /absolute/argus-store \
  --execution <agent-review-execution-id>
./build/argus agent-review execution reconcile \
  --store /absolute/argus-store \
  --execution <agent-review-execution-id>
```

若 shadow import 已 commit、但随后 completion 写入失败，错误 JSON 还会携带
`unconfirmed_committed_result`，plain 输出为 `unconfirmed_manifest=<id>`；两种输出都显式
标记 `outcome_acknowledged=false`。它可以通过 `agent-review show --manifest-id <id>` 独立
检查，但只是 crash-window 中的已提交引用。极端情况下 completion 已 rename 可见、目录
fsync acknowledgement 随后失败，严格 Attempt 投影可能已经是 succeeded；调用方仍必须以
命令成功且 `outcome_acknowledged=true` 作为本次操作确认，不能只看状态。host 会为 runner、
协议、binding、mapping、encoding、import 或 completion 错误保存固定 taxonomy、4 KiB 上限且不含 raw
error/stderr/URL/credential 的 immutable observation；它只诊断 `unknown_outcome`。LocalBridge
会在 import 前持久化 immutable authorization，绑定 exact canonical plan/hypothesis/receipt、
intent/runtime digests、combined input digest 和 expected manifest。reconcile 只会在完整重验
这份 authorization、ImportRecord 和 exact committed result 后补写 succeeded completion；
legacy/direct Import 没有这份证明，authorization 存在但 import 未 committed 时仍保持 unknown，
且 completion host time 不得早于 import commit。整个过程不会调用 worker/provider。

`argus agent-review analytics rebuild/show/export` 可从 committed shadow imports 和
execution attempts 构建 v1alpha3 不可变 `agent_execution.*` 诊断快照并导出 JSON/CSV；
facts/projection/snapshot/manifest/metric/policy/export 均使用 v1alpha3。成功态保留
完整 task/tool/usage facts；failed/canceled/unknown-outcome 只生成 host-observed execution
fact，不伪造不存在的 receipt 或 token usage。它不进入正式 Finding funnel，也不提供
provider bill、成本或 ROI 推断。在单次 current-projection rebuild 内，每个 execution 只按
当时 Attempt 的 host-observed `ObservedAt` 归属一个半开窗口；没有 execution intent 的
legacy direct Import 仍按 observation 保留，有 attempt 但当前属于其他窗口的 import 会被
过滤。reconcile 后 current 状态会移动到 terminal 窗口，但既有 immutable snapshot 保留
先前 unknown；跨不同构建时点的 snapshot 不能直接求和，稳定历史还需要 immutable
transition facts/as-of 语义。duration
p50/p95 只使用带 manifest 的 worker-report 样本；attempt-aware rebuild 使用单批 execution
lookup，不再为每个 result 重扫 intent store。旧版 projection snapshot/manifest 会被显式
拒绝，重建时必须改用新的 v1alpha3 snapshot ID。export 目录必须位于 Argus
store 外；磁盘 manifest 会绑定 snapshot
ID/SHA-256、scope、target 和 dataset manifest，相同内容可幂等重试，已有不同内容不会
被覆盖。

命令组的嵌套 `-h`/`--help` 都以 0 退出并输出完整 command-group usage，便于脚本做能力
探测。

默认还会在首次模型调用前执行硬准入：最多 32 个文件、4 MiB 冻结源码、8 个分组、
32 个 retained normalized Candidate、全局 96 次 provider/model turn，每个 turn 最多
8192 个输出
token。每次
进入 Pi `streamFn` 都会原子扣减同一个全局 turn 预算，provider 内部重试固定关闭，
因此工具往返和透明重试都不能绕过。可用对应的
`--max-*` 参数显式调整；建议先运行 `--plan --format json` 检查目标、跳过项、分组和
Agent task/模型 turn 最坏调用数。准入还会为全部 retained Candidate 预留一次独立
verification task 的最低 turn，避免 review 完成后才因 verifier 无额度而降级。单个文件超过分组字节上限时会作为 coverage gap
跳过，不会静默突破预算。

## 当前工作流

```text
capture target
  -> deterministic directory/language groups
  -> per-group context collector
  -> correctness / concurrency-data / error-contract reviewers
  -> resource-lifecycle / security-contract / transaction-state reviewers
  -> preserve raw candidates + explicit normalize/deduplicate decisions
  -> independent verifier per candidate
  -> confirmed findings + all candidate verdicts
```

Reviewer 和 verifier 都必须通过 typed terminal tool 返回结构化结果。Candidate 的
anchor 必须覆盖变更行；Candidate 和 confirmed verdict 至少包含一条可回读的
`path + side + line range + exact excerpt` 证据。证据最多覆盖 20 行，excerpt 必须与
冻结源码的完整引用范围精确一致，不能用单字符或局部 substring 绕过。原始候选、
invalid/retained/merged-duplicate/excluded-budget 决策和 survivor lineage 都保留；
预算外 raw claim 不进入 verifier，但不会被静默丢弃。同模型的两次意见
不能仅靠相互同意成为 Finding。

默认六个 builtin Skill 按职责切分：correctness 关注计算与控制流，concurrency-data
关注并发和共享内存，error-contract 关注失败与 unknown outcome，resource-lifecycle
关注所有权和释放，security-contract 关注信任/权限/外部契约，transaction-state
关注持久化原子性和状态机不变量。Skill Markdown 内的 `Revision:` 元数据和正文
SHA-256 共同进入 snapshot；standalone/frozen/formal 三条路径都拒绝 revision 或 bytes
替换。worker/Plan 最多接收 16 个 review dimensions，默认六个之外仍可容纳受治理的
仓库和业务线 Skill。

每次运行还输出 diagnostic-only 的 Pi execution snapshot 与逐 Agent task receipt：
目标、分组、Skill/Knowledge、prompt bundle、runtime/SDK、provider profile、预算、
turn/tool/token usage、耗时和稳定错误码均有 digest 或结构化字段。task evidence 还可在
有界预算内保存 exact system/user prompt、structured output 和 typed tool arguments/results；
它是 `exact_local_sensitive/worker_self_report/diagnostic_only`，不含 assistant reasoning，
不能冒充 provider transcript。formal worker 的 prompt bundle、按 Plan 排序的 review skills 与
governed knowledge packs
均由 host 以内联 Artifact binding 传入，worker strict decode 并复算 digest/size/revision 后才
构造 runtime；review skill/knowledge 不再按 ID 从本地文件二次加载。prompt 用于
context/review/verification 与 terminal finalizer，skill 只定义缺陷发现标准；代码所有的只读、
凭据和 untrusted-repository 安全基线始终额外包裹，skill/knowledge 不能扩大权限、读取凭据、
产生副作用或修改输出契约。knowledge 会以明确的 untrusted reference section 注入
context/review/verifier，并在 execution snapshot 回显 exact ID/digest/bytes。
当前 snapshot 仍标记 `non_replayable`，不能冒充 Hailix Trace、平台 attestation、billing truth
或正式 Finding 证据。

standalone CLI 只向 Agent 暴露以下只读工具：

- bounded `read_file`；
- literal `search_code`；
- bounded `list_files`；
- 仓库已经存在 `.codegraph/` 时的 `query_codegraph`。

严格 worker 模式只暴露前三项的 frozen in-memory 实现，不暴露 CodeGraph。

不会自动读取或执行目标仓库中的 `.pi/`、skills、hooks、MCP、插件或 Agent 配置，
也不提供 shell、write、edit、测试和编译能力。额外 Skill/Knowledge 必须通过
`--skill <file>` / `--knowledge <file>` 显式传入，并且只作为文本数据加载。

## 开发门禁

```bash
npm run format
npm run check
npm test
npm run build

# 根目录：build worker 并做真实 Go <-> TypeScript 协议往返，不调用 provider
make pi-smoke
```

测试使用 fake Agent，不调用远程模型。真实模型 smoke test 需要显式设置环境变量并
手工运行 CLI。存在 Candidate 且使用 `--no-verify` 时结果为 `partial`；任何
`partial`/`failed` 默认返回非零，只有调用者显式传入
`--allow-partial`，不完整覆盖才可能返回 0。fake-provider contract 不能替代真实模型
质量验收；当前验收事实见 [项目状态](../../docs/roadmap/STATUS.md)。
