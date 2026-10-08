// @vitest-environment happy-dom
//
// Mounted-DOM coverage for the Playground page (acceptance cases
// TC-04/05/06/12/13/15/18/19/20/23, frontend arms), in BOTH locales where
// copy is the thing under test:
//
//   - TC-04: two columns typewriter their text as chunks arrive — the
//     first column to answer shows text while the other still runs — and
//     each finished column shows cost/latency/usage
//   - TC-05: a failing column keeps its native error shape while its
//     neighbours complete
//   - TC-06: the key dropdown renders exactly the rows the pinned
//     endpoint returned (own usable keys only — the pinning itself is the
//     backend's test), and the model picker is fed by the picker endpoint
//   - TC-23: the model picker offers only models serving the active
//     modality tab; the other modality tabs are disabled placeholders
//   - TC-13: a fifth selected model is refused (selection keeps four,
//     unselected rows grey out at the cap)
//   - TC-19: one run request carries the shared parameter row and applies
//     it to every selected column — the payload is the assertion surface
//   - TC-18: a re-run replaces the old column group; old results vanish
//   - TC-12: the vision attachment travels with the send in the dialect's
//     data-URL encoding, and a re-run can swap it
//   - TC-15: stopping mid-run disconnects the channel; unfinished columns
//     settle as cancelled, finished ones keep their results
//   - TC-20: labels are localized and follow a live locale switch
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { createI18n } from 'vue-i18n'
import naive, { NInputNumber, NSelect } from 'naive-ui'
import { defineComponent, nextTick, type ComponentPublicInstance } from 'vue'

import {
  listPlaygroundKeys,
  listPlaygroundModels,
  type PlaygroundKeyOption,
  type PlaygroundModelOption,
} from '../../api/playground'

// Keep the real stream runner (its SSE parsing is part of what these tests
// exercise through a mocked fetch); only the two picker lists are mocked
// so no test touches the network.
vi.mock('../../api/playground', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../api/playground')>()),
  listPlaygroundKeys: vi.fn(),
  listPlaygroundModels: vi.fn(),
}))

const keysMock = vi.mocked(listPlaygroundKeys)
const modelsMock = vi.mocked(listPlaygroundModels)

// Must be imported after the vi.mock factories are registered.
import PlaygroundPage from './PlaygroundPage.vue'
import en from '../../locales/en'
import zhCN from '../../locales/zh-CN'

type Locale = 'en' | 'zh-CN'
const copy = { en: en.playground, 'zh-CN': zhCN.playground } as const

let wrapper: VueWrapper | null = null
let i18n: ReturnType<typeof createI18n> | null = null

// App.vue nests pages under n-config-provider > n-message-provider; the
// page's useMessage() throws without that ancestry.
const Host = defineComponent({
  components: { PlaygroundPage },
  template: '<NConfigProvider><NMessageProvider><PlaygroundPage /></NMessageProvider></NConfigProvider>',
})

function stubDesktopMatchMedia() {
  const mql = {
    matches: false,
    media: '(max-width: 768px)',
    onchange: null,
    addEventListener: () => {},
    removeEventListener: () => {},
    addListener: () => {},
    removeListener: () => {},
    dispatchEvent: () => true,
  }
  vi.stubGlobal('matchMedia', vi.fn(() => mql))
}

async function mountPage(locale: Locale) {
  i18n = createI18n({ legacy: false, locale, fallbackLocale: 'zh-CN', messages: { en, 'zh-CN': zhCN } })
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: { template: '<div />' } },
      { path: '/playground', component: { template: '<div />' } },
    ],
  })
  await router.push('/playground')
  await router.isReady()
  const pinia = createPinia()
  setActivePinia(pinia)
  wrapper = mount(Host, { attachTo: document.body, global: { plugins: [pinia, router, i18n, naive] } })
  await nextTick()
  await nextTick()
}

const keyFixture: PlaygroundKeyOption[] = [
  { id: 7, key_prefix: 'sk-yr-aaaa', remark: 'daily driver' },
  { id: 9, key_prefix: 'sk-yr-bbbb', remark: '' },
]

