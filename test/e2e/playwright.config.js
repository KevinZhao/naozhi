const { defineConfig, devices } = require('@playwright/test');

module.exports = defineConfig({
  testDir: '.',
  timeout: 30000,
  // One retry under CI absorbs a one-off flake; locally a failure stays red.
  retries: process.env.CI ? 1 : 0,
  reporter: 'list',
  use: {
    // Keep the trace of every failed attempt for the test-results/ artifact
    // CI uploads.
    trace: 'retain-on-failure',
  },
  projects: [
    {
      name: 'desktop-chrome',
      use: { ...devices['Desktop Chrome'] },
    },
  ],
});
