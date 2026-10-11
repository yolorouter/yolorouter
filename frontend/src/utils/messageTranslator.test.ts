// frontend/src/utils/messageTranslator.test.ts
//
// Table-driven unit matrices for the session-detail message translator
// (pure functions — node env, no DOM), one matrix per body side:
//
//   request side         OpenAI multi-turn / empty content / mixed parts,
//                        Anthropic system + multi blocks
//                        (text+image+tool_use), ambiguous bodies asserted
//                        against the FIXED Anthropic-first judgment order
//   response side        OpenAI choices[].message, Anthropic top-level
//                        content, non-JSON → fallback, truncation marker →
//                        truncation signal
//   SSE increment merge  OpenAI delta sequences (first chunk role),
//                        Anthropic text_delta sequences across
//                        content_block_start/delta/stop boundaries,
//                        thinking_delta sequences (off-axis thinking
//                        part), tool_use reassembled from input_json_delta
//                        fragments (call part with id/name), interleaved
//                        and malformed lines, empty capture → not-recorded
//                        placeholder, terminator-only stream → empty reply
//   bare message array   the session-replay slice form: one message-object
//                        array per dialect, asserted isomorphic to the
//                        whole-body translation of the equivalent body,
//                        plus the degradation legs (nothing delivered →
//                        placeholder, no chat shape → fallback)
//
// Fixtures mirror the wire formats as the gateway relays them (the stream
// fixtures match what the on-disk SSE capture contains: verbatim event:
// lines plus `data: {json}` per chunk).
import { describe, expect, it } from 'vitest'
import {
  translateMessageArray,
  translateRequestBody,
  translateResponseBody,
  translateStreamBody,
  type ChatMessage,
  type TranslatedBody,
} from './messageTranslator'

// The backend's inline truncation suffix (request-log service's
// inlineTruncationMarker) — byte counts only matter for detection here.
const MARKER = '\n\n… [truncated: showing first 1048576 of 2097152 bytes]'

/** Shorthand: run the request-side translator and assert the happy-path
 *  envelope, returning the messages for per-item assertions. */
function reqMessages(raw: string): ChatMessage[] {
  const out = translateRequestBody(raw)
  expect(out.kind).toBe('messages')
  expect(out.truncated).toBe(false)
  return out.messages
}

function resMessages(raw: string): ChatMessage[] {
  const out = translateResponseBody(raw)
  expect(out.kind).toBe('messages')
  expect(out.truncated).toBe(false)
  return out.messages
}

/** Full-envelope equality against the exact internal message array — pins
 *  role/text and every part's type and label, per item. */
function msg(role: string, text: string, parts: ChatMessage['parts']): ChatMessage {
  return { role, text, parts }
}

// ---------------------------------------------------------------------------
// Request-side matrix
// ---------------------------------------------------------------------------

