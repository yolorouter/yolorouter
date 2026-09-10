// @vitest-environment happy-dom
//
// Mounted-DOM coverage for the CC-Switch export dialog's model-picking mode:
// the load state machine (ready / error+retry / legacy), the planner's
// degradation into manual entry, and the zero-await confirm payload the
// opening page hands to the deep link.
import { afterEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { defineComponent, h, nextTick, ref } from 'vue'
import { createI18n } from 'vue-i18n'

import { APIError } from '../../api/client'
import { getAPIKeyPlaintext, discoverGatewayModels, type APIKey } from '../../api/apiKeys'

vi.mock('../../api/apiKeys', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api/apiKeys')>()
  return {
    ...actual,
    getAPIKeyPlaintext: vi.fn(),
    discoverGatewayModels: vi.fn(),
  }
})

const plaintextMock = vi.mocked(getAPIKeyPlaintext)
const discoverMock = vi.mocked(discoverGatewayModels)

// Must be imported after the vi.mock factory is registered.
import CCSwitchImportModal from './CCSwitchImportModal.vue'
import en from '../../locales/en'

const i18n = createI18n({ legacy: false, locale: 'en', messages: { en } })

const KEY: APIKey = {
  id: 42,
  key_prefix: 'sk-yr-abc12345678',
  user_id: 7,
  owner_username: 'alice',
  remark: '',
  status: 1,
  display_status: 'active',
  expires_at: null,
  rpm_limit: null,
  tpm_limit: null,
  concurrency_limit: null,
  budget_limit_micros: null,
  budget_spent_micros: 0,
  allow_all_models: true,
  model_ids: [],
  custom_system_prompt_enabled_override: false,
  custom_system_prompt_enabled: false,
  custom_system_prompt: '',
  compress_enabled_override: false,
  compress_enabled: false,
} as unknown as APIKey

const CATALOG = [
  { id: 1, name: 'glm-4.7', running_status: 'available' },
  { id: 2, name: 'kling-video', running_status: 'unavailable' },
]

function makeHost(catalog: typeof CATALOG | null) {
  return defineComponent({
    render() {
      return h(CcsModalHost, { catalog })
    },
  })
}

// Binds the modal open with the fixed key so tests never fight v-model.
const CcsModalHost = defineComponent({
  props: { catalog: { type: Array as () => typeof CATALOG | null, default: null } },
  setup(props) {
    const show = ref(true)
    return () =>
      h(CCSwitchImportModal, {
        show: show.value,
        'onUpdate:show': (v: boolean) => {
          show.value = v
        },
        apiKeyRow: KEY,
        catalog: props.catalog,
        onConfirm: () => {},
      })
  },
})

afterEach(() => {
  document.body.innerHTML = ''
  plaintextMock.mockReset()
  discoverMock.mockReset()
})

function clickConfirm() {
  const btn = [...document.body.querySelectorAll('button')].find((b) =>
    (b.textContent ?? '').includes(en.ccswitch.confirmLaunchButton),
  )
  expect(btn, 'confirm button rendered').toBeTruthy()
  btn!.dispatchEvent(new Event('click'))
  return btn!
}

