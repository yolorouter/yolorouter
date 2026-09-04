import { describe, expect, it } from 'vitest'
import {
  buildExampleCatalog,
  chatCurlOneLiner,
  geminiBaseUrlOf,
  openAIBaseUrlOf,
  type ExampleGroup,
} from './apiExamples'

const ENDPOINT = 'https://gateway.example.com'

function chatGroup(key?: string): ExampleGroup {
  const catalog = buildExampleCatalog({ endpoint: ENDPOINT, key })
  const group = catalog.find((g) => g.id === 'openai-chat')
  if (!group) throw new Error('openai-chat group missing from catalog')
  return group
}

function requestOf(group: ExampleGroup, language: string, streaming = false): string {
  const lang = group.languages.find((l) => l.language === language)
  if (!lang) throw new Error(`language ${language} missing from group`)
  const snippet = lang.snippets.find((s) => s.kind === 'request' && s.streaming === streaming)
  if (!snippet) throw new Error(`${language} ${streaming ? 'streaming ' : ''}request missing`)
  return snippet.code
}

describe('buildExampleCatalog', () => {
  it('exposes the OpenAI capability groups in display order', () => {
    const catalog = buildExampleCatalog({ endpoint: ENDPOINT })
    expect(catalog.map((g) => g.id)).toEqual([
      'openai-chat',
      'openai-images-generations',
      'openai-images-edits',
      'openai-videos',
      'openai-models',
    ])
    expect(catalog.every((g) => g.protocol === 'openai')).toBe(true)
  })

  it('exposes the OpenAI chat group for the openai protocol with the full language matrix', () => {
    const group = chatGroup()
    expect(group.protocol).toBe('openai')
    expect(group.languages.map((l) => l.language)).toEqual(['curl', 'python', 'node', 'go'])
  })

  it('targets /v1/chat/completions with bearer auth in every language', () => {
    const group = chatGroup()
    expect(requestOf(group, 'curl')).toContain(`curl ${ENDPOINT}/v1/chat/completions`)
    expect(requestOf(group, 'curl')).toContain('Authorization: Bearer <API Key>')
    expect(requestOf(group, 'python')).toContain(`base_url="${ENDPOINT}/v1"`)
    expect(requestOf(group, 'python')).toContain('api_key="<API Key>"')
    expect(requestOf(group, 'node')).toContain(`baseURL: '${ENDPOINT}/v1'`)
    expect(requestOf(group, 'node')).toContain("apiKey: '<API Key>'")
    expect(requestOf(group, 'go')).toContain(`option.WithBaseURL("${ENDPOINT}/v1")`)
    expect(requestOf(group, 'go')).toContain('option.WithAPIKey("<API Key>")')
  })

  it('pairs each request flavour with a response and flags the streaming one', () => {
    const group = chatGroup()
    for (const lang of group.languages) {
      expect(lang.snippets.map((s) => `${s.streaming ? 'stream ' : ''}${s.kind}`)).toEqual([
        'request',
        'response',
        'stream request',
        'stream response',
      ])
      const streamResponse = lang.snippets.find((s) => s.kind === 'response' && s.streaming)
      expect(streamResponse?.code).toContain('data: [DONE]')
    }
    expect(requestOf(group, 'curl', true)).toContain('"stream": true')
    expect(requestOf(group, 'python', true)).toContain('stream=True')
    expect(requestOf(group, 'node', true)).toContain('stream: true')
  })

  it('keeps the <model> placeholder in every request', () => {
    const group = chatGroup()
    for (const lang of group.languages) {
      expect(requestOf(group, lang.language)).toContain('<model>')
      expect(requestOf(group, lang.language, true)).toContain('<model>')
    }
  })

  it('injects a real key into every request and leaves no placeholder behind', () => {
    const group = chatGroup('sk-live-123')
    for (const lang of group.languages) {
      expect(requestOf(group, lang.language)).toContain('sk-live-123')
      expect(requestOf(group, lang.language, true)).toContain('sk-live-123')
      for (const snippet of lang.snippets) {
        expect(snippet.code).not.toContain('<API Key>')
      }
    }
  })
})

function groupById(id: string, key?: string): ExampleGroup {
  const catalog = buildExampleCatalog({ endpoint: ENDPOINT, key })
  const group = catalog.find((g) => g.id === id)
  if (!group) throw new Error(`group ${id} missing from catalog`)
  return group
}

function codeOf(group: ExampleGroup, language: string): string {
  const lang = group.languages.find((l) => l.language === language)
  if (!lang) throw new Error(`language ${language} missing from group`)
  return lang.snippets.map((s) => s.code).join('\n\n')
}

