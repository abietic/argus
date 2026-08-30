# Metrics, Evaluation And Value

## 1. 指标树

```text
Net Review Value
├── Defect value
│   ├── high-severity recall
│   ├── published precision
│   ├── verified fixes
│   └── escaped/recurrent defects
├── Developer cost
│   ├── false-positive interruption
│   ├── review waiting time
│   └── manual verification effort
├── Platform cost
│   ├── model/tool/compute cost
│   ├── latency and failure
│   └── operations/data retention
└── Learning velocity
    ├── label coverage and delay
    ├── replay throughput
    └── experiment-to-promotion lead time
```

每个 dashboard tile 必须显示口径、时间窗、版本和样本量。

## 2. Finding 漏斗

标准漏斗：

```text
candidate
  -> normalized
  -> verified / rejected / inconclusive
  -> published / suppressed / human_queue

Feedback projection: published -> accepted / dismissed / wont_fix / outdated
Outcome projection:  published/accepted -> fixed / recurred / unknown
```

同时展示 count 和 rate。评论限额可能提高发布 precision，但不能据此宣称 detector
precision 提高；必须分别看 candidate、verified 和 published 三层。Feedback 与
Outcome 是独立 ledger，漏斗只是只读 projection，不能混入 FindingDecision。

## 3. 离线评测

### 3.1 Case 类型

- `positive_localized`：已知缺陷及可接受 anchor；
- `negative_clean`：已审查的无缺陷区域；
- `false_positive_regression`：历史误报；
- `missed_defect_regression`：历史漏报；
- `fix_validation`：Finding 对应修复是否正确；
- `mutation_diagnostic`：变异体，只测特定能力；
- `workflow_invariant`：超时、partial、重复、stale、权限等工程行为。

### 3.2 核心指标

- detection recall / precision；
- localization exact/overlap；
- severity/category accuracy；
- evidence support / contradiction；
- semantic dedup precision；
- published precision；
- fix compile/test/apply success；
- cost and latency per correctly found defect；
- run-to-run instability。

当前本地 evaluator 已实现 detection/category presence、localization overlap，以及受结构化真值约束的
filter efficacy 整数事实：
LabelAnchor 冻结 `path/side/start_line/end_line/source_digest`，Finding 必须 source digest 相同且区间重叠
才命中；同时记录 expected/matched anchor 和 localized finding 数。false-positive regression 另冻结
canonical candidate cluster fingerprint，只有 target 在完整 GovernedReport 中出现后，Candidate 的
rejected/confirmed/inconclusive disposition 才计入 suppressed/escaped/inconclusive；target 不出现不能
算过滤成功。Experiment 保存 paired suppressed/escaped delta。exact-match、跨 revision remap、Apply、
成本仍需独立权威事实，partial report 不进入定位或过滤统计。

同配置重复性由独立 RepeatabilityRun 记录，不从 Experiment 的单变量 before/after 推断。它只比较同一
非 replay baseline 的 direct `variable=none` exact replays，按完整 GovernedReport 的稳定 FindingID 集合
计算 unordered-pair Jaccard、exact-set rate、presence rate，并单独记录 expected-anchor hit 与 verdict
flip（PPM scale=1,000,000）。任一 partial report 会让 Finding-set stability unavailable；该状态不是
“稳定无缺陷”。这些数回答“相同冻结任务是否重复得到相同结果”，不能作为 precision/recall 的替代。

fix validation 已使用独立 ApplyTrial：exact Finding suggestion 对应的 edit script 必须有 dry-run、compile、
test 的 content-addressed evidence；任一失败可判 invalid，全部成功必须三项齐全，partial 不参与有效性
比较。Experiment 输出 paired passed/failed check delta。当前证据 authority 是
`local_host_unattested`，所以可用于本地回归和调参，不可冒充生产沙箱/Hailix attestation。

formal Pi token 使用量来自 committed `ReviewRun.agent_execution_receipt_ref`：EvaluationRun 严格读取
receipt collection 并汇总 provider-reported/partial/unavailable 数及 input/output/cache/reasoning/total
counter；只有 baseline/variant 两侧全部为 provider-reported 时才输出 paired token delta。authority 是
`worker_self_report_diagnostic`，可用于本地相对比较，不是 provider attestation 或 billing truth。
PricingCeiling 只做调用前上限准入，实际 cost 在权威账单接口接入前固定 unavailable。

六维 Skill 的评测采用 evaluator 显式 applicability slice，而不是按 label category 自动映射：每个 case
只评价请求中 exact `id/revision/sha256` 的 dimension。review receipt 直接归因，verifier receipt 通过
hypothesis occurrence 回连，shared context 不进行任意成本分摊。维度输出 Candidate disposition、
Finding/localization、执行覆盖、turn/tool、累计 task duration 和 attributable token；没有成功 review
receipt 时 clean/miss verdict 均为 inconclusive。Experiment 只按相同 dimension ID 配对，同时保留两侧
exact revision，Dashboard 使用受控 `review_dimension` 生成独立 token/duration facts。新 formal Pi run
已把 exact raw candidate collection 纳入 terminal ReviewRun closure，EvaluationRun 可按维度复算
retained/merged-duplicate/rejected-invalid/excluded-budget 整数事实，Experiment 仅在两侧 closure 都可用时
输出 paired delta；旧 run 继续显式 unavailable。context gap 同样从 committed coverage + task/group receipt
做逐维归因，不解析 prompt 或平均分摊 shared context。Dashboard 已发布逐维 duplicate/invalid/budget/context
整数 facts，并以逐维 `sum(fate)/sum(raw candidates)`、scale=6 发布 duplicate/invalid/budget ratio。
任一 normalization closure 不可用或某 arm 的 raw 分母为零时 rate 保持 partial；不得用 governed
Candidate 数替代 raw 分母，也不得把单个 rate 当成独立质量结论。

