import { ref } from 'vue'

/**
 * Tracks a single in-flight row action (reorder/test/etc) so concurrent clicks
 * are blocked and the active row shows loading feedback. Replaces the
 * duplicated testingCandidateId / testingKeyId / reorderingCandidateId patterns.
 *
 * `direction` is null for non-directional actions (test), 'up'/'down' for reorder.
 */
export function useSingleRowAction() {
  const activeId = ref<number | null>(null)
  const direction = ref<'up' | 'down' | null>(null)

  async function run(id: number, fn: () => Promise<void>, dir: 'up' | 'down' | null = null) {
    if (activeId.value !== null) return
    activeId.value = id
    direction.value = dir
    try {
      await fn()
    } finally {
      activeId.value = null
      direction.value = null
    }
  }

  return { activeId, direction, run }
}
