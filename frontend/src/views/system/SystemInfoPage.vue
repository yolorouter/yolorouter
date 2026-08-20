<!-- frontend/src/views/system/SystemInfoPage.vue

     The About page (/about): product identity, the running version, the
     update story, and project links — one card, three quiet sections. The
     update area is state-driven: checking, check failed (with retry), up to
     date, or "current → latest" with the action this deployment supports
     (one-click button for in_place, a pull-the-image hint for containers, a
     manual-download hint for windows, the release link otherwise). -->
<template>
  <div class="common-page">
    <PageHeader :eyebrow="t('system.eyebrow')" :title="t('system.pageTitle')" :description="t('system.pageDescription')" />

    <section class="about-card">
      <div class="about-identity">
        <img :src="logo" alt="" class="about-logo" />
        <div>
          <div class="about-name-row">
            <span class="about-name">Yolorouter</span>
            <span v-if="updateStore.version" class="about-version">{{ updateStore.version }}</span>
          </div>
          <p class="about-tagline">{{ t('system.tagline') }}</p>
        </div>
      </div>

      <div class="about-update">
        <!-- A deployment that cannot take an update through this page says
             so in one line instead of offering controls that cannot work —
             the same way browsers built by a distro point at the package
             manager. -->
        <template v-if="updateStore.updateMode === 'disabled'">
          <p class="about-status about-status--muted">{{ t('system.updatesDisabledHint') }}</p>
        </template>
        <template v-else-if="updateStore.updateMode === 'dev'">
          <p class="about-status about-status--muted">{{ t('system.devBuildHint') }}</p>
        </template>
        <template v-else-if="updateStore.hasUpdate">
          <p class="about-status">
            <span class="about-dot" />
            {{ t('system.newVersionAvailable') }}
            <span class="about-versions">
              <span class="about-mono">{{ updateStore.version }}</span>
              <span class="about-arrow">→</span>
              <span class="about-mono about-mono--next">{{ updateStore.latest }}</span>
            </span>
          </p>
          <div class="about-action">
            <n-button
              v-if="updateStore.updateMode === 'in_place'"
              type="primary"
              :loading="phase !== 'idle'"
              @click="showConfirm = true"
            >
              {{ t('system.updateNow') }}
            </n-button>
            <p v-else-if="updateStore.updateMode === 'container'" class="about-hint">{{ t('system.containerHint') }}</p>
            <p v-else-if="updateStore.updateMode === 'windows'" class="about-hint">{{ t('system.windowsHint') }}</p>
            <p v-else-if="updateStore.updateMode === 'capabilities'" class="about-hint">{{ t('system.capabilitiesHint') }}</p>
            <n-button
              v-if="updateStore.updateMode !== 'in_place' && updateStore.releaseUrl"
              text
              type="primary"
              tag="a"
              :href="updateStore.releaseUrl"
              target="_blank"
              rel="noopener noreferrer"
            >
              {{ t('system.viewRelease') }}
            </n-button>
          </div>
        </template>
        <!-- No known update: a check button with its result inline — the
             button is the action, the text is the answer. -->
        <template v-else>
          <div class="about-check-row">
            <n-button size="small" :loading="checking" @click="checkNow(true)">
              {{ t('system.checkUpdates') }}
            </n-button>
            <span v-if="checking" class="about-check-result">{{ t('system.loading') }}</span>
            <span v-else-if="updateStore.checkFailed" class="about-check-result">{{ t('system.checkFailed') }}</span>
            <span v-else-if="updateStore.version" class="about-check-result">{{ t('system.upToDate') }}</span>
          </div>
        </template>
      </div>

      <div class="about-footer">
        <nav class="about-links">
          <a href="https://github.com/yolorouter/yolorouter" target="_blank" rel="noopener noreferrer">GitHub</a>
          <a href="https://yolorouter.com/help?utm_source=oss-console&utm_medium=about" target="_blank" rel="noopener noreferrer">{{ t('system.docs') }}</a>
          <a href="https://github.com/yolorouter/yolorouter/releases" target="_blank" rel="noopener noreferrer">{{ t('system.changelog') }}</a>
          <a href="https://github.com/yolorouter/yolorouter/blob/main/LICENSE" target="_blank" rel="noopener noreferrer">Apache-2.0</a>
        </nav>
      </div>
    </section>

    <ModalDrawer
      v-model:show="showConfirm"
      :title="t('system.updateConfirmTitle', { version: updateStore.latest })"
      :confirm-text="t('system.updateConfirmOk')"
      :cancel-text="t('common.cancel')"
      :loading="phase !== 'idle'"
      :mask-closable="false"
      :close-on-esc="false"
      :dismissable="phase === 'idle'"
      :back-label="t('common.back')"
      max-width="520px"
      @confirm="startUpdate"
    >
      <p class="update-warn">{{ t('system.updateWarnRestart') }}</p>
      <!-- SQLite gets an automatic pre-migration backup at restart; PostgreSQL
           does not, so this is the operator's only advance prompt to back up. -->
      <p v-if="updateStore.dbDriver === 'postgres'" class="update-warn">{{ t('system.updateWarnBackupPostgres') }}</p>
      <p class="update-warn">{{ t('system.updateWarnForeground') }}</p>
      <p v-if="phase === 'updating'" class="update-status">{{ t('system.updating') }}</p>
      <p v-else-if="phase === 'restarting'" class="update-status">{{ t('system.restarting') }}</p>
    </ModalDrawer>
  </div>