describe('translateRequestBody (request-side matrix)', () => {
  it.each<[string, string, ChatMessage[]]>([
    [
      'OpenAI multi-turn string contents',
      JSON.stringify({
        model: 'gpt-4o',
        messages: [
          { role: 'system', content: 'You are helpful.' },
          { role: 'user', content: 'Hello' },
          { role: 'assistant', content: 'Hi! How can I help?' },
          { role: 'user', content: 'What is 2+2?' },
        ],
      }),
      [
        msg('system', 'You are helpful.', [{ type: 'text', text: 'You are helpful.' }]),
        msg('user', 'Hello', [{ type: 'text', text: 'Hello' }]),
        msg('assistant', 'Hi! How can I help?', [{ type: 'text', text: 'Hi! How can I help?' }]),
        msg('user', 'What is 2+2?', [{ type: 'text', text: 'What is 2+2?' }]),
      ],
    ],
    [
      'OpenAI empty content renders an empty bubble, not a fallback',
      JSON.stringify({ messages: [{ role: 'user', content: '' }] }),
      [msg('user', '', [])],
    ],
    [
      'OpenAI mixed parts: text verbatim + image placeholder type',
      JSON.stringify({
        messages: [
          {
            role: 'user',
            content: [
              { type: 'text', text: 'What is in this picture?' },
              { type: 'image_url', image_url: { url: 'data:image/png;base64,AAAA' } },
            ],
          },
        ],
      }),
      [msg('user', 'What is in this picture?', [{ type: 'text', text: 'What is in this picture?' }, { type: 'image' }])],
    ],
    [
      'OpenAI assistant tool_calls + role-tool result message',
      JSON.stringify({
        messages: [
          { role: 'user', content: 'Weather?' },
          {
            role: 'assistant',
            content: null,
            tool_calls: [{ id: 'call_1', type: 'function', function: { name: 'get_weather', arguments: '{}' } }],
          },
          { role: 'tool', tool_call_id: 'call_1', content: '22C sunny' },
        ],
      }),
      [
        msg('user', 'Weather?', [{ type: 'text', text: 'Weather?' }]),
        msg('assistant', '', [{ type: 'tool', label: 'tool_calls: get_weather', name: 'get_weather', variant: 'call', id: 'call_1' }]),
        msg('tool', '22C sunny', [{ type: 'text', text: '22C sunny' }]),
      ],
    ],
    [
      'Anthropic system string + multi blocks (text+image+tool_use)',
      JSON.stringify({
        model: 'claude-sonnet-4',
        system: 'You are helpful.',
        messages: [
          {
            role: 'user',
            content: [
              { type: 'text', text: 'Read this chart' },
              { type: 'image', source: { type: 'base64', media_type: 'image/png', data: 'AAAA' } },
            ],
          },
          {
            role: 'assistant',
            content: [
              { type: 'text', text: 'I will run the analysis.' },
              { type: 'tool_use', id: 'toolu_1', name: 'Bash', input: { command: 'ls' } },
            ],
          },
        ],
      }),
      [
        msg('system', 'You are helpful.', [{ type: 'text', text: 'You are helpful.' }]),
        msg('user', 'Read this chart', [{ type: 'text', text: 'Read this chart' }, { type: 'image' }]),
        msg('assistant', 'I will run the analysis.', [
          { type: 'text', text: 'I will run the analysis.' },
          { type: 'tool', label: 'tool_use: Bash', name: 'Bash', variant: 'call', id: 'toolu_1', detail: '{\n  "command": "ls"\n}', detailLang: 'json' },
        ]),
      ],
    ],
    [
      'Anthropic system as blocks array',
      JSON.stringify({
        system: [{ type: 'text', text: 'Be terse.' }],
        messages: [{ role: 'user', content: 'hi' }],
      }),
      [
        msg('system', 'Be terse.', [{ type: 'text', text: 'Be terse.' }]),
        msg('user', 'hi', [{ type: 'text', text: 'hi' }]),
      ],
    ],
    [
      'Anthropic tool_result block shows a bare type label (no name on the wire)',
      JSON.stringify({
        messages: [{ role: 'user', content: [{ type: 'tool_result', tool_use_id: 'toolu_1', content: '22C sunny' }] }],
      }),
      [msg('user', '', [{ type: 'tool', label: 'tool_result', variant: 'result', detail: '22C sunny' }])],
    ],
  ])('translates %s', (_name, raw, expected) => {
    expect(reqMessages(raw)).toEqual(expected)
  })

  it('judges ambiguous bodies Anthropic-first: the top-level system survives', () => {
    // A pure-text content array matches BOTH vocabularies ('text' exists
    // in each); the fixed order runs the Anthropic parser, whose visible
    // difference is keeping the top-level system as a leading message —
    // the OpenAI reading has no such field and would drop it.
    const out = translateRequestBody(
      JSON.stringify({
        system: 'be terse',
        messages: [{ role: 'user', content: [{ type: 'text', text: 'hi' }] }],
      }),
    )
    expect(out.kind).toBe('messages')
    expect(out.messages).toEqual([
      msg('system', 'be terse', [{ type: 'text', text: 'be terse' }]),
      msg('user', 'hi', [{ type: 'text', text: 'hi' }]),
    ])
  })

  it('keeps a both-features body renderable under the Anthropic-first order', () => {
    // Top-level system (Anthropic) + an image_url part (OpenAI-only
    // vocabulary): the fixed order takes the Anthropic path, so the
    // image_url block degrades to a type label instead of an image
    // placeholder — degraded but fully renderable, never a throw.
    const out = translateRequestBody(
      JSON.stringify({
        system: 's',
        messages: [
          { role: 'user', content: [{ type: 'text', text: 'pic' }, { type: 'image_url', image_url: { url: 'data:' } }] },
        ],
      }),
    )
    expect(out.kind).toBe('messages')
    expect(out.messages).toEqual([
      msg('system', 's', [{ type: 'text', text: 's' }]),
      msg('user', 'pic', [{ type: 'text', text: 'pic' }, { type: 'tool', label: 'image_url' }]),
    ])
  })

  it.each<[string, string]>([
    ['non-JSON text', 'upstream exploded'],
    ['JSON scalar', '42'],
    ['JSON array', '[1,2,3]'],
    ['valid JSON but no chat shape (embeddings)', JSON.stringify({ model: 'text-embedding-3-small', input: 'hello' })],
    ['empty messages array', JSON.stringify({ messages: [] })],
    ['messages of wrong type', JSON.stringify({ messages: 'nope' })],
  ])('falls back on %s', (_name, raw) => {
    const out = translateRequestBody(raw)
    expect(out.kind).toBe('fallback')
    expect(out.messages).toEqual([])
    expect(out.truncated).toBe(false)
  })

  it('signals truncation instead of parsing a marker-carrying body', () => {
    const raw = JSON.stringify({ messages: [{ role: 'user', content: 'hello' }] }) + MARKER
    const out = translateRequestBody(raw)
    expect(out.kind).toBe('fallback')
    expect(out.truncated).toBe(true)
  })

  it('returns the placeholder signal for an empty body', () => {
    const out = translateRequestBody('   ')
    expect(out.kind).toBe('placeholder')
    expect(out.messages).toEqual([])
  })
})

