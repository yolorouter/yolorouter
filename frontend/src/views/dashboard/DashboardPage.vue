<!-- frontend/src/views/dashboard/DashboardPage.vue
     Overview dashboard (PRD §6.6). Renders today's KPI cards, a 7-day trend
     chart, top callers, recent failures, and upstream status — all from one
     GET /api/admin/dashboard round trip.

     All sections share a single loading state because the dashboard envelope
     is fetched atomically. A failure surfaces a single toast and leaves the
     previous data on screen (the trend / cards just go blank on first load). -->
<template>
  <div class="dashboard-page">
    <PageHeader :eyebrow="t('dashboard.eyebrow')" :title="t('dashboard.pageTitle')" :description="t('dashboard.pageDescription')" />

    <EmptyState v-if="!loading && isEmpty" :title="t('dashboard.emptyTitle')" :description="t('dashboard.emptyDesc')" />

    <template v-else>
      <!-- KPI cards row -->
      <div class="kpi-row">
        <div class="kpi">
          <div class="kpi__icon kpi__icon--accent">
            <Activity :size="18" />
          </div>
          <div class="kpi__body">
            <div class="kpi__label">
              <HelpLabel :tip="t('dashboard.callsCard_tip')">{{ t('dashboard.callsCard') }}</HelpLabel>
            </div>
            <div class="kpi__value">{{ formatNumber(data?.today.calls ?? 0) }}</div>
            <div class="kpi__sub">{{ t('dashboard.callsCard_sub') }}</div>
          </div>
        </div>

        <div class="kpi">
          <div class="kpi__icon kpi__icon--success">
            <DollarSign :size="18" />
          </div>
          <div class="kpi__body">
            <div class="kpi__label">
              <HelpLabel :tip="t('dashboard.costCard_tip')">{{ t('dashboard.costCard') }}</HelpLabel>
            </div>
            <div class="kpi__value">¥{{ formatMicros(data?.today.total_cost_micros ?? 0) }}</div>
            <div class="kpi__sub">{{ t('dashboard.costCard_sub') }}</div>
          </div>
        </div>

        <div class="kpi">
          <div class="kpi__icon kpi__icon--purple">
            <TrendingUp :size="18" />
          </div>
          <div class="kpi__body">
            <div class="kpi__label">
              <HelpLabel :tip="t('dashboard.successRateCard_tip')">{{ t('dashboard.successRateCard') }}</HelpLabel>
            </div>
            <div class="kpi__value">{{ formatRate(data?.today.success_rate ?? 0) }}</div>
            <div class="kpi__sub">{{ t('dashboard.successRateCard_sub') }}</div>
          </div>
        </div>

        <div class="kpi">
          <div class="kpi__icon kpi__icon--warning">
            <AlertTriangle :size="18" />
          </div>
          <div class="kpi__body">
            <div class="kpi__label">
              <HelpLabel :tip="t('dashboard.unknownCostCard_tip')">{{ t('dashboard.unknownCostCard') }}</HelpLabel>
            </div>
            <div class="kpi__value">{{ formatNumber(data?.today.unknown_cost_calls ?? 0) }}</div>
            <div class="kpi__sub">{{ t('dashboard.unknownCostCard_sub') }}</div>
          </div>
        </div>
      </div>

      <!-- Trend chart -->
      <section class="section-card">
        <header class="section-head">
          <h2 class="section-title">{{ t('dashboard.trendTitle') }}</h2>
          <span class="section-sub">{{ t('dashboard.trendSub') }}</span>
        </header>
        <TrendChart :points="data?.trend ?? []" />
      </section>

      <!-- Two-column: top callers + recent failures -->
      <div class="two-col">
        <section class="section-card">
          <header class="section-head">
            <h2 class="section-title">{{ t('dashboard.topCallersTitle') }}</h2>
            <span class="section-sub">{{ t('dashboard.topCallersSub') }}</span>
          </header>
          <div v-if="!data?.top_callers?.length" class="section-empty">{{ t('dashboard.topCallersEmpty') }}</div>
          <ul v-else class="caller-list">
            <li v-for="(c, i) in data.top_callers" :key="c.api_key_id" class="caller-row">
              <span class="caller-rank">{{ i + 1 }}</span>
              <span class="caller-label">{{ c.owner_label || t('dashboard.unknownCaller') }}</span>
              <span class="caller-meta">{{ formatNumber(c.calls) }} {{ t('dashboard.callsUnit') }}</span>
              <span class="caller-cost">¥{{ formatMicros(c.cost_micros) }}</span>
            </li>
          </ul>
        </section>

        <section class="section-card">
          <header class="section-head">
            <h2 class="section-title">{{ t('dashboard.recentFailuresTitle') }}</h2>
            <span class="section-sub">{{ t('dashboard.recentFailuresSub') }}</span>
          </header>
          <div v-if="!data?.recent_failures?.length" class="section-empty">{{ t('dashboard.recentFailuresEmpty') }}</div>
          <ul v-else class="failure-list">
            <li
              v-for="f in data.recent_failures"
              :key="f.request_id"
              class="failure-row"
              :title="t('dashboard.viewRequestDetail')"
              @click="goToRequestLog(f.request_id)"
            >
              <div class="failure-main">
                <span class="failure-status" :class="failureStatusClass(f.status_code)">{{ f.status_code }}</span>
                <span class="failure-model">{{ f.model_name || '—' }}</span>
                <span class="failure-reason">{{ f.fail_reason || t('dashboard.noFailReason') }}</span>
              </div>
              <div class="failure-meta">
                <span>{{ formatRelativeTime(f.created_at) }}</span>
                <span>{{ f.duration_ms }}ms</span>
              </div>
            </li>
          </ul>
        </section>
      </div>

      <!-- Upstream status -->
      <section class="section-card">
        <header class="section-head">
          <h2 class="section-title">{{ t('dashboard.upstreamTitle') }}</h2>
        </header>
        <div class="upstream-row">
          <div class="upstream-item">
            <span class="upstream-value upstream-value--success">{{ data?.upstream_status.available_providers ?? 0 }}</span>
            <span class="upstream-label">
              <HelpLabel :tip="t('dashboard.upstreamProviders_tip')">{{ t('dashboard.upstreamProviders') }}</HelpLabel>
            </span>
          </div>
          <div class="upstream-item">
            <span class="upstream-value" :class="{ 'upstream-value--warning': (data?.upstream_status.abnormal_keys ?? 0) > 0 }">
              {{ data?.upstream_status.abnormal_keys ?? 0 }}
            </span>
            <span class="upstream-label">
              <HelpLabel :tip="t('dashboard.upstreamAbnormalKeys_tip')">{{ t('dashboard.upstreamAbnormalKeys') }}</HelpLabel>
            </span>
          </div>
          <div class="upstream-item">
            <span class="upstream-value" :class="{ 'upstream-value--danger': (data?.upstream_status.unavailable_models ?? 0) > 0 }">
              {{ data?.upstream_status.unavailable_models ?? 0 }}
            </span>
            <span class="upstream-label">
              <HelpLabel :tip="t('dashboard.upstreamUnavailableModels_tip')">{{ t('dashboard.upstreamUnavailableModels') }}</HelpLabel>
            </span>
          </div>
        </div>
      </section>
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import { useMessage } from 'naive-ui'
import { Activity, AlertTriangle, DollarSign, TrendingUp } from '@lucide/vue'
import PageHeader from '../../components/PageHeader.vue'
import EmptyState from '../../components/EmptyState.vue'
import HelpLabel from '../../components/HelpLabel.vue'
import TrendChart from '../../components/dashboard/TrendChart.vue'
import { getDashboard, type DashboardData } from '../../api/analytics'
import { displayMessage } from '../../api/client'
import { formatMicros } from '../../utils/money'

