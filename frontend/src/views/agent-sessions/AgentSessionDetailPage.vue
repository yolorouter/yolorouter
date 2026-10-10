<!-- frontend/src/views/agent-sessions/AgentSessionDetailPage.vue
     Tool-session replay (/agent-sessions/:sessionId): the WHOLE session as
     one continuous conversation, the way the agent tool itself showed it
     while the work happened — not a table of requests to click through.

     The replay loads lazily, one cursor-paginated page at a time:

       - opening the page fetches the LATEST page and lands the viewport on
         the newest turn (the agent-tool convention for resuming a session:
         the reader comes back to see how far the work got);
       - a sentinel above the loaded turns watches for the reader scrolling
         up; each hit fetches the next older page through the exclusive
         before cursor, prepends it, and compensates scrollTop by the
         inserted height so the viewport stays on the turn being read;
       - when the page was entered through a #req-<id> deep link, the first
         fetch anchors AT that turn instead (the inclusive after cursor);
         the older direction keeps working through the same sentinel, and
         the toward-newer direction gets its own sentinel below the turns
         until the session's tail is loaded;
       - a session short enough to fit one page renders no paging controls
         at all — small sessions see exactly the old behavior.

     The transcript page carries everything else whole on every fetch:

       - a two-line info bar: tool / session id / request count / from-to
         clock plus elapsed span on the first line; tokens in/out, the
         known/unknown cost doublet, and the success ratio on the second.
         Same aggregate semantics as the session list row (the backend
         derives both from the same rows) — and whole-session by contract,
         so the numbers never depend on how much is loaded;
       - the deduplicated system-prompt timeline as ONE collapsed group at
         the top of the replay — titled simply "System Prompt" and collapsed
         to that bare title line (real agent traffic rewrites the prompt on
         nearly every request, so the historical versions stay SECONDARY,
         living inside the expansion; no first-appearance/change-count
         summary copy). The group ships METADATA only; expanding it fetches
         the timeline's raw text once, and each entry then expands into its
         prompt, annotated with the request that carried it;
       - one replay block per loaded request, in chronological order: the
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
     hash scrolls the turn into view (scrollIntoView on the block itself —
     the app shell scrolls inside layout containers, the document never
     overflows). -->
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

    <template v-else-if="session">
      <!-- Info bar, line 1: tool attribution, the verbatim session id, the
           request count, and the from/to clock doublet with the elapsed
           span between the session's first and last request. -->
      <div class="replay-meta">
        <div class="replay-meta__line">
          <NTag size="small" round :bordered="false" type="info">{{ toolName }}</NTag>
          <span class="replay-meta__id">{{ session.agent_session_id }}</span>
          <span class="replay-meta__item">{{ session.request_count }} {{ t('agentSessions.detailRequestsUnit') }}</span>
          <span class="replay-meta__item replay-meta__item--clock">
            {{ t('agentSessions.firstSeenLabel') }} {{ formatShortClock(session.first_seen_at) }}
            → {{ t('agentSessions.lastSeenLabel') }} {{ formatShortClock(session.last_seen_at) }}
            · {{ t('agentSessions.replayElapsed', { span: sessionSpan }) }}
          </span>
        </div>
        <!-- Info bar, line 2: token sums, the cost doublet (known sum plus
             the unknown-rows marker — unknown rows are never folded into
             the total), and the success ratio. The aggregate is
             whole-session on every page, so upscrolling never rewrites
             these numbers. -->
        <div class="replay-meta__line">
          <span class="replay-meta__item">
            {{ t('requestLogs.tokenRowIn') }} {{ session.input_tokens }} / {{ t('requestLogs.tokenRowOut') }} {{ session.output_tokens }}
          </span>
          <span class="replay-meta__item replay-meta__item--cost">
            {{ formatMicros(session.known_cost_micros) }}
            <NTag v-if="session.unknown_cost_count > 0" size="tiny" round :bordered="false" type="warning">
              {{ t('agentSessions.costUnknownNote', { n: session.unknown_cost_count }) }}
            </NTag>
          </span>
          <span class="replay-meta__item">{{ t('agentSessions.replaySuccessRatio', { success: session.success_count, total: session.request_count }) }}</span>
        </div>
      </div>

      <!-- The deduplicated system-prompt timeline as ONE collapsed group —
           titled simply "System Prompt". Real agent-tool traffic rewrites
           the system prompt on nearly every request (volatile environment
           info rides inside it), so the timeline can run to dozens of
           entries; they stay SECONDARY — the group collapses to the bare
           title line, and the historical versions live inside the
           expansion. The raw prompt text is NOT part of any transcript
           page: expanding the group fetches the whole timeline's raw text
           once (.../transcript/system), and each entry then expands into
           its own collapsible block, annotated with the request that
           carried it. -->
      <NCollapse v-if="systemMeta.length > 0" v-model:expanded-names="systemExpandedNames" class="replay-system">
        <NCollapseItem name="system">
          <template #header>
            <span class="replay-system__title">{{ t('agentSessions.systemGroupTitle') }}</span>
          </template>
          <div v-if="systemRawError" class="replay-pager__row replay-pager__row--inside">
            <span class="replay-pager__text replay-pager__text--error">{{ t('agentSessions.systemRawFailed') }}</span>
            <NButton text size="tiny" type="primary" @click="loadSystemRaw">{{ t('agentSessions.retry') }}</NButton>
          </div>
          <div v-else-if="systemRaw === null" class="replay-pager__row replay-pager__row--inside">
            <span class="replay-pager__text">{{ t('common.loading') }}</span>
          </div>
          <NCollapse v-for="(entry, i) in systemRaw ?? []" :key="`sys-${i}`" class="replay-system__entry">
            <NCollapseItem name="system">
              <template #header>
                <span class="replay-system__seen">{{ t('agentSessions.systemEntrySeenAt', { id: entry.seen_at_request }) }}</span>
                <span v-if="entry.truncated" class="replay-system__truncated">{{ t('agentSessions.flowTruncated') }}</span>
              </template>
              <ChatMessageFlow :body="systemFlow(entry)" :raw="systemRawText(entry)" />
            </NCollapseItem>
          </NCollapse>
        </NCollapseItem>
      </NCollapse>

      <!-- The older-direction boundary. The sentinel row exists exactly
           while older blocks remain; when idle it renders no chrome at all
           (a session that fits one page never shows it — nothing to page),
           while busy it carries the loading copy, and a failed page turns
           it into the retry row. -->
      <div v-if="windowState.hasMoreBefore" ref="topSentinelEl" class="replay-pager" data-testid="replay-pager-top">
        <span v-if="olderBusy" class="replay-pager__text">{{ t('agentSessions.loadOlder') }}</span>
        <template v-else-if="olderError">
          <span class="replay-pager__text replay-pager__text--error">{{ t('agentSessions.turnLoadFailed') }}</span>
          <NButton text size="tiny" type="primary" @click="loadOlder">{{ t('agentSessions.retry') }}</NButton>
        </template>
      </div>

      <!-- The replay itself: one block per loaded request, chronological.
           Each block grades its added slice by the turn store's event
           classes (utils/turnEvents mirrors the backend's write-time
           classification): only a HUMAN input renders a user bubble on the
           conversation axis; tool results render as compact result cards;
           machine-injected context collapses behind one quiet line. The
           conversation reads as "system prompt + user input + reply", the
           way a mature chat view renders a session. -->
      <section v-for="block in windowState.blocks" :id="`req-${block.request_id}`" :key="block.request_id" class="replay-block">
        <!-- The caller-side messages this request newly contributed, graded
             per event class. A truncated request body degrades to an empty
             array upstream, so the walk renders only when there is anything
             to render and the truncation notice line carries the "why" on
             its own. -->
        <template v-for="(seg, si) in addedSegments(block)" :key="si">
          <ChatMessageFlow v-if="seg.kind === 'user'" :body="userSegmentBody(seg.message)" :raw="''" />
          <ToolCard
            v-else-if="seg.kind === 'tool_result'"
            kind="result"
            :name="toolNameById.get(seg.toolUseId)"
            :label="seg.toolUseId ? 'tool_result' : undefined"
            :preview="firstPreviewLine(toolResultDetail(seg.content))"
            :detail="toolResultDetail(seg.content)"
          />
          <div v-else class="replay-injected">
            <button type="button" class="replay-injected__toggle" @click="toggleInjected(block.request_id, si)">
              <span class="replay-injected__label">{{ t('agentSessions.injectedContext', { n: seg.count }) }}</span>
              <span class="replay-injected__state">{{ isInjectedOpen(block.request_id, si) ? t('agentSessions.collapse') : t('agentSessions.expand') }}</span>
            </button>
            <div v-if="isInjectedOpen(block.request_id, si)" class="replay-injected__body">
              <pre v-for="(text, ti) in seg.texts" :key="ti" class="replay-injected__text">{{ text }}</pre>
            </div>
          </div>
        </template>
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

      <!-- The toward-newer boundary: exists only when the page entered
           through a mid-history anchor (or a continuation stopped short
           of the tail), same chrome rules as the top sentinel. -->
      <div v-if="windowState.hasMoreAfter" ref="bottomSentinelEl" class="replay-pager" data-testid="replay-pager-bottom">
        <span v-if="newerBusy" class="replay-pager__text">{{ t('agentSessions.loadNewer') }}</span>
        <template v-else-if="newerError">
          <span class="replay-pager__text replay-pager__text--error">{{ t('agentSessions.turnLoadFailed') }}</span>
          <NButton text size="tiny" type="primary" @click="loadNewer">{{ t('agentSessions.retry') }}</NButton>
        </template>
      </div>
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'
import { NButton, NCollapse, NCollapseItem, NTag, useMessage } from 'naive-ui'
import {
  getAgentSessionTranscript,
  getAgentSessionTranscriptSystem,
  type AgentSessionReplayBlock,
  type AgentSessionReplaySystem,
  type AgentSessionRow,
  type AgentSessionReplaySystemMeta,
} from '../../api/agentSessions'
import { APIError, displayMessage } from '../../api/client'
import { formatDuration, formatShortClock, formatSpan } from '../../utils/format'
import { formatMicros } from '../../utils/money'
import { agentClientLabelKey } from '../../utils/agentClient'
import { translateMessageArray, translateResponseBody, translateStreamBody, type ChatMessage, type TranslatedBody } from '../../utils/messageTranslator'
import { buildAddedSegments, collectToolNames, firstPreviewLine, toolResultDetail, type AddedSegment } from '../../utils/turnEvents'
import PageHeader from '../../components/PageHeader.vue'
import EmptyState from '../../components/EmptyState.vue'
import StatusClassTag from '../../components/request-logs/StatusClassTag.vue'
import ChatMessageFlow from '../../components/agent-sessions/ChatMessageFlow.vue'
import ToolCard from '../../components/agent-sessions/ToolCard.vue'
import { anchorRequestId, mergeWindow, prependCompensation, windowFromPage, type TranscriptWindow } from './replayPaging'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const message = useMessage()

