<!-- frontend/src/components/request-logs/StatusClassTag.vue
     Shared renderer for the five status_class buckets (success / failed /
     partial / cancelled / rejected) used by both RequestLogListPage and
     RequestLogDetailPage. The buckets mirror repository.applyStatusClass +
     service.DeriveStatusClass — both the SQL WHERE filter and the per-row
     derivation use the same five outputs, so a row's tag colour and its
     filter bucket always agree.

     NTag is in main.ts's create() components list, so it doesn't need a
     per-file import here. -->
<template>
  <NTag :type="tagType" size="small" :bordered="false">{{ label }}</NTag>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { NTag } from 'naive-ui'
import type { StatusClass } from '../../api/requestLogs'

const props = defineProps<{ status: StatusClass }>()
const { t } = useI18n()

// success: green | failed: red | partial: amber (2xx with a recorded
// fail_reason — the gateway recovered via failover but logged the dip) |
// cancelled: neutral grey (499 caller-abort, not a server fault) |
// rejected: amber (4xx auth/permission/rate-limit — caller-side, not a
// server fault). amber vs red mirrors the dashboard's " ended = 2xx + 4xx/5xx"
// success-rate rule: rejected and partial are surfaced but don't read as
// "broken" the way a hard 5xx does.
const TYPE_MAP: Record<StatusClass, 'success' | 'error' | 'warning' | 'default'> = {
  success: 'success',
  failed: 'error',
  partial: 'warning',
  cancelled: 'default',
  rejected: 'warning',
}

const tagType = computed(() => TYPE_MAP[props.status] ?? 'default')
const label = computed(() => t(`requestLogs.status_${props.status}`))
</script>
