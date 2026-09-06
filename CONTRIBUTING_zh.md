# 贡献 Yolorouter

[English](CONTRIBUTING.md) · 简体中文

感谢你有兴趣改进 Yolorouter！本指南介绍如何搭建开发环境、我们强制执行的编码规范，
以及改动如何合入。

参与即表示你同意遵守我们的[行为准则](CODE_OF_CONDUCT_zh.md)。

## 快速上手

环境要求：

- **Go 1.25.7+**
- **Node.js 22.12+**

```bash
git clone https://github.com/yolorouter/yolorouter.git
cd yolorouter

# 重新构建前端 + 后端、执行迁移并（重新）启动开发服务器。
./scripts/dev.sh
```

```powershell
# Windows（PowerShell）下同理。
.\scripts\dev.ps1
```

常用参数：`--backend`、`--frontend`、`--migrate`、`--restart`
（PowerShell 下为 `-Backend`、`-Frontend`、`-Migrate`、`-Restart`）。加 `--help`
（`-Help`）运行可查看完整列表；输出语言跟随系统 locale，可用 `YOLO_LANG=zh|en`
强制指定。

### 本地开发与调试

`./scripts/dev.sh` 会在 `http://localhost:8080` 起一个服务，并把日志写到
`logs/server.log`——排查问题先看这里。配置在 `configs/config.yaml`，SQLite 数据库在
`data/yolorouter.db`，两者都在首次运行时生成；删掉数据库文件即可从干净状态重来。

两侧代码库各自最快的迭代回路：

- **后端** —— 改完 Go 代码后 `./scripts/dev.sh --backend`（重新构建 + 重启，跳过前端）。
  要挂在调试器下或加额外参数，同一个入口直接可用：`go run ./cmd/yolorouter serve`。
- **前端** —— 完全不用重新构建。`cd frontend && npm run dev` 会在 5173 端口以热更新
  方式起控制台，并把 `/healthz`、`/api`、`/v1` 代理到后端（用 `VITE_BACKEND_TARGET`
  可覆盖目标地址）。开发服务器监听所有网卡，可以用手机打开来验证移动端布局。

要做请求级调试——协议转换、供应商故障切换、计费——在控制台打开请求日志的详情页：
每条中转记录都保存了完整的客户端请求体、上游请求体、上游响应体，以及逐次尝试的
路由链（每次尝试打到哪个供应商、哪把 key、为什么换下一个）。

### 构建与测试

```bash
make build          # 仅后端 -> ./bin/yolorouter
make build-embed    # 内嵌控制台的完整二进制

# 交叉编译（内嵌前端）
make build-macos            # -> ./bin/yolorouter-darwin-{amd64,arm64}
make build-windows          # -> ./bin/yolorouter-windows-{amd64,arm64}.exe
make build-windows-check    # 快速编译检查，不构建前端、不产出二进制

make test           # go test ./...
make test-embed     # 带 embedded-frontend 构建标签的测试
make vet            # go vet（普通 + -tags release）
```

## 项目结构

```
cmd/yolorouter/     CLI 入口（serve、db:migrate、update、version）
internal/           后端：handler → service → repository、中间件
  gateway/          中转内核：准入、候选选择、分发、交付、结算。
                    不承载任何自身功能逻辑。
  capability/       网关在「转发」之外对请求做的每一件事各占一个包——
                    限流、内容检查、系统提示注入、请求日志、输入压缩、
                    输出上限钳制。每个能力在装配处
                    （internal/router/capabilities.go）自行注册，
                    且永不 import 内核。
  decision/         一张表，规定每类观察结果对请求意味着什么：是否重试、
                    调用方看到什么、按什么计费。能力只上报，这张表做决定。
  fact/             能力上报所用的词汇表——路由判定与记账记录。在这里
                    新增一种记录类型，就是能力把信息写上审计行的通道。
  gates/            以测试形态运行的结构检查。它们强制执行 linter 管不了
                    的规则：错误码必须注册、决策表必须穷尽、注释不得指向
                    已不存在的符号。触发即失败——在本地跑 `make gates`。
  loopback/         网关自调用所需的进程级密钥与头名称；内核和回调进来的
                    能力都 import 它，从而互不 import。
  protocols/        线上格式（OpenAI chat、Anthropic、Gemini、Responses）
                    及它们互相转换所经的中间表示。
  selfupdate/       二进制原地升级机制（release 查找、校验和验证、原子
                    替换），`update` CLI 命令与管理端更新接口共用。
pkg/                可复用包（crypto、database、response……）
migrations/         goose 迁移（sqlite/ 与 postgres/）
web/                构建产物的 go:embed
frontend/           Vue 3 + TypeScript 管理控制台（Vite）
```

## 编码规范

### Go

- `gofmt` 强制。提交前跑 `gofmt -w`（或用编辑器的保存时格式化）。
- 代码、注释、字符串字面量一律**英文**。
- Lint 必须通过：

  ```bash
  golangci-lint run      # 配置在 .golangci.yml
  ```

- 测试与 vet 必须通过，含构建标签变体：

  ```bash
  make test              # go test ./...
  make vet               # go vet，普通 + -tags release
  make test-release      # -tags release
  make test-embed        # -tags embed（需先构建前端）
  ```

### 前端（Vue / TypeScript）

- `naive-ui` 组件必须显式 import（禁止全局自动注册）。
- 图标用 `@lucide/vue`。
- 构建通过 `vue-tsc` 做类型检查；类型检查红即 CI 失败：

  ```bash
  cd frontend && npm run build
  ```

## 提交与 Pull Request

- 尽量使用清晰的约定式提交主题（如 `feat(gateway): ...`、`fix(auth): ...`）。
- PR 保持聚焦；一个 PR 一个逻辑变更最易评审。
- 填写 pull request 模板——改了什么、为什么、怎么验证的。
- 请求评审前确保 CI 全绿（test、lint、embedded build）。
- 新行为应附带测试。

## 报告 Bug 与功能建议

用对应模板开 issue。Bug 请附上版本号（`./yolorouter --version`）、OS/架构、数据库
驱动、清晰的复现步骤。安全问题**不要**开公开 issue——见
[SECURITY_zh.md](SECURITY_zh.md)。

## 许可证

参与贡献即表示你同意你的贡献以 [Apache License 2.0](LICENSE) 授权。
