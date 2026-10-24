// @vitest-environment happy-dom
//
// CandidateTable — the shared mapping table's own contract, mounted directly
// (the provider detail page's models tab and the model detail page's route
// tab both render it; the two pages' own suites pin their wiring).
//
// Covered acceptance cases:
//
//   TC-02 the probe six-state badge column renders every display state with
//        its own label and tag type — passed / passed-disabled / failed /
//        queued / probing / inconclusive / idle, plus the no-position
//        pending fallback
//   TC-03 the row-identity adapter's two shapes: the provider page's
//        (candidate_id + model_id) and the model page's (row.id) — every
//        action callback receives the correctly resolved ids through the
//        same code path, and the menu's item set follows the injected
//        retestable predicate
//   TC-04 column ownership follows the config: the probe badge column only
//        under the provider config, the capability tri-state columns only
//        under the model config; the busy-row prop arms exactly that row's
//        menu
//   TC-05 the filter bar reuses the shared composable: keyword over the
//        injected name fields, bucket over the raw verification_status,
//        intersection, and the pager restarting at page 1 — the filter sees
//        the whole list, never the current page's slice
//   TC-05b the unpriced badge and the price column's empty-value display:
//        token/audio rows the server flagged undeclared badge (with the
//        pricing-guidance tooltip); explicitly free rows show plain 0 and
//        never badge; image/video rows without tiers get the tier hint in
//        the price cell instead of a badge; a pricing save (the refreshed
//        row's prices_declared) clears the badge
//   TC-08 the filter-miss empty state renders the injected title; the
//        loading state rides the table without dropping the rows
//   TC-09 a narrow viewport serves the row actions through the OptionSheet
//        bottom panel, with the retest entry still following the predicate
//
// The component never learns which page renders it — page differences
// arrive as props (identity adapter, column groups, config flags, handlers),
// so both adapter shapes below drive the very same component.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import naive, { NConfigProvider, NDialogProvider, NInput, NMessageProvider, NTag } from 'naive-ui'
import { defineComponent, h, nextTick, reactive, type ComponentPublicInstance } from 'vue'

import CandidateTable from './CandidateTable.vue'
import ResponsiveDropdown from '../common/ResponsiveDropdown.vue'
import FilterSelectField from '../common/FilterSelectField.vue'
import zhCN from '../../locales/zh-CN'
import { useClientPagination, type ClientPagination } from '../../composables/useClientPagination'
import type { ModelCandidate, ProviderCandidate } from '../../api/models'
import {
  CANDIDATE_STATUS_DISABLED,
  CANDIDATE_STATUS_ENABLED,
  VERIFICATION_FAILED,
  VERIFICATION_PASSED,
  VERIFICATION_UNTESTED,
} from '../../api/candidateStatus'

let wrapper: VueWrapper | null = null

// A controllable MediaQueryList (same stub shape as the page tests): the
// desktop table branches are the default; the OptionSheet leg flips it.
function stubMatchMedia(initialMobile: boolean) {
  const listeners = new Set<(e: { matches: boolean }) => void>()
  const mql = {
    matches: initialMobile,
    media: '(max-width: 768px)',
    onchange: null,
    addEventListener: (_type: string, cb: (e: { matches: boolean }) => void) => listeners.add(cb),
    removeEventListener: (_type: string, cb: (e: { matches: boolean }) => void) => listeners.delete(cb),
    addListener: (cb: (e: { matches: boolean }) => void) => listeners.add(cb),
    removeListener: (cb: (e: { matches: boolean }) => void) => listeners.delete(cb),
    dispatchEvent: () => true,
  }
  vi.stubGlobal('matchMedia', vi.fn(() => mql))
}

// A settled provider-page mapping row — the provider-scoped list endpoint's
// vocabulary (candidate_id + model_id).
function providerRow(overrides: Partial<ProviderCandidate> = {}): ProviderCandidate {
  return {
    candidate_id: 1,
    model_id: 100,
    model_name: 'model-one',
    provider_model_name: 'up-one',
    input_price: 1.5,
    output_price: 2.5,
    cache_write_price: null,
    cache_read_price: null,
    billing_mode: 'token',
    image_pricing_tiers: null,
    video_pricing_tiers: null,
    audio_unit_price: null,
    max_output: 4096,
    management_status: CANDIDATE_STATUS_ENABLED,
    verification_status: VERIFICATION_PASSED,
    last_test_result: 0,
    last_tested_at: '2026-09-01T00:00:00Z',
    last_test_error: null,
    queue_state: '',
    auto_enable_on_pass: false,
    ...overrides,
  }
}

