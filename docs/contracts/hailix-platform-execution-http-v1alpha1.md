# Hailix Platform Execution HTTP Consumer Contract v1alpha1

本契约定义 Argus 正式 Agent Stage 对 Hailix 公共执行服务的最小需求。它是
consumer-driven contract，不表示当前 Hailix `DirectTaskInput` 已实现这些能力。
Hailix 服务端联调时必须逐项通过 Argus contract tests；不得通过读取 Hailix 私有数据库、
Outbox 或 `internal` package 适配。

## 1. 边界与版本

- Base URL 只能是 HTTPS；本地 contract test 允许 literal loopback HTTP。禁止 URL userinfo、
  query、fragment、redirect 和非规范 base path。
- 所有操作使用 `POST` 和 `Content-Type: application/json`。
- 请求、响应和错误 envelope 固定
  `schema_version = hailix.platform_execution_http.v1alpha1`，同时发送
  `X-Hailix-Platform-Execution-Version`。
- canonical Plan、StageExecutionRequest 和 StageExecutionResult 作为 JSON 子对象传输，不能
  变成聊天文本、路径、漂移的 latest ref 或服务端重新生成的近似输入。
- StageExecutionRequest 的 capability 必须携带 `trust.authority=platform`、exact capability
  verifier 和 callback verifier；三者参与 capability/request digest。它们是 immutable trust
  identity，不是 bearer credential 或 attestation 本身。
- request/response body 上限均为 4 MiB，callback proof decoded 上限为 64 KiB；成功响应、
  错误响应都严格拒绝未知字段和 trailing JSON。

机器可读 schema 位于
[`api/schema/v1alpha1/hailix-platform-execution-http.schema.json`](../../api/schema/v1alpha1/hailix-platform-execution-http.schema.json)。

## 2. 身份与凭据

每个 command 都包含完整 `tenant_id / organization_id / workspace_id / repository_id`
subject。该 subject 只是待校验的请求绑定；Hailix 必须从 bearer credential 得到真实 actor、
membership 与 permission，再与 body subject 做 exact comparison。

Argus 的 `CredentialSource` 在每次 HTTP 请求前即时解析 bearer token 和 credential revision。
token 不进入 command、dispatch intent、artifact、binding、日志或错误。HTTP header 固定包含：

- `Authorization: Bearer <ephemeral token>`；
- `X-Hailix-Credential-Revision: <revision>`；
- `Idempotency-Key: <exact operation identity>`。

credential revision 不能替代 Hailix 服务端权限重验或 runtime generation fencing。
正式 CLI 只接受 `--execution-backend hailix-http`、base URL 和 pinned verifier refs；不接受 token
参数或任意环境变量名。credential 固定从 `ARGUS_HAILIX_BEARER_TOKEN` 与
`ARGUS_HAILIX_CREDENTIAL_REVISION` 读取，缺任一值必须在 capability 网络调用前失败关闭。

## 3. 操作

| Path | 输入 | 成功响应 | 强制语义 |
|---|---|---|---|
| `/v1/platform-executions/capabilities:resolve` | exact subject、Plan ID/SHA、canonical AgentStagePlan | capability + pinned verifier receipt | capability 必须绑定同一 Plan 和 subject，不得扩大 tool/model/filesystem/network/delegation authority |
| `/v1/platform-executions:ensure` | exact subject + canonical StageExecutionRequest | one immutable execution receipt | `Idempotency-Key` 必须原子绑定完整 request；exact retry 返回同一 handle；同 key 异值冲突 |
| `/v1/platform-executions:lookup` | 与 ensure 相同的完整 request | receipt 或 empty-body `404` | 只按 exact request identity 查询，不创建执行，不按 caller-supplied handle 猜测 |
| `/v1/platform-executions:cancel` | terminal gate、intent、handle、attempt/generation/fence 和两类 idempotency key | empty-body `204` | cancel key 原子绑定 exact terminal winner 和 target；exact retry 无额外副作用 |
| `/v1/platform-executions:await` | exact immutable handle tuple | canonical typed result + callback proof | 只返回终态；结果必须回显 request/capability/attempt/generation/fence/key |
| `/v1/platform-executions/callbacks:verify` | durable binding、canonical result、proof bytes 与两者 digest | verifier receipt | 服务端验证签名/attestation、subject、binding、result 和 proof；receipt verifier 必须同时命中 exact persisted request 中的 callback verifier 与 Argus composition root 的 pinned trust root |

