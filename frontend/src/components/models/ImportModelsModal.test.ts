// @vitest-environment happy-dom
//
// Mounted-DOM coverage for the bulk-import dialog's two-pane master-detail
// redesign (the console half of the batch):
//
//   - list set (TC-10): the left pane renders exactly checkbox + display
//     name + output chips + status dot + suggestion badge — the editable
//     price columns are gone (prices live in the detail form only); the
//     first catalog row is detail-bound by default
//   - detail field set (TC-10): public name, context length + the shared
//     quick presets, input/output modalities, tool/reasoning tri-states,
//     temperature, max output, the five price inputs, the probe button
//     (live for rows this dialog can shape, disabled with its tooltip on
//     added rows) and the bottom guard hint; added rows read disabled with
//     the server-owned note
//   - selection plumbing (TC-10): row checkboxes drive the footer's
//     "Import selected (N)" count and its disabled-at-zero state; the
//     filter matches the public display name AND the upstream id; a
//     suggestion-priced row comes pre-checked and prefilled; select-all
//     under a filter flips only the visible rows and reads indeterminate
//     on a partial visible selection
//   - output-modality exclusivity: a text+audio output selection collapses
//     to audio on the row and in the picker (the shared exclusivity watch)
//   - stacked layout: below the mobile breakpoint the two panes stack
//     vertically (import-layout--stacked)
//   - detail→list mirroring (TC-11): renaming the public name rewrites the
//     list row on the spot with the status dot still grey (a name is not a
//     capability), then declaring a capability flips the dot green
//   - lost-batch reconciliation: a rejected import batch adopts owed-work
//     rows from the server by provider_model_name, so a row re-imported
//     under a custom public name is not missed (the old model_name match
//     skipped it)
//   - single-model probe (TC-16): the footer Probe button fires the
//     standalone pre-import probe with the row's output declaration; the
//     verdict bar renders per-probe ✓✗/unknown plus the total time; a
//     confirmed capability fills an UNDECLARED slot (badged "probe
//     suggested"), a declared slot is never overwritten (kept-hint only);
//     nothing is saved; the request-rejected outcome (backend enum 10)
//     renders as "Request rejected"
//   - probe sessions (TC-16): a probe left in flight when the dialog closes
//     starts the reopened session clean — the button is usable at once, and
//     the stale promise landing (or failing) afterwards writes nothing and
//     toasts nothing into the fresh session
//   - suggestion staleness: a hand edit retires what the suggestion state
//     still claims — editing an applied value clears its badge, clearing a
//     declared slot after a not-applied probe drops the kept-hint, and so
//     does a declaration extended until it states what the probe suggested
//     (adding image after a not-applied vision probe, flipping tools to
//     supported); a re-probe over slots the last probe filled (no manual
//     edit in between) keeps the applied badges instead of demoting them to
//     the kept-hints
//   - failure details: a failed basic arm quotes its detail verbatim (the
//     no-usable-key path has no other explanation), same pre as the vision
//     arm's refusal
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import { createMemoryHistory, createRouter } from 'vue-router'
import naive, { NCheckbox, NInput, NInputNumber, NSelect } from 'naive-ui'
import { defineComponent, nextTick } from 'vue'

import type { ImportProviderModelsResult, ProviderCandidate } from '../../api/models'
import { TEST_OUTCOME_REQUEST_REJECTED } from '../../utils/testOutcomeDisplay'

// The dialog talks to four endpoints; all captured at the API boundary.
// vi.hoisted because the vi.mock factories lift above every const here.
const { listCatalogueMock, listCandidatesMock, suggestPricesMock, importProviderModelsMock, probeProviderModelMock } = vi.hoisted(() => ({
  listCatalogueMock: vi.fn(),
  listCandidatesMock: vi.fn(),
  suggestPricesMock: vi.fn(),
  importProviderModelsMock: vi.fn(),
  probeProviderModelMock: vi.fn(),
}))
vi.mock('../../api/providers', () => ({ listModelsForProvider: listCatalogueMock }))
vi.mock('../../api/models', () => ({
  importProviderModels: importProviderModelsMock,
  listProviderCandidates: listCandidatesMock,
  suggestPrices: suggestPricesMock,
  probeProviderModel: probeProviderModelMock,
}))

// Must be imported after the vi.mock factories are registered.
import ImportModelsModal from './ImportModelsModal.vue'
import en from '../../locales/en'

// A price suggestion as the batch suggest-prices endpoint answers.
const price = (input: number, output: number, source: 'seed' | 'history' = 'seed') => ({
  input_price: input,
  output_price: output,
  cache_write_price: null,
  cache_read_price: null,
  audio_unit_price: null,
  source,
  catalog_updated_at: '2026-10-01',
})

// One arm's verdict as the standalone probe endpoint reports it (each probe
// here ran; duration identical so a total is predictable).
const verdict = (supported: boolean | null, outcome: number | null = null, detail = '') => ({
  ran: true,
  supported,
  outcome,
  duration_ms: 400,
  detail,
})

// A full standalone-probe answer, every arm confirmed by default; each test
// overrides only the arms it is about.
const probeResult = (arms: {
  basic?: ReturnType<typeof verdict>
  streaming?: ReturnType<typeof verdict>
  function_calling?: ReturnType<typeof verdict>
  vision?: ReturnType<typeof verdict>
} = {}) => ({
  report: {
    basic: arms.basic ?? verdict(true, 0),
    streaming: arms.streaming ?? verdict(true, 0),
    function_calling: arms.function_calling ?? verdict(true, 0),
  },
  vision: arms.vision ?? verdict(true, 0),
})

