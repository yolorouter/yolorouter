<!-- Shared language switcher — used by both DefaultLayout.vue (inside the
     protected app shell) and AuthCard.vue (setup/login pages, which render
     outside DefaultLayout — see router/index.ts), so adding a language or
     changing the option list only needs one edit instead of two kept in
     lockstep by hand. -->
<template>
  <n-select
    :value="locale.locale"
    :options="options"
    size="small"
    style="width: 128px"
    @update:value="onLocaleChange"
  />
</template>

<script setup lang="ts">
import { type SelectOption } from 'naive-ui'
import { useLocaleStore } from '../store/locale'

const locale = useLocaleStore()

const options: SelectOption[] = [
  { label: '简体中文', value: 'zh-CN' },
  { label: 'English', value: 'en' },
]

function onLocaleChange(value: 'zh-CN' | 'en') {
  locale.setLocale(value)
}
</script>
