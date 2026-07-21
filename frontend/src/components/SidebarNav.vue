<!-- Sidebar nav item group, ported from the reference project's
     layout/SidebarNav.vue (yolorouter-frontend/src/user/layout/SidebarNav.vue)
     so the admin shell reads as the same product line. -->
<script setup lang="ts">
import { computed, type Component } from 'vue'
import { useRoute } from 'vue-router'

export interface NavItem {
  key: string
  label: string
  icon: Component
  to: string
  // badge lights a small indicator dot (e.g. "new version available") at the
  // item's top-right. Optional; absent means no badge.
  badge?: boolean
}

const props = defineProps<{
  items: NavItem[]
  collapsed: boolean
  title?: string
}>()

const route = useRoute()

function isActive(to: string): boolean {
  return route.path === to || route.path.startsWith(to + '/')
}

const resolvedItems = computed(() =>
  props.items.map((item) => ({ ...item, active: isActive(item.to) })),
)
</script>

<template>
  <nav class="sidebar-nav" :class="{ 'sidebar-nav--collapsed': collapsed }">
    <div v-if="title && !collapsed" class="sidebar-nav__title">{{ title }}</div>
    <RouterLink
      v-for="item in resolvedItems"
      :key="item.key"
      :to="item.to"
      class="sidebar-nav-item"
      :class="{ 'sidebar-nav-item--active': item.active }"
    >
      <span class="sidebar-nav-item__icon">
        <component :is="item.icon" :size="18" :stroke-width="1.8" />
      </span>
      <span v-if="!collapsed" class="sidebar-nav-item__label">{{ item.label }}</span>
      <span v-if="item.badge" class="sidebar-nav-item__dot" :title="item.label" />
    </RouterLink>
  </nav>
</template>

<style scoped>
.sidebar-nav {
  display: flex;
  flex-direction: column;
  gap: 2px;
  padding: 0 16px;
}

.sidebar-nav--collapsed {
  padding: 0 6px;
}

.sidebar-nav__title {
  margin: 12px 0 4px 10px;
  color: var(--color-text-muted);
  font-size: 10px;
  font-weight: 700;
  letter-spacing: 0.08em;
  text-transform: uppercase;
}

.sidebar-nav-item {
  position: relative;
  display: flex;
  align-items: center;
  gap: 10px;
  height: 38px;
  padding: 0 10px;
  border-radius: var(--radius-md);
  color: var(--color-text-secondary);
  font-size: var(--text-sm);
  font-weight: 500;
  transition:
    color var(--duration-fast) var(--ease-out),
    background var(--duration-fast) var(--ease-out);
}

.sidebar-nav--collapsed .sidebar-nav-item {
  justify-content: center;
  padding: 0;
}

.sidebar-nav-item:hover,
.sidebar-nav-item--active {
  background: var(--sidebar-active-color);
  color: var(--sidebar-active-text-color);
  box-shadow: rgba(0, 0, 0, 0.06) 0px 1px 2px;
  font-weight: 500;
}

.sidebar-nav-item__icon {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 16px;
  height: 16px;
  flex-shrink: 0;
}

.sidebar-nav-item__label {
  overflow: hidden;
  white-space: nowrap;
  text-overflow: ellipsis;
}

/* The update-available indicator dot. .sidebar-nav-item is position:relative,
   so this absolute dot anchors to the item's top-right corner — visible in
   both expanded and collapsed sidebar states. */
.sidebar-nav-item__dot {
  position: absolute;
  top: 8px;
  right: 10px;
  width: 8px;
  height: 8px;
  border-radius: var(--radius-full);
  background: var(--color-danger, #d03050);
  pointer-events: none;
}
</style>
