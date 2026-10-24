<!-- frontend/src/components/models/CandidateUnpricedTag.vue
     The unpriced badge for token/audio mappings whose prices were never
     declared — the SINGLE implementation, shared by the mapping tables
     (CandidateTable's provider-model-name column) and the model pricing
     page's rows. Renders nothing for a candidate the predicate calls
     priced: otherwise the warning tag with the pricing guidance on its
     tooltip, so the operator knows the way out is the edit dialog's
     pricing save. One component, so the table and the page cannot drift
     apart again (they once rendered two hand-rolled copies). -->
<template>
  <n-tooltip v-if="unpriced" trigger="hover">
    <template #trigger>
      <n-tag size="small" type="warning" :bordered="false">{{ t('providers.candidateUnpriced') }}</n-tag>
    </template>
    {{ t('providers.candidateUnpriced_tip') }}
  </n-tooltip>
</template>

<script setup lang="ts">
// NTooltip is NOT in main.ts's create() components list (only ~28 common
// ones are) — import it explicitly, or it silently renders as an unknown
// element (vue-tsc / vite build stay green). Same treatment for NTag.
import { computed } from 'vue'
import { NTag, NTooltip } from 'naive-ui'
import { useI18n } from 'vue-i18n'
import { candidateUnpriced } from '../../utils/imagePriceSummary'

// The predicate's own parameter shape: both mapping-row vocabularies (the
// provider page's ProviderCandidate, the model/pricing pages'
// ModelCandidate) satisfy it.
const props = defineProps<{ candidate: Parameters<typeof candidateUnpriced>[0] }>()

const { t } = useI18n()

// Self-guarding: a priced candidate renders nothing, so a caller cannot
// badge a row the shared predicate calls priced.
const unpriced = computed(() => candidateUnpriced(props.candidate))
</script>
