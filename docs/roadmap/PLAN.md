# Plan

本文件只记录已接受的执行顺序。缺口但未进入顺序的事项放在
[REMAINING](REMAINING.md)。

## M0 Foundation

1. 冻结需求、领域边界和三仓 ownership。
2. 建立 ReviewSpec v1alpha1、严格样例、DAG validator 和工程门禁。
3. 建立安全基线：remote writes deny、repository config untrusted、replay no side
   effects。
4. 以独立 review 检查文档/代码一致性并运行 `make verify`。

## M1 Local Vertical Slice

1. 实现 Git source adapter：
   - exact base/head；
   - canonical patch；
   - file/language/coverage manifest；
   - skipped/partial/context-gap reason。
2. 实现本地 append-only store：
   - ReviewRun/StageRun；
   - ExecutionSnapshot；
   - ArtifactRef/Argus local stage-log JSONL；
   - Finding/Decision。
3. 用 deterministic fixture detector 跑通：
   `materialize -> detect -> normalize -> verify -> adjudicate -> report`。
4. 实现 stage checkpoint、retry、cancel 和 same-input replay。
5. 实现 `argus review`、`argus replay`、`argus compare`，所有远程副作用保持 deny。
6. 建立最小 evaluation fixtures：真阳性、误报、无 Finding、partial、stale、
   duplicate 和 cancellation。

M1 必须让 production path、offline evaluation 和 replay 共用同一 Workflow
implementation；禁止另写一套 evaluator。

当前 1-6 已按本地 deterministic 基线落地并通过全量门禁。完成依据不是
“命令能运行”，而是
[M1 本地纵向链路](M1_LOCAL_VERTICAL_SLICE.md)中的 exact target、证据闭包、
checkpoint replay、比较、partial/cancel 和 no-side-effect 检查全部通过，并重跑
`make verify`。marker baseline 只用于验证这条执行路径，不计作真实 detector。
后续已在不改变阶段 owner 的前提下把 local artifact chain 扩展为
`materialize_target/plan_context/detect/normalize/verify/adjudicate/report/publish/
capture_feedback/export_evaluation`，并新增一条冻结源码 Go AST detector；后三个领域阶段
仍只是 remote-disabled/awaiting/candidate-only lifecycle 投影，AST + marker 仍不等于
M2 的生产多 detector 组合。

Finding lifecycle 的下一段已增加 source-rooted append-only human Decision ledger 与
`decision record` CLI；它不修改源报告，也不把 Feedback 当作发布授权。最新 `publish` Decision
现已绑定 provider-neutral publication request，并作为 dispatch mandatory authorizer 二次校验。
独立 post-review PublicationGrant ledger、最长 24h exact binding、单次 reservation、严格恢复及 CLI
已落地；review runtime 和 replay 仍保持 remote side effect deny。下一步按 R-CORE-006 接真实
GitHub adapter 的 exact PR/base/head/anchor、comment create/marker lookup 与 unknown-outcome fault E2E 已
落地。显式 single-user-local dispatch/reconcile CLI 已用固定 env、固定 permission revision、secret
redaction 和 full-store leak scan 闭合本地 MVP。下一步按 R-CORE-006 接 Hailix 公共 credential/IAM
服务、生产 dispatch API 和 live canary；不 import Hailix internal secret storage。

## M1.2 Agentic Review CLI

1. 在 `runtime/pi-review` 建立隔离的 TypeScript CLI，以固定版本 Pi Agent SDK 作为
   单个 Agent 的执行内核，不复制 Hailix 调度或修改 Go 平台语义。
2. 支持 working changes、exact commit diff 和显式 files；先冻结目标、执行确定性
   分组，再并行运行 context collector 和多 Skill reviewer。
3. 将 reviewer 输出保存为 Candidate，由独立 verifier 判断；只有 anchor 覆盖变更且
   Candidate/verdict 的结构化源码证据都能被宿主回读验证时才生成 Finding。
4. 只开放有界只读工具；显式加载额外 Skill/Knowledge；对敏感路径、symlink 越界、
   工作区漂移、超时、partial coverage 和子进程凭据继承失败关闭。
5. 在首次模型请求前执行文件、分组、Candidate 和 output-token 准入；每次 Pi
   `streamFn` 调用原子扣减全局 provider/model turn 预算，并由 `--plan` 分开显示
   Agent task 与 provider turn 上界。
6. 将 TypeScript format/typecheck/test/build 纳入根 `make verify`，用 fake provider
   覆盖真实 Pi tool-submit loop，再用显式、受限的 provider profile 完成最小真实缺陷
   smoke。

当前 1-6 的最小纵向链路已完成：`anthropic-official` 保持默认，另以
`deepseek-anthropic-env@1` 显式允许当前 DeepSeek Anthropic-compatible endpoint；真实
Pi context/review/verifier 链路已在已知缺陷 fixture 上产出带宿主回读证据的 confirmed
verdict，clean fixture 产出 0 Candidate。两次受限 smoke 都因 context gap 正确返回
`partial`，因此它们证明“链路可运行”和覆盖状态诚实，不证明完整覆盖、生产隔离或
真实数据集上的 precision/recall。live timeout/auth failure 仍需在 M1.3 集成验收矩阵
中补齐。

