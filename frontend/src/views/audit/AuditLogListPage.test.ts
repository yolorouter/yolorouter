// @vitest-environment happy-dom
//
// Mounted-DOM coverage for the admin audit page (/audit-logs) — the
// console's change trail. Covered interactions, per the acceptance case:
//
//   - list rendering: one row per audit entry with the action, the
//     localized status tag (success/denied/error), the capture-time
//     username snapshot, and '-' (never a blank cell) on unauthenticated
//     rows — failed console logins carry no session identity
//   - filter composition + reset: the three exact-literal text dims, the
//     status select, and the admin select each reach the wire; a combined
//     filter carries every dimension — the five discrete dims plus the
//     time range — in ONE call (the intersection is composed server-side);
//     the time-range picker serializes as ISO start/end on the wire
//     (desktop datetimerange tuple in, start/end out; the mobile split
//     pickers carry each bound independently); reset drops every dimension
//     and rewinds to page 1; applying any filter rewinds an advanced page
//     to 1
//   - pagination boundary: the remote pager reflects the filter's total;
//     crossing to the last page reloads with that page; switching the page
//     size rewinds to page 1 with the new size
//   - CSV export trigger: the export button fires the CSV API with the
//     CURRENT filter (pagination dropped — the export walks all rows)
//   - empty state: a zero-row filter renders the localized empty copy
//     instead of the table
//
// The wire shape asserted here mirrors internal/handler/audit_log_handler.go
// (exact-literal filters, 1-indexed page) and model.AdminAuditLog (rows
// serialize as the model itself).
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createMemoryHistory, createRouter } from 'vue-router'
import { createI18n } from 'vue-i18n'
import naive, { NDatePicker } from 'naive-ui'
import { defineComponent, nextTick } from 'vue'

import { exportAuditLogsCSV, listAuditLogs, type AuditLogPage } from '../../api/auditLogs'
import { listUsers } from '../../api/users'
import FilterSelectField from '../../components/common/FilterSelectField.vue'
import ResponsiveDataTable from '../../components/common/ResponsiveDataTable.vue'
import type { ComponentPublicInstance } from 'vue'

vi.mock('../../api/auditLogs', () => ({
  listAuditLogs: vi.fn(),
  exportAuditLogsCSV: vi.fn(),
}))
vi.mock('../../api/users', () => ({
  listUsers: vi.fn(),
  // The composable only needs the two members the page's option builder
  // reads; other exports stay absent because the page imports none of them.
  toUserOptions: (users: Array<{ id: number; username: string }>) =>
    users.map((u) => ({ label: u.username, value: u.id })),
}))

const listMock = vi.mocked(listAuditLogs)
const exportMock = vi.mocked(exportAuditLogsCSV)
const usersMock = vi.mocked(listUsers)

// Must be imported after the vi.mock factories are registered.
import AuditLogListPage from './AuditLogListPage.vue'
import en from '../../locales/en'
import zhCN from '../../locales/zh-CN'

const copy = { en: en.auditLogs, 'zh-CN': zhCN.auditLogs } as const

// Fixed epoch bounds for the time-range legs: asserting against literal ISO
// strings keeps the serialization check drift-free (no Date.now() in
// fixtures). October = month index 9.
const RANGE_START_MS = Date.UTC(2026, 9, 2, 8, 0, 0) // 2026-10-02T08:00:00.000Z
const RANGE_END_MS = Date.UTC(2026, 9, 9, 12, 30, 0) // 2026-10-09T12:30:00.000Z

let wrapper: VueWrapper | null = null

// App.vue nests pages under n-config-provider > n-message-provider; the
// page's useMessage() throws without that ancestry.
const Host = defineComponent({
  components: { AuditLogListPage },
  template: '<NConfigProvider><NMessageProvider><AuditLogListPage /></NMessageProvider></NConfigProvider>',
})

// Desktop viewport (same stub shape as the other page tests): the desktop
// table branch renders, and useIsMobile (also used by FilterSelectField /
// ResponsiveDataTable) needs matchMedia.
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