// A model-page candidate row — the rows ARE the candidates of one model.
function modelCandidate(overrides: Partial<ModelCandidate> = {}): ModelCandidate {
  return {
    id: 1,
    provider_id: 10,
    provider_name: 'alpha',
    provider_model_name: 'alpha-model',
    input_price: 1.5,
    output_price: 2.5,
    cache_write_price: null,
    cache_read_price: 0.1,
    max_output: 4096,
    billing_mode: 'token',
    image_pricing_tiers: null,
    video_pricing_tiers: null,
    audio_unit_price: null,
    supports_streaming: true,
    supports_function_calling: null,
    management_status: CANDIDATE_STATUS_ENABLED,
    sort_order: 1,
    verification_status: VERIFICATION_PASSED,
    routable: true,
    blocked_by: '',
    last_test_result: 0,
    last_test_duration_ms: 120,
    last_tested_at: '2026-09-01T00:00:00Z',
    binding_count: 0,
    ...overrides,
  }
}

// The column titles' help glyphs call useMessage, so the table needs the
// provider chain App.vue nests pages under. A render-function wrapper avoids
// the runtime template compiler and passes the props bag straight through.
function mountWithProviders(tableProps: Record<string, unknown>) {
  const i18n = createI18n({ legacy: false, locale: 'zh-CN', fallbackLocale: 'zh-CN', messages: { 'zh-CN': zhCN } })
  const Host = defineComponent({
    name: 'CandidateTableHost',
    setup() {
      return () =>
        h(NConfigProvider, null, () =>
          h(NMessageProvider, null, () => h(NDialogProvider, null, () => h(CandidateTable, tableProps as never))),
        )
    },
  })
  wrapper = mount(Host, { attachTo: document.body, global: { plugins: [i18n, naive] } })
}

// The table's own emit records — the host re-emits nothing, so they are read
// off the inner component itself. (Generic SFCs type VTU's getComponent
// loosely, hence the cast to the wrapper shape emitted() lives on.)
function tableEmitted(event: string): unknown[][] | undefined {
  return (wrapper!.getComponent(CandidateTable) as unknown as VueWrapper).emitted(event)
}

// The provider page's injection shape: provider-row identity adapter, probe
// badge column on, and a retest predicate keyed on the queue (rows the queue
// holds get no retest entry — the injection seam, not the page's real rule,
// which its own suite pins).
function mountProviderTable(rows: ProviderCandidate[], extra: Record<string, unknown> = {}): ClientPagination {
  const { pagination } = useClientPagination()
  mountWithProviders({
    rows,
    pagination,
    rowIds: (row: ProviderCandidate) => ({ candidateId: row.candidate_id, modelId: row.model_id }),
    nameFields: (row: ProviderCandidate) => [row.model_name, row.provider_model_name],
    searchPlaceholder: `${zhCN.models.name} / ${zhCN.models.providerModelName}`,
    emptyTitle: zhCN.providers.modelsEmpty,
    priceColumn: { title: zhCN.providers.candidatePrice, tip: zhCN.providers.candidatePrice_tip },
    showProbeStateColumn: true,
    retestable: (row: ProviderCandidate) => row.queue_state === '',
    disableRetestWhileBusy: true,
    ...extra,
  })
  return pagination
}

// The model page's injection shape: model-row identity adapter (row.id + the
// page's one model id) and the capability tri-state columns on.
const MODEL_ID = 7
function mountModelTable(candidates: ModelCandidate[], extra: Record<string, unknown> = {}): ClientPagination {
  const { pagination } = useClientPagination()
  mountWithProviders({
    rows: candidates,
    pagination,
    rowIds: (row: ModelCandidate) => ({ candidateId: row.id, modelId: MODEL_ID }),
    nameFields: (row: ModelCandidate) => [row.provider_name, row.provider_model_name],
    searchPlaceholder: `${zhCN.models.provider} / ${zhCN.models.providerModelName}`,
    emptyTitle: zhCN.models.routeChainEmpty,
    priceColumn: { title: zhCN.models.billingPriceColumn, tip: zhCN.models.billingPriceColumn_tip, showBillingModeTag: true },
    showCapabilityColumns: true,
    ...extra,
  })
  return pagination
}

// The body row rendering a mapping, located by a substring unique to it
// (the provider model name is unique in every fixture here).
function rowTr(locator: string): HTMLTableRowElement {
  const tr = [...document.body.querySelectorAll('tr')].find((el) => (el.textContent ?? '').includes(locator))
  expect(tr, `row for ${locator} rendered`).toBeTruthy()
  return tr!
}

