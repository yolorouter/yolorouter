<!-- frontend/src/views/playground/PlaygroundPage.vue

     The model comparison workbench: one prompt, up to four models, the real
     gateway underneath. The page is two panes — the bench (left, fixed
     width, sticky) carries the whole run setup (the modality segment, the
     operator's own API key, the model multi-select, the prompt, the shared
     parameter area, the speech run's per-column voices) with the run
     button pinned to the bench's foot; the canvas (right, the remaining
     width) renders one column per selected model — output, cost, latency,
     usage, or the column's native error shape — and, before any run, the
     four-step guide. Narrow viewports stack the panes (bench above
     canvas) rather than floating anything.

     Single-round semantics: the page keeps no run history. A re-run
     replaces the previous columns wholesale (the request log under the
     run's own source remains the durable record — every column carries
     its audit row's request id for exactly that cross-reference).

     This is the shell: columns render per-column terminal results. The
     text modality streams its answers chunk by chunk; the image modality
     runs text-to-image columns that answer once (rendered as image cards,
     with the truncation note when the channel cut an over-cap answer);
     the audio modality runs text-to-speech columns whose voices are the
     run's one per-column parameter (each column's dropdown offers only
     its own model's vendors' voices) and whose answers render as audio
     players fed by the console's owner-pinned playback route; the video
     modality submits asynchronous tasks — a column shows its render
     clock while the backend bridges the task polling, may stop WAITING
     mid-render (the task keeps running at full cost, its bill lands in
     the request log when completion is next observed), and renders the
     finished clip through the console's owner-pinned proxy on done. -->
