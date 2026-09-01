<!-- frontend/src/components/models/CandidateEditModal.vue -->
<template>
  <ModalDrawer
    v-model:show="showModel"
    :title="editingCandidate ? t('models.editCandidate') : t('models.addCandidate')"
    max-width="520px"
    :mask-closable="false"
    :close-on-esc="false"
    :back-label="t('models.cancel')"
  >
    <div v-if="modelName" class="outward-model">
      <span class="outward-model__label">{{ t('models.name') }}</span>
      <span class="outward-model__value">{{ modelName }}</span>
    </div>
    <n-form require-mark-placement="left" ref="formRef" :model="form" :rules="rules">
      <n-form-item v-if="!editingCandidate" path="providerId">
        <template #label>
          <div class="label-row">
            <HelpLabel :tip="t('models.provider_tip')">{{ t('models.provider') }}</HelpLabel>
            <n-button text type="primary" size="tiny" @click="openNewProviderModal">
              {{ t('providers.createButton') }}
            </n-button>
          </div>
        </template>
        <FilterSelectField
          v-model:value="form.providerId"
          :label="t('models.provider')"
          :options="providerOptions"
          :placeholder="t('models.provider')"
          :clearable="false"
          width="100%"
          class="w-full"
          size="medium"
        />
      </n-form-item>
      <n-form-item path="providerModelName">
        <template #label>
          <HelpLabel :tip="t('models.providerModelName_tip')">{{ t('models.providerModelName') }}</HelpLabel>
        </template>
        <FilterSelectField
          v-model:value="providerModelName"
          :label="t('models.providerModelName')"
          :options="modelOptions"
          :loading="loadingModels"
          filterable
          tag
          clearable
          :placeholder="t('models.providerModelNameHint')"
          width="100%"
          class="w-full"
          size="medium"
        />
      </n-form-item>
      <div class="price-grid">
      <n-form-item path="inputPrice">
        <template #label>
          <HelpLabel :tip="t('models.inputPrice_tip')">{{ t('models.inputPrice') }}</HelpLabel>
        </template>
        <n-input-number
          :value="form.inputPrice"
          :min="0"
          style="width: 100%"
          @update:value="(v: number | null) => onPriceInput('inputPrice', v)"
        />
      </n-form-item>
      <n-form-item path="outputPrice">
        <template #label>
          <HelpLabel :tip="t('models.outputPrice_tip')">{{ t('models.outputPrice') }}</HelpLabel>
        </template>
        <n-input-number
          :value="form.outputPrice"
          :min="0"
          style="width: 100%"
          @update:value="(v: number | null) => onPriceInput('outputPrice', v)"
        />
      </n-form-item>
      <n-form-item>
        <template #label>
          <HelpLabel :tip="t('models.cacheWritePrice_tip')">{{ t('models.cacheWritePrice') }}</HelpLabel>
        </template>
        <n-input-number
          :value="form.cacheWritePrice"
          :min="0"
          style="width: 100%"
          @update:value="(v: number | null) => onPriceInput('cacheWritePrice', v)"
        />
      </n-form-item>
      <n-form-item>
        <template #label>
          <HelpLabel :tip="t('models.cacheReadPrice_tip')">{{ t('models.cacheReadPrice') }}</HelpLabel>
        </template>
        <n-input-number
          :value="form.cacheReadPrice"
          :min="0"
          style="width: 100%"
          @update:value="(v: number | null) => onPriceInput('cacheReadPrice', v)"
        />
      </n-form-item>
      <n-form-item>
        <template #label>
          <HelpLabel :tip="t('models.maxOutput_tip')">{{ t('models.maxOutput') }}</HelpLabel>
        </template>
        <n-input-number v-model:value="form.maxOutput" :min="0" style="width: 100%" />
      </n-form-item>
      </div>
      <n-form-item>
        <template #label>
          <HelpLabel :tip="t('models.statusEnabled_tip')">{{ t('models.statusEnabled') }}</HelpLabel>
        </template>
        <n-switch v-model:value="form.enabled" @update:value="statusTouched = true" />
      </n-form-item>
      <n-form-item>
        <template #label>
          <HelpLabel :tip="t('models.billingMode_tip')">{{ t('models.billingMode') }}</HelpLabel>
        </template>
        <n-select v-model:value="form.billingMode" :options="billingModeOptions" />
      </n-form-item>
      <div v-if="form.billingMode === 'image'" class="tiers-section">
        <div class="tiers-section__head">
          <span class="tiers-section__label">{{ t('models.imageTiers') }}</span>
          <n-button size="small" @click="addTier">{{ t('models.imageTierAdd') }}</n-button>
        </div>
        <!-- One row per quality×size price. Quality/size left empty are
             wildcards server-side; first match wins, so rows keep the order
             they were entered in. -->
        <div v-for="(tier, i) in form.imageTiers" :key="i" class="tier-row">
          <n-input v-model:value="tier.quality" :placeholder="t('models.imageTierQuality')" />
          <n-input v-model:value="tier.size" :placeholder="t('models.imageTierSize')" />
          <n-input-number
            v-model:value="tier.price"
            :min="0"
            :placeholder="t('models.imageTierPrice')"
            style="width: 100%"
          />
          <n-button quaternary size="small" :aria-label="t('models.imageTierRemove')" @click="removeTier(i)">
            ✕
          </n-button>
        </div>
        <n-form-item>
          <template #label>
            <HelpLabel :tip="t('models.imageDefaultPrice_tip')">{{ t('models.imageDefaultPrice') }}</HelpLabel>
          </template>
          <n-input-number v-model:value="form.imageDefaultPrice" :min="0" style="width: 100%" />
        </n-form-item>
        <n-alert v-if="tierErrorKey" type="warning" style="margin-top: 4px">
          {{ t(tierErrorKey) }}
        </n-alert>
      </div>
    </n-form>

    <!-- Rendered in place rather than as a toast: this one blocks saving, so it
         has to stay on screen until the operator answers it. -->
    <n-alert v-if="priceUnresolvedKey" type="warning" style="margin-top: 4px">
      <div class="price-alert">
        <span>{{ t(priceUnresolvedKey) }}</span>
        <n-button text type="primary" size="small" @click="keepUnresolvedPrices">
          {{ t('models.priceKeepAnyway') }}
        </n-button>
      </div>
    </n-alert>

    <!-- Only rendered once there is something to report: while idle the modal
         stays a plain form, matching the provider and key dialogs. -->
    <div v-if="submitting || report" class="probe-section">
      <div v-if="submitting" class="probe-section__pending">
        <n-spin size="small" />
        <div class="probe-section__pending-text">
          <div>{{ t('models.probing') }}</div>
          <div class="probe-section__hint">{{ t('models.probingHint') }}</div>
        </div>
      </div>
      <template v-else-if="report">
        <div class="probe-section__label">{{ t('models.probeResultTitle') }}</div>
        <div v-for="row in probeRows" :key="row.label" class="probe-row">
          <span class="probe-row__icon" :class="`probe-row__icon--${row.tone}`">{{ row.icon }}</span>
          <span class="probe-row__label">{{ row.label }}</span>
          <span class="probe-row__verdict" :class="`probe-row__verdict--${row.tone}`">{{ row.verdict }}</span>
        </div>
        <n-alert v-if="alert" :type="alert.type" style="margin-top: 12px">
          {{ alert.text }}
        </n-alert>
      </template>
    </div>

    <template #footer>
      <n-space justify="end">
        <!-- Once the candidate is stored, the only remaining action is to close:
             re-submitting would hit the one-candidate-per-provider constraint and
             report "provider already used" for a row this modal just saved. -->
        <template v-if="persisted">
          <n-button type="primary" @click="onUpdateShow(false)">{{ t('models.done') }}</n-button>
        </template>
        <template v-else>
          <n-button :disabled="submitting" @click="onUpdateShow(false)">{{ t('models.cancel') }}</n-button>
          <n-tooltip v-if="basicFailedBlockingSave" trigger="hover" placement="top">
            <template #trigger>
              <n-button
                :loading="savingDisabled"
                :disabled="submitting || priceUnresolvedKey !== null"
                @click="onSaveAnywayDisabled"
              >
                {{ t('models.saveAnywayDisabled') }}
              </n-button>
            </template>
            {{ t('models.saveAnywayDisabled_tip') }}
          </n-tooltip>
          <n-tooltip :disabled="priceUnresolvedKey === null" trigger="hover" placement="top">
            <template #trigger>
              <!-- A disabled <button> fires no hover events, so the tooltip
                   hangs off a wrapper span: without it the greyed-out Save has
                   no explanation anywhere the operator is looking. -->
              <span>
                <n-button
                  type="primary"
                  :loading="submitting"
                  :disabled="savingDisabled || priceUnresolvedKey !== null"
                  @click="onSave"
                >
                  {{ basicFailedBlockingSave ? t('models.retry') : t('models.save') }}
                </n-button>
              </span>
            </template>
            {{ priceUnresolvedKey ? t(priceUnresolvedKey) : '' }}
          </n-tooltip>
        </template>
      </n-space>
    </template>
  </ModalDrawer>

  <NewProviderModal v-model:show="showNewProviderModal" @created="onProviderCreated" />
