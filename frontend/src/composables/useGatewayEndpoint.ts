// frontend/src/composables/useGatewayEndpoint.ts
//
// Shared, module-cached accessor for the gateway's public base URL — the
// address API clients should point at — together with the two shapes every
// caller derives from it. Server-resolved via /api/admin/system/endpoint
// (configured server.external_url wins, request-derived otherwise).
//
// What it hands out is never empty: until the server answers, and if the
// server never does, the console's own origin stands in. That guess is the
// correct answer on the single-host deployments that need no configuration
// at all, and on the rest it is at least a whole URL — callers must never
// render or copy a host-less "/v1".

import { computed, type ComputedRef, ref, type Ref } from 'vue'
import { getSystemEndpoint } from '../api/system'
import { geminiBaseUrlOf, openAIBaseUrlOf } from '../utils/apiExamples'

// Module-scoped rather than per-caller, unlike most composables here: the
// address is one process-wide fact, and several components ask for it on
// the same screen. Per-caller state would mean a network round trip each
// time one of them mounts, for an answer that cannot differ.
//
// Empty means "the server has not told us yet" — the fallback lives in the
// exported computed below, in one place, so no call site can forget it.
const serverEndpoint = ref('')

// Latched only on a real answer. A failure — or an empty one — deliberately
// leaves this false: the origin fallback is a guess, and pinning it for the
// rest of the session would keep showing the console's own address on every
// deployment where the gateway lives elsewhere.
//
// The retry that buys is coarse, and worth stating plainly: callers on one
// screen mount in the same tick and share a single in-flight request, so a
// failure leaves that whole screen on the fallback. What retries is the
// next screen — navigating away and back re-runs load().
let resolved = false
let inFlight: Promise<void> | null = null

// True only while an answer is still on its way. Consumers use it to keep
// the fallback from being copied: what they render during that window is a
// guess, and a guess pasted into a client config is worse than a moment's
// wait. It goes false again when the request settles either way — after a
// failure the fallback is all there will ever be, so blocking on it
// forever would help nobody.
const pending = ref(false)

async function load() {
  pending.value = true
  try {
    const answer = (await getSystemEndpoint()).endpoint
    if (answer) {
      serverEndpoint.value = answer
      resolved = true
    }
    // An empty answer is not an answer: leave the fallback in place and the
    // latch open, exactly as for a failed request.
  } catch {
    // Same handling, and nothing to write — the fallback is already what
    // the computed below yields.
  } finally {
    inFlight = null
    pending.value = false
  }
}

const endpoint = computed(() => serverEndpoint.value || window.location.origin)

// OpenAI-compatible clients call {base}/chat/completions, so they need the
// /v1 suffix. Anthropic-compatible ones take the bare endpoint and append
// /v1/messages themselves, and Gemini-native ones call
// {base}/v1beta/models/{model}:generateContent, so that family gets the
// /v1beta suffix. Both suffix derivations live with the samples in
// utils/apiExamples.ts so the panel rows and every sample agree.
const openAIBaseUrl = computed(() => openAIBaseUrlOf(endpoint.value))
const geminiBaseUrl = computed(() => geminiBaseUrlOf(endpoint.value))


export function useGatewayEndpoint(): {
  endpoint: ComputedRef<string>
  openAIBaseUrl: ComputedRef<string>
  geminiBaseUrl: ComputedRef<string>
  pending: Ref<boolean>
} {
  // load() must never throw synchronously: it is assigned to inFlight, and
  // its own finally is what clears that. A synchronous throw would escape
  // before the assignment, leaving inFlight non-null with resolved false —
  // no caller would ever retry. It holds because load() is async and the
  // fetch beneath it is too; keep it that way.
  if (!resolved && !inFlight) {
    inFlight = load()
  }
  return { endpoint, openAIBaseUrl, geminiBaseUrl, pending }
}