<template>
  <div class="common-page">
    <PageHeader :title="t('nav.playground')" :description="t('playground.pageDescription')" />

    <!-- The two-pane workbench. The layout containers are addressed by
         their convention classes (.pg-workbench/.pg-bench/.pg-canvas) —
         the batch's e2e contract for the split itself. -->
    <div class="pg-workbench">
      <!-- The bench: every control that shapes a run, vertically compact,
           in a sticky left pane whose foot carries the run controls — the
           button never scrolls out of view however long the setup grows. -->
      <section class="pg-bench">
        <!-- The modality segment filters the model picker to the models that
             serve that modality and swaps the parameter row to the axes that
             modality's dialect declares. -->
        <NTabs :value="modality" type="segment" size="small" class="pg-modality-tabs" @update:value="onModalityUpdate">
          <NTabPane name="text" :tab="t('models.outputModalityText')" />
          <NTabPane name="image" :tab="t('models.outputModalityImage')" />
          <NTabPane name="audio" :tab="t('models.outputModalityAudio')" />
          <NTabPane name="video" :tab="t('models.outputModalityVideo')" />
        </NTabs>

        <NForm require-mark-placement="left" class="pg-bench__body">
          <NFormItem>
            <template #label>
              <HelpLabel :tip="t('playground.apiKey_tip')">{{ t('playground.apiKey') }}</HelpLabel>
            </template>
            <NSelect
              v-model:value="keyId"
              class="pg-key-select"
              :options="keyOptions"
              :placeholder="keys.length === 0 ? t('playground.apiKeyEmpty') : ''"
            />
          </NFormItem>

          <NFormItem>
            <template #label>
              <HelpLabel :tip="t('playground.modelSelect_tip')">{{ t('playground.modelSelect') }}</HelpLabel>
            </template>
            <div class="pg-models">
              <NSelect
                class="pg-models-select"
                multiple
                clearable
                :value="selectedModels"
                :options="modelOptions"
                @update:value="onModelsUpdate"
              />
              <span class="pg-models-count">{{ t('playground.modelCount', { count: selectedModels.length, max: MAX_MODELS }) }}</span>
            </div>
          </NFormItem>

          <NFormItem>
            <template #label>
              <HelpLabel :tip="t('playground.prompt_tip')">{{ t('playground.prompt') }}</HelpLabel>
            </template>
            <div class="pg-prompt-block">
              <NInput v-model:value="prompt" class="pg-prompt" type="textarea" :rows="3" />
              <!-- The vision attachment travels with the user turn, in the
                   dialect's own encoding (a base64 data URL); it is re-read
                   per send, so a re-run can swap or drop it freely. It
                   belongs to the text modality alone — an image run's input
                   is the prompt itself. -->
              <div v-if="modality === 'text'" class="pg-attach">
                <input
                  ref="imageInput"
                  type="file"
                  accept="image/png,image/jpeg,image/webp,image/gif"
                  class="pg-attach-input"
                  data-testid="pg-attach-input"
                  @change="onImagePicked"
                />
                <NButton size="small" :disabled="running" data-testid="pg-attach-button" @click="pickImage">
                  {{ t('playground.attachImage') }}
                </NButton>
                <template v-if="attachedImage">
                  <img :src="attachedImage.dataUrl" class="pg-attach-thumb" alt="" data-testid="pg-attach-thumb" />
                  <span class="pg-attach-name">{{ attachedImage.name }}</span>
                  <NButton size="tiny" quaternary data-testid="pg-attach-remove" @click="clearImage">
                    {{ t('playground.removeImage') }}
                  </NButton>
                </template>
              </div>
            </div>
          </NFormItem>

          <!-- The shared parameter row: whatever is set here is applied to
               EVERY selected column of the run — a comparison varies the
               model, not the sampling. Only parameters the gateway dialects
               actually declare are exposed: the text family below, the
               image family's two axes (size and count), or the audio
               family's one axis (the response format). The VOICE is not
               here by design — it is the run's one per-column exception,
               and each column gets its own control below. -->
          <div v-if="modality === 'text'" class="pg-params">
            <NFormItem>
              <template #label>
                <HelpLabel :tip="t('playground.systemPrompt_tip')">{{ t('playground.systemPrompt') }}</HelpLabel>
              </template>
              <NInput v-model:value="systemPrompt" class="pg-param-system" type="textarea" :rows="2" />
            </NFormItem>
            <NFormItem>
              <template #label>
                <HelpLabel :tip="t('playground.temperature_tip')">{{ t('playground.temperature') }}</HelpLabel>
              </template>
              <NInputNumber v-model:value="temperature" class="pg-param-number" :min="0" :max="2" :step="0.1" />
            </NFormItem>
            <NFormItem>
              <template #label>
                <HelpLabel :tip="t('playground.maxTokens_tip')">{{ t('playground.maxTokens') }}</HelpLabel>
              </template>
              <NInputNumber v-model:value="maxTokens" class="pg-param-number" :min="1" :max="100000" :precision="0" />
            </NFormItem>
          </div>
          <div v-else-if="modality === 'image'" class="pg-params" data-testid="pg-image-params">
            <NFormItem>
              <template #label>
                <HelpLabel :tip="t('playground.imageSize_tip')">{{ t('playground.imageSize') }}</HelpLabel>
              </template>
              <NSelect
                v-model:value="imageSize"
                class="pg-image-size"
                filterable
                tag
                :options="imageSizeOptions"
              />
            </NFormItem>
            <NFormItem>
              <template #label>
                <HelpLabel :tip="t('playground.imageCount_tip')">{{ t('playground.imageCount') }}</HelpLabel>
              </template>
              <NInputNumber v-model:value="imageCount" class="pg-param-number" :min="1" :max="10" :precision="0" data-testid="pg-image-count" />
            </NFormItem>
          </div>
          <div v-else-if="modality === 'audio'" class="pg-params" data-testid="pg-audio-params">
            <NFormItem>
              <template #label>
                <HelpLabel :tip="t('playground.audioFormat_tip')">{{ t('playground.audioFormat') }}</HelpLabel>
              </template>
              <NSelect v-model:value="audioFormat" class="pg-audio-format" clearable :options="audioFormatOptions" data-testid="pg-audio-format" />
            </NFormItem>
          </div>
          <div v-else-if="modality === 'video'" class="pg-params" data-testid="pg-video-params">
            <NFormItem>
              <template #label>
                <HelpLabel :tip="t('playground.videoSeconds_tip')">{{ t('playground.videoSeconds') }}</HelpLabel>
              </template>
              <NSelect v-model:value="videoSeconds" class="pg-video-seconds" clearable :options="videoSecondsOptions" data-testid="pg-video-seconds" />
            </NFormItem>
            <NFormItem>
              <template #label>
                <HelpLabel :tip="t('playground.videoSize_tip')">{{ t('playground.videoSize') }}</HelpLabel>
              </template>
              <NSelect v-model:value="videoSize" class="pg-video-size" clearable :options="videoSizeOptions" data-testid="pg-video-size" />
            </NFormItem>
          </div>

          <!-- The speech run's per-column voices — the ONE parameter that
               is not shared. A voice is a vendor-private id: sending one
               shared value would 400 every column of a mixed-vendor
               comparison, so each selected model gets its own control
               offering only that model's vendors' voices (taggable — a
               vendor's cloned voice id is legal and unlisted, and the
               upstream's own 400 is the only legality verdict). -->
          <div v-if="modality === 'audio' && selectedModels.length > 0" class="pg-voices" data-testid="pg-voices">
            <NFormItem v-for="name in selectedModels" :key="name">
              <template #label>
                <HelpLabel :tip="t('playground.voice_tip')">{{ t('playground.voiceLabel') }} · {{ name }}</HelpLabel>
              </template>
              <NSelect
                :value="voiceOf(name)"
                class="pg-voice-select"
                filterable
                tag
                :options="voiceOptionsFor(name)"
                :data-testid="`pg-voice-${name}`"
                @update:value="(v: string) => setVoice(name, v)"
              />
            </NFormItem>
          </div>

        </NForm>

        <!-- The bench's pinned foot: the run controls stay in view while
             the setup above them scrolls — the form body owns the scroll,
             the foot never leaves the screen. -->
        <div class="pg-actions">
          <NButton type="primary" :loading="running" @click="onSend">{{ sendLabel }}</NButton>
          <!-- Stopping disconnects the aggregate channel: the backend
               propagates the disconnect into the in-flight columns, whose
               costs still settle into the request log. -->
          <NButton v-if="running" class="pg-stop" data-testid="pg-stop" @click="onStop">
            {{ t('playground.stop') }}
          </NButton>
        </div>
      </section>

      <!-- The canvas: the results pane — the page's main character. Empty
           runs see the four-step guide; a run renders its columns. -->
      <section class="pg-canvas">
        <!-- The grid's :key is the run counter, so a re-run remounts the
             column set instead of patching the old one — old results are
             replaced, never merged into the new run. -->
        <section v-if="columns.length > 0" :key="runId" class="pg-grid" data-testid="pg-columns">
          <article v-for="(col, i) in columns" :key="i" class="pg-column" :class="`pg-column--${col.state}`">
            <header class="pg-column__head">
              <span class="pg-column__model">{{ col.model }}</span>
              <NTag size="small" :type="col.state === 'done' ? 'success' : col.state === 'error' ? 'error' : col.state === 'cancelled' || col.state === 'stopped' ? 'warning' : 'info'">
                {{ stateLabel(col.state) }}
              </NTag>
            </header>

            <!-- A finished column shows what its modality delivered: streamed
                 text, an image answer's cards, or — when the channel cut an
                 over-cap answer — the truncation note over the capped prefix.
                 Streaming columns show the text as it grows, chunk by chunk:
                 the first column to answer is the first column on screen,
                 never held back by a slower neighbour. -->
            <template v-if="col.state === 'streaming' || col.state === 'done'">
              <div v-if="col.state === 'done' && imageArtifacts(col).length > 0" class="pg-images">
                <a
                  v-for="(img, k) in imageArtifacts(col)"
                  :key="k"
                  :href="img.src"
                  target="_blank"
                  rel="noopener noreferrer"
                >
                  <img :src="img.src" :alt="img.alt" class="pg-image" data-testid="pg-image-card" />
                </a>
              </div>
              <!-- A finished speech column plays its delivered clip: the done
                   event's reference points at the console's owner-pinned
                   playback URL, and the native audio element streams it
                   same-origin with the session cookie. -->
              <div v-else-if="col.state === 'done' && col.audio" class="pg-audio" data-testid="pg-audio-player">
                <audio controls :src="col.audio.url" :type="col.audio.content_type" preload="none" />
              </div>
              <!-- A finished video column plays its delivered clip the same
                   way: the reference points at the console's owner-pinned
                   proxy of the upstream's result URL. -->
              <div v-else-if="col.state === 'done' && col.video" class="pg-video" data-testid="pg-video-card">
                <video controls :src="col.video.url" :type="col.video.content_type" preload="metadata" />
              </div>
              <div
                v-else-if="col.state === 'done' && col.responseTruncated"
                class="pg-column__truncated"
                data-testid="pg-column-truncated"
              >
                <div class="pg-column__truncated-note">{{ t('playground.responseTruncated') }}</div>
                <pre class="pg-column__truncated-body">{{ col.response }}</pre>
              </div>
              <pre v-else class="pg-column__output" data-testid="pg-column-output">{{ col.output }}</pre>
              <dl v-if="col.state === 'done'" class="pg-metrics">
                <div class="pg-metric">
                  <dt>{{ t('playground.costLabel') }}</dt>
                  <dd>
                    {{ col.costKnown ? `${formatMicros(col.costMicros)} ${t('playground.currencyUnit')}` : t('playground.costUnknown') }}
                  </dd>
                </div>
                <div class="pg-metric">
                  <dt>{{ t('playground.latencyLabel') }}</dt>
                  <dd>{{ formatDuration(col.durationMs) }}</dd>
                </div>
                <div class="pg-metric">
                  <dt>{{ t('playground.usageLabel') }}</dt>
                  <dd>
                    <template v-if="(col.usage?.seconds ?? 0) > 0">
                      {{ t('playground.usageSeconds', { count: col.usage?.seconds ?? 0 }) }}
                    </template>
                    <template v-else-if="(col.usage?.characters ?? 0) > 0">
                      {{ t('playground.usageCharacters', { count: col.usage?.characters ?? 0 }) }}
                    </template>
                    <template v-else-if="(col.usage?.images ?? 0) > 0">
                      {{ t('playground.usageImages', { count: col.usage?.images ?? 0 }) }}
                    </template>
                    <template v-else>
                      {{ t('playground.usageTokens', { input: col.usage?.input_tokens ?? 0, output: col.usage?.output_tokens ?? 0 }) }}
                    </template>
                  </dd>
                </div>
              </dl>
            </template>

            <!-- A running VIDEO column shows the elapsed render clock (the
                 only thing the page can honestly promise about a task whose
                 cost is decided late) and, once its submit is acknowledged,
                 the stop-waiting escape hatch: stopping the wait stops THIS
                 PAGE's polling — the task itself keeps running at full cost. -->
            <div v-else-if="col.state === 'running' && modality === 'video'" class="pg-column__state pg-video-running">
              <span data-testid="pg-video-timer">{{ t('playground.columnStateRunning') }} · {{ elapsedText() }}</span>
              <NButton
                v-if="col.requestId"
                size="tiny"
                class="pg-stop-wait"
                :data-testid="`pg-stop-wait-${slotOf(col)}`"
                @click="onStopWait(col)"
              >
                {{ t('playground.stopWaiting') }}
              </NButton>
            </div>
            <div v-else-if="col.state === 'running'" class="pg-column__state">{{ t('playground.columnStateRunning') }}</div>

            <!-- A cancelled column: the run was stopped from this page; what
                 it had already cost is in the request log under its id. -->
            <div v-else-if="col.state === 'cancelled'" class="pg-column__state" data-testid="pg-column-cancelled">
              {{ t('playground.columnCancelledNote') }}
            </div>

            <!-- A stopped-waiting VIDEO column: the honest reading. The task
                 was NOT cancelled — it keeps rendering at full cost, and the
                 bill lands in the request log when completion is next
                 observed. The page promises no live cost figure for it. -->
            <div v-else-if="col.state === 'stopped'" class="pg-column__state" data-testid="pg-column-stopped">
              {{ t('playground.columnStoppedNote') }}
            </div>

            <!-- A failed column shows the failure in its native dialect shape:
                 the status the relay produced and the error object the gateway
                 answered, verbatim — quota, rate-limit and upstream failures
                 read the way the API reports them. -->
            <div v-else class="pg-column__error">
              <div class="pg-column__error-status">HTTP {{ col.errorStatus }}</div>
              <div class="pg-column__error-message">{{ col.errorMessage }}</div>
              <div v-if="col.errorType" class="pg-column__error-type">{{ t('playground.errorTypeLabel') }}: {{ col.errorType }}</div>
            </div>

            <footer v-if="col.requestId" class="pg-column__request">
              {{ t('playground.requestLabel') }}: {{ col.requestId }}
            </footer>
          </article>
        </section>

        <!-- The empty canvas: the shared EmptyState with the four-step guide
             riding its #detail slot — no bespoke empty styling. The steps
             mirror the bench top-to-bottom, so the guide reads as a walkthrough
             of the pane beside it. -->
        <EmptyState
          v-else
          :icon="FlaskConical"
          :title="t('playground.emptyTitle')"
          :description="t('playground.emptyDescription')"
        >
          <template #detail>
            <ol class="pg-empty-steps">
              <li>{{ t('playground.emptyStepKey') }}</li>
              <li>{{ t('playground.emptyStepModels') }}</li>
              <li>{{ t('playground.emptyStepPrompt') }}</li>
              <li>{{ t('playground.emptyStepRun') }}</li>
            </ol>
          </template>
        </EmptyState>
      </section>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useMessage } from 'naive-ui'
