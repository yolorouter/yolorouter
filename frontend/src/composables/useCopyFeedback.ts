// frontend/src/composables/useCopyFeedback.ts
//
// Shared copy-button feedback: the label flips to "copied" for two
// seconds on a successful write, and a failed write (plain-HTTP deploy,
// permission denial) says so instead of staying silent.
//
// Callers that need a different feedback SHAPE keep their own small
// handlers over copyToClipboard: a boolean result (copying is the save
// itself, so failure must keep the dialog open), or a plain toast (icon
// buttons have no label to flip). Describing shapes rather than listing
// components — member lists rot the moment the next caller lands.

import { ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useMessage } from 'naive-ui'

import { copyToClipboard } from '../utils/clipboard'

export function useCopyFeedback() {
  const { t } = useI18n()
  const message = useMessage()

  const copied = ref(false)

  async function copy(value: string): Promise<void> {
    if (!value) return
    if (await copyToClipboard(value)) {
      copied.value = true
      setTimeout(() => {
        copied.value = false
      }, 2000)
    } else {
      message.error(t('common.copyFailed'))
    }
  }

  return { copied, copy }
}
