// E2E: an Enter that picks an IME candidate never sends (planner RFC §15.14
// ⑧). Chrome marks that keydown isComposing; Safari fires compositionend
// first and then the keydown with isComposing false and keyCode 229, which
// the Planner box, the 追问 drawer and the Home's quick ask used to take for
// a send. Each input that sends on Enter gets both sequences and keeps the
// half-written text, then a plain Enter still sends; so do the boxes that
// submit on Enter: the palette, the workspace and token boxes, rename and the
// manual model ids.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');
const { openSessionCard } = require('./session_nav');

const SESSION = 'dashboard:direct:2026-01-01-120000-1:myproject';
const PLANNER = 'planner:main';
const desktop = { viewport: { width: 1280, height: 800 } };

function json(body) {
  return { status: 200, contentType: 'application/json', body: JSON.stringify(body) };
}

// imeEnter plays the events an IME's candidate Enter produces on the
// focused element, the way the named browser orders them.
async function imeEnter(page, selector, browser) {
  await page.locator(selector).evaluate((el, safari) => {
    const enter = () => {
      const ev = new KeyboardEvent('keydown', { key: 'Enter', code: 'Enter', bubbles: true, cancelable: true, isComposing: !safari });
      Object.defineProperty(ev, 'keyCode', { get: () => 229 });
      Object.defineProperty(ev, 'which', { get: () => 229 });
      el.dispatchEvent(ev);
    };
    const end = () => el.dispatchEvent(new CompositionEvent('compositionend', { bubbles: true, data: '词' }));
    el.dispatchEvent(new CompositionEvent('compositionstart', { bubbles: true, data: '' }));
    if (safari) { end(); enter(); } else { enter(); end(); }
  }, browser === 'safari');
  // Long enough for a send that slipped through to reach the mock.
  await page.waitForTimeout(300);
}

