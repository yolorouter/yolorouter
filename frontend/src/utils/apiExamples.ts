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

export interface ExampleSnippet {
  kind: 'request' | 'response'
  // SSE flavour of the request or response.
  streaming: boolean
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

// The one-liner the create-key dialog shows next to a fresh key: same URL
// and credential rules as the catalog's chat samples, compressed to a single
// copy-pasteable line for that moment when the user just wants one command
// that works.
export function chatCurlOneLiner(ctx: SampleContext): string {
  return `curl ${chatUrl(ctx.endpoint)} -H "Authorization: Bearer ${cred(ctx.key)}" -d '{"model":"${MODEL_PLACEHOLDER}","messages":[{"role":"user","content":"hi"}]}'`
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

function pythonChatSetup(ctx: SampleContext): string {
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

function nodeChatSetup(ctx: SampleContext): string {
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

function goChatSetup(ctx: SampleContext): string {
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

function chatLanguage(language: ExampleLanguage, ctx: SampleContext): LanguageExamples {
  // Every request block is self-contained — the modal shows request and
  // streaming side by side as separate copyable snippets, so each carries
  // its own client setup instead of assuming the block above it.
  const request = (streaming: boolean): string => {
    switch (language) {
      case 'curl':
        return curlChatRequest(ctx, streaming)
      case 'python':
        return `${pythonChatSetup(ctx)}\n\n${pythonChatRequest(streaming)}`
      case 'node':
        return `${nodeChatSetup(ctx)}\n\n${nodeChatRequest(streaming)}`
      case 'go':
        return `package main\n\n${goChatImports()}\n\nfunc main() {\n    ${goChatSetup(ctx)}\n\n${goChatRequest(streaming)}\n}`
    }
  }
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

export function buildExampleCatalog(options: ExampleCatalogOptions): ExampleGroup[] {
  return [chatGroup(options)]
}
