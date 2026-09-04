// frontend/src/utils/apiExamples.ts
//
// Pure generator for the API examples the access-info modal shows: turn the
// gateway's public base URL (and an optional real credential) into the full
// catalog of copy-ready samples, grouped by entry protocol and capability.
// No Vue, no fetching — every URL, path, header and placeholder a sample
// carries is plain data here, which is what lets unit tests pin the samples'
// correctness without mounting anything.
//
// Credentials: a real key is passed only where its plaintext is already on
// screen (the create-key dialog's one-time step); everywhere else the
// <API Key> placeholder stands in. Like the <model> family of placeholders,
// it is a property of the sample rather than of the caller, and samples stay
// untranslated by code-sample convention.

export type ExampleProtocol = 'openai' | 'anthropic' | 'gemini'
export type ExampleLanguage = 'curl' | 'python' | 'node' | 'go'

// The chat group also ships Go; every other group serves these three.
type CoreLanguage = Exclude<ExampleLanguage, 'go'>

const CORE_LANGUAGES: CoreLanguage[] = ['curl', 'python', 'node']

export interface ExampleSnippet {
  kind: 'request' | 'response'
  // SSE flavour of the request or response.
  streaming: boolean
  // Which part of a flow or which variant of the sample this is: the video
  // lifecycle steps (submit / poll / download), or the function-calling
  // variant of a Responses request. Single-variant groups leave it out.
  tag?: 'submit' | 'poll' | 'download' | 'tools'
  code: string
}

export interface LanguageExamples {
  language: ExampleLanguage
  snippets: ExampleSnippet[]
}

export interface ExampleGroup {
  // Stable id the modal keys its tabs on and tests assert against.
  id: string
  // Which base-URL row of the access panel owns this group; the row's
  // example button opens the modal straight at the first group of its
  // protocol.
  protocol: ExampleProtocol
  languages: LanguageExamples[]
}

export interface ExampleCatalogOptions {
  endpoint: string
  key?: string
}

// The option pair travels as one context through every builder below — each
// new group lands with the same single parameter instead of re-threading
// (endpoint, key) through another signature chain.
type SampleContext = ExampleCatalogOptions

const API_KEY_PLACEHOLDER = '<API Key>'
const MODEL_PLACEHOLDER = '<model>'
const IMAGE_MODEL_PLACEHOLDER = '<image model>'
const VIDEO_MODEL_PLACEHOLDER = '<video model>'

// The protocol base-URL shapes of the access panel, in one place so the
// panel rows, the samples, and the tests all agree on how each ecosystem's
// address is derived from the bare endpoint.
export function openAIBaseUrlOf(endpoint: string): string {
  return `${endpoint}/v1`
}

export function geminiBaseUrlOf(endpoint: string): string {
  return `${endpoint}/v1beta`
}

function chatUrl(endpoint: string): string {
  return `${openAIBaseUrlOf(endpoint)}/chat/completions`
}

function cred(key: string | undefined): string {
  return key ?? API_KEY_PLACEHOLDER
}

// --- OpenAI-compatible chat completions ----------------------------------
//
// Sample bodies mirror the docs site's SDK page so the console and the docs
// never teach two different shapes.

function curlChatRequest(ctx: SampleContext, streaming: boolean): string {
  const { endpoint, key } = ctx
  const body = streaming
    ? `{
    "model": "${MODEL_PLACEHOLDER}",
    "messages": [{"role": "user", "content": "Count to 5."}],
    "stream": true
  }`
    : `{
    "model": "${MODEL_PLACEHOLDER}",
    "messages": [
      {"role": "system", "content": "You are a helpful assistant."},
      {"role": "user", "content": "Hello!"}
    ]
  }`
  return `curl ${chatUrl(endpoint)} \\
  -H "Authorization: Bearer ${cred(key)}" \\
  -H "Content-Type: application/json" \\
  -d '${body}'`
}

