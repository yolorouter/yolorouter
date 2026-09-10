# Image generation

[中文版](image-generation_zh.md) · [日本語](image-generation_ja.md)

`POST /v1/images/generations` and `POST /v1/images/edits` serve models
declared with the **image** output modality in the console. The request and
response are the OpenAI Images API shape, so OpenAI SDKs work unmodified:

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

## What is forwarded

The request body is forwarded with only the `model` field rewritten to the
provider's own model id. Fields this gateway does not read (`style`,
`background`, provider-private extensions, …) reach the upstream exactly as
you wrote them.

## Upstream dialects

- **OpenAI-compatible providers** are passed through as-is.
- **DashScope providers** (base URL on `dashscope.aliyuncs.com`,
  `dashscope-intl.aliyuncs.com`, or a Model Studio workspace domain
  `{workspaceId}.{region}.maas.aliyuncs.com`) are served through the native
  multimodal-generation endpoint: the request is re-encoded into the dialect's
  shape (size separator `1024x1024` becomes `1024*1024`), and the response is
  decoded back into the OpenAI shape. DashScope answers with image **URLs
  only** — a `response_format: "b64_json"` request is refused for those
  candidates with the reason recorded on the attempt.
- On OpenAI-compatible routes, models of the `qwen-image-*`, `wanx-*`, and
  `wan2.*` families get the size separator converted automatically.

## Edits and streaming

`POST /v1/images/edits` accepts the OpenAI multipart upload (reference
images, mask, prompt) and forwards it to OpenAI-compatible providers with
only the model field rewritten; the re-encode is cached across failover
attempts. On a DashScope host the reference images are re-encoded into the
native dialect as base64 data URIs — that dialect has no mask field, so a
masked ask is refused per candidate rather than silently dropped. Settlement
reuses the per-image rules below.

`stream: true` is served as named-event SSE for `gpt-image-*` models —
`partial_image` and `completed` events, on both the generation and the edit
route. Any other model keeps the 400. Usage is read from the completed
events, and an incomplete delivery — the upstream's own error event, a stream
that never completes an image, a broken read — bills nothing.

## Billing

Per the candidate's declared billing mode:

- **Per image** — a quality×size price table (with an optional default price)
  resolves the unit price, multiplied by the number of images **actually
  delivered** (asking for 4 and receiving 2 bills 2). A table that matches
  nothing and has no default leaves the request unpriced — recorded as
  unknown, which is not the same as free.
- **Per token** — the upstream's reported token counts, priced at the
  candidate's per-million rates.

A request that fails, or an HTTP 200 answer that delivered no images, bills
nothing. Every priced row carries a snapshot of what it was priced by
(request axes, requested vs delivered count, unit price) in the request log.

## Limits

- Image streaming is a `gpt-image-*`-family capability — other families
  answer a streaming ask with 400.
- Returned URLs (and their expiry) belong to the upstream; this gateway does
  not rehost image bytes.
