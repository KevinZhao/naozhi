// E2E: a session the server has just made opens before the sidebar refresh
// answers. Resuming from the history popover and saving a 追问 as a session
// select the new key with its card and header title at once; a poll sent
// before the server made it does not take the card away; the refresh's row
// then replaces the stand-in. The 追问 drawer opens on the quote while
// scratch_open is out (a send made meanwhile goes once the key is known),
// and closes with the error if it fails, keeping what was typed. A card
// removed while the refresh is out stays removed, and the refresh's row of a
// session left meanwhile is not an unread turn.
//
// 跑法：cd test/e2e && npx playwright test optimistic_resume.test.js --project=desktop-chrome
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const desktop = { viewport: { width: 1280, height: 800 } };
const SESSION = 'dashboard:direct:2026-01-01-120000-1:myproject';
// The mock's POST /api/sessions/resume answers this key.
const RESUMED = 'dashboard:direct:resumed:myproject';
// The mock's POST /api/scratch/open answers this key and id.
const SCRATCH = 'dashboard:scratch:2026-01-01-130000-9:myproject';
const PROMOTED = 'dashboard:direct:aside-0123456789abcdef:general';

const isSessionsGet = (req) => req.method() === 'GET' && new URL(req.url()).pathname === '/api/sessions';

// holdSessionsAfter holds the first GET /api/sessions made once armed() is
// true until release(); while it is held, any other GET is answered with
// the list as it stood (stale: it lacks the new key). The held one, once
// released, is answered with extra(row) added to the mock's list.
async function holdSessionsAfter(page, armed, extra) {
  const h = { held: null, stale: 0, release: null, done: false };
  let gate = null;
  await page.route((u) => u.pathname === '/api/sessions', async (route) => {
    if (!isSessionsGet(route.request()) || !armed()) return route.continue();
    if (!h.held) {
      h.held = route;
      await new Promise((r) => { gate = r; });
      const resp = await route.fetch();
      const body = await resp.json();
      if (extra) body.sessions = (body.sessions || []).filter((s) => s.key !== extra.key).concat([extra]);
      if (body.stats) body.stats = Object.assign({}, body.stats, { version: (body.stats.version || 1) + 100 });
      await route.fulfill({ response: resp, json: body });
      h.done = true;
      return;
    }
    h.stale++;
    const resp = await route.fetch();
    const body = await resp.json();
    if (extra) body.sessions = (body.sessions || []).filter((s) => s.key !== extra.key);
    return route.fulfill({ response: resp, json: body });
  });
  h.release = () => gate && gate();
  return h;
}

