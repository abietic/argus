# Argus Requirements

**Status:** draft baseline
**Version:** 0.1
**Last verified:** 2026-08-24

## 1. 问题定义

代码评审的难点不是“让模型对一段 diff 发表评论”，而是同时解决四类问题：

1. **发现问题：** 对变更代码、用户圈选代码、指定仓库或目录发现真实的不合理
   逻辑、潜在缺陷和高价值改进。
2. **控制行为：** 不同组织、仓库、目录、语言和风险等级需要不同规则、工具、
   模型、预算、发布策略与数据策略。
3. **建立证据：** 一次评审为什么运行、看到了什么、使用了什么配置、产出了
   哪些候选、为什么过滤或发布，必须能被追踪和复核。
4. **持续改进：** 历史反馈必须能构建评测集，在冻结快照上重放单个原子阶段，
   比较改动前后效果，并通过门禁回流，而不是依赖主观挑选成功案例。

Argus 要把这四类问题放进同一个领域模型和数据闭环中。

## 2. 目标与成功标准

### 2.1 产品目标

- 用一套 `ReviewSpec` 统一 diff、selection 和 scope 三种评审入口。
- 用版本化 DAG 表达评审核心，每个阶段输入/输出可持久化；只有声明幂等/可重试，
  或声明 replayable 且无副作用的阶段，才能执行对应操作。
- 保留所有候选 Finding 及其决策轨迹，发布评论只是 Finding 生命周期的一部分。
- 给用户提供按组织/仓库/目录生效的配置平台，并可解释最终有效配置。
- 给开发、算法和产品分别提供历史、诊断、评测、实验和收益看板。
- 通过 Hailix 的通用 Agent 平台能力驱动 Eino-Agent，并形成三仓自举闭环。

### 2.2 质量成功标准

所有指标必须带数据集/流量范围、版本、时间窗和置信边界。P0 不承诺具体阈值，
但必须能正确计算：

- 高严重度缺陷召回率与分类型召回率；
- 发布 Finding 的精确率、误报率和校准误差；
- 无结果率、执行失败率、证据不完整率；
- p50/p95 端到端时延、阶段时延、模型/工具成本；
- Finding 接受、驳回、修复、复发和未知结果的比例；
- 与基线相比的研发等待时间、人工评审工作量和逃逸缺陷变化。

“评论数量”“模型分数”“生成 token 数”只能作为诊断指标，不能单独证明收益。

## 3. 用户与核心场景

| 用户 | 核心需求 |
|---|---|
| 开发者 | 在 MR/PR、IDE 圈选或本地变更上获得少而可信、可定位、可修复的 Finding |
| 仓库 Maintainer | 配置规则、路径、预算和发布策略，观察仓库质量趋势 |
| 质量/平台工程师 | 排障、回放、构建评测集、运行实验、设置 promotion gate |
| 组织管理员 | 管理租户、权限、数据保留、模型出域和默认策略 |
| Argus 开发者 | 在 Argus/Hailix/Eino-Agent 上 shadow review 并安全回流 |

## 4. 通用语言

| 术语 | 定义 |
|---|---|
| `ReviewSpec` | 用户意图、目标、约束和版本引用组成的评审请求 |
| `TargetSnapshot` | 仓库 revision、diff/selection/scope 内容及 digest 的不可变快照 |
| `ExecutionSnapshot` | Target、配置、工作流、模型、工具、索引和运行环境的完整冻结引用 |
| `PlatformExecutionBinding` | dispatch 后追加的 StageRun 与 Task/WorkerSession/WorkerRun/AgentTurn 绑定 |
| `RunEvidence` | 执行完成后追加的 TraceManifest/ArtifactRef、完整性和校验信息 |
| `ReviewRun` | 一次领域评审执行，可包含多个 StageRun 和 AgentTurn |
| `StageRun` | DAG 中一个原子阶段的一次执行 |
| `CandidateFinding` | 任一 detector 产生、尚未完成裁决的候选问题 |
| `Finding` | 已规范化且拥有稳定身份、锚点和证据链的问题实体 |
| `FindingDecision` | verify/reject/suppress/publish 等显式决策，不覆盖 Finding 本体 |
| `Feedback` | 用户或系统对 Finding 的态度/处置选择，如接受、驳回、wont_fix 或 outdated |
| `Outcome` | 在归因窗口内验证到的实际后果，如修复合入、测试失败或缺陷复发 |
| `ConfigBundle` | 按作用域解析后的不可变有效配置及其来源解释 |
| `AgentReviewPolicy` | formal Agent Stage 的 agent/component/authority/budget 有效策略 |
| `ConfigResolutionReceipt` | 绑定一次本地 lifecycle 解析结果与 published revision provenance 的治理记录；不是签名或远端 attestation |
| `AgentStagePlan` | dispatch 前冻结 formal Agent Stage 输入、exact components/runtime/build、权限、预算和 hypothesis-only 输出语义的 canonical plan |
| `RulePack` | 规则定义、适用范围、例子、严重度和验证方法的版本集合 |
| `EvaluationCase` | 带输入快照、期望标签、来源、许可和 split 的评测样本 |
| `ReplayRun` | 从历史快照开始、只改变声明变量的重放执行 |
| `ValueObservation` | 带证据等级、基线和归因窗口的收益/损失观测 |

