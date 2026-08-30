# Context Map

## Argus 内部

| Upstream | Downstream | 契约 | 约束 |
|---|---|---|---|
| Review Intake | Review Policy | target identity, policy scope | 解析配置不能改变用户目标 |
| Review Intake | Review Execution | ReviewSpec, TargetSnapshot | 执行前必须冻结 digest |
| Review Policy | Review Execution | ConfigBundle, WorkflowRef | 只消费 published immutable revision |
| Review Execution | Finding | CandidateArtifact, StageResult | partial/checksum 状态必须保留 |
| Finding | Feedback | Finding, PublicationReceipt | Feedback 不覆盖 Finding |
| Feedback | Evaluation | FeedbackEvidence, Outcome | 未治理反馈只能进入 candidate pool |
| Evaluation | Review Policy | PromotionProposal | 不能直接修改 active config |
| Finding / Evaluation | Value | normalized facts | Value 不扫描原始 prompt/源码 |

## 外部系统

| 外部 Context | Argus 关系 | Owner |
|---|---|---|
| Hailix Identity & Workspace | tenant/user/repository authorization、revision 和 WorkingCopy | Hailix |
| Hailix Task & Worker Runtime | TaskSaga、session、container、budget、recovery | Hailix |
| Hailix ACP Adapter | capability negotiation、prompt/update/cancel/permission | Hailix |
| Hailix Artifact & Trace | Trace JSONL、TraceManifest、ArtifactRef、retention | Hailix |
| Hailix EventCenter | IntegrationEvent transport/outbox/projection | Hailix |
| Hailix LLMProxy | model route、credentials、cost、Langfuse projection | Hailix |
| Eino-Agent | repository reasoning、tools、subagents、typed review execution | Eino-Agent |
| Code Platform | MR/PR events、diff、comments、status checks | Provider adapter |
| Static/Graph Tools | AST/LSP/test/compiler/CodeGraph evidence | Evidence provider |

## 依赖方向

```text
Code platform / UI
        |
        v
Argus interfaces -> Argus application -> Argus domain
        |                  |
        |                  v
        +----------> platform ports
                           ^
                           |
                  Hailix adapters / local fakes
                           |
                           v
                  ACP -> Eino-Agent
```

Argus domain 不依赖 Hailix/Eino-Agent SDK。Hailix 和 Eino-Agent 的当前实现只决定
adapter 是否可用，不改变 Argus 的 Finding、Evaluation 或 Value 语义。

## 身份映射

不要假设一个 ReviewRun 等于一个 Hailix Task 或一个 ACP session。使用显式绑定：

```text
ReviewRun
  -> 0..n StageRun
      -> 0..n PlatformExecutionBinding
          -> Hailix Task / WorkerSession / WorkerRun / AgentTurn
          -> ACP AgentSession
          -> 0..n RunEvidence
              -> exact binding + attempt/generation
              -> TraceManifest / ArtifactRef / completeness
```

这样并行 detector、stage replay、runtime recovery 和未来多 Agent 都不会改变
ReviewRun 的领域身份。
