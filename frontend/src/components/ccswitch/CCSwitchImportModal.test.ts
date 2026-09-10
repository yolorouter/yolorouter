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
import { NSelect } from 'naive-ui'

import { APIError } from '../../api/client'
import {
  getAPIKeyPlaintext,
  discoverGatewayModels,
  listAPIKeys,
  type APIKey,
} from '../../api/apiKeys'

vi.mock('../../api/apiKeys', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api/apiKeys')>()
  return {
    ...actual,
    getAPIKeyPlaintext: vi.fn(),
    discoverGatewayModels: vi.fn(),
    listAPIKeys: vi.fn(),
  }
})

const plaintextMock = vi.mocked(getAPIKeyPlaintext)
const discoverMock = vi.mocked(discoverGatewayModels)
const listKeysMock = vi.mocked(listAPIKeys)

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
  created_at: '2026-09-01T00:00:00Z',
  updated_at: '2026-09-01T00:00:00Z',
}

const CATALOG = [
  { id: 1, name: 'glm-4.7', running_status: 'available' },
  { id: 2, name: 'kling-video', running_status: 'unavailable' },
]

// Binds the modal open with the row passed as a prop, so tests never fight
// v-model, and a test can switch rows (close-then-reopen, useRowModal
// semantics) via setProps.
const CcsModalHost = defineComponent({
  props: {
    catalog: { type: Array as () => typeof CATALOG | null, default: null },
    row: { type: Object as () => APIKey, required: true },
  },
  setup(props) {
    const show = ref(true)
    return () =>
      h(CCSwitchImportModal, {
        show: show.value,
        'onUpdate:show': (v: boolean) => {
          show.value = v
        },
        apiKeyRow: props.row,
        catalog: props.catalog,
        onConfirm: () => {},
      })
  },
})

afterEach(() => {
  document.body.innerHTML = ''
  plaintextMock.mockReset()
  discoverMock.mockReset()
  listKeysMock.mockReset()
})

// Buttons live inside the teleported modal, so they are reachable through
// the shared body, not the host wrapper's element tree.
function findButtonByText(text: string): HTMLButtonElement | undefined {
  return [...document.body.querySelectorAll('button')].find((b) =>
    (b.textContent ?? '').includes(text),
  )
}

function clickConfirm() {
  const btn = findButtonByText(en.common.confirm)
  expect(btn, 'confirm button rendered').toBeTruthy()
  btn!.dispatchEvent(new Event('click'))
  return btn!
}

