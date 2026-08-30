# ADR 0002: Hailix Is The Execution And Evidence Platform

**Status:** accepted
**Date:** 2026-07-26

## Context

Argus、Hailix 和 Eino-Agent 需要形成自举闭环。重复实现 Worker Runtime、ACP、
Trace、Artifact 和模型代理会制造三个不一致的平台。

## Decision

Hailix 长期拥有通用执行与证据平台；Argus 拥有 Code Review 领域控制面；
Eino-Agent 是 ACP worker runtime。

跨仓只使用版本化 API、事件和 Artifact，不共享数据库，不 import `internal` Go
package。Argus 永久拥有 Code Review ExecutionSnapshot、ReplayRun、
ExperimentRun 和 evaluation/analytics；Hailix 只有在出现第二个真实消费场景后，
才提取 platform execution input/ref、隔离 namespace 和 side-effect enforcement。

## Consequences

- Argus 不自行构造 Hailix 内部 TaskSpec，而通过其受支持的 admission API。
- Hailix Trace 是执行证据，Argus Finding/Metric 是领域事实。
- 当前 Hailix 的 single-user/Codex-only/typed-artifact 缺口必须显式解决，不能用
  文档愿景替代实现。
