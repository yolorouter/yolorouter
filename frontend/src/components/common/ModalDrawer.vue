<!-- frontend/src/components/common/ModalDrawer.vue
     A dialog container that adapts to viewport width:

       - Desktop: a centered naive card modal with an end-aligned two-button
         footer (Cancel + Confirm), the usual dialog.
       - Mobile: a full-screen right-to-left drawer with a back-arrow header
         (no close "×" — the arrow doubles as dismiss / cancel) and a single
         full-width Confirm button, matching a native "push a screen" flow.

     The caller owns the open state (`v-model:show`) and provides the body via
     the default slot. The footer is built in — callers pass button text
     (`confirmText` / `cancelText`) and listen for `confirm`; Cancel closes the
     dialog for them (and emits `cancel` if they need to react). Pass the
     `footer` slot only when a caller needs a fully custom footer. -->
<template>
  <!-- Desktop: centered card modal, end-aligned Cancel + Confirm footer. -->
  <NModal
    v-if="!isMobile"
    v-model:show="show"
    preset="card"
    :title="title"
    :style="{ maxWidth }"
    :mask-closable="maskClosable && dismissable"
    :close-on-esc="closeOnEsc && dismissable"
    :closable="dismissable"
    @after-leave="emit('after-leave')"
  >
    <slot />
    <template #footer>
      <slot name="footer">
        <div class="modal-drawer__actions">
          <NButton v-if="dismissable" @click="onCancel">{{
            cancelText || t("common.cancel")
          }}</NButton>
          <NButton type="primary" :loading="loading" @click="emit('confirm')">
            {{ confirmText }}
          </NButton>
        </div>
      </slot>
    </template>
  </NModal>

  <!-- Mobile: full-screen drawer that slides in from the right. The header's
       back arrow is the only dismiss affordance (native "×" is hidden); the
       footer is a single full-width Confirm button. -->
  <NDrawer
    v-else
    v-model:show="show"
    placement="right"
    width="100%"
    class="modal-drawer"
    :mask-closable="maskClosable && dismissable"
    :close-on-esc="closeOnEsc && dismissable"
    @after-leave="emit('after-leave')"
  >
    <NDrawerContent :native-scrollbar="false" body-content-style="padding: 0;">
      <div class="modal-drawer__inner">
        <header class="modal-drawer__header">
          <button
            type="button"
            class="modal-drawer__back"
            :aria-label="backLabel"
            :disabled="!dismissable"
            @click="onCancel"
          >
            <NIcon :size="22"><ArrowLeft /></NIcon>
          </button>
          <span class="modal-drawer__title">{{ title }}</span>
        </header>
        <div class="scroll-container">
          <div class="modal-drawer__body">
            <slot />
          </div>

          <footer class="modal-drawer__footer">
            <slot name="footer">
              <NButton
                type="primary"
                block
                :loading="loading"
                @click="emit('confirm')"
              >
                {{ confirmText }}
              </NButton>
            </slot>
          </footer>
        </div>
      </div>
    </NDrawerContent>
  </NDrawer>
</template>

<script setup lang="ts">
import { NButton, NDrawer, NDrawerContent, NIcon, NModal } from "naive-ui";
import { ArrowLeft } from "@lucide/vue";
import { useI18n } from "vue-i18n";
import { useIsMobile } from "../../composables/useIsMobile";

const props = withDefaults(
  defineProps<{
    title: string;
    confirmText?: string;
    cancelText?: string;
    loading?: boolean;
    backLabel?: string;
    maxWidth?: string;
    // Apply to both renderings: the desktop modal's mask/Esc, and the
    // mobile drawer's mask/Esc (naive-ui defaults both to true there too —
    // the back arrow is the visible affordance, not the only one).
    maskClosable?: boolean;
    closeOnEsc?: boolean;
    // When false, every dismiss affordance is withheld (desktop Cancel
    // button hidden, mobile back arrow disabled) — for flows that have
    // passed their point of no return, where closing the dialog would only
    // hide an operation that keeps running.
    dismissable?: boolean;
  }>(),
  {
    confirmText: "",
    cancelText: "",
    loading: false,
    backLabel: "Back",
    maxWidth: "400px",
    maskClosable: true,
    closeOnEsc: true,
    dismissable: true,
  },
);

const emit = defineEmits<{
  confirm: [];
  cancel: [];
  "after-leave": [];
}>();

const { t } = useI18n();

const show = defineModel<boolean>("show", { required: true });

// Close the dialog if the viewport crosses the breakpoint, so a modal opened on
// desktop doesn't strand as a half-rendered drawer (or vice versa) after a
// resize/rotate — but never while the dialog is locked (`dismissable=false`):
// a rotate mid-operation must not hide progress the user cannot reopen. The
// dialog then re-renders in the other branch with its state intact.
const isMobile = useIsMobile(() => {
  if (props.dismissable) show.value = false;
});

// Cancel (desktop button or mobile back arrow) closes the dialog for the caller
// and notifies them, so they don't have to wire the close themselves.
function onCancel() {
  show.value = false;
  emit("cancel");
}
</script>

<style scoped>
.modal-drawer__actions {
  display: flex;
  justify-content: flex-end;
  gap: var(--space-2);
}

:deep(.modal-drawer.n-drawer) {
  background: var(--color-bg);
}

.modal-drawer__inner {
  display: flex;
  flex-direction: column;
  height: 100%;
  min-height: 0;
}

.modal-drawer__header {
  display: flex;
  flex-shrink: 0;
  align-items: center;
  gap: var(--space-2);
  height: 56px;
  padding: 0 var(--space-2) 0 var(--space-1);
  border-bottom: 1px solid var(--color-border);
}

.modal-drawer__back {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 40px;
  height: 40px;
  border: none;
  border-radius: var(--radius-md);
  background: transparent;
  color: var(--color-text);
  cursor: pointer;
  transition: background var(--duration-fast) var(--ease-out);
}

.modal-drawer__back:active {
  background: var(--color-surface-hover);
}

.modal-drawer__back:disabled {
  opacity: 0.35;
  cursor: not-allowed;
}

.modal-drawer__title {
  font-size: var(--text-base);
  font-weight: 600;
  color: var(--color-text);
}

.modal-drawer__body {
  flex: 1;
  min-height: 0;
  overflow-y: auto;
  padding: var(--space-4) var(--space-4) var(--space-3);
}

.modal-drawer__footer {
  flex-shrink: 0;
  padding: var(--space-3) var(--space-4) var(--space-4);
}
.scroll-container {
  max-height: calc(100vh - 56px);
  overflow: auto;
}
</style>
