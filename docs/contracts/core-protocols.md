# Core Protocols

**Status:** `v1alpha1`
**Compatibility:** experimental

## 1. ReviewSpec

`ReviewSpec` 是入口契约，只表达用户授权与领域意图，不包含运行时动态结果。

必填身份：

- `request_id`、`idempotency_key`
- `tenant_id`、`workspace_id`
- `repository`
- `target`
- `config_bundle_ref`
- `workflow_ref`
- `requested_outputs`

Target 是判别联合：

| mode | 必填 | 禁止/约束 |
|---|---|---|
| `diff` | base revision、head revision、patch artifact | base != head |
| `selection` | revision、path、exact selector、content artifact | selector 恰为 legacy start/end、`ranges` 或 `symbol` 之一 |
| `scope` | revision、非空 include patterns | exclude 可空但必须显式数组 |

所有 content ref 使用 `uri + sha256 + size_bytes`。`uri` 只允许平台签发的逻辑
`artifact://authority/path`，不是调用方可令服务端直接访问的 URL；禁止
`file://`、HTTP(S)、userinfo、query、fragment 和路径穿越。resolver 必须按
tenant/workspace 重新授权并验证 digest/size。

Go 类型和严格 decoder 位于 `pkg/contracts/v1alpha1`，规范样例位于 `examples/`。
未知或重复 JSON 字段必须拒绝，以避免跨语言解析歧义，或让调用方认为某个字段
已生效而运行时静默忽略。

Selection selector 语义：

- legacy `start_line + end_line` 继续兼容，并在 materialize 时归一为一个
  `effective_range`；
- `ranges` 为 1-based、已排序、互不重叠的 1..128 个区间；
- `symbol` 当前只支持 Go 的 `function | method | type`。symbol 必须在冻结的
  revision/overlay 中恰好解析为一个 AST declaration；零个、多个或源码解析失败均
  fail closed；
- `effective_ranges` 是 workflow 唯一可生成 Finding anchor 的授权边界。多区间
  selection content 按区间顺序拼接，每段保留一个结尾 LF；manifest 与 ranges
  共同消除边界歧义。

JSON Schema 与 Go validator 共同校验正例和安全负例；artifact URI、portable path、
未知字段等结构/边界由 parity gate 覆盖。跨字段约束（例如 base/head 不同、行区间
有序）仍以 Go semantic validator 为 M0 权威；进入跨语言 beta 前应生成 schema 或
扩大完整的 validator parity corpus。

## 2. TargetSnapshot

ReviewSpec 通过 admission 后生成：

```json
{
  "schema_version": "argus.target_snapshot.v1alpha1",
  "target_snapshot_id": "...",
  "review_request_id": "...",
  "repository_identity": "...",
  "resolved_revision": "...",
  "manifest_ref": {"uri": "...", "sha256": "...", "size_bytes": 1},
  "dirty_state": "clean|dirty|overlay|unknown",
  "captured_at": "...",
  "captured_by": "..."
}
```

TargetSnapshot 是事实，不随分支移动。`unknown` 不能进入自动发布路径。

Selection TargetSnapshot 同时保留调用方 selector（legacy range 或 symbol）与显式
`effective_ranges`。对于 range selector，两者必须逐项一致；对于 symbol selector，
effective range 是在冻结源码上解析出的单一区间。SelectionManifest、ReviewSpec、
TargetSnapshot 和 ReviewInput regions 在持久化 closure gate 中逐项核对，任何
selector/range/content digest 篡改都会拒绝最终运行。

### 2.1 Target 外动态上下文

Target 外的仓库搜索、CodeGraph/index、依赖源码、构建或测试结果不得混入 target
files/regions。每次动态上下文请求只能落为以下封闭联合之一：

- `ContextRef`：`context_id + kind + revision + digest + coverage + provenance`，
  并引用平台签发的 `artifact://` artifact、contract 和 size。这里的 `digest`
  是引用 artifact 的 SHA-256；
- `ContextGap`：保留相同的 identity、revision、coverage 和 provenance，加稳定
  `reason_code`。Gap 的 `digest` 是规范化上下文请求的 SHA-256，而不是虚构的结果
  digest。

`coverage` 显式记录 path/line spans 与 symbol；`provenance` 显式记录 provider、
producer id/revision。ContextRef/Gap 是 Execution 输入和证据可追踪事实，但永远不
扩展 ReviewInput `regions`。detect/normalize checkpoint（包括 replay 复用）中的
Candidate/Finding anchor 必须重新证明落在 canonical patch 的 target added lines、
deletion-only hunk 的有界存活 target context 或 frozen regions 内；只有上下文覆盖而没有
target 授权的行不能成为 Finding anchor。deletion-only 例外用于表达“删除导致的缺失逻辑”，
TypeScript Candidate 主 anchor 只接受与删除相邻的存活 target context；mixed replacement
hunk 不获得该例外，冻结输入中的 old-side anchor 仍失败关闭。

跨语言校验使用 `api/schema/v1alpha1/review-input.schema.json`；ContextRef 与
ContextGap 正例分别位于 `examples/review-input.selection-context-ref.json` 和
`examples/review-input.selection-context-gap.json`，两者同时经过 JSON Schema 与
Go strict decoder gate。

本地自动 Go provider 的 artifact contract 是 `argus.context.go_ast.v1alpha1`。它绑定
exact `commit_oid`、排序后的 target paths、symbol/type/call facts、coverage counters 和
typed gaps。adapter revision 2 为 direct call 增加 exact line/column，并只从仓内
`go_types_exact` 边派生最多三跳的 upstream/downstream `call_paths`：downstream 从 target
开始，upstream 以 target 结束，每个 site 必须逐边闭合相邻 symbol。external typed、name match、
unresolved、interface/dynamic 与 module 外未闭合边不得进入 transitive path，也不得与 exact
resolution 混为一类证据；path/fact/artifact 超限必须显式 truncated gap。Go strict decoder、JSON Schema 和正例分别位于
`internal/contextcapture/goast`、`api/schema/v1alpha1/go-ast-context.schema.json` 和
`examples/go-ast-context.json`。该 artifact 仍是 untrusted ContextRef evidence，不能扩大
ReviewInput regions。

本地自动 repository search provider 使用 `argus.context.repository_search.v1alpha1`。adapter/provider
revision 2 从 exact `commit_oid` 中读取 target-policy 允许的普通 UTF-8 blob；diff/scope 从完整 target
文件提取 bounded 高区分度标识符，selection 则只从 frozen `target_ranges` 前后各 32 行的合并窗口提取，
并在 artifact 中保存精确范围与 `query_halo_lines`。随后输出 target occurrence、外部 repository occurrence、matched-file count、
逐行 path/line/column、完整行 SHA-256 与有界 line text。`.env`、credential、state 和 key
路径必须在 blob read 前生成 typed sensitive gap；example/sample/template 文件是显式可读例外。
Artifact 必须保存 files matched/retained/read/unavailable/sensitive、target found、candidate/query/
match 与 truncation coverage。该 provider 是 exact lexical search，不声明 declaration/call resolution，
不把 partial 未命中解释成“仓库不存在该符号”，也不能扩大 Finding anchor。Go strict decoder、
JSON Schema 和正例分别位于 `internal/contextcapture/reposearch`、
`api/schema/v1alpha1/repository-search-context.schema.json` 和
`examples/repository-search-context.json`。

第二个本地自动 provider 使用 `argus.context.go_dependencies.v1alpha1`，绑定 exact
`commit_oid`、module path、排序后的 target paths、package/import edges、resolution、coverage 和
typed gaps。internal import 只有在 exact revision 中存在对应 package directory 时才标记为
`internal_exact`；stdlib、external 与 unresolved 必须保持独立。Go strict decoder、JSON Schema
和正例分别位于 `internal/contextcapture/godeps`、
`api/schema/v1alpha1/go-dependencies-context.schema.json` 和
`examples/go-dependencies-context.json`。

第三个本地自动 provider 使用 `argus.context.go_compile.v1alpha1`。它从 exact commit
物化受限 Go/module/assembly bytes，并以 host 固定参数执行 `go test -c`；测试源码会被编译，
但 Test、TestMain、init 和目标二进制均不执行。Artifact 绑定 Go version/OS/arch、
`CGO_ENABLED=0`、`GOPROXY=off`、`GOWORK=off`、local unattested authority、逐 package
passed/failed/unavailable、诊断、原始输出 digest/size、coverage 和 gap。embed、cgo、local replace、
`.syso`、缺失输入、离线依赖、timeout 与输出超限均失败关闭或显式 unavailable，不得冒充已验证
compile failure。Go strict decoder、JSON Schema 和正例分别位于
`internal/contextcapture/gocompile`、`api/schema/v1alpha1/go-compile-context.schema.json` 和
`examples/go-compile-context.json`。它是 Pi 可消费的独立 ContextRef evidence，不是 Finding anchor、
Hailix sandbox attestation 或 repository test execution。

provider 执行列表必须来自本次冻结的 ConfigBundle。默认本地 invocation 只能选择内建
`repository_search`、`go_ast`、`go_dependencies` 或 `go_compile`，并先把 exact definition
写入 ConfigBundle；published config 模式拒绝 invocation
叠加 provider，只执行 `execution.context_providers`。成功输出由 application host 发布并复算
ContextRef；executor missing、adapter identity mismatch、timeout、overlay mismatch、capture 或
output invalid 都生成绑定 canonical provider request digest 的 ContextGap。当前 Gap 是最小失败
证据。host 在持久化任何成功输出前必须再次核对 executor `observed_revision` 与请求的 exact
commit；四个本地 provider 还会分别核对 source listing 和每个 file result 的 revision。任一处
漂移都不得消费返回内容，只生成 `context_revision_mismatch` Gap。每次执行另生成
`argus.context_provider_execution_receipt.v1alpha1`：绑定 provider/adapter、request digest、exact
target paths/ranges、请求 `commit_oid`、实际 `observed_revision`、ContextRef/Gap、timeout/timing 和
`local_host_observation` authority；成功时两者必须相同，revision mismatch Gap 则必须保存互异的
exact OID，其他 Gap 不得虚构 observed revision，
由 ExecutionSnapshot 的 `context_provider_receipt_refs` 引用；replay 复用同一 refs。
`execution.context_provider_max_concurrency` 是冻结的独立上限（当前合法范围 1..16），不得借用
deterministic stage 的全局并发预算。host 以该上限运行 provider worker pool，每个 provider 仍使用
自身 timeout；输出和 receipt 按配置顺序稳定归位，不按完成顺序重排。Go contract、
Schema 与正例位于 `pkg/contracts/v1alpha1/context_provider_execution_receipt.go`、
`api/schema/v1alpha1/context-provider-execution-receipt.schema.json` 和
`examples/context-provider-execution-receipt.json`。本地 receipt 不能冒充未来的 Hailix attestation。

### 2.1.1 Evaluation candidate derivation

`argus.evaluation_candidate_derivation_request.v1alpha1` 只接受 case/run/finding/source identity，
以及不能从评审事实推断的 license、consent、classification、owner、label-policy、restrictions 和
collected time。调用方不能提交 repository、input snapshot、evidence refs、label、split、dataset state、
allowed uses 或 eligibility。

`human_decision` 只接受该 Finding 最新的显式 publish Decision；`production_feedback` 只接受存在且
未被后续 correction supersede 的 accept/dismiss Feedback。accept 形成 provisional
`positive_localized`，dismiss 形成带 exact Finding fingerprint suppression target 的 provisional
`false_positive_regression`；wont_fix/outdated/needs_discussion 因语义含糊而拒绝。控制面从 committed
ReviewRun/ExecutionSnapshot/ReviewSpec/Finding source 回读 repository、TargetSnapshot、source artifact、
source-digest anchor 与 fingerprint，并对 exact artifacts 执行 `candidate_pool` integrity gate。

输出永远是 `review_state=pending`、`dataset_state=candidate_pool`、`split=unassigned`、
`allowed_uses=[candidate_pool]`、全部 eligibility=false。它只创建待治理事实，不能自动成为 gold、
holdout、训练数据或 promotion evidence。Schema/正例位于
`api/schema/v1alpha1/evaluation-candidate-derivation-request.schema.json` 和
`examples/evaluation-candidate-derivation-request.json`。

`source=reviewed_bug_fix_pair` 另要求 `fix_run_id`。`source_id` 必须是该 defect Finding 上未被
correction supersede 的 latest `Outcome=fixed`；fix run 必须是同 repository 的 succeeded non-replay
diff ReviewRun，且 `fix.base_revision == defect.head_revision`。Outcome 必须同时以 `change` 与 `ci_run`
SourceRef 将 revision 精确绑定到 `fix.head_revision`，并晚于 fix review 完成。控制面冻结 defect Finding
source/TargetSnapshot 与 fix TargetSnapshot/authoritative report，全部通过 candidate-pool integrity gate 后
只生成 `fix_validation + fix_valid` provisional candidate。它证明“有受评审修复变更及 CI fixed assertion”，
不证明 Apply 在任意环境成功，仍需上述多人治理和后续 EvaluationRun ApplyTrial。

`source=incident_missed_defect` 不接受 `finding_id`：漏检不能依附一个当时已经存在的 Finding 来
自证。来源先以 `argus.missed_defect_incident.v1alpha1` 进入独立 append-only ledger，冻结 external
source authority/id/revision、defect fingerprint、结构化 anchors 和 exact evidence Artifact refs；
correction 必须形成单链，禁止分叉，旧 incident 保留但不能继续派生。`incident_ingest` 与 dataset
curator/reviewer/adjudicator 是不同角色。

派生时控制面要求 source ReviewRun 为 succeeded formal non-replay、Incident 晚于 run completion、
GovernedReport completeness=complete，并对 ReviewSpec、TargetSnapshot、GovernedReport 和全部 incident
evidence 执行 candidate-pool integrity gate。anchor 必须匹配 frozen target path/source digest；selection
还必须完全位于 authorized ranges。随后对报告中的全部 canonical Candidate 做 fingerprint 或
path/side/source-digest/range-overlap negative search：任何命中、partial report 或 target mismatch 都拒绝
`missed_defect_regression`。Incident 是独立缺陷断言，negative search 是当时“未报告”的确定性证据，
两者缺一不可；输出仍是 pending candidate，必须继续经过多人治理。

Incident Schema/正例位于 `api/schema/v1alpha1/missed-defect-incident.schema.json` 和
`examples/missed-defect-incident.json`。

`source=mutation_probe|synthetic_probe|workflow_invariant_probe` 不接受 `finding_id/fix_run_id`，而是
解析最新、未被 correction supersede 的 `argus.evaluation_probe.v1alpha1`。Probe ledger 以
`probe_ingest` 角色单链追加，冻结 source authority/revision、独立 `ProbeOracle`、formal ReviewRun 和
exact `EvaluationProbeReceipt` ref。Oracle authority 必须不同于 source authority；receipt 还必须由第三个
executor authority 产生，绑定 exact input TargetSnapshot、oracle semantic SHA-256、版本化 construction
method、实际 observed outcome、remote-writes=deny 和 evidence artifacts。mutation receipt 额外绑定不同但
同仓的 baseline MaterializedTarget；synthetic/workflow 不得伪造 baseline。

派生时控制面回读 receipt、ReviewSpec、input/baseline target 和全部 oracle/receipt evidence，并执行
candidate-pool integrity gate；receipt identity、chronology、target、oracle digest 或 observed outcome 任一
不闭合即拒绝。缺陷 anchor 必须位于 frozen target/selection；Argus Finding/Candidate/Report/Agent receipt
等 review-output contract 明确不能充当 oracle evidence。`synthetic_defect` 形成 mutation diagnostic，
`synthetic_clean` 形成独立 negative-clean corpus，`workflow_invariant` 形成 pass/fail invariant Case；三者
强制 synthetic consent。所有结果仍是 pending、unassigned、ineligible candidate，且 synthetic/mutation
永远不能声明 production distribution。

Probe 与 receipt Schema/正例位于 `api/schema/v1alpha1/evaluation-probe.schema.json`、
`api/schema/v1alpha1/evaluation-probe-receipt.schema.json`、`examples/evaluation-probe.json` 和
`examples/evaluation-probe-receipt.json`。

### 2.1.2 Evaluation candidate review governance

派生后的 Case 保留 immutable ingress object；`CaseRecord.current_governance` 与 current label
只由同一 append-only dataset ledger 投影。review 必须先写入
`argus.evaluation_case_review_assignment.v1alpha1`：dataset curator 对 exact governance/label revision
分配 2..8 个排序唯一 reviewer，`blind=true`，并把状态推进 `in_review`。未分配 reviewer 不能提交；
reviewer 读取 assignment 时只能看到自身 identity，annotation history 也只返回自己的事件，不能观察同轮
其他人的 verdict/rationale。`argus.evaluation_case_annotation.v1alpha1` 是单个 human reviewer 的独立
annotation，本身不能改变 label、gold 或 active；同一 actor 在同一轮只能提交一次。

`argus.evaluation_case_adjudication.v1alpha1` 冻结排序唯一的 annotation event IDs。adjudicator
必须是不同于全部 reviewer 的第三个 actor；approve 只能选择至少一个已引用 approve annotation
实际提出的 label/policy，reject 至少引用一个 reject annotation；且必须引用本轮每个 assigned reviewer
各一条 event。通过后只进入 approved + gold +
unassigned + ineligible；拒绝进入 rejected + retired。任何一条 annotation 都不能单独升级真值。

`argus.evaluation_case_activation.v1alpha1` 是 gold 到 active 的独立 transition，使用
expected governance revision，重新冻结 split、eligibility 与完整 license/consent。普通 split 需要
dataset curator，holdout 需要 holdout maintainer；license、holdout、synthetic/mutation production
distribution 与 repository/time/clone split contamination 全部失败关闭。EvaluationRun、exposure、
holdout ACL 和 promotion holdout gate 均读取 current governance，而不是 immutable ingress state。
新治理事件使用 dataset stream expected-sequence CAS，exact retry 幂等、stale revision 和并发写冲突。

`CaseReviewAgreement` 不是可写分数，而是从最新 assignment/annotation 事件确定性重算 assigned/completed、
approve/reject counts、verdict unanimous/disagreement/incomplete 与 exact-label agreement；只向 curator、
adjudicator 或 holdout operator 展示，reviewer 在盲审期间只得到 unavailable。`argus.evaluation_case_reopen.v1alpha1`
允许 curator 将已 adjudicated gold/active/retired Case 降回 pending candidate_pool，清空 split/eligibility 并
恢复 ingress candidate-only license；holdout 另需 maintainer。reopen 必须冻结 exact governance revision、
evidence refs 和 server-recomputed affected experiment IDs，历史 annotation/adjudication/exposure/experiment
均不删除，新一轮必须重新 assignment。

