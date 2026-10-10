// @vitest-environment happy-dom
//
// Mounted-DOM coverage for the model edit dialog's capability-metadata
// controls (context length + quick presets, input-modalities multi-select,
// tool/reasoning tri-states, recommended temperature) — the console half of
// the model-capabilities batch:
//
//   - prefill (TC-08): a fully-declared model opens with every control
//     showing its stored value; the presets and the input-modalities
//     vocabulary render
//   - quick preset (TC-08): one click fills the context-length input
//   - temperature bounds (TC-08): the input binds the same [0,2] closed
//     range the playground parameter panel uses
//   - payload set-ness (TC-09): store.update is called with each capability
//     field PRECISELY when the form moved it away from the prefilled row —
//     ① changed to "no" submits supportsTools:'no'; ② toggled away and put
//     back to undeclared (on a row that was undeclared) and ③ never touched
//     both submit the field ABSENT, pinned as two independent cases because
//     under the absent=keep protocol the two scenarios are indistinguishable
//     on the wire; a declared row switched to "unknown" submits 'unknown'
//     (an explicit clear, distinct from absence); numbers submit null to
//     clear; the input list submits [] to clear. Every control also gets a
//     moved-arm of its own: the reasoning select changed submits its value,
//     and the temperature input cleared submits null (mirroring the
//     context-length clear)
//   - untouched save (protocol guard): a form nothing changed closes without
//     any request — absence must not degrade into a noisy full PATCH
//   - legacy empty state (TC-14): an all-null model opens with blank inputs
//     and "Undeclared" selects, no crash
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createI18n } from 'vue-i18n'
import naive, { NInputNumber, NSelect } from 'naive-ui'
import { defineComponent, nextTick } from 'vue'

import type { Model, ModelPatch, TriStateChoice } from '../../api/models'

// TC-09's target: the payload the dialog hands to the store, captured at the
// store boundary (store.update passes its patch verbatim to the API).
// vi.hoisted because the vi.mock factory lifts above every const here.
const { updateMock } = vi.hoisted(() => ({ updateMock: vi.fn() }))
vi.mock('../../store/models', () => ({ useModelsStore: () => ({ update: updateMock }) }))

// Must be imported after the vi.mock factory is registered.
import ModelEditModal from './ModelEditModal.vue'
import en from '../../locales/en'

function modelFixture(overrides: Partial<Model> = {}): Model {
  return {
    id: 7,
    name: 'm1',
    management_status: 1,
    running_status: 'available',
    scheduling_mode: 'failover',
    supports_image_input: null,
    output_modalities: ['text'],
    context_length: null,
    input_modalities: null,
    supports_tools: null,
    supports_reasoning: null,
    temperature: null,
    candidates: [],
    created_at: '2026-09-01T00:00:00Z',
    ...overrides,
  }
}

const Host = defineComponent({
  components: { ModelEditModal },
  template:
    '<NConfigProvider><NMessageProvider><NDialogProvider><ModelEditModal :show="show" :model="model" /></NDialogProvider></NMessageProvider></NConfigProvider>',
  props: {
    model: { type: Object as () => Model, required: true },
    show: { type: Boolean, default: false },
  },
})

let wrapper: VueWrapper | null = null

// The capability-metadata fixture: every field declared, so each prefill
// assertion sees a distinct stored value.
const DECLARED = {
  context_length: 131072,
  input_modalities: ['text', 'image'],
  supports_tools: true,
  supports_reasoning: false,
  temperature: 0.7,
}

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

async function mountModal(model: Model) {
  const i18n = createI18n({ legacy: false, locale: 'en', fallbackLocale: 'en', messages: { en } })
  const pinia = createPinia()
  setActivePinia(pinia)
  wrapper = mount(Host, {
    attachTo: document.body,
    props: { model },
    global: { plugins: [pinia, i18n, naive] },
  })
  // The prefill watch keys on show/model; reproduce the real open transition
  // (show false → true) rather than mounting mid-open.
  await wrapper.setProps({ show: true })
  await nextTick()
  await nextTick()
}

function body(): HTMLElement {
  return document.body
}

function numberInput(controlClass: string): HTMLInputElement {
  const input = body().querySelector<HTMLInputElement>(`.${controlClass} input`)
  expect(input, `${controlClass} input rendered`).toBeTruthy()
  return input!
}

function numberControl(controlClass: string) {
  const control = wrapper!.findAllComponents(NInputNumber).find((c) => c.classes().includes(controlClass))
  expect(control, `${controlClass} component found`).toBeTruthy()
  return control!
}

