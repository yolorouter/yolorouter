<!-- frontend/src/views/request-logs/RequestLogListPage.vue
     Request-log list. Server-side paged with a filter set matching what the
     backend handler accepts (request_log_handler.go): request_id /
     model_name / key_prefix / request_path / api_key_id / provider_id /
     status_class / is_stream / cost_known / start / end.

     Rows expand for identity/routing detail (full request id, retry
     breakdown, cache tokens); the View button opens the
     /request-logs/:requestId detail page. Export CSV streams the current
     filter via the same params. No scroll-x: several columns use ellipsis,
     which puts the table in fixed layout, so columns compress to the
     container instead of overflowing sideways. -->
<template>
  <div class="common-page">
    <PageHeader :eyebrow="t('requestLogs.eyebrow')" :title="t('requestLogs.pageTitle')" :description="t('requestLogs.pageDescription')">
      <template #actions>
        <NButton :loading="exporting" :disabled="exporting || loading" @click="onExport">
          <template #icon><Download :size="16" /></template>
          {{ t('requestLogs.exportCsv') }}
        </NButton>
      </template>
    </PageHeader>

    <!-- Filter row. NDatePicker / NSelect are not in main.ts's create()
         list, so they're imported explicitly below. Silently rendering as
         unknown elements is the worst-case failure mode here, not a
         typecheck error. -->
    <div class="filter-panel">
      <div class="filter-grid">
        <div class="filter-item filter-item--grow">
          <NInput
            v-model:value="filter.request_id"
            :placeholder="t('requestLogs.filterRequestId')"
            clearable
            size="small"
            @keyup.enter="onSearch"
            @update:value="onRequestIdInput"
          >
            <template #prefix><Search :size="14" /></template>
          </NInput>
        </div>
        <div class="filter-item filter-item--grow">
          <NInput
            v-model:value="filter.model_name"
            :placeholder="t('requestLogs.filterModel')"
            clearable
            size="small"
            @keyup.enter="onSearch"
            @update:value="onModelNameInput"
          />
        </div>
        <div class="filter-item filter-item--search">
          <NInput
            v-model:value="filter.key_prefix"
            :placeholder="t('requestLogs.filterKeyPrefix')"
            clearable
            size="small"
            @keyup.enter="onSearch"
            @update:value="onFilterChange"
          />
        </div>
        <FilterSelectField
          :label="t('requestLogs.filterUser')"
          :value="filter.user_id"
          :options="userOptions"
          :placeholder="t('common.allAccounts')"
          filterable
          width="100%"
          @update:value="onUserChange"
        />
        <FilterSelectField
          :label="t('requestLogs.filterCaller')"
          :value="filter.api_key_id"
          :options="callerOptions"
          :placeholder="t('requestLogs.allFilterCaller')"
          filterable
          width="100%"
          @update:value="onCallerChange"
        />
        <FilterSelectField
          :label="t('requestLogs.filterProvider')"
          :value="filter.provider_id"
          :options="providerOptions"
          :placeholder="t('requestLogs.allFilterProvider')"
          width="100%"
          @update:value="onProviderChange"
        />
        <FilterSelectField
          :label="t('requestLogs.filterStatus')"
          :value="filter.status"
          :options="statusOptions"
          :placeholder="t('requestLogs.allFilterStatus')"
          width="100%"
          @update:value="onStatusChange"
        />
        <FilterSelectField
          :label="t('requestLogs.filterStream')"
          :value="streamSelect"
          :options="streamOptions"
          :placeholder="t('requestLogs.allFilterStream')"
          width="100%"
          @update:value="onStreamChange"
        />
        <FilterSelectField
          :label="t('requestLogs.filterCostKnown')"
          :value="costSelect"
          :options="costOptions"
          :placeholder="t('requestLogs.allFilterCostKnown')"
          width="100%"
          @update:value="onCostKnownChange"
        />
        <FilterSelectField
          :label="t('requestLogs.filterEndpoint')"
          :value="filter.request_path"
          :options="endpointOptions"
          :placeholder="t('requestLogs.allFilterEndpoint')"
          width="100%"
          @update:value="onEndpointChange"
        />
        <FilterSelectField
          :label="t('requestLogs.filterSource')"
          :value="filter.source"
          :options="sourceOptions"
          :placeholder="t('requestLogs.allFilterSource')"
          width="100%"
          @update:value="onSourceChange"
        />
        <div class="filter-item filter-item--range">
          <!-- Desktop: a single datetimerange picker. On mobile the range
               variant is too wide to fit, so it's split into two standalone
               datetime pickers (start / end) driven by the same startTime /
               endTime refs the range picker writes through. -->
          <NDatePicker
            v-if="!isMobile"
            :value="timeRange"
            type="datetimerange"
            clearable
            size="small"
            :shortcuts="rangeShortcuts"
            :placeholder="t('requestLogs.filterTimeRange')"
            @update:value="onRangeChange"
          />
          <div v-else class="filter-range-split">
            <NDatePicker
              v-model:value="startTime"
              type="datetime"
              clearable
              size="small"
              :placeholder="t('requestLogs.filterStartTime')"
              :is-date-disabled="disableAfterEnd"
              @update:value="onSearch"
            />
            <NDatePicker
              v-model:value="endTime"
              type="datetime"
              clearable
              size="small"
              :placeholder="t('requestLogs.filterEndTime')"
              :is-date-disabled="disableBeforeStart"
              @update:value="onSearch"
            />
          </div>
        </div>
        <div class="filter-actions">
          <NButton size="small" type="primary" @click="onSearch">{{ t('requestLogs.search') }}</NButton>
          <NButton size="small" quaternary @click="onReset">{{ t('requestLogs.reset') }}</NButton>
        </div>
      </div>
    </div>

    <EmptyState v-if="!loading && rows.length === 0" :icon="FileSearch" :title="t('requestLogs.listEmpty')" />
    <div v-else class="data-table-wrapper">
      <ResponsiveDataTable
        :columns="columns"
        :data="rows"
        :loading="loading"
        :row-key="(row: RequestLogRow) => row.request_id"
        :pagination="pagination"
        remote
      >
        <template #empty>
          <EmptyState :icon="FileSearch" :title="t('requestLogs.listEmpty')" />
        </template>
      </ResponsiveDataTable>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, h, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import {
  NButton,
  NDatePicker,
  NInput,
  NTag,
  useMessage,
  type DataTableColumns,
  type PaginationProps,
  type SelectOption,
} from 'naive-ui'
import { Download, FileSearch, Search } from '@lucide/vue'
import {
  listRequestLogs,
  exportRequestLogsCSV,
  type RequestLogRow,
  type RequestLogListParams,
  type StatusClass,
} from '../../api/requestLogs'
import { listProviders, type Provider } from '../../api/providers'
import { listAPIKeys, toAPIKeyOptions, type APIKey } from '../../api/apiKeys'
import { useUserOptions } from '../../composables/useUserOptions'
import { displayMessage } from '../../api/client'
import { formatMicros } from '../../utils/money'
import { formatImagePrice } from '../../utils/imagePriceSummary'
import { columnTitle } from '../../utils/columnTitle'
import PageHeader from '../../components/PageHeader.vue'
import EmptyState from '../../components/EmptyState.vue'
import FilterSelectField from '../../components/common/FilterSelectField.vue'
import ResponsiveDataTable from '../../components/common/ResponsiveDataTable.vue'
import StatusClassTag from '../../components/request-logs/StatusClassTag.vue'
import { useIsMobile } from '../../composables/useIsMobile'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const message = useMessage()

