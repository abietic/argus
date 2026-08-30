# ADR 0004: Repository Agent Configuration Is Untrusted Input

**Status:** accepted
**Date:** 2026-07-26

## Context

当前 Eino-Agent 的 ACP session 会加载目标仓库内 hooks、MCP、skills/plugins 和
settings；部分 hook/MCP process 可在首次 prompt 或 permission 之前运行。工具
allowlist 不会自动移除 MCP，文件工具也不构成强 workspace sandbox。

## Decision

任意被评审仓库中的 Agent 配置都按不可信源码处理，不能成为 runtime 配置。
Eino-Agent 接入前必须同时具备：

- review-safe composition mode；
- 外部无密钥、禁网、资源受限隔离；
- 源码只读和 hard path containment；
- platform-owned clean config/home；
- mutation/background/subagent 默认禁用；
- capability snapshot 和负向 contract test。

## Consequences

- M1 使用 deterministic fake，不直接对任意 checkout 启动 Eino-Agent。
- 若需要评审 `.claude`/MCP/skill 文件，通过普通 Artifact 暴露 bytes，不执行它们。
- Apply 使用独立授权 WorkingCopy 和 runtime，不能复用 review session。
