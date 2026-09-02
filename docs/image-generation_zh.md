# 图片生成

[English](image-generation.md)

`POST /v1/images/generations` 服务于在后台声明了**图片**输出模态的模型。请求与响应
都是 OpenAI Images API 形状，OpenAI SDK 无需改造直接可用：

```bash
curl https://your-router/v1/images/generations \
  -H "Authorization: Bearer sk-yours" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "my-image-model",
    "prompt": "雪地里的一只红狐狸",
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

## 转发规则

请求体只改写 `model` 字段为供应商的真实模型 id，其余原样转发。本网关不读取的
字段（`style`、`background`、供应商私有扩展等）会与你填写的完全一致地到达上游。

## 上游方言

- **OpenAI 兼容供应商**原样透传。
- **DashScope 供应商**（base URL 在 `dashscope.aliyuncs.com`、
  `dashscope-intl.aliyuncs.com` 或百炼专属工作空间域
  `{workspaceId}.{region}.maas.aliyuncs.com` 下）经由原生 multimodal-generation
  端点服务：
  请求会转编码为方言形状（尺寸分隔符 `1024x1024` 变为 `1024*1024`），响应解码回
  OpenAI 形状。DashScope 只返回图片 **URL**——`response_format: "b64_json"` 的
  请求对这些候选逐一拒绝，原因记录在尝试详情里。
- OpenAI 兼容路由上，`qwen-image-*`、`wanx-*`、`wan2.*` 家族的模型会自动转换
  尺寸分隔符。

## 计费

按候选声明的计费方式：

- **按张** —— 由质量×尺寸价格表（可配默认单价）解析出单价，乘以**实际交付**的
  张数（请求 4 张实收 2 张按 2 张计）。没有命中且无默认价时该请求不计价——记录为
  未知，与免费不是一回事。
- **按 token** —— 按上游上报的 token 用量、候选的每百万单价计费。

请求失败、或 HTTP 200 但没有交付图片，一律不计费。每条计价的请求日志都带有计价
快照（请求参数、请求与实收张数、单价）。

## 限制

- `stream: true` 会收到明确的 400——渐进式图片流式暂不支持。
- `POST /v1/images/edits` 暂未提供。
- 返回的 URL（及其时效）属于上游；本网关不转存图片字节。