// ---------------------------------------------------------------------------
// Bare message-array slice (session replay)
// ---------------------------------------------------------------------------

// The slice fixtures the replay transcript delivers: message objects with
// the assistant/system roles already stripped (the added slice) or
// assistant-only (the resend copies), one per dialect.
const OPENAI_SLICE = [
  { role: 'user', content: 'Weather in Beijing?' },
  { role: 'assistant', content: null, tool_calls: [{ id: 'c1', type: 'function', function: { name: 'get_weather', arguments: '{"city":"北京"}' } }] },
  { role: 'tool', tool_call_id: 'c1', content: '晴 26 度' },
]
const ANTHROPIC_SLICE = [
  { role: 'user', content: [{ type: 'text', text: 'Run the deploy check' }] },
  { role: 'assistant', content: [{ type: 'text', text: 'Checking.' }, { type: 'tool_use', id: 'tu_1', name: 'Bash', input: { command: 'ls' } }] },
  { role: 'user', content: [{ type: 'tool_result', tool_use_id: 'tu_1', content: 'total 0' }] },
]

describe('translateMessageArray (bare message-array matrix)', () => {
  it.each([
    ['OpenAI dialect group: user / assistant tool_calls / tool result', OPENAI_SLICE as unknown[], [
      msg('user', 'Weather in Beijing?', [{ type: 'text', text: 'Weather in Beijing?' }]),
      msg('assistant', '', [{ type: 'tool', label: 'tool_calls: get_weather', name: 'get_weather', variant: 'call', id: 'c1', detail: '{\n  "city": "北京"\n}', detailLang: 'json' }]),
      msg('tool', '晴 26 度', [{ type: 'text', text: '晴 26 度' }]),
    ]],
    ['Anthropic dialect group: blocks walk the blocks parser with in-array tool naming', ANTHROPIC_SLICE as unknown[], [
      msg('user', 'Run the deploy check', [{ type: 'text', text: 'Run the deploy check' }]),
      msg('assistant', 'Checking.', [
        { type: 'text', text: 'Checking.' },
        { type: 'tool', label: 'tool_use: Bash', name: 'Bash', variant: 'call', id: 'tu_1', detail: '{\n  "command": "ls"\n}', detailLang: 'json' },
      ]),
      msg('user', '', [{ type: 'tool', label: 'tool_result: Bash', name: 'Bash', variant: 'result', detail: 'total 0' }]),
    ]],
  ] as Array<[string, unknown[], ChatMessage[]]>)('translates the %s', (_name, input, expected) => {
    const out = translateMessageArray(input)
    expect(out.kind).toBe('messages')
    expect(out.truncated).toBe(false)
    expect(out.messages).toEqual(expected)
  })

  it.each([
    ['the OpenAI dialect slice', OPENAI_SLICE],
    ['the Anthropic dialect slice', ANTHROPIC_SLICE],
  ])('emits the same structure the whole-body translation emits for %s', (_name, slice) => {
    // "Isomorphic to the whole-body translation" pinned literally: the
    // bare array and the equivalent wrapped body must produce the exact
    // same internal envelope, so the replay page renders slices through
    // the very shape the existing message flow renders whole bodies.
    expect(translateMessageArray(slice)).toEqual(translateRequestBody(JSON.stringify({ messages: slice })))
  })

  it('renders every resent assistant copy when several arrive (blocks and tool_calls shapes)', () => {
    const out = translateMessageArray([
      { role: 'assistant', content: [{ type: 'text', text: 'First attempt.' }] },
      { role: 'assistant', content: null, tool_calls: [{ id: 'c9', type: 'function', function: { name: 'calc', arguments: '{}' } }] },
      { role: 'assistant', content: [{ type: 'text', text: 'Done.' }] },
    ])
    expect(out.kind).toBe('messages')
    expect(out.messages).toEqual([
      msg('assistant', 'First attempt.', [{ type: 'text', text: 'First attempt.' }]),
      msg('assistant', '', [{ type: 'tool', label: 'tool_calls: calc', name: 'calc', variant: 'call', id: 'c9' }]),
      msg('assistant', 'Done.', [{ type: 'text', text: 'Done.' }]),
    ])
  })

  it('judges each message on its own content, so a mixed-dialect array still renders', () => {
    const out = translateMessageArray([
      { role: 'user', content: 'string content walks the OpenAI reading' },
      { role: 'user', content: [{ type: 'text', text: 'block content walks the blocks parser' }, { type: 'image', source: { type: 'base64', media_type: 'image/png', data: 'AAAA' } }] },
    ])
    expect(out.kind).toBe('messages')
    expect(out.messages).toEqual([
      msg('user', 'string content walks the OpenAI reading', [{ type: 'text', text: 'string content walks the OpenAI reading' }]),
      msg('user', 'block content walks the blocks parser', [
        { type: 'text', text: 'block content walks the blocks parser' },
        { type: 'image' },
      ]),
    ])
  })

  it('degrades a single untranslatable message to an empty bubble, not a failed array', () => {
    const out = translateMessageArray([{ role: 'user', content: null }, 'not a record', 7])
    expect(out.kind).toBe('messages')
    expect(out.messages).toEqual([msg('user', '', [])])
  })

  it.each([
    ['nothing delivered (null)', null],
    ['nothing delivered (undefined)', undefined],
  ])('returns the placeholder signal for %s', (_name, input) => {
    const out = translateMessageArray(input)
    expect(out.kind).toBe('placeholder')
    expect(out.messages).toEqual([])
    expect(out.truncated).toBe(false)
  })

  it.each([
    ['a non-array object', { role: 'user', content: 'hi' }],
    ['a bare string', 'hello'],
    ['a number', 42],
    ['an empty array (a pure retry that contributed nothing)', []],
    ['an array of non-record elements only', [1, 'x', null]],
  ])('falls back on %s', (_name, input) => {
    const out = translateMessageArray(input)
    expect(out.kind).toBe('fallback')
    expect(out.messages).toEqual([])
    expect(out.truncated).toBe(false)
  })
})

