# 画像生成

[English](image-generation.md) · [中文版](image-generation_zh.md) · 日本語

`POST /v1/images/generations` は、コンソールで出力モダリティが**画像**と宣言された
モデルを提供します。リクエストとレスポンスは OpenAI Images API 形状のため、
OpenAI SDK が無改造で動きます:

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

## 何が転送されるか

リクエストボディは `model` フィールドだけがプロバイダー自身のモデル ID に書き
換えられて転送されます。このゲートウェイが読まないフィールド（`style`、
`background`、プロバイダー私有の拡張……）は、書いたままの形で上流に届きます。

## 上流方言

- **OpenAI 互換プロバイダー**はそのまま透過されます。
- **DashScope プロバイダー**（ベース URL が `dashscope.aliyuncs.com`、
  `dashscope-intl.aliyuncs.com`、または Model Studio ワークスペースドメイン
  `{workspaceId}.{region}.maas.aliyuncs.com`）はネイティブのマルチモーダル生成
  エンドポイント経由で応答します: リクエストはその方言の形状に再エンコードされ
  （サイズ区切り `1024x1024` は `1024*1024` に）、レスポンスは OpenAI 形状に
  デコードされます。DashScope は**URL のみ**で応答します——
  `response_format: "b64_json"` リクエストは、その理由を試行行に記録したうえで
  それらの候補では拒否されます。
- OpenAI 互換ルートでは、`qwen-image-*`、`wanx-*`、`wan2.*` ファミリのモデルに
  対してサイズ区切りが自動変換されます。

## 課金

候補が宣言した課金モードに従います:

- **1 枚ごと** —— 品質×サイズの価格表（任意のデフォルト価格付き）が単価を解決し、
  **実際に納品された**枚数を掛けます（4 枚要求して 2 枚受け取れば 2 枚分の請求）。
  どの行にも一致せずデフォルトもない価格表は、リクエストを未価格のままにします
  ——不明として記録され、無料とは異なります。
- **トークンごと** —— 上流が報告したトークン数を、候補の 100 万単価で計算します。

失敗したリクエスト、または 1 枚も納品しなかった HTTP 200 応答は課金されません。
価格付けされた各行は、何によって価格付けされたかのスナップショット（リクエスト
軸、要求数と納品数、単価）をリクエストログに携えて記録されます。

## 制限

- `stream: true` は明確な 400 で拒否されます——漸進的画像ストリーミングは
  サポートされていません。
- `POST /v1/images/edits` はまだ提供されていません。
- 返される URL（とその有効期限）は上流に属します。このゲートウェイは画像バイトを
  再ホストしません。
