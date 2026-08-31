<!-- frontend/src/views/oauth/OAuthProviderListPage.vue
     External-login provider management. The create/edit modal is built
     around progressive disclosure: the admin picks how the provider is
     configured (GitHub / Google preset, OIDC discovery, or manual
     endpoints), and only the fields that mode actually needs stay
     visible — endpoints, scopes, token auth style and field mapping all
     have correct defaults and live under a collapsed advanced section.
     The client secret is write-only: the form never shows the stored
     value, and leaving the field blank on edit keeps it. -->
<template>
  <div class="common-page">
    <PageHeader
      :eyebrow="t('oauthProviders.eyebrow')"
      :title="t('oauthProviders.pageTitle')"
      :description="t('oauthProviders.pageDescription')"
    >
      <template #actions>
        <NButton type="primary" @click="openCreate">{{ t('oauthProviders.createButton') }}</NButton>
      </template>
    </PageHeader>

    <EmptyState v-if="!loading && providers.length === 0" :icon="LogIn" :title="t('oauthProviders.listEmpty')">
      <template #action>
        <NButton type="primary" @click="openCreate">{{ t('oauthProviders.createButton') }}</NButton>
      </template>
    </EmptyState>
    <NDataTable
      v-else
      :columns="columns"
      :data="providers"
      :loading="loading"
      :row-key="(row: OAuthProviderView) => row.id"
      :bordered="false"
    />

    <NModal
      v-model:show="showModal"
      preset="card"
      :title="editing ? t('oauthProviders.editTitle') : t('oauthProviders.createTitle')"
      style="max-width: 520px"
      :mask-closable="false"
      :close-on-esc="false"
    >
      <NForm ref="formRef" :model="form" :rules="rules" label-placement="top" require-mark-placement="left">
        <!-- Setup method: how the endpoint configuration is obtained. Only a
             choice on create — editing an existing provider just edits its
             stored values. -->
        <NFormItem v-if="!editing">
          <template #label>
            <HelpLabel :tip="t('oauthProviders.modeLabel_tip')">{{ t('oauthProviders.modeLabel') }}</HelpLabel>
          </template>
          <NSelect :value="mode" :options="modeOptions" @update:value="selectMode" />
        </NFormItem>

        <!-- OIDC mode: one URL replaces the three endpoint fields. Discovery
             runs automatically when the field loses focus; the result is a
             one-line status instead of three visible inputs. -->
        <NFormItem v-if="!editing && mode === 'oidc'" :show-feedback="discoveryState === 'idle'">
          <template #label>
            <HelpLabel :tip="t('oauthProviders.wellKnownLabel_tip')">{{ t('oauthProviders.wellKnownLabel') }}</HelpLabel>
          </template>
          <NInput
            :value="wellKnownURL"
            :loading="discovering"
            :placeholder="t('oauthProviders.wellKnownPlaceholder')"
            @update:value="onWellKnownInput"
            @blur="autoDiscover"
            @keyup.enter="autoDiscover"
          />
        </NFormItem>
        <div v-if="!editing && mode === 'oidc' && discoveryState !== 'idle'" class="discover-status">
          <span v-if="discoveryState === 'ok'" class="discover-status--ok">✓ {{ t('oauthProviders.discoverSuccess') }}</span>
          <template v-else>
            <span class="discover-status--failed">{{ t('oauthProviders.discoverFailed') }}</span>
            <NButton text size="tiny" type="primary" @click="autoDiscover">{{ t('oauthProviders.discoverRetry') }}</NButton>
          </template>
        </div>

        <NFormItem path="name">
          <template #label>
            <HelpLabel :tip="t('oauthProviders.nameLabel_tip')">{{ t('oauthProviders.nameLabel') }}</HelpLabel>
          </template>
          <NInput :value="form.name" :placeholder="t('oauthProviders.namePlaceholder')" @update:value="onNameInput" />
        </NFormItem>
        <!-- The slug derives from the name; showing it as a caption keeps it
             out of the way while keeping the callback URL below honest. It
             expands into a real input on click (create only — the slug is
             immutable once saved because it is part of the callback URL).
             Both states stay mounted (v-show, not v-if/v-else) so the slug
             rule is always registered with the form and validate() covers it
             even while the caption is showing. -->
        <div class="slug-line" v-show="!slugExpanded || editing">
          <span class="slug-line__label">{{ t('oauthProviders.slugLabel') }}:</span>
          <span class="slug-line__value">{{ form.slug || '—' }}</span>
          <NButton v-if="!editing" text size="tiny" type="primary" @click="slugExpanded = true">
            {{ t('common.edit') }}
          </NButton>
        </div>
        <NFormItem v-show="slugExpanded && !editing" path="slug">
          <template #label>
            <HelpLabel :tip="t('oauthProviders.slugLabel_tip')">{{ t('oauthProviders.slugLabel') }}</HelpLabel>
          </template>
          <NInput v-model:value="form.slug" :placeholder="t('oauthProviders.slugPlaceholder')" @update:value="slugTouched = true" />
        </NFormItem>

        <div class="form-grid">
          <NFormItem path="client_id">
            <template #label>
              <HelpLabel :tip="t('oauthProviders.clientIdLabel_tip')">{{ t('oauthProviders.clientIdLabel') }}</HelpLabel>
            </template>
            <NInput v-model:value="form.client_id" />
          </NFormItem>
          <NFormItem :path="editing ? undefined : 'client_secret'">
            <template #label>
              <HelpLabel :tip="t('oauthProviders.clientSecretLabel_tip')">{{ t('oauthProviders.clientSecretLabel') }}</HelpLabel>
            </template>
            <NInput
              v-model:value="form.client_secret"
              type="password"
              show-password-on="click"
              :placeholder="editing ? t('oauthProviders.secretKeepPlaceholder') : ''"
            />
          </NFormItem>
        </div>

        <!-- The redirect_uri the admin must register at the identity
             provider. Read-only and always visible: forgetting it is the
             single most common way an OAuth setup fails. -->
        <NFormItem>
          <template #label>
            <HelpLabel :tip="t('oauthProviders.callbackLabel_tip')">{{ t('oauthProviders.callbackLabel') }}</HelpLabel>
          </template>
          <NInput :value="callbackURL" readonly />
          <NButton class="callback-copy-btn" @click="copyCallback">{{ t('common.copy') }}</NButton>
        </NFormItem>

        <!-- Manual mode is the only case where the endpoints must be typed,
             so it is the only case where they sit in the main form. The same
             descriptor list renders the advanced-section copy below; exactly
             one of the two is mounted at a time, so each form path registers
             once. -->
        <template v-if="endpointsInMainForm">
          <NFormItem v-for="ep in endpointFields" :key="ep.path" :path="ep.path">
            <template #label>
              <HelpLabel :tip="t(ep.tipKey)">{{ t(ep.labelKey) }}</HelpLabel>
            </template>
            <NInput v-model:value="form[ep.path]" :placeholder="ep.placeholder" />
          </NFormItem>
        </template>

        <!-- display-directive="show" keeps the collapsed content mounted:
             the endpoint/slug rules must stay registered with the form, or
             validate() would silently skip everything hidden in here and an
             empty-endpoint create would sail through to the server. -->
        <NCollapse v-model:expanded-names="advancedOpen" class="advanced-collapse" display-directive="show">
          <NCollapseItem :title="t('oauthProviders.advancedTitle')" name="advanced" display-directive="show">
            <template v-if="!endpointsInMainForm">
              <NFormItem v-for="ep in endpointFields" :key="ep.path" :path="ep.path">
                <template #label>
                  <HelpLabel :tip="t(ep.tipKey)">{{ t(ep.labelKey) }}</HelpLabel>
                </template>
                <NInput v-model:value="form[ep.path]" :placeholder="ep.placeholder" />
              </NFormItem>
            </template>

            <div class="form-grid">
              <NFormItem>
                <template #label>
                  <HelpLabel :tip="t('oauthProviders.scopesLabel_tip')">{{ t('oauthProviders.scopesLabel') }}</HelpLabel>
                </template>
                <NInput v-model:value="form.scopes" placeholder="openid profile email" />
              </NFormItem>
              <NFormItem>
                <template #label>
                  <HelpLabel :tip="t('oauthProviders.authStyleLabel_tip')">{{ t('oauthProviders.authStyleLabel') }}</HelpLabel>
                </template>
                <NSelect v-model:value="form.auth_style" :options="authStyleOptions" />
              </NFormItem>
              <NFormItem>
                <template #label>
                  <HelpLabel :tip="t('oauthProviders.tokenRequestStyleLabel_tip')">{{ t('oauthProviders.tokenRequestStyleLabel') }}</HelpLabel>
                </template>
                <NSelect v-model:value="form.token_request_style" :options="tokenRequestStyleOptions" />
              </NFormItem>
              <NFormItem>
                <template #label>
                  <HelpLabel :tip="t('oauthProviders.tokenFieldStyleLabel_tip')">{{ t('oauthProviders.tokenFieldStyleLabel') }}</HelpLabel>
                </template>
                <NSelect v-model:value="form.token_field_style" :options="tokenFieldStyleOptions" />
              </NFormItem>
            </div>

            <div class="form-grid">
              <NFormItem>
                <template #label>
                  <HelpLabel :tip="t('oauthProviders.userinfoTokenHeaderLabel_tip')">{{ t('oauthProviders.userinfoTokenHeaderLabel') }}</HelpLabel>
                </template>
                <NInput v-model:value="form.userinfo_token_header" placeholder="x-acs-dingtalk-access-token" />
              </NFormItem>
              <NFormItem>
                <template #label>
                  <HelpLabel :tip="t('oauthProviders.pkceEnabledLabel_tip')">{{ t('oauthProviders.pkceEnabledLabel') }}</HelpLabel>
                </template>
                <NSwitch v-model:value="form.pkce_enabled" />
              </NFormItem>
              <NFormItem>
                <template #label>
                  <HelpLabel :tip="t('oauthProviders.extraAuthorizeParamsLabel_tip')">{{ t('oauthProviders.extraAuthorizeParamsLabel') }}</HelpLabel>
                </template>
                <NInput v-model:value="form.extra_authorize_params" placeholder="prompt=consent" />
              </NFormItem>
            </div>

            <NDivider class="mapping-divider">{{ t('oauthProviders.mappingDivider') }}</NDivider>
            <div class="form-grid">
              <NFormItem>
                <template #label>
                  <HelpLabel :tip="t('oauthProviders.userIdFieldLabel_tip')">{{ t('oauthProviders.userIdFieldLabel') }}</HelpLabel>
                </template>
                <NInput v-model:value="form.user_id_field" placeholder="sub" />
              </NFormItem>
              <NFormItem>
                <template #label>
                  <HelpLabel :tip="t('oauthProviders.usernameFieldLabel_tip')">{{ t('oauthProviders.usernameFieldLabel') }}</HelpLabel>
                </template>
                <NInput v-model:value="form.username_field" placeholder="preferred_username" />
              </NFormItem>
              <NFormItem>
                <template #label>
                  <HelpLabel :tip="t('oauthProviders.displayNameFieldLabel_tip')">{{ t('oauthProviders.displayNameFieldLabel') }}</HelpLabel>
                </template>
                <NInput v-model:value="form.display_name_field" placeholder="name" />
              </NFormItem>
              <NFormItem>
                <template #label>
                  <HelpLabel :tip="t('oauthProviders.emailFieldLabel_tip')">{{ t('oauthProviders.emailFieldLabel') }}</HelpLabel>
                </template>
                <NInput v-model:value="form.email_field" placeholder="email" />
              </NFormItem>
            </div>
          </NCollapseItem>
        </NCollapse>
      </NForm>
      <template #footer>
        <NSpace justify="end">
          <NButton @click="showModal = false">{{ t('common.cancel') }}</NButton>
          <NButton type="primary" :loading="saving" @click="save">{{ t('common.save') }}</NButton>
        </NSpace>
      </template>
    </NModal>
  </div>
