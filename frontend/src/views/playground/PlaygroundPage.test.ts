// @vitest-environment happy-dom
//
// Mounted-DOM coverage for the Playground page (acceptance cases
// TC-04/05/06/12/13/15/18/19/20/23/29/34, frontend arms), in BOTH locales
// where copy is the thing under test:
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
//   - TC-29: the empty canvas renders the four-step guide (key → models →
//     prompt → run) through the shared EmptyState's #detail slot, inside
//     the two-pane shell (bench beside canvas)
//   - TC-34: the speech run's per-column voice controls render inside the
//     bench, stacked after the shared parameter area
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { createI18n } from 'vue-i18n'
import naive, { NInputNumber, NSelect, NTabs } from 'naive-ui'
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
  // Two speech models of two different vendors' dictionaries: each
  // carries only its own vendor's voice presets (the picker endpoint
  // derives them from the speech dialect table).
  { name: 'audio-vendor-a', output_modalities: ['audio'], voices: ['alloy', 'echo', 'fable'] },
  { name: 'audio-vendor-b', output_modalities: ['audio'], voices: ['tongtong', 'chuichui'] },
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

// The modality tab group's current value (the controlled half of the tab
// switch; the tests emit the update half directly).
function modalityTabValue(): string | number | undefined {
  return wrapper!.findAllComponents(NTabs)[0].props('value') as string | number | undefined
}

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

    // All four modality tabs are selectable dialects now (video landed
    // with its async-task column). The nav tab carries the disabled state
    // as a class (a disabled pane component is never mounted).
    const tab = (name: string) => document.querySelector(`.n-tabs-tab[data-name="${name}"]`)
    for (const name of ['text', 'image', 'audio', 'video']) {
      expect(tab(name)?.classList.contains('n-tabs-tab--disabled'), `tab ${name} is selectable`).toBe(false)
    }
  })

  it('TC-23: switching to the image tab refilters the picker, drops the cross-modality selection, and swaps the parameter row', async () => {
    await mountPage('en')
    await vi.waitFor(() => expect(modelsMock).toHaveBeenCalledTimes(1))

    // A selection made under the text tab…
    modelsSelect().vm.$emit('update:value', ['text-only-model'])
    await nextTick()

    // …does not survive the switch: the image tab serves image models only.
    const tabs = wrapper!.findAllComponents(NTabs)[0]
    tabs.vm.$emit('update:value', 'image')
    await nextTick()
    expect(modalityTabValue()).toBe('image')
    expect(modelsSelect().props('value')).toEqual([])
    expect(modelsSelect().props('options')!.map((o) => o.value)).toEqual(['image-only-model', 'text-and-image-model'])

    // The parameter row is the image family: size and count replace the
    // text sampling controls, and the vision attach control is gone (an
    // image run's input is the prompt itself).
    expect(document.querySelector('[data-testid="pg-image-params"]')).not.toBeNull()
    expect(document.querySelector('[data-testid="pg-attach-input"]')).toBeNull()
    expect(wrapper!.findAll('textarea')).toHaveLength(1) // the prompt alone

    // And switching back restores the text family.
    tabs.vm.$emit('update:value', 'text')
    await nextTick()
    expect(document.querySelector('[data-testid="pg-image-params"]')).toBeNull()
    expect(document.querySelector('[data-testid="pg-attach-input"]')).not.toBeNull()
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
      request: { messages: Array<{ role: string; content: string }> }
      params: { system?: string; temperature?: number; max_tokens?: number }
    }
    expect(payload.key_id).toBe(7)
    expect(payload.modality).toBe('text')
    expect(payload.models).toEqual(['text-only-model', 'text-and-image-model'])
    expect(payload.request.messages).toEqual([{ role: 'user', content: 'compare this prompt' }])
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
      request: { messages: Array<{ role: string; content: string }> }
      params: { system?: string; temperature?: number; max_tokens?: number }
    }
    expect(payload2.models).toEqual(['text-only-model'])
    expect(payload2.request.messages).toEqual([{ role: 'user', content: 'revised prompt for the re-run' }])
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

