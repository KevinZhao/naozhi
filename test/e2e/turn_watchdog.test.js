// @ts-check
//
// 卡死的「处理中…」横幅必须自愈：session_state 的 running 推送到达后，如果
// 终态信号（result 事件 / ready 广播）在连接仍然存活时被丢弃，唯一能把横幅
// 收回去的就是 fetchSessions 的 reconcile（onSessionState 会把 lastVersion
// 清零，保证下一次 poll 穿过 version 短路门；updateSendButton('running')
// 还会启动 turn watchdog 的兜底轮询）。
//
// 这条链路曾由 static_turn_watchdog_contract_test.go 用源码 grep 钉着，删除
// 时的理由是「mock-server 拒绝 WS 升级，行为化不可达」——ws:true 出现后该
// 前提不再成立，本 spec 把它整链行为化：推 running、故意不发终态、REST 快照
// 一直说 ready，断言横幅在一个 poll 周期内收回。
//
// 跑法：cd test/e2e && npx playwright test turn_watchdog.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, '仅 desktop-chrome project 跑');
  }
});

test.describe('掉了终态信号的 turn 自愈', () => {
  /** @type {Awaited<ReturnType<typeof startMockServer>>} */
  let mock;
  // 首次加载之后的 sessions 响应扣住 3s：自愈（REST reconcile）最快也要
  // 等到那次响应落地，running 态的断言窗口不再和它赛跑——修 #2777 上线后
  // 约 1/4 概率的抖动（自愈近乎瞬时，btn-stop 在断言前就被收回了）。
  test.beforeAll(async () => { mock = await startMockServer({ ws: true, sessionsDelayMs: 3000, sessionsDelayAfterCalls: 1 }); });
  test.afterAll(() => mock.server.close());

  test('WS 推 running 后终态丢失，REST reconcile 在一个 poll 周期内收回横幅', async ({ browser }) => {
    const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
    const page = await ctx.newPage();
    /** @type {string[]} */
    const pageErrors = [];
    page.on('pageerror', (e) => pageErrors.push(String(e)));
    await page.clock.install();

    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click(`.session-card[data-key="${KEY}"]`);
    await expect(page.locator('#msg-input')).toBeVisible();

    // WS 已经完成 auth 握手（ws:true 的 mock 应答 auth_ok）才谈得上
    // 「连接存活时信号被丢」。
    await expect.poll(() => mock.wsConnections.length, { timeout: 5000 }).toBeGreaterThan(0);
    const conn = mock.wsConnections[mock.wsConnections.length - 1];

    // 推 running；REST /api/sessions 里这个会话一直是 ready —— 等价于
    // 终态信号在网络上蒸发了。
    conn.send({ type: 'session_state', key: KEY, state: 'running' });

    // 两个界面都进入 running：主区换成停止按钮 + 横幅出现，侧栏圆点变 running。
    await expect(page.locator('#btn-stop')).toBeVisible();
    await expect(page.locator('#running-banner')).not.toHaveClass(/nz-hidden/);
    await expect(page.locator(`.session-card[data-key="${KEY}"] .sc-dot`)).toHaveClass(/dot-running/);

    // 不发任何终态。推送之后发出的 poll 才能对齐（推送之前发出的快照不能
    // 覆盖推送），WS 在线时它来自 watchdog 的 15s tick：快进一个 tick，
    // 再等 mock 扣住的 3s。卡死则一直是停止按钮。
    await page.clock.runFor(16000);
    await expect(page.locator('#btn-send')).toBeVisible({ timeout: 9000 });
    await expect(page.locator('#btn-stop')).toBeHidden();
    await expect(page.locator('#running-banner')).toHaveClass(/nz-hidden/);

    expect(pageErrors, '自愈链路不应抛未捕获异常').toEqual([]);
    await ctx.close();
  });
});

// running_banner.js's updateSendButton owns the other two halves of
// that chain. 'running' arms the turn watchdog, a 15 s interval that pulls
// /api/sessions while the open session runs, because the 5 s poll is off while
// the socket is up. Any other state replaces a stale loading indicator, left
// over from a turn that ended before its events arrived, with 暂无事件.
test.describe('updateSendButton 的 watchdog 与 loading indicator', () => {
  const RUNNING = 'dashboard:direct:2026-01-01-120001-2:otherproject';
  /** @type {Awaited<ReturnType<typeof startMockServer>> | undefined} */
  let mock;
  test.beforeEach(async () => { mock = await startMockServer({ ws: true, eventsByKey: { [RUNNING]: [] } }); });
  // The file-level beforeEach skips before this describe's beforeEach runs.
  test.afterEach(() => { mock?.server.close(); mock = undefined; });

  /** @param {import('@playwright/test').Browser} browser */
  async function openRunning(browser) {
    const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
    const page = await ctx.newPage();
    /** @type {string[]} */
    const pageErrors = [];
    page.on('pageerror', (e) => pageErrors.push(String(e)));
    await page.clock.install();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await expect.poll(() => mock.wsConnections.length, { timeout: 5000 }).toBeGreaterThan(0);
    await page.click(`.session-card[data-key="${RUNNING}"]`);
    await expect(page.locator('#btn-stop')).toBeVisible();
    return { ctx, page, pageErrors };
  }

  test('running 时 watchdog 每 15s 拉一次 /api/sessions', async ({ browser }) => {
    const { ctx, page, pageErrors } = await openRunning(browser);
    try {
      // Let the load-time fetches settle, then count only what the clock drives.
      await page.clock.runFor(2000);
      const before = mock.sessionsGetCalls;
      await page.clock.runFor(16000); // one 15 s tick + the 300 ms debounce
      await expect.poll(() => mock.sessionsGetCalls, { timeout: 3000 }).toBeGreaterThan(before);
      expect(pageErrors).toEqual([]);
    } finally {
      await ctx.close();
    }
  });

  test('turn 在事件到达前结束：loading indicator 换成「暂无事件」', async ({ browser }) => {
    const { ctx, page, pageErrors } = await openRunning(browser);
    try {
      const indicator = page.locator('#events-scroll .loading-indicator');
      await expect(indicator).toHaveCount(1);
      await expect(indicator).not.toHaveText('暂无事件');
      const conn = mock.wsConnections[mock.wsConnections.length - 1];
      conn.send({ type: 'session_state', key: RUNNING, state: 'ready' });
      await expect(page.locator('#btn-send')).toBeVisible();
      await expect(indicator).toHaveText('暂无事件');
      expect(pageErrors).toEqual([]);
    } finally {
      await ctx.close();
    }
  });
});