`CreateCase` 是 candidate ingress，不是通用数据导入：它只接受 pending + candidate_pool + unassigned +
全部 eligibility=false 的 Case。外部系统已经完成治理的 approved gold/active Case 必须走独立的
`argus.external_governed_case_import.v1alpha1`。导入请求冻结 exact Case SHA-256、输入与 provenance
evidence closure、外部 policy revision、2..8 个排序唯一 reviewer、独立 adjudicator、Ed25519 签名，
以及按 repository/classification/validity interval 收窄的 key revision。签名、scope、时间、Case digest、
导入操作员独立性或 holdout 权限任一不闭合即拒绝；事件重放时会再次验证完整签名与绑定，成功导入的
attestation/key/import actor 作为 `CaseRecord.external_governance` 保留。exact retry 幂等，同 case 的不同
导入冲突，不能借 direct create 绕过治理。

import 之前必须由独立 `governance_trust_admin` 以
`argus.governance_trust_key_registration.v1alpha1` 将 exact key revision 写入同一 append-only dataset
stream；请求内 key 必须逐字段匹配 active registry record，不能以自带公钥自证。注册者还必须独立于
import operator、外部 reviewer 和 adjudicator。`argus.governance_trust_key_revocation.v1alpha1` 使 revision
对后续 import 失效，但不追溯删除或改写撤销前已验证的 Case；registration/revocation exact retry 幂等，
重放按 event sequence 恢复当时信任状态。当前 trust-admin actor/role 仍由本地 authority 声明，不证明
公钥来自 Hailix/IAM；生产接入仍需平台认证身份成为 registry 的上游 trust root。Schema/正例位于
`api/schema/v1alpha1/external-governed-case-import.schema.json`、
`api/schema/v1alpha1/governance-trust-key-registration.schema.json`、
`api/schema/v1alpha1/governance-trust-key-revocation.schema.json` 和对应 `examples/`。

平台批量操作使用 `argus.governance_batch_request.v1alpha1`，一次只允许 assign、annotate、adjudicate、
activate 或 reopen 之一，包含 1..500 个按唯一 `event_id` 排序且 case 不重复的 item。每个 item 仍是上述
原生严格契约，expected revision、角色、盲审、污染和 holdout 校验不会因 batch 放宽；所有 item 时间与
`submitted_at` 相同，intent、item、terminal event ID 两两分离。

批次不伪装成跨多个 dataset event 的数据库事务。Repository 先把 immutable intent 写入独立 append-only
stream，再按稳定顺序调用同一单项领域方法；每个 item 的 event ID 就是 durable checkpoint。进程在任意
item 后退出时，exact request 重试会复用已提交 item、只继续缺失项。确定性 unauthorized/not-found/
conflict/invalid-transition 会产生 `argus.governance_batch_result.v1alpha1`，逐项保留 succeeded、failed、
not_started；I/O、cancel 或 stream CAS 竞争保持 running 并返回错误，不能被固化为业务失败。terminal
重放还会逐项验证 exact dataset event type/payload/actor/roles/audit/time，不能拿无关 event 冒充 checkpoint。
annotation batch 继续按 actor 隔离盲审读取；holdout batch 继续使用 Case ACL。Schema/正例位于
`api/schema/v1alpha1/governance-batch-request.schema.json`、
`api/schema/v1alpha1/governance-batch-result.schema.json` 和对应 `examples/`。

本地平台 transport 使用 `argus.local_api_principal.v1alpha1` 和
`argus.local_api_mutation.v1alpha1`。principal 只在 `api serve` 启动时从 strict regular JSON 文件加载，
且 permissions 与 roles 都必须排序唯一；permissions 控制 transport service/action（含独立 `review_read`、
`review_write`），Evaluation roles 只供 Evaluation ACL，`finding_roles` 只供 post-review Decision authority。
带 `review_write` 的 principal 必须冻结 `actor_kind` 与至少一个 finding role；带 evaluation permission 的
principal 必须有非空 Evaluation roles，config/dashboard-only
principal 必须使用显式空 roles 数组，不得借 dataset role 获取平台权限；
mutation 只包含 `idempotency_key/audit/at`，不得包含 actor 或 roles。Case import、GovernanceBatch、trust-key
register/revoke 分别使用独立 command wrapper，把既有领域 request 与 local mutation 组合；wrapper schema 和
正例位于 `api/schema/v1alpha1/local-api-*.schema.json` 与 `examples/local-api-*.json`。HTTP adapter 必须把
固定 principal 注入 `evaluation.Mutation/Access`，strict reject 请求中的未知字段，并要求领域时间等于
mutation time。

Config transport 使用 `argus.local_api_config_create_command.v1alpha1` 将 exact `ConfigRevision` 与 mutation
绑定；validate/publish/rollback 使用 `argus.local_api_config_transition_command.v1alpha1`。publish 必须显式
携带 rollout，validate/rollback 必须不携带 rollout。HTTP path 中的 ID/revision 只定位 immutable record，
actor 永远由 principal 注入 `configrepo.Mutation`。show 必须从一次 repository projection 返回 record 与
append-only history，不能跨两次可竞态查询拼装。Dashboard transport 只开放 manifest list 和 exact snapshot
show；show 不返回 `FactSet`，不开放 rebuild/export，也不读取 authoritative ledgers。其数据仍由 CLI rebuild
冻结并通过 `ProjectionSnapshot.Validate` 闭合。

ReviewRun transport 提供 `GET /v1/review-runs` 与 exact run show。列表 cursor 冻结首次读取时的
`run-index` sequence watermark，后续页面按该 watermark 重放首个 terminal authority 和 latest lifecycle
projection；新 run 或既有 run 的后续事件不得进入同一次遍历。pending/running show 只返回 HistoryEntry；
terminal show 必须通过 `LoadRun` 的 ExecutionSnapshot、source lineage 和 artifact closure 校验，并只解码返回
deterministic Report 或 formal GovernedCandidateSet/GovernedReviewReport。raw prompt、agent task evidence、
execution receipt 和 Markdown bytes 不属于该 transport view；调用方也不能把 list page 当 EvaluationSnapshot。
`GET /v1/review-run-impacts` 使用 exact `kind + id + revision` 与可选 SHA-256 反查冻结组件；
`kind` 只允许 `config_revision`、`config_bundle`、`rule_pack`、`workflow`、`model`。
查询 cursor 同时绑定 selector 与首次查询的 run-index watermark。反查先使用 append-only
`argus.review_run_impact_index_fact.v1alpha1`，其每个 run row 冻结 ExecutionSnapshot ID 以及 applied config
revision、ConfigBundle、RulePack、Workflow 和执行/formal model binding；正常 writer 在 authoritative
run event 前幂等写入，旧 ledger 只能通过显式 `RebuildImpactIndex` 补齐，GET 不执行 read-repair。索引 row
如果先写而 run event 未提交，只是不会被 watermark history 采纳的 orphan；同 run 的 changed row 会在 run
event 前 conflict。schema/正例位于
`api/schema/v1alpha1/review-run-impact-index-fact.schema.json` 与
`examples/review-run-impact-index-fact.json`。

索引只缩小需要闭包校验的集合，不替代权威事实：实际命中的 terminal run 仍必须通过 whole-run closure，
命中的 nonterminal run 仍必须通过唯一 `run.created -> ExecutionSnapshot` 且 snapshot ID 与索引一致。缺 snapshot
或未 rebuild 的旧 row 分别进入 `execution_snapshot_unavailable` / `impact_index_unavailable` gap；索引 schema、
排序、source 或同 run 唯一性损坏失败关闭。每个 match 返回实际 frozen binding 与 source 字段，不能把 ID
同名当作命中。
`GET /v1/review-runs/{run}/findings/{finding}` 必须调用 control-plane exact join，而不是直接按 finding ID 扫
feedback ledger；这样可先证明 finding 属于 committed succeeded run，再分别返回 deterministic/governed initial
Decision、append-only human Decision、Feedback 和 Outcome。publication provider request/result 可能含外部评论
payload，不属于该只读 view；需要 publication 审计时应另设更窄权限和去敏契约。

同一 exact finding path 下的 `POST .../decisions`、`POST .../feedback`、`POST .../outcomes` 需要独立
`review_write`。三个 command wrapper 都不接受 actor、roles、recorded_at、run_id 或 finding_id；handler 从
literal path 与 process-fixed principal 注入这些 authority。Decision 使用 principal 的 exact `finding_roles`；
Feedback/Outcome 固定 actor ID/kind，并继续经过 control-plane run/finding ownership 与 code-host publication
provenance 校验。Feedback response 只返回 `candidate_only` Evaluation eligibility，不能直接成为 gold label。
Schema/正例为 `api/schema/v1alpha1/local-api-{finding-decision,feedback,outcome}-write-command.schema.json`
与对应 `examples/`。

本 transport profile 只允许 literal loopback bind 和 mandatory bearer，不提供 tenant/Workspace/IAM
证明。所有 response/error 分别携带 `argus.local_api_response.v1alpha1` 或
`argus.local_api_error.v1alpha1`；除 ReviewRun watermarked traversal 外，list 的 `next_cursor` 只表示稳定排序
marker；ReviewRun watermark 也不是 immutable Evaluation dataset snapshot。评测或 replay 若需要冻结集合，仍必须使用领域 ExecutionSnapshot/EvaluationRun 契约，不能把分页
结果当作可重放输入。

唯一未认证路由是 embedded `/ui/` HTML/CSS/JS 静态壳及根路径跳转；它不得注入 principal、token、store path
或任何领域数据。静态壳必须 no-store、same-origin resource policy、deny frame/referrer/permissions，并以
`default-src 'none'` 只放行 self script/style/connect。token 由用户输入后只保存在当前页面内存，API fetch
不得携带 ambient cookie，断开/pagehide 必须清除；所有 data/API（含 health）仍走同一 bearer/permission
handler。UI 不能新增绕过 command schema、fixed principal 或领域 transition 的内部入口。

Evaluation Case/GovernanceBatch 的完整 show identity 使用 singular query：
`GET /v1/evaluation/case?case_id=<exact>` 与
`GET /v1/evaluation/governance-batch?batch_id=<exact>`。这是因为领域 ID 允许 `/`、`:`，而 transport 明确拒绝
encoded path。query 只允许对应参数出现一次且非空；plural path show 只是 safe single-segment ID 的便利别名，
不能作为领域 ID 约束来源。

ExperimentBatch/RepeatabilityBatch 的本地异步恢复分别使用
`POST /v1/evaluation/experiment-batch/resume?batch_id=<exact>` 与
`POST /v1/evaluation/repeatability-batch/resume?batch_id=<exact>`。两者共用 strict
`argus.local_api_evaluation_batch_resume_command.v1alpha1`，body 只能提供 mutation；handler 以固定 principal
注入 actor/roles，要求 `evaluation_write`，并把 `resume_requested` 作为独立 append-only audit 追加，不能替换
原始 batch intent。adapter 必须先从 intent 的 exact `executor_template_ref` 加载并重验 credential-free
template，再用 service-owned context 异步运行；202 只代表恢复请求被接受。执行失败只能持久化受控
`execution_failed`/`service_stopping` 与 worker、lease、generation/fencing、observed time，禁止保存 raw
provider/subprocess error。Schema/正例位于
`api/schema/v1alpha1/local-api-evaluation-batch-resume-command.schema.json` 与
`examples/local-api-evaluation-batch-resume-command.json`。

本地平台提交复用 plural collection endpoint：
`POST /v1/evaluation/experiment-batches` 接受
`argus.local_api_experiment_batch_submit_command.v1alpha1` 的 `budget` 单变量与 1..86400000 ms
variant timeout、`model` 单变量与 exact model ID，或 `prompt`/`skill_pack`/`knowledge_pack` 单变量与
exact `component_refs`，以及 `index` 单变量与非空 ordered `context_providers`；
`POST /v1/evaluation/repeatability-batches` 只接受
`argus.local_api_repeatability_batch_submit_command.v1alpha1` 的 exact replay。两者都要求
`evaluation_write`，handler 注入 process-fixed actor/roles，并要求 request `created_at` 等于 mutation
`at`。caller 必须省略 `executor_template_ref`，command 没有 prompt/skill/knowledge path 字段；adapter
只能从 `api serve` 启动时已验证的 formal Pi profile 构造 replay flags，冻结 credential-free、
content-addressed template。组件 ref 必须排序唯一，并在每个 case 的 baseline ReviewRun 所导出的
tenant/organization/workspace/repository subject 下解析 exact artifact/content；adapter 在 batch admission 和
provider 调用前对所有 subject 做 formal closure preflight。prompt 保持固定 runtime ID；skill 只能替换既有
review phase 的同 ID/phase 集合，knowledge 只能替换既有同 ID/顺序集合，禁止借 variant 增删或重排维度。
index provider 必须是服务支持且具有 exact descriptor bytes 的本地 built-in adapter；adapter ref 与发布的
`argus.context_provider_adapter.v1alpha1` component digest 相同。每个 baseline 必须发生真实 provider policy
变化，否则整批在 intent 前拒绝。
随后 adapter 持久化 batch intent，再用 service-owned context 启动。相同 command 的 exact
idempotent retry 返回同一 record，已终态时不得再次调用 provider。202 只表示 intent 已持久化且被本地
launcher 接受，不表示成功；formal profile 未配置时以 invalid transition 拒绝。Schema/正例位于
`api/schema/v1alpha1/local-api-{experiment,repeatability}-batch-submit-command.schema.json` 与对应
`examples/`。model variant 只能替换服务启动时已配置 provider profile 下的 model ID，不能替换 provider、
API protocol、runtime、预算或 credential；template 冻结服务器构造的 exact model component/build identity，
并在每个 run intent 之后、provider 之前发布到其冻结 subject。prompt/skill/knowledge 必须引用同 subject 下
既有受治理 publication，不能让实验 command 或 API server 读取 ambient local path。

组件发布使用独立 `POST /v1/agent-components` 与 `component_write` permission。strict
`argus.local_api_agent_component_publish_command.v1alpha1` 只接受 committed
`baseline_review_run_id`、prompt/skill/knowledge contract、exact VersionedRef、canonical base64 内容和
mutation；不接受 tenant/organization/workspace/repository、actor、role 或 path。adapter 必须先验证完整
committed ReviewRun closure，再从其 ExecutionSnapshot/ReviewSpec 推导唯一 subject；handler 注入固定 principal
actor。ref SHA-256 必须绑定解码字节，prompt 还必须是严格 PromptBundle 且内部 revision 与 ref 一致，
skill/knowledge 必须满足有界 UTF-8 contract。publication 的 actor/audit/idempotency/time 与组件 identity 一并
持久化，exact retry 返回同一 binding，任何审计或 identity 改写都冲突。Schema/正例位于
`api/schema/v1alpha1/local-api-agent-component-publish-command.schema.json` 与
`examples/local-api-agent-component-publish-command.json`。这是本地受信 control plane，不是签名供应链或 hosted IAM。

ReviewJob transport 使用 `argus.local_api_review_job_submit_command.v1alpha1` 与
`argus.local_api_review_job_cancel_command.v1alpha1`。submit 需要独立 `review_execute` permission；请求以
`execution_profile` 选择互斥输入：`deterministic_review_v1` 只描述 repository target，
`formal_pi_review_v1` 只描述已成功 committed 的 `source_run_id`；二者都携带 deadline，均不允许 actor、credential、
inline overlay、外部 ContextRef 或 remote-write authority。服务用 principal actor + idempotency key 确定性生成 job/run identity，先按该 run 的 exact
ResolutionContext 从 lifecycle repository 解析 ConfigBundle/ConfigResolutionReceipt，再把两者和请求封成
content-addressed `argus.review_job_command.v1alpha1`，最后提交绑定 command URI 的 scheduling Workload。formal command
额外冻结 non-secret LocalPiBootstrap options、runtime/component exact bytes 与 governed pricing ceiling；credential value
始终是 execution-time env secret，不进入 command。
相同 actor/idempotency key 的 exact retry 返回原 command；任何字段变化冲突，配置后来发布/回滚也不能改变旧 job。

后台 local coordinator 复用 scheduling 的 class admission、lease、heartbeat、generation/fencing、callback、timeout
reconcile 与 permanent run cancellation；HTTP request context 不成为 execution context。显式 cancel 才同时写 run
cancel fence 并取消同进程活跃 execution。进程丢失后，过期 lease 可重派发；如果 exact run 已 terminal，worker
只补 scheduling callback。deterministic run ledger 已有 nonterminal authority 时以 `orphaned_nonterminal_run` 失败关闭，
不得用同一 run ID 重放部分执行；formal Pi profile 则使用同一 coordinator claim 的 lease/generation/fencing token 驱动
既有 stage ledger 恢复，不创建第二个 workload。formal 执行前必须重建并 exact-compare 已冻结 runtime/component，
漂移在 provider 调用前失败。该 local profile 不等于 Hailix production worker；本地物理
kill/restart 已覆盖 lease 接管与 generation fence；2026-08-27 真实 DeepSeek 又在 Pi revision 1
candidate-verification checkpoint 后切断 generation 1 lease owner，证明 generation 2 复用已完成 verifier、
提交正式成功终态并拒绝旧代 callback。Hailix worker recovery 和 platform attestation 仍是开放项。

`GET /v1/review-jobs/{job_id}/timeline` 需要 `review_read`，返回
`argus.workload_timeline_event.v1alpha1` 的 sequence-ordered page。它只投影属于该 workload 的 scheduling facts；
run-wide cancel 与 global reconcile 必须先收窄到该 run/workload。该视图不得改写事件、伪造 span，或暴露其他
workload 作为存在性 oracle。Schema/正例为
`api/schema/v1alpha1/workload-timeline-event.schema.json` 与 `examples/workload-timeline-event.json`。

`argus workload pressure --at <UTC>` 与 authenticated
`GET /v1/workloads/pressure?at=<UTC>` 读取同一 authority，返回
`argus.workload_pressure_snapshot.v1alpha1`。快照必须绑定 policy revision/SHA-256、ledger sequence、
显式 observation time，并让 global/class/tenant 的 queue、active、oldest-wait 与 workload state
counters 完整对账；admitted/queued/rejected/throttled 取自 durable admission fact。过期 pending/lease
只进入 `stale.*_requires_reconcile`，查询不得隐式追加 reconcile event。HTTP 端点只允许 GET、要求
`review_read` 并拒绝未知、重复或非 UTC query。Schema/正例为
`api/schema/v1alpha1/workload-pressure-snapshot.schema.json` 与
`examples/workload-pressure-snapshot.json`。

Schema/正例位于 `api/schema/v1alpha1/evaluation-case-review-assignment.schema.json`、
`api/schema/v1alpha1/evaluation-case-annotation.schema.json`、
`api/schema/v1alpha1/evaluation-case-adjudication.schema.json`、
`api/schema/v1alpha1/evaluation-case-activation.schema.json`、
`api/schema/v1alpha1/evaluation-case-reopen.schema.json` 与对应 `examples/` 文件。当前这是本地
CLI/ledger 加认证本地 HTTP 治理子集；外部导入有内容签名但没有平台签名身份，且仍不包含 hosted
多租户异步 job/UI 或生产 ObjectStore。

