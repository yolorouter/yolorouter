<!-- frontend/src/components/costs/CostOverviewCards.vue
     Entity-scoped overview cards shared by the three per-entity cost detail
     pages. Shows the four window-scoped figures for the pinned entity:
     spend, calls + success rate, token volume, and net cache savings (the
     last one via the shared CacheSavingsCard). The base .metric-row /
     .metric__* shells come from the shared global stylesheet. -->
<template>
  <div class="metric-row">
    <div class="metric one-line">
      <div class="metric__label">
        <HelpLabel :tip="t('costs.overview.spend_tip')">{{ t('costs.overview.spend') }}</HelpLabel>
      </div>
      <div class="metric__value">¥{{ formatMicros(overview?.cost_micros ?? 0,2) }}</div>
      <div v-if="(overview?.unknown_cost_calls ?? 0) > 0" class="metric__sub">
        {{ t('costs.overview.unknownCostSub', { n: overview?.unknown_cost_calls ?? 0 }) }}
      </div>
    </div>

    <div class="metric">
      <div class="metric__label">
        <HelpLabel :tip="t('analytics.callsColumn_tip')">{{ t('costs.detail.callsCard') }}</HelpLabel>
      </div>
      <div class="metric__value">{{ formatNumber(overview?.total_calls ?? 0) }}</div>
      <div class="metric__sub">
        {{ t('costs.detail.successRateSub', { rate: formatRate(overview?.success_rate ?? 0) }) }}
      </div>
    </div>

    <div class="metric">
      <div class="metric__label">
        <HelpLabel :tip="t('analytics.inputTokensColumn_tip')">{{ t('costs.detail.tokensCard') }}</HelpLabel>
      </div>
      <div class="metric__value">{{ formatNumber(overview?.input_tokens ?? 0) }}</div>
      <div class="metric__sub">
        {{ t('costs.detail.outputTokensSub', { n: formatNumber(overview?.output_tokens ?? 0) }) }}
      </div>
    </div>

    <CacheSavingsCard class="one-line" :overview="overview" />
  </div>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import HelpLabel from '../HelpLabel.vue'
import CacheSavingsCard from '../common/CacheSavingsCard.vue'
import { formatMicros } from '../../utils/money'
import { formatNumber, formatRate } from '../../utils/format'
import type { OverviewRow } from '../../api/analytics'

// Single defineProps — the overview payload comes from the parent page's
// analytics fetch (entity-scoped, e.g. filtered by model_name / provider_id).
defineProps<{ overview: OverviewRow | null }>()

const { t } = useI18n()
</script>

<style scoped lang="less">
/* Four-up grid override on the shared .metric-row shell. Drops to 2-up on
   tablet and 1-up on phone, matching the page-level cost stats layout. */
.metric-row {
  grid-template-columns: repeat(4, 1fr);
}

@media (max-width: 1100px) {
  .metric-row {
    grid-template-columns: repeat(2, 1fr);
  }
}

@media (max-width: @mobile-breakpoint) {
  /* .metric-row .metric.one-line{
     grid-column: 1 / -1;
  } */
}
</style>