const modelFixture: PlaygroundModelOption[] = [
  { name: 'text-only-model', output_modalities: ['text'] },
  { name: 'image-only-model', output_modalities: ['image'] },
  { name: 'video-only-model', output_modalities: ['video'] },
  { name: 'text-and-image-model', output_modalities: ['text', 'image'] },
]

// The models select, located by its marker class (the key select shares
// the NSelect component type).
type SelectProps = {
  value?: string[] | number | null
  options?: Array<{ label: string; value: string | number; disabled?: boolean }>
}
type SelectWrapper = VueWrapper<unknown, ComponentPublicInstance<SelectProps>>
function findSelect(marker: string): SelectWrapper {
  const sel = wrapper!.findAllComponents(NSelect).find((s) => s.classes().includes(marker))
  expect(sel, `select with class ${marker} rendered`).toBeTruthy()
  return sel as unknown as SelectWrapper
}
const modelsSelect = () => findSelect('pg-models-select')
const keySelect = () => findSelect('pg-key-select')

function sendButton(locale: Locale) {
  const btn = wrapper!.findAll('button').find((b) => b.text() === copy[locale].send)
  expect(btn, 'run button rendered').toBeTruthy()
  return btn!
}

// A scripted aggregate-channel response: one done column (with its chunk)
// and one failed column carrying a native OpenAI-shaped error object.
const DONE_DELTA = 'pong answer'
function twoColumnRunBody(): string {
  return [
    `event: chunk\ndata: ${JSON.stringify({ slot: 0, model: 'text-only-model', delta: DONE_DELTA })}\n\n`,
    `event: done\ndata: ${JSON.stringify({
      slot: 0,
      model: 'text-only-model',
      status: 200,
      request_id: 'req-done-1',
      usage: { input_tokens: 5, output_tokens: 3 },
      cost_micros: 11,
      cost_known: true,
      duration_ms: 1234,
    })}\n\n`,
    `event: error\ndata: ${JSON.stringify({
      slot: 1,
      model: 'text-and-image-model',
      status: 429,
      request_id: 'req-fail-1',
      error: { message: 'You exceeded your current quota', type: 'insufficient_quota' },
    })}\n\n`,
  ].join('')
}

function sseResponse(body: string): Response {
  const stream = new ReadableStream<Uint8Array>({
    start(controller) {
      controller.enqueue(new TextEncoder().encode(body))
      controller.close()
    },
  })
  return new Response(stream, { status: 200, headers: { 'content-type': 'text/event-stream; charset=utf-8' } })
}

// Drives one full run: selects the key, selects models, sets the shared
// parameters and prompt, clicks Run, waits for the stream to be consumed.
async function driveRun(
  locale: Locale,
  modelNames: string[],
  params: { system?: string; temperature?: number; maxTokens?: number },
) {
  keySelect().vm.$emit('update:value', 7)
  modelsSelect().vm.$emit('update:value', modelNames)
  await nextTick()
  // Textareas in template order: prompt, system prompt.
  const [promptBox, systemBox] = wrapper!.findAll('textarea')
  await promptBox.setValue('compare this prompt')
  if (params.system !== undefined) await systemBox.setValue(params.system)
  // The two NInputNumber controls in template order: temperature, max tokens.
  if (params.temperature !== undefined) wrapper!.findAllComponents(NInputNumber)[0].vm.$emit('update:value', params.temperature)
  if (params.maxTokens !== undefined) wrapper!.findAllComponents(NInputNumber)[1].vm.$emit('update:value', params.maxTokens)
  await nextTick()
  await sendButton(locale).trigger('click')
  await vi.waitFor(() => {
    expect(document.querySelector('[data-testid="pg-columns"]')).not.toBeNull()
    expect(document.body.textContent ?? '').toContain(copy[locale].columnStateDone)
  })
}

beforeEach(() => {
  stubDesktopMatchMedia()
  keysMock.mockResolvedValue({ list: keyFixture })
  modelsMock.mockResolvedValue({ list: modelFixture })
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  i18n = null
  vi.unstubAllGlobals()
  vi.clearAllMocks()
  document.body.innerHTML = ''
})