// ---------- Lazy-load state ----------

// The loaded window: rendered blocks plus one continuation cursor pair per
// side (see replayPaging.ts). Seeded by the initial page, then extended by
// the sentinels.
const windowState = ref<TranscriptWindow>({ blocks: [], hasMoreBefore: false, nextBefore: '', hasMoreAfter: false, nextAfter: '' })
// The whole-session aggregate and system timeline metadata — every page
// carries them identically, so the info bar's numbers never depend on how
// much is loaded.
const session = ref<AgentSessionRow | null>(null)
const systemMeta = ref<AgentSessionReplaySystemMeta[]>([])
// The system timeline's raw text, fetched once when the collapsed group is
// first expanded (null = not fetched yet; the group shows its loading row).
const systemRaw = ref<AgentSessionReplaySystem[] | null>(null)
const systemRawBusy = ref(false)
const systemRawError = ref(false)
const systemExpandedNames = ref<Array<string | number>>([])

// First-screen states; the sentinel directions keep their own busy/error
// flags so a failed upscroll degrades to a retry row, never a dead page.
const loading = ref(false)
const notFound = ref(false)
const loadError = ref('')
const olderBusy = ref(false)
const olderError = ref(false)
const newerBusy = ref(false)
const newerError = ref(false)

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

// reload fetches the first page. Without an anchor hash that is the LATEST
// page and the viewport lands on the newest turn; with #req-<id> the fetch
// anchors at that turn instead (after is inclusive), and the viewport
// lands on it. A stale anchor — the server answers an unknown cursor with
// an empty page — falls back to the latest page rather than rendering an
// empty replay (a session that truly has requests cannot answer zero
// blocks otherwise).
async function reload() {
  if (!sessionId.value) {
    notFound.value = true
    return
  }
  loading.value = true
  notFound.value = false
  loadError.value = ''
  olderError.value = false
  newerError.value = false
  let anchor: string | null = null
  try {
    anchor = anchorRequestId(route.hash)
    let page = await getAgentSessionTranscript(sessionId.value, anchor ? { after: anchor } : undefined)
    if (anchor && page.blocks.length === 0 && page.session.request_count > 0) {
      page = await getAgentSessionTranscript(sessionId.value)
      anchor = null
    }
    session.value = page.session
    systemMeta.value = page.system
    systemRaw.value = null
    systemRawError.value = false
    windowState.value = windowFromPage(page)
    // Drop the first-screen gate BEFORE the landing scroll: the blocks
    // render on the next flush, and the scroll has to find them in the
    // DOM (the finally below is the error paths' backstop, it runs too
    // late for the scroll).
    loading.value = false
    await scrollInitial(anchor)
    // A re-anchor with the system group already open dropped the raw text
    // above; the expanded-names watch will not refire on its own, so pick
    // the fetch back up here.
    if (systemExpandedNames.value.includes('system')) void loadSystemRaw()
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

// ---------- Landing and anchor scrolling ----------

// scrollInitial parks the viewport after the first page renders:
// an anchor lands its turn at the viewport top, everything else lands the
// NEWEST turn at the viewport bottom (the resume-a-session reading). Both
// run post-flush via scrollIntoView on the block element itself — the app
// shell scrolls inside naive-ui layout containers and the document never
// overflows, so window.scrollTo would be a no-op. The optional call guards
// test DOMs that define no scrollIntoView; real browsers always have it.
async function scrollInitial(anchor: string | null) {
  await nextTick()
  await nextTick()
  scrollToAnchor(anchor ?? '')
}

// scrollToAnchor scrolls one turn into view by its #req-<id> anchor. An
// empty or unloaded id falls back to the newest loaded turn (bottom
// landing) — the caller decides which is the intended target.
function scrollToAnchor(anchor: string) {
  const anchorEl = anchor ? document.getElementById(`req-${anchor}`) : null
  const target = anchorEl ?? lastBlockEl()
  const atAnchor = Boolean(anchor && anchorEl === target)
  target?.scrollIntoView?.({ block: atAnchor ? 'start' : 'end' })
}

function lastBlockEl(): HTMLElement | null {
  const blocks = windowState.value.blocks
  if (blocks.length === 0) return null
  return document.getElementById(`req-${blocks[blocks.length - 1].request_id}`)
}

// A #req-<id> hash arriving after mount (same route, new anchor): a turn
// already in the loaded window only scrolls to it — no refetch; an
// unloaded turn re-anchors the whole view (fresh window from that turn,
// both directions continuable), exactly like entering the page by that
// deep link.
watch(
  () => route.hash,
  (hash) => {
    if (loading.value) return
    const anchor = anchorRequestId(hash)
    if (!anchor) return
    if (windowState.value.blocks.some((b) => b.request_id === anchor)) {
      void nextTick().then(() => scrollToAnchor(anchor))
      return
    }
    void reload().catch((err) => message.error(displayMessage(err, t)))
  },
)

// ---------- Sentinel paging ----------

const topSentinelEl = ref<HTMLElement | null>(null)
const bottomSentinelEl = ref<HTMLElement | null>(null)

// One observer watches both sentinels: each hit pages its own direction.
// The root margin makes the boundary fire slightly BEFORE the sentinel
// reaches the viewport edge, so the next page is usually in place by the
// time the reader arrives. Test DOMs without IntersectionObserver (or with
// the no-op stub happy-dom ships) simply never fire — the retry rows and
// the anchors below still work, paging is the only thing idle.
let viewportObserver: IntersectionObserver | null = null

function ensureObserver(): IntersectionObserver | null {
  if (typeof IntersectionObserver === 'undefined') return null
  if (!viewportObserver) {
    viewportObserver = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) {
          if (!entry.isIntersecting) continue
          if (entry.target === topSentinelEl.value) void loadOlder()
          else if (entry.target === bottomSentinelEl.value) void loadNewer()
        }
      },
      { rootMargin: '120px 0px' },
    )
  }
  return viewportObserver
}