// ---------------------------------------------------------------------------
// Response-side matrix
// ---------------------------------------------------------------------------

describe('translateResponseBody (response-side matrix)', () => {
  it.each<[string, string, ChatMessage[]]>([
    [
      'OpenAI choices[].message with string content',
      JSON.stringify({
        id: 'chatcmpl-1',
        object: 'chat.completion',
        choices: [{ index: 0, message: { role: 'assistant', content: 'Hello! How can I help?' }, finish_reason: 'stop' }],
        usage: { prompt_tokens: 9, completion_tokens: 7 },
      }),
      [msg('assistant', 'Hello! How can I help?', [{ type: 'text', text: 'Hello! How can I help?' }])],
    ],
    [
      'OpenAI message content parts + tool_calls',
      JSON.stringify({
        choices: [
          {
            message: {
              role: 'assistant',
              content: [{ type: 'text', text: 'Let me check.' }],
              tool_calls: [{ id: 'c1', type: 'function', function: { name: 'search', arguments: '{}' } }],
            },
          },
        ],
      }),
      [
        msg('assistant', 'Let me check.', [
          { type: 'text', text: 'Let me check.' },
          { type: 'tool', label: 'tool_calls: search', name: 'search', variant: 'call', id: 'c1' },
        ]),
      ],
    ],
    [
      'OpenAI null content with tool_calls only',
      JSON.stringify({
        choices: [{ message: { role: 'assistant', content: null, tool_calls: [{ function: { name: 'calc' } }] } }],
      }),
      [msg('assistant', '', [{ type: 'tool', label: 'tool_calls: calc', name: 'calc', variant: 'call' }])],
    ],
    [
      'OpenAI multiple choices render one message each',
      JSON.stringify({
        choices: [
          { index: 0, message: { role: 'assistant', content: 'first' } },
          { index: 1, message: { role: 'assistant', content: 'second' } },
        ],
      }),
      [
        msg('assistant', 'first', [{ type: 'text', text: 'first' }]),
        msg('assistant', 'second', [{ type: 'text', text: 'second' }]),
      ],
    ],
    [
      'Anthropic top-level content array',
      JSON.stringify({
        id: 'msg_1',
        type: 'message',
        role: 'assistant',
        content: [{ type: 'text', text: 'Good morning!' }],
        model: 'claude-sonnet-4',
        usage: { input_tokens: 3, output_tokens: 5 },
      }),
      [msg('assistant', 'Good morning!', [{ type: 'text', text: 'Good morning!' }])],
    ],
    [
      'Anthropic content with tool_use block',
      JSON.stringify({
        role: 'assistant',
        content: [
          { type: 'text', text: 'Running it.' },
          { type: 'tool_use', id: 'toolu_1', name: 'Read', input: { path: '/tmp' } },
        ],
      }),
      [
        msg('assistant', 'Running it.', [
          { type: 'text', text: 'Running it.' },
          { type: 'tool', label: 'tool_use: Read', name: 'Read', variant: 'call', id: 'toolu_1', detail: '{\n  "path": "/tmp"\n}', detailLang: 'json' },
        ]),
      ],
    ],
    [
      'Anthropic missing role defaults to assistant',
      JSON.stringify({ content: [{ type: 'text', text: 'anon' }] }),
      [msg('assistant', 'anon', [{ type: 'text', text: 'anon' }])],
    ],
  ])('translates %s', (_name, raw, expected) => {
    expect(resMessages(raw)).toEqual(expected)
  })

  it.each<[string, string]>([
    ['non-JSON (upstream error page)', '<html>502 Bad Gateway</html>'],
    ['empty object', '{}'],
    ['empty choices array', JSON.stringify({ choices: [] })],
    ['choices without message objects', JSON.stringify({ choices: [{ index: 0 }] })],
    ['empty content array', JSON.stringify({ content: [] })],
  ])('falls back on %s', (_name, raw) => {
    const out = translateResponseBody(raw)
    expect(out.kind).toBe('fallback')
    expect(out.messages).toEqual([])
    expect(out.truncated).toBe(false)
  })

  it('signals truncation for a marker-carrying body (post-truncation JSON is not parseable)', () => {
    const raw = JSON.stringify({ choices: [{ message: { role: 'assistant', content: 'partial...' } }] }) + MARKER
    const out = translateResponseBody(raw)
    expect(out.kind).toBe('fallback')
    expect(out.truncated).toBe(true)
  })

  it('returns the placeholder signal for an empty body (stream response lives on disk)', () => {
    const out = translateResponseBody('')
    expect(out.kind).toBe('placeholder')
    expect(out.messages).toEqual([])
    expect(out.truncated).toBe(false)
  })
})

