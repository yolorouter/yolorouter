<!-- frontend/src/views/settings/GeneralSettingsPage.vue

     The General settings page (/settings/general, admin-only like /about):
     the System Settings group's persistent-form home for instance-wide
     settings. Six items live here — request log retention, admin audit
     retention, outbound proxy, capability enforcement, language, and key
     auto recovery.

     The five server-backed items are cards with their own load + version-CAS
     save lifecycle, lifted from the settings modals (OptimizationSettingsModal
     / VisionFallbackModal / the key-recovery modal this page replaced): the GET is
     authoritative, the PUT carries its version, and a 409 means another admin
     committed first — surface it, reload the committed row, let the user
     review and save again. A successful save also re-reads the GET so the
     form reflects the committed row, not just what we asked the server to
     write. Key auto recovery is the old modal's form migrated as-is (same
     utils, same message keys, same bounds); only its housing changed from
     modal to page.

     The language item is different on purpose: it is a personal, per-browser
     preference, not an instance setting — it never touches the settings API
     or the database. It reads and writes the locale store (the same store
     the sidebar Language entry uses), so the two entries stay in sync with
     no wiring of our own, and picking an option applies immediately (there
     is nothing to round-trip, so there is no save button either). -->
<template>
  <div class="common-page">
    <PageHeader
      :eyebrow="t('nav.groupSystem')"
      :title="t('nav.generalSettings')"
      :description="t('generalSettings.pageDescription')"
    />

    <!-- Request log retention -->
    <section class="settings-card">
      <h2 class="settings-card__title">{{ t('generalSettings.retention.title') }}</h2>
      <p class="settings-card__desc">{{ t('generalSettings.retention.desc') }}</p>
      <div v-if="retentionLoad === 'loading'" class="settings-card__state">{{ t('common.loading') }}</div>
      <div v-else-if="retentionLoad === 'error'" class="settings-card__state settings-card__state--err">
        <span>{{ t('generalSettings.retention.loadFailed') }}</span>
        <NButton size="small" @click="loadRetention">{{ t('generalSettings.retention.retry') }}</NButton>
      </div>
      <!-- Validation runs through NForm rules (blur trigger) so the red
           feedback appears inline on leaving the field; onSave re-validates
           via formRef.validate() before anything is sent. -->
      <NForm v-else ref="retentionFormRef" :model="retentionForm" :rules="retentionRules" require-mark-placement="left">
        <NFormItem path="days">
          <template #label>
            <HelpLabel :tip="t('generalSettings.retention.daysTip')">{{ t('generalSettings.retention.days') }}</HelpLabel>
          </template>
          <!-- No :min/:max/:precision on the input itself on purpose:
               NInputNumber clamps out-of-range values silently on blur and
               would round a decimal the same way, while this form must reject
               both with an explicit message (the rule below) so the admin
               knows what was wrong instead of discovering a changed value. -->
          <NInputNumber v-model:value="retentionForm.days" class="gs-retention__days" />
        </NFormItem>
        <div class="settings-card__actions">
          <NButton
            type="primary"
            class="gs-retention__save"
            :loading="retentionSaving"
            :disabled="retentionLoad !== 'ready'"
            @click="saveRetention"
          >
            {{ t('common.save') }}
          </NButton>
        </div>
      </NForm>
    </section>

    <!-- Admin audit retention: the console change trail's own retention,
        independent of request-log retention above (separate setting row,
        separate background loop). Same card lifecycle; changing it is
        itself audited server-side. -->
    <section class="settings-card">
      <h2 class="settings-card__title">{{ t('generalSettings.auditRetention.title') }}</h2>
      <p class="settings-card__desc">{{ t('generalSettings.auditRetention.desc') }}</p>
      <div v-if="auditRetentionLoad === 'loading'" class="settings-card__state">{{ t('common.loading') }}</div>
      <div v-else-if="auditRetentionLoad === 'error'" class="settings-card__state settings-card__state--err">
        <span>{{ t('generalSettings.auditRetention.loadFailed') }}</span>
        <NButton size="small" @click="loadAuditRetention">{{ t('generalSettings.auditRetention.retry') }}</NButton>
      </div>
      <NForm
        v-else
        ref="auditRetentionFormRef"
        :model="auditRetentionForm"
        :rules="auditRetentionRules"
        require-mark-placement="left"
      >
        <NFormItem path="days">
          <template #label>
            <HelpLabel :tip="t('generalSettings.auditRetention.daysTip')">{{
              t('generalSettings.auditRetention.days')
            }}</HelpLabel>
          </template>
          <!-- Same no-silent-clamp contract as the request-log retention
               input above: the rule rejects with an explicit message. -->
          <NInputNumber v-model:value="auditRetentionForm.days" class="gs-audit-retention__days" />
        </NFormItem>
        <div class="settings-card__actions">
          <NButton
            type="primary"
            class="gs-audit-retention__save"
            :loading="auditRetentionSaving"
            :disabled="auditRetentionLoad !== 'ready'"
            @click="saveAuditRetention"
          >
            {{ t('common.save') }}
          </NButton>
        </div>
      </NForm>
    </section>

    <!-- Outbound proxy: the global default for upstream traffic. Same card
         lifecycle as the retention items (authoritative GET, version-CAS
         PUT, 409 → reload and re-save). The system-mode notice renders only
         while that mode is selected — it is the one mode whose knobs this
         console cannot reach (they live in the process environment). -->
    <section class="settings-card">
      <h2 class="settings-card__title">{{ t('generalSettings.outboundProxy.title') }}</h2>
      <p class="settings-card__desc">{{ t('generalSettings.outboundProxy.desc') }}</p>
      <div v-if="proxyLoad === 'loading'" class="settings-card__state">{{ t('common.loading') }}</div>
      <div v-else-if="proxyLoad === 'error'" class="settings-card__state settings-card__state--err">
        <span>{{ t('generalSettings.outboundProxy.loadFailed') }}</span>
        <NButton size="small" @click="loadProxy">{{ t('generalSettings.outboundProxy.retry') }}</NButton>
      </div>
      <NForm v-else ref="proxyFormRef" :model="proxyForm" :rules="proxyRules" require-mark-placement="left">
        <NFormItem path="mode">
          <template #label>
            <HelpLabel :tip="t('generalSettings.outboundProxy.modeTip')">{{
              t('generalSettings.outboundProxy.mode')
            }}</HelpLabel>
          </template>
          <NSelect
            v-model:value="proxyForm.mode"
            class="gs-proxy__mode"
            :options="proxyModeOptions"
            @update:value="onProxyModeSelect"
          />
        </NFormItem>
        <!-- The notice is the system mode's own documentation: env vars are
             process-level, so a container-side change needs a container
             restart — worth saying exactly when the admin picks that mode. -->
        <p v-if="proxyForm.mode === 'system'" class="gs-proxy__system-notice" data-testid="proxy-system-notice">
          {{ t('generalSettings.outboundProxy.systemNotice') }}
        </p>
        <NFormItem path="url">
          <template #label>
            <HelpLabel :tip="t('generalSettings.outboundProxy.urlTip')">{{
              t('generalSettings.outboundProxy.url')
            }}</HelpLabel>
          </template>
          <!-- Kept visible-but-disabled outside custom mode: a previously
               saved URL stays readable (and rides along on save) instead of
               vanishing the moment the mode flips. -->
          <NInput
            v-model:value="proxyForm.url"
            class="gs-proxy__url"
            :placeholder="t('generalSettings.outboundProxy.urlPlaceholder')"
            :disabled="proxyForm.mode !== 'custom'"
          />
        </NFormItem>
        <div class="settings-card__actions">
          <NButton
            type="primary"
            class="gs-proxy__save"
            :loading="proxySaving"
            :disabled="proxyLoad !== 'ready'"
            @click="saveProxy"
          >
            {{ t('common.save') }}
          </NButton>
        </div>
      </NForm>
    </section>

    <!-- Capability enforcement: the six forwarding rules driven by each
         model's declared capability fields, plus the context-limit margin
         they share. Same card lifecycle as the items above, with one
         structural difference: the seven rows behind this card share ONE
         version and save as one snapshot — there is no per-switch PUT, so
         every save carries all six switches and the margin together. -->
    <section class="settings-card">
      <h2 class="settings-card__title">{{ t('generalSettings.capability.title') }}</h2>
      <p class="settings-card__desc">{{ t('generalSettings.capability.desc') }}</p>
      <div v-if="capabilityLoad === 'loading'" class="settings-card__state">{{ t('common.loading') }}</div>
      <div v-else-if="capabilityLoad === 'error'" class="settings-card__state settings-card__state--err">
        <span>{{ t('generalSettings.capability.loadFailed') }}</span>
        <NButton size="small" @click="loadCapability">{{ t('generalSettings.capability.retry') }}</NButton>
      </div>
      <NForm
        v-else
        ref="capabilityFormRef"
        :model="capabilityForm"
        :rules="capabilityRules"
        require-mark-placement="left"
      >
        <NFormItem path="contextLimitBlock">
          <template #label>
            <HelpLabel :tip="t('generalSettings.capability.contextLimitBlockTip')">{{
              t('generalSettings.capability.contextLimitBlock')
            }}</HelpLabel>
          </template>
          <NSwitch v-model:value="capabilityForm.contextLimitBlock" class="gs-capability__switch-context-limit" />
        </NFormItem>
        <NFormItem path="temperatureDefault">
          <template #label>
            <HelpLabel :tip="t('generalSettings.capability.temperatureDefaultTip')">{{
              t('generalSettings.capability.temperatureDefault')
            }}</HelpLabel>
          </template>
          <NSwitch v-model:value="capabilityForm.temperatureDefault" class="gs-capability__switch-temperature-default" />
        </NFormItem>
        <NFormItem path="temperatureForce">
          <template #label>
            <HelpLabel :tip="t('generalSettings.capability.temperatureForceTip')">{{
              t('generalSettings.capability.temperatureForce')
            }}</HelpLabel>
          </template>
          <NSwitch v-model:value="capabilityForm.temperatureForce" class="gs-capability__switch-temperature-force" />
        </NFormItem>
        <NFormItem path="toolsStrict">
          <template #label>
            <HelpLabel :tip="t('generalSettings.capability.toolsStrictTip')">{{
              t('generalSettings.capability.toolsStrict')
            }}</HelpLabel>
          </template>
          <NSwitch v-model:value="capabilityForm.toolsStrict" class="gs-capability__switch-tools-strict" />
        </NFormItem>
        <NFormItem path="reasoningStrict">
          <template #label>
            <HelpLabel :tip="t('generalSettings.capability.reasoningStrictTip')">{{
              t('generalSettings.capability.reasoningStrict')
            }}</HelpLabel>
          </template>
          <NSwitch v-model:value="capabilityForm.reasoningStrict" class="gs-capability__switch-reasoning-strict" />
        </NFormItem>
        <NFormItem path="reasoningEgressClear">
          <template #label>
            <HelpLabel :tip="t('generalSettings.capability.reasoningEgressClearTip')">{{
              t('generalSettings.capability.reasoningEgressClear')
            }}</HelpLabel>
          </template>
          <NSwitch v-model:value="capabilityForm.reasoningEgressClear" class="gs-capability__switch-reasoning-clear" />
        </NFormItem>
        <NFormItem path="margin">
          <template #label>
            <HelpLabel :tip="t('generalSettings.capability.marginTip')">{{
              t('generalSettings.capability.margin')
            }}</HelpLabel>
          </template>
          <!-- Same no-silent-clamp contract as the retention inputs above:
               the rule rejects with an explicit message. -->
          <NInputNumber v-model:value="capabilityForm.margin" class="gs-capability__margin" />
        </NFormItem>
        <div class="settings-card__actions">
          <NButton
            type="primary"
            class="gs-capability__save"
            :loading="capabilitySaving"
            :disabled="capabilityLoad !== 'ready'"
            @click="saveCapability"
          >
            {{ t('common.save') }}
          </NButton>
        </div>
      </NForm>
    </section>

    <!-- Language: personal, per-browser — reads/writes the locale store the
         sidebar Language entry also uses (one source of truth), applies on
         pick. No load state (the store reads synchronously) and no save
         button (nothing is sent anywhere); no rules/path because a picker
         over two options cannot produce an invalid value. -->
    <section class="settings-card">
      <h2 class="settings-card__title">{{ t('generalSettings.language.title') }}</h2>
      <p class="settings-card__desc">{{ t('generalSettings.language.desc') }}</p>
      <NForm require-mark-placement="left">
        <NFormItem>
          <template #label>
            <HelpLabel :tip="t('generalSettings.language.labelTip')">{{
              t('generalSettings.language.label')
            }}</HelpLabel>
          </template>
          <NSelect
            class="gs-language__select"
            :value="localeStore.locale"
            :options="LOCALES"
            @update:value="onLanguageSelect"
          />
        </NFormItem>
      </NForm>
    </section>

    <!-- Key auto recovery: the sidebar modal's form migrated onto this page.
         Same utils (utils/keyAutoRecovery), same i18n message keys, same
         [1, 1440] bounds — only the housing changed. -->
    <section class="settings-card">
      <h2 class="settings-card__title">{{ t('keyAutoRecovery.title') }}</h2>
      <p class="settings-card__desc">{{ t('keyAutoRecovery.desc') }}</p>
      <div v-if="recoveryLoad === 'loading'" class="settings-card__state">{{ t('common.loading') }}</div>
      <div v-else-if="recoveryLoad === 'error'" class="settings-card__state settings-card__state--err">
        <span>{{ t('keyAutoRecovery.loadFailed') }}</span>
        <NButton size="small" @click="loadRecovery">{{ t('keyAutoRecovery.retry') }}</NButton>
      </div>
      <NForm v-else ref="recoveryFormRef" :model="recoveryForm" :rules="recoveryRules" require-mark-placement="left">
        <NFormItem path="enabled">
          <template #label>
            <HelpLabel :tip="t('keyAutoRecovery.enabledTip')">{{ t('keyAutoRecovery.enabled') }}</HelpLabel>
          </template>
          <NSwitch v-model:value="recoveryForm.enabled" class="gs-recovery__switch" />
        </NFormItem>
        <NFormItem path="interval">
          <template #label>
            <HelpLabel :tip="t('keyAutoRecovery.intervalTip')">{{ t('keyAutoRecovery.interval') }}</HelpLabel>
          </template>
          <!-- Same no-clamp contract as the retention input above. -->
          <NInputNumber v-model:value="recoveryForm.interval" class="gs-recovery__interval" />
        </NFormItem>
        <div class="settings-card__actions">
          <NButton
            type="primary"
            class="gs-recovery__save"
            :loading="recoverySaving"
            :disabled="recoveryLoad !== 'ready'"
            @click="saveRecovery"
          >
            {{ t('common.save') }}
          </NButton>
        </div>
      </NForm>
    </section>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  NButton,
  NForm,
  NFormItem,
  NInput,
  NInputNumber,
  NSelect,
  NSwitch,
  useMessage,
  type FormInst,
  type FormRules,
  type SelectOption,
} from 'naive-ui'
import PageHeader from '../../components/PageHeader.vue'
import HelpLabel from '../../components/HelpLabel.vue'
import { useLocaleStore } from '../../store/locale'
import { LOCALES } from '../../i18n'
import { APIError, displayMessage } from '../../api/client'
import {
  getAdminAuditRetention,
  getCapabilityEnforcement,
  getKeyAutoRecovery,
  getOutboundProxy,
  getRequestLogRetention,
  updateAdminAuditRetention,
  updateCapabilityEnforcement,
  updateKeyAutoRecovery,
  updateOutboundProxy,
  updateRequestLogRetention,
} from '../../api/systemSettings'
import {
  ADMIN_AUDIT_RETENTION_CONFLICT,
  CAPABILITY_ENFORCEMENT_CONFLICT,
  KEY_AUTO_RECOVERY_CONFLICT,
  OUTBOUND_PROXY_CONFLICT,
  REQUEST_LOG_RETENTION_CONFLICT,
} from '../../api/errcodes'
import {
  checkRetentionDays,
  retentionFormFromSetting,
  retentionIssueMessageKey,
  retentionSavePlan,
  type RetentionForm,
} from '../../utils/requestLogRetention'
import {
  auditRetentionFormFromSetting,
  auditRetentionIssueMessageKey,
  auditRetentionSavePlan,
  checkAuditRetentionDays,
  type AuditRetentionForm,
} from '../../utils/auditRetention'
import {
  checkIntervalMinutes,
  intervalIssueMessageKey,
  keyRecoveryFormFromSetting,
  keyRecoverySavePlan,
  type KeyRecoveryForm,
} from '../../utils/keyAutoRecovery'
import {
  checkProxyUrl,
  outboundProxyFormFromSetting,
  outboundProxySavePlan,
  proxyUrlIssueMessageKey,
  type OutboundProxyForm,
} from '../../utils/outboundProxy'
import {
  capabilityFormFromSetting,
  capabilitySavePlan,
  checkMarginPercent,
  marginIssueMessageKey,
  type CapabilityEnforcementForm,
} from '../../utils/capabilityEnforcement'

