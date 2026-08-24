<!-- frontend/src/components/HelpLabel.vue -->
<!-- Label text + an inline "?" tooltip. Used directly in form-item #label
     slots and via columnTitle() for table headers. This is the SINGLE
     implementation source for the "?" glyph (NIcon + lucide CircleHelp +
     NTooltip) — do not inline it elsewhere. The tooltip caps at 320px so a
     long tip wraps instead of stretching past the viewport on one line. -->
<template>
  <span class="help-label">
    <slot />
    <!-- Mobile: hover tooltips don't exist on touch, so the "?" becomes a tap
         target that surfaces the same text as a transient message toast. -->
    <NIcon
      v-if="isMobile"
      :size="13"
      style="cursor: pointer; opacity: 0.45"
      role="button"
      :aria-label="tip"
      @click.stop="onTap"
    >
      <CircleHelp />
    </NIcon>
    <NTooltip v-else trigger="hover" placement="top" :max-width="TIP_MAX_WIDTH">
      <template #trigger>
        <!-- Clicks stop here: a help icon inside a clickable card must not
             trigger the card's navigation. -->
        <NIcon :size="13" style="cursor: help; opacity: 0.45" tabindex="0" role="img" :aria-label="tip" @click.stop>
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
import { NTooltip, NIcon, useMessage } from 'naive-ui'
import { CircleHelp } from '@lucide/vue'
import { useIsMobile } from '../composables/useIsMobile'

// Bubble cap for every desktop tooltip: long tips wrap instead of
// stretching past the viewport on one line.
const TIP_MAX_WIDTH = 320

const props = defineProps<{ tip: string }>()

const message = useMessage()

// On mobile the hover tooltip is swapped for a tap-to-toast, since touch
// devices have no hover.
const isMobile = useIsMobile()

function onTap() {
  message.info(props.tip)
}
</script>

<style scoped>
.help-label {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  cursor: default;
}
</style>
