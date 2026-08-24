import { getCurrentInstance, onUnmounted } from 'vue'

// What one poll tick tells the loop:
//   'again' — keep polling at the base pace
//   'error' — keep polling, but back off (doubling up to the cap)
//   'done'  — settled; stop
//   'stop'  — terminal for another reason (e.g. session expired); stop
export type PollTickOutcome = 'again' | 'error' | 'done' | 'stop'

// A generation-guarded polling loop with error backoff, shared by the import
// dialog's progress view and the provider detail page's candidates takeover.
// The pacing MECHANISM lives here; the shared pace VALUES live next to the
// progress semantics (PROGRESS_POLL_BASE_MS / PROGRESS_POLL_BACKOFF_CAP_MS).
//
// Guarantees: stop() (and a new start()) invalidates any in-flight tick — it
// neither reschedules nor should write state, which is what the isCurrent
// callback passed to the tick is for; the backoff belongs to one start()'s
// loop and resets with the next; the loop dies with the component.
export function useBackoffPoll(baseMs: number, capMs: number) {
  let timer: ReturnType<typeof setTimeout> | null = null
  let generation = 0
  let delay = baseMs

  function stop() {
    generation++
    delay = baseMs
    if (timer !== null) {
      clearTimeout(timer)
      timer = null
    }
  }

  function start(tick: (isCurrent: () => boolean) => Promise<PollTickOutcome>) {
    stop()
    const gen = generation
    const isCurrent = () => gen === generation
    const run = async () => {
      const outcome = await tick(isCurrent)
      if (!isCurrent()) return
      if (outcome === 'done' || outcome === 'stop') return
      delay = outcome === 'error' ? Math.min(delay * 2, capMs) : baseMs
      timer = setTimeout(() => void run(), delay)
    }
    void run()
  }

  // Usable outside a component too (plain modules, tests): the lifetime hook
  // only applies when there is a component instance to tie it to.
  if (getCurrentInstance()) onUnmounted(stop)
  return { start, stop }
}