// Opens a row's '⋯' actions menu the way a user does and returns the menu's
// rendered item labels (dividers excluded).
async function openRowMenu(locator: string): Promise<string[]> {
  const trigger = rowTr(locator).querySelector('button')
  expect(trigger, `the actions trigger on ${locator}'s row rendered`).toBeTruthy()
  trigger!.dispatchEvent(new MouseEvent('click', { bubbles: true }))
  await nextTick()
  await nextTick()
  const menus = [...document.body.querySelectorAll('.n-dropdown-menu')]
  const menu = menus[menus.length - 1]
  expect(menu, `the actions menu for ${locator} opened`).toBeTruthy()
  return [...menu!.querySelectorAll('.n-dropdown-option')]
    .filter((opt) => !opt.classList.contains('n-dropdown-option--divider'))
    .map((opt) => (opt.textContent ?? '').trim())
}

// Clicks the currently open menu's item by its visible label.
async function clickMenuItem(label: string) {
  const menus = [...document.body.querySelectorAll('.n-dropdown-menu')]
  const menu = menus[menus.length - 1]
  const item = [...menu!.querySelectorAll('.n-dropdown-option')].find(
    (opt) => (opt.textContent ?? '').trim() === label,
  )
  expect(item, `menu item "${label}" rendered`).toBeTruthy()
  const target = item!.querySelector('.n-dropdown-option-body') ?? item!
  target.dispatchEvent(new MouseEvent('click', { bubbles: true }))
  await nextTick()
  await nextTick()
}

// The search input, located by its placeholder (the injected one).
type SearchInputWrapper = VueWrapper<unknown, ComponentPublicInstance<{ placeholder?: string }>>
function searchField(): SearchInputWrapper {
  const expected = `${zhCN.models.name} / ${zhCN.models.providerModelName}`
  const inputs = wrapper!.findAllComponents(NInput) as unknown as SearchInputWrapper[]
  const field = inputs.find((c) => c.props('placeholder') === expected)
  expect(field, `the search input (placeholder "${expected}") rendered`).toBeTruthy()
  return field!
}

async function typeKeyword(keyword: string) {
  ;(searchField() as unknown as VueWrapper).vm.$emit('update:value', keyword)
  await nextTick()
  await nextTick()
}

type FilterSelectWrapper = VueWrapper<
  unknown,
  ComponentPublicInstance<{
    label?: string
    placeholder?: string
    value?: unknown
    options?: Array<{ label: string; value: string }>
  }>
>
function filterSelect(): FilterSelectWrapper {
  const selects = wrapper!.findAllComponents(FilterSelectField) as unknown as FilterSelectWrapper[]
  expect(selects.length, 'the verification filter select rendered').toBeGreaterThanOrEqual(1)
  return selects[0]
}

async function pickBucket(value: 'passed' | 'failed' | 'unverified' | null) {
  ;(filterSelect() as unknown as VueWrapper).vm.$emit('update:value', value)
  await nextTick()
  await nextTick()
}

function bodyText(): string {
  return document.body.textContent ?? ''
}

// The rows currently rendered in the table, told apart by substrings unique
// to each row (the provider model names).
function expectRows(visible: string[], absent: string[]) {
  const texts = [...document.body.querySelectorAll('tr')].map((el) => el.textContent ?? '')
  for (const name of visible) {
    expect(texts.some((text) => text.includes(name)), `row for ${name} is rendered`).toBe(true)
  }
  for (const name of absent) {
    expect(texts.some((text) => text.includes(name)), `row for ${name} is NOT rendered`).toBe(false)
  }
}

// The probe badge a row renders: its visible label in the row's DOM, and its
// tag type read off the tag component (naive-ui paints the type via CSS
// variables rather than classes, so the type prop is the observable tier).
// The tier IS the behavior — the six states are told apart at a glance.
type TagWrapper = VueWrapper<unknown, ComponentPublicInstance<{ type?: string }>>

// The tag components rendered inside a row, by the row's DOM subtree.
function tagsInRow(tr: HTMLTableRowElement): TagWrapper[] {
  return (wrapper!.findAllComponents(NTag) as unknown as TagWrapper[]).filter((w) => tr.contains(w.element))
}

function expectBadge(rowLocator: string, label: string, type: 'success' | 'warning' | 'error' | 'default' | 'info') {
  const tr = rowTr(rowLocator)
  const rendered = [...tr.querySelectorAll('.n-tag')].some((el) => (el.textContent ?? '').trim() === label)
  expect(rendered, `${rowLocator} renders the "${label}" badge`).toBe(true)
  const tag = tagsInRow(tr).find((w) => (w.element.textContent ?? '').trim() === label)
  expect(tag, `${rowLocator}'s "${label}" badge is a rendered tag`).toBeTruthy()
  expect(tag!.props('type'), `${rowLocator}'s "${label}" badge is a ${type} tag`).toBe(type)
}

