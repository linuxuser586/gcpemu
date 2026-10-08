import { defineConfig, devices } from '@playwright/test'

// FR-UI-023: the console against a real Ephemeral Instance on Chromium,
// Firefox and WebKit. The GKE view tests create real clusters and skip
// without a container runtime; nothing else needs one. Its
// gateway URL reaches the workers through the environment.
export default defineConfig({
  testDir: 'e2e',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [['list'], ['html', { open: 'never' }]] : 'list',
  use: {
    baseURL: process.env.GCPEMU_E2E_URL,
    trace: 'retain-on-failure',
  },
  projects: [
    { name: 'chromium', use: { ...devices['Desktop Chrome'] } },
    { name: 'firefox', use: { ...devices['Desktop Firefox'] } },
    { name: 'webkit', use: { ...devices['Desktop Safari'] } },
  ],
  webServer: {
    command: 'node e2e/emulator.ts',
    wait: { stdout: /gcpemu ready at (?<gcpemu_e2e_url>http:\/\/\S+)/ },
    timeout: 90_000,
    gracefulShutdown: { signal: 'SIGTERM', timeout: 15_000 },
  },
})
