// E2E: the states the dashboard was silent or wrong about (UX review
// "states"): the page's own socket down shows one pill until it is back;
// without a socket the 等你 badge is still read and kept fresh; a neutral
// toast is not error red and clears the send button; the closed 追问 drawer
// and the header's switches, the history popover's results by keyboard;
// Chinese wording; counts readable; a count or list that could not be read
// is not said as 0 or "none"; one word for a failure, said once.
const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');
const { waitForWs, waitForShim } = require('./shim_wait');

const DESKTOP = { viewport: { width: 1440, height: 900 } };
const PHONE = { viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true };
const SESSION = 'dashboard:direct:2026-01-01-120000-1:myproject';
const PLANNER = 'planner:main';
const now = Date.now();
const iso = (ms) => new Date(ms).toISOString();
const planner = { enabled: true, stopped: false, key: PLANNER, alive: true };

const item = (id, kind, title, extra) => Object.assign({ id, kind, level: 'timely', title, why: '', created: iso(now - 60000), state: 'open', source: 's:' + id }, extra);
function inbox() {
  return {
    open: [
      item('in_err', 'error', 'nightly 失败', { unit: 'cron:nightly', why: '定时任务运行失败' }),
      item('in_q', 'question', '会话在等你回复', { unit: SESSION }),
      item('in_d', 'decision', '周报用哪个模板？', { decision: { handler: 'ask', actions: ['A', 'B'] } }),
    ],
    closed: [],
    counts: { open: 3, badge: 3 },
  };
}
const tasks = () => ({ tasks: [
  { id: 'task:aaaaaaaaaaaa', kind: 'session', title: '修登录页', state: 'doing', at: iso(now - 60000), created: iso(now - 60000) },
  { id: 'task:bbbbbbbbbbbb', kind: 'session', title: '写周报', state: 'doing', at: iso(now - 60000), created: iso(now - 60000) },
] });
const shot = (page, name) => page.screenshot({ path: test.info().outputPath(name + '.png') });

