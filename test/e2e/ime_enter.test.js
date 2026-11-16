// E2E: an Enter that picks an IME candidate never sends (planner RFC §15.14
// ⑧). Chrome marks that keydown isComposing; Safari fires compositionend
// first and then the keydown with isComposing false and keyCode 229, which
// the Planner box, the 追问 drawer and the Home's quick ask used to take for
// a send. Each input that sends on Enter gets both sequences and keeps the
// half-written text, then a plain Enter still sends.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const SESSION = 'dashboard:direct:2026-01-01-120000-1:myproject';
const PLANNER = 'planner:main';
const desktop = { viewport: { width: 1280, height: 800 } };

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