import { FlaskConical } from '@lucide/vue'
import PageHeader from '../../components/PageHeader.vue'
import HelpLabel from '../../components/HelpLabel.vue'
import EmptyState from '../../components/EmptyState.vue'
import { displayMessage, RequestAbortedError } from '../../api/client'
import {
  listPlaygroundKeys,
  listPlaygroundModels,
  runPlayground,
  stopPlaygroundVideoWait,
  type PlaygroundContentPart,
  type PlaygroundKeyOption,
  type PlaygroundModelOption,
} from '../../api/playground'
import { foldEvent, initialColumns, type DemuxColumn, type DemuxColumnState } from '../../api/playgroundDemux'
import { formatMicros } from '../../utils/money'
import { formatDuration } from '../../utils/format'
import { maxAttachableImageBytes } from './attachLimit'

const { t } = useI18n()
const message = useMessage()

// The comparison width: one run holds at most four columns, which is the
// run contract's own bound — the picker refuses a fifth up front rather
// than letting the backend reject the whole run after the click.
const MAX_MODELS = 4

const modality = ref<'text' | 'image' | 'audio' | 'video'>('text')

const keys = ref<PlaygroundKeyOption[]>([])
const keyId = ref<number | null>(null)
const models = ref<PlaygroundModelOption[]>([])
const selectedModels = ref<string[]>([])
const prompt = ref('')
const systemPrompt = ref('')
const temperature = ref<number | null>(null)
const maxTokens = ref<number | null>(null)