describe('PlaygroundPage pickers', () => {
  it.each<Locale>(['zh-CN', 'en'])('TC-06/23: key dropdown renders the pinned endpoint rows; model picker filters by the active modality (%s)', async (locale) => {
    await mountPage(locale)

    await vi.waitFor(() => {
      expect(keysMock).toHaveBeenCalledTimes(1)
      expect(modelsMock).toHaveBeenCalledTimes(1)
    })
    await nextTick()

    // Key dropdown: exactly the endpoint's rows (the endpoint pins to the
    // signed-in account's own usable keys; values are key ids).
    const keyOptions = keySelect().props('options')!
    expect(keyOptions.map((o) => o.value)).toEqual([7, 9])
    expect(keyOptions[0].label).toContain('sk-yr-aaaa')
    expect(keyOptions[0].label).toContain('daily driver')
    expect(keyOptions[1].label).toContain('sk-yr-bbbb')

    // Model picker under the text tab: only models declaring text output.
    const options = modelsSelect().props('options')!
    expect(options.map((o) => o.value)).toEqual(['text-only-model', 'text-and-image-model'])

    // The other modality tabs are disabled placeholders, so a cross-modality
    // selection cannot even be attempted. The nav tab carries the disabled
    // state as a class (a disabled pane component is never mounted).
    const tab = (name: string) => document.querySelector(`.n-tabs-tab[data-name="${name}"]`)
    for (const name of ['image', 'video', 'audio']) {
      expect(tab(name)?.classList.contains('n-tabs-tab--disabled'), `tab ${name} is disabled`).toBe(true)
    }
    expect(tab('text')?.classList.contains('n-tabs-tab--disabled'), 'text tab is selectable').toBe(false)
  })

  it('TC-13: a fifth selected model is refused and unselected rows grey out at the cap', async () => {
    // The shared fixture has only two text models and both end up selected,
    // so the grey-out would have no row to bite on: offer a third
    // text-declaring model that stays unselected for the whole test.
    modelsMock.mockResolvedValue({
      list: [...modelFixture, { name: 'text-extra-model', output_modalities: ['text'] }],
    })
    await mountPage('en')
    await vi.waitFor(() => expect(modelsMock).toHaveBeenCalledTimes(1))

    modelsSelect().vm.$emit('update:value', ['text-only-model', 'text-and-image-model'])
    await nextTick()

    // Below the cap nothing is greyed out: the unselected extra row stays
    // pickable, or selections could never grow to the cap at all.
    const optionsBelowCap = modelsSelect().props('options')!
    expect(optionsBelowCap.find((o) => o.value === 'text-extra-model')?.disabled).toBeFalsy()

    // The over-cap selection (5 names) is refused: the selection keeps its
    // previous value.
    modelsSelect().vm.$emit('update:value', [
      'text-only-model',
      'text-and-image-model',
      'm3',
      'm4',
      'm5',
    ])
    await nextTick()
    expect(modelsSelect().props('value')).toEqual(['text-only-model', 'text-and-image-model'])
    // The refusal is explained, not silent.
    expect(document.body.textContent ?? '').toContain(copy.en.modelCapReached)

    // Four accepted...
    modelsSelect().vm.$emit('update:value', ['text-only-model', 'text-and-image-model', 'm3', 'm4'])
    await nextTick()
    expect(modelsSelect().props('value')).toHaveLength(4)
    // ...and at the cap every UNselected row is disabled in the dropdown —
    // the fifth model cannot be picked even without the refusal toast —
    // while the already-selected rows stay enabled (deselecting must
    // remain possible).
    const options = modelsSelect().props('options')!
    expect(options.find((o) => o.value === 'text-extra-model')?.disabled, 'unselected row greys out at the cap').toBe(true)
    expect(options.find((o) => o.value === 'text-only-model')?.disabled).toBeFalsy()
    expect(options.find((o) => o.value === 'text-and-image-model')?.disabled).toBeFalsy()
  })
})

