<!-- frontend/src/views/costs/CostOptimizationPage.vue
     Data-first cost-optimization dashboard. Leads with savings metrics and
     breakdowns (by API key / model / provider / day), with the two global
     optimization switches (custom system prompt + input compression) behind
     a settings modal reachable from the page header and CTA banner.
     The CTA banner surfaces how many optimizations remain off; when both
     are on it shows a quiet all-clear.

     Uses the same reloadSeq stale-guard as AnalyticsPage so rapid filter
     changes never leave stale financial data on screen. The settings-enabled
     GETs run in parallel on mount and gate the CTA banner so it never flashes
     a false "off" state before the real values arrive.

     Savings render as two group cards, one per optimization switch: the
     measured input-compression roll-up on the left, and the projected
     concise-output unit rate (per 1M output tokens) on the right. -->
<template>
  <div class="common-page">
    <PageHeader
      class="new-line"
      :eyebrow="t('costOptimization.eyebrow')"
      :title="t('costOptimization.title')"
      :description="t('costOptimization.pageDescription')"
    >
      <template #actions>
        <UserFilterSelect v-if="authStore.isAdmin" :value="selectedUserId" :options="userOptions" @update:value="onUserChange" />
        <TimeRangeSelect v-model="timeRange" :preset="preset" @update:preset="onPresetChange" />
        <NButton @click="settingsShow = true">
          <template #icon><Settings :size="16" /></template>
          {{ t('costOptimization.settingsAction') }}
        </NButton>
      </template>
    </PageHeader>

    <!-- CTA banner: surfaces how many optimizations remain off. -->
    <div v-if="settingsLoaded" class="cta-banner section-card">
      <div v-if="allOn" class="cta-banner__quiet">
        <CheckCircle2 :size="20" class="cta-banner__icon cta-banner__icon--ok" />
        <span>{{ t('costOptimization.ctaAllOn') }}</span>
      </div>
      <div v-else class="cta-banner__main">
        <div class="cta-banner__lead">
          <Sparkles :size="20" class="cta-banner__icon" />
          <span>{{ ctaLead }}</span>
        </div>
        <div class="cta-banner__pills">
          <span class="status-pill" :class="concisePill.cls">
            {{ t('costOptimization.cspSubTitle') }} · {{ concisePill.label }}
          </span>
          <span class="status-pill" :class="compressPill.cls">
            {{ t('costOptimization.inputCompression.title') }} · {{ compressPill.label }}
          </span>
        </div>
        <NButton type="primary" size="small" @click="settingsShow = true">
          {{ t('costOptimization.ctaAction') }}
        </NButton>
      </div>
    </div>
    <!-- Two savings groups, one per optimization switch: the measured
         compression roll-up on the left, the projected concise-output unit
         rate on the right. The banner above stays a pure switch CTA. -->
    <div class="savings-groups">
      <div class="section-card">
        <div class="section-card__head group-head">
          <HelpLabel :tip="t('costOptimization.inputCompression.titleTip')">
            {{ t('costOptimization.groupCompress') }}
          </HelpLabel>
          <span class="status-pill" :class="compressPill.cls">{{ compressPill.label }}</span>
        </div>
        <div class="group-metrics">
          <div class="metric-cell">
            <div class="metric__label">
              <HelpLabel :tip="t('costOptimization.metricTokensSaved_tip')">{{ t('costOptimization.metricTokensSaved') }}</HelpLabel>
            </div>
            <div class="metric__value">{{ formatNumber(totals.tokens_saved) }}</div>
          </div>
          <div class="metric-cell">
            <div class="metric__label">
              <HelpLabel :tip="t('costOptimization.metricCostSaved_tip')">{{ t('costOptimization.metricCostSaved') }}</HelpLabel>
            </div>
            <div class="metric__value">¥{{ formatMicros(totals.cost_saved_micros, 2) }}</div>
          </div>
          <div class="metric-cell">
            <div class="metric__label">
              <HelpLabel :tip="t('costOptimization.metricCompressRate_tip')">{{ t('costOptimization.metricCompressRate') }}</HelpLabel>
            </div>
            <div class="metric__value">{{ formatRate(compressRate) }}</div>
          </div>
          <div class="metric-cell">
            <div class="metric__label">
              <HelpLabel :tip="t('costOptimization.metricCompressedCalls_tip')">{{ t('costOptimization.metricCompressedCalls') }}</HelpLabel>
            </div>
            <div class="metric__value">{{ formatNumber(totals.compressed_calls) }}</div>
          </div>
        </div>
      </div>

      <div class="section-card">
        <div class="section-card__head group-head">
          <HelpLabel :tip="t('costOptimization.groupConcise_tip')">{{ t('costOptimization.groupConcise') }}</HelpLabel>
          <span class="status-pill" :class="concisePill.cls">{{ concisePill.label }}</span>
        </div>
        <div class="group-metrics">
          <div class="metric-cell">
            <div class="metric__label">
              <HelpLabel :tip="t('costOptimization.concisePerMillion_tip')">{{ t('costOptimization.concisePerMillion') }}</HelpLabel>
            </div>
            <div class="metric__value">{{ projectionValue }}</div>
          </div>
          <div class="metric-cell">
            <div class="metric__label">
              <HelpLabel :tip="t('costOptimization.conciseCoefficient_tip')">{{ t('costOptimization.conciseCoefficient') }}</HelpLabel>
            </div>
            <div class="metric__value">{{ projection ? formatRate(projection.coefficient) : '—' }}</div>
          </div>
          <div class="metric-cell">
            <div class="metric__label">
              <HelpLabel :tip="t('costOptimization.concisePricedTokens_tip')">{{ t('costOptimization.concisePricedTokens') }}</HelpLabel>
            </div>
            <div class="metric__value">{{ projection ? formatNumber(projection.priced_output_tokens) : '—' }}</div>
          </div>
          <div class="metric-cell">
            <div class="metric__label">
              <HelpLabel :tip="t('costOptimization.conciseCoverage_tip')">{{ t('costOptimization.conciseCoverage') }}</HelpLabel>
            </div>
            <div class="metric__value">{{ coverageRate }}</div>
          </div>
        </div>
        <!-- Footnote always carries the benchmark link (language-matched
             doc URL from i18n); the basis note joins it when a figure
             exists. -->
        <div class="group-footnote">
          <span v-if="projectionNote">{{ projectionNote }} · </span>
          <a :href="benchmarkDocUrl(locale)" target="_blank" rel="noopener" class="group-footnote__link">
            {{ t('costOptimization.projectionBenchmarkLink') }}
            <ExternalLink :size="12" />
          </a>
        </div>
      </div>
    </div>

    <!-- Dimension cards: four line charts (daily / API key / model / provider)
         in a 2x2 grid. All four breakdowns come from one stats call, so every
         card renders from the same stats ref without per-card reload. -->
    <div class="chart-grid">
      <div class="section-card">
        <div class="section-card__head">
          <HelpLabel :tip="t('costOptimization.dimDaily_tip')">{{ t('costOptimization.dimDaily') }}</HelpLabel>
        </div>
        <CompressLineChart
          :labels="dailyLabels"
          :values="dailyTokensSaved"
          :format-value="formatNumber"
          show-average
          :empty-text="t('costOptimization.noData')"
        />
      </div>
      <div class="section-card">
        <div class="section-card__head">
          <HelpLabel :tip="t('costOptimization.dimApiKey_tip')">{{ t('costOptimization.dimApiKey') }}</HelpLabel>
        </div>
        <CompressLineChart
          :labels="apiKeyLabels"
          :values="apiKeyTokensSaved"
          :format-value="formatNumber"
          :empty-text="t('costOptimization.noData')"
        />
      </div>
      <div class="section-card">
        <div class="section-card__head">
          <HelpLabel :tip="t('costOptimization.dimModel_tip')">{{ t('costOptimization.dimModel') }}</HelpLabel>
        </div>
        <CompressLineChart
          :labels="modelLabels"
          :values="modelTokensSaved"
          :format-value="formatNumber"
          :empty-text="t('costOptimization.noData')"
        />
      </div>
      <div class="section-card">
        <div class="section-card__head">
          <HelpLabel :tip="t('costOptimization.dimProvider_tip')">{{ t('costOptimization.dimProvider') }}</HelpLabel>
        </div>
        <CompressLineChart
          :labels="providerLabels"
          :values="providerTokensSaved"
          :format-value="formatNumber"
          :empty-text="t('costOptimization.noData')"
        />
      </div>
    </div>
    <!-- Breakdown: compressor hits + skip reasons side by side. -->
    <div class="breakdown-row">
      <div class="section-card table-card">
        <div class="section-card__head">
          <HelpLabel :tip="t('costOptimization.compressorsTitle_tip')">{{ t('costOptimization.compressorsTitle') }}</HelpLabel>
        </div>
        <NDataTable
          :columns="compressorColumns"
          :data="compressorRows"
          :loading="loading"
          :bordered="false"
          :single-line="false"
          :row-key="(r: CompressorHitRow) => r.name"
          size="small"
        >
          <template #empty>
            <EmptyState :icon="BarChart3" :title="t('costOptimization.noData')" />
          </template>
        </NDataTable>
      </div>
      <div class="section-card table-card">
        <div class="section-card__head">
          <HelpLabel :tip="t('costOptimization.skipReasonTitle_tip')">{{ t('costOptimization.skipReasonTitle') }}</HelpLabel>
        </div>
        <NDataTable
          :columns="skipReasonColumns"
          :data="skipReasonRows"
          :loading="loading"
          :bordered="false"
          :single-line="false"
          :row-key="(r: CompressSkipReasonRow) => r.skip_reason || '__ok__'"
          size="small"
        >
          <template #empty>
            <EmptyState :icon="BarChart3" :title="t('costOptimization.noData')" />
          </template>
        </NDataTable>
      </div>
    </div>

    <OptimizationSettingsModal v-model:show="settingsShow" @saved="onSettingsSaved" />
  </div>
