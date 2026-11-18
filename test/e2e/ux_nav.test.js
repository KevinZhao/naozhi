// E2E: the navigation surfaces' UX review (fix/ux-nav).
//   1. A dialog takes the focus when it opens; Esc closes it wherever the
//      focus is, and over stacked dialogs only the top one, keeping what was
//      typed below; Tab stays inside.
//   2. A first /api/sessions that fails with the WS up is retried until the
//      list paints, and says so in place of the skeletons.
//   3. The asset reader shows the row clicked last, not the reply that came
//      in last.
//   4. Asset rows and group heads answer the keyboard.
//   5. Rename counts UTF-8 bytes as the gateway does and keeps the dialog
//      open with what was typed.
//   6. Option+N on a Mac (key "Dead", code KeyN) opens a new session.
//   7. 加载更多 in the cron run history shows what it loaded.
//   8. Esc closes a cron row's ⋯ menu; the menu says 立即执行 like the drawer.
//   9. Settings · 访问档与模型: a failed models read hides no profile; a
//      profile with missing credentials says so.
//  10. Chinese wording on the navigation surfaces.
// With NZ_SHOTS_DIR set the screenshots go there too.
const fs = require('fs');
const path = require('path');
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const DESKTOP = { viewport: { width: 1440, height: 900 } };
const PHONE = { viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true };
const json = (body, status = 200) => ({ status, contentType: 'application/json', body: JSON.stringify(body) });

async function shot(page, name) {
  const dir = process.env.NZ_SHOTS_DIR;
  if (!dir) return;
  await page.waitForTimeout(250);
  fs.mkdirSync(dir, { recursive: true });
  await page.screenshot({ path: path.join(dir, name + '.png') });
}

// homeLoaded waits for the Home to settle: on a cold start the quick-ask box
// takes the focus 50ms after its module runs, which can land after the first
// session card paints and would swallow the next keystroke.
async function homeLoaded(page) {
  await page.waitForSelector('.session-card');
  await expect(page.locator('#quick-ask-input')).toBeFocused();
}

const focusInside = (page, sel) => page.evaluate((s) => {
  const o = [...document.querySelectorAll(s)].pop();
  return !!o && o.contains(document.activeElement);
}, sel);

test.describe('dialogs: focus and Esc', () => {
  let mock;
  test.beforeAll(async () => { mock = await startMockServer(); });
  test.afterAll(() => mock.server.close());

  test('the shortcut sheet takes the focus and Esc closes it', async ({ browser }) => {
    const ctx = await browser.newContext(DESKTOP);
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await homeLoaded(page);
    // '?' has to land on the page itself, not in a box that took the focus
    // late: press it only with nothing focused, and again if it was swallowed.
    await expect(async () => {
      await page.evaluate(() => document.activeElement && document.activeElement.blur());
      expect(await page.evaluate(() => document.activeElement === document.body)).toBe(true);
      await page.keyboard.press('?');
      await expect(page.locator('.cheatsheet-overlay')).toHaveCount(1, { timeout: 1000 });
    }).toPass({ timeout: 10000 });
    expect(await focusInside(page, '.cheatsheet-overlay')).toBe(true);
    // The sheet fits the window and scrolls inside: its 好的 is reachable.
    const box = await page.locator('.modal.cheatsheet').boundingBox();
    expect(box.y).toBeGreaterThanOrEqual(0);
    expect(box.y + box.height).toBeLessThanOrEqual(900);
    await page.keyboard.press('Tab');
    expect(await focusInside(page, '.cheatsheet-overlay')).toBe(true);
    await expect(page.locator('[data-action="cheatsheet-dismiss"]')).toBeInViewport();
    await shot(page, 'cheatsheet-focus');
    await page.keyboard.press('Escape');
    await expect(page.locator('.cheatsheet-overlay')).toHaveCount(0);
    await ctx.close();
  });

  test('project settings: focus in, Tab stays in, Esc closes even from the backdrop', async ({ browser }) => {
    const ctx = await browser.newContext(DESKTOP);
    const page = await ctx.newPage();
    await page.route((u) => u.pathname === '/api/projects/config', (r) => r.fulfill(json({})));
    await page.goto(mock.url + '/dashboard');
    await page.locator('[data-action="project-settings"][data-name="myproject"]').click();
    const ov = page.locator('.modal-overlay');
    await expect(ov).toHaveCount(1);
    expect(await focusInside(page, '.modal-overlay')).toBe(true);
    for (let i = 0; i < 3; i++) {
      await page.keyboard.press('Tab');
      expect(await focusInside(page, '.modal-overlay')).toBe(true);
    }
    await page.keyboard.press('Escape');
    await expect(ov).toHaveCount(0);

    // A click on the backdrop's edge leaves the focus on <body>.
    await page.locator('[data-action="project-settings"][data-name="myproject"]').click();
    await expect(ov).toHaveCount(1);
    await page.evaluate(() => document.activeElement && document.activeElement.blur());
    await page.keyboard.press('Escape');
    await expect(ov).toHaveCount(0);
    await ctx.close();
  });

  test('stacked dialogs: Esc closes the top one and keeps the edit below', async ({ browser }) => {
    const ctx = await browser.newContext(DESKTOP);
    const page = await ctx.newPage();
    await page.route((u) => u.pathname === '/api/projects/config', (r) => r.fulfill(json({})));
    await page.goto(mock.url + '/dashboard');
    await page.locator('[data-action="project-settings"][data-name="myproject"]').click();
    await page.locator('#ps-display-name').fill('改过的名字');
    await page.locator('[data-action="ps-new-profile"]').click();
    await expect(page.locator('.modal-overlay')).toHaveCount(2);
    expect(await focusInside(page, '.modal-overlay')).toBe(true);
    await shot(page, 'stacked-open');
    await page.keyboard.press('Escape');
    await expect(page.locator('.modal-overlay')).toHaveCount(1);
    await expect(page.locator('#ps-display-name')).toHaveValue('改过的名字');
    await expect(page.locator('#cap-template')).toHaveCount(0);
    await shot(page, 'stacked-after-esc');
    await page.keyboard.press('Escape');
    await expect(page.locator('.modal-overlay')).toHaveCount(0);
    await ctx.close();
  });

  test('the cron edit dialog takes the focus and Esc closes it', async ({ browser }) => {
    const ctx = await browser.newContext(DESKTOP);
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click('#abnav-cron');
    await page.locator('.cj-row[data-cron-id="cron-001"] .cj-schedule').click();
    const ov = page.locator('.modal-overlay');
    await expect(ov).toHaveCount(1);
    expect(await focusInside(page, '.modal-overlay')).toBe(true);
    await page.keyboard.press('Escape');
    await expect(ov).toHaveCount(0);
    await ctx.close();
  });
});