function pythonChatRequest(streaming: boolean): string {
  if (streaming) {
    return `stream = client.chat.completions.create(
    model="${MODEL_PLACEHOLDER}",
    messages=[{"role": "user", "content": "Count to 5."}],
    stream=True,
)
for chunk in stream:
    print(chunk.choices[0].delta.content or "", end="", flush=True)`
  }
  return `response = client.chat.completions.create(
    model="${MODEL_PLACEHOLDER}",
    messages=[
        {"role": "system", "content": "You are a helpful assistant."},
        {"role": "user", "content": "Explain quantum computing in one sentence."},
    ],
    max_tokens=200,
)
print(response.choices[0].message.content)`
}

function pythonSdkSetup(ctx: SampleContext): string {
  const { endpoint, key } = ctx
  return `from openai import OpenAI

client = OpenAI(
    api_key="${cred(key)}",
    base_url="${openAIBaseUrlOf(endpoint)}",
)`
}

function nodeChatRequest(streaming: boolean): string {
  if (streaming) {
    return `const stream = await client.chat.completions.create({
  model: '${MODEL_PLACEHOLDER}',
  messages: [{ role: 'user', content: 'Count to 5.' }],
  stream: true,
})
for await (const chunk of stream) {
  process.stdout.write(chunk.choices[0]?.delta?.content ?? '')
}`
  }
  return `const response = await client.chat.completions.create({
  model: '${MODEL_PLACEHOLDER}',
  messages: [
    { role: 'system', content: 'You are a helpful assistant.' },
    { role: 'user', content: 'Explain quantum computing in one sentence.' },
  ],
  max_tokens: 200,
})
console.log(response.choices[0].message.content)`
}

function nodeSdkSetup(ctx: SampleContext): string {
  const { endpoint, key } = ctx
  return `import OpenAI from 'openai'

const client = new OpenAI({
  apiKey: '${cred(key)}',
  baseURL: '${openAIBaseUrlOf(endpoint)}',
})`
}

function goChatRequest(streaming: boolean): string {
  if (streaming) {
    return `stream := client.Chat.Completions.NewStreaming(context.Background(),
    openai.ChatCompletionNewParams{
        Model: "${MODEL_PLACEHOLDER}",
        Messages: []openai.ChatCompletionMessageParamUnion{
            openai.UserMessage("Count to 5."),
        },
    },
)
for stream.Next() {
    for _, choice := range stream.Current().Choices {
        fmt.Print(choice.Delta.Content)
    }
}
if err := stream.Err(); err != nil {
    panic(err)
}`
  }
  return `resp, err := client.Chat.Completions.New(context.Background(),
    openai.ChatCompletionNewParams{
        Model: "${MODEL_PLACEHOLDER}",
        Messages: []openai.ChatCompletionMessageParamUnion{
            openai.SystemMessage("You are a helpful assistant."),
            openai.UserMessage("Hello!"),
        },
    },
)
if err != nil {
    panic(err)
}
fmt.Println(resp.Choices[0].Message.Content)`
}

function goSdkSetup(ctx: SampleContext): string {
  const { endpoint, key } = ctx
  return `client := openai.NewClient(
    option.WithAPIKey("${cred(key)}"),
    option.WithBaseURL("${openAIBaseUrlOf(endpoint)}"),
)`
}

function goChatImports(): string {
  return `import (
    "context"
    "fmt"

    "github.com/openai/openai-go"
    "github.com/openai/openai-go/option"
)`
}

// The wire shape is the same whatever language asked for it, so the response
// samples are shared verbatim across every language tab — each tab stays
// self-contained (request and what comes back in one place) at the cost of
// some repeated data.
const CHAT_RESPONSE = `{
  "id": "chatcmpl-...",
  "object": "chat.completion",
  "model": "${MODEL_PLACEHOLDER}",
  "choices": [
    {
      "index": 0,
      "message": {"role": "assistant", "content": "Hello! How can I help you today?"},
      "finish_reason": "stop"
    }
  ],
  "usage": {"prompt_tokens": 20, "completion_tokens": 9, "total_tokens": 29}
}`

