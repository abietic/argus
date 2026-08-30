# Security Policy

## Supported versions

Argus 当前处于 `v1alpha1` 开发阶段，仅最新 `main` 接收安全修复；尚不承诺稳定版本的
长期支持或兼容窗口。

## Reporting a vulnerability

请使用 GitHub 仓库的 **Security → Report a vulnerability** 私下提交报告，不要创建
包含利用细节、凭据、源码或生产标识的公开 Issue。

报告应尽量包含：

- 受影响的 commit、契约或执行路径；
- 最小复现步骤和影响边界；
- 是否涉及远程写入、权限绕过、敏感 Artifact、Replay 副作用或 unknown outcome；
- 已知缓解方式。

维护者会先确认收到报告，再根据影响和可复现性协调修复与披露时间。请不要在修复
发布前公开漏洞细节。
