# Remaining

本文件是 gap inventory，不等于已接受 backlog 或完成度分数。

## 需要产品决策

| 问题 | 推荐默认 | 最晚决策点 |
|---|---|---|
| 首个代码平台 | 本地 Git，remote write deny | M1 |
| 首个目标语言 | Go，用 Argus/Hailix/Eino-Agent fixtures | M1 |
| 前端归属 | local operator UI 已在 Argus；hosted 多租户入口仍归 Hailix vertical app | M3 |
| 数据保留 | 源码/Trace 短期、聚合长期，具体数字待合规/成本证据 | M2 |
| 用户规则审核 | maintainer publish + org policy deny-wins | M3 |
| 自动评论门限 | shadow 建立基线前保持关闭 | M5 |

## Argus 领域缺口

- 通用 stage 级 durable dispatch 与远端 crash-resume；当前本地 HTTP ReviewJob 已用 immutable command、exact config
  binding 和既有 workload admission/lease/heartbeat/reconciler/callback 实现 run-level async execution；terminal
  run 可在重派发后补 callback；deterministic diff/selection orphaned nonterminal 失败关闭，scope 仅从冻结
  manifest 与可验证 prefix 恢复；formal Pi 已复用其 stage
  ledger 作为 resumable authority，并消费同一 coordinator workload lease。跨进程 SIGKILL acceptance 已验证
  lease expiry、generation 2 接管、受控失败终态收敛和 generation 1 stale callback fence。deterministic scope 已有
  Argus-owned immutable shard inputs、并行 checkpoint、outer-generation fence、aggregate/fan-in 与非终态
  run prefix recovery；物理子进程测试证明只补 pending shard、最终闭合 65 个 Finding 并拒绝旧代迟到写。
  formal Pi 已增加 content-addressed group checkpoint、后代注入与 generation fence；revision 0 保存完整
  context+all-review，后续单调 revision 累积成功 candidate verification。TS/Go 测试已证明 exact candidate
  复用、失败 verifier 重跑、乱序 callback 收敛以及 candidate/revision tamper rejection。2026-08-27 真实
  DeepSeek 物理测试已在 revision 1 fsync 后切断 generation 1 lease owner，并证明 generation 2 复用已完成
  verifier、正式成功终态与旧代 callback fence。
  仍缺任意动态 stage fan-out 和 Hailix remote worker recovery。
- Hailix worker recovery 与低延迟 remote cancel wakeup；当前 `agent-review formal run/replay` 已能通过显式
  `hailix-http` backend、pinned verifier 和 request-time environment credential 调用 public consumer contract，
  但 Hailix 尚无对应 server endpoint。Experiment/Repeatability durable batch 已能冻结并恢复该 credential-free
  transport，当前 run workload 能永久 cancel、拒绝 stale generation
  callback，并把 lease fencing token 绑定到 stage evidence。跨进程本地 worker 已通过 heartbeat fence 将 cancel
  传播到 Pi runner context，但仍没有平台 attestation、push signal 或远程 runtime adapter。
- Hailix production workload-class resource pool、跨实例 tenant quota/fairness、平台 attested metrics 与
  saturation SLO；本地 scheduling 已执行四类 pool、global/class/tenant bounded queue、quota、priority/
  aging、公平调度和显式 reject/throttle，并可通过 versioned CLI/API pressure snapshot 查询，但这不是
  分布式容量证明。
- Hailix-owned durable worker service/outbox；当前 local coordinator 的状态与 fence 已持久化，但实际执行和轮询仍由
  `api serve` 进程内 goroutine 驱动。
- production typed result sink；Pi CLI 的 terminal tool 只证明本地 Agent 输出可结构化，
  不等于 Hailix/ACP 已提供正式 sink。
- Hailix provider execution 下的 ToolInvocationPolicy attestation、分类 retry 和
  unknown-outcome recovery；本地 deterministic/Pi runtime 的策略不能代替平台证明。
- Config hosted Web 管理面、租户级认证发布/灰度、远程分发/缓存失效和跨实例一致性；本地 RulePack、
  ConfigRevision/ConfigBundle resolve/publish/rollback/explain 以及 loopback bearer API 已存在。当前 process-fixed
  principal 只有 action permission，没有 Hailix/IAM 签名身份或 tenant/repository scope，不能开放公网。本地
  `/ui/` 已能浏览和执行 config lifecycle、解析 exact effective ConfigBundle/receipt，并能查看 Evaluation/Experiment/Repeatability 运行和
  Experiment/Repeatability batch 状态、lease/failure，提交 budget、exact published
  prompt/skill/knowledge、sealed RulePack、exact formal workflow stage budget、ordered built-in index provider Experiment 与 exact replay batch，并发起受审计 async resume，但不是 hosted
  管理面。平台已有 principal-bound prompt/skill/knowledge publication command 与 configured-provider
  model-only variant，仍缺签名供应链与 hosted registry，不能用 HTTP local path 代替。