ensure 示例：
[`examples/hailix-platform-execution-http.ensure-request.json`](../../examples/hailix-platform-execution-http.ensure-request.json)。
terminal 示例：
[`examples/hailix-platform-execution-http.terminal-response.json`](../../examples/hailix-platform-execution-http.terminal-response.json)。

## 4. Unknown outcome 与恢复

Argus 在 provider 调用前已经持久化 exact `AgentStageDispatchIntent` 并取得 durable claim。
HTTP 层不再建立第二套 Outbox：

1. credential、marshal 或本地校验失败发生在发送前，不是远端 unknown outcome；
2. ensure/cancel 的连接错误、响应读取失败、5xx 或成功响应损坏统一标记
   `ErrHTTPOutcomeUnknown`；
3. 恢复只能使用已持久化的同一 canonical request 调用 exact lookup 或 exact ensure；
4. Hailix 必须用 provider-authoritative atomic create-or-return-one-handle 保证重复 ensure 不启动
   第二个 Worker execution；
5. Argus 将返回 handle 与 request 的 execution/attempt/generation/fence/idempotency/request SHA/
   capability SHA 逐字段比较，任何替换都是 contract violation；
6. durable binding 已存在时，恢复直接复用 binding，不再请求 provider；terminal gate 已获胜时，
   不得再次 ensure；
7. fresh callback、已登记 receipt 的 crash recovery 和 terminal exact retry 都从同一个 persisted
   request 取得 trust。callback verifier 与该 trust 或当前 pinned Hailix trust 任一不一致即 contract
   violation，且不得调用远端 verify 试探另一个信任域。

因此 Argus 可以证明本地 write-ahead authority 与可恢复调用顺序，但只有 Hailix 真正实现原子
ensure/lookup、authenticated generation head、runtime attestation 和 durable result，才能把远端
执行描述成 platform-attested；HTTP client 或本地 mock green 不能单独证明这一点。

## 5. 状态与错误

- lookup 未找到：`404` 且 body 必须为空；
- cancel 成功：`204` 且 body 必须为空；
- 其他成功：`2xx` 且 body 必须符合对应 response schema；
- 非成功响应使用严格 envelope：

```json
{
  "schema_version": "hailix.platform_execution_http.v1alpha1",
  "error": {
    "code": "stable_machine_code",
    "message": "bounded operator-facing summary",
    "retryable": true
  }
}
```

`429/5xx/retryable=true` 是 unavailable；ensure/cancel 的 5xx 进一步属于 unknown outcome。
明确的 4xx 业务拒绝属于 known rejection。客户端不回显响应正文、credential 或可能含敏感数据的
服务端 message。

## 6. 当前验证边界

`internal/hailixexecution/http_client_test.go` 使用真实 loopback HTTP transport 覆盖六个操作、
严格 envelope、subject/header、每请求 credential resolution、redirect/unsafe endpoint 拒绝、
lookup miss、provider 已提交但首个 ensure 响应丢失后的 exact retry。既有
`internal/application/agent_dispatch_test.go` 独立覆盖 durable claim、崩溃窗口、generation CAS、
terminal winner、request-bound trust、fresh/recovered callback receipt 与 exact recovery。两组测试共同约束 consumer；Hailix 服务端仍需实现相同
provider-authoritative contract 并接受跨仓联调。
`cmd/argus/hailix_formal_integration_test.go` 另从正式 CLI flags 构造 backend，证明 request-time
environment credential、pinned trust、unknown-outcome recovery 与 terminal exact retry 在 production
composition 上成立，且该路径不会启动本地 Pi runner。
