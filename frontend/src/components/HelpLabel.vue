<!-- frontend/src/components/HelpLabel.vue -->
<!-- Label text + an inline "?" tooltip. Used directly in form-item #label
     slots and via columnTitle() for table headers. This is the SINGLE
     implementation source for the "?" glyph (NIcon + lucide CircleHelp +
     NTooltip) — do not inline it elsewhere. -->
<template>
  <span class="help-label">
    <slot />
    <NTooltip trigger="hover" placement="top">
      <template #trigger>
        <NIcon :size="13" style="cursor: help; opacity: 0.45" tabindex="0" role="img" :aria-label="tip">
          <CircleHelp />
        </NIcon>
      </template>
      {{ tip }}
    </NTooltip>
  </span>
</template>

<script setup lang="ts">
// NTooltip/NIcon are NOT in main.ts's create() components list (only ~28
// common ones are). Import them explicitly, or they silently render as
// unknown elements (vue-tsc / vite build stay green).
import { NTooltip, NIcon } from 'naive-ui'
import { CircleHelp } from '@lucide/vue'

defineProps<{ tip: string }>()
</script>

<style scoped>
.help-label {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  cursor: default;
}
</style>
