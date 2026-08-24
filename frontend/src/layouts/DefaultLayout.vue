<!-- frontend/src/layouts/DefaultLayout.vue -->
<template>
  <n-layout :has-sider="!isMobile" class="app-shell">
    <!-- Desktop: a persistent collapsible sider. On mobile it's replaced by a
         top bar + slide-in drawer (below), so the sider is skipped entirely to
         avoid stealing horizontal space on a phone viewport. -->
    <n-layout-sider
      v-if="!isMobile"
      :collapsed="collapsed"
      :collapsed-width="64"
      :width="220"
      collapse-mode="width"
      show-trigger="bar"
      bordered
      :native-scrollbar="false"
      class="layout-sider"
      @update:collapsed="(v: boolean) => (collapsed = v)"
    >
      <div class="sidebar-inner">
        <RouterLink to="/" class="sidebar-logo" :class="{ 'sidebar-logo--collapsed': collapsed }">
          <img :src="logo" alt="" width="26" />
          <span v-if="!collapsed" class="sidebar-logo__title">Yolorouter</span>
        </RouterLink>

        <div class="sidebar-nav-main">
          <SidebarNav
            :items="navItems"
            :collapsed="collapsed"
            :soon-label="t('nav.soonBadge')"
            :soon-tooltip="t('nav.comingSoon')"
          />
        </div>

        <div class="sidebar-bottom">
          <n-dropdown :options="userMenuOptions" placement="top-start" @select="onLogout">
            <button class="sidebar-user" :class="{ 'sidebar-user--collapsed': collapsed }">
              <span class="sidebar-user__avatar">{{ userInitial }}</span>
              <span v-if="!collapsed" class="sidebar-user__name">{{ authStore.username }}</span>
            </button>
          </n-dropdown>
        </div>
      </div>
    </n-layout-sider>

    <n-layout class="layout-main">
      <!-- Mobile top bar: a hamburger opens the nav drawer, since there's no
           persistent sider at this width. -->
      <header v-if="isMobile" class="mobile-topbar">
        <button class="mobile-topbar__menu" type="button" :aria-label="t('nav.overview')" @click="drawerOpen = true">
          <NIcon :size="22"><Menu /></NIcon>
        </button>
        <RouterLink to="/" class="mobile-topbar__brand">
          <img :src="logo" alt="" width="24" />
          <span>Yolorouter</span>
        </RouterLink>
      </header>

      <n-layout-content>
        <div class="layout-content">
          <!-- Bind :key to the full path so Vue remounts the matched component
               on every route change. Detail pages capture their path param
               (e.g. `Number(route.params.id)`) once at setup; without this key
               Vue Router reuses the component instance across same-route
               different-param navigations (browser back/forward between two
               detail URLs), so the captured id would go stale. -->
          <router-view :key="$route.fullPath" />
        </div>
      </n-layout-content>
    </n-layout>

    <!-- Mobile navigation drawer: hosts the same nav + account footer as the
         desktop sider. Tapping any link closes it (handled by the route watch
         below) so navigating doesn't leave the overlay covering the page. -->
    <n-drawer v-if="isMobile" v-model:show="drawerOpen" :width="260" placement="left">
      <n-drawer-content :native-scrollbar="false" body-content-style="padding: 0;">
        <div class="sidebar-inner sidebar-inner--drawer">
          <RouterLink to="/" class="sidebar-logo">
            <img :src="logo" alt="" width="26" />
            <span class="sidebar-logo__title">Yolorouter</span>
          </RouterLink>

          <div class="sidebar-nav-main">
            <SidebarNav
              :items="navItems"
              :collapsed="false"
              :soon-label="t('nav.soonBadge')"
              :soon-tooltip="t('nav.comingSoon')"
            />
          </div>

          <div class="sidebar-bottom">
            <n-dropdown :options="userMenuOptions" placement="top-start" @select="onLogout">
              <button class="sidebar-user">
                <span class="sidebar-user__avatar">{{ userInitial }}</span>
                <span class="sidebar-user__name">{{ authStore.username }}</span>
              </button>
            </n-dropdown>
          </div>
        </div>
      </n-drawer-content>
    </n-drawer>

    <ModalDrawer
      v-model:show="showChangePassword"
      :title="t('auth.changePasswordTitle')"
      :confirm-text="t('auth.changePasswordButton')"
      :loading="changingPassword"
      :back-label="t('common.back')"
      @confirm="onChangePasswordSubmit"
      @after-leave="resetChangePasswordForm"
    >
      <n-form require-mark-placement="left" ref="changePasswordFormRef" :model="changePasswordForm" :rules="changePasswordRules">
        <n-form-item path="currentPassword">
          <template #label>
            <HelpLabel :tip="t('auth.currentPassword_tip')">{{ t('auth.currentPassword') }}</HelpLabel>
          </template>
          <n-input v-model:value="changePasswordForm.currentPassword" type="password" show-password-on="click" />
        </n-form-item>
        <n-form-item path="newPassword">
          <template #label>
            <HelpLabel :tip="t('auth.newPassword_tip')">{{ t('auth.newPassword') }}</HelpLabel>
          </template>
          <n-input v-model:value="changePasswordForm.newPassword" type="password" show-password-on="click" />
        </n-form-item>
        <n-form-item path="confirmNewPassword">
          <template #label>
            <HelpLabel :tip="t('auth.confirmNewPassword_tip')">{{ t('auth.confirmNewPassword') }}</HelpLabel>
          </template>
          <n-input v-model:value="changePasswordForm.confirmNewPassword" type="password" show-password-on="click" />
        </n-form-item>
      </n-form>
    </ModalDrawer>

    <!-- Language picker. Desktop keeps the centered modal; on a phone it reads
         better as a bottom sheet (shared OptionSheet), matching the rest of the
         mobile option pickers. -->
    <n-modal v-if="!isMobile" v-model:show="showLanguage" preset="card" :title="t('nav.language')" style="max-width: 340px">
      <div class="lang-options">
        <button
          v-for="lang in LOCALES"
          :key="lang.value"
          type="button"
          class="lang-option"
          :class="{ 'lang-option--active': lang.value === localeStore.locale }"
          @click="onSelectLanguage(lang.value)"
        >
          <span>{{ lang.label }}</span>
          <NIcon v-if="lang.value === localeStore.locale" :size="16"><Check /></NIcon>
        </button>
      </div>
    </n-modal>

    <OptionSheet
      v-else
      v-model:show="showLanguage"
      :title="t('nav.language')"
      :options="languageOptions"
      :value="localeStore.locale"
      @select="onSelectLanguage"
      :height="200"
    />
  </n-layout>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { NIcon, useDialog, useMessage, type DropdownOption, type FormInst, type FormRules } from 'naive-ui'
