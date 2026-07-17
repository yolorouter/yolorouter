<!-- frontend/src/layouts/DefaultLayout.vue -->
<template>
  <div class="layout">
    <header>
      <span class="brand">
        <img :src="logo" alt="Yolorouter CE" class="brand-logo" />
        <span>Yolorouter CE</span>
      </span>
      <select :value="locale.locale" @change="onChange">
        <option value="zh-CN">简体中文</option>
        <option value="en">English</option>
      </select>
    </header>
    <nav><!-- 导航项由各模块自己往里加 --></nav>
    <main><router-view /></main>
  </div>
</template>

<script setup lang="ts">
import { useLocaleStore } from '../store/locale'
import logo from '../assets/logo.svg'
const locale = useLocaleStore()
function onChange(e: Event) {
  locale.setLocale((e.target as HTMLSelectElement).value as 'zh-CN' | 'en')
}
</script>

<style scoped>
.brand {
  display: inline-flex;
  align-items: center;
  gap: 0.5rem;
}
.brand-logo {
  height: 1.5rem;
  width: auto;
}
</style>