- Agentic Evaluation/Experiment 的权威 cost/provider latency attestation、judge calibration 和真实
  shadow/canary；本地 ExperimentRun 已从 committed ReviewRun StageAttempt 台账生成成对总时延与
  delta；独立 RepeatabilityRun 已对 caller 提供的 direct exact-replay EvaluationRuns 生成 Finding-set
  Jaccard/exact-set/presence/anchor-hit 与 verdict-flip PPM，并让 partial report fail closed；逐 dimension
  还保存 Finding/verdict 稳定性与 execution/usage 范围。本地 durable ExperimentBatch 和
  RepeatabilityBatch 已分别实现逐 case 与 sample×case checkpoint、缺失单元恢复，以及基于 sequence CAS
  的 claim/heartbeat/generation/fencing/过期接管；两者的 durable intent 已绑定 credential-free exact Pi
  executor template，并有无参数 resume、drift/ref mismatch provider-before-fail 语义。executor template 现可冻结
  `hailix-http` endpoint 与 pinned verifier，CLI batch 复用 formal replay flags，local API 通过独立
  `formal-batch-*` server profile 选择 transport，credential 仍按请求从固定环境读取；仍缺真实 Hailix server。
  local API 已覆盖 budget、exact published prompt/skill/knowledge、ordered built-in index provider Experiment submit 与
  exact Repeatability submit，并提供 subject-derived component publication。deterministic workflow policy
  variant 已通过 exact definition/config binding、真实 scheduler consumption 和 terminal whole-closure 接通；
  deterministic 与 formal Pi index variant 已通过 fresh provider execution/context receipt、model-before admission、
  Evaluation target-equivalence 和 terminal closure 接通，
  formal `filter_policy` 已通过纯后处理路径接通 CLI/API/Batch，并兼容旧 `finding_governance`
  事实；formal `rule_pack` 已以 exact sealed bytes 接通 Pi context/review/verifier 与 CLI/API/UI/Batch；
  formal `workflow` 已只允许会改变有效 AgentStagePlan 的 exact 单阶段资源预算并接通 CLI/API/UI/Batch；正式
  run/replay CLI 与 Experiment/Repeatability durable batch 已接通 Hailix HTTP consumer composition，但仍缺 Hailix server、
  hosted component distribution/attestation。
  本地 label/exposure/holdout/promotion ledger 已存在。
- 隔离 Apply executor 与 Hailix/sandbox attestation。当前 ApplyTrial 已绑定 exact Finding suggestion、
  edit script 和 dry-run/compile/test evidence，并由 evaluator 重算 artifact digest/size；authority 仍是
  `local_host_unattested`，不能证明命令执行环境。suppression/filter truth 已使用排序唯一的 canonical
  candidate cluster fingerprint 闭合；localization 已使用结构化 LabelAnchor 和 source-digest-bound
  range overlap 闭合，partial report 不参与定位或过滤判断。
- 远程代码平台 Feedback/Outcome 归因与线上 ValueObservation；本地 ledger、analytics
  fact/projection 和 Parquet export 已存在。
- ReviewRun 已有 permission-gated local HTTP list/show、watermarked pagination、exact Finding drill-down，
  ReviewJob 异步触发，以及 process-fixed principal 注入的 Decision/Feedback/Outcome 写 API；按
  config/model/rule revision 的本地 append-only 增量反向索引、旧 ledger 显式 rebuild 和 exact impact API
  已闭合，仍缺 hosted tenant/repository-scoped UI/API、跨实例索引和生产迁移/retention。

## R-DATA-003 数据完整性剩余项

- local ReviewRun Artifact 的 quarantine/release/tombstone 与 publication/evaluation/training/export
  eligibility API 已闭合；剩余是 PostgreSQL 元数据与生产 ObjectStore bytes 的双向 checksum
  reconciliation、修复副本 provenance、批量扫描/告警和受认证 integrity operator。