const { t } = useI18n()
const message = useMessage()

// --- Language (personal, per-browser) --------------------------------------

// The same store the sidebar Language entry writes through — reading the
// committed value here and switching through the same action is what keeps
// the two entries in sync (the store persists to localStorage and flips
// i18n's global locale, which re-renders this page's copy too).
const localeStore = useLocaleStore()

function onLanguageSelect(value: string | number) {
  // NSelect hands back the picked option's value; the options come straight
  // from LOCALES, so a match is guaranteed — the find only narrows the type
  // back to Locale.
  const next = LOCALES.find((l) => l.value === value)
  if (next) localeStore.setLocale(next.value)
}

// --- Request log retention ------------------------------------------------

// Three-state load, same as the settings modals: the form stays hidden until
// the GET resolves so a failed load can't expose editable defaults that
// would overwrite the real row on save.
const retentionLoad = ref<'loading' | 'error' | 'ready'>('loading')
const retentionSaving = ref(false)
const retentionVersion = ref(0)
const retentionFormRef = ref<FormInst | null>(null)
const retentionForm = reactive<RetentionForm>({ days: null })

// The rule's message is picked by the pure validator's issue, so the bounds
// live in exactly one place (utils/requestLogRetention). The computed reads
// retentionForm.days through retentionValidationMessage(), so every
// keystroke rebuilds the rule with the message for the value now in the box
// (empty for a valid one) — a message captured once would go stale after the
// first edit.
const retentionRules = computed<FormRules>(() => ({
  days: [
    {
      required: true,
      validator: (_rule, value: number | null) => checkRetentionDays(value).ok,
      message: retentionValidationMessage(),
      trigger: ['blur'],
    },
  ],
}))