</template>

<script setup lang="ts">
import { computed, h, nextTick, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  NButton, NCollapse, NCollapseItem, NDataTable, NDivider, NForm, NFormItem,
  NInput, NModal, NSelect, NSpace, NSwitch, NTag,
  useDialog, useMessage,
  type DataTableColumns, type FormInst, type FormRules, type SelectOption,
} from 'naive-ui'
import { LogIn } from '@lucide/vue'
import PageHeader from '../../components/PageHeader.vue'
import EmptyState from '../../components/EmptyState.vue'
import HelpLabel from '../../components/HelpLabel.vue'
import { columnTitle, STATUS_COL_WIDTH } from '../../utils/columnTitle'
import { displayMessage } from '../../api/client'
import { copyToClipboard } from '../../utils/clipboard'
import {
  createOAuthProvider, deleteOAuthProvider, discoverOIDC, listOAuthProviders,
  updateOAuthProvider, type OAuthProviderInput, type OAuthProviderView,
} from '../../api/oauth'

const { t } = useI18n()
const message = useMessage()
const dialog = useDialog()

// === List =================================================================

const providers = ref<OAuthProviderView[]>([])
const loading = ref(false)
// Server-provided callback prefix (external_url-aware); the page origin is
// only a fallback until the first list response arrives.
const callbackBase = ref('')