</template>

<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'
import { useMessage } from 'naive-ui'
import { useUpdateStore } from '../../store/update'
import { useAuthStore } from '../../store/auth'
import { getSystemVersion, postSystemUpdate } from '../../api/system'
import { APIError, displayMessage, errorCodeOf } from '../../api/client'
import { ACCOUNT_SESSION_INVALID } from '../../api/errcodes'
import PageHeader from '../../components/PageHeader.vue'
import ModalDrawer from '../../components/common/ModalDrawer.vue'
import logo from '../../assets/logo.svg'

const { t } = useI18n()
const router = useRouter()
const updateStore = useUpdateStore()
const message = useMessage()

// sessionExpired routes a lapsed admin session to reauth: clear the auth
// state AND navigate — handleSessionExpired alone does not navigate, and
// route guards don't rerun on a store change, so without the push the
// protected shell stays visible with dead data.
function sessionExpired(err: unknown): boolean {
  if (errorCodeOf(err) !== ACCOUNT_SESSION_INVALID) return false
  useAuthStore().handleSessionExpired()
  void router.push('/login')
  return true
}

// Load through the shared store action (lastFetchId race-guarded) rather than
// calling getSystemVersion directly: a direct load would race
// DefaultLayout's mount-time check, and an older delayed response could
// overwrite the newer one in the shared badge / release-URL state.
// checkForUpdates swallows its own errors (a failed check is an
// expected pre-public / GitHub-outage state), so fire-and-forget is safe.
onMounted(() => {
  // checkForUpdates never rejects (store/update.ts documents "NEVER throws"
  // and wraps its entire body in try/catch); the store surfaces failures via
  // checkFailed, not a rejected promise, so no .catch is needed.
  void checkNow(false)
})

// checking covers both the mount-time check and manual re-checks, so the
// button always reflects a run that is actually in flight. A manual click
// forces a cache bypass (the server caches check results for a few
// minutes); the passive mount-time load is fine with the cached answer.
const checking = ref(false)
async function checkNow(force: boolean) {
  if (checking.value) return
  checking.value = true
  try {
    await updateStore.checkForUpdates(force)
  } finally {
    // checkForUpdates documents "never throws", but a stuck-true `checking`
    // would permanently disable the button — cheap insurance.
    checking.value = false
  }
}

// --- One-click update flow -------------------------------------------------
//
// idle -> updating (POST in flight; the server downloads, verifies, and
// swaps the binary before replying) -> restarting (server replied "updated"
// and is now draining + restarting; poll the version endpoint until a
// different version answers) -> idle again with a success toast.

type UpdatePhase = 'idle' | 'updating' | 'restarting'
const phase = ref<UpdatePhase>('idle')
const showConfirm = ref(false)

// Polling must stop when the user navigates away — `disposed` is checked
// after every await so an unmounted page never toasts or writes state.
let disposed = false
onBeforeUnmount(() => {
  disposed = true
})

const sleep = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms))

async function startUpdate() {
  // Defensive double-submit guard: naive-ui's button already ignores clicks
  // while loading, but this function must stay safe even if a future caller
  // wires it to a control without that behavior.
  if (phase.value !== 'idle') return
  const before = updateStore.version
  phase.value = 'updating'
  try {
    const res = await postSystemUpdate()
    if (res.status === 'up_to_date') {
      // The badge raced a release change or a concurrent update: nothing
      // was installed, no restart is coming. Force the follow-up check —
      // the server's positive cache is exactly what went stale here, and an
      // unforced check would re-serve it, leaving the badge and button up
      // for another cache lifetime.
      phase.value = 'idle'
      showConfirm.value = false
      message.info(t('system.upToDate'))
      void updateStore.checkForUpdates(true)
      return
    }
    phase.value = 'restarting'
    await waitForRestart(before)
  } catch (err) {
    if (disposed) return
    // A lapsed session must reauth, not read as "update failed" — and it
    // must be recognized before the generic APIError branch swallows it.
    if (sessionExpired(err)) return
    if (err instanceof APIError) {
      // The server refused or the update run failed before anything was
      // replaced — the process is still up, no restart is coming.
      phase.value = 'idle'
      message.error(displayMessage(err, t))
      return
    }
    // Transport error: the connection may have been cut by the restart
    // itself (or a proxy timed the long request out after the swap). Fall
    // through to polling and let the reported version tell the truth.
    phase.value = 'restarting'
    await waitForRestart(before)
  }
}