- Hailix 拥有 Trace sequence/integrity。Argus 仍需等待其公共 TraceManifest/segment contract 后保存
  授权 ref 和 quarantine projection；不会读取 Hailix 私有存储，也不会用 Argus local JSONL 充当
  Trace sequence 证明。
- training 已有 fail-closed reference-only materializer 和 strict-text exporter：前者闭合 EvaluationCase
  governance/label revision、consent/license、独立 authority、exact ref 与 eligibility；后者闭合固定版本 detector、
  leakage corpus、内容寻址 redacted artifacts/receipts、restore replay 和 store 外原子 portable JSONL publish；
  本地 Operator UI 已接通 manifest/export/job 历史和 materialize/build/prepare/observe，但按设计不开放 publish
  或 provider remote execution。
  剩余是通用 DLP/secret-manager 或平台 attestation、生产 ObjectStore/IAM/tenant scope、retention/删除传播、
  provider-specific dataset adapter/受控 executor、签名 provider attestation/billing receipt、训练产物 registry，
  以及训练后独立 evaluation/promotion bridge。当前 external-manual JobPlan/receipt 只做不可 promotion 的本地追踪，
  fixed detector 也不宣称覆盖未知秘密格式。

## R-EVAL-001 评测来源剩余项

- human publish Decision、production accept/dismiss Feedback、exact reviewed bug-fix pair、独立 incident
  missed defect，以及 mutation/synthetic defect/clean/workflow invariant Probe 已能派生 candidate-only Case；
  source-specific builder 的本地闭环已完成。仍需真实规模 corpus、外部 source/oracle/executor attestation、
  production ObjectStore 与受认证 ingestion transport。
- candidate 到 gold/active 的最小本地治理已实现：至少两个 distinct reviewer annotation、第三方
  adjudication、只从已复核 label 中选择、先 gold 后独立 activation，并以 revision/CAS/idempotency
  失败关闭；mandatory blind assignment、reviewer read isolation、event-derived agreement 和 reopen round
  已接通。direct create 已限制为 candidate ingress；外部已治理 gold/active Case 有 Ed25519 exact-case/
  evidence/policy attestation、repository/classification/time-scoped frozen key、独立 import operator 和
  restore revalidation。key 需由独立 governance_trust_admin 预注册，active exact revision 才能 import；
  append-only revocation 只阻止后续导入，CLI/list/restart recovery 已闭合。仍缺 reviewer/adjudicator/
  trust-admin/import operator 的 Hailix/IAM 签名身份与平台 trust root、hosted 治理 UI、生产 ObjectStore；
  本地 registry 不能证明 actor 身份由平台认证。loopback HTTP adapter 已阻止 request 自报 actor/roles，并
  提供 Case/batch/trust-key 的 bearer-authenticated 本地 transport；ReviewRun list 单独使用 run-index
  watermark cursor，但 Evaluation marker cursor 和同步 handler 仍不是多租户 IAM、冻结评测集或异步 job API。五类 homogeneous batch 的
  intent/checkpoint/partial terminal/crash resume 已由本地 service/CLI/HTTP 闭合，但不承诺跨 item 原子回滚。
- normalization oracle 已有 scoped Ed25519 signature、预注册 trust-key、append-only CAS registration/
  replacement/revocation、exact quality binding、restore revalidation 和 revoke/drift fail-closed；test/holdout
  split 的 managed promotion gate、不可变阈值 policy/decision、exact baseline ConfigRevision 派生、
  shadow/canary run 重验、authorization/rollback-monitor、单调 activation/rollback、CLI、本地 API 和 embedded UI 已闭合。
  剩余是 Hailix/IAM 认证 reviewer/adjudicator/trust-admin/operator 身份、hosted transport/UI、真实规模
  dev/test/holdout corpus，以及真实流量的 shadow/canary 执行与阈值验收。仓库仍没有足量 independently
  governed normalization case，示例阈值不是生产验收值，也不能据此声称总体 unique-defect precision/recall。
  runtime 已不再硬编码 v2：ConfigBundle、AgentStagePlan、worker plan、Pi snapshot/checkpoint 与 Go evaluator
  共同消费 `candidate-normalization@v0|v1|v2 + exact worker SHA-256`，未知 revision、digest drift 和跨版本
  checkpoint 均失败关闭。promotion managed binding 也已冻结候选/回滚 exact selector 与同一 worker digest。
  机制已能从当前 baseline 创建/validate 候选 ConfigRevision，以 percentage canary 发布并由真实 ReviewRun
  证明命中，再用同 seed 扩到 100% 或恢复原 publish frame。剩余缺口是实际受治理样本、真实 shadow/canary
  运行、Hailix/IAM 身份和远端 attestation；当前测试闭环仍不能被解释为真实上线。