import SidebarNav, { type NavItem } from '../components/SidebarNav.vue'
import {
  BarChart3,
  Box,
  Check,
  Info,
  Key,
  KeyRound,
  Languages,
  LogIn,
  LayoutGrid,
  Menu,
  Receipt,
  ScrollText,
  UsersRound,
  Server,
  Tags,
  TrendingDown,
} from '@lucide/vue'
import { useAuthStore } from '../store/auth'
import { useUpdateStore } from '../store/update'
import { useLocaleStore } from '../store/locale'
import { LOCALES, type Locale } from '../i18n'
import { APIError, displayMessage } from '../api/client'
import { ACCOUNT_SESSION_INVALID } from '../api/errcodes'
import { passwordStrengthRule, confirmPasswordRule } from '../utils/authValidators'
import HelpLabel from '../components/HelpLabel.vue'
import OptionSheet from '../components/common/OptionSheet.vue'
import ModalDrawer from '../components/common/ModalDrawer.vue'
import { useIsMobile } from '../composables/useIsMobile'
import logo from '../assets/logo.svg'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const authStore = useAuthStore()
const dialog = useDialog()
const message = useMessage()
const localeStore = useLocaleStore()

const collapsed = ref(false)
const updateStore = useUpdateStore()

// Below the mobile breakpoint the persistent sider is dropped for a top bar +
// slide-in drawer. Leaving mobile with the drawer still open would strand an
// invisible overlay over the restored desktop sider — close it on the way out.
const drawerOpen = ref(false)
const isMobile = useIsMobile(() => {
  drawerOpen.value = false
})

// Navigating from a drawer link must close the drawer; otherwise the overlay
// stays up covering the page the user just navigated to.
watch(() => route.fullPath, () => (drawerOpen.value = false))

// Fire the background update check once when the admin shell mounts, so the
// sidebar badge reflects "new version available" without the user having to
// visit the system page first. checkForUpdates swallows its own errors (a
// failed check is an expected pre-public / GitHub-outage state), so
// fire-and-forget here is safe — no unhandled rejection.
onMounted(() => {
  // The version endpoint is admin-only; a member shell has no about page or
  // update badge, so skip the call rather than fire a guaranteed 403.
  if (authStore.isAdmin) {
    void updateStore.checkForUpdates()
  }
})