// ---------------------------------------------------------------------------
// SSE increment-merge matrix
// ---------------------------------------------------------------------------

describe('translateStreamBody (SSE merge matrix)', () => {
  it('merges an OpenAI delta sequence, taking the role from the first chunk', () => {
    const raw = [
      'data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}',
      '',
      'data: {"id":"c1","choices":[{"index":0,"delta":{"content":"Hel"}}]}',
      '',
      'data: {"id":"c1","choices":[{"index":0,"delta":{"content":"lo!"}}]}',
      '',
      'data: [DONE]',
      '',
    ].join('\n')
    expect(translateStreamBody(raw)).toEqual({
      kind: 'messages',
      messages: [msg('assistant', 'Hello!', [{ type: 'text', text: 'Hello!' }])],
      truncated: false,
    } satisfies TranslatedBody)
  })

  it('accepts the compact `data:` form (no space) and CRLF line ends', () => {
    const raw = [
      'data:{"choices":[{"delta":{"content":"A"}}]}\r',
      'data:{"choices":[{"delta":{"content":"B"}}]}',
    ].join('\n')
    expect(translateStreamBody(raw).messages[0]?.text).toBe('AB')
  })

  it('merges an Anthropic text_delta sequence across block boundaries', () => {
    const raw = [
      'event: message_start',
      'data: {"type":"message_start","message":{"id":"msg_1","role":"assistant","content":[]}}',
      '',
      'event: content_block_start',
      'data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}',
      '',
      'event: content_block_delta',
      'data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Good "}}',
      '',
      'event: content_block_delta',
      'data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"morning!"}}',
      '',
      'event: content_block_stop',
      'data: {"type":"content_block_stop","index":0}',
      '',
      'event: message_delta',
      'data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":6}}',
      '',
      'event: message_stop',
      'data: {"type":"message_stop"}',
      '',
    ].join('\n')
    expect(translateStreamBody(raw)).toEqual({
      kind: 'messages',
      messages: [msg('assistant', 'Good morning!', [{ type: 'text', text: 'Good morning!' }])],
      truncated: false,
    } satisfies TranslatedBody)
  })

  it('merges interleaved multi-block text in arrival order while the tool_use block assembles beside it', () => {
    // Blocks 0 and 1 interleave (a delta for 0 arrives after block 1
    // started); block 2 is a tool_use whose input_json_delta fragments
    // must contribute nothing to the merged TEXT — they land in the
    // assembled call part instead.
    const raw = [
      'data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"Part one. "}}',
      'data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"More A. "}}',
      'data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}',
      'data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Part two."}}',
      'data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"t1","name":"Bash","input":{}}}',
      'data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Tail A."}}',
      'data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\\"c\\""}}',
      'data: {"type":"content_block_stop","index":0}',
      'data: {"type":"content_block_stop","index":1}',
      'data: {"type":"content_block_stop","index":2}',
      'data: {"type":"message_stop"}',
    ].join('\n')
    const reply = translateStreamBody(raw).messages[0]!
    expect(reply.text, 'text still merges in arrival order').toBe('Part one. More A. Part two.Tail A.')
    expect(reply.parts.filter((p) => p.type === 'tool')).toEqual([
      // The lone fragment '{"c"' no longer parses — it stays verbatim with
      // no JSON highlighting, exactly what arrived.
      { type: 'tool', label: 'tool_use: Bash', name: 'Bash', variant: 'call', id: 't1', detail: '{"c"' },
    ])
  })

  it('skips malformed and stray lines without breaking the merge', () => {
    const raw = [
      ': keep-alive comment',
      'event: content_block_delta',
      'not an sse line at all',
      'data: {broken json',
      'data: ',
      'data: 42',
      'data: {"choices":[{"delta":{"content":"ok"}}]}',
    ].join('\n')
    const out = translateStreamBody(raw)
    expect(out.kind).toBe('messages')
    expect(out.messages[0]?.text).toBe('ok')
  })

  it.each<[string, string]>([
    ['an empty capture', ''],
    ['a whitespace-only capture', '\n\n \n'],
  ])('renders %s as the not-recorded placeholder — nothing was captured', (_name, raw) => {
    expect(translateStreamBody(raw)).toEqual({
      kind: 'placeholder',
      messages: [],
      truncated: false,
    } satisfies TranslatedBody)
  })

  it('merges a terminator-only OpenAI stream into an empty reply bubble — a real stream that said nothing', () => {
    expect(translateStreamBody('data: [DONE]\n')).toEqual({
      kind: 'messages',
      messages: [msg('assistant', '', [])],
      truncated: false,
    } satisfies TranslatedBody)
  })

  it('merges thinking_delta sequences into an off-axis thinking part, keeping the answer text separate', () => {
    const raw = [
      'data: {"type":"message_start","message":{"role":"assistant","content":[]}}',
      'data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}',
      'data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Plan: "}}',
      'data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"list first."}}',
      'data: {"type":"content_block_stop","index":0}',
      'data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}',
      'data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Here is the listing."}}',
      'data: {"type":"message_stop"}',
    ].join('\n')
    const out = translateStreamBody(raw)
    expect(out.kind).toBe('messages')
    expect(out.messages).toEqual([
      {
        role: 'assistant',
        // The answer text alone — reasoning never pollutes the bubble text.
        text: 'Here is the listing.',
        parts: [
          { type: 'thinking', text: 'Plan: list first.' },
          { type: 'text', text: 'Here is the listing.' },
        ],
      },
    ])
  })

  it('reassembles a streamed tool_use from its input_json_delta fragments — the all-machine-product capture that used to paint a bare empty bubble', () => {
    // The four replay regression captures' shape: thinking block + tool_use
    // block, no text delta at all. The old merge dropped both deltas and
    // answered an empty bubble; the rework assembles both into parts.
    const raw = [
      'data: {"type":"message_start","message":{"role":"assistant","content":[]}}',
      'data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}',
      'data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Run it."}}',
      'data: {"type":"content_block_stop","index":0}',
      'data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_01","name":"Bash","input":{}}}',
      'data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\\"comm"}}',
      'data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"and\\": \\"ls -la\\"}"}}',
      'data: {"type":"content_block_stop","index":1}',
      'data: {"type":"message_stop"}',
    ].join('\n')
    const out = translateStreamBody(raw)
    expect(out.kind).toBe('messages')
    expect(out.messages).toHaveLength(1)
    const reply = out.messages[0]!
    expect(reply.text, 'no text delta means no text — but the bubble is NOT empty').toBe('')
    expect(reply.parts).toEqual([
      { type: 'thinking', text: 'Run it.' },
      {
        type: 'tool',
        label: 'tool_use: Bash',
        name: 'Bash',
        variant: 'call',
        id: 'toolu_01',
        detail: '{\n  "command": "ls -la"\n}',
        detailLang: 'json',
      },
    ])
  })

  it('merges a mixed reasoning turn: thinking, answer text, then the tool call — parts in reading order', () => {
    const raw = [
      'data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}',
      'data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Think."}}',
      'data: {"type":"content_block_stop","index":0}',
      'data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}',
      'data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Answering, and I will call the tool."}}',
      'data: {"type":"content_block_stop","index":1}',
      'data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_02","name":"Read","input":{}}}',
      'data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\\"path\\": \\"/tmp\\"}"}}',
      'data: {"type":"content_block_stop","index":2}',
      'data: {"type":"message_stop"}',
    ].join('\n')
    const out = translateStreamBody(raw)
    const reply = out.messages[0]!
    expect(reply.text).toBe('Answering, and I will call the tool.')
    expect(reply.parts.map((p) => p.type)).toEqual(['thinking', 'text', 'tool'])
    expect(reply.parts[2]).toMatchObject({ name: 'Read', variant: 'call', id: 'toolu_02', detailLang: 'json' })
  })

  it('keeps an unparseable input_json_delta tail verbatim instead of dropping the call', () => {
    // A capture cut mid-arguments: the fragments concatenate to something
    // that no longer parses — the card still shows exactly what arrived.
    const raw = [
      'data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_03","name":"Bash","input":{}}}',
      'data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\\"comm"}}',
      'data: {"type":"message_stop"}',
    ].join('\n')
    const out = translateStreamBody(raw)
    expect(out.messages[0]?.parts[0]).toEqual({
      type: 'tool',
      label: 'tool_use: Bash',
      name: 'Bash',
      variant: 'call',
      id: 'toolu_03',
      detail: '{"comm',
    })
  })

  it('renders thinking blocks in resent assistant copies as thinking parts (the message-array path)', () => {
    const out = translateMessageArray([
      { role: 'assistant', content: [{ type: 'thinking', thinking: 'prior reasoning' }, { type: 'text', text: 'prior answer' }] },
    ])
    expect(out.messages[0]?.parts).toEqual([
      { type: 'thinking', text: 'prior reasoning' },
      { type: 'text', text: 'prior answer' },
    ])
    expect(out.messages[0]?.text, 'thinking stays out of the bubble text').toBe('prior answer')
  })

  it.each<[string, string]>([
    ['a plain-JSON capture (non-SSE upstream inlined)', JSON.stringify({ single: 'json' })],
    ['plain text', 'upstream error page'],
    ['garbage data lines only', 'data: oops\n'],
  ])('falls back on %s', (_name, raw) => {
    const out = translateStreamBody(raw)
    expect(out.kind).toBe('fallback')
    expect(out.messages).toEqual([])
    expect(out.truncated).toBe(false)
  })

  it('flags the truncation marker on a cut stream while keeping the merged text readable', () => {
    const raw = 'data: {"choices":[{"delta":{"content":"visible prefix "}}]}' + MARKER
    const out = translateStreamBody(raw)
    expect(out.kind).toBe('messages')
    expect(out.truncated).toBe(true)
    expect(out.messages[0]?.text).toBe('visible prefix ')
  })
})