## 5. 评审核心需求

### R-CORE-001 统一评审目标

系统必须支持：

- `diff`：两个不可变 revision 之间的变更，可映射 MR/PR 或本地 Git diff；
- `selection`：文件、行/符号范围和可选未保存 overlay 的内容快照；
- `scope`：固定 revision 下的仓库、目录、文件集合或 include/exclude pattern。

运行开始后不得静默扩大目标。动态发现的依赖文件作为 `ContextRef` 记录，不改变
原始 Target。

### R-CORE-002 不可变执行快照

首次执行前必须生成 `ExecutionSnapshot`，至少包含：

- repository identity、base/head revision、dirty/overlay 状态；
- diff/selection/scope manifest 及 content digest；
- ConfigBundle、RulePack、WorkflowDefinition 的版本和 digest；
- Agent/model/profile、tool/MCP/Skill、CodeGraph/index revision；
- required runtime profile、image/build digest、预算、授权、网络与数据策略；
- Hailix Workspace/source input ref，以及所有调用方提供内容的逻辑 ArtifactRef。

`ExecutionSnapshot` 是 dispatch 前事实，不得混入尚未产生的 Task、Trace 或输出
Artifact。dispatch 后以 append-only `PlatformExecutionBinding` 记录 Task、
WorkerSession、WorkerRun、AgentTurn、实际 runtime/container identity、attempt
generation 和 fencing token；执行完成后以 `RunEvidence` 追加
`platform_execution_binding_id`、attempt/generation、幂等 identity、
TraceManifest/ArtifactRef、checksum 和 completeness。

调用方只能提交平台签发的逻辑 `artifact://authority/path` 引用，不能提交 `file://`
或网络 URL。resolver 每次读取都必须重新校验 tenant/workspace 授权、digest 和
size，不能把 URI 当作可直接抓取的地址。

缺失影响复现或授权的字段时必须失败关闭。重放表示“相同输入与版本”，不承诺
非确定性模型产生逐字节相同输出。

formal Agent Stage 还必须在 dispatch 前生成 `AgentStagePlan`，至少绑定 exact
ExecutionSnapshot、ConfigBundle、ConfigResolutionReceipt、WorkflowDefinition、ReviewSpec、
ReviewInput、agent/provider/model/runtime/prompt/API protocol、按策略顺序排列的
Skill/Knowledge/context provider、build identity、authority 与 budget。Plan 不能保存
credential、provider endpoint、raw prompt、可变 latest 或本地 repository path；输出在进入
独立 normalize/verify/adjudicate 之前只能是 `ReviewHypothesisSet`，不得直接成为 Finding。
Pi shadow 路径必须把 normalize 前的有界 raw candidate 独立内容寻址保存，并让
`rejected_invalid/excluded_budget` 与 normalization decision 可查询、可校验且不静默丢失；
该 worker self-report payload 不能冒充可信源码 evidence 或 gold label。
本地 Pi MVP 的 ExecutionSnapshot 还必须保存与 governed runtime component 同字节摘要的
runtime-file manifest，至少覆盖 Node executable、worker emitted modules、package lock 和实际
production dependency 文件；dispatch 与 process start 均重新验证。该本地主机观测不能标记为
Hailix/platform attestation，也不能替代 prompt/context/tool transcript evidence。