test('the Planner box: a candidate Enter keeps the text, a plain Enter sends', async ({ browser }) => {
  const ctx = await browser.newContext(desktop);
  const page = await ctx.newPage();
  const mock = await startMockServer({
    ws: true, plannerEvents: [], inbox: { open: [], closed: [], counts: { open: 0, badge: 0 } },
    planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
  });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click('#abnav-planner');
    const conn = mock.wsConnections[mock.wsConnections.length - 1];
    await expect.poll(() => conn.messages.some((m) => m.type === 'subscribe' && m.key === PLANNER)).toBe(true);
    conn.send({ type: 'subscribed', key: PLANNER, state: 'ready' });
    await expect(page.locator('#pl-input')).toBeEnabled();

    await page.fill('#pl-input', 'iso 帮我看看 nao');
    await page.focus('#pl-input');
    for (const b of ['chrome', 'safari']) {
      await imeEnter(page, '#pl-input', b);
      expect(mock.plannerSendCalls, b).toEqual([]);
      await expect(page.locator('#pl-input')).toHaveValue('iso 帮我看看 nao');
    }
    await page.press('#pl-input', 'Enter');
    await expect.poll(() => mock.plannerSendCalls).toEqual([{ text: 'iso 帮我看看 nao' }]);
    await expect(page.locator('#pl-input')).toHaveValue('');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('the chat input and the 追问 drawer: a candidate Enter keeps the text, a plain Enter sends', async ({ browser }) => {
  const ctx = await browser.newContext(desktop);
  const page = await ctx.newPage();
  const mock = await startMockServer({
    keyEvents: { [SESSION]: [
      { type: 'user', detail: '解释一下', time: Date.now() - 60000, uuid: 'ime-u1' },
      // Over 500 characters, the length that gets 追问.
      { type: 'text', detail: '这是回答。'.repeat(120), time: Date.now() - 50000, uuid: 'ime-t1' },
    ] },
  });
  try {
    await page.goto(mock.url + '/dashboard');
    await page.click('.session-card[data-key="' + SESSION + '"]');
    const input = page.locator('#msg-input');
    await input.click();
    await input.pressSequentially('half');
    mock.resetCalls();
    for (const b of ['chrome', 'safari']) {
      await imeEnter(page, '#msg-input', b);
      expect(mock.sendCalls.length, b).toBe(0);
    }
    expect(await input.evaluate((el) => el.innerText.trim())).toBe('half');
    // Past the compositionend guard the chat input keeps for Safari.
    await page.waitForTimeout(50);
    await page.keyboard.press('Enter');
    await expect.poll(() => mock.sendCalls.length).toBe(1);
    expect(JSON.parse(mock.sendCalls[0]).text).toBe('half');

    // 追问 on the reply opens the drawer on a scratch session.
    const reply = page.locator('#events-scroll .event.text').last();
    await reply.hover();
    await reply.locator('[data-action="ask-aside"]').click();
    await expect(page.locator('#ad-input')).toBeVisible();
    await page.fill('#ad-input', '再说说 xi');
    await page.focus('#ad-input');
    mock.resetCalls();
    for (const b of ['chrome', 'safari']) {
      await imeEnter(page, '#ad-input', b);
      expect(mock.sendCalls.length, b).toBe(0);
      await expect(page.locator('#ad-input')).toHaveValue('再说说 xi');
    }
    await page.press('#ad-input', 'Enter');
    await expect.poll(() => mock.sendCalls.length).toBe(1);
    expect(JSON.parse(mock.sendCalls[0]).text).toBe('再说说 xi');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('the Home quick ask: a candidate Enter keeps the text, a plain Enter submits', async ({ browser }) => {
  const ctx = await browser.newContext(desktop);
  const page = await ctx.newPage();
  const mock = await startMockServer();
  try {
    await page.goto(mock.url + '/dashboard');
    const ta = page.locator('#quick-ask-input');
    await expect(ta).toBeVisible();
    await ta.fill('新问题 we');
    await ta.focus();
    for (const b of ['chrome', 'safari']) {
      await imeEnter(page, '#quick-ask-input', b);
      await expect(ta, b).toBeEnabled();
      await expect(ta).toHaveValue('新问题 we');
    }
    await ta.press('Enter');
    // Submitting replaces the Home with the new session's chat.
    await expect(page.locator('#quick-ask-input')).toHaveCount(0);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

// Both IME sequences on selector leave the dialog or popover in front open,
// with the half-written text still in the box.
async function expectImeEnterIgnored(page, selector, open, text) {
  for (const b of ['chrome', 'safari']) {
    await imeEnter(page, selector, b);
    await expect(page.locator(open), b).toHaveCount(1);
    await expect(page.locator(selector), b).toHaveValue(text);
  }
}

test('the palette and the custom workspace box: a candidate Enter neither picks nor creates', async ({ browser }) => {
  const ctx = await browser.newContext(desktop);
  const page = await ctx.newPage();
  const mock = await startMockServer();
  try {
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click('#btn-new-session');
    await page.waitForFunction(() => document.activeElement && document.activeElement.id === 'cp-input');
    await page.fill('#cp-input', 'my');
    await expectImeEnterIgnored(page, '#cp-input', '.cmd-palette-overlay', 'my');
    await page.press('#cp-input', 'Escape');
    await page.waitForSelector('.cmd-palette-overlay', { state: 'detached' });

    await page.click('#btn-new-session');
    await page.locator('.cmd-palette-item', { hasText: '打开自定义工作目录' }).click();
    const ws = page.locator('#new-workspace');
    await expect(ws).toBeVisible();
    await ws.fill('/tmp/工作');
    await expectImeEnterIgnored(page, '#new-workspace', '.modal-overlay', '/tmp/工作');
    await ws.press('Enter');
    await expect(page.locator('.modal-overlay')).toHaveCount(0);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('the token box: a candidate Enter does not log in', async ({ browser }) => {
  const ctx = await browser.newContext(desktop);
  const page = await ctx.newPage();
  const mock = await startMockServer({ requireAuth: true, authToken: 'secret-token' });
  try {
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('#token-input');
    await page.fill('#token-input', 'secret-token');
    await expectImeEnterIgnored(page, '#token-input', '.modal-overlay', 'secret-token');
    await page.press('#token-input', 'Enter');
    await page.waitForSelector('.modal-overlay', { state: 'detached' });
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('rename and the model boxes: a candidate Enter does not apply the half-written text', async ({ browser }) => {
  const ctx = await browser.newContext(desktop);
  const page = await ctx.newPage();
  const mock = await startMockServer();
  await page.route((url) => url.pathname === '/api/access-profiles', (route) => route.fulfill(json({
    profiles: [{ id: 'isengard', display_name: 'Isengard', secret_ok: true }], default: '',
  })));
  try {
    await page.goto(mock.url + '/dashboard');
    await openSessionCard(page, page.locator('.session-card[data-key="' + SESSION + '"]'));

    await page.click('[data-action="session-rename"]');
    await page.fill('.prompt-input', '新名 zi');
    await expectImeEnterIgnored(page, '.prompt-input', '.prompt-overlay', '新名 zi');
    await page.press('.prompt-input', 'Enter');
    await expect(page.locator('.prompt-overlay')).toHaveCount(0);

    await page.click('#header-model');
    await page.fill('#tuning-manual-input', 'claude-op');
    await expectImeEnterIgnored(page, '#tuning-manual-input', '#tuning-popover', 'claude-op');
    expect(mock.overrideCalls).toEqual([]);
    await page.press('#tuning-manual-input', 'Enter');
    await expect.poll(() => mock.overrideCalls).toEqual([{ key: SESSION, model: 'claude-op' }]);

    await page.click('#header-access-profile');
    const pop = page.locator('#tuning-popover');
    await pop.locator('.ap-opt[data-profile="isengard"]').click();
    await pop.locator('#ap-model-select').selectOption({ label: '手动输入…' });
    await pop.locator('#ap-model-input').fill('claude-fa');
    await expectImeEnterIgnored(page, '#ap-model-input', '#tuning-popover', 'claude-fa');
    expect(mock.overrideCalls).toHaveLength(1);
    await page.press('#ap-model-input', 'Enter');
    await expect.poll(() => mock.overrideCalls.length).toBe(2);
    expect(mock.overrideCalls[1]).toEqual({ key: SESSION, access_profile: 'isengard', model: 'claude-fa' });
  } finally {
    await ctx.close();
    mock.server.close();
  }
});