## M1.3 Pi shadow bridge 缺口

- shadow direct 与 formal local Pi 均已将 exact prompt、结构化 context/review/verifier output 与
  typed tool transcript 作为 bounded `exact_local_sensitive` Artifact 入库；shadow 与 receipt
  digest/counter 闭合，formal 通过 StageExecutionResult、host-localization、terminal bytes 和
  ReviewRun ref 闭合。两条路径都缺 provider/Hailix attestation，负向搜索也只有 worker tool transcript 而无独立证明；
  因此仍固定 `non_replayable/non_attested`，本地 manifest 不能冒充 Hailix runtime attestation。
- shadow/formal task evidence 已有本地敏感 use/role、默认 JSON 去敏、purpose-bound audited read
  和 irreversible logical tombstone；formal access 从 committed terminal whole-closure 推导
  governed ref/scope，tombstone 后 manifest/receipt/analytics/ReviewRun 历史保持可查。剩余
  retention 缺口是由产品/合规确定 TTL、跨 ref 引用计数和可证明的物理 GC，以及 hosted
  IAM/KMS/OS isolation。当前
  `until_explicit_revocation` 不是法务意义上的删除 SLA，磁盘管理员仍可直接访问本地 store。
- 继续扩展 Go host -> strict worker -> live DeepSeek Anthropic-compatible endpoint ->
  shadow import 验收矩阵。2026-08-26 已完成 known-defect、clean、context-partial、真实
  provider timeout 与无效凭证 auth-failure canary；auth 失败以 failed manifest/非零 CLI
  收敛，synthetic dependency cancellation 的缺失 prompt transcript 显式标记
  `task_evidence_unavailable`。同日 opt-in physical canary 已在完整六维 review checkpoint 后 SIGKILL
  Go/Node worker，由 generation 2 复用 context/review、重跑 verifier 并成功闭合。2026-08-27 又完成
  candidate-verification revision 1 的真实 provider 物理 canary：generation 2 复用已完成 verifier 并成功闭合。
  仍缺真实仓库 corpus
  上的重复运行、质量/稳定性/成本统计；这些 local direct-provider 证据仍不能替代
  Hailix/provider attestation。
- formal PlatformPort 已用真实 DeepSeek 完成 known-defect、zero-candidate partial、exact retry 和无效凭证
  failure：success 至少要求一个 group 完成全部 review dimensions，0 reviewed group 不再 fail-open。
  `evaluation corpus` 已提供可重复本地 runner：`CorpusSnapshot` 先冻结 Case/source execution closure，并已
  强制 purpose/single-split、clone-group 去重及 holdout 历史 exposure fail-closed；随后
  只接受 governed active Case 作为独立 oracle，整体 preflight 后记录 exposure，以稳定 key 执行 formal
  review、逐 Case checkpoint 并自动生成 EvaluationRun；当前 E2E
  使用 fake provider 验证两 Case、配置复用与 retry/resume。单 run 的真实 DeepSeek provider SIGKILL
  success-resume 已闭合；Hailix 单历史缺陷已给出一次真实 inconclusive baseline，并暴露/修复 repository
  search 超时；selection search v2 已收紧到 exact range + 32-line halo，但仍缺未污染的
  train/dev/holdout、多仓 precision/recall/repeatability/cost，以及
  provider/Hailix attestation。
  zero-candidate partial 不是 proven-clean label。
- rename-aware diff 与金额 budget。committed formal receipt 已提供带
  `worker_self_report_diagnostic` authority 的 input/output/cache/reasoning/total token facts 和 paired
  delta；金额、provider bill 和成本在 Hailix authoritative billing 接入前保持 unknown，不从
  PricingCeiling 或 worker self-report 推算。
- AgentExecution diagnostic analytics 已有 immutable rebuild/query 与 JSON/CSV export，
  并已为 succeeded/failed/canceled/unknown-outcome 建立互斥状态计数；execution history
  查询仍是有界 local store 扫描；attempt-aware rebuild 已使用单批 strict execution lookup
  与一次性 execution-id 索引，消除了逐 result 重扫 intent store。仍缺服务端增量索引、
  pagination/latest、Parquet、
  retention/GC 和 hosted dashboard。没有 execution intent 的 legacy direct Import 当前会
  保留，有 attempt 但当前归属其他窗口的 import 会在单次 rebuild 中过滤。旧版 projection
  snapshot/manifest 与当前 v1alpha3 显式不兼容，必须使用新的 snapshot ID 重建；生产级
  migration、retention 和 version negotiation 尚未设计。它永远不能自动写入正式
  Finding/Value/ROI funnel。
