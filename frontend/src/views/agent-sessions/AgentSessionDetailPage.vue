<!-- frontend/src/views/agent-sessions/AgentSessionDetailPage.vue
     Tool-session replay (/agent-sessions/:sessionId): the WHOLE session as
     one continuous conversation, the way the agent tool itself showed it
     while the work happened — not a table of requests to click through.

     One fetch (the transcript endpoint) carries everything:

       - a two-line info bar: tool / session id / request count / from-to
         clock plus elapsed span on the first line; tokens in/out, the
         known/unknown cost doublet, and the success ratio on the second.
         Same aggregate semantics as the session list row (the backend
         derives both from the same rows);
       - the deduplicated system-prompt timeline as ONE collapsed group at
         the top of the replay — a single summary line (first appearance
         plus the change count; a never-changed prompt drops the count)
         that expands into the per-entry timeline, each entry annotated
         with the request that first carried it;
       - one replay block per request, in chronological order: the
         caller-side messages that request newly contributed (the backend's
         prefix-dedup pass already stripped everything resent), then the
         assistant turn with its metadata sign (model, duration, in/out
         tokens, a status tag when the request did not succeed) and a link
         to the full request-log detail.

     The assistant reply is attributed by source (the backend's four-way
     priority): inline bodies and stream captures translate through the
     whole-body translators (a stream capture additionally carries the
     "merged from the sent chunks" note), resent copies translate as a bare
     message array, and a reply nothing recorded renders the explicit
     "not recorded" placeholder — never a fake empty bubble.

     Every block's root carries id="req-<request_id>", so a #req-<id> URL
     hash scrolls the turn into view on mount (scrollIntoView on the block
     itself — the app shell scrolls inside layout containers, the document
     never overflows). -->
