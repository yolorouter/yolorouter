// @vitest-environment happy-dom
//
// Mounted-DOM coverage for the preset/base_url contract in the create-provider
// dialog:
//
//   - the dialog opens with the YoloRouter Cloud preset applied, whose extra
//     protocols (anthropic/gemini/responses) are enabled with EMPTY urls,
//     meaning "serve them off base_url"
//   - editing base_url away from the preset's address voids that premise: the
//     empty-url extras fall away and the highlight moves to the custom card —
//     editing a preset in place must not silently declare all four dialects
//     at the new address, where a single-dialect upstream's key could never
//     pass the all-destination verification gate
//   - an extra the admin typed an explicit URL into is theirs and survives
//   - keeping base_url at the preset's address preserves the preset intent
//     (every extra riding along)
//   - typing the preset's URL back does NOT re-apply the preset — only
//     clicking a card does; that click must re-apply the preset in full,
//     rebuilding the extras over the watcher's cleared state
import { afterEach, describe, expect, it, vi, type Mock } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createI18n } from 'vue-i18n'
import naive from 'naive-ui'
import { defineComponent, nextTick } from 'vue'

import { useProvidersStore } from '../../store/providers'
import type { ProtocolConfigModel } from '../../utils/providerProtocol'
import type { Provider } from '../../api/providers'
import zhCN from '../../locales/zh-CN'

// Must be imported after the modules under test are registered.
import NewProviderModal from './NewProviderModal.vue'
import ProtocolConfigFields from './ProtocolConfigFields.vue'

const PRESET_BASE_URL = 'https://api.yolorouter.com/v1'
const CUSTOM_BASE_URL = 'https://api.deepseek.com/v1'
// What the default preset serializes to while its empty-url extras stand.
const PRESET_SERIALIZED = '{"anthropic":"","gemini":"","responses":""}'

function createdProvider(): Provider {
  return {
    id: 9,
    name: 'DeepSeek',
    provider_type: 'openai',
    base_url: CUSTOM_BASE_URL,
    proxy_mode: '',
    proxy_url: '',
    note: '',
    management_status: 1,
    running_status: 'available',
    keys: [],
    created_at: '2026-01-01T00:00:00Z',
    protocol_endpoints: '',
  }
}

const Host = defineComponent({
  components: { NewProviderModal },
  template:
    '<NConfigProvider><NMessageProvider><NewProviderModal :show="show" /></NMessageProvider></NConfigProvider>',
  props: { show: { type: Boolean, default: false } },
})

let wrapper: VueWrapper | null = null
let createSpy: Mock

async function mountModal() {
  const i18n = createI18n({
    legacy: false,
    locale: 'zh-CN',
    fallbackLocale: 'zh-CN',
    messages: { 'zh-CN': zhCN },
  })
  const pinia = createPinia()
  setActivePinia(pinia)
  // The same store instance the modal's setup resolves; the spy on its create
  // captures the exact payload the form serializes at submit.
  createSpy = vi.spyOn(useProvidersStore(), 'create').mockResolvedValue(createdProvider())
  wrapper = mount(Host, {
    attachTo: document.body,
    global: { plugins: [pinia, i18n, naive] },
  })
  // The prefill watch keys on show; in the real app the dialog opens AFTER
  // mount (show false → true), which is exactly what applies the default
  // preset. Reproduce that transition rather than mounting mid-open.
  await wrapper.setProps({ show: true })
  await nextTick()
  await nextTick()
}

// The dialog's plain n-input fields, in form order: name, base URL, key label,
// key plaintext (the note is a textarea and the selects are no n-inputs).
function formInputs(): HTMLInputElement[] {
  return Array.from(document.body.querySelectorAll<HTMLInputElement>('.n-input input'))
}

function setPlainInput(input: HTMLInputElement, value: string) {
  input.value = value
  input.dispatchEvent(new Event('input'))
}

// Whatever the required rules need beyond what the default preset prefilled
// (base URL and test model come prefilled; the key fields never do).
function fillKeyFields() {
  const inputs = formInputs()
  expect(inputs.length, 'the four plain inputs rendered').toBe(4)
  expect(inputs[1]!.placeholder).toBe('https://api.example.com/v1')
  expect(inputs[3]!.type).toBe('password')
  setPlainInput(inputs[0]!, 'DeepSeek')
  setPlainInput(inputs[2]!, 'primary key')
  setPlainInput(inputs[3]!, 'sk-12345678')
}

async function setBaseUrl(value: string) {
  setPlainInput(formInputs()[1]!, value)
  await nextTick()
}

function presetCard(text: string): HTMLButtonElement {
  const cards = Array.from(document.body.querySelectorAll<HTMLButtonElement>('.preset-card'))
  const card = cards.find((c) => (c.textContent ?? '').includes(text))
  expect(card, `preset card "${text}" rendered`).toBeTruthy()
  return card!
}

