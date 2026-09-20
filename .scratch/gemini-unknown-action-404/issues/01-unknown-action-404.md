# 01: Gemini 命名空间未知方法名 404（双仓同步）

**What to build:** 调用方带合法密钥 POST `/v1beta/models/{model}:任意未知方法`
时收到 404（与网关其它未识别路由同封套：RouteNotFound）；无密钥仍 401；
已知两个动作行为不变。改动落在统一入口 handler（parity 共享文件），
OSS 与托管仓逐字同步。

**Blocked by:** None (can start immediately).

**Status:** resolved

- [x] 有效密钥 + `:streamGenerateReply`（未知）→ HTTP 404，封套 code 为
      RouteNotFound（与 NoRoute 网关命名空间行为一致）
- [x] 无密钥 + 未知方法 → 仍 HTTP 401（鉴权先行不被破坏）
- [x] 已知动作 `:generateContent` / `:streamGenerateContent` 回归不变
      （非流式/流式各一发真机或测试级验证）
- [x] 守卫只影响 Gemini 前缀路径；其它入口（/v1/chat/completions 等）
      行为零变化
- [x] 双仓同步：OSS 与托管共享文件逐字一致，gateway-parity 门禁绿
- [x] CHANGELOG 双语 Unreleased 各补一条
- [x] 双轴 loop-review 收敛（标准轴 + 规格轴独立子代理）
- [x] 单独 commit 推送（OSS main + 托管 master + 超项目 bump）

## 完成注记（2026-09-20）

**实现**：共享层新增 `WriteUnknownRouteError`（middleware NoRoute 分发器
的 gateway 侧孪生——因 middleware 反向导入 gateway 无法复用，分支结构与
载荷逐一对照重实现；OpenAI 分支用有序结构体保证与 WriteGatewayError
序列化逐字节一致）；统一入口 handler 在 ingress 判定后加守卫：Gemini
前缀且未判为 Gemini 协议 → 404。守卫位于鉴权中间件之后（401 先行结构
性成立）、auth-context 读取之前。

**验证证据**：新路由测试 2 条（带密钥打错名 → 404 + route_not_found
封套断言；无密钥 → 401）+ 既有 Gemini 路由测试 4 条全过；internal/
gateway 53s 全量绿、internal/router、middleware 绿、go vet 干净；
gateway-parity 通过（规格轴复算 48 文件 sweep drift=0）；双仓镜像
归一化 diff 为空；已知动作回归：既有 generateContent 路由测试过 +
本日早前隔离实例 S6 真机已验非流式/流式（guard 结构上不可能对已知
动作触发：IngressProtocol 恰将两后缀判为 Gemini，negotiate_test 钉死）。

**评审轮次**：2 轮双轴收敛。R1：标准轴 1 项（gin.H 字母序键序破坏
"逐字节一致"契约 → 改有序结构体修复）；规格轴 2 项（同一键序发现，
已由同一修复覆盖；Claude/Gemini 分支当前调用点不可达 → 记登记表）。
R2：双轴零发现。

**登记表**：1 条——WriteUnknownRouteError 的 Claude/Gemini 分支在当前
唯一调用点不可达（ingress 恒为 OpenAI），有意保留：该函数的契约是
NoRoute 分发器的分支级孪生（标准轴要求逐分支对照），删掉会破坏孪生
结构并使未来 Claude/Gemini 调用点失去协议封套。

**偏差**：无。