const CHAT_STREAM_RESPONSE = `data: {"id":"chatcmpl-...","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}

data: {"id":"chatcmpl-...","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]`

// Per-language request builders, keyed by language: a new group extends a
// record instead of growing another switch on the same type. Every request
// block is self-contained — the modal shows request and streaming side by
// side as separate copyable snippets, so each carries its own client setup
// instead of assuming the block above it.
const CHAT_REQUEST_SAMPLES: Record<
  ExampleLanguage,
  (ctx: SampleContext, streaming: boolean) => string
> = {
  curl: (ctx, streaming) => curlChatRequest(ctx, streaming),
  python: (ctx, streaming) => `${pythonSdkSetup(ctx)}\n\n${pythonChatRequest(streaming)}`,
  node: (ctx, streaming) => `${nodeSdkSetup(ctx)}\n\n${nodeChatRequest(streaming)}`,
  go: (ctx, streaming) =>
    `package main\n\n${goChatImports()}\n\nfunc main() {\n    ${goSdkSetup(ctx)}\n\n${goChatRequest(streaming)}\n}`,
}

function chatLanguage(language: ExampleLanguage, ctx: SampleContext): LanguageExamples {
  const request = (streaming: boolean): string => CHAT_REQUEST_SAMPLES[language](ctx, streaming)
  return {
    language,
    snippets: [
      { kind: 'request', streaming: false, code: request(false) },
      { kind: 'response', streaming: false, code: CHAT_RESPONSE },
      { kind: 'request', streaming: true, code: request(true) },
      { kind: 'response', streaming: true, code: CHAT_STREAM_RESPONSE },
    ],
  }
}

function chatGroup(ctx: SampleContext): ExampleGroup {
  const languages: ExampleLanguage[] = ['curl', 'python', 'node', 'go']
  return {
    id: 'openai-chat',
    protocol: 'openai',
    languages: languages.map((language) => chatLanguage(language, ctx)),
  }
}

// --- OpenAI-compatible image generation ----------------------------------
//
// Bodies mirror the docs site's self-hosted image page. Every upstream
// delivers one flavour per entry; the response sample shows both field
// positions side by side so a reader knows where the image lands either
// way, and the request samples repeat the point where they read it.

const IMAGES_RESPONSE = `{
  "created": 1759000000,
  "data": [
    {"url": "https://upstream.example.com/image.png"},
    {"b64_json": "iVBORw0KGgoAAAANSUhEUg..."}
  ]
}`

function imageResultLoop(): string {
  return `# data[] carries url or b64_json depending on the upstream.
for item in image.data:
    print(item.url or item.b64_json[:32] + "...")`
}

// --- OpenAI-compatible video jobs ----------------------------------------
//
// The Videos API is submit / poll / download; status is the four-value
// vocabulary queued / in_progress / completed / failed, and the finished
// clip is only downloadable until the job's expires_at passes.

const VIDEO_SUBMIT_RESPONSE = `{
  "id": "vid_...",
  "object": "video",
  "status": "queued",
  "model": "${VIDEO_MODEL_PLACEHOLDER}",
  "created_at": 1759000000
}`

const VIDEO_POLL_RESPONSE = `{
  "id": "vid_...",
  "object": "video",
  "status": "completed",
  "model": "${VIDEO_MODEL_PLACEHOLDER}",
  "created_at": 1759000000,
  "completed_at": 1759000120,
  "expires_at": 1759086400
}`

// --- OpenAI-compatible model listing -------------------------------------

const MODELS_RESPONSE = `{
  "object": "list",
  "data": [
    {
      "id": "${MODEL_PLACEHOLDER}",
      "object": "model",
      "owned_by": "yolorouter",
      "output_modalities": ["text"]
    },
    {
      "id": "${IMAGE_MODEL_PLACEHOLDER}",
      "object": "model",
      "owned_by": "yolorouter",
      "output_modalities": ["image"]
    }
  ]
}`