// Filter state — every field the backend actually accepts. `timeRange` is
// the matching pair (start/end) held together as a tuple because
// NDatePicker's datetimerange mode emits them as one value. We split the
// tuple into RFC3339 strings in buildListParams before sending.
interface ListFilter {
  request_id: string
  model_name: string
  key_prefix: string
  api_key_id: number | null
  user_id: number | null
  provider_id: number | null
  status: StatusClass | null
  is_stream: boolean | null
  cost_known: boolean | null
  // The selected endpoint option's value. The backend matches it exactly,
  // except a trailing "/" selects the whole subtree (the Gemini option).
  request_path: string | null
  // "" would double as "all" and "normal", so the wire values are explicit:
  // null = all, 'caller' = normal requests, 'vision_fallback' = sub-calls.
  source: 'caller' | 'vision_fallback' | null
}
const filter = reactive<ListFilter>({
  request_id: '',
  model_name: '',
  key_prefix: '',
  api_key_id: null,
  user_id: null,
  provider_id: null,
  status: null,
  is_stream: null,
  cost_known: null,
  request_path: null,
  source: null,
})
// Stream filter UI value. null means "no filter" (cleared select, matches
// the placeholder's "all streams" wording); 'stream' / 'non-stream' map to
// filter.is_stream = true / false. Wired via :value + @update:value rather
// than v-model so the null → is_stream=null mapping happens in one place
// (onStreamChange).
const streamSelect = ref<'stream' | 'non-stream' | null>(null)
// Same controlled-input shape as streamSelect: the UI value decodes to
// filter.cost_known = true / false / null in one place.
const costSelect = ref<'known' | 'unknown' | null>(null)
// Start / end are the source of truth for the time filter. Desktop binds a
// single datetimerange picker through the `timeRange` computed below; mobile
// binds these two refs directly to standalone datetime pickers. Keeping the
// pair split (rather than a [start, end] tuple) lets the mobile UI set just
// one bound, and lets buildListParams send start / end independently.
const startTime = ref<number | null>(null)
const endTime = ref<number | null>(null)