- Context Provider 本地 receipt 已进入正式 analytics/dashboard，能够区分执行与 replay
  复用并导出 typed Gap reason/latency；仍缺 Hailix authoritative Trace/Task attestation、
  服务端增量时序、跨实例 provider SLO/告警和 hosted drill-down。
- host-failure observation 与 pre-authorized committed-import reconciliation 已完成本地闭环；
  authorization 在 import 前持久化，避免 import 已提交但 post-link 尚未落盘的不可恢复窗口；
  剩余生产缺口
  是增量/分布式索引下的 crash recovery 调度、retention 与跨版本迁移，而不是重新运行
  provider。当前 observation 只保存固定 taxonomy，不保存 raw error/stderr/credential；
  reconcile 只接受 exact pre-authorized committed result closure；只有 authorization、没有
  committed result 时仍保持 `unknown_outcome`。
- AgentExecution analytics 当前是 rebuild-time current projection，不是 transition event
  ledger。reconcile 会改变后续 rebuild 的窗口归属，但既有 immutable snapshot 保留旧状态；
  生产历史统计需要 immutable state-transition facts、as-of watermark 与跨 snapshot 去重，
  不能直接累加不同构建时点的窗口快照。
- 当前 `internal/agentshadow` 仍是 local single-user adapter；CLI 固定 `local/local`，但
  internal Import/Query port 的 scope 仍由 in-process caller 传入。暴露 Web/API 或接
  Hailix 前必须由认证上下文注入 Subject，或把 Service 实例固定绑定到授权 scope，禁止
  外部调用方自行声明 tenant/workspace。

## Formal Agent Stage production/terminal projection 缺口

- 已冻结的 ContextRef Artifact 已有 CLI import、governed resolution、exact worker transport 与 Pi
  consumption；显式本地 `repository_search`、`go_ast`、`go_dependencies` 与 compile-only
  `go_compile` provider 已从 exact Git object 自动生成 lexical repository matches、
  declaration/type/direct caller-callee/三跳 exact local-module call path、module/package/import 与 compile diagnostics facts、coverage 和 gap，并支持
  diff/selection/scope。application port 现在会解释 invocation 或 published ConfigBundle 的
  `execution.context_providers`，以冻结的独立并发上限执行 exact adapter，
  将 missing/mismatch/timeout/overlay/capture/output failure 变成 request-bound ContextGap。compile provider
  只执行 `go test -c`，不会执行 Test/TestMain/init；它关闭 ambient module/network/workspace/cgo/toolchain
  fallback，对 embed/cgo/local replace/`.syso`/不完整输入保守 unavailable。repository search 在读取前
  拒绝 credential-sensitive path，并明确限定为 lexical evidence。仍缺 Hailix sandbox 中的真实
  test execution、interface/dynamic call graph、CodeGraph/LSP semantic adapter、provider 多 attempt/generation/
  recovery 与远端 attestation。当前每次执行已有 config/request/target/outcome/timeout/latency-bound
  local receipt 并进入 ExecutionSnapshot，但不能描述成 Hailix/远端平台签发的执行收据。

- `AgentStagePlan` admission、canonical request、dispatch/claim/binding、callback receipt、terminal
	gate、Hypothesis evidence、本地 Pi PlatformPort 和 bootstrap/run/show CLI 已接通；Argus 侧
	Hailix PlatformPort adapter、严格 HTTP client、request-time credential port、machine schema 与
	formal unknown-outcome recovery E2E 也已完成。尚缺 Hailix 服务端 public implementation、真实
	credential broker/IAM trust root、typed Artifact/Trace 交付、provider atomic idempotency 和 runtime
	attestation；因此不能称为 production Hailix integration。
- 本地 `AgentComponentResolver` 已有 content-addressed subject-bound registry/resolver；生产
  tenant authorization、cache invalidation、retention 和跨实例一致性仍未实现。
- formal governed task-evidence source 已拒绝普通 read/export，并有显式 audited read/revoke CLI；
  terminal-local content-addressed copy 仍为 immutable whole-closure 保留且不会被逻辑 tombstone
  物理 unlink。缺少 reference-aware GC、TTL/KMS/hosted IAM 前，不能声称全生命周期物理删除或
  合规保留 SLA。
