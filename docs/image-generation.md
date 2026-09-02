# Image generation

[中文版](image-generation_zh.md)

`POST /v1/images/generations` serves models declared with the **image** output
modality in the console. The request and response are the OpenAI Images API
shape, so OpenAI SDKs work unmodified:

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

- `stream: true` is refused with a clear 400 — progressive image streaming is
  not supported.
- `POST /v1/images/edits` is not served yet.
- Returned URLs (and their expiry) belong to the upstream; this gateway does
  not rehost image bytes.
