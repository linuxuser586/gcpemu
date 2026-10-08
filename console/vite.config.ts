import path from 'node:path'

import tailwindcss from '@tailwindcss/vite'
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vitest/config'

// `pnpm dev` serves the console at /console/ and sends every other path
// (the admin API and the public APIs) to a running Instance's API gateway.
const gateway = process.env.GCPEMU_GATEWAY ?? '127.0.0.1:4510'

export default defineConfig({
  base: '/console/',
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: { '@': path.resolve(import.meta.dirname, 'src') },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    // The budget that matters is FR-UI-020's (scripts/check-bundle-size.ts).
    chunkSizeWarningLimit: 1500,
  },
  server: {
    proxy: {
      '^/(?!console(/|$))': { target: `http://${gateway}` },
    },
  },
  test: {
    environment: 'jsdom',
    include: ['src/**/*.test.{ts,tsx}'],
    setupFiles: ['src/test/setup.ts'],
    restoreMocks: true,
  },
})