<template>
  <div class="common-page">
    <PageHeader :eyebrow="t('agentSessions.detailEyebrow')" :title="t('agentSessions.detailTitle')" :description="t('agentSessions.detailDescription')">
      <template #actions>
        <NButton quaternary size="small" @click="onBack">{{ t('agentSessions.backToList') }}</NButton>
      </template>
    </PageHeader>

    <div v-if="loading" class="loading-state">{{ t('common.loading') }}</div>

    <EmptyState v-else-if="notFound" type="compact" :title="t('agentSessions.notFound')">
      <template #action>
        <NButton quaternary size="small" @click="onBack">{{ t('agentSessions.backToList') }}</NButton>
      </template>
    </EmptyState>

    <!-- A transport-level failure keeps the page (and its back button) and
         shows the localized displayMessage copy inline — never the raw
         technical error string. -->
    <EmptyState v-else-if="loadError" type="compact" :title="t('agentSessions.loadFailed')" :description="loadError">
      <template #action>
        <NButton quaternary size="small" @click="onBack">{{ t('agentSessions.backToList') }}</NButton>
      </template>
    </EmptyState>

    <template v-else-if="transcript">
      <!-- Info bar, line 1: tool attribution, the verbatim session id, the
           request count, and the from/to clock doublet with the elapsed
           span between the session's first and last request. -->
      <div class="replay-meta">
        <div class="replay-meta__line">
          <NTag size="small" round :bordered="false" type="info">{{ toolName }}</NTag>
          <span class="replay-meta__id">{{ transcript.session.agent_session_id }}</span>
          <span class="replay-meta__item">{{ transcript.session.request_count }} {{ t('agentSessions.detailRequestsUnit') }}</span>
          <span class="replay-meta__item replay-meta__item--clock">
            {{ t('agentSessions.firstSeenLabel') }} {{ formatShortClock(transcript.session.first_seen_at) }}
            → {{ t('agentSessions.lastSeenLabel') }} {{ formatShortClock(transcript.session.last_seen_at) }}
            · {{ t('agentSessions.replayElapsed', { span: sessionSpan }) }}
          </span>
        </div>
        <!-- Info bar, line 2: token sums, the cost doublet (known sum plus
             the unknown-rows marker — unknown rows are never folded into
             the total), and the success ratio. -->
        <div class="replay-meta__line">
          <span class="replay-meta__item">
            {{ t('requestLogs.tokenRowIn') }} {{ transcript.session.input_tokens }} / {{ t('requestLogs.tokenRowOut') }} {{ transcript.session.output_tokens }}
          </span>
          <span class="replay-meta__item replay-meta__item--cost">
            {{ formatMicros(transcript.session.known_cost_micros) }}
            <NTag v-if="transcript.session.unknown_cost_count > 0" size="tiny" round :bordered="false" type="warning">
              {{ t('agentSessions.costUnknownNote', { n: transcript.session.unknown_cost_count }) }}
            </NTag>
          </span>
          <span class="replay-meta__item">{{ t('agentSessions.replaySuccessRatio', { success: transcript.session.success_count, total: transcript.session.request_count }) }}</span>
        </div>
      </div>

      <!-- The deduplicated system-prompt timeline as ONE collapsed group.
           Real agent-tool traffic rewrites the system prompt on nearly
           every request (volatile environment info rides inside it), so
           the first-appearance-plus-changes timeline can run to dozens of
           entries — a collapsible block per entry filled the whole first
           screen above the conversation. The group therefore collapses to
           a single summary line by default (first appearance plus the
           change count; a never-changed prompt drops the count), and
           expanding it reveals the original per-entry timeline: each entry
           still its own collapsible block, annotated with the request that
           first carried it and its truncation mark. -->
      <NCollapse v-if="transcript.system.length > 0" class="replay-system">
        <NCollapseItem name="system">
          <template #header>
            <span class="replay-system__title">{{ systemSummary }}</span>
          </template>
          <NCollapse v-for="(entry, i) in transcript.system" :key="`sys-${i}`" class="replay-system__entry">
            <NCollapseItem name="system">
              <template #header>
                <span class="replay-system__entry-title">{{ t('agentSessions.systemBlockTitle') }}</span>
                <span class="replay-system__seen">{{ t('agentSessions.systemSeenAt', { id: entry.seen_at_request }) }}</span>
                <span v-if="entry.truncated" class="replay-system__truncated">{{ t('agentSessions.flowTruncated') }}</span>
              </template>
              <ChatMessageFlow :body="systemFlow(entry)" :raw="systemRaw(entry)" />
            </NCollapseItem>
          </NCollapse>
        </NCollapseItem>
      </NCollapse>

      <!-- The replay itself: one block per request, chronological. -->
      <section v-for="block in transcript.blocks" :id="`req-${block.request_id}`" :key="block.request_id" class="replay-block">
        <!-- The caller-side messages this request newly contributed. A
             truncated request body degrades to an empty array upstream, so
             the flow renders only when there is anything to render and the
             truncation notice line carries the "why" on its own. -->
        <ChatMessageFlow v-if="block.added.length > 0" :body="addedFlow(block)" :raw="blockRaw(block.added)" />
        <p v-if="block.added_truncated" class="replay-block__truncated">{{ t('agentSessions.flowTruncated') }}</p>

        <!-- The assistant turn: metadata sign first (model, duration, in/out
             tokens, a status tag for every non-success request — a failed
             turn must read as failed, never silently disappear), the
             "merged from the sent chunks" note for stream captures, then
             the reply itself through the source-routed translator. -->
        <div class="replay-block__assistant">
          <div class="replay-block__meta">
            <span class="replay-block__model" :title="block.model_name">{{ block.model_name || '—' }}</span>
            <span class="replay-block__figure">{{ formatDuration(block.duration_ms) }}</span>
            <span class="replay-block__figure">{{ t('requestLogs.tokenRowIn') }} {{ block.input_tokens }} / {{ t('requestLogs.tokenRowOut') }} {{ block.output_tokens }}</span>
            <StatusClassTag v-if="block.status_class !== 'success'" :status="block.status_class" />
            <span v-if="block.assistant.source === 'stream'" class="replay-block__mergednote">{{ t('agentSessions.streamMergedNote') }}</span>
            <NButton text size="tiny" type="primary" @click="goRequestDetail(block.request_id)">{{ t('agentSessions.replayRequestLink') }}</NButton>
          </div>
          <ChatMessageFlow :body="assistantFlow(block)" :raw="assistantRaw(block)" />
        </div>
      </section>
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import { NButton, NCollapse, NCollapseItem, NTag, useMessage } from 'naive-ui'
import { getAgentSessionTranscript, type AgentSessionReplayBlock, type AgentSessionReplaySystem, type AgentSessionTranscript } from '../../api/agentSessions'
import { APIError, displayMessage } from '../../api/client'
import { formatDuration, formatShortClock, formatSpan } from '../../utils/format'
import { formatMicros } from '../../utils/money'
import { agentClientLabelKey } from '../../utils/agentClient'
import { translateMessageArray, translateResponseBody, translateStreamBody, type TranslatedBody } from '../../utils/messageTranslator'
import PageHeader from '../../components/PageHeader.vue'
import EmptyState from '../../components/EmptyState.vue'
import StatusClassTag from '../../components/request-logs/StatusClassTag.vue'
import ChatMessageFlow from '../../components/agent-sessions/ChatMessageFlow.vue'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const message = useMessage()