// The absence half of expectBadge: no tag with this label anywhere in the
// row (tag-scoped, so the price column's muted providers.candidateUnpriced
// note never masquerades as the badge).
function expectNoBadge(rowLocator: string, label: string) {
  const tr = rowTr(rowLocator)
  const rendered = [...tr.querySelectorAll('.n-tag')].some((el) => (el.textContent ?? '').trim() === label)
  expect(rendered, `${rowLocator} must NOT render the "${label}" badge`).toBe(false)
}

// The menu labels of the nth row's actions dropdown (dividers excluded) —
// the props seam, for asserting disabled entries the DOM menu hides.
function rowMenuOptions(rowIndex: number): Array<{ label?: string; key?: string | number; disabled?: boolean; type?: string }> {
  const dropdowns = wrapper!.findAllComponents(ResponsiveDropdown)
  expect(dropdowns.length, 'one actions dropdown per row').toBeGreaterThanOrEqual(rowIndex + 1)
  return dropdowns[rowIndex].props('options') as Array<{
    label?: string
    key?: string | number
    disabled?: boolean
    type?: string
  }>
}

beforeEach(() => {
  stubMatchMedia(false)
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  vi.unstubAllGlobals()
  vi.clearAllMocks()
  document.body.innerHTML = ''
})

describe('CandidateTable probe six-state badge column (TC-02)', () => {
  it('renders every display state with its own label and tag type', () => {
    mountProviderTable([
      providerRow({ candidate_id: 1, provider_model_name: 'row-passed', verification_status: VERIFICATION_PASSED }),
      providerRow({
        candidate_id: 2,
        provider_model_name: 'row-passed-disabled',
        verification_status: VERIFICATION_PASSED,
        management_status: CANDIDATE_STATUS_DISABLED,
      }),
      providerRow({
        candidate_id: 3,
        provider_model_name: 'row-failed',
        verification_status: VERIFICATION_FAILED,
        last_test_result: 7,
        last_test_error: 'upstream 500',
      }),
      providerRow({
        candidate_id: 4,
        provider_model_name: 'row-queued',
        verification_status: VERIFICATION_UNTESTED,
        last_test_result: null,
        last_tested_at: null,
        queue_state: 'queued',
      }),
      providerRow({
        candidate_id: 5,
        provider_model_name: 'row-probing',
        verification_status: VERIFICATION_UNTESTED,
        last_test_result: null,
        last_tested_at: null,
        queue_state: 'probing',
      }),
      providerRow({
        candidate_id: 6,
        provider_model_name: 'row-inconclusive',
        verification_status: VERIFICATION_UNTESTED,
        last_test_result: 7,
        last_test_error: 'rate limited',
      }),
      providerRow({
        candidate_id: 7,
        provider_model_name: 'row-idle',
        verification_status: VERIFICATION_UNTESTED,
        last_test_result: null,
        last_tested_at: null,
        auto_enable_on_pass: false,
      }),
      providerRow({
        candidate_id: 8,
        provider_model_name: 'row-pending-fallback',
        verification_status: VERIFICATION_UNTESTED,
        last_test_result: null,
        last_tested_at: null,
        probe_owed: true,
      }),
    ])

    expectBadge('row-passed', zhCN.models.importStatePassed, 'success')
    expectBadge('row-passed-disabled', zhCN.models.importStatePassedDisabled, 'warning')
    expectBadge('row-failed', zhCN.models.importStateFailed, 'error')
    expectBadge('row-queued', zhCN.models.importStateQueued, 'default')
    expectBadge('row-probing', zhCN.models.importStateProbing, 'info')
    expectBadge('row-inconclusive', zhCN.models.importStateInconclusive, 'warning')
    expectBadge('row-idle', zhCN.models.importStateIdle, 'default')
    // The pending row with no queue position falls back to the caller's
    // untested label (the standing probe debt still owes it a probe).
    expectBadge('row-pending-fallback', zhCN.providers.verificationUntested, 'default')
  })
})