function retentionValidationMessage(): string {
  const check = checkRetentionDays(retentionForm.days)
  return check.ok ? '' : t(retentionIssueMessageKey(check.issue))
}

async function loadRetention() {
  retentionLoad.value = 'loading'
  try {
    const s = await getRequestLogRetention()
    const next = retentionFormFromSetting(s)
    retentionForm.days = next.days
    retentionVersion.value = s.version
    retentionFormRef.value?.restoreValidation()
    retentionLoad.value = 'ready'
  } catch (err) {
    retentionLoad.value = 'error'
    if (!(err instanceof APIError)) message.error(displayMessage(err, t))
  }
}

async function saveRetention() {
  // Nothing loaded means nothing valid to save: a PUT here would carry
  // version 0 and bounce off the backend's version check anyway.
  if (retentionLoad.value !== 'ready' || retentionSaving.value) return
  try {
    await retentionFormRef.value?.validate()
  } catch {
    return
  }
  const plan = retentionSavePlan({ days: retentionForm.days }, retentionVersion.value)
  if (!plan.ok) {
    // Defensive double lock: validate() already gates on the same rule, so
    // this only fires if the form ref was somehow absent above.
    message.error(t(retentionIssueMessageKey(plan.issue)))
    return
  }
  retentionSaving.value = true
  try {
    await updateRequestLogRetention(plan.payload)
    message.success(t('generalSettings.retention.saved'))
    // Re-read the committed row (not just the PUT echo) so the form shows
    // the authoritative state — the same authority contract as the GET on
    // load, and it refreshes the version for any subsequent save.
    await loadRetention()
  } catch (err) {
    if (err instanceof APIError && err.code === REQUEST_LOG_RETENTION_CONFLICT) {
      // Concurrent edit — surface it and reload the committed row (which
      // also re-syncs the form and its version).
      message.error(t('generalSettings.retention.conflict'))
      void loadRetention()
    } else {
      message.error(displayMessage(err, t))
    }
  } finally {
    retentionSaving.value = false
  }
}

