<!-- Shared shell for the setup/login pages (frontend/src/views/auth/*.vue):
     centered card, title + subtitle, and a slot for the page's own form.
     Neither page renders inside DefaultLayout (they're top-level routes,
     not its children — see router/index.ts), so DefaultLayout's own
     locale <select> never reaches them; this is the only other place a
     language switcher can live, and PRD §11.1 requires one on both pages. -->
<template>
  <div class="auth-page">
    <LocaleSwitcher class="auth-locale-select" />
    <div class="auth-card-wrap">
      <div class="auth-card">
        <h1 class="auth-title">{{ title }}</h1>
        <p class="auth-subtitle">{{ subtitle }}</p>
        <slot />
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import LocaleSwitcher from './LocaleSwitcher.vue'

defineProps<{ title: string; subtitle: string }>()
</script>

<style scoped>
.auth-page {
  position: relative;
  min-height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 24px 16px;
  box-sizing: border-box;
  background:
    radial-gradient(circle at 18% -8%, oklch(80% 0.11 248 / 0.28), transparent 34rem),
    radial-gradient(circle at 92% 8%, oklch(82% 0.1 300 / 0.18), transparent 30rem),
    var(--color-bg);
}

.auth-card-wrap {
  width: 100%;
  max-width: 420px;
}

.auth-card {
  background: var(--color-surface);
  border: 1px solid var(--color-border-subtle);
  border-radius: var(--radius-xl);
  box-shadow: var(--shadow-lg);
  padding: 40px 32px;
  box-sizing: border-box;
}

.auth-locale-select {
  position: absolute;
  top: 16px;
  right: 16px;
}

.auth-title {
  margin: 0;
  font-size: var(--text-2xl);
  font-weight: 800;
  line-height: 1.1;
  color: var(--color-text);
}

.auth-subtitle {
  margin: var(--space-2) 0 0;
  color: var(--color-text-secondary);
  font-size: var(--text-sm);
}

@media (max-width: 480px) {
  .auth-card {
    padding: 32px 20px;
  }
}
</style>