- S1 已允许 formal same-input exact replay 和 budget/model/prompt/skill_pack/knowledge_pack/rule_pack/workflow/index/filter_policy-only variant：source/root/namespace/change set、
	源或派生 ConfigBundle/receipt、runtime evidence 和 remote deny 都进入闭包；budget 只允许原子改变
	stage/Pi timeout，model 只改变 model component 与 manifest build identity，prompt 只改变 exact
	prompt component 与对应 build identity，skill_pack 只替换同 ID/phase/order 的 exact governed
	review-skill Artifact；workflow 仅允许 exact 单 stage timeout/input/output/concurrency 预算变化并将逐项最小值写入 Plan。
	执行层已有受限 Pi group/candidate-verification checkpoint，但仍拒绝 upstream dependency、
	任意 stage checkpoint reuse、新增/删除 skill以及 budget/retry 之外的 formal workflow 变化。multi-attempt
	retry 已把 effective max、backoff、错误码白名单、attempt generation/idempotency/unknown-outcome 加入
	Plan/Attempt binding；仍缺 Hailix 服务端原生 retry/cancel runtime 与远端 attestation。
- prompt Artifact 的 operational closure 与单变量 replay 已完成：formal PlatformPort resolve exact bytes，
  worker envelope/TS decoder 复算 digest/size/revision，Pi runtime 实际消费三个 role prompt 与
  finalizer，exact task evidence/snapshot 回显同一 identity；prompt-only derived ConfigResolutionReceipt、
  variant classifier/initializer/compiler/repository/CLI 与 baseline-vs-variant E2E 已闭合。下一步变量
  扩展集中在 workflow 及正常配置发布的新 skill 维度，不复用 caller-supplied config ref 绕过治理。
- review skill Artifact 的 operational closure 与 `skill_pack` 单变量 replay 已完成：host 从 Plan
  subject-bound Artifact 解析每项 exact bytes，worker 严格校验顺序/ref/contract/size/base64/UTF-8/SHA
  并直接构造 runtime SkillDefinition；derived receipt、initializer/compiler/repository/CLI、exact retry
  和同键异值 provider 前冲突已有 E2E。为了保留 FieldSource provenance，新增、删除、重排或 rephase
  仍必须走正常 ConfigRevision lifecycle，而不是 replay。
- governed KnowledgePack 的 operational closure 已完成：formal bootstrap/run 可用 repeatable
  `--knowledge` 发布 exact repository/business-line reference，Plan/manifest/build identity、worker
	transport 与 snapshot 共同闭合，并由 Pi runtime 实际消费。`knowledge_pack` 单变量 replay、
	baseline/variant lineage、exact retry 与同键异值冲突也已闭合；剩余是新增/删除 knowledge 的正常
	配置生命周期与更大规模 evaluation runner，而不是退回 ambient 文件加载。
- 默认 Pi review pack 已拆成六个 versioned dimension，并通过 fake runtime 证明每个维度
  独立产生 reviewer task、候选和 verifier task；这只证明执行与证据隔离，不证明缺陷发现
  效果。EvaluationRun 已支持 evaluator 显式声明 exact dimension applicability，并从 committed
  Candidate/Finding/receipt 记录每维 verdict、localization、review/verification task、turn/tool、
  cumulative duration 与 attributable token；ExperimentRun 按相同 dimension ID 保留两侧 exact
  revision 并生成差值，analytics 也已有受控 `review_dimension` token/duration facts。formal
  ReviewRun 已引用 exact raw-candidate collection，Evaluation/Experiment 已产生逐维 normalization
  整数及 paired delta；Dashboard rate 已固定 raw 分母、scale 与 availability 语义。context gap 已按
  committed task/group evidence 精确绑定到 review dimension；仍需建设足量
  active/holdout corpus 后才能得到
  verified precision、miss rate和业务线增益，再通过正常配置 lifecycle 灰度或回滚默认 pack；
  未经治理的 Argus 自生成 Finding 不能充当这些 gold label。
- `ConfigResolutionReceipt` 是本地 lifecycle 治理记录，不是签名或 Hailix/远端 attestation。
  application 已要求独立 `AgentPlanningSubject` 闭合 tenant/organization/workspace/repository，
  但生产环境仍需由 Hailix/IAM authenticated adapter 签发或注入该 subject、服务端 repository
  authority、跨实例一致性与平台 execution attestation；不能把 caller 构造的内存投影或
  receipt 自校验当作远端身份/发布证明。