function selectControl(controlClass: string) {
  const select = wrapper!.findAllComponents(NSelect).find((c) => c.classes().includes(controlClass))
  expect(select, `${controlClass} select found`).toBeTruthy()
  return select!
}

function pickTriState(controlClass: string, value: TriStateChoice) {
  selectControl(controlClass).vm.$emit('update:value', value)
}

function setInputNumber(controlClass: string, value: number | null) {
  numberControl(controlClass).vm.$emit('update:value', value)
}

function clickPreset(label: string) {
  const button = [...body().querySelectorAll<HTMLButtonElement>('.model-edit__context-presets button')].find((b) =>
    (b.textContent ?? '').trim() === label,
  )
  expect(button, `preset ${label} rendered`).toBeTruthy()
  button!.dispatchEvent(new Event('click'))
}

function submit() {
  // The modal's footer teleports to body, so the confirm button is looked
  // up in the document, inside ModalDrawer's actions container.
  const buttons = Array.from(body().querySelectorAll<HTMLButtonElement>('.modal-drawer__actions button'))
  const confirm = buttons.filter((b) => (b.textContent ?? '').includes('Save'))
  expect(confirm.length, 'confirm button rendered').toBeGreaterThan(0)
  confirm[0]!.dispatchEvent(new Event('click'))
}

async function submittedPatch(): Promise<ModelPatch> {
  await vi.waitFor(() => expect(updateMock).toHaveBeenCalledTimes(1))
  return updateMock.mock.calls[0]![1] as ModelPatch
}

beforeEach(() => {
  stubDesktopMatchMedia()
  updateMock.mockReset()
  updateMock.mockResolvedValue(modelFixture())
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  vi.clearAllMocks()
  vi.unstubAllGlobals()
  body().innerHTML = ''
})

describe('ModelEditModal capability metadata — mount (TC-08 / TC-14)', () => {
  it('prefills every control from a fully-declared model', async () => {
    await mountModal(modelFixture(DECLARED))
    expect(numberInput('model-edit__context-input').value).toBe('131072')
    expect(numberInput('model-edit__temperature').value).toBe('0.7')
    // The selects show their stored choice's label in the rendered text.
    expect(selectControl('model-edit__tools').text()).toContain(en.models.triStateYes)
    expect(selectControl('model-edit__reasoning').text()).toContain(en.models.triStateNo)
    expect(selectControl('model-edit__input-modalities').text()).toContain(en.models.inputModalityText)
    expect(selectControl('model-edit__input-modalities').text()).toContain(en.models.inputModalityImage)
  })

  it('renders the four context-length quick presets', async () => {
    await mountModal(modelFixture({}))
    const labels = [...body().querySelectorAll('.model-edit__context-presets button')].map((b) =>
      (b.textContent ?? '').trim(),
    )
    expect(labels).toEqual(['32K', '128K', '200K', '1M'])
  })

  it('one preset click fills the context-length input (1M → 1,048,576)', async () => {
    await mountModal(modelFixture({}))
    clickPreset('1M')
    await nextTick()
    expect(numberInput('model-edit__context-input').value).toBe('1048576')
  })

  it('binds the temperature input to the shared [0,2] closed range', async () => {
    await mountModal(modelFixture({}))
    const temperature = numberControl('model-edit__temperature')
    expect(temperature.props('min')).toBe(0)
    expect(temperature.props('max')).toBe(2)
  })

  it('offers exactly the input-modality vocabulary as a multi-select', async () => {
    await mountModal(modelFixture({}))
    const values = (selectControl('model-edit__input-modalities').props('options') as { value: string }[]).map(
      (o) => o.value,
    )
    expect(values).toEqual(['text', 'image', 'audio', 'video'])
  })

  it('renders a legacy all-undeclared model with blank inputs and Undeclared selects (TC-14)', async () => {
    await mountModal(modelFixture({}))
    expect(numberInput('model-edit__context-input').value).toBe('')
    expect(numberInput('model-edit__temperature').value).toBe('')
    expect(selectControl('model-edit__tools').text()).toContain(en.models.triStateUnknown)
    expect(selectControl('model-edit__reasoning').text()).toContain(en.models.triStateUnknown)
    expect(selectControl('model-edit__input-modalities').text()).not.toContain(en.models.inputModalityImage)
  })
})