describe('CandidateTable row-identity adapters (TC-03)', () => {
  it('the provider-page adapter resolves candidate_id + model_id into every action callback', async () => {
    const row = providerRow({ candidate_id: 11, model_id: 100, provider_model_name: 'up-passed' })
    mountProviderTable([row])

    const ids = { candidateId: 11, modelId: 100 }
    await openRowMenu('up-passed')
    await clickMenuItem(zhCN.models.editCandidate)
    expect(tableEmitted('edit'), 'edit carries the resolved provider-row ids').toEqual([[row, ids]])

    await openRowMenu('up-passed')
    await clickMenuItem(zhCN.models.retest)
    expect(tableEmitted('retest'), 'retest carries the resolved provider-row ids').toEqual([[row, ids]])

    await openRowMenu('up-passed')
    await clickMenuItem(zhCN.models.deleteCandidate)
    expect(tableEmitted('delete'), 'delete carries the resolved provider-row ids').toEqual([[row, ids]])
  })

  it('the model-page adapter resolves row.id + the page model id through the same path', async () => {
    const row = modelCandidate({ id: 2, provider_model_name: 'beta-model' })
    mountModelTable([row])

    const ids = { candidateId: 2, modelId: MODEL_ID }
    await openRowMenu('beta-model')
    await clickMenuItem(zhCN.models.editCandidate)
    expect(tableEmitted('edit')).toEqual([[row, ids]])

    await openRowMenu('beta-model')
    await clickMenuItem(zhCN.models.retest)
    expect(tableEmitted('retest')).toEqual([[row, ids]])

    await openRowMenu('beta-model')
    await clickMenuItem(zhCN.models.deleteCandidate)
    expect(tableEmitted('delete')).toEqual([[row, ids]])
  })

  it("the menu's item set follows the injected retestable predicate and busy state", async () => {
    const retestable = providerRow({ candidate_id: 11, provider_model_name: 'up-free' })
    const queued = providerRow({
      candidate_id: 12,
      provider_model_name: 'up-queued',
      verification_status: VERIFICATION_UNTESTED,
      last_test_result: null,
      last_tested_at: null,
      queue_state: 'queued',
    })
    mountProviderTable([retestable, queued])

    // Both rows edit and delete; only the predicate-passing row retests.
    expect(await openRowMenu('up-free')).toEqual([zhCN.models.editCandidate, zhCN.models.retest, zhCN.models.deleteCandidate])
    expect(await openRowMenu('up-queued')).toEqual([zhCN.models.editCandidate, zhCN.models.deleteCandidate])

    // The single-flight rule is injected too: while ANY row is busy, the
    // retest entry disables (a single-flight runner would swallow the click).
    wrapper!.unmount()
    document.body.innerHTML = ''
    mountProviderTable([retestable, queued], { busyRowId: 12 })
    const options = rowMenuOptions(0).filter((o) => o.type !== 'divider')
    const retest = options.find((o) => o.key === 'retest')
    expect(retest, "the free row's retest entry is present").toBeTruthy()
    expect(retest!.disabled, 'the retest entry disables while another row is busy').toBe(true)
    // The busy row's own trigger is the one that spins and disables.
    const dropdowns = wrapper!.findAllComponents(ResponsiveDropdown)
    expect(dropdowns[0].props('disabled'), 'the free row stays operable').toBe(false)
    expect(dropdowns[1].props('disabled'), 'the busy row disables its trigger').toBe(true)
  })
})

describe('CandidateTable column ownership injection (TC-04)', () => {
  it('provider config: the probe badge column renders, the capability columns do not', () => {
    mountProviderTable([providerRow()])
    expect(bodyText(), 'the probe badge column header rendered').toContain(zhCN.providers.candidateProbeStatus)
    expect(bodyText(), 'no streaming capability column').not.toContain(zhCN.models.supportsStreaming)
    expect(bodyText(), 'no function-calling capability column').not.toContain(zhCN.models.supportsFunctionCalling)
  })

  it('model config: the capability columns render, the probe badge column does not', () => {
    mountModelTable([modelCandidate()])
    expect(bodyText(), 'the streaming capability column header rendered').toContain(zhCN.models.supportsStreaming)
    expect(bodyText(), 'the function-calling capability column header rendered').toContain(zhCN.models.supportsFunctionCalling)
    expect(bodyText(), 'no probe badge column').not.toContain(zhCN.providers.candidateProbeStatus)
  })

  it('capability cells render the tri-states; an unconfirmed tag retests through the shared emit', async () => {
    const rows = [
      modelCandidate({ id: 1, provider_model_name: 'cap-yes', supports_streaming: true }),
      modelCandidate({ id: 2, provider_model_name: 'cap-no', supports_streaming: false }),
      modelCandidate({ id: 3, provider_model_name: 'cap-unknown', supports_streaming: null }),
    ]
    mountModelTable(rows)

    const confirmed = tagsInRow(rowTr('cap-yes')).find((w) => w.props('type') === 'success')
    expect(confirmed?.element.textContent ?? '', 'a confirmed capability renders the ✓ mark').toContain('✓')
    const unsupported = tagsInRow(rowTr('cap-no')).find((w) => w.props('type') === 'error')
    expect(unsupported?.element.textContent ?? '', 'an unsupported capability renders the ✗ mark').toContain('✗')
    const unconfirmed = tagsInRow(rowTr('cap-unknown')).find((w) => w.props('type') === 'warning')
    expect(unconfirmed?.element.textContent ?? '', 'an unconfirmed capability renders the ? mark').toContain('?')

    // The unconfirmed tag is clickable and retests through the SAME emit the
    // menu's retest entry uses, carrying the row's resolved ids.
    unconfirmed!.element.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await nextTick()
    expect(tableEmitted('retest')).toEqual([[rows[2], { candidateId: 3, modelId: MODEL_ID }]])
  })
})