const transcript = ref<AgentSessionTranscript | null>(null)
const loading = ref(false)
const notFound = ref(false)
const loadError = ref('')

// sessionId comes from the URL, decoded once here. Session ids are opaque
// tool-side identifiers (uuids for Claude Code, arbitrary strings for
// others), so no shape validation — an unknown id renders the not-found
// state below rather than a validation error.
const sessionId = computed(() => decodeURIComponent(String(route.params.sessionId ?? '')))

onMounted(() => {
  void reload().catch((err) => message.error(displayMessage(err, t)))
})

// 14006 = errcode.AgentSessionNotFound (pkg/errcode/errcode.go). Detected
// by code, not message text, so the not-found rendering is
// locale-independent — same convention as the request-log detail's 14005.
const AGENT_SESSION_NOT_FOUND_CODE = 14006

async function reload() {
  if (!sessionId.value) {
    notFound.value = true
    return
  }
  loading.value = true
  notFound.value = false
  loadError.value = ''
  try {
    transcript.value = await getAgentSessionTranscript(sessionId.value)
  } catch (err) {
    if (err instanceof APIError && err.code === AGENT_SESSION_NOT_FOUND_CODE) {
      notFound.value = true
      return
    }
    // The repo-wide convention (api/client.ts displayMessage): an APIError's
    // already-localized message, or the localized network fallback for
    // NetworkError/timeout — never a raw technical string in the UI.
    loadError.value = displayMessage(err, t)
  } finally {
    loading.value = false
  }
}

// ---------- Anchor scrolling ----------

// A #req-<requestId> hash deep-links to one replay turn. The scroll runs
// post-flush (after the transcript's DOM is committed) via scrollIntoView
// on the block element itself — the app shell scrolls inside naive-ui
// layout containers and the document never overflows, so window.scrollTo
// would be a no-op. The optional call guards test DOMs that define no
// scrollIntoView; real browsers always have it.
watch(
  transcript,
  (tr) => {
    if (!tr) return
    const hash = route.hash
    if (!hash.startsWith('#req-')) return
    document.getElementById(hash.slice(1))?.scrollIntoView?.({ block: 'start' })
  },
  { flush: 'post' },
)

// ---------- Derived figures ----------

// Tool display name via the shared enum mirror; a client outside the enum
// (newer gateway) falls back to the raw value — still the truth.
const toolName = computed(() => {
  const client = transcript.value?.session.agent_client ?? ''
  const key = agentClientLabelKey(client)
  return key ? t(key) : client || '-'
})

