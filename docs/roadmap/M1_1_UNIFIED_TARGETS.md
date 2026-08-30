# M1.1 Unified Local Targets

**Last verified:** 2026-07-27

M1.1 将 `diff`、`selection` 和 `scope` 三种入口接入同一条本地
review/replay/compare/history 链路。它完成的是 exact target 与证据闭包，不是可恢复
分布式 full scan，也不代表真实 AI 评审质量。

## 目标语义

- `diff`：先把 base/head 解析为 exact commit OID，再冻结 canonical patch、变更
  manifest 和 exact-head 文件内容。
- `selection`：冻结 exact revision、单文件行范围和完整文件内容；可选 overlay 会按
  bytes 冻结，不读取后续工作树。评审只遍历授权行，不能把同文件其他行变成 Finding。
- `scope`：从 exact commit 的 Git tree 流式枚举 include/exclude；`**` 可跨目录，
  exclude 优先。工作树文件、hook、attributes 和 Agent 配置不参与目标解析。
- 三种模式都在 dispatch 前保存 mode-aware TargetSnapshot、ReviewSpec、
  ReviewInput 和 ExecutionSnapshot。selection/scope 不合成伪 Git diff。
- symbolic revision 在读取完成后重新解析；移动或消失会成为 explicit partial reason。
  scope 的文件上限、binary、symlink、submodule、非文本和超限文件进入 coverage/
  skipped reason，不被静默省略。

## CLI

Diff：

```bash
./build/argus review \
  --repo /absolute/path/to/repository \
  --mode diff \
  --base HEAD^ \
  --head HEAD \
  --store /absolute/path/to/argus-store
```

圈选 exact commit 中的行：

```bash
./build/argus review \
  --repo /absolute/path/to/repository \
  --mode selection \
  --revision HEAD \
  --path internal/review/service.go \
  --start-line 40 \
  --end-line 72 \
  --store /absolute/path/to/argus-store
```

使用未保存 overlay 时，`--overlay-file` 必须是 clean absolute regular-file path；
overlay bytes 会进入冻结 artifact：

```bash
./build/argus review \
  --repo /absolute/path/to/repository \
  --mode selection \
  --revision HEAD \
  --path internal/review/service.go \
  --start-line 40 \
  --end-line 72 \
  --overlay-file /absolute/path/to/overlay.go \
  --store /absolute/path/to/argus-store
```

仓库或目录 scope：

```bash
./build/argus review \
  --repo /absolute/path/to/repository \
  --mode scope \
  --revision HEAD \
  --include 'internal/**' \
  --include 'pkg/**' \
  --exclude '**/*_test.go' \
  --store /absolute/path/to/argus-store
```

以上 run 均可使用原有 `replay`、`compare`、`history` 和 `show`。compare 仍要求两次
run 共享 exact materialized target 与 ReviewInput。

## 已验证边界

- exact commit 内容不受 capture 后工作树变更影响；
- selection 只产生授权行中的候选，overlay 与 commit 内容可明确区分；
- scope include/exclude、完整 tree 计数和 retained/skipped coverage 守恒；
- 三模式都能通过 final run artifact/ledger 闭包校验并从 checkpoint replay；
- no-match scope 仍保存显式 coverage 并产出零 Finding 报告；
- remote write、workspace write、network 和 delegation 继续保持 deny。

未完成项以 [MVP 验收矩阵](MVP_ACCEPTANCE_MATRIX.md)为准。symbol/multi-range selection
已在后续本地实现；精确版本的配置驱动 Go AST Context Provider、ContextRef/ContextGap
以及本地执行回执也已闭环。仍需完成 CodeGraph/LSP/依赖图等上下文源、可恢复
shard/checkpoint、正式配置管理面、真实 detector 的平台接线和 Hailix/Eino-Agent
production adapter。