// A successful catalogue read: outcome 0 is the backend TestOutcome success
// value (a null outcome would read as a fetch failure here).
const catalogue = (models: string[]) => ({ models, outcome: 0, detail: '' })

// A full provider-candidate row as the candidates endpoint reports it.
const candidate = (overrides: Partial<ProviderCandidate> = {}): ProviderCandidate => ({
  candidate_id: 1,
  model_id: 1,
  model_name: 'm',
  provider_model_name: 'm',
  input_price: 0,
  output_price: 0,
  cache_write_price: null,
  cache_read_price: null,
  billing_mode: 'token',
  image_pricing_tiers: null,
  video_pricing_tiers: null,
  audio_unit_price: null,
  max_output: 0,
  management_status: 1,
  verification_status: 1,
  last_test_result: 1,
  last_tested_at: '2026-10-01T00:00:00Z',
  last_test_error: null,
  queue_state: '',
  auto_enable_on_pass: false,
  probe_owed: false,
  ...overrides,
})

// The shared three-row fixture:
//   deepseek-v4    — seed-priced, so pre-checked and prefilled (smart default)
//   claude-sonnet-x— already mapped under the CUSTOM public name
//                    "sonnet-public": a settled added row
//   text-embed-3   — unpriced, unchecked, undeclared
function seedStandardFixtures() {
  listCatalogueMock.mockResolvedValue(catalogue(['deepseek-v4', 'claude-sonnet-x', 'text-embed-3']))
  listCandidatesMock.mockResolvedValue({
    list: [candidate({ candidate_id: 11, model_id: 5, model_name: 'sonnet-public', provider_model_name: 'claude-sonnet-x' })],
  })
  suggestPricesMock.mockResolvedValue({ prices: { 'deepseek-v4': price(2, 8) } })
}

const Host = defineComponent({
  components: { ImportModelsModal },
  template:
    '<NConfigProvider><NMessageProvider><NDialogProvider><ImportModelsModal :show="show" :provider-id="3" /></NDialogProvider></NMessageProvider></NConfigProvider>',
  props: {
    show: { type: Boolean, default: false },
  },
})

// A contract-faithful stand-in for ModalDrawer for the close-mid-probe
// scenarios. The real NModal never finishes its leave transition under
// happy-dom (the content hides via display:none and after-leave never fires),
// so reset() would never run there — a browser limitation, not the
// component's. The stub renders the slot and footer verbatim, and emits
// after-leave on the tick after show flips false: exactly the ordering the
// real transition hands reset(), so the tests below exercise the dialog's own
// reset/generation logic, not naive-ui's transition internals.
const FastLeaveDrawer = defineComponent({
  props: {
    show: { type: Boolean, default: false },
    title: { type: String, default: '' },
    confirmText: { type: String, default: '' },
    confirmDisabled: { type: Boolean, default: false },
  },
  emits: ['update:show', 'confirm', 'after-leave'],
  template: `
    <div v-if="show" class="modal-drawer-stub">
      <slot />
      <div class="modal-drawer__actions">
        <button type="button" :disabled="confirmDisabled" @click="$emit('confirm')">{{ confirmText }}</button>
      </div>
    </div>
  `,
  watch: {
    show(open: boolean) {
      if (!open) void nextTick(() => this.$emit('after-leave'))
    },
  },
})

let wrapper: VueWrapper | null = null

function stubMatchMedia(matches: boolean) {
  const mql = {
    matches,
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

function stubDesktopMatchMedia() {
  stubMatchMedia(false)
}

function stubMobileMatchMedia() {
  stubMatchMedia(true)
}

async function openDialog(expectedRows = 3, opts: { fastLeave?: boolean } = {}) {
  const i18n = createI18n({ legacy: false, locale: 'en', fallbackLocale: 'en', messages: { en } })
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [{ path: '/', component: { template: '<div />' } }],
  })
  await router.push('/')
  await router.isReady()
  wrapper = mount(Host, {
    attachTo: document.body,
    props: { show: false },
    global: {
      plugins: [i18n, router, naive],
      ...(opts.fastLeave ? { stubs: { ModalDrawer: FastLeaveDrawer } } : {}),
    },
  })
  // The load watch keys on show; reproduce the real open transition rather
  // than mounting mid-open, then wait for the catalogue rows to land.
  await wrapper.setProps({ show: true })
  await vi.waitFor(() => expect(document.body.querySelectorAll('.import-row').length).toBe(expectedRows))
  await nextTick()
}

function body(): HTMLElement {
  return document.body
}

function listRows(): HTMLElement[] {
  return [...body().querySelectorAll<HTMLElement>('.import-row')]
}

function rowFor(text: string): HTMLElement {
  const row = listRows().find((r) => (r.textContent ?? '').includes(text))
  expect(row, `list row containing "${text}" rendered`).toBeTruthy()
  return row!
}

function clickRow(row: HTMLElement) {
  row.dispatchEvent(new MouseEvent('click', { bubbles: true }))
}

function detailSelect(controlClass: string) {
  const select = wrapper!.findAllComponents(NSelect).find((c) => c.classes().includes(controlClass))
  expect(select, `${controlClass} select found`).toBeTruthy()
  return select!
}

function detailInput(controlClass: string) {
  const input = wrapper!.findAllComponents(NInput).find((c) => c.classes().includes(controlClass))
  expect(input, `${controlClass} input found`).toBeTruthy()
  return input!
}

function detailNumber(controlClass: string): HTMLInputElement {
  const control = wrapper!.findAllComponents(NInputNumber).find((c) => c.classes().includes(controlClass))
  expect(control, `${controlClass} number input found`).toBeTruthy()
  return control!.element.querySelector('input')!
}

