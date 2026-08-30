# M1 Local Vertical Slice

本文说明 Argus M1 本地纵向链路的能力边界、操作方式和完成标准。它是本地平台
语义基线，不是生产 AI Code Review 的质量声明。

## 能解决什么问题

M1 将一次本地 Git commit-to-commit diff 冻结为 exact target，然后让同一套五阶段
实现完成评审、checkpoint replay、结果比较和历史查询。每个阶段的输入、输出、
binding 和 evidence 都能回到 content-addressed artifact 与 append-only run ledger，
因此“跑过什么、复用了什么、为什么发布或转人工”不依赖聊天文本或可变 working
tree。

当前只支持 diff target。ReviewSpec 中的 selection/scope，以及仓库/目录 full
scan，尚未接入 materialization。

## 当前执行路径

```text
exact base/head commit OID
  -> canonical patch + change/coverage manifest
  -> exact-head regular text file content
  -> ReviewSpec + immutable ExecutionSnapshot
  -> detect -> normalize -> verify -> adjudicate -> report
  -> StageAttempt + PlatformExecutionBinding + RunEvidence
  -> content-addressed final ReviewRun + terminal ledger event
```

detector 是 deterministic marker fixture：只在新增 Go 代码注释中查找 `TODO`、
`FIXME` 和 `ARGUS_BUG`。它的用途是证明 candidate/finding/decision、verification、
partial context、重放和比较语义，而不是模拟或替代真实 AI detector。

## Quickstart

先运行工程门禁并构建 CLI：

```bash
make verify
make build
./build/argus version
```

选择另一个至少包含两个 commit 的本地 Git 仓库。store 必须位于被评审仓库之外；
以下 `mktemp` 默认满足这个约束。脚本使用 `jq` 从 JSON 输出中提取 run ID：

```bash
ARGUS_REPO="/absolute/path/to/repository"
ARGUS_STORE="$(mktemp -d)"
ARGUS_BASE="$(git -C "$ARGUS_REPO" rev-parse HEAD^)"
ARGUS_HEAD="$(git -C "$ARGUS_REPO" rev-parse HEAD)"

ARGUS_REVIEW_JSON="$(
  ./build/argus review \
    --repo "$ARGUS_REPO" \
    --base "$ARGUS_BASE" \
    --head "$ARGUS_HEAD" \
    --store "$ARGUS_STORE" \
    --json
)"
ARGUS_REVIEW_RUN="$(
  printf '%s' "$ARGUS_REVIEW_JSON" | jq -r '.run.run_id'
)"
printf '%s' "$ARGUS_REVIEW_JSON" |
  jq '{run_id: .run.run_id, status: .run.status, summary: .report.summary}'
```

从 `verify` 开始重放。`detect`、`normalize` 的 exact checkpoint 会被复用：

```bash
ARGUS_REPLAY_JSON="$(
  ./build/argus replay \
    --run "$ARGUS_REVIEW_RUN" \
    --from verify \
    --store "$ARGUS_STORE" \
    --json
)"
ARGUS_REPLAY_RUN="$(
  printf '%s' "$ARGUS_REPLAY_JSON" | jq -r '.run.run_id'
)"
printf '%s' "$ARGUS_REPLAY_JSON" |
  jq '{run_id: .run.run_id, status: .run.status, reused_stages}'
```

比较、历史和单次 run 查询都读取同一 store：

```bash
./build/argus compare \
  --baseline "$ARGUS_REVIEW_RUN" \
  --variant "$ARGUS_REPLAY_RUN" \
  --store "$ARGUS_STORE"

./build/argus history --limit 10 --store "$ARGUS_STORE"
./build/argus show --run "$ARGUS_REVIEW_RUN" --store "$ARGUS_STORE"
```

省略 `--json` 时输出适合人读的 Markdown；`review/replay/show` 的 JSON 顶层包含
`run`，其中 run ID 位于 `.run.run_id`。`replay --from` 支持 `detect`、
`normalize`、`verify`、`adjudicate` 和 `report`。默认 store 位于操作系统用户配置
目录下的 `argus/local-store`，但验收和自动化建议显式传 `--store`。

2026-07-26 已按上述路径完成两次独立 smoke：一个 exact 4-file diff 验证零
Finding 路径；另一个 exact Go diff 产生 1 条 verified/publish Finding。两次
review 与从 `verify` 开始的 replay 均成功，replay 复用两个 checkpoint；
compare、history 和 show 均可从同一临时 store 读取。Finding smoke 的 compare
结果为 added 0、removed 0、unchanged 1。

## 验收清单

- [x] review 输出 resolved base/head commit OID，而不是只保存可移动 revision。
- [x] canonical patch、manifest、exact-head file content、ReviewInput 和
  ExecutionSnapshot 都能按 artifact digest 读取并通过闭包校验。
- [x] working tree 在 capture 后发生变化，不会改变已冻结的 ReviewInput。
- [x] 完整 run 有五个成功 stage checkpoint、五个独立 binding 和五个 evidence；
  retry 会追加 attempt/binding，不覆盖旧事实。
- [x] 从 `verify` replay 复用 exact `detect`、`normalize` artifact，只重新执行
  `verify -> adjudicate -> report`。
- [x] compare 只接受 exact materialized target 与 ReviewInput 一致的两个成功 run，
  并按稳定 fingerprint 输出 added/removed/unchanged。
- [x] patch/file 限额、unsafe path、binary/symlink/submodule 和缺失 context 不被
  静默忽略；partial evidence 导致 inconclusive/human-review。
- [x] cancellation 停止后续 stage 调度并提交 canceled terminal run；失败与取消
  不生成成功 report。
- [x] local execution 的 network、workspace write、remote write 和 delegation
  均为 deny。
- [x] `make verify` 在最终 CLI 接线后通过。

全部检查由 focused/race/shuffle Go tests、2026-07-26 CLI smoke 和最终
`make verify` 共同验证。这里的“可用”仅指本地 deterministic 纵向基线，不是生产
release 或真实 AI 评审质量声明。

## 非目标与下一步

M1 不包含真实 AI detector/verifier/adjudicator，不包含 selection/scope/full-scan，
也不包含 Hailix/Eino-Agent production adapter、代码平台写回、数据库、Web、
dashboard、evaluation dataset/experiment/promotion 或收益归因。

下一阶段先冻结 typed `StageExecutionRequest/Result` 和 review-safe runtime authority，
再把当前 local `PlatformExecutionBinding` 替换为可由 Hailix attested 的 immutable
platform binding；Argus 仍保留 ReviewRun、Finding、Decision、Evaluation 和 Value
领域事实。