semantic dedup 的质量使用独立 adjudicated raw-claim equivalence partition，而不是“合并后数量更少”。对每个
eligible raw unordered pair，oracle/prediction 都同类计 TP，只有 prediction 同类计 FP，只有 oracle 同类计
FN，两侧都不同计 TN。聚合指标使用 `sum(TP)/sum(TP+FP)` precision、`sum(TP)/sum(TP+FN)` recall 和
`sum(FP)/sum(TP+FP)` false-merge rate，PPM scale=1,000,000；无 predicted duplicate 或无 oracle duplicate
时对应指标 unavailable。另展示 oracle/predicted unique-claim delta、exact partition match、样本量和
policy-exposed case 数。policy exposure 不删除事实，但从该 revision 的 independent evidence 分母排除。
质量聚合还必须展示并保留每个 case 的 exact oracle registry binding；未注册、已撤销、被新 CAS revision
替代、trust key 已撤销或 case label/governance 漂移的 oracle 不进入新质量运行，也不能通过 `show`
伪装成当前有效证据。历史 artifact 仍可审计，但“曾经生成”与“当前可用于晋级”是两个状态。

正式 Dashboard funnel 同时接受 deterministic FindingSet 与 formal governed report family。formal
路径按 committed `ReviewHypothesisSet + GovernedCandidateSet + GovernedReviewReport` 复算：所有
Candidate 都进入候选数，只有 confirmed Candidate 进入 Finding/verified，初始
`queued_for_human` 只能显示为 human queue，不能计为 publish eligible 或 published。三份 artifact digest
保存在 projection source binding 中；无 Candidate 的完整 formal run 也必须显式给出零值 funnel summary。

Judge 评价必须保存 rubric、model/profile、prompt digest 和 calibration。Judge 无法
替代 compiler/test 或人工 gold label。

## 4. 在线观测

- 启用仓库、活跃开发者、触发/完成/partial/失败；
- no-finding、no-published、timeout、stale、publication unknown；
- feedback coverage、feedback lag、unknown outcome；
- accept/dismiss/fix/reopen；
- 每语言/规则/路径/版本的漂移；
- token、tool、compute、storage、p50/p95 latency。

Context Provider 另外展示 attempt/succeeded/gap/reuse、success rate、typed gap reason 和
duration p50/p95。Replay 对冻结 receipt 的复用只进入 reuse，不进入 attempt、success rate
或 latency；本地 receipt 指标必须带 `local_host_observation_not_platform_attestation`，不能
替代 Hailix 运行证明。

避免把未反馈当“正确”，也避免把没有评论当“没有缺陷”。

## 5. 收益归因

正式收益使用 `ValueObservation`：

```text
identity + metric
baseline cohort/revision
observation window
attribution window
evidence tier
value and uncertainty
source refs
policy revision
```

推荐优先级：

1. verified fix / escaped defect 对照；
2. review lead-time 对照；
3. 人工抽样核验的工作量变化；
4. 行为信号；
5. 模型估算只作探索。

ROI 至少扣除模型/算力、人工复核、误报打断、平台运维和存储成本。

## 6. 数据集治理

- 每个 case 有 provenance、consent/license、classification、owner、review state；
- label correction append-only，保留旧版本和影响的历史 experiment；
- semantic clone group 防止同一 bug 的变体跨 split；
- holdout 只由受限角色维护；
- production feedback 默认进入 candidate pool；
- 删除请求传播到 Artifact、export 和训练 eligibility，并留下 tombstone。

## 7. Promotion

一个 variant 只有同时满足以下条件才可晋级：

- 关键 regression case 不退化；
- holdout 的高严重度 recall 不下降到门限外；
- published precision、成本和 p95 时延满足联合门限；
- failure/partial/instability 不恶化；
- shadow/canary 无新的安全或权限事件；
- 有明确 rollback revision。

门限数字在获得基线后写入 versioned policy，不在 M0 凭空设定。

激活后由 `CalibrationPromotionObservation` 比较两个显式 immutable Dashboard snapshot。baseline window
必须在 activation 前结束，observation window 必须在 activation 后开始；同一 snapshot 中只选择 exact
baseline/variant ConfigBundle SHA 的 ReviewRun，避免 canary 混合 cohort。当前受治理指标固定为：

- ReviewRun success rate 与 complete-result rate；
- published Finding 的 known Outcome coverage；
- known published Outcome 中的 fixed rate；
- known published Outcome 中的 recurred/escaped adverse rate。

全部比例使用整数 PPM 和原始 numerator/denominator。partial snapshot 是 `unavailable`，样本未达到每指标
policy 分母是 `insufficient_data`，二者都不能被当作 0 或 regression。只有超过 policy regression
容忍度的已评价指标才生成回滚建议；系统不自动回滚。Feedback 接受、评论点击、评论数量、模型 score
仍只属于行为/诊断信号，不能单独驱动该建议。该机制证明本地监控契约与失败安全，不证明示例门限有生产
统计意义，也不替代 E2/E3 ValueObservation 或真实长期 corpus。