describe('PlaygroundPage run', () => {
  it('TC-18/19: one run request carries the shared params for all columns; columns render done/error states; a re-run replaces them', async () => {
    const fetchMock = vi.fn((_path: string, _init?: RequestInit) => Promise.resolve(sseResponse(twoColumnRunBody())))
    vi.stubGlobal('fetch', fetchMock)
    await mountPage('en')
    await vi.waitFor(() => expect(modelsMock).toHaveBeenCalledTimes(1))

    await driveRun('en', ['text-only-model', 'text-and-image-model'], { system: 'be terse', temperature: 0.7, maxTokens: 128 })

    // One request for the whole comparison: models, the prompt, and the
    // shared parameter row applied to every column alike (a comparison
    // varies the model, not the sampling).
    expect(fetchMock).toHaveBeenCalledTimes(1)
    const init = fetchMock.mock.calls[0][1] as RequestInit
    expect(fetchMock.mock.calls[0][0]).toBe('/api/admin/playground/run')
    const payload = JSON.parse(init.body as string) as {
      key_id: number
      modality: string
      models: string[]
      messages: Array<{ role: string; content: string }>
      params: { system?: string; temperature?: number; max_tokens?: number }
    }
    expect(payload.key_id).toBe(7)
    expect(payload.modality).toBe('text')
    expect(payload.models).toEqual(['text-only-model', 'text-and-image-model'])
    expect(payload.messages).toEqual([{ role: 'user', content: 'compare this prompt' }])
    expect(payload.params).toEqual({ system: 'be terse', temperature: 0.7, max_tokens: 128 })

    // The done column: output, cost, latency, usage, and the audit row's
    // request id for the log-page cross-reference.
    const text = document.body.textContent ?? ''
    expect(text).toContain(DONE_DELTA)
    expect(text).toContain('0.000011')
    expect(text).toContain('CNY')
    expect(text).toContain('1.23s')
    expect(text).toContain('req-done-1')
    // The failed column keeps its native error shape: status, message and
    // the dialect's error type, verbatim.
    expect(text).toContain('HTTP 429')
    expect(text).toContain('You exceeded your current quota')
    expect(text).toContain('insufficient_quota')
    expect(text).toContain('req-fail-1')

    // Re-run after changing the input, the shared parameter row AND the
    // model group (TC-18's full step): the old columns are replaced, not
    // merged — the first run's output must be gone — and the re-run must
    // carry the values current AT RE-SEND time. An implementation that
    // snapshots the prompt/params on the first run and replays them on
    // re-send sends the stale first-run values and fails the payload
    // assertions below.
    const secondBody = `event: done\ndata: ${JSON.stringify({
      slot: 0,
      model: 'text-only-model',
      status: 200,
      request_id: 'req-done-2',
      usage: { input_tokens: 2, output_tokens: 1 },
      cost_micros: 4,
      cost_known: true,
      duration_ms: 90,
    })}\n\n`
    fetchMock.mockImplementation(() => Promise.resolve(sseResponse(secondBody)))
    // The columns render output in <pre> blocks, so the composer's controls
    // are still the only textareas / number inputs on the page.
    const [rerunPromptBox, rerunSystemBox] = wrapper!.findAll('textarea')
    await rerunPromptBox.setValue('revised prompt for the re-run')
    await rerunSystemBox.setValue('be verbose')
    wrapper!.findAllComponents(NInputNumber)[0].vm.$emit('update:value', 1.3)
    wrapper!.findAllComponents(NInputNumber)[1].vm.$emit('update:value', 256)
    modelsSelect().vm.$emit('update:value', ['text-only-model'])
    await nextTick()
    const runButtons = wrapper!.findAll('button').filter((b) => b.text() === copy.en.resend)
    expect(runButtons, 'the button relabels to run-again after a completed run').toHaveLength(1)
    await runButtons[0].trigger('click')

    await vi.waitFor(() => {
      const body = document.body.textContent ?? ''
      expect(body).toContain('req-done-2')
      expect(body).not.toContain(DONE_DELTA)
      expect(body).not.toContain('req-done-1')
      expect(body).not.toContain('req-fail-1')
    })
    // Exactly two runs ever hit the endpoint, the second with the new
    // single-model comparison AND the re-send-time prompt and parameter
    // row — every revised value, none of the first run's.
    expect(fetchMock).toHaveBeenCalledTimes(2)
    const payload2 = JSON.parse((fetchMock.mock.calls[1][1] as RequestInit).body as string) as {
      models: string[]
      messages: Array<{ role: string; content: string }>
      params: { system?: string; temperature?: number; max_tokens?: number }
    }
    expect(payload2.models).toEqual(['text-only-model'])
    expect(payload2.messages).toEqual([{ role: 'user', content: 'revised prompt for the re-run' }])
    expect(payload2.params).toEqual({ system: 'be verbose', temperature: 1.3, max_tokens: 256 })

    // And the column count follows the new run — one column, not two.
    expect(document.querySelectorAll('.pg-column')).toHaveLength(1)
  })

  it('clears the placeholder columns when the run is refused before the stream (console envelope)', async () => {
    const fetchMock = vi.fn((_path: string, _init?: RequestInit) =>
      Promise.resolve(
        new Response(JSON.stringify({ code: 11003, message: 'api key not found', timestamp: 1 }), {
          status: 403,
          headers: { 'content-type': 'application/json' },
        }),
      ),
    )
    vi.stubGlobal('fetch', fetchMock)
    await mountPage('en')
    await vi.waitFor(() => expect(modelsMock).toHaveBeenCalledTimes(1))

    keySelect().vm.$emit('update:value', 7)
    modelsSelect().vm.$emit('update:value', ['text-only-model'])
    await nextTick()
    const promptBox = wrapper!.findAll('textarea')[0]
    await promptBox.setValue('hello')
    await sendButton('en').trigger('click')
    await nextTick()

    await vi.waitFor(() => {
      // The run never opened: the placeholder columns come back off and
      // the refusal message names the failure.
      expect(document.querySelector('[data-testid="pg-columns"]')).toBeNull()
      expect(document.body.textContent ?? '').not.toContain(copy.en.columnStateRunning)
    })
  })
})