describe('PlaygroundPage video modality', () => {
  // A done frame for a finished video column: the LATE settlement's
  // figures and the console playback reference (no chunks ever precede
  // it; a video column is submit-then-wait, not streaming text).
  function videoDoneFrame(slot: number, model: string, over: Record<string, unknown> = {}): string {
    return `event: done\ndata: ${JSON.stringify({
      slot,
      model,
      status: 200,
      request_id: `req-vid-${slot}`,
      usage: { input_tokens: 0, output_tokens: 0, seconds: 4 },
      cost_micros: 2000000,
      cost_known: true,
      duration_ms: 890,
      video: {
        url: `/api/admin/playground/video/req-vid-${slot}`,
        content_type: 'video/mp4',
        seconds: 4,
      },
      ...over,
    })}\n\n`
  }

  function submittedFrame(slot: number, model: string): string {
    return `event: submitted\ndata: ${JSON.stringify({ slot, model, request_id: `req-vid-${slot}` })}\n\n`
  }

  function waitStoppedFrame(slot: number, model: string): string {
    return `event: wait_stopped\ndata: ${JSON.stringify({ slot, model, request_id: `req-vid-${slot}` })}\n\n`
  }

  // Selects the key and the video model, sets the prompt and the two
  // video axes, clicks run — the payload is the assertion surface. The
  // fetch mock is returned so the arms can assert on its calls (the run
  // POST first, the stop-wait POST second).
  async function driveVideoRun(stream: { response: () => Response }) {
    // The fetch mock routes by path: the run POST opens the controlled
    // aggregate stream, while the stop-wait control answers a plain JSON
    // envelope (routing both at one stream would let the second response
    // steal the stream's controller — exactly the aliasing the real
    // transports never share).
    const fetchMock = vi.fn((path: string, _init?: RequestInit) =>
      path === '/api/admin/playground/video/stop-wait'
        ? Promise.resolve(
            new Response(JSON.stringify({ code: 0, message: 'ok', data: { stopped: true }, timestamp: 1 }), {
              status: 200,
              headers: { 'content-type': 'application/json' },
            }),
          )
        : Promise.resolve(stream.response()),
    )
    vi.stubGlobal('fetch', fetchMock)
    await mountPage('en')
    await vi.waitFor(() => expect(modelsMock).toHaveBeenCalledTimes(1))
    wrapper!.findAllComponents(NTabs)[0].vm.$emit('update:value', 'video')
    await nextTick()
    // Under the video tab the picker offers only video models.
    expect(modelsSelect().props('options')!.map((o) => o.value)).toEqual(['video-only-model'])
    keySelect().vm.$emit('update:value', 7)
    modelsSelect().vm.$emit('update:value', ['video-only-model'])
    await nextTick()
    // The shared parameter row is the video family: seconds and size.
    expect(document.querySelector('[data-testid="pg-video-params"]')).not.toBeNull()
    findSelect('pg-video-seconds').vm.$emit('update:value', 8)
    findSelect('pg-video-size').vm.$emit('update:value', '1280x720')
    await nextTick()
    const promptBox = wrapper!.findAll('textarea')[0]
    await promptBox.setValue('a calico cat on stage')
    await sendButton('en').trigger('click')
    await nextTick()
    return fetchMock
  }

  it('TC-11: the video run carries the prompt and both dialect axes; the column shows its render clock while running, then the finished clip with the settled seconds and cost', async () => {
    const stream = controlledEventStream()
    const fetchMock = await driveVideoRun(stream)

    expect(fetchMock).toHaveBeenCalledTimes(1)
    const payload = JSON.parse((fetchMock.mock.calls[0][1] as RequestInit).body as string) as {
      key_id: number
      modality: string
      models: string[]
      request: { prompt: string; messages?: unknown }
      voices?: unknown
      params: { seconds?: number; size?: string }
    }
    expect(payload.modality).toBe('video')
    expect(payload.models).toEqual(['video-only-model'])
    expect(payload.request.prompt).toBe('a calico cat on stage')
    expect(payload.request.messages).toBeUndefined()
    expect(payload.voices).toBeUndefined()
    expect(payload.params).toEqual({ seconds: 8, size: '1280x720' })

    // Two-state in progress: the running column counts its render clock
    // and shows no clip; before the submit acknowledgement there is no
    // stop-waiting control either (nothing to address yet).
    expect(document.querySelector('[data-testid="pg-video-timer"]')).not.toBeNull()
    expect(document.querySelector('[data-testid="pg-video-card"]')).toBeNull()
    expect(document.querySelector('[data-testid="pg-stop-wait-0"]')).toBeNull()

    // The submit acknowledgement names the audit id — the stop-waiting
    // control appears against it, while the column keeps running.
    stream.emit(submittedFrame(0, 'video-only-model'))
    await vi.waitFor(() => {
      expect(document.querySelector('[data-testid="pg-stop-wait-0"]')).not.toBeNull()
    })
    expect(document.body.textContent ?? '').toContain(copy.en.columnStateRunning)

    // Terminal: the finished clip renders through the console proxy, with
    // the delivered-seconds usage and the late-settled cost.
    stream.emit(videoDoneFrame(0, 'video-only-model'))
    stream.close()
    await vi.waitFor(() => expect(document.querySelector('[data-testid="pg-video-card"]')).not.toBeNull())
    const card = document.querySelector('[data-testid="pg-video-card"] video') as HTMLVideoElement
    expect(card.getAttribute('src')).toBe('/api/admin/playground/video/req-vid-0')
    expect(card.getAttribute('type')).toBe('video/mp4')
    const text = document.body.textContent ?? ''
    expect(text).toContain(copy.en.usageSeconds.replace('{count}', '4'))
    expect(text).toContain('2.000000')
  })

  it('TC-11 stop-wait: clicking stop-wait addresses the column’s audit id; wait_stopped renders the honest stopped-waiting note, not a cancellation', async () => {
    const stream = controlledEventStream()
    const fetchMock = await driveVideoRun(stream)

    stream.emit(submittedFrame(0, 'video-only-model'))
    await vi.waitFor(() => expect(document.querySelector('[data-testid="pg-stop-wait-0"]')).not.toBeNull())

    // The control posts the stop-wait request with the column's audit id
    // (the apiFetch path — same origin, JSON envelope, session cookie).
    const stopButton = document.querySelector('[data-testid="pg-stop-wait-0"]') as HTMLElement
    stopButton.click()
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2))
    expect(fetchMock.mock.calls[1][0]).toBe('/api/admin/playground/video/stop-wait')
    const stopBody = JSON.parse((fetchMock.mock.calls[1][1] as RequestInit).body as string) as { request_id: string }
    expect(stopBody).toEqual({ request_id: 'req-vid-0' })

    // The channel's acknowledgement settles the column into the honest
    // stopped-waiting reading — the task is NOT cancelled, the cost is
    // the request log's to tell.
    stream.emit(waitStoppedFrame(0, 'video-only-model'))
    stream.close()
    await vi.waitFor(() => expect(document.querySelector('[data-testid="pg-column-stopped"]')).not.toBeNull())
    const text = document.body.textContent ?? ''
    expect(text).toContain(copy.en.columnStateStopped)
    expect(text).toContain(copy.en.columnStoppedNote)
    // Never a cancelled reading, never an invented clip.
    expect(document.querySelector('[data-testid="pg-column-cancelled"]')).toBeNull()
    expect(document.querySelector('[data-testid="pg-video-card"]')).toBeNull()
  })

  it('TC-15 video arm: stopping mid-run disconnects the channel and settles the unfinished video column as stopped waiting, not cancelled', async () => {
    // A stream that stays open and errors when the fetch signal aborts —
    // the honest shape of a channel the page disconnects. The submit is
    // acknowledged (the stop-wait control is on the column) but no
    // terminal event ever arrives: the operator stops the WHOLE run, the
    // video leg of the page-level disconnect.
    let streamAbort: (() => void) | null = null
    const fetchMock = vi.fn((_path: string, init?: RequestInit) => {
      const signal = init?.signal
      return Promise.resolve(
        new Response(
          new ReadableStream<Uint8Array>({
            start(controller) {
              streamAbort = () => controller.error(new DOMException('aborted', 'AbortError'))
              signal?.addEventListener('abort', () => streamAbort?.(), { once: true })
              controller.enqueue(new TextEncoder().encode(submittedFrame(0, 'video-only-model')))
              // No done, no wait_stopped: the column is still in flight
              // when the operator stops the run.
            },
          }),
          { status: 200, headers: { 'content-type': 'text/event-stream; charset=utf-8' } },
        ),
      )
    })
    vi.stubGlobal('fetch', fetchMock)
    await mountPage('en')
    await vi.waitFor(() => expect(modelsMock).toHaveBeenCalledTimes(1))

    wrapper!.findAllComponents(NTabs)[0].vm.$emit('update:value', 'video')
    await nextTick()
    keySelect().vm.$emit('update:value', 7)
    modelsSelect().vm.$emit('update:value', ['video-only-model'])
    await nextTick()
    const promptBox = wrapper!.findAll('textarea')[0]
    await promptBox.setValue('stop my wait')
    await sendButton('en').trigger('click')
    await nextTick()

    // The run is in flight: the submit acknowledgement landed (its audit
    // id names the column) and both exits are on the page — the run-level
    // stop and the column's stop-waiting control.
    await vi.waitFor(() => {
      expect(document.querySelector('[data-testid="pg-stop-wait-0"]')).not.toBeNull()
      expect(document.querySelector('[data-testid="pg-stop"]')).not.toBeNull()
    })

    // The run-level stop aborts the channel — it is NOT the stop-waiting
    // control, so no stop-wait request may leave the page.
    document.querySelector('[data-testid="pg-stop"]')!.dispatchEvent(new Event('click'))
    await vi.waitFor(() => {
      // The abort reached the fetch's signal and errored the channel.
      expect(streamAbort).not.toBeNull()
      // The unfinished video column settles as STOPPED WAITING — the
      // honest reading of a disconnect for a task that was never
      // cancellable — with its note, never the cancelled reading, and no
      // clip invented for a task nobody watched finish.
      expect(document.querySelector('[data-testid="pg-column-stopped"]')).not.toBeNull()
      const text = document.body.textContent ?? ''
      expect(text).toContain(copy.en.columnStateStopped)
      expect(text).toContain(copy.en.columnStoppedNote)
      expect(document.querySelector('[data-testid="pg-column-cancelled"]')).toBeNull()
      expect(document.querySelector('[data-testid="pg-video-card"]')).toBeNull()
      expect(document.querySelector('[data-testid="pg-stop"]')).toBeNull()
    })
    expect(fetchMock).toHaveBeenCalledTimes(1)
    expect(fetchMock.mock.calls.some(([path]) => path === '/api/admin/playground/video/stop-wait')).toBe(false)
  })

  it('TC-15 video arm, pre-submit window: a column interrupted before its submit was acknowledged settles as cancelled, not a full-cost stopped wait', async () => {
    // The submit round-trip is still on the wire when the operator stops
    // the run: one column's submit acknowledgement landed, the other's
    // never did. The acknowledged column holds a live task nobody can
    // terminate — stopped-waiting is its honest reading. The other's
    // exchange was cancelled by the shared cancel choreography — no
    // task exists, nothing will bill — so a "task keeps running at full
    // cost" note on it would be a money-side false promise.
    let streamAbort: (() => void) | null = null
    const fetchMock = vi.fn((_path: string, init?: RequestInit) => {
      const signal = init?.signal
      return Promise.resolve(
        new Response(
          new ReadableStream<Uint8Array>({
            start(controller) {
              streamAbort = () => controller.error(new DOMException('aborted', 'AbortError'))
              signal?.addEventListener('abort', () => streamAbort?.(), { once: true })
              // Only slot 0's submit is acknowledged; slot 1's exchange
              // never resolves — it is the pre-submit window.
              controller.enqueue(new TextEncoder().encode(submittedFrame(0, 'video-only-model')))
            },
          }),
          { status: 200, headers: { 'content-type': 'text/event-stream; charset=utf-8' } },
        ),
      )
    })
    modelsMock.mockResolvedValueOnce({
      list: [...modelFixture, { name: 'video-second-model', output_modalities: ['video'] }],
    })
    vi.stubGlobal('fetch', fetchMock)
    await mountPage('en')
    await vi.waitFor(() => expect(modelsMock).toHaveBeenCalledTimes(1))

    wrapper!.findAllComponents(NTabs)[0].vm.$emit('update:value', 'video')
    await nextTick()
    keySelect().vm.$emit('update:value', 7)
    modelsSelect().vm.$emit('update:value', ['video-only-model', 'video-second-model'])
    await nextTick()
    const promptBox = wrapper!.findAll('textarea')[0]
    await promptBox.setValue('stop mid-submit')
    await sendButton('en').trigger('click')
    await nextTick()

    // The acknowledged column carries the stop-waiting control; the
    // still-submitting one has nothing to address yet.
    await vi.waitFor(() => {
      expect(document.querySelector('[data-testid="pg-stop-wait-0"]')).not.toBeNull()
    })
    expect(document.querySelector('[data-testid="pg-stop-wait-1"]')).toBeNull()

    // The run-level stop aborts the channel mid-submit.
    document.querySelector('[data-testid="pg-stop"]')!.dispatchEvent(new Event('click'))
    await vi.waitFor(() => {
      expect(streamAbort).not.toBeNull()
      // Each column gets its own honest reading, in its own slot: the
      // acknowledged one is stopped WAITING (its task keeps running at
      // full cost), the never-submitted one is cancelled — its exchange
      // died with the run, no task exists to bill.
      const cols = document.querySelectorAll('.pg-column')
      expect(cols[0].querySelector('[data-testid="pg-column-stopped"]')).not.toBeNull()
      expect(cols[1].querySelector('[data-testid="pg-column-cancelled"]')).not.toBeNull()
      expect(document.querySelectorAll('[data-testid="pg-column-stopped"]')).toHaveLength(1)
      expect(document.querySelectorAll('[data-testid="pg-column-cancelled"]')).toHaveLength(1)
      const text = document.body.textContent ?? ''
      expect(text).toContain(copy.en.columnStateStopped)
      expect(text).toContain(copy.en.columnStoppedNote)
      expect(text).toContain(copy.en.columnStateCancelled)
      expect(text).toContain(copy.en.columnCancelledNote)
      // No clip is invented for either column.
      expect(document.querySelector('[data-testid="pg-video-card"]')).toBeNull()
      expect(document.querySelector('[data-testid="pg-stop"]')).toBeNull()
    })
    // And the run-level stop never fired a stop-wait request.
    expect(fetchMock.mock.calls.some(([path]) => path === '/api/admin/playground/video/stop-wait')).toBe(false)
  })

  it('TC-22 video arm: a failed video column keeps its native error placeholder while the neighbour renders its clip', async () => {
    const stream = controlledEventStream()
    // Two video models, driven in one run from the start.
    modelsMock.mockResolvedValueOnce({
      list: [...modelFixture, { name: 'video-second-model', output_modalities: ['video'] }],
    })
    vi.stubGlobal(
      'fetch',
      vi.fn((_path: string, _init?: RequestInit) => Promise.resolve(stream.response())),
    )
    await mountPage('en')
    await vi.waitFor(() => expect(modelsMock).toHaveBeenCalledTimes(1))
    wrapper!.findAllComponents(NTabs)[0].vm.$emit('update:value', 'video')
    await nextTick()
    keySelect().vm.$emit('update:value', 7)
    modelsSelect().vm.$emit('update:value', ['video-only-model', 'video-second-model'])
    await nextTick()
    const promptBox = wrapper!.findAll('textarea')[0]
    await promptBox.setValue('a calico cat on stage')
    await sendButton('en').trigger('click')
    await nextTick()

    stream.emit(submittedFrame(0, 'video-only-model'))
    stream.emit(submittedFrame(1, 'video-second-model'))
    stream.emit(videoDoneFrame(0, 'video-only-model'))
    stream.emit(`event: error\ndata: ${JSON.stringify({
      slot: 1,
      model: 'video-second-model',
      status: 422,
      request_id: 'req-vid-1',
      error: { code: 'InternalServiceError', message: 'the render was refused' },
    })}\n\n`)
    stream.close()
    await vi.waitFor(() => expect(document.querySelectorAll('[data-testid="pg-video-card"]')).toHaveLength(1))
    const text = document.body.textContent ?? ''
    expect(text).toContain('HTTP 422')
    expect(text).toContain('the render was refused')
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
// A done frame for an audio column: no chunks ever precede it (binary
// audio has no deltas), the usage counts settled characters, and the
// response rides as the playback reference instead of an inline body.
function audioDoneFrame(slot: number, model: string, over: Record<string, unknown> = {}): string {
  return `event: done\ndata: ${JSON.stringify({
    slot,
    model,
    status: 200,
    request_id: `req-aud-${slot}`,
    usage: { input_tokens: 0, output_tokens: 0, characters: 9 },
    cost_micros: 18,
    cost_known: true,
    duration_ms: 2100,
    audio: {
      url: `/api/admin/playground/audio/req-aud-${slot}`,
      content_type: 'audio/mpeg',
      bytes: 2048,
    },
    ...over,
  })}\n\n`
}

// A done frame for an image column: no chunks ever precede it (a JSON
// answer has no deltas), the usage counts delivered images, and the
// response carries the artifacts — whole (object) or cut (string + flag).
function imageDoneFrame(slot: number, model: string, over: Record<string, unknown> = {}): string {
  return `event: done\ndata: ${JSON.stringify({
    slot,
    model,
    status: 200,
    request_id: `req-img-${slot}`,
    usage: { input_tokens: 4, output_tokens: 0, images: 2 },
    cost_micros: 80000,
    cost_known: true,
    duration_ms: 4321,
    response: {
      data: [
        { url: 'https://img.example/one.png', revised_prompt: 'a red cat' },
        { b64_json: 'QUJD' },
      ],
    },
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
      request: { messages: Array<{ role: string; content: unknown }> }
    }
    expect(payload.request.messages).toHaveLength(1)
    const parts = payload.request.messages[0].content as Array<{ type: string; text?: string; image_url?: { url: string } }>
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
      request: { messages: Array<{ role: string; content: Array<{ type: string; image_url?: { url: string } }> }> }
    }
    const parts2 = payload2.request.messages[0].content
    expect(parts2[1].image_url?.url).toBe('data:image/png;base64,CQk=')
  })

  it('TC-09: an image run sends the prompt and both axes; done columns render image cards with per-image usage', async () => {
    const stream = controlledEventStream()
    const fetchMock = vi.fn((_path: string, _init?: RequestInit) => Promise.resolve(stream.response()))
    vi.stubGlobal('fetch', fetchMock)
    await mountPage('en')
    await vi.waitFor(() => expect(modelsMock).toHaveBeenCalledTimes(1))

    // The image tab, an image model, the prompt, and both exposed axes.
    wrapper!.findAllComponents(NTabs)[0].vm.$emit('update:value', 'image')
    await nextTick()
    keySelect().vm.$emit('update:value', 7)
    modelsSelect().vm.$emit('update:value', ['image-only-model', 'text-and-image-model'])
    await nextTick()
    const promptBox = wrapper!.findAll('textarea')[0]
    await promptBox.setValue('a red cat')
    findSelect('pg-image-size').vm.$emit('update:value', '1024x1536')
    wrapper!.findAllComponents(NInputNumber)[0].vm.$emit('update:value', 2)
    await nextTick()
    await sendButton('en').trigger('click')
    await nextTick()

    // The run request is the image modality's own shape: the prompt (no
    // messages array), and the size/count axes — nothing from the text
    // family rides along.
    expect(fetchMock).toHaveBeenCalledTimes(1)
    const payload = JSON.parse((fetchMock.mock.calls[0][1] as RequestInit).body as string) as {
      modality: string
      models: string[]
      request: { prompt: string; messages?: unknown }
      params: { size?: string; n?: number; system?: string; temperature?: number; max_tokens?: number }
    }
    expect(payload.modality).toBe('image')
    expect(payload.models).toEqual(['image-only-model', 'text-and-image-model'])
    expect(payload.request.prompt).toBe('a red cat')
    expect(payload.request.messages).toBeUndefined()
    expect(payload.params).toEqual({ size: '1024x1536', n: 2 })

    // Two-state: before any terminal event, both image columns sit in the
    // running placeholder — an image column has no streaming state.
    expect(document.body.textContent ?? '').toContain(copy.en.columnStateRunning)
    expect(document.querySelector('[data-testid="pg-image-card"]')).toBeNull()

    // The first column lands: two cards — the remote reference as-is, the
    // inline base64 pixels as a data URL — with the per-image usage line.
    stream.emit(imageDoneFrame(0, 'image-only-model'))
    await vi.waitFor(() => {
      expect(document.querySelectorAll('[data-testid="pg-image-card"]')).toHaveLength(2)
    })
    const cards = [...document.querySelectorAll('[data-testid="pg-image-card"]')] as HTMLImageElement[]
    expect(cards[0].getAttribute('src')).toBe('https://img.example/one.png')
    expect(cards[0].getAttribute('alt')).toBe('a red cat')
    expect(cards[1].getAttribute('src')).toBe('data:image/png;base64,QUJD')
    const text = document.body.textContent ?? ''
    expect(text).toContain(copy.en.usageImages.replace('{count}', '2'))
    expect(text).toContain('0.080000')
    expect(text).toContain('4.32s')
    // The second image column is still running — its cards arrive only
    // with its own terminal event.
    expect(text).toContain(copy.en.columnStateRunning)

    stream.emit(imageDoneFrame(1, 'text-and-image-model'))
    stream.close()
    await vi.waitFor(() => expect(document.querySelectorAll('[data-testid="pg-image-card"]')).toHaveLength(4))
  })

  it('TC-22 image arm: a failed image column keeps its native error placeholder while the neighbour renders cards', async () => {
    const stream = controlledEventStream()
    vi.stubGlobal(
      'fetch',
      vi.fn((_path: string, _init?: RequestInit) => Promise.resolve(stream.response())),
    )
    await mountPage('en')
    await vi.waitFor(() => expect(modelsMock).toHaveBeenCalledTimes(1))

    wrapper!.findAllComponents(NTabs)[0].vm.$emit('update:value', 'image')
    await nextTick()
    keySelect().vm.$emit('update:value', 7)
    modelsSelect().vm.$emit('update:value', ['image-only-model', 'text-and-image-model'])
    await nextTick()
    const promptBox = wrapper!.findAll('textarea')[0]
    await promptBox.setValue('one fails one renders')
    await sendButton('en').trigger('click')
    await nextTick()

    stream.emit(errorFrame(0, 'image-only-model'))
    stream.emit(imageDoneFrame(1, 'text-and-image-model'))
    stream.close()

    await vi.waitFor(() => {
      const text = document.body.textContent ?? ''
      // The failed column's native shape — no cards, no running
      // placeholder, the dialect's own error object verbatim.
      expect(text).toContain('HTTP 429')
      expect(text).toContain('You exceeded your current quota')
      expect(text).toContain(copy.en.columnStateFailed)
      // Its neighbour delivered its images all the same.
      expect(document.querySelectorAll('[data-testid="pg-image-card"]')).toHaveLength(2)
    })
  })

  it('TC-17: an over-cap image answer shows the truncation note and the capped prefix, never silence', async () => {
    const stream = controlledEventStream()
    vi.stubGlobal(
      'fetch',
      vi.fn((_path: string, _init?: RequestInit) => Promise.resolve(stream.response())),
    )
    await mountPage('en')
    await vi.waitFor(() => expect(modelsMock).toHaveBeenCalledTimes(1))

    wrapper!.findAllComponents(NTabs)[0].vm.$emit('update:value', 'image')
    await nextTick()
    keySelect().vm.$emit('update:value', 7)
    modelsSelect().vm.$emit('update:value', ['image-only-model'])
    await nextTick()
    const promptBox = wrapper!.findAll('textarea')[0]
    await promptBox.setValue('huge answer')
    await sendButton('en').trigger('click')
    await nextTick()

    // The cut shape: the byte prefix as a plain string plus the flag —
    // the channel never claims completeness it does not have.
    const cutPrefix = '{"created":1,"data":[{"b64_json":"AAAA'
    stream.emit(
      imageDoneFrame(0, 'image-only-model', {
        response: cutPrefix,
        response_truncated: true,
      }),
    )
    stream.close()

    await vi.waitFor(() => {
      const text = document.body.textContent ?? ''
      expect(document.querySelector('[data-testid="pg-column-truncated"]')).not.toBeNull()
      expect(text).toContain(copy.en.responseTruncated)
      // The capped prefix stays inspectable — the answer is marked as
      // cut, not swallowed whole.
      expect(text).toContain(cutPrefix)
      // And no card is invented from a cut body.
      expect(document.querySelector('[data-testid="pg-image-card"]')).toBeNull()
      // The figures that do not depend on the body bytes still land.
      expect(text).toContain(copy.en.usageImages.replace('{count}', '2'))
    })
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

describe('PlaygroundPage audio modality', () => {
  // The speech run's per-model voice control, located by its DOM testid
  // (every voice select carries the same marker class).
  function voiceSelect(model: string): SelectWrapper {
    const sel = wrapper!.findAllComponents(NSelect).find((s) => s.attributes('data-testid') === `pg-voice-${model}`)
    expect(sel, `voice select for ${model} rendered`).toBeTruthy()
    return sel as unknown as SelectWrapper
  }

  it('TC-10: per-column voice dropdowns list only their own vendor’s voices; the run carries one voice per column; done renders an audio player with character usage', async () => {
    const stream = controlledEventStream()
    const fetchMock = vi.fn((_path: string, _init?: RequestInit) => Promise.resolve(stream.response()))
    vi.stubGlobal('fetch', fetchMock)
    await mountPage('en')
    await vi.waitFor(() => expect(modelsMock).toHaveBeenCalledTimes(1))

    wrapper!.findAllComponents(NTabs)[0].vm.$emit('update:value', 'audio')
    await nextTick()
    // Under the audio tab the picker offers only speech models.
    expect(modelsSelect().props('options')!.map((o) => o.value)).toEqual(['audio-vendor-a', 'audio-vendor-b'])

    keySelect().vm.$emit('update:value', 7)
    modelsSelect().vm.$emit('update:value', ['audio-vendor-a', 'audio-vendor-b'])
    await nextTick()

    // The voice control hangs on each column and lists ONLY that column's
    // model's vendors — vendor A's dictionary never offers vendor B's ids
    // and vice versa, so a cross-vendor voice slip cannot even be picked.
    const optionsA = voiceSelect('audio-vendor-a').props('options')!
    const optionsB = voiceSelect('audio-vendor-b').props('options')!
    expect(optionsA.map((o) => o.value)).toEqual(['alloy', 'echo', 'fable'])
    expect(optionsB.map((o) => o.value)).toEqual(['tongtong', 'chuichui'])
    // Each column defaults to its own list's first preset.
    expect(voiceSelect('audio-vendor-a').props('value')).toBe('alloy')
    expect(voiceSelect('audio-vendor-b').props('value')).toBe('tongtong')

    // One column picks a different voice of its OWN dictionary — the two
    // columns run with two different vendor voices, the exact shape a
    // shared-voice design would 400 on.
    voiceSelect('audio-vendor-a').vm.$emit('update:value', 'echo')
    await nextTick()
    const promptBox = wrapper!.findAll('textarea')[0]
    await promptBox.setValue('a red cat')
    // The shared parameter row is the audio family's one axis.
    findSelect('pg-audio-format').vm.$emit('update:value', 'mp3')
    await nextTick()
    await sendButton('en').trigger('click')
    await nextTick()

    expect(fetchMock).toHaveBeenCalledTimes(1)
    const payload = JSON.parse((fetchMock.mock.calls[0][1] as RequestInit).body as string) as {
      key_id: number
      modality: string
      models: string[]
      request: { prompt: string; messages?: unknown }
      voices: Record<string, string>
      params: { format?: string; size?: string; system?: string }
    }
    expect(payload.modality).toBe('audio')
    expect(payload.models).toEqual(['audio-vendor-a', 'audio-vendor-b'])
    expect(payload.request.prompt).toBe('a red cat')
    expect(payload.request.messages).toBeUndefined()
    expect(payload.voices).toEqual({ 'audio-vendor-a': 'echo', 'audio-vendor-b': 'tongtong' })
    expect(payload.params).toEqual({ format: 'mp3' })

    // Two-state: before any terminal event, both speech columns sit in
    // the running placeholder — no player exists yet.
    expect(document.body.textContent ?? '').toContain(copy.en.columnStateRunning)
    expect(document.querySelector('[data-testid="pg-audio-player"]')).toBeNull()

    stream.emit(audioDoneFrame(0, 'audio-vendor-a'))
    await vi.waitFor(() => {
      expect(document.querySelectorAll('[data-testid="pg-audio-player"]')).toHaveLength(1)
    })
    const player = document.querySelector('[data-testid="pg-audio-player"] audio') as HTMLAudioElement
    expect(player.getAttribute('src')).toBe('/api/admin/playground/audio/req-aud-0')
    expect(player.getAttribute('type')).toBe('audio/mpeg')
    // The character usage line lands with the player.
    const text0 = document.body.textContent ?? ''
    expect(text0).toContain(copy.en.usageCharacters.replace('{count}', '9'))
    expect(text0).toContain('0.000018')
    // The second column is still running — its player arrives only with
    // its own terminal event.
    expect(text0).toContain(copy.en.columnStateRunning)

    stream.emit(audioDoneFrame(1, 'audio-vendor-b'))
    stream.close()
    await vi.waitFor(() => expect(document.querySelectorAll('[data-testid="pg-audio-player"]')).toHaveLength(2))
  })

  it('TC-10 arm: a column with no listed presets still runs once a custom voice id is typed (taggable control)', async () => {
    modelsMock.mockResolvedValue({
      list: [...modelFixture, { name: 'audio-unknown-vendor', output_modalities: ['audio'] }],
    })
    const stream = controlledEventStream()
    vi.stubGlobal(
      'fetch',
      vi.fn((_path: string, _init?: RequestInit) => Promise.resolve(stream.response())),
    )
    await mountPage('en')
    await vi.waitFor(() => expect(modelsMock).toHaveBeenCalledTimes(1))

    wrapper!.findAllComponents(NTabs)[0].vm.$emit('update:value', 'audio')
    await nextTick()
    keySelect().vm.$emit('update:value', 7)
    modelsSelect().vm.$emit('update:value', ['audio-unknown-vendor'])
    await nextTick()

    // No presets: the control renders empty and the send is refused with
    // the voice warning rather than a doomed run.
    const sel = voiceSelect('audio-unknown-vendor')
    expect(sel.props('options')).toEqual([])
    expect(sel.props('value')).toBe('')
    const promptBox = wrapper!.findAll('textarea')[0]
    await promptBox.setValue('hello')
    await sendButton('en').trigger('click')
    await nextTick()
    expect(document.body.textContent ?? '').toContain(copy.en.needVoice)
    expect(document.querySelector('[data-testid="pg-columns"]')).toBeNull()

    // A typed-in id (a cloned voice) is accepted: the taggable control
    // does not gate on the preset list.
    sel.vm.$emit('update:value', 'voice-clone-8f2')
    await nextTick()
    await sendButton('en').trigger('click')
    await nextTick()
    const payload = JSON.parse(
      ((vi.mocked(fetch) as unknown as ReturnType<typeof vi.fn>).mock.calls[0][1] as RequestInit).body as string,
    ) as { voices: Record<string, string> }
    expect(payload.voices).toEqual({ 'audio-unknown-vendor': 'voice-clone-8f2' })
    stream.emit(audioDoneFrame(0, 'audio-unknown-vendor'))
    stream.close()
    await vi.waitFor(() => expect(document.body.textContent ?? '').toContain(copy.en.columnStateDone))
  })

  it('TC-22 audio arm: a failed speech column keeps its native error placeholder while the neighbour renders its player', async () => {
    const stream = controlledEventStream()
    vi.stubGlobal(
      'fetch',
      vi.fn((_path: string, _init?: RequestInit) => Promise.resolve(stream.response())),
    )
    await mountPage('en')
    await vi.waitFor(() => expect(modelsMock).toHaveBeenCalledTimes(1))

    wrapper!.findAllComponents(NTabs)[0].vm.$emit('update:value', 'audio')
    await nextTick()
    keySelect().vm.$emit('update:value', 7)
    modelsSelect().vm.$emit('update:value', ['audio-vendor-a', 'audio-vendor-b'])
    await nextTick()
    const promptBox = wrapper!.findAll('textarea')[0]
    await promptBox.setValue('one fails one speaks')
    await sendButton('en').trigger('click')
    await nextTick()

    stream.emit(errorFrame(0, 'audio-vendor-a', { status: 400 }))
    stream.emit(audioDoneFrame(1, 'audio-vendor-b'))
    stream.close()

    await vi.waitFor(() => {
      const text = document.body.textContent ?? ''
      // The failed column's native shape — no player, no running
      // placeholder, the dialect's own error object verbatim.
      expect(text).toContain('HTTP 400')
      expect(text).toContain('You exceeded your current quota')
      expect(text).toContain(copy.en.columnStateFailed)
      // Its neighbour delivered playable audio all the same.
      expect(document.querySelectorAll('[data-testid="pg-audio-player"]')).toHaveLength(1)
    })
    // Exactly one player for the whole run: the failed column never
    // invents audio.
    expect(document.querySelectorAll('[data-testid="pg-audio-player"]')).toHaveLength(1)
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

describe('PlaygroundPage workbench shell', () => {
  it.each<Locale>(['zh-CN', 'en'])(
    'TC-29: the empty canvas renders the four-step guide in the shared EmptyState #detail slot, inside the bench/canvas shell (%s)',
    async (locale) => {
      await mountPage(locale)
      await vi.waitFor(() => expect(modelsMock).toHaveBeenCalledTimes(1))

      // The two-pane shell exists, addressed by the convention classes the
      // batch's e2e relies on (layout containers deliberately carry no
      // testid).
      const bench = document.querySelector('.pg-bench')
      const canvas = document.querySelector('.pg-canvas')
      expect(document.querySelector('.pg-workbench')).not.toBeNull()
      expect(bench, 'bench pane rendered').not.toBeNull()
      expect(canvas, 'canvas pane rendered').not.toBeNull()

      // The bench carries the whole run setup: the modality segment, the
      // key and model selects, the prompt — all inside the left pane.
      expect(bench!.querySelector('.pg-modality-tabs')).not.toBeNull()
      expect(bench!.querySelector('.pg-key-select')).not.toBeNull()
      expect(bench!.querySelector('.pg-models-select')).not.toBeNull()
      expect(bench!.querySelector('textarea')).not.toBeNull()

      // The run controls are the bench's pinned foot — its LAST block, so
      // the button sits at the pane's bottom edge (TC-32's DOM half; the
      // stays-visible-while-scrolling half is the e2e's).
      const actions = bench!.querySelector('.pg-actions')
      expect(actions, 'pinned actions foot rendered').not.toBeNull()
      expect(actions!.textContent).toContain(copy[locale].send)
      expect(bench!.lastElementChild).toBe(actions)

      // The empty canvas is the shared EmptyState with the four-step guide
      // riding its #detail slot — no bespoke empty styling, four steps in
      // bench order: key → models → prompt → run.
      const detail = canvas!.querySelector('.empty-state__detail')
      expect(detail, 'EmptyState #detail block rendered in the canvas').not.toBeNull()
      const steps = [...detail!.querySelectorAll('li')].map((li) => li.textContent)
      expect(steps).toEqual([
        copy[locale].emptyStepKey,
        copy[locale].emptyStepModels,
        copy[locale].emptyStepPrompt,
        copy[locale].emptyStepRun,
      ])
    },
  )

  it('TC-34: per-column voice controls stack inside the bench after the shared parameter area', async () => {
    await mountPage('en')
    await vi.waitFor(() => expect(modelsMock).toHaveBeenCalledTimes(1))

    wrapper!.findAllComponents(NTabs)[0].vm.$emit('update:value', 'audio')
    await nextTick()

    // No models selected yet: the per-column block has nothing to show.
    expect(document.querySelector('[data-testid="pg-voices"]')).toBeNull()

    modelsSelect().vm.$emit('update:value', ['audio-vendor-a', 'audio-vendor-b'])
    await nextTick()

    // With a speech selection the voices live INSIDE the bench, one
    // compact control per selected column.
    const bench = document.querySelector('.pg-bench')!
    const voices = bench.querySelector('[data-testid="pg-voices"]')
    expect(voices, 'voices block rendered inside the bench').not.toBeNull()
    expect(voices!.querySelectorAll('.pg-voice-select')).toHaveLength(2)

    // And they follow the shared parameter area — the per-column exception
    // reads after the shared axes it excepts, not before them.
    const params = bench.querySelector('[data-testid="pg-audio-params"]')
    expect(params, 'audio parameter area rendered inside the bench').not.toBeNull()
    expect(
      params!.compareDocumentPosition(voices!) & Node.DOCUMENT_POSITION_FOLLOWING,
      'voices render after the parameter area',
    ).toBeTruthy()
  })
})
