// @vitest-environment happy-dom
//
// Mounted-DOM coverage for the model capability card (the detail page's
// read-only summary of the five capability-metadata fields plus the
// candidate max-output ceiling):
//
//   - declared render (TC-08): every field shows its stored value — context
//     length grouped, input modalities as tags, tool/reasoning as verdict
//     tags, temperature as the exact stored double
//   - MaxOutput surface (TC-08/US9): the line reports the highest max_output
//     among the ENABLED candidates (a disabled row's ceiling does not
//     speak), renders the 0 sentinel as "no extra limit", and an all-zero /
//     no-candidate chain degrades honestly
//   - bilingual (TC-08/TC-12): both locales render their own copy
//   - legacy empty state (TC-14): an all-null model shows every line as the
//     muted Undeclared mark — no crash, no invented default, never a false
//     "not supported"
import { afterEach, describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import naive from 'naive-ui'
import { defineComponent } from 'vue'

import type { Model, ModelCandidate } from '../../api/models'
import ModelCapabilityCard from './ModelCapabilityCard.vue'
import en from '../../locales/en'
import zhCN from '../../locales/zh-CN'

function candidateFixture(overrides: Partial<ModelCandidate> = {}): ModelCandidate {
  return {
    id: 1,
    provider_id: 10,
    provider_name: 'alpha',
    provider_model_name: 'alpha-model',
    input_price: 1.5,
    output_price: 2.5,
    cache_write_price: null,
    cache_read_price: null,
    max_output: 8192,
    billing_mode: 'token',
    image_pricing_tiers: null,
    video_pricing_tiers: null,
    audio_unit_price: null,
    supports_streaming: true,
    supports_function_calling: null,
    management_status: 1,
    sort_order: 1,
    verification_status: 0,
    routable: true,
    blocked_by: '',
    last_test_result: 0,
    last_test_duration_ms: 120,
    last_tested_at: '2026-09-01T00:00:00Z',
    binding_count: 0,
    ...overrides,
  }
}

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
  components: { ModelCapabilityCard },
  template: '<NConfigProvider><NMessageProvider><ModelCapabilityCard :model="model" /></NMessageProvider></NConfigProvider>',
  props: { model: { type: Object as () => Model, required: true } },
})

type Locale = 'en' | 'zh-CN'

async function mountCard(model: Model, locale: Locale = 'en'): Promise<HTMLElement> {
  // Two literal createI18n calls rather than one with a merged messages
  // object: the options' schema typing rejects a union message tree.
  const i18n =
    locale === 'en'
      ? createI18n({ legacy: false, locale: 'en', fallbackLocale: 'en', messages: { en } })
      : createI18n({ legacy: false, locale: 'zh-CN', fallbackLocale: 'zh-CN', messages: { 'zh-CN': zhCN } })
  mount(Host, {
    attachTo: document.body,
    props: { model },
    global: { plugins: [i18n, naive] },
  })
  const card = document.body.querySelector<HTMLElement>('.model-capability-card')
  expect(card, 'capability card rendered').toBeTruthy()
  return card!
}

function fieldText(card: HTMLElement, field: string): string {
  const item = card.querySelector<HTMLElement>(`[data-field="${field}"]`)
  expect(item, `field ${field} rendered`).toBeTruthy()
  return (item!.querySelector('dd')?.textContent ?? '').trim()
}

afterEach(() => {
  document.body.innerHTML = ''
})

describe('ModelCapabilityCard — declared render (TC-08)', () => {
  it('shows every declared field, MaxOutput taken from the enabled candidates', async () => {
    const card = await mountCard(
      modelFixture({
        context_length: 131072,
        input_modalities: ['text', 'image'],
        supports_tools: true,
        supports_reasoning: false,
        temperature: 0.705,
        candidates: [
          candidateFixture({ id: 1, max_output: 8192, management_status: 1 }),
          // A disabled row's higher ceiling must not speak for the model.
          candidateFixture({ id: 2, max_output: 999999, management_status: 2 }),
        ],
      }),
    )
    expect(fieldText(card, 'context-length')).toBe('131,072')
    expect(fieldText(card, 'input-modalities')).toContain(en.models.inputModalityText)
    expect(fieldText(card, 'input-modalities')).toContain(en.models.inputModalityImage)
    expect(fieldText(card, 'supports-tools')).toBe(en.models.triStateYes)
    expect(fieldText(card, 'supports-reasoning')).toBe(en.models.triStateNo)
    // The exact stored double: a 0.705 declaration shows 0.705, not 0.71.
    expect(fieldText(card, 'temperature')).toBe('0.705')
    expect(fieldText(card, 'max-output')).toBe('8,192')
    // The batch's red line stays visible on the card itself.
    expect(card.textContent ?? '').toContain(en.models.capabilityInformationalNote)
  })

  it('renders the 0 max-output sentinel as "no extra limit", not a zero ceiling', async () => {
    const card = await mountCard(
      modelFixture({ candidates: [candidateFixture({ max_output: 0, management_status: 1 })] }),
    )
    expect(fieldText(card, 'max-output')).toBe(en.models.maxOutputUnlimited)
  })

  it('falls back to the declared ceiling when no candidate is enabled', async () => {
    const card = await mountCard(
      modelFixture({
        candidates: [candidateFixture({ id: 1, max_output: 4096, management_status: 2 })],
      }),
    )
    expect(fieldText(card, 'max-output')).toBe('4,096')
  })
})

describe('ModelCapabilityCard — bilingual copy (TC-08 / TC-12)', () => {
  it('renders the Chinese copy under zh-CN', async () => {
    const card = await mountCard(
      modelFixture({ supports_tools: true, supports_reasoning: null }),
      'zh-CN',
    )
    expect(card.textContent ?? '').toContain(zhCN.models.capabilityTitle)
    expect(fieldText(card, 'supports-tools')).toBe(zhCN.models.triStateYes)
    expect(fieldText(card, 'supports-reasoning')).toBe(zhCN.models.triStateUnknown)
    expect(fieldText(card, 'context-length')).toBe(zhCN.models.triStateUnknown)
  })

  it('renders the English copy under en', async () => {
    const card = await mountCard(modelFixture({ supports_tools: true }))
    expect(card.textContent ?? '').toContain(en.models.capabilityTitle)
    expect(fieldText(card, 'supports-tools')).toBe(en.models.triStateYes)
  })
})

describe('ModelCapabilityCard — legacy empty state (TC-14)', () => {
  it('an all-undeclared model shows every line Undeclared — no crash, no invented default', async () => {
    const card = await mountCard(modelFixture({}))
    const items = [...card.querySelectorAll('[data-field]')]
    expect(items.length).toBe(6)
    for (const item of items) {
      expect((item.querySelector('dd')?.textContent ?? '').trim()).toBe(en.models.triStateUnknown)
    }
    // Never a false verdict: no ✓/✗ tags on an undeclared row.
    expect(card.querySelectorAll('.n-tag').length).toBe(0)
  })
})
