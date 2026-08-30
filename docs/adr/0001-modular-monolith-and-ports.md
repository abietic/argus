# ADR 0001: Modular Monolith And Ports

**Status:** accepted
**Date:** 2026-07-26

## Context

旧系统能力跨事件入口、Agent、推理、沙箱和评论后处理服务分散。Argus 从空仓开始，
若直接恢复同等微服务数量，会先承担部署和一致性成本，却没有稳定领域边界。

## Decision

Argus 先采用按 bounded context 分区的模块化单体。Domain/Application 通过端口依赖
Hailix、Agent、数据库、代码平台和 evidence provider。只在安全、故障隔离、独立
扩缩容或组织 ownership 被实际证明后拆服务。

## Consequences

- production、evaluation 和 replay 可以共用同一 workflow implementation。
- 模块边界必须由 package dependency、contract 和测试维持，不能把所有逻辑放入
  shared package。
- durable worker 可以独立部署，但不拥有 Review/Finding 领域事实。