### R-CORE-003 版本化评审 DAG

评审工作流至少表达以下逻辑阶段：

1. `materialize_target`
2. `plan_context`
3. `detect`
4. `normalize`
5. `verify`
6. `adjudicate`
7. `publish`
8. `capture_feedback`
9. `export_evaluation`

阶段可按语言/文件/规则并行，但必须有稳定 `stage_id`、输入/输出契约、版本、
依赖、预算、超时、失败策略和 replay policy。ACP 是阶段执行适配器，不是 DAG
业务状态机。

每个外部工具调用还必须受版本化 `ToolInvocationPolicy` 约束：工具/参数 schema、
路径与网络范围、单次 deadline、输出 bytes/token 上限、并发/委派深度、截断与
partial evidence 语义、取消传播，以及按错误分类的 retry/backoff+jitter/
max-attempt/idempotency/unknown-outcome 策略。

生产评审、离线评测和 replay 必须调用同一 Workflow/Stage implementation；不得
另写一套“评测版逻辑”再假设它代表线上行为。

在 stage dependency、replay source/input/change set 或 retry generation 尚未进入
`AgentStagePlan`/Attempt identity 前，对应 formal Agent Stage 必须拒绝这些执行模式，不能
用未绑定的隐式上下文或 silent retry 破坏 exact closure。

### R-CORE-004 多类 Detector 组合

系统必须能组合：

- 确定性规则、AST/LSP、编译、测试和静态扫描；
- repository search、CodeGraph、调用链和依赖证据；
- LLM semantic detector；
- 多 Agent 分析、反思、验证和裁决。

每个候选必须记录 detector、rule、输入范围和证据引用。不得把 LLM 自述“已验证”
当成工具证据。

任何外部上下文提供者必须返回 `complete | partial | missing` 及原因、revision 和
coverage。CodeGraph 没查到调用边不等于不存在调用边。

### R-CORE-005 Finding 生命周期

系统必须把以下内容分开保存：

- 候选生成；
- 规范化与语义去重；
- 证据验证；
- 严重度/置信度校准；
- 抑制、发布和渠道投影；
- 用户反馈与结果验证。

分数过滤、评论限额、渠道失败或重复评论只能产生决策，不得删除候选。Finding
需要稳定 fingerprint，但必须允许同一缺陷在不同 revision 上建立 lineage，而
不是把相似文本永久合并。

### R-CORE-006 发布安全

- 自动发布只能引用当前 Target 中的稳定 anchor。
- 发布前必须重验 repository revision、权限、策略版本和 Finding 身份。
- 远程评论写入必须幂等，并记录 provider request/result；未知结果不得盲目重试。
- Review ExecutionSnapshot 必须保持远程写关闭；远程评论只能由评审后独立、短期、
  一次性且精确绑定 publication identity、最新人工 publish Decision、Target、配置和
  Finding source 的 PublicationGrant 授权。Grant 的签发与消费必须 append-only、可恢复，
  过期、撤回后的 Decision、重放运行或第二个不同消费请求全部失败关闭。
- 修复建议与自动 Apply 是独立受保护动作，不由 Finding 发布隐式授权。
- `selection` 与本地 `scope` 默认只产出本地报告，不执行远程写入。

### R-CORE-007 覆盖率与持久执行

- 大 diff/scope 的切片必须生成 coverage manifest：总文件/hunk、已处理、跳过、
  截断和失败原因；禁止静默丢弃。
- Full Scan 使用可恢复 shard、checkpoint、lease、heartbeat 和 cancel；不得依赖
  单进程 goroutine 或数小时长轮询维持业务状态。
- callback/event 必须幂等，实例迁移后由 reconciler 收敛未知状态。
- 配置或能力 fallback 必须成为显式运行事实；发布路径默认 fail closed。
- ReviewRun 的用户取消会阻止未来 stage 调度、取消所有活跃子 attempt 并永久禁止
  该 run 发布；attempt timeout 只取消当前 generation，retry 使用新 fencing token，
  迟到 callback/result 必须丢弃。
- `incremental_mr`、`interactive`、`full_scan`、`eval_replay` 是独立 workload
  class。调度器必须定义 resource pool、租户配额、优先级/公平性、queue limit、
  starvation protection 和 backpressure 指标，full scan/replay 不能拖垮交互评审。