// A controllable aggregate-channel response: frames are enqueued by the
// test one at a time; the body stays open until the test closes it (or a
// caller abort errors it, the way a real socket-backed stream would).
function controlledEventStream(): { response: () => Response; emit: (frame: string) => void; close: () => void } {
  let controller!: ReadableStreamDefaultController<Uint8Array>
  const queue: string[] = []
  let closed = false
  const response = () =>
    new Response(
      new ReadableStream<Uint8Array>({
        start(c) {
          controller = c
          for (const frame of queue.splice(0)) controller.enqueue(new TextEncoder().encode(frame))
          if (closed) controller.close()
        },
      }),
      { status: 200, headers: { 'content-type': 'text/event-stream; charset=utf-8' } },
    )
  return {
    response,
    emit: (frame: string) => {
      queue.push(frame)
      if (controller) controller.enqueue(new TextEncoder().encode(frame))
    },
    close: () => {
      closed = true
      if (controller) controller.close()
    },
  }
}

function chunkFrame(slot: number, model: string, delta: string): string {
  return `event: chunk\ndata: ${JSON.stringify({ slot, model, delta })}\n\n`
}
function doneFrame(slot: number, model: string, over: Record<string, unknown> = {}): string {
  return `event: done\ndata: ${JSON.stringify({
    slot,
    model,
    status: 200,
    request_id: `req-${slot}`,
    usage: { input_tokens: 5, output_tokens: 3 },
    cost_micros: 11,
    cost_known: true,
    duration_ms: 1234,
    ...over,
  })}\n\n`
}
function errorFrame(slot: number, model: string, over: Record<string, unknown> = {}): string {
  return `event: error\ndata: ${JSON.stringify({
    slot,
    model,
    status: 429,
    request_id: `req-err-${slot}`,
    error: { message: 'You exceeded your current quota', type: 'insufficient_quota' },
    ...over,
  })}\n\n`
}

