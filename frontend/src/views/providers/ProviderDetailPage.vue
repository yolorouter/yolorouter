<!-- frontend/src/views/providers/ProviderDetailPage.vue -->
<template>
  <div class="provider-detail-page" v-if="provider">
    <PageHeader :eyebrow="t('providers.eyebrow')" :title="provider.name" :description="provider.base_url">
      <template #actions>
        <n-button quaternary @click="onToggleProviderStatus">
          {{ provider.management_status === 1 ? t('providers.statusDisabled') : t('providers.statusEnabled') }}
        </n-button>
      </template>
    </PageHeader>

    <n-tabs v-model:value="activeTab" type="line" animated>
      <n-tab-pane name="overview" :tab="t('providers.tabOverview')">
        <div class="section-card">
          <n-descriptions :column="1" label-placement="left">
            <n-descriptions-item :label="t('providers.name')">{{ provider.name }}</n-descriptions-item>
            <n-descriptions-item :label="t('providers.baseUrl')">{{ provider.base_url }}</n-descriptions-item>
            <n-descriptions-item :label="t('providers.note')">{{ provider.note || '-' }}</n-descriptions-item>
          </n-descriptions>
        </div>
      </n-tab-pane>

      <n-tab-pane name="keys" :tab="t('providers.tabKeys')">
        <div class="keys-toolbar">
          <span v-if="pendingCount !== null" class="keys-toolbar__count">
            {{ t('providers.testAllPendingCount', { count: pendingCount }) }}
          </span>
          <n-space>
            <n-button @click="showAddKey = true">
              <template #icon><Plus :size="16" /></template>
              {{ t('providers.addKey') }}
            </n-button>
            <n-button type="primary" :loading="testingAll" @click="onTestAll">
              <template #icon><PlayCircle :size="16" /></template>
              {{ t('providers.testAllButton') }}
            </n-button>
          </n-space>
        </div>

        <div class="data-table-wrapper">
          <n-data-table
            :columns="keyColumns"
            :data="provider.keys"
            :loading="testingAll"
            :bordered="false"
            :single-line="false"
            :row-key="(row: ProviderKey) => row.id"
          />
        </div>

        <n-alert v-if="batchSummary" type="info" class="summary">{{ batchSummary }}</n-alert>
      </n-tab-pane>

      <n-tab-pane name="models" :tab="t('providers.tabModels')">
        <EmptyState v-if="modelsStore.error" :title="t('common.networkError')" />
        <EmptyState v-else-if="!modelsStore.loading && linkedModelRows.length === 0" :title="t('providers.modelsEmpty')" />
        <div v-else class="data-table-wrapper">
          <n-data-table
            :columns="modelColumns"
            :data="linkedModelRows"
            :loading="modelsStore.loading"
            :bordered="false"
            :row-key="(row: LinkedModelRow) => row.candidateId"
          />
        </div>
      </n-tab-pane>
    </n-tabs>

    <KeyEditDrawer v-model:show="showAddKey" :provider-id="provider.id" @saved="reload" />
    <KeyEditDrawer v-model:show="showEditKey" :provider-id="provider.id" :editing-key="editingKey" @saved="reload" />
  </div>
</template>

<script setup lang="ts">
import { computed, h, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute } from 'vue-router'
import { NButton, NDropdown, NSpace, NSwitch, NTag, useDialog, useMessage, type DataTableColumns } from 'naive-ui'
import { MoreHorizontal, Plus, PlayCircle } from '@lucide/vue'
import { useProvidersStore } from '../../store/providers'
import { useModelsStore } from '../../store/models'
import type { ModelCandidate } from '../../api/models'
import { displayMessage } from '../../api/client'
import type { BatchTestResult, Provider, ProviderKey } from '../../api/providers'
import PageHeader from '../../components/PageHeader.vue'
import EmptyState from '../../components/EmptyState.vue'
import KeyEditDrawer from '../../components/providers/KeyEditDrawer.vue'
import { columnTitle, STATUS_COL_WIDTH } from '../../utils/columnTitle'
import { testOutcomeI18nKey } from '../../utils/testOutcomeDisplay'

const { t } = useI18n()
const route = useRoute()
const dialog = useDialog()
const message = useMessage()
const store = useProvidersStore()
const modelsStore = useModelsStore()