// The bread-and-butter layout: one copy-ready request per language plus
// the shared response shape.
function singleRequestGroup(
  id: string,
  protocol: ExampleProtocol,
  ctx: SampleContext,
  requests: Record<CoreLanguage, string>,
  response: string,
): ExampleGroup {
  const snippets = (request: string): ExampleSnippet[] => [
    { kind: 'request', streaming: false, code: request },
    { kind: 'response', streaming: false, code: response },
  ]
  return {
    id,
    protocol,
    languages: [
      { language: 'curl', snippets: snippets(requests.curl) },
      { language: 'python', snippets: snippets(`${pythonSdkSetup(ctx)}\n\n${requests.python}`) },
      { language: 'node', snippets: snippets(`${nodeSdkSetup(ctx)}\n\n${requests.node}`) },
    ],
  }
}

function imagesGenerationsGroup(ctx: SampleContext): ExampleGroup {
  const { endpoint, key } = ctx
  return singleRequestGroup('openai-images-generations', 'openai', ctx, {
    curl: `curl ${openAIBaseUrlOf(endpoint)}/images/generations \\
  -H "Authorization: Bearer ${cred(key)}" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "${IMAGE_MODEL_PLACEHOLDER}",
    "prompt": "a cat",
    "size": "1024x1024"
  }'`,
    python: `image = client.images.generate(
    model="${IMAGE_MODEL_PLACEHOLDER}",
    prompt="a cat",
    size="1024x1024",
)
${imageResultLoop()}`,
    node: `const image = await client.images.generate({
  model: '${IMAGE_MODEL_PLACEHOLDER}',
  prompt: 'a cat',
  size: '1024x1024',
})
// data[] carries url or b64_json depending on the upstream.
console.log(image.data[0]?.url ?? image.data[0]?.b64_json?.slice(0, 32))`,
  }, IMAGES_RESPONSE)
}

function imagesEditsGroup(ctx: SampleContext): ExampleGroup {
  const { endpoint, key } = ctx
  return singleRequestGroup('openai-images-edits', 'openai', ctx, {
    curl: `curl ${openAIBaseUrlOf(endpoint)}/images/edits \\
  -H "Authorization: Bearer ${cred(key)}" \\
  -F "model=${IMAGE_MODEL_PLACEHOLDER}" \\
  -F prompt="make the background pure white" \\
  -F size=1024x1024 \\
  -F image=@input.png`,
    python: `image = client.images.edit(
    model="${IMAGE_MODEL_PLACEHOLDER}",
    prompt="make the background pure white",
    image=open("input.png", "rb"),
)
${imageResultLoop()}`,
    node: `import fs from 'node:fs'

const image = await client.images.edit({
  model: '${IMAGE_MODEL_PLACEHOLDER}',
  prompt: 'make the background pure white',
  image: fs.createReadStream('input.png'),
})
console.log(image.data[0]?.url ?? image.data[0]?.b64_json?.slice(0, 32))`,
  }, IMAGES_RESPONSE)
}

// The video lifecycle per language, same record-driven shape as the chat
// requests: each entry builds that language's submit / poll / download
// snippets. The SDK steps continue the submit step's client and job
// variables — noted in-code — rather than repeating the setup.
const VIDEO_STEP_SAMPLES: Record<
  CoreLanguage,
  (ctx: SampleContext) => { submit: string; poll: string; download: string }