// The image modality's two axes — the request fields the images dialect
// itself declares. The size vocabulary offers the ecosystem's common
// spellings as presets and accepts any custom axis string (taggable
// select): a model's tiers decide what its upstream honours, and the
// console refuses to invent values on the dialect's behalf.
const imageSize = ref('1024x1024')
const imageCount = ref(1)
const imageSizeOptions = ['512x512', '768x768', '1024x1024', '1024x1536', '1536x1024'].map((s) => ({
  label: s,
  value: s,
}))

// The audio modality's shared axis — the response format, in the speech
// dialect's own canonical vocabulary. Unset (cleared) leaves each
// column's dialect default in force, which is the safest shared value
// across a mixed selection (each vendor's default is one it serves).
const audioFormat = ref<string | null>(null)
const audioFormatOptions = ['mp3', 'opus', 'aac', 'flac', 'wav', 'pcm'].map((f) => ({ label: f, value: f }))

// The video modality's two shared axes, in the videos dialect's own
// vocabulary. Unset (cleared) leaves the dialect's own defaults in force
// upstream — the page refuses to invent values on the dialect's behalf.
const videoSeconds = ref<number | null>(null)
const videoSecondsOptions = [4, 8, 12].map((s) => ({ label: String(s), value: s }))
const videoSize = ref<string | null>(null)
const videoSizeOptions = ['720x1280', '1280x720', '1024x1792', '1792x1024'].map((s) => ({ label: s, value: s }))

// The video run's elapsed-render clock. A video column's only honest
// in-progress facts are "still rendering" and how long it has been — its
// cost is decided when completion is observed, so the page counts seconds
// instead of promising figures. One clock serves the whole run: every
// column of a run submits at the same click.
const runStartedAt = ref(0)
const nowMs = ref(0)
let elapsedTimer: ReturnType<typeof setInterval> | null = null
function elapsedText(): string {
  const elapsed = Math.max(0, Math.floor((nowMs.value - runStartedAt.value) / 1000))
  const m = Math.floor(elapsed / 60)
  const s = elapsed % 60
  return `${String(m).padStart(2, '0')}:${String(s).padStart(2, '0')}`
}
function startElapsedClock() {
  runStartedAt.value = Date.now()
  nowMs.value = runStartedAt.value
  stopElapsedClock()
  elapsedTimer = setInterval(() => {
    nowMs.value = Date.now()
  }, 1000)
}
function stopElapsedClock() {
  if (elapsedTimer !== null) {
    clearInterval(elapsedTimer)
    elapsedTimer = null
  }
}