本地 review pack 的下一轮职责拆分也已完成：默认六个 builtin dimension 分别负责
correctness、concurrency/data、error/outcome、resource lifecycle、security/contract 和
transaction/durable state；revision 从 Markdown 元数据解析并与 exact SHA-256 一起进入
standalone/shadow/formal identity。Go/TS/Schema 上限扩为 16，给正常配置发布的业务 Skill
留出容量；admission 会在 context/review base task 外为全部 retained Candidate 预留一次
independent verification task，默认 provider/model turn budget 调整为 96。下一步不是继续
增加 prompt 数量，而是用治理后的 evaluation corpus 分维度量 precision/recall/coverage，
再决定默认 pack 的灰度、合并或回滚。

## M1.3 Pi Shadow Review Evidence Bridge

1. 将 Pi 输出冻结为独立的 Agent Review 领域契约：
   - AgentReviewPlan；
   - ReviewHypothesisSet；
   - AgentExecutionReceipt；
   - AgentReviewResultManifest；
   - AgentReviewObservation。
2. 保留 raw hypothesis 与 normalize/dedup/verify 决策，不把 Pi 输出直接转换成现有
   deterministic `CandidateFinding`，不进入反馈真值、gold、promotion 或远程发布。
3. CLI 生成 diagnostic-only ExecutionSnapshot 和逐 task receipt，记录 target、配置、
   Skill/Knowledge、prompt/runtime/provider/budget digest 及 turn/tool/token/latency；另以
   bounded `exact_local_sensitive` collection 保存 exact prompt、结构化 task output 和 typed
   tool arguments/results，预算截断必须留下 digest/size/reason。永不保存 credential 或
   assistant reasoning，也不把该 collection 当作 provider/platform transcript。
4. Go host 严格校验 contract、artifact binding、target/anchor/evidence、execution
   identity 和输出限额后，先写内容寻址 artifact，再追加幂等的 shadow observation。
   本地 stdio envelope 必须校验 attempt/generation/fence/idempotency/capability；future
   Hailix adapter 还需消费平台 attestation。TS worker 不直接写 Argus store。
5. 本地 direct-provider 运行固定标记 `worker_self_report`、`diagnostic_only`、
   `shadow_only` 和 `non_replayable`；现有 `StageExecutionRequest network=deny` 不放宽，
   也不伪造 Hailix Trace/attestation。
6. 新增 shadow import/show 与 diagnostic analytics fact，覆盖 defect、clean、partial、
   timeout、auth failure、无效/伪造证据、重复导入和同 key 冲突。
7. Hailix 接入时由 Hailix 拥有 Task/Worker、credential、provider egress、Trace/Artifact
   和 authoritative usage/cost；Argus 只通过 anti-corruption port 消费，不在本仓复制
   通用 Worker Runtime。

本阶段先完成本地 shadow evidence bridge，再进入 M2 的可接管执行。它建立可追踪和
平台导入边界，但不声称已经可重放；冻结输入和 actual prompt/tool transcript 已入库，只有
provider execution evidence/attestation 与完整 lifecycle/replay closure 也闭合后，才允许升级
replayability 状态。

当前 M1.3.3 已完成本地 direct-provider shadow 纵向链路：strict worker envelope、
frozen-input TypeScript worker、Go subprocess/intent/unknown-outcome 控制、Pi report 到
Hypothesis/Receipt 的 host mapping、shadow import/show、committed-only result List，以及
覆盖 `succeeded`/`failed`/`canceled`/`unknown_outcome` 的 v1alpha3 execution attempt
history。completion 分离 worker `completed_at` 与 host `recorded_at`，terminal Attempt 只按
后者归属窗口；unknown 可绑定 bounded/redacted host-failure observation。LocalBridge 在
import 前用 immutable authorization 绑定 intent/runtime 与 canonical import provenance，只有
该 authorization 对应的 exact committed-import crash window 可幂等 reconcile；只有
authorization、没有 committed import 时仍为 unknown，且两种情况都不重跑 worker/provider。
隔离的 AgentExecution
diagnostic facts/projection/JSON/CSV CLI 已整体使用 v1alpha3；它对成功结果保留
task/tool/usage，对其余三类只建立 host-observed 稀疏事实，
保留无 execution intent 的 legacy direct Import，并过滤归属其他 attempt 窗口的 import。
attempt-aware rebuild 使用一次 batch execution lookup，不再逐 result 重扫 intent store。
旧版 projection snapshot/manifest 会显式拒绝，必须用新的 snapshot ID 重建。根门禁
包含 fake provider tests 和不调用 provider 的真实 Go <-> built worker 协议往返；CLI 的
嵌套 help 也已统一为成功退出并显示完整 usage。