</template>

<script setup lang="ts">
import { computed, h, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
// NSpin and NSwitch are not in main.ts's create() registry, so they must be
// imported explicitly or Vue renders them as inert unknown elements.
import {
  NButton,
  NSpin,
  NSwitch,
  NTooltip,
  useMessage,
  type FormInst,
  type FormRules,
  type MessageReactive,
} from 'naive-ui'
import { useModelsStore } from '../../store/models'
import { useProvidersStore } from '../../store/providers'
import type { Provider } from '../../api/providers'
import { displayMessage } from '../../api/client'
import { providerModelNameRule, nonNegativePriceRule } from '../../utils/modelValidators'
import { capabilityState } from '../../utils/modelStatusDisplay'
import { testOutcomeI18nKey } from '../../utils/testOutcomeDisplay'
import { providerRunningStatusDisplay, usableKeyCount } from '../../utils/providerStatusDisplay'
import HelpLabel from '../HelpLabel.vue'
import ModalDrawer from '../common/ModalDrawer.vue'
import FilterSelectField from '../common/FilterSelectField.vue'
import NewProviderModal from '../providers/NewProviderModal.vue'
import type {
  CandidateTestReport,
  ImagePricingTier,
  ModelCandidate,
  ProbeReport,
  SuggestedPrice,
} from '../../api/models'
import { suggestCandidatePrice } from '../../api/models'
import { CANDIDATE_STATUS_DISABLED, CANDIDATE_STATUS_ENABLED } from '../../api/candidateStatus'

const props = defineProps<{
  show: boolean
  modelId: number
  // modelName is the outward model this candidate maps to, shown read-only so
  // the admin knows which model they are configuring — and, since a blank
  // provider model name defaults to it, which name a mapping falls back to.
  modelName?: string
  editingCandidate?: ModelCandidate | null
}>()
const emit = defineEmits<{ 'update:show': [boolean]; saved: []; retest: [number] }>()

// ModalDrawer owns a v-model:show; bridge it to this component's existing
// :show / @update:show contract. The footer still calls onUpdateShow directly
// for its explicit buttons; this only handles the drawer's own dismiss paths
// (back arrow / mask), routing them through the same emit.
const showModel = computed({
  get: () => props.show,
  set: (v) => {
    if (!v) onUpdateShow(false)
  },
})

const { t } = useI18n()
const message = useMessage()
const store = useModelsStore()
const providersStore = useProvidersStore()

const formRef = ref<FormInst | null>(null)
// submitting covers the probe-and-save round trip; savingDisabled covers the
// separate no-probe "save as disabled anyway" call, so each button spins on its
// own action instead of both reacting to one shared flag.
const submitting = ref(false)
const savingDisabled = ref(false)
const report = ref<CandidateTestReport | null>(null)
// persisted records that this modal has already written the candidate. It gates
// the footer down to a single close action, because a second submit would try to
// create the same provider mapping again and fail on the uniqueness constraint.
const persisted = ref(false)
// saveSeq is bumped whenever the dialog opens, so a save whose probe outlives its
// own opening can tell that its results are no longer wanted.
let saveSeq = 0
// The same guard for the price look-up, which is fired by picking a model name
// and can just as easily outlive the opening that started it.
let priceFillSeq = 0
// The look-up currently in flight, so a save can wait for it. Saving is far more
// likely than not to be the very next thing the admin does after picking a
// model, and a payload built while the prices are still arriving would store the
// previous mapping's rates while the form goes on to display the new ones.
let pendingPriceFill: Promise<void> | null = null

const showNewProviderModal = ref(false)

// The numeric fields are `number | null` because that is what NInputNumber
// itself uses for "empty" — it emits null when the box is cleared. Typing them
// as plain numbers let a null through unchecked and made "untouched" impossible
// to tell from "cleared".
const form = reactive({
  providerId: null as number | null,
  providerModelName: '',
  inputPrice: 0 as number | null,
  outputPrice: 0 as number | null,
  cacheWritePrice: null as number | null,
  cacheReadPrice: null as number | null,
  maxOutput: 0 as number | null,
  enabled: true,
  billingMode: 'token' as 'token' | 'image',
  imageTiers: [] as ImagePricingTier[],
  imageDefaultPrice: null as number | null,
})

const billingModeOptions = computed(() => [
  { label: t('models.billingModeToken'), value: 'token' },
  { label: t('models.billingModeImage'), value: 'image' },
])

function addTier() {
  form.imageTiers.push({ quality: '', size: '', price: 0 })
}

function removeTier(index: number) {
  form.imageTiers.splice(index, 1)
}

// tierErrorKey holds an i18n key while the image declaration cannot price a
// request — no tier, no default — or a price is negative. Blocking is local:
// the server re-validates, this exists so the operator never has to round-trip
// to learn the table is empty.
const tierErrorKey = computed<string | null>(() => {
  if (form.billingMode !== 'image') return null
  const hasTier = form.imageTiers.length > 0
  const hasDefault = form.imageDefaultPrice !== null
  if (!hasTier && !hasDefault) return 'models.imageTiersEmpty'
  if (form.imageTiers.some((tier) => tier.price < 0)) return 'models.imageTierNegative'
  return null
})

type PriceField = 'inputPrice' | 'outputPrice' | 'cacheWritePrice' | 'cacheReadPrice'

// For each price field, the provider+model pair that was selected when the admin
// last changed it by hand — or null if they never did. Auto-fill never
// overwrites a hand-entered rate: it exists to save typing, not to overrule
// someone who read the number off a vendor's pricing page. Which pair the edit
// belongs to still matters, because an edit made for a DIFFERENT pair than the
// one now suggested means the form is about to hold two models' rates at once,
// and that has to be said out loud rather than silently preserved.
const priceEditedAt = reactive<Record<PriceField, string | null>>({
  inputPrice: null,
  outputPrice: null,
  cacheWritePrice: null,
  cacheReadPrice: null,
})

const PRICE_FIELDS: PriceField[] = ['inputPrice', 'outputPrice', 'cacheWritePrice', 'cacheReadPrice']

// Whether the price fields stand for a specific provider+model pair rather than
// being the untouched defaults of a blank form. Tracked rather than inferred
// from the values because zero is a real rate — a free or self-hosted model —
// and a form showing 0/0 for the previously selected pair still needs the
// "check these before saving" warning when the next pair has no known price.
let pricesDescribeAnotherPair = false

// Holds an i18n key while the prices on screen have not been shown to apply to
// the pair now selected — the look-up either failed or knows no price for it,
// and what the fields hold was established for a different provider or model.
// Saving is blocked until the operator edits a price or explicitly keeps it:
// these numbers feed cost accounting and API-key budgets, and a warning that
// scrolls away in a toast is no protection for someone who already clicked Save.
const priceUnresolvedKey = ref<string | null>(null)

// Bound instead of v-model so an edit is recorded as it happens. NInputNumber
// suppresses its own update when the value would not change, so focusing a
// field and tabbing out does not count as an edit — only a real change does.
function onPriceInput(field: PriceField, value: number | null) {
  form[field] = value
  priceEditedAt[field] = currentPairKey()
  pricesDescribeAnotherPair = true
  // Touching a price is the operator looking at it, which is all the block was
  // ever waiting for.
  priceUnresolvedKey.value = null
}

// The explicit way out for prices that are already correct — a provider whose
// rate the catalog has never heard of, typed once and reused.
function keepUnresolvedPrices() {
  priceUnresolvedKey.value = null
}

function resetPriceEdited() {
  for (const field of PRICE_FIELDS) priceEditedAt[field] = null
}

// The basic probe having failed while the admin asked to enable is the one state
// that blocks saving: nothing was stored, so the footer offers a retry and the
// disabled-anyway escape hatch instead of a plain save.
// The mapping was probed and rejected. Drives the explanation shown to the
// operator, independently of whether anything was stored.
const basicProbeFailed = computed(() => {
  const r = report.value
  return r !== null && r.basic.ran && !probePassed(r.basic)
})

// Nothing could be stored: enablement was asked for and the mapping does not
// work, so the create path wrote nothing. Drives the footer's retry / save-as-
// disabled escape hatch, which only makes sense before anything is persisted.
const basicFailedBlockingSave = computed(
  () => !persisted.value && form.enabled && basicProbeFailed.value,
)

const hasUnknownCapability = computed(() => {
  const r = report.value
  if (!r) return false
  return [r.streaming, r.function_calling].some((p) => p.ran && p.supported === null)
})

// One alert explaining the outcome, picked by what actually went wrong so the
// operator is never pointed at a fault that is not theirs to fix.
const alert = computed<{ type: 'error' | 'warning' | 'info'; text: string } | null>(() => {
  const r = report.value
  if (!r) return null
  if (!r.basic.ran) {
    // Nothing was probed — the provider has no usable key yet, which is a
    // different problem from a mapping that was tested and rejected.
    return { type: 'warning', text: t('models.probeNotTestedHint') }
  }
  if (basicProbeFailed.value) {
    // Two different situations: nothing was stored (create, enable requested),
    // or the edit was stored but could not be left enabled.
    return {
      type: 'error',
      text: persisted.value ? t('models.probeFailedSavedDisabledHint') : t('models.probeBasicFailedHint'),
    }
  }
  if (hasUnknownCapability.value) return { type: 'info', text: t('models.probeUnconfirmedHint') }
  return null
})

function probePassed(p: ProbeReport): boolean {
  return p.ran && p.supported === true
}

type ProbeTone = 'supported' | 'unsupported' | 'unknown' | 'untested'

// One display row per probe. The basic probe is binary (it either proves the
// mapping works or it does not), while the capability probes carry the tri-state
// so an inconclusive result never reads as a proven "not supported".
const probeRows = computed(() => {
  const r = report.value
  if (!r) return []
  return [
    basicRow(r.basic),
    capabilityRow(t('models.supportsStreaming'), r.streaming),
    capabilityRow(t('models.supportsFunctionCalling'), r.function_calling),
  ]
})

function basicRow(p: ProbeReport) {
  const label = t('models.basicText')
  if (probePassed(p)) {
    return { label, icon: '✓', tone: 'supported' as ProbeTone, verdict: t('models.testPassed') }
  }
  // A probe that never ran must not be rendered as a failure. The server
  // reports this when it could not probe at all — no usable provider key yet —
  // and claiming "test failed" would send the operator looking for a fault in
  // the model name or credential that the probe never examined.
  if (!p.ran) {
    return { label, icon: '—', tone: 'untested' as ProbeTone, verdict: t('models.probeNotTested') }
  }
  return { label, icon: '✗', tone: 'unsupported' as ProbeTone, verdict: failureVerdict(p) }
}

function capabilityRow(label: string, p: ProbeReport) {
  if (!p.ran) {
    return { label, icon: '—', tone: 'untested' as ProbeTone, verdict: t('models.probeNotTested') }
  }
  switch (capabilityState(p.supported)) {
    case 'confirmed':
      return { label, icon: '✓', tone: 'supported' as ProbeTone, verdict: t('models.probeConfirmed') }
    case 'unsupported':
      return { label, icon: '✗', tone: 'unsupported' as ProbeTone, verdict: t('models.probeUnsupported') }
    default:
      return { label, icon: '?', tone: 'unknown' as ProbeTone, verdict: t('models.probeUnconfirmed') }
  }
}

// Names the specific upstream reason when one is known, so a wrong model name is
// distinguishable from a bad key or an unreachable address.
function failureVerdict(p: ProbeReport): string {
  if (p.outcome === null || p.outcome === undefined) return t('models.testFailed')
  return `${t('models.testFailed')}: ${t(`providers.${testOutcomeI18nKey(p.outcome)}`)}`
}

// Each option carries the provider's state alongside its name: a manually
// disabled provider says so outright (its running status may still read
// fine, but no candidate on it can route), otherwise the live running
// status; plus how many keys are actually usable for routing — so a
// provider that will silently fail is visible before the candidate is saved.
const providerOptions = computed(() =>
  providersStore.list.map((p) => ({
    label: t('models.candidateProviderOption', {
      name: p.name,
      status:
        p.management_status === 1
          ? t(`providers.running${providerRunningStatusDisplay(p.running_status).i18nKey}`)
          : t('providers.statusDisabled'),
      n: usableKeyCount(p.keys),
    }),
    value: p.id,
    disabled: false,
  })),
)

// Model-name picker: the catalogue is fetched lazily for the selected provider
// and merged with the current value so a value not in the catalogue (a custom
// tag, or an edited candidate's stored name) still renders. The field remains
// a free-text combobox (filterable + tag), so a failed/empty fetch degrades to
// manual entry rather than blocking the field.
const fetchedModels = ref<string[]>([])
const loadingModels = ref(false)
let modelFetchSeq = 0

const modelOptions = computed(() => {
  const names = new Set(fetchedModels.value)
  if (form.providerModelName) names.add(form.providerModelName)
  return Array.from(names, (m) => ({ label: m, value: m }))
})

// NSelect emits null when cleared; keep form.providerModelName a string and
// treat blank as null so the placeholder ("blank = use the model name itself")
// shows instead of an empty-string value.
const providerModelName = computed<string | null>({
  get: () => form.providerModelName || null,
  set: (value) => {
    form.providerModelName = value ?? ''
  },
})

// The name that will actually be sent upstream. A blank field means "use the
// model's own name", and the server makes that substitution when the candidate
// is saved — so the price look-up has to make it too, or the default flow (pick
// a provider, leave the name blank) would never be priced at all.
const effectiveProviderModelName = computed(
  () => form.providerModelName.trim() || (props.modelName ?? '').trim(),
)

// The provider+model pair the price fields currently correspond to. Seeding the
// form records the pair it seeded, so merely opening an existing candidate does
// not re-price it; any later change to either half is a different pair and gets
// a fresh look-up. Prices follow the provider AND the model, so switching either
// one leaves the fields describing something the candidate no longer is.
let pricedPairKey = ''
function pairKey(providerId: number | null, modelName: string): string {
  // The separator cannot occur in either half: an id is digits and a model
  // name is trimmed, so a space keeps two different pairs from colliding.
  return `${providerId ?? 0} ${modelName.toLowerCase()}`
}

function currentPairKey(): string {
  return pairKey(form.providerId, effectiveProviderModelName.value)
}

watch([() => form.providerId, effectiveProviderModelName], ([providerId, modelName]) => {
  // The modal is kept alive between openings, so a parent-driven prop change
  // can move this pair while nothing is on screen. The next opening seeds the
  // pair itself, which makes a look-up now both invisible and pointless.
  if (!props.show || !providerId || !modelName) return
  const key = pairKey(providerId, modelName)
  if (key === pricedPairKey) return
  pricedPairKey = key
  // The look-up about to run is the authority on this pair; any verdict left
  // over from the previous one says nothing about it.
  priceUnresolvedKey.value = null

  const seq = ++priceFillSeq
  pendingPriceFill = suggestCandidatePrice(providerId, modelName)
    .then(
      (s) => {
        // A newer look-up started, or the dialog was closed or reopened —
        // either way these prices describe a pair the form no longer shows.
        if (seq !== priceFillSeq || !props.show) return
        applySuggestion(s, key)
      },
      () => {
        // A rejection handler rather than a trailing .catch: chained after the
        // fulfilled path it would also swallow anything applySuggestion throws
        // — a missing i18n key, a toast fired after its provider is gone — and
        // report a look-up that actually succeeded as a failed one.
        //
        // Auto-fill is a convenience and its failure never blocks a blank form.
        // But prices left over from another pair are now unvouched for, and the
        // operator is the only one who can say whether they still apply.
        if (seq !== priceFillSeq || !props.show) return
        if (pricesDescribeAnotherPair) priceUnresolvedKey.value = 'models.priceLookupFailed'
      },
    )
    .finally(() => {
      if (seq === priceFillSeq) pendingPriceFill = null
    })
})

function applySuggestion(s: SuggestedPrice, key: string) {
  if (!s.source) {
    // Nothing is known about this pair, so there is nothing to fill. Whatever
    // the fields hold was established for a different provider or model, and
    // silently keeping it is how traffic ends up billed at another model's rate
    // — say so instead. This includes a deliberate zero, which is a real rate
    // ("free"), not an empty field.
    if (pricesDescribeAnotherPair) priceUnresolvedKey.value = 'models.priceCheckAfterChange'
    return
  }
  // A field typed by hand for an EARLIER pair is kept — that number was
  // deliberate, often read straight off a vendor's pricing page. But keeping it
  // silently while the other fields move to this pair's rates would leave the
  // candidate priced half from one model and half from another, under a toast
  // reporting success. So it is kept AND reported.
  const staleEdits = PRICE_FIELDS.some((f) => priceEditedAt[f] !== null && priceEditedAt[f] !== key)
  const changed = PRICE_FIELDS.map((f) => fillPrice(f, suggestedValue(s, f)))
  pricesDescribeAnotherPair = true
  if (staleEdits) {
    priceUnresolvedKey.value = 'models.priceMixedAfterChange'
    return
  }
  // The fields now hold this pair's own prices, vouched for by the look-up.
  priceUnresolvedKey.value = null
  if (changed.some(Boolean)) {
    message.info(
      s.source === 'history'
        ? t('models.pricePrefilledFromHistory')
        : t('models.pricePrefilledFromSeed', { date: s.catalog_updated_at }),
    )
  }
}

function suggestedValue(s: SuggestedPrice, field: PriceField): number | null {
  switch (field) {
    case 'inputPrice':
      return s.input_price
    case 'outputPrice':
      return s.output_price
    case 'cacheWritePrice':
      return s.cache_write_price
    default:
      return s.cache_read_price
  }
}

// Writes one suggested price unless the admin has typed into that field. A null
// is applied like any other value: a model with no cache pricing has to clear a
// cache price carried over from the model that was selected before.
function fillPrice(field: PriceField, value: number | null): boolean {
  if (priceEditedAt[field] !== null || form[field] === value) return false
  form[field] = value
  return true
}

// Waits for an auto-fill still in flight, so a save started right after picking
// a model stores the prices the admin is about to be shown rather than the ones
// the form happened to hold when they clicked.
async function settlePendingPriceFill() {
  let pending = pendingPriceFill
  while (pending) {
    await pending
    // Changing the pair again during the wait starts a newer look-up, and it is
    // that one the payload has to reflect — not whichever happened to be in
    // flight when the save started.
    const next = pendingPriceFill
    pending = next === pending ? null : next
  }
}

async function loadProviderModels(providerId: number | null) {
  const seq = ++modelFetchSeq
  fetchedModels.value = []
  // Bumping seq above already invalidated any in-flight fetch, so its finally
  // can no longer clear the flag — reset it here or clearing the provider
  // while a fetch is pending would leave the picker spinning forever.
  if (!providerId) {
    loadingModels.value = false
    return
  }
  loadingModels.value = true
  try {
    const { models } = await providersStore.listModelsForProvider(providerId)
    // A newer fetch started while this was in flight — its result wins.
    if (seq !== modelFetchSeq) return
    fetchedModels.value = models
  } catch {
    // Silent by design: the catalogue is a convenience. On any failure the
    // field stays a free-text combobox so the admin can type the name.
  } finally {
    if (seq === modelFetchSeq) loadingModels.value = false
  }
}

// Reload the catalogue whenever the target provider changes — including when
// the show-watch seeds providerId (edit mode) or openNewProviderModal sets a
// freshly created provider.
watch(
  () => form.providerId,
  (id) => {
    void loadProviderModels(id)
  },
)

const rules: FormRules = {
  providerId: [{ required: true, type: 'number', message: t('models.fieldRequired'), trigger: ['change', 'blur'] }],
  providerModelName: providerModelNameRule(t),
  inputPrice: nonNegativePriceRule(t),
  outputPrice: nonNegativePriceRule(t),
}

watch(
  () => props.show,
  (visible) => {
    // A price look-up is abandoned the moment the dialog closes, not merely when
    // it reopens: left alive it would fill a hidden form and toast about prices
    // for a candidate the operator has already cancelled out of. A save in
    // flight is deliberately NOT invalidated here — it is still going to be
    // written, and its 'saved' emit is what refreshes the list behind the modal.
    priceFillSeq += 1
    pendingPriceFill = null
    if (!visible) return
    // Invalidate any probe still in flight from a previous opening. Without this
    // a run the operator abandoned by closing the dialog would land its verdicts,
    // its persisted flag and even its auto-close on whichever candidate is open
    // when it finally resolves, up to 30 seconds later.
    saveSeq += 1
    resetPriceEdited()
    priceUnresolvedKey.value = null
    submitting.value = false
    savingDisabled.value = false
    report.value = null
    persisted.value = false
    if (props.editingCandidate) {
      form.providerId = props.editingCandidate.provider_id
      form.providerModelName = props.editingCandidate.provider_model_name
      form.inputPrice = props.editingCandidate.input_price
      form.outputPrice = props.editingCandidate.output_price
      form.cacheWritePrice = props.editingCandidate.cache_write_price
      form.cacheReadPrice = props.editingCandidate.cache_read_price
      form.maxOutput = props.editingCandidate.max_output
      form.enabled = props.editingCandidate.management_status === CANDIDATE_STATUS_ENABLED
      form.billingMode = props.editingCandidate.billing_mode === 'image' ? 'image' : 'token'
      const stored = props.editingCandidate.image_pricing_tiers
      form.imageTiers = stored
        ? stored.tiers.map((tier) => ({ quality: tier.quality, size: tier.size, price: tier.price }))
        : []
      form.imageDefaultPrice = stored?.default_price ?? null
      statusTouched.value = false
      // The stored prices already describe this pair, so opening the dialog is
      // not a reason to re-price it. Only a change from here is.
      pricedPairKey = pairKey(form.providerId, effectiveProviderModelName.value)
      pricesDescribeAnotherPair = true
    } else {
      form.providerId = null
      form.providerModelName = ''
      form.inputPrice = 0
      form.outputPrice = 0
      form.cacheWritePrice = null
      form.cacheReadPrice = null
      form.maxOutput = 0
      form.enabled = true
      pricedPairKey = ''
      // A blank form's zeros stand for nothing yet, so replacing them silently
      // is correct and there is nothing to warn about.
      pricesDescribeAnotherPair = false
      providersStore.fetchList()
    }
  },
)

function onUpdateShow(value: boolean) {
  emit('update:show', value)
}

function openNewProviderModal() {
  showNewProviderModal.value = true
}

// The exact created provider comes from the modal's own event — inferring it
// by diffing ids around the close would pick up a provider some OTHER admin
// created concurrently, and a later save would silently bind the mapping (and
// its probes) to the wrong upstream. Selecting before the list refresh keeps
// the precise id even when that refresh fails.
function onProviderCreated(created: Provider) {
  form.providerId = created.id
  void providersStore.fetchList()
}

// Closing without creating (cancel) only refreshes the options; the current
// selection is not second-guessed.
watch(showNewProviderModal, (visible) => {
  if (visible) return
  void providersStore.fetchList()
})

// True once the operator actually flips the enable switch this opening; a
// pristine switch stays out of the edit payload entirely. The server treats a
// PRESENT disabled value as an explicit instruction — it advances the probe
// token and revokes the auto-enable promise — so a price-only save on an
// imported, still-queued candidate must not carry one, and a stale form must
// not overwrite an enable that landed after the dialog opened.
const statusTouched = ref(false)

function candidatePayload() {
  const base = {
    provider_model_name: form.providerModelName,
    // The required fields are validated non-null before this runs; the ?? is
    // what keeps the payload's types honest rather than a second default.
    input_price: form.inputPrice ?? 0,
    output_price: form.outputPrice ?? 0,
    // A cleared optional price is omitted, which the server stores as NULL —
    // "this model has no cache pricing", not "it is free".
    cache_write_price: form.cacheWritePrice ?? undefined,
    cache_read_price: form.cacheReadPrice ?? undefined,
    max_output: form.maxOutput ?? 0,
  }
  const withBilling = {
    ...base,
    billing_mode: form.billingMode,
    image_pricing_tiers:
      form.billingMode === 'image'
        ? {
            mode: 'per_image',
            tiers: form.imageTiers.map((tier) => ({ ...tier })),
            default_price: form.imageDefaultPrice ?? null,
          }
        : null,
  }
  if (props.editingCandidate && !statusTouched.value) return withBilling
  return {
    ...withBilling,
    management_status: form.enabled ? CANDIDATE_STATUS_ENABLED : CANDIDATE_STATUS_DISABLED,
  }
}

// A run whose every probe came back affirmative needs no acknowledgement — the
// modal closes and a toast reports the outcome. Anything less stays on screen so
// the operator cannot miss it.
function reportIsAllClear(r: CandidateTestReport | null): boolean {
  if (!r) return true
  return probePassed(r.basic) && probePassed(r.streaming) && probePassed(r.function_calling)
}

// Called once the candidate is stored. An all-clear run closes the modal with a
// toast; anything less keeps it open so the verdicts are actually read, but the
// row is already saved, so persisted flips and the footer becomes a single
// close action rather than a save that would collide with itself.
//
// enabled is the PERSISTED row's state, not the form's request: the service
// can honor the save and the verdict while forfeiting the enable to a
// concurrent admin action, and announcing the requested state would claim
// "saved and enabled" over a row that is off.
function finishSave(enabled: boolean, r: CandidateTestReport | null, reportApplied: boolean, editingId: number | null) {
  persisted.value = true
  emit('saved')
  // A probe that lost the commit race to a concurrent run produced verdicts
  // the row does not hold — showing them (or claiming "saved and enabled")
  // would contradict the list. The fields did save; say so and defer to the
  // list for the probe state, same wording as a superseded retest. Gated on
  // the basic probe having RUN: a probe that never started (no usable key
  // yet) also comes back not-applied, but there was no race — the modal's
  // own "not tested" explanation is the honest story for that row.
  if (r !== null && r.basic.ran && !reportApplied) {
    onUpdateShow(false)
    message.info(t('providers.retestSuperseded'))
    return
  }
  if (!reportIsAllClear(r)) return
  onUpdateShow(false)
  // A null report means the edit could not have invalidated the stored verdicts
  // — a price or max-output change on a candidate whose target and enablement
  // both stayed put — so nothing was probed. Saying "saved and enabled" here
  // would imply a verification that never happened, so the outcome is reported
  // honestly and a retest is offered for an operator who wants one anyway.
  // Disabling also skips the probe, but there the plain "saved as disabled"
  // already tells the whole story and a retest nudge would just be noise.
  if (r === null && enabled && editingId !== null) {
    toastSavedWithoutRetest(editingId)
    return
  }
  message.success(enabled ? t('models.savedEnabled') : t('models.savedDisabled'))
}

function toastSavedWithoutRetest(candidateId: number) {
  // The click handler needs to dismiss the toast it lives inside, so the handle
  // is held in a box the closure reads at click time rather than being captured
  // by name — message.info has not returned yet while render is being built.
  const toast: { handle: MessageReactive | null } = { handle: null }
  toast.handle = message.info(t('models.savedNoRetest'), {
    duration: 8000,
    // Named msg rather than props so it does not shadow the component's props.
    render: (msg) =>
      h('div', { style: 'display: flex; align-items: center; gap: 12px' }, [
        h('span', null, msg.content as string),
        h(
          NButton,
          {
            text: true,
            type: 'primary',
            size: 'small',
            onClick: () => {
              toast.handle?.destroy()
              emit('retest', candidateId)
            },
          },
          { default: () => t('models.retestNow') },
        ),
      ]),
  })
}

async function onSave() {
  try {
    await formRef.value?.validate()
  } catch {
    return
  }
  submitting.value = true
  const seq = saveSeq
  const enabled = form.enabled
  const editingId = props.editingCandidate?.id ?? null
  try {
    // Saving right after picking a model is the common case, so the payload has
    // to wait for the price the form is about to display. Building it now would
    // store the previous mapping's rates — or a blank form's zeros — under a
    // toast announcing the new ones.
    await settlePendingPriceFill()
    if (seq !== saveSeq) return
    // The look-up that just landed could not vouch for these prices, so nothing
    // is sent until the operator answers the alert. Reported as a toast too:
    // the alert lives in the scrollable modal body and can be off-screen, and a
    // spinner that just stops with no explanation reads as a broken button.
    if (priceUnresolvedKey.value) {
      message.warning(t(priceUnresolvedKey.value))
      return
    }
    if (tierErrorKey.value) {
      message.warning(t(tierErrorKey.value))
      return
    }
    // Clearing the previous verdicts waits until here so an aborted save leaves
    // the probe results the operator was reading on screen.
    report.value = null
    if (editingId !== null) {
      const result = await store.updateCandidate(props.modelId, editingId, candidatePayload())
      if (seq !== saveSeq) return
      report.value = result.report
      // The edit always persists — a probe result only decides enablement — so
      // this counts as saved regardless of how the probes turned out. The
      // feedback reads the row as the server returned it, not as the form
      // requested it: a concurrent admin action can leave a passing save
      // disabled, and that is what the operator must be told.
      finishSave(result.candidate.management_status === CANDIDATE_STATUS_ENABLED,
        result.report, result.report_applied, editingId)
      return
    }
    const result = await store.testAndCreateCandidate(props.modelId, {
      provider_id: form.providerId!,
      ...candidatePayload(),
    })
    if (seq !== saveSeq) return
    report.value = result.report
    // Test-and-create's probe wrote the freshly inserted row — nothing can
    // have raced it, so its report is always the row's own.
    if (result.created) {
      finishSave(
        result.candidate !== null ? result.candidate.management_status === CANDIDATE_STATUS_ENABLED : enabled,
        result.report, true, null,
      )
    }
  } catch (err) {
    if (seq !== saveSeq) return
    message.error(displayMessage(err, t))
  } finally {
    // Only the run that still owns the dialog may clear the spinner; a superseded
    // run doing so would unstick a spinner that belongs to the current save.
    if (seq === saveSeq) submitting.value = false
  }
}

// The escape hatch after a failed probe run: store the mapping disabled without
// probing again. The verdicts just shown are deliberately not sent along — a
// client able to assert its own verification status could create a candidate
// that reads as verified and then enable it through the status endpoint.
async function onSaveAnywayDisabled() {
  // Validated like the primary save: this path writes the same fields, so
  // skipping it would let an invalid form through the side door.
  try {
    await formRef.value?.validate()
  } catch {
    return
  }
  savingDisabled.value = true
  const seq = saveSeq
  try {
    // Same reason as onSave: this path writes the same price columns.
    await settlePendingPriceFill()
    if (seq !== saveSeq) return
    if (priceUnresolvedKey.value) {
      message.warning(t(priceUnresolvedKey.value))
      return
    }
    if (tierErrorKey.value) {
      message.warning(t(tierErrorKey.value))
      return
    }
    if (props.editingCandidate) {
      await store.updateCandidate(props.modelId, props.editingCandidate.id, {
        ...candidatePayload(),
        management_status: CANDIDATE_STATUS_DISABLED,
      })
    } else {
      await store.createCandidate(props.modelId, {
        provider_id: form.providerId!,
        ...candidatePayload(),
        management_status: CANDIDATE_STATUS_DISABLED,
      })
    }
    if (seq !== saveSeq) return
    persisted.value = true
    emit('saved')
    message.success(t('models.savedDisabled'))
    onUpdateShow(false)
  } catch (err) {
    if (seq !== saveSeq) return
    message.error(displayMessage(err, t))
  } finally {
    if (seq === saveSeq) savingDisabled.value = false
  }
}
</script>

<style scoped lang="less">
.tiers-section {
  display: flex;
  flex-direction: column;
  gap: 8px;
  margin: 4px 0 8px;
}

.tiers-section__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
}