function checkboxOf(componentClass: string, within?: HTMLElement) {
  const box = wrapper!
    .findAllComponents(NCheckbox)
    .find((c) => c.classes().includes(componentClass) && (within === undefined || within.contains(c.element as Node)))
  expect(box, `${componentClass} checkbox found`).toBeTruthy()
  return box!
}

function confirmButton(): HTMLButtonElement {
  // The modal footer teleports to body, inside ModalDrawer's actions container.
  const buttons = [...body().querySelectorAll<HTMLButtonElement>('.modal-drawer__actions button')]
  const confirm = buttons.find((b) => (b.textContent ?? '').includes('Import selected'))
  expect(confirm, 'confirm button rendered').toBeTruthy()
  return confirm!
}

function probeButton(): HTMLButtonElement {
  const probe = body().querySelector<HTMLButtonElement>('.import-detail__probe')
  expect(probe, 'probe button rendered').toBeTruthy()
  return probe!
}

async function setFilter(text: string) {
  detailInput('import-toolbar__filter').vm.$emit('update:value', text)
  await nextTick()
}

beforeEach(() => {
  stubDesktopMatchMedia()
  importProviderModelsMock.mockReset()
  importProviderModelsMock.mockResolvedValue({ items: [], created: 0, appended: 0, skipped: 0 })
  probeProviderModelMock.mockReset()
  listCatalogueMock.mockReset()
  listCandidatesMock.mockReset()
  suggestPricesMock.mockReset()
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  vi.clearAllMocks()
  vi.unstubAllGlobals()
  body().innerHTML = ''
})

