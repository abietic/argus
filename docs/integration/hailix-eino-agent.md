# Hailix / Eino-Agent Integration

**Hailix evidence date:** 2026-08-26
**Eino-Agent evidence date:** 2026-07-26; current `../eino-agent` has no source checkout
**Status:** Argus HTTP consumer、正式 CLI composition 与 formal recovery harness 已实现；Hailix public server externally blocked

## 1. 推荐拓扑

```text
Argus ReviewWorkflow
  -> Hailix Task/Worker API
     -> Hailix RuntimeSupervisor + ACP client
        -> Eino-Agent ACP server
     -> Hailix Trace/Artifact/Event/LLMProxy
  <- normalized StageResult + immutable refs
```

Argus 不直接管理生产 ACP process。为本地测试可以提供 fake/loopback adapter，但
正式路径由 Hailix 负责身份、沙箱、认证、取消、恢复和 Trace。Argus 冻结领域预算
策略；Hailix 只报告并执行其当前实际支持的资源/Turn deadline capability。

## 2. Hailix 当前可复用能力

源码已验证存在：

- strict `TaskSpec` 与 digest/admission；
- Workspace/WorkingCopy、WorkerSession/WorkerRun/AgentTurn identity；
- `internal/localproduct/app.go` 的 production composition 已把 Gateway、Frontier、
  Artifact content、Outbox 和可选 Worker Runtime 接到同一个本地产品入口；
- `StartWorkerRun` 在 Hailix application/PostgreSQL 内部以 permit、session revision、
  runtime generation 和事务性 Task/WorkerRun/AgentTurn/Runtime/Outbox 写入启动；这不是
  vertical app 可直接调用的公共 API；
- production Codex ACP client、session/update/exact active-turn cancel、进程组回收和
  runtime startup recovery；
- Trace JSONL、TraceManifest、ArtifactRef、checksum 和 retention metadata/ref；
- 内部 IntegrationEvent/Outbox/Projection；
- LiteLLM/Langfuse、cost/trace metadata；
- PostgreSQL TaskSaga 和 async Worker lifecycle。

这些能力应 `preserve/adapt`，不在 Argus 复制。

当前外部 `POST /api/v1/tasks` 接收的是 Hailix 收窄的 DirectTaskInput，由服务端
生成 membership/readiness/creation intent 和 TaskSpec。Argus 只能调用该 admission
入口，不能自行拼内部 TaskSpec。该入口不能绑定 immutable TargetSnapshot、
Stage input Artifact/schema/checksum，因此只能做 loopback smoke，不能作为正式
Argus stage execution contract。

生产源码同时确认，WorkerControl gRPC、`StartWorkerRun` 和 settlement checkpoint 都是
Hailix 内部 runtime 协议。Argus 不能为了绕开公共 API 缺口而直接调用这些内部入口。
当前公共 Artifact content 只按授权 Artifact ID 返回 report/patch bytes；
TraceManifest 元数据虽已持久化，但没有与 Argus stage contract 对齐的公共查询协议。

## 3. Hailix 当前接入缺口

当前源码同时表明：

- TaskSpec 的生产校验只接受 `agent.adapter = codex-acp`；
- ArtifactRef 类型只允许 `report` 和 `patch`；
- 当前 ACP client 的 permission handler 默认返回 cancelled；
- terminal client capability 未实现，ACP 初始化仍声明可写文件，不能充当 review-safe
  只读 capability；
- TaskSpec 没有稳定的领域 payload/typed stage output 扩展契约；
- 当前仅支持 loopback single-user local profile；
- Frontier 提供 projection snapshot/SSE，不是原始 EventCenter subscription；
- LiteLLM 是 Worker/Assistant 嵌入组件，不是共享模型 API；
- 配置 revision、通用 replay/experiment/evaluation 尚未形成稳定对外服务。

因此 Argus M0 只能定义 anti-corruption contract，不能把“已有 ACP/Trace”推断为
Eino-Agent 和 Argus 已经即插即用。

### 3.1 Argus 已实现的 consumer adapter

`internal/hailixexecution` 已实现一个不依赖 Hailix `internal` package 的
anti-corruption adapter，并以当前 Argus 正式执行状态机的真实端口作为验收面：

- `AgentExecutorCapabilityResolver`：把 exact `AgentStagePlan` 交给平台解析实际能力，
  严格校验 subject、plan digest、capability self digest 和 pinned verifier；
- `PlatformPort`：canonical `StageExecutionRequest` 的 exact-idempotent
  Ensure/Lookup/Cancel/Await；每个 lifecycle command 都携带完整 tenant/organization/workspace/
  repository Subject，Ensure receipt 必须 exact 回显，adapter 拒绝 request/receipt、跨仓 subject 和
  fencing substitution；
- `FormalAgentStageExecutor`：只接受与 handle 完全一致的 canonical typed terminal
  result 和非空 callback proof；