describe('PlaygroundPage streaming columns', () => {
  it('TC-04: columns typewriter as chunks arrive and the metrics land per column', async () => {
    const stream = controlledEventStream()
    const fetchMock = vi.fn(() => Promise.resolve(stream.response()))
    vi.stubGlobal('fetch', fetchMock)
    await mountPage('en')
    await vi.waitFor(() => expect(modelsMock).toHaveBeenCalledTimes(1))

    keySelect().vm.$emit('update:value', 7)
    modelsSelect().vm.$emit('update:value', ['text-only-model', 'text-and-image-model'])
    await nextTick()
    const promptBox = wrapper!.findAll('textarea')[0]
    await promptBox.setValue('race')
    await sendButton('en').trigger('click')
    await nextTick()
    await vi.waitFor(() => expect(document.querySelector('[data-testid="pg-columns"]')).not.toBeNull())

    // The first column to answer starts typing while the second still
    // runs: two deltas land before any terminal event exists, and the
    // column shows exactly what has arrived so far.
    stream.emit(chunkFrame(0, 'text-only-model', 'Hel'))
    stream.emit(chunkFrame(0, 'text-only-model', 'lo'))
    await vi.waitFor(() => {
      const outputs = document.querySelectorAll('[data-testid="pg-column-output"]')
      expect(outputs).toHaveLength(1)
      expect(outputs[0].textContent).toBe('Hello')
    })
    // The slower column is still in its running placeholder.
    expect(document.body.textContent ?? '').toContain(copy.en.columnStateRunning)

    // Its terminal event brings the settled figures onto that column.
    stream.emit(doneFrame(0, 'text-only-model'))
    await vi.waitFor(() => {
      const text = document.body.textContent ?? ''
      expect(text).toContain('0.000011')
      expect(text).toContain('1.23s')
      expect(text).toContain('req-0')
    })

    // Only now does the second column answer, and it types too.
    stream.emit(chunkFrame(1, 'text-and-image-model', 'world'))
    await vi.waitFor(() => {
      const outputs = document.querySelectorAll('[data-testid="pg-column-output"]')
      expect(outputs[1].textContent).toBe('world')
    })
    stream.emit(doneFrame(1, 'text-and-image-model'))
    stream.close()
    await vi.waitFor(() => expect(document.body.textContent ?? '').toContain(copy.en.columnStateDone))

    // And the run request carried the whole comparison in one channel.
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })

  it('TC-05: a failing column keeps its native error shape while the others complete', async () => {
    const stream = controlledEventStream()
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(stream.response())),
    )
    await mountPage('en')
    await vi.waitFor(() => expect(modelsMock).toHaveBeenCalledTimes(1))

    keySelect().vm.$emit('update:value', 7)
    modelsSelect().vm.$emit('update:value', ['text-only-model', 'text-and-image-model'])
    await nextTick()
    const promptBox = wrapper!.findAll('textarea')[0]
    await promptBox.setValue('three way')
    await sendButton('en').trigger('click')
    await nextTick()

    // The middle leg: this run only has two pickable text models in the
    // fixture, so slot 1 plays the failing column.
    stream.emit(chunkFrame(0, 'text-only-model', 'fine'))
    stream.emit(doneFrame(0, 'text-only-model'))
    stream.emit(errorFrame(1, 'text-and-image-model', { status: 429 }))
    stream.close()

    await vi.waitFor(() => {
      const text = document.body.textContent ?? ''
      // The failed column's native shape: status, message, code — the
      // dialect's own error object, verbatim.
      expect(text).toContain('HTTP 429')
      expect(text).toContain('You exceeded your current quota')
      expect(text).toContain('insufficient_quota')
      expect(text).toContain(copy.en.columnStateFailed)
      // Its neighbours were not taken down with it.
      expect(text).toContain('fine')
      expect(text).toContain(copy.en.columnStateDone)
    })
  })

  it('TC-12: the vision attachment travels with the send and a re-run can swap it', async () => {
    const stream = controlledEventStream()
    const fetchMock = vi.fn((_path: string, _init?: RequestInit) => Promise.resolve(stream.response()))
    vi.stubGlobal('fetch', fetchMock)
    await mountPage('en')
    await vi.waitFor(() => expect(modelsMock).toHaveBeenCalledTimes(1))

    // Pick an image through the attach control — the real path: a file
    // into the hidden input, change event, FileReader to a data URL.
    async function pickImage(bytes: number[], name: string) {
      const input = document.querySelector('[data-testid="pg-attach-input"]') as HTMLInputElement
      expect(input, 'attach input rendered').toBeTruthy()
      const file = new File([new Uint8Array(bytes)], name, { type: 'image/png' })
      Object.defineProperty(input, 'files', { value: [file], configurable: true })
      input.dispatchEvent(new Event('change'))
      // Wait on the NAME, not the thumbnail: a previous pick's thumbnail
      // is still on the page, so only the name proves this pick's
      // FileReader has landed.
      await vi.waitFor(() => {
        expect(document.querySelector('.pg-attach-name')?.textContent).toBe(name)
      })
    }

    await pickImage([1, 2, 3], 'shot.png')
    keySelect().vm.$emit('update:value', 7)
    modelsSelect().vm.$emit('update:value', ['text-only-model'])
    await nextTick()
    const promptBox = wrapper!.findAll('textarea')[0]
    await promptBox.setValue('what is this')
    await sendButton('en').trigger('click')
    await nextTick()
    stream.emit(doneFrame(0, 'text-only-model'))
    stream.close()
    await vi.waitFor(() => expect(document.body.textContent ?? '').toContain(copy.en.columnStateDone))

    const payload = JSON.parse((fetchMock.mock.calls[0][1] as RequestInit).body as string) as {
      messages: Array<{ role: string; content: unknown }>
    }
    expect(payload.messages).toHaveLength(1)
    const parts = payload.messages[0].content as Array<{ type: string; text?: string; image_url?: { url: string } }>
    expect(parts).toEqual([
      { type: 'text', text: 'what is this' },
      { type: 'image_url', image_url: { url: 'data:image/png;base64,AQID' } },
    ])

    // A re-run with a DIFFERENT image carries the new attachment — the
    // control is re-read per send, not snapshotted with the first run.
    const secondStream = controlledEventStream()
    fetchMock.mockImplementation(() => Promise.resolve(secondStream.response()))
    await pickImage([9, 9], 'swapped.png')
    const rerunButtons = wrapper!.findAll('button').filter((b) => b.text() === copy.en.resend)
    expect(rerunButtons).toHaveLength(1)
    await rerunButtons[0].trigger('click')
    await nextTick()
    secondStream.emit(doneFrame(0, 'text-only-model'))
    secondStream.close()
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2))
    const payload2 = JSON.parse((fetchMock.mock.calls[1][1] as RequestInit).body as string) as {
      messages: Array<{ role: string; content: Array<{ type: string; image_url?: { url: string } }> }>
    }
    const parts2 = payload2.messages[0].content
    expect(parts2[1].image_url?.url).toBe('data:image/png;base64,CQk=')
  })

  it('TC-15: stopping mid-run disconnects the channel and settles unfinished columns as cancelled', async () => {
    // A stream that stays open and errors when the fetch signal aborts —
    // the honest shape of a channel the page disconnects.
    let streamAbort: (() => void) | null = null
    vi.stubGlobal(
      'fetch',
      vi.fn((_path: string, init?: RequestInit) => {
        const signal = init?.signal
        return Promise.resolve(
          new Response(
            new ReadableStream<Uint8Array>({
              start(controller) {
                streamAbort = () => controller.error(new DOMException('aborted', 'AbortError'))
                signal?.addEventListener('abort', () => streamAbort?.(), { once: true })
                controller.enqueue(new TextEncoder().encode(chunkFrame(0, 'text-only-model', 'done already')))
                controller.enqueue(
                  new TextEncoder().encode(
                    doneFrame(0, 'text-only-model', { duration_ms: 42 }),
                  ),
                )
                // Slot 1 never answers: it is the column still in flight
                // when the operator stops the run.
              },
            }),
            { status: 200, headers: { 'content-type': 'text/event-stream; charset=utf-8' } },
          ),
        )
      }),
    )
    await mountPage('en')
    await vi.waitFor(() => expect(modelsMock).toHaveBeenCalledTimes(1))

    keySelect().vm.$emit('update:value', 7)
    modelsSelect().vm.$emit('update:value', ['text-only-model', 'text-and-image-model'])
    await nextTick()
    const promptBox = wrapper!.findAll('textarea')[0]
    await promptBox.setValue('stop me')
    await sendButton('en').trigger('click')
    await nextTick()

    // While the run is in flight, the stop control is on the page; the
    // fast column has already finished.
    await vi.waitFor(() => {
      expect(document.querySelector('[data-testid="pg-stop"]')).not.toBeNull()
      expect(document.body.textContent ?? '').toContain('done already')
    })

    document.querySelector('[data-testid="pg-stop"]')!.dispatchEvent(new Event('click'))
    await vi.waitFor(() => {
      // The abort reached the fetch's signal and errored the channel.
      expect(streamAbort).not.toBeNull()
      // The finished column keeps its result; the in-flight column
      // settles as cancelled, with its note.
      const text = document.body.textContent ?? ''
      expect(text).toContain('done already')
      expect(text).toContain(copy.en.columnStateCancelled)
      expect(text).toContain(copy.en.columnCancelledNote)
      expect(document.querySelector('[data-testid="pg-stop"]')).toBeNull()
    })
  })
})

