// useCCSwitchImport centralizes the CC-Switch deep-link import used by both the
// models list and the API-keys list. Deep links can't report back whether the
// desktop app actually handled them, so we heuristically detect a successful
// hand-off: if the tab loses focus / is hidden shortly after we navigate to the
// ccswitch:// URL, the OS most likely opened the app; if nothing happens within
// a few seconds while the tab is still visible, the protocol handler is
// probably not registered and we surface an install hint.
import { onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useDialog, useMessage } from 'naive-ui'

import { useGatewayEndpoint } from './useGatewayEndpoint'

// Fallback when the caller cannot supply the real key (keys created before
// plaintext persistence existed cannot be read back): the import carries a
// placeholder the user replaces inside CC-Switch.
const PLACEHOLDER_API_KEY = 'sk-'

// Milliseconds to wait for a focus/visibility change before deciding the deep
// link was not handled by any installed app.
const OPEN_DETECT_MS = 5000

export interface CCSwitchImportParams {
  // Provider display name shown in CC-Switch.
  name: string
  // Optional model name to preselect; omitted for provider-only imports.
  model?: string
  // The real key plaintext. When omitted (unreadable legacy key), the import
  // carries the placeholder and the user pastes the key inside CC-Switch.
  apiKey?: string
}

export function useCCSwitchImport() {
  const { t } = useI18n()
  const message = useMessage()
  const dialog = useDialog()
  // The exported profile has to carry the same address the console shows
  // beside the keys. The console's own origin is not it whenever the
  // gateway sits behind a proxy or a configured external_url — exporting
  // that would hand the user a profile that cannot reach the gateway.
  // Never empty: the composable falls back to the origin itself.
  const { endpoint: gatewayEndpoint } = useGatewayEndpoint()

  let openTimer: ReturnType<typeof setTimeout> | null = null
  let openCleanup: (() => void) | null = null

  function buildUrl(p: CCSwitchImportParams): string {
    const base = gatewayEndpoint.value
    const params = new URLSearchParams({
      resource: 'provider',
      app: 'claude',
      name: p.name,
      endpoint: base,
      apiKey: p.apiKey || PLACEHOLDER_API_KEY,
      homepage: base,
    })
    if (p.model) params.set('model', p.model)
    return `ccswitch://v1/import?${params.toString()}`
  }

  // importToCCS launches the deep link, but only while the browser still
  // honors it: an external-protocol navigation needs live transient user
  // activation (roughly a five-second window after the last real click), and
  // callers may have awaited network requests since that click. Once the
  // window has expired the navigation would be silently blocked and the
  // no-focus-change heuristic below would then report "not installed" on a
  // perfectly good installation — so instead we ask for one fresh click and
  // launch from that click's own handler. Browsers without the
  // userActivation API keep the direct-launch behavior.
  function importToCCS(p: CCSwitchImportParams) {
    const activation = window.navigator.userActivation
    if (activation && !activation.isActive) {
      dialog.info({
        title: t('ccswitch.confirmLaunchTitle'),
        content: t('ccswitch.confirmLaunchContent'),
        positiveText: t('ccswitch.confirmLaunchButton'),
        negativeText: t('common.cancel'),
        onPositiveClick: () => launchCCS(p),
      })
      return
    }
    launchCCS(p)
  }

  function launchCCS(p: CCSwitchImportParams) {
    let maybeOpened = false

    const cleanup = () => {
      window.removeEventListener('blur', markOpened)
      window.removeEventListener('pagehide', markOpened)
      document.removeEventListener('visibilitychange', handleVisibilityChange)
    }
    const markOpened = () => {
      maybeOpened = true
      cleanup()
    }
    const handleVisibilityChange = () => {
      if (document.hidden) markOpened()
    }

    // Cancel any in-flight detection from a previous click so overlapping
    // imports don't race their timers/listeners.
    if (openTimer) {
      clearTimeout(openTimer)
      openTimer = null
    }
    if (openCleanup) {
      openCleanup()
      openCleanup = null
    }

    window.addEventListener('blur', markOpened, { once: true })
    window.addEventListener('pagehide', markOpened, { once: true })
    document.addEventListener('visibilitychange', handleVisibilityChange)
    openCleanup = cleanup

    message.info(t('ccswitch.opening'))
    window.location.href = buildUrl(p)

    openTimer = setTimeout(() => {
      cleanup()
      openTimer = null
      openCleanup = null
      if (!maybeOpened && document.visibilityState === 'visible') {
        message.error(t('ccswitch.openFailed'))
      }
    }, OPEN_DETECT_MS)
  }

  // A detection started right before the component unmounts (e.g. the user
  // clicks Import then navigates away) would otherwise leave a dangling timer
  // and listeners that fire a misleading toast on an unrelated page.
  onUnmounted(() => {
    if (openTimer) {
      clearTimeout(openTimer)
      openTimer = null
    }
    if (openCleanup) {
      openCleanup()
      openCleanup = null
    }
  })

  return { importToCCS }
}
