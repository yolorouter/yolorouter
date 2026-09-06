# 安全策略

[English](SECURITY.md) · 简体中文

## 报告漏洞

**请勿通过公开的 GitHub issue、讨论或 pull request 报告安全漏洞。**

请改用 GitHub 的私密
[**Report a vulnerability**](https://github.com/yolorouter/yolorouter/security/advisories/new)
流程（Security → Advisories），报告会私下送达维护者。

请尽量包含以下信息：

- 问题描述及其影响。
- 复现步骤或概念验证。
- 受影响版本（`./yolorouter --version`）与配置（数据库驱动、部署形态）。
- 如有缓解建议，一并附上。

我们会确认收到你的报告、同步修复进展，并在修复发布后在 release notes 中致谢
（除非你希望匿名）。

## 范围

Yolorouter 存有敏感材料——上游供应商密钥（AES-256 静态加密）、管理员凭据哈希、
API key 哈希。涉及这些机密保密性、鉴权与会话处理、访问控制绕过（模型白名单、
预算、key 状态）、出站请求安全（SSRF）的报告尤其有价值。

## 支持版本

Yolorouter 尚在 1.0 之前、迭代很快。安全修复只针对最新版本；报告前请先升级确认
问题仍可复现。
