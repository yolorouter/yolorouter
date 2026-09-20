# Spec: Gemini 命名空间下未知方法名返回 404

## Problem Statement

网关的 Gemini 原生入口是单条通配路由 `POST /v1beta/models/:modelaction`，
任何 `:方法名` 都能匹配上。方法名拼错（如 `:streamGenerateReply`）时：
无密钥 → 401"missing API key"（鉴权先于路由语义，用户看不出是名字打
错）；有合法密钥 → 协议分类回退成 OpenAI，请求被当普通聊天补全处理，
发到 Gemini 地址却收到聊天响应。调用方得不到任何"这个方法不存在"的
信号。

## Solution

Gemini 命名空间（`/v1beta/models/` 前缀）内、方法名不是两个已知动作
（`:generateContent` / `:streamGenerateContent`）的请求，在入口处直接
返回 404，封套与网关其它未识别路由的 NoRoute 行为完全一致（网关命名
空间 + errcode.RouteNotFound）。无密钥的请求保持现状 401（鉴权先行的
安全性质是既有测试钉死的，不因本修复改变）。

## User Stories

1. 作为 Gemini 调用方，我打错方法名时想立刻看到 404"路由不存在"，
   这样一眼知道是名字错了而不是去检查密钥。
2. 作为 Gemini 调用方，我带合法密钥打错方法名时不想收到一个语义错乱
   的聊天补全响应，这样不会把网关的兜底误当成正常结果。
3. 作为 API 维护者，我要未知方法的 404 与网关其它未识别路由同一封套
   （errcode.RouteNotFound），这样客户端用一套错误处理覆盖所有 404。
4. 作为 API 维护者，我要无密钥访问该路径仍先回 401，这样"鉴权先于
   路由语义"的安全性质不被破坏。
5. 作为 API 维护者，我要已知两个动作（非流式/流式）的行为一字不变，
   这样存量调用零感知。
6. 作为未来读者，我要守卫处注释讲清"为什么在入口而不是路由层判"
   （通配路由无法在 gin 路由层区分动作名），这样不会有人想把它搬错
   地方。

## Implementation Decisions

- 守卫放在统一入口 handler 计算完 ingress 协议之后、进入内核服务之前：
  路径命中 Gemini 前缀但协议未判为 Gemini → 写 404。该入口已被
  `/v1beta` 组的鉴权中间件保护，守卫天然在鉴权之后（满足故事 4）。
- 协议分类函数（negotiate）不改：分类回退 OpenAI 的行为被其它路径
  依赖；404 是命名空间级守卫，不是分类变更。
- 404 封套复用 NoRoute 对网关命名空间路径的既有写法（同一 errcode、
  同一 namespaced envelope 助手），不新造错误码。
- 涉及的入口 handler 属于 gateway-parity 逐字共享集，改动须双仓同步
  （OSS + 托管，模块路径归一），并过 parity 门禁。

## Testing Decisions

- 路由层测试：有效密钥 + 未知方法名 → 404 且封套为 RouteNotFound；
  未知方法名 + 无密钥 → 仍 401（钉住鉴权先行）；已知两动作回归不变。
- 断言只看外部可观察事实（HTTP 状态、错误码、消息），不测内部调用序。
- 先例：router_test.go 既有 Gemini 路由测试（401 无密钥、斜杠 404）。

## Out of Scope

- negotiate 的协议分类语义；其它协议入口的行为；无 colon 路径之外的
  /v1beta 新路由（如未来 countTokens）——届时在已知动作集合中登记即可。
- CHANGELOG 之外的发布动作（不发版、不打 tag）。

## Further Notes

- 背景：2026-09-19 e2e 全量跑 S6 时踩中（技能文档笔误
  streamGenerateReply），F46 记档；调查报告确认通配路由 + 鉴权先行 +
  分类回退三因叠加。用户选定修法 A（404 对齐 NoRoute）。