for (const [name, opts] of [['desktop', DESKTOP], ['phone', PHONE]]) {
  test(`a failed first sessions load is retried with the WS up (${name})`, async ({ browser }) => {
    const mock = await startMockServer({ ws: true });
    const ctx = await browser.newContext(opts);
    const page = await ctx.newPage();
    let failing = true;
    let calls = 0;
    await page.route((u) => u.pathname === '/api/sessions', (r) => {
      calls++;
      return failing ? r.fulfill(json({ error: 'bad gateway' }, 502)) : r.continue();
    });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page);
      await expect(page.locator('#session-list .sl-load-err')).toHaveText('会话列表加载失败，重试中…');
      await shot(page, 'sessions-fail-' + name);
      failing = false;
      await expect(page.locator('.session-card').first()).toBeVisible({ timeout: 8000 });
      await expect(page.locator('#session-list .sl-load-err')).toHaveCount(0);
      await shot(page, 'sessions-recovered-' + name);
      // Painted with the WS up: the fallback poll stops.
      const n = calls;
      await page.waitForTimeout(5600);
      expect(calls).toBe(n);
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
}

const INVENTORY = {
  assets: [
    { kind: 'skill', name: 'skill-0', description: '第一个', source: { kind: 'user' }, rel_path: 'skills/s0/SKILL.md' },
    { kind: 'skill', name: 'skill-1', description: '第二个', source: { kind: 'user' }, rel_path: 'skills/s1/SKILL.md' },
    { kind: 'skill', name: 'skill-p', description: '插件里的', source: { kind: 'plugin', plugin: 'demo' }, rel_path: 'skills/p/SKILL.md' },
  ],
  totals: { skill: 3 },
  plugins: [],
};

async function routeAssets(page) {
  await page.route((u) => u.pathname === '/api/cc/assets', (r) => r.fulfill(json(INVENTORY)));
  await page.route((u) => u.pathname === '/api/cc/assets/raw', async (r) => {
    const rel = new URL(r.request().url()).searchParams.get('rel');
    if (rel === 'skills/s0/SKILL.md') await new Promise((res) => setTimeout(res, 1200));
    return r.fulfill({ status: 200, contentType: 'text/plain', body: 'CONTENT OF ' + rel });
  });
}

test.describe('assets', () => {
  let mock;
  test.beforeAll(async () => { mock = await startMockServer(); });
  test.afterAll(() => mock.server.close());

  for (const [name, opts] of [['desktop', DESKTOP], ['phone', PHONE]]) {
    test(`a slow earlier reply does not overwrite the row clicked last (${name})`, async ({ browser }) => {
      const ctx = await browser.newContext(opts);
      const page = await ctx.newPage();
      await routeAssets(page);
      await page.goto(mock.url + '/dashboard');
      await page.click('#abnav-assets');
      await page.locator('.asset-row', { hasText: 'skill-0' }).click();
      if (name === 'phone') await page.click('#asset-back');
      await page.locator('.asset-row', { hasText: 'skill-1' }).click();
      await expect(page.locator('#asset-main-title')).toHaveText('skill-1');
      await expect(page.locator('#asset-main-body')).toHaveText('CONTENT OF skills/s1/SKILL.md');
      await page.waitForTimeout(1500);
      await expect(page.locator('#asset-main-body')).toHaveText('CONTENT OF skills/s1/SKILL.md');
      await shot(page, 'assets-race-' + name);
      await ctx.close();
    });
  }

  test('group heads and rows answer the keyboard', async ({ browser }) => {
    const ctx = await browser.newContext(DESKTOP);
    const page = await ctx.newPage();
    await routeAssets(page);
    await page.goto(mock.url + '/dashboard');
    await page.click('#abnav-assets');
    await expect(page.locator('.asset-grp')).toHaveCount(2);
    await page.locator('#asset-search').focus();
    await page.keyboard.press('Tab');
    const head = page.locator('.asset-grp').first();
    await expect(head).toBeFocused();
    await expect(head).toHaveAttribute('aria-expanded', 'true');
    await page.keyboard.press('Tab');
    await expect(page.locator('.asset-row', { hasText: 'skill-0' })).toBeFocused();
    await page.keyboard.press('Tab');
    await page.keyboard.press('Enter');
    await expect(page.locator('#asset-main-title')).toHaveText('skill-1');
    await expect(page.locator('#asset-main-body')).toHaveText('CONTENT OF skills/s1/SKILL.md');
    // The plugin group starts folded; Space unfolds it.
    await page.keyboard.press('Tab');
    const plug = page.locator('.asset-grp[data-g="plugin:demo"]');
    await expect(plug).toBeFocused();
    await expect(plug).toHaveAttribute('aria-expanded', 'false');
    await page.keyboard.press(' ');
    await expect(plug).toHaveAttribute('aria-expanded', 'true');
    await expect(page.locator('.asset-row', { hasText: 'skill-p' })).toBeVisible();
    await shot(page, 'assets-keyboard');
    await ctx.close();
  });
});

test.describe('chat surfaces', () => {
  let mock;
  test.beforeAll(async () => { mock = await startMockServer(); });
  test.afterAll(() => mock.server.close());

  test('rename counts bytes and keeps the dialog and the text when too long', async ({ browser }) => {
    const ctx = await browser.newContext(DESKTOP);
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.locator('.session-card').first().click();
    await page.click('[data-action="session-rename"]');
    const long = '汉'.repeat(55);
    await page.fill('.prompt-input', long);
    await page.press('.prompt-input', 'Enter');
    await expect(page.locator('.prompt-overlay')).toHaveCount(1);
    await expect(page.locator('.prompt-error')).toHaveText('太长了，最多 128 字节（约 42 个汉字）');
    await expect(page.locator('.prompt-input')).toHaveValue(long);
    expect(mock.labelCalls).toHaveLength(0);
    await shot(page, 'rename-too-long');
    // Typing clears the line; 42 汉字 (126 bytes) goes through.
    await page.fill('.prompt-input', '汉'.repeat(42));
    await expect(page.locator('.prompt-error')).toBeHidden();
    await page.press('.prompt-input', 'Enter');
    await expect(page.locator('.prompt-overlay')).toHaveCount(0);
    await expect.poll(() => mock.labelCalls.map((c) => c.status)).toEqual([200]);
    await ctx.close();
  });

  test('Option+N on a Mac (key Dead, code KeyN) opens a new session', async ({ browser }) => {
    const ctx = await browser.newContext(DESKTOP);
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.evaluate(() => {
      document.activeElement && document.activeElement.blur();
      document.body.dispatchEvent(new KeyboardEvent('keydown', { key: 'Dead', code: 'KeyN', altKey: true, bubbles: true, cancelable: true }));
    });
    await expect(page.locator('.cmd-palette-overlay')).toHaveCount(1);
    await ctx.close();
  });

  test('wording: the 自动化 rail title, the new-session empty match', async ({ browser }) => {
    const ctx = await browser.newContext(DESKTOP);
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await expect(page.locator('#abnav-cron')).toHaveAttribute('title', '自动化（定时任务）');
    await page.click('#btn-new-session');
    await page.fill('#cp-input', 'zzzz-not-there');
    await expect(page.locator('.cmd-palette-empty')).toHaveText('没有匹配“zzzz-not-there”的项目');
    await shot(page, 'new-nomatch');
    await ctx.close();
  });
});

test.describe('cron', () => {
  test('加载更多 past the five embedded runs shows the loaded rows at once', async ({ browser }) => {
    const now = Date.now();
    const run = (i) => ({ run_id: 'run-p' + String(i).padStart(3, '0'), state: 'succeeded', started_at: now - i * 3600000, ended_at: now - i * 3600000 + 5000, duration_ms: 5000, trigger: 'cron' });
    const jobs = [{
      id: 'cron-001', schedule: '@every 1h', prompt: 'check server status', work_dir: '/home/user/workspace/myproject',
      paused: false, created_at: now - 86400000, next_run: now + 3600000,
      recent_runs: [1, 2, 3, 4, 5].map(run), stats: { total: 120, succeeded: 120, failed: 0, skipped: 0 },
    }];
    const mock = await startMockServer({ cronJobs: jobs });
    const ctx = await browser.newContext(DESKTOP);
    const page = await ctx.newPage();
    await page.route((u) => u.pathname === '/api/cron/runs', (r) => r.fulfill(json({
      runs: Array.from({ length: 50 }, (_, i) => run(6 + i)), next_before: now - 56 * 3600000,
    })));
    try {
      await page.goto(mock.url + '/dashboard');
      await page.waitForSelector('.session-card');
      await page.click('#abnav-cron');
      await page.click('.cj-row[data-cron-id="cron-001"]');
      const more = page.locator('#cron-timeline-panel .ct-more-btn');
      await expect(more).toHaveText('加载更多');
      await more.click();
      await expect(page.locator('#cron-timeline-panel .ctr:visible')).toHaveCount(55);
      await expect(more).toHaveText('加载更多');
      await shot(page, 'cron-loadmore');
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });

  test('Esc closes a row ⋯ menu, which says 立即执行', async ({ browser }) => {
    const mock = await startMockServer();
    const ctx = await browser.newContext(DESKTOP);
    const page = await ctx.newPage();
    try {
      await page.goto(mock.url + '/dashboard');
      await page.waitForSelector('.session-card');
      await page.click('#abnav-cron');
      const btn = page.locator('.cj-row[data-cron-id="cron-001"] .cj-menu-btn');
      await btn.click();
      await expect(page.locator('.cj-menu')).toHaveCount(1);
      await expect(btn).toHaveAttribute('aria-expanded', 'true');
      await expect(page.locator('.cj-menu-item[data-menu-action="run"]')).toHaveText('立即执行');
      await page.keyboard.press('Escape');
      await expect(page.locator('.cj-menu')).toHaveCount(0);
      await expect(btn).toHaveAttribute('aria-expanded', 'false');
      await expect(btn).toBeFocused();
      // The next row's ⋯ now opens its own menu.
      await page.locator('.cj-row[data-cron-id="cron-002"] .cj-menu-btn').click();
      await expect(page.locator('.cj-menu-item[data-id="cron-002"]').first()).toBeVisible();
      await shot(page, 'cron-menu-row2');
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
});

test('settings: a failed models read hides no profile; missing credentials are marked', async ({ browser }) => {
  const mock = await startMockServer();
  const ctx = await browser.newContext(DESKTOP);
  const page = await ctx.newPage();
  await page.route((u) => u.pathname === '/api/access-profiles', (r) => r.fulfill(json({
    profiles: [
      { id: 'broken', display_name: 'Broken', secret_ok: false },
      { id: 'team', display_name: 'Team Bedrock', secret_ok: true },
    ], default: '',
  })));
  await page.route((u) => u.pathname === '/api/settings/models', (r) => r.fulfill(json({ error: 'invalid settings JSON' }, 500)));
  try {
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click('#abnav-settings');
    const sec = page.locator('#settings-auth');
    await expect(sec.locator('.sa-row', { hasText: 'Team Bedrock' })).toHaveCount(1);
    const broken = sec.locator('.sa-row', { hasText: 'Broken' });
    await expect(broken.locator('.sa-tag-warn')).toHaveText('⚠ 凭证缺失');
    await expect(sec.locator('.sa-row', { hasText: 'Team Bedrock' }).locator('.sa-tag-warn')).toHaveCount(0);
    await expect(sec).toContainText('模型加载失败：invalid settings JSON');
    await expect(sec).not.toContainText('未开 naozhi_settings.enabled');
    await sec.scrollIntoViewIfNeeded();
    await shot(page, 'settings-auth');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});
