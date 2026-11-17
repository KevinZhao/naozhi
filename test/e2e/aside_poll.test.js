// E2E: the 追问 drawer's poll (aside_drawer.js). The bubble it draws of what
// you sent carries the browser's clock and leaves the watermark alone, so a
// reply the server stamped before it still comes; a send while a poll is out
// leaves one poll running, not two; sends merged into one turn come back as
// the bubbles already drawn; and a poll answered after its drawer closed
// draws nothing into the next one.
//
// 跑法：cd test/e2e && npx playwright test aside_poll.test.js --project=desktop-chrome
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const SESSION = 'dashboard:direct:2026-01-01-120000-1:myproject';
// The mock's POST /api/scratch/open answers this key.
const SCRATCH = 'dashboard:scratch:2026-01-01-130000-9:myproject';
const desktop = { viewport: { width: 1280, height: 800 } };

function sessionEvents() {
  return { [SESSION]: [
    { type: 'user', detail: '解释一下', time: Date.now() - 60000, uuid: 'ap-u1' },
    // Over 500 characters, the length that gets 追问.
    { type: 'text', detail: '这是回答。'.repeat(120), time: Date.now() - 50000, uuid: 'ap-t1' },
  ] };
}

async function openAside(page) {
  const reply = page.locator('#events-scroll .event.text').last();
  await reply.hover();
  await reply.locator('[data-action="ask-aside"]').click();
  await expect(page.locator('#ad-input')).toBeVisible();
}

const scratchGet = (u) => u.pathname === '/api/sessions/events' && u.searchParams.get('key');

test('a reply the server stamped before the bubble of what was sent still shows', async ({ browser }) => {
  const ctx = await browser.newContext(desktop);
  const page = await ctx.newPage();
  const keyEvents = Object.assign(sessionEvents(), { [SCRATCH]: [] });
  const mock = await startMockServer({ keyEvents });
  try {
    await page.goto(mock.url + '/dashboard');
    await page.click('.session-card[data-key="' + SESSION + '"]');
    await openAside(page);
    await page.fill('#ad-input', 'iso 再说说');
    await page.press('#ad-input', 'Enter');
    await expect.poll(() => mock.sendCalls.length).toBe(1);
    // The server's clock is 3s behind the browser's: its echo of the message
    // and the reply are stamped before the bubble the drawer drew.
    const t = Date.now() - 3000;
    keyEvents[SCRATCH].push(
      { type: 'user', detail: 'iso 再说说', time: t, uuid: 'sx-u1' },
      { type: 'text', detail: 'iso 早到的回答', time: t + 100, uuid: 'sx-t1' },
    );
    const msgs = page.locator('#ad-messages');
    await expect(msgs.locator('.event.text')).toContainText('iso 早到的回答', { timeout: 10000 });
    // The echo is the bubble already drawn, not a second one.
    await expect(msgs.locator('.event.user')).toHaveCount(1);
    await expect(page.locator('#ad-loading')).not.toHaveClass(/visible/);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('two sends merged into one turn come back as the bubbles already drawn', async ({ browser }) => {
  const ctx = await browser.newContext(desktop);
  const page = await ctx.newPage();
  const keyEvents = Object.assign(sessionEvents(), { [SCRATCH]: [] });
  const mock = await startMockServer({ keyEvents });
  try {
    await page.goto(mock.url + '/dashboard');
    await page.click('.session-card[data-key="' + SESSION + '"]');
    await openAside(page);
    for (const text of ['iso 甲', 'iso 乙']) {
      await page.fill('#ad-input', text);
      await page.press('#ad-input', 'Enter');
      await expect(page.locator('#ad-input')).toHaveValue('');
    }
    await expect.poll(() => mock.sendCalls.length).toBe(2);
    // A turn was running: the server merged both into one user event
    // (turn.Coalesce).
    const t = Date.now();
    keyEvents[SCRATCH].push(
      { type: 'user', detail: '[以下是用户在你处理上一条消息期间追加发送的内容]\n\n[12:00] iso 甲\n\n[12:00] iso 乙\n', time: t, uuid: 'sx-c1' },
      { type: 'text', detail: 'iso 合并后的回答', time: t + 100, uuid: 'sx-c2' },
    );
    const msgs = page.locator('#ad-messages');
    await expect(msgs.locator('.event.text')).toContainText('iso 合并后的回答', { timeout: 10000 });
    await expect(msgs.locator('.event.user')).toHaveCount(2);
    await expect(msgs).not.toContainText('追加发送的内容');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('a send while a poll is out leaves one poll running', async ({ browser }) => {
  test.setTimeout(60000);
  const ctx = await browser.newContext(desktop);
  const page = await ctx.newPage();
  const mock = await startMockServer({ keyEvents: Object.assign(sessionEvents(), { [SCRATCH]: [] }) });
  try {
    const polls = [];
    let release = null;
    let held = 0;
    await page.route((u) => scratchGet(u) === SCRATCH, async (route) => {
      polls.push(Date.now());
      // The first poll stays out until the send went through.
      if (held++ === 0) await new Promise((r) => { release = r; });
      await route.continue();
    });
    await page.goto(mock.url + '/dashboard');
    await page.click('.session-card[data-key="' + SESSION + '"]');
    await openAside(page);
    await expect.poll(() => release !== null).toBe(true);
    await page.fill('#ad-input', 'iso 再说说');
    await page.press('#ad-input', 'Enter');
    await expect.poll(() => mock.sendCalls.length).toBe(1);
    await page.waitForTimeout(100);
    release();
    // One poll a second: two chains would make about twice as many.
    const from = Date.now();
    await page.waitForTimeout(6000);
    const n = polls.filter((t) => t >= from).length;
    expect(n, 'polls in 6s').toBeLessThanOrEqual(8);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('a poll answered after its drawer closed draws nothing into the next one', async ({ browser }) => {
  test.setTimeout(60000);
  const ctx = await browser.newContext(desktop);
  const page = await ctx.newPage();
  const mock = await startMockServer({ keyEvents: sessionEvents() });
  const OLD = 'dashboard:scratch:2026-01-01-130000-1:myproject';
  const NEW = 'dashboard:scratch:2026-01-01-130000-2:myproject';
  try {
    let opened = 0;
    await page.route('**/api/scratch/open', (route) => {
      opened++;
      route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({
        scratch_id: 'scr-' + opened, key: opened === 1 ? OLD : NEW, agent_id: 'general', context_turns: 0, context_truncated: false,
      }) });
    });
    let release = null;
    await page.route((u) => scratchGet(u) === OLD, async (route) => {
      await new Promise((r) => { release = r; });
      route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify([
        { type: 'text', detail: 'iso 上一个追问的回答', time: Date.now(), uuid: 'old-t1' },
      ]) });
    });
    await page.route((u) => scratchGet(u) === NEW, (route) => {
      route.fulfill({ status: 200, contentType: 'application/json', body: '[]' });
    });
    await page.goto(mock.url + '/dashboard');
    await page.click('.session-card[data-key="' + SESSION + '"]');
    await openAside(page);
    // The old drawer's poll is out; the drawer closes and another opens.
    await expect.poll(() => release !== null).toBe(true);
    await page.click('#ad-close');
    await expect(page.locator('#aside-drawer')).not.toHaveClass(/visible/);
    await openAside(page);
    await expect.poll(() => opened).toBe(2);
    release();
    await page.waitForTimeout(2500);
    await expect(page.locator('#ad-messages')).not.toContainText('iso 上一个追问的回答');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});