M1.3 的 raw candidate 与 task evidence bounded governed payload 已完成：shadow import、
Manifest、查询、幂等输入与 crash reconciliation 均绑定独立内容寻址 collection，formal result
也已持久化同一 task evidence。敏感治理的本地 MVP 已完成：普通 read/export deny、内部
process 与显式 disclosure 分权、generic JSON 去敏、purpose-bound access receipt、显式
tombstone revocation，以及 tombstone 后的历史只读投影。剩余项收窄为补
provider/platform execution attestation、合规 TTL/KMS/hosted IAM 和 reference-aware GC，
形成可重放 closure；使用可用凭据完成 Go -> worker -> live provider -> import 的
defect、clean、timeout/auth 矩阵；定义 v1alpha3 diagnostic projection 的生产迁移、保留与
版本协商策略；将 local
caller-supplied scope 升级为 authenticated Subject；按第 7 项另立跨仓任务接 Hailix。
上述剩余项完成前不得升级 `non_replayable`、`non_attested` 或 `diagnostic_only` 标记。

R-DATA-003 的本地 Argus 子集已完成：run Artifact 有独立 append-only quarantine/release/
tombstone ledger，checksum 失败自动隔离，release 双重重验，publication/evaluation 显式接入统一
eligibility gate；reference-only training materializer 现同时执行 training/export gate、内容重验、Case/
license/独立 label authority 与 strict-redaction ConfigBundle admission，并保存内容寻址 manifest。其下游
strict-text exporter 已实现固定版本 detector、leakage corpus、redaction receipt、restore replay 和 store 外
原子 portable JSONL publish；provider-neutral JobPlan 又冻结 exact export/model/参数，并以不可 promotion 的
unattested receipt 追踪外部 submitted/terminal 状态。本地 Operator UI 已复用同一 API 接通清单/导出/作业
历史与四个受治理动作，且不开放 filesystem publish 或 provider execution。下一步不再扩写另一套本地状态机、把 fixed detector
冒充通用 DLP，或把 operator observation 冒充 provider attestation，
而是等待生产 ObjectStore/PostgreSQL 和 Hailix 公共 Trace contract 后补跨存储 checksum reconcile、
Trace sequence quarantine 与受认证 operator/IAM；这些平台项不由当前 local CLI 冒充。

## S1 Formal Agent Stage Planning

1. 把 Agent review 配置收敛为 typed `AgentReviewPolicy`，冻结 agent/provider/model/prompt/
   API protocol、ordered Skill/Knowledge、authority 和 Agent budget。
2. 从 config lifecycle 的同一次受信解析返回 exact ConfigBundle +
   `ConfigResolutionReceipt`；receipt 只作为本地治理记录，不冒充签名或远端 attestation。
   local platform 现已通过 strict `POST /v1/config/resolutions` 和 UI 表单开放同一次原子解析；
   请求不能自报 actor/roles，invocation ID 保持为 rollout identity 的显式输入。
3. 为 workflow stage 增加显式 authority ceiling；配置、snapshot 与 stage 任一层超权或
   超预算都失败关闭。
4. 通过只接受 governed identity 的 component resolver，闭合 exact agent/provider/model/
   runtime/prompt/API protocol/Skill/Knowledge/context-provider artifacts 与 build identity。
5. 使用纯 compiler 生成 canonical `AgentStagePlan`，固定
   `ReviewHypothesisSet + hypothesis_only + side_effects=deny`，并为所有 cross-artifact、顺序、
   digest、budget 和拒绝路径建立 contract/schema/example/test。