const providerId = Number(route.params.id)
const provider = ref<Provider | null>(null)
const activeTab = ref('overview')
const showAddKey = ref(false)
const showEditKey = ref(false)
const editingKey = ref<ProviderKey | null>(null)
const testingAll = ref(false)
// Tracks the single key currently running its own "Test Connection" (distinct from
// testingAll's batch run) so the actions button can show a spinner instead
// of silently doing nothing until the request resolves.
const testingKeyId = ref<number | null>(null)
const batchSummary = ref('')
// Keyed by provider_key.id — populated once per completed batch test,
// cleared at the start of the next one (see onTestAll). Rendered as a
// per-key badge in the template above.
const batchResultByKeyId = ref<Record<number, BatchTestResult>>({})

// "N keys pending" = the keys batch test will actually hit. Batch test
// covers every key that isn't awaiting re-entry (regardless of enabled
// status — a fresh key is disabled until it passes), so this count must
// match that scope, not just the enabled subset.
const pendingCount = computed(() => {
  if (!provider.value) return null
  return provider.value.keys.filter((k) => !k.needs_reentry).length
})

function batchResultLabel(result: BatchTestResult): string {
  if (result.needs_reentry) return t('providers.needsReentry')
  if (result.skipped || result.outcome === null) return t('providers.testFailed')
  return t(`providers.${testOutcomeI18nKey(result.outcome)}`) + ` (${result.duration_ms}ms)`
}

function batchResultTagType(result: BatchTestResult): 'success' | 'warning' | 'error' {
  if (result.needs_reentry || result.skipped) return 'warning'
  return result.outcome === 0 ? 'success' : 'error'
}

onMounted(async () => {
  try {
    await reload()
  } catch (err) {
    message.error(displayMessage(err, t))
    return
  }
  try {
    await modelsStore.fetchList()
  } catch (err) {
    message.error(displayMessage(err, t))
  }
})

async function reload() {
  provider.value = await store.fetchDetail(providerId)
}

// Models referencing this provider as a candidate (the "模型映射" tab). M3
// left this tab as an EmptyState placeholder; this joins modelsStore.list
// (every model with its candidates) on candidate.provider_id.
type LinkedModelRow = { candidateId: number; modelName: string; candidate: ModelCandidate }

const linkedModelRows = computed<LinkedModelRow[]>(() => {
  if (!provider.value) return []
  const rows: LinkedModelRow[] = []
  for (const m of modelsStore.list) {
    for (const c of m.candidates) {
      if (c.provider_id === providerId) {
        rows.push({ candidateId: c.id, modelName: m.name, candidate: c })
      }
    }
  }
  return rows
})

const modelColumns = computed<DataTableColumns<LinkedModelRow>>(() => [
  {
    title: columnTitle(t('models.name'), t('models.name_tip')),
    key: 'modelName',
    minWidth: 160,
  },
  {
    title: columnTitle(t('models.providerModelName'), t('models.providerModelName_tip')),
    key: 'providerModelName',
    minWidth: 160,
    render: (row) => row.candidate.provider_model_name || '-',
  },
  {
    // Reads row.candidate.management_status (candidate-level), NOT the
    // model-level status — so the tooltip must describe candidate
    // semantics ("skips this candidate only"), not model semantics
    // ("model rejects all requests"). Reusing models.managementStatusColumn
    // here would mislabel the column (codex review finding).
    title: columnTitle(t('providers.candidateStatus'), t('providers.candidateStatus_tip')),
    key: 'management_status',
    width: 100,
    align: 'center',
    render: (row) =>
      h(
        NTag,
        { size: 'small', bordered: false, type: row.candidate.management_status === 1 ? 'success' : 'default' },
        { default: () => (row.candidate.management_status === 1 ? t('providers.statusEnabled') : t('providers.statusDisabled')) },
      ),
  },
])

function verificationLabel(status: number): string {
  if (status === 1) return t('providers.verificationPassed')
  if (status === 2) return t('providers.verificationFailed')
  return t('providers.verificationUntested')
}

function verificationTagType(status: number): 'success' | 'error' | 'default' {
  if (status === 1) return 'success'
  if (status === 2) return 'error'
  return 'default'
}