describe('ImportModelsModal master list (TC-10)', () => {
  it('renders the list set without price columns and detail-binds the first catalog row by default', async () => {
    seedStandardFixtures()
    await openDialog()
    const allRows = listRows()
    expect(allRows).toHaveLength(3)
    // The row anatomy: checkbox, name, chips, dot, suggestion badge — and
    // NOT a single editable price input (they moved to the detail form).
    for (const row of allRows) {
      expect(row.querySelector('.import-row__name'), 'name cell').toBeTruthy()
      expect(row.querySelector('.import-row__chip'), 'modality chip').toBeTruthy()
      expect(row.querySelector('.import-row__dot'), 'status dot').toBeTruthy()
      expect(row.querySelector('.import-row__suggestion'), 'suggestion badge').toBeTruthy()
      expect(checkboxOf('import-row__check-box', row)).toBeTruthy()
    }
    expect(body().querySelectorAll('.import-pane--list .n-input-number')).toHaveLength(0)
    // Catalog order picks the default detail row; it reads selected.
    expect(allRows[0]!.classList.contains('import-row--selected')).toBe(true)
    expect(body().querySelector('.import-detail__name')?.textContent).toBe('deepseek-v4')
    // No public name declared yet: the upstream id shows muted (fallback).
    expect(allRows[0]!.querySelector('.import-row__name')!.classList.contains('import-row__name--fallback')).toBe(true)
    // The custom-named added row shows its stored public name, unmuted.
    expect(rowFor('sonnet-public').querySelector('.import-row__name')!.classList.contains('import-row__name--fallback')).toBe(false)
  })

  it('renders the full detail field set with a live probe button and the guard hint in the DOM', async () => {
    seedStandardFixtures()
    await openDialog()
    for (const cls of [
      'import-detail__pubname',
      'import-detail__ctx',
      'import-detail__in-mods',
      'import-detail__out-mods',
      'import-detail__tools',
      'import-detail__reasoning',
      'import-detail__temp',
      'import-detail__maxout',
      'import-detail__input-price',
      'import-detail__output-price',
      'import-detail__cache-write-price',
      'import-detail__cache-read-price',
      'import-detail__audio-price',
    ]) {
      expect(body().querySelector(`.${cls}`), `${cls} rendered`).toBeTruthy()
    }
    // The probe button is live for a row this dialog can still shape, with
    // the standalone-probe tooltip attached (the disabled tooltip belongs to
    // added rows only).
    const probe = probeButton()
    expect(probe.disabled).toBe(false)
    expect(probe.getAttribute('title')).toBe(en.models.importDetailProbeTip)
    // The bottom guard rail: unpriced rows never auto-enable.
    expect(body().textContent).toContain(en.models.importDetailUnpricedHint)
    expect(body().textContent).toContain(en.models.importDetailPricedHint)
    // The four shared context-length quick presets render beside the input.
    const labels = [...body().querySelectorAll('.import-detail__presets button')].map((b) => (b.textContent ?? '').trim())
    expect(labels).toEqual(['32K', '128K', '200K', '1M'])
    // One preset click fills the input (the same tiers the model edit uses).
    const preset1M = [...body().querySelectorAll<HTMLButtonElement>('.import-detail__presets button')].find((b) =>
      (b.textContent ?? '').trim() === '1M',
    )
    preset1M!.dispatchEvent(new Event('click'))
    await nextTick()
    expect(detailNumber('import-detail__ctx').value).toBe('1048576')
  })

  it('moves the detail binding on row click; an added row reads disabled with the server-owned note', async () => {
    seedStandardFixtures()
    await openDialog()
    clickRow(rowFor('text-embed-3'))
    await nextTick()
    expect(body().querySelector('.import-detail__name')?.textContent).toBe('text-embed-3')
    expect(rowFor('text-embed-3').classList.contains('import-row--selected')).toBe(true)
    // An unpriced row opens with blank price inputs.
    expect(detailNumber('import-detail__input-price').value).toBe('')
    // The added (settled) row: the whole declaration form reads disabled and
    // carries the explanation — checking it only requeues a probe.
    clickRow(rowFor('sonnet-public'))
    await nextTick()
    expect(detailNumber('import-detail__input-price').disabled).toBe(true)
    expect(detailNumber('import-detail__audio-price').disabled).toBe(true)
    expect(detailInput('import-detail__pubname').props('disabled')).toBe(true)
    expect(detailSelect('import-detail__tools').props('disabled')).toBe(true)
    expect(body().textContent).toContain(en.models.importDetailAddedNote)
    // The probe button stays disabled there too — an added row's declarations
    // are server-owned, so there is nothing a probe could fill.
    expect(probeButton().disabled).toBe(true)
    expect(probeButton().getAttribute('title')).toBe(en.models.importDetailProbeDisabledTip)
  })

  it('wires row checkboxes to the import count and disables the confirm at zero', async () => {
    seedStandardFixtures()
    await openDialog()
    // Smart default: the seed-priced row starts checked.
    expect(checkboxOf('import-row__check-box', rowFor('deepseek-v4')).props('checked')).toBe(true)
    expect(confirmButton().textContent).toContain('Import selected (1)')
    expect(body().querySelector('.import-toolbar__count')?.textContent).toContain('1 selected')
    expect(confirmButton().disabled).toBe(false)
    // Checking an unpriced row bumps the count.
    checkboxOf('import-row__check-box', rowFor('text-embed-3')).vm.$emit('update:checked', true)
    await nextTick()
    expect(confirmButton().textContent).toContain('Import selected (2)')
    // Unchecking everything drives the count to 0 — and the footer's button
    // to disabled (the no-op import is blocked at the affordance, not just
    // warned about after the click).
    checkboxOf('import-row__check-box', rowFor('deepseek-v4')).vm.$emit('update:checked', false)
    await nextTick()
    checkboxOf('import-row__check-box', rowFor('text-embed-3')).vm.$emit('update:checked', false)
    await nextTick()
    expect(confirmButton().textContent).toContain('Import selected (0)')
    expect(confirmButton().disabled).toBe(true)
    expect(body().querySelector('.import-toolbar__count')?.textContent).toContain('0 selected')
  })

  it('filters rows by the public display name and by the upstream id', async () => {
    seedStandardFixtures()
    await openDialog()
    // Public-name fragment: only the custom-named added row carries it.
    await setFilter('public')
    expect(listRows().map((r) => r.querySelector('.import-row__name')?.textContent)).toEqual(['sonnet-public'])
    // Upstream-id fragment: the same row, matched by its catalog id instead.
    await setFilter('claude-sonnet')
    expect(listRows()).toHaveLength(1)
    expect(listRows()[0]!.textContent).toContain('sonnet-public')
    // A different fragment hits a different row.
    await setFilter('embed')
    expect(listRows()).toHaveLength(1)
    expect(listRows()[0]!.textContent).toContain('text-embed-3')
    // No match at all: the empty note, not a blank pane.
    await setFilter('zzz-no-match')
    expect(listRows()).toHaveLength(0)
    expect(body().textContent).toContain(en.models.importFilterEmpty)
  })

  it('scopes select-all to the filtered view and turns indeterminate on a partial visible selection', async () => {
    // Four fresh unpriced rows, none pre-checked: filtering on "alpha" hides
    // zeta-d, so a select-all that reached past the filter would check a model
    // the admin cannot currently see.
    listCatalogueMock.mockResolvedValue(catalogue(['alpha-a', 'alpha-b', 'alpha-c', 'zeta-d']))
    listCandidatesMock.mockResolvedValue({ list: [] })
    suggestPricesMock.mockResolvedValue({ prices: {} })
    await openDialog(4)
    await setFilter('alpha')
    expect(listRows()).toHaveLength(3)
    checkboxOf('import-toolbar__all').vm.$emit('update:checked', true)
    await nextTick()
    expect(confirmButton().textContent).toContain('Import selected (3)')
    // Only the visible rows flipped: clearing the filter reveals zeta-d still
    // unchecked, while the three filtered-in rows kept their new checks.
    await setFilter('')
    expect(listRows()).toHaveLength(4)
    for (const text of ['alpha-a', 'alpha-b', 'alpha-c']) {
      expect(checkboxOf('import-row__check-box', rowFor(text)).props('checked')).toBe(true)
    }
    expect(checkboxOf('import-row__check-box', rowFor('zeta-d')).props('checked')).toBe(false)
    // Unchecking one visible row leaves SOME but not ALL of the filtered view
    // selected — the select-all box must read indeterminate, not checked.
    checkboxOf('import-row__check-box', rowFor('alpha-b')).vm.$emit('update:checked', false)
    await nextTick()
    const all = checkboxOf('import-toolbar__all')
    expect(all.props('indeterminate')).toBe(true)
    expect(all.props('checked')).toBe(false)
  })

  it('pre-checks and prefills the suggestion-priced row (smart default)', async () => {
    seedStandardFixtures()
    await openDialog()
    // The default-selected first row IS the priced one.
    expect(checkboxOf('import-row__check-box', rowFor('deepseek-v4')).props('checked')).toBe(true)
    expect(body().querySelector('.import-detail__name')?.textContent).toBe('deepseek-v4')
    expect(detailNumber('import-detail__input-price').value).toBe('2')
    expect(detailNumber('import-detail__output-price').value).toBe('8')
    // Its badge names the suggestion source; the unpriced row warns instead.
    expect(rowFor('deepseek-v4').querySelector('.import-row__suggestion')?.textContent).toContain(en.models.importPriceHit)
    expect(rowFor('text-embed-3').querySelector('.import-row__suggestion')?.textContent).toContain(en.models.importNoPrice)
  })
})

