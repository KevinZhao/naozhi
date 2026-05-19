// @ts-check
const { defineConfig, devices } = require('@playwright/test');

module.exports = defineConfig({
  testDir: '.',
  timeout: 30000,
  retries: 0,
  reporter: 'list',
  // globalSetup runs once before any test process starts. The
  // multibackend.* spec uses NAOZHI_LIVE_E2E=1 to opt into a single
  // auth/login round-trip that writes the cookie state to disk; tests
  // then rehydrate from that file without re-logging in (login is
  // per-IP rate-limited at ~5/min, so re-logging per beforeAll trips
  // 429 by case 4).
  globalSetup: require.resolve('./multibackend.global-setup.js'),
  use: {
    trace: 'on-first-retry',
  },
  projects: [
    {
      name: 'desktop-chrome',
      use: { ...devices['Desktop Chrome'] },
    },
    {
      name: 'mobile-safari',
      use: { ...devices['iPhone 13'] },
    },
  ],
});