async function load() {
  loading.value = true
  try {
    const res = await listOAuthProviders()
    providers.value = res.providers
    callbackBase.value = res.callback_base
  } catch (err) {
    message.error(displayMessage(err, t))
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  void load()
})

async function toggleEnabled(row: OAuthProviderView, value: boolean) {
  try {
    await updateOAuthProvider(row.id, { enabled: value })
    row.enabled = value
  } catch (err) {
    message.error(displayMessage(err, t))
  }
}

function confirmDelete(row: OAuthProviderView) {
  dialog.warning({
    title: t('oauthProviders.confirmDeleteTitle'),
    content:
      row.identity_count > 0
        ? t('oauthProviders.confirmDeleteWithIdentities', { name: row.name, count: row.identity_count })
        : t('oauthProviders.confirmDeleteContent', { name: row.name }),
    positiveText: t('common.confirm'),
    negativeText: t('common.cancel'),
    onPositiveClick: async () => {
      try {
        await deleteOAuthProvider(row.id)
        message.success(t('oauthProviders.deleted'))
        void load()
      } catch (err) {
        message.error(displayMessage(err, t))
      }
    },
  })
}

const columns = computed<DataTableColumns<OAuthProviderView>>(() => [
  {
    title: columnTitle(t('oauthProviders.nameColumn'), t('oauthProviders.nameColumn_tip')),
    key: 'name',
    minWidth: 160,
    render: (row) =>
      h('div', {}, [
        h('div', {}, row.name),
        h('div', { class: 'slug-sub' }, row.slug),
      ]),
  },
  {
    title: columnTitle(t('oauthProviders.enabledColumn'), t('oauthProviders.enabledColumn_tip')),
    key: 'enabled',
    width: STATUS_COL_WIDTH,
    render: (row) =>
      h(NSwitch, {
        value: row.enabled,
        size: 'small',
        'onUpdate:value': (v: boolean) => void toggleEnabled(row, v),
      }),
  },
  {
    title: columnTitle(t('oauthProviders.identitiesColumn'), t('oauthProviders.identitiesColumn_tip')),
    key: 'identity_count',
    width: 110,
    render: (row) =>
      h(NTag, { size: 'small', bordered: false, type: row.identity_count > 0 ? 'info' : 'default' },
        { default: () => String(row.identity_count) }),
  },
  {
    title: columnTitle(t('oauthProviders.updatedColumn'), t('oauthProviders.updatedColumn_tip')),
    key: 'updated_at',
    width: 180,
    render: (row) => new Date(row.updated_at).toLocaleString(),
  },
  {
    title: t('common.actions'),
    key: 'actions',
    width: 150,
    render: (row) =>
      h(NSpace, { size: 'small' }, {
        default: () => [
          h(NButton, { size: 'small', onClick: () => openEdit(row) }, { default: () => t('common.edit') }),
          h(NButton, { size: 'small', type: 'error', quaternary: true, onClick: () => confirmDelete(row) },
            { default: () => t('common.delete') }),
        ],
      }),
  },
])