// A real NDataTable with defined columns (matching the reference
// project's ApiKeysPage.vue convention) rather than a hand-rolled list of
// flex rows — kept as a computed so the columns re-render when the active
// locale or batchResultByKeyId changes.
const keyColumns = computed<DataTableColumns<ProviderKey>>(() => [
  { title: columnTitle(t('providers.keyLabel'), t('providers.keyLabel_tip')), key: 'label', minWidth: 140 },
  {
    title: columnTitle(t('providers.keyPlaintext'), t('providers.keyPlaintext_tip')),
    key: 'key_prefix',
    minWidth: 140,
    render: (row) => h('span', { class: 'key-prefix-cell' }, `${row.key_prefix}***`),
  },
  {
    title: columnTitle(t('providers.testModel'), t('providers.testModel_tip')),
    key: 'test_model',
    minWidth: 140,
  },
  {
    title: columnTitle(t('providers.statusColumn'), t('providers.statusColumn_tip')),
    key: 'status',
    minWidth: 220,
    render: (row) => {
      const tags = [
        h(
          NTag,
          { size: 'small', bordered: false, type: verificationTagType(row.verification_status) },
          { default: () => verificationLabel(row.verification_status) },
        ),
      ]
      if (row.needs_reentry) {
        tags.push(
          h(NTag, { type: 'warning', size: 'small', bordered: false }, { default: () => t('providers.needsReentry') }),
        )
      }
      const batchResult = batchResultByKeyId.value[row.id]
      if (batchResult) {
        tags.push(
          h(
            NTag,
            { type: batchResultTagType(batchResult), size: 'small', bordered: false },
            { default: () => batchResultLabel(batchResult) },
          ),
        )
      }
      return h(NSpace, { size: 4 }, { default: () => tags })
    },
  },
  {
    title: columnTitle(t('providers.managementStatusColumn'), t('providers.managementStatusColumn_tip')),
    key: 'management_status',
    width: STATUS_COL_WIDTH,
    align: 'center',
    render: (row) =>
      h(NSwitch, {
        value: row.management_status === 1,
        'onUpdate:value': (v: boolean) => onToggleKeyStatus(row.id, v),
      }),
  },
  {
    // Matches the reference project's ApiKeysPage.vue actions-column
    // convention: a single compact "···" dropdown rather than several
    // inline text buttons — the inline-button version made this column
    // wide enough to force the whole table into horizontal scroll.
    title: t('common.actions'),
    key: 'actions',
    width: 60,
    align: 'center',
    render: (row) =>
      h(
        NDropdown,
        {
          trigger: 'click',
          placement: 'bottom-end',
          options: [
            { label: t('providers.editKey'), key: 'edit' },
            { label: t('providers.testConnection'), key: 'test', disabled: row.needs_reentry },
            { label: t('providers.moveUp'), key: 'up' },
            { label: t('providers.moveDown'), key: 'down' },
          ],
          onSelect: (key: string) => {
            if (key === 'edit') onEditKey(row)
            else if (key === 'test') onTestOneKey(row.id)
            else if (key === 'up') onReorder(row.id, 'up')
            else if (key === 'down') onReorder(row.id, 'down')
          },
        },
        {
          default: () =>
            h(
              NButton,
              { size: 'small', quaternary: true, circle: true, loading: testingKeyId.value === row.id, disabled: testingKeyId.value === row.id },
              { icon: () => h(MoreHorizontal, { size: 16 }) },
            ),
        },
      ),
  },
])

function onEditKey(key: ProviderKey) {
  editingKey.value = key
  showEditKey.value = true
}

async function onTestOneKey(keyId: number) {
  testingKeyId.value = keyId
  try {
    const updated = await store.testKey(providerId, keyId)
    await reload()
    // Two-tier feedback so the click is never silent: pass (green) vs
    // everything else (yellow) named by its specific outcome reason
    // (e.g. "unreachable"). Deliberately not mirroring the backend's
    // definitive-fail vs inconclusive split — that lives in
    // classifyTestResult and would drift if duplicated here.
    const outcome = updated.last_test_result
    if (outcome === null) return
    const label = t(`providers.${testOutcomeI18nKey(outcome)}`)
    if (outcome === 0) message.success(label)
    else message.warning(label)
  } catch (err) {
    message.error(displayMessage(err, t))
  } finally {
    testingKeyId.value = null
  }
}