// Reactive mobile flag — the same breakpoint composable the rest of the app
// uses. Drives the datetimerange (desktop) vs. two datetime pickers (mobile)
// switch in the template.
const isMobile = useIsMobile()

// Adapter for the desktop datetimerange picker: it holds a [start, end] tuple
// or null, so surface both bounds together and only when both are present.
const timeRange = computed<[number, number] | null>(() =>
  startTime.value != null && endTime.value != null ? [startTime.value, endTime.value] : null,
)

// datetimerange emits the whole tuple (or null on clear); fan it back out to
// the two source refs, then search. Shortcuts flow through here too.
function onRangeChange(v: [number, number] | null) {
  startTime.value = v ? v[0] : null
  endTime.value = v ? v[1] : null
  void onSearch()
}

// Cross-bound guards for the two mobile pickers so start can't exceed end and
// vice versa. NDatePicker's is-date-disabled works at day granularity, which
// is enough to keep the pair coherent.
function disableAfterEnd(ts: number): boolean {
  return endTime.value != null && ts > endTime.value
}
function disableBeforeStart(ts: number): boolean {
  return startTime.value != null && ts < startTime.value
}

// Flags tracking whether model_name / request_id originated from a URL
// query param (a deep link from a cost detail page). Values sourced that
// way are EXACT identifiers — analytics may carry intentional surrounding
// whitespace — so they must reach the backend verbatim. The submit-time
// .trim() in buildListParams serves typed input only, where stray
// whitespace is unintended; these flags branch that behavior.
const querySourcedModelName = ref(false)
const querySourcedRequestId = ref(false)