### 2.2 EvaluationRun 评分闭包

`argus.evaluation_run_request.v1alpha1` 只声明 evaluation run identity、evaluator revision、
创建时间，以及按 case ID 排序的 `case_id + expected_label_revision + review_run_id`；binding 还可显式
声明按 dimension ID 排序唯一的 exact `dimension_scope[] (id + revision + sha256)`。scope 是本次 evaluator
对该 case 的适用性声明，不会从 category 猜测，也不会自动把一个专业化正例算进所有 Skill。它不是执行
prompt，也不授予模型或远程副作用权限。record gate 必须闭合：

- Case 为 approved、active、evaluation eligible，许可包含 evaluation；
- 同 evaluation run/case 的 prompt/rule/model/index exposure 已先落账；holdout 只接受 not_seen；
- label revision 在评分前和提交前都保持 exact，竞态修订失败关闭；
- ReviewRun 已权威提交成功，TargetSnapshot URI 与 Case input snapshot 相同；
- formal GovernedReviewReport 与 committed ReviewRun 相互绑定；ExecutionSnapshot 和 ToolPolicy
  的 remote writes 都是 deny。

输出 `argus.evaluation_run.v1alpha1` 保存 exact run/report/snapshot refs、label revision、期望标签、
finding counters、逐 case verdict 和整数 summary。EvaluationCase/LabelCorrection 将 anchor provenance
Artifact 与结构化定位真值分离；结构化 anchor 必须包含安全 repository path、old/new/file side、
正向 line range 和 source SHA-256。当前 evaluator 声明 defect/category presence 可用；完整报告且存在
结构化真值时，localization 以 path/side/source digest 全等和 range overlap 计算 expected/matched
anchor 与 localized finding 计数。`false_positive_regression` Label 必须另存排序唯一的
`suppression_targets[].cluster_fingerprint`；只按 GovernedReport 中同 fingerprint canonical Candidate
的 disposition 计算 observed/suppressed/escaped/inconclusive target 整数事实。完整报告若 target 未
出现，filter efficacy 为 `suppression_target_not_observed`，不得由 Finding 缺失推断成功；partial
report 的定位与过滤都保持 unavailable。非 fix case 的 apply fidelity 为 not-applicable。Schema 与正例位于
`api/schema/v1alpha1/evaluation-case.schema.json`、
`api/schema/v1alpha1/evaluation-label-correction.schema.json`、
`api/schema/v1alpha1/evaluation-case-annotation.schema.json`、
`api/schema/v1alpha1/evaluation-case-adjudication.schema.json`、
`api/schema/v1alpha1/evaluation-case-activation.schema.json`、
`api/schema/v1alpha1/evaluation-run-request.schema.json`、
`api/schema/v1alpha1/evaluation-run.schema.json`、`examples/evaluation-case.json`、
`examples/evaluation-label-correction.json`、`examples/evaluation-run-request.json` 和
`examples/evaluation-run.json`。

当 `dimension_scope` 非空时，EvaluationRun 还必须生成同顺序的 `dimensions[]`。每项只接受 exact
dimension 的 governed Candidate/Finding；review receipt 直接按 dimension ref 归因，verification receipt
只能通过其 hypothesis occurrence 回连原 dimension。共享 context receipt 不分摊给任何 dimension。
结果保存 Candidate disposition、Finding/localization、review/verification task 状态、model turn、tool call、
累计 task duration 与 token facts；若没有成功的 review receipt，维度质量固定为 inconclusive，不能把
零 Finding 当成 clean/miss。新 formal Pi run 还必须把 exact `AgentReviewRawCandidateCollection` 纳入
`StageExecutionResult`、本地 admission 和 committed `ReviewRun.raw_candidate_collection_ref`；EvaluationRun
同时保存 hypothesis/raw direct refs，并按 dimension 复算 retained/merged-duplicate/rejected-invalid/
excluded-budget 整数事实。旧 formal run 没有该 ref 时 normalization 固定为
`raw_candidate_dimension_evidence_unavailable`，不得从 retained Candidate 倒推。context gap 只按
committed hypothesis coverage 与 receipts 归因：group/task-scoped gap 仅进入执行过该 group 的 dimension，
repository/provider 级 gap 进入所有实际有 review receipt 的 dimension；没有 review receipt 时保持
unavailable。ExperimentRun 只在两侧各自 closure 可用时生成 paired normalization/context-gap delta。
Analytics 以 `sum(fate_count) / sum(raw_candidate_count)`、scale=6 生成 duplicate/invalid/budget ratio；
分母为零或任一 normalization closure 不可用时该 arm 必须 partial，不能把 `0/0` 写成零缺陷率。
这些是 operational normalization signals，不单独证明 review quality。usage authority 仍是
`worker_self_report_diagnostic`，duration 是 receipt 的累计 task time，不是 case wall-clock 或 provider
attestation。

`fix_validation` 的 `EvaluationCaseRunBinding.apply_trial` 必须内嵌
`argus.apply_trial.v1alpha1`。trial 绑定 exact case/label revision/ReviewRun、input snapshot、
GovernedReport、Finding ID/fingerprint、Finding suggestion SHA-256 与 edit-script artifact；checks 只能是
严格的 dry_run/compile/test 前缀，并绑定 command digest、状态、duration 和 content-addressed evidence
artifact。EvaluationRun recorder 必须重新读取 edit/check artifact 并复算 digest/size。任一失败或三项
全部成功才是 conclusive；unfinished all-passing prefix 是 partial。conclusive trial 驱动
fix_valid/fix_invalid verdict，partial 驱动 inconclusive，绝不由 Finding 数量代替。authority 当前固定
`local_host_unattested`，不是 Hailix/provider/沙箱 attestation。Schema/正例位于
`api/schema/v1alpha1/apply-trial.schema.json` 和 `examples/apply-trial.json`。

`argus.experiment_run_request.v1alpha1` 声明 baseline/variant EvaluationRun、实验 revision 和唯一
非 `none` replay variable。`argus.experiment_run.v1alpha1` 只在两侧 evaluator、case ID、label
revision/policy/outcome/category 全等，且每个 variant ReviewRun 的 immediate source 和
ReplayVariable 与声明一致时生成。每个 comparison 保存两侧 committed ReviewRun refs、exact
ReplayChangeSet ref 和 baseline/variant config SHA-256；change set 必须复算通过且 remote writes deny。
逐 case transition 为 improved、regressed、unchanged 或
inconclusive；inconclusive 不被排成“中间质量”。当前 quality 可用；每个 comparison 必须包含由
两侧 EvaluationRun 冻结的 structured-localization availability、expected/matched anchor 计数和 exact
variant-minus-baseline matched-anchor delta；任一侧 localization unavailable 时 paired delta 也必须
显式 unavailable，不能把零当作未命中。comparison 同样冻结两侧 suppression/escape/inconclusive
整数事实；只有两侧 filter efficacy 都 available 才产生 exact variant-minus-baseline
suppressed/escaped target delta，否则 paired filter efficacy 显式 unavailable。comparison 也冻结两侧
Apply passed/failed check 整数事实；只有两侧 apply fidelity 都 available 才产生 exact paired delta，
否则 paired apply fidelity 显式 unavailable。每个 comparison 还必须包含由
baseline/variant committed ReviewRun 全部 StageAttempt `duration_ms` 求和得到的
`baseline_duration_ms`、`variant_duration_ms` 和 exact `latency_delta_ms`，并固定
`latency_authority=review_run_stage_attempts`。formal `filter_policy` 纯后处理没有 variant provider timing，
因此 comparison 固定 `latency_authority=unavailable_postprocessing_only`、variant/delta 为零占位，
ExperimentRun latency 以 `postprocessing_has_no_provider_latency` 显式 unavailable；不能把它解释为
零延迟收益。其他 variant 任一侧没有完整终态 StageAttempt timing 时拒绝生成 ExperimentRun；这些
authority 都不是 provider/Hailix attestation。cost/instability 缺权威事实时必须
携带 unavailable reason。若 EvaluationRun 带 dimension slice，baseline/variant 必须拥有相同的
dimension ID 集合；comparison 按 ID 配对并分别保留两侧 exact ref，允许 skill revision/SHA-256 改变，
输出 Candidate/Finding、累计 task duration、attributable token 与 verdict transition。不同 dimension ID
不能强行比较。analytics adapter 以受控 `review_dimension` 维度输出 token/duration ExperimentFact，避免与
case 总量重复计数；这些仍是本地 worker receipt 事实。Schema/正例位于
`api/schema/v1alpha1/experiment-run-request.schema.json`、
`api/schema/v1alpha1/experiment-run.schema.json`、`examples/experiment-run-request.json` 和
`examples/experiment-run.json`。

`argus.repeatability_run_request.v1alpha1` 声明一个 baseline EvaluationRun、1..31 个按 ID 唯一排序的
replay EvaluationRun 和 repeatability revision。baseline 的每个 ReviewRun 必须是 succeeded 非 replay；
其余每个 ReviewRun 必须是该 case baseline 的直接 succeeded `variable=none` exact replay。host 必须回读
committed run refs、ExecutionSnapshot、ReplayChangeSet 和 GovernedReviewReport，证明相同 Target、ReviewInput、
ConfigBundle、Workflow、Runtime/Profile、build identity、context/runtime evidence、ToolPolicy 与 remote deny；
chained replay、stale ref 或任意漂移都拒绝。

`argus.repeatability_run.v1alpha1` 保存每个样本的 Evaluation/ReviewRun/ref、GovernedReport ref、完整性、
排序 FindingID、matched-anchor、localization availability 和 verdict。逐 case 以 scale=1,000,000 记录
unordered sample pairs 的 Finding-set Jaccard 均值、exact-set rate 和 verdict-flip rate，并记录 finding
presence、expected-anchor hit 与 finding-count range。两个空的完整集合 Jaccard 为 1；任一 partial report
使该 case Finding-set comparison unavailable，reason 为 `incomplete_governed_report`，不得把不完整空集判为
稳定。verdict flip 仍可由 committed Evaluation verdict 计算。Repeatability 指标不等价于 precision、recall
或收益。存在 dimension scope 时，每维另存 exact dimension ref 和逐样本 Finding、Candidate/raw Candidate、
context gap、task terminal、duration、token、execution/usage availability 与 verdict；聚合保存 Finding-set/
verdict stability 和执行指标 min/max，任何 sample 缺 closure 都必须在对应 availability 中说明。
Schema/正例位于 `api/schema/v1alpha1/repeatability-run-request.schema.json`、
`api/schema/v1alpha1/repeatability-run.schema.json`、`examples/repeatability-run-request.json` 和
`examples/repeatability-run.json`。

`RepeatabilityBatchRequest` 在 provider 前冻结 baseline EvaluationRun、1..31 个排序唯一的 replay
EvaluationRun identity、RepeatabilityRun identity/revision、executor revision、exact
`executor_template_ref`、1..16 并发上限和排序的
case/exposure。每个 replay sample×case 都必须产生独立 direct `variable=none` ReviewRun；host 回读
committed run/ref、ExecutionSnapshot、ConfigBundle 和 ReplayChangeSet，证明 exact baseline execution
identity、remote deny 和零 changed fields 后才能 checkpoint。每个 sample 的全部 case 完成后生成其
EvaluationRun，最后生成 RepeatabilityRun。claim/heartbeat/checkpoint/terminal 使用 append-only stream、
sequence CAS、generation/fencing；恢复只补缺失单元，旧 lease 写入失败关闭。CLI input 必须省略
`executor_template_ref`，由本地 adapter 在 intent 前冻结并注入；持久化 intent 不允许缺少该 ref，resume
也不接受执行参数覆盖。`RepeatabilityBatchResult`
保存按 EvaluationRun ID、case ID 排序的 committed replay run/change-set refs。Schema/正例位于
`api/schema/v1alpha1/repeatability-batch-request.schema.json`、
`api/schema/v1alpha1/repeatability-batch-result.schema.json`、
`examples/repeatability-batch-request.json` 与 `examples/repeatability-batch-result.json`。

Analytics 不得把 RepeatabilityRun 降格成 Experiment arm。`argus.analytics_repeatability_fact.v1alpha1`
按 repeatability run/revision、case、可选 review dimension 和 metric 保存 fixed-point observation、样本量、
完整性、受控 Dimensions 与排序 source refs；source refs 至少包含 RepeatabilityRun 与 EvaluationRun。
Finding/execution/usage 不可用必须生成 partial fact 和原 reason，不得以零值 observed tile 代替。
Dashboard 使用 `repeatability.<metric>.observation`，canonical CSV/Parquet 分别使用
`repeatability.csv`/`repeatability.parquet`，并与 Experiment quality delta 保持不同事实表。

本地平台只读契约同样不得混淆这些事实：EvaluationRun、ExperimentRun、RepeatabilityRun 使用独立的
list/show endpoint；ExperimentBatch 与 RepeatabilityBatch 使用独立的状态 list/show endpoint。
handler 必须从进程固定 principal 派生 `evaluation.Access`，并调用领域 repository 的 Case-aware ACL；
请求不能提交 actor/roles。列表 marker cursor 仅用于稳定排序结果的有界浏览，不是评测集冻结快照，也
不能替代 ExecutionSnapshot 或 batch intent。

`ExperimentBatchRequest` 为每个 case 冻结 baseline run、expected label revision、baseline/variant
ConfigBundle **内嵌语义 SHA-256** 和按 prompt/rule/model/index 顺序的 exposure observations；该摘要
与 `ConfigBundleRef.SHA256` 的 artifact bytes digest 是两类 identity，不能互换。请求同时冻结 executor revision、
exact `executor_template_ref`、
唯一 replay variable 和 1..16 并发上限。Batch intent 必须先于 exposure/provider；每个 exposure 与
replay 使用从 batch/case 派生的稳定幂等键。executor 只返回 variant run ID，host 必须回读 committed
run/snapshot/config bundle/change-set 复算 artifact 与语义绑定后才能 terminal。`ExperimentBatchResult` 保存排序后的 variant
committed run/change-set refs。每个通过 host revalidation 的 case 都先追加独立 `case_completed`
checkpoint；恢复时只调度缺失 case，terminal 必须与全部 checkpoint 字节级一致。terminal 必须由 intent actor 写入，并引用已存在的 EvaluationRun 和
ExperimentRun。执行前必须取得 batch-scoped durable lease；claim/heartbeat/checkpoint/terminal 使用
同一事件流 sequence CAS。heartbeat 只可延长 exact lease ID、worker、generation 和 fencing token；
过期接管同时递增 generation/token，任何旧 lease 写入都失败关闭。本地 CLI adapter 必须在 intent 前把
exact Pi runtime/component bytes、非 secret options、pricing ceiling 与 variant 参数冻结为
content-addressed `argus.replay_executor_template.v1alpha1` Artifact；CLI 请求样例故意不带 ref，由 adapter
注入。持久化 intent 必须带 ref，resume 只能加载该 ref，runtime/component 漂移和 ref 不一致必须在
provider 前失败。provider credential value 不得进入模板。该契约只拥有 Argus Evaluation
批次状态，不复制通用 Worker Runtime。Schema/正例位于 `api/schema/v1alpha1/experiment-batch-request.schema.json`、
`api/schema/v1alpha1/experiment-batch-result.schema.json`、
`examples/experiment-batch-request.json` 与 `examples/experiment-batch-result.json`。

`CorpusSnapshotRequest` 在任何模型看到语料前选择排序唯一的 Case，并要求调用者提交 corpus purpose、
单一 split、预期 governance/label revision 与 source ReviewRun ID。`development` 只接受 train/dev，
`quality_gate` 只接受 test，`promotion_gate` 只接受 holdout。builder 只接受 approved + active + evaluation-eligible、
许可包含 evaluation 且 split 匹配的 Case；同一 snapshot 拒绝重复 clone group，holdout 拒绝任何历史
`seen` exposure。生成的 content-addressed `CorpusSnapshot` 冻结完整 Case 语义
SHA-256、split/clone group、source committed ReviewRun ref、TargetSnapshot ref 与 source ExecutionSnapshot
SHA-256。它不包含模型、prompt、skill 或 review output label，且拒绝需要 ApplyTrial 的 fix-validation。
Schema/正例位于 `api/schema/v1alpha1/corpus-snapshot-request.schema.json`、
`api/schema/v1alpha1/corpus-snapshot.schema.json`、`examples/corpus-snapshot-request.json` 与
`examples/corpus-snapshot.json`。

`FormalCorpusBatchRequest` 提供首次 formal 评审的 governed corpus 入口，而不是把已有 formal
结果当 baseline replay。请求必须引用 exact `argus.corpus_snapshot.v1alpha1` Artifact，并逐 Case 重申
label revision、source ReviewRun 和 prompt/rule/model/index exposure observations；可选 dimension scope
继续使用 exact component ref。runner 在任何 provider 调用前对 snapshot artifact eligibility、语料成员、
Case 当前语义、label、license/consent、holdout 权限与历史 exposure、source committed closure、target/snapshot digest 与
remote-write deny 做整体 preflight，并先写入所有 exposure。任一漂移都在 provider 前失败关闭。

CLI 在 intent 前冻结 credential-free `argus.formal_corpus_executor_template.v1alpha1`，包含 exact
runtime/component bytes、配置状态路径、预算与 pricing ceiling；API key 仍只在执行时从环境读取。
每个 Case 使用由 batch/case 派生的稳定 formal idempotency key，成功后 host 回读 committed
ReviewRun、ExecutionSnapshot 和全部 governed ledgers，再追加 `case_completed` checkpoint。进程若在
formal terminal 后、checkpoint 前退出，重跑从正式 ReviewRun 恢复而不再次调用 provider；checkpoint
后的 Case 直接跳过。全部 Case 完成后先追加 immutable `finalizing_at`，随后以同一时间和稳定 mutation
写入 EvaluationRun，最后提交 batch terminal，避免“EvaluationRun 已写而 terminal 时间漂移”的 crash
窗口。provider/preflight 失败只持久化固定 code/case/time，不保存 raw error。当前本地 runner 没有复制
Hailix Worker lease；跨进程并发的 provider 单赢家仍由 formal dispatch/generation CAS 保证。Schema/正例
位于 `api/schema/v1alpha1/formal-corpus-batch-{request,result}.schema.json` 与
`examples/formal-corpus-batch-{request,result}.json`。

## 3. ExecutionSnapshot

ExecutionSnapshot 引用而不复制以下不可变对象：

- TargetSnapshot；
- ConfigBundle/RulePack/WorkflowDefinition；
- agent/model/tool/MCP/Skill capability snapshot；
- CodeGraph/index、required runtime profile 和 image/build digest；
- Hailix Workspace/source input refs；
- authorization/network/data/approval/budget policy；
- parent run/replay lineage。

