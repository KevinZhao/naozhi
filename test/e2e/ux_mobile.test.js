// E2E: the mobile UX review's fixes (11-18). On a phone the system back
// closes the full-screen workbench and the 追问 drawer instead of leaving
// the session (the Planner page's own entries are ux_planner's); a long
// session name leaves the header its rename and export buttons; the
// keyboard takes the room the Planner / settings panels kept for the tab
// bar; 设置's red dot leads to a 系统 row at its top, and 系统 keeps 设置 lit;
// the cron run list says how many it unfolds; the Planner page's small
// buttons are tap-sized; Esc closes the Planner's "⋯". Screenshots go to
// test-results, and to $NZ_SHOTS_DIR when set.
const fs = require('fs');
const path = require('path');
const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const SHOTS = process.env.NZ_SHOTS_DIR || '';
const PLANNER = 'planner:main';
const SESSION = 'dashboard:direct:2026-01-01-120000-1:myproject';
const TASK = 'task:uxm0001';
const now = Date.now();
const iso = (ms) => new Date(ms).toISOString();
const PHONE = { viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true };

async function shot(page, name) {
  const file = test.info().outputPath(name + '.png');
  await page.screenshot({ path: file });
  if (SHOTS) {
    fs.mkdirSync(SHOTS, { recursive: true });
    fs.copyFileSync(file, path.join(SHOTS, name + '.png'));
  }
}

const tasks = () => ({ tasks: [{
  id: TASK, kind: 'session', title: 'iso 写 hello 程序', state: 'doing', by: 'planner', at: iso(now - 60000),
  created: iso(now - 3600000), new: true, result_at: iso(now - 60000),
}] });
const planner = { enabled: true, stopped: false, key: PLANNER, alive: true };
const emptyInbox = { open: [], closed: [], counts: { open: 0, badge: 0 } };
const plannerEvents = [
  { type: 'user', detail: 'iso 今天做什么', time: now - 600000, uuid: 'uxm-u1' },
  { type: 'text', detail: 'iso 我派了一个会话写 hello 程序。', time: now - 590000, uuid: 'uxm-t1' },
];

function plannerMock(extra) {
  return startMockServer(Object.assign({
    ws: true, planner, tasks: tasks(), inbox: emptyInbox, plannerEvents,
    keyEvents: { [TASK]: [{ type: 'text', detail: 'iso hello 写好了', time: now - 60000, uuid: 'uxm-k1' }] },
  }, extra || {}));
}

const viewOf = (page) => page.evaluate(() => (history.state && history.state.view) || '');