async function waitForRestart(before: string) {
  const deadline = Date.now() + 5 * 60_000
  while (!disposed && Date.now() < deadline) {
    await sleep(2000)
    if (disposed) return
    try {
      // Bypass the store here: its fetch-token guard is for shared state,
      // while this poll only needs a raw "who answers now" probe.
      const info = await getSystemVersion()
      if (info.version && info.version !== before) {
        phase.value = 'idle'
        showConfirm.value = false
        message.success(t('system.updateDone', { version: info.version }))
        // Refresh the shared state so the sidebar badge and the card above
        // flip to the new version without a manual reload.
        void updateStore.checkForUpdates()
        return
      }
    } catch (err) {
      // A lapsed session would otherwise be swallowed here for the full
      // five minutes and misreported as a restart timeout.
      if (sessionExpired(err)) return
      // Connection refused / timeout while the server restarts — keep
      // polling until the deadline.
    }
  }
  if (disposed) return
  phase.value = 'idle'
  message.error(t('system.updateTimeout'))
}
</script>

<style scoped>
.common-page {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
}

.about-card {
  max-width: 560px;
  background: var(--color-surface);
  border: 1px solid var(--color-border-subtle);
  border-radius: var(--radius-lg);
}

/* Three stacked sections share one internal rhythm; the borders between them
   come from the sections themselves so the card needs no extra divider
   elements. */
.about-identity,
.about-update,
.about-footer {
  padding: var(--space-5) var(--space-6);
}

.about-update,
.about-footer {
  border-top: 1px solid var(--color-border-subtle);
}

.about-identity {
  display: flex;
  align-items: center;
  gap: var(--space-4);
}

.about-logo {
  width: 44px;
  height: 44px;
  flex-shrink: 0;
}

.about-name-row {
  display: flex;
  align-items: baseline;
  gap: var(--space-3);
  flex-wrap: wrap;
}

.about-name {
  font-size: 20px;
  font-weight: 700;
  letter-spacing: -0.01em;
  color: var(--color-text);
}

.about-version {
  font-family: var(--font-mono);
  font-size: 13px;
  color: var(--color-text-secondary);
  background: var(--color-bg-soft);
  border-radius: var(--radius-full);
  padding: 1px 10px;
}

.about-tagline {
  margin: var(--space-1) 0 0;
  font-size: 13px;
  color: var(--color-text-secondary);
}

.about-status {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  margin: 0;
  font-size: 14px;
  color: var(--color-text);
}

.about-status--muted {
  color: var(--color-text-muted);
}

.about-dot {
  width: 8px;
  height: 8px;
  border-radius: var(--radius-full);
  background: var(--color-accent);
  flex-shrink: 0;
}

.about-versions {
  display: inline-flex;
  align-items: baseline;
  gap: var(--space-2);
}

.about-mono {
  font-family: var(--font-mono);
  font-size: 13px;
  color: var(--color-text-secondary);
}

.about-mono--next {
  color: var(--color-accent);
  font-weight: 600;
}

.about-arrow {
  color: var(--color-text-muted);
}

.about-action {
  margin-top: var(--space-3);
  display: flex;
  align-items: center;
  gap: var(--space-3);
  flex-wrap: wrap;
}

.about-check-row {
  display: flex;
  align-items: center;
  gap: var(--space-3);
}

.about-check-result {
  font-size: 13px;
  color: var(--color-text-muted);
}

.about-hint {
  margin: 0;
  font-size: 13px;
  line-height: 1.6;
  color: var(--color-text-secondary);
}

.about-links {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: var(--space-2);
}

.about-links a {
  font-size: 13px;
  color: var(--color-text-secondary);
  text-decoration: none;
  border-radius: var(--radius-sm);
}

.about-links a:hover {
  color: var(--color-accent);
}

.about-links a:focus-visible {
  outline: 2px solid var(--color-accent);
  outline-offset: 2px;
}

/* Interpunct separators between links, not after the last one. */
.about-links a + a::before {
  content: '·';
  color: var(--color-text-muted);
  margin-right: var(--space-2);
}

.update-warn {
  margin: 0 0 var(--space-2);
}

.update-status {
  margin: var(--space-2) 0 0;
  font-weight: 600;
}
</style>
