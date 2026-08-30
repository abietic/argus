# Contributing to Argus

Argus 把代码评审结论视为需要证据验证的假设。贡献应优先提高真实缺陷发现质量、
证据完整性和可恢复性，而不是增加无法验证的评论数量。

## 开发流程

1. 从最新 `main` 创建 `codex/<topic>` 或其他语义清晰的 topic branch。
2. 在修改前确认需求 owner 和事实 owner；不要把愿景文档当成已实现能力。
3. 新增功能时同时覆盖成功、边界和失败路径。并发、权限、持久化、重放、幂等、
   恢复或外部写入变化必须有专门的拒绝/恢复测试。
4. 本地运行：

   ```bash
   make fmt
   make verify
   ```

5. 通过 Pull Request 合入 `main`。PR 必须说明用户问题、契约影响、验证证据、风险和
   回滚方式；不得绕过必需的远端检查。

## 契约变更

`v1alpha1` 阶段允许不兼容调整，但必须在同一 PR 中更新：

- Go 类型、严格 decoder 和 `Validate`；
- `api/schema/` 下的 JSON Schema；
- `examples/` 中的正向样例与拒绝测试；
- `docs/contracts/core-protocols.md`；
- 真实发生变化的 `docs/roadmap/STATUS.md`、`PLAN.md` 或 `REMAINING.md` owner。

## 安全与数据

- 不提交凭据、真实生产标识、个人信息、内部链接或未经授权的数据。
- 远程写入默认关闭；所有外部写入必须具有幂等键、unknown-outcome 语义和 exact
  target revalidation。
- Replay 默认拒绝远程副作用。
- Argus 自己生成的 Finding 不得自动成为 gold label 或训练数据。

安全漏洞请按 [SECURITY.md](SECURITY.md) 私下报告，不要在公开 Issue 中披露细节。
