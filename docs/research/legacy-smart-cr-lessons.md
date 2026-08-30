# Legacy Smart CR Evidence Synthesis

**Evidence source:** local gbrain source pages
**Purpose:** extract reusable product lessons without copying proprietary data
**Verified:** 2026-07-26

## 1. 证据成熟度

gbrain 页面同时包含设计提案、实现说明和事后总结，不能把“写过方案”等同于
“生产验证”。本文件使用四级口径：`proposal`、`documented implementation`、
`code-aligned`、`production-verified`；没有运行证据时不提升到最后一级。

| 能力 | 当前证据成熟度 | 可安全得出的结论 |
|---|---|---|
| MR 事件、IDE/交互入口、diff/文件/函数链路 | `code-aligned` | 多入口与 Core/Agent/Arena 分工有源码/架构说明支撑 |
| 生成—反思—验证、CodeGraph、评分/过滤/去重/评论 | `documented implementation` / `code-aligned` | 核心阶段和后处理确实存在过，但版本一致性与线上效果未知 |
| full scan | `proposal` / partial implementation | 有 shard/任务设计与局部实现描述，未证明规模、取消和恢复达到生产目标 |
| 仓库/路径/规则/触发配置 | partial `documented implementation` | 存在局部配置注入；统一平台、字段级 merge/explain/rollback 未验证 |
| AutoEval、Reflect/回放、fix-apply 数据集 | partial `documented implementation` | 有阶段性评测/回放雏形，不等于同一生产 workflow 的端到端 replay |
| 回调、对象存储、任务状态、链路观测 | `code-aligned` | 平台组件存在；durable callback、完整 lineage 与离线事实一致性未证明 |
| 收益看板与 ROI | evidence not found | 不能把评论数、平均耗时或模型估算当成已验证收益 |

这些证据说明 Argus 应继承完整问题空间，而不是旧服务拆分；统一配置平台、端到端
replay、收益看板和可信 ROI 必须作为新需求实现，不能写成继承能力。

## 2. 采纳决策

| 旧模式 | 决策 | Argus 处理 |
|---|---|---|
| 多入口评审 | `combine` | 用 ReviewSpec/Target union 统一 |
| 生成—反思—验证 | `adapt` | 变成版本化、可原子重放 DAG |
| CodeGraph 证据 | `adapt` | 作为 EvidenceProvider，记录 index revision |
| full scan 单独链路 | `combine` | 与 diff/selection 共用 lifecycle，仅 target adapter 不同 |
| 固定 pipeline + ReAct | `combine` | DAG 决定边界，Agent 只在授权 stage 内选择工具 |
| score filter/comment limit | `adapt` | 变成 FindingDecision/PublicationPolicy，不丢 candidate |
| semantic dedup | `adapt` | 保留 lineage/指纹/证据，不按文本永久合并 |
| YAML extra config blob | `reject` | typed revision、字段 merge policy、effective explain |
| parent_id 历史 | `reject` | immutable revision DAG + publication/rollback ledger |
| AutoEval/Reflect | `adapt` | ExecutionSnapshot + ReplayRun + ExperimentRun |
| 评论/耗时指标 | `adapt` | 分离诊断、质量、采用、收益和证据等级 |
| 多服务硬编码路由 | `reject` | bounded context + port/adapter + versioned contract |
| 生产/离线两套 workflow | `reject` | 同一 implementation 驱动 production/eval/replay |
| 文件级静默截断 | `reject` | hunk/shard coverage manifest + skip/partial reason |
| 长轮询依赖进程存活 | `reject` | lease/heartbeat/checkpoint/reconciler |
| Agent 最终文本作结果 | `reject` | typed result sink + strict schema |
| Apply 使用模型行号 | `reject` | stable anchor/edit script + dry-run/test |

## 3. 旧系统暴露出的产品问题

- 同一评审能力分散在事件入口、Agent、Arena、沙箱和评论后处理服务，版本与 lineage
  容易断裂。
- 自定义配置按生效阶段散落，使用扩展 blob 提高了写入灵活性，却削弱 schema、
  可解释性和安全回滚。
- score、过滤、去重和评论数量策略会改变最终可见结果；若不保留全量 candidate，
  无法判断“模型没发现”还是“平台没发布”。
- 交互式 Agent 动态选工具有价值，但若 scope/权限/工具能力未快照，会破坏可追踪
  和可重放。
- 回放入口和评测集已有雏形，但只有把 Target、配置、工作流、模型、工具/index
  和阶段 Artifact 一起冻结，才能进行可信对比。
- 业务收益不能从单周平均耗时或评论数直接推导，必须有基线、样本量和归因证据。
- CodeGraph/调用图存在延迟、陈旧和新增函数缺图；查询结果必须携带 completeness，
  “未命中”不能当作“无风险”。
- full scan 需要可恢复 shard/checkpoint/cancel；进程内轮询在实例迁移后会丢失。
- 配置 fallback 若只写日志会让结果静默降级；必须进入 ExecutionSnapshot 和
  Decision/Metric。

## 4. 使用过的 gbrain 页面

主要参考以下本地 page slug/title：

- `cr-smart-cr-architecture-knowledge-summary`
- `cr-smart-cr-core-architecture-analysis`
- `cr-smart-cr-agent-architecture-analysis`
- `cr-smart-cr-arena-architecture-analysis`
- `20260324-cr-agent-full-scan-design`
- `20250805-cr-v3-custom-rules-capability`
- `20250827-cr-centralized-config-and-custom-scan-trigger`
- `20250603-cr-fix-apply-evaluation-dataset-and-pipeline`
- `20250619-cr-score-filter-and-comment-limit`
- `20260310-cr-comment-semantic-dedup-cr-agent`
- `cr-metrics-analysis-report`
- `cr-problem-analysis`
- `20250521-cr-arena-service-migration`
- `20250528-cr-arena-repo-iteration-spec`
- `20250624-cr-workflow-v3-diff-processing-optimization`
- `20260331-cr-defect-detection-stability-q1-q2`
- `agentx-model-call-previous-response-not-found-postmortem`

仓库不复制这些页面中的内部链接、生产 endpoint、凭据、组织标识或专有数据。