- 本地 dispatch claim 与 terminal admission 已使用同一 append-only generation stream 做跨进程
	CAS：后代 claim 与旧 completion 并发只允许一个赢家，exact retry 复用同一 decision。它仍不能
	把本地 claim 与远端 provider Create 合成一个事务；Argus HTTP client 已把 mutation transport/
	5xx/invalid-success 保留为 unknown outcome，并由 durable intent exact retry 恢复，但真实远端仍需要
	provider-authoritative atomic create-or-return-one-handle/idempotency 与 outbox，不能由 consumer
	test 声称平台 exactly-once。
- formal callback、failed/canceled typed sink 与 `ReviewHypothesisSet` ingress 已完成；下游现可
	生成独立、可查询并保留全部 canonical Candidate 的 `GovernedCandidateSet`，以及单独内容寻址、
	由 ReviewRun/Evaluation/analytics 绑定的 `CandidateVerificationLedger`；Candidate/Finding verdict
	仅作为 ledger projection。系统只提升 confirmed Finding、并固定 `queued_for_human` 的
	`GovernedReviewReport`，提交 unified terminal `ReviewRun`，且已贯通 EvaluationRun/ExperimentRun 与
	Dashboard funnel/diagnostic token delta。后续人工 Decision ledger 已接通 legacy/formal source root、CLI
	与严格恢复，并作为 provider-neutral publication request builder 与 mandatory dispatch authorizer 的
	最新授权来源。独立 post-review PublicationGrant 已用 append-only exact binding、最长 24h 生命周期和
	单次 durable reservation 闭合；评审 ExecutionSnapshot 继续 deny remote write。独立
	Calibration/Suppression ledger 已进入 terminal/Evaluation/analytics closure；版本化单调分段线性 profile、
	blind raw confidence、整数 PPM、threshold/max-findings 排序决策和全部 loser 保留已闭合。受治理
	calibration fitter 已实现 exact candidate-level 独立真值、train/dev-test 隔离、整数 PAV、
	overall/repository/dimension Brier/ECE/drift gate 和不可变 profile candidate/report。activation 前后 exact
	Dashboard snapshot 的不可变质量窗口、整数阈值、partial/样本不足状态与人工 rollback recommendation 已实现；
	仍缺足量真实 corpus 校准有统计意义的门限、连续调度/通知以及 repository/dimension 业务线长期告警。passed profile 到 ConfigRevision、单变量 experiment、
	独立 holdout、显式 activation/rollback 的操作闭环已实现；仍缺真实生产 corpus 上的物理验收与长期漂移证据；
	lineage 已有 relation/method/policy 分布 Dashboard，仍缺足量真实跨时间 corpus 的稳定性基线、漂移门限和告警；另缺可重放的
	normalize/verify/adjudicate stages、通用 current-cancel service。GitHub provider adapter、PR identity、
	base/head/diff anchor revalidation 与 unknown-outcome fault E2E 已完成；仍缺生产 credential source、认证
	permission broker、生产 dispatch API wiring、live GitHub canary，以及 GitLab 等其他 provider；显式
	single-user-local dispatch/reconcile CLI 已完成，但不能作为多租户 IAM 证据；formal
	Finding 的平台 UI/API Feedback/Outcome 已接通 control-plane、CLI 和 analytics，但代码托管平台来源
	仍必须等待真实 publication evidence。本地 Finding 或 Feedback 不表示已发布、已修复或成为 gold label。

## M1 后续加固

- 将 stale target、patch/file partial、false positive、no finding、duplicate、
  cancellation 和 replay tamper 场景进一步固化为独立、可重复的 CLI acceptance
  fixtures；当前 focused Go tests 已覆盖这些核心失败语义，且本地纵向基线已通过
  `make verify`。这些是持续加固项，不阻塞 M1 本地可用结论。

## Hailix 集成缺口

- Argus 侧 `internal/hailixexecution` 已实现 capability、exact Ensure/Lookup/Cancel、typed
  terminal 与 pinned callback-verifier consumer contract；executor trust roots 已绑定 capability/request
  identity，fresh/recovered callback 都从 persisted request 取 trust 并拒绝 drift。当前缺口是 Hailix public API、
  认证 transport client 和跨仓 contract fixture，不是再造 Worker Runtime。
- immutable platform execution input binding；当前 DirectTaskInput 只能 smoke，不能
  绑定 TargetSnapshot/stage Artifact/schema/checksum。
