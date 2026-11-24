// E2E: the 系统 view's 接口监控 section renders GET /api/system/apiwatch
// (totals, latency, the two bar charts, the problem classes and the recent
// problems) and its window buttons ask for another range.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const now = Date.now();
const iso = (ms) => new Date(ms).toISOString();
const buckets = Array.from({ length: 24 }, (_, i) => ({
  start: iso(now - (24 - i) * 3600000), turns: 4, failed: i === 20 ? 1 : 0,
  retries: i === 20 ? 3 : 0, timeouts: 0, ttft_p95: 1500 + i * 100, api_p95: 8000,
}));
const REGION = 'API Error: 400 Access to Anthropic models is not allowed from unsupported countries, regions, or territories.';
const REPORT = {
  since: iso(now - 86400000), until: iso(now), bucket_sec: 3600,
  summary: { turns: 96, failed_turns: 1, retries: 3, timeouts: 0, retried_turns: 1,
    ttft: { n: 96, p50: 1800, p95: 3200, max: 9000 }, api: { n: 96, p50: 6000, p95: 21000, max: 64000 } },
  buckets,
  classes: [
    { class: 'region', count: 1, last: iso(now - 4 * 3600000), sample: REGION },
    { class: 'network', count: 3, last: iso(now - 4 * 3600000) },
  ],
  sessions: [{ key: 'dashboard:direct:x:general', turns: 10, problems: 4, last: iso(now - 4 * 3600000) }],
  recent: [
    { at: iso(now - 4 * 3600000), kind: 'turn', key: 'dashboard:direct:x:general', status: 400, failed: true, error: REGION, class: 'region' },
    { at: iso(now - 4 * 3600000 - 5000), kind: 'retry', key: 'dashboard:direct:x:general', attempt: 2, max_retries: 10, retry_delay_ms: 1200, category: 'unknown', class: 'network' },
  ],
  slowest: [{ at: iso(now - 7200000), kind: 'turn', key: 'dashboard:direct:x:general', model: 'claude-opus-5-5', api_ms: 64000, ttft_ms: 9000, duration_ms: 80000 }],
};

let mock;

test.beforeAll(async () => {
  mock = await startMockServer({ apiWatch: REPORT });
});

test.afterAll(async () => {
  await new Promise(r => mock.server.close(r));
});

test('apiwatch section shows totals, charts, classes and recent problems', async ({ page }) => {
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  await page.goto(`${mock.url}/dashboard`);
  await page.waitForSelector('.session-card');
  await page.click('#abnav-system');

  const section = page.locator('.sys-apiwatch');
  await expect(section.locator('.aw-stat')).toHaveCount(6);
  await expect(section.locator('.aw-stat').nth(0)).toContainText('96');
  await expect(section.locator('.aw-stat').nth(4)).toContainText('1.8s / 3.2s');
  await expect(section.locator('.aw-chart')).toHaveCount(2);
  await expect(section.locator('.aw-chart').first().locator('.aw-bar')).toHaveCount(24);
  await expect(section.locator('.aw-chart').first().locator('.aw-chart-max')).toHaveText('峰值 4');
  await expect(section.locator('.aw-class')).toHaveCount(2);
  await expect(section.locator('.aw-class').first()).toContainText('地区限制');
  await expect(section.locator('.aw-class').first()).toContainText('VPN');
  const rows = section.locator('.aw-recent tbody tr');
  await expect(rows).toHaveCount(2);
  await expect(rows.first()).toContainText('HTTP 400');
  await expect(rows.nth(1)).toContainText('第 2/10 次');
  await expect(rows.nth(1)).toContainText('连接错误');
  // The health section is untouched: it 404s here and takes no room.
  await expect(page.locator('.sys-health')).toHaveCount(0);

  expect(mock.apiWatchCalls[0]).toBe('24');
  await section.locator('[data-action="apiwatch-window"][data-hours="168"]').click();
  await expect.poll(() => mock.apiWatchCalls.at(-1)).toBe('168');
  await expect(section.locator('.aw-win.on')).toHaveText('7 天');
  expect(errors).toEqual([]);
});
