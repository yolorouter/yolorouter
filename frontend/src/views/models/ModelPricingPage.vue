<!-- frontend/src/views/models/ModelPricingPage.vue -->
<!-- The model pricing page skeleton: the PageHeader shell plus the shared
     EmptyState while the grouped comparison table has not landed yet (the
     next ticket replaces the body with the real GET /api/admin/models fetch
     and the per-model grouped price list). -->
<template>
  <div class="common-page">
    <PageHeader
      :eyebrow="t('modelPricing.eyebrow')"
      :title="t('modelPricing.pageTitle')"
      :description="t('modelPricing.pageDescription')"
    >
      <template #actions>
        <n-button @click="refetch">{{ t('modelPricing.refresh') }}</n-button>
      </template>
    </PageHeader>

    <!-- Until the pricing data lands, the body is intentionally just the
         shared empty state — the same component every list page uses. -->
    <EmptyState :icon="Tags" :title="t('modelPricing.emptyTitle')" :description="t('modelPricing.emptyDescription')" />
  </div>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { Tags } from '@lucide/vue'
import PageHeader from '../../components/PageHeader.vue'
import EmptyState from '../../components/EmptyState.vue'

const { t } = useI18n()

// Wired no-op placeholder: there is no data to refetch until the pricing
// fetch arrives with the grouped table (next ticket). n-button needs no
// explicit import — it is in main.ts's create() list.
function refetch() {}
</script>
