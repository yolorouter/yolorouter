<!-- frontend/src/components/common/CacheSavingsCard.vue
     The net-cache-saving metric card, shared by the analytics overview row
     and the cost pages' overview card rows so the two surfaces cannot drift.
     Value is the signed net saving (sign before the currency mark, red when
     negative); the sub-line lists the hit rate, the read saving and the
     write premium. Every figure shares one availability gate: until the
     overview has loaded AND its window recorded cache tokens, the card shows
     em-dashes — an unavailable or unmetered window must never read as a
     verified ¥0.00 / 0%. The base .metric / .metric__* shells come from the
     shared global stylesheet; only the card-specific variants live here. -->
<template>
  <div class="metric">
    <div class="metric__label">
      <HelpLabel :tip="t('costs.overview.cacheSaved_tip')">{{ t('costs.overview.cacheSaved') }}</HelpLabel>
    </div>
    <div class="metric__value" :class="{ 'metric__value--negative': metered && isNegativeMicros(netCacheSaved, 2) }">
      {{ metered ? formatSignedYuan(netCacheSaved, 2) : '—' }}
    </div>
    <div class="metric__sub metric__sub--split">
      <span class="metric__chip">
        {{ t('analytics.cacheHitRateColumn') }} {{ cacheHitRate == null ? '—' : formatRate(cacheHitRate) }}
      </span>
      <span class="metric__chip" :class="{ 'metric__chip--up': metered }">
        {{ t('costs.overview.cacheReadSaved') }} {{ metered ? `¥${formatMicros(overview?.cache_read_saved_micros ?? 0, 2)}` : '—' }}
      </span>
      <span class="metric__chip" :class="{ 'metric__chip--down': metered }">
        {{ t('costs.overview.cacheWriteExtra') }} {{ metered ? `¥${formatMicros(overview?.cache_write_extra_micros ?? 0, 2)}` : '—' }}
      </span>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import HelpLabel from '../HelpLabel.vue'
import { hasCacheMetering, overviewCacheHitRate } from '../../utils/cacheEcon'
import { formatMicros, formatSignedYuan, isNegativeMicros, netCacheSavedMicros } from '../../utils/money'
import { formatRate } from '../../utils/format'
import type { OverviewRow } from '../../api/analytics'

const props = defineProps<{ overview: OverviewRow | null }>()

const { t } = useI18n()

// The card-wide availability gate: overview loaded and cache metering seen.
const metered = computed(() => {
  const o = props.overview
  return o != null && hasCacheMetering(o.cache_read_tokens, o.cache_write_tokens)
})

const netCacheSaved = computed(() => netCacheSavedMicros(props.overview))
const cacheHitRate = computed(() => overviewCacheHitRate(props.overview))
</script>

<style scoped lang="less">
/* Negative-value highlight when cache writes outweigh reads. */
.metric__value--negative {
  color: var(--color-danger, #d03050);
}

/* Split sub-line: hit rate, read saving and write premium side by side. */
.metric__sub--split {
  display: flex;
  flex-wrap: wrap;
  gap: 4px 10px;
}
.metric__chip--up {
  color: var(--color-success, #18a058);
}
.metric__chip--down {
  color: var(--color-text-secondary);
}
</style>
