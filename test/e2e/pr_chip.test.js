// pr_chip.test.js — header PR chip. A session whose /api/sessions row carries
// code_changes (newest last) shows the newest PRs first as links with the
// newest one's state; older ones fold into "+N". Only http(s) URLs render, and
// CLI-scraped text never becomes markup. A session without code_changes
// collapses the mount.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const pr = (n, action, extra) => ({
  provider: 'github', url: 'https://github.com/acme/myproject/pull/' + n,
  repo: 'acme/myproject', identifier: String(n), action, ...extra,
});

function prSessions() {
  const base = {
    state: 'ready',
    platform: 'dashboard',
    agent: 'general',
    cli_name: 'claude',
    backend: 'claude',
    workspace: '/home/user/workspace/myproject',
    last_active: Date.now() - 60000,
    node: 'local',
    project: 'myproject',
  };
  return {
    sessions: [
      {
        ...base,
        key: 'dashboard:direct:2026-01-01-120000-1:myproject',
        code_changes: [
          pr(10, 'closed'),
          pr(11, 'created'),
          { url: 'javascript:alert(1)', identifier: '666', action: 'created' },
          pr(12, 'pushed', { identifier: '<img src=x onerror=alert(1)>' }),
          pr(13, 'merged', { branch: 'feat-x' }),
        ],
      },
      { ...base, key: 'dashboard:direct:2026-01-01-120001-2:clean' },
    ],
    stats: { version: 1, running: 0, ready: 2, max_procs: 10 },
    nodes: {},
    history_sessions: [],
  };
}

async function selectByKey(page, keySuffix) {
  await page.locator(`.session-card[data-key*="${keySuffix}"]`).first().click();
  await expect(page.locator('.main-header #header-pr')).toHaveCount(1);
}

test.describe('PR chip', () => {
  let server;
  let baseURL;

  test.beforeAll(async () => {
    server = await startMockServer({ sessions: prSessions() });
    baseURL = server.url;
  });

  test.afterAll(async () => {
    if (server) await new Promise(r => server.server.close(r));
  });

  test('newest PRs lead as links; older ones fold into +N', async ({ page }) => {
    await page.goto(baseURL + '/dashboard');
    await page.waitForSelector('.session-card', { timeout: 8000 });
    await selectByKey(page, '120000-1:myproject');

    const chip = page.locator('.main-header #header-pr .pr-chip');
    await expect(chip).toHaveCount(1);
    const links = chip.locator('a.pr-chip-link');
    await expect(links).toHaveCount(3);
    await expect(links.nth(0)).toHaveText('#13');
    await expect(links.nth(0)).toHaveAttribute('href', 'https://github.com/acme/myproject/pull/13');
    await expect(links.nth(0)).toHaveAttribute('rel', 'noopener noreferrer');
    await expect(links.nth(0)).toHaveAttribute('target', '_blank');
    await expect(links.nth(0)).toHaveClass(/pr-action-merged/);
    expect(await links.nth(0).getAttribute('title')).toContain('分支: feat-x');
    await expect(chip.locator('.pr-chip-action')).toHaveText('已合并');
    await expect(links.nth(2)).toHaveText('#11');

    const more = chip.locator('.pr-chip-more');
    await expect(more).toHaveText('+1');
    expect(await more.getAttribute('title')).toContain('acme/myproject#10 · 已关闭');

    // The javascript: entry is dropped and the scraped identifier is text.
    await expect(chip.locator('a[href^="javascript"]')).toHaveCount(0);
    await expect(chip.locator('img')).toHaveCount(0);
    await expect(links.nth(1)).toHaveText('#<img src=x onerror=alert(1)>');
  });

  test('a session without code changes collapses the mount', async ({ page }) => {
    await page.goto(baseURL + '/dashboard');
    await page.waitForSelector('.session-card', { timeout: 8000 });
    await selectByKey(page, 'clean');

    await expect(page.locator('.main-header #header-pr .pr-chip')).toHaveCount(0);
    const display = await page.locator('.main-header #header-pr').evaluate(el => getComputedStyle(el).display);
    expect(display).toBe('none');
  });
});