scope ReviewJob 可额外冻结 `review_shard_manifest_ref`。manifest 必须绑定 exact
`run_id + ReviewInputRef + target_digest + limits`，并把每个源文件守恒地归入一个 ordered
shard 或一个显式 gap。checkpoint 不是可变 manifest 字段，而是 append-only fact，绑定
outer workload 的 worker/attempt/generation/fencing；aggregate 只引用已验证的 succeeded
checkpoint。恢复不得读取漂移的仓库或重新规划已冻结 shard。

每个引用包含 schema version、revision 和 digest。缺少关键引用时只能运行明确标记的
`non_replayable` 调试任务，且结果不能发布、进入 gold dataset 或参与 promotion。

ExecutionSnapshot 必须在 dispatch 前封存，不包含尚未生成的 Task/Trace/output
Artifact。随后分开追加：

- `PlatformExecutionBinding`：StageRun/Attempt 与 Task、WorkerSession、WorkerRun、
  AgentTurn、实际 runtime/container identity、attempt generation、fencing token；
- `RunEvidence`：必须引用 exact `platform_execution_binding_id`、attempt/generation
  和幂等 identity，再记录 TraceManifest、ArtifactRef、checksum、sequence、
  completeness 和 terminal classification。

## 4. WorkflowDefinition

```json
{
  "schema_version": "argus.workflow.v1alpha1",
  "workflow_id": "local-deterministic-review",
  "revision": "2",
  "stages": [
    {
      "stage_id": "materialize_target",
      "kind": "materialize_target",
      "implementation_revision": "1",
      "input_contract": "argus.review_input.v1alpha1",
      "output_contract": "argus.reviewcore_artifact.v1alpha1",
      "depends_on": [],
      "executor": "deterministic-local",
      "required_capabilities": [],
      "budget": {
        "timeout_ms": 30000,
        "max_input_bytes": 16777216,
        "max_output_bytes": 16777216,
        "max_concurrency": 1
      },
      "retry": {
        "max_attempts": 2,
        "backoff_ms": 0,
        "jitter": false,
        "retryable_codes": ["stage_timeout", "temporary"],
        "unknown_outcome": "fail"
      },
      "failure_policy": "fail_run",
      "side_effect": "none",
      "replay_policy": "checkpoint"
    }
  ]
}
```

当前本地基线的确定性顺序为：
`materialize_target -> plan_context -> detect -> normalize -> verify -> adjudicate ->
report -> publish -> capture_feedback -> export_evaluation`。后三个 stage 在 remote
write、外部反馈或标签治理尚未发生时，只能记录 `remote_disabled`、
`awaiting_feedback` 和 `candidate_only`，不能据此生成 provider-published、
feedback 或 gold-label 事实。

约束：

- stage id 唯一；
- dependencies 必须存在且无环；
- 至少一个 stage；
- implementation revision、input/output contract、capability requirements、budget、
  retry、failure policy 和 replay policy 均为不可变定义的一部分；
- side effect 只能为 `none`、`remote_publish` 或未来显式扩展；
- `remote_publish` 必须使用 `replay_policy=forbidden`；replay workflow 禁止任何
  非 `none` stage 或 `forbidden` stage；
- executor 能力在 dispatch 前与 Hailix/ACP capability snapshot 比较。
- admission 必须同时校验 ReviewSpec policy、WorkflowDefinition digest、stage
  side-effect 声明和平台 attested effective capability；声明为 `none` 不能消除
  executor 实际拥有的 remote write、网络、项目配置加载或后台执行权限。
- ConfigBundle 的 `target.max_patch_bytes` 约束待评审目标，`budget.max_output_bytes`
  约束 stage 输出，两者不得互相代用；`budget.max_attempts` 只能收紧 stage retry
  上限，不能扩大 WorkflowDefinition。

严格 Go decoder、JSON Schema 和规范样例分别位于
`internal/workflow`、`api/schema/v1alpha1/workflow-definition.schema.json` 和
`examples/workflow-definition.json`；duplicate/unknown/trailing JSON 均拒绝。

每个外部执行 stage 还冻结 `ToolInvocationPolicy`：

- allowlisted tool/schema 与 path/network scope；
- per-call deadline、输出 byte/token 上限、并发和 delegation depth；
- cancel propagation 与 truncation/partial evidence 行为；
- 按错误分类的 retry、exponential backoff+jitter、max attempts；
- idempotency key、unknown outcome 收敛和禁止盲重试的副作用类型。

## 4.1 ConfigRevision 与 ConfigBundle

`ConfigRevision` 是不可变的 typed patch，当前支持六级 scope：
`platform -> tenant -> organization -> repository -> path -> invocation`。每个 scope
使用封闭 selector：platform 必须为空；其余 scope 必须包含完整 ancestry，且只能
携带本级 selector 字段。path selector 使用 repository-relative literal prefix，
invocation selector 不接受 path prefix。

Patch 至少修改一个 typed section。set patch 的 `add`/`remove`、rule 和 context
provider patch 的 `upsert`/`remove` 即使为空也必须显式出现。Workflow、agent、
model、detector 和 adapter 使用带 revision 与 SHA-256 的原子引用；模型凭据只允许
`secret://authority/path` 引用，配置中不保存 secret material。

`Resolve` 只应用与 resolution context 匹配的 revision，并按 scope、path depth
从低到高确定性合并；调用方输入顺序不影响结果。同一 identity 或同一适用 selector
存在歧义时 fail closed。当前字段合并策略为：

- scalar 使用 `override`；
- rules/context providers 使用 `ordered_stable_id_override`；
- target/tool/evidence/channel 集合使用 `set_union_subtract`；
- permission 和 redaction 安全边界使用 `deny_wins`；
- workflow/agent/model/credential 以及完整 calibration profile 使用 `replace_only`。

`finding_governance` 是可选的完整 effective section。任一 scope 开始配置后，最终 bundle
必须同时闭合 exact `CalibrationProfile`、`minimum_confidence_ppm` 与非负 `max_findings`；
profile 至少两个点，覆盖 raw PPM 的 0/1,000,000 端点，raw 坐标严格递增且 confidence
单调不降，并拥有独立内容 SHA-256。resolved policy 固定为 `effective@resolved`，自身 digest
同时绑定 profile、threshold 与 limit。示例曲线只用于机制验证，不代表统计校准质量。

`ConfigBundle` 只能是 `Resolve` 的完整输出，包含 resolution context、按 specificity
排序的 applied revisions、有效 RulePack、全部 effective policy、逐字段
`field_sources` 与逐操作 `explain`。RulePack SHA-256、Bundle SHA-256 和
`bundle-<digest-prefix>` identity 均由 Go validator 复算；修改内容而不重新解析会
被拒绝。自动发布只有在 remote writes 被允许、channel 非空且 comment limit 为正
时才有效；training 被允许时必须使用 strict redaction。

严格 decoder、Schema 和规范样例分别位于：

- `internal/reviewconfig`；
- `api/schema/v1alpha1/config-revision.schema.json` 与
  `api/schema/v1alpha1/config-bundle.schema.json`；
- `examples/config-revision.json`、`examples/config-bundle.json` 与
  `examples/config-revision.finding-governance.json`。

规范样例由 `scripts/config_examples` 使用 production default constructor 和真实
`Resolve` 生成。docs gate 对正例同时执行 Go/Schema 校验，对 unknown field 和关键
语义负例同时执行拒绝校验，并重新 Resolve revision，要求结果与 bundle 对象完全
相等。

JSON Schema 不能表达数组字典序、stable-id 跨数组冲突、field source 全字段闭包、
applied revision specificity 顺序、minimum evidence 与 evidence kinds 数量关系，
也不能复算 RulePack/Bundle digest；这些语义继续由 Go validator 作为权威。

### 4.1.1 CalibrationFitRequest 与 ProfileCandidate

`CalibrationFitRequest v1alpha1` 只接受显式 candidate-level 二元真值，不从 Finding、Verifier
结论、Feedback 点击或 case 级结果自动推导 label。每个 Observation 必须同时绑定 exact
EvaluationCase governance/label revision、ReviewRun、Candidate、cluster fingerprint、review
dimension revision/digest 和 raw confidence；服务端回读 committed ReviewRun 与 ReviewSpec，重验
repository、TargetSnapshot、Candidate 原始事实和 Artifact training/evaluation eligibility。

真值来源只有两类：本地双人 blind review + 独立 adjudicator，或已经过签名导入的 external
governance。两类都必须回连既有 governance record，并明确声明
`independently_adjudicated_not_argus_output`。`valid_defect` 还必须与当前 label 的 category 和
source-digest anchor 相交；`false_positive` 必须来自 whole-snapshot clean label，或命中
false-positive regression 的 exact suppression fingerprint。拟合 operator 不能是该样本 reviewer
或 adjudicator。因此 Argus 自己的 Candidate/Finding/verification 不足以成为 gold truth。

训练集只接受 `active + approved + split=train + training eligible`；验证集只接受
`active + approved + split=dev|test + evaluation eligible`。同 case、clone group 或 exact
run/candidate 不得跨分区或重复，train/dev-test 各自必须同时包含正负真值。holdout 不进入拟合或
常规验证，它继续只属于独立 promotion gate。

fitter 使用整数 PPM 的 pool-adjacent-violators 单调回归，不使用 float、随机数或 ambient time，
并输出覆盖 0/1,000,000 端点的 exact `CalibrationProfile`。独立 report 保存 overall、repository、
dimension 的 sample count、positive rate、mean raw/calibrated confidence、Brier、ECE，以及 train/
validation label-rate 与 mean-raw drift；样本不足的 slice 显式为 `insufficient_samples`，不伪造零漂移。
gate 失败也会持久化不可变 manifest/profile candidate/report，方便重放审计。

输出永远是 `auto_published=false` 的 `gate_passed|gate_failed` ProfileCandidate；拟合本身不会创建、
发布或激活 ConfigRevision。passed candidate 只能由 `internal/calibrationpromotion` 显式准备：服务回读
exact CalibrationRun SHA、仍为当前 lifecycle resolution 的 baseline ReviewRun/ConfigBundle 和其中的
published baseline ConfigRevision，保持 threshold/max-findings 不变，只替换 CalibrationProfile，生成
draft 后立即 validate，但不 publish。生成的 filter PromotionVariant 带 typed managed binding；通用
promotion register/gate/rollback 入口对它失败关闭，不能绕过领域校验。

managed gate 依既有顺序推进。`targeted_regression` pass 必须绑定一个
`variable=filter_policy`（旧 `finding_governance` 事实兼容读取）、无 regression/inconclusive 且每个 comparison 的 baseline/variant config
SHA 均与 plan 相同的 ExperimentRun。`fixed_holdout` pass 必须由不同于 fitter/owner 的 holdout actor
执行，EvaluationRun 的 case 集必须与 gate 完全相同，每个 ReviewRun 都使用 exact variant bundle，且
case 不得出现在 train/validation manifest；既有 exposure gate 仍要求 active promotion holdout 和四类
component 全部 `not_seen`。最后一个通用 gate 只令 plan 成为 `gates_passed`；调用方仍需显式 activate，
服务才按 rollout 发布 exact validated ConfigRevision。activate/rollback 都先持久化 intent，再以派生
幂等键跨 config/promotion ledger 执行，进程在任一外部写后中断均可用原 mutation 恢复。

CLI 为
`argus calibration fit|show|list`，本地 API 为 `GET|POST /v1/calibration/runs` 与
`GET /v1/calibration/run?run_id=...`；promotion CLI 为
`argus calibration promotion prepare|gate|activate|rollback|show|list`，API 位于
`/v1/calibration/promotion/{plans,plan,gates,activate,rollback}`，读写同时要求 evaluation 与 config
权限。Schema/样例位于
`api/schema/v1alpha1/calibration-fit-request.schema.json`、
`api/schema/v1alpha1/calibration-promotion-prepare-request.schema.json`、
`api/schema/v1alpha1/calibration-promotion-gate-request.schema.json`、对应 local API command schema 与
`examples/`。

激活后的质量监控使用独立不可变 `CalibrationPromotionObservation`，不回写 Plan、ConfigRevision、
Dashboard 或 Outcome ledger。请求必须显式绑定一个 active managed plan、activation 前完整结束的 baseline
snapshot、activation 后开始的 observation snapshot，以及版本化整数 PPM policy；服务在持久化前后重读
Plan 并要求 SHA-256 不变。两个 snapshot 必须具有相同 scope/group-by，每个 cohort 只选择
`RunSourceBinding.config_sha256` 与 plan 的 exact baseline/variant bundle 相同的 ReviewRun。

固定指标为 ReviewRun success/complete、published Finding 的 known Outcome coverage、known Outcome 中的
fixed 与 recurred-or-escaped rate。Feedback 接受、评论点击、模型分数不参与质量回滚判断。partial source
投影为 `unavailable`，分母不足投影为 `insufficient_data`，两者都不生成 regression；只有全部所需指标
可评价且超过 versioned threshold 时状态才为 `regressed` 并给出
`rollback_recommendation=true`。该字段只是证据化建议，监控服务没有 rollback port，不能自动修改配置。

CLI 为 `argus calibration promotion observe|observation|observations`；API 为
`GET|POST /v1/calibration/promotion/observations` 与
`GET /v1/calibration/promotion/observation?observation_id=...`，要求 dashboard read 以及对应的 evaluation/
config read 或 write 权限。契约、命令 Schema 和规范样例分别位于
[`calibration-promotion-observation-request.schema.json`](../../api/schema/v1alpha1/calibration-promotion-observation-request.schema.json)、
[`calibration-promotion-observation.schema.json`](../../api/schema/v1alpha1/calibration-promotion-observation.schema.json)、
[`local-api-calibration-promotion-observe-command.schema.json`](../../api/schema/v1alpha1/local-api-calibration-promotion-observe-command.schema.json)
和 [`examples/`](../../examples/)。

### 4.1.2 TrainingMaterializationRequest 与 DatasetManifest

`TrainingMaterializationRequest v1alpha1` 是 EvaluationCase 到下游训练系统之间的受治理交接，当前只生成
`reference_only_strict_redaction_required` manifest，不复制源码、prompt 或 evidence 正文。请求必须绑定
exact ConfigBundle ref、repository、dataset id/revision、创建时间，以及按 case ID 排序的 case binding；每个
binding 固定当前 governance/label revision、独立 label authority 和完整 ArtifactRef 集。

服务端只接纳 `active + approved + split=train + training eligible`、许可 `allowed_uses` 明确包含
`training` 的 Case；holdout、漂移 revision、物化 operator 同时是 reviewer/adjudicator、或 authority 无法回连
本地批准 adjudication / trusted external governance 时一律拒绝。请求提供的 refs 必须无缺失、无多余地覆盖
InputSnapshot、source evidence、label anchor refs 与 authority evidence refs。ConfigBundle 和每个 ref 都必须
分别通过 `training` 与 `export` eligibility gate；源 artifact 还会被实际读取以重验 size/SHA-256。配置必须同时
满足 `data.training=allow`、`data.export=allow` 和 `data.redaction=strict`，且 repository context 完全一致。

输出 manifest 冻结 Case 当前治理、label/license/consent/classification/provenance、全部 exact refs、配置 context
与 DataPolicy，并强制 `contains_source_bytes=false`、`self_labels_allowed=false`。manifest 由内容寻址 identity
保护，append-only ledger 保存原请求与 mutation；exact retry 返回同一 ref，同键异请求、同 dataset revision
重复、ledger/artifact 篡改或 quarantine/tombstone 均失败关闭。这是严格脱敏 exporter 的受治理输入边界，
本身不等于已经产出可直接 fine-tune 的源码样本。

CLI 为 `argus training materialize|show|list`；authenticated local API 为
`GET|POST /v1/training/manifests` 与 `GET /v1/training/manifest?manifest_id=...`，复用 process-fixed principal、
evaluation read/write permission 和 DatasetCurator/Adjudicator roles。契约、Schema 和样例位于
[`training-materialization-request.schema.json`](../../api/schema/v1alpha1/training-materialization-request.schema.json)、
[`training-dataset-manifest.schema.json`](../../api/schema/v1alpha1/training-dataset-manifest.schema.json)、
[`local-api-training-materialize-command.schema.json`](../../api/schema/v1alpha1/local-api-training-materialize-command.schema.json)
和 [`examples/`](../../examples/)。

### 4.1.3 StrictRedactionPolicy 与 TrainingExportBundle

`StrictRedactionPolicy v1alpha1` 是不可由调用方缩减或重排的内建文本策略：固定 policy ID/revision、12 个
detector ID、替换标记、1 MiB 单 artifact 上限和 policy SHA-256。transformer 只接收无 NUL 的 UTF-8 文本；
二进制、超限输入、策略漂移或第二遍仍产生匹配全部失败关闭。每个 source ref 生成一个内容寻址 redacted ref
和 `RedactionReceipt`，receipt 只记录 exact source/output ref、policy SHA、字节数、编码及按 rule ID 排序的
命中计数，不记录命中的敏感正文。该规则集通过 secret/path/identity leakage corpus 与幂等测试，但它是明确
覆盖范围的 deterministic detector，不宣称具备通用语义 DLP 的无泄漏保证。

`TrainingExportRequest v1alpha1` 必须绑定 exact manifest ID/ref 和完整 closed policy。Exporter 会重新读取并
验证 manifest、每个 source artifact 及其 training/export eligibility，重新执行脱敏，再把
`TrainingExportBundle v1alpha1`、redacted artifacts 和 append-only ledger 以内容寻址方式保存。restore 会重放
脱敏并逐字节比较输出和 receipt；quarantine/tombstone、ref 漂移、同 idempotency key 异请求或同 export ID
异请求都失败关闭。Bundle 保留受治理 sample，但 `contains_unredacted_source_bytes` 固定为 false。

CLI `training export build|show|list` 管理 bundle；`training export publish` 只允许把 `manifest.json` 与仅含
redacted body 的 `records.jsonl` 原子发布到 Argus store 外的全新真实目录。已存在目录只有在文件集合和字节
完全一致时才视为 exact retry，API 不接受服务器文件路径也不执行 publish。authenticated local API 为
`GET|POST /v1/training/exports` 与 `GET /v1/training/export?export_id=...`。对应 Schema/样例为
[`training-strict-redaction-policy.schema.json`](../../api/schema/v1alpha1/training-strict-redaction-policy.schema.json)、
[`training-export-request.schema.json`](../../api/schema/v1alpha1/training-export-request.schema.json)、
[`training-export-bundle.schema.json`](../../api/schema/v1alpha1/training-export-bundle.schema.json)、
[`local-api-training-export-build-command.schema.json`](../../api/schema/v1alpha1/local-api-training-export-build-command.schema.json)
和 [`examples/`](../../examples/)。当前输出是 provider-neutral portable JSONL，不是下游 provider 的训练作业
提交或完成 receipt。