test('a resumed history session opens with its card before the sidebar refresh answers', async ({ browser }) => {
  const ctx = await browser.newContext(desktop);
  const page = await ctx.newPage();
  const mock = await startMockServer({});
  try {
    let resumed = false;
    page.on('response', (r) => { if (r.url().includes('/api/sessions/resume')) resumed = true; });
    const server = {
      key: RESUMED, state: 'ready', platform: 'dashboard', agent: 'myproject', cli_name: 'claude',
      workspace: '/home/user/workspace/myproject', project: 'myproject', node: 'local',
      session_id: 'hist-001', last_prompt: 'old task from yesterday', user_label: '服务端的标题',
      last_active: Date.now(), created_at: Date.now(),
    };
    const hold = await holdSessionsAfter(page, () => resumed, server);
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click('#btn-history');
    await page.click('.history-popover-item[data-sid="hist-001"]');
    await expect.poll(() => !!hold.held).toBe(true);

    // The refresh is still out: the session is open, its card is in the
    // sidebar and the header names it from the history entry.
    const card = page.locator(`.session-card[data-key="${RESUMED}"]`);
    await expect(card).toHaveCount(1);
    await expect(card).toHaveClass(/active/);
    await expect(page.locator('#main .hdr-title-t')).toHaveText('old task from yesterday');
    expect(await page.evaluate(() => window.nz.test.selectedKey)).toBe(RESUMED);
    await expect(page.locator('#main #msg-input')).toBeVisible();

    // A poll that answers with the list from before the resume does not take
    // the card away.
    await page.evaluate(() => window.nz.test.fetchSessions());
    expect(hold.stale).toBeGreaterThan(0);
    await expect(card).toHaveCount(1);
    await expect(card).toHaveClass(/active/);

    // The refresh's row replaces the stand-in, header included.
    hold.release();
    await expect(page.locator('#main .hdr-title-t')).toHaveText('服务端的标题');
    await expect(card).toHaveCount(1);
    await expect(card).toContainText('服务端的标题');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

// resumeHeld resumes hist-001 and returns once the sidebar refresh it starts
// is held; server is the row that refresh answers with.
async function resumeHeld(page, mock, server) {
  let resumed = false;
  page.on('response', (r) => { if (r.url().includes('/api/sessions/resume')) resumed = true; });
  const hold = await holdSessionsAfter(page, () => resumed, server);
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.click('#btn-history');
  await page.click('.history-popover-item[data-sid="hist-001"]');
  await expect.poll(() => !!hold.held).toBe(true);
  await expect(page.locator(`.session-card[data-key="${RESUMED}"]`)).toHaveCount(1);
  return hold;
}

const resumedRow = (extra) => Object.assign({
  key: RESUMED, state: 'ready', platform: 'dashboard', agent: 'myproject', cli_name: 'claude',
  workspace: '/home/user/workspace/myproject', project: 'myproject', node: 'local',
  session_id: 'hist-001', last_prompt: 'old task from yesterday', user_label: '服务端的标题',
  last_active: Date.now(), created_at: Date.now(),
}, extra);

test('a resumed card removed while the refresh is out stays removed when it answers', async ({ browser }) => {
  const ctx = await browser.newContext(desktop);
  const page = await ctx.newPage();
  const mock = await startMockServer({});
  try {
    const hold = await resumeHeld(page, mock, resumedRow());
    const card = page.locator(`.session-card[data-key="${RESUMED}"]`);
    await card.locator('.btn-dismiss').click();
    await expect(card).toHaveCount(0);

    // The refresh lists the session (its DELETE waits out the undo window):
    // the stand-in does not put the card back.
    hold.release();
    await expect.poll(() => hold.done).toBe(true);
    // Counted once, not retried: the DELETE after the undo window would
    // take a resurrected card away again.
    await page.waitForTimeout(300);
    expect(await card.count()).toBe(0);
    await expect(page.locator('#undo-toast')).toContainText('已移除');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('the refresh row of a resumed session left meanwhile does not count as unread', async ({ browser }) => {
  const ctx = await browser.newContext(desktop);
  const page = await ctx.newPage();
  const mock = await startMockServer({});
  try {
    const hold = await resumeHeld(page, mock, resumedRow({ last_response: '昨天的回答' }));
    await page.click(`.session-card[data-key="${SESSION}"]`);
    expect(await page.evaluate(() => window.nz.test.selectedKey)).toBe(SESSION);
    hold.release();
    const card = page.locator(`.session-card[data-key="${RESUMED}"]`);
    await expect(card).toContainText('服务端的标题');
    await page.waitForTimeout(300);
    expect(await card.locator('.sc-unread').count()).toBe(0);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('a resume the server refuses changes nothing and says why', async ({ browser }) => {
  const ctx = await browser.newContext(desktop);
  const page = await ctx.newPage();
  const mock = await startMockServer({});
  try {
    await page.route((u) => u.pathname === '/api/sessions/resume', (route) =>
      route.fulfill({ status: 500, contentType: 'text/plain', body: 'boom' }));
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click(`.session-card[data-key="${SESSION}"]`);
    await page.click('#btn-history');
    await page.click('.history-popover-item[data-sid="hist-001"]');
    await expect(page.locator('#toast')).toHaveClass(/show/);
    await expect(page.locator('#toast')).toContainText('恢复会话');
    await expect(page.locator(`.session-card[data-key="${RESUMED}"]`)).toHaveCount(0);
    expect(await page.evaluate(() => window.nz.test.selectedKey)).toBe(SESSION);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

function sessionEvents() {
  return { [SESSION]: [
    { type: 'user', detail: '解释一下', time: Date.now() - 60000, uuid: 'or-u1' },
    // Over 500 characters, the length that gets 追问.
    { type: 'text', detail: '这是回答。'.repeat(120), time: Date.now() - 50000, uuid: 'or-t1' },
  ] };
}

async function clickAskAside(page) {
  const reply = page.locator('#events-scroll .event.text').last();
  await reply.hover();
  await reply.locator('[data-action="ask-aside"]').click();
}

test('a 追问 saved as a session opens at once, before the sidebar refresh answers', async ({ browser }) => {
  const ctx = await browser.newContext(desktop);
  const page = await ctx.newPage();
  const keyEvents = Object.assign(sessionEvents(), { [SCRATCH]: [] });
  const mock = await startMockServer({ keyEvents });
  try {
    let promoted = false;
    page.on('response', (r) => { if (/\/api\/scratch\/[^/]+\/promote$/.test(new URL(r.url()).pathname)) promoted = true; });
    const server = {
      key: PROMOTED, state: 'ready', platform: 'dashboard', agent: 'general', cli_name: 'claude',
      workspace: '/home/user/workspace/myproject', project: 'myproject', node: 'local',
      last_prompt: 'iso 再说说', user_label: '保存后的标题', last_active: Date.now(), created_at: Date.now(),
    };
    const hold = await holdSessionsAfter(page, () => promoted, server);
    await page.goto(mock.url + '/dashboard');
    await page.click(`.session-card[data-key="${SESSION}"]`);
    await clickAskAside(page);
    await expect(page.locator('#ad-input')).toBeVisible();
    await page.fill('#ad-input', 'iso 再说说');
    await page.press('#ad-input', 'Enter');
    await expect.poll(() => mock.sendCalls.length).toBe(1);
    keyEvents[SCRATCH].push(
      { type: 'user', detail: 'iso 再说说', time: Date.now(), uuid: 'or-su1' },
      { type: 'text', detail: 'iso 追问的回答', time: Date.now() + 1, uuid: 'or-st1' },
    );
    await expect(page.locator('#ad-save')).toBeVisible({ timeout: 10000 });
    await page.click('#ad-save');
    await expect.poll(() => !!hold.held).toBe(true);

    // The refresh is still out: the drawer is gone and the new session is
    // open with its card, titled by what was last asked.
    await expect(page.locator('#aside-drawer')).not.toHaveClass(/visible/);
    const card = page.locator(`.session-card[data-key="${PROMOTED}"]`);
    await expect(card).toHaveCount(1);
    await expect(card).toHaveClass(/active/);
    expect(await page.evaluate(() => window.nz.test.selectedKey)).toBe(PROMOTED);
    await expect(page.locator('#main .hdr-title-t')).toHaveText('iso 再说说');

    await page.evaluate(() => window.nz.test.fetchSessions());
    expect(hold.stale).toBeGreaterThan(0);
    await expect(card).toHaveCount(1);

    hold.release();
    await expect(page.locator('#main .hdr-title-t')).toHaveText('保存后的标题');
    await expect(card).toHaveCount(1);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('the 追问 drawer opens on the quote while scratch_open is out', async ({ browser }) => {
  const ctx = await browser.newContext(desktop);
  const page = await ctx.newPage();
  const mock = await startMockServer({ keyEvents: Object.assign(sessionEvents(), { [SCRATCH]: [] }) });
  try {
    let release = null;
    await page.route((u) => u.pathname === '/api/scratch/open', async (route) => {
      await new Promise((r) => { release = r; });
      await route.continue();
    });
    const scratchPolls = [];
    page.on('request', (r) => {
      const u = new URL(r.url());
      if (u.pathname === '/api/sessions/events' && u.searchParams.get('key') === SCRATCH) scratchPolls.push(r);
    });
    await page.goto(mock.url + '/dashboard');
    await page.click(`.session-card[data-key="${SESSION}"]`);
    await clickAskAside(page);
    await expect.poll(() => release !== null).toBe(true);

    // Open on the quote, saying it is opening; a send waits for the key.
    await expect(page.locator('#aside-drawer')).toHaveClass(/visible/);
    await expect(page.locator('#ad-quote-preview')).toContainText('这是回答。');
    await expect(page.locator('#ad-loading')).toHaveClass(/visible/);
    await expect(page.locator('#ad-loading')).toHaveText('正在打开…');
    await page.fill('#ad-input', 'iso 先打的字');
    await page.press('#ad-input', 'Enter');
    await page.waitForTimeout(200);
    expect(mock.sendCalls.length).toBe(0);
    await expect(page.locator('#ad-input')).toHaveValue('iso 先打的字');
    expect(scratchPolls.length).toBe(0);

    // The key comes: polling starts and what was sent meanwhile goes, once.
    release();
    await expect.poll(() => mock.sendCalls.length).toBe(1);
    expect(JSON.parse(mock.sendCalls[0])).toMatchObject({ key: SCRATCH, text: 'iso 先打的字' });
    await expect(page.locator('#ad-messages .event.user')).toHaveText(/iso 先打的字/);
    await expect(page.locator('#ad-input')).toHaveValue('');
    await expect(page.locator('#ad-loading')).toHaveText('正在思考…');
    await expect.poll(() => scratchPolls.length, { timeout: 10000 }).toBeGreaterThan(0);
    await page.waitForTimeout(300);
    expect(mock.sendCalls.length).toBe(1);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('a scratch_open that fails closes the drawer and says why', async ({ browser }) => {
  const ctx = await browser.newContext(desktop);
  const page = await ctx.newPage();
  const mock = await startMockServer({ keyEvents: sessionEvents() });
  try {
    let release = null;
    await page.route((u) => u.pathname === '/api/scratch/open', async (route) => {
      await new Promise((r) => { release = r; });
      await route.fulfill({ status: 500, contentType: 'text/plain', body: 'boom' });
    });
    await page.goto(mock.url + '/dashboard');
    await page.click(`.session-card[data-key="${SESSION}"]`);
    await clickAskAside(page);
    await expect.poll(() => release !== null).toBe(true);
    await expect(page.locator('#aside-drawer')).toHaveClass(/visible/);
    // A question typed and sent while it opens is not thrown away.
    await page.fill('#ad-input', 'iso 打开时打的问题');
    await page.press('#ad-input', 'Enter');
    release();
    await expect(page.locator('#aside-drawer')).not.toHaveClass(/visible/);
    await expect(page.locator('#toast')).toHaveClass(/show/);
    await expect(page.locator('#toast')).toContainText('打开追问');
    await expect(page.locator('#ad-loading')).not.toHaveClass(/visible/);
    expect(await page.evaluate(() => window.nz.test.selectedKey)).toBe(SESSION);
    expect(mock.sendCalls.length).toBe(0);

    // The next 追问 opens on it.
    release = null;
    await clickAskAside(page);
    await expect.poll(() => release !== null).toBe(true);
    await expect(page.locator('#aside-drawer')).toHaveClass(/visible/);
    await expect(page.locator('#ad-input')).toHaveValue('iso 打开时打的问题');
    release();
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('a drawer closed while scratch_open is out deletes the scratch that answers', async ({ browser }) => {
  const ctx = await browser.newContext(desktop);
  const page = await ctx.newPage();
  const mock = await startMockServer({ keyEvents: Object.assign(sessionEvents(), { [SCRATCH]: [] }) });
  try {
    let release = null;
    await page.route((u) => u.pathname === '/api/scratch/open', async (route) => {
      await new Promise((r) => { release = r; });
      await route.continue();
    });
    const deletes = [];
    page.on('request', (r) => {
      if (r.method() === 'DELETE' && new URL(r.url()).pathname.startsWith('/api/scratch/')) deletes.push(new URL(r.url()).pathname);
    });
    await page.goto(mock.url + '/dashboard');
    await page.click(`.session-card[data-key="${SESSION}"]`);
    await clickAskAside(page);
    await expect.poll(() => release !== null).toBe(true);
    await page.click('#ad-close');
    await expect(page.locator('#aside-drawer')).not.toHaveClass(/visible/);
    expect(deletes).toEqual([]);
    release();
    await expect.poll(() => deletes).toEqual(['/api/scratch/scr-0001']);
    // The drawer stays shut: the answer was for the one closed.
    await page.waitForTimeout(200);
    await expect(page.locator('#aside-drawer')).not.toHaveClass(/visible/);
    expect(mock.sendCalls.length).toBe(0);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});
