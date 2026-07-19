import { useDialog } from 'naive-ui'

type DialogApi = ReturnType<typeof useDialog>

interface ConfirmDisableCopy {
  title: string
  content: string
  positiveText: string
  negativeText: string
}

// Shared "toggle management_status, confirm before disabling" flow used by
// ModelListPage.vue and ModelDetailPage.vue: enabling proceeds immediately,
// disabling shows a confirm dialog first. `proceed` performs the actual
// toggle + reload and owns its own error handling.
export function toggleStatusWithConfirm(
  dialog: DialogApi,
  enable: boolean,
  confirmCopy: ConfirmDisableCopy,
  proceed: () => Promise<void>,
) {
  if (!enable) {
    dialog.warning({ ...confirmCopy, onPositiveClick: proceed })
    return
  }
  void proceed()
}