// === Create / edit modal ==================================================

type ProviderMode = 'github' | 'google' | 'dingtalk' | 'feishu' | 'oidc' | 'manual'

const showModal = ref(false)
const editing = ref<OAuthProviderView | null>(null)
const saving = ref(false)
const formRef = ref<FormInst | null>(null)
const wellKnownURL = ref('')
const discovering = ref(false)
// The URL that last completed discovery — blur re-runs discovery only when
// the field actually changed, so tabbing through the form doesn't refetch.
const lastDiscovered = ref('')
const discoveryState = ref<'idle' | 'ok' | 'failed'>('idle')
const mode = ref<ProviderMode>('oidc')
const slugTouched = ref(false)
// Whether the display name was typed by the admin (as opposed to prefilled
// by a preset) — a preset's own name/slug must not follow a mode switch.
const nameTouched = ref(false)
const slugExpanded = ref(false)
const advancedOpen = ref<string[]>([])

function emptyForm(): OAuthProviderInput {
  return {
    slug: '', name: '', icon: '', enabled: true,
    client_id: '', client_secret: '',
    authorization_endpoint: '', token_endpoint: '', userinfo_endpoint: '',
    scopes: 'openid profile email',
    user_id_field: 'sub', username_field: 'preferred_username',
    display_name_field: 'name', email_field: 'email',
    auth_style: 'post',
    // Protocol knobs mirror the server's defaults: a provider that never
    // touches the advanced section behaves like a standard OIDC one.
    token_request_style: 'form', token_field_style: 'snake',
    userinfo_token_header: '', pkce_enabled: true, extra_authorize_params: '',
  }
}

