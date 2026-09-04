<!-- frontend/src/components/apikeys/ApiExamplesModal.vue
     The API examples dialog: every copy-ready sample this console can show,
     grouped by entry protocol × capability (outer tabs) with one tab per
     language inside each group. Samples come from the pure generator in
     utils/apiExamples.ts bound to the resolved gateway address; this
     component owns presentation only — tabs, highlighting (NCode over the
     shared hljs core instance), and per-block copy.

     An optional real key (create-key dialog's one-time step) is injected
     into every sample; without it the <API Key> placeholder stands in. -->
<template>
  <ModalDrawer
    v-model:show="show"
    :title="t('apiKeys.examplesTitle')"
    max-width="860px"
    :back-label="t('common.back')"
  >
    <div class="examples">
      <n-tabs :value="activeGroup" type="line" size="small" @update:value="onGroupChange">
        <n-tab-pane
          v-for="group in catalog"
          :key="group.id"
          :name="group.id"
          :tab="groupLabel(group.id)"
        >
          <n-tabs
            :value="activeLanguage(group.id)"
            type="segment"
            size="small"
            class="examples__langs"
            @update:value="(v: string | number) => onLanguageChange(group.id, v)"
          >
            <n-tab-pane
              v-for="lang in group.languages"
              :key="lang.language"
              :name="lang.language"
              :tab="languageLabel(lang.language)"
            >
              <div
                v-for="(snippet, i) in lang.snippets"
                :key="i"
                class="example-block"
                :class="{ 'example-block--divider': i > 0 }"
              >
                <div class="example-block__head">
                  <span class="example-block__label">{{ snippetLabel(snippet) }}</span>
                  <NButton size="tiny" quaternary @click="copy(snippet.code)">
                    <template #icon><Copy :size="13" /></template>
                  </NButton>
                </div>
                <NCode
                  class="example-block__code"
                  :code="snippet.code"
                  :hljs="hljs"
                  :language="hljsLanguage(lang.language, snippet.kind)"
                />
              </div>
            </n-tab-pane>
          </n-tabs>
        </n-tab-pane>
      </n-tabs>
    </div>
    <template #footer>
      <NSpace justify="end">
        <NButton @click="show = false">{{ t('common.close') }}</NButton>
      </NSpace>
    </template>
  </ModalDrawer>
</template>

<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { NButton, NCode, NSpace, NTabPane, NTabs, useMessage } from 'naive-ui'
import { Copy } from '@lucide/vue'

import ModalDrawer from '../common/ModalDrawer.vue'
import { useGatewayEndpoint } from '../../composables/useGatewayEndpoint'
import {
  buildExampleCatalog,
  type ExampleLanguage,
  type ExampleProtocol,
  type ExampleSnippet,
} from '../../utils/apiExamples'
import { copyToClipboard } from '../../utils/clipboard'
import hljs from '../../utils/hljs'

const props = withDefaults(
  defineProps<{
    // Which base-URL row opened this modal — the outer tab lands on the
    // first group of that protocol.
    initialProtocol?: ExampleProtocol
    // Real credential for samples. No caller passes it yet; the create-key
    // dialog's one-time step is the intended consumer. Omitting it keeps
    // the <API Key> placeholder in every sample.
    apiKey?: string
  }>(),
  { initialProtocol: 'openai', apiKey: undefined },
)

const show = defineModel<boolean>('show', { required: true })

const { t } = useI18n()
const message = useMessage()
const { endpoint } = useGatewayEndpoint()

const catalog = computed(() =>
  buildExampleCatalog({ endpoint: endpoint.value, key: props.apiKey }),
)

const activeGroup = ref(catalog.value[0]?.id ?? '')
// Per-group inner tab: switching groups preserves each group's last language
// rather than snapping back to the first — the languages line up across
// groups, so keeping position feels like staying put.
const activeLanguageByGroup = reactive<Record<string, ExampleLanguage>>({})

function activeLanguage(groupId: string): ExampleLanguage {
  const group = catalog.value.find((g) => g.id === groupId)
  return activeLanguageByGroup[groupId] ?? group?.languages[0]?.language ?? 'curl'
}

function onGroupChange(value: string | number) {
  activeGroup.value = String(value)
}

function onLanguageChange(groupId: string, value: string | number) {
  activeLanguageByGroup[groupId] = value as ExampleLanguage
}

// The opener's protocol decides the landing tab; re-run on every open so a
// modal reused by different rows always lands where the click came from.
// A protocol with no group yet keeps the first tab rather than blanking the
// modal.
watch(
  () => [show.value, props.initialProtocol] as const,
  ([isOpen]) => {
    if (!isOpen) return
    const group = catalog.value.find((g) => g.protocol === props.initialProtocol)
    if (group) activeGroup.value = group.id
  },
  { immediate: true },
)

// Label keys are registered together with each group; nothing else guards
// that registration (the locale parity test only compares the zh/en key
// sets), so a missing entry falls back to the raw group id — loud enough
// to spot the moment a group is added without its key.
const GROUP_LABEL_KEYS: Record<string, string> = {
  'openai-chat': 'apiKeys.exampleGroupChat',
}

function groupLabel(id: string): string {
  const key = GROUP_LABEL_KEYS[id]
  return key ? t(key) : id
}

const LANGUAGE_LABEL_KEYS: Record<ExampleLanguage, string> = {
  curl: 'apiKeys.exampleLangCurl',
  python: 'apiKeys.exampleLangPython',
  node: 'apiKeys.exampleLangNode',
  go: 'apiKeys.exampleLangGo',
}

function languageLabel(language: ExampleLanguage): string {
  return t(LANGUAGE_LABEL_KEYS[language])
}

function snippetLabel(snippet: ExampleSnippet): string {
  if (snippet.kind === 'response') {
    return snippet.streaming
      ? t('apiKeys.exampleStreamResponse')
      : t('apiKeys.exampleResponse')
  }
  return snippet.streaming ? t('apiKeys.exampleStreamRequest') : t('apiKeys.exampleRequest')
}

// Requests highlight in their language's grammar (curl as bash); response
// bodies are JSON on the wire no matter which client asked for them.
const REQUEST_HLJS: Record<ExampleLanguage, string> = {
  curl: 'bash',
  python: 'python',
  node: 'javascript',
  go: 'go',
}

function hljsLanguage(language: ExampleLanguage, kind: ExampleSnippet['kind']): string {
  return kind === 'response' ? 'json' : REQUEST_HLJS[language]
}

async function copy(code: string) {
  if (await copyToClipboard(code)) {
    message.success(t('common.copied'))
  } else {
    message.error(t('common.copyFailed'))
  }
}
</script>

<style scoped lang="less">
.examples {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}

// Segment tabs need a hair of air below before the first code block.
.examples__langs {
  margin-top: var(--space-2);
}

.example-block {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
}

.example-block--divider {
  margin-top: var(--space-3);
}

.example-block__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-2);
}

.example-block__label {
  font-size: var(--text-xs);
  color: var(--color-text-secondary);
}

.example-block__code {
  border: 1px solid var(--color-border);
  border-radius: var(--radius-md);

  :deep(pre) {
    margin: 0;
    font-size: var(--text-xs);
  }
}
</style>