describe('ImportModelsModal detail→list mirroring (TC-11)', () => {
  it('① a public-name edit rewrites the list row and keeps the dot grey; ② a capability declaration flips it green', async () => {
    seedStandardFixtures()
    await openDialog()
    clickRow(rowFor('text-embed-3'))
    await nextTick()
    const dot = () => rowFor('my-text-model').querySelector('.import-row__dot')!
    // Step 1 — rename only. The list row follows the detail form live (the
    // muted fallback is gone because a public name now exists), and the dot
    // STAYS grey: a name is not a capability (statusOf ignores pubName).
    detailInput('import-detail__pubname').vm.$emit('update:value', 'my-text-model')
    await nextTick()
    const renamed = rowFor('my-text-model')
    expect(renamed.querySelector('.import-row__name')?.textContent).toBe('my-text-model')
    expect(renamed.querySelector('.import-row__name')!.classList.contains('import-row__name--fallback')).toBe(false)
    expect(dot().classList.contains('import-row__dot--unconfigured')).toBe(true)
    // The untouched row keeps its upstream-id fallback display.
    expect(rowFor('deepseek-v4').querySelector('.import-row__name')!.classList.contains('import-row__name--fallback')).toBe(true)
    // Step 2 — declare one capability. Same row, same name, green dot.
    detailSelect('import-detail__tools').vm.$emit('update:value', 1)
    await nextTick()
    expect(dot().classList.contains('import-row__dot--configured')).toBe(true)
    expect(dot().classList.contains('import-row__dot--unconfigured')).toBe(false)
  })
})

describe('ImportModelsModal detail output-modality exclusivity', () => {
  it('collapses a text+audio output selection to audio alone, on the row and in the picker', async () => {
    seedStandardFixtures()
    await openDialog()
    // The default detail row is deepseek-v4, declared ['text']. Submitting
    // text+audio together must collapse to audio alone — audio is exclusive
    // server-side, and the shared exclusivity watch (not anything
    // picker-local) owns that rule.
    const select = detailSelect('import-detail__out-mods')
    select.vm.$emit('update:value', ['text', 'audio'])
    await nextTick()
    // The picker's bound value is the COLLAPSED declaration...
    expect(select.props('value')).toEqual(['audio'])
    // ...and the list row's chips mirror exactly that declaration.
    const chips = [...rowFor('deepseek-v4').querySelectorAll('.import-row__chip')].map((c) => (c.textContent ?? '').trim())
    expect(chips).toEqual([en.models.outputModalityAudio])
  })
})

describe('ImportModelsModal stacked layout (mobile)', () => {
  it('stacks the list above the detail pane below the mobile breakpoint', async () => {
    // Same matchMedia stub shape as stubDesktopMatchMedia, with matches: true
    // — useIsMobile reads it at setup, so it must be stubbed before the
    // dialog mounts.
    stubMobileMatchMedia()
    seedStandardFixtures()
    await openDialog()
    expect(body().querySelector('.import-layout')!.classList.contains('import-layout--stacked')).toBe(true)
  })
})

describe('ImportModelsModal lost-batch reconciliation', () => {
  it('adopts owed-work rows by provider_model_name, so a custom-named row is not missed', async () => {
    listCatalogueMock.mockResolvedValue(catalogue(['custom-upstream']))
    listCandidatesMock.mockResolvedValue({ list: [] })
    suggestPricesMock.mockResolvedValue({ prices: {} })
    await openDialog(1)
    // One unpriced row; check it and give it a CUSTOM public name, so the
    // submission's upstream id and its public name diverge.
    checkboxOf('import-row__check-box', rowFor('custom-upstream')).vm.$emit('update:checked', true)
    await nextTick()
    detailInput('import-detail__pubname').vm.$emit('update:value', 'public-name')
    await nextTick()
    // The batch request itself is lost after the server committed it.
    importProviderModelsMock.mockRejectedValueOnce(new Error('connection reset'))
    // The reconciliation read then finds the committed mapping: untested,
    // never probed, probe debt standing — visibly owed work, stored under
    // the CUSTOM public name. A model_name match would skip it (the
    // submitted set holds upstream ids only); the provider_model_name match
    // must adopt it.
    listCandidatesMock.mockResolvedValueOnce({
      list: [
        candidate({
          candidate_id: 21,
          model_id: 7,
          model_name: 'public-name',
          provider_model_name: 'custom-upstream',
          verification_status: 0,
          last_test_result: null,
          last_tested_at: null,
          management_status: 2,
          auto_enable_on_pass: false,
          probe_owed: true,
        }),
      ],
    })
    confirmButton().dispatchEvent(new Event('click'))
    const modal = wrapper!.findComponent(ImportModelsModal)
    await vi.waitFor(() => expect(modal.emitted('imported')).toBeTruthy())
    // The submission carried the custom name and the upstream id, as built.
    const submittedBatch = importProviderModelsMock.mock.calls[0]![1] as ImportProviderModelsResult['items']
    expect(submittedBatch).toEqual([
      { provider_model_name: 'custom-upstream', model_name: 'public-name', output_modalities: ['text'] },
    ])
    // The adopted row made it into the parent's result — under its public
    // name, pointing at the stored candidate.
    const result = modal.emitted('imported')![0]![0] as ImportProviderModelsResult
    expect(result.items).toEqual([{ name: 'public-name', status: 'appended', candidate_id: 21, model_id: 7 }])
    // The dialog switched to the progress view to watch the adopted probe.
    await vi.waitFor(() => expect(body().textContent).toContain(en.models.importProgressTitle))
  })
})