## 6. 配置平台需求

### R-CONFIG-001 作用域与优先级

配置至少支持 `platform -> tenant -> organization -> repository -> path ->
invocation` 层级。每个字段必须声明 merge 语义；不允许对任意 YAML/JSON 做隐式
深合并。

### R-CONFIG-002 配置领域拆分

有效配置至少区分：

- Trigger/Target policy；
- path/include/exclude policy；
- RulePack 与 applicability；
- WorkflowDefinition；
- Agent/model/tool/context policy；
- budget/timeout/concurrency policy；
- verification/adjudication policy；
- publication/channel policy；
- data retention/redaction/training policy。

其中 formal Agent Stage 使用独立的 typed `AgentReviewPolicy` 闭合 agent/provider/model/
prompt/API protocol、按 stable ID 有序合并的 Skill/Knowledge、工具/模型权限和预算；
credential 只能保存 secret ref，不能进入 component resolution request 或 stage plan。

### R-CONFIG-003 版本与发布

配置修改必须经过 `draft -> validated -> published -> superseded/rolled_back`
生命周期。每次运行只读取一个不可变 `ConfigBundle`。平台必须展示：

- 最终值；
- 值来自哪个 scope/revision；
- 哪条规则覆盖或合并了什么；
- schema/semantic validation 结果；
- 发布人、时间、灰度范围和回滚目标。

历史链不能只依赖 `parent_id` 或可变 blob。密钥只保存引用，不能进入配置快照、
Trace、Artifact 或离线数据。

正式 Agent planning 必须先把 authenticated host adapter 注入的 tenant/organization/
workspace/repository subject 与 ReviewRun/ReviewSpec/resolution context、repository provider、
invocation 和 selection/non-selection path scope 逐项闭合，再访问配置或 component provider；
component resolution 还必须绑定完整已准入 subject，禁止形成跨 organization/workspace
confused deputy。随后由同一个受信配置 provider 调用同时
返回 `ConfigBundle` 和 `ConfigResolutionReceipt`。receipt 必须绑定 resolution context、exact
bundle 与全部 applied published revisions 的 lifecycle provenance，但它只是可校验的本地
治理记录，不是签名或远端 attestation；仅验证 receipt 内容不能替代配置 repository 对
publication 的授权判断；caller 自报 organization 也不能替代 authenticated subject。

## 7. 历史、反馈与数据需求

### R-DATA-001 权威数据与投影分离

- PostgreSQL 保存领域实体、状态、版本、事件索引和幂等记录。
- Argus ObjectStore 保存不可变 diff/snapshot、Stage input/output、原始候选、报告和
  大对象；local fake 的 stage log 可保存为 Argus 自有日志，但不能冒充 Hailix Trace。
- 正式 Hailix adapter 下，Hailix 保存权威 Trace JSONL；Argus 只保存受权 TraceRef 和
  派生去敏 facts。
- TraceManifest/ArtifactRef 保存 checksum、sequence、权限、完整性和生命周期。
- 看板消费结构化事实/投影，不在查询时扫描原始 JSONL。
- 离线分析使用版本化 Parquet export；DuckDB 可作为本地分析器，不是线上事实源。

### R-DATA-002 历史可追踪

用户必须能从 ReviewRun 向下追到 StageRun、AgentTurn、Finding、Decision、
Feedback、Outcome、Trace segment、Artifact 和配置/工作流版本，也能反向查看某个
规则或版本影响了哪些运行。

### R-DATA-003 数据完整性

- 事件、segment、artifact 和 snapshot 使用稳定 ID、SHA-256、sequence 和
  idempotency key 对齐。
- 部分成功必须显式标记 `partial` 与原因。
- checksum 不一致的数据进入 quarantine，不能用于发布、评测或训练。
- 删除原始证据后保留 tombstone，不得伪装为证据仍可读取。

## 8. 评测、重放与回流需求

### R-EVAL-001 评测集来源

允许的候选来源包括：

- 人工确认的真实 Finding；
- 明确驳回的误报；
- 事故/线上缺陷反推的漏报；
- 已审查的 bug-fix pair；
- mutation/synthetic defect、独立 clean corpus 和 workflow invariant case，但必须由独立 oracle 与
  可重放 construction receipt 支撑、单独标识，不能冒充生产分布或用 Argus 自身输出自标注。

