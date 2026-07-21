<!-- frontend/src/views/system/SystemInfoPage.vue -->
<template>
  <div class="system-page">
    <PageHeader :eyebrow="t('system.eyebrow')" :title="t('system.pageTitle')" :description="t('system.pageDescription')" />

    <n-descriptions label-placement="left" bordered :column="1" size="large" class="system-block" :label-style="labelStyle">
      <n-descriptions-item :label="t('system.currentVersion')">
        <span class="system-version">{{ updateStore.version || '—' }}</span>
      </n-descriptions-item>
      <n-descriptions-item :label="t('system.commit')">{{ updateStore.commit || '—' }}</n-descriptions-item>
      <n-descriptions-item :label="t('system.buildTime')">{{ updateStore.buildTime || '—' }}</n-descriptions-item>
      <n-descriptions-item :label="t('system.goVersion')">{{ updateStore.goVersion || '—' }}</n-descriptions-item>
      <n-descriptions-item :label="t('system.platform')">{{ platform }}</n-descriptions-item>
      <n-descriptions-item :label="t('system.uptime')">{{ uptimeLabel }}</n-descriptions-item>
      <n-descriptions-item :label="t('system.database')">{{ updateStore.dbDriver || '—' }}</n-descriptions-item>
    </n-descriptions>

    <n-descriptions label-placement="left" bordered :column="1" size="large" class="system-block" :label-style="labelStyle">
      <n-descriptions-item :label="t('system.latestVersion')">
        <n-tag v-if="updateStore.checkFailed" type="warning" size="small">{{ t('system.checkFailed') }}</n-tag>
        <n-tag v-else-if="!updateStore.version" type="default" size="small">{{ t('system.loading') }}</n-tag>
        <n-tag v-else-if="updateStore.hasUpdate" type="success" size="small">{{ t('system.newVersionAvailable') }}</n-tag>
        <n-tag v-else type="default" size="small">{{ t('system.upToDate') }}</n-tag>
        <span class="system-latest">{{ updateStore.latest || '—' }}</span>
      </n-descriptions-item>
      <n-descriptions-item :label="t('system.releaseLabel')">
        <n-button
          v-if="updateStore.releaseUrl"
          text
          tag="a"
          :href="updateStore.releaseUrl"
          target="_blank"
          rel="noopener noreferrer"
        >
          {{ t('system.viewRelease') }}
        </n-button>
        <span v-else>—</span>
      </n-descriptions-item>
    </n-descriptions>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useUpdateStore } from '../../store/update'
import PageHeader from '../../components/PageHeader.vue'

const { t } = useI18n()
const updateStore = useUpdateStore()

// Both descriptions tables auto-size their label column to their own content,
// so their content dividers drift out of alignment. Pin a shared label width
// to keep the vertical divider aligned across both blocks.
const labelStyle = { width: '160px' }

// Load through the shared store action (lastFetchId race-guarded) rather than
// calling getSystemVersion directly: a direct /system load would race
// DefaultLayout's mount-time check, and an older delayed response could
// overwrite the newer one in the shared badge / release-URL state (Codex
// review P2). checkForUpdates swallows its own errors (a failed check is an
// expected pre-public / GitHub-outage state), so fire-and-forget is safe.
onMounted(() => {
  // checkForUpdates never rejects (store/update.ts documents "NEVER throws"
  // and wraps its entire body in try/catch); the store surfaces failures via
  // checkFailed, not a rejected promise, so no .catch is needed (Codex P2).
  void updateStore.checkForUpdates()
})

const platform = computed(() => (updateStore.goos ? `${updateStore.goos} / ${updateStore.goarch}` : '—'))

// uptimeSeconds is an integer count from the server; render it as a short
// human-readable duration rather than a raw number of seconds.
const uptimeLabel = computed(() => {
  // Distinguish "not loaded / fetch failed" (version still empty) from a
  // legitimate uptime of 0 (a freshly-booted server): `!secs` would treat 0
  // as not-loaded (JS falsy-zero) and render '—' instead of '0m' (Codex P2).
  if (!updateStore.version) return '—'
  const secs = updateStore.uptimeSeconds
  const days = Math.floor(secs / 86400)
  const hours = Math.floor((secs % 86400) / 3600)
  const minutes = Math.floor((secs % 3600) / 60)
  const parts: string[] = []
  if (days > 0) parts.push(`${days}d`)
  if (hours > 0 || days > 0) parts.push(`${hours}h`)
  parts.push(`${minutes}m`)
  return parts.join(' ')
})
</script>

<style scoped>
.system-page {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
}

.system-block {
  max-width: 640px;
}

.system-version {
  font-weight: 600;
}

.system-latest {
  margin-left: var(--space-2);
}
</style>
