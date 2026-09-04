<div align="center">

# Yolorouter

**让 Claude Code（或任何 AI CLI）跑在任意供应商上——免费、自托管的单二进制 LLM 网关：四种对话协议任意进出，外加 OpenAI 图片与视频 API、多供应商自动 failover、上游 Key 容量池、多用户管理后台内嵌。**

[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![CI](https://github.com/yolorouter/yolorouter/actions/workflows/ci.yml/badge.svg)](https://github.com/yolorouter/yolorouter/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/yolorouter/yolorouter)](https://goreportcard.com/report/github.com/yolorouter/yolorouter)
[![Release](https://img.shields.io/github/v/release/yolorouter/yolorouter?sort=semver)](https://github.com/yolorouter/yolorouter/releases)
[![Go](https://img.shields.io/badge/go-1.25.7+-00ADD8.svg)](go.mod)

[English](README.md) · 简体中文

[快速开始](#快速开始) · [协议](#协议) · [成本优化](#成本优化) · [文档](#文档) · [贡献](#贡献)

⚡ **低开销流式代理** · 🔀 **任意协议进、任意协议出** · 🆓 **免费开源** · 📦 **单二进制 · 零外部依赖** · 🔁 **自动 failover + Key 容量池** · 👥 **多用户与 SSO** · 💰 **成本分析与优化**

</div>

---

把你的应用指向**一个**端点、**一个** API Key。Yolorouter 站在你的应用和上游供应商之间，
把那些麻烦事——管理多个供应商账号、轮换被限流的 Key、账号出问题时自动切换、按 Key
控制预算、搞清楚每一笔花了多少钱——都收在一个地方，而不是散落在每个代码库里。

它接受**四种协议**入口：OpenAI Chat Completions、OpenAI Responses、Anthropic Messages
和 Gemini `generateContent`，转发时可以把其中任意一种翻译成另一种。只会 OpenAI
的供应商也能跑 Claude Code；只会 Anthropic 的供应商也能服务 OpenAI SDK。流式、
工具调用、推理/思考块都能完整穿过这层翻译；图片内容除 Responses 入口外也都支持
（见[协议](#协议)）。

所有东西都在**一个二进制**里，管理后台已内嵌。不需要 Node 运行时，不需要单独部署前端，
不依赖任何外部服务——SQLite 开箱即用，需要时可换 PostgreSQL。

## 为什么用 Yolorouter

**路由**

- **多供应商 failover** —— 把一个对外模型名（如 `smart`）映射到有序的供应商候选列表。某个不可用时自动切到下一个，调用方全程只看到同一个对外模型名。
- **每模型调度模式：故障转移 / 均衡调度** —— 默认故障转移（主备优先，排最前者承接全部流量）；也可把模型切到均衡调度：不同调用方 API Key 均匀分摊到各供应商，同一 Key 粘住同一家，保住上游 prompt 缓存。详见[调度模式](#调度模式)。
- **上游 Key 容量池** —— 每个供应商配一个 Key 池，负载按轮询分摊到全部 Key。被限流的 Key 会按其 `Retry-After` 窗口自动冷却避让（后续请求优先走健康 Key）；认证失败、额度不可用的 Key 直接摘除，待重测通过后回池。
- **一键批量导入模型** —— 添加供应商后，网关自动拉取其真实上游模型目录，勾选想要的模型即可一次导入。每个导入的映射会在后台对真实上游探测验证，通过的自动启用；失败的保留诊断信息，可一键重测。命中内置价目表的模型自动预填价格。
- **模型别名** —— 调用方用稳定的对外名；每个供应商候选把它映射到该供应商实际接受的模型 id。保存候选映射时会真实探测一次上游，配错的模型名在配置阶段就暴露，而不是等到半夜出故障。
- **图片与视频生成** —— 对话之外同时服务 OpenAI Images 与 Videos API。图片按实际交付张数走质量×尺寸价格表计费（或按 token）；视频是任务方言——`POST /v1/videos` 返回可轮询的任务，完成首次被观测到时按秒×分辨率档价一次性结算，Key 预算把每个在途任务的定价上界计入预留。原生 API 非 OpenAI 形状的供应商按其方言直连：DashScope（wan 视频、qwen-image）、火山方舟（Seedance 视频、Seedream 图片）、可灵（`kling-3.0` 系视频、`kling-v3` 图片、多参考图的 Omni 家族——调用方的 `image_list` 等可灵原生字段原样透传）、MiniMax（`MiniMax-H3` / `MiniMax-H3-Max` 视频经 V2 任务 API——文生视频与首帧图生视频；注意 H3-Max 生成 5~15 秒短片，4 秒请求对该模型被拒；该域名同域服务 chat 方言，chat 探测无法服务该模型时 key 验证自动回退视频探测）。媒体单模型账号的 Key，在 chat 探测无法服务该模型时改经真实媒体探测验证。
- **视觉回退** —— 让纯文本模型也能"看图"。在后台把模型标记为不支持图片并指定一个视觉模型后，请求里的图片会先由视觉模型转成文字描述再转发，调用方无感知，四种接入协议都支持；没配视觉模型时，图片会被替换成明确的占位说明，而不是让上游直接报错。
- **流式做对了** —— Key 切换与 failover 都发生在首字节抵达客户端**之前**；一旦开始流式，供应商即被锁定，绝不把两个供应商的内容拼进同一个响应。
- **为推理模型调过的超时** —— 七个互相独立、可配置的阶段，而不是一刀切的总超时，所以"想了八分钟才吐第一个 token"的模型不会被中途掐断。

**管控与成本**

- **按 Key 访问控制** —— 模型白名单、速率与并发限制、累计预算上限、可选过期时间，支持即时吊销。
- **多用户与 SSO** —— 团队成员通过任意 OAuth2/OIDC 身份源（Zitadel、GitHub、Keycloak、钉钉、飞书等，钉钉/飞书接入见[接入指南](docs/dingtalk-feishu-login_zh.md)）登录、首次登录自动建号，也可以由管理员在后台直接创建本地用户名密码账号——两种方式都无需邀请。成员自助管理自己的 API Key，只能看到自己的用量与费用；管理员拥有全局视图，可按账号筛选所有统计，并可对账号做创建、升降角色与禁用。禁用即刻生效：会话立即登出、名下所有 Key 立即失效。
- **成本优化** —— 可全局或按 Key 注入自定义系统提示词；把体积大的工具输出在发往上游前压缩。后台会显示压缩的实测节省，以及系统提示词按公开基准估算的所选时段「预计节省费用 / Token」。
- **内置可观测性** —— token / 成本 KPI 仪表盘，按模型 / 供应商 / 时间 / 账号 / 令牌的用量与成本分析，以及含完整逐次尝试路由链的请求日志。任意视图可导出 CSV。
- **双语后台** —— 简体中文与 English，登录前后随处可切；时区跟随浏览器。
- **自更新** —— 二进制可检查并应用新版本。

## 截图

<div align="center">
  <img src="docs/screenshots/dashboard.png" alt="仪表盘" width="49%" />
  <img src="docs/screenshots/analytics.png" alt="分析" width="49%" />
</div>

## 快速开始

### Docker

```bash
docker run -d --name yolorouter --restart unless-stopped \
  -p 8080:8080 -v "$PWD/yolorouter:/yolorouter" \
  ghcr.io/yolorouter/yolorouter:latest
```

或者下载 [docker-compose.yml](docker-compose.yml) 后执行 `docker compose up -d`。
每次发版都会发布 amd64 和 arm64 双架构镜像。

容器写的所有东西都在挂载的这一个目录里：自动生成的 `configs/config.yaml`
（含加密上游 Key 的主密钥）和 SQLite 数据库。备份这个目录就是备份整个部署。

**docker compose 方式升级：**

```bash
docker compose pull   # 下载最新镜像；正在运行的容器不受影响
docker compose up -d  # 用新镜像重建容器（已是最新则什么也不做）
```

**直接 `docker run` 方式升级**分三步。这样做是安全的，因为容器的文件系统本来
就是一次性的：你的状态都不在容器里——配置和数据库存在宿主机的挂载目录中，
删掉容器它们原地不动。

```bash
# 1. 下载最新镜像。正在运行的容器照常服务——这一步只是把新镜像下载到本机。
docker pull ghcr.io/yolorouter/yolorouter:latest

# 2. 停止并删除旧容器。你的数据不在容器里：全部在宿主机挂载目录中，不会丢。
docker rm -f yolorouter

# 3. 启动新容器——和第一次部署时完全相同的命令、挂载同一个目录，
#    它会自动使用第 1 步拉下来的新镜像。
docker run -d --name yolorouter --restart unless-stopped \
  -p 8080:8080 -v "$PWD/yolorouter:/yolorouter" \
  ghcr.io/yolorouter/yolorouter:latest
```

两个值得知道的细节：

- 第 3 步要**在当初启动容器的同一个目录下执行**——`-v "$PWD/yolorouter:/yolorouter"`
  这个挂载是相对当前目录解析的，换个目录跑就等于挂了一个空数据目录，会看到
  全新的初始化界面。`-v` 里直接写绝对路径可以彻底避开这个坑。
- 新版本第一次启动时会自动执行待应用的数据库迁移，然后照常服务。如果在迁移
  已经执行之后想退回旧版本，请用升级前的备份恢复数据目录，而不是直接换回旧
  镜像启动——旧二进制可能不认识新 schema。想固定在某个版本不自动跟进，用带
  版本号的 tag（如 `...:v0.1.6`）代替 `:latest` 即可。

> **🇨🇳 国内拉取镜像**：`ghcr.io` 在大陆直连不畅时，可用 GHCR 镜像加速站拉取后改名，
> 例如南京大学镜像站：
>
> ```bash
> docker pull ghcr.nju.edu.cn/yolorouter/yolorouter:latest
> docker tag ghcr.nju.edu.cn/yolorouter/yolorouter:latest ghcr.io/yolorouter/yolorouter:latest
> ```
>
> 也可以直接把 compose 里的 `image` 换成加速站地址。第三方镜像站的可用性以其自身公告为准。

### 安装成系统服务

不用 Docker、或想要内置的在线自更新能力，就装成开机自启的后台服务——Linux 用
systemd，macOS 用 launchd，Windows 用计划任务：

```bash
# Linux / macOS
curl -fsSL https://get.yolorouter.com/install.sh | bash
```

```powershell
# Windows，PowerShell 5.1+
irm https://get.yolorouter.com/install.ps1 | iex
```

Windows 上，用管理员身份运行 PowerShell 会装成开机自启的系统级服务；用普通权限运行则装
在当前用户下，登录时自启。

> **🇨🇳 国内加速安装**：如果你在国内、直连 GitHub 慢或不通，把 `get.yolorouter.com`
> 换成 `gh.yolorouter.com` —— 同一个安装器，经 Cloudflare 代理下载，装完后的自动升级
> 也会一直走加速通道，无需额外配置。

重跑同一条命令即可升级，配置和数据库原样保留，升级前会先自动备份数据库。想直接跑二进制？
从[发布页](https://github.com/yolorouter/yolorouter/releases)下载后执行 `./yolorouter serve`
（Windows 上是 `.\yolorouter.exe serve`）。

### 首次运行

无论用哪种方式启动，首次运行都会生成 `configs/config.yaml`、执行数据库迁移，并在 8080
端口启动后台。创建首个管理员账号后按引导操作：添加供应商并填入上游 Key——保存后后台会
自动拉取该供应商的上游模型目录，勾选需要的模型即可一键导入；每个导入的模型会在后台对
真实上游探测验证，通过的自动启用。最后签发 API Key 即可开始调用。

→ **完整安装说明（全平台，含从源码构建）：**
[yolorouter.com/docs/self-hosted/installation](https://yolorouter.com/docs/self-hosted/installation?utm_source=oss-readme&utm_medium=repo)

## 协议

下面每个入口都用**同一个** Yolorouter API Key 认证、都支持流式，并且都可以由**任意**
已配置的供应商来承接——不管那个供应商原生说哪种协议。

| 入口路由 | 协议 | 可用的认证头 |
| --- | --- | --- |
| `POST /v1/chat/completions` | OpenAI Chat Completions | `Authorization: Bearer`、`X-Api-Key` |
| `POST /v1/responses` | OpenAI Responses | `Authorization: Bearer`、`X-Api-Key` |
| `POST /v1/messages` | Anthropic Messages | `Authorization: Bearer`、`X-Api-Key` |
| `POST /v1/images/generations` | OpenAI Images（图片生成） | `Authorization: Bearer`、`X-Api-Key` |
| `POST /v1/images/edits` | OpenAI Images（图片编辑） | `Authorization: Bearer`、`X-Api-Key` |
| `POST /v1/videos`、`GET /v1/videos/{id}`、`GET /v1/videos/{id}/content` | OpenAI Videos（任务方言） | `Authorization: Bearer`、`X-Api-Key` |
| `POST /v1beta/models/{model}:generateContent`<br>`POST /v1beta/models/{model}:streamGenerateContent` | Gemini | `x-goog-api-key`、`?key=`、`Authorization: Bearer`、`X-Api-Key` |
| `GET /v1/models`、`GET /v1/models/{model}` | 模型发现 | `Authorization: Bearer`、`X-Api-Key` |

图片入口只服务于在后台声明了**图片**输出模态的模型。OpenAI 兼容供应商原样透传；
DashScope 与可灵域名的供应商经由其原生任务方言服务，同步应答为 OpenAI 形状（只返回
图片 URL——请求 `b64_json` 的候选会被逐一拒绝）。图片模型按候选声明的口径计费：按
实际交付张数走质量×尺寸价格表，或按 token 用量；请求没有交付图片则不计费。图片编辑
接收 OpenAI multipart 上传；DashScope 上参考图重编码进原生方言（该方言无 mask 字段，
携带 mask 的请求对其候选拒绝），`gpt-image-*` 系模型以命名 SSE 事件流式吐出渐进分图。

视频入口是任务方言：`POST /v1/videos` 提交生成并返回任务资源，调用方经
`GET /v1/videos/{id}` 轮询、从 `GET /v1/videos/{id}/content` 下载成片；OpenAI 官方 SDK
无改动可用（`create_and_poll` 自带轮询）。结算一次性发生——完成首次被观测到时，按
上游实际交付的秒数 × 请求尺寸对应的分辨率档计价；失败、取消、过期任务零计费。视频
上游均为任务制方言（DashScope wan、方舟 Seedance、可灵新版端点、MiniMax V2）；已受理
的任务绝不重投其他候选——上游受理即渲染、即产生成本，无论调用方最终是否被计费。MiniMax
几点说明：`MiniMax-H3-Max` 只收 5~15 秒（4 秒请求携原因被拒）；其最大输出为 768P，而
`MiniMax-H3` 的大尺寸档位对应 2K 输出；任务上游仅可查 7 天，超窗的未终态任务按过期零计费；成片链接
限时（官方未载明时长），请及时下载或转存。MiniMax 的视频生成走**按量余额**计费（Token Plan 订阅、
积分包与海螺视频资源包均不覆盖 H3 系模型）。

请求里的 `model` 是你在后台配置的**对外名**。Yolorouter 会挑选供应商候选、替换成真实的
上游模型 id，并在返回时保持你的对外名不变。

> **已知限制**：Responses 入口的 `input_image` 条目，在请求需要翻译成另一种出口协议时
> 会被丢弃，只有文本被传递。同协议透传不受影响，另外三个入口的图片内容翻译正常。
>
> **媒体说明**：图片 `stream` 是 `gpt-image-*` 家族的能力——其他家族的流式请求收到
> 400。返回的图片与视频 URL 均来自上游、时效由上游决定（Yolorouter 只代理、不转存）；
> 视频任务没有取消面——已接线的任务方言均未暴露取消能力。

### 让现有 SDK 和工具直接指过来

因为入口就是真正的原生协议，官方 SDK 和 agent 工具只要改两个设置就能接进来，不需要
适配层。

```python
# OpenAI Python SDK
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8080/v1", api_key="sk-yr-your-key")
print(client.chat.completions.create(
    model="smart",
    messages=[{"role": "user", "content": "你好！"}],
).choices[0].message.content)
```

```bash
# Claude Code——经 Yolorouter 转发到你配置的任意供应商
export ANTHROPIC_BASE_URL=http://localhost:8080
export ANTHROPIC_AUTH_TOKEN=sk-yr-your-key
claude
```

→ **各协议的完整请求示例，以及 19 个 agent 工具的接入指南**
（Claude Code、Cursor、Codex CLI、Cherry Studio、Gemini CLI、opencode……）：
[yolorouter.com/docs](https://yolorouter.com/docs?utm_source=oss-readme&utm_medium=repo)

## 调度模式

每个模型都路由到一条按序排列的供应商候选链，调度模式决定一次请求从哪家候选进入：

- **故障转移**（默认）：主备优先——排最前的候选承接全部流量，链上其余候选只在它失败时顶上。所有模型默认如此。
- **均衡调度**：把不同调用方 API Key 均匀分摊到各供应商。每个 Key 被分给当前绑定数最少的一家并从此粘住它：同一 Key 的多轮对话始终打到同一家供应商，尽量保持上游 prompt 缓存命中——中途换供应商等于每轮把已缓存的 token 重新全价计费。（网关保证的是供应商亲和；缓存是否命中还取决于该供应商的上游 Key 池——不同上游账号的 Key 不共享缓存域。）绑定的供应商触发熔断后，熔断窗口内发起请求的 Key 自动换绑到其他家（休眠 Key 保留原绑定，直到下次调用）；因此恢复后它的绑定数偏少、优先吸引新分配，无需后台再平衡即可自愈。模型详情页显示每家供应商当前的绑定 Key 数（进入页面时的瞬时快照）。

其余一切——失败转移、Key 轮换、熔断、预算——两种模式完全一致。

**已知限制：** 绑定表为进程内存态。重启后 Key 会被重新分配（很快收敛回同样的均匀分布）；多实例部署下各实例独立计算自己的分摊，没有跨实例共享的绑定表。从不含调度模式的旧版本滚动升级期间，未升级实例会把所有模型当 failover 运行——请在全部实例升级完成后再把模型切到均衡调度。绑定表全局上限 4096 条（Key × 模型组合），超限后最久未用的绑定被淘汰、其 Key 下次请求时重新分配——极宽的部署（数百 Key × 数十个均衡模型）会在边际上损失部分粘性。

## 成本优化

两项功能默认关闭，在后台全局设置，也可以按 API Key 单独覆盖。

**自定义系统提示词注入。** 不改客户端代码，就能给每个请求的系统提示追加统一规则。追加
的位置跟随调用方自己的协议形态，而且是确定性的：多次请求得到的系统内容字节一致，仍然能
命中上游的 prompt 缓存。后台展示的「预计节省费用 / Token」背后是一组公开的开/关
配对基准实验——实验方法与全部 150 对原始数据见
[docs/concise-output-benchmark_zh.md](docs/concise-output-benchmark_zh.md)。

**输入压缩。** 编码类 agent 会回传大量高度冗余的工具输出。Yolorouter 会识别请求中每个
内容块的类型——`go test` 输出、git diff、grep 结果、普通日志——只去掉噪声、保留信号：
失败、堆栈、每一条不同的匹配都会保留。压缩不会碰对话尾部的活跃编辑区，并且只有压完确实
更短时才替换。

缓存读 / 缓存写 token 在仪表盘、分析和成本页里全程单独计量和计价，所以 prompt 缓存省下
多少是一个能看到的数字，不是感觉。

→ **细节与调优：**
[yolorouter.com/docs/self-hosted/configuration](https://yolorouter.com/docs/self-hosted/configuration?utm_source=oss-readme&utm_medium=repo)

## 文档

| 主题 | 链接 |
| --- | --- |
| 安装（全平台、从源码构建） | [安装](https://yolorouter.com/docs/self-hosted/installation?utm_source=oss-readme&utm_medium=repo) |
| `config.yaml` 全字段与 CLI | [配置](https://yolorouter.com/docs/self-hosted/configuration?utm_source=oss-readme&utm_medium=repo) |
| 升级、回滚、卸载 | [升级与卸载](https://yolorouter.com/docs/self-hosted/updating?utm_source=oss-readme&utm_medium=repo) |
| 分层结构、协议 IR、存储 | [架构](https://yolorouter.com/docs/self-hosted/architecture?utm_source=oss-readme&utm_medium=repo) |
| API 参考与模型列表 | [文档首页](https://yolorouter.com/docs?utm_source=oss-readme&utm_medium=repo) |
| 钉钉 / 飞书登录接入 | [钉钉 / 飞书登录](docs/dingtalk-feishu-login_zh.md) |

自托管需要你自己准备各家上游供应商的 API Key。如果你不想一家家去注册和充值，
**YoloRouter Cloud** 已经在后台的预置供应商列表里，可以作为其中一个上游选中使用——
详见[托管版](https://yolorouter.com/pricing?utm_source=oss-readme&utm_medium=repo)。

## 从源码构建

依赖：**Go 1.25.7+** 与 **Node.js 22.12+**。

```bash
make build          # 仅后端 -> ./bin/yolorouter
make build-embed    # 内嵌后台的完整二进制
```

### 本地开发与调试

日常开发用一个脚本：重建、跑迁移、重启本地服务。

```bash
./scripts/dev.sh          # 全量重建 + 重启，服务在 http://localhost:8080
./scripts/dev.sh --backend    # 只改了 Go 代码用这个；前端改动用 --frontend
tail -f logs/server.log   # 服务日志 —— 调试问题先看这里
```

配置文件在 `configs/config.yaml`，SQLite 数据库在 `data/yolorouter.db`，都在
首次启动时自动生成。调试具体某个请求时，去后台请求日志的详情页：每次转发的
完整客户端请求体、上游请求体、上游响应体和逐次尝试的路由链都在那里。

改前端不需要走重建循环 —— Vite 开发服务器带热更新，跑在 5173 端口，并把
`/api` 与 `/v1` 代理到后端：

```bash
cd frontend && npm run dev
```

`make test` 跑 Go 测试，`make gates` 跑 CI 强制执行的结构检查。Windows 脚本
（`scripts/dev.ps1`）、lint 与交叉编译目标见
[CONTRIBUTING.md](CONTRIBUTING.md#local-development-and-debugging)。

## 贡献

欢迎提 Issue 和 PR。请先阅读 [CONTRIBUTING.md](CONTRIBUTING.md) 与
[行为准则](CODE_OF_CONDUCT.md)。报告安全问题见 [SECURITY.md](SECURITY.md)。

## 许可证

基于 [Apache License 2.0](LICENSE) 授权。