当前 1-5、formal Plan admission、canonical request、durable intent/claim/binding、callback
receipt、result/cancel terminal gate、Hypothesis evidence、stale reconciliation，以及本地 Pi
PlatformPort/bootstrap/run/show composition 已完成。成功与失败 CLI E2E 均已闭合，终态 exact
retry 不重跑 provider；admitted Hypothesis 已能确定性生成 Candidate、confirmed Finding 和仅
`queued_for_human` 的初始 Decision JSON/Markdown artifact。formal attempt/evidence/report
投影成统一 terminal `ReviewRun` 的工作已经完成，通用 history/show
能够消费，成功/失败 exact retry 均复用同一 final artifact；formal sensitive disclosure/revoke
也已从 whole-closure 推导 governed ref，拒绝 caller-supplied scope/ref。formal same-input exact
replay 已从 committed source closure 重跑唯一 stage，并固定 `variable=none/remote deny`；budget
variant 已用 derived config receipt 原子改变 stage/Pi timeout；model variant 同样绑定 derived
receipt、subject-bound model component 与新 manifest build identity，并由 formal compare 输出稳定
Candidate/Finding/Decision/latency delta；prompt variant 已用 derived receipt 只替换 exact prompt
component，并保持 model/runtime/tool policy/authority/budget 冻结；skill_pack variant 已接通 exact
subject-bound review-skill bytes transport，只允许同 ID/phase/order 的 Artifact 替换，并保持
model/prompt/runtime/tool policy/authority/budget 冻结。deterministic workflow policy variant 已通过 exact
definition + ConfigBundle binding、scheduler consumption 和 terminal whole-closure 接通；deterministic
index variant 也已通过 exact provider policy diff、source target revalidation、fresh context receipt 和
terminal whole-closure 接通；formal Pi index 已进一步接通 host materialization、adapter component、
model-before receipt admission、CLI/API/Batch 与 Evaluation target-equivalence gate。新增/删除 skill、
formal workflow variant 和 checkpoint
recovery 仍未开放。
knowledge_pack variant 也已用 derived receipt 只替换同 ID/order 的 exact subject-bound knowledge
Artifact，并闭合实际 Pi 消费、snapshot identity、exact retry 与同键异值冲突；新增/删除 knowledge
继续走正常 ConfigRevision lifecycle。
formal live DeepSeek defect/zero-candidate/auth-failure 已通过真实 PlatformPort：model wire identity 与安全
registry identity 分离，build-bearing component revision 和 config revision 各自内容寻址，task transcript/
receipt counter 守恒，0 reviewed group 失败关闭。`evaluation corpus run/resume/show` 已把 governed active
Case 的独立 label/oracle、source snapshot、pre-provider exposure、formal stable key、逐 Case checkpoint 和
immutable EvaluationRun terminal 接成可恢复本地 corpus runner；两 Case E2E 证明同一配置跨 source 复用且
exact retry/resume 不重跑 provider。`CorpusSnapshot` 已进一步冻结完整 Case/source execution closure，formal
batch 不再读取漂移的 current membership。snapshot admission 已增加 purpose/single-split、clone-group
去重与 holdout 全历史 exposure 门禁；repository search v2 也已把 selection range + 32-line halo 纳入
request/receipt/replay。下一步先建立不用于调参的 holdout 与独立 train/dev 语料，针对
TypeScript context/index 成本和 skill 召回分别做单变量 Experiment；随后扩展 Argus/Hailix 多仓
corpus 与重复性/成本验收。不得把本轮参与诊断的 Hailix test clone 继续当 blind promotion evidence；单 run 的真实 provider
SIGKILL/checkpoint success-resume 已完成。
本地 dispatch claim/terminal admission 已通过 shared generation CAS 闭合并发赢家；Argus 侧
Hailix consumer adapter 已覆盖 capability、exact create-or-lookup/cancel、typed terminal 和
pinned callback verifier；executor trust roots 已冻结进 capability/request identity，fresh/recovered
callback admission 都以 persisted request trust 失败关闭。严格 HTTPS/loopback client、request-time credential、machine schema 与
provider-commit/response-loss 后的 formal durable recovery E2E 也已完成。下一步优先由 Hailix 实现
并联调该 public platform-execution contract、provider-authoritative create-or-return-one-handle/
outbox、typed Artifact/Trace 与 runtime attestation，以及 formal
workflow variant/stage recovery。
formal Candidate、Verification、Calibration 与 Suppression 已分别提升为独立 content-addressed
artifact，并由 terminal ReviewRun 引用、通过 `candidate list/show`、通用 `show` 和 authenticated
ReviewRun API 查询；Evaluation/analytics source binding 同时冻结全部 ledger SHA-256。版本化整数 calibration
profile、raw confidence blind boundary、threshold/max-findings suppression、缺分数整体延后和全部 loser 保留已完成。
Finding lineage 也已完成本地保守 matcher、内容寻址聚合、append-only exact retry、CLI 与 authenticated API，
并以同一 Git common-dir、exact OID、strict ancestor/merge-base 和 5000 BPS rename mapping 闭合新 build 的
ancestry evidence；旧 caller-order policy 只保留读取兼容。
`filter_policy` 受控单变量 replay 已下沉为复用 source verification ledger、不重跑 provider 的
纯后处理 Experiment，并接通 durable batch。受治理 calibration fitting 也已接通：candidate-level truth
必须回连 exact active case governance 与 committed Candidate，train/dev-test 隔离、clone/candidate
contamination、Artifact training gate、整数 PAV、overall/repository/dimension Brier/ECE/drift gate、
不可变失败报告和 `auto_published=false` publication boundary 均已由 CLI/API 与 focused tests 闭合。
passed candidate 到 ConfigRevision、filter-only experiment、独立 holdout、显式 rollout activation 与双 ledger
可恢复 rollback 的操作闭环已完成；activation 前后 exact snapshot 的不可变质量窗口机制、样本/partial
fail-closed 状态和人工 rollback recommendation 也已接通。下一步领域工作是用足量真实 corpus 校准 policy
门限并积累长期/业务线漂移证据。lineage 的独立 analytics table、按 relation/method/policy 的
relationship-only Dashboard counts 与 reviewed bug-fix evaluation provenance 已接通；下一步是用足量真实
跨时间 corpus 形成稳定性/漂移基线，relation name 仍不得推导 Outcome 或 gold label。
本地 runtime-file manifest 已闭合 Node、dist、lockfile 与 production dependency 实体文件，并在
dispatch/process start 双重重验，但它只是 `local_host_observation`。当前仍拒绝
未绑定 upstream、非 budget/model/prompt/skill_pack/knowledge_pack/rule_pack/index/workflow/filter_policy variant replay，不能把本地 immutable-ledger callback proof 或
worker self-report 描述为远端平台 attestation。

