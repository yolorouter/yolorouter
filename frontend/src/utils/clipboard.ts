// frontend/src/utils/clipboard.ts
//
// Single source of truth for writing text to the clipboard: every copy
// button in the console routes through here, directly or via the
// useCopyFeedback composable, so the fallback recipe lives in one place.
// No consumer may call navigator.clipboard itself.
//
// navigator.clipboard is undefined in a non-secure context (plain HTTP — the
// default 127.0.0.1:8084 deploy) and can also reject on permission denial. In
// those cases we fall back to a transient off-screen input + the legacy
// document.execCommand('copy'), which remains the only option without HTTPS.

// copyToClipboard writes `text` to the clipboard, returning whether the write
// succeeded. It tries the async Clipboard API first; when that is unavailable
// (non-secure context) or rejects, it falls back to execCommand. The caller
// owns the user-facing toast — this function stays free of UI side effects.
export async function copyToClipboard(text: string): Promise<boolean> {
  if (navigator.clipboard) {
    try {
      await navigator.clipboard.writeText(text)
      return true
    } catch {
      // Permission denial or clipboard locked — fall through to the legacy path
      // rather than giving up, so a copy still works where execCommand succeeds.
    }
  }
  return copyViaExecCommand(text)
}

// copyViaExecCommand writes text through a transient off-screen input and the
// legacy document.execCommand('copy'). Positioned off-screen rather than
// display:none so select() still works across browsers. Returns false only if
// execCommand reports failure (or throws, e.g. no selection in some browsers).
function copyViaExecCommand(text: string): boolean {
  const input = document.createElement('input')
  input.value = text
  input.style.position = 'fixed'
  input.style.opacity = '0'
  document.body.appendChild(input)
  input.select()
  let ok = false
  try {
    ok = document.execCommand('copy')
  } catch {
    ok = false
  }
  document.body.removeChild(input)
  return ok
}