.tiers-section__label {
  font-size: 13px;
  opacity: 0.75;
}

.tier-row {
  display: grid;
  grid-template-columns: 1fr 1fr 1fr auto;
  gap: 8px;
  align-items: center;
}

/* Read-only context: which outward model this candidate is being mapped to.
   A blank provider model name defaults to this, so it doubles as the fallback
   reference the admin needs when choosing the provider-side model. */
.outward-model {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 20px;
  padding: 10px 12px;
  border-radius: 6px;
  background: var(--n-color-embedded, rgba(0, 0, 0, 0.03));
}

.outward-model__label {
  font-size: 13px;
  color: var(--n-text-color-3, rgba(0, 0, 0, 0.45));
}

.outward-model__value {
  font-weight: 600;
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
}

/* Keep the "new provider" shortcut on the label row so the select below can
   use the full modal width, matching every other field. */
.label-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  width: 100%;
}

/* Separate the probe results from the form above with a hairline rule, so the
   three verdicts read as one reported outcome rather than floating text. */
.probe-section {
  margin-top: 4px;
  padding-top: 16px;
  border-top: 1px solid var(--n-divider-color, rgba(0, 0, 0, 0.09));
}

.probe-section__label {
  margin-bottom: 8px;
  font-size: 13px;
  color: var(--n-text-color-3, rgba(0, 0, 0, 0.45));
}

