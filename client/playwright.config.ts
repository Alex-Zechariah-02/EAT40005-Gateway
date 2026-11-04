import { defineConfig } from '@playwright/test'

// The browser tests run one real deployment in order, so they run serially
// in one worker. They use the installed Chrome, not a downloaded browser.
export default defineConfig({
  testDir: 'e2e',
  workers: 1,
  fullyParallel: false,
  timeout: 180_000,
  expect: { timeout: 15_000 },
  reporter: [['list']],
  use: {
    channel: 'chrome',
    headless: true,
    actionTimeout: 15_000,
    viewport: { width: 1280, height: 860 },
    // Kept only for a failing test, so its run can be replayed step by step.
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
})
