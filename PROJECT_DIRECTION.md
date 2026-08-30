# Project Direction

**Status:** current
**Last verified:** 2026-07-26

## 产品目标

Argus 的目标不是最大化模型输出的评论数量，而是在给定代码快照、评审策略和
资源预算下，尽可能早地发现真实缺陷，并让每个结论都可解释、可追踪、可复核，
同时能够证明它是否为研发过程带来净收益。

目标函数按优先级排序：

1. 不漏掉高损失缺陷，且不以不可接受的误报打断研发。
2. 任意发布 Finding 都能回到输入、配置、工作流、模型/工具与证据。
3. 声明 replayable 且无副作用的工作流阶段能在冻结快照上重放，并明确唯一变化的
   实验变量。
4. 用户配置可治理、可灰度、可回滚，运行时只消费已发布的不可变版本。
5. 线上反馈能进入评测候选池，但不能未经审核直接改变生产行为。
6. 成本、时延和收益使用可审计口径，不用“评论数”代替产品价值。

## 产品原则

- **Finding 与 Comment 分离。** Finding 是候选事实；Comment 只是一次渠道
  发布决策。过滤或限额不能删除候选及其证据。
- **快照先于执行。** 没有 pre-dispatch `ExecutionSnapshot`，以及随后追加的执行
  binding/evidence，就没有可审计的评测或重放。
- **领域工作流与运行时分离。** Argus 定义评审 DAG 和阶段语义；Hailix 承载
  通用 Task/Worker/ACP 生命周期；Eino-Agent 执行 Agent 能力。
- **确定性能力与 LLM 能力组合。** AST/LSP/CodeGraph、编译、测试、静态分析
  是证据提供者；LLM 负责语义假设和综合判断，不能伪装成确定性验证。
- **离线评测与线上价值分层。** 离线质量决定是否允许试运行，线上结果决定
  是否扩大流量，两者不能互相替代。
- **失败关闭。** 快照不完整、权限过期、配置解析不确定、证据损坏或身份错配
  时不发布评论。
- **自举但不自我背书。** Argus 可以评审 Argus/Hailix/Eino-Agent，但自己的
  输出不能自动成为自己的训练真值或发布授权。

## Reference 采纳规则

旧 Smart CR、Hailix、Eino-Agent 以及其他评审/Agent 系统只提供证据，不自动
定义 Argus 的产品范围。每个参考决策必须使用以下分类之一：

| 决策 | 含义 |
|---|---|
| `preserve` | 保留已证明有价值且难以重建的可观察能力 |
| `adapt` | 保留结果，改造成 Argus/Hailix 适配的实现 |
| `combine` | 组合多个来源，形成 Argus 自有契约 |
| `project-native` | 由 Argus 的用户问题直接推导 |
| `reject` | 价值、风险或复杂度不成立，不采纳 |
| `defer` | 证据或优先级不足，不视为已接受 backlog |

旧系统中值得 `adapt/combine` 的核心模式包括：多目标评审、生成—反思—验证、
CodeGraph 证据、候选后处理、用户规则、全仓扫描和回放入口。需要明确拒绝的是：
把大块 YAML/JSON 当无类型配置源、把分数阈值当真值、过滤后丢失候选、用评论数
衡量收益、以及由 Agent 隐式扩大评审范围。

## 阶段方向

| 阶段 | 成功标准 |
|---|---|
| M0 Foundation | 契约、边界、DAG、严格校验和工程门禁可运行 |
| M1 Local Vertical Slice | 对固定 Git diff 完成快照、评审、验证、Finding 报告和本地重放 |
| M2 Hailix Integration | 使用 Hailix Workspace/Worker/ACP/Trace/Artifact 执行 Eino-Agent |
| M3 Configuration Plane | 规则包、策略版本、发布、灰度、回滚和有效配置解释 |
| M4 Evaluation Plane | 数据集、阶段重放、实验对比、污染防护和发布门禁 |
| M5 Product & Value | 历史、反馈、看板、收益证据和代码平台集成 |
| M6 Bootstrap | 三仓 shadow review、修复验证、受控 promotion 和回归监控 |

阶段名表示方向，不代表当前完成度。当前事实只记录在
[STATUS](docs/roadmap/STATUS.md)，已接受执行顺序只记录在
[PLAN](docs/roadmap/PLAN.md)。