</template>

<script setup lang="ts">
import { computed, h, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { NButton, NDataTable, useMessage, type DataTableColumns } from 'naive-ui'
import { BarChart3, CheckCircle2, ExternalLink, Settings, Sparkles } from '@lucide/vue'
import PageHeader from '../../components/PageHeader.vue'
import EmptyState from '../../components/EmptyState.vue'
import HelpLabel from '../../components/HelpLabel.vue'
import TimeRangeSelect, { type RangePreset, type TimeRange } from '../../components/analytics/TimeRangeSelect.vue'
import { initialLast7DaysRange } from '../../utils/timeRange'
import { formatMicros } from '../../utils/money'
import { callerDisplay, formatNumber, formatRate } from '../../utils/format'
import { displayMessage } from '../../api/client'
import OptimizationSettingsModal from '../../components/costs/OptimizationSettingsModal.vue'
import CompressLineChart from '../../components/costs/CompressLineChart.vue'
import { SKIP_REASON_KEYS } from '../../utils/compressSkipReason'
import { getCustomSystemPrompt, getInputCompression } from '../../api/systemSettings'
import { benchmarkDocUrl, coverageRatio, projectionDisplay } from '../../utils/conciseProjection'
import { useAuthStore } from '../../store/auth'
import { useUserFilter } from '../../composables/useUserFilter'
import UserFilterSelect from '../../components/common/UserFilterSelect.vue'
import {
  getCompressStats,
  getConciseOutputProjection,
  type AnalyticsFilter,
  type CompressSkipReasonRow,
  type CompressStatsResult,
  type CompressorHitRow,
  type ConciseOutputProjection,
} from '../../api/analytics'

const { t, locale } = useI18n()
const message = useMessage()
const authStore = useAuthStore()

// Per-account scope for every savings figure on the page (seeded from
// ?user_id=; see useUserFilter). The route is admin-only already; the
// admin gate on the control matches the convention every other stats
// page uses, and stays correct if the route ever opens up.
const { selectedUserId, userOptions, loadUserOptions, onUserChange } = useUserFilter(() => void reload())

// === Settings modal visibility ============================================
const settingsShow = ref(false)

// === Settings-enabled state ===============================================
// Each switch is a THREE-state value — on / off / unknown — and every reader
// on this page (CTA banner and both savings card headers) uses the same one.
// Unknown is not folded into "off": a pill next to a savings figure, and a
// banner counting how many optimizations to go turn on, are both claims about
// the instance, and "we could not read it" is not the same claim as "it is
// disabled". settingsLoaded gates the banner so it never renders any of the
// three before the first read settles.
type SwitchState = 'on' | 'off' | 'unknown'
const cspState = ref<SwitchState>('unknown')
const icState = ref<SwitchState>('unknown')
const settingsLoaded = ref(false)

// settingsSeq is a latest-wins token like reload()'s. A save triggers a
// refresh while the mount's read may still be in flight; without it the older
// response could land last and show pre-save state as current.
let settingsSeq = 0

async function loadSettingsEnabled() {
  const mySeq = ++settingsSeq
  // Back to unknown before re-reading. A save is the usual trigger, and the
  // value on screen is the one it just replaced — holding it there while the
  // re-read is in flight would present a stale answer as the current one.
  // settingsSeq stops an older response from landing; it cannot stop an older
  // VALUE from staying on screen.
  cspState.value = 'unknown'
  icState.value = 'unknown'
  // Each GET commits on its own — one hanging must not hold back the other's
  // answer — and a failed read of the LATEST request resets that switch to
  // unknown rather than leaving a stale value on screen as if it were fresh.
  const commit = <T,>(p: Promise<T>, read: (v: T) => boolean, state: typeof cspState) =>
    p.then(
      (v) => {
        if (mySeq === settingsSeq) state.value = read(v) ? 'on' : 'off'
      },
      () => {
        if (mySeq === settingsSeq) state.value = 'unknown'
      },
    ).finally(() => {
      // Revealed by whichever request settles FIRST, not by both: gating on
      // the pair would hide the whole banner behind one slow endpoint until
      // it timed out, even though the other switch already has an answer.
      // The one still in flight renders as unknown, which is what it is.
      if (mySeq === settingsSeq) settingsLoaded.value = true
    })
  await Promise.all([
    commit(getCustomSystemPrompt(), (v) => v.enabled, cspState),
    commit(getInputCompression(), (v) => v.enabled, icState),
  ])
}

// Pill rendering for one switch, shared by the banner and both card headers
// so they can never disagree about what is known.
function switchPill(state: SwitchState) {
  if (state === 'unknown') {
    return { label: t('costOptimization.statusUnknown'), cls: 'status-pill--unknown' }
  }
  return state === 'on'
    ? { label: t('costOptimization.statusOn'), cls: 'status-pill--on' }
    : { label: t('costOptimization.statusOff'), cls: 'status-pill--off' }
}
const compressPill = computed(() => switchPill(icState.value))
const concisePill = computed(() => switchPill(cspState.value))

const allOn = computed(() => cspState.value === 'on' && icState.value === 'on')
// Only switches read as off are counted — an unread one is not something the
// user can be told to go enable.
const offCount = computed(
  () => Number(cspState.value === 'off') + Number(icState.value === 'off'),
)
// The banner's headline. It only reaches the unknown wording when nothing is
// confirmed off yet something is unconfirmed — at that point the only honest
// thing left to say is that the state could not be read.
const ctaLead = computed(() =>
  offCount.value > 0
    ? t('costOptimization.ctaNotAllOn', { n: offCount.value })
    : t('costOptimization.ctaUnknown'),
)

// === Time range state =====================================================
// Default window = last 7 days, shared with the other dashboards via
// utils/timeRange.ts so every page opens on the same window.
const preset = ref<RangePreset>('last7d')
const timeRange = ref<TimeRange>(initialLast7DaysRange())

// === Stats state ==========================================================
const stats = ref<CompressStatsResult | null>(null)
// Concise-output projection for the right-hand savings card — same filter,
// same stale-guard, fetched alongside the stats in one reload so the two
// cards can never disagree on the window they cover.
const projection = ref<ConciseOutputProjection | null>(null)
const loading = ref(false)

// Totals accessor: returns zeros when stats hasn't loaded yet so the metric
// tiles render "0" rather than nothing during the initial fetch.
const totals = computed(() => ({
  tokens_saved: stats.value?.totals.tokens_saved ?? 0,
  cost_saved_micros: stats.value?.totals.cost_saved_micros ?? 0,
  compressed_calls: stats.value?.totals.compressed_calls ?? 0,
  total_estimated_tokens: stats.value?.totals.total_estimated_tokens ?? 0,
}))

// compressRate = tokens_saved / (sent + saved) over the filtered window.
// total_estimated_tokens sums the COMPRESSED (post-compression) input volume
// the upstream actually saw; tokens_saved is the char-delta estimate of what
// compression removed. The denominator must therefore be the estimated
// pre-compression volume = sent + saved, otherwise a high-ratio request
// (saved > sent) pushes the rate past 100%. Clamped to [0, 1] defensively.
const compressRate = computed(() => {
  const sent = totals.value.total_estimated_tokens
  const saved = totals.value.tokens_saved
  const denom = sent + saved
  if (!denom) return 0
  const rate = saved / denom
  if (rate < 0) return 0
  if (rate > 1) return 1
  return rate
})

// === Concise-output projection card =======================================
// Display decisions live in utils/conciseProjection (missing / empty /
// all-unpriced / amount) so the state machine is unit-tested without
// mounting the page. The figure is a per-1M-output-token unit rate
// (traffic-weighted output price x coefficient), so it stays meaningful on
// a lightly-used instance instead of reading as cents per month. No
// traffic / all-unpriced windows render an em-dash plus an explanatory
// note rather than a ¥0.00 figure.
const projectionState = computed(() => projectionDisplay(projection.value))

const projectionValue = computed(() => {
  const d = projectionState.value
  return d.kind === 'amount' ? `¥${formatMicros(d.micros, 2)}` : '—'
})

// coverageRate = priced requests / requests with output traffic in the
// window; '—' until the projection lands or when there is nothing to cover.
const coverageRate = computed(() => {
  const ratio = coverageRatio(projection.value)
  return ratio === null ? '—' : formatRate(ratio)
})

// Card footnote: the pricing basis once a figure exists, the empty /
// all-unpriced explanation when it does not.
const projectionNote = computed(() => {
  switch (projectionState.value.kind) {
    case 'missing':
      return ''
    case 'empty':
      return t('costOptimization.projectionEmpty')
    case 'unpriced-all':
      return t('costOptimization.projectionUnpricedAll')
    default: {
      let note = t('costOptimization.projectionFootnote')
      const p = projection.value
      if (p && p.priced_rows < p.output_rows) note += ` · ${t('costOptimization.projectionUnpriced')}`
      return note
    }
  }
})

// Dimension card data. Each card consumes one breakdown array from the
// single stats call: labels are the x categories (date / owner / model /
// provider name), values are the tokens_saved series plotted on the y axis.
// The daily series is newest-first from the backend; sort ascending by bucket
// so the line reads left-to-right (older -> newer), matching CostTrendChart.
const dailyRowsAsc = computed(() => {
  const rows = stats.value?.daily_series ?? []
  return [...rows].sort((a, b) => (a.bucket < b.bucket ? -1 : a.bucket > b.bucket ? 1 : 0))
})
// "YYYY-MM-DD" -> "MM-DD" so a 30-bucket axis stays legible.
const dailyLabels = computed(() => dailyRowsAsc.value.map((r) => r.bucket.slice(5)))
const dailyTokensSaved = computed(() => dailyRowsAsc.value.map((r) => r.tokens_saved))

const apiKeyLabels = computed(() => (stats.value?.top_api_keys ?? []).map((r) => callerDisplay(r.username, r.key_prefix)))
const apiKeyTokensSaved = computed(() => (stats.value?.top_api_keys ?? []).map((r) => r.tokens_saved))

const modelLabels = computed(() => (stats.value?.top_models ?? []).map((r) => r.model_name))
const modelTokensSaved = computed(() => (stats.value?.top_models ?? []).map((r) => r.tokens_saved))

const providerLabels = computed(() => (stats.value?.top_providers ?? []).map((r) => r.provider_name))
const providerTokensSaved = computed(() => (stats.value?.top_providers ?? []).map((r) => r.tokens_saved))

const compressorRows = computed(() => stats.value?.compressor_hits ?? [])
const skipReasonRows = computed(() => stats.value?.skip_reason_breakdown ?? [])

// === Reload (stale-guarded, mirrors AnalyticsPage) ========================
// reloadSeq is a monotonic token: each reload captures its own seq and bails
// (without writing state) if a newer reload has started. Without this guard,
// a rapid filter change could let the older response land last and overwrite
// the newer stats with stale data.
let reloadSeq = 0

async function reload() {
  const mySeq = ++reloadSeq
  loading.value = true
  // Clear immediately so a failed reload under new filters can't leave stale
  // financial data on screen. The user sees a brief loading state rather
  // than the previous filter's numbers; on error the results stay cleared.
  stats.value = null
  projection.value = null
  const filter: AnalyticsFilter = { start: timeRange.value.start, end: timeRange.value.end, user_id: selectedUserId.value }
  // The two requests commit independently rather than as a pair. Awaiting
  // both together would let the optional projection hold the dashboard back:
  // a rejection would discard an already-successful compress-stats response,
  // and a slow one would keep the charts and tables in their loading state
  // until it timed out. The projection's absence is already a rendered state
  // (em-dashes), so nothing waits on it.
  //
  // Every commit re-checks mySeq: without it a slower earlier reload could
  // still land after a newer one and overwrite it with stale data.
  let notified = false
  const onFailure = (err: unknown) => {
    // One toast per reload even if both halves fail — two stacked errors for
    // one filter change is noise, and the first is the one to act on.
    if (mySeq !== reloadSeq || notified) return
    notified = true
    message.error(displayMessage(err, t))
  }
  const statsDone = getCompressStats(filter)
    .then((result) => {
      if (mySeq === reloadSeq) stats.value = result
    }, onFailure)
    .finally(() => {
      // loading gates the compress-stats tables only, so it clears with them.
      if (mySeq === reloadSeq) loading.value = false
    })
  const projectionDone = getConciseOutputProjection(filter).then((result) => {
    if (mySeq === reloadSeq) projection.value = result
  }, onFailure)
  // Both rejections are already handled above, so this never rejects; it
  // exists so awaiting reload() means the whole page has settled.
  await Promise.all([statsDone, projectionDone])
}

// Settings-enabled GETs run on mount to drive the CTA banner. Stats are
// deliberately NOT loaded here: TimeRangeSelect resolves and emits its range
// synchronously on mount, which sets timeRange and fires the watch below —
// loading stats from onMounted too would double every request on first paint.
// loadSettingsEnabled handles its own errors, so no .catch is needed.
onMounted(() => {
  void loadSettingsEnabled()
  if (authStore.isAdmin) void loadUserOptions()
})

// Stats reload whenever the time range changes — including the initial range
// TimeRangeSelect emits on mount, which is what performs the first load.
// reload handles its own errors, so no .catch is needed.
watch(
  timeRange,
  () => {
    void reload()
  },
  { deep: true },
)

function onPresetChange(v: RangePreset) {
  preset.value = v
}

// After a settings save, re-read the enabled flags (for the CTA banner)
// and reload stats (the toggle may affect new data going forward).
function onSettingsSaved() {
  void loadSettingsEnabled()
  void reload()
}

// === Column definitions ===================================================
//
// Two breakdown tables (compressor hits + skip reasons). The four dimension
// breakdowns now render as line charts (see chart-grid above), so their
// column definitions have been removed.

// --- Compressor hits breakdown ---
const compressorColumns = computed<DataTableColumns<CompressorHitRow>>(() => [
  {
    title: () => t('costOptimization.colCompressor'),
    key: 'name',
    minWidth: 200,
    render: (r) => h('div', { class: 'mono-cell' }, r.name),
  },
  {
    title: () => h(HelpLabel, { tip: t('costOptimization.colHits_tip') }, { default: () => t('costOptimization.colHits') }),
    key: 'hits',
    align: 'right',
    render: (r) => formatNumber(r.hits),
  },
])

// --- Skip reasons breakdown ---
// skip_reason = '' means compression actually ran (OK bucket); any other
// value is one of the short stable skip codes the compress stage emits when
// it bypasses a request. The codes mirror internal/compress/result.go's
// SkipReason enum; render them via i18n so users see a localized label
// instead of the raw code. Unknown codes fall back to a generic "other"
// label with the raw code in parentheses for debuggability.

function formatSkipReason(code: string): string {
  if (code === '') return t('costOptimization.skipReasonOk')
  const key = SKIP_REASON_KEYS[code]
  if (key) return t(key)
  return t('costOptimization.skipReasonUnknown', { code })
}

const skipReasonColumns = computed<DataTableColumns<CompressSkipReasonRow>>(() => [
  {
    title: () => t('costOptimization.colSkipReason'),
    key: 'skip_reason',
    minWidth: 200,
    render: (r) => formatSkipReason(r.skip_reason),
  },
  {
    title: () => t('costOptimization.colCalls'),
    key: 'calls',
    align: 'right',
    render: (r) => formatNumber(r.calls),
  },
])
</script>

<style scoped lang="less">
/* CTA banner — a flat card sitting right under the header. */
.cta-banner {
  display: flex;
  align-items: center;
  padding: var(--space-4) var(--space-5);
}

/* Two savings groups (measured compression / projected concise output):
   one section-card per optimization switch, each holding a 2x2 metric
   grid that reuses the global .metric__label / .metric__value atoms. */
.savings-groups {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: var(--space-6);
}

.group-head {
  justify-content: space-between;
}

.group-metrics {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: var(--space-4) var(--space-5);
}

.group-footnote {
  margin-top: var(--space-3);
  font-size: var(--text-xs);
  color: var(--color-text-secondary);
}

.group-footnote__link {
  display: inline-flex;
  align-items: center;
  gap: 3px;
  color: var(--color-accent);
}

.cta-banner__quiet {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  color: var(--color-success, #18a058);
  font-weight: 600;
}

.cta-banner__main {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: var(--space-3) var(--space-4);
  width: 100%;
}

.cta-banner__lead {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-weight: 600;
  color: var(--color-text);
}

.cta-banner__icon {
  flex-shrink: 0;
  color: var(--color-text-secondary);
}

.cta-banner__icon--ok {
  color: var(--color-success, #18a058);
}

.cta-banner__pills {
  display: flex;
  flex-wrap: wrap;
  gap: var(--space-2);
}

.status-pill {
  display: inline-flex;
  align-items: center;
  padding: 2px 10px;
  border-radius: 999px;
  font-size: var(--text-xs);
  font-weight: 600;
  white-space: nowrap;
}

.status-pill--on {
  background: rgba(24, 160, 88, 0.12);
  color: var(--color-success, #18a058);
}

.status-pill--off {
  background: rgba(208, 48, 80, 0.10);
  color: var(--color-danger, #d03050);
}

/* Neutral, for a switch whose state could not be read — never styled as
   "off", which would read as a fact about the instance. */
.status-pill--unknown {
  background: var(--color-fill-2, rgba(0, 0, 0, 0.05));
  color: var(--color-text-secondary);
}

/* Section heading inside a section-card (for the breakdown tables + chart cards). */
.section-card__head {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  margin-bottom: var(--space-4);
  font-weight: 700;
  color: var(--color-text);
}

/* Chart grid: 2x2 layout for the four dimension line charts. Collapses to a
   single column under 1100px, matching CostStatsPage's .split-row rule. */
.chart-grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: var(--space-6);
}

/* Breakdown row: two tables side by side, collapses on narrow screens. */
.breakdown-row {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: var(--space-6);
}

:deep(.mono-cell) {
  font-family: var(--font-mono, monospace);
  font-weight: 600;
  color: var(--color-text);
}

@media (max-width: 1100px) {
  .savings-groups {
    grid-template-columns: 1fr;
  }
  .chart-grid {
    grid-template-columns: 1fr;
  }
  .breakdown-row {
    grid-template-columns: 1fr;
  }
}

@media (max-width: @mobile-breakpoint) {
  .section-card,
  .cta-banner {
    padding: var(--space-3);
  }
  .section-card {
    padding: var(--space-3);
  }
  .section-card.table-card {
    padding: 0;
  }
  .section-card.table-card .section-card__head{
    padding: var(--space-3);
    margin-bottom: 0;
  }
}
</style>