const form = ref<OAuthProviderInput>(emptyForm())

const rules = computed<FormRules>(() => {
  const required = { required: true, message: t('oauthProviders.fieldRequired'), trigger: ['blur', 'input'] }
  return {
    name: [required],
    // Mirrors the server's create rule (3-32 chars of [A-Za-z0-9_-], the
    // same set its alnum_dash validator accepts); without it a too-short
    // derived slug (e.g. from the name "AI") would pass the form and
    // bounce off the API instead. Edit skips the rule entirely: the slug
    // is immutable, never sent in the PATCH, and its input never renders
    // there — a rule that can fail only invisibly would make Save a
    // silent no-op for slugs created through the API.
    slug: editing.value ? [] : [required, {
      validator: (_rule: unknown, v: string) => !v || /^[A-Za-z0-9_-]{3,32}$/.test(v),
      message: t('oauthProviders.slugInvalid'),
      trigger: ['blur', 'input'],
    }],
    client_id: [required],
    client_secret: [required],
    authorization_endpoint: [required],
    token_endpoint: [required],
    userinfo_endpoint: [required],
  }
})

const authStyleOptions = computed<SelectOption[]>(() => [
  { label: t('oauthProviders.authStylePost'), value: 'post' },
  { label: t('oauthProviders.authStyleBasic'), value: 'basic' },
])

const tokenRequestStyleOptions = computed<SelectOption[]>(() => [
  { label: t('oauthProviders.tokenRequestStyleForm'), value: 'form' },
  { label: t('oauthProviders.tokenRequestStyleJson'), value: 'json' },
])

const tokenFieldStyleOptions = computed<SelectOption[]>(() => [
  { label: t('oauthProviders.tokenFieldStyleSnake'), value: 'snake' },
  { label: t('oauthProviders.tokenFieldStyleCamel'), value: 'camel' },
])

const modeOptions = computed<SelectOption[]>(() => [
  { label: t('oauthProviders.presetOIDC'), value: 'oidc' },
  { label: 'GitHub', value: 'github' },
  { label: 'Google', value: 'google' },
  { label: t('oauthProviders.presetDingtalk'), value: 'dingtalk' },
  { label: t('oauthProviders.presetFeishu'), value: 'feishu' },
  { label: t('oauthProviders.modeManual'), value: 'manual' },
])

// The three endpoint inputs, rendered from one descriptor list in both
// possible homes (main form for manual mode, advanced section otherwise) so
// the two copies cannot drift apart.
type EndpointPath = 'authorization_endpoint' | 'token_endpoint' | 'userinfo_endpoint'
const endpointFields: Array<{ path: EndpointPath; labelKey: string; tipKey: string; placeholder: string }> = [
  { path: 'authorization_endpoint', labelKey: 'oauthProviders.authorizeLabel', tipKey: 'oauthProviders.authorizeLabel_tip', placeholder: 'https://idp.example.com/authorize' },
  { path: 'token_endpoint', labelKey: 'oauthProviders.tokenLabel', tipKey: 'oauthProviders.tokenLabel_tip', placeholder: 'https://idp.example.com/token' },
  { path: 'userinfo_endpoint', labelKey: 'oauthProviders.userinfoLabel', tipKey: 'oauthProviders.userinfoLabel_tip', placeholder: 'https://idp.example.com/userinfo' },
]
const endpointsInMainForm = computed(() => !editing.value && mode.value === 'manual')

