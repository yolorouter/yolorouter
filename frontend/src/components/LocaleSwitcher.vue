<!-- Shared language <select> — used by both DefaultLayout.vue (inside the
     protected app shell) and AuthCard.vue (setup/login pages, which render
     outside DefaultLayout — see router/index.ts), so adding a language or
     changing the option list only needs one edit instead of two kept in
     lockstep by hand. -->
<template>
  <select :value="locale.locale" @change="onLocaleChange">
    <option value="zh-CN">简体中文</option>
    <option value="en">English</option>
  </select>
</template>

<script setup lang="ts">
import { useLocaleStore } from '../store/locale'

const locale = useLocaleStore()
function onLocaleChange(e: Event) {
  locale.setLocale((e.target as HTMLSelectElement).value as 'zh-CN' | 'en')
}
</script>
