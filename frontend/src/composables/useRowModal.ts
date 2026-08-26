import { computed, ref, type Ref, type WritableComputedRef } from 'vue'

/**
 * Binds a per-row modal's v-model:show to the row it acts on: assigning a
 * row opens the modal, closing it clears the row. Keeping the two in one
 * place stops a page with several row modals from drifting into
 * open-without-a-row (or row-without-a-modal) states.
 *
 * The row carries only what its modal renders, plus the id every such
 * modal needs to address the record it is editing.
 */
export function useRowModal<T extends { id: number }>(): {
  row: Ref<T | null>
  show: WritableComputedRef<boolean>
} {
  const row = ref<T | null>(null) as Ref<T | null>
  const show = computed({
    get: () => row.value !== null,
    set: (v: boolean) => {
      if (!v) row.value = null
    },
  })
  return { row, show }
}