describe('OpenAI capability groups', () => {
  it('serves images generations with the response shape beside every language', () => {
    const group = groupById('openai-images-generations')
    expect(group.languages.map((l) => l.language)).toEqual(['curl', 'python', 'node'])
    for (const lang of group.languages) {
      expect(lang.snippets.map((s) => s.kind)).toEqual(['request', 'response'])
    }
    // Only curl spells the full path; SDK samples point the client at the
    // base URL and let it append the route.
    expect(group.languages[0].snippets[0].code).toContain(`${ENDPOINT}/v1/images/generations`)
    expect(group.languages[0].snippets[0].code).toContain('Authorization: Bearer <API Key>')
    expect(codeOf(group, 'python')).toContain(`base_url="${ENDPOINT}/v1"`)
    expect(codeOf(group, 'node')).toContain(`baseURL: '${ENDPOINT}/v1'`)
    expect(group.languages[0].snippets[1].code).toContain('"url"')
  })

  it('serves images edits as multipart with the reference image attached', () => {
    const group = groupById('openai-images-edits')
    expect(codeOf(group, 'curl')).toContain(`${ENDPOINT}/v1/images/edits`)
    expect(codeOf(group, 'curl')).toContain('-F image=@input.png')
    expect(codeOf(group, 'python')).toContain('client.images.edit(')
    expect(codeOf(group, 'node')).toContain("fs.createReadStream('input.png')")
  })

  it('walks the video job lifecycle with the real path and fields', () => {
    const group = groupById('openai-videos')
    expect(group.languages.map((l) => l.language)).toEqual(['curl', 'python', 'node'])
    for (const lang of group.languages) {
      expect(lang.snippets.map((s) => `${s.step} ${s.kind}`)).toEqual([
        'submit request',
        'submit response',
        'poll request',
        'poll response',
        'download request',
      ])
      const all = lang.snippets.map((s) => s.code).join('\n')
      // The gateway's real contract: seconds/size, never duration/resolution.
      // curl spells the JSON fields; the SDKs use their own keyword syntax.
      if (lang.language === 'curl') {
        expect(all).toContain('"seconds": 4')
        expect(all).toContain('"size": "720x1280"')
      } else if (lang.language === 'python') {
        expect(all).toContain('seconds=4')
        expect(all).toContain('size="720x1280"')
      } else {
        expect(all).toContain('seconds: 4')
        expect(all).toContain(`size: '720x1280'`)
      }
      expect(all).not.toContain('duration')
      expect(all).not.toContain('resolution')
    }
    const curl = codeOf(group, 'curl')
    expect(curl).toContain(`${ENDPOINT}/v1/videos \\`)
    expect(curl).toContain(`${ENDPOINT}/v1/videos/vid_... -H`)
    expect(curl).toContain(`${ENDPOINT}/v1/videos/vid_.../content`)
    expect(group.languages[0].snippets[1].code).toContain('"status": "queued"')
    expect(group.languages[0].snippets[3].code).toContain('"status": "completed"')
    expect(group.languages[0].snippets[3].code).toContain('"expires_at"')
  })

  it('lists models with their output modalities', () => {
    const group = groupById('openai-models')
    expect(codeOf(group, 'curl')).toContain(`curl ${ENDPOINT}/v1/models`)
    expect(codeOf(group, 'python')).toContain('client.models.list()')
    expect(codeOf(group, 'node')).toContain('client.models.list()')
    expect(group.languages[0].snippets[1].code).toContain('output_modalities')
  })
})

describe('protocol base URLs', () => {
  it('derives each ecosystem base from the bare endpoint', () => {
    expect(openAIBaseUrlOf(ENDPOINT)).toBe(`${ENDPOINT}/v1`)
    expect(geminiBaseUrlOf(ENDPOINT)).toBe(`${ENDPOINT}/v1beta`)
  })
})

describe('chatCurlOneLiner', () => {
  it('renders the single-line copy-pasteable request the create-key dialog shows', () => {
    expect(chatCurlOneLiner({ endpoint: ENDPOINT })).toBe(
      `curl ${ENDPOINT}/v1/chat/completions -H "Authorization: Bearer <API Key>" -d '{"model":"<model>","messages":[{"role":"user","content":"hi"}]}'`,
    )
  })

  it('carries the credential the caller holds on screen', () => {
    expect(chatCurlOneLiner({ endpoint: ENDPOINT, key: 'sk-fresh' })).toContain('Bearer sk-fresh')
  })
})