自动上下文 provider 的本地 application port 已完成：显式 `repository_search`、`go_ast`、
`go_dependencies` 与 compile-only `go_compile` 对三种 target mode 先冻结 exact commit，再生成
严格 lexical repository match、symbol/type/direct caller-callee/bounded exact call-path、module/package/import/compile/gap Artifact，
并进入既有 formal worker
ContextRef transport；invocation 和 published ConfigBundle 都能驱动，published 模式拒绝调用方叠加
旁路。失败已投影 ContextGap，每次执行的 local receipt 绑定 request/target/outcome/timeout/latency 并
进入 ExecutionSnapshot，exact replay 复用 receipt 而不重跑 provider。配置冻结独立的 provider
并发上限，application worker pool 已验证 bounded concurrency、稳定结果顺序与取消传播。
`go_ast` revision 2 已增加 line/column-bound direct call，并以 exact `go.mod` + frozen source
local importer 生成跨本仓 package 的三跳 upstream/downstream path；module 外 external import 不读
ambient cache，dynamic/external/unresolved 保持非 transitive evidence。`go_compile` 已验证 TestMain 不执行、offline module、process-group cancel、输出/Artifact budget 和
不完整输入失败关闭；`repository_search` 已验证敏感路径 read-before-deny 不发生、配额/partial
显式化、exact commit 与 replay 复用。host 与四个本地 provider 现已分别重验 executor、source
listing 和逐 file observed revision；revision 漂移只生成保留 requested/observed OID 的 typed Gap
receipt，不发布上下文 artifact。下一步在 Hailix sandbox 中增加真正 test execution，再补
多 attempt/generation/recovery、interface/dynamic call graph，接 CodeGraph/LSP semantic adapter。

## M2 Safe Agent Execution

1. 冻结 `StageExecutionRequest/Result` 和 typed result sink；不解析最终聊天文本作为
   正式 Finding。
2. 同时建立 Eino-Agent `review-safe` composition 和 Hailix 外部强隔离：
   - 禁 project/user hook、MCP auto-load、plugin/skill/instruction 扩权；
   - hard workspace containment；
   - no network/no secret；
   - no mutation/background/subagent；
   - exact cancellation。
3. Argus 已提供 `internal/hailixexecution` consumer adapter、安全 HTTP client、JSON Schema/
   examples、正式 CLI backend composition 和 formal recovery E2E；Hailix 下一步实现
   `hailix.platform_execution_http.v1alpha1` 的 capability resolve、exact ensure/lookup/cancel、
   await terminal 与 callback verification。现有 DirectTaskInput 只保留 smoke。
4. 为 Hailix 增加 provider-neutral ACP adapter profile、typed Artifact 和公共
   TraceManifest contract。
5. Argus 通过 Hailix API + Frontier snapshot/SSE 执行，不读 Hailix 数据库或 Outbox。
6. 保存 `StageRun/Attempt <-> Task/WorkerRun/AgentTurn` 的 PlatformExecutionBinding，
   再分开追加 Trace/Artifact RunEvidence。

跨仓代码修改需要单独明确任务；本计划不授权当前轮次修改相邻仓库。

## M3 Configuration Plane

1. 定义 RuleDefinition/RulePack、ConfigRevision/ConfigBundle schema。
2. 实现字段级 merge semantics 和 effective config explain。
3. 实现 draft/validate/publish/supersede/rollback。
4. 实现 repository/path scope、灰度 assignment 和 fallback-as-data。
5. 先提供 API/CLI，再依据真实操作流设计 UI。

## M4 Evaluation Plane