> = {
  curl: (ctx) => {
    const base = openAIBaseUrlOf(ctx.endpoint)
    return {
      submit: `curl ${base}/videos \\
  -H "Authorization: Bearer ${cred(ctx.key)}" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "${VIDEO_MODEL_PLACEHOLDER}",
    "prompt": "a lantern festival at night",
    "seconds": 4,
    "size": "720x1280"
  }'`,
      poll: `# Poll until status is completed or failed.\ncurl ${base}/videos/vid_... -H "Authorization: Bearer ${cred(ctx.key)}"`,
      download: `# Download the finished clip before expires_at passes.\ncurl ${base}/videos/vid_.../content \\\n  -H "Authorization: Bearer ${cred(ctx.key)}" \\\n  -o clip.mp4`,
    }
  },
  python: (ctx) => ({
    submit: `${pythonSdkSetup(ctx)}\n\nimport time\n\nvideo = client.videos.create(\n    model="${VIDEO_MODEL_PLACEHOLDER}",\n    prompt="a lantern festival at night",\n    seconds=4,\n    size="720x1280",\n)\nprint(video.id, video.status)`,
    poll: `# Continues from the submit step's client and video.\nwhile video.status not in ("completed", "failed"):\n    time.sleep(5)\n    video = client.videos.retrieve(video.id)\nprint(video.status)`,
    download: `content = client.videos.content(video.id)\ncontent.write_to_file("clip.mp4")`,
  }),
  node: (ctx) => ({
    submit: `${nodeSdkSetup(ctx)}\n\nconst video = await client.videos.create({\n  model: '${VIDEO_MODEL_PLACEHOLDER}',\n  prompt: 'a lantern festival at night',\n  seconds: 4,\n  size: '720x1280',\n})\nconsole.log(video.id, video.status)`,
    poll: `// Continues from the submit step's client and video.\nlet job = await client.videos.retrieve(video.id)\nwhile (job.status !== 'completed' && job.status !== 'failed') {\n  await new Promise((resolve) => setTimeout(resolve, 5000))\n  job = await client.videos.retrieve(video.id)\n}\nconsole.log(job.status)`,
    download: `import fs from 'node:fs'\n\nconst response = await client.videos.content(video.id)\nfs.writeFileSync('clip.mp4', Buffer.from(await response.arrayBuffer()))`,
  }),
}

function videoSnippets(language: CoreLanguage, ctx: SampleContext): ExampleSnippet[] {
  const { submit, poll, download } = VIDEO_STEP_SAMPLES[language](ctx)
  return [
    { kind: 'request', streaming: false, tag: 'submit', code: submit },
    { kind: 'response', streaming: false, tag: 'submit', code: VIDEO_SUBMIT_RESPONSE },
    { kind: 'request', streaming: false, tag: 'poll', code: poll },
    { kind: 'response', streaming: false, tag: 'poll', code: VIDEO_POLL_RESPONSE },
    { kind: 'request', streaming: false, tag: 'download', code: download },
  ]
}

function videosGroup(ctx: SampleContext): ExampleGroup {
  const languages = CORE_LANGUAGES
  return {
    id: 'openai-videos',
    protocol: 'openai',
    languages: languages.map((language) => ({
      language,
      snippets: videoSnippets(language, ctx),
    })),
  }
}

function modelsGroup(ctx: SampleContext): ExampleGroup {
  const { endpoint, key } = ctx
  return singleRequestGroup('openai-models', 'openai', ctx, {
    curl: `curl ${openAIBaseUrlOf(endpoint)}/models -H "Authorization: Bearer ${cred(key)}"`,
    python: `for model in client.models.list():
    print(model.id, model.output_modalities)`,
    node: `const page = await client.models.list()
for await (const model of page) {
  console.log(model.id, model.output_modalities)
}`,
  }, MODELS_RESPONSE)
}

// --- Anthropic-native chat (/v1/messages) --------------------------------
//
// The gateway takes the key in X-Api-Key — the Anthropic SDK and Claude
// Code never send Authorization — and speaks the Messages wire shape
// (max_tokens required). The SDK base URL is the bare endpoint: the SDK
// appends /v1/messages itself.

const ANTHROPIC_RESPONSE = `{
  "id": "msg_...",
  "type": "message",
  "role": "assistant",
  "model": "${MODEL_PLACEHOLDER}",
  "content": [{"type": "text", "text": "Hello! How can I help you today?"}],
  "stop_reason": "end_turn",
  "usage": {"input_tokens": 8, "output_tokens": 9}
}`

const ANTHROPIC_STREAM_RESPONSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_...","role":"assistant"}}

