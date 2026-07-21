# Yolorouter — 前端

[English](README.md) · 简体中文

[Yolorouter](../README_zh.md) 的管理后台：基于 Vite 与 [naive-ui](https://www.naiveui.com/)
构建的 Vue 3 + TypeScript 单页应用。

生产环境下，本应用会被编译并通过 `go:embed` 内嵌进单个 Go 二进制文件
（见 [`../web`](../web)）。只有做前端开发时才需要下面的步骤。

## 环境要求

- Node.js >= 22.12

## 开发

```bash
npm ci
npm run dev      # 启动带热更新的 Vite 开发服务器
```

把开发服务器指向一个正在运行的后端（完整的全栈开发流程见仓库根目录
[`README_zh.md`](../README_zh.md) 的 *开发* 一节，通过 `./scripts/dev.sh` 启动）。

## 构建

```bash
npm run build    # 类型检查（vue-tsc）+ 生产构建，产物输出到 dist/
```

如需生成内嵌了 UI 的二进制文件，请在仓库根目录执行：

```bash
make build-embed
```

## 约定

- `naive-ui` 的组件必须显式 import。
- 图标使用 `@lucide/vue`。
- 提交 PR 前请先阅读仓库的 [`CONTRIBUTING.md`](../CONTRIBUTING.md)。