样本必须记录 provenance、license/consent、敏感级别、label policy revision、
review state 和可用范围。

### R-EVAL-002 污染防护

- 按 repository、时间和语义克隆组切分 train/dev/test/holdout。
- 评测运行记录 prompt/rule/model/index 是否见过样本。
- 生产回流不能直接进入 holdout。
- Judge 模型输出只是带版本的评价证据；关键门禁需要确定性检查或人工标注校准。

检测正确性、过滤有效性和修复 Apply 忠实度必须分别评测。模型生成的行号不能
直接作为补丁协议；Apply 应使用稳定 anchor/edit script，并经过 dry-run、编译和
测试。

### R-EVAL-003 原子重放

用户必须能从任意 replayable stage 开始：

- 复用上游冻结 Artifact；
- 声明唯一改变的变量；
- 将输出写入新 namespace；
- 保留 source run、parent replay、变更集和 lineage；
- 对比候选、决策、质量、时延和成本。

重放不得覆盖历史运行，也不得触发生产评论或自动 Apply。

同配置重复运行与单变量实验必须是两类事实。RepeatabilityRun 只接受一个 succeeded
非 replay baseline 与至少一个从该 baseline 直接产生的 `variable=none` exact replay；所有样本必须
使用相同 evaluator、case/label、Target/ReviewInput、ConfigBundle、Workflow、Runtime 和工具策略。
它按稳定 Finding identity 计算 pairwise Jaccard、exact-set rate、finding presence、expected-anchor hit
与 verdict flip；partial report 必须令 Finding-set stability 显式 unavailable，不能被当作稳定空集。
这些指标描述重复性，不替代 precision、recall 或业务收益。

跨 dimension normalization 必须使用独立、预先封存的 claim equivalence oracle 评价，不能把当前
normalizer 的 cluster 或 Argus Finding 自动当作真值。Oracle 至少绑定 governed CorpusSnapshot case/
label revision、committed succeeded ReviewRun/raw-candidate ref、完整 eligible raw ID partition、两个独立
reviewer、第三方 adjudicator、外部 evidence refs 和已观察过的 policy revision。质量结果至少记录 pairwise
true/false/missed duplicate、precision、recall、false-merge rate、unique-claim count delta、exact partition
match 与 policy exposure；没有正例 pair 时相应比例必须 unavailable，不能把零分母写成 100%。
Oracle artifact 本身不构成治理授权：进入质量运行前必须由外部 Ed25519 key 对 exact oracle digest
签名，且该 scoped key revision 已由独立 trust admin 预注册。Oracle registry 必须 append-only，首次
注册与替换使用 expected current revision/event CAS，撤销也绑定 exact current registration；注册 operator、
reviewer、adjudicator 与 trust admin 必须保持职责分离。Quality request/result 必须携带 exact active
registration binding；key/oracle 撤销、case label/governance 漂移或 binding 过期后失败关闭。
Normalization promotion 必须将阈值策略保存为不可变 Artifact，并把 exact policy revision、gate policy
identity/digest 写入 managed promotion binding。targeted regression 只能消费 test split，fixed holdout 只能
消费 holdout split；二者都必须重新计算整个 quality closure，拒绝 policy-exposed、非独立、已撤销或漂移的
oracle。样本覆盖不足或必需比例 unavailable 必须为 inconclusive，精度/召回/误合并/exact partition 未达
整数阈值必须 fail，不能用一个 float score 或调用方自报摘要替代。

### R-EVAL-004 Promotion gate

规则、prompt、模型、工作流或过滤策略变更必须依次经过：

1. schema/contract validation；
2. targeted regression；
3. 固定 holdout；
4. shadow traffic；
5. canary；
6. 人工或策略授权 promotion；
7. rollback monitor。

任何线上反馈都只能先成为候选证据，不能直接修改 active 版本。

## 9. 收益与看板需求

看板至少提供：

- 运行健康：量、成功率、无结果率、partial、时延、成本；
- 质量漏斗 projection：candidate -> verified -> published -> accepted -> fixed；其中
  Decision、Feedback、Outcome 仍来自三套独立 ledger；
- 规则/语言/仓库/路径/版本维度的质量和漂移；
- 反馈延迟、未知结果比例和 label coverage；
- 实验基线与 variant 的质量/成本/时延对比；
- 收益观测及证据等级。