event: content_block_delta
data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello"}}

event: message_stop
data: {"type":"message_stop"}`

function anthropicCurlRequest(ctx: SampleContext, streaming: boolean): string {
  const body = streaming
    ? `{
    "model": "${MODEL_PLACEHOLDER}",
    "max_tokens": 200,
    "messages": [{"role": "user", "content": "Count to 5."}],
    "stream": true
  }`
    : `{
    "model": "${MODEL_PLACEHOLDER}",
    "max_tokens": 200,
    "messages": [{"role": "user", "content": "Hello!"}]
  }`
  return `curl ${ctx.endpoint}/v1/messages \\
  -H "x-api-key: ${cred(ctx.key)}" \\
  -H "anthropic-version: 2023-06-01" \\
  -H "Content-Type: application/json" \\
  -d '${body}'`
}

function anthropicPythonSetup(ctx: SampleContext): string {
  return `from anthropic import Anthropic

client = Anthropic(
    api_key="${cred(ctx.key)}",
    base_url="${ctx.endpoint}",
)`
}

function anthropicPythonRequest(ctx: SampleContext, streaming: boolean): string {
  const body = streaming
    ? `with client.messages.stream(
    model="${MODEL_PLACEHOLDER}",
    max_tokens=200,
    messages=[{"role": "user", "content": "Count to 5."}],
) as stream:
    for text in stream.text_stream:
        print(text, end="", flush=True)`
    : `message = client.messages.create(
    model="${MODEL_PLACEHOLDER}",
    max_tokens=200,
    messages=[{"role": "user", "content": "Explain quantum computing in one sentence."}],
)
print(message.content[0].text)`
  return `${anthropicPythonSetup(ctx)}\n\n${body}`
}

function anthropicNodeSetup(ctx: SampleContext): string {
  return `import Anthropic from '@anthropic-ai/sdk'

const client = new Anthropic({
  apiKey: '${cred(ctx.key)}',
  baseURL: '${ctx.endpoint}',
})`
}

function anthropicNodeRequest(ctx: SampleContext, streaming: boolean): string {
  const body = streaming
    ? `const stream = await client.messages.create({
  model: '${MODEL_PLACEHOLDER}',
  max_tokens: 200,
  messages: [{ role: 'user', content: 'Count to 5.' }],
  stream: true,
})
for await (const event of stream) {
  if (event.type === 'content_block_delta') {
    process.stdout.write(event.delta.text ?? '')
  }
}`
    : `const message = await client.messages.create({
  model: '${MODEL_PLACEHOLDER}',
  max_tokens: 200,
  messages: [{ role: 'user', content: 'Explain quantum computing in one sentence.' }],
})
console.log(message.content[0].text)`
  return `${anthropicNodeSetup(ctx)}\n\n${body}`
}

const ANTHROPIC_REQUESTS: Record<
  CoreLanguage,
  (ctx: SampleContext, streaming: boolean) => string
> = {
  curl: anthropicCurlRequest,
  python: anthropicPythonRequest,
  node: anthropicNodeRequest,
}

function anthropicLanguage(language: CoreLanguage, ctx: SampleContext): LanguageExamples {
  const request = (streaming: boolean): string => ANTHROPIC_REQUESTS[language](ctx, streaming)
  return {
    language,
    snippets: [
      { kind: 'request', streaming: false, code: request(false) },
      { kind: 'response', streaming: false, code: ANTHROPIC_RESPONSE },
      { kind: 'request', streaming: true, code: request(true) },
      { kind: 'response', streaming: true, code: ANTHROPIC_STREAM_RESPONSE },
    ],
  }
}

function anthropicChatGroup(ctx: SampleContext): ExampleGroup {
  const languages = CORE_LANGUAGES
  return {
    id: 'anthropic-chat',
    protocol: 'anthropic',
    languages: languages.map((language) => anthropicLanguage(language, ctx)),
  }
}

// --- Gemini-native chat (/v1beta) ----------------------------------------
//
// Gemini clients spell the key as x-goog-api-key (or ?key=) — that
// spelling is accepted on this ingress only — and the path carries the
// action suffix. The SDK base URL is the bare endpoint; the SDK appends
// /v1beta/models/{model}:generateContent itself.

const GEMINI_RESPONSE = `{
  "candidates": [
    {
      "content": {"role": "model", "parts": [{"text": "Hello! How can I help you today?"}]},
      "finishReason": "STOP"
    }
  ],
  "usageMetadata": {"promptTokenCount": 8, "candidatesTokenCount": 9}
}`

function geminiChatGroup(ctx: SampleContext): ExampleGroup {
  const curl = `curl ${geminiBaseUrlOf(ctx.endpoint)}/models/${MODEL_PLACEHOLDER}:generateContent \\
  -H "x-goog-api-key: ${cred(ctx.key)}" \\
  -H "Content-Type: application/json" \\
  -d '{
    "contents": [{"role": "user", "parts": [{"text": "Hello!"}]}]
  }'`
  const python = `from google import genai

client = genai.Client(
    api_key="${cred(ctx.key)}",
    http_options={"base_url": "${ctx.endpoint}"},
)

response = client.models.generate_content(
    model="${MODEL_PLACEHOLDER}",
    contents="Explain quantum computing in one sentence.",
)
print(response.text)`
  const node = `import { GoogleGenAI } from '@google/genai'

const ai = new GoogleGenAI({
  apiKey: '${cred(ctx.key)}',
  httpOptions: { baseUrl: '${ctx.endpoint}' },
})

const response = await ai.models.generateContent({
  model: '${MODEL_PLACEHOLDER}',
  contents: 'Explain quantum computing in one sentence.',
})
console.log(response.text)`
  return singleRequestGroup('gemini-chat', 'gemini', ctx, { curl, python, node }, GEMINI_RESPONSE)
}

