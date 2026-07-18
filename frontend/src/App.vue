<script setup lang="ts">
import { computed } from 'vue'
import { zhCN, enUS, dateZhCN, dateEnUS, type GlobalThemeOverrides } from 'naive-ui'
import { useLocaleStore } from './store/locale'

const localeStore = useLocaleStore()
const naiveLocale = computed(() => (localeStore.locale === 'en' ? enUS : zhCN))
const naiveDateLocale = computed(() => (localeStore.locale === 'en' ? dateEnUS : dateZhCN))

// Matches the reference project's accent color exactly
// (yolorouter-frontend/src/user/App.vue) so this admin UI reads as the
// same product, not a second, differently-branded tool.
const themeOverrides: GlobalThemeOverrides = {
  common: {
    primaryColor: '#6467f2',
    primaryColorHover: '#7375f2',
    primaryColorSuppl: '#7375f2',
    primaryColorPressed: '#7375f2',
  },
}
</script>

<template>
  <n-config-provider :locale="naiveLocale" :date-locale="naiveDateLocale" :theme-overrides="themeOverrides">
    <n-global-style />
    <n-message-provider>
      <n-dialog-provider>
        <router-view />
      </n-dialog-provider>
    </n-message-provider>
  </n-config-provider>
</template>