// --- Admin audit retention --------------------------------------------------

// Identical lifecycle to the request-log retention item above; only the
// setting family (API endpoints, conflict code, message keys, bounds owner
// in utils/auditRetention) differs. Kept as its own block rather than a
// parameterized generic over the two retention items — two explicit blocks
// read clearer than one abstraction with four injected knobs.
const auditRetentionLoad = ref<'loading' | 'error' | 'ready'>('loading')
const auditRetentionSaving = ref(false)
const auditRetentionVersion = ref(0)
const auditRetentionFormRef = ref<FormInst | null>(null)
const auditRetentionForm = reactive<AuditRetentionForm>({ days: null })

const auditRetentionRules = computed<FormRules>(() => ({
  days: [
    {
      required: true,
      validator: (_rule, value: number | null) => checkAuditRetentionDays(value).ok,
      message: auditRetentionValidationMessage(),
      trigger: ['blur'],
    },
  ],
}))

function auditRetentionValidationMessage(): string {
  const check = checkAuditRetentionDays(auditRetentionForm.days)
  return check.ok ? '' : t(auditRetentionIssueMessageKey(check.issue))
}

async function loadAuditRetention() {
  auditRetentionLoad.value = 'loading'
  try {
    const s = await getAdminAuditRetention()
    const next = auditRetentionFormFromSetting(s)
    auditRetentionForm.days = next.days
    auditRetentionVersion.value = s.version
    auditRetentionFormRef.value?.restoreValidation()
    auditRetentionLoad.value = 'ready'
  } catch (err) {
    auditRetentionLoad.value = 'error'
    if (!(err instanceof APIError)) message.error(displayMessage(err, t))
  }
}