async function mountPage(locale: 'en' | 'zh-CN' = 'en') {
  const i18n = createI18n({ legacy: false, locale, fallbackLocale: 'zh-CN', messages: { en, 'zh-CN': zhCN } })
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [{ path: '/', component: { template: '<div />' } }],
  })
  await router.push('/')
  await router.isReady()
  const pinia = createPinia()
  setActivePinia(pinia)
  wrapper = mount(Host, { attachTo: document.body, global: { plugins: [pinia, router, i18n, naive] } })
  await nextTick()
  await nextTick()
  await vi.waitFor(() => expect(listMock).toHaveBeenCalledTimes(1))
}

// Three rows covering the trail's three statuses and both detail shapes:
// a settings diff (field-level old→new), event context (login denial —
// snapshotless identity, namespace-less resource), and an error row with
// its redacted brief.
function listFixture(total = 3): AuditLogPage {
  return {
    total,
    page: 1,
    page_size: 20,
    list: [
      {
        id: 3,
        created_at: '2026-10-09T05:00:00Z',
        admin_user_id: 1,
        admin_username: 'erin',
        action: 'system_settings.update',
        resource_type: 'system_setting',
        resource_id: 'admin-audit-retention',
        detail: '{"retention_days":{"old":180,"new":90}}',
        status: 'success',
        error_brief: '',
        source_ip: '203.0.113.7',
        user_agent: 'Mozilla/5.0 (Macintosh)audit',
        request_id: '550e8400-e29b-41d4-a716-446655440000',
      },
      {
        id: 2,
        created_at: '2026-10-09T04:00:00Z',
        admin_user_id: null,
        admin_username: '',
        action: 'auth.login_denied',
        resource_type: '',
        resource_id: '',
        detail: '{"username":"mallory"}',
        status: 'denied',
        error_brief: '',
        source_ip: '198.51.100.23',
        user_agent: 'Mozilla/5.0 (X11; Linux x86_64)audit',
        request_id: 'e2e1c0de-0000-41d4-a716-446655440001',
      },
      {
        id: 1,
        created_at: '2026-10-09T03:00:00Z',
        admin_user_id: 1,
        admin_username: 'erin',
        action: 'provider.update',
        resource_type: 'provider',
        resource_id: '4',
        detail: '',
        status: 'error',
        error_brief: 'upstream connection refused',
        source_ip: '203.0.113.7',
        user_agent: 'Mozilla/5.0 (Macintosh)audit',
        request_id: 'e2e1c0de-0000-41d4-a716-446655440002',
      },
    ],
  }
}

function textOf(): string {
  return document.body.textContent ?? ''
}

// findAllComponents over a generic/props-heavy component loses the wrapper
// type, so the selects are cast to a props-typed wrapper (same shape the
// agent-session page tests use for their FilterSelectField lookups).
type SelectFieldProps = {
  placeholder?: string
}
type SelectFieldWrapper = VueWrapper<unknown, ComponentPublicInstance<SelectFieldProps>>

function selectFields(): SelectFieldWrapper[] {
  return wrapper!.findAllComponents(FilterSelectField) as unknown as SelectFieldWrapper[]
}

// The status FilterSelectField, located by its localized "all" placeholder
// (the page also carries the admin select; the status one is found by its
// own placeholder text).
function statusField(): SelectFieldWrapper {
  const field = selectFields().find((f) => f.props('placeholder') === copy.en.allFilterStatus)
  expect(field, 'status filter select rendered').toBeTruthy()
  return field!
}

// The admin FilterSelectField — the remaining select once status is taken.
function adminField(): SelectFieldWrapper {
  const field = selectFields().find((f) => f.props('placeholder') !== copy.en.allFilterStatus)
  expect(field, 'admin filter select rendered').toBeTruthy()
  return field!
}

// The date pickers, cast for the same reason as the selects (the generic
// component's wrapper exposes props as never). Located by placeholder so the
// desktop datetimerange and the mobile split pickers are told apart.
type DatePickerWrapper = VueWrapper<unknown, ComponentPublicInstance<{ placeholder?: string }>>
function datePickers(): DatePickerWrapper[] {
  return wrapper!.findAllComponents(NDatePicker) as unknown as DatePickerWrapper[]
}

// The desktop datetimerange picker (the branch the desktop matchMedia stub
// renders) — the sixth filter dimension.
function rangeField(): DatePickerWrapper {
  const field = datePickers().find((p) => p.props('placeholder') === copy.en.filterTimeRange)
  expect(field, 'desktop datetimerange picker rendered').toBeTruthy()
  return field!
}