- `AgentStageResultCallbackVerifier`：把 durable binding、canonical result 和 proof
  digest 交给平台验证，并要求返回 pinned verifier 的 exact receipt。

`HTTPClient` 已将 client contract 冻结为
[`hailix.platform_execution_http.v1alpha1`](../contracts/hailix-platform-execution-http-v1alpha1.md)：
六个 POST operation、严格 nested canonical JSON、HTTPS/literal-loopback、no redirect、4 MiB
response bound、request-time bearer credential 和 stable error taxonomy。focused consumer tests 覆盖
unknown Ensure outcome 后 exact Lookup/retry、跨 subject command/receipt 拒绝、receipt/result
substitution、terminal cancel、callback receipt/trust-root drift，以及 unsafe endpoint/redirect/
unknown field 拒绝。formal composition 的真实 HTTP E2E 进一步证明第一次 ensure 在 provider 已创建
后返回 503，第二次 invocation 从 durable claim 重发 byte-identical request、provider start 仍为 1，
最终完成 typed result、callback admission 和 ReviewRun；第三次 terminal retry 不访问远端。

正式 `agent-review formal run/replay` CLI 现可显式选择
`--execution-backend hailix-http`，并要求配置 clean HTTPS/literal-loopback base URL 与 capability/callback
pinned verifier 的 ID、revision、SHA-256。Bearer token 与 credential revision 不接受 CLI 参数，固定由
`ARGUS_HAILIX_BEARER_TOKEN`、`ARGUS_HAILIX_CREDENTIAL_REVISION` 在每次 HTTP 请求时重新解析；缺失凭据在
capability preflight 前失败关闭。CLI production composition E2E 已证明该路径不启动本地 Pi runner。
这只使 Argus consumer 可由 operator 调用，不代表 Hailix 已实现 server endpoint；远端还必须能够解析
StageExecutionRequest 中的 exact Artifact binding，不能通过共享本地磁盘形成隐式协议。

Experiment/Repeatability durable batch 使用同一 production composition。content-addressed executor
template 冻结 `hailix-http` base URL 与两组 pinned verifier refs，但不保存 bearer token 或 credential
revision；CLI batch 复用 formal replay flags，local API 通过独立 `formal-batch-*` server profile 配置，
因此普通 formal ReviewJob 不会被隐式切换。batch resume 只从模板恢复 transport，实际 credential 仍在
每次 HTTP 请求时从固定环境读取。

该 adapter 没有接 `DirectTaskInput`；当前 Hailix 也尚未实现这六个 public operation、artifact fetch/
delivery 和 attestation。因此它是 production adapter 的 Argus 半边与 consumer contract，不是三仓
已联通证据。

## 4. Eino-Agent 当前 ACP 能力

本节来自 2026-07-26 的已验证源码快照。2026-08-25 检查时，约定的相邻
`../eino-agent` 目录只包含 `.yhc` 会话数据，没有 `AGENTS.md`、`go.mod` 或源码；在
canonical checkout 恢复前，以下结论不得升级为当前事实，也无法运行三仓 contract test。

源码已验证：

- ACP Agent 支持 session、prompt stream、list/load/resume 等生命周期；
- cancel 绑定当前 prompt context，不调用 engine-wide stop 误伤下一 turn；
- tool/plan permission 可投影到 ACP；
- command discovery 有 ACP entrypoint capability filter；
- provider runtime 支持可替换模型路由。

正式接入仍需 capability handshake 和 contract test，不能依赖私有 CLI 命令或
注册但不可达的 command。

当前不能假设存在：

- ACP-native review workflow、DAG 或 `/review` RPC；
- schema-constrained review output；
- durable trace replay/cursor/sequence；
- hard filesystem containment；
- 完整权威 `/diff`；
- background subagent 随 parent cancel；
- permission request 携带完整 tool input；
- 精确表达所有失败的 stop reason。

## 5. 不可信仓库配置风险

这是 Argus 的硬门禁，不是普通加固项。

当前 Eino-Agent 在 ACP `NewSession`/engine 构造期间会读取目标 CWD 的项目配置，
包括 hooks、MCP、skills/plugins。项目 `UserPromptSubmit` hook 可在首个 prompt 前
通过 shell 执行；MCP stdio server 可在 session 初始化/connect 阶段启动。
`--tools` 只裁剪 builtin tools，不能关闭 MCP，simple mode 也不能构成隔离。

结论：

- 不能把任意 PR checkout 直接作为 Eino-Agent CWD；
- 不能把“只读 builtins”当作 review sandbox；
- 在该问题解决前，不得给 runtime 注入源码之外的 secret、内网或公共网络；
- repository 内 `.claude/`、`.mcp.json`、skills/plugins 等都是待评审源码，不是
  可信运行配置。

### 5.1 P0 Review Safe Profile

以下两层必须同时满足，否则 Eino-Agent adapter 保持 disabled：