// A single ordered list where static group headers interleave with standalone
// links, so the sidebar renders the full information architecture in one pass.
// computed rather than a plain array so labels (and the update badge) stay in
// sync when the user switches locale or a new version is detected.
//
// Short nav.* labels are used here (e.g. "用量统计") rather than the pages'
// own longer PageHeader titles, so the sidebar reads cleanly while each page
// keeps its fuller heading. Entries carrying `disabled` are placeholders for
// features that aren't built yet. `onClick` entries open a modal instead of
// navigating.
const navItems = computed<NavItem[]>(() => {
  // A member's sidebar carries exactly the self-service surface: overview,
  // usage, costs, their API keys, and language. The change-password entry
  // additionally requires a password-backed local account — an OAuth
  // account has no password to change.
  if (!authStore.isAdmin) {
    const items: NavItem[] = [
      { key: 'overview', label: t('nav.overview'), icon: LayoutGrid, to: '/' },
      { key: 'group-analytics', label: t('nav.groupAnalytics'), group: true },
      { key: 'usage', label: t('nav.usage'), icon: BarChart3, to: '/analytics' },
      { key: 'cost-stats', label: t('nav.costStats'), icon: Receipt, to: '/costs' },
      { key: 'tokens', label: t('nav.tokens'), icon: Key, to: '/api-keys' },
      { key: 'group-system', label: t('nav.groupSystem'), group: true },
      { key: 'language', label: t('nav.language'), icon: Languages, onClick: () => (showLanguage.value = true) },
    ]
    if (authStore.isLocal) {
      items.push({ key: 'change-password', label: t('auth.changePasswordTitle'), icon: KeyRound, onClick: () => (showChangePassword.value = true) })
    }
    return items
  }
  return [
    { key: 'overview', label: t('nav.overview'), icon: LayoutGrid, to: '/' },

    { key: 'group-analytics', label: t('nav.groupAnalytics'), group: true },
    { key: 'usage', label: t('nav.usage'), icon: BarChart3, to: '/analytics' },
    { key: 'log-audit', label: t('nav.logAudit'), icon: ScrollText, to: '/request-logs' },
    { key: 'cost-stats', label: t('nav.costStats'), icon: Receipt, to: '/costs' },

    { key: 'group-models', label: t('nav.groupModels'), group: true },
    { key: 'providers', label: t('nav.providers'), icon: Server, to: '/providers' },
    { key: 'models', label: t('nav.models'), icon: Box, to: '/models' },

    { key: 'tokens', label: t('nav.tokens'), icon: Key, to: '/api-keys' },

    { key: 'cost-optimization', label: t('nav.costOptimization'), icon: TrendingDown, to: '/cost-optimization', tag: t('nav.saveBadge') },

    // Account administration gets its own group: users and login providers
    // manage who can enter the instance (admin-only concerns), while the
    // system group below holds every signed-in user's own preferences —
    // mixing the two under one heading read as if user management were a
    // personal setting.
    { key: 'group-accounts', label: t('nav.groupAccounts'), group: true },
    { key: 'users', label: t('nav.users'), icon: UsersRound, to: '/users' },
    { key: 'oauth-providers', label: t('nav.oauthProviders'), icon: LogIn, to: '/oauth-providers' },

    { key: 'group-system', label: t('nav.groupSystem'), group: true },
    { key: 'language', label: t('nav.language'), icon: Languages, onClick: () => (showLanguage.value = true) },
    // An admin promoted from an OAuth account has no password to change —
    // same is_local gate as the member branch above.
    ...(authStore.isLocal
      ? [{ key: 'change-password', label: t('auth.changePasswordTitle'), icon: KeyRound, onClick: () => (showChangePassword.value = true) } satisfies NavItem]
      : []),
    // New-release indicator: the text chip (same superscript style as the
    // cost-optimization tag) renders in the expanded sidebar; SidebarNav
    // itself falls back to the badge dot whenever the chip isn't visible, so
    // each rendering instance (collapsed sider, mobile drawer) shows exactly
    // one signal.
    {
      key: 'about',
      label: t('nav.about'),
      icon: Info,
      to: '/about',
      tag: updateStore.hasUpdate ? t('nav.newVersionBadge') : undefined,
      badge: updateStore.hasUpdate,
    },
    // Hidden from the sidebar for now (feature not built yet).
    { key: 'model-pricing', label: t('nav.modelPricing'), icon: Tags, disabled: true, hidden: true },
  ]
})

