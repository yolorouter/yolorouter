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
          <NInput v-model:value="prompt" class="pg-prompt" type="textarea" :rows="3" />
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
          <NTag size="small" :type="col.state === 'done' ? 'success' : col.state === 'error' ? 'error' : 'info'">
            {{ stateLabel(col.state) }}
          </NTag>
        </header>

        <div v-if="col.state === 'running'" class="pg-column__state">{{ t('playground.columnStateRunning') }}</div>

        <template v-else-if="col.state === 'done'">
          <pre class="pg-column__output">{{ col.output }}</pre>
          <dl class="pg-metrics">
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
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useMessage } from 'naive-ui'
import { FlaskConical } from '@lucide/vue'
import PageHeader from '../../components/PageHeader.vue'
import HelpLabel from '../../components/HelpLabel.vue'
import EmptyState from '../../components/EmptyState.vue'
import { displayMessage } from '../../api/client'
import {
  listPlaygroundKeys,
  listPlaygroundModels,
  runPlayground,
  type PlaygroundKeyOption,
  type PlaygroundModelOption,
  type PlaygroundStreamEvent,
} from '../../api/playground'
import { formatMicros } from '../../utils/money'
import { formatDuration } from '../../utils/format'

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

const running = ref(false)
const runId = ref(0)

type ColumnState = 'running' | 'done' | 'error'

interface Column {
  model: string
  state: ColumnState
  output: string
  costMicros: number
  costKnown: boolean
  durationMs: number
  usage: { input_tokens: number; output_tokens: number } | null
  requestId: string
  errorStatus: number
  errorMessage: string
  errorType: string
}

const columns = ref<Column[]>([])

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

function stateLabel(state: ColumnState): string {
  if (state === 'done') return t('playground.columnStateDone')
  if (state === 'error') return t('playground.columnStateFailed')
  return t('playground.columnStateRunning')
}

onMounted(() => {
  void loadPickers()
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
  columns.value = runModels.map((model) => ({
    model,
    state: 'running' as ColumnState,
    output: '',
    costMicros: 0,
    costKnown: false,
    durationMs: 0,
    usage: null,
    requestId: '',
    errorStatus: 0,
    errorMessage: '',
    errorType: '',
  }))
  runId.value++
  running.value = true
  try {
    await runPlayground(
      {
        key_id: keyId.value,
        modality: 'text',
        models: runModels,
        messages: [{ role: 'user', content: prompt.value }],
        params: {
          system: systemPrompt.value || undefined,
          temperature: temperature.value ?? undefined,
          max_tokens: maxTokens.value ?? undefined,
        },
      },
      applyEvent,
    )
  } catch (err) {
    // Pre-stream gate failures (key ownership/state, bind errors) answer
    // with the console envelope: no column ever started, so the placeholder
    // columns come back off and the message names the failure.
    columns.value = []
    message.error(displayMessage(err, t))
  } finally {
    running.value = false
  }
}

// applyEvent folds one aggregate-channel event into its column. slot is
// the column's position in the submitted models list; an event naming a
// slot outside the run cannot happen from this page's own submissions and
// is dropped rather than thrown.
function applyEvent(ev: PlaygroundStreamEvent) {
  const col = columns.value[ev.slot]
  if (!col) return
  if (ev.type === 'chunk') {
    col.output += ev.delta
    return
  }
  if (ev.type === 'done') {
    col.state = 'done'
    col.costMicros = ev.cost_micros
    col.costKnown = ev.cost_known
    col.durationMs = ev.duration_ms
    col.usage = ev.usage
    col.requestId = ev.request_id
    return
  }
  col.state = 'error'
  col.errorStatus = ev.status
  col.requestId = ev.request_id
  // The error object is the relay dialect's native shape; read the two
  // members every OpenAI-shaped dialect carries and leave the rest to the
  // raw payload the run contract documents.
  const err = (ev.error ?? null) as { message?: unknown; type?: unknown } | null
  col.errorMessage = typeof err?.message === 'string' ? err.message : ''
  col.errorType = typeof err?.type === 'string' ? err.type : ''
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

.pg-prompt {
  max-width: 640px;
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