### 4.1.4 TrainingJobPlan 与 ProviderJobReceipt

`TrainingJobPrepareRequest v1alpha1` 从 exact `TrainingExportBundle` 创建 provider-neutral、内容寻址的
`TrainingJobPlan v1alpha1`，冻结 provider/profile revision、base model exact ref、SFT objective、整数
hyperparameters 与 seed。Argus 当前不调用训练 provider：`execution_mode=external_manual`、
`remote_side_effects=deny` 固定，prepare/replay 均不会产生远端提交或重复计费。

外部执行只能通过 `TrainingJobObservationRequest` 追加 `submitted -> succeeded|failed|canceled` 两阶段事实。
每次 observation 使用 plan SHA 做 CAS，并绑定同一个 provider/external job ID；receipt 自带 semantic SHA，正文
另存为 content-addressed Artifact，restore 会重新读取、strict decode 并比较。receipt 固定声明
`authority=operator_recorded_unattested`、`contains_secret=false`，JobPlan 固定
`promotion_eligible=false`。因此成功状态只表示本地 operator 记录了外部结果，不能作为 provider attestation、
billing truth、模型发布或自动自举证据。

CLI 为 `training job prepare|observe|show|list`；authenticated API 为 `GET|POST /v1/training/jobs`、
`GET /v1/training/job?job_id=...` 与 `POST /v1/training/job/observations`。请求不能自报 principal，API 不持有
provider credential，也不执行远端训练。契约与样例见
[`training-job-prepare-request.schema.json`](../../api/schema/v1alpha1/training-job-prepare-request.schema.json)、
[`training-provider-job-receipt.schema.json`](../../api/schema/v1alpha1/training-provider-job-receipt.schema.json)、
[`training-job-observation-request.schema.json`](../../api/schema/v1alpha1/training-job-observation-request.schema.json)、
[`training-job-plan.schema.json`](../../api/schema/v1alpha1/training-job-plan.schema.json) 和
[`examples/`](../../examples/)。

embedded Operator UI 的“训练治理”页面只复用以上 manifest/export/job collection、detail 与 observation
端点。它可提交 materialize、export build、job prepare 和 operator observation，但不提供 filesystem publish
路径，也不产生 provider side effect。浏览器生成的 receipt SHA 仅密封 `contains_secret=false` 的回执 metadata；
其 authority 仍为 `operator_recorded_unattested`，不能提升为 provider 证明或 promotion evidence。

## 4.2 AgentReviewPolicy 与 ConfigResolutionReceipt

`AgentReviewPolicy` 是 `ConfigBundle.agent_review` 的可选 typed effective section。旧
ConfigRevision/ConfigBundle 没有该 section 时保持原 canonical identity；一旦任一匹配 scope
开始配置，最终结果必须完整闭合：

- agent、provider、model、prompt、API protocol 以及 candidate normalization implementation 的 exact
  `VersionedRef`；normalization 固定 `candidate-normalization@v0|v1|v2`，其 SHA-256 必须等于
  agent/worker implementation SHA-256；
- 以 stable ID 有序合并的 Skill pack（`grouping | context | review | verification`）和
  Knowledge pack；Skill 至少包含一个 `review` phase；
- `provider_broker_only` model egress、tool allowlist、tool network deny、
  `frozen_input_only` workspace read、workspace/remote write deny、delegation depth 0；
- file/group/hypothesis/model-call/tool-call/target/group/output/token/cost/timeout/concurrency
  的正数预算。

resolved policy 固定为 `effective@resolved` 并拥有自身 SHA-256。credential 仍只允许
`secret://authority/path` ref；secret material 不进入 ConfigBundle、component artifact 或
`AgentStagePlan`。

`ConfigResolutionReceipt v1alpha1` 与 ConfigBundle 分离，避免 bundle digest 自引用。它固定：

- exact `ResolutionContext`、`bundle_id` 与 `bundle_sha256`；
- 每个 applied revision 的 SourceRef、revision digest、publish event ID/sequence/time 和
  assignment digest；
- 正常解析使用 `origin=lifecycle_published`；受治理 replay variant 使用
  `origin=replay_variant_derived`，额外绑定 source receipt、baseline bundle、单一 variable 与
  sorted changed fields，不能冒充 lifecycle publication；两者都有 canonical receipt ID 与 SHA-256。

receipt 是 **local governance record**，不是签名，也不是 remote/platform attestation。
`Validate`/`ValidateAgainst` 只验证 receipt 自洽并精确绑定 bundle，不证明 lifecycle ledger
中真实存在这些发布。正式入口必须调用
`GovernedConfigProvider.ResolvePublishedWithReceipt(context)`。应用入口在访问 provider 前先把
authenticated host adapter 注入的 `AgentPlanningSubject`（tenant/organization/workspace/
repository）与 ReviewRun/ReviewSpec/resolution context、repository provider、invocation 及
selection/non-selection path scope 逐项闭合；同一次受信配置 repository projection 再原子
返回 bundle 与 receipt。caller 自报 organization、legacy `ResolvePublished` 都不能用于
formal Agent Stage planning。低层 compiler 只验证 record integrity，不建立 publication 或
authentication trust。

本地平台的 `argus.local_api_config_resolution_query.v1alpha1` 只封装 strict
`ResolutionContext`；`POST /v1/config/resolutions` 使用进程固定 principal 的
`config_read` permission，原子返回同一次 `ResolvePublishedWithReceipt` 的 ConfigBundle 与
ConfigResolutionReceipt。query 不含 mutation、actor 或 roles；调用方提供的 invocation ID 参与
rollout assignment identity，HTTP adapter 不得代造。Schema/正例位于
`api/schema/v1alpha1/local-api-config-resolution-query.schema.json` 与
`examples/local-api-config-resolution-query.json`。

Go 类型/严格 decoder、Schema 与规范样例位于：

- [`internal/reviewconfig`](../../internal/reviewconfig)；
- [`api/schema/v1alpha1`](../../api/schema/v1alpha1) 下的 `config-revision`、
  `config-bundle` 与 `config-resolution-receipt` schema；
- [`examples`](../../examples) 下对应 JSON example，包括 lifecycle receipt 与
  [`budget replay derived receipt`](../../examples/config-resolution-receipt.replay-budget.json)。

## 5. Finding 与 Decision

Finding 使用稳定业务身份，Decision 使用 append-only 事件：

```text
Finding
  identity + anchor + rule/detector lineage
  evidence refs + counter evidence
  raw/calibrated confidence
  content + optional fix suggestion

FindingDecision
  decision_id + finding_id
  action + reason_codes
  policy/evaluator revision
  actor + occurred_at
  prior_decision_ref
```

允许的基础 action：

- `verify`
- `reject`
- `mark_inconclusive`
- `suppress`
- `queue_for_human`
- `publish`
- `publication_failed`

自然语言 reason 不能替代稳定 `reason_code`。

用户反馈与工程结果不能伪装成 Decision：

- `Feedback`：`accept | dismiss | wont_fix | outdated | needs_discussion`；
- `Outcome`：`fixed | recurred | escaped | unknown`。

control-plane 的 Finding resolver 同时接受 committed legacy `FindingSet` 和 formal
`GovernedReviewReport`，但输出保持 source-typed：`finding/decisions` 与
`governed_finding/governed_decisions` 互斥，不能把 formal evidence 强转成 deterministic Finding。
后续人工 Decision 使用 `FindingDecisionRequest + FindingDecisionMutation`：request 提交
`run_id/finding_id/action/reason_code/evidence_refs/occurred_at`，mutation 提交幂等键、actor、
canonical roles、audit 和记录时间。source root 不属于调用方输入；control-plane 从 committed
run 解出 `source_contract/source_sha256/initial_decision_id`，ledger 从 sequence 2 连续追加并把
每条记录绑定到相同 root。`publish` 仅授权 `publication_approver`，`reject/human_review` 授权
`finding_reviewer` 或 approver；同幂等键异值、断裂 prior/sequence、source root 漂移、损坏恢复
以及 replay publish 全部失败关闭。对应 strict contracts 与样例见
[`finding-decision-request.json`](../../examples/finding-decision-request.json)、
[`finding-decision-mutation.json`](../../examples/finding-decision-mutation.json) 和
[`finding-decision.json`](../../examples/finding-decision.json)。

review ExecutionSnapshot 永远不授予远程写。发布审批人另行提交
[`PublicationGrantRequest`](../../examples/publication-grant-request.json) 与
[`PublicationGrantMutation`](../../examples/publication-grant-mutation.json)；control-plane 将
[`PublicationGrant`](../../examples/publication-grant.json) 精确绑定到一个 publication identity、
审批人选择的 provider/repository/pull-request/channel/权限 revision，以及服务端解析的最新 publish
Decision、run/snapshot/config/source 和 diff base/head，
生命周期上限 24 小时。Grant ledger append-only；dispatch authorizer 以
[`PublicationGrantReservation`](../../examples/publication-grant-reservation.json) 单次消费，exact retry
幂等，不同的第二次消费拒绝。

远程发布调用方只可提交 [`PublicationIntent`](../../examples/publication-intent.json) 的
`grant_id + created_at`。control-plane 从 Grant 与当前 committed authority 派生
[`PublicationRequest`](../../examples/publication-request.json)：其中
`finding_source_ref + finding_source_contract` 必须精确区分 legacy FindingSet 与 formal
GovernedReviewReport，并绑定最新 post-review `publish` Decision。publication service 不接受缺失
`RequestAuthorizer` 的构造；dispatch 前验证当前 Decision/config/source 仍能重建 byte-equivalent
request 并 reserve Grant。provider 的 fresh head、permission、finding/anchor current 与
unknown-outcome lookup-only recovery 仍是独立边界。request build 不代表发生了远程副作用。

首个 provider adapter 是 GitHub Pull Request：fresh revalidation 要求 PR open 且 provider base/head
逐项等于 Grant，inline anchor 必须仍落在 GitHub changed-file patch 的 head line 上，permission broker
必须返回 exact expected revision。credential source 和 broker 是 mandatory ports，secret 不落盘。
create body 带 idempotency-key digest marker；网络在 create 后断开时 ledger 进入 unknown，reconcile
只分页查询 inline/issue comments 的 marker，不再次创建。当前 Argus CLI 尚未绑定生产 credential/IAM
实现，因此 provider contract test 通过不等于真实 GitHub 评论已发布。

local MVP 另提供显式 `publication github dispatch/reconcile --single-user-local`。该 profile 只读取三个
固定环境变量，不接受 token 参数或任意 env 名，且 Grant 必须绑定
`expected_permission_revision=single-user-local-v1`。`dispatch` 可以创建远程评论；`reconcile` 只接受
`dispatching/unknown` 或读取 terminal publication，拒绝从 `requested` 发起新写入。生产部署必须用
Hailix/IAM 实现 mandatory ports；本地 profile 不构成多租户授权证据。

代码托管平台来源必须绑定 publication ledger 中实际 published comment；Argus UI/API 可对
`queued_for_human` formal Finding 记录人工 Feedback/Outcome。analytics 只有在真实记录存在后才投影
未发布 human queue，且这些事实不计入 published acceptance/ROI；无记录时不能生成
`no_feedback/no_outcome` 来暗示用户已看见该 Finding。

看板漏斗通过 projection 按 finding/run/revision/attribution window 连接三类 ledger，
但不回写或覆盖其源记录。

## 6. Stage Artifact

Stage 输入和输出统一使用 ArtifactRef，并增加 Argus domain metadata：

```json
{
  "artifact_ref": "...",
  "contract": "argus.candidate_set.v1alpha1",
  "review_run_id": "...",
  "stage_run_id": "...",
  "completeness": "complete|partial",
  "completeness_reasons": [],
  "record_count": 0,
  "content_digest": "sha256:..."
}
```

Hailix 当前 ArtifactRef 只接受 `report`/`patch`，正式集成前需要通用 typed artifact
扩展，而不是把结构化 candidate 冒充 report。

## 6.1 Local Agent Review Shadow 契约

M1.3 先为 Pi direct-provider CLI 冻结一组 Argus-owned shadow 契约。它们只建立本地
证据闭包，不接入 `StageExecutionRequest`，也不声称获得 Hailix fencing、Worker
attestation 或正式执行授权：

| contract | 语义边界 |
|---|---|
| `AgentReviewPlan` | 在模型调用前冻结 source/execution/review identity、execution/review artifact refs、target digest、实现/normalization/分组/context/review/verifier/knowledge/runtime/profile/agent/provider/model revision、protocol、tool policy 和预算（包括 `max_target_bytes`）。normalization selector 只允许 `candidate-normalization@v0|v1|v2`，并必须与 worker implementation/agent 共用 exact SHA-256；worker 由 selector 派生 execution snapshot 与 checkpoint workflow revision，宿主用同一 selector 独立重算 normalization。当前 frozen-input Pi stdio worker 只接受 1 个 context dimension、1..16 个 review dimensions、0..8 个 governed knowledge refs，以及按序精确为 `list_files/read_file/search_code` 的 allowed tools；Go、Schema 与 worker 必须同步修改这些边界。两个输入 contract 分别固定为 `argus.execution_snapshot.v1alpha1` 和 `argus.review_input.v1alpha1`。`execution_class=local_direct_provider_shadow`、`attestation=non_attested`、`side_effects=deny` 固定。 |
| `ReviewHypothesisSet` | `normalization_decisions` 保留每一个 raw candidate 的 claim digest、action 和 reason；`Hypotheses` 只物化 canonical `retained` occurrence 及其 dimension、exact source anchor/evidence digest、verification observation、coverage gap 和 dedup cluster。`merged_duplicate` raw decision 只指向 canonical occurrence，不重复创建 Hypothesis 或消耗 verifier；reason 明确区分 exact `duplicate_fingerprint` 与宿主可重算的保守 `semantic_duplicate`。无效或超预算 candidate 分别显式记为 `rejected_invalid`、`excluded_budget`，不能静默丢弃。`confirmed` 仍只是 hypothesis observation，不创建 `Finding` 或 `Decision`。 |
| `AgentReviewRawCandidateCollection` | 独立保存 worker 提交、宿主 normalize 前的有界 typed claim，包括 `rejected_invalid` 与 `excluded_budget`；每项绑定 plan review dimension、group、ordinal、canonical claim digest、action 和 reason，并与 `ReviewHypothesisSet.normalization_decisions` 一一闭合。固定 `authority=worker_self_report`、`disposition=shadow_only`；其 anchor/excerpt 未经宿主源码验证，不能作为 Finding evidence、gold label 或 platform attestation。 |
| `AgentReviewTaskEvidenceCollection` | 独立保存实际执行 Pi task 的 exact system/user prompt、结构化 task output 和按执行顺序排列的 typed tool arguments/results。内容为 `exact_local_sensitive`；worker/report 预算不足时只移除正文，保留 SHA-256、字节数与 `evidence_budget_exceeded`，缺失 synthetic dependency task 使用 `task_evidence_unavailable`。Go host 将 identity/status、prompt/output digest 和 tool invocation/failure counter 与 receipt 交叉复算。固定 `worker_self_report/diagnostic_only/shadow_only`，不含 assistant reasoning，不是 provider transcript、Hailix Trace、attestation、Finding evidence 或 gold label。 |
| `AgentExecutionReceipt` | 每个 context/review/verification Agent task 的 worker self-report，只含 versioned runtime/profile/model/protocol、时序、状态、prompt/output digest、`model_turns_started/completed`、tool/token counter，不保存 prompt/output 正文；started 但未 completed 的 provider 失败不会被抹掉。成功 task 必须同时有 `prompt_digest`、`output_digest` 和恰一次成功的、与 role 对应的 typed terminal submit（`submit_context | submit_candidates | submit_verdict`）；schema、bounded-text 或 frozen-source evidence 校验拒绝的 terminal 尝试可在这次成功前重试，并以 `failure_count` 留证。任何 started model turn 必须有 prompt digest，failed/canceled 禁止 output digest，零 turn canceled 可没有 prompt digest。terminal submit 计入观测总数但不占只读 repository-tool budget。token usage 必须显式为 `provider_reported`、`partial` 或 `unavailable`：`partial` 保存已完成 turn 的正数观测下界并携带缺失部分 reason，`unavailable` 携带 reason 且不以裸 `0` 冒充已观测值；started 大于 completed 时禁止标为完整 `provider_reported`，reasoning breakdown 可保持未观测。`provenance_class=worker_self_report` 与 `authority=diagnostic_only` 固定；不能用作 platform attestation、billing truth 或 promotion evidence。 |
| `AgentExecutionReceiptCollection` | Manifest 所引用的 receipt artifact 闭合 envelope；冻结 plan/source/execution/review lineage，items 按 `task_id + receipt_id` 稳定排序且两类 identity 分别唯一。每个 item 仍逐条经过 `AgentExecutionReceipt` strict validation。 |
| `AgentReviewResultManifest` | 由 Argus Go host 生成，固定引用 `execution_snapshot_ref`、`review_input_ref`、`agent_review_plan_ref`、`hypothesis_set_ref`、`raw_candidate_collection_ref`、`agent_task_evidence_ref`、`agent_execution_receipt_ref` 七个 artifact，并从 decoded hypothesis/receipt 复算 summary/status。`disposition=shadow_only` 固定。 |
| `AgentReviewObservation` | Argus 基于已校验 manifest 追加的去敏 analytics fact，只保留 version/dimension、counter、reason code 和时间；不保存源码、prompt、transcript、credential 或 tool payload。 |
| `AgentReviewPromptBundle` | 受治理的 behavior-bearing prompt contract；固定 context/review/verification 三类 system role instruction 与 terminal finalizer instruction。每个字段有 UTF-8/trim/byte bound，strict decoder 拒绝 duplicate/unknown/null/trailing JSON。worker 仍在其外包裹代码所有的只读、凭据、repository-untrusted 与证据安全基线，配置不能覆盖安全授权。 |
| `ModelProfile` | `argus.model_profile.v1alpha1` 将 Argus 内部可用于 registry/path 的安全 component identity 与 provider-owned opaque `wire_model` 分开。artifact 同时冻结 exact `provider_id` 与未经 normalize/sanitize 的 wire model；formal host 重算 contract/digest/size/provider closure 后才把 wire model 降低到 worker plan。类似 `deepseek-v4-pro[1m]` 的合法供应商标识不会被塞进内部 ID，也不会被静默改名。 |
| `AgentReviewWorkerRequest` | Go host 到本地 stdio Pi worker 的独立 fenced envelope；携带 work item/attempt/generation/fencing/idempotency identity、deadline、完整 `AgentReviewPlan`、base64 内联的 immutable `ReviewInput`，以及 exact `prompt_bundle(ref + artifact binding + base64 bytes)`、按 `plan.review_dimensions` 顺序排列的 `review_skills[]`、按 `plan.knowledge` 排列的 `knowledge_packs[]` 和按 ReviewInput ContextRef 顺序排列的 `context_artifacts[]`。context transport 绑定 context ID/kind/revision、immutable local Artifact URI/contract/digest/size 与 base64 bytes；ContextGap 不伪造 payload。请求还冻结从 exact Plan/ReviewInput 派生的 `checkpoint_scope_sha256` 和按 group ID 唯一排序的 `group_checkpoints[]`；每项以非负 `checkpoint_revision` 同时绑定 transport 与 content identity，最多 256 项，单项最大 16 MiB、合计最大 64 MiB，Go/TS 双侧重算 canonical base64、size、SHA-256、scope、group 与 revision。Go host/TS worker 复算并 strict decode，formal host 从 source-run local content-addressed Artifact 解析 exact bytes，不能用 ambient/latest/runtime 常量替代。knowledge/context/checkpoint 只作为 untrusted reference evidence，不能扩大 ReviewInput anchor、改变工具、凭据、副作用或输出契约。capability 只描述 protocol、frozen-input transport 和 input/output byte limit，不含 credential 或授权，且 `sha256` 绑定按 key 字典序的 compact JSON（计算时 `sha256=""`）。 |
| `AgentReviewWorkerResult` | worker 对 request identity、fencing 和 capability digest 的 exact echo。`succeeded` 只携带由原始 JSON bytes 计算的 `report_sha256 + report`；`failed/canceled` 只携带结构化稳定 `failure(code/message/retryable)`。它仍是 worker self-report，不是 Hailix attestation，也不直接成为 manifest/Finding。 |