async function saveAuditRetention() {
  if (auditRetentionLoad.value !== 'ready' || auditRetentionSaving.value) return
  try {
    await auditRetentionFormRef.value?.validate()
  } catch {
    return
  }
  const plan = auditRetentionSavePlan({ days: auditRetentionForm.days }, auditRetentionVersion.value)
  if (!plan.ok) {
    message.error(t(auditRetentionIssueMessageKey(plan.issue)))
    return
  }
  auditRetentionSaving.value = true
  try {
    await updateAdminAuditRetention(plan.payload)
    message.success(t('generalSettings.auditRetention.saved'))
    await loadAuditRetention()
  } catch (err) {
    if (err instanceof APIError && err.code === ADMIN_AUDIT_RETENTION_CONFLICT) {
      message.error(t('generalSettings.auditRetention.conflict'))
      void loadAuditRetention()
    } else {
      message.error(displayMessage(err, t))
    }
  } finally {
    auditRetentionSaving.value = false
  }
}

// --- Outbound proxy ---------------------------------------------------------

// Identical lifecycle to the retention items above; the setting family is
// its own endpoint + conflict code (11025). The URL rule's bounds stay
// owned by utils/outboundProxy — the client pre-checks syntax (parseable,
// host, supported scheme) and the server remains the authority, additionally
// refusing reserved-range addresses the browser cannot judge from the
// router's network position.
const proxyLoad = ref<'loading' | 'error' | 'ready'>('loading')
const proxySaving = ref(false)
const proxyVersion = ref(0)
const proxyFormRef = ref<FormInst | null>(null)
const proxyForm = reactive<OutboundProxyForm>({ mode: 'off', url: '' })