async function onReorder(keyId: number, direction: 'up' | 'down') {
  try {
    await store.reorderKey(providerId, keyId, direction)
    await reload()
  } catch (err) {
    message.error(displayMessage(err, t))
  }
}

// A key actually contributes to routing only when it's enabled AND has
// passed verification AND doesn't need re-entry (design doc §4's
// availability rule) — "enabled" alone (management_status === 1) is not
// the same thing, and a max-effort code-review round found the warning
// below used the weaker enabled-count check, so it silently skipped the
// "you're about to disable the only key actually keeping this provider
// available" warning whenever another merely-enabled-but-unverified key
// was also present.
function isKeyAvailable(k: ProviderKey): boolean {
  return k.management_status === 1 && k.verification_status === 1 && !k.needs_reentry
}

function onToggleKeyStatus(keyId: number, enable: boolean) {
  const targetKey = provider.value?.keys.find((k) => k.id === keyId)
  const isLastAvailable =
    !enable &&
    provider.value !== null &&
    targetKey !== undefined &&
    isKeyAvailable(targetKey) &&
    provider.value.keys.filter(isKeyAvailable).length === 1
  const proceed = async () => {
    try {
      await store.setKeyStatus(providerId, keyId, enable)
      await reload()
    } catch (err) {
      message.error(displayMessage(err, t))
    }
  }
  if (isLastAvailable) {
    dialog.warning({
      title: t('providers.confirmDisableLastKeyTitle'),
      content: t('providers.confirmDisableLastKeyContent'),
      positiveText: t('providers.statusDisabled'),
      negativeText: t('providers.cancel'),
      onPositiveClick: proceed,
    })
    return
  }
  void proceed()
}

function onToggleProviderStatus() {
  if (!provider.value) return
  const enabling = provider.value.management_status !== 1
  const proceed = async () => {
    try {
      await store.setStatus(providerId, enabling)
      await reload()
    } catch (err) {
      message.error(displayMessage(err, t))
    }
  }
  if (!enabling) {
    dialog.warning({
      title: t('providers.confirmDisableProviderTitle'),
      content: t('providers.confirmDisableProviderContent'),
      positiveText: t('providers.statusDisabled'),
      negativeText: t('providers.cancel'),
      onPositiveClick: proceed,
    })
    return
  }
  void proceed()
}

async function onTestAll() {
  if (!provider.value) return
  testingAll.value = true
  batchSummary.value = ''
  batchResultByKeyId.value = {}
  try {
    // Timeout budget must count every key batch test will hit — that's
    // exactly pendingCount's scope (all !needs_reentry keys), so reuse it
    // rather than recomputing the same filter.
    const { results } = await store.testAll(providerId, pendingCount.value ?? 0)
    // `skipped` and `outcome === 0` are not mutually exclusive: a result
    // can be both TestSuccess AND skipped (its CAS write was lost to a
    // concurrent edit — the test itself succeeded, but nothing was
    // persisted). `passed` must exclude skipped results, or a skipped+
    // successful result gets double-counted and `failed` goes negative.
    const skipped = results.filter((r) => r.skipped).length
    const passed = results.filter((r) => !r.skipped && r.outcome === 0).length
    const failed = results.length - passed - skipped
    batchSummary.value = t('providers.testAllSummary', { passed, failed, skipped })
    batchResultByKeyId.value = Object.fromEntries(results.map((r) => [r.key_id, r]))
    await reload()
  } catch (err) {
    message.error(displayMessage(err, t))
  } finally {
    testingAll.value = false
  }
}
</script>

<style scoped>
.provider-detail-page {
  display: flex;
  flex-direction: column;
  gap: var(--space-6);
}

.keys-toolbar {
  display: flex;
  justify-content: space-between;
  align-items: center;
  margin-bottom: var(--space-4);
}

.keys-toolbar__count {
  color: var(--color-text-secondary);
  font-size: var(--text-sm);
}

:deep(.key-prefix-cell) {
  color: var(--color-text-muted);
  font-size: var(--text-xs);
  font-family: var(--font-mono);
}

.summary {
  margin-top: var(--space-3);
}
</style>