// --- OpenAI Responses API (/v1/responses) ---------------------------------
//
// Three variants: plain, streaming (SSE response events), and the
// function-calling flavour whose response carries a function_call output.

const RESPONSES_RESPONSE = `{
  "id": "resp_...",
  "object": "response",
  "model": "${MODEL_PLACEHOLDER}",
  "status": "completed",
  "output": [
    {
      "type": "message",
      "role": "assistant",
      "content": [{"type": "output_text", "text": "Bonjour !"}]
    }
  ],
  "usage": {"input_tokens": 12, "output_tokens": 4}
}`

const RESPONSES_STREAM_RESPONSE = `event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"Bonjour"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_...","status":"completed"}}`

const RESPONSES_TOOLS_RESPONSE = `{
  "id": "resp_...",
  "object": "response",
  "model": "${MODEL_PLACEHOLDER}",
  "status": "completed",
  "output": [
    {
      "type": "function_call",
      "name": "get_weather",
      "arguments": "{\\"city\\": \\"Tokyo\\"}",
      "call_id": "call_..."
    }
  ]
}`

function responsesCurlRequest(ctx: SampleContext, variant: 'plain' | 'streaming' | 'tools'): string {
  const body =
    variant === 'tools'
      ? `{
    "model": "${MODEL_PLACEHOLDER}",
    "input": "What is the weather in Tokyo?",
    "tools": [
      {
        "type": "function",
        "name": "get_weather",
        "description": "Get the current weather for a city",
        "parameters": {
          "type": "object",
          "properties": {"city": {"type": "string"}}
        }
      }
    ]
  }`
      : variant === 'streaming'
        ? `{
    "model": "${MODEL_PLACEHOLDER}",
    "input": [{"role": "user", "content": "Count to 5."}],
    "stream": true
  }`
        : `{
    "model": "${MODEL_PLACEHOLDER}",
    "input": "Say hello in French."
  }`
  return `curl ${openAIBaseUrlOf(ctx.endpoint)}/responses \\
  -H "Authorization: Bearer ${cred(ctx.key)}" \\
  -H "Content-Type: application/json" \\
  -d '${body}'`
}