收益必须分层：

| 等级 | 例子 | 可用于 |
|---|---|---|
| E0 推测 | 模型估算“节省 10 分钟” | 调研，不进入正式收益 |
| E1 行为信号 | 评论被打开、回复、接受 | 产品采用趋势 |
| E2 工程结果 | Finding 对应修复合入且验证通过 | 可信质量收益 |
| E3 反事实/对照 | holdout、A/B 或可信基线显示缺陷/耗时变化 | 决策与 ROI |

## 10. 非功能需求

- **隔离：** tenant/workspace/repository 权限必须贯穿 Trace、Artifact、评测与导出。
- **隐私：** 代码默认不用于训练；导出/回流需要策略和来源许可；凭据必须确定性
  脱敏。
- **可靠性：** 长任务异步执行，状态机、Outbox、幂等、fencing 和恢复语义明确。
- **背压：** admission、队列和 executor pool 都必须有界；过载显式拒绝或降级并
  记录原因，不能无限排队或静默挤占在线 workload。
- **可移植：** 核心领域不依赖单一代码平台、模型厂商或 Agent 实现。
- **可扩展：** 语言、detector、evidence provider 和 publisher 通过版本化端口接入。
- **性能：** 大 diff/scope 使用 manifest、切片和预算，不把完整仓库塞入单次 prompt。
- **可维护：** 领域层不依赖 HTTP、数据库、ACP SDK 或模型 SDK。
- **不可信输入：** 仓库内 hook、MCP、skill、plugin、Agent setting 都按待评审
  源码处理，不能自动成为评审 runtime 配置。

## 11. Hailix / Eino-Agent 边界

Hailix 长期应提供身份/授权、Workspace/WorkingCopy、Task/Worker Runtime、ACP
session、Trace/Artifact、IntegrationEvent、模型代理和成本记录。Argus 提供
ReviewSpec、ReviewWorkflow、RulePack、Finding、Feedback、Evaluation/Experiment
和 Value attribution。未来可复用的只是在 Hailix 侧抽取 platform execution
input/ref、隔离 namespace 和 side-effect enforcement；Argus 的
ExecutionSnapshot、ReplayRun、ExperimentRun 始终属于 Code Review 领域。

Eino-Agent 提供可被 ACP 控制的仓库理解与执行能力。Argus 不读取其内部 session
数据库，也不依赖 TUI/CLI 私有命令；Hailix/Argus 只能依赖协商后的 ACP 能力和
版本化输出契约。当前 Eino-Agent 没有 schema-constrained ACP review output；
正式链路需要 typed result sink，不能把最终聊天文本解析成业务事实。

详见 [集成边界](docs/integration/hailix-eino-agent.md)。

## 12. M1 纵向切片验收

M1 完成必须同时满足：

1. 对本地 Git 仓库的固定 base/head 生成 TargetSnapshot。
2. 使用一个版本化工作流完成 detect -> verify -> adjudicate。
3. 产出严格 JSON Finding 和人类可读报告，不远程写评论。
4. 所有输入/输出、版本、耗时和成本引用落盘。
5. 能从 detect 或 verify 阶段重放，并比较两次结果。
6. 至少有一组真阳性、误报和无 Finding fixture。
7. `make verify` 全绿。

## 13. 当前假设与待决策

当前默认假设：

- Go 1.26 作为服务端和领域实现语言。
- 公开 Go module path 固定为 `github.com/abietic/argus`。
- Hailix 是通用平台能力 owner，跨仓通过 API/事件/Artifact 契约集成，不共享
  `internal` Go package。
- PostgreSQL + ObjectStore 是长期事实存储；M1 可先使用本地 adapter。
- 生产 remote write 默认关闭。

仍需用户/实验证据决定：

- 首个代码平台与首个目标语言；
- Argus 是否拥有独立前端，还是先作为 Hailix 的 vertical app；
- P0 使用 Eino-Agent 还是先用 deterministic fake/runtime adapter 验证闭环；
- 组织级规则的审核者、发布者与紧急回滚权限；
- 源码、Trace 和评测样本的默认保留期；

这些问题不会阻塞 M0；会改变生产数据、安全或集成方式的决策必须在进入对应
里程碑前冻结。