describe('CCSwitchImportModal (model mode)', () => {
  it('loads, preselects the first catalog-available model, and confirms the pair', async () => {
    plaintextMock.mockResolvedValue({ plaintext_key: 'sk-test-123' })
    discoverMock.mockResolvedValue(['kling-video', 'glm-4.7'])

    const wrapper = mount(makeHost(CATALOG), {
      global: { plugins: [i18n] },
      attachTo: document.body,
    })
    await vi.waitFor(() =>
      expect(document.body.textContent ?? '').toContain('glm-4.7'),
    )

    const body = document.body.textContent ?? ''
    expect(body).toContain(en.ccswitch.modalTitle)
    expect(body).toContain('alice (#42)')
    expect(body).toContain(KEY.key_prefix)

    // Both discovered names are offered (the unselected one lives in the
    // closed dropdown's option list, asserted through the component tree).
    const select = wrapper.getComponent(CCSwitchImportModal).findComponent({ name: 'Select' })
    const options = select.props('options') as Array<{ label: string; value: string }>
    expect(options.map((o) => o.value)).toEqual(['kling-video', 'glm-4.7'])

    clickConfirm()
    await nextTick()
    const events = wrapper.getComponent(CCSwitchImportModal).emitted<{ apiKey?: string; model?: string }[]>('confirm')
    expect(events).toEqual([[{ apiKey: 'sk-test-123', model: 'glm-4.7' }]])
    wrapper.unmount()
  })

  it('degrades a legacy key to the catalog fallback with the paste-by-hand notice', async () => {
    plaintextMock.mockRejectedValue(new APIError(11016))
    discoverMock.mockResolvedValue([])

    const wrapper = mount(makeHost(CATALOG), {
      global: { plugins: [i18n] },
      attachTo: document.body,
    })
    await vi.waitFor(() =>
      expect(document.body.textContent ?? '').toContain(en.ccswitch.plaintextUnavailable),
    )
    // Discovery must not run without a credential to authenticate with.
    expect(discoverMock).not.toHaveBeenCalled()
    // Catalog backfill still gives the picker its options.
    expect(document.body.textContent ?? '').toContain('glm-4.7')

    clickConfirm()
    await nextTick()
    const events = wrapper.getComponent(CCSwitchImportModal).emitted<{ apiKey?: string; model?: string }[]>('confirm')
    expect(events).toEqual([[{ apiKey: undefined, model: 'glm-4.7' }]])
    wrapper.unmount()
  })

  it('degrades a member discovery failure to manual entry, empty allowed', async () => {
    plaintextMock.mockResolvedValue({ plaintext_key: 'sk-test-123' })
    discoverMock.mockRejectedValue(new Error('gateway down'))

    const wrapper = mount(makeHost(null), {
      global: { plugins: [i18n] },
      attachTo: document.body,
    })
    await vi.waitFor(() =>
      expect(document.body.textContent ?? '').toContain(en.ccswitch.modelManualHint),
    )
    // The degradation is surfaced, not silent: the notice explains why the
    // list is second-best and offers the retry that might restore it.
    expect(document.body.textContent ?? '').toContain(en.ccswitch.discoveryFailedHint)

    const input = document.body.querySelector('input')
    expect(input, 'manual input rendered').toBeTruthy()
    input!.value = 'glm-4.7'
    input!.dispatchEvent(new Event('input'))

    clickConfirm()
    await nextTick()
    const events = wrapper.getComponent(CCSwitchImportModal).emitted<{ apiKey?: string; model?: string }[]>('confirm')
    expect(events).toEqual([[{ apiKey: 'sk-test-123', model: 'glm-4.7' }]])
    wrapper.unmount()
  })

  it('shows an in-modal error with retry for transient reveal failures', async () => {
    plaintextMock.mockRejectedValueOnce(new APIError(9999, undefined))
    plaintextMock.mockResolvedValueOnce({ plaintext_key: 'sk-test-123' })
    discoverMock.mockResolvedValue(['glm-4.7'])

    const wrapper = mount(makeHost(CATALOG), {
      global: { plugins: [i18n] },
      attachTo: document.body,
    })
    await vi.waitFor(() =>
      expect(document.body.textContent ?? '').toContain(en.ccswitch.retry),
    )
    // Nothing to confirm against: the confirm button is withheld.
    const confirmBtn = [...document.body.querySelectorAll('button')].find((b) =>
      (b.textContent ?? '').includes(en.ccswitch.confirmLaunchButton),
    )
    expect(confirmBtn!.hasAttribute('disabled')).toBe(true)

    const retryBtn = [...document.body.querySelectorAll('button')].find((b) =>
      (b.textContent ?? '').includes(en.ccswitch.retry),
    )
    retryBtn!.dispatchEvent(new Event('click'))
    await vi.waitFor(() =>
      expect(document.body.textContent ?? '').toContain('glm-4.7'),
    )
    expect(plaintextMock).toHaveBeenCalledTimes(2)
    wrapper.unmount()
  })
})