1. EvaluationCase provenance、label revision、clone group 和 split。已完成的 candidate derivation
   可从最新 human publish Decision 或未被更正的 accept/dismiss Feedback 回读 exact run/finding
   closure，并只创建 pending/unassigned/candidate-only Case；candidate review 已完成最小本地治理链：
   两个不同 reviewer annotation + 第三方 adjudication + 独立 gold activation，所有事件保留 exact
   revision/idempotency/actor lineage。reviewed bug-fix pair builder 也已闭合 defect head -> fix base、
   same repository、latest fixed Outcome、change/CI exact fix revision 与 defect/fix artifact gate。
   incident missed defect 也已通过独立 Incident ledger、完整 formal report 和 deterministic negative
   search 闭合。mutation/synthetic defect/clean/workflow invariant 也已通过独立 oracle + exact construction
   receipt 的 Probe ledger/source builder 闭合，且禁止 Argus review output 自标注。mandatory blind assignment、
   event-derived agreement 与 reopen round 也已闭合。direct CreateCase 现只允许 candidate ingress；外部
   approved gold/active Case 已有 Ed25519 exact-case/evidence/policy attestation、scoped frozen key、独立
   operator、restore revalidation 和幂等 import。key 还必须由独立 governance_trust_admin 预注册到同一
   append-only stream，import exact 匹配 active revision；revocation 阻止后续 import 且不破坏历史 Case。
   五类治理操作也已有 intent-first recoverable batch service/CLI，以 item event 为 checkpoint，显式保留
   partial failure 并在 restore 时闭合 exact dataset payload。第一版本地 HTTP adapter 已用 loopback +
   mandatory bearer + process-fixed principal 暴露 Case/batch/trust-key 治理，禁止 request 自报 actor/roles，
   并提供有界 cursor 分页。Config lifecycle 与 immutable Dashboard query 已复用同一 adapter；平台 permission
   与 Evaluation role 已分离，配置 show 原子返回 audit history，Dashboard 不暴露 raw facts 或现场 rebuild。
   ReviewRun list/show 现已复用权威 run repository：watermark cursor 冻结分页观察，terminal detail 先重验
   ExecutionSnapshot/artifact closure，并只开放结构化 result family。Finding drill-down 也已复用 control-plane
   exact join，保持 model/governed/human Decision、Feedback、Outcome 为不同事实且不暴露 provider publication
   payload。ReviewRun impact 已增加 typed append-only 增量反向索引：正常 writer index-first、同 run changed row
   conflict，API 启动显式 rebuild 旧 ledger，GET 保持只读，并只对实际命中 run 重验 terminal closure 或
   nonterminal created lineage。本地 operator UI 现已完成 credential-free static shell + in-memory bearer，覆盖 ReviewRun/Finding、Config、
   Dashboard、Evaluation 浏览、Config lifecycle，以及 Finding Decision/Feedback/Outcome append-only 写操作；
   Finding 写入不允许页面自报 actor/role，Config 已通过真实浏览器 create/validate/publish smoke。
   Evaluation 页面已进一步接入三类运行事实和两类 replay batch 的只读 list/show；HTTP 通过窄
	`EvaluationHistoryReader` port 复用领域 Case ACL，batch list 也不绕过 baseline 可见性。该入口仍是
	single-process local authority，不是 hosted tenant/repository scope。两类 running batch 已增加
	principal-bound async resume：恢复 audit、exact template load、service-owned context 和 bounded durable
	failure observation 均已闭合。budget、exact published prompt/skill/knowledge Experiment 与 exact
	Repeatability 已增加 principal-bound async submit：formal profile 冻结、逐 baseline subject ref resolve/
	preflight、intent-first、caller template/path deny 和 terminal retry no-rerun 已闭合。principal-bound
	component publication command 也已闭合 committed-run subject derivation、content contract/digest、审计和
	幂等。exact configured-provider model-only 平台 variant 也已闭合 template identity、非 model closure preflight、
	run-intent 后 publication 和 terminal no-rerun；它仍不是分布式 Hailix worker transport。
   durable async ReviewJob 现已用 immutable command + exact published config binding 接到既有 scheduling
   admission/lease/heartbeat/fencing/callback/reconcile，提供 submit/list/show/cancel；客户端断开不取消，terminal
   run 可补 callback；deterministic diff/selection/scope 仅从 exact config/build/target 与可验证 stage prefix 恢复，
   scope 另需冻结 shard manifest；prepared-input binding 覆盖 lifecycle 前窗口。quick 已接同一 job 链，
   原命令同 key 复用冻结输入和两套配置。formal_pi_review_v1 已作为互斥 profile
   接入同一 job 契约，冻结 source/config/runtime/component/pricing 并复用单一 workload lease；进程级 SIGKILL
   acceptance 已验证 lease expiry、generation 2 接管、stage ledger 恢复、终态 callback 和 stale generation fence；
   跨进程 cancel acceptance 也已验证另一个进程追加永久 fence 后，worker heartbeat 会取消 Pi runner 且不接受
   后续 callback。deterministic scope 现已冻结 shard manifest、并行 checkpoint、generation
   fence、aggregate/fan-in，并从非终态 run prefix 恢复；物理 SIGKILL acceptance 已证明只补
   pending shard、闭合 65 个 Finding 与拒绝旧代迟到 checkpoint。formal Pi 已增加组级
   context/review checkpoint、跨 generation 注入和 stale writer fence；2026-08-27 已进一步增加单调
   candidate-verification revision，fake runtime/Go ledger 验证 completed verifier 复用、failed verifier 重跑、
   乱序 callback 收敛和 tamper rejection。2026-08-27 真实 DeepSeek canary 已在 revision 1 fsync 后物理
   终止 generation 1 lease owner，generation 2 复用已完成 verifier、正式成功终态，并拒绝旧代 callback。
   跨代 task 窗口使用 checkpoint ledger 的 host-authored generation binding，frozen anchor side 在 worker/host
   双侧 canonicalize。下一步补 Hailix worker adapter 与通用 stage recovery；
   local operator 侧已增加 workload-scoped scheduling timeline，以及绑定 policy digest/ledger sequence 的
   global/class/tenant pressure CLI/API；pressure 只读 projection 会显式报告 requires-reconcile stale work，
   已有 class isolation、tenant fairness、bounded queue tests。下一步再把 Hailix 公共 TraceManifest ref 与
   platform-attested resource metrics 作为授权外部证据关联进来，不读取其私有存储。
   多租户部署必须把 principal 接到 Hailix/IAM 认证身份，不能复用本地 bearer 充当平台 trust root。