async function submit() {
  // The modal's footer teleports to body, so the confirm button is looked
  // up in the document, inside ModalDrawer's actions container.
  const buttons = Array.from(document.body.querySelectorAll<HTMLButtonElement>('.modal-drawer__actions button'))
  const confirm = buttons.filter((b) => (b.textContent ?? '').includes(zhCN.providers.save))
  expect(confirm.length, 'confirm button rendered').toBeGreaterThan(0)
  confirm[0]!.dispatchEvent(new Event('click'))
}

function createPayload(): Record<string, unknown> {
  expect(createSpy).toHaveBeenCalledTimes(1)
  return createSpy.mock.calls[0]![0] as Record<string, unknown>
}

afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  vi.restoreAllMocks()
  document.body.innerHTML = ''
})

describe('NewProviderModal preset vs edited base_url', () => {
  it('opens with the default preset applied at its own address', async () => {
    await mountModal()
    expect(formInputs()[1]!.value).toBe(PRESET_BASE_URL)
    expect(presetCard('YoloRouter Cloud').classList.contains('active')).toBe(true)
  })

  it('editing base_url away from the preset drops the empty-url extras and highlights custom', async () => {
    await mountModal()
    fillKeyFields()
    await setBaseUrl(CUSTOM_BASE_URL)
    await submit()
    await vi.waitFor(() => expect(createSpy).toHaveBeenCalledTimes(1))
    const payload = createPayload()
    expect(payload.base_url).toBe(CUSTOM_BASE_URL)
    // The preset's "reuse base_url" premise died with its address; a
    // single-dialect upstream must not inherit three phantom dialects.
    expect(payload.protocol_endpoints).toBe('')
    // The UI must match what was saved: the preset card is no longer in
    // charge.
    expect(presetCard(zhCN.providers.presetCustom).classList.contains('active')).toBe(true)
    expect(presetCard('YoloRouter Cloud').classList.contains('active')).toBe(false)
  })

  it('keeping base_url at the preset preserves the preset-owned extras', async () => {
    await mountModal()
    fillKeyFields()
    await submit()
    await vi.waitFor(() => expect(createSpy).toHaveBeenCalledTimes(1))
    expect(createPayload().protocol_endpoints).toBe(PRESET_SERIALIZED)
  })

  it('an extra the admin typed a URL into survives the base_url divergence', async () => {
    await mountModal()
    // Type an explicit URL into one extra row (the anthropic endpoint) of the
    // preset-filled editor, as the ProtocolConfigFields input would report
    // it — the other two rows stay enabled with empty urls.
    const fields = wrapper!.findComponent(ProtocolConfigFields)
    const edited = JSON.parse(JSON.stringify(fields.props('modelValue'))) as ProtocolConfigModel
    expect(edited.endpoints.anthropic.enabled, 'the preset enabled the anthropic row').toBe(true)
    expect(edited.endpoints.anthropic.url).toBe('')
    edited.endpoints.anthropic = { enabled: true, url: 'https://anthropic.example.com' }
    fields.vm.$emit('update:modelValue', edited)
    await nextTick()
    fillKeyFields()
    await setBaseUrl(CUSTOM_BASE_URL)
    await submit()
    await vi.waitFor(() => expect(createSpy).toHaveBeenCalledTimes(1))
    // The typed extra is the admin's own and stays; the empty-url extras,
    // whose premise was the preset's address, fall away.
    expect(createPayload().protocol_endpoints).toBe('{"anthropic":"https://anthropic.example.com"}')
  })

  it('typing the preset URL back does not re-apply the preset', async () => {
    await mountModal()
    fillKeyFields()
    await setBaseUrl(CUSTOM_BASE_URL)
    await setBaseUrl(PRESET_BASE_URL)
    await submit()
    await vi.waitFor(() => expect(createSpy).toHaveBeenCalledTimes(1))
    // No reverse magic: the extras were already dropped and stay dropped even
    // though the address matches the preset again.
    expect(createPayload().protocol_endpoints).toBe('')
    // The highlight stays on custom: only clicking a card re-applies a preset.
    expect(presetCard(zhCN.providers.presetCustom).classList.contains('active')).toBe(true)
    expect(presetCard('YoloRouter Cloud').classList.contains('active')).toBe(false)
  })

  it('clicking the preset card after a divergence re-applies its full extras', async () => {
    await mountModal()
    fillKeyFields()
    await setBaseUrl(CUSTOM_BASE_URL)
    // Divergence voided the preset: extras dropped, custom card in charge.
    expect(presetCard(zhCN.providers.presetCustom).classList.contains('active')).toBe(true)
    // The re-apply gesture is the click: selectPreset must rebuild the whole
    // endpoint set over the watcher's cleared state, not partially recover.
    presetCard('YoloRouter Cloud').dispatchEvent(new Event('click'))
    await nextTick()
    await submit()
    await vi.waitFor(() => expect(createSpy).toHaveBeenCalledTimes(1))
    expect(createPayload().protocol_endpoints).toBe(PRESET_SERIALIZED)
    expect(presetCard('YoloRouter Cloud').classList.contains('active')).toBe(true)
    expect(presetCard(zhCN.providers.presetCustom).classList.contains('active')).toBe(false)
  })
})