formal stdio worker deadline 由外层 stage window 确定性降低：预留 `min(5s, window/10)` 给结果解码、callback
认证和持久 terminal admission。host mapper 必须从 immutable worker-plan creation time 与外层 deadline 重算
相同值；任意更早/更晚替换都按 execution-fence drift 拒绝。外层 deadline 不变，预留不能扩展模型或副作用权限。

worker 可在 stderr 发送 `argus.agent_review_worker_progress.v1alpha1` JSONL；只有
`phase=checkpoint` 会进入 durable ledger。checkpoint 必须绑定当前 work item、scope、group、revision、
content digest/size。revision 0 只允许表示 context 成功并完成全部 review skill 的组结果；此后每个
revision 都是该组累计增加一个成功 verifier 结果的完整快照。失败/中断 verifier 不得伪装为 completed
checkpoint。host 先将内容写入 content-addressed Artifact，再在 scope stream 中以 generation/fencing CAS
追加；同 revision 不同内容冲突，已落盘较新累计 revision 后到达的较旧 revision 安全 no-op。新 generation
会 fence 旧 generation 的迟到写，并只复用更早 generation 每组最新 revision。
ledger 必须保存每个 checkpoint 对应的 host-authored generation binding；恢复请求若实际复用 prior
checkpoint，`AgentReviewPlan.created_at` 只能下调到这些 binding 的最早时间，不能使用 checkpoint
self-report 构造窗口。这样既允许严格映射 prior-generation task receipt，又不放宽无 checkpoint 的执行。
恢复仍重跑全局 normalization/dedup；checkpoint 中的 verification 只有在 candidate ID、blind candidate
canonical SHA-256、verdict identity 与 frozen source evidence 全部重验一致时才复用，unknown candidate、
重复/乱序 entry、candidate/revision substitution 或无证据 confirmation 均在新模型调用前失败关闭。未完成
verification 会重新执行。checkpoint 仍是 worker self-report，最终完整 report 必须经过正常 Go mapper 与
terminal admission。progress callback 失败会使本次执行失败，不能把未确认持久化的 checkpoint 当作可恢复事实。

frozen ReviewInput 只提供 target-side bytes。worker 在 Candidate fingerprint、checkpoint 和 evidence
校验前把 `new/file` alias canonicalize 为 diff=`new`、selection/scope=`file`，`old` 失败关闭；Go host
mapper 独立执行相同 canonicalization，application terminal admission 最后按 exact ReviewInput 再验 side、
source digest 与 excerpt。主 Hypothesis anchor 必须命中 ReviewInput 授权行；supporting evidence 可引用同一
冻结 file manifest 中授权行之外的精确源码上下文，但不能越过 frozen path/content/digest/line boundary。
TS/Go 都把末尾 LF/CRLF 视为前一行的终止符而非额外可寻址空行。raw candidate 仍保留 provider 原始 claim，
不能以 canonicalization 抹掉输入事实。

review 与 verification 的 terminal tool 在接受结构化结果前，必须对每条 evidence 执行同一 frozen-source
校验：repository-relative path 必须存在于目标 file manifest，range 为有效的 1-based 至多 20 行范围，excerpt
在 CRLF 归一化和 outer trim 后必须逐字匹配该完整范围。上下文 provider 的非目标文件事实可以支持推理，但
不能作为 terminal evidence 越权进入候选或 verdict。失败尝试返回 typed tool error，允许模型在同一 task 内
纠正；原始失败参数/结果进入 exact task evidence，receipt 的 terminal `failure_count` 同步增加。worker 接受
并不替代宿主信任边界：normalization、checkpoint 恢复和 application terminal admission 仍需独立重验 exact
ReviewInput，避免 runtime/host 实现漂移把非法证据提升为 Hypothesis 或 Finding。

跨 dimension 的 `semantic_duplicate` 不是模型自由声明的等价关系。当前保守 revision 只合并同一 group、
同一 target path、相同 target-side 语义且主 anchor 行区间相交的 Candidate；双方 title 经固定 ASCII
tokenization、stop-word/adverb 去噪、尾部复数归一和集合去重后，必须各至少 4 个 token、交集至少 4 个，
并满足以下一条：title Jaccard 不低于 3/4；或双方 title 共享同一 camelCase/underscore 代码标识符且至少
一对 source evidence range 相交后，canonical title token 交集至少 4 并覆盖较短集合至少一半；或在同样的
identifier/evidence gate 后，root-cause description canonical token 各至少 12、交集至少 12 且覆盖较短集合
至少 55%。排序后的首个
Candidate 成为 canonical winner，后续 raw claim 保留并指向该
occurrence，只运行一次 verifier。Pi worker 与 Go host 都从原始 claim 独立重算；exact fingerprint 相同却
声明 semantic、不同 group/path/side、不相交 anchor、relaxed path 缺共享标识符/evidence 或低于阈值均失败关闭。
该语义绑定 `argus-pi-review-workflow-v2`，v0/v1 checkpoint 不得复用。它只是一条 precision-first
启发式，不能替代 corpus oracle、人工 adjudication 或未来版本化 semantic judge。

`formal normalize-preview` 只能消费 committed succeeded formal ReviewRun 已闭合的
`AgentReviewRawCandidateCollection`。它以当前 policy 对 prior retained/merged raw claim 重算 cluster；prior
`rejected_invalid`/`excluded_budget` 固定为 `ineligible`，不能借离线工具提升。输出不含 claim text/excerpt，
固定 `diagnostic_only/preview_only`，不落盘、不调用 provider、不创建任何领域事实，因此不能作为 ReviewRun、
replay、Finding、Evaluation label 或 promotion evidence。

`formal normalize-compare` 在上述边界上增加 batch paired projection：每个 case 必须绑定 committed
succeeded ReviewRun ref 与 exact raw-candidate collection ref，baseline 是该 collection 已记录的
normalization decision，variant 是当前 deterministic policy。输出只保留 ID、ref、整数计数、decision delta
和 cluster identity，并以 `argus.normalization_policy_comparison.v1alpha1+json` content-addressed artifact
持久化；固定 `diagnostic_only/comparison_only`。它没有 oracle label、provider execution 或人工 adjudication，
所以不得写入 EvaluationRun、Finding、Decision 或 promotion gate。Schema 与示例分别为
`api/schema/v1alpha1/normalization-policy-comparison.schema.json` 和
`examples/normalization-policy-comparison.json`。

`NormalizationOracle` 不使用 Argus 的 cluster 作为 gold。它把一个已治理 CorpusSnapshot case/label revision
与 candidate-producing committed ReviewRun、exact raw collection、target digest 绑定，并要求所有 prior
retained/merged raw IDs 被 equivalence classes 恰好分区。adjudication 至少包含两个排序去重 reviewer、一个
不重合 adjudicator、可读取的外部 evidence artifacts，以及 `observed_policy_revisions` 污染事实。seal 会
重验 corpus ACL、replay root TargetSnapshot、whole-closure run/raw refs、eligible ID 集合和时间顺序。

seal 只产生不可变 oracle artifact，不能直接被 QualityRun 消费。`NormalizationOracleAttestation` 由外部
Ed25519 key 签 exact oracle SHA-256、oracle/authority/key/policy identity 和 issued time；该 exact key
revision 必须先进入现有 governance trust-key ledger，并覆盖 Case 的 repository/classification。随后
`NormalizationOracleRegistration` 以 expected current revision/event 做 CAS，append-only 保存完整 oracle、
attestation、key 和 immutable ref，以便重启不依赖 artifact 内容也能重验。替换产生新 revision；revoke
绑定 exact current revision/event，不删除历史。registration operator、reviewer/adjudicator 和 trust admin
必须独立。

`NormalizationQualityRun` 只消费当前活动的 `NormalizationOracleBinding`；binding 冻结 registry revision、
registration event、oracle ref、attestation ID、exact trust-key revision 及其 registration event。当前 CLI
只允许执行本地已实现的 exact policy revision。每个 unordered raw pair 按 oracle/prediction 是否同类形成 TP/FP/FN/TN，整数 PPM 指标定义为：
`precision=TP/(TP+FP)`、`recall=TP/(TP+FN)`、`false_merge_rate=FP/(TP+FP)`。零分母必须 unavailable；
同时保存 oracle/predicted unique-claim count、delta、exact partition match 与 policy exposure。show 不是直接
回显 artifact，而是重新授权并重算 exact oracle/raw closure；不一致失败关闭。该结果具有
`independent_oracle_evaluation` authority，但 policy 已曝光 case 不能作为该 revision 的独立 promotion evidence。
对应 schema/example 为 `normalization-oracle`、`normalization-oracle-attestation`、
`normalization-oracle-registration`、`normalization-oracle-revocation`、
`normalization-quality-run-request` 和 `normalization-quality-run` 六组 v1alpha1 文件。

`NormalizationPromotionPolicy` 分别冻结 test 与 holdout 的最小 case、eligible candidate、oracle duplicate/
distinct pair 覆盖，以及 pairwise precision/recall、false-merge rate、exact-partition 的整数 PPM 阈值；同时冻结
最小 shadow/canary 成功运行数和 1–99 的 canary percentage。`prepare` 从一个 succeeded baseline ReviewRun
回读 exact ConfigBundle，并要求它仍等于当前 lifecycle resolution；调用方指定的 baseline ConfigRevision 必须
同时出现在 receipt 中并拥有 `agent_review.normalization` 字段。服务只 clone 该 immutable revision 并替换
normalization selector，创建、validate 候选 ConfigRevision，重算候选 bundle，然后把 baseline/variant bundle、
两版 config revision/digest、候选/回滚 `candidate-normalization@v0|v1|v2 + exact worker SHA-256` 与 gate policy
完整 ArtifactRef identity/URI/digest/size 一并冻结到 `kind=normalization_policy` managed binding。候选与回滚必须来自同一 worker bytes，
policy revision 必须由 selector 唯一推导；quality service 可对三版 exact selector 重算。schema gate 由服务在
typed contract 和候选配置验证后写入共享 append-only promotion ledger，通用 promotion endpoint 不能推进该 variant。

`targeted_regression` 只接受 test split，要求 `dataset_curator + promotion_operator`；`fixed_holdout` 只接受
holdout split，要求独立于 owner 的 `holdout_runner + promotion_operator`。两者均通过同一
`normalizationeval.Service` 从活动 registry binding 重新授权并重算 entire oracle/raw closure；提交的
QualityRun Artifact 只是定位符，不是可信摘要。policy exposure、非独立 evidence、revoke、label/governance
漂移、split 混用、policy/time 不匹配均拒绝且不写门禁。覆盖不足或必需 metric unavailable 为
`inconclusive`，阈值未达为 `fail`，全部满足才为 `pass`；decision 另存不可变 Artifact，并通过
`normalization_quality_run_id` 绑定 gate evidence。`shadow_traffic` 只接受达到策略最小数量的 succeeded
ReviewRun，且每个 run 的 ExecutionSnapshot 必须绑定 exact variant bundle；`canary` 还要求候选配置已经以
策略指定 percentage 和非空稳定 seed 发布、run 完成时间不早于发布，并按该 run 的 resolution context 从当前
ledger 重新解析到候选 revision/receipt。安全事件非零会写 fail，样本不足写 inconclusive。随后
`promotion_authorization` 必须由 `promotion_approver` 本人签发，`rollback_monitor` 必须证明原 baseline 仍为
published、候选仍是可回滚的非 100% canary。全部七个 gate 通过只使 shared promotion projection active；
显式 `activate` 才用同一 seed 调用 `AdvanceRollout` 单调扩到 100%。显式 rollback 先恢复配置 publish frame，
再在 active promotion 上写 managed rollback；canary 阶段失败或中止也可只回滚部分发布配置。

配置 lifecycle 的 `rollout_advanced` 只允许扩大当前 percentage、禁止缩小/相等/seed 漂移，并保留第一次
publish 的 rollback frame 和 publish sequence。扩到 100% 时原 baseline 转为 superseded；之后 rollback
仍恢复 canary 之前的 exact baseline。该能力同时通过 config CLI 与 Local API 暴露，适合作为未来 Hailix 共用
的通用配置发布原语，但身份认证和远端 attestation 仍不属于当前本地实现。

对应 schema/example 包含 `normalization-promotion-policy`、`normalization-promotion-prepare-request`、
`normalization-promotion-gate-request`、`normalization-promotion-gate-decision`、
`normalization-promotion-operational-gate-request`，以及 prepare/quality gate/operational gate/canary/lifecycle
五类 local API command。prepare/canary/activate/rollback HTTP mutation 同时要求 evaluation-write 与
config-write；operational gate 要求 evaluation-write 与 config-read，request 不能自报 principal。
Candidate fingerprint 的 wire revision 与 Pi worker `JSON.stringify` 完全一致；Go host 编码该固定数组时必须
禁用 `encoding/json` 的 HTML escape，否则标题中的普通 `<`、`>`、`&` 会在 Go 侧变为 `\u003c`、
`\u003e`、`\u0026` 并制造假 fingerprint drift。包含这些字符的跨语言固定向量必须作为门禁。

formal local Pi 成功结果另外通过 `StageExecutionResult.agent_task_evidence` 引用同一
`AgentReviewTaskEvidenceCollection` governed Artifact。formal host admission 会 strict decode、校验
plan/run/execution/target identity，并持久化相同 digest/size/contract 的 local ref；canonical
StageExecutionResult、terminal winner whole-closure 与最终 `ReviewRun.agent_task_evidence_ref` 都必须
指向相同 bytes。该字段不是 `trace_manifest`，不能占用 `hailix.trace_manifest.v1alpha1` owner 边界。

同一成功结果还通过 `StageExecutionResult.agent_execution_receipts` 引用严格的
`AgentExecutionReceiptCollection`。formal host 在 terminal gate 之前校验 artifact contract、digest、
size 和 plan/source-run/execution/review-run lineage，再持久化同内容 local ref；最终
`ReviewRun.agent_execution_receipt_ref` 与 canonical terminal result 必须逐字节一致。EvaluationRun
只从这条 committed success closure 汇总 token counter，并标记
`worker_self_report_diagnostic`；receipt 缺失、为空或含 partial/unavailable usage 时，paired usage
保持 unavailable。该证据不是 provider/Hailix attestation，也不能推导真实账单成本。

当 worker envelope 成功且 report 已 strict decode，但 coverage/host mapping 仍失败或取消时，
`StageExecutionResult` 允许 `agent_task_evidence + agent_execution_receipts` 作为不可拆分的诊断对；
仍然禁止 output、raw candidates 和 Hypothesis。host admission 会对两份 artifact 分别 strict decode、
校验 plan/run/execution/target lineage、持久化 exact local bytes，并将 governed/local projection 只封入
`failed_result_accepted|canceled_result_accepted` terminal gate。失败 `ReviewRun` 的
`agent_task_evidence_ref`、`agent_execution_receipt_ref`、Finding/Report/Evaluation business evidence
必须保持为空；因此这条 closure 只能说明 worker self-report 的任务执行状态，不能把失败运行提升为成功
评审事实。如果该诊断对存在但任一 artifact 发布失败，host 以
`pi_failure_diagnostic_persistence_failed` 失败关闭并保留原始失败码为 completeness note，不能静默
丢失诊断后仍回显原故障。

## 6.2 Sensitive Artifact 本地治理

`AgentReviewTaskEvidenceCollection` 的 governed Artifact 不允许普通 `read`，而是同时声明：

| use | role | 语义 |
|---|---|---|
| `sensitive_process` | `artifact_sensitive_processor` | 仅受信 Go host strict decode、binding validation 和 bounded derivation；不得作为用户披露或 export 路径 |
| `sensitive_read` | `artifact_sensitive_reader` | 显式披露；只能通过 `ResolveSensitive`，不能调用通用 `Resolve` 绕过审计 |

regular `read` 与 sensitive read class 互斥，sensitive Artifact 必须同时允许 process 和
audited read；`export` 不在 allowlist。`ResolveSensitive` 在返回正文前，先将
`request_id + actor + purpose + UTC at + authority/scope/object/contract/content digest/size`
绑定为 `argus.artifact_sensitive_access_receipt.v1alpha1` append-only event。同一 request 的
exact retry 返回相同 stream/sequence/receipt；任何字段替换产生 audit conflict。purpose 是闭集：
`local_debug`、`evaluation_replay`、`incident_investigation`，不包含 export。

通用 `Result` JSON 永不序列化 exact collection，只返回 ref、content policy、state、
`export_policy=deny`、`retention_policy=until_explicit_revocation` 与
`deletion_semantics=logical_tombstone_shared_content_gc_deferred`。CLI 的显式读取还要求
`--acknowledge-sensitive-output --json`。revocation 使用现有 integrity-operator + idempotent
mutation ledger 写入不可逆 tombstone；此后 processing/disclosure 都失败，但去敏 manifest、
receipt、observation 和 analytics projection 保留可查。

