import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

const backendTarget = process.env.VITE_BACKEND_TARGET || 'http://127.0.0.1:8080'

export default defineConfig({
  plugins: [vue()],
  // Pages are route-level lazy imports, so the entry stays ~70 kB and every
  // page loads its own small chunk. Two on-demand chunks legitimately exceed
  // Vite's default 500 kB advisory: the echarts bundle (chart pages only)
  // and the shared UI chunk. Fragmenting a single vendor further has no
  // navigational benefit (it is one cacheable file either way) — and the
  // rolldown group auto-splitting that tried it produced broken cross-chunk
  // bindings (a blank page with "TypeError: w is not a function"). Raise the
  // advisory to cover the largest legitimate on-demand chunk instead.
  build: {
    chunkSizeWarningLimit: 700,
  },
  server: {
    // Expose the dev server on all interfaces so the frontend is reachable
    // from other devices on the LAN (e.g. a phone used to test the mobile
    // layout). Fixed port so the proxy + bookmarks stay stable.
    host: '0.0.0.0',
    port: 5173,
    proxy: {
      '/healthz': backendTarget,
      '/api': backendTarget,
      '/v1': backendTarget,
      // External-login callback must flow through the dev server too: the
      // backend derives the OAuth redirect URI from the request's Host
      // header (which this proxy preserves), and the state-binding cookie
      // lives on the dev-server origin — a callback that bypassed the
      // proxy would arrive without it and be rejected.
      '/oauth/callback': backendTarget,
    },
  },
  css: {
    preprocessorOptions: {
      less: {
        // Single source of truth for the responsive breakpoint. Prepended to
        // every LESS compile — plain .less files AND <style lang="less"> blocks
        // in .vue — so @mobile-breakpoint resolves everywhere. Kept in sync
        // with the JS constant MOBILE_BREAKPOINT in composables/useIsMobile.ts.
        // CSS media queries can't read a JS/CSS variable, so the number lives
        // in these two places by design; use @mobile-breakpoint rather than
        // hard-coding 768px.
        additionalData: '@mobile-breakpoint: 768px;',
      },
    },
  },
})
