// E2E: the Planner UX review of 11-08, items 3, 4, 6, 7, 11 and 12 (v0.7.3).
//   3. One table for a state's word and colour: a sidebar row that waits on
//      you says 等你 / 停滞 beside its dot (失败 on its status line), the dot
//      8px.
//   4. "只看需要我" atop the sidebar keeps the rows 等你 / 停滞 / 失败, and
//      stays on across a reload.
//   6. × removes a session at once with 已移除 · 撤销: 撤销 brings it back
//      and nothing is deleted; left alone the DELETE goes out after 5 s. The
//      × is at least 24px with ≥3:1 contrast.
//   7. The empty Planner conversation's examples fill the input box and send
//      nothing; empty-state text is ≥4.5:1.
//   11. The desktop session view's hints are Chinese.
// (12, the sidebar's 等你 bar, is side_wait.test.js; 11's phone button is
// planner_chat.test.js.)
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const now = Date.now();
const DESKTOP = { viewport: { width: 1440, height: 900 } };
const PLANNER = 'planner:main';
const planner = { enabled: true, stopped: false, key: PLANNER, alive: true };
const WAITING = 'dashboard:direct:2026-11-08-090000-1:naozhi';
const STALLED = 'dashboard:direct:2026-11-08-090001-2:naozhi';
const FAILED = 'dashboard:direct:2026-11-08-090002-3:naozhi';
const IDLE = 'dashboard:direct:2026-11-08-090003-4:naozhi';

function row(key, prompt, health, extra) {
  return Object.assign({
    key, state: 'ready', platform: 'dashboard', agent: 'general', cli_name: 'claude', node: 'local',
    workspace: '/Users/me/workspace/naozhi', project: 'naozhi', last_prompt: prompt, last_active: now - 600000,
    created_at: now - 3600000 + key.length, health: Object.assign({ since: now - 300000 }, health),
  }, extra);
}

function sessions() {
  return {
    sessions: [
      row(WAITING, 'iso 等你演示', { state: 'waiting' }),
      row(STALLED, 'iso 停滞演示', { state: 'stalled' }),
      row(FAILED, 'iso 失败演示', { state: 'failed', error_class: 'auth' }),
      row(IDLE, 'iso 空闲演示', { state: 'idle' }),
    ],
    stats: { total: 4, running: 0, ready: 4, active: 4, uptime: '1h', backend: 'cc', max_procs: 20, default_workspace: '/Users/me/workspace', agents: ['general'], projects: [], version: 1 },
  };
}

// contrast is the WCAG contrast ratio of el's text colour (alpha and
// opacity applied) over the first opaque background up its ancestors.
function contrast(el) {
  const rgb = (c) => { const m = c.match(/[\d.]+/g).map(Number); return { r: m[0], g: m[1], b: m[2], a: m.length > 3 ? m[3] : 1 }; };
  let bg = null;
  for (let n = el; n && !bg; n = n.parentElement) {
    const c = rgb(getComputedStyle(n).backgroundColor);
    if (c.a === 1) bg = c;
  }
  bg = bg || { r: 255, g: 255, b: 255 };
  const fg = rgb(getComputedStyle(el).color);
  let a = fg.a;
  for (let n = el; n; n = n.parentElement) a *= Number(getComputedStyle(n).opacity);
  const mix = (k) => fg[k] * a + bg[k] * (1 - a);
  const lum = (c) => {
    const ch = ['r', 'g', 'b'].map((k) => { const v = c[k] / 255; return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); });
    return 0.2126 * ch[0] + 0.7152 * ch[1] + 0.0722 * ch[2];
  };
  const L1 = lum({ r: mix('r'), g: mix('g'), b: mix('b') }), L2 = lum(bg);
  return (Math.max(L1, L2) + 0.05) / (Math.min(L1, L2) + 0.05);
}