2. Stage replay、variant、baseline、MetricObservation。已完成的最小 EvaluationRun recorder 会把
   exact Case label/exposure 与 committed formal run/report 闭合，并只对有证据的 defect presence
   评分；最小 ExperimentRun recorder 已闭合 baseline/variant 的 case、label、evaluator 与 direct
   replay variable lineage。本地批量 runner 已用持久 intent、pre-execution exposure、bounded worker
   pool、stable per-case idempotency 和 append-only case checkpoint 编排同一 formal replay
   implementation，并自动生成两层结果；checkpoint 后恢复只执行缺失 case。
   Evaluation binding 现可声明 exact dimension applicability；结果按 dimension 独立保存 Candidate/
   Finding、执行覆盖、review/verification receipt、turn/tool、累计 task duration 与 attributable token。
   Experiment 按相同 dimension ID 配对并保留两侧 exact revision，analytics 使用受控
   `review_dimension` 投影 token/duration。exact raw candidate 已纳入 formal terminal closure，并进入
   Evaluation/Experiment 的逐维 normalization 整数事实；committed coverage + group/task receipt 也已闭合
   dimension-specific context gap 计数并进入 analytics。逐维 duplicate/invalid/budget rate 已固定为
   `sum(fate)/sum(raw)` 和 scale=6，零分母/缺 closure 保持 partial；不能从 retained Candidate 或 shared
   context 猜测 duplicate/coverage。
   本地 batch-scoped 跨进程 claim/heartbeat/generation/fencing 与过期接管已完成；Experiment/Repeatability
	intent 也已绑定 credential-free、content-addressed exact executor template；template 已进一步冻结
	`local-pi`/`hailix-http` transport identity 而不保存 request-time credential，CLI 可只凭 batch/access
	resume，重启时重新校验 runtime/component bytes，终态恢复不重复调用 provider；local API 已可从启动时
	formal profile 提交 budget、model、published prompt/skill/knowledge、sealed RulePack、ordered built-in index provider 与 exact replay batch，并通过独立 permission
	发布 exact 三类组件。RulePack 已作为 exact bytes 进入 formal Pi context/review/verifier，而非
	config-only 伪变更。formal workflow variant 现已只开放可被 AgentStagePlan 实际消费的单阶段资源预算
	变化，并贯通 CLI/API/UI/Batch；下一步仍是 Hailix executor transport 与远端 attestation。
3. Finding match、localization、filter efficacy、cost/latency/instability。已完成 committed ReviewRun
   StageAttempt 台账口径的逐 case paired latency 与 exact delta；结构化 LabelAnchor、digest-bound
   range-overlap localization 评分及 baseline/variant matched-anchor delta也已完成。结构化 suppression
   target、Governed Candidate disposition 评分和 paired suppressed/escaped delta 已闭合；target 未出现
   不算过滤成功。ApplyTrial contract、artifact digest/size revalidation、fix verdict 和 paired check delta
   已闭合为 `local_host_unattested` 子集。formal receipt 也已固化到 committed ReviewRun，EvaluationRun
   可复算 worker-self-reported token facts，ExperimentRun 在两侧完整时输出 paired input/output/total
   token delta；analytics source/dashboard projection 也已消费这些 committed facts，partial 显示 unknown。
   分维度 attributable token/cumulative task duration 也已进入 ExperimentFact；raw-candidate
   normalization/dedup 与 context-gap 的 paired 整数事实和 raw-denominator rate 已闭合。独立
   RepeatabilityRun 已闭合同一 baseline 的 direct exact-replay 样本、Finding-set Jaccard/exact-set/
   presence/anchor-hit、verdict flip、partial-unavailable、append-only persistence 与 CLI；逐 dimension
   Finding/verdict 稳定性和 execution/usage 范围已闭合。durable RepeatabilityBatch 已按 sample×case
   冻结、checkpoint、恢复并自动生成各 EvaluationRun/最终 RepeatabilityRun；下一步仍需隔离 Apply
   executor/Hailix attestation、分布式 transport 和权威 billing 成本事实。