// The sentinel elements come and go with their has_more flags (a session
// that fits one page never renders one); the watches keep the observer
// pointed at whatever elements currently exist.
watch(topSentinelEl, (el, prev) => {
  if (prev) viewportObserver?.unobserve(prev)
  if (el) ensureObserver()?.observe(el)
})
watch(bottomSentinelEl, (el, prev) => {
  if (prev) viewportObserver?.unobserve(prev)
  if (el) ensureObserver()?.observe(el)
})

onBeforeUnmount(() => {
  viewportObserver?.disconnect()
  viewportObserver = null
})

// scrollParentOf walks up from el to the nearest scrollable ancestor — the
// app shell scrolls inside a layout container, not the document, so the
// prepend compensation has to adjust that container's scrollTop. Test DOMs
// expose no overflow style, return null there and the (pure, separately
// tested) compensation is simply skipped.
function scrollParentOf(el: Element | null): HTMLElement | null {
  for (let node = el?.parentElement; node; node = node.parentElement) {
    const overflowY = window.getComputedStyle(node).overflowY
    if ((overflowY === 'auto' || overflowY === 'scroll') && node.scrollHeight > node.clientHeight) return node
  }
  return null
}

// loadOlder pages one step toward the session's head: the exclusive before
// cursor asks for the blocks strictly older than the loaded window's
// oldest, they prepend, and scrollTop grows by the inserted height so the
// turn under the viewport stays under the viewport (the compensation value
// is the pure before/after scrollHeight difference — see replayPaging.ts).
// The busy flag collapses re-entrant fires (an in-flight direction does
// not re-request); a failure flips the sentinel row to its retry form.
async function loadOlder() {
  if (!windowState.value.hasMoreBefore || olderBusy.value) return
  olderBusy.value = true
  olderError.value = false
  const scroller = scrollParentOf(topSentinelEl.value)
  const heightBefore = scroller?.scrollHeight ?? 0
  try {
    const page = await getAgentSessionTranscript(sessionId.value, { before: windowState.value.nextBefore })
    windowState.value = mergeWindow(windowState.value, page, 'older')
    session.value = page.session
    systemMeta.value = page.system
    await nextTick()
    if (scroller) scroller.scrollTop += prependCompensation(heightBefore, scroller.scrollHeight)
  } catch {
    olderError.value = true
  } finally {
    olderBusy.value = false
  }
}

