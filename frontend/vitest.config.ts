import { defineConfig } from 'vitest/config'
import vue from '@vitejs/plugin-vue'

// Explicit test discovery for the build gate: `npm run build` runs
// `vitest run` before the type check and bundle, and this include pins
// what that gate covers. The vue plugin is here for the mounted component
// tests (files opting into a DOM via @vitest-environment); pure-function
// tests keep running in node. The setup file installs the window-timer
// teardown guard (see its header for why the gate needs it).
export default defineConfig({
  plugins: [vue()],
  test: {
    include: ['src/**/*.test.ts'],
    setupFiles: ['./vitest.setup.ts'],
    // The mounted view tests (21-row tables, pagination flips, multi-round
    // awaits) spend 2.5-3.5s each on an idle machine; vitest's 5s default
    // leaves no headroom for suite-parallel transform/import load or a
    // concurrent build, which is how green-at-CI tests timed out locally in
    // flakes that moved between tests run to run. A timeout's job here is
    // catching a hang, not policing speed under load.
    testTimeout: 15000,
  },
})
