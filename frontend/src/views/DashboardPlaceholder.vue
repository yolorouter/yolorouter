<template>
  <n-empty :description="t('common.empty')" style="margin-top: 20vh" />
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
const { t } = useI18n()
</script>