- Eino-Agent ACP adapter；当前只接受 Codex。
- typed Artifact registry；当前只接受 report/patch。
- 公共 TraceManifest/Trace content API。
- 外部 EventCenter subscription/relay；当前主要是内部 Outbox/Projection。
- 动态 tenant/IAM/Workspace provisioning；当前是 single-user local。
- 共享模型 API；当前 LiteLLM 是嵌入式 runtime component。
- 当前源码、migration 和在线部署版本需要在正式联调前冻结一致。

## Eino-Agent 集成缺口

- 恢复或定位 canonical source checkout；2026-08-25 当前 `../eino-agent` 只有 `.yhc`
  会话数据，无法刷新源码审计或运行跨仓 contract test。
- review-safe composition profile。
- hard checkout containment 和 absolute path denial。
- 关闭 project/user hooks、MCP、plugin、skill/instruction 扩权。
- schema-constrained ACP result 或正式 typed result tool。
- tool permission 中的 exact input/identity。
- background child 与 parent cancel 一致性。
- 完整 ACP event/stop reason/sequence/lineage。
- canonical PR diff 由 Argus 提供，不能依赖有截断的 `/diff`。

## 证据缺口

- 旧系统真实 gold dataset 规模、分布、标注一致性和 judge 误差。
- 可信线上 precision/recall、单位成本和 ROI。
- full scan 是否达到目标规模并具备 cancel/recovery。
- 配置平台、端到端 replay、收益看板是否曾完整投产。
- 反馈中的 `wont_fix/outdated/resolved` 与真实标签的映射。
- 模型、CodeGraph/index 陈旧对质量的量化影响。
- `repository_search@v2` 的 terminal evidence 已在 worker 接受前执行可纠错 frozen-source 校验，并由
  Go host 二次重验；新的未曝光 synthetic dev case 已从 target-only 0 Finding 提升到 index-only 2 个
  confirmed Finding，且越权引用 context 文件与错误 excerpt 会作为 terminal tool failure 留证并由模型
  修正。两个 Finding 实际描述同一状态迁移缺陷；当前已增加 conservative、host-recomputed 的跨 dimension
  `semantic_duplicate` 并通过正反例。exposed-case physical regression 的 180 秒 run 以
  `deadline_exceeded` 失败；600 秒 baseline 成功但显示 target-only 结果由 0 漂移为 1 Finding，随后 index
  replay 以 `pi_no_review_completed` 失败。隔离的 governed failure-diagnostic closure、terminal-only
  projection、脱敏 show 与 purpose-bound exact read 已完成，且失败 ReviewRun 不吸收 task/receipt 引用；
  第一次新实现 physical replay 已由 immutable checkpoint 定位到 Candidate 标题中 `>` 触发的 TS/Go
  fingerprint JSON escaping 漂移，同时暴露 diagnostics 仍晚于 business mapping；两项已修复。随后 physical
  replay 成功闭合 11/11 task，却因 4 个 reviewer 的标题措辞差异把同一状态覆写根因保留为 4 个 Finding。
  第二版 host-recomputed title relation 与 `workflowRevision=v1` 已实现并通过正反测试；physical run 将
  5 raw 降为 3 canonical/verification/finding，证明部分有效但仍漏并。v2 已增加 shared identifier/evidence 后的
  root-cause description 55% overlap gate，并用该 run 的三条 exact claim 形态通过 worker/host 正反测试；
  normalize-preview 已对 committed v1 raw evidence 得到 5 raw -> 1 diagnostic cluster，且 prior invalid/budget
  claim 不会被提升。v2 physical run 已闭合 4 raw -> 1 canonical/verification/finding；content-addressed
  normalize comparison 也已对 v1/v2 两个 committed run 形成 paired artifact，并证明 v2 recorded/preview
  0 decision delta。剩余缺口是把这类 diagnostic batch 绑定到预注册、受治理的真实 corpus 和独立 oracle，以及
  足量预注册 oracle 下的误合并率、unique-defect recall/precision 与重复性评测；
  该 synthetic 结果不能作为 promotion evidence。
  当前已具备 CorpusSnapshot-bound NormalizationOracle、pairwise quality evaluator 和 content-addressed
  seal/run/show，但 oracle reviewer/adjudicator 身份仍是 artifact 内声明，尚无独立 append-only registration、
  signature/CAS/revocation ledger；真实 dev/test/holdout case 数仍为零。

旧资料或求职总结中的百分比和倍数不能填补这些证据缺口。