tombstone 是逻辑撤销，不删除共享 content-addressed bytes。formal disclosure/revoke 不接受
caller-supplied artifact 或 scope，而是由 run repository 从 committed ReviewRun、最终 stage
attempt、dispatch intent、terminal gate 和 canonical StageExecutionResult 推导 governed binding
与 Subject；成功运行的 scope 为 `review_run_supporting_evidence`，失败/取消运行固定为
`terminal_failure_diagnostics`。`formal show` 只返回 authority/provenance/disposition、artifact state、
task role/status 和稳定 failure-code 计数，不返回 prompt、tool 或 output 正文；显式 `formal evidence read`
在 task evidence 的 audited sensitive-read receipt 落盘后，才同时返回 exact task evidence 与配对 receipts。
host-localized exact copy 继续用于 immutable terminal
closure。物理删除必须等待 exact ref inventory/reference count 和可恢复 GC。这里的 local role
adapter 不是生产 IAM、KMS、磁盘加密或 OS ACL 证明，Hailix 集成后必须由认证 Subject 和平台
policy 签发对应 authority。

### 6.2.1 Review Artifact integrity lifecycle

通用 ReviewRun Artifact 使用 `argus.artifact_integrity_change.v1alpha1` 作为人工状态变更输入，
绑定 exact `ArtifactRef(uri + sha256 + size_bytes + contract)`、单行 reason，以及
`idempotency_key + actor + audit + UTC at`。状态事件按 ref identity 写入 append-only ledger：

```text
active -> quarantined -> active
active|quarantined -> tombstoned
```

不存在 lifecycle event 表示 legacy-compatible active；它不表示内容已绕过校验。每次实际读取仍
重新校验 content-addressed bytes。校验失败会自动追加 `content_verification_failed` quarantine；
人工 release 在追加前和追加后都重验 bytes，失败则保持或重新进入 quarantine。tombstone 不可逆，
不会物理删除共享 bytes，也不会擦除 ReviewRun、Finding、Decision 或 evaluation lineage。

`CheckArtifactEligibility(ref, use)` 接受的 use 只有 `read/candidate_pool/publication/evaluation/training/export`。
quarantined/tombstoned 以及损坏的 lifecycle ledger 对所有 use 失败关闭。发布派生和评测记录除
正常 read checksum 外还必须分别通过 publication/evaluation gate。该本地 Argus ledger 不拥有
Hailix Trace sequence/integrity，也不证明 PostgreSQL/ObjectStore 跨存储对账。

## 6.3 Formal Agent Generation CAS

formal dispatch 的 provider authority 与 terminal admission 不能只在各自 stream 中做
“先读再写”。本地 authority repository 先把 sealed `AgentStageDispatchIntent` 或
`AgentStageTerminalGate` 写入同一 run-scoped generation decision stream，并使用 expected
sequence 做跨进程 compare-and-swap：

- 底层 sequence CAS 显式返回 `appended` owner bit；exact dispatch claim retry 返回既有 decision
  且 `appended=false`，不重复授予 first-Ensure ownership；
- 更高 generation claim 先赢时，旧 generation completion 以 stale 失败；
- completion 先赢时，后续更高 generation claim 冲突；
- cancellation 仍是 exact intent 的 terminal winner，但不伪装为整个 workload 的成功完成；
- coordinator event 内嵌 sealed intent/gate，并反向核对 authoritative admission/intent，未知字段、
  envelope identity/time 或 source binding 漂移均失败关闭。

该 CAS 只解决 Argus 本地 dispatch/terminal 的跨 stream TOCTOU。Argus 已提供严格
`hailix.platform_execution_http.v1alpha1` consumer client：mutation transport/5xx/invalid-success
一律保留 unknown outcome，恢复只重发 durable intent 中的 exact request；完整 wire contract 见
[`hailix-platform-execution-http-v1alpha1.md`](hailix-platform-execution-http-v1alpha1.md)。远端平台仍必须
提供 provider-authoritative atomic create-or-return-one-handle、outbox/recovery 与 authenticated
generation head；本地 decision、HTTP mock 或 consumer test 不能宣称 Hailix exactly-once 或
provider attestation。

`AgentReviewPlan.tool_policy.tool_network=deny` 指 Agent 可调用工具没有网络能力；host
到显式 provider endpoint 的模型传输由 `local_direct_provider_shadow` execution class
单独揭示，不能据此修改或放宽正式 `StageExecutionRequest` 的 `network=deny`。direct
provider receipt 永远是 non-attested diagnostic evidence。

Worker request validator 要求 `work_item_id == plan.execution_id`，并在启动 worker 前
严格 base64 解码 `review_input_base64`：decoded bytes 必须同时命中 capability/plan byte
limit、`plan.review_input_ref` 的 exact size/SHA-256 以及 `plan.target_digest`。Result binding
随后核对完整 attempt/generation/fencing/idempotency/capability identity、deadline 和 output
byte limit。这里的 fencing 只能防止 Argus 本地 bridge 接受 stale result；没有 Hailix
authoritative task/worker lease evidence 时，不得把它标成 platform-attested execution。

所有数组型 lineage/evidence/reason/tool usage 都必须显式出现并按稳定 identity 排序；
unknown、duplicate、显式 `null` 和 trailing JSON 一律拒绝。Hypothesis evidence digest 由完整
`evidence_id + statement + anchor/source_digest + excerpt` 复算；dedup cluster 必须覆盖
每个 canonical occurrence 恰好一次，且每个 Hypothesis 必须恰有一条 `retained`
normalization decision；duplicate raw candidate 通过 `merged_duplicate -> canonical_occurrence_id`
保留谱系而不物化第二个 occurrence。Exact evidence excerpt 还必须至少包含 8 个非空白
Unicode 字符，避免单字符“证据”形式化绕过。Manifest closure gate 同时核对
plan/set/raw-candidate/receipt identity、六个 ref、固定输入 contract、runtime/profile/agent/provider/model/protocol、预算与
host 重算 summary。Host 还会从唯一 `(role, group, dimension, occurrence)` task identity
复算 `groups_total * context_dimensions` 和 `groups_total * review_dimensions` receipt 闭包、
成功 review task 与完整 reviewed group；`complete` 要求所有 context/review task 成功，且每个
hypothesis 恰有一个成功 verification receipt 和至少一条由 plan verifier 产生的 observation。
未完成 verification 只能进入带 occurrence-specific verification gap/reason 的 `partial`。
Hypothesis dimension、verification observation verifier 和 receipt dimension 都必须命中 plan
冻结的 exact `VersionedRef`。`max_tool_calls` 只累计 plan `allowed_tools` 中的 repository tools；
role-matching typed terminal submit 另行限为恰一次成功；schema 拒绝的 terminal 尝试允许重试并保留 failure counter，其他 tool fail closed。`max_candidates`
只约束 retained canonical Hypothesis；超额 raw 必须落为 `excluded_budget` decision。Summary 分开记录 raw/retained/merged/rejected/excluded normalization 计数、
verdict 计数和 provider-reported/partial/unavailable usage receipt 数；unknown usage 不计作零成本或
零 token 的已观测事实。

Go 类型、严格 decoder、Schema 和规范样例分别位于：

- [`pkg/contracts/v1alpha1`](../../pkg/contracts/v1alpha1)；
- [`api/schema/v1alpha1`](../../api/schema/v1alpha1) 下的
  `agent-review-plan`、`review-hypothesis-set`、`agent-execution-receipt`、
  `agent-execution-receipt-collection`、`agent-review-result-manifest`、
  `agent-review-observation`、`agent-review-worker-request`、
  `agent-review-worker-result` schema；
- [`examples`](../../examples) 下同名 JSON example。

JSON Schema 负责 closed shape/enum、typed terminal role/次数和基础边界；数组排序、evidence digest、normalization/
dedup closure、repository-tool budget、receipt tool-call/token sum、coverage/verification 跨 artifact identity 和 manifest summary 重算由 Go validator 作为
权威。`agent_execution_receipt_ref` 当前引用一个 `task_id`、`receipt_id` 分别唯一且按
`task_id + receipt_id` 稳定排序的 receipt collection artifact，其 item contract 是
`AgentExecutionReceipt`；正式平台集成前不得把该
collection 冒充单条 platform-attested StageResult。

## 6.2 Formal AgentStagePlan 契约

S1 为 formal `agent_hypothesize` stage 定义 `AgentStagePlan v1alpha1`。它与 6.1 的
direct-provider shadow `AgentReviewPlan` 是不同契约；当前两条路径没有互相 dispatch 或导入。

| 字段组 | 冻结语义 |
|---|---|
| Run/stage/target | `review_run_id`、exact stage revision/digest、`target_digest`；target digest 必须等于 `review_input.ref.sha256` |
| Domain artifacts | ExecutionSnapshot、ConfigBundle、ConfigResolutionReceipt、WorkflowDefinition、ReviewSpec、ReviewInput 的 exact contract/SHA-256/size binding |
| Components | AgentReviewPolicy identity；agent/provider/model/runtime/prompt/API protocol；与 agent worker digest 相同的 exact candidate-normalization selector；按治理顺序保留的 Skill/Knowledge/context-provider adapter exact artifact |
| Runtime identity | runtime component 与 ExecutionSnapshot `build_identity`；两者都进入 behavior digest |
| Authority/budget | provider-broker-only model egress、tool/read/write/delegation ceiling，以及完整 Agent budget；不得静默扩大或 clamp 超限配置 |
| Output/effects | `output_contract=argus.review_hypothesis_set.v1alpha1`、`disposition=hypothesis_only`、`side_effects=deny` |

Plan 不表示 credential、provider endpoint、raw prompt、repository path、mutable latest、
Candidate 或 Finding。Skill、Knowledge 和 context provider 数组保持 policy 的 ordered
stable-ID sequence；工具 allowlist 作为集合 canonicalize。`behavior_sha256` 绑定会影响执行
行为的 stage/target/input、components/runtime/build、authority/budget/output；full `sha256`
另外绑定完整 run 与 publication lineage，`plan_id` 从 full digest 派生。

正式应用入口固定为：

1. authenticated host adapter 注入 `AgentPlanningSubject`；在访问任何 provider 前校验其
   tenant/organization/workspace/repository 与 ReviewRun/ReviewSpec/resolution context、
   repository provider、invocation 及 selection/non-selection path scope 都匹配，再从
   `GovernedConfigProvider` 的同一次调用获取 bundle + receipt；
2. 用 `AgentComponentResolver` 按完整已准入 subject 与 governed identities
   解析 exact content-addressed components，防止 confused-deputy；resolver request 不含
   credential；
3. 纯 compiler 对 snapshot/target/spec/input/config/workflow/component/authority/budget 做
   cross-artifact closure；compiler 不读文件、网络、环境、时钟、随机数、secret 或 provider；
4. canonical seal 并输出 `AgentStagePlan`。

当前 formal plan 支持普通单 stage 运行、具备完整 source/change-set closure 的
same-input exact replay，以及 `budget`、`model`、`prompt`、`skill_pack`、`knowledge_pack`、`rule_pack`、`workflow`、`index`、
`filter_policy` 九种单变量 variant replay。`filter_policy` 只接受 strict decode 且已 seal 的
完整 policy，只能替换基线通过正常 ConfigRevision lifecycle 启用的 policy；derived receipt、ConfigBundle、
ReplayChangeSet 与 ReviewRun 都绑定该变量，Pi build/tool/runtime authority 保持不变。当前它从
`finding_governance` 开始纯后处理：新 ReviewRun 通过 immutable source-run ref 复用 exact
Hypothesis/Candidate/Verification/GovernedReport/Markdown，只派生新的 Calibration/Suppression ledger；
StageAttempt、PlatformExecutionBinding、RunEvidence、raw/task/usage receipt 均为空，因而不会冒充新的
provider 执行或重复计费。链式 replay 保留 immediate source/root lineage，并从已验证的父链重建 exact
derived config receipt。旧 `variable=finding_governance` 仅保留历史事实重建、读取和 exact retry 兼容；
新 CLI/API/Batch 以 `filter_policy` 为 canonical 变量。`index` 从 `materialize_target` 开始在 source
immutable revision 上重新执行 ordered context-provider policy；其他 executable variant 仍从
`agent_hypothesize` 重跑 provider。formal retry 将 effective `max_attempts=min(ConfigBundle, WorkflowDefinition)`、
精确 backoff、排序错误码白名单和 `unknown_outcome` 写入 AgentStagePlan；unknown outcome 只恢复同一 attempt，
只有已认证 failed result 同时满足 `retryable=true`、错误码命中白名单且未耗尽次数时才能推进新 attempt。
本地 Pi worker 的 failure 属于 self-report：host 只保留 Plan 已授权且 worker 明确标记 retryable 的 bounded
错误码，message 始终脱敏；未授权码归一为不可重试 `pi_worker_failed`。对 envelope 成功但零 reviewed-group
的 report，只有 strict mapping 后全部 review receipt 都是 `provider_error`/`timeout`，才能映射成
`provider_error`/`deadline_exceeded` 候选；混入 dependency、structured-output 或 unknown failure 时不可重试。
终态 exact retry 从 claimed dispatch
ledger 解析最新内层 attempt/generation，不能用外层 scheduling lease 的 attempt/generation 代替。
仍拒绝 upstream dependency、任意 stage checkpoint reuse 和 budget/retry 之外的 workflow 变化，
其他情况失败关闭。这是 planning contract，不是执行证明：尚无
production Hailix executor、远端 credential broker 或 platform attestation。本地 Pi executor、
Plan persistence/dispatch、typed Hypothesis ingestion 与受限 Finding 转换见下节；不能把它们
外推为远端 trust root。

`StageExecutionRequest.capability.trust` 将执行信任域冻结进 capability digest 与 request
digest：`authority` 只能是 `local_host` 或 `platform`，并携带 exact
`capability_verifier` 和 `callback_verifier` VersionedRef。该字段只是不可变身份绑定，不是
attestation 本身。host 在认证 fresh callback 前必须从 dispatch intent 指向的 exact persisted
request 读取 trust，不能从 callback body 或当前 adapter 配置补出；verifier 返回值必须逐字段命中
冻结的 callback verifier。receipt-to-terminal crash recovery、terminal/evidence exact retry 同样
必须重读 proof-redacted receipt，并校验其 verifier 与原请求 trust 一致。切换 trust authority、任一
verifier ID/revision/SHA-256 都会改变 capability/request identity，不能沿用旧 dispatch/binding。

执行层另有有界的 Pi group/candidate-verification checkpoint。它不改变 Plan DAG，不开放
upstream dependency，也不复用 normalization、governance 或 terminal Artifact；verification 也只复用
exact candidate/evidence 重验后的 completed item，因此不能写成通用 stage checkpoint replay。

本地 formal Pi execution 已把 `AgentStagePlan.prompt` 从 provenance identity 变为实际执行输入：
PlatformPort 解析 exact governed Artifact bytes，worker envelope 绑定相同 ref/artifact/content，
TypeScript admission 重新计算 digest/size/revision 后才构造 runtime。context/review/verification
task 的实际 system prompt 与 terminal finalizer 都消费该 bundle；exact system/user prompt 继续
进入 governed task evidence，execution snapshot 回显同一 prompt revision/digest。prompt-only replay
已基于该闭包接通 derived ConfigReceipt、variant admission、CLI 与 baseline-vs-variant E2E；
`variable=prompt` 只允许改变 exact prompt component 与对应 build identity。

`AgentStagePlan.model.ref` 现在只表示安全、内容寻址的 governed component；它引用的
`ModelProfile` artifact 才保存 exact provider wire model。PlatformPort 在 subprocess 启动前 strict decode
并校验 provider/ref/artifact 三向闭包，worker request 的 `plan.model.id` 原样使用 `wire_model`，revision/digest
仍绑定 formal component。runtime、agent、grouping、context 与 verifier 这些随 worker build 变化的组件使用
`<base>-<sha256-prefix>` revision；registry 继续禁止同一 `id@revision` 重绑 bytes。bootstrap 遇到已经发布的
exact ref 时只解析复用，配置 revision 则从完整 ConfigRevision 语义独立派生，不能用 runtime manifest identity
替代 config identity。

formal PlatformPort 同样把每个 Plan review dimension 的 governed Skill Artifact 解析为 worker
`review_skills[]`。Go/TS 共同校验顺序、ref、contract、size、canonical base64、UTF-8 和 SHA-256，
运行时直接从这些 bytes 构造 SkillDefinition，不再按 skill ID 打开本地 builtin 文件。
本地 builtin 文件还必须声明唯一合法的 `Revision: builtin-vN`；host/standalone loader 从
正文解析 revision，并与 exact SHA-256 一起冻结，不能再由代码给全部 Skill 伪造同一个
revision。默认 pack 含六个职责分离的 review dimensions，协议上限 16 为正常配置 lifecycle
发布的 repository/business-line Skill 留出容量。
`skill_pack` replay 只允许保持完整 ordered skill set 的 ID/phase 不变并替换至少一个 exact Ref；
由此保留 baseline FieldSource provenance。新增、删除、重排或 rephase 必须通过正常 ConfigRevision
publication，不能由 derived replay receipt 伪造 lifecycle 来源。

Go 类型、Schema 和规范样例位于
[`pkg/contracts/v1alpha1`](../../pkg/contracts/v1alpha1)、
[`api/schema/v1alpha1/agent-stage-plan.schema.json`](../../api/schema/v1alpha1/agent-stage-plan.schema.json)
与 [`examples/agent-stage-plan.json`](../../examples/agent-stage-plan.json)。strict decoder 拒绝
duplicate/unknown/null/trailing JSON；跨 artifact identity、顺序、digest 和 budget 语义由 Go
validator/compiler 作为权威。

## 6.3 Verification、Calibration、Suppression 与 Governed Report 契约

formal succeeded terminal 先产生已准入 `ReviewHypothesisSet`，再由纯确定性 governance
projection 分别生成独立 `CandidateVerificationLedger v1alpha1`、`GovernedCandidateSet v1alpha1`、
`GovernedReviewReport v1alpha1`、`FindingCalibrationLedger v1alpha1` 与
`FindingSuppressionLedger v1alpha1`。Hypothesis 中的 observation 是 worker 执行证据；只有独立 ledger
是正式验证 authority，Candidate/Finding 中的 verdict 只是必须回连该 ledger 的只读投影：