// The redirect_uri the admin registers at the identity provider — the
// server-provided prefix (which honors external_url, unlike this page's
// own origin) plus the live slug.
const callbackURL = computed(() =>
  `${callbackBase.value || `${location.origin}/oauth/callback/`}${form.value.slug || 'your-slug'}`)

async function copyCallback() {
  if (await copyToClipboard(callbackURL.value)) {
    message.success(t('oauthProviders.callbackCopied'))
  } else {
    // The URL sits in a selectable read-only input right above the button.
    message.error(t('oauthProviders.callbackCopyFailed'))
  }
}

// slugify derives a callback-safe slug from the display name: lowercase
// alphanumerics and single hyphens. Capped at 32 characters to match the
// server's 3-32 slug rule, so pasting a long string (say, a URL) into the
// name field can't derive a slug the create request would reject.
function slugify(name: string): string {
  return name
    .toLowerCase()
    .replace(/[\s_]+/g, '-')
    .replace(/[^a-z0-9-]/g, '')
    .replace(/-{2,}/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 32)
    .replace(/-+$/, '')
}

function onNameInput(v: string) {
  form.value.name = v
  nameTouched.value = true
  if (!slugTouched.value && !editing.value) {
    form.value.slug = slugify(v)
  }
}

// The modes that prefill the form, kept as a set so the leave-a-preset
// check in selectMode stays one lookup.
const PRESET_MODES: ReadonlySet<ProviderMode> = new Set(['github', 'google', 'dingtalk', 'feishu'])

// selectMode prefills what the chosen mode already knows. Popular presets
// carry their full endpoint + mapping sets; oidc/manual reset to the
// defaults and rely on discovery / typing. Client credentials and anything
// the admin actually typed survive the switch — but a preset's own
// prefilled name/slug does NOT follow into another mode, or "GitHub" would
// masquerade as user input after switching away. Returning to OIDC with a
// discovery URL still in the field re-runs discovery, since the endpoints
// were just reset.
function selectMode(v: ProviderMode) {
  const leavingPreset = PRESET_MODES.has(mode.value)
  mode.value = v
  const base = emptyForm()
  base.client_id = form.value.client_id
  base.client_secret = form.value.client_secret
  discoveryState.value = 'idle'
  lastDiscovered.value = ''
  switch (v) {
    case 'github':
      Object.assign(base, {
        slug: 'github', name: 'GitHub',
        authorization_endpoint: 'https://github.com/login/oauth/authorize',
        token_endpoint: 'https://github.com/login/oauth/access_token',
        userinfo_endpoint: 'https://api.github.com/user',
        scopes: 'read:user user:email',
        user_id_field: 'id', username_field: 'login',
        display_name_field: 'name', email_field: 'email',
      })
      slugTouched.value = false
      nameTouched.value = false
      break
    case 'google':
      Object.assign(base, {
        slug: 'google', name: 'Google',
        authorization_endpoint: 'https://accounts.google.com/o/oauth2/v2/auth',
        token_endpoint: 'https://oauth2.googleapis.com/token',
        userinfo_endpoint: 'https://openidconnect.googleapis.com/v1/userinfo',
        username_field: 'email',
      })
      slugTouched.value = false
      nameTouched.value = false
      break
    case 'dingtalk':
      // DingTalk deviates from OAuth2 on every axis the protocol knobs
      // cover: JSON + camelCase token exchange, a custom userinfo auth
      // header, no PKCE, and a required prompt=consent authorize parameter.
      Object.assign(base, {
        slug: 'dingtalk', name: t('oauthProviders.presetDingtalk'),
        authorization_endpoint: 'https://login.dingtalk.com/oauth2/auth',
        token_endpoint: 'https://api.dingtalk.com/v1.0/oauth2/userAccessToken',
        userinfo_endpoint: 'https://api.dingtalk.com/v1.0/contact/users/me',
        scopes: 'openid',
        user_id_field: 'unionId', username_field: 'nick',
        display_name_field: 'nick', email_field: 'email',
        auth_style: 'post',
        token_request_style: 'json', token_field_style: 'camel',
        userinfo_token_header: 'x-acs-dingtalk-access-token',
        pkce_enabled: false,
        extra_authorize_params: 'prompt=consent',
      })
      slugTouched.value = false
      nameTouched.value = false
      break
    case 'feishu':
      Object.assign(base, {
        slug: 'feishu', name: t('oauthProviders.presetFeishu'),
        authorization_endpoint: 'https://accounts.feishu.cn/oauth/v3/authorize',
        token_endpoint: 'https://accounts.feishu.cn/oauth/v3/token',
        userinfo_endpoint: 'https://open.feishu.cn/open-apis/authen/v1/user_info',
        scopes: '',
        user_id_field: 'data.union_id', username_field: 'data.name',
        display_name_field: 'data.name', email_field: 'data.email',
        auth_style: 'post',
        token_request_style: 'json', token_field_style: 'snake',
      })
      slugTouched.value = false
      nameTouched.value = false
      break
    default:
      // Each field keeps its own value only if the admin actually typed it
      // — judged independently, so a hand-edited slug survives leaving a
      // preset even when the name is still the preset's.
      if (!(leavingPreset && !nameTouched.value)) base.name = form.value.name
      if (!(leavingPreset && !slugTouched.value)) base.slug = form.value.slug
      break
  }
  form.value = base
  if (v === 'oidc' && wellKnownURL.value.trim()) {
    void autoDiscover()
  }
}