describe('CandidateTable filter wiring (TC-05)', () => {
  // One row per verification flavor, names chosen so the keyword can tell
  // them apart through either injected name field.
  function bucketRows(): ProviderCandidate[] {
    return [
      providerRow({ candidate_id: 11, model_name: 'model-alpha', provider_model_name: 'up-alpha' }),
      providerRow({
        candidate_id: 12,
        model_name: 'model-beta',
        provider_model_name: 'up-beta',
        verification_status: VERIFICATION_FAILED,
        last_test_result: 7,
        last_test_error: 'upstream 500',
      }),
      providerRow({
        candidate_id: 13,
        model_name: 'model-gamma',
        provider_model_name: 'up-gamma',
        verification_status: VERIFICATION_UNTESTED,
        last_test_result: 7,
        last_test_error: 'rate limited',
      }),
      providerRow({
        candidate_id: 14,
        model_name: 'model-delta',
        provider_model_name: 'up-delta',
        verification_status: VERIFICATION_UNTESTED,
        last_test_result: null,
        last_tested_at: null,
      }),
    ]
  }

  it('the verification select is wired once with the shared bucket options', () => {
    mountProviderTable(bucketRows())
    const select = filterSelect()
    expect(select.props('options')).toEqual([
      { label: zhCN.providers.verificationPassed, value: 'passed' },
      { label: zhCN.providers.verificationFailed, value: 'failed' },
      { label: zhCN.providers.verificationUnverified, value: 'unverified' },
    ])
    expect(select.props('label')).toBe(zhCN.models.probeResultTitle)
    expect(searchField().props('placeholder')).toBe(`${zhCN.models.name} / ${zhCN.models.providerModelName}`)
  })

  it('keyword over the injected name fields and bucket over the raw status intersect; clearing restores', async () => {
    mountProviderTable(bucketRows())

    // Keyword over the FIRST name field (the model name — searchable even
    // though this minimal mount renders no column for it), case-insensitive.
    await typeKeyword('MODEL-BETA')
    expectRows(['up-beta'], ['up-alpha', 'up-gamma', 'up-delta'])
    // ... and over the second (the rendered provider model name).
    await typeKeyword('up-gamma')
    expectRows(['up-gamma'], ['up-alpha', 'up-beta', 'up-delta'])
    await typeKeyword('')

    // Bucket equality on the raw three-value status: the unverified bucket
    // merges the probed-but-inconclusive and the never-probed rows.
    await pickBucket('unverified')
    expectRows(['up-gamma', 'up-delta'], ['up-alpha', 'up-beta'])

    // Both conditions at once: only their intersection survives.
    await typeKeyword('delta')
    expectRows(['up-delta'], ['up-alpha', 'up-beta', 'up-gamma'])

    // Clearing either condition restores that condition's own result.
    await pickBucket(null)
    expectRows(['up-delta'], ['up-alpha', 'up-beta', 'up-gamma'])
    await typeKeyword('')
    expectRows(['up-alpha', 'up-beta', 'up-gamma', 'up-delta'], [])
  })

  it('the filter sees the whole list — after flipping to page 2, a keyword restarts at page 1', async () => {
    // 21 rows against the default 20-per-page: bulk-01..bulk-20 fill page 1,
    // tail-row is page 2's only row.
    const rows: ProviderCandidate[] = []
    for (let i = 1; i <= 20; i++) {
      rows.push(
        providerRow({
          candidate_id: i,
          model_id: 100 + i,
          model_name: `bulk-${String(i).padStart(2, '0')}`,
          provider_model_name: `up-${String(i).padStart(2, '0')}`,
        }),
      )
    }
    rows.push(providerRow({ candidate_id: 21, model_id: 121, model_name: 'tail-row', provider_model_name: 'up-tail' }))
    const pagination = mountProviderTable(rows)

    // Flip to page 2 like a user: only the tail row renders there.
    const page2 = [...document.body.querySelectorAll('.n-pagination .n-pagination-item')].find(
      (el) => (el.textContent ?? '').trim() === '2',
    )
    expect(page2, 'the pager offers a page 2').toBeTruthy()
    page2!.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await nextTick()
    await nextTick()
    expectRows(['up-tail'], ['up-01', 'up-02'])
    expect(pagination.page, 'the pager really turned to page 2').toBe(2)

    // Typing a keyword that matches only PAGE-1 rows (by their model names,
    // which this minimal mount does not even render) proves the filter sees
    // every row and restarts the pager at page 1.
    await typeKeyword('bulk-')
    expectRows(['up-01', 'up-02', 'up-20'], ['up-tail'])
    expect(pagination.page, 'the keyword restarted the pager at page 1').toBe(1)

    await typeKeyword('')
    expectRows(['up-01', 'up-20'], ['up-tail'])
  })
})

