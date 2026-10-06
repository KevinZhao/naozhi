// @ts-check
const { defineConfig, devices } = require('@playwright/test');

module.exports = defineConfig({
  testDir: '.',
  timeout: 30000,
  // No retries: a retried pass turns the job green and the flaky-report
  // job (ci.yml) never sees the failure, so the flake goes unfiled.
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
    // With retries at 0, 'on-first-retry' never records anything, so a
    // [flaky] issue carried only the assertion line. On CI every test is
    // traced and the trace is kept only when it fails (#3440); locally the
    // cheaper mode stays.
    trace: process.env.CI ? 'retain-on-failure' : 'on-first-retry',
    screenshot: 'only-on-failure',
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