for (const scheme of ['dark', 'light']) {
  test(`sidebar (${scheme}): the state in a word, the 8px dot, and 只看需要我 across a reload`, async ({ browser }) => {
    const ctx = await browser.newContext({ ...DESKTOP, colorScheme: scheme });
    const page = await ctx.newPage();
    const mock = await startMockServer({ sessions: sessions() });
    try {
      await page.goto(mock.url + '/dashboard');
      const card = (k) => page.locator('.session-card[data-key="' + k + '"]');
      await expect(card(WAITING).locator('.sc-state')).toHaveText('等你');
      await expect(card(STALLED).locator('.sc-state')).toHaveText('停滞');
      await expect(card(FAILED).locator('.sc-state')).toHaveCount(0);
      await expect(card(FAILED).locator('.sc-status')).toContainText('失败');
      await expect(card(IDLE).locator('.sc-state')).toHaveCount(0);
      const dot = await card(WAITING).locator('.sc-dot').boundingBox();
      expect(dot.width).toBe(8);
      expect(dot.height).toBe(8);
      // The word is the dot's amber, readable.
      const [word, dotBg] = await Promise.all([
        card(WAITING).locator('.sc-state').evaluate((el) => getComputedStyle(el).color),
        card(WAITING).locator('.sc-dot').evaluate((el) => getComputedStyle(el).backgroundColor),
      ]);
      expect(word).toBe(dotBg);
      expect(await card(WAITING).locator('.sc-state').evaluate(contrast)).toBeGreaterThanOrEqual(3);

      const chip = page.locator('.sb-filter-chip[data-action="sidebar-only-need"]');
      await expect(chip).toHaveText('只看需要我 3');
      await expect(chip).toHaveAttribute('aria-pressed', 'false');
      await chip.click();
      await expect(chip).toHaveAttribute('aria-pressed', 'true');
      await expect(page.locator('#session-list .session-card')).toHaveCount(3);
      await expect(card(IDLE)).toHaveCount(0);
      await page.screenshot({ path: test.info().outputPath(`only-need-${scheme}.png`) });
      await page.reload();
      await expect(page.locator('.sb-filter-chip[data-action="sidebar-only-need"]')).toHaveAttribute('aria-pressed', 'true');
      await expect(page.locator('#session-list .session-card')).toHaveCount(3);
      await page.locator('.sb-filter-chip[data-action="sidebar-only-need"]').click();
      await expect(page.locator('#session-list .session-card')).toHaveCount(4);
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });

  test(`× (${scheme}): removed at once, 撤销 brings it back, else DELETE after 5 s`, async ({ browser }) => {
    test.setTimeout(45000);
    const ctx = await browser.newContext({ ...DESKTOP, colorScheme: scheme });
    const page = await ctx.newPage();
    const mock = await startMockServer({ sessions: sessions() });
    try {
      await page.goto(mock.url + '/dashboard');
      const card = page.locator('.session-card[data-key="' + IDLE + '"]');
      await card.hover();
      const x = card.locator('.btn-dismiss');
      const box = await x.boundingBox();
      expect(box.width).toBeGreaterThanOrEqual(24);
      expect(box.height).toBeGreaterThanOrEqual(24);
      // Once its fade-in on hover is over.
      await expect.poll(() => x.evaluate(contrast)).toBeGreaterThanOrEqual(3);
      await x.click();
      await expect(card).toHaveCount(0);
      const toast = page.locator('#undo-toast');
      await expect(toast).toBeVisible();
      await expect(toast).toContainText('已移除「iso 空闲演示」');
      await page.screenshot({ path: test.info().outputPath(`undo-toast-${scheme}.png`) });
      await toast.getByRole('button', { name: '撤销' }).click();
      await expect(toast).toBeHidden();
      await expect(card).toHaveCount(1);
      await page.waitForTimeout(5500);
      expect(mock.deleteCalls).toEqual([]);
      await expect(card).toHaveCount(1);

      // Left alone, it goes.
      await card.hover();
      await card.locator('.btn-dismiss').click();
      await expect(card).toHaveCount(0);
      await page.waitForTimeout(1000);
      expect(mock.deleteCalls).toEqual([]);
      await expect.poll(() => mock.deleteCalls, { timeout: 8000 }).toEqual([{ key: IDLE }]);
      await expect(toast).toBeHidden();
      await expect(card).toHaveCount(0);

      // A second removal sends the first one's DELETE at once.
      for (const k of [WAITING, STALLED]) {
        const c = page.locator('.session-card[data-key="' + k + '"]');
        await c.hover();
        await c.locator('.btn-dismiss').click();
        await expect(c).toHaveCount(0);
      }
      await expect.poll(() => mock.deleteCalls.map((b) => b.key)).toEqual([IDLE, WAITING]);
      await expect(toast).toContainText('iso 停滞演示');
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });

  test(`empty Planner conversation (${scheme}): its examples fill the input, readable`, async ({ browser }) => {
    const ctx = await browser.newContext({ ...DESKTOP, colorScheme: scheme });
    const page = await ctx.newPage();
    const mock = await startMockServer({ ws: true, sessions: sessions(), plannerEvents: [], planner,
      inbox: { open: [], closed: [], counts: { open: 0, badge: 0 } }, tasks: { tasks: [] } });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');
      await page.click('#abnav-planner');
      const empty = page.locator('#pl-stream > .pl-empty');
      await expect(empty).toBeVisible();
      expect(await empty.evaluate(contrast)).toBeGreaterThanOrEqual(4.5);
      expect(await page.locator('#pl-wait > .pl-empty').evaluate(contrast)).toBeGreaterThanOrEqual(4.5);
      const ex = empty.locator('.pl-example');
      await expect(ex).toHaveText(['今天要做…帮我派出去', '现在都在干什么？', '明早 9 点提醒我…']);
      await page.screenshot({ path: test.info().outputPath(`planner-empty-${scheme}.png`) });
      await ex.nth(0).click();
      const input = page.locator('#pl-input');
      await expect(input).toHaveValue('今天要做帮我派出去');
      await expect(input).toBeFocused();
      // The caret is where the "…" was.
      expect(await input.evaluate((el) => el.selectionStart)).toBe(4);
      await ex.nth(1).click();
      await expect(input).toHaveValue('现在都在干什么？');
      expect(mock.plannerSendCalls).toEqual([]);
      await expect(empty).toBeVisible();
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
}

test('desktop session view: the input hints are Chinese', async ({ browser }) => {
  const ctx = await browser.newContext(DESKTOP);
  const page = await ctx.newPage();
  const mock = await startMockServer({ sessions: sessions() });
  try {
    await page.goto(mock.url + '/dashboard');
    await page.click('.session-card[data-key="' + IDLE + '"]');
    await expect(page.locator('.input-hints')).toHaveText('Enter 发送 · Shift+Enter 换行 · Esc 中断');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});
