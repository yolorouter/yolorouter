<!-- The shared page header: eyebrow + title + description, with an actions
     slot on the right. title-suffix renders inline after the title text for
     a badge-style annotation (e.g. a model's modality); absent on most
     pages, so it renders nothing unless a page provides it. -->
<script setup lang="ts">
defineProps<{
  eyebrow?: string
  title: string
  description?: string
}>()
</script>

<template>
  <header class="page-header">
    <div>
      <h1 class="page-header__title">
        {{ title }}<span v-if="$slots['title-suffix']" class="page-header__title-suffix"><slot name="title-suffix" /></span>
      </h1>
      <p v-if="description" class="page-header__description">{{ description }}</p>
    </div>
    <div v-if="$slots.actions" class="page-header__actions">
      <slot name="actions" />
    </div>
  </header>
</template>

<style scoped>
.page-header__title-suffix {
  margin-left: 8px;
  vertical-align: middle;
}
</style>
