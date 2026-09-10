# 图片生成

[English](image-generation.md) · [日本語](image-generation_ja.md)

`POST /v1/images/generations` 与 `POST /v1/images/edits` 服务于在后台声明了
**图片**输出模态的模型。请求与响应都是 OpenAI Images API 形状，OpenAI SDK
无需改造直接可用：

```bash
curl https://your-router/v1/images/generations \
  -H "Authorization: Bearer sk-yours" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "my-image-model",
    "prompt": "a red fox in the snow",
    "n": 1,
    "size": "1024x1024"
  }'
```

```json
{
  "created": 1700000000,
  "data": [{ "url": "https://upstream.example/img.png" }]
}
```

## 转发什么

请求体原样转发，只有 `model` 字段被改写为供应商自己的模型 ID。本网关不读取的
字段（`style`、`background`、供应商私有扩展……）按你写的原样到达上游。

## 上游方言

- **OpenAI 兼容供应商**原样透传。
- **DashScope 供应商**（base URL 在 `dashscope.aliyuncs.com`、
  `dashscope-intl.aliyuncs.com` 或 Model Studio 工作空间域名
  `{workspaceId}.{region}.maas.aliyuncs.com` 上）经原生多模态生成端点服务：
  请求被重新编码为该方言的形状（尺寸分隔符 `1024x1024` 变成 `1024*1024`），
  响应被解码回 OpenAI 形状。DashScope 只用图片 **URL** 应答——对这些候选，
  `response_format: "b64_json"` 请求会被拒绝，原因记录在尝试行上。
- OpenAI 兼容路由上，`qwen-image-*`、`wanx-*`、`wan2.*` 家族的模型会自动
  转换尺寸分隔符。

## 编辑与流式

`POST /v1/images/edits` 接受 OpenAI multipart 上传（参考图、mask、prompt），
转发给 OpenAI 兼容供应商时只改写模型字段；重新编码的结果在故障切换的各次
尝试间缓存。DashScope 主机上参考图会重新编码进原生方言（base64 data URI）——
该方言没有 mask 字段，因此带 mask 的请求按候选逐一拒绝，而不是被静默丢弃。
结算复用下面的按张规则。

`stream: true` 对 `gpt-image-*` 模型以命名事件 SSE 服务——`partial_image` 与
`completed` 事件，生成与编辑两条路由都支持。其他模型维持 400。用量从
completed 事件读取；未完成的交付——上游自己的错误事件、始终没完成一张图的
流、读取中断——不计费。

## 计费

按候选声明的计费模式：

- **按张计费** —— 品质×尺寸价格表（可带默认价）解析出单价，乘以**实际交付**
  的张数（请求 4 张收到 2 张就按 2 张计费）。没有任何匹配且无默认价的价格表
  会让请求保持未定价——记录为「未知」，那不等于免费。
- **按 token 计费** —— 按上游报告的 token 数，以候选的每百万单价计价。

失败的请求、或没有交付任何图片的 HTTP 200 应答，都不计费。每条已定价的记录
都在请求日志里携带定价依据快照（请求维度、请求与交付张数、单价）。

## 限制

- 图片流式是 `gpt-image-*` 家族的能力——其他家族对流式请求回应 400。
- 返回的 URL（及其有效期）属于上游；本网关不会重新托管图片字节。