describe('CandidateTable unpriced badge and empty-value price display (TC-05b)', () => {
  const BADGE = zhCN.providers.candidateUnpriced

  // One row per acceptance class: the badge is a token/audio fact keyed on
  // the server's prices_declared (the zero heuristic only when the field is
  // absent), image/video rows never badge — their no-tier state lives in the
  // price cell's own tier hint.
  function eightClassRows(): ProviderCandidate[] {
    return [
      // 1. imported unpriced token row: stored 0/0, undeclared.
      providerRow({
        candidate_id: 41,
        provider_model_name: 'up-badge-token-import',
        input_price: 0,
        output_price: 0,
        prices_declared: false,
        probe_owed: true,
      }),
      // 2. explicitly free token row: stored 0/0, declared.
      providerRow({
        candidate_id: 42,
        provider_model_name: 'up-free-token',
        input_price: 0,
        output_price: 0,
        prices_declared: true,
      }),
      // 3. image row with no tier table: tier hint, never the badge.
      providerRow({
        candidate_id: 43,
        provider_model_name: 'up-image-no-tier',
        billing_mode: 'image',
        image_pricing_tiers: null,
      }),
      // 4. audio row with no character price, undeclared.
      providerRow({
        candidate_id: 44,
        provider_model_name: 'up-audio-unset',
        billing_mode: 'audio',
        audio_unit_price: null,
        input_price: 0,
        output_price: 0,
        prices_declared: false,
      }),
      // 5. explicitly free audio row: stored 0 character price, declared.
      providerRow({
        candidate_id: 45,
        provider_model_name: 'up-audio-free',
        billing_mode: 'audio',
        audio_unit_price: 0,
        input_price: 0,
        output_price: 0,
        prices_declared: true,
      }),
      // 6. video row with no tier table: same treatment as image.
      providerRow({
        candidate_id: 46,
        provider_model_name: 'up-video-no-tier',
        billing_mode: 'video',
        video_pricing_tiers: null,
      }),
      // 7. manually created with the price fields left empty: stored 0/0,
      //    undeclared — the same display rule as the imported row.
      providerRow({
        candidate_id: 47,
        provider_model_name: 'up-manual-empty',
        input_price: 0,
        output_price: 0,
        prices_declared: false,
        verification_status: VERIFICATION_UNTESTED,
        last_test_result: null,
        last_tested_at: null,
      }),
      // 8. manually created with explicit zeros: declared, never a badge.
      providerRow({
        candidate_id: 48,
        provider_model_name: 'up-manual-zero',
        input_price: 0,
        output_price: 0,
        prices_declared: true,
        verification_status: VERIFICATION_UNTESTED,
        last_test_result: null,
        last_tested_at: null,
      }),
    ]
  }

  it('badges undeclared token/audio rows only; image/video rows get the tier hint instead', () => {
    mountProviderTable(eightClassRows())

    expectBadge('up-badge-token-import', BADGE, 'warning')
    expectBadge('up-audio-unset', BADGE, 'warning')
    expectBadge('up-manual-empty', BADGE, 'warning')
    expectNoBadge('up-free-token', BADGE)
    expectNoBadge('up-audio-free', BADGE)
    expectNoBadge('up-manual-zero', BADGE)
    expectNoBadge('up-image-no-tier', BADGE)
    expectNoBadge('up-video-no-tier', BADGE)
  })

  it('the undeclared zero slots read as the muted unpriced note; explicit free rows show plain 0', () => {
    mountProviderTable(eightClassRows())

    // Undeclared token rows: the muted note replaces the stored 0 / 0.
    const importRow = rowTr('up-badge-token-import')
    expect(importRow.textContent, 'the muted unpriced note shows').toContain(BADGE)
    expect(importRow.textContent, 'no fake free-price 0 / 0').not.toContain('0 / 0')

    // Declared free token rows keep their honest 0 / 0.
    expect(rowTr('up-free-token').textContent, 'an explicit free row shows 0 / 0').toContain('0 / 0')
    expect(rowTr('up-manual-zero').textContent, 'the manual explicit-zero row shows 0 / 0').toContain('0 / 0')

    // Tier-less image/video rows carry their tier hint, not any badge text.
    expect(rowTr('up-image-no-tier').textContent).toContain(zhCN.providers.candidateImageUnpriced)
    expect(rowTr('up-video-no-tier').textContent).toContain(zhCN.providers.candidateVideoUnpriced)

    // The declared-free audio row shows its stored 0 as a real price.
    expect(rowTr('up-audio-free').textContent).toContain(zhCN.providers.candidateAudioPrice.replace('{price}', '0'))
  })

  it('a pricing save clears the badge: the refreshed row flipping prices_declared drops it', async () => {
    // The edit dialog's dirty-tracked price save re-derives the declaration
    // server-side; the page's list refresh then carries the declared row and
    // the shared predicate drops the badge on that same key. The rows arrive
    // as a reactive list (the page's fetched state), so the refresh is an
    // in-place splice.
    const rows = reactive([
      providerRow({
        candidate_id: 49,
        provider_model_name: 'up-then-priced',
        input_price: 0,
        output_price: 0,
        prices_declared: false,
      }),
    ])
    mountProviderTable(rows)
    expectBadge('up-then-priced', BADGE, 'warning')

    rows.splice(0, 1, { ...rows[0], input_price: 2, output_price: 5, prices_declared: true })
    await nextTick()
    await nextTick()
    expectNoBadge('up-then-priced', BADGE)
    expect(rowTr('up-then-priced').textContent, 'the priced row shows its real numbers').toContain('2 / 5')
  })
})

