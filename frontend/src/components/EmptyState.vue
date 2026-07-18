<!-- Adapted from the reference project's shared/components/EmptyState.vue
     (yolorouter-frontend/src/shared/components/EmptyState.vue) — uses the
     simpler gradient "mark" treatment (.empty-state__mark, styles/global.less)
     rather than porting the full inline illustration SVG, since this
     admin tool's empty states are small inline messages, not full-page
     moments. -->
<script setup lang="ts">
defineProps<{
  title?: string
  description?: string
}>()
</script>

<template>
  <div class="empty-state">
    <div class="empty-state__mark" />
    <h3 v-if="title">{{ title }}</h3>
    <p v-if="description">{{ description }}</p>
    <div v-if="$slots.action" class="empty-state__action">
      <slot name="action" />
    </div>
  </div>
</template>
