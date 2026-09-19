// @ts-check
//
// session 运行记录面板消费统一 run 词表（#2540）：/api/sessions/runs 现在说
// state（runtelemetry.RunState）而不是私有的 outcome 枚举，且渲染词表与 cron
// 时间轴共用一份（nz_util.runStateDot/runStateLabel）。本 spec 钉住：同一个
// timed_out 在 session 面板与 cron 时间轴呈现同一个中文标签与同一个色类 ——
// 三种 run 历史"同状态同呈现"正是 DTO 合并的用户可见收益。
//
// 跑法：cd test/e2e && npx playwright test session_runs_state_vocab.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';

test.describe('session 运行记录的统一词表', () => {
  /** @type {Awaited<ReturnType<typeof startMockServer>>} */
  let mock;
  test.beforeAll(async () => {
    mock = await startMockServer({
      sessionRuns: {
        [KEY]: {
          runs: [
            { run_id: 'r-ok', subsystem: 'session', started_at: Date.now() - 600000, duration_ms: 5000, state: 'succeeded', cost_usd: 0.1 },
            { run_id: 'r-to', subsystem: 'session', started_at: Date.now() - 300000, duration_ms: 9000, state: 'timed_out' },
            { run_id: 'r-ca', subsystem: 'session', started_at: Date.now() - 100000, duration_ms: 800, state: 'canceled' },
          ],
          stats: { count: 3, total_ms: 14800, total_cost_usd: 0.1, timeout_count: 1 },
        },
      },
    });
  });
  test.afterAll(() => mock.server.close());

  test('state 词表渲染：succeeded/timed_out/canceled 的标签与色类', async ({ page }) => {
    await page.setViewportSize({ width: 1280, height: 800 });
    await page.goto(`${mock.url}/dashboard`);
    await page.waitForSelector(`.session-card[data-key="${KEY}"]`);
    const runsSettled = page.waitForResponse((r) => r.url().includes('/api/sessions/runs'));
    await page.locator(`.session-card[data-key="${KEY}"]`).click();
    await runsSettled;

    // 面板是 <details>，默认折叠；点 summary 展开后行才参与 innerText。
    const panel = page.locator('#session-runs-panel');
    await panel.waitFor();
    await panel.locator('.srp-summary').click();

    // 与 cron 时间轴同一份词表：succeeded→成功、timed_out→超时、canceled→已取消。
    const labels = await panel.locator('.srr-state').allInnerTexts();
    expect(labels).toContain('成功');
    expect(labels).toContain('超时');
    expect(labels).toContain('已取消');
    // 旧 outcome 词表的痕迹不得出现（'完成'/'出错' 是旧表独有的措辞）。
    expect(labels).not.toContain('完成');

    // 色类跟着统一词表走：timed_out 是 warn（琥珀），不再是旧表的红。
    await expect(panel.locator('.srr-dot.ok')).toHaveCount(1);
    await expect(panel.locator('.srr-dot.warn')).toHaveCount(1);
    await expect(panel.locator('.srr-dot.cancel')).toHaveCount(1);
    await expect(panel.locator('.srr-dot.timeout')).toHaveCount(0);
  });
});