describe('ModelEditModal capability metadata — payload set-ness (TC-09)', () => {
  it('① a tri-state switched to "no" submits supportsTools:"no"', async () => {
    await mountModal(modelFixture({}))
    pickTriState('model-edit__tools', 'no')
    await nextTick()
    submit()
    const patch = await submittedPatch()
    expect(patch.supportsTools).toBe('no')
    // Only the moved field rides along; everything else stays absent.
    expect(patch.supportsReasoning).toBeUndefined()
    expect(patch.contextLength).toBeUndefined()
    expect(patch.temperature).toBeUndefined()
    expect(patch.inputModalities).toBeUndefined()
    expect('imageInput' in patch).toBe(false)
  })

  it('② a tri-state toggled away and put back to Undeclared submits the field absent', async () => {
    await mountModal(modelFixture({}))
    pickTriState('model-edit__tools', 'yes')
    await nextTick()
    pickTriState('model-edit__tools', 'unknown')
    await nextTick()
    // Another field moves so the save fires at all — the no-op shortcut
    // below would close the dialog without any request.
    clickPreset('32K')
    await nextTick()
    submit()
    const patch = await submittedPatch()
    expect(patch.supportsTools, 'reverted-to-undeclared rides absent (absent = keep)').toBeUndefined()
    expect(patch.contextLength).toBe(32768)
  })

  it('③ a never-touched tri-state submits the field absent', async () => {
    await mountModal(modelFixture({}))
    // An unrelated field moves so the save fires; the tools select is never
    // interacted with at all.
    setInputNumber('model-edit__temperature', 0.5)
    await nextTick()
    submit()
    const patch = await submittedPatch()
    expect(patch.supportsTools, 'untouched field rides absent (absent = keep)').toBeUndefined()
    expect(patch.supportsReasoning).toBeUndefined()
    expect(patch.inputModalities).toBeUndefined()
    expect(patch.contextLength).toBeUndefined()
    expect(patch.temperature).toBe(0.5)
  })

  it('a DECLARED tri-state switched to "unknown" submits "unknown" — an explicit clear, not an absence', async () => {
    await mountModal(modelFixture({ supports_tools: true }))
    pickTriState('model-edit__tools', 'unknown')
    await nextTick()
    submit()
    const patch = await submittedPatch()
    expect(patch.supportsTools).toBe('unknown')
  })

  it('a tri-state changed on the REASONING select submits that field — the second tri-state gets its own moved-arm', async () => {
    await mountModal(modelFixture({}))
    pickTriState('model-edit__reasoning', 'yes')
    await nextTick()
    submit()
    const patch = await submittedPatch()
    expect(patch.supportsReasoning).toBe('yes')
    // Only the moved field rides along; everything else stays absent.
    expect(patch.supportsTools).toBeUndefined()
    expect(patch.contextLength).toBeUndefined()
    expect(patch.temperature).toBeUndefined()
    expect(patch.inputModalities).toBeUndefined()
  })

  it('clearing a declared context length submits null (clear, not keep)', async () => {
    await mountModal(modelFixture({ context_length: 131072 }))
    setInputNumber('model-edit__context-input', null)
    await nextTick()
    submit()
    const patch = await submittedPatch()
    expect(patch.contextLength).toBeNull()
  })

  it('clearing a declared temperature submits null (clear, not keep)', async () => {
    await mountModal(modelFixture({ temperature: 0.7 }))
    setInputNumber('model-edit__temperature', null)
    await nextTick()
    submit()
    const patch = await submittedPatch()
    expect(patch.temperature).toBeNull()
  })

  it('changing the input modalities submits the array', async () => {
    await mountModal(modelFixture({ input_modalities: ['text'] }))
    selectControl('model-edit__input-modalities').vm.$emit('update:value', ['text', 'image'])
    await nextTick()
    submit()
    const patch = await submittedPatch()
    expect(patch.inputModalities).toEqual(['text', 'image'])
  })

  it('clearing the input modalities submits the empty array (clear back to undeclared)', async () => {
    await mountModal(modelFixture({ input_modalities: ['text'] }))
    selectControl('model-edit__input-modalities').vm.$emit('update:value', [])
    await nextTick()
    submit()
    const patch = await submittedPatch()
    // In-band empty list = clear back to undeclared on the wire.
    expect(patch.inputModalities).toEqual([])
  })

  it('submits nothing at all when the form did not move — closes without a request', async () => {
    await mountModal(modelFixture(DECLARED))
    submit()
    await nextTick()
    await nextTick()
    expect(updateMock, 'an untouched form must not PATCH').not.toHaveBeenCalled()
    // ...and the dialog still closes (the modal component itself emits the
    // close; the Host only passes the prop down).
    const modal = wrapper!.findComponent(ModelEditModal)
    await vi.waitFor(() =>
      expect(modal.emitted('update:show')?.some((args) => args[0] === false)).toBe(true),
    )
  })
})
