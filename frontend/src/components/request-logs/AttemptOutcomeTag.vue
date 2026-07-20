<!-- frontend/src/components/request-logs/AttemptOutcomeTag.vue
     Renderer for an AttemptRecord.outcome string (the AttemptOutcome*
     constants in internal/gateway/types.go: success / auth_failed /
     rate_limited / conn_error / server_error / client_error / bad_status).

     Colour groups reflect the relay loop's switch decision, not just the
     HTTP status: green = the attempt that succeeded, amber = key-rotation
     triggers (same candidate retried with another key), red = candidate-
     failover triggers and terminal caller-side errors. Keeping the colour
     semantics aligned with the gateway's switch rule lets an admin scan a
     multi-attempt request and tell "did the chain failover across
     providers" at a glance. -->
<template>
  <NTag :type="tagType" size="small" :bordered="false">{{ label }}</NTag>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { NTag } from 'naive-ui'

const props = defineProps<{ outcome: string }>()
const { t } = useI18n()

const TYPE_MAP: Record<string, 'success' | 'error' | 'warning' | 'default'> = {
  success: 'success',
  auth_failed: 'warning',
  rate_limited: 'warning',
  conn_error: 'error',
  server_error: 'error',
  client_error: 'error',
  bad_status: 'error',
}

const tagType = computed(() => TYPE_MAP[props.outcome] ?? 'default')

// Unknown outcome values (e.g. a future gateway adds a new outcome before
// this i18n key lands) fall back to the raw string rather than a missing-
// key warning — the admin still sees *something* actionable.
const label = computed(() => {
  const key = `requestLogs.attempt_outcome_${props.outcome}`
  const translated = t(key)
  return translated === key ? props.outcome : translated
})
</script>