test('the page\'s own socket down shows one pill, gone once it is back', async ({ browser }) => {
  test.setTimeout(90000);
  const ctx = await browser.newContext(DESKTOP);
  const page = await ctx.newPage();
  const mock = await startMockServer({ ws: true });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    const pill = page.locator('#conn-status');
    await expect(pill).toBeHidden();
    await ctx.setOffline(true);
    for (const c of mock.wsConnections) c.close();
    // Past 5s down it says so, on whichever view is open.
    await expect(pill).toBeVisible({ timeout: 12000 });
    await expect(pill).toContainText(/已断开 \d+ 秒/);
    await page.click('#abnav-planner');
    await expect(pill).toBeVisible();
    await shot(page, 'conn-down-desktop');
    await ctx.setOffline(false);
    // Once the backoff passed 8s it offers 重连; either way it comes back.
    const again = pill.locator('[data-action="ws-reconnect"]');
    if (await again.isVisible()) await again.click();
    await waitForWs(page, 'CONNECTED', { timeout: 40000 });
    await expect(pill).toBeHidden();
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('the pill sits over the tab bar on a phone', async ({ browser }) => {
  const ctx = await browser.newContext(PHONE);
  const page = await ctx.newPage();
  // No upgrade listener: the page stays on the HTTP fallback.
  const mock = await startMockServer({});
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForShim(page);
    const pill = page.locator('#conn-status');
    await expect(pill).toBeVisible({ timeout: 12000 });
    const p = await pill.boundingBox();
    const bar = await page.locator('#activity-bar').boundingBox();
    expect(p.y + p.height).toBeLessThanOrEqual(bar.y);
    await shot(page, 'conn-down-phone');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('without a socket the 等你 badge is still read and kept fresh', async ({ browser }) => {
  test.setTimeout(60000);
  const ctx = await browser.newContext(DESKTOP);
  const page = await ctx.newPage();
  const box = inbox();
  const mock = await startMockServer({ inbox: box, tasks: tasks(), planner });
  try {
    await page.goto(mock.url + '/dashboard');
    const badge = page.locator('#abnav-planner-count');
    await expect(badge).toHaveText('3', { timeout: 8000 });
    await expect(page.locator('#sw-toggle')).toBeVisible();
    box.open.push(item('in_q2', 'question', '另一个会话在问你', { unit: SESSION }));
    box.counts = { open: 4, badge: 4 };
    await expect(badge).toHaveText('4', { timeout: 20000 });
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('a toast is neutral unless typed, and clears the send button', async ({ browser }) => {
  const ctx = await browser.newContext(DESKTOP);
  const page = await ctx.newPage();
  const mock = await startMockServer({});
  try {
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click(`.session-card[data-key="${SESSION}"]`);
    await expect(page.locator('.btn-send')).toBeVisible();
    const toastBg = (msg, type) => page.evaluate(async ([m, t]) => {
      (await import('/static/nz_util.js')).showToast(m, t);
      return getComputedStyle(document.getElementById('toast')).backgroundColor;
    }, [msg, type]);
    const red = await page.evaluate(() => {
      const el = document.createElement('div');
      el.style.color = 'var(--nz-red)';
      document.body.appendChild(el);
      const c = getComputedStyle(el).color;
      el.remove();
      return c;
    });
    for (const type of [undefined, 'info']) expect(await toastBg('已重命名', type)).not.toBe(red);
    expect(await toastBg('提示', 'warn')).toBe(await toastBg('提示', 'warning'));
    expect(await toastBg('已重命名')).not.toBe(await toastBg('发送消息失败', 'error'));
    await toastBg('已重命名');
    await expect(page.locator('#toast')).toHaveClass(/show/);
    const t = await page.locator('#toast').boundingBox();
    const s = await page.locator('.btn-send').boundingBox();
    expect(t.y + t.height <= s.y || t.y >= s.y + s.height).toBe(true);
    await shot(page, 'toast-neutral-desktop');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('the closed 追问 drawer is out of the Tab order', async ({ browser }) => {
  const ctx = await browser.newContext(DESKTOP);
  const page = await ctx.newPage();
  const mock = await startMockServer({});
  try {
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await expect(page.locator('#aside-drawer')).toBeHidden();
    await page.locator('body').focus();
    for (let i = 0; i < 60; i++) {
      await page.keyboard.press('Tab');
      expect(await page.evaluate(() => !!document.activeElement.closest('#aside-drawer'))).toBe(false);
    }
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('the history popover works by keyboard alone', async ({ browser }) => {
  const ctx = await browser.newContext(DESKTOP);
  const page = await ctx.newPage();
  const mock = await startMockServer({});
  try {
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.locator('#btn-history').focus();
    await page.keyboard.press('Enter');
    const search = page.locator('#hp-search');
    await expect(search).toBeFocused();
    await page.keyboard.type('old');
    const hit = page.locator('.history-popover-item[data-sid="hist-001"]');
    await expect(hit).toHaveCount(1);
    // ↓ reaches the result; Tab stays in the popover covering the page.
    await page.keyboard.press('ArrowDown');
    await expect(hit).toBeFocused();
    await page.keyboard.press('Tab');
    expect(await page.evaluate(() => !!document.activeElement.closest('.history-popover'))).toBe(true);
    await search.focus();
    await shot(page, 'history-keyboard-desktop');
    // Enter in the search box opens the first match.
    const resumed = page.waitForRequest((r) => r.method() === 'POST' && r.url().includes('/resume'));
    await page.keyboard.press('Enter');
    await resumed;
    await expect(page.locator('.history-popover')).toHaveCount(0);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('the header\'s model and access-profile switches are buttons the keyboard reaches', async ({ browser }) => {
  const ctx = await browser.newContext(DESKTOP);
  const page = await ctx.newPage();
  const mock = await startMockServer({});
  try {
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click(`.session-card[data-key="${SESSION}"]`);
    const model = page.locator('#header-model');
    await expect(model).toBeVisible();
    expect(await model.evaluate((el) => el.tagName)).toBe('BUTTON');
    await model.focus();
    await expect(model).toBeFocused();
    await page.keyboard.press('Enter');
    await expect(page.locator('#tuning-popover')).toBeVisible();
    // 选模型 in the label's tone, not the decorative faint one.
    const unset = await page.evaluate(() => {
      const probe = (cls) => {
        const el = document.createElement('button');
        el.className = cls;
        document.querySelector('.main-header').appendChild(el);
        const c = getComputedStyle(el).color;
        el.remove();
        return c;
      };
      return [probe('model-label model-label-unset'), probe('model-label')];
    });
    expect(unset[0]).toBe(unset[1]);
    await shot(page, 'header-switches-desktop');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('wording: Chinese, no raw browser error', async ({ browser }) => {
  const ctx = await browser.newContext(DESKTOP);
  const page = await ctx.newPage();
  const mock = await startMockServer({});
  try {
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await expect(page.locator('#btn-new-session')).toHaveAttribute('title', '新建会话');
    await expect(page.locator('.nz-empty-hint, .nz-hint-dim').first()).toContainText('默认目录 · 通用助手');
    const text = await page.evaluate(async () => {
      (await import('/static/utilities.js')).showNetworkError('发送消息', new TypeError('Failed to fetch'));
      return document.getElementById('toast').textContent;
    });
    expect(text).toBe('发送消息失败：网络错误');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('the left column\'s counts read in their label\'s colour', async ({ browser }) => {
  const ctx = await browser.newContext(DESKTOP);
  const page = await ctx.newPage();
  const mock = await startMockServer({ ws: true, inbox: inbox(), tasks: tasks(), planner, plannerEvents: [] });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click('#abnav-planner');
    const n = page.locator('#pl-wait-h .tk-n');
    await expect(n).toHaveText('3');
    const [count, label] = await page.evaluate(() => [
      getComputedStyle(document.querySelector('#pl-wait-h .tk-n')).color,
      getComputedStyle(document.querySelector('#pl-wait-h')).color,
    ]);
    expect(count).toBe(label);
    const opacity = await page.evaluate(() => {
      const el = document.createElement('div');
      el.className = 'event-time-divider';
      document.getElementById('pl-stream').appendChild(el);
      const o = getComputedStyle(el).opacity;
      el.remove();
      return o;
    });
    expect(opacity).toBe('1');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('offline after load: the phone block and the stream do not say 0 or nothing', async ({ browser }) => {
  const ctx = await browser.newContext(PHONE);
  const page = await ctx.newPage();
  const mock = await startMockServer({ ws: true, inbox: inbox(), tasks: tasks(), planner, plannerEvents: [{ type: 'text', detail: '早上好', time: now - 50000, uuid: 'p-1' }] });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await expect(page.locator('#abnav-planner-count')).toHaveText('3');
    await ctx.setOffline(true);
    await page.click('#abnav-planner');
    await expect(page.locator('#pl-stream')).toContainText('加载失败，连上后自动刷新');
    const toggle = page.locator('#pl-left-toggle span');
    await expect(toggle).toHaveText('等你 3 · 任务');
    await shot(page, 'offline-planner-phone');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

function remoteSessions(status) {
  const s = defaultSessions();
  s.nodes = {
    local: { display_name: 'EC2', status: 'ok' },
    pc: { display_name: 'MBP', status, caps: ['files', 'planner', 'subscribe-history'] },
  };
  return s;
}

test('a node offline: 等你 is not "none", the state is said once', async ({ browser }) => {
  const ctx = await browser.newContext(DESKTOP);
  const page = await ctx.newPage();
  const node = { offline: true, planner, inbox: inbox(), tasks: tasks() };
  const mock = await startMockServer({ ws: true, sessions: remoteSessions('offline'), remote: { pc: node }, planner: { enabled: false } });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click('#abnav-planner');
    await expect(page.locator('#pl-status')).toHaveText('离线');
    await expect(page.locator('#pl-note')).toContainText('MBP 离线');
    await expect(page.locator('#pl-wait')).toHaveText('连上后显示');
    await expect(page.locator('#pl-target-note')).toHaveText('');
    await expect(page.locator('#pl-input')).toBeDisabled();
    await shot(page, 'node-offline-desktop');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('Planner stopped: said by the chip and the note, not again over the input', async ({ browser }) => {
  const ctx = await browser.newContext(DESKTOP);
  const page = await ctx.newPage();
  const mock = await startMockServer({ ws: true, inbox: inbox(), tasks: tasks(), planner: Object.assign({}, planner, { stopped: true }), plannerEvents: [] });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click('#abnav-planner');
    await expect(page.locator('#pl-status')).toHaveText('已停用');
    await expect(page.locator('#pl-note')).toContainText('Planner 已停用');
    await expect(page.locator('#pl-target-note')).toHaveText('');
    await expect(page.locator('#pl-input')).toBeDisabled();
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('a failure is 失败 everywhere, and the Home says no 失败 0 over one', async ({ browser }) => {
  const ctx = await browser.newContext(DESKTOP);
  const page = await ctx.newPage();
  const sessions = defaultSessions();
  for (const s of sessions.sessions) s.state = 'ready';
  const mock = await startMockServer({ ws: true, sessions, inbox: inbox(), tasks: tasks(), planner, plannerEvents: [] });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    const card = page.locator('#home-attn .ha-card');
    await expect(card.locator('.ha-n-wait')).toHaveText('等你 3');
    await expect(card.locator('.ha-n-fail')).toHaveCount(0);
    await expect(card.locator('.ha-row[data-id="in_err"] .ha-kind')).toHaveText('失败');
    await shot(page, 'home-failure-desktop');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});