describe('CandidateTable empty and loading states (TC-08)', () => {
  it('a filter nothing matches renders the injected empty title and no rows', async () => {
    mountProviderTable([providerRow()])
    await typeKeyword('zzz-no-match')
    expectRows([], ['up-one'])
    expect(bodyText()).toContain(zhCN.providers.modelsEmpty)
  })

  it('loading rides the table without dropping the rows', () => {
    mountProviderTable([providerRow()], { loading: true })
    expect(rowTr('up-one'), 'the rows stay rendered while loading').toBeTruthy()
    expect(document.body.querySelector('.n-data-table--loading'), 'the loading indicator rendered').toBeTruthy()
  })
})

describe('CandidateTable mobile actions panel (TC-09)', () => {
  it('a narrow viewport serves the row actions through the OptionSheet bottom panel', async () => {
    stubMatchMedia(true)
    const free = providerRow({ candidate_id: 11, provider_model_name: 'up-free' })
    const queued = providerRow({
      candidate_id: 12,
      provider_model_name: 'up-queued',
      verification_status: VERIFICATION_UNTESTED,
      last_test_result: null,
      last_tested_at: null,
      queue_state: 'queued',
    })
    mountProviderTable([free, queued])

    // Mobile renders the table as cards; the row's actions trigger becomes a
    // labelled button that raises the bottom sheet.
    const sheetLabels = async (locator: string): Promise<string[]> => {
      const card = [...document.body.querySelectorAll('.rdt-card')].find((el) =>
        (el.textContent ?? '').includes(locator),
      )
      expect(card, `card for ${locator} rendered`).toBeTruthy()
      const trigger = card!.querySelector<HTMLButtonElement>('.rd-trigger-btn')
      expect(trigger, `the actions trigger on ${locator}'s card rendered`).toBeTruthy()
      trigger!.click()
      await nextTick()
      await nextTick()
      const sheets = [...document.body.querySelectorAll('.option-sheet')]
      const sheet = sheets[sheets.length - 1]
      expect(sheet, `the option sheet for ${locator} opened`).toBeTruthy()
      return [...sheet!.querySelectorAll<HTMLButtonElement>('.option-sheet__option')].map((b) =>
        (b.textContent ?? '').trim(),
      )
    }

    // Same item sets as the desktop menu: the predicate still applies.
    expect(await sheetLabels('up-free')).toEqual([zhCN.models.editCandidate, zhCN.models.retest, zhCN.models.deleteCandidate])
    expect(await sheetLabels('up-queued')).toEqual([zhCN.models.editCandidate, zhCN.models.deleteCandidate])
  })
})