test('phone: the system back closes the workbench over a session, then leaves the session', async ({ browser }) => {
  const ctx = await browser.newContext(PHONE);
  const page = await ctx.newPage();
  const mock = await startMockServer({ ws: true });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click('.session-card[data-key="' + SESSION + '"]');
    await expect(page.locator('body')).toHaveClass(/mobile-chat-view/);
    await page.locator('.btn-workbench').click();
    const wb = page.locator('#fv-drawer');
    await expect(wb).toHaveClass(/fv-open/);
    await page.locator('.wb-tab[data-wb-tab="details"]').click();
    await page.goBack();
    await expect(wb).not.toHaveClass(/fv-open/);
    await expect(page.locator('body')).toHaveClass(/mobile-chat-view/);
    await shot(page, 'wb-back-phone');
    await page.goBack();
    await expect(page.locator('body')).toHaveClass(/mobile-list-view/);
    expect(page.url()).toContain('/dashboard');

    // Closed with its ×, the next back leaves the session.
    await page.click('.session-card[data-key="' + SESSION + '"]');
    await page.locator('.btn-workbench').click();
    await expect(wb).toHaveClass(/fv-open/);
    await page.locator('#fv-btn-close').click();
    await expect.poll(() => viewOf(page)).toBe('chat');
    await page.goBack();
    await expect(page.locator('body')).toHaveClass(/mobile-list-view/);
    expect(page.url()).toContain('/dashboard');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

for (const theme of ['light', 'dark']) {
  test(`phone ${theme}: a long session name leaves the header its rename and export buttons`, async ({ browser }) => {
    const ctx = await browser.newContext(Object.assign({ colorScheme: theme }, PHONE));
    const page = await ctx.newPage();
    const s = defaultSessions();
    s.sessions[0].last_prompt = 'iso 帮我把这周合并的所有 PR 整理成一份周报，按模块分组，附上链接';
    const mock = await startMockServer({ ws: true, sessions: s });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');
      await page.click('.session-card[data-key="' + SESSION + '"]');
      await expect(page.locator('.sidebar')).not.toBeInViewport(); // slid away
      const h2 = page.locator('.main-header h2');
      await expect(h2).toContainText('iso 帮我');
      for (const sel of ['.btn-rename:not(.btn-download)', '.btn-download']) {
        const btn = h2.locator(sel);
        await expect(btn).toBeInViewport();
        // Nothing covers or clips it: its centre is the button.
        // Polled: the chat view slides in.
        await expect.poll(() => btn.evaluate((el) => {
          const r = el.getBoundingClientRect();
          const at = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
          return el.contains(at) || (at && at.outerHTML.slice(0, 80));
        })).toBe(true);
      }
      // The name is what is cut.
      expect(await h2.locator('.hdr-title-t').evaluate((el) => el.scrollWidth > el.clientWidth)).toBe(true);
      await shot(page, `session-header-${theme}`);
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
}

test('phone: with the keyboard up the Planner and settings panels keep no room for the tab bar', async ({ browser }) => {
  const ctx = await browser.newContext(PHONE);
  const page = await ctx.newPage();
  const mock = await plannerMock();
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    // As mobile_nav.js does on a device when the keyboard opens.
    const keyboard = () => page.evaluate(() => {
      document.body.classList.add('kbd-open');
      document.documentElement.style.setProperty('--vv-height', '500px');
    });
    const pad = (sel) => page.locator(sel).evaluate((el) => getComputedStyle(el).paddingBottom);
    await page.locator('#abnav-planner').click();
    await expect(page.locator('#pl-input')).toBeVisible();
    await keyboard();
    expect(await pad('#planner-main')).toBe('0px');
    const input = await page.locator('#pl-input').boundingBox();
    expect(500 - (input.y + input.height)).toBeLessThan(40);
    await shot(page, 'kbd-planner-phone');
    await page.evaluate(() => document.body.classList.remove('kbd-open'));
    await page.locator('#abnav-settings').click();
    await keyboard();
    expect(await pad('#settings-main')).toBe('0px');
    await page.evaluate(() => document.body.classList.remove('kbd-open'));
    await page.locator('#abnav-cron').click();
    await keyboard();
    expect(await pad('#cron-main')).toBe('0px');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

for (const theme of ['light', 'dark']) {
  test(`phone ${theme}: 设置's red dot leads to the 系统 row at its top; 系统 keeps 设置 lit`, async ({ browser }) => {
    const ctx = await browser.newContext(Object.assign({ colorScheme: theme }, PHONE));
    const page = await ctx.newPage();
    const mock = await startMockServer({ ws: true });
    await page.route('**/api/system/daemons', (route) => route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify([{ id: 'auto-rename', enabled: true, consecutive_cli_failures: 2, last_run: { state: 'failed', started_at: now - 60000 } }]),
    }));
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');
      await page.locator('#abnav-settings').click();
      await expect(page.locator('#abnav-settings-badge')).toBeVisible();
      const row = page.locator('#settings-open-system');
      await expect(row).toBeInViewport();
      // It comes before 主题, and carries the dot.
      expect(await row.evaluate((el) => {
        const theme = document.getElementById('settings-theme-group');
        return !!(el.compareDocumentPosition(theme) & Node.DOCUMENT_POSITION_FOLLOWING);
      })).toBe(true);
      expect(await row.evaluate((el) => getComputedStyle(el, '::after').backgroundColor)).not.toBe('rgba(0, 0, 0, 0)');
      await shot(page, `sysbadge-settings-${theme}`);
      await row.click();
      await expect(page.locator('#system-main')).toBeVisible();
      await expect(page.locator('#abnav-settings')).toHaveAttribute('aria-pressed', 'true');
      await expect(page.locator('#abnav-settings')).toHaveClass(/active/);
      await shot(page, `sysbadge-system-${theme}`);
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
}

test('desktop: no dot on the 系统 row, and 系统 lights only its own tab', async ({ browser }) => {
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const page = await ctx.newPage();
  const mock = await startMockServer({ ws: true });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.locator('#abnav-system').click();
    await expect(page.locator('#abnav-system')).toHaveAttribute('aria-pressed', 'true');
    await expect(page.locator('#abnav-settings')).toHaveAttribute('aria-pressed', 'false');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('cron: the run list says how many it unfolds, and what 加载更多 brings stays unfolded', async ({ browser }) => {
  const ctx = await browser.newContext(PHONE);
  const page = await ctx.newPage();
  const mock = await startMockServer({});
  try {
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click('#abnav-cron');
    await page.locator('.cj-row').filter({ hasText: 'check server status' }).first().click();
    const panel = page.locator('#cron-timeline-panel');
    await expect(panel.locator('.ctr').first()).toBeVisible();
    const n = await panel.locator('.ctr').count();
    test.skip(n <= 5, 'the fixture embeds five runs or fewer');
    const btn = panel.locator('.ct-show-all');
    await expect(btn).toHaveText('展开其余 ' + (n - 5) + ' 条');
    await expect(btn).not.toContainText('全部');
    await shot(page, 'cron-drawer-phone');
    await btn.click();
    await expect(panel.locator('.ctr:visible')).toHaveCount(n);
    await shot(page, 'cron-showall-phone');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

for (const theme of ['light', 'dark']) {
  test(`phone ${theme}: the Planner page's small buttons are tap-sized`, async ({ browser }) => {
    const ctx = await browser.newContext(Object.assign({ colorScheme: theme }, PHONE));
    const page = await ctx.newPage();
    const mock = await plannerMock();
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');
      await page.locator('#abnav-planner').click();
      const h = (loc) => loc.evaluate((el) => el.getBoundingClientRect().height);
      const news = page.locator('#pl-news [data-action="tasks-only-new"]');
      await expect(news).toBeVisible();
      expect(await h(news)).toBeGreaterThanOrEqual(32);
      await page.locator('#pl-more > summary').click();
      for (const b of await page.locator('#pl-tools .pl-layer-b').all()) expect(await h(b)).toBeGreaterThanOrEqual(32);
      expect(await h(page.locator('#pl-tools .pl-switch'))).toBeGreaterThanOrEqual(44);
      expect(await h(page.locator('#pl-tools [data-action="memory-open"]'))).toBeGreaterThanOrEqual(44);
      await shot(page, `planner-more-${theme}`);
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
}

test('desktop: Esc closes the Planner page\'s "⋯" back onto it', async ({ browser }) => {
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const page = await ctx.newPage();
  const mock = await plannerMock();
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.locator('#abnav-planner').click();
    const more = page.locator('#pl-more');
    await page.locator('#pl-more > summary').focus();
    await page.keyboard.press('Enter');
    await expect(more).toHaveJSProperty('open', true);
    await page.keyboard.press('Escape');
    await expect(more).toHaveJSProperty('open', false);
    await expect(page.locator('#pl-more > summary')).toBeFocused();
    // From a button inside it too.
    await page.keyboard.press('Enter');
    await page.locator('#pl-tools button').first().focus();
    await page.keyboard.press('Escape');
    await expect(more).toHaveJSProperty('open', false);
    // A palette opened over it takes the first Esc.
    await page.locator('#pl-more > summary').click();
    await expect(more).toHaveJSProperty('open', true);
    await page.keyboard.press('Control+k');
    await expect(page.locator('#cp-input')).toBeFocused();
    await page.keyboard.press('Escape');
    await expect(page.locator('.cmd-palette-overlay')).toHaveCount(0);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('phone: the system back closes the 追问 drawer and stays in the session', async ({ browser }) => {
  const ctx = await browser.newContext(PHONE);
  const page = await ctx.newPage();
  const keyEvents = {
    [SESSION]: [
      { type: 'user', detail: '解释一下', time: now - 60000, uuid: 'uxm-a1' },
      { type: 'text', detail: '这是回答。'.repeat(120), time: now - 50000, uuid: 'uxm-a2' },
    ],
    'dashboard:scratch:2026-01-01-130000-9:myproject': [],
  };
  const mock = await startMockServer({ keyEvents });
  try {
    await page.goto(mock.url + '/dashboard');
    await page.click('.session-card[data-key="' + SESSION + '"]');
    await expect(page.locator('body')).toHaveClass(/mobile-chat-view/);
    await page.locator('#events-scroll .event.text').last().locator('[data-action="ask-aside"]').click();
    const drawer = page.locator('#aside-drawer');
    await expect(drawer).toHaveClass(/visible/);
    await page.goBack();
    await expect(drawer).not.toHaveClass(/visible/);
    await expect(page.locator('body')).toHaveClass(/mobile-chat-view/);
    await page.goBack();
    await expect(page.locator('body')).toHaveClass(/mobile-list-view/);
    expect(page.url()).toContain('/dashboard');

    // Closed with its ×, the next back leaves the session.
    await page.click('.session-card[data-key="' + SESSION + '"]');
    await page.locator('#events-scroll .event.text').last().locator('[data-action="ask-aside"]').click();
    await expect(drawer).toHaveClass(/visible/);
    await expect.poll(() => viewOf(page)).toBe('scratch');
    await page.locator('#ad-close').click();
    await expect(drawer).not.toHaveClass(/visible/);
    await expect.poll(() => viewOf(page)).toBe('chat');
    await page.goBack();
    await expect(page.locator('body')).toHaveClass(/mobile-list-view/);
    expect(page.url()).toContain('/dashboard');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});