const proxyModeOptions = computed<SelectOption[]>(() => [
  { value: 'off', label: t('generalSettings.outboundProxy.modeOff') },
  { value: 'system', label: t('generalSettings.outboundProxy.modeSystem') },
  { value: 'custom', label: t('generalSettings.outboundProxy.modeCustom') },
])

const proxyRules = computed<FormRules>(() => ({
  url: [
    {
      // Required only under custom mode; a parked URL elsewhere is still
      // syntax-checked (the same rule the server enforces), but clearing it
      // under off/system is legal.
      required: true,
      validator: (_rule, value: string) =>
        proxyForm.mode !== 'custom' || checkProxyUrl(value).ok,
      message: proxyValidationMessage(),
      trigger: ['blur'],
    },
  ],
}))

function proxyValidationMessage(): string {
  const check = checkProxyUrl(proxyForm.url)
  return check.ok ? '' : t(proxyUrlIssueMessageKey('settings', check.issue))
}

// Selecting a mode only clears the previous mode's validation feedback:
// the URL rule is mode-dependent (required under custom alone), so a
// message left by an earlier failed save must not linger once the stance
// changed. No eager validation runs here — the message (re)appears at the
// next save click.
function onProxyModeSelect() {
  void proxyFormRef.value?.restoreValidation()
}

async function loadProxy() {
  proxyLoad.value = 'loading'
  try {
    const s = await getOutboundProxy()
    const next = outboundProxyFormFromSetting(s)
    proxyForm.mode = next.mode
    proxyForm.url = next.url
    proxyVersion.value = s.version
    proxyFormRef.value?.restoreValidation()
    proxyLoad.value = 'ready'
  } catch (err) {
    proxyLoad.value = 'error'
    if (!(err instanceof APIError)) message.error(displayMessage(err, t))
  }
}