// ---------------------------------------------------------------------------
// OpenAI Responses dialect (/v1/responses — codex's wire format)
// ---------------------------------------------------------------------------

describe('translateRequestBody (responses dialect)', () => {
  it('renders instructions + input items as the same conversation bubbles (real codex shape)', () => {
    const raw = JSON.stringify({
      model: 'deepseek-ai/DeepSeek-R1',
      instructions: 'You are a coding agent running in the Codex CLI.',
      input: [
        { type: 'message', role: 'developer', content: [{ type: 'input_text', text: '配置说明' }] },
        { type: 'message', role: 'user', content: [{ type: 'input_text', text: '看看这个仓库' }] },
        { type: 'function_call', name: 'shell', arguments: '{}', call_id: 'call_1' },
        { type: 'function_call_output', call_id: 'call_1', output: 'exit 0' },
        { type: 'reasoning', summary: [] },
      ],
      stream: true,
    })
    const out = translateRequestBody(raw)
    expect(out.kind).toBe('messages')
    expect(out.truncated).toBe(false)
    expect(out.messages.map((m) => m.role)).toEqual(['system', 'developer', 'user', 'assistant', 'tool'])
    expect(out.messages[0]?.text).toBe('You are a coding agent running in the Codex CLI.')
    expect(out.messages[1]?.text).toBe('配置说明')
    expect(out.messages[3]?.parts).toEqual([{ type: 'tool', label: 'function_call: shell', name: 'shell', variant: 'call', id: 'call_1' }])
    expect(out.messages[4]?.text).toBe('exit 0')
  })

  it('keeps embeddings-shaped bodies on the fallback path (string input, no instructions)', () => {
    const raw = JSON.stringify({ model: 'text-embedding-3-small', input: ['hello', 'world'] })
    const out = translateRequestBody(raw)
    expect(out.kind).toBe('fallback')
  })
})