// onStopWait is a running video column's escape hatch: stop THIS page's
// bridged polling of that column. The task is not terminated — it keeps
// rendering at full cost and settles into the request log later — which
// is exactly what the column then renders. The wait_stopped event on the
// run channel is the authoritative confirmation; a 404 means the wait
// already ended (the terminal event is on the stream either way).
async function onStopWait(col: DemuxColumn) {
  if (!col.requestId || col.state !== 'running') return
  try {
    await stopPlaygroundVideoWait(col.requestId)
  } catch (err) {
    message.error(displayMessage(err, t))
  }
}

// slotOf finds a column's slot index — the stop-waiting button's testid
// anchor (per-column controls need per-column addresses).
function slotOf(col: DemuxColumn): number {
  return columns.value.indexOf(col)
}

// The speech run's per-column voices: model name → the voice picked for
// that column. The picker row carries each model's vendors' preset
// vocabulary; a column with no listed presets still accepts a custom id
// (cloned voices are legal and unlisted). The first preset is the default
// so an immediate run never trips the voice gate.
const voicesByModel = ref<Record<string, string>>({})
function modelByName(name: string): PlaygroundModelOption | undefined {
  return models.value.find((m) => m.name === name)
}
function voiceOptionsFor(name: string): Array<{ label: string; value: string }> {
  return (modelByName(name)?.voices ?? []).map((v) => ({ label: v, value: v }))
}
function voiceOf(name: string): string {
  const picked = voicesByModel.value[name]
  if (picked) return picked
  return modelByName(name)?.voices?.[0] ?? ''
}
function setVoice(name: string, value: string) {
  voicesByModel.value = { ...voicesByModel.value, [name]: value }
}

// The vision attachment of the next run: name for display, data URL for
// the wire. Re-read on every pick, so a re-run can swap or drop it.
interface AttachedImage {
  name: string
  dataUrl: string
}
const attachedImage = ref<AttachedImage | null>(null)
const imageInput = ref<HTMLInputElement | null>(null)

const running = ref(false)
const runId = ref(0)
// The in-flight run's disconnect switch. Aborting it closes the aggregate
// channel; the backend propagates the disconnect into the in-flight
// columns.
let runController: AbortController | null = null
let sawStreamEvent = false

const columns = ref<DemuxColumn[]>([])

// The key picker's rows. The list is the operator's own usable keys
// server-side (pinned there for both roles — the run endpoint accepts a
// key only from its owner), so every row here is a row a run accepts.
const keyOptions = computed(() =>
  keys.value.map((k) => ({
    label: k.remark ? `${k.key_prefix}… · ${k.remark}` : `${k.key_prefix}…`,
    value: k.id,
  })),
)

// The model picker's rows, filtered to the active modality tab: a model
// is offered only when it declares the modality's output. At the cap the
// unselected rows grey out, so the four-column bound is visible in the
// dropdown itself and not only in a refusal toast.
const modelOptions = computed(() =>
  models.value
    .filter((m) => m.output_modalities.includes(modality.value))
    .map((m) => ({
      label: m.name,
      value: m.name,
      disabled: selectedModels.value.length >= MAX_MODELS && !selectedModels.value.includes(m.name),
    })),
)

function onModelsUpdate(value: string[]) {
  if (value.length > MAX_MODELS) {
    // Refuse the over-cap selection outright: keep the previous value.
    message.warning(t('playground.modelCapReached'))
    return
  }
  selectedModels.value = value
  // Voice picks follow the selection: a deselected model's pick cannot
  // reach the wire (the payload rebuilds the map from the current
  // selection), and dropping it here keeps the control's own state
  // honest for a later re-select.
  const kept: Record<string, string> = {}
  for (const name of value) {
    if (voicesByModel.value[name]) kept[name] = voicesByModel.value[name]
  }
  voicesByModel.value = kept
}

// Switching the modality tab empties the selection: a model chosen under
// one tab does not serve the next, and a cross-modality selection is not
// a comparison the run endpoint could honestly execute.
function onModalityUpdate(name: string | number) {
  const next = String(name) as typeof modality.value
  if (next === modality.value) return
  modality.value = next
  selectedModels.value = []
  // Voice picks belong to speech models; leaving the tab drops them with
  // the selection (a stale pick for a model that is no longer selected
  // would ride along in the payload's map otherwise — harmless to the
  // backend, but not the page's contract with itself).
  voicesByModel.value = {}
}

// A finished media column's renderable images: the artifacts the delivered
// response carried, as src/alt pairs — a remote reference as-is, inline
// base64 pixels wrapped into the data URL an <img> consumes. An answer
// without artifacts (or a truncated one, whose bytes are a cut prefix)
// answers empty and the column falls through to its other renderings.
function imageArtifacts(col: DemuxColumn): Array<{ src: string; alt: string }> {
  if (col.state !== 'done' || col.response === null || typeof col.response !== 'object') return []
  const out: Array<{ src: string; alt: string }> = []
  for (const d of col.response.data ?? []) {
    const src = d.url ? d.url : d.b64_json ? `data:image/png;base64,${d.b64_json}` : ''
    if (!src) continue
    out.push({ src, alt: d.revised_prompt || col.model })
  }
  return out
}