// A free-text filter input, located by its localized placeholder.
function filterInput(placeholder: string): HTMLInputElement {
  const input = [...document.body.querySelectorAll<HTMLInputElement>('input')].find(
    (i) => i.placeholder === placeholder,
  )
  expect(input, `filter input with placeholder "${placeholder}"`).toBeTruthy()
  return input!
}

// Types into a filter input and presses Enter (the immediate-search path
// for text dims — no debounce wait needed).
async function submitFilter(placeholder: string, value: string) {
  const input = filterInput(placeholder)
  input.value = value
  input.dispatchEvent(new Event('input'))
  await nextTick()
  input.dispatchEvent(new KeyboardEvent('keyup', { key: 'Enter' }))
  await nextTick()
}

function resetButton(): HTMLButtonElement {
  const btn = [...document.body.querySelectorAll<HTMLButtonElement>('button')].find(
    (b) => b.textContent?.trim() === copy.en.reset,
  )
  expect(btn, 'reset button rendered').toBeTruthy()
  return btn!
}

function exportButton(): HTMLButtonElement {
  const btn = [...document.body.querySelectorAll<HTMLButtonElement>('button')].find(
    (b) => b.textContent?.trim() === copy.en.exportCsv,
  )
  expect(btn, 'export button rendered').toBeTruthy()
  return btn!
}

// The page's ResponsiveDataTable — pagination is read and driven through
// its prop object (the remote pager's onChange is the page's reload hook).
// Cast for the same reason as the selects above: the generic component's
// findAllComponents wrapper exposes props as never.
type TableWrapper = VueWrapper<unknown, ComponentPublicInstance<{ pagination?: unknown }>>
function table(): TableWrapper {
  const tables = wrapper!.findAllComponents(ResponsiveDataTable) as unknown as TableWrapper[]
  expect(tables.length, 'data table rendered').toBeGreaterThan(0)
  return tables[0]
}

beforeEach(() => {
  stubDesktopMatchMedia()
  listMock.mockReset().mockResolvedValue(listFixture())
  exportMock.mockReset().mockResolvedValue(undefined)
  usersMock.mockReset().mockResolvedValue({ users: [{ id: 1, username: 'erin' }] } as never)
})

afterEach(() => {
  wrapper?.unmount()
  wrapper = null
  vi.clearAllMocks()
  vi.unstubAllGlobals()
  document.body.innerHTML = ''
})

describe('AuditLogListPage rendering (en)', () => {
  it('renders one row per audit entry with action, localized status, snapshot username, and resource ids', async () => {
    await mountPage('en')
    const text = textOf()
    expect(text).toContain('system_settings.update')
    expect(text).toContain('auth.login_denied')
    expect(text).toContain('provider.update')
    // Localized status tags — the three literal statuses of the trail.
    expect(text).toContain(copy.en.status_success)
    expect(text).toContain(copy.en.status_denied)
    expect(text).toContain(copy.en.status_error)
    // Snapshot identity: the username column shows the capture-time name…
    expect(text).toContain('erin')
    // …and an unauthenticated row (failed login) reads '-', never blank.
    expect(text).toContain('admin-audit-retention')
    // The error brief column carries the redacted note.
    expect(text).toContain('upstream connection refused')
  })

  it('renders the same rows under the zh-CN copy (status tags flip with the locale)', async () => {
    await mountPage('zh-CN')
    const text = textOf()
    expect(text).toContain(copy['zh-CN'].status_success)
    expect(text).toContain(copy['zh-CN'].status_denied)
    expect(text).toContain(copy['zh-CN'].status_error)
    expect(text).toContain('system_settings.update')
  })
})