// loadNewer pages one step toward the tail: the inclusive after cursor
// resends the loaded window's newest block (mergeWindow drops the
// duplicate), the new blocks append below, and the viewport needs no
// compensation — content added below the reading position does not move
// it. This direction only exists for an anchored entry (a latest-first
// opening already sits at the tail).
async function loadNewer() {
  if (!windowState.value.hasMoreAfter || newerBusy.value) return
  newerBusy.value = true
  newerError.value = false
  try {
    const page = await getAgentSessionTranscript(sessionId.value, { after: windowState.value.nextAfter })
    windowState.value = mergeWindow(windowState.value, page, 'newer')
    session.value = page.session
    systemMeta.value = page.system
  } catch {
    newerError.value = true
  } finally {
    newerBusy.value = false
  }
}

// ---------- System-prompt raw text (on expand) ----------

// The collapsed group's expansion is the fetch trigger: the transcript
// pages carry metadata only, the raw text is one dedicated request the
// first time the group opens. Re-collapsing and re-expanding does not
// refetch (systemRaw is kept), and neither does continuing to page blocks
// — the timeline is session-wide.
watch(systemExpandedNames, (names) => {
  if (!names.includes('system')) return
  void loadSystemRaw()
})

async function loadSystemRaw() {
  if (systemRaw.value !== null || systemRawBusy.value) return
  systemRawBusy.value = true
  systemRawError.value = false
  try {
    systemRaw.value = (await getAgentSessionTranscriptSystem(sessionId.value)).system
  } catch {
    systemRawError.value = true
  } finally {
    systemRawBusy.value = false
  }
}