const sendLabel = computed(() => {
  if (running.value) return t('playground.running')
  return columns.value.length > 0 ? t('playground.resend') : t('playground.send')
})

function stateLabel(state: DemuxColumnState): string {
  if (state === 'done') return t('playground.columnStateDone')
  if (state === 'error') return t('playground.columnStateFailed')
  if (state === 'streaming') return t('playground.columnStateStreaming')
  if (state === 'cancelled') return t('playground.columnStateCancelled')
  if (state === 'stopped') return t('playground.columnStateStopped')
  return t('playground.columnStateRunning')
}

function pickImage() {
  imageInput.value?.click()
}

function clearImage() {
  attachedImage.value = null
  if (imageInput.value) imageInput.value.value = ''
}

function onImagePicked(event: Event) {
  const input = event.target as HTMLInputElement
  const file = input.files?.[0]
  if (!file) return
  // The cap is the exact mirror of the backend's per-image data-URL limit
  // (attachLimit.ts carries the computation): every file accepted here
  // produces a data URL the run endpoint accepts, so the console never
  // posts a run the endpoint would refuse after the click.
  if (file.size > maxAttachableImageBytes) {
    message.warning(t('playground.imageTooLarge'))
    input.value = ''
    return
  }
  const reader = new FileReader()
  reader.onload = () => {
    attachedImage.value = { name: file.name, dataUrl: String(reader.result) }
  }
  reader.readAsDataURL(file)
}

onMounted(() => {
  void loadPickers()
})

// Leaving the page mid-run is a disconnect like any other.
onBeforeUnmount(() => {
  runController?.abort()
  stopElapsedClock()
})

async function loadPickers() {
  try {
    const [keyPage, modelPage] = await Promise.all([listPlaygroundKeys(), listPlaygroundModels()])
    keys.value = keyPage.list
    models.value = modelPage.list
  } catch (err) {
    message.error(displayMessage(err, t))
  }
}

// The user turn in its wire shape: plain text, or a vision turn carrying
// the prompt and the attachment in the dialect's own encoding.
function userContent(): string | PlaygroundContentPart[] {
  if (!attachedImage.value) return prompt.value
  return [
    { type: 'text', text: prompt.value },
    { type: 'image_url', image_url: { url: attachedImage.value.dataUrl } },
  ]
}

async function onSend() {
  if (running.value) return
  if (keyId.value == null) {
    message.warning(t('playground.needKey'))
    return
  }
  if (selectedModels.value.length === 0) {
    message.warning(t('playground.needModel'))
    return
  }
  if (!prompt.value.trim()) {
    message.warning(t('playground.needPrompt'))
    return
  }
  // A speech column without a voice cannot run (the dialect requires
  // one); the page refuses up front rather than letting the endpoint 400
  // the whole run.
  if (modality.value === 'audio' && selectedModels.value.some((name) => !voiceOf(name))) {
    message.warning(t('playground.needVoice'))
    return
  }

  // A (re)run starts from a fresh column set built from the CURRENT
  // selection — the previous run's columns are replaced, never appended
  // to, so stale results cannot masquerade as part of the new comparison.
  const runModels = selectedModels.value.slice()
  columns.value = initialColumns(runModels)
  runId.value++
  running.value = true
  sawStreamEvent = false
  runController = new AbortController()
  // A video run counts its render seconds from the send click — the one
  // in-progress fact the page can honestly show for a late-billed task.
  if (modality.value === 'video') startElapsedClock()
  // The run's body is the active modality's own shape: a text run sends
  // the user turn (with its optional vision attachment) and the sampling
  // family; an image run sends the prompt and the two image axes; an
  // audio run sends the prompt, the shared format, and one voice PER
  // COLUMN. The other families' controls are not merely ignored — they
  // are not on the wire at all, which is what the upstream whitelist
  // asserts.
  // The modality's own body rides nested under `request` — the endpoint's
  // contract (its pre-stream validation reads request.messages /
  // request.prompt), not a flat top-level field.
  const input =
    modality.value === 'image'
      ? {
          key_id: keyId.value,
          modality: 'image' as const,
          models: runModels,
          request: { prompt: prompt.value },
          params: {
            size: imageSize.value || undefined,
            n: imageCount.value ?? undefined,
          },
        }
      : modality.value === 'audio'
        ? {
            key_id: keyId.value,
            modality: 'audio' as const,
            models: runModels,
            request: { prompt: prompt.value },
            voices: Object.fromEntries(runModels.map((name) => [name, voiceOf(name)])),
            params: {
              format: audioFormat.value || undefined,
            },
          }
        : modality.value === 'video'
          ? {
              key_id: keyId.value,
              modality: 'video' as const,
              models: runModels,
              request: { prompt: prompt.value },
              params: {
                seconds: videoSeconds.value ?? undefined,
                size: videoSize.value || undefined,
              },
            }
          : {
            key_id: keyId.value,
            modality: 'text' as const,
            models: runModels,
            request: { messages: [{ role: 'user' as const, content: userContent() }] },
            params: {
              system: systemPrompt.value || undefined,
              temperature: temperature.value ?? undefined,
              max_tokens: maxTokens.value ?? undefined,
            },
          }
  try {
    await runPlayground(input, applyEvent, { signal: runController.signal })
  } catch (err) {
    if (err instanceof RequestAbortedError) {
      // The operator stopped the run (or left the page): the columns that
      // never reached a terminal state are stopped, not failed — what
      // they cost is in the request log under each column's id. A video
      // run's reading splits by how far each column got (see
      // markUnfinished): past its submit acknowledgement, the
      // stopped-WAITING note — the task was never cancellable and keeps
      // running at full cost; still inside the submit exchange, the
      // plain cancelled note — the exchange was cancelled with the run,
      // no task exists to bill.
      markUnfinished(modality.value === 'video' ? 'stopped' : 'cancelled')
    } else if (!sawStreamEvent) {
      // Pre-stream gate failures (key ownership/state, bind errors) answer
      // with the console envelope: no column ever started, so the
      // placeholder columns come back off and the message names the
      // failure.
      columns.value = []
      message.error(displayMessage(err, t))
    } else {
      // The channel broke mid-run: finished columns keep their results;
      // the ones still in flight are marked as errored on the spot.
      message.error(displayMessage(err, t))
      markUnfinished('error', displayMessage(err, t))
    }
  } finally {
    running.value = false
    runController = null
    stopElapsedClock()
  }
}