| 事实 | 最小语义 |
|---|---|
| Verification | 每个 observation 形成稳定 `verification_fact_id`，绑定 Candidate/occurrence、sequence、exact verifier/verdict/reason/evidence；ledger summary 从每个 Candidate 的 latest fact 重算，并显式保留 unverified 数量 |
| Candidate | 每个 canonical retained Hypothesis 恰好一个 Candidate；独立 CandidateSet 保存原 Hypothesis 全量结构和 ledger-derived latest disposition，不能因 rejected/inconclusive 被物理删除；`candidate list/show` 同时返回验证 ledger/fact |
| Finding | 只有 latest verification observation 为 `confirmed` 的 Candidate 才能生成；字段必须是该 Hypothesis 的 exact projection。Report 内 confidence 仍保持 `available=false, reason=not_calibrated`，后置校准不得改写不可变 Finding |
| Calibration | Reviewer 可提交 0..1,000,000 的 `raw_confidence_ppm` 自报值，但该字段不进入 blind verifier 输入，也不是验证证据。配置了 exact `CalibrationProfile` 时，ledger 以单调分段线性、`uint64` 中间值和向下取整生成整数 PPM；缺 profile 或 raw score 时显式 `unavailable`，不得猜测或用零冒充 calibrated confidence |
| Suppression | 每个 Finding 恰好一个 sequence-1 suppression fact，绑定 calibration fact 与初始 Decision evidence。exact `FindingGovernancePolicy` 只有在全部 Finding 均可校准时才执行；按 confidence 降序、FindingID 升序生成稳定 rank，先应用 minimum-confidence，再对合格项应用 max-findings。每个 threshold/limit loser 仍保留为 `suppressed` fact；任一校准不可用时整体延后并保持 human queue |
| FindingDecision | 与 Finding 分离；MVP 只允许 sequence 1 的 `queued_for_human / agent_confirmed_requires_human_review`，evidence IDs 必须精确绑定 confirmed observation |
| Report | report ID、Candidate/Finding/Decision ID 和 summary 均从 canonical facts 重算；Report 中 Candidate projection 必须与独立 CandidateSet 逐项相同；JSON/Markdown content-addressed 保存，exact terminal query 可重建同一引用 |

正式终态不是旁路可变记录。成功执行把 exact `ReviewHypothesisSet`、
`CandidateVerificationLedger`、`GovernedCandidateSet`、`GovernedReviewReport`、
`FindingCalibrationLedger`、`FindingSuppressionLedger`、
`GovernedReviewMarkdown` 作为 formal report family 写入
`ReviewRun`；失败/取消保留 typed provider failure 且不得携带报告或 RunEvidence。Finalizer
只能从 dispatch intent、execution binding、terminal gate 与 admitted hypothesis evidence
投影 StageAttempt/PlatformExecutionBinding/RunEvidence，并追加确定性 run-ledger facts；最终仍由
Repository 对 frozen target、config/workflow、formal ledgers、canonical JSON/Markdown 和 run
ledger 做完整闭包校验。通用 history/show 因而消费同一 terminal authority。

analytics rebuild 不得把 formal report family 当作“没有 FindingSet”。它必须重新读取并校验
`ReviewHypothesisSet`、`CandidateVerificationLedger`、`GovernedCandidateSet`、
`GovernedReviewReport`、`FindingCalibrationLedger`、`FindingSuppressionLedger` 的 exact refs，把每个 Candidate
保留为 funnel fact，只将 confirmed Candidate 计为 normalized/verified Finding；`not_suppressed`
映射为 `human_queue + publication_not_reached`，`suppressed` 映射为同一 Candidate/Finding 上的
`suppressed + publication_not_reached`。Evaluation 的 suppression target 同时消费 verifier disposition
与 exact suppression ledger，不得把已确认但被策略过滤的 Candidate 误计为 escaped。不可变 projection 的
RunSourceBinding 必须同时保存 hypothesis/verification/candidate/report/calibration/suppression 六个 SHA-256；不得与 legacy
`FindingSet + detect stage` binding 混用。

deterministic `argus replay --change workflow` 接受 source-derived variant ConfigBundle 与独立
strict `WorkflowDefinition` 文件。两者的 workflow ID/revision/SHA-256 必须 exact match，且保持
同一 workflow ID、使用新 revision。允许的唯一变化是 deterministic scheduler 实际消费的逐 stage
budget，以及 retry `max_attempts/backoff_ms/retryable_codes`；stage graph/order、implementation、
contracts、executor、capability/authority、failure、side-effect、unknown-outcome 和 replay policy
全部冻结。host 从 exact workflow diff 推导排序后的 `changed_fields` 与最早受影响 stage，禁止复用
受影响 checkpoint。variant definition bytes、ReviewSpec ref 和 ConfigBundle ref 一并进入新
ExecutionSnapshot；terminal whole-closure 会重读 source/variant 两份 workflow artifact 并独立重算
差分。该能力不等于 formal Pi workflow variant，也不允许 workflow 改写 remote-write authority。

deterministic `argus replay --change index` 把
`ConfigBundle.execution.context_providers` 作为唯一原子变量；agent/model/credential、allowed tools、
provider concurrency 和所有非 execution 配置保持冻结。provider 的有序集合及其 ID/revision/kind/
adapter exact ref 可变，host 从两份 ConfigBundle 计算排序唯一的字段级 change set。该 replay 必须从
`materialize_target` 开始且不得复用 checkpoint；host 重新确认当前 repository 对应 source 的 exact
repository ID/head commit，从 immutable target manifest 恢复 target paths，并重新执行 variant provider。
新 ContextRef/Gap、execution receipt、ReviewInput 和包含 contexts 的 MaterializedTarget 都进入 variant
ExecutionSnapshot；source 中不受配置管理的 caller-supplied context 必须逐字节保留。terminal
whole-closure 会剥离两侧 configured contexts 后重验 target/input 其余字段相等、caller context 相等、
receipt 与 provider 定义相符。这是 local-host deterministic index experiment，不等于 formal Pi index
variant 或 Hailix attestation。

Verification ledger 与 CandidateSet 的 Go 类型、strict decoder、Schema 和规范样例分别位于

- [`candidate_verification_ledger.go`](../../pkg/contracts/v1alpha1/candidate_verification_ledger.go)
- [`candidate-verification-ledger.schema.json`](../../api/schema/v1alpha1/candidate-verification-ledger.schema.json)
- [`candidate-verification-ledger.json`](../../examples/candidate-verification-ledger.json)
- [`governed_candidate_set.go`](../../pkg/contracts/v1alpha1/governed_candidate_set.go)
- [`governed-candidate-set.schema.json`](../../api/schema/v1alpha1/governed-candidate-set.schema.json)
- [`governed-candidate-set.json`](../../examples/governed-candidate-set.json)
- [`finding_governance_ledgers.go`](../../pkg/contracts/v1alpha1/finding_governance_ledgers.go)
- [`finding-calibration-ledger.schema.json`](../../api/schema/v1alpha1/finding-calibration-ledger.schema.json)
- [`finding-suppression-ledger.schema.json`](../../api/schema/v1alpha1/finding-suppression-ledger.schema.json)
- [`config-revision.finding-governance.json`](../../examples/config-revision.finding-governance.json)

formal replay 的 source 必须是 committed succeeded formal `ReviewRun`。same-input exact 模式
复用 source target/ReviewInput/config/workflow/runtime evidence/tool policy，只更新 ReviewSpec
request identity，并持久化 `ReplayChangeSet(variable=none, changed_fields=[])`。`budget` variant
只允许同时改变 `budget.stage_timeout_ms` 与 `agent_review.budget.timeout_ms`，二者必须相等；新
ConfigBundle 由 exact baseline 派生，`ConfigResolutionReceipt(origin=replay_variant_derived)` 绑定
source receipt、baseline bundle、`variable=budget` 和固定 changed fields，不能伪装成新的
lifecycle publication。相同 source/idempotency key 请求不同 timeout 会在 dispatch/provider 前冲突。

`model` variant 只允许改变 `agent_review.model`。所选 model 必须由当前 Pi bootstrap 生成 exact
content-addressed component 并进入相同 subject-bound component registry；derived receipt 绑定 source
receipt/baseline/model changed field，ExecutionSnapshot build identity 必须随新 Pi manifest 改变，
runtime profile、runtime-file evidence、tool policy 与预算必须保持不变。model component publication
和 plan/capability preflight 都发生在 workload claim/provider 调用前；同一 replay key 改用另一模型
必须冲突。

`prompt` variant 只允许改变 `agent_review.prompt`。CLI 从 clean absolute path 读取 bounded、strict
`AgentReviewPromptBundle` JSON，按 bundle revision 发布 exact subject-bound component；derived receipt
固定 `changed_fields=["agent_review.prompt"]`。ExecutionSnapshot build identity 必须随 prompt component
改变，而 model、runtime-file evidence、tool policy、authority 与预算保持冻结；worker 再次校验
ref/artifact/bytes 并实际消费 variant prompt。相同 replay key 改用另一 prompt 必须在 provider 前冲突。

`skill_pack` variant 只允许改变 `agent_review.skill_packs` 中已有 review skill 的 exact Artifact ref。
CLI 以 Markdown 文件名 stem 匹配 baseline skill ID，未显式覆盖的 builtin skill 保持原 ref；派生
receipt 固定 `changed_fields=["agent_review.skill_packs"]`。完整 skill ID/phase/order、model、prompt、
runtime-file evidence、tool policy、authority 与预算都必须冻结，manifest build identity 必须随实际
skill bytes 改变。worker 消费 variant bytes，task receipt 按唯一 plan skill ID 解析回 exact variant
revision；同一 replay key 改用另一组 bytes 必须在 provider 前冲突。

`knowledge_pack` variant 只允许改变 `agent_review.knowledge_packs` 中已有 knowledge 的 exact
Artifact ref。完整 knowledge ID/order 必须与 baseline 一致且至少一个 ref 改变；derived receipt 固定
`changed_fields=["agent_review.knowledge_packs"]`，model、prompt、skills、runtime-file evidence、tool
policy、authority 与预算保持冻结，manifest build identity 随知识 bytes 改变。variant bytes 由 Pi
context/review/verifier 实际消费并在 snapshot 回显；exact retry 不重跑 provider，同一 replay key
换内容在 dispatch 前冲突。新增、删除或重排 knowledge 必须走 ConfigRevision lifecycle。

`rule_pack` variant 只允许改变 ConfigBundle 的 exact sealed `rule_pack`，derived receipt 固定
`changed_fields=["rule_pack"]`。AgentStagePlan 同时携带 RulePack 的语义 VersionedRef 与 canonical
base64 JSON bytes；Plan full digest 绑定 exact serialization，RulePack SHA-256 绑定将自身 `sha256`
清空后的语义内容。Go host 和 TS worker 均严格校验 schema、大小、UTF-8、identity 与语义 digest，
worker 将同一规则内容作为受治理但不可信的 reference data 注入 context/review/verifier。规则只能描述
缺陷判据；不能改变 model/tool authority、credentials、output contract、side effects 或 target scope。
该变量不改变 Pi runtime/component build identity，但会改变 ConfigBundle、Plan behavior/full digest；
exact retry 不重跑已提交结果，同一 replay key 换另一规则集在 provider 前冲突。

`workflow` variant 只允许替换 exact `formal-pi-agent-review` 单阶段 WorkflowDefinition 的新 revision。
graph、stage identity/kind/implementation、input/output contract、dependency、executor、capability、authority、
retry、failure、side effect 与 replay policy 必须逐字段不变；可变字段仅为
`timeout_ms`、`max_input_bytes`、`max_output_bytes`、`max_concurrency`。ConfigBundle 只替换 exact workflow
VersionedRef，derived receipt 与 ReplayChangeSet 记录由两侧定义重算出的排序 field paths。compiler 将
AgentReviewPolicy 与 workflow stage budget 逐项取最小，所得有效 timeout/target bytes/output bytes/concurrency
写入 sealed AgentStagePlan 并由 worker request 复核，因此 identity-only 或移动未生效上限会失败关闭。
snapshot 同时保存 variant definition bytes/ref；build identity、runtime evidence、ToolPolicy、target/input、
remote deny 与全部 provider components 保持冻结。exact retry 和从 workflow variant 发起的链式 replay 会
重验 exact definition/config/receipt/terminal closure，不会读取 lifecycle latest。

`index` variant 只允许改变 ordered `execution.context_providers`，agent/model/credential/tools、provider
concurrency、workflow、budget、runtime evidence 和 build identity 保持冻结。ReplayChangeSet 从
`materialize_target` 开始且不得复用 checkpoint。宿主从 source immutable manifest 恢复 exact target paths，
重验 repository ID/head commit 后执行 variant providers，冻结新的 ContextRef/Gap、execution receipt、
ReviewInput 和 MaterializedTarget；caller-supplied context 必须逐项保持。provider adapter descriptor 作为
exact subject-bound component 发布；它在 AgentStagePlan 中只表示宿主物化 provenance，Pi worker 只消费冻结
context Artifact。plan admission 在 provider 后、模型调用前重验 receipt 与 config/adapter/repository/commit/
binding/provenance，terminal gate 再独立证明 target/input 除 configured contexts 外未变化。exact retry 在
provider 执行前恢复并验证已有 snapshot，因此不会重复采集或调用 Pi。Evaluation 只有在 direct/chained atomic
index lineage 且 Target 除 contexts 完全相等时，才允许 variant 继续绑定原 Case input snapshot。

formal `filter_policy` variant 只允许改变 ConfigBundle 的 exact `finding_governance` section，且 source
必须已经通过正常 ConfigRevision lifecycle 启用该 section。derived receipt 和 ReplayChangeSet 以
`variable=filter_policy`、`changed_fields=["finding_governance"]` 绑定；Pi build、runtime evidence、tool
policy 和全部 provider 输入保持不变。执行从 `finding_governance` 后处理边界开始，复用 source exact
Hypothesis/Candidate/Verification/Report/Markdown refs，只派生新的 Calibration/Suppression ledger；不得
创建 workload、StageAttempt、binding、task/usage receipt 或 provider latency。旧
`variable=finding_governance` 仅为 immutable v1alpha1 历史事实读取、重建和 exact retry 保留。

计划阶段从源 admission 读取 exact ConfigBundle + ConfigResolutionReceipt，不读取 lifecycle latest；
当前 Node/dist/dependency manifest 必须与源 runtime evidence 相同。新 workload class 固定为
`eval_replay`，remote writes 继续 deny。`argus compare` 可比较 deterministic 或 formal report
family；formal Candidate 用 canonical cluster fingerprint 匹配，并保留 baseline/variant 两侧的
run-local Candidate ID。任何越界 checkpoint reuse、未声明配置/组件/ReviewSpec intent 漂移、
新增/删除 skill、budget/retry 之外的 formal workflow 变化均失败关闭。

### FindingLineage v1alpha1

`FindingLineage` 是跨 revision 的独立不可变事实，不写回 Candidate、Finding、Decision 或
`RunComparison`。它绑定两侧 committed formal ReviewRun 及 ReviewSpec、TargetSnapshot、CandidateSet、
Verification/Calibration/Suppression ledger、GovernedReport 的 exact SHA-256，并要求同
tenant/workspace/repository、同 target mode、不同 target digest。

- `continued`：canonical fingerprint 一对一、剩余 stable family 一对一，或 exact Git rename mapping
  转换 path 后的一对一；
- `split`：一个 baseline Finding 对同一 stable family 的多个 variant Finding；
- `merged`：多个 baseline Finding 对同一 stable family 的一个 variant Finding；
- `introduced/resolved`：无法保守闭合的单侧 Finding；
- stable family 仅由 exact dimension version、category、path、规范化 title 构成；`git_rename_family` 还要求
  baseline path 到 variant path 的显式 Git rename mapping；不使用 embedding、LLM
  判断或模糊阈值；多对多歧义必须留下 `ambiguous_many_to_many_family`，不得伪造关系。

relation 必须对两侧每个 Finding 恰好覆盖一次，ID 与聚合 ID 从 canonical facts 派生。build ledger
append-only 保存 request/ref/time，默认 idempotency key 由 baseline、variant 和 policy digest 派生；同键异值
或同 pair/policy 的第二个 key 冲突。CLI 为 `argus lineage build|show|list`；authenticated local API 为
`GET|POST /v1/finding-lineages` 与 `GET /v1/finding-lineages/{id}`。local CLI/API 的新 build 固定使用
`ancestry_authority=local_git_object_graph` 和 5000 BPS rename threshold：两侧 repository path 必须属于
同一 canonical Git common-dir，run head 必须是 exact commit OID，隔离 object view 必须证明 baseline 是
variant 的严格祖先且 merge-base 等于 baseline。ancestry evidence digest 同时绑定 object format、两端 OID、
merge-base 和排序后的 rename mappings。`caller_order_unverified` 仅用于严格读取既有 v1alpha1 artifact，
配置了 Git provider 的 repository 不允许用它创建新 lineage。

Go 类型、strict decoder/semantic validator、Schema 和规范样例位于
[`finding_lineage.go`](../../pkg/contracts/v1alpha1/finding_lineage.go)、
[`finding-lineage.schema.json`](../../api/schema/v1alpha1/finding-lineage.schema.json) 与
[`finding-lineage.json`](../../examples/finding-lineage.json)。

formal ExecutionSnapshot 还必须冻结恰好一个
`argus.local_runtime_file_manifest.v1alpha1` local artifact。其 canonical bytes 与 admitted
AgentStagePlan 的 governed runtime component digest 完全一致，记录 Node executable、全部
`dist/*.js`、package metadata/lockfile 和 production dependency 实体文件的相对路径、字节数与
SHA-256。local Pi adapter 在 dispatch 和 process start 两次重新计算；缺失、symlink、越界、
超限或 digest 漂移都失败关闭且不得调用 provider。manifest authority 固定为
`local_host_observation`，不能当作 Hailix platform attestation，也不能替代 shadow/formal path
中的 worker-self-reported task evidence。Go 类型、strict decoder、Schema 和规范样例位于
[`local_runtime_file_manifest.go`](../../pkg/contracts/v1alpha1/local_runtime_file_manifest.go)、
[`local-runtime-file-manifest.schema.json`](../../api/schema/v1alpha1/local-runtime-file-manifest.schema.json)
与 [`local-runtime-file-manifest.json`](../../examples/local-runtime-file-manifest.json)。

该契约不包含 publication、Feedback、Outcome、evaluation label 或 Apply 授权。Go 类型、严格
decoder/validator、Schema 和规范样例位于
[`pkg/contracts/v1alpha1`](../../pkg/contracts/v1alpha1)、
[`api/schema/v1alpha1/governed-review-report.schema.json`](../../api/schema/v1alpha1/governed-review-report.schema.json)
与 [`examples/governed-review-report.json`](../../examples/governed-review-report.json)。

## 7. ReplaySpec

ReplaySpec 必填：

- source execution snapshot；
- start/end stage；
- reused artifact refs；
- override dimensions；
- output namespace；
- evaluator/baseline；
- `remote_side_effects = deny`。

若没有 override，称为 same-input replay；不称为 bitwise deterministic replay。
所有 ReplayRun 生成新 ID、新 Trace 和新 Artifact，不覆盖原运行。

## 8. Analytics Fact

事实事件只含计算指标所需的去敏字段：

- stable identity/dimensions；
- schema/policy/model/workflow revision；
- timestamps、duration、cost、token/tool counts；
- funnel transition、reason code、label/outcome state；
- source event id、watermark 和 idempotency key。

不得包含源码、完整 prompt、credential、完整 tool output 或用户私有文本。

## 9. 版本策略

- `v1alpha1` 可不兼容修改，但必须同步类型、schema、样例、测试和文档。
- `v1beta1` 开始提供 migration note 和双读窗口。
- `v1` 后字段语义不可重用；删除使用新 version。
- schema version 与 workflow/config/model revision 是不同维度，必须分别记录。
