import { defineConfig } from 'vitest/config'
import vue from '@vitejs/plugin-vue'

// Explicit test discovery for the build gate: `npm run build` runs
// `vitest run` before the type check and bundle, and this include pins
// what that gate covers. The vue plugin is here for the mounted component
// tests (files opting into a DOM via @vitest-environment); pure-function
// tests keep running in node.
export default defineConfig({
  plugins: [vue()],
  test: {
    include: ['src/**/*.test.ts'],
  },
})