function onStop() {
  runController?.abort()
}

// markUnfinished settles columns that never received a terminal event —
// the local half of a disconnect the backend acknowledges best-effort.
// 'stopped' is the video run's reading of the same disconnect, and it
// splits BY COLUMN: a requestId on the column means its submit was
// acknowledged — a task nobody can terminate keeps running at full cost,
// which is what the stopped-waiting note honestly says. No requestId
// means the column was still inside its submit exchange when the run
// stopped, and the shared cancel choreography cancelled the exchange
// itself — no task exists, nothing will bill — so the honest reading is
// the plain cancelled note, never a full-cost promise about a task that
// never was.
function markUnfinished(state: 'cancelled' | 'stopped' | 'error', errorMessage = '') {
  for (const col of columns.value) {
    if (col.state === 'running' || col.state === 'streaming') {
      if (state === 'stopped' && !col.requestId) {
        col.state = 'cancelled'
      } else {
        col.state = state
      }
      if (state === 'error') col.errorMessage = errorMessage
    }
  }
}

// applyEvent folds one aggregate-channel event into its column through
// the demux pure function — slot routing, per-column terminal states,
// stray-event tolerance all live there and are tested there.
function applyEvent(ev: Parameters<typeof foldEvent>[1]) {
  sawStreamEvent = true
  foldEvent(columns.value, ev)
}
</script>

<style scoped lang="less">
/* The two-pane workbench: bench (settings) beside canvas (results). The
   containers are the batch's e2e addressing contract — .pg-workbench /
   .pg-bench / .pg-canvas — so the split itself is assertable. */
.pg-workbench {
  display: flex;
  align-items: flex-start;
  gap: var(--space-5);
}

/* The bench: a fixed-width sticky card whose body scrolls on its own, so
   the run controls pinned to its foot stay on screen however long the
   setup grows. The scroll container it sticks in is the layout's own
   .layout-content (100dvh, overflow auto), hence the viewport-relative
   max-height. */
.pg-bench {
  display: flex;
  flex-direction: column;
  position: sticky;
  top: var(--space-3);
  width: 360px;
  flex-shrink: 0;
  // 2 × --space-3 equals --space-6 on the spacing scale; written as the
  // single token so LESS never rewrites the calc arithmetic.
  max-height: calc(100dvh - var(--space-6));
  padding: var(--space-4) var(--space-5);
  background: var(--color-surface);
  border: 1px solid var(--color-border-subtle);
  border-radius: var(--radius-lg);
}

/* The scrollable settings body of the bench — everything above the pinned
   foot. Select menus portal to body, so the clip never hides them. */
.pg-bench__body {
  flex: 1 1 auto;
  min-height: 0;
  overflow-y: auto;
  overscroll-behavior: contain;
}

.pg-modality-tabs {
  margin-bottom: var(--space-2);
}

.pg-key-select {
  width: 100%;
}

.pg-models {
  display: flex;
  align-items: center;
  gap: var(--space-3);
  width: 100%;
}

.pg-models-select {
  flex: 1 1 auto;
  min-width: 0;
}

.pg-models-count {
  flex-shrink: 0;
  color: var(--color-text-muted);
  font-size: var(--text-sm);
  font-variant-numeric: tabular-nums;
}

.pg-prompt-block {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
  width: 100%;
}

.pg-prompt {
  width: 100%;
}

/* The vision attachment row under the prompt: a hidden native file input
   driven by a button, plus the chosen image's thumbnail/name and a way to
   drop it. */
.pg-attach {
  display: flex;
  align-items: center;
  gap: var(--space-2);
}

.pg-attach-input {
  display: none;
}

.pg-attach-thumb {
  width: 32px;
  height: 32px;
  object-fit: cover;
  border-radius: var(--radius-md);
  border: 1px solid var(--color-border-subtle);
}

.pg-attach-name {
  overflow: hidden;
  max-width: 220px;
  color: var(--color-text-muted);
  font-size: var(--text-sm);
  text-overflow: ellipsis;
  white-space: nowrap;
}

/* The shared parameter area: one axis per row — the bench is narrow, so
   the axes stack vertically instead of the old wide page's two-column
   grid. */
.pg-params {
  display: grid;
  grid-template-columns: minmax(0, 1fr);
  gap: 0;
}

/* The speech run's per-column voice controls: one compact row per
   selected model, each offering only that model's vendors' voices. */
.pg-voices {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
}

