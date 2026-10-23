// @vitest-environment happy-dom
//
// CandidateEditModal — the edit payload's price dirty-tracking, i.e. the
// client half of the update endpoint's absent = keep-stored contract for the
// primary token prices:
//
//   - an unpriced import row (stored 0/0, undeclared) saved for an UNRELATED
//     field (max_output) submits NEITHER price. Echoing the stored zeros back
//     would be judged submission-side as a fresh price claim and re-derive the
//     row as explicitly free — which re-arms the auto-enable promise its
//     requeue deliberately withholds. (The save also carries no status: the
//     untouched switch must stay out for the same requeue reasons.)
//   - a price the operator actually changed submits — and ALONE: the untouched
//     sibling slot stays absent, so one edited rate cannot silently declare
//     the other one free.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import naive, { NInputNumber } from 'naive-ui'
import { defineComponent, nextTick } from 'vue'

import type { ModelCandidate } from '../../api/models'
import { CANDIDATE_STATUS_DISABLED } from '../../api/candidateStatus'

// TC target: the payload the dialog hands to the store, captured at the store
// boundary. vi.hoisted because the vi.mock factory lifts above every const
// here.
const { updateCandidateMock, providersStoreStub } = vi.hoisted(() => ({
  updateCandidateMock: vi.fn(),
  providersStoreStub: {
    list: [] as unknown[],
    fetchList: vi.fn(async () => undefined),
    listModelsForProvider: vi.fn(async () => ({ models: [] as string[] })),
  },
}))
vi.mock('../../store/models', () => ({
  useModelsStore: () => ({
    updateCandidate: updateCandidateMock,
    createCandidate: vi.fn(),
    testAndCreateCandidate: vi.fn(),
  }),
}))
vi.mock('../../store/providers', () => ({ useProvidersStore: () => providersStoreStub }))

// Must be imported after the vi.mock factories are registered.
import CandidateEditModal from './CandidateEditModal.vue'
import en from '../../locales/en'

// An imported mapping as the import flow stores it: prices 0/0, undeclared,
// disabled, never probed. The exact row shape whose unrelated-field saves used
// to come out re-declared as explicitly free.
function candidateFixture(overrides: Partial<ModelCandidate> = {}): ModelCandidate {
  return {
    id: 42,
    provider_id: 10,
    provider_name: 'alpha',
    provider_model_name: 'up-alpha',
    input_price: 0,
    output_price: 0,
    cache_write_price: null,
    cache_read_price: null,
    max_output: 0,
    billing_mode: 'token',
    image_pricing_tiers: null,
    video_pricing_tiers: null,
    audio_unit_price: null,
    supports_streaming: null,
    supports_function_calling: null,
    management_status: CANDIDATE_STATUS_DISABLED,
    sort_order: 1,
    verification_status: 0,
    routable: false,
    blocked_by: '',
    last_test_result: null,
    last_test_duration_ms: null,
    last_tested_at: null,
    binding_count: 0,
    ...overrides,
  }
}

const Host = defineComponent({
  components: { CandidateEditModal },
  template:
    '<NConfigProvider><NMessageProvider><NDialogProvider><CandidateEditModal :show="show" :model-id="1" model-name="big-model" :editing-candidate="candidate" /></NDialogProvider></NMessageProvider></NConfigProvider>',
  props: {
    candidate: { type: Object as () => ModelCandidate | null, default: null },
    show: { type: Boolean, default: false },
  },
})

let wrapper: VueWrapper | null = null

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

async function mountModal(candidate: ModelCandidate) {
  const i18n = createI18n({ legacy: false, locale: 'en', fallbackLocale: 'en', messages: { en } })
  wrapper = mount(Host, {
    attachTo: document.body,
    props: { candidate },
    global: { plugins: [i18n, naive] },
  })
  // The seed watch keys on show; reproduce the real open transition
  // (show false → true) rather than mounting mid-open.
  await wrapper.setProps({ show: true })
  // Token mode renders five number fields: input, output, cache write,
  // cache read, max output — in that order.
  await vi.waitFor(() => expect(wrapper!.findAllComponents(NInputNumber)).toHaveLength(5))
}

// The token grid's number fields in render order; the modal's own labels carry
// no test hooks, so the fixed order IS the contract this file pins.
function numberControls() {
  return wrapper!.findAllComponents(NInputNumber)
}

function submit() {
  // The custom footer teleports to body with the modal and uses no shared
  // actions class, so the save button is found by its exact label.
  const buttons = Array.from(document.body.querySelectorAll<HTMLButtonElement>('button'))
  const save = buttons.filter((b) => (b.textContent ?? '').trim() === en.models.save)
  expect(save.length, 'save button rendered').toBeGreaterThan(0)
  save[0]!.dispatchEvent(new Event('click'))
}

async function submittedPayload(): Promise<Record<string, unknown>> {
  await vi.waitFor(() => expect(updateCandidateMock).toHaveBeenCalledTimes(1))
  return updateCandidateMock.mock.calls[0]![2] as Record<string, unknown>
}

beforeEach(() => {
  stubDesktopMatchMedia()
  updateCandidateMock.mockReset()
  updateCandidateMock.mockResolvedValue({
    candidate: { management_status: CANDIDATE_STATUS_DISABLED },
    report: null,
    report_applied: true,
  })
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  vi.clearAllMocks()
  vi.unstubAllGlobals()
  document.body.innerHTML = ''
})

describe('CandidateEditModal edit payload — price dirty tracking', () => {
  it('an unpriced row saved for max_output alone submits neither price', async () => {
    await mountModal(candidateFixture())
    numberControls()[4]!.vm.$emit('update:value', 4096)
    await nextTick()
    submit()
    const payload = await submittedPayload()
    // Stored zeros echoed back would count as a fresh free-price submission
    // server-side — the laundering this tracking exists to prevent.
    expect('input_price' in payload, 'untouched input price rides absent (absent = keep)').toBe(false)
    expect('output_price' in payload, 'untouched output price rides absent (absent = keep)').toBe(false)
    expect(payload.max_output).toBe(4096)
    expect(payload.provider_model_name).toBe('up-alpha')
    // The untouched enable switch stays out too: a PRESENT status is an
    // explicit instruction, and the requeue flow depends on its absence.
    expect('management_status' in payload).toBe(false)
  })

  it('an edited input price submits — alone, with the untouched output price absent', async () => {
    await mountModal(candidateFixture())
    numberControls()[0]!.vm.$emit('update:value', 3)
    await nextTick()
    submit()
    const payload = await submittedPayload()
    expect(payload.input_price).toBe(3)
    expect('output_price' in payload, 'the untouched sibling price must not be declared free').toBe(false)
    expect(payload.max_output).toBe(0)
  })
})