describe('ImportModelsModal single-model probe (TC-16)', () => {
  // Every scenario probes the same row: text-embed-3, the unpriced,
  // undeclared, unchecked one — so any value the backfill writes is visible
  // as a change from the all-undeclared baseline.
  async function openUndeclaredRow(opts: { fastLeave?: boolean } = {}) {
    seedStandardFixtures()
    await openDialog(3, opts)
    clickRow(rowFor('text-embed-3'))
    await nextTick()
  }

  it('runs the probe with the row\'s output declaration: Probing… while in flight, then the verdict bar lands', async () => {
    await openUndeclaredRow()
    let resolveProbe!: (v: unknown) => void
    probeProviderModelMock.mockReturnValue(new Promise((resolve) => { resolveProbe = resolve }))
    probeButton().dispatchEvent(new Event('click'))
    await nextTick()
    // In flight: the button disables itself and names the state, so a slow
    // upstream reads as progress, not as a dead dialog.
    expect(probeButton().disabled).toBe(true)
    expect(probeButton().textContent).toContain(en.models.importProbeRunning)
    // The request carried the upstream id AND the row's output-modality
    // declaration — it decides which endpoint family the probe speaks.
    expect(probeProviderModelMock).toHaveBeenCalledWith(3, {
      provider_model_name: 'text-embed-3',
      output_modalities: ['text'],
    })
    resolveProbe(probeResult())
    await vi.waitFor(() => expect(body().textContent).toContain(en.models.importProbeReportTitle))
    // Settled: the button is live again and the bar names all four probes
    // plus the summed time (4 arms x 400ms).
    expect(probeButton().disabled).toBe(false)
    expect(body().textContent).toContain(en.models.basicText)
    expect(body().textContent).toContain(en.models.supportsStreaming)
    expect(body().textContent).toContain(en.models.supportsFunctionCalling)
    expect(body().textContent).toContain(en.models.importProbeVisionLabel)
    expect(body().textContent).toContain(en.models.importProbeDuration.replace('{ms}', '1600'))
  })

  it('① backfills undeclared slots on a double confirmation (tools=1, inMods=[text,image]) with suggestion badges, and saves nothing', async () => {
    await openUndeclaredRow()
    probeProviderModelMock.mockResolvedValue(probeResult())
    probeButton().dispatchEvent(new Event('click'))
    await vi.waitFor(() => expect(body().textContent).toContain(en.models.importProbeReportTitle))
    // Both undeclared slots were filled by the probe's verdicts.
    expect(detailSelect('import-detail__tools').props('value')).toBe(1)
    expect(detailSelect('import-detail__in-mods').props('value')).toEqual(['text', 'image'])
    // Both field labels carry the probe-suggested badge — exactly the two.
    expect(body().querySelectorAll('.import-field__suggested')).toHaveLength(2)
    // Nothing was submitted: probing shapes the form only, and the row it
    // touched stays un-checked — the pending selection is still just the
    // smart-default deepseek-v4, so the pending import payload is unchanged.
    expect(importProviderModelsMock).not.toHaveBeenCalled()
    expect(confirmButton().textContent).toContain('Import selected (1)')
  })

  it('② a negative verdict on both arms writes nothing: no backfill, no badge, no kept-hint — but the vision refusal shows verbatim', async () => {
    await openUndeclaredRow()
    probeProviderModelMock.mockResolvedValue(
      probeResult({
        function_calling: verdict(false, 0),
        vision: verdict(false, 0, 'HTTP 400: image parts not accepted'),
      }),
    )
    probeButton().dispatchEvent(new Event('click'))
    await vi.waitFor(() => expect(body().textContent).toContain(en.models.importProbeReportTitle))
    expect(detailSelect('import-detail__tools').props('value')).toBe(0)
    expect(detailSelect('import-detail__in-mods').props('value')).toEqual([])
    expect(body().querySelectorAll('.import-field__suggested')).toHaveLength(0)
    expect(body().textContent).not.toContain(en.models.importProbeToolsKept)
    expect(body().textContent).not.toContain(en.models.importProbeVisionKept)
    // A failed vision arm quotes the upstream's own words verbatim.
    expect(body().textContent).toContain('HTTP 400: image parts not accepted')
  })

  it('③ a hand-declared tools=2 survives a confirming function-calling probe; only the kept-hint renders', async () => {
    await openUndeclaredRow()
    detailSelect('import-detail__tools').vm.$emit('update:value', 2)
    await nextTick()
    probeProviderModelMock.mockResolvedValue(probeResult({ vision: verdict(false, 0) }))
    probeButton().dispatchEvent(new Event('click'))
    await vi.waitFor(() => expect(body().textContent).toContain(en.models.importProbeReportTitle))
    // The manual "not supported" declaration was NOT overwritten by the
    // probe's "supported" — the admin's word wins.
    expect(detailSelect('import-detail__tools').props('value')).toBe(2)
    expect(body().querySelectorAll('.import-field__suggested')).toHaveLength(0)
    expect(body().textContent).toContain(en.models.importProbeToolsKept)
  })

  it('④ a declared input set [text] is not extended with image on a confirming vision probe; only the kept-hint renders', async () => {
    await openUndeclaredRow()
    detailSelect('import-detail__in-mods').vm.$emit('update:value', ['text'])
    await nextTick()
    probeProviderModelMock.mockResolvedValue(probeResult({ function_calling: verdict(false, 0) }))
    probeButton().dispatchEvent(new Event('click'))
    await vi.waitFor(() => expect(body().textContent).toContain(en.models.importProbeReportTitle))
    expect(detailSelect('import-detail__in-mods').props('value')).toEqual(['text'])
    expect(body().querySelectorAll('.import-field__suggested')).toHaveLength(0)
    expect(body().textContent).toContain(en.models.importProbeVisionKept)
  })

  it('renders the request-rejected outcome as "Request rejected" instead of falling back to "Upstream error"', async () => {
    await openUndeclaredRow()
    probeProviderModelMock.mockResolvedValue(
      probeResult({
        function_calling: verdict(true, 0),
        vision: verdict(false, TEST_OUTCOME_REQUEST_REJECTED, 'HTTP 415: unsupported media type'),
      }),
    )
    probeButton().dispatchEvent(new Event('click'))
    await vi.waitFor(() => expect(body().textContent).toContain(en.models.importProbeReportTitle))
    // The eleventh Go enum entry names its own category...
    expect(body().textContent).toContain(en.providers.outcomeRequestRejected)
    // ...and never the out-of-range fallback the missing entry used to
    // produce (which pointed at a server fault that does not exist).
    expect(body().textContent).not.toContain(en.providers.outcomeUpstreamError)
    // The refusal's own body still shows verbatim.
    expect(body().textContent).toContain('HTTP 415: unsupported media type')
  })

  it('a failed basic arm quotes its detail verbatim — the no-usable-key path has no other explanation', async () => {
    await openUndeclaredRow()
    probeProviderModelMock.mockResolvedValue(
      probeResult({
        basic: verdict(false, 7, 'No enabled, usable key for this provider'),
        vision: verdict(false, 0, 'HTTP 400: image parts not accepted'),
      }),
    )
    probeButton().dispatchEvent(new Event('click'))
    await vi.waitFor(() => expect(body().textContent).toContain(en.models.importProbeReportTitle))
    // Both failing arms quote their upstream words, each in its own pre —
    // basic first, exactly the channel the vision refusal always had.
    const pres = [...body().querySelectorAll('.upstream-detail')]
    expect(pres).toHaveLength(2)
    expect(pres[0]!.textContent).toContain('No enabled, usable key for this provider')
    expect(pres[1]!.textContent).toContain('HTTP 400: image parts not accepted')
  })

  it('a probe left in flight across close/reopen starts the fresh session clean, and its late result lands nowhere', async () => {
    await openUndeclaredRow({ fastLeave: true })
    let resolveStale!: (v: unknown) => void
    probeProviderModelMock.mockReturnValue(new Promise((resolve) => { resolveStale = resolve }))
    probeButton().dispatchEvent(new Event('click'))
    await nextTick()
    expect(probeButton().disabled).toBe(true)
    // Close mid-probe (reset owns the flag), then reopen: the fresh session's
    // button must be immediately usable — not pinned on "Probing…" by the old
    // generation's request, which may never land.
    await wrapper!.setProps({ show: false })
    await vi.waitFor(() => expect(body().querySelector('.import-row')).toBeNull())
    await wrapper!.setProps({ show: true })
    await vi.waitFor(() => expect(body().querySelectorAll('.import-row')).toHaveLength(3))
    expect(probeButton().disabled).toBe(false)
    expect(probeButton().textContent).not.toContain(en.models.importProbeRunning)
    expect(probeButton().textContent).toContain(en.models.importDetailProbe)
    // The stale promise landing afterwards is dropped on the floor: no report
    // bar, no backfill, and the fresh session's button stays usable.
    resolveStale(probeResult())
    await nextTick()
    await nextTick()
    expect(body().textContent).not.toContain(en.models.importProbeReportTitle)
    expect(body().querySelectorAll('.import-field__suggested')).toHaveLength(0)
    expect(probeButton().disabled).toBe(false)
  })

  it('a probe that fails after the dialog closed must not toast its error into the reopened session', async () => {
    await openUndeclaredRow({ fastLeave: true })
    let rejectStale!: (e: unknown) => void
    probeProviderModelMock.mockReturnValue(new Promise((_, reject) => { rejectStale = reject }))
    probeButton().dispatchEvent(new Event('click'))
    await nextTick()
    await wrapper!.setProps({ show: false })
    await vi.waitFor(() => expect(body().querySelector('.import-row')).toBeNull())
    await wrapper!.setProps({ show: true })
    await vi.waitFor(() => expect(body().querySelectorAll('.import-row')).toHaveLength(3))
    // The old generation's probe fails while the fresh session is open: the
    // error is dropped silently (no toast — a plain Error localizes to the
    // shared network-error text), and the fresh session's button is untouched.
    rejectStale(new Error('connection reset'))
    await nextTick()
    await nextTick()
    expect(body().querySelector('.n-message--error-type')).toBeNull()
    expect(body().textContent).not.toContain(en.common.networkError)
    expect(probeButton().disabled).toBe(false)
  })

  it('editing an applied suggestion away clears its probe-suggested badge', async () => {
    await openUndeclaredRow()
    probeProviderModelMock.mockResolvedValue(probeResult({ vision: verdict(false, 0) }))
    probeButton().dispatchEvent(new Event('click'))
    await vi.waitFor(() => expect(body().textContent).toContain(en.models.importProbeReportTitle))
    // The tools slot was backfilled (applied): one badge on the field label.
    expect(detailSelect('import-detail__tools').props('value')).toBe(1)
    expect(body().querySelectorAll('.import-field__suggested')).toHaveLength(1)
    // The admin overrides the suggested value by hand: the badge would now
    // vouch for a value that is no longer in the field, so it goes — and no
    // kept-hint appears in its place (the suggestion is gone, not kept).
    detailSelect('import-detail__tools').vm.$emit('update:value', 2)
    await nextTick()
    expect(detailSelect('import-detail__tools').props('value')).toBe(2)
    expect(body().querySelectorAll('.import-field__suggested')).toHaveLength(0)
    expect(body().textContent).not.toContain(en.models.importProbeToolsKept)
  })

  it('clearing a declared slot after a not-applied probe drops the stale kept-hint', async () => {
    await openUndeclaredRow()
    detailSelect('import-detail__tools').vm.$emit('update:value', 2)
    await nextTick()
    probeProviderModelMock.mockResolvedValue(probeResult({ vision: verdict(false, 0) }))
    probeButton().dispatchEvent(new Event('click'))
    await vi.waitFor(() => expect(body().textContent).toContain(en.models.importProbeReportTitle))
    // The probe confirmed tools but the row declared "not supported": the
    // kept-hint renders and nothing was written.
    expect(detailSelect('import-detail__tools').props('value')).toBe(2)
    expect(body().querySelectorAll('.import-field__suggested')).toHaveLength(0)
    expect(body().textContent).toContain(en.models.importProbeToolsKept)
    // The admin then clears the declaration to undeclared: the hint's claim
    // ("your declaration was left unchanged") no longer describes the form,
    // so it goes with the state it described.
    detailSelect('import-detail__tools').vm.$emit('update:value', 0)
    await nextTick()
    expect(detailSelect('import-detail__tools').props('value')).toBe(0)
    expect(body().textContent).not.toContain(en.models.importProbeToolsKept)
  })

  it('adding image by hand after a not-applied vision probe retires the vision kept-hint', async () => {
    await openUndeclaredRow()
    detailSelect('import-detail__in-mods').vm.$emit('update:value', ['text'])
    await nextTick()
    probeProviderModelMock.mockResolvedValue(probeResult({ function_calling: verdict(false, 0) }))
    probeButton().dispatchEvent(new Event('click'))
    await vi.waitFor(() => expect(body().textContent).toContain(en.models.importProbeReportTitle))
    // Vision confirmed but ['text'] was already declared: nothing written,
    // the kept-hint ("consider adding Image") is the only trace.
    expect(detailSelect('import-detail__in-mods').props('value')).toEqual(['text'])
    expect(body().querySelectorAll('.import-field__suggested')).toHaveLength(0)
    expect(body().textContent).toContain(en.models.importProbeVisionKept)
    // The admin carries out the hint's own ask by hand: image joins the set,
    // so the form now declares what the probe suggested — the hint retires
    // with the stale claim it was describing.
    detailSelect('import-detail__in-mods').vm.$emit('update:value', ['text', 'image'])
    await nextTick()
    expect(detailSelect('import-detail__in-mods').props('value')).toEqual(['text', 'image'])
    expect(body().textContent).not.toContain(en.models.importProbeVisionKept)
  })

  it('flipping tools to the suggested supported value after a not-applied probe retires the tools kept-hint', async () => {
    await openUndeclaredRow()
    detailSelect('import-detail__tools').vm.$emit('update:value', 2)
    await nextTick()
    probeProviderModelMock.mockResolvedValue(probeResult({ vision: verdict(false, 0) }))
    probeButton().dispatchEvent(new Event('click'))
    await vi.waitFor(() => expect(body().textContent).toContain(en.models.importProbeReportTitle))
    expect(detailSelect('import-detail__tools').props('value')).toBe(2)
    expect(body().querySelectorAll('.import-field__suggested')).toHaveLength(0)
    expect(body().textContent).toContain(en.models.importProbeToolsKept)
    // The admin corrects the declaration to "supported" — exactly the value
    // the probe would have written into an undeclared slot: "your declaration
    // was left unchanged" no longer describes the form, so the hint retires.
    detailSelect('import-detail__tools').vm.$emit('update:value', 1)
    await nextTick()
    expect(detailSelect('import-detail__tools').props('value')).toBe(1)
    expect(body().textContent).not.toContain(en.models.importProbeToolsKept)
  })

  it('a re-probe over slots the last probe filled keeps the applied badges — no kept-hint downgrade', async () => {
    await openUndeclaredRow()
    probeProviderModelMock.mockResolvedValue(probeResult())
    probeButton().dispatchEvent(new Event('click'))
    await vi.waitFor(() => expect(body().textContent).toContain(en.models.importProbeReportTitle))
    expect(body().querySelectorAll('.import-field__suggested')).toHaveLength(2)
    // Second round, same verdicts, no manual edit in between: the values in
    // the slots are still the first probe's backfill — a hand edit would have
    // retired the applied state — so the badges stay applied. Demoting them
    // to the kept-hints would attribute the values to admin declarations
    // that were never made.
    probeButton().dispatchEvent(new Event('click'))
    await vi.waitFor(() => expect(probeProviderModelMock).toHaveBeenCalledTimes(2))
    await nextTick()
    expect(detailSelect('import-detail__tools').props('value')).toBe(1)
    expect(detailSelect('import-detail__in-mods').props('value')).toEqual(['text', 'image'])
    expect(body().querySelectorAll('.import-field__suggested')).toHaveLength(2)
    expect(body().textContent).not.toContain(en.models.importProbeToolsKept)
    expect(body().textContent).not.toContain(en.models.importProbeVisionKept)
  })
})