describe('translateResponseBody (responses dialect)', () => {
  it('renders the output item list as messages', () => {
    const raw = JSON.stringify({
      object: 'response',
      status: 'completed',
      output: [
        { type: 'reasoning' },
        { type: 'message', role: 'assistant', status: 'completed', content: [{ type: 'output_text', text: '答案在这里', annotations: [] }] },
        { type: 'function_call', name: 'shell', arguments: '{}', call_id: 'c1' },
      ],
    })
    const out = translateResponseBody(raw)
    expect(out.kind).toBe('messages')
    expect(out.messages.map((m) => m.role)).toEqual(['assistant', 'assistant'])
    expect(out.messages[0]?.text).toBe('答案在这里')
    expect(out.messages[1]?.parts).toEqual([{ type: 'tool', label: 'function_call: shell', name: 'shell', variant: 'call', id: 'c1' }])
  })
})

describe('translateStreamBody (responses dialect)', () => {
  it('merges done-frame items as the reply and skips reasoning summaries (real upstream shape)', () => {
    const frames = [
      'data: {"response":{"id":"r1","output":[],"status":"in_progress"},"type":"response.created"}',
      'data: {"item":{"id":"rs_1","type":"reasoning"},"output_index":0,"type":"response.output_item.added"}',
      'data: {"delta":"We are given","item_id":"rs_1","type":"response.reasoning_summary_text.delta"}',
      'data: {"item":{"id":"rs_1","type":"reasoning"},"output_index":0,"type":"response.output_item.done"}',
      'data: {"item":{"content":[{"annotations":[],"text":"\\n我是基于 OpenAI 的先进语言模型","type":"output_text"}],"id":"msg_1","role":"assistant","status":"completed","type":"message"},"output_index":1,"type":"response.output_item.done"}',
      'data: {"response":{"id":"r1","output":[],"status":"completed","usage":{}},"type":"response.completed"}',
    ].join('\n')
    const out = translateStreamBody(frames)
    expect(out.kind).toBe('messages')
    expect(out.messages).toHaveLength(1)
    expect(out.messages[0]?.role).toBe('assistant')
    expect(out.messages[0]?.text).toContain('先进语言模型')
    expect(out.messages[0]?.text).not.toContain('We are given')
  })

  it('falls back to concatenated output_text deltas for a capture truncated before any item done', () => {
    const frames = [
      'data: {"response":{"id":"r1","output":[],"status":"in_progress"},"type":"response.created"}',
      'data: {"delta":"部分可见","item_id":"msg_1","type":"response.output_text.delta"}',
      'data: {"delta":" 的前缀","item_id":"msg_1","type":"response.output_text.delta"}',
    ].join('\n')
    const out = translateStreamBody(frames)
    expect(out.kind).toBe('messages')
    expect(out.messages[0]?.text).toBe('部分可见 的前缀')
  })

  it('renders an all-reasoning stream as an empty assistant bubble, not fallback', () => {
    const frames = [
      'data: {"item":{"id":"rs_1","type":"reasoning"},"output_index":0,"type":"response.output_item.added"}',
      'data: {"item":{"id":"rs_1","type":"reasoning"},"output_index":0,"type":"response.output_item.done"}',
    ].join('\n')
    const out = translateStreamBody(frames)
    expect(out.kind).toBe('messages')
    expect(out.messages).toHaveLength(1)
    expect(out.messages[0]?.text).toBe('')
  })
})

