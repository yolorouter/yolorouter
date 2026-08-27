<!-- frontend/src/components/providers/KeyTestTargetList.vue
     The per-protocol breakdown panel of a key test: a titled list with one
     line per destination — its name, its own outcome category and, the point
     of the whole panel, the upstream's own words, verbatim in a monospace
     block, since that is what tells a bad key from a protocol the upstream
     simply does not serve.

     The panel owns its shell and title so every surface shows the same
     header; callers fill only what differs — a reveal toggle beside the
     title (header-extra).

     expanded=false keeps the diagnostics behind the caller's toggle; the
     caller only offers that toggle when some row has one to show. -->
<template>
  <div class="key-targets">
    <div class="key-targets__head">
      <span class="key-targets__title">{{ t('providers.keyTestTargetsTitle') }}</span>
      <slot name="header-extra" />
    </div>
    <div v-for="row in rows" :key="row.protocol" class="key-target">
      <div class="key-target__head">
        <span class="key-target__proto">{{ row.protocolLabel }}</span>
        <n-tag size="tiny" :bordered="false" :type="row.passed ? 'success' : 'error'">
          {{ row.outcomeLabel }}
        </n-tag>
        <span class="key-target__duration">{{ row.durationMs }}ms</span>
      </div>
      <!-- A passing destination has nothing to quote, and an empty block would
           read as "the upstream said nothing" on a failure that did say
           something. -->
      <pre v-if="expanded && row.detail" class="key-target__detail upstream-detail">{{ row.detail }}</pre>
    </div>
  </div>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import type { KeyTestTargetRow } from '../../utils/keyTestTargets'

defineProps<{
  rows: KeyTestTargetRow[]
  /** True renders every diagnostic inline; false hides them all. */
  expanded: boolean
}>()

const { t } = useI18n()
</script>