const { t } = useI18n()
const router = useRouter()
const message = useMessage()

const data = ref<DashboardData | null>(null)
const loading = ref(true)

// isEmpty distinguishes "first load not done yet" (loading=true) from
// "envelope loaded but every section is empty" (which is the only case the
// empty state should cover — a dashboard with today's calls=0 but a 7-day
// trend still has something to show).
const isEmpty = computed(() => {
  if (!data.value) return true
  const d = data.value
  // Upstream health is meaningful even with zero request traffic — a freshly
  // set-up deployment with abnormal keys or unavailable models must NOT be
  // hidden behind "no data" (Codex adversarial finding). Show the dashboard
  // as long as any provider/model signal exists.
  const hasUpstreamSignal =
    d.upstream_status.available_providers > 0 ||
    d.upstream_status.abnormal_keys > 0 ||
    d.upstream_status.unavailable_models > 0
  return (
    d.today.calls === 0 &&
    d.today.unknown_cost_calls === 0 &&
    d.trend.every((p) => p.calls === 0) &&
    d.top_callers.length === 0 &&
    d.recent_failures.length === 0 &&
    !hasUpstreamSignal
  )
})

async function reload() {
  loading.value = true
  try {
    data.value = await getDashboard()
  } catch (err) {
    message.error(displayMessage(err, t))
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  void reload()
})