describe('tool part payloads (detail lifting)', () => {
  it('lifts tool_use input and correlates the tool_result name + output text (Anthropic shape)', () => {
    const raw = JSON.stringify({
      model: 'claude-x',
      messages: [
        { role: 'assistant', content: [{ type: 'tool_use', id: 'tu_1', name: 'Bash', input: { command: 'ls -la' } }] },
        { role: 'user', content: [{ type: 'tool_result', tool_use_id: 'tu_1', content: [{ type: 'text', text: 'total 0\nexit 0' }] }] },
      ],
    })
    const out = translateRequestBody(raw)
    expect(out.kind).toBe('messages')
    expect(out.messages[0]?.parts[0]).toMatchObject({ type: 'tool', label: 'tool_use: Bash', detail: expect.stringContaining('"command": "ls -la"'), detailLang: 'json' })
    expect(out.messages[1]?.parts[0]).toMatchObject({ type: 'tool', label: 'tool_result: Bash', detail: expect.stringContaining('total 0') })
    // Arbitrary tool output is NOT marked as JSON — the bubble keeps it
    // plain mono instead of highlighting terminal text.
    expect(out.messages[1]?.parts[0]).not.toHaveProperty('detailLang')
  })

  it('lifts OpenAI tool_calls arguments as pretty JSON', () => {
    const raw = JSON.stringify({
      model: 'gpt-4o',
      messages: [
        { role: 'assistant', content: null, tool_calls: [{ id: 'c1', type: 'function', function: { name: 'get_weather', arguments: '{"city":"北京"}' } }] },
        { role: 'tool', tool_call_id: 'c1', content: '晴 26 度' },
      ],
    })
    const out = translateRequestBody(raw)
    expect(out.messages[0]?.parts[0]).toMatchObject({ type: 'tool', label: 'tool_calls: get_weather', detail: expect.stringContaining('"city": "北京"'), detailLang: 'json' })
    // The tool role's plain-string content stays a normal text part.
    expect(out.messages[1]?.text).toBe('晴 26 度')
  })

  it('marks a tool_result whose output itself parses as JSON for highlighting', () => {
    const raw = JSON.stringify({
      model: 'claude-x',
      messages: [
        { role: 'assistant', content: [{ type: 'tool_use', id: 'tu_1', name: 'Read', input: { path: '/tmp/x.json' } }] },
        { role: 'user', content: [{ type: 'tool_result', tool_use_id: 'tu_1', content: [{ type: 'text', text: '{"status":"ok","rows":3}' }] }] },
      ],
    })
    const out = translateRequestBody(raw)
    // The string payload reparses and pretty-prints as JSON, so it gets the
    // same highlighting as call arguments.
    expect(out.messages[1]?.parts[0]).toMatchObject({ detail: expect.stringContaining('"status": "ok"'), detailLang: 'json' })
  })

  it('keeps a bare tool part pill when the block carries no payload', () => {
    const raw = JSON.stringify({ model: 'claude-x', messages: [{ role: 'user', content: [{ type: 'tool_result', tool_use_id: 'nope', content: '' }] }] })
    const out = translateRequestBody(raw)
    expect(out.messages[0]?.parts[0]).toEqual({ type: 'tool', label: 'tool_result', variant: 'result' })
  })
})
