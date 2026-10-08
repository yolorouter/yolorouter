<!-- frontend/src/views/playground/PlaygroundPage.vue

     The model comparison bench: one prompt, up to four models, the real
     gateway underneath. The control bar at the top (modality tab, the
     operator's own API key, the model multi-select, the shared parameter
     row, the run button) submits a single run; the area below renders one
     column per selected model — output, cost, latency, usage, or the
     column's native error shape.

     Single-round semantics: the page keeps no run history. A re-run
     replaces the previous columns wholesale (the request log under the
     run's own source remains the durable record — every column carries
     its audit row's request id for exactly that cross-reference).

     This is the shell: columns render per-column terminal results, and
     only the text modality is wired. The remaining modality tabs stay
     visible but disabled until their dialects arrive. -->
<template>
  <div class="common-page">
    <PageHeader
      :eyebrow="t('nav.groupAnalytics')"
      :title="t('nav.playground')"
      :description="t('playground.pageDescription')"
    />

    <!-- Control bar -->
    <section class="pg-card pg-control">
      <!-- The modality tab filters the model picker to the models that
           serve that modality. Only text is selectable today; the other
           tabs are disabled placeholders rather than omitted, so the
           page's shape is stable while its dialects land. -->
      <NTabs :value="modality" type="line" size="small" class="pg-modality-tabs">
        <NTabPane name="text" :tab="t('models.outputModalityText')" />
        <NTabPane name="image" :tab="t('models.outputModalityImage')" disabled />
        <NTabPane name="audio" :tab="t('models.outputModalityAudio')" disabled />
        <NTabPane name="video" :tab="t('models.outputModalityVideo')" disabled />
      </NTabs>

      <NForm require-mark-placement="left">
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
                 per send, so a re-run can swap or drop it freely. -->
            <div class="pg-attach">
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
             actually accept are exposed. -->
        <div class="pg-params">
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

        <div class="pg-actions">
          <NButton type="primary" :loading="running" @click="onSend">{{ sendLabel }}</NButton>
          <!-- Stopping disconnects the aggregate channel: the backend
               propagates the disconnect into the in-flight columns, whose
               costs still settle into the request log. -->
          <NButton v-if="running" class="pg-stop" data-testid="pg-stop" @click="onStop">
            {{ t('playground.stop') }}
          </NButton>
        </div>
      </NForm>
    </section>

    <!-- Comparison columns. :key on the grid is the run counter, so a
         re-run remounts the column set instead of patching the old one —
         old results are replaced, never merged into the new run. -->
    <section v-if="columns.length > 0" :key="runId" class="pg-grid" data-testid="pg-columns">
      <article v-for="(col, i) in columns" :key="i" class="pg-column" :class="`pg-column--${col.state}`">
        <header class="pg-column__head">
          <span class="pg-column__model">{{ col.model }}</span>
          <NTag size="small" :type="col.state === 'done' ? 'success' : col.state === 'error' ? 'error' : col.state === 'cancelled' ? 'warning' : 'info'">
            {{ stateLabel(col.state) }}
          </NTag>
        </header>

        <!-- Streaming and finished columns show the text as it grows —
             chunk by chunk, the first column to answer is the first column
             on screen, never held back by a slower neighbour. -->
        <template v-if="col.state === 'streaming' || col.state === 'done'">
          <pre class="pg-column__output" data-testid="pg-column-output">{{ col.output }}</pre>
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
                {{ t('playground.usageTokens', { input: col.usage?.input_tokens ?? 0, output: col.usage?.output_tokens ?? 0 }) }}
              </dd>
            </div>
          </dl>
        </template>

        <div v-else-if="col.state === 'running'" class="pg-column__state">{{ t('playground.columnStateRunning') }}</div>

        <!-- A cancelled column: the run was stopped from this page; what
             it had already cost is in the request log under its id. -->
        <div v-else-if="col.state === 'cancelled'" class="pg-column__state" data-testid="pg-column-cancelled">
          {{ t('playground.columnCancelledNote') }}
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

    <EmptyState
      v-else
      :icon="FlaskConical"
      :title="t('playground.emptyTitle')"
      :description="t('playground.emptyDescription')"
    />
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

  // A (re)run starts from a fresh column set built from the CURRENT
  // selection — the previous run's columns are replaced, never appended
  // to, so stale results cannot masquerade as part of the new comparison.
  const runModels = selectedModels.value.slice()
  columns.value = initialColumns(runModels)
  runId.value++
  running.value = true
  sawStreamEvent = false
  runController = new AbortController()
  try {
    await runPlayground(
      {
        key_id: keyId.value,
        modality: 'text',
        models: runModels,
        messages: [{ role: 'user', content: userContent() }],
        params: {
          system: systemPrompt.value || undefined,
          temperature: temperature.value ?? undefined,
          max_tokens: maxTokens.value ?? undefined,
        },
      },
      applyEvent,
      { signal: runController.signal },
    )
  } catch (err) {
    if (err instanceof RequestAbortedError) {
      // The operator stopped the run (or left the page): the columns that
      // never reached a terminal state are stopped, not failed — what
      // they cost is in the request log under each column's id.
      markUnfinished('cancelled')
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
  }
}

function onStop() {
  runController?.abort()
}

// markUnfinished settles columns that never received a terminal event —
// the local half of a disconnect the backend acknowledges best-effort.
function markUnfinished(state: 'cancelled' | 'error', errorMessage = '') {
  for (const col of columns.value) {
    if (col.state === 'running' || col.state === 'streaming') {
      col.state = state
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
.pg-card {
  padding: var(--space-5) var(--space-6);
  background: var(--color-surface);
  border: 1px solid var(--color-border-subtle);
  border-radius: var(--radius-lg);
}

.pg-modality-tabs {
  margin-bottom: var(--space-2);
}

.pg-key-select {
  max-width: 420px;
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
  max-width: 640px;
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

.pg-params {
  display: grid;
  grid-template-columns: minmax(0, 2fr) minmax(0, 1fr);
  gap: 0 var(--space-5);
}

.pg-param-number {
  width: 100%;
}

.pg-actions {
  display: flex;
  justify-content: flex-end;
  margin-top: var(--space-2);
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

@media (max-width: @mobile-breakpoint) {
  .pg-params {
    grid-template-columns: minmax(0, 1fr);
  }
}
</style>
