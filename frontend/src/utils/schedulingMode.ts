// Shared option list for the model scheduling-mode selector. Both the
// create and edit dialogs offer the same two modes with the same labels, so
// the list lives here rather than being re-declared per modal.
import { computed, type ComputedRef } from 'vue'
import { useI18n } from 'vue-i18n'
import type { SelectOption } from 'naive-ui'
import type { SchedulingMode } from '../api/models'

export function useSchedulingModeOptions(): ComputedRef<SelectOption[]> {
  const { t } = useI18n()
  return computed(() => [
    { label: t('models.schedulingModeFailover'), value: 'failover' satisfies SchedulingMode },
    { label: t('models.schedulingModeBalanced'), value: 'balanced' satisfies SchedulingMode },
  ])
}

// isBalancedModel keeps the mode comparison next to the vocabulary it
// belongs to, so views never re-type the literal.
export function isBalancedModel(m: { scheduling_mode: SchedulingMode }): boolean {
  return m.scheduling_mode === 'balanced'
}