async function saveProxy() {
  if (proxyLoad.value !== 'ready' || proxySaving.value) return
  try {
    await proxyFormRef.value?.validate()
  } catch {
    return
  }
  const plan = outboundProxySavePlan({ mode: proxyForm.mode, url: proxyForm.url }, proxyVersion.value)
  if (!plan.ok) {
    // Defensive double lock, same as the retention item.
    message.error(t(proxyUrlIssueMessageKey('settings', plan.issue)))
    return
  }
  proxySaving.value = true
  try {
    await updateOutboundProxy(plan.payload)
    message.success(t('generalSettings.outboundProxy.saved'))
    await loadProxy()
  } catch (err) {
    if (err instanceof APIError && err.code === OUTBOUND_PROXY_CONFLICT) {
      message.error(t('generalSettings.outboundProxy.conflict'))
      void loadProxy()
    } else {
      message.error(displayMessage(err, t))
    }
  } finally {
    proxySaving.value = false
  }
}

// --- Capability enforcement -------------------------------------------------

// Identical lifecycle to the items above; the setting family is its own
// endpoint + conflict code (11027). The six switches are booleans straight
// off NSwitch, so the margin is the only validated field — its bounds stay
// owned by utils/capabilityEnforcement, the module this card shares with
// its tests. The seven rows behind the endpoint share one version, so a
// save always PUTs the whole snapshot (all six switches + margin + the
// CAS version), never a per-switch patch.
const capabilityLoad = ref<'loading' | 'error' | 'ready'>('loading')
const capabilitySaving = ref(false)
const capabilityVersion = ref(0)
const capabilityFormRef = ref<FormInst | null>(null)
const capabilityForm = reactive<CapabilityEnforcementForm>({
  contextLimitBlock: false,
  temperatureDefault: false,
  temperatureForce: false,
  toolsStrict: false,
  reasoningStrict: false,
  reasoningEgressClear: false,
  margin: null,
})

const capabilityRules = computed<FormRules>(() => ({
  margin: [
    {
      required: true,
      validator: (_rule, value: number | null) => checkMarginPercent(value).ok,
      message: capabilityValidationMessage(),
      trigger: ['blur'],
    },
  ],
}))

function capabilityValidationMessage(): string {
  const check = checkMarginPercent(capabilityForm.margin)
  return check.ok ? '' : t(marginIssueMessageKey(check.issue))
}

async function loadCapability() {
  capabilityLoad.value = 'loading'
  try {
    const s = await getCapabilityEnforcement()
    const next = capabilityFormFromSetting(s)
    capabilityForm.contextLimitBlock = next.contextLimitBlock
    capabilityForm.temperatureDefault = next.temperatureDefault
    capabilityForm.temperatureForce = next.temperatureForce
    capabilityForm.toolsStrict = next.toolsStrict
    capabilityForm.reasoningStrict = next.reasoningStrict
    capabilityForm.reasoningEgressClear = next.reasoningEgressClear
    capabilityForm.margin = next.margin
    capabilityVersion.value = s.version
    capabilityFormRef.value?.restoreValidation()
    capabilityLoad.value = 'ready'
  } catch (err) {
    capabilityLoad.value = 'error'
    if (!(err instanceof APIError)) message.error(displayMessage(err, t))
  }
}

async function saveCapability() {
  if (capabilityLoad.value !== 'ready' || capabilitySaving.value) return
  try {
    await capabilityFormRef.value?.validate()
  } catch {
    return
  }
  const plan = capabilitySavePlan(
    {
      contextLimitBlock: capabilityForm.contextLimitBlock,
      temperatureDefault: capabilityForm.temperatureDefault,
      temperatureForce: capabilityForm.temperatureForce,
      toolsStrict: capabilityForm.toolsStrict,
      reasoningStrict: capabilityForm.reasoningStrict,
      reasoningEgressClear: capabilityForm.reasoningEgressClear,
      margin: capabilityForm.margin,
    },
    capabilityVersion.value,
  )
  if (!plan.ok) {
    // Defensive double lock, same as the retention item.
    message.error(t(marginIssueMessageKey(plan.issue)))
    return
  }
  capabilitySaving.value = true
  try {
    await updateCapabilityEnforcement(plan.payload)
    message.success(t('generalSettings.capability.saved'))
    await loadCapability()
  } catch (err) {
    if (err instanceof APIError && err.code === CAPABILITY_ENFORCEMENT_CONFLICT) {
      message.error(t('generalSettings.capability.conflict'))
      void loadCapability()
    } else {
      message.error(displayMessage(err, t))
    }
  } finally {
    capabilitySaving.value = false
  }
}

// --- Key auto recovery ----------------------------------------------------