// Monotonic token invalidating in-flight discovery: bumped each time a
// new discovery starts, so an older in-flight response is dropped instead
// of overwriting endpoints that no longer belong to it. Staleness from a
// mode switch, a closed modal, or an edited URL is caught separately by
// the direct state checks where results land.
let discoverSeq = 0
// The URL the currently in-flight discovery is fetching — dedupes repeated
// blur/Enter on the same value without blocking a NEW url from starting
// its own discovery while an older one is still running.
let inflightURL = ''

// onWellKnownInput invalidates previous discovery the moment the URL is
// edited: the endpoints on the form belong to the OLD url, and keeping
// them would let a save go through with the previous issuer's endpoints
// while the new discovery is still running (or after it failed).
function onWellKnownInput(v: string) {
  wellKnownURL.value = v
  if (v.trim() !== lastDiscovered.value && discoveryState.value !== 'idle') {
    discoveryState.value = 'idle'
    lastDiscovered.value = ''
    form.value.authorization_endpoint = ''
    form.value.token_endpoint = ''
    form.value.userinfo_endpoint = ''
  }
}

// autoDiscover runs OIDC discovery when the well-known field settles (blur
// or Enter), replacing the old explicit button. Skips silently when the
// field is empty or unchanged since the last successful run.
async function autoDiscover() {
  const url = wellKnownURL.value.trim()
  if (!url) return
  // Dedupe only the SAME url: a repeated blur while its request is in
  // flight, or one that already succeeded. A different url always starts
  // its own discovery immediately — the seq guard retires the older
  // flight — so correcting the address mid-flight is never silently lost.
  if (discovering.value && url === inflightURL) return
  if (url === lastDiscovered.value && discoveryState.value === 'ok') return
  const seq = ++discoverSeq
  inflightURL = url
  discovering.value = true
  try {
    const doc = await discoverOIDC(url)
    // Stale guard: the admin may have switched mode, closed the modal, or
    // edited the URL while this request was in flight.
    if (seq !== discoverSeq || mode.value !== 'oidc' || !showModal.value || url !== wellKnownURL.value.trim()) return
    form.value.authorization_endpoint = doc.authorization_endpoint
    form.value.token_endpoint = doc.token_endpoint
    // Unconditional: a document without a userinfo endpoint must clear a
    // stale one from a previous run, not silently keep it — the required-
    // field validation then forces an explicit value.
    form.value.userinfo_endpoint = doc.userinfo_endpoint
    lastDiscovered.value = url
    discoveryState.value = 'ok'
  } catch (err) {
    // Same staleness rules as the success path — including the URL check,
    // so an old address's failure is never reported against the corrected
    // address the admin has since typed.
    if (seq !== discoverSeq || mode.value !== 'oidc' || !showModal.value || url !== wellKnownURL.value.trim()) return
    discoveryState.value = 'failed'
    message.error(displayMessage(err, t))
  } finally {
    if (seq === discoverSeq) discovering.value = false
  }
}