// ---------- Derived figures ----------

// Tool display name via the shared enum mirror; a client outside the enum
// (newer gateway) falls back to the raw value — still the truth.
const toolName = computed(() => {
  const client = session.value?.agent_client ?? ''
  const key = agentClientLabelKey(client)
  return key ? t(key) : client || '-'
})

// Elapsed span between the session's first and last request — formatSpan
// is the shared session-length formatter (sub-second ms, sub-minute
// seconds, then h/m/s). Malformed timestamps parse to NaN and render as
// NaN honesty rather than crashing the page.
const sessionSpan = computed(() => {
  const s = session.value
  if (!s) return ''
  return formatSpan(Date.parse(s.last_seen_at) - Date.parse(s.first_seen_at))
})

// ---------- Translation (pure functions, applied per render) ----------

// Pretty-printed JSON for the fallback views (a system entry's structured
// raw, a resend copy's message objects).
function jsonRaw(value: unknown): string {
  return JSON.stringify(value, null, 2)
}

// The added slice, graded by the turn store's event classes: user inputs
// stay on the conversation axis as bubbles, tool results become cards,
// injected context folds into collapsed runs — the render order is the
// wire order, so the chronology of a turn is never rearranged.
function addedSegments(block: AgentSessionReplayBlock): AddedSegment[] {
  return buildAddedSegments(block.added)
}