describe('CCSwitchImportModal (model mode)', () => {
  it('loads, preselects the first catalog-available model, and confirms the pair', async () => {
    plaintextMock.mockResolvedValue({ plaintext_key: 'sk-test-123' })
    discoverMock.mockResolvedValue(['kling-video', 'glm-4.7'])

    const wrapper = mount(CcsModalHost, {
      props: { row: KEY, catalog: CATALOG },
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
    const select = wrapper.getComponent(CCSwitchImportModal).findComponent(NSelect)
    const options = select.props('options') as Array<{
      label: string
      value: string
      render: () => unknown
    }>
    expect(options.map((o) => o.value)).toEqual(['kling-video', 'glm-4.7'])
    // The admin catalog annotates: annotated options render rich labels
    // (VNode), unlike the member view's plain strings.
    expect(typeof options[0].render()).toBe('object')

    clickConfirm()
    await nextTick()
    const events = wrapper.getComponent(CCSwitchImportModal).emitted<{ apiKey?: string; model?: string }[]>('confirm')
    expect(events).toEqual([[{ apiKey: 'sk-test-123', model: 'glm-4.7' }]])
    wrapper.unmount()
  })

  it('degrades a legacy key to the catalog fallback with the paste-by-hand notice', async () => {
    plaintextMock.mockRejectedValue(new APIError(11016))
    discoverMock.mockResolvedValue([])

    const wrapper = mount(CcsModalHost, {
      props: { row: KEY, catalog: CATALOG },
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

    const wrapper = mount(CcsModalHost, {
      props: { row: KEY, catalog: null },
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

    // The notice's retry actually rewires into a full reload: plaintext is
    // prefetched again, not just the hint re-rendered. Clicked BEFORE
    // confirm — after it the modal is closed and, under the production
    // useRowModal wiring, the cleared row would stop any reload; a
    // post-confirm click would test a state production cannot reach.
    const retryBtn = findButtonByText(en.ccswitch.retry)
    expect(retryBtn, 'discovery retry rendered').toBeTruthy()
    retryBtn!.dispatchEvent(new Event('click'))
    // Wait for the reload to reach readiness, not just to start: while the
    // second load is in flight the confirm button is (correctly) withheld.
    // The reload also clears the manual input (a fresh plan may preselect),
    // so the choice is re-entered after it settles.
    await vi.waitFor(() => {
      expect(plaintextMock).toHaveBeenCalledTimes(2)
      expect(discoverMock).toHaveBeenCalledTimes(2)
      expect(findButtonByText(en.common.confirm)!.hasAttribute('disabled')).toBe(false)
    })
    const inputAfterRetry = document.body.querySelector('input')
    inputAfterRetry!.value = 'glm-4.7'
    inputAfterRetry!.dispatchEvent(new Event('input'))

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

    const wrapper = mount(CcsModalHost, {
      props: { row: KEY, catalog: CATALOG },
      global: { plugins: [i18n] },
      attachTo: document.body,
    })
    await vi.waitFor(() =>
      expect(document.body.textContent ?? '').toContain(en.ccswitch.retry),
    )
    // Nothing to confirm against: the confirm button is withheld.
    expect(findButtonByText(en.common.confirm)!.hasAttribute('disabled')).toBe(true)

    findButtonByText(en.ccswitch.retry)!.dispatchEvent(new Event('click'))
    await vi.waitFor(() =>
      expect(document.body.textContent ?? '').toContain('glm-4.7'),
    )
    expect(plaintextMock).toHaveBeenCalledTimes(2)
    wrapper.unmount()
  })

  it('drops a stale load\'s plaintext when it resolves after the row switched', async () => {
    // Row A's reveal is parked on a deferred; row B's resolves instantly.
    // A resolving LAST must not overwrite B's credential — the confirm
    // would then emit B's profile name carrying A's key.
    let resolveA!: (v: { plaintext_key: string }) => void
    plaintextMock.mockImplementationOnce(
      () => new Promise((resolve) => {
        resolveA = resolve
      }),
    )
    plaintextMock.mockImplementationOnce(() =>
      Promise.resolve({ plaintext_key: 'sk-key-B' }),
    )
    discoverMock.mockResolvedValue(['glm-4.7'])

    const wrapper = mount(CcsModalHost, {
      props: { row: KEY, catalog: CATALOG },
      global: { plugins: [i18n] },
      attachTo: document.body,
    })
    await vi.waitFor(() => expect(plaintextMock).toHaveBeenCalledTimes(1))

    // Switch to row B while A's reveal is still parked: the watch reloads
    // for B (loadId bumped), exactly like close-then-reopen in production.
    const rowB: APIKey = { ...KEY, id: 43, owner_username: 'bob' }
    await wrapper.setProps({ row: rowB })
    // B's readiness, not just its start: the confirm gate lifting means the
    // plan for B is in place.
    await vi.waitFor(() =>
      expect(
        findButtonByText(en.common.confirm)!.hasAttribute('disabled'),
      ).toBe(false),
    )

    // A's answer lands after B is ready — it must be dropped. One nextTick
    // flushes A's continuation (plain microtasks) before the click reads
    // the payload, so a missing guard would surface as A's key below.
    resolveA({ plaintext_key: 'sk-key-A' })
    await nextTick()

    clickConfirm()
    await nextTick()
    const events = wrapper.getComponent(CCSwitchImportModal).emitted<{ apiKey?: string; model?: string }[]>('confirm')
    expect(events).toEqual([[{ apiKey: 'sk-key-B', model: 'glm-4.7' }]])
    wrapper.unmount()
  })
})

// --- key mode (models page) ------------------------------------------------

import { createRouter, createMemoryHistory } from 'vue-router'

const MODEL_ROW = { id: 7, name: 'glm-4.7' }

// The modal's empty-state CTA navigates; a minimal memory router both
// silences vue-router's missing-provider warning and makes the navigation
// itself assertable. The bare `/` route keeps the router's initial
// location from warning about having no match.
function makeRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: { render: () => null } },
      { path: '/api-keys', component: { render: () => null } },
    ],
  })
}

function ownKey(id: number, over: Partial<APIKey> = {}): APIKey {
  return {
    ...KEY,
    id,
    key_prefix: `sk-yr-k${id}prefix0000`,
    remark: '',
    owner_username: 'alice',
    display_status: 'active',
    allow_all_models: false,
    model_ids: [MODEL_ROW.id],
    ...over,
  }
}

// Binds the modal open in KEY mode with the fixed model row and the login
// username the picker filters by.
const CcsKeyModeHost = defineComponent({
  setup() {
    const show = ref(true)
    return () =>
      h(CCSwitchImportModal, {
        show: show.value,
        'onUpdate:show': (v: boolean) => {
          show.value = v
        },
        mode: 'key',
        modelRow: MODEL_ROW,
        ownerUsername: 'alice',
        onConfirm: () => {},
      })
  },
})

function keyPage(list: APIKey[], total = list.length) {
  return { total, page: 1, page_size: list.length, list }
}

describe('CCSwitchImportModal (key mode)', () => {
  it('lists only own in-scope active keys, preselects the first, confirms the pair', async () => {
    plaintextMock.mockResolvedValue({ plaintext_key: 'sk-first' })
    // Mixed page: the third key is out of scope and must not be offered.
    // The second own key carries no remark — its option falls back to the
    // key prefix.
    listKeysMock.mockResolvedValueOnce(
      keyPage([
        ownKey(1, { remark: '主力' }),
        ownKey(5),
        ownKey(2, { model_ids: [99] }),
        ownKey(3, { owner_username: 'bob', allow_all_models: true }),
      ]),
    )

    const wrapper = mount(CcsKeyModeHost, {
      global: { plugins: [i18n, makeRouter()] },
      attachTo: document.body,
    })
    await vi.waitFor(() =>
      expect(document.body.textContent ?? '').toContain('主力'),
    )
    // Server-side filter narrows traffic to the one routable status.
    expect(listKeysMock).toHaveBeenCalledWith(
      expect.objectContaining({ status: 'active', page: 1, pageSize: 200 }),
    )

    const select = wrapper.getComponent(CCSwitchImportModal).findComponent(NSelect)
    const options = select.props('options') as Array<{ label: string; value: number }>
    expect(options).toEqual([
      { label: '主力', value: 1 },
      { label: 'sk-yr-k5prefix0000', value: 5 },
    ])

    clickConfirm()
    await nextTick()
    const events = wrapper.getComponent(CCSwitchImportModal).emitted<{ apiKey?: string; model?: string }[]>('confirm')
    expect(events).toEqual([[{ apiKey: 'sk-first', model: 'glm-4.7' }]])
    wrapper.unmount()
  })

  it('pages until the reported total is collected, ignoring drift duplicates', async () => {
    plaintextMock.mockResolvedValue({ plaintext_key: 'sk-second-page' })
    // Page 2 re-lists page 1's key (created/revoked drift between fetches):
    // the dedupe must keep the dropdown duplicate-free while the total
    // still gets collected.
    listKeysMock.mockResolvedValueOnce(keyPage([ownKey(1)], 2))
    listKeysMock.mockResolvedValueOnce(keyPage([ownKey(1), ownKey(2, { allow_all_models: true })], 2))

    const wrapper = mount(CcsKeyModeHost, {
      global: { plugins: [i18n, makeRouter()] },
      attachTo: document.body,
    })
    await vi.waitFor(() => expect(listKeysMock).toHaveBeenCalledTimes(2))
    await vi.waitFor(() =>
      expect(findButtonByText(en.common.confirm)!.hasAttribute('disabled')).toBe(false),
    )
    // Preselect stays the FIRST compatible key across pages; the re-listed
    // key appears exactly once.
    expect(plaintextMock).toHaveBeenCalledWith(1)
    const options = wrapper
      .getComponent(CCSwitchImportModal)
      .findComponent(NSelect)
      .props('options') as Array<{ value: number }>
    expect(options.map((o) => o.value)).toEqual([1, 2])
    wrapper.unmount()
  })

  it('withholds confirm across a selection switch until the new key settles', async () => {
    // Key 1's reveal resolves instantly (gate armed); key 2's is parked.
    // Switching must DROP the gate until key 2 settles, and key 1's
    // plaintext must not survive into the confirm payload.
    let resolveSecond!: (v: { plaintext_key: string }) => void
    plaintextMock.mockImplementationOnce(() =>
      Promise.resolve({ plaintext_key: 'sk-key-1' }),
    )
    plaintextMock.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          resolveSecond = resolve
        }),
    )
    listKeysMock.mockResolvedValueOnce(keyPage([ownKey(1), ownKey(2)]))

    const wrapper = mount(CcsKeyModeHost, {
      global: { plugins: [i18n, makeRouter()] },
      attachTo: document.body,
    })
    // Preselected key 1 settles → confirm arms.
    await vi.waitFor(() =>
      expect(findButtonByText(en.common.confirm)!.hasAttribute('disabled')).toBe(false),
    )

    // Switch to key 2: the gate must drop synchronously with the switch,
    // before key 2's reveal has any chance to answer.
    wrapper.getComponent(CCSwitchImportModal).findComponent(NSelect).vm.$emit('update:value', 2)
    await nextTick()
    expect(findButtonByText(en.common.confirm)!.hasAttribute('disabled')).toBe(true)

    resolveSecond({ plaintext_key: 'sk-key-2' })
    await vi.waitFor(() =>
      expect(findButtonByText(en.common.confirm)!.hasAttribute('disabled')).toBe(false),
    )

    clickConfirm()
    await nextTick()
    const events = wrapper.getComponent(CCSwitchImportModal).emitted<{ apiKey?: string; model?: string }[]>('confirm')
    expect(events).toEqual([[{ apiKey: 'sk-key-2', model: 'glm-4.7' }]])
    wrapper.unmount()
  })

  it('shows the empty state with a create-key way out when nothing qualifies', async () => {
    listKeysMock.mockResolvedValueOnce(keyPage([]))
    const router = makeRouter()

    const wrapper = mount(CcsKeyModeHost, {
      global: { plugins: [i18n, router] },
      attachTo: document.body,
    })
    await vi.waitFor(() =>
      expect(document.body.textContent ?? '').toContain(en.ccswitch.keysEmptyTitle),
    )
    expect(findButtonByText(en.common.confirm)!.hasAttribute('disabled')).toBe(true)

    // The CTA actually navigates to where keys are made.
    findButtonByText(en.apiKeys.createButton)!.dispatchEvent(new Event('click'))
    await vi.waitFor(() => expect(router.currentRoute.value.path).toBe('/api-keys'))
    wrapper.unmount()
  })

  it('surfaces a transient selected-key reveal failure with retry, never the placeholder', async () => {
    // First reveal flakes; the retry succeeds. The flake must withhold
    // Confirm (no placeholder export for a readable credential) and show
    // the error with the retry that re-runs the prefetch.
    plaintextMock.mockRejectedValueOnce(new APIError(9999, undefined))
    plaintextMock.mockResolvedValueOnce({ plaintext_key: 'sk-after-retry' })
    listKeysMock.mockResolvedValueOnce(keyPage([ownKey(1)]))

    const wrapper = mount(CcsKeyModeHost, {
      global: { plugins: [i18n, makeRouter()] },
      attachTo: document.body,
    })
    await vi.waitFor(() => expect(plaintextMock).toHaveBeenCalledTimes(1))
    // The transient branch, not the legacy one: no paste-by-hand notice.
    expect(document.body.textContent ?? '').not.toContain(en.ccswitch.plaintextUnavailable)
    expect(findButtonByText(en.common.confirm)!.hasAttribute('disabled')).toBe(true)

    findButtonByText(en.ccswitch.retry)!.dispatchEvent(new Event('click'))
    await vi.waitFor(() =>
      expect(findButtonByText(en.common.confirm)!.hasAttribute('disabled')).toBe(false),
    )

    clickConfirm()
    await nextTick()
    const events = wrapper.getComponent(CCSwitchImportModal).emitted<{ apiKey?: string; model?: string }[]>('confirm')
    expect(events).toEqual([[{ apiKey: 'sk-after-retry', model: 'glm-4.7' }]])
    wrapper.unmount()
  })

  it('exports a legacy selected key with the placeholder notice', async () => {
    plaintextMock.mockRejectedValue(new APIError(11016))
    listKeysMock.mockResolvedValueOnce(keyPage([ownKey(1)]))

    const wrapper = mount(CcsKeyModeHost, {
      global: { plugins: [i18n, makeRouter()] },
      attachTo: document.body,
    })
    await vi.waitFor(() =>
      expect(document.body.textContent ?? '').toContain(en.ccswitch.plaintextUnavailable),
    )
    clickConfirm()
    await nextTick()
    const events = wrapper.getComponent(CCSwitchImportModal).emitted<{ apiKey?: string; model?: string }[]>('confirm')
    expect(events).toEqual([[{ apiKey: undefined, model: 'glm-4.7' }]])
    wrapper.unmount()
  })
})
