// E2E: a remote session's workbench tabs read its node's files (planner RFC
// §15.5 ④): 文件 lists the session tree with node=, a file previews from the
// node, 产出 asks the node; a node whose version does not serve files says so
// and is never asked, so nothing is read from this host instead.
const { test, expect, devices } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');

const REMOTE = 'dashboard:direct:2026-01-02-090000-1:myproject';
const OLD = 'dashboard:direct:2026-01-02-090001-2:myproject';
const WS = '/home/ec2-user/workspace/myproject';

function remoteSessions() {
  const s = defaultSessions();
  const remote = (key, node, prompt) => ({
    key, node, state: 'ready', platform: 'dashboard', agent: 'general', cli_name: 'claude',
    workspace: WS, last_active: Date.now() - 1000, last_prompt: prompt, project: 'myproject',
  });
  s.sessions.push(remote(REMOTE, 'ec2', 'iso remote session'), remote(OLD, 'old', 'iso old node session'));
  s.nodes = {
    local: { display_name: 'Local', status: 'ok' },
    ec2: { display_name: 'EC2', status: 'ok', caps: ['files', 'subscribe-history'] },
    old: { display_name: 'Old', status: 'ok', caps: ['subscribe-history'] },
  };
  return s;
}

// routeNodeFiles answers the file API as the node would, recording each URL.
async function routeNodeFiles(page, calls) {
  await page.route('**/api/projects/files/list?*', (route) => {
    const u = new URL(route.request().url());
    calls.push(u.pathname + u.search);
    const dir = u.searchParams.get('dir') || '';
    route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify({ root_name: 'myproject', dir: dir || '.', entries: dir ? [{ name: 'notes.txt', size: 9 }] : [{ name: 'src', is_dir: true }, { name: 'README.md', size: 12 }] }),
    });
  });
  await page.route('**/api/projects/file?*', (route) => {
    const u = new URL(route.request().url());
    calls.push(u.pathname + u.search);
    route.fulfill({ contentType: 'application/json', body: JSON.stringify({ content: 'node copy of ' + u.searchParams.get('path'), size: 20, mime: 'text/plain' }) });
  });
  await page.route('**/api/sessions/outputs?*', (route) => {
    const u = new URL(route.request().url());
    calls.push(u.pathname + u.search);
    const key = u.searchParams.get('key');
    route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify({ outputs: [{ key, time: new Date().toISOString(), type: 'output', summary: 'out.md', path: 'out.md', dir: WS, root: 'session:' + key }] }),
    });
  });
}

const drawer = (page) => page.locator('#fv-drawer');
const tab = (page, name) => page.locator(`#fv-drawer .wb-tab[data-wb-tab="${name}"]`);

for (const [label, device] of [['desktop', null], ['phone', devices['iPhone 13']]]) {
  test(`${label}: a remote session's 文件 and 产出 read its node; an old node says it cannot`, async ({ browser }, testInfo) => {
    const ctx = await browser.newContext(device ? { ...device } : { viewport: { width: 1280, height: 800 } });
    const page = await ctx.newPage();
    const mock = await startMockServer({ sessions: remoteSessions() });
    const calls = [];
    await routeNodeFiles(page, calls);
    try {
      await page.goto(mock.url + '/dashboard');
      await page.click(`.session-card[data-key="${REMOTE}"][data-node="ec2"]`);
      await page.click('.main-header .btn-workbench');
      await expect(drawer(page)).toHaveClass(/fv-open/);

      // 产出 asks the node and previews from it.
      await tab(page, 'outputs').click();
      const out = page.locator('#wb-outputs .files-row');
      await expect(out).toHaveCount(1);
      expect(calls.some((c) => c.startsWith('/api/sessions/outputs?') && c.includes('node=ec2'))).toBe(true);
      await out.first().click();
      await expect(page.locator('#fv-body')).toContainText('node copy of out.md');

      // 文件 lists the session tree on the node; a file previews from there.
      await tab(page, 'files').click();
      await expect(page.locator('#wb-files-crumbs')).toHaveText('会话 · myproject');
      await page.locator('#wb-files-list .files-row[data-name="src"]').click();
      await page.locator('#wb-files-list .files-row[data-name="notes.txt"]').click();
      await expect(tab(page, 'preview')).toHaveAttribute('aria-selected', 'true');
      await expect(page.locator('#fv-body')).toContainText('node copy of src/notes.txt');
      const fileCalls = calls.filter((c) => c.startsWith('/api/projects/'));
      expect(fileCalls.length).toBeGreaterThanOrEqual(3);
      for (const c of fileCalls) {
        expect(c).toContain('node=ec2');
        expect(c).toContain('root=' + encodeURIComponent('session:' + REMOTE));
      }
      await page.screenshot({ path: testInfo.outputPath(`remote-files-${label}.png`) });

      // A node predating remote files: both tabs say so and nothing is fetched.
      const before = calls.length;
      await page.evaluate((k) => window.selectSession(k, 'old'), OLD);
      await tab(page, 'files').click();
      await expect(page.locator('#wb-files-list')).toHaveText('该节点版本不支持远程文件');
      await tab(page, 'outputs').click();
      await expect(page.locator('#wb-outputs')).toHaveText('该节点版本不支持远程文件');
      expect(calls.slice(before).filter((c) => c.includes('node=old'))).toEqual([]);
      await page.screenshot({ path: testInfo.outputPath(`remote-files-old-${label}.png`) });
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
}
