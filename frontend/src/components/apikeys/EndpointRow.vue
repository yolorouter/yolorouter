<template>
  <div class="endpoint-row" :class="{ 'endpoint-row--wide': wide }">
    <span v-if="label" class="endpoint-row__label">{{ label }}</span>
    <code class="endpoint-row__value">{{ value }}</code>
    <n-button size="tiny" quaternary :disabled="pending" @click="copy">
      <template #icon><Copy :size="13" /></template>
    </n-button>
  </div>
</template>

<script setup lang="ts">
// One line of gateway access info: an optional label, a monospace value, and
// a copy button. The API Keys page and the create-key dialog both show
// several of these; owning the copy-and-toast here keeps them from each
// carrying their own copy of the same handler.
//
// wide is for values that are whole example requests: they wrap instead of
// ellipsizing, and the button aligns to the first line.
import { NButton, useMessage } from 'naive-ui'
import { Copy } from '@lucide/vue'
import { useI18n } from 'vue-i18n'

import { copyToClipboard } from '../../utils/clipboard'

const props = defineProps<{
  value: string
  label?: string
  wide?: boolean
  // While the gateway address is still being resolved, the value on screen
  // is the origin fallback — fine to look at, not fine to paste into a
  // client config. Copying is held back until the real answer lands (or
  // the request settles without one).
  pending?: boolean
}>()

const { t } = useI18n()
const message = useMessage()

// The failure path matters here more than most: a copy can only fail on a
// non-secure context, i.e. plain HTTP — which is exactly how a self-hosted
// gateway on a LAN address is usually reached. Staying silent there would
// leave the user believing they hold the endpoint they never got.
async function copy() {
  if (await copyToClipboard(props.value)) {
    message.success(t('common.copied'))
  } else {
    message.error(t('common.copyFailed'))
  }
}
</script>

<style scoped lang="less">
.endpoint-row {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  min-width: 0;
}

.endpoint-row__label {
  flex: 0 0 auto;
  font-size: var(--text-xs);
  color: var(--color-text-secondary);
}

.endpoint-row__value {
  flex: 1 1 auto;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-family: var(--font-mono);
  font-size: var(--text-xs);
}

.endpoint-row--wide {
  align-items: flex-start;

  .endpoint-row__value {
    overflow: visible;
    white-space: normal;
    word-break: break-all;
  }
}
</style>
