// The global outbound-proxy mode, as the provider pages need it: their
// per-row warning icon marks providers whose stance resolves to proxied,
// and an inheriting ('') stance resolves against the GLOBAL setting. Each
// consuming page fetches the setting once on its own mount — the value
// only changes through the settings page, so a fresh mount is exactly the
// moment a stale snapshot would otherwise survive. A failed fetch keeps
// 'off', mirroring the backend's fail-open-to-direct default: better a
// missing warning than one invented by a network hiccup.
import { ref } from 'vue'

import { getOutboundProxy } from '../api/systemSettings'
import { outboundProxyFormFromSetting } from '../utils/outboundProxy'

export function useGlobalProxyMode() {
  const mode = ref<'off' | 'system' | 'custom'>('off')
  const fetchGlobalMode = () =>
    getOutboundProxy()
      .then((setting) => {
        mode.value = outboundProxyFormFromSetting(setting).mode
      })
      .catch(() => {
        // Keep 'off' — same fail-open posture as the transport layer's read.
      })
  return { mode, fetchGlobalMode }
}