function openCreate() {
  editing.value = null
  mode.value = 'oidc'
  wellKnownURL.value = ''
  lastDiscovered.value = ''
  discoveryState.value = 'idle'
  slugTouched.value = false
  nameTouched.value = false
  slugExpanded.value = false
  advancedOpen.value = []
  form.value = emptyForm()
  showModal.value = true
}

function openEdit(row: OAuthProviderView) {
  editing.value = row
  mode.value = 'oidc'
  wellKnownURL.value = ''
  lastDiscovered.value = ''
  discoveryState.value = 'idle'
  slugTouched.value = true
  nameTouched.value = true
  slugExpanded.value = false
  advancedOpen.value = []
  form.value = {
    slug: row.slug, name: row.name, icon: row.icon, enabled: row.enabled,
    client_id: row.client_id, client_secret: '',
    authorization_endpoint: row.authorization_endpoint,
    token_endpoint: row.token_endpoint,
    userinfo_endpoint: row.userinfo_endpoint,
    scopes: row.scopes,
    user_id_field: row.user_id_field, username_field: row.username_field,
    display_name_field: row.display_name_field, email_field: row.email_field,
    auth_style: row.auth_style,
    token_request_style: row.token_request_style,
    token_field_style: row.token_field_style,
    userinfo_token_header: row.userinfo_token_header,
    pkce_enabled: row.pkce_enabled,
    extra_authorize_params: row.extra_authorize_params,
  }
  showModal.value = true
}

async function save() {
  // Rules can only run for mounted fields, and NCollapseItem lazy-mounts
  // its body until the first expand — even with display-directive="show".
  // So a required field hiding in the never-opened advanced section (the
  // endpoints) or behind the slug caption would silently skip validation
  // and the create would sail through to the server. Pre-check those
  // fields by hand; when any is empty, expand their homes first so the
  // FormItems mount and register, then let validate() paint the errors.
  const f = form.value
  if (!f.slug || !f.authorization_endpoint || !f.token_endpoint || !f.userinfo_endpoint) {
    advancedOpen.value = ['advanced']
    if (!editing.value) slugExpanded.value = true
    await nextTick()
  }
  try {
    await formRef.value?.validate()
  } catch {
    // A failed rule may sit inside a collapsed or caption-collapsed area
    // (endpoints under the advanced section, the slug caption) — open both
    // so the error message is actually visible.
    advancedOpen.value = ['advanced']
    if (!editing.value) slugExpanded.value = true
    return
  }
  saving.value = true
  try {
    if (editing.value) {
      // Sparse PATCH: the secret rides along only when the admin typed a
      // replacement — an empty field means "keep the stored one".
      const patch: Partial<OAuthProviderInput> = { ...form.value }
      delete patch.slug
      if (!form.value.client_secret) delete patch.client_secret
      await updateOAuthProvider(editing.value.id, patch)
    } else {
      await createOAuthProvider(form.value)
    }
    message.success(t('oauthProviders.saved'))
    showModal.value = false
    void load()
  } catch (err) {
    message.error(displayMessage(err, t))
  } finally {
    saving.value = false
  }
}
</script>

<style scoped>
.form-grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 0 16px;
}

.discover-status {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 12px;
  margin: 2px 0 12px;
}

.discover-status--ok {
  color: var(--color-success, #18a058);
}

.discover-status--failed {
  color: var(--color-danger, #d03050);
}

.slug-line {
  display: flex;
  align-items: baseline;
  gap: 6px;
  font-size: 12px;
  margin: 0 0 14px;
}

.slug-line__label {
  color: var(--color-text-secondary);
}

.slug-line__value {
  font-family: monospace;
}

.callback-copy-btn {
  margin-left: 8px;
  flex-shrink: 0;
}

.advanced-collapse {
  margin-top: 4px;
}

.mapping-divider {
  margin: 4px 0 12px;
  font-size: 12px;
}

:deep(.slug-sub) {
  font-size: 12px;
  color: #999;
}
</style>