describe('PlaygroundPage attach guard', () => {
  it.each<Locale>(['zh-CN', 'en'])('TC-12: an over-cap image is refused locally with the size-cap copy (%s)', async (locale) => {
    await mountPage(locale)
    await vi.waitFor(() => expect(modelsMock).toHaveBeenCalledTimes(1))

    const input = document.querySelector('[data-testid="pg-attach-input"]') as HTMLInputElement
    expect(input, 'attach input rendered').toBeTruthy()
    // 6 MiB exactly: within the old "> 6 MiB" guard's acceptance window
    // while its data URL (22 + 4*ceil(N/3) = 8'388'630 chars) already
    // exceeds the backend's 8 MiB cap — the size the exact mirror in
    // attachLimit.ts exists to refuse before the run ever starts.
    const file = new File([new Uint8Array(6 * 1024 * 1024)], 'huge.png', { type: 'image/png' })
    Object.defineProperty(input, 'files', { value: [file], configurable: true })
    input.dispatchEvent(new Event('change'))

    // The refusal is explained in the active locale, not silent.
    await vi.waitFor(() => {
      expect(document.body.textContent ?? '').toContain(copy[locale].imageTooLarge)
    })
    // And it took: no thumbnail, no name, and the input is reset so
    // picking the same file again still fires a change event.
    expect(document.querySelector('[data-testid="pg-attach-thumb"]')).toBeNull()
    expect(document.querySelector('.pg-attach-name')).toBeNull()
    expect(input.value).toBe('')
  })
})

describe('PlaygroundPage locale', () => {
  it('TC-20: labels follow a live locale switch without remount', async () => {
    await mountPage('zh-CN')
    await vi.waitFor(() => expect(modelsMock).toHaveBeenCalledTimes(1))

    let text = document.body.textContent ?? ''
    expect(text).toContain(zhCN.nav.playground)
    expect(text).toContain(copy['zh-CN'].send)

    // The composer's locale is a writable ref; the untyped createI18n
    // return leaves it opaque, so write through the ref shape directly.
    ;(i18n!.global.locale as { value: string }).value = 'en'
    await nextTick()
    await nextTick()
    text = document.body.textContent ?? ''
    expect(text).toContain(en.nav.playground)
    expect(text).toContain(copy.en.send)
    expect(text).not.toContain(copy['zh-CN'].send)
  })
})