// Just "logout" now — change password moved into the System Settings group in
// the sidebar. computed rather than a plain array so the label stays in sync
// when the user switches locale without needing to re-open the dropdown.
const userMenuOptions = computed<DropdownOption[]>(() => [
  { label: t('auth.logout'), key: 'logout' },
])

// A single-character avatar fallback (no avatar upload in this admin
// tool) — uppercased so a lowercase username still reads as a deliberate
// initial, not a truncation artifact.
const userInitial = computed(() => (authStore.username?.[0] ?? '?').toUpperCase())

// Logout is the dropdown's only entry, so this runs straight from @select
// without switching on the key.
function onLogout() {
  dialog.warning({
    title: t('auth.logoutConfirmTitle'),
    content: t('auth.logoutConfirmContent'),
    positiveText: t('auth.logout'),
    negativeText: t('common.cancel'),
    onPositiveClick: async () => {
      try {
        await authStore.logout()
      } catch (err) {
        if (err instanceof APIError && err.code === ACCOUNT_SESSION_INVALID) {
          // The session was already gone server-side — api/auth.ts's
          // session-invalid handling already cleared local auth state
          // and navigated to /login, so there's nothing left to do here.
          return
        }
        message.error(displayMessage(err, t))
        return
      }
      router.push('/login')
    },
  })
}

// Language picker — the corner switcher was removed in favor of a "Language"
// entry in the System Settings group, which opens this modal. The option list
// (LOCALES) and check-mark treatment are shared with the LocaleSwitcher.
const showLanguage = ref(false)

// The mobile OptionSheet takes a flat {label, value} list; LOCALES is already
// in that shape (label/value), so this just narrows it to what the sheet wants.
const languageOptions = computed(() => LOCALES.map((l) => ({ label: l.label, value: l.value })))

function onSelectLanguage(value: Locale) {
  localeStore.setLocale(value)
  showLanguage.value = false
}

const showChangePassword = ref(false)
const changingPassword = ref(false)
const changePasswordFormRef = ref<FormInst | null>(null)
const changePasswordForm = reactive({ currentPassword: '', newPassword: '', confirmNewPassword: '' })

// Cleared whenever the modal fully closes (@after-leave, below — fires for
// every close path: cancel, backdrop click, Esc, and the success path's
// own showChangePassword = false), not just on successful submit. Plain
// reactive state that outlives the modal being open would otherwise let
// anyone who regains access to an already-logged-in tab reopen the modal
// and read back a previously-typed password via "show password".
function resetChangePasswordForm() {
  changePasswordForm.currentPassword = ''
  changePasswordForm.newPassword = ''
  changePasswordForm.confirmNewPassword = ''
}

// computed rather than a plain object so the messages stay in the current
// locale if the user switches language while the modal happens to be open
// — matches userMenuOptions above.
const changePasswordRules = computed<FormRules>(() => ({
  currentPassword: [{ required: true, message: t('auth.fieldRequired'), trigger: ['blur', 'input'] }],
  newPassword: [passwordStrengthRule(t)],
  confirmNewPassword: [confirmPasswordRule(t, () => changePasswordForm.newPassword)],
}))

async function onChangePasswordSubmit() {
  try {
    await changePasswordFormRef.value?.validate()
  } catch {
    return
  }

  dialog.warning({
    title: t('auth.changePasswordConfirmTitle'),
    content: t('auth.changePasswordConfirmContent'),
    positiveText: t('auth.changePasswordButton'),
    negativeText: t('common.cancel'),
    onPositiveClick: async () => {
      changingPassword.value = true
      try {
        await authStore.changePassword(changePasswordForm.currentPassword, changePasswordForm.newPassword)
        showChangePassword.value = false
        message.success(t('auth.changePasswordSuccess'))
        router.push('/login')
      } catch (err) {
        if (err instanceof APIError && err.code === ACCOUNT_SESSION_INVALID) {
          // api/auth.ts's session-invalid handling already cleared local
          // auth state and navigated to /login — just close this modal
          // and explain why, instead of leaving it open on top of the
          // login page.
          showChangePassword.value = false
          message.error(err.message)
        } else {
          message.error(displayMessage(err, t))
        }
      } finally {
        changingPassword.value = false
      }
    },
  })
}
</script>

<style scoped lang="less">
.layout-sider {
  background-color: var(--color-sidebar);
}

.sidebar-inner {
  display: flex;
  flex-direction: column;
  height: 100dvh;
  overflow: hidden;
}