4. Judge calibration、holdout contamination gate 和 experiment comparison。
   terminal submit 的源码证据校验已前移为可纠错工具反馈，并继续由 Go host 独立重验。新的未曝光
   synthetic dev case 上，target-only baseline 为 0 Candidate/0 Finding；同 source 的
   `repository_search@v2` index-only replay 找到 2 个跨 dimension Candidate，均在剔除越权 context-file
   evidence 后改用 exact target evidence 并被独立 verifier 确认。同一状态迁移缺陷跨
   `correctness`/`error-contract` 形成 2 个 Finding 后，已增加由 worker/Go host 双侧重算的保守
   `semantic_duplicate`：同 group/path、重叠 anchor 且固定 title token Jaccard 至少 3/4 才合并，并保留
   两条 raw claim、只验证 canonical winner；正向和相同 anchor 不同语义拒并测试已通过。已曝光 case 的
	regression-only physical rerun 进一步暴露了失败诊断缺口：默认 180 秒运行以 `deadline_exceeded` 终止；
	600 秒配置的 baseline 成功但由此前 0 Finding 漂移为 1，index replay 则在 334 秒以
	`pi_no_review_completed` 失败，旧契约无法定位各 dimension 的具体失败。现在“worker report 已 strict
	decode、但 formal coverage/host mapping 失败”的 governed failure-diagnostic closure 已完成：证据对只进入
	non-succeeded terminal gate，`formal show` 只给脱敏聚合，exact read 复用 purpose-bound 审计入口，失败
	ReviewRun/Finding/Evaluation 不吸收这些诊断。下一步用新 runtime/host 物理复跑该 exposed index variant，
	第一次新 host replay 已从 immutable checkpoint 定位到 `JSON.stringify` 与 Go 默认 HTML escaping 导致的
	Candidate fingerprint 假漂移，同时暴露 diagnostics mapping 仍过晚；两项均已修复并加固定向量/失败分支
	测试。修复后 replay 已成功闭合，但真实输出的 4 个不同标题仍是同一根因，证明 3/4 Jaccard 过于严格。
	第二版保留原 precision gate，并增加 shared code identifier + overlapping evidence + canonical title token
	overlap 的宿主双侧重算路径，同时将 workflow/checkpoint revision 升为 v1。v1 physical run 把 5 raw 降为
	3 canonical，仍未达到 unique defect 目标；immutable claim 描述的 pairwise overlap 为 56%–64%。v2 增加
	高门槛 root-cause description relation 并再次升级 revision。normalize-only diagnostic preview 已可从
	committed raw collection 零 provider 调用重算，并在 v1 physical output 上得到 5 raw -> 1 cluster。下一步用
	v2 runtime 的物理复跑已保留全部 raw lineage、只产生 1 个 canonical Candidate/Verifier/Finding；
	normalize policy 也已接入 content-addressed batch paired comparison，保持 diagnostic authority，不冒充正式
	replay。下一步
	建立有预注册 oracle、不用于调参的足量 dev/holdout corpus，测量 unique-defect recall/precision、误合并率、
   稳定性和成本。上述 synthetic 结果只用于 development diagnosis，不进入 promotion/holdout 证据。
	NormalizationOracle/QualityRun 的最小 contract、pairwise metric、seal/sign/register/run/show/revoke、
	append-only CAS ledger 和 restore-time 重验已完成。下一步构建多个不同根因、相同 anchor 不同语义、
	clean/false-positive 及跨语言真实 dev/test case。最小 normalization promotion gate 已加入：test/holdout
	严格隔离、whole-closure 重算、整数阈值 decision 与 shared ledger 均已闭合；下一步先扩充真实 corpus 并
	校准阈值。runtime selector 已闭合为 `candidate-normalization@v0|v1|v2 + worker SHA-256`，由 ConfigBundle、
	AgentStagePlan、worker plan、Pi snapshot/checkpoint 与 Go evaluator 实际消费；managed promotion 也已绑定
	候选/回滚 exact selector 与同一 worker digest。受治理 ConfigRevision 派生、shadow/canary 实际 run
	binding、authorization/rollback-monitor、同 seed 单调 activation 和双 ledger rollback 机制已闭合。
	下一步以足量独立 corpus 校准阈值，并执行真实 shadow/canary；在真实样本量和阈值未验收前不激活策略。
5. shadow/canary/promotion/rollback ledger。

Argus 永久拥有 Code Review 的 ExecutionSnapshot、ReplayRun 和 ExperimentRun。
当 Hailix 自身出现第二个真实使用者后，只提取 platform execution input/ref、隔离
namespace 和 side-effect enforcement，避免预先泛化。

## M5 Product And Value

1. Review history、Finding detail、config explain、replay diff。
2. Feedback/outcome、dedup cluster winner/loser lineage。
3. 结构化 analytics facts、九表 Parquet export 和 dashboard；Context Provider receipt、独立
   RepeatabilityFact
   区分 executed/reused 并投影成功、Gap reason 与 latency。
4. Code platform event/comment adapter 和 unknown-outcome recovery。
5. Evidence-tiered ValueObservation 与 ROI。

## M6 Bootstrap

1. Argus、Hailix、Eino-Agent 三仓 shadow review。
2. Finding 经人工/测试验证后进入 evaluation candidate。
3. Eino-Agent 可生成修复建议，但 Apply 在独立授权 WorkingCopy 执行。
4. 回归、holdout、shadow、canary 全绿后人工 promotion。
5. 持续监控误报、漏报、漂移、成本和自我强化偏差。
