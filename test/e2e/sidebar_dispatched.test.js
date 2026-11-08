// E2E: the sessions the Planner dispatched (purpose task) in the session
// list. Each sits in its workspace's group like any other session, its card
// marked 派出; one with no project goes under 未分组. The chip atop the list,
// 只看 Planner 派出 N, narrows the list to them (empty favorite groups left
// out), is kept across a reload, and is not shown without a dispatched
// session. Desktop and phone.
const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');

const now = Date.now();
const MY = 'task:aaaa00000001';
const OTHER = 'task:aaaa00000002';
const LOOSE = 'task:aaaa00000003';

function task(key, title, workspace, project, ago) {
  return {
    key, state: 'ready', platform: 'task', agent: 'general', cli_name: 'claude', purpose: 'task',
    purpose_title: title, user_label: title, workspace, project, last_active: now - ago, created_at: now - ago,
    last_prompt: title, node: 'local',
  };
}

function sessions() {
  const s = defaultSessions();
  s.sessions.push(
    task(MY, 'iso 修 sidebar 分组', '/home/user/workspace/myproject', 'myproject', 3600000),
    task(OTHER, 'iso 跑 go vet', '/home/user/workspace/otherproject', 'otherproject', 120000),
    task(LOOSE, 'iso 无工作区', '', '', 60000),
  );
  return s;
}

// rows is the list as header / card tokens, top to bottom.
const rows = (list) => list.locator(':scope > *').evaluateAll((els) => els.map((e) =>
  e.classList.contains('session-card') ? 'C:' + e.dataset.key
    : e.classList.contains('section-header') ? 'H:' + (e.querySelector('.sh-name')?.textContent || '')
      : e.classList.contains('sb-filter') ? 'F' : '?'));

for (const vp of [{ name: 'desktop', width: 1280, height: 800 }, { name: 'phone', width: 390, height: 844, isMobile: true, hasTouch: true }]) {
  test(vp.name + ': dispatched sessions list in their workspace group, marked, with a filter', async ({ browser }) => {
    const ctx = await browser.newContext({ viewport: { width: vp.width, height: vp.height }, isMobile: !!vp.isMobile, hasTouch: !!vp.hasTouch });
    const page = await ctx.newPage();
    const mock = await startMockServer({ sessions: sessions() });
    try {
      await page.goto(mock.url + '/dashboard');
      const list = page.locator('#session-list');
      await expect(list.locator('.session-card[data-key="' + MY + '"]')).toHaveCount(1);
      // No group of their own; each under its workspace's header.
      await expect(list.locator('.section-header', { hasText: 'Planner 派出' })).toHaveCount(0);
      const all = await rows(list);
      const under = (key) => {
        const i = all.indexOf('C:' + key);
        for (let j = i; j >= 0; j--) if (all[j].startsWith('H:')) return all[j];
        return '';
      };
      expect(all[0]).toBe('F');
      expect(under(MY)).toBe('H:myproject');
      expect(under(OTHER)).toBe('H:otherproject');
      expect(under(LOOSE)).toBe('H:未分组');
      for (const k of [MY, OTHER, LOOSE]) {
        await expect(list.locator('.session-card[data-key="' + k + '"] .sc-origin.kind-planner')).toHaveText('派出');
      }
      await expect(list.locator('.session-card[data-key^="dashboard:"] .sc-origin.kind-planner')).toHaveCount(0);
      await page.screenshot({ path: test.info().outputPath('dispatched-' + vp.name + '.png') });

      // The filter: only the dispatched ones, the empty favorite left out.
      const chip = list.locator('.sb-filter-chip');
      await expect(chip).toHaveText('只看 Planner 派出 3');
      await expect(chip).toHaveAttribute('aria-pressed', 'false');
      await chip.click();
      await expect(chip).toHaveAttribute('aria-pressed', 'true');
      await expect(list.locator('.session-card')).toHaveCount(3);
      await expect(list.locator('.section-header', { hasText: 'pinned-empty' })).toHaveCount(0);
      expect(await rows(list)).toEqual(['F', 'H:myproject', 'C:' + MY, 'H:otherproject', 'C:' + OTHER, 'H:未分组', 'C:' + LOOSE]);
      await page.screenshot({ path: test.info().outputPath('dispatched-filter-' + vp.name + '.png') });

      // Kept across a reload; a second click shows everything again.
      await page.reload();
      await expect(list.locator('.sb-filter-chip')).toHaveAttribute('aria-pressed', 'true');
      await expect(list.locator('.session-card')).toHaveCount(3);
      await list.locator('.sb-filter-chip').click();
      await expect(list.locator('.session-card')).toHaveCount(6);
      await expect(list.locator('.section-header', { hasText: 'pinned-empty' })).toHaveCount(1);
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
}

test('no dispatched session: no filter chip, and a kept filter does not hide the list', async ({ browser }) => {
  const ctx = await browser.newContext();
  await ctx.addInitScript(() => localStorage.setItem('nz_sidebarOnlyDispatched', '1'));
  const page = await ctx.newPage();
  const mock = await startMockServer({});
  try {
    await page.goto(mock.url + '/dashboard');
    const list = page.locator('#session-list');
    await expect(list.locator('.session-card')).toHaveCount(3);
    await expect(list.locator('.sb-filter')).toHaveCount(0);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});