.sidebar-logo {
  display: flex;
  flex-shrink: 0;
  align-items: center;
  gap: 0.5rem;
  height: 64px;
  padding: 0 16px;
  color: var(--color-text);
  font-weight: 700;
}

.sidebar-logo--collapsed {
  justify-content: center;
  padding: 0;
}

/* The nav is the one flexible region: it takes the space between the fixed logo
   and account footer and scrolls internally, so on short viewports every entry
   (including language / password / logout) stays reachable instead of being
   clipped. min-height:0 lets a flex child actually shrink below its content
   height, which is what enables the scroll. */
.sidebar-nav-main {
  flex: 1 1 auto;
  min-height: 0;
  margin-top: var(--space-2);
  overflow-y: auto;
  overscroll-behavior: contain;
  scrollbar-width: thin;
  scrollbar-color: var(--color-border) transparent;
}

.sidebar-nav-main::-webkit-scrollbar {
  width: 6px;
}

.sidebar-nav-main::-webkit-scrollbar-thumb {
  background: var(--color-border);
  border-radius: var(--radius-full);
}

.sidebar-bottom {
  display: flex;
  flex-shrink: 0;
  flex-direction: column;
  gap: var(--space-2);
  padding: var(--space-3) 16px var(--space-4);
  border-top: 1px solid var(--color-border);
}

.sidebar-user {
  display: flex;
  align-items: center;
  gap: 10px;
  height: 44px;
  padding: 0 8px;
  border: none;
  border-radius: var(--radius-md);
  background: transparent;
  color: var(--color-text);
  font: inherit;
  cursor: pointer;
  transition: background var(--duration-fast) var(--ease-out);
}

.sidebar-user:hover {
  background: var(--color-surface-hover);
}

.sidebar-user--collapsed {
  justify-content: center;
  padding: 0;
}

.sidebar-user__avatar {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 28px;
  height: 28px;
  flex-shrink: 0;
  border-radius: var(--radius-full);
  background: var(--color-accent);
  color: #fff;
  font-size: var(--text-xs);
  font-weight: 700;
}

.sidebar-user__name {
  overflow: hidden;
  white-space: nowrap;
  text-overflow: ellipsis;
  font-size: var(--text-sm);
  font-weight: 600;
}

.layout-main {
  background: transparent;
}

.layout-content {
  position: relative;
  height: 100dvh;
  overflow: auto;
}

/* Mobile top bar: a sticky header carrying the hamburger + brand, shown only
   below the mobile breakpoint (@mobile-breakpoint) where the persistent sider
   is dropped. */
.mobile-topbar {
  position: sticky;
  top: 0;
  z-index: 10;
  display: flex;
  align-items: center;
  gap: var(--space-3);
  height: 56px;
  padding: 0 var(--space-3);
  background: var(--color-sidebar);
  border-bottom: 1px solid var(--color-border);
}

.mobile-topbar__menu {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 40px;
  height: 40px;
  border: none;
  border-radius: var(--radius-md);
  background: transparent;
  color: var(--color-text);
  cursor: pointer;
  transition: background var(--duration-fast) var(--ease-out);
}

.mobile-topbar__menu:hover {
  background: var(--color-surface-hover);
}

.mobile-topbar__brand {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  color: var(--color-text);
  font-weight: 700;
}

/* The drawer variant of the sidebar shell: the desktop version pins to
   100dvh inside the sider; inside the drawer it should just fill the drawer
   body, which naive already sizes. */

/* With the top bar in play the content no longer starts at the viewport top,
   so its own height must subtract the bar to keep the internal scroll
   working (the desktop rule above uses the full 100dvh). */
@media (max-width: @mobile-breakpoint) {
  .layout-content {
    height: calc(100dvh - 56px);
  }
}

/* Language picker rows inside the modal — a check mark on the right marks the
   active language, matching the shared LocaleSwitcher's treatment. */
.lang-options {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
}

.lang-option {
  display: flex;
  align-items: center;
  justify-content: space-between;
  height: 42px;
  padding: 0 12px;
  border: none;
  border-radius: var(--radius-md);
  background: transparent;
  color: var(--color-text);
  font: inherit;
  font-size: var(--text-sm);
  text-align: left;
  cursor: pointer;
  transition: background var(--duration-fast) var(--ease-out);
}

.lang-option:hover {
  background: var(--color-surface-hover);
}

.lang-option--active {
  color: var(--color-accent);
  font-weight: 600;
}

@media (max-width: @mobile-breakpoint) {
  .sidebar-bottom {
    padding: var(--space-3) 6px var(--space-4);
  }
}
</style>