function formatNumber(n: number): string {
  // toLocaleString respects the user's locale for grouping separators.
  return n.toLocaleString()
}

function formatRate(r: number): string {
  // r is in [0,1]; show one decimal place for a stable column width.
  return `${(r * 100).toFixed(1)}%`
}

function failureStatusClass(code: number): string {
  if (code >= 500) return 'failure-status--error'
  if (code >= 400) return 'failure-status--warning'
  return 'failure-status--error' // any non-2xx in this list is a failure by definition
}

// formatRelativeTime renders "5m ago" / "2h ago" / "3d ago" — the dashboard
// doesn't need exact timestamps at first glance, only "how stale is this
// failure". On hover the user can click into the detail page for the
// RFC3339 timestamp.
function formatRelativeTime(rfc3339: string): string {
  const ts = Date.parse(rfc3339)
  if (Number.isNaN(ts)) return ''
  const diffMs = Date.now() - ts
  const sec = Math.floor(diffMs / 1000)
  if (sec < 60) return t('dashboard.justNow')
  const min = Math.floor(sec / 60)
  if (min < 60) return t('dashboard.minutesAgo', { n: min })
  const hr = Math.floor(min / 60)
  if (hr < 24) return t('dashboard.hoursAgo', { n: hr })
  const day = Math.floor(hr / 24)
  return t('dashboard.daysAgo', { n: day })
}

function goToRequestLog(requestId: string) {
  // Route is registered by the main router task at the end of M6.1; if the
  // route isn't there yet, vue-router will log a warning and stay put —
  // safe failure mode for a forward reference.
  router.push(`/request-logs/${requestId}`).catch(() => {
    message.error(t('dashboard.requestLogUnavailable'))
  })
}
</script>

<style scoped>
.dashboard-page {
  display: flex;
  flex-direction: column;
  gap: var(--space-6);
}

.kpi-row {
  display: grid;
  grid-template-columns: repeat(4, 1fr);
  gap: var(--space-4);
}

.kpi {
  display: flex;
  align-items: flex-start;
  gap: var(--space-3);
  padding: var(--space-4);
  background: var(--color-surface);
  border: 1px solid var(--color-border-subtle);
  border-radius: var(--radius-lg);
  transition: border-color 150ms;
}

.kpi:hover {
  border-color: var(--color-border);
}

.kpi__icon {
  flex-shrink: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  width: 36px;
  height: 36px;
  border-radius: var(--radius-md);
}

.kpi__icon--accent {
  background: var(--color-accent-subtle);
  color: var(--color-accent);
}

.kpi__icon--success {
  background: var(--color-success-subtle);
  color: var(--color-success);
}

.kpi__icon--purple {
  background: var(--color-purple-subtle);
  color: var(--color-purple);
}

.kpi__icon--warning {
  background: var(--color-warning-subtle);
  color: var(--color-warning);
}

.kpi__label {
  font-size: var(--text-xs);
  font-weight: 700;
  letter-spacing: 0.05em;
  text-transform: uppercase;
  color: var(--color-text-muted);
  margin-bottom: 4px;
}