const rows = ref<RequestLogRow[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(20)
const loading = ref(false)
const exporting = ref(false)

// Providers come from the same admin providers endpoint used by the
// providers store; loaded once on mount. We don't go through the Pinia
// store here because the request-logs page is read-only w.r.t. providers
// and doesn't need the store's race-guard / mutation actions — a one-shot
// fetch is simpler and avoids coupling this page to provider CRUD state.
const providers = ref<Provider[]>([])
const providerOptions = computed<SelectOption[]>(() =>
  providers.value.map((p) => ({ label: p.name, value: p.id })),
)

// The "caller" filter reuses the existing api_key_id filter param — the
// backend already filters request_logs by api_key_id, so no backend change is
// needed. Options are the API keys (owner username + key_prefix to
// disambiguate keys sharing an owner account). Revoked keys are kept in the
// list so historical logs of a since-revoked key stay filterable. One-shot
// fetch on mount, same rationale as loadProviders above; 200 covers every
// realistic v0.1 key count without a remote-search handshake.
const apiKeys = ref<APIKey[]>([])
const callerOptions = computed<SelectOption[]>(() => toAPIKeyOptions(apiKeys.value))
const { userOptions, loadUserOptions } = useUserOptions()

const statusOptions = computed<SelectOption[]>(() => ([
  { label: t('requestLogs.status_success'), value: 'success' },
  { label: t('requestLogs.status_failed'), value: 'failed' },
  { label: t('requestLogs.status_partial'), value: 'partial' },
  { label: t('requestLogs.status_cancelled'), value: 'cancelled' },
  { label: t('requestLogs.status_rejected'), value: 'rejected' },
]))

const streamOptions = computed<SelectOption[]>(() => ([
  { label: t('requestLogs.stream_true'), value: 'stream' },
  { label: t('requestLogs.stream_false'), value: 'non-stream' },
]))

const costOptions = computed<SelectOption[]>(() => ([
  { label: t('requestLogs.costKnown_true'), value: 'known' },
  { label: t('requestLogs.costKnown_false'), value: 'unknown' },
]))

// Endpoint options mirror the gateway's ingress routes (router.go). The
// backend matches the value exactly, except a trailing "/" selects the whole
// subtree — the Gemini-compatible ingress embeds the model name in its path
// (/v1beta/models/{model}:{action}), so its option is the family prefix.
const sourceOptions = computed<SelectOption[]>(() => ([
  { label: t('requestLogs.sourceCaller'), value: 'caller' },
  { label: t('requestLogs.sourceVisionFallback'), value: 'vision_fallback' },
]))

const endpointOptions = computed<SelectOption[]>(() => ([
  { label: '/v1/chat/completions', value: '/v1/chat/completions' },
  { label: '/v1/messages', value: '/v1/messages' },
  { label: '/v1/responses', value: '/v1/responses' },
  { label: t('requestLogs.filterEndpointGemini'), value: '/v1beta/' },
]))

// Preset shortcuts for the date-range picker: today / yesterday / last 7
// days / last 30 days. End is set to "now" for the rolling windows so the
// preset matches the admin's mental model ("last 7 days" includes today),
// not "midnight 7 days ago to midnight now".
const rangeShortcuts = computed<Record<string, () => [number, number]>>(() => ({
  [t('requestLogs.rangeToday')]: () => {
    const now = Date.now()
    const startOfToday = new Date()
    startOfToday.setHours(0, 0, 0, 0)
    return [startOfToday.getTime(), now]
  },
  [t('requestLogs.rangeYesterday')]: () => {
    const start = new Date()
    start.setDate(start.getDate() - 1)
    start.setHours(0, 0, 0, 0)
    const end = new Date()
    end.setHours(0, 0, 0, 0)
    return [start.getTime(), end.getTime()]
  },
  [t('requestLogs.range7d')]: () => [Date.now() - 7 * 24 * 60 * 60 * 1000, Date.now()],
  [t('requestLogs.range30d')]: () => [Date.now() - 30 * 24 * 60 * 60 * 1000, Date.now()],
}))

let searchTimer: ReturnType<typeof setTimeout> | null = null
onBeforeUnmount(() => {
  if (searchTimer) {
    clearTimeout(searchTimer)
    searchTimer = null
  }
})

// URL query keys this page knows how to ingest. Used by hasRelevantQuery
// to decide whether mount should apply the query before the first load.
const RELEVANT_QUERY_KEYS = [
  'request_id', 'model_name', 'key_prefix', 'request_path', 'source', 'api_key_id',
  'user_id', 'provider_id', 'status', 'is_stream', 'cost_known', 'start', 'end',
] as const

function hasRelevantQuery(): boolean {
  const q = route.query
  return RELEVANT_QUERY_KEYS.some((k) => {
    const v = q[k]
    return v != null && v !== ''
  })
}

// applyQueryFilter maps deep-link query params onto the local filter model.
// Cost detail pages emit /request-logs?api_key_id=X&start=...&end=... etc.
// — start/end arrive as RFC3339 strings and are converted to the epoch-ms
// tuple NDatePicker holds. is_stream's UI value mirrors the streamSelect
// ref ('stream' | 'non-stream' | null). model_name and request_id are
// preserved verbatim (see querySourced* flags).
function applyQueryFilter() {
  const q = route.query
  if (typeof q.request_id === 'string' && q.request_id) {
    filter.request_id = q.request_id
    querySourcedRequestId.value = true
  }
  if (typeof q.model_name === 'string' && q.model_name) {
    filter.model_name = q.model_name
    querySourcedModelName.value = true
  }
  if (typeof q.key_prefix === 'string' && q.key_prefix) {
    filter.key_prefix = q.key_prefix
  }
  if (typeof q.request_path === 'string' && q.request_path) {
    filter.request_path = q.request_path
  }
  if (q.source === 'caller' || q.source === 'vision_fallback') {
    filter.source = q.source
  }
  if (typeof q.api_key_id === 'string' && q.api_key_id) {
    const n = Number(q.api_key_id)
    if (!Number.isNaN(n)) filter.api_key_id = n
  }
  if (typeof q.user_id === 'string' && q.user_id) {
    const n = Number(q.user_id)
    if (!Number.isNaN(n)) filter.user_id = n
  }
  if (typeof q.provider_id === 'string' && q.provider_id) {
    const n = Number(q.provider_id)
    if (!Number.isNaN(n)) filter.provider_id = n
  }
  if (typeof q.status === 'string' && q.status) {
    filter.status = q.status as StatusClass
  }
  if (typeof q.cost_known === 'string') {
    const v: 'known' | 'unknown' | null =
      q.cost_known === 'true' ? 'known'
        : q.cost_known === 'false' ? 'unknown'
          : null
    costSelect.value = v
    filter.cost_known = v === 'known' ? true : v === 'unknown' ? false : null
  }
  if (typeof q.is_stream === 'string') {
    const v: 'stream' | 'non-stream' | null =
      q.is_stream === 'true' ? 'stream'
        : q.is_stream === 'false' ? 'non-stream'
          : null
    streamSelect.value = v
    filter.is_stream = v === 'stream' ? true : v === 'non-stream' ? false : null
  }
  if (typeof q.start === 'string' && typeof q.end === 'string' && q.start && q.end) {
    const startMs = Date.parse(q.start)
    const endMs = Date.parse(q.end)
    if (!Number.isNaN(startMs) && !Number.isNaN(endMs)) {
      startTime.value = startMs
      endTime.value = endMs
    }
  }
}

onMounted(() => {
  // Ingest URL query params (deep links from cost detail pages) before the
  // first load. Guarded so a plain mount (no query) keeps its single
  // initial reload — applying an empty query would be a no-op but the
  // guard makes the intent explicit and protects against future side
  // effects creeping into applyQueryFilter.
  if (hasRelevantQuery()) {
    applyQueryFilter()
  }
  void reload().catch((err) => message.error(displayMessage(err, t)))
  void loadProviders().catch((err) => message.error(displayMessage(err, t)))
  void loadCallers().catch((err) => message.error(displayMessage(err, t)))
  void loadUserOptions()
})

async function loadProviders() {
  const { list } = await listProviders()
  providers.value = list
}

async function loadCallers() {
  const { list } = await listAPIKeys({ q: '', status: '', page: 1, pageSize: 200 })
  apiKeys.value = list
}


function buildListParams(): RequestLogListParams {
  const params: RequestLogListParams = {
    page: page.value,
    page_size: pageSize.value,
  }
  // request_id / model_name: when sourced from a URL query, preserve the
  // value verbatim (no trim) — analytics-sourced identifiers may carry
  // intentional surrounding whitespace. For typed input, keep the existing
  // trim to protect against stray whitespace producing empty params.
  if (querySourcedRequestId.value) {
    if (filter.request_id) params.request_id = filter.request_id
  } else if (filter.request_id.trim()) {
    params.request_id = filter.request_id.trim()
  }
  if (querySourcedModelName.value) {
    if (filter.model_name) params.model_name = filter.model_name
  } else if (filter.model_name.trim()) {
    params.model_name = filter.model_name.trim()
  }
  if (filter.api_key_id != null) params.api_key_id = filter.api_key_id
  if (filter.user_id != null) params.user_id = filter.user_id
  if (filter.provider_id != null) params.provider_id = filter.provider_id
  if (filter.status) params.status = filter.status
  if (filter.key_prefix.trim()) params.key_prefix = filter.key_prefix.trim()
  if (filter.request_path) params.request_path = filter.request_path
  if (filter.source) params.source = filter.source
  if (filter.is_stream != null) params.is_stream = filter.is_stream
  if (filter.cost_known != null) params.cost_known = filter.cost_known
  // start / end are independent bounds — on mobile the user may set only one.
  if (startTime.value != null) params.start = new Date(startTime.value).toISOString()
  if (endTime.value != null) params.end = new Date(endTime.value).toISOString()
  return params
}

// Monotonic fetch token: a stale list response can't clobber a newer one
// if the user fires a second search before the first resolves. Same guard
// pattern the API-key/models stores use, kept inline because this page
// doesn't have a Pinia store — the request-log list is page-local state.
let fetchId = 0
async function reload() {
  const currentId = ++fetchId
  loading.value = true
  try {
    const res = await listRequestLogs(buildListParams())
    if (currentId !== fetchId) return
    rows.value = res.list
    total.value = res.total
  } catch (err) {
    if (currentId !== fetchId) return
    throw err
  } finally {
    if (currentId === fetchId) loading.value = false
  }
}

// Debounced search for free-text inputs (request_id, model_name). The two
// NSelect filters call onSearch directly on @update:value, so this debounce
// only fires for keystroke-level changes.
function onFilterChange() {
  if (searchTimer) clearTimeout(searchTimer)
  searchTimer = setTimeout(() => {
    void onSearch()
  }, 300)
}

// When the user edits a deep-linked request_id / model_name, the value is no
// longer the verbatim query-sourced one — clear its flag so the debounced
// search resumes the normal trim path (matching plain typed searches).
function onRequestIdInput() {
  querySourcedRequestId.value = false
  onFilterChange()
}
function onModelNameInput() {
  querySourcedModelName.value = false
  onFilterChange()
}

async function onSearch() {
  if (searchTimer) {
    clearTimeout(searchTimer)
    searchTimer = null
  }
  page.value = 1
  try {
    await reload()
  } catch (err) {
    message.error(displayMessage(err, t))
  }
}

function onReset() {
  filter.request_id = ''
  filter.model_name = ''
  filter.key_prefix = ''
  filter.api_key_id = null
  filter.provider_id = null
  filter.status = null
  filter.is_stream = null
  streamSelect.value = null
  filter.cost_known = null
  costSelect.value = null
  filter.request_path = null
  filter.source = null
  startTime.value = null
  endTime.value = null
  // Drop the verbatim-no-trim override too, so post-reset typed searches
  // return to the normal submit-time trim behavior.
  querySourcedModelName.value = false
  querySourcedRequestId.value = false
  page.value = 1
  void reload().catch((err) => message.error(displayMessage(err, t)))
}

async function onExport() {
  exporting.value = true
  try {
    await exportRequestLogsCSV(buildListParams())
    message.success(t('requestLogs.exportSuccess'))
  } catch (err) {
    message.error(displayMessage(err, t) || t('requestLogs.exportFailed'))
  } finally {
    exporting.value = false
  }
}

// onStreamChange decodes the UI value into the boolean-or-null the backend
// expects, then fires a search. null = cleared select = no filter. Wired
// via :value + @update:value rather than v-model so the null → null mapping
// happens in one place.
function onStreamChange(v: 'stream' | 'non-stream' | null) {
  streamSelect.value = v
  filter.is_stream = v === 'stream' ? true : v === 'non-stream' ? false : null
  void onSearch()
}

function onCostKnownChange(v: 'known' | 'unknown' | null) {
  costSelect.value = v
  filter.cost_known = v === 'known' ? true : v === 'unknown' ? false : null
  void onSearch()
}

function onEndpointChange(v: string | null) {
  filter.request_path = v
  void onSearch()
}

function onSourceChange(v: 'caller' | 'vision_fallback' | null) {
  filter.source = v
  void onSearch()
}

// FilterSelectField is a controlled input (no v-model), so each select's
// handler writes the reactive filter field and then searches — mirroring the
// old NSelect `@update:value="onSearch"` after v-model wrote the value.
function onUserChange(v: number | null) {
  filter.user_id = v
  void onSearch()
}

function onCallerChange(v: number | null) {
  filter.api_key_id = v
  void onSearch()
}

function onProviderChange(v: number | null) {
  filter.provider_id = v
  void onSearch()
}

function onStatusChange(v: StatusClass | null) {
  filter.status = v
  void onSearch()
}

const pagination = computed<PaginationProps>(() => ({
  page: page.value,
  pageSize: pageSize.value,
  itemCount: total.value,
  showSizePicker: true,
  pageSizes: [10, 20, 50, 100],
  onChange: (p: number) => {
    page.value = p
    void reload().catch((err) => message.error(displayMessage(err, t)))
  },
  onUpdatePageSize: (ps: number) => {
    pageSize.value = ps
    page.value = 1
    void reload().catch((err) => message.error(displayMessage(err, t)))
  },
}))

function goDetail(requestId: string) {
  router.push(`/request-logs/${encodeURIComponent(requestId)}`)
}

// ---------- Render helpers ----------

function formatTime(iso: string): string {
  // Locale-aware short timestamp for table density; detail page uses a
  // longer format. The toLocaleString options are kept inline rather than
  // extracted to a util because the table + detail page intentionally use
  // different granularities.
  return new Date(iso).toLocaleString(undefined, {
    year: '2-digit',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  })
}

function formatDuration(ms: number): string {
  if (ms < 1000) return `${ms}ms`
  return `${(ms / 1000).toFixed(2)}s`
}

// expandField renders one label/value pair inside the expanded row. Inline
// styles on purpose: scoped CSS does not apply to h()-rendered elements.
function expandField(label: string, value: string) {
  return h('div', { style: 'display:flex; align-items:baseline; gap:5px; min-width:170px;' }, [
    h('span', { style: 'font-size:12px; color:var(--color-text-muted, #909399); white-space:nowrap; flex-shrink:0;' }, `${label}:`),
    h('span', { style: `font-size:13px; ${value ? '' : 'color:#bbb;'} word-break:break-all;` }, value || '-'),
  ])
}

// The expanded row carries identity and routing detail that would crowd the
// main columns: the full request id, the retry breakdown, and cache tokens.
function renderRouteExpand(row: RequestLogRow) {
  return h('div', { style: 'display:flex; flex-wrap:wrap; gap:8px 24px; padding:10px 16px;' }, [
    expandField(t('requestLogs.col_requestId'), row.request_id),
    expandField(t('requestLogs.col_attempts'), String(row.attempts)),
    expandField(t('requestLogs.col_keySwitches'), String(row.key_switches)),
    expandField(t('requestLogs.col_failovers'), String(row.failovers)),
    expandField(t('requestLogs.cacheTokensLabel'), `${row.cache_write_tokens} / ${row.cache_read_tokens}`),
  ])
}

// tokenRow is one labeled line of the vertical token breakdown; lines appear
// only when their count is nonzero, so the common no-cache row stays short.
function tokenRow(label: string, value: number) {
  return h('div', { style: 'display:flex; gap:6px;' }, [
    h('span', { style: 'color:var(--color-text-muted, #909399);' }, label),
    h('span', { style: 'font-variant-numeric: tabular-nums;' }, String(value)),
  ])
}

// The usage cell renders one billing unit per row: a per-image settlement
// reads "count × unit price"; everything else keeps the vertical token
// breakdown. An unpriced image row carries no snapshot (and usually no
// token counts), so it lands on the same '-' as any unmeasured row.
function usageCell(row: RequestLogRow) {
  if (row.image_count > 0 && row.image_unit_price != null) {
    return h(
      'span',
      { style: 'font-variant-numeric: tabular-nums; font-size:12px; white-space:nowrap;' },
      t('requestLogs.usageImageLine', { n: row.image_count, price: formatImagePrice(row.image_unit_price) }),
    )
  }
  const lines = []
  if (row.input_tokens > 0) lines.push(tokenRow(t('requestLogs.tokenRowIn'), row.input_tokens))
  if (row.output_tokens > 0) lines.push(tokenRow(t('requestLogs.tokenRowOut'), row.output_tokens))
  if (row.cache_read_tokens > 0) lines.push(tokenRow(t('requestLogs.tokenRowCacheRead'), row.cache_read_tokens))
  if (row.cache_write_tokens > 0) lines.push(tokenRow(t('requestLogs.tokenRowCacheWrite'), row.cache_write_tokens))
  if (lines.length === 0) return h('span', { style: 'color:#bbb;' }, '-')
  return h('div', { style: 'display:inline-flex; flex-direction:column; align-items:flex-start; font-size:12px; line-height:1.5;' }, lines)
}

// The provider cell is composite: who served (bold), under which provider-side
// model name, with retry badges only when the router actually had to work.
function providerCell(row: RequestLogRow) {
  const children = [
    h('div', { style: 'font-weight:600; font-size:12px;' }, row.provider_name || '-'),
  ]
  if (row.final_provider_model) {
    children.push(h('div', { style: 'font-size:12px; color:var(--color-text-muted, #909399); margin-top:2px;' }, row.final_provider_model))
  }
  const badges = []
  if (row.key_switches > 0) {
    badges.push(h(NTag, { size: 'tiny', round: true, bordered: false, type: 'warning' }, { default: () => t('requestLogs.badgeKeySwitches', { n: row.key_switches }) }))
  }
  if (row.failovers > 0) {
    badges.push(h(NTag, { size: 'tiny', round: true, bordered: false, type: 'warning' }, { default: () => t('requestLogs.badgeFailovers', { n: row.failovers }) }))
  }
  if (badges.length > 0) {
    children.push(h('div', { style: 'display:flex; gap:4px; margin-top:2px;' }, badges))
  }
  return h('div', { style: 'line-height:1.5;' }, children)
}

function costCell(row: RequestLogRow) {
  if (!row.cost_known) {
    return h(NTag, { size: 'small', bordered: false, type: 'default' }, { default: () => t('requestLogs.costUnknown') })
  }
  return h('span', { style: 'font-variant-numeric: tabular-nums;' }, formatMicros(row.cost_micros))
}

// The card layout on mobile treats the FIRST column as the card header and
// has no expand mechanism, so the expand column exists only on desktop; on
// mobile the same detail fields become ordinary labeled card rows instead.
const sharedColumns = computed<DataTableColumns<RequestLogRow>>(() => [
  {
    title: columnTitle(t('requestLogs.col_created'), t('requestLogs.col_created_tip')),
    key: 'created_at',
    width: 150,
    render: (row) => h('span', { style: 'font-variant-numeric: tabular-nums; font-size:12px;' }, formatTime(row.created_at)),
  },
  {
    title: columnTitle(t('requestLogs.col_user'), t('requestLogs.col_user_tip')),
    key: 'username',
    width: 100,
    ellipsis: { tooltip: true },
    render: (row) => row.username || '-',
  },
  {
    title: columnTitle(t('requestLogs.col_model'), t('requestLogs.col_model_tip')),
    key: 'model_name',
    width: 150,
    // No naive ellipsis config here: NEllipsis wraps the whole rendered
    // cell in a nowrap clip, which would swallow the sub-call badge exactly
    // when the model name is long. Truncation is done by hand instead — the
    // badge is shrink-proof and leads, the name clips with its own
    // ellipsis, and the title attribute keeps the full name reachable.
    // Fixed table layout is still engaged by the other ellipsis columns.
    render: (row) => {
      const name = h(
        'span',
        {
          style: 'font-weight:600; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; min-width:0;',
          title: row.model_name,
        },
        row.model_name,
      )
      if (row.source !== 'vision_fallback') return h('div', { style: 'display:flex; min-width:0;' }, [name])
      return h('div', { style: 'display:flex; align-items:center; gap:6px; min-width:0;' }, [
        h(NTag, { size: 'small', round: true, bordered: false, type: 'info', style: 'flex-shrink:0;' }, { default: () => t('requestLogs.sourceBadge') }),
        name,
      ])
    },
  },
  {
    title: columnTitle(t('requestLogs.col_endpoint'), t('requestLogs.col_endpoint_tip')),
    key: 'request_path',
    width: 170,
    ellipsis: { tooltip: true },
    render: (row) => h('span', { style: 'font-size:12px;' }, row.request_path || '-'),
  },
  {
    title: columnTitle(t('requestLogs.col_provider'), t('requestLogs.col_provider_tip')),
    key: 'provider_name',
    width: 180,
    render: (row) => providerCell(row),
  },
  {
    title: columnTitle(t('requestLogs.col_stream'), t('requestLogs.col_stream_tip')),
    key: 'is_stream',
    width: 88,
    align: 'center',
    render: (row) =>
      row.is_stream
        ? h(NTag, { size: 'small', round: true, bordered: false, type: 'info' }, { default: () => t('requestLogs.stream_true') })
        : h(NTag, { size: 'small', round: true, bordered: false, type: 'default' }, { default: () => '-' }),
  },
  {
    title: columnTitle(t('requestLogs.col_status'), t('requestLogs.col_status_tip')),
    key: 'status_class',
    width: 96,
    align: 'center',
    render: (row) => h(StatusClassTag, { status: row.status_class }),
  },
  {
    title: columnTitle(t('requestLogs.col_usage'), t('requestLogs.col_usage_tip')),
    key: 'usage',
    width: 126,
    render: (row) => usageCell(row),
  },
  {
    title: columnTitle(t('requestLogs.col_cost'), t('requestLogs.col_cost_tip')),
    key: 'cost',
    width: 110,
    align: 'right',
    render: (row) => costCell(row),
  },
  {
    title: columnTitle(t('requestLogs.col_duration'), t('requestLogs.col_duration_tip')),
    key: 'duration_ms',
    width: 80,
    align: 'right',
    render: (row) => h('span', { style: 'font-variant-numeric: tabular-nums;' }, formatDuration(row.duration_ms)),
  },
  {
    title: t('common.actions'),
    key: 'actions',
    width: 76,
    align: 'center',
    render: (row) =>
      h(NButton, { size: 'tiny', secondary: true, onClick: () => goDetail(row.request_id) }, { default: () => t('requestLogs.viewDetail') }),
  },
])

const mobileDetailColumns = computed<DataTableColumns<RequestLogRow>>(() => [
  { title: columnTitle(t('requestLogs.col_requestId'), t('requestLogs.col_requestId_tip')), key: 'request_id', render: (row) => row.request_id },
  { title: columnTitle(t('requestLogs.col_attempts'), t('requestLogs.col_attempts_tip')), key: 'attempts', render: (row) => String(row.attempts) },
  { title: columnTitle(t('requestLogs.col_keySwitches'), t('requestLogs.col_keySwitches_tip')), key: 'key_switches', render: (row) => String(row.key_switches) },
  { title: columnTitle(t('requestLogs.col_failovers'), t('requestLogs.col_failovers_tip')), key: 'failovers', render: (row) => String(row.failovers) },
  { title: columnTitle(t('requestLogs.cacheTokensLabel'), t('requestLogs.cacheTokensLabel_tip')), key: 'cache', render: (row) => `${row.cache_write_tokens} / ${row.cache_read_tokens}` },
])

const columns = computed<DataTableColumns<RequestLogRow>>(() =>
  isMobile.value
    ? [...sharedColumns.value, ...mobileDetailColumns.value]
    : [{ type: 'expand', expandable: () => true, renderExpand: renderRouteExpand }, ...sharedColumns.value],
)
</script>

<style scoped>
/* Filter-bar styles (.filter-panel / .filter-grid / .filter-item /
   .filter-actions) are the canonical shared classes in styles/global.less —
   this page is the reference every other list page's filter bar matches. */

/* Mobile-only: the datetimerange picker is split into two stacked datetime
   pickers so each fits the narrow viewport. .filter-item--range already goes
   full-width under the global mobile breakpoint (@mobile-breakpoint). */
.filter-range-split {
  display: flex;
  gap: var(--space-2);
  width: 100%;
}

.filter-range-split :deep(.n-date-picker) {
  width: 100%;
}
</style>
