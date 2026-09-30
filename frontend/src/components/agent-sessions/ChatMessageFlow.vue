<!-- frontend/src/components/agent-sessions/ChatMessageFlow.vue
     Renders one translated body (the messageTranslator's TranslatedBody) as
     the session drawer's conversation view:

       - kind 'messages'    chat bubbles: assistant on the right, every other
                            role on the left, each bubble carrying its role
                            label; text parts read verbatim (pre-wrap), image
                            parts render as a placeholder block (the payload
                            is deliberately not carried into the DOM), and
                            non-text parts (tool_use / tool_calls / ...) show
                            their type label as a tag
       - kind 'placeholder' a muted "not recorded" note — e.g. a stream
                            request whose response body lives on disk
       - kind 'fallback'    a collapsible formatted-JSON view (the shared
                            BodyViewer), for bodies that cannot be read as
                            chat

     A truncation signal (the backend cut the inline body at 1 MiB) shows a
     hint line next to whatever rendering applies. -->
<template>
  <div class="msg-flow">
    <p v-if="body.truncated" class="msg-flow__truncated">{{ t('agentSessions.flowTruncated') }}</p>

    <template v-if="body.kind === 'messages'">
      <div
        v-for="(message, i) in body.messages"
        :key="i"
        class="msg-flow__row"
        :class="isAssistant(message) ? 'msg-flow__row--right' : 'msg-flow__row--left'"
      >
        <div class="msg-flow__bubble" :class="isAssistant(message) ? 'msg-flow__bubble--assistant' : 'msg-flow__bubble--other'">
          <span class="msg-flow__role">{{ message.role || '—' }}</span>
          <template v-for="(part, j) in message.parts" :key="j">
            <p v-if="part.type === 'text'" class="msg-flow__text">{{ part.text }}</p>
            <div v-else-if="part.type === 'image'" class="msg-flow__image">
              <ImageIcon :size="14" />
              <span>{{ t('agentSessions.partImage') }}</span>
            </div>
            <span v-else class="msg-flow__tool">{{ part.label }}</span>
          </template>
        </div>
      </div>
    </template>

    <div v-else-if="body.kind === 'placeholder'" class="msg-flow__note">
      {{ t('requestLogs.bodyNotRecorded') }}
    </div>

    <NCollapse v-else class="msg-flow__fallback">
      <NCollapseItem :title="t('agentSessions.flowFallbackTitle')" name="raw">
        <BodyViewer :raw="raw" :raw-hint="t('requestLogs.bodyRawHint')" />
      </NCollapseItem>
    </NCollapse>
  </div>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { NCollapse, NCollapseItem } from 'naive-ui'
import { Image as ImageIcon } from '@lucide/vue'
import type { TranslatedBody } from '../../utils/messageTranslator'
import BodyViewer from '../request-logs/BodyViewer.vue'

const props = defineProps<{
  body: TranslatedBody
  /** The raw body string, for the fallback JSON view (ignored otherwise). */
  raw: string
}>()

const { t } = useI18n()

// Only the assistant side reads as "the model talking back" — user prompts,
// system prompts, and tool-role messages all belong on the left.
function isAssistant(message: { role: string }): boolean {
  return message.role === 'assistant'
}
</script>

<style scoped>
.msg-flow {
  display: flex;
  flex-direction: column;
  gap: var(--space-2, 8px);
}

.msg-flow__truncated {
  margin: 0;
  padding: var(--space-1, 4px) var(--space-2, 8px);
  border-radius: var(--radius-md, 6px);
  background: var(--color-warning-subtle, #fdf6ec);
  color: var(--color-text-secondary);
  font-size: var(--text-xs, 12px);
}

.msg-flow__row {
  display: flex;
}

.msg-flow__row--left {
  justify-content: flex-start;
}

.msg-flow__row--right {
  justify-content: flex-end;
}

.msg-flow__bubble {
  max-width: 85%;
  padding: var(--space-2, 8px) var(--space-3, 12px);
  border-radius: var(--radius-md, 6px);
  font-size: var(--text-sm, 13px);
  line-height: 1.6;
}

.msg-flow__bubble--other {
  background: var(--color-bg-soft, #f2f3f5);
  color: var(--color-text);
}

.msg-flow__bubble--assistant {
  background: var(--color-accent-subtle, #eef0fe);
  color: var(--color-text);
}

.msg-flow__role {
  display: block;
  margin-bottom: 2px;
  font-size: var(--text-xs, 11px);
  color: var(--color-text-muted);
  text-transform: uppercase;
  letter-spacing: 0.04em;
}

.msg-flow__text {
  margin: 0;
  white-space: pre-wrap;
  word-break: break-word;
}

.msg-flow__image {
  display: inline-flex;
  align-items: center;
  gap: var(--space-1, 4px);
  padding: var(--space-1, 4px) var(--space-2, 8px);
  border: 1px dashed var(--color-border, #e0e0e6);
  border-radius: var(--radius-md, 6px);
  color: var(--color-text-secondary);
  font-size: var(--text-xs, 12px);
}

.msg-flow__tool {
  display: inline-block;
  padding: 1px var(--space-2, 8px);
  border-radius: 999px;
  background: var(--color-purple-subtle, #f3efff);
  color: var(--color-purple, #7c3aed);
  font-family: var(--font-mono, monospace);
  font-size: var(--text-xs, 12px);
}

.msg-flow__note {
  padding: var(--space-2, 8px) var(--space-3, 12px);
  border-radius: var(--radius-md, 6px);
  background: var(--color-bg-soft, #f2f3f5);
  color: var(--color-text-muted);
  font-size: var(--text-xs, 12px);
}
</style>