// One user segment's single-message body: the shared bubble renderer with
// exactly the message that belongs on the axis. kind is always 'messages'
// by construction (the segment builder only emits translated messages).
function userSegmentBody(message: ChatMessage): TranslatedBody {
  return { kind: 'messages', messages: [message], truncated: false }
}

// The session-wide tool id → name index over the loaded window: every
// assistant reply's tool calls (inline bodies, stream captures, resent
// copies — all four sources) contributes its id and name, so a LATER
// turn's tool-result cards can name the tool that produced them (the wire
// carries only ids on the result side). Grows reactively as pages load;
// a result whose calling turn sits outside the window renders with the
// bare card title until that page arrives.
const toolNameById = computed(() => {
  const map = new Map<string, string>()
  for (const block of windowState.value.blocks) {
    collectToolNames(assistantFlow(block), map)
  }
  return map
})

// ---------- Injected-context runs ----------

// Which collapsed injected runs are open — keyed by block id + segment
// index (segment indices are stable per loaded window; a re-anchor rebuilds
// the window and the keys with it, which simply closes the runs again).
const openInjectedRuns = ref(new Set<string>())
const injectedKey = (requestId: string, segIndex: number) => `${requestId}:${segIndex}`
function isInjectedOpen(requestId: string, segIndex: number): boolean {
  return openInjectedRuns.value.has(injectedKey(requestId, segIndex))
}
function toggleInjected(requestId: string, segIndex: number) {
  const key = injectedKey(requestId, segIndex)
  const next = new Set(openInjectedRuns.value)
  if (next.has(key)) next.delete(key)
  else next.add(key)
  openInjectedRuns.value = next
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

function systemRawText(entry: AgentSessionReplaySystem): string {
  const raw = entry.raw
  if (typeof raw === 'string') return raw
  return jsonRaw(Array.isArray(raw) ? raw : [raw])
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
  if (a.source === 'resend') return jsonRaw(Array.isArray(a.raw) ? a.raw : [])
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

.replay-system__seen {
  font-family: var(--font-mono, monospace);
  font-size: 11px;
  color: var(--color-text-muted);
}

.replay-system__truncated {
  margin-left: var(--space-2, 8px);
  font-size: 11px;
  color: var(--color-warning-text, #d48806);
}

/* Machine-injected context (system reminders, skill listings, command
   echoes): NEVER on the conversation axis — one quiet dashed line that
   names how many sections arrived, expandable into their verbatim text.
   Reads as a footnote to the turn, not a participant in it. */
.replay-injected {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
}

.replay-injected__toggle {
  display: inline-flex;
  align-items: baseline;
  gap: var(--space-2, 8px);
  appearance: none;
  border: 1px dashed var(--color-border, #e0e0e6);
  border-radius: var(--radius-full, 999px);
  background: none;
  padding: 1px 10px;
  font-size: 11px;
  cursor: pointer;
}

.replay-injected__toggle:focus-visible {
  outline: 2px solid var(--color-accent, #6467f2);
  outline-offset: 2px;
}

.replay-injected__label {
  font-weight: 600;
  color: var(--color-text-muted);
  font-variant-numeric: tabular-nums;
}

.replay-injected__state {
  color: var(--color-accent, #6467f2);
}

.replay-injected__toggle:hover .replay-injected__state {
  text-decoration: underline;
}

.replay-injected__body {
  display: flex;
  flex-direction: column;
  gap: var(--space-1, 4px);
  width: 100%;
  min-width: 0;
  margin-top: var(--space-1, 4px);
}

/* Each injected section verbatim: mono, capped, scrollable — the payloads
   are XML-ish wrapper walls, shown exactly as captured for inspection. */
.replay-injected__text {
  margin: 0;
  padding: var(--space-2, 8px) var(--space-3, 12px);
  border: 1px solid var(--color-border-subtle, #eee);
  border-radius: var(--radius-sm, 6px);
  background: var(--color-bg-soft, #f2f3f5);
  max-height: 320px;
  overflow: auto;
  font-family: var(--font-mono, monospace);
  font-size: var(--text-xs, 12px);
  line-height: 1.55;
  white-space: pre-wrap;
  word-break: break-word;
  color: var(--color-text-secondary);
}

/* Paging boundaries at the loaded window's edges. Idle they render as a
   slim hit-area only (a session that fits one page never mounts one at
   all); busy or failed they carry the loading copy / the retry row. */
.replay-pager {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: var(--space-2, 8px);
  min-height: 32px;
  margin-bottom: var(--space-3, 12px);
}

.replay-pager__text {
  font-size: 12px;
  color: var(--color-text-muted);
}

.replay-pager__text--error {
  color: var(--color-error-text, #d03050);
}

/* The system group's on-expand rows (loading / retry) sit inside the
   group's own padding. */
.replay-pager__row--inside {
  min-height: 24px;
  margin: var(--space-2, 8px) var(--space-3, 12px);
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
