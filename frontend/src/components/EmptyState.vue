<!-- Shared inline empty-state block. The mark is a tinted rounded icon tile
     matching the dashboard KPI icon treatment, so an empty list reads as a
     finished state rather than a blank placeholder box. Callers may pass a
     contextual icon; otherwise a neutral "inbox" glyph is used.

     Two slots, for two different things: #action takes the button that leads
     out of the empty state, #detail takes supporting text under the
     description — a diagnostic, a quoted error — which reads left-aligned
     rather than centred with the rest. -->

<script setup lang="ts">
import type { Component } from 'vue'
import { Inbox } from '@lucide/vue'

withDefaults(
  defineProps<{
    title?: string
    description?: string
    icon?: Component
    type?: 'default' | 'compact'
  }>(),
  // A lucide icon is itself a function component; return it from a factory so
  // Vue does not mistake the default for a factory it should invoke as a render.
  { icon: () => Inbox, type: 'default' },
)
</script>

<template>
  <div v-if="type === 'compact'" class="empty-state empty-state--compact">
    <div class="empty-state__mark empty-state__mark--compact icon-tile">
      <component :is="icon" :size="36" :stroke-width="1.75" />
    </div>
    <div v-if="title">{{ title }}</div>
    <p v-if="description">{{ description }}</p>
    <div v-if="$slots.detail" class="empty-state__detail">
      <slot name="detail" />
    </div>
    <div v-if="$slots.action" class="empty-state__action">
      <slot name="action" />
    </div>
  </div>
  <div v-else class="empty-state">
    <div class="empty-state__mark icon-tile">
      <component :is="icon" :size="22" :stroke-width="1.75" />
    </div>
    <h3 v-if="title">{{ title }}</h3>
    <p v-if="description">{{ description }}</p>
    <div v-if="$slots.detail" class="empty-state__detail">
      <slot name="detail" />
    </div>
    <div v-if="$slots.action" class="empty-state__action">
      <slot name="action" />
    </div>
  </div>
</template>