function responsesPythonRequest(ctx: SampleContext, variant: 'plain' | 'streaming' | 'tools'): string {
  const body =
    variant === 'tools'
      ? `response = client.responses.create(
    model="${MODEL_PLACEHOLDER}",
    input="What is the weather in Tokyo?",
    tools=[{
        "type": "function",
        "name": "get_weather",
        "description": "Get the current weather for a city",
        "parameters": {"type": "object", "properties": {"city": {"type": "string"}}},
    }],
)
for item in response.output:
    if item.type == "function_call":
        print(item.name, item.arguments)`
      : variant === 'streaming'
        ? `stream = client.responses.create(
    model="${MODEL_PLACEHOLDER}",
    input=[{"role": "user", "content": "Count to 5."}],
    stream=True,
)
for event in stream:
    if event.type == "response.output_text.delta":
        print(event.delta, end="", flush=True)`
        : `response = client.responses.create(
    model="${MODEL_PLACEHOLDER}",
    input="Say hello in French.",
)
print(response.output_text)`
  return `${pythonSdkSetup(ctx)}\n\n${body}`
}

function responsesNodeRequest(ctx: SampleContext, variant: 'plain' | 'streaming' | 'tools'): string {
  const body =
    variant === 'tools'
      ? `const response = await client.responses.create({
  model: '${MODEL_PLACEHOLDER}',
  input: 'What is the weather in Tokyo?',
  tools: [
    {
      type: 'function',
      name: 'get_weather',
      description: 'Get the current weather for a city',
      parameters: { type: 'object', properties: { city: { type: 'string' } } },
    },
  ],
})
for (const item of response.output) {
  if (item.type === 'function_call') {
    console.log(item.name, item.arguments)
  }
}`
      : variant === 'streaming'
        ? `const stream = await client.responses.create({
  model: '${MODEL_PLACEHOLDER}',
  input: [{ role: 'user', content: 'Count to 5.' }],
  stream: true,
})
for await (const event of stream) {
  if (event.type === 'response.output_text.delta') {
    process.stdout.write(event.delta ?? '')
  }
}`
        : `const response = await client.responses.create({
  model: '${MODEL_PLACEHOLDER}',
  input: 'Say hello in French.',
})
console.log(response.output_text)`
  return `${nodeSdkSetup(ctx)}\n\n${body}`
}

const RESPONSES_REQUESTS: Record<
  CoreLanguage,
  (ctx: SampleContext, variant: 'plain' | 'streaming' | 'tools') => string
> = {
  curl: responsesCurlRequest,
  python: responsesPythonRequest,
  node: responsesNodeRequest,
}

function responsesGroup(ctx: SampleContext): ExampleGroup {
  const languages = CORE_LANGUAGES
  return {
    id: 'openai-responses',
    protocol: 'openai',
    languages: languages.map((language) => ({
      language,
      snippets: [
        {
          kind: 'request',
          streaming: false,
          code: RESPONSES_REQUESTS[language](ctx, 'plain'),
        },
        { kind: 'response', streaming: false, code: RESPONSES_RESPONSE },
        {
          kind: 'request',
          streaming: true,
          code: RESPONSES_REQUESTS[language](ctx, 'streaming'),
        },
        { kind: 'response', streaming: true, code: RESPONSES_STREAM_RESPONSE },
        {
          kind: 'request',
          streaming: false,
          tag: 'tools',
          code: RESPONSES_REQUESTS[language](ctx, 'tools'),
        },
        { kind: 'response', streaming: false, tag: 'tools', code: RESPONSES_TOOLS_RESPONSE },
      ],
    })),
  }
}

export function buildExampleCatalog(options: ExampleCatalogOptions): ExampleGroup[] {
  return [
    chatGroup(options),
    imagesGenerationsGroup(options),
    imagesEditsGroup(options),
    videosGroup(options),
    modelsGroup(options),
    anthropicChatGroup(options),
    geminiChatGroup(options),
    responsesGroup(options),
  ]
}