// Identical lifecycle to the retention item above; only the setting family
// (form shape, API, conflict code) differs. The interval bounds and their
// messages stay owned by utils/keyAutoRecovery — the module the migrated
// modal already used, unchanged.
const recoveryLoad = ref<'loading' | 'error' | 'ready'>('loading')
const recoverySaving = ref(false)
const recoveryVersion = ref(0)
const recoveryFormRef = ref<FormInst | null>(null)
const recoveryForm = reactive<KeyRecoveryForm>({ enabled: false, interval: null })

const recoveryRules = computed<FormRules>(() => ({
  interval: [
    {
      required: true,
      validator: (_rule, value: number | null) => checkIntervalMinutes(value).ok,
      message: intervalValidationMessage(),
      trigger: ['blur'],
    },
  ],
}))

function intervalValidationMessage(): string {
  const check = checkIntervalMinutes(recoveryForm.interval)
  return check.ok ? '' : t(intervalIssueMessageKey(check.issue))
}

async function loadRecovery() {
  recoveryLoad.value = 'loading'
  try {
    const s = await getKeyAutoRecovery()
    const next = keyRecoveryFormFromSetting(s)
    recoveryForm.enabled = next.enabled
    recoveryForm.interval = next.interval
    recoveryVersion.value = s.version
    recoveryFormRef.value?.restoreValidation()
    recoveryLoad.value = 'ready'
  } catch (err) {
    recoveryLoad.value = 'error'
    if (!(err instanceof APIError)) message.error(displayMessage(err, t))
  }
}

async function saveRecovery() {
  if (recoveryLoad.value !== 'ready' || recoverySaving.value) return
  try {
    await recoveryFormRef.value?.validate()
  } catch {
    return
  }
  const plan = keyRecoverySavePlan(
    { enabled: recoveryForm.enabled, interval: recoveryForm.interval },
    recoveryVersion.value,
  )
  if (!plan.ok) {
    message.error(t(intervalIssueMessageKey(plan.issue)))
    return
  }
  recoverySaving.value = true
  try {
    await updateKeyAutoRecovery(plan.payload)
    message.success(t('keyAutoRecovery.saved'))
    await loadRecovery()
  } catch (err) {
    if (err instanceof APIError && err.code === KEY_AUTO_RECOVERY_CONFLICT) {
      message.error(t('keyAutoRecovery.conflict'))
      void loadRecovery()
    } else {
      message.error(displayMessage(err, t))
    }
  } finally {
    recoverySaving.value = false
  }
}

// The page is a persistent surface (no open/close transition to hook), so
// both settings load once on mount. Saves re-read through the same loaders.
onMounted(() => {
  void loadRetention()
  void loadAuditRetention()
  void loadProxy()
  void loadCapability()
  void loadRecovery()
})
</script>

<style scoped>
.common-page {
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
}

.settings-card {
  max-width: 560px;
  padding: var(--space-5) var(--space-6);
  background: var(--color-surface);
  border: 1px solid var(--color-border-subtle);
  border-radius: var(--radius-lg);
}

.settings-card__title {
  margin: 0;
  font-size: 15px;
  font-weight: 600;
  color: var(--color-text);
}

.settings-card__desc {
  margin: var(--space-1) 0 var(--space-4);
  font-size: 13px;
  line-height: 1.6;
  color: var(--color-text-secondary);
}

.settings-card__state {
  display: flex;
  align-items: center;
  gap: 12px;
  color: var(--color-text-muted);
  font-size: var(--text-sm);
}

.settings-card__state--err {
  color: var(--color-text-secondary);
}

.settings-card__actions {
  display: flex;
  justify-content: flex-end;
  margin-top: var(--space-2);
}

/* The switch's form-item row reads cleaner with the toggle pinned right,
   matching the migrated modal's block-switch treatment — the capability
   card's six switches take the same stance. */
:deep(.gs-recovery__switch),
:deep([class*='gs-capability__switch-']) {
  margin-left: auto;
}

/* The system-mode notice is a mode-scoped caveat, not a form error: quiet
   callout styling instead of the alert component's loud framing. */
.gs-proxy__system-notice {
  margin: 0 0 var(--space-3);
  padding: var(--space-2) var(--space-3);
  font-size: 12px;
  line-height: 1.6;
  color: var(--color-text-secondary);
  background: var(--color-surface-raised);
  border: 1px solid var(--color-border-subtle);
  border-radius: var(--radius-md);
}
</style>