1. Eino-Agent 提供可验证的 `review-safe` profile，在保留源码可读的同时禁用
   project/user hooks、MCP auto-load、plugins、skills、background/subagent、
   mutation tools 和 project-controlled model/provider config；并且
2. Hailix 在外部建立无密钥、无网络、只读源码、受限进程/资源的隔离层，并用
   platform-owned config 启动经过 allowlist 的 ACP binary。

运行时 capability snapshot 必须证明这些禁用项，而不是只依赖启动参数字符串。
当前 Hailix 的 `trusted_full_network_v1` profile 不满足该门禁。

### 5.2 Source 与 Runtime CWD 分离

长期推荐：

- 原始仓库作为只读 source snapshot；
- runtime 使用 platform-owned clean CWD/config；
- review tool 通过受控文件端口读取 source；
- 若需评审仓库内 agent 配置文件，把它们当普通 bytes 暴露，禁止 loader 执行；
- Apply 在独立授权 WorkingCopy 中进行，不复用 review runtime。

## 6. 当前 P0 API 适配

在 Hailix 扩展前，以下路径只允许做非正式 smoke：

1. Argus 创建自己的 StageRun/AnalysisJob；
2. 调用 Hailix DirectTaskInput API；
3. 保存 `stage_run_id -> task/view_ref`；
4. 通过 Frontier snapshot/SSE 跟踪；
5. 通过受支持 content API 读取 Artifact；当前没有稳定公共 TraceManifest/Trace
   content API，不能声称获得完整 trace；
6. 不读取 Hailix 数据库，不直接消费/写入 Outbox；
7. 限定 loopback single-user profile。

当前 Hailix 仍是 Codex-only，所以上述路径不能直接选择 Eino-Agent；需要先完成
provider-neutral adapter 或做完全隔离的 direct ACP spike。

DirectTaskInput smoke 不得进入正式 M2 验收、gold dataset 或 promotion。正式接入前
必须先提供 `CreatePlatformExecution` 等价能力，把 TargetSnapshot、stage input
Artifact/schema/checksum、Workflow/Config refs 和 effective capability snapshot
不可变绑定到 execution identity。

## 7. 目标跨仓契约

Hailix 需要实现 Argus 已冻结的 HTTP contract，并进一步提供：

1. exact `EnsureExecution/LookupExecution`：把 platform execution identity 与 immutable Artifact
   inputs、schema/checksum 和 opaque caller correlation 原子绑定；
2. `GetCapabilitySnapshot`：adapter、ACP、tools、safe profile、network/data policy，并
   返回可由调用方 pinned trust root 验证的 receipt；
3. `StreamExecutionUpdates`：typed progress/evidence/terminal；
4. `CancelExecution`：取消 exact platform execution/attempt；ReviewRun 级联取消和
   future scheduling fence 由 Argus workflow controller 管理；
5. `GetTraceManifest` / `GetArtifactRef`；
6. IntegrationEvent subscription with sequence/cursor。
7. `VerifyResultCallback` 等价能力或可离线验证的签名/attestation，使 Argus 能把 typed
   terminal result 与 exact Task/WorkerRun/AgentTurn 决策闭合，而不是信任 echoed JSON。

ReplayRun 由 Argus 创建新 namespace 并固定 remote side effects deny，然后调用同一个
`CreatePlatformExecution`；Hailix 不需要理解 code-review replay 语义。

Argus 提供：

- ReviewSpec/ExecutionSnapshot/StageExecutionRequest；
- stage input/output schema；
- Finding/Decision/Feedback domain events；
- required capability policy 和 contract fixtures；
- evaluation labels 与 domain metrics。

## 8. 共享平台能力归属

已经有两个真实消费者或明显属于执行平台的能力应在 Hailix 内建设：

| 通用能力 | 原因 |
|---|---|
| immutable platform execution input/ref | 所有 Agent task 都需要复现和 lineage |
| typed Artifact registry | 不应让每个 vertical app 冒充 report/patch |
| Event relay/subscription | vertical app 不能读取内部 Outbox |
| provider-neutral ACP/runtime profile | Hailix 不应固化 Codex |
| platform model invocation | 凭证、预算、限流和脱敏属于平台 |

Finding、RulePack、review target、comment publication 和 code-review value attribution
必须留在 Argus。Config/Replay/Evaluation/Metric 先由 Argus 验证领域语义；只有
Hailix 自身出现第二个具体需求后再提取通用原语。共享平台是服务与契约，不是复制
Go struct 到两个仓库。

## 9. 分阶段集成

1. Argus 使用 deterministic fake 完成 M1 数据闭环。
2. 在 Eino-Agent 增加并验证 review-safe profile。
3. Hailix 增加 provider-neutral ACP adapter profile 与 typed Artifact。
4. 用 contract test 跑单文件/小 diff，无网络无写入。
5. 引入 Trace/LLMProxy 和 stage replay。
6. shadow review 三个本地仓库。
7. 质量与安全门禁通过后才启用代码平台写回。