// Elapsed span between the session's first and last request — formatSpan
// is the shared session-length formatter (sub-second ms, sub-minute
// seconds, then h/m/s). Malformed timestamps parse to NaN and render as
// NaN honesty rather than crashing the page.
const sessionSpan = computed(() => {
  const s = transcript.value?.session
  if (!s) return ''
  return formatSpan(Date.parse(s.last_seen_at) - Date.parse(s.first_seen_at))
})

// The collapsed group's one-line summary: the first appearance plus the
// change count (N = timeline entries - 1; the first entry IS the first
// appearance). A prompt that never changed degenerates to the plain
// first-appearance copy with no count.
const systemSummary = computed(() => {
  const entries = transcript.value?.system ?? []
  return entries.length > 1
    ? t('agentSessions.systemGroupChanges', { n: entries.length - 1 })
    : t('agentSessions.systemGroupFirstOnly')
})

// ---------- Translation (pure functions, applied per render) ----------

function blockRaw(added: unknown[]): string {
  return JSON.stringify(added, null, 2)
}

// The added slice: a bare message array, already parsed out of the request
// body by the backend's dedup pass. Translates through the slice entry;
// an untranslatable slice degrades to the fallback signal the flow
// component renders.
function addedFlow(block: AgentSessionReplayBlock): TranslatedBody {
  return translateMessageArray(block.added)
}

// One system-timeline entry. Raw is polymorphic by dialect: the Anthropic
// top-level system value (string or block array) wraps into a synthetic
// system message, while the OpenAI dialect's role=system message objects
// are already a message array and translate directly.
function systemFlow(entry: AgentSessionReplaySystem): TranslatedBody {
  const raw = entry.raw
  if (Array.isArray(raw) && raw.some((el) => typeof el === 'object' && el !== null && 'role' in el)) {
    return translateMessageArray(raw)
  }
  return translateMessageArray([{ role: 'system', content: raw }])
}

function systemRaw(entry: AgentSessionReplaySystem): string {
  const raw = entry.raw
  if (typeof raw === 'string') return raw
  return blockRaw(Array.isArray(raw) ? raw : [raw])
}

// The assistant half of one turn, routed by the backend's source tag:
//   inline — the recorded response body, whole-body response translator;
//   stream — the sent-SSE capture, merged back into the full reply (the
//             merged-note in the metadata sign marks this path);
//   resend — the assistant copies the next request resent as history,
//             translated as a bare message array;
//   none   — nothing was recorded: the explicit placeholder, never a fake
//             empty bubble ("not recorded" must not read as "the model
//             chose to say nothing").
// The backend's truncation flag rides alongside whatever the translator
// itself detected in the text, so the hint shows on either signal.
function assistantFlow(block: AgentSessionReplayBlock): TranslatedBody {
  const a = block.assistant
  let body: TranslatedBody
  if (a.source === 'inline') body = translateResponseBody(typeof a.raw === 'string' ? a.raw : '')
  else if (a.source === 'stream') body = translateStreamBody(typeof a.raw === 'string' ? a.raw : '')
  else if (a.source === 'resend') body = translateMessageArray(a.raw)
  else body = { kind: 'placeholder', messages: [], truncated: false }
  return a.truncated && !body.truncated ? { ...body, truncated: true } : body
}

// The fallback view's raw text: the captured text sources feed their text,
// the resend copies (message objects) feed their pretty-printed JSON.
function assistantRaw(block: AgentSessionReplayBlock): string {
  const a = block.assistant
  if (a.source === 'inline' || a.source === 'stream') return typeof a.raw === 'string' ? a.raw : ''
  if (a.source === 'resend') return blockRaw(Array.isArray(a.raw) ? a.raw : [])
  return ''
}

// ---------- Navigation ----------

function onBack() {
  router.push('/agent-sessions')
}

// The escape hatch to the full request-log detail — everything the replay
// deliberately does not repeat (attempts, upstream bodies, ...).
function goRequestDetail(requestId: string) {
  router.push(`/request-logs/${encodeURIComponent(requestId)}`)
}
</script>

