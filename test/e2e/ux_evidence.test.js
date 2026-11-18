// E2E: the session view under a failing turn (UX review 11-18, evidence).
// A run of the CLI's API retries reads as one row "接口重试 ×N" titled with
// the run's span, live retries counted into it; a CLI subtype the gateway
// has no words for is no row, compact_boundary reads 上下文已压缩, and a
// backend's prose notice stays. ■ shows 停止中 and takes no second click
// until the stopped turn ends; a turn still going after STOP_SLOW_MS says so
// and ■ is offered again.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');
const { openSessionCard } = require('./session_nav');

const A = 'dashboard:direct:2026-01-01-120000-1:myproject';
const B = 'dashboard:direct:2026-01-01-120001-2:otherproject'; // running
const now = Date.now();
const VIEWPORTS = [
  { name: 'desktop', viewport: { width: 1440, height: 900 } },
  { name: 'phone', viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true },
];
const SHOTS = process.env.UX_SHOTS || '';

async function open(browser, vp, key) {
  const ctx = await browser.newContext({ viewport: vp.viewport, isMobile: !!vp.isMobile, hasTouch: !!vp.hasTouch });
  const page = await ctx.newPage();
  const mock = await startMockServer({ ws: true });
  await page.goto(mock.url + '/dashboard');
  await waitForWs(page, 'CONNECTED');
  await openSessionCard(page, page.locator(`.session-card[data-key="${key}"]`));
  const conn = mock.wsConnections[mock.wsConnections.length - 1];
  await expect.poll(() => conn.messages.filter((m) => m.type === 'subscribe' && m.key === key).length).toBe(1);
  return { ctx, page, mock, conn };
}

const retry = (i, t) => ({ type: 'system', summary: 'api_retry', time: t, uuid: 'r' + i });

test('a run of API retries is one counted row', async ({ browser }) => {
  for (const vp of VIEWPORTS) {
    const { ctx, page, mock, conn } = await open(browser, vp, A);
    try {
      const t0 = now - 20 * 60000;
      const events = [{ type: 'user', detail: '继续', time: t0, uuid: 'u1' }];
      for (let i = 0; i < 10; i++) events.push(retry(i, t0 + 1000 + i * 40000));
      events.push({ type: 'text', detail: 'Request timed out', time: t0 + 420000, uuid: 't1' });
      conn.send({ type: 'subscribed', key: A, state: 'ready' });
      conn.send({ type: 'history', key: A, initial: true, events });

      const rows = page.locator('#events-scroll .event.system:visible');
      await expect(rows).toHaveCount(1);
      await expect(rows.locator('.event-content')).toHaveText('接口重试 ×10');
      await expect(rows).toHaveAttribute('title', / – /);
      await expect(page.locator('#events-scroll .event.text')).toContainText('Request timed out');
      if (SHOTS) await page.screenshot({ path: SHOTS + '/retry-' + vp.name + '.png' });

      // A live retry after the reply starts a run of its own; the next is
      // counted into it.
      conn.send({ type: 'event', key: A, event: retry(20, t0 + 430000) });
      conn.send({ type: 'event', key: A, event: retry(21, t0 + 440000) });
      await expect(rows).toHaveCount(2);
      await expect(rows.nth(1).locator('.event-content')).toHaveText('接口重试 ×2');
      await expect(rows.nth(0).locator('.event-content')).toHaveText('接口重试 ×10');
    } finally {
      await ctx.close();
      mock.server.close();
    }
  }
});

test('a bare CLI subtype is no row; compaction reads in words', async ({ browser }) => {
  for (const vp of VIEWPORTS) {
    const { ctx, page, mock, conn } = await open(browser, vp, A);
    try {
      conn.send({ type: 'subscribed', key: A, state: 'ready' });
      conn.send({ type: 'history', key: A, initial: true, events: [
        { type: 'user', detail: '你好', time: now - 60000, uuid: 'u1' },
        { type: 'system', summary: 'per_turn_effort_changed', time: now - 55000, uuid: 's1' },
        { type: 'system', summary: 'status', time: now - 54000, uuid: 's2' },
        { type: 'system', summary: 'informational', time: now - 53000, uuid: 's3' },
        { type: 'system', summary: 'compact_boundary', time: now - 52000, uuid: 's4' },
        { type: 'system', summary: '发送失败：节点不可达', time: now - 51000, uuid: 's5' },
        { type: 'text', detail: '你好！', time: now - 50000, uuid: 't1' },
      ] });
      const scroll = page.locator('#events-scroll');
      await expect(scroll.locator('.event.text')).toContainText('你好！');
      await expect(scroll.locator('.event.system .event-content')).toHaveText(['上下文已压缩', '发送失败：节点不可达']);
      for (const raw of ['per_turn_effort_changed', 'informational', 'compact_boundary']) await expect(scroll).not.toContainText(raw);
      if (SHOTS) await page.screenshot({ path: SHOTS + '/system-' + vp.name + '.png' });
    } finally {
      await ctx.close();
      mock.server.close();
    }
  }
});

test('■ shows 停止中 until the stopped turn ends', async ({ browser }) => {
  test.setTimeout(60000);
  for (const vp of VIEWPORTS) {
    const { ctx, page, mock, conn } = await open(browser, vp, B);
    try {
      conn.send({ type: 'subscribed', key: B, state: 'running' });
      conn.send({ type: 'history', key: B, initial: true, events: [
        { type: 'user', detail: '跑一下测试', time: now - 60000, uuid: 'u1' },
        { type: 'tool_use', tool: 'Bash', summary: 'go test ./...', time: now - 50000, uuid: 'b1' },
      ] });
      const stop = page.locator('#btn-stop');
      await expect(stop).toBeVisible();
      const interrupts = () => conn.messages.filter((m) => m.type === 'interrupt').length;

      await stop.click();
      await expect.poll(interrupts).toBe(1);
      for (const m of conn.messages.filter((x) => x.type === 'interrupt')) conn.send({ type: 'interrupt_ack', id: m.id, key: B, status: 'ok' });
      await expect(stop).toBeDisabled();
      await expect(stop).toHaveAttribute('aria-busy', 'true');
      await expect(stop).toHaveAttribute('title', '停止中…');
      if (SHOTS) { await page.waitForTimeout(600); await page.screenshot({ path: SHOTS + '/stop-' + vp.name + '.png' }); }
      await stop.click({ force: true });
      await page.waitForTimeout(300);
      expect(interrupts()).toBe(1);

      // Still running past STOP_SLOW_MS: told once, ■ offered again.
      await expect(page.locator('.toast', { hasText: '回合还没停下' })).toBeVisible({ timeout: 15000 });
      await expect(stop).toBeEnabled();
      await expect(stop).toHaveAttribute('title', '停止');
      await stop.click();
      await expect.poll(interrupts).toBe(2);
      await expect(stop).toBeDisabled();

      // The turn ends: ■ goes, and the next turn's ■ is a plain one.
      conn.send({ type: 'session_state', key: B, state: 'ready' });
      await expect(stop).toBeHidden();
      conn.send({ type: 'session_state', key: B, state: 'running' });
      await expect(stop).toBeVisible();
      await expect(stop).toBeEnabled();
      await expect(stop).not.toHaveAttribute('aria-busy', 'true');
    } finally {
      await ctx.close();
      mock.server.close();
    }
  }
});