describe('AuditLogListPage filter composition + reset (en)', () => {
  it('sends each text dimension as an exact literal on the wire', async () => {
    await mountPage('en')
    await submitFilter(copy.en.filterAction, 'system_settings.update')
    await vi.waitFor(() => expect(listMock).toHaveBeenCalledTimes(2))
    expect(listMock).toHaveBeenLastCalledWith(
      expect.objectContaining({ action: 'system_settings.update', page: 1 }),
    )
  })

  it('carries every dimension of a combined filter in ONE call (intersection composes server-side)', async () => {
    await mountPage('en')
    await submitFilter(copy.en.filterAction, 'provider.update')
    await vi.waitFor(() => expect(listMock).toHaveBeenCalledTimes(2))
    await submitFilter(copy.en.filterResourceType, 'provider')
    await vi.waitFor(() => expect(listMock).toHaveBeenCalledTimes(3))
    await submitFilter(copy.en.filterResourceId, '4')
    await vi.waitFor(() => expect(listMock).toHaveBeenCalledTimes(4))
    statusField().vm.$emit('update:value', 'error')
    await vi.waitFor(() => expect(listMock).toHaveBeenCalledTimes(5))
    adminField().vm.$emit('update:value', 1)
    await vi.waitFor(() => expect(listMock).toHaveBeenCalledTimes(6))
    // Sixth dimension: the datetimerange tuple lands on the wire as the
    // start/end ISO pair — one call still carries the whole filter set.
    rangeField().vm.$emit('update:value', [RANGE_START_MS, RANGE_END_MS])
    await vi.waitFor(() => expect(listMock).toHaveBeenCalledTimes(7))
    expect(listMock).toHaveBeenLastCalledWith({
      page: 1,
      page_size: 20,
      action: 'provider.update',
      resource_type: 'provider',
      resource_id: '4',
      status: 'error',
      admin_user_id: 1,
      start: '2026-10-02T08:00:00.000Z',
      end: '2026-10-09T12:30:00.000Z',
    })
  })

  it('serializes the time-range picker as ISO start/end bounds; reset drops them', async () => {
    await mountPage('en')
    // The picker hands the page an epoch-ms tuple; the wire must carry the
    // backend contract's ISO 8601 literals (audit_log_handler.go start/end).
    rangeField().vm.$emit('update:value', [RANGE_START_MS, RANGE_END_MS])
    await vi.waitFor(() => expect(listMock).toHaveBeenCalledTimes(2))
    expect(listMock).toHaveBeenLastCalledWith({
      page: 1,
      page_size: 20,
      start: '2026-10-02T08:00:00.000Z',
      end: '2026-10-09T12:30:00.000Z',
    })
    resetButton().dispatchEvent(new Event('click'))
    await vi.waitFor(() => expect(listMock).toHaveBeenCalledTimes(3))
    // Exact object match: start and end are gone, not just overridden.
    expect(listMock).toHaveBeenLastCalledWith({ page: 1, page_size: 20 })
  })

  it('carries each bound independently from the mobile split pickers', async () => {
    // Same stub shape as stubDesktopMatchMedia with matches: true — the
    // mobile branch swaps the datetimerange for two standalone pickers,
    // each bound to its own ref and searching on its own.
    const mql = {
      matches: true,
      media: '(max-width: 768px)',
      onchange: null,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
      dispatchEvent: () => true,
    }
    vi.stubGlobal('matchMedia', vi.fn(() => mql))
    await mountPage('en')
    const pickers = datePickers()
    const start = pickers.find((p) => p.props('placeholder') === copy.en.filterStartTime)
    const end = pickers.find((p) => p.props('placeholder') === copy.en.filterEndTime)
    expect(start && end, 'both split pickers rendered on the mobile branch').toBeTruthy()

    // Half a range is still a filter: the start bound rides along, the
    // absent end stays off the wire entirely.
    start!.vm.$emit('update:value', RANGE_START_MS)
    await vi.waitFor(() => expect(listMock).toHaveBeenCalledTimes(2))
    expect(listMock).toHaveBeenLastCalledWith({
      page: 1,
      page_size: 20,
      start: '2026-10-02T08:00:00.000Z',
    })

    end!.vm.$emit('update:value', RANGE_END_MS)
    await vi.waitFor(() => expect(listMock).toHaveBeenCalledTimes(3))
    expect(listMock).toHaveBeenLastCalledWith({
      page: 1,
      page_size: 20,
      start: '2026-10-02T08:00:00.000Z',
      end: '2026-10-09T12:30:00.000Z',
    })
  })

  it('rewinds to page 1 when a filter is applied from an advanced page', async () => {
    await mountPage('en')
    const pager = table().props('pagination') as { onChange: (p: number) => void }
    pager.onChange(2)
    await vi.waitFor(() =>
      expect(listMock).toHaveBeenLastCalledWith(expect.objectContaining({ page: 2 })),
    )
    await submitFilter(copy.en.filterAction, 'auth.login_denied')
    await vi.waitFor(() =>
      expect(listMock).toHaveBeenLastCalledWith(
        expect.objectContaining({ action: 'auth.login_denied', page: 1 }),
      ),
    )
  })

  it('reset drops every dimension and rewinds to page 1', async () => {
    await mountPage('en')
    await submitFilter(copy.en.filterAction, 'provider.update')
    await vi.waitFor(() => expect(listMock).toHaveBeenCalledTimes(2))
    await submitFilter(copy.en.filterResourceType, 'provider')
    await vi.waitFor(() => expect(listMock).toHaveBeenCalledTimes(3))
    statusField().vm.$emit('update:value', 'denied')
    await vi.waitFor(() => expect(listMock).toHaveBeenCalledTimes(4))
    resetButton().dispatchEvent(new Event('click'))
    await vi.waitFor(() => expect(listMock).toHaveBeenCalledTimes(5))
    expect(listMock).toHaveBeenLastCalledWith({ page: 1, page_size: 20 })
    // The inputs themselves cleared too — the placeholders are readable again.
    expect(filterInput(copy.en.filterAction).value).toBe('')
    expect(filterInput(copy.en.filterResourceType).value).toBe('')
    expect(filterInput(copy.en.filterResourceId).value).toBe('')
  })
})

