# ADR 0003: Snapshot First And Append-only Decision Ledger

**Status:** accepted
**Date:** 2026-07-26

## Context

如果运行时读取 latest 配置、移动分支或可变工具索引，历史结果无法解释；如果过滤
直接删除 candidate，也无法区分 detector、verifier 和 publisher 的真实效果。

## Decision

每个 ReviewRun 在 dispatch 前冻结 ExecutionSnapshot。dispatch 后追加
PlatformExecutionBinding，完成后追加 RunEvidence；Stage input/output、Finding 和
Decision 都保留不可变 lineage。Replay 创建子运行和新 namespace，声明变更维度，
不覆盖历史。

Candidate、Finding、Decision、Publication、Feedback 和 Outcome 分开建模。过滤、
去重、限额和 fallback 都写 append-only decision/reason。

## Consequences

- 存储量增加，但可以按 Artifact retention 分层。
- Dashboard 可正确计算 Generation、Verification、Exposure 各阶段效果。
- schema、config、workflow、model、tool/index 和 runtime version 成为强制输入。
