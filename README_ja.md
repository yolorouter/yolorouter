<div align="center">

# Yolorouter

**Claude Code（および任意の AI CLI）をどのプロバイダーでも動かす——無料・セルフホストの LLM ゲートウェイ。単一バイナリが 4 つのチャット線形プロトコルと OpenAI Images / Videos API を話し、プロバイダー横断でフェイルオーバーし、上流キーをプールし、マルチユーザー管理コンソール同梱。**

[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![CI](https://github.com/yolorouter/yolorouter/actions/workflows/ci.yml/badge.svg)](https://github.com/yolorouter/yolorouter/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/yolorouter/yolorouter)](https://goreportcard.com/report/github.com/yolorouter/yolorouter)
[![Release](https://img.shields.io/github/v/release/yolorouter/yolorouter?sort=semver)](https://github.com/yolorouter/yolorouter/releases)
[![Go](https://img.shields.io/badge/go-1.25.7+-00ADD8.svg)](go.mod)

[English](README.md) · [简体中文](README_zh.md) · 日本語

[クイックスタート](#クイックスタート) · [プロトコル](#プロトコル) · [コスト最適化](#コスト最適化) · [ドキュメント](#ドキュメント) · [コントリビュート](#コントリビュート)

⚡ **低オーバーヘッドのストリーミングプロキシ** · 🔀 **任意のプロトコル IN、任意のプロトコル OUT** · 🆓 **無料・オープンソース** · 📦 **単一バイナリ、外部依存ゼロ** · 🔁 **自動フェイルオーバー + キープール** · 👥 **SSO 付きマルチユーザー** · 💰 **コスト分析と最適化**

</div>

---

アプリケーションは**1 つ**のエンドポイントと**1 つ**の API キーだけを指します。
Yolorouter はアプリと上流プロバイダーの間に立ち、面倒な部分を——プロバイダーアカウントの
使い分け、レート制限されたキーのローテーション、アカウント故障時のフェイルオーバー、
キーごとの予算強制、そして「すべてがいくらかかるか」の把握——コードベースのあちこちに
散らばさせるのではなく、1 ヵ所に集めます。

**4 つの線形プロトコル**（OpenAI Chat Completions、OpenAI Responses、Anthropic Messages、
Gemini `generateContent`）を受け付け、出口でどの組み合わせにも変換できます。OpenAI しか
話さないプロバイダーが Claude Code に応答し、Anthropic しか話さないプロバイダーが
OpenAI SDK に応答できます。ストリーミング、ツール呼び出し、reasoning/thinking ブロックは
すべて旅を生き延び、Responses 以外の全入口で画像コンテンツも同様です
（[プロトコル](#プロトコル)参照）。

すべては Web コンソールを組み込んだ**単一バイナリ**として動きます。Node ランタイムの
インストールも、フロントエンドの別デプロイも不要です。SQLite がすぐ使え、
必要になったら PostgreSQL に切り替えられます。

## Yolorouter を選ぶ理由

**ルーティング**

- **マルチプロバイダーフェイルオーバー。** 1 つの公開モデル名（例: `smart`）を順序付きのプロバイダー候補リストにマッピング。1 社が落ちるとリクエストは次へフェイルオーバーします。呼び出し側が別のモデル名を見ることはありません。
- **モデルごとのスケジューリングモード: failover / balanced。** デフォルトの failover はプライマリ優先——先頭候補が全トラフィックを受け持ちます。モデルを balanced に切り替えると、呼び出し側 API キーがプロバイダー間に均等に分散され、各キーは 1 社に貼り付くため上流のプロンプトキャッシュが温かく保たれます。[スケジューリングモード](#スケジューリングモード)を参照。
- **上流キープール。** 各プロバイダーに上流キーのプールを与えると、負荷がラウンドロビンで分散します。レート制限されたキーは `Retry-After` ウィンドウの間ローテーションから外され（以降のリクエストはより健康なキーを優先）、未認証・枠枯渇キーは再テスト合格まで運用から外れます。
- **ワンクリックモデルインポート。** プロバイダーを追加すると、ゲートウェイがそのライブモデルカタログを取得します——欲しいモデルにチェックを入れて一括インポート。インポートされたすべてのマッピングはバックグラウンドで実際の上流に対して検証され、合格すると自動有効化。失敗は診断情報を保持し、ワンクリックで再テストできます。提案価格は毎日更新されるコミュニティ運営の[価格カタログ](https://github.com/yolorouter/price-catalog)由来で、たいていのモデルは価格付きで届きます。
- **モデルエイリアス。** 呼び出し側は安定した公開名を要求し、各プロバイダー候補がそれをそのプロバイダーが実際に期待するモデル ID に対応付けます。候補マッピングは保存時に実際の上流へプローブされるため、タイプミスは深夜 3 時ではなく設定時に判明します。
- **画像・動画生成。** OpenAI Images と Videos API がチャットの隣で提供されます。画像は品質×サイズの価格表で 1 枚ごとに課金（またはトークン単価）。動画はジョブ方言です——`POST /v1/videos` がポーリング可能なジョブを返し、完了を初めて観測した時点で解像度階層表に照らして秒単位で決済し、キーの予算は仕掛かり中ジョブの価格上限をすべて抑えます。ネイティブメディア API が OpenAI 形状でないプロバイダーとはネイティブ方言で対話します: DashScope（wan 動画、qwen-image）、火山エンジン Ark（Seedance 動画、Seedream 画像）、Kling（`kling-3.0` 系動画、`kling-v3` 画像、複数参照の Omni ペア——`image_list` のような呼び出し側 kling ネイティブフィールドはそのまま通します）、MiniMax（V2 タスク API 経由の `MiniMax-H3` / `MiniMax-H3-Max` 動画——テキストから動画と最初のフレーム画像からの動画に対応。H3-Max は 5〜15 秒のクリップを生成するため、4 秒の指定はそのモデルでは拒否されます。同一ホストがチャット方言も提供するため、チャットプローブがそのモデルを検証できないときはキー検証が動画プローブにフォールバックします）。メディア専用アカウントのキーは、チャットプローブがそのモデルを検証できないとき、実際のメディアプローブで検証されます。
- **ビジョンフォールバック。** テキスト専用モデルに「目」を与えます。コンソールでモデルを画像非対応とマークし、ビジョンモデルを選択。侵入リクエスト内の画像はビジョンモデルによって記述され、テキストとして転送されます——呼び出し側には透過的で、全入口プロトコルで機能します。ビジョンモデル未設定の場合、画像は上流エラーではなく明確なプレースホルダーに格下げされます。
- **正しいストリーミング。** キーローテーションとフェイルオーバーは最初のバイトがクライアントに届く**前に**完了し、ストリーミング開始後はプロバイダーが確定します。2 社のコンテンツが 1 つのレスポンスに継ぎ合わされることはありません。
- **推論モデル向けのタイムアウト。** 1 つの壁時計ではなく、独立して設定可能な 7 フェーズ。トークンを吐き出す前に 8 分考えるモデルが思考の途中で殺されることはありません。

**制御とコスト**

- **キーごとのアクセス制御。** モデル許可リスト、レートと同時実行の制限、累積予算上限、任意の有効期限、即時失効。
- **SSO 付きマルチユーザー。** チームメンバーは任意の OAuth2/OIDC プロバイダー（Zitadel、GitHub、Keycloak、DingTalk、Feishu など——[DingTalk / Feishu セットアップガイド](docs/dingtalk-feishu-login.md)参照）でログインして初回ログイン時にアカウントが作られるか、管理者がコンソールからローカルのユーザー名/パスワードアカウントを直接作成します——どちらの道でも招待は不要です。メンバーは自分の API キーをセルフサービスで管理し、自分の使用量とコストだけを見ます。管理者はすべてを見渡せ、あらゆる統計をアカウントで絞り込み、アカウントの作成・昇格・降格・無効化ができます。アカウントを無効化すると即座にサインアウトさせ、そのキーすべてが即時に切れます。
- **コスト最適化。** カスタムシステムプロンプトをグローバルまたはキーごとに注入。肥大したツール出力を上流に届く前に圧縮します。コンソールは圧縮の実測節約額と、システムプロンプトについて公開ベンチマークに裏付けられた期間内の予測節約コストと出力トークンを報告します。
- **組み込みの可観測性。** トークンとコストの KPI、モデル/プロバイダー/時間/アカウント/キー別の使用量、試行ごとの完全なルーティングチェーン付きリクエストログ。どのビューも CSV エクスポート可能。
- **バイリンガルコンソール。** 英語と簡体字中国語、どこでも切り替え可能。タイムゾーンはブラウザに追従。
- **セルフアップデート。** バイナリが新しいリリースの確認と適用を行えます。

## スクリーンショット

<div align="center">
  <img src="docs/screenshots/dashboard.png" alt="ダッシュボード" width="49%" />
  <img src="docs/screenshots/analytics.png" alt="分析" width="49%" />
</div>

## クイックスタート

### Docker

```bash
docker run -d --name yolorouter --restart unless-stopped \
  -p 8080:8080 -v "$PWD/yolorouter:/yolorouter" \
  ghcr.io/yolorouter/yolorouter:latest
```

または [docker-compose.yml](docker-compose.yml) を取得して `docker compose up -d`。
イメージは毎リリース amd64 と arm64 向けに公開されます。

コンテナが書き込むすべては、マウントされた 1 つのフォルダに存在します: 生成される
`configs/config.yaml`（上流キーを暗号化する鍵を含む）と SQLite データベース。
そのフォルダをバックアップすれば、デプロイ全体をバックアップしたことになります。

**docker compose でのアップグレード:**

```bash
docker compose pull   # 最新イメージを取得。稼働中のコンテナには触れない
docker compose up -d  # 新イメージでコンテナを作り直す（最新なら何もしない）
```

**素の `docker run` でのアップグレード**は 3 ステップ。コンテナのファイルシステムは
設計上使い捨てなのでこれは安全です——状態は何一つコンテナの中に置かれず、設定も
データベースもホスト側のマウントフォルダにあり、コンテナを削除しても生き延びます。

```bash
# 1. 最新イメージを取得。実行中のコンテナはそのままサービスを続けます——
#    このステップはローカルのイメージストアにバイトを取得するだけです。
docker pull ghcr.io/yolorouter/yolorouter:latest

# 2. 古いコンテナを停止して削除。あなたのデータはその中にはありません:
#    すべてはホスト側のマウントフォルダにあり、そのまま残ります。
docker rm -f yolorouter

# 3. 新しいコンテナを起動——初回とまったく同じコマンドで、同じフォルダを
#    マウントします。ステップ 1 で取得したイメージが使われます。
docker run -d --name yolorouter --restart unless-stopped \
  -p 8080:8080 -v "$PWD/yolorouter:/yolorouter" \
  ghcr.io/yolorouter/yolorouter:latest
```

知っておくべき 2 つの細部:

- ステップ 3 は**当初コンテナを起動したのと同じディレクトリから**実行してください——
  `-v "$PWD/yolorouter:/yolorouter"` のマウントはカレントディレクトリ基準で解決され、
  別ディレクトリは空のデータフォルダと真っさらなセットアップ画面を意味します。
  `-v` に絶対パスを使えばこの落とし穴は完全に回避できます。
- 新バージョンは初回起動時に未適用のデータベースマイグレーションを自動適用してから、
  以前と同様にサービスを始めます。データベースが移行された**後**に旧バージョンへ
  戻す必要が出たら、旧イメージを起動するのではなく、アップグレード前のバックアップから
  データフォルダを復元してください——古いバイナリは新しいスキーマを理解しない可能性が
  あります。そもそも固定バージョンに留まりたい場合は `:latest` ではなく
  ピン留めタグ（例: `...:v0.1.6`）を使ってください。

### システムサービスとしてインストール

Docker なし、または内蔵セルフアップデータを使いたい場合? ブート時に起動する
バックグラウンドサービスとしてインストールできます: Linux では systemd、macOS では
launchd、Windows ではスケジュールタスク。

```bash
# Linux / macOS
curl -fsSL https://get.yolorouter.com/install.sh | bash
```

```powershell
# Windows、PowerShell 5.1+
irm https://get.yolorouter.com/install.ps1 | iex
```

Windows では、昇格した PowerShell がブート時に起動するシステム全体のサービスを、
通常の PowerShell はアカウント配下・ログオン時に起動するものをインストールします。

> **🇨🇳 中国ミラー**: GitHub が遅い・到達できないネットワークでは、`get.yolorouter.com`
> を `gh.yolorouter.com` に置き換えてください。同じインストーラが Cloudflare プロキシ経由で
> 届き、自動更新も以後ミラーを使い続けます。

アップグレードは同じコマンドの再実行です。設定とデータベースは保持され、データベースは
先にバックアップされます。素のバイナリがよければ [リリース](https://github.com/yolorouter/yolorouter/releases)
から取得して `./yolorouter serve` を実行してください（Windows では `.\yolorouter.exe serve`）。

### 初回起動

起動方法を問わず、初回実行で `configs/config.yaml` が生成され、マイグレーションが適用され、
8080 番ポートでコンソールが始まります。最初の管理者アカウントを作成したら、ガイド付きの
フローに従ってください: 上流キー付きのプロバイダーを追加——コンソールがそのプロバイダーの
モデルカタログを取得するので、欲しいモデルをワンクリックでインポートできます。インポート
された各モデルはバックグラウンドで実際の上流に対して検証され、合格すると自動的に有効化
されます。最後に API キーを発行して呼び出し開始です。

→ **ソースからのビルドを含む全プラットフォームの完全なインストールガイド:**
[yolorouter.com/docs/self-hosted/installation](https://yolorouter.com/docs/self-hosted/installation?utm_source=oss-readme&utm_medium=repo)

## プロトコル

以下のすべての入口は**同じ** Yolorouter API キーで認証し、ストリーミングをサポートし、
そのプロバイダーがネイティブに話すプロトコルが何であれ、設定された**任意の**
プロバイダーから応答できます。

| 入口ルート | プロトコル | 受け付ける認証ヘッダー |
| --- | --- | --- |
| `POST /v1/chat/completions` | OpenAI Chat Completions | `Authorization: Bearer`、`X-Api-Key` |
| `POST /v1/responses` | OpenAI Responses | `Authorization: Bearer`、`X-Api-Key` |
| `POST /v1/messages` | Anthropic Messages | `Authorization: Bearer`、`X-Api-Key` |
| `POST /v1/images/generations` | OpenAI Images（生成） | `Authorization: Bearer`、`X-Api-Key` |
| `POST /v1/images/edits` | OpenAI Images（編集） | `Authorization: Bearer`、`X-Api-Key` |
| `POST /v1/videos`、`GET /v1/videos/{id}`、`GET /v1/videos/{id}/content` | OpenAI Videos（ジョブ方言） | `Authorization: Bearer`、`X-Api-Key` |
| `POST /v1/audio/speech` | OpenAI Speech | `Authorization: Bearer`、`X-Api-Key` |
| `POST /v1beta/models/{model}:generateContent`<br>`POST /v1beta/models/{model}:streamGenerateContent` | Gemini | `x-goog-api-key`、`?key=`、`Authorization: Bearer`、`X-Api-Key` |
| `GET /v1/models`、`GET /v1/models/{model}` | モデル一覧 | `Authorization: Bearer`、`X-Api-Key` |

画像入口は、コンソールで出力モダリティが**画像**と宣言されたモデルを提供します。
OpenAI 互換プロバイダーはそのまま透過され、DashScope または Kling ホストの
プロバイダーはネイティブタスク方言で応答します（OpenAI 形状で同期的に返ります。
URL のみ——`b64_json` リクエストは候補単位で拒否されます）。画像モデルは、候補の
宣言に従い、品質×サイズの価格表による 1 枚ごと課金かトークン数課金のどちらかで
決済され、1 枚も納品しなかったリクエストは課金されません。編集は OpenAI の
マルチパートアップロードを受け付けます。DashScope ホストでは参照画像がネイティブ
方言に再エンコードされ（マスクのアップロードはそこでは拒否——その方言にはフィールドが
ありません）、`gpt-image-*` モデルは名前付き SSE イベントとして漸進的な部分画像を
ストリーミングします。

動画入口はジョブ方言です: `POST /v1/videos` が生成を投入してジョブリソースを返し、
呼び出し側は `GET /v1/videos/{id}` でポーリングし、`GET /v1/videos/{id}/content` から
完成クリップをダウンロードします。OpenAI 公式 SDK がそのまま動きます
（`create_and_poll` がループを回します）。決済は完了を初めて観測したときに一度だけ
行われ、リクエストのサイズが対応する解像度階層に照らして、上流が実際に納品した秒数に
対して課金します——失敗・キャンセル・期限切れのジョブは課金されません。動画上流は
タスク方言です（DashScope wan、Ark Seedance、Kling 新設計エンドポイント、MiniMax V2）。
投入済みジョブが別の候補に再投入されることはありません——受理されたタスクは、呼び出し
側が課金されるかどうかにかかわらず運用者のコストでレンダリングされるからです。
MiniMax の注記: `MiniMax-H3-Max` は 5〜15 秒のクリップを受け付け（4 秒の指定はその
理由付きで拒否）、最大出力は 768P にとどまる一方、`MiniMax-H3` は大きなドアサイズに
乗って 2K まで出ます。タスク照会は 7 日間応答可能です——それを過ぎた未完了ジョブは
課金されずに期限切れになります。完成クリップのリンクは時間制限あり（ベンダーは有効
期間を明示していません）なので、速やかにダウンロードまたは再ホストしてください。
MiniMax の動画生成は従量課金残高から支払われます（Token Plan サブスクリプション、
クレジットパック、Hailuo 動画リソースパックは H3 モデルをカバーしません）。

音声入口は出力モダリティが**音声**と宣言されたモデルを提供します: OpenAI 形状の
JSON リクエスト 1 つ——`model`、`input`、`voice` が必須、任意の `response_format`
（`mp3`、`opus`、`aac`、`flac`、`wav`、`pcm`）と `speed`——に対してバイナリ音声を、
到着順に転送して返します。たいていのベースは OpenAI speech 形状そのもので話しかけ
られ、3 つのホストが独自方言を持ちます——SiliconFlow（`mp3/opus/wav/pcm`、デフォルト
は `mp3`、入力の UTF-8 バイト単位で課金）、智谱 Zhipu（`wav/pcm` のみ、未指定の
呼び出し側へのデフォルトは `wav`）、MiniMax（`t2a_v2` エンドポイント: `mp3/pcm/wav/
flac/opus` でデフォルト `mp3`、音声と速度は `voice_setting` 内、音声は JSON 封筒に
hex エンコードで届きゲートウェイが復号）。候補は計上文字 100 万単位で、決済する
プロバイダー自身の計数ルールで課金されます——MiniMax では封筒自身の
`usage_characters` が存在すればそれが請求額を決めます——そのため同一モデルでも
プロバイダー候補が異なれば同じテキストの計量が変わることがあります。リクエスト
ログの用量詳細が、各請求が使った計量器を明示します。音声リクエストが別プロバイダーへ
フェイルオーバーすることはありません: 音色は呼び出し側自身の選択であり、音色は
ベンダー間を移らないため、失敗は呼び出し側が対処できるエラーであって、別の声では
ありません。入力長の上限は上流側です（MiniMax 10,000 文字、智谱 1,024）で、上流
自身のエラーで応答されます。`instructions` と `stream_format` は入口で拒否されます
——実装済みの方言はどれも対応せず、呼び出し側が設定したフィールドを黙って落とせば
効いたと信じさせることになるからです。

すべてのリクエストにおける `model` は、あなたが設定した**公開名**です。Yolorouter が
プロバイダー候補を選び、実際の上流モデル ID を差し込み、レスポンスではあなたの公開名を
保ちます。

> **既知の制限**: Responses 入口の `input_image` は、リクエストが別の出口プロトコルへ
> 変換されるときに落とされます。転送されるのはテキストのみです。同プロトコルの
> 透過は影響を受けず、画像コンテンツは他の 3 つの入口で正しく変換されます。
>
> **メディア注記**: 画像の `stream` は `gpt-image-*` ファミリの機能です——他の
> ファミリはストリーミング要求に 400 で応えます。返される画像・動画 URL は上流由来で、
> 上流の有効期限に従います（Yolorouter はプロキシするだけで再ホストしません）。
> 動画ジョブにキャンセル面はありません——接続済みのタスク方言はどれも露出させて
> いません。

### 既存の SDK とツールを向ける

入口が本物のネイティブプロトコルなので、公式 SDK とエージェントツールは 2 つの設定を
変えるだけで、アダプター層は不要です:

```python
# OpenAI Python SDK
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8080/v1", api_key="sk-yr-your-key")
print(client.chat.completions.create(
    model="smart",
    messages=[{"role": "user", "content": "Hello!"}],
).choices[0].message.content)
```

```bash
# Claude Code — Yolorouter 経由で設定した任意のプロバイダーへルーティング
export ANTHROPIC_BASE_URL=http://localhost:8080
export ANTHROPIC_AUTH_TOKEN=sk-yr-your-key
claude
```

→ **プロトコル別のリクエスト例と 19 のエージェントツールのセットアップガイド**
（Claude Code、Cursor、Codex CLI、Cherry Studio、Gemini CLI、opencode など）:
[yolorouter.com/docs](https://yolorouter.com/docs?utm_source=oss-readme&utm_medium=repo)

## スケジューリングモード

すべてのモデルは順序付きプロバイダー候補リストを経由してルーティングされ、その
スケジューリングモードがリクエストが最初に入る候補を決めます。

- **Failover**（デフォルト）はプライマリ優先: 設定した順序の先頭が全トラフィックを
 受け持ち、残りのチェーンはそれが故障したときのためのものです。切り替えるまで、
 すべてのモデルはこの方式で動きます。
- **Balanced** は呼び出し側 API キーをプロバイダー間に均等に広げます。各キーは現在
  最もキーの少ないプロバイダーに割り当てられた後、そこに貼り付きます: 1 つのキーから
  のマルチターン会話は同じプロバイダーに当たり続けるため、上流のプロンプトキャッシュが
  温かく保たれます——会話の途中でプロバイダーを渡り歩けば、キャッシュされたすべての
  トークンを再課金することになります。（ゲートウェイが保証するのはプロバイダー親和性
  です。キャッシュヒットが続くかはプロバイダーのキープールにも依存します——異なる
  上流アカウントの上流キーはキャッシュを共有しません。）紐付いたプロバイダーが
  サーキットブレーカーを踏むと、障害中にリクエストを送ったキーは別の場所に再紐付け
  されます（休眠中のキーは次に呼ぶまで古い場所を保持）。回復したプロバイダーは
  より少ない紐付けを抱え、新しい割り当てを最初に引き寄せるため、リバランサなしで
  分布が自然に治ります。モデル詳細ページは現在のプロバイダー別紐付け数を表示します
  （ページ読み込み時に更新されるある時点のスナップショット）。

それ以外——障害処理、キーローテーション、サーキットブレーキング、予算——は
両モードで同一です。

**既知の制限:** 紐付けはプロセスメモリ上にあります。再起動はキーを割り当て直すだけで
（同じ均等分布に収束します）、マルチインスタンスデプロイでは各インスタンスが自分の
分布を計算します——インスタンス横断の紐付けテーブルはありません。スケジューリング
モード以前のバージョンからのローリングアップグレード中、未アップグレードの
インスタンスはすべてのモデルを failover で実行します——モデルを balanced に切り替える
のは全機体のアップグレード後にしてください。紐付けテーブルは全モデルで (キー, モデル)
ペアを最大 4096 保持します。それを超えると最も長く使われていない紐付けが追い出され、
そのキーは次のリクエストで再割り当てされるため、極端に広いデプロイ（何百ものキー ×
何十もの balanced モデル）では周縁で粘着性が若干失われます。

## コスト最適化

2 つの機能はどちらもデフォルトでオフ、コンソールでグローバルに設定し、API キー
ごとに上書きできます。

**カスタムシステムプロンプト注入。** クライアントコードに触れずに、すべてのリクエスト
のシステムプロンプントにハウスルールを追記します。注入は呼び出し側自身のプロトコル
形状に従い決定的なので、繰り返しのリクエストはバイト同一のシステムコンテンツを
生成し、上流のプロンプトキャッシュにきちんとヒットします。この機能のコンソール上の
予測節約額は、公開されたオン/オフ対抗ベンチマークに裏付けられています——手法と
150 組すべての生測定値は
[docs/concise-output-benchmark_ja.md](docs/concise-output-benchmark_ja.md) にあります。

**入力圧縮。** コーディングエージェントは巨大で高度に冗長なツール出力を送り返します。
Yolorouter は各コンテンツブロックが何か（`go test` 出力、git diff、grep 結果、通常の
ログ）を認識し、シグナルを保ったままノイズを剥ぎます: 失敗、スタックトレース、個々の
ユニークなマッチはすべて生き延びます。会話の末尾にあるアクティブな編集領域には決して
触れず、圧縮形が実際に短くなるときにだけブロックを置き換えます。

キャッシュ読みとキャッシュ書きのトークンはダッシュボード全体で別々に計量・価格付け
されるため、プロンプトキャッシュの節約は感覚ではなく数値として見られます。

→ **詳細とチューニング:**
[yolorouter.com/docs/self-hosted/configuration](https://yolorouter.com/docs/self-hosted/configuration?utm_source=oss-readme&utm_medium=repo)

## ドキュメント

| トピック | リンク |
| --- | --- |
| インストール（全プラットフォーム、ソースから） | [Installation](https://yolorouter.com/docs/self-hosted/installation?utm_source=oss-readme&utm_medium=repo) |
| `config.yaml` の全フィールドと CLI | [Configuration](https://yolorouter.com/docs/self-hosted/configuration?utm_source=oss-readme&utm_medium=repo) |
| アップグレード、ロールバック、アンインストール | [Updating](https://yolorouter.com/docs/self-hosted/updating?utm_source=oss-readme&utm_medium=repo) |
| レイヤリング、プロトコル IR、ストレージ | [Architecture](https://yolorouter.com/docs/self-hosted/architecture?utm_source=oss-readme&utm_medium=repo) |
| API リファレンスとモデルカタログ | [ドキュメントホーム](https://yolorouter.com/docs?utm_source=oss-readme&utm_medium=repo) |
| DingTalk / Feishu ログイン設定 | [DingTalk / Feishu login](docs/dingtalk-feishu-login.md) |

セルフホストとは、自分の上流 API キーを持ち込むことです。プロバイダーごとに個別登録
したくない場合は、**YoloRouter Cloud** がコンソールのプロバイダープリセットリストに選択
できる上流の 1 つとして同梱されています。[ホステッドオプション](https://yolorouter.com/pricing?utm_source=oss-readme&utm_medium=repo)
を参照してください。

## ソースからビルド

**Go 1.25.7+** と **Node.js 22.12+** が必要です。

```bash
make build          # バックエンドのみ -> ./bin/yolorouter
make build-embed    # コンソール組み込みの完全バイナリ
```

### 開発とデバッグ

1 つのスクリプトがすべてを再ビルドし、マイグレーションを実行し、ローカルサーバーを
再起動します:

```bash
./scripts/dev.sh          # 完全再ビルド + http://localhost:8080 で再起動
./scripts/dev.sh --backend    # Go 変更のみ。コンソール変更は --frontend
tail -f logs/server.log   # サーバーログ——デバッグの最初の見どころ
```

設定は `configs/config.yaml` に、SQLite データベースは `data/yolorouter.db` に、
どちらも初回実行時に作成されます。リクエストレベルのデバッグでは、コンソールのリクエスト
ログ詳細ページが各リレーの完全なクライアント/上流ボディと試行ごとのルーティング
チェーンを表示します。

フロントエンドの作業では、再ビルドのループを完全にスキップできます。Vite が 5173 番
ポートでホットリロード付きのコンソールを提供し、`/api` と `/v1` をバックエンドに
プロキシします:

```bash
cd frontend && npm run dev
```

`make test` が Go テストを、`make gates` が CI が強制する構造チェックを実行します。
Windows スクリプト（`scripts/dev.ps1`）、lint、クロスコンパイルターゲットは
[CONTRIBUTING.md](CONTRIBUTING.md#local-development-and-debugging) に記載されています。

## コントリビュート

Issue とプルリクエストを歓迎します。まず [CONTRIBUTING.md](CONTRIBUTING.md) と
[行動規範](CODE_OF_CONDUCT.md)をお読みください。セキュリティ報告については
[SECURITY.md](SECURITY.md) を参照してください。

## ライセンス

[Apache License 2.0](LICENSE) の下でライセンスされます。
