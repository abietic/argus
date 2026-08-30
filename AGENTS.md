# AGENTS.md

本文给参与 Argus 的 Coding Agent 使用。若本文与源码、测试、运行配置或用户当前
要求冲突，以后者为准。

## 默认协作

- 默认用中文回复，先给结论和可执行结果，再解释关键取舍与风险。
- 主动读取当前源码、测试、配置和相邻项目真实状态；不要把愿景文档当已实现能力。
- 不回滚、覆盖或吸收用户已有未提交改动。
- 架构、协议、权限、数据兼容、评测真值和不可逆迁移由主 Agent 决策；支持性调研
  可以并行，但必须由主 Agent 集成和验收。
- `argus` 出现 `.codegraph/` 后，理解或修改代码前优先使用 CodeGraph；不要自行
  初始化索引。

## 项目定位

Argus 是 Code Review 垂直领域产品：

- Argus 拥有 ReviewSpec、TargetSnapshot、ReviewWorkflow、Finding、Decision、
  Feedback、Evaluation 和 Value 语义。
- Hailix 拥有通用身份/Workspace、Task/Worker Runtime、ACP、Trace/Artifact、
  运行时事件与模型执行证据；Argus 的 Evaluation/Experiment 不预先下沉。
- Eino-Agent 是 ACP 可控执行 Runtime，不拥有 Argus 业务状态。

不要在 Argus 复制 Hailix Worker Runtime，也不要通过读取 Eino-Agent 私有存储来
完成集成。

## 第一性原则

1. 找到真实缺陷比生成更多评论重要。
2. Candidate、Finding、Decision、Comment 和 Outcome 是不同事实，不能合并成一张
   可变结果表。
3. 没有不可变 ExecutionSnapshot、执行绑定和结果证据，就不能声称可重放、
   可比较或可回流。
4. 过滤、限额和去重必须留下决策证据，不能物理丢弃候选。
5. LLM 结论是待验证假设；编译、测试、AST/LSP、CodeGraph 和仓库证据具有独立
   provenance。
6. 配置必须版本化、可解释、可灰度、可回滚；运行时不读取漂移的 latest。
7. 线上反馈先进入候选池，经过治理后才能成为评测标签或训练数据。
8. 评论数、接受点击和模型分数不能单独证明收益。

## 事实与文档 owner

| 文件 | Owner 内容 |
|---|---|
| `PROJECT_DIRECTION.md` | 产品目标、原则、reference 采纳和长期方向 |
| `REQUIREMENTS.md` | 功能/非功能需求、成功标准、假设和开放问题 |
| `TECHNICAL_DESIGN.md` | 当前架构基线和关键运行语义 |
| `docs/contracts/core-protocols.md` | 对外/跨 Context 契约 |
| `docs/roadmap/STATUS.md` | 已验证的当前事实 |
| `docs/roadmap/PLAN.md` | 已接受的执行顺序 |
| `docs/roadmap/REMAINING.md` | 缺口清单，不代表已接受 backlog |

不要为了让每层文档“都有变化”而复制同一事实。

## Reference 决策

使用旧 Smart CR、Hailix、Eino-Agent 或其他实现时，必须标记：
`preserve`、`adapt`、`combine`、`project-native`、`reject` 或 `defer`。

- 先写用户问题和 Argus 自有可观察契约，再比较 reference。
- 必须验证 production wiring、入口、测试、失败和恢复，注册表或文档存在不等于
  可用。
- 不把旧系统内部链接、凭据、生产标识、个人信息或未经授权的专有数据写入仓库。
- 跨仓修改不是 Argus 任务的隐含授权；先在 Argus 记录 integration gap，用户明确
  要求后再改 Hailix/Eino-Agent。

## 领域与依赖规则

长期代码按 `domain -> application ports <- adapter` 组织：

```text
interfaces/adapter -> application -> domain
infrastructure/adapter -> application/domain ports
domain 不依赖数据库、HTTP、ACP、模型或代码平台 SDK
```

- `pkg/contracts` 只放稳定的跨进程/跨 Context 数据契约。
- 不 import 相邻仓库的 `internal` package。
- 所有外部写入需要幂等键、unknown-outcome 语义和 exact target revalidation。
- 所有 replay 默认 deny remote side effects。
- 时间、ID、随机数和外部能力通过端口注入，避免不可重放的隐式全局状态。

## 契约变更

当前 `v1alpha1` 允许不兼容调整，但每次调整必须：

1. 更新 Go 类型、严格 decoder 和 Validate；
2. 更新 JSON Schema 与 `examples/`；
3. 增加正向、边界和拒绝测试；
4. 更新 `docs/contracts/core-protocols.md`；
5. 在 `STATUS/PLAN/REMAINING` 中只更新真实变化的 owner。

进入 `v1` 后只允许向后兼容扩展或新 schema version。

## 工程门禁

代码或契约变更后运行：

```bash
make fmt
make verify
```

`make verify` 必须覆盖格式、vet、测试、文档/样例校验和 build。新增功能必须有
focused test；并发、权限、持久化、重放、幂等和恢复变化需要失败路径测试。

## 禁止事项

- 不自动提交、推送、创建远端或改相邻仓库，除非用户明确要求。
- 不把设计目标写成“已实现”。
- 不用 float score + 单阈值替代证据、校准和决策原因。
- 不在可变 JSON/YAML blob 中埋所有配置。
- 不让 Agent 隐式扩大评审范围或获得远程写权限。
- 不让 Argus 自己生成的 Finding 自动成为自己的 gold label。