.pg-voice-select {
  width: 100%;
}

/* A finished speech column's player: the console playback URL in a
   native audio element, full column width. */
.pg-audio {
  width: 100%;
}

.pg-audio audio {
  width: 100%;
}

.pg-param-number {
  width: 100%;
}

/* The bench's pinned foot: outside the scrolling body, so the run
   controls stay visible through any scroll of the settings above. The
   primary run control takes the row's width — the one action the whole
   bench exists for. */
.pg-actions {
  display: flex;
  flex-shrink: 0;
  gap: var(--space-2);
  margin-top: var(--space-2);
  padding-top: var(--space-3);
  border-top: 1px solid var(--color-border-subtle);
}

.pg-actions > .n-button:first-child {
  flex: 1 1 auto;
}

/* The canvas: the results pane, taking every remaining pixel beside the
   bench. min-width: 0 lets wide columns shrink inside instead of pushing
   the layout wider. */
.pg-canvas {
  flex: 1 1 auto;
  min-width: 0;
}

/* Comparison columns: a responsive grid that settles at four abreast —
   one column per comparison slot — and reflows to fewer on narrower
   viewports, where the columns stay readable by stacking. */
.pg-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(260px, 1fr));
  gap: var(--space-4);
  align-items: start;
}

.pg-column {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  padding: var(--space-4);
  background: var(--color-surface);
  border: 1px solid var(--color-border-subtle);
  border-radius: var(--radius-lg);
}

.pg-column--error {
  border-color: var(--color-error, #d03050);
}

.pg-column--cancelled {
  border-color: var(--color-warning, #f0a020);
}

.pg-stop {
  margin-left: var(--space-2);
}

.pg-column__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-2);
}

.pg-column__model {
  overflow: hidden;
  font-weight: 600;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.pg-column__state {
  color: var(--color-text-muted);
  font-size: var(--text-sm);
}

.pg-column__output {
  max-height: 320px;
  overflow: auto;
  margin: 0;
  padding: var(--space-3);
  border-radius: var(--radius-md);
  background: var(--color-surface-hover);
  font-size: var(--text-sm);
  line-height: 1.6;
  white-space: pre-wrap;
  word-break: break-word;
}

/* An image answer's cards: one per delivered artifact, opening the
   reference in a new tab on click. */
.pg-images {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(120px, 1fr));
  gap: var(--space-2);
}

.pg-image {
  display: block;
  width: 100%;
  max-height: 220px;
  object-fit: cover;
  border-radius: var(--radius-md);
  border: 1px solid var(--color-border-subtle);
  background: var(--color-surface-hover);
}

/* A finished video column's player: the console proxy URL in a native
   video element, full column width. */
.pg-video {
  width: 100%;
}

.pg-video video {
  width: 100%;
  border-radius: var(--radius-md);
  background: #000;
}

/* The running video column's state row: the elapsed clock beside the
   stop-waiting escape hatch. */
.pg-video-running {
  display: flex;
  align-items: center;
  gap: var(--space-2);
}

/* A truncated media answer: the note explains the cut, the capped prefix
   stays inspectable beneath it — the answer is marked, not swallowed. */
.pg-column__truncated {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}

.pg-column__truncated-note {
  color: var(--color-warning, #f0a020);
  font-size: var(--text-sm);
}

.pg-column__truncated-body {
  max-height: 160px;
  overflow: auto;
  margin: 0;
  padding: var(--space-3);
  border-radius: var(--radius-md);
  background: var(--color-surface-hover);
  color: var(--color-text-muted);
  font-size: var(--text-xs);
  line-height: 1.5;
  white-space: pre-wrap;
  word-break: break-all;
}

.pg-metrics {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
  margin: 0;
  font-size: var(--text-sm);
}

.pg-metric {
  display: flex;
  justify-content: space-between;
  gap: var(--space-3);
}

.pg-metric dt {
  color: var(--color-text-muted);
}

.pg-metric dd {
  margin: 0;
  font-variant-numeric: tabular-nums;
}

.pg-column__error {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
  padding: var(--space-3);
  border-radius: var(--radius-md);
  background: var(--color-surface-hover);
  font-size: var(--text-sm);
}

.pg-column__error-status {
  font-weight: 600;
  font-variant-numeric: tabular-nums;
}

.pg-column__error-message {
  line-height: 1.6;
  word-break: break-word;
}

.pg-column__error-type {
  color: var(--color-text-muted);
}

.pg-column__request {
  overflow: hidden;
  color: var(--color-text-muted);
  font-size: var(--text-xs);
  text-overflow: ellipsis;
  white-space: nowrap;
}

/* The empty canvas's four-step guide: the shared EmptyState's #detail
   block owns the alignment; this only keeps the steps visually compact
   (native ordered numbering, one line per step). */
.pg-empty-steps {
  margin: 0;
  padding-left: var(--space-5);
  color: var(--color-text-muted);
  font-size: var(--text-sm);
  line-height: 1.8;
}

@media (max-width: @mobile-breakpoint) {
  /* Narrow viewports stack the two panes — bench above canvas — with the
     bench back in normal flow: no sticky, no internal scroll, the page
     scrolls as one column. */
  .pg-workbench {
    flex-direction: column;
    gap: var(--space-3);
  }

  .pg-bench {
    position: static;
    width: 100%;
    max-height: none;
  }

  .pg-bench__body {
    overflow: visible;
  }
}
</style>
