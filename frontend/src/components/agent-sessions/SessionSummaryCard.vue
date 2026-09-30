<!-- frontend/src/components/agent-sessions/SessionSummaryCard.vue
     Top-of-page aggregate card for the tool-session detail view. Recomputes
     the session's figures from the detail payload's own requests array —
     the same rows the timeline renders — so a deep link / refresh into the
     detail page shows the summary without depending on the list endpoint's
     GROUP BY (whose aggregate lives only on list rows).

     Each figure is computed with the SAME semantics as the list SQL
     (repository/agent_session_query.go's aggregate SELECT):
       - request count        COUNT(*)
       - success count        the success bucket of the five-class status
                              taxonomy (the SQL spells it as
                              2xx-without-fail_reason, which is exactly the
                              predicate that derives status_class='success'
                              on the row — counting rows by their
                              status_class is the same bucket)
       - token sums           SUM(input_tokens) / SUM(output_tokens)
       - cost                 SUM(cost_micros) over cost_known=true rows
                              ONLY, plus a count of the cost_known=false
                              rows shown as a disclosure marker — unknown
                              rows are never folded into the total
       - session duration     MAX(created_at) - MIN(created_at), derived
                              here because the wire payload carries no such
                              column (first/last-seen exist only on list
                              rows) -->
<template>
  <div class="metric-row session-summary">
    <div class="metric one-line">
      <div class="metric__label">
        <HelpLabel :tip="t('agentSessions.summaryRequests_tip')">{{ t('agentSessions.summaryRequests') }}</HelpLabel>
      </div>
      <div class="metric__value">{{ requests.length }}</div>
      <div class="metric__sub">{{ t('agentSessions.successCountLine', { n: successCount }) }}</div>
    </div>

    <div class="metric one-line">
      <div class="metric__label">
        <HelpLabel :tip="t('agentSessions.summaryTokens_tip')">{{ t('agentSessions.summaryTokens') }}</HelpLabel>
      </div>
      <div class="metric__value">{{ formatNumber(inputTokens) }}</div>
      <div class="metric__sub">
        {{ t('requestLogs.tokenRowOut') }} {{ formatNumber(outputTokens) }}
      </div>
    </div>

    <div class="metric one-line">
      <div class="metric__label">
        <HelpLabel :tip="t('agentSessions.summaryCost_tip')">{{ t('agentSessions.summaryCost') }}</HelpLabel>
      </div>
      <div class="metric__value">¥{{ formatMicros(knownCostMicros) }}</div>
      <div v-if="unknownCostCount > 0" class="metric__sub">
        {{ t('agentSessions.costUnknownNote', { n: unknownCostCount }) }}
      </div>
    </div>

    <div class="metric one-line">
      <div class="metric__label">
        <HelpLabel :tip="t('agentSessions.summaryDuration_tip')">{{ t('agentSessions.summaryDuration') }}</HelpLabel>
      </div>
      <div class="metric__value">{{ formatSpan(sessionSpanMs) }}</div>
      <div class="metric__sub">{{ rangeDoublet }}</div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { formatMicros } from '../../utils/money'
import { formatNumber, formatShortClock, formatSpan } from '../../utils/format'
import type { RequestLogRow } from '../../api/requestLogs'
import HelpLabel from '../HelpLabel.vue'

const props = defineProps<{ requests: RequestLogRow[] }>()
const { t } = useI18n()

const successCount = computed(() => props.requests.filter((r) => r.status_class === 'success').length)

const inputTokens = computed(() => props.requests.reduce((sum, r) => sum + r.input_tokens, 0))
const outputTokens = computed(() => props.requests.reduce((sum, r) => sum + r.output_tokens, 0))

// The known/unknown cost doublet: only cost_known=true rows enter the sum
// (a hand-mangled unknown row with a nonzero cost_micros stays out), and
// the unknown count is disclosed rather than folded in.
const knownCostMicros = computed(
  () => props.requests.reduce((sum, r) => (r.cost_known ? sum + r.cost_micros : sum), 0),
)
const unknownCostCount = computed(() => props.requests.filter((r) => !r.cost_known).length)

// Session span = last created_at - first created_at. Timestamps parse to
// epoch ms; NaN (a malformed wire timestamp) is treated as 0 so one bad row
// cannot poison the whole subtraction into NaN.
const createdMs = computed(() => props.requests.map((r) => Date.parse(r.created_at)).map((ms) => (Number.isNaN(ms) ? 0 : ms)))
const sessionSpanMs = computed(() => {
  if (createdMs.value.length === 0) return 0
  return Math.max(...createdMs.value) - Math.min(...createdMs.value)
})

// The first→last doublet under the summary value, same "from/to" labels
// the list rows use so the two views read identically. The wall clock is
// the shared short-clock formatter (the timeline column shows the same
// granularity; the summary is the page's canonical "when was this
// session").
const rangeDoublet = computed(() => {
  if (createdMs.value.length === 0) return ''
  const first = new Date(Math.min(...createdMs.value))
  const last = new Date(Math.max(...createdMs.value))
  return `${t('agentSessions.firstSeenLabel')} ${formatShortClock(first)} — ${t('agentSessions.lastSeenLabel')} ${formatShortClock(last)}`
})
</script>

<style scoped lang="less">
/* The shared .metric-row shell carries NO column template of its own —
   every consumer sets grid-template-columns in its own scoped block (the
   cost pages and analytics do the same). This card renders FOUR metrics;
   four-up on desktop, 2-up on tablet, 1-up on phone so the figures stay
   legible. */
.session-summary {
  margin-bottom: var(--space-4, 16px);
  grid-template-columns: repeat(4, 1fr);
}

@media (max-width: 1100px) {
  .session-summary {
    grid-template-columns: repeat(2, 1fr);
  }
}

@media (max-width: @mobile-breakpoint) {
  .session-summary {
    grid-template-columns: 1fr;
  }
}
</style>
