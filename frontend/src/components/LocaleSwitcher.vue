<!-- Shared language switcher — used by both DefaultLayout.vue (inside the
     protected app shell) and AuthCard.vue (setup/login pages, which render
     outside DefaultLayout — see router/index.ts), so adding a language or
     changing the option list only needs one edit instead of two kept in
     lockstep by hand. Borderless ghost trigger + dropdown menu; the active
     language carries a check mark. -->
<template>
  <!-- Desktop: borderless trigger + naive dropdown menu. -->
  <n-dropdown
    v-if="!isMobile"
    trigger="click"
    placement="bottom-end"
    :options="options"
    @select="onLocaleChange"
  >
    <button class="locale-switch" type="button" aria-haspopup="menu">
      <NIcon :size="16" class="locale-switch__globe"><Globe /></NIcon>
      <span class="locale-switch__label">{{ currentLabel }}</span>
      <NIcon :size="14" class="locale-switch__chevron"><ChevronDown /></NIcon>
    </button>
  </n-dropdown>

  <!-- Mobile: the same trigger raises the shared OptionSheet (a plain value
       picker over LOCALES), so every phone overlay reads the same. -->
  <template v-else>
    <button class="locale-switch" type="button" aria-haspopup="menu" @click="sheetOpen = true">
      <NIcon :size="16" class="locale-switch__globe"><Globe /></NIcon>
      <span class="locale-switch__label">{{ currentLabel }}</span>
      <NIcon :size="14" class="locale-switch__chevron"><ChevronDown /></NIcon>
    </button>

    <OptionSheet
      v-model:show="sheetOpen"
      :options="LOCALES"
      :value="locale.locale"
      :height="sheetHeight"
      @select="onSheetSelect"
    />
  </template>
</template>

<script setup lang="ts">
import { computed, h, ref } from 'vue'
import { NIcon, type DropdownOption } from 'naive-ui'
import { Globe, ChevronDown, Check } from '@lucide/vue'
import OptionSheet from './common/OptionSheet.vue'
import { useLocaleStore } from '../store/locale'
import { useIsMobile } from '../composables/useIsMobile'
import { LOCALES } from '../i18n'

const locale = useLocaleStore()

const currentLabel = computed(
  () => LOCALES.find((l) => l.value === locale.locale)?.label ?? LOCALES[0].label,
)

// Close the sheet if the viewport grows back to desktop, so it never strands
// an open overlay over the dropdown trigger (same guard as TimeRangeSelect).
const sheetOpen = ref(false)
const sheetHeight = 200
const isMobile = useIsMobile(() => {
  sheetOpen.value = false
})

// A leading check mark marks the active language; inactive rows reserve the
// same slot (a transparent placeholder) so labels stay left-aligned.
const options = computed<DropdownOption[]>(() =>
  LOCALES.map((l) => ({
    label: l.label,
    key: l.value,
    icon: () =>
      h(
        NIcon,
        { style: l.value === locale.locale ? undefined : { opacity: 0 } },
        { default: () => h(Check) },
      ),
  })),
)

function onLocaleChange(value: 'zh-CN' | 'en' | 'ru-RU') {
  locale.setLocale(value)
}

function onSheetSelect(value: 'zh-CN' | 'en' | 'ru-RU') {
  locale.setLocale(value)
}
</script>

<style scoped>
.locale-switch {
  display: inline-flex;
  align-items: center;
  gap: var(--space-2);
  height: 34px;
  padding: 0 10px;
  border: none;
  border-radius: var(--radius-md);
  background: transparent;
  color: var(--color-text-secondary);
  font: inherit;
  font-size: var(--text-sm);
  cursor: pointer;
  transition: background var(--duration-fast) var(--ease-out), color var(--duration-fast) var(--ease-out);
}

.locale-switch:hover {
  background: var(--color-surface-hover);
  color: var(--color-text);
}

.locale-switch__globe {
  color: var(--color-text-muted);
}

.locale-switch__chevron {
  color: var(--color-text-muted);
}
</style>