<style scoped>
/* Info bar: two compact lines above the replay. Both wrap on narrow
   viewports (each item is an unbreakable-ish chip with gaps between), so
   the mobile reading is a stacked list rather than a clipped strip. */
.replay-meta {
  display: flex;
  flex-direction: column;
  gap: var(--space-1, 4px);
  margin-bottom: var(--space-4, 16px);
  padding: var(--space-3, 12px) var(--space-4, 16px);
  border: 1px solid var(--color-border, #e0e0e6);
  border-radius: var(--radius-md, 8px);
  background: var(--color-surface, #fff);
}

.replay-meta__line {
  display: flex;
  align-items: center;
  gap: var(--space-3, 12px);
  flex-wrap: wrap;
  font-size: 12px;
  line-height: 1.6;
}

.replay-meta__id {
  font-family: var(--font-mono, monospace);
  font-size: 12px;
  color: var(--color-text-secondary);
  word-break: break-all;
}

.replay-meta__item {
  color: var(--color-text-secondary);
  font-variant-numeric: tabular-nums;
}

.replay-meta__item--clock {
  color: var(--color-text-muted);
}

.replay-meta__item--cost {
  display: inline-flex;
  align-items: center;
  gap: var(--space-2, 8px);
  font-variant-numeric: tabular-nums;
}

/* System-prompt timeline: ONE bordered group container, collapsed by
   default to its single summary line — quieter than the conversation
   turns, and never taller than that line until the reader opens it. */
.replay-system {
  margin-bottom: var(--space-3, 12px);
  border: 1px dashed var(--color-border, #e0e0e6);
  border-radius: var(--radius-md, 8px);
  background: var(--color-bg-soft, #f2f3f5);
}

.replay-system__title {
  font-size: 12px;
  font-weight: 600;
  color: var(--color-text-secondary);
}

/* Per-entry blocks inside the expanded group: the original timeline, one
   collapsible block per distinct value, separated by dashed rules. */
.replay-system__entry {
  border-top: 1px dashed var(--color-border, #e0e0e6);
}

.replay-system__entry:first-child {
  border-top: none;
}

.replay-system__entry-title {
  font-size: 12px;
  font-weight: 600;
  color: var(--color-text-secondary);
}

.replay-system__seen {
  margin-left: var(--space-2, 8px);
  font-family: var(--font-mono, monospace);
  font-size: 11px;
  color: var(--color-text-muted);
}

.replay-system__truncated {
  margin-left: var(--space-2, 8px);
  font-size: 11px;
  color: var(--color-warning-text, #d48806);
}

/* One replay turn: the newly-added messages, then the assistant half. The
   assistant metadata sign rides above the reply, right-aligned so it reads
   as attached to the assistant side of the conversation. */
.replay-block {
  display: flex;
  flex-direction: column;
  gap: var(--space-3, 12px);
  margin-bottom: var(--space-5, 20px);
  scroll-margin-top: var(--space-4, 16px);
}

.replay-block__assistant {
  display: flex;
  flex-direction: column;
  gap: var(--space-2, 8px);
}

.replay-block__meta {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: var(--space-2, 8px);
  flex-wrap: wrap;
  font-size: 11px;
  line-height: 1.6;
}

.replay-block__model {
  max-width: 46ch;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-weight: 600;
  color: var(--color-text-secondary);
}

.replay-block__figure {
  color: var(--color-text-muted);
  font-variant-numeric: tabular-nums;
  white-space: nowrap;
}

.replay-block__mergednote {
  padding: 1px var(--space-2, 8px);
  border-radius: var(--radius-full, 999px);
  background: var(--color-cyan-subtle, #e6f5fa);
  color: var(--color-cyan, #2080f0);
  white-space: nowrap;
}

.replay-block__truncated {
  margin: 0;
  padding: var(--space-1, 4px) var(--space-2, 8px);
  border-radius: var(--radius-sm, 6px);
  background: var(--color-warning-subtle, #fdf6ec);
  color: var(--color-text-secondary);
  font-size: var(--text-xs, 12px);
  align-self: flex-start;
}
</style>