.probe-section__pending {
  display: flex;
  align-items: center;
  gap: 12px;
}

.probe-section__pending-text {
  font-size: 13px;
}

.probe-section__hint {
  margin-top: 2px;
  font-size: 12px;
  color: var(--n-text-color-3, rgba(0, 0, 0, 0.45));
}

.probe-row {
  display: flex;
  align-items: baseline;
  gap: 8px;
  padding: 3px 0;
  font-size: 13px;
}

.probe-row__icon {
  width: 14px;
  text-align: center;
  font-weight: 600;
}

.probe-row__label {
  min-width: 88px;
}

.probe-row__verdict {
  color: var(--n-text-color-3, rgba(0, 0, 0, 0.45));
}

/* Hex rather than CSS variables so the tones hold up in both themes without
   depending on which naive-ui variables happen to be in scope here. */
.probe-row__icon--supported,
.probe-row__verdict--supported {
  color: #18a058;
}

.probe-row__icon--unsupported,
.probe-row__verdict--unsupported {
  color: #d03050;
}

/* An inconclusive probe is a warning, not a failure: routing still happens and
   the remedy is to retest, so it must not look like a proven "not supported". */
.probe-row__icon--unknown,
.probe-row__verdict--unknown {
  color: #f0a020;
}

.probe-row__icon--untested,
.probe-row__verdict--untested {
  color: var(--n-text-color-3, rgba(0, 0, 0, 0.45));
}

/* The acknowledgement sits inside the alert body because NAlert has no action
   slot — only default, header and icon — so a #action template would render
   nothing and leave the save block with no way out. */
.price-alert {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: 6px;
}

/* Numeric fields (prices, max output) hold at most a handful of digits, so a
   full-width row each wastes horizontal space and stretches the modal. Lay
   them out two per row; each control fills its own cell. The select fields
   above stay full width. */
.price-grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  column-gap: 16px;
}

@media (max-width: @mobile-breakpoint) {
  .price-grid {
    grid-template-columns: 1fr;
  }
}
</style>