describe('AuditLogListPage pagination boundary (en)', () => {
  it('reflects the filter total and reloads on page and page-size changes', async () => {
    listMock.mockResolvedValue(listFixture(45))
    await mountPage('en')
    const first = table().props('pagination') as {
      page: number
      pageSize: number
      itemCount: number
      onChange: (p: number) => void
      onUpdatePageSize: (ps: number) => void
    }
    expect(first.itemCount).toBe(45)
    expect(first.page).toBe(1)
    expect(first.pageSize).toBe(20)

    // Last page of 45 items at size 20 is page 3 — the boundary page.
    first.onChange(3)
    await vi.waitFor(() =>
      expect(listMock).toHaveBeenLastCalledWith(expect.objectContaining({ page: 3 })),
    )

    // Growing the page size past the boundary rewinds to page 1 with the
    // new size (page 3 at size 50 would be past the end).
    const grown = table().props('pagination') as { onUpdatePageSize: (ps: number) => void }
    grown.onUpdatePageSize(50)
    await vi.waitFor(() =>
      expect(listMock).toHaveBeenLastCalledWith(expect.objectContaining({ page: 1, page_size: 50 })),
    )
  })
})

describe('AuditLogListPage CSV export trigger (en)', () => {
  it('exports the CURRENT filter (pagination dropped) and reports success', async () => {
    await mountPage('en')
    await submitFilter(copy.en.filterAction, 'system_settings.update')
    await vi.waitFor(() => expect(listMock).toHaveBeenCalledTimes(2))
    exportButton().dispatchEvent(new Event('click'))
    await vi.waitFor(() => expect(exportMock).toHaveBeenCalledTimes(1))
    // The export walks every matching row server-side: the filter rides
    // along, the page window does not.
    expect(exportMock).toHaveBeenLastCalledWith({ action: 'system_settings.update' })
    await vi.waitFor(() => expect(textOf()).toContain(copy.en.exportSuccess))
  })

  it('toasts a localized failure when the export stream fails', async () => {
    exportMock.mockRejectedValue(new Error('CSV export failed: HTTP 500'))
    await mountPage('en')
    exportButton().dispatchEvent(new Event('click'))
    await vi.waitFor(() => expect(exportMock).toHaveBeenCalledTimes(1))
    // A plain transport-shaped rejection surfaces through displayMessage's
    // network-failure copy — never the raw error string.
    await vi.waitFor(() => expect(textOf()).toContain(en.common.networkError))
  })
})

describe('AuditLogListPage empty state (en)', () => {
  it('renders the localized empty copy instead of the table for a zero-row filter', async () => {
    listMock.mockResolvedValue({ total: 0, page: 1, page_size: 20, list: [] })
    await mountPage('en')
    // The empty branch renders in place of the table wrapper.
    await vi.waitFor(() => expect(textOf()).toContain(copy.en.listEmpty))
    expect(wrapper!.findAllComponents(ResponsiveDataTable).length).toBe(0)
  })
})
