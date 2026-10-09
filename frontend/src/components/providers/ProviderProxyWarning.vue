<!-- frontend/src/components/providers/ProviderProxyWarning.vue
     The warning mark the provider pages hang on a provider whose outbound
     traffic resolves to proxied: a small alert triangle whose hover tooltip
     says what changed about the link — traffic now leaves through the
     configured proxy. The parent decides visibility (it owns the stance
     resolution; see utils/outboundProxy.providerProxyEffective); this
     component only renders the mark. Desktop shows the tooltip on hover;
     mobile, which has no hover, swaps in a tap-to-toast — the same split
     HelpLabel established. -->
<template>
  <!-- Desktop: hover tooltip. -->
  <NTooltip v-if="!isMobile" trigger="hover" placement="top">
    <template #trigger>
      <!-- Clicks stop here: the icon shares a clickable row/header with
           navigation, which it must not trigger. -->
      <NIcon
        :size="14"
        class="provider-proxy-warning"
        data-testid="provider-proxy-warning"
        role="img"
        :aria-label="tip"
        tabindex="0"
        @click.stop
      >
        <AlertTriangle />
      </NIcon>
    </template>
    {{ tip }}
  </NTooltip>
  <!-- Mobile: tap surfaces the same text as a transient toast. -->
  <NIcon
    v-else
    :size="14"
    class="provider-proxy-warning"
    data-testid="provider-proxy-warning"
    role="button"
    :aria-label="tip"
    style="cursor: pointer"
    @click.stop="onTap"
  >
    <AlertTriangle />
  </NIcon>
</template>

<script setup lang="ts">
// NTooltip/NIcon are NOT in main.ts's create() components list (only ~28
// common ones are). Import them explicitly, or they silently render as
// unknown elements (vue-tsc / vite build stay green).
import { computed } from 'vue'
import { NTooltip, NIcon, useMessage } from 'naive-ui'
import { AlertTriangle } from '@lucide/vue'
import { useI18n } from 'vue-i18n'
import { useIsMobile } from '../../composables/useIsMobile'

const { t } = useI18n()
const message = useMessage()
const isMobile = useIsMobile()

// Computed, not a one-shot const: a locale switch must re-translate the
// tooltip and the accessible name together.
const tip = computed(() => t('providers.proxyWarningTooltip'))

function onTap() {
  message.info(tip.value)
}
</script>

<style scoped>
.provider-proxy-warning {
  color: var(--color-warning);
  cursor: help;
  flex-shrink: 0;
}
</style>