.kpi__value {
  font-size: 1.625rem;
  font-weight: 800;
  line-height: 1;
  font-variant-numeric: tabular-nums;
  color: var(--color-text);
}

.kpi__sub {
  font-size: var(--text-xs);
  color: var(--color-text-muted);
  margin-top: 4px;
}

.section-card {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  padding: var(--space-5);
  background: var(--color-surface);
  border: 1px solid var(--color-border-subtle);
  border-radius: var(--radius-lg);
}

.section-head {
  display: flex;
  align-items: baseline;
  gap: var(--space-2);
}

.section-title {
  font-size: var(--text-sm);
  font-weight: 700;
  color: var(--color-text);
}

.section-sub {
  font-size: var(--text-xs);
  color: var(--color-text-muted);
}

.section-empty {
  padding: var(--space-6);
  text-align: center;
  color: var(--color-text-muted);
  font-size: var(--text-sm);
}

.two-col {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: var(--space-4);
}

.caller-list,
.failure-list {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
  padding: 0;
  list-style: none;
}

.caller-row {
  display: grid;
  grid-template-columns: 24px 1fr auto auto;
  align-items: center;
  gap: var(--space-3);
  padding: var(--space-2) var(--space-3);
  border-radius: var(--radius-md);
  font-size: var(--text-sm);
}

.caller-row:hover {
  background: var(--color-surface-hover);
}

.caller-rank {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 22px;
  height: 22px;
  background: var(--color-accent-subtle);
  color: var(--color-accent);
  font-size: var(--text-xs);
  font-weight: 700;
  border-radius: 50%;
}

.caller-label {
  font-weight: 600;
  color: var(--color-text);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.caller-meta {
  color: var(--color-text-muted);
  font-variant-numeric: tabular-nums;
}

.caller-cost {
  color: var(--color-text);
  font-weight: 600;
  font-variant-numeric: tabular-nums;
}

.failure-row {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
  padding: var(--space-3);
  border-radius: var(--radius-md);
  cursor: pointer;
  transition: background 120ms;
}

.failure-row:hover {
  background: var(--color-surface-hover);
}

.failure-main {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-size: var(--text-sm);
}

.failure-status {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  min-width: 36px;
  height: 22px;
  padding: 0 6px;
  border-radius: var(--radius-sm);
  font-family: var(--font-mono);
  font-size: var(--text-xs);
  font-weight: 700;
  color: #fff;
}

.failure-status--error {
  background: var(--color-danger);
}

.failure-status--warning {
  background: var(--color-warning);
}

.failure-model {
  flex: 1;
  color: var(--color-text);
  font-weight: 600;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.failure-reason {
  color: var(--color-text-muted);
  font-size: var(--text-xs);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.failure-meta {
  display: flex;
  gap: var(--space-3);
  font-size: var(--text-xs);
  color: var(--color-text-muted);
  font-variant-numeric: tabular-nums;
}

.upstream-row {
  display: grid;
  grid-template-columns: repeat(3, 1fr);
  gap: var(--space-4);
}

.upstream-item {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: var(--space-1);
  padding: var(--space-3);
  background: var(--color-bg-soft);
  border-radius: var(--radius-md);
}

.upstream-value {
  font-size: 1.5rem;
  font-weight: 800;
  color: var(--color-text);
  font-variant-numeric: tabular-nums;
}

.upstream-value--success {
  color: var(--color-success);
}

.upstream-value--warning {
  color: var(--color-warning);
}

.upstream-value--danger {
  color: var(--color-danger);
}

.upstream-label {
  font-size: var(--text-xs);
  color: var(--color-text-muted);
}

@media (max-width: 900px) {
  .kpi-row {
    grid-template-columns: repeat(2, 1fr);
  }

  .two-col {
    grid-template-columns: 1fr;
  }
}

@media (max-width: 640px) {
  .kpi-row {
    grid-template-columns: 1fr;
  }

  .upstream-row {
    grid-template-columns: 1fr;
  }
}
</style>
