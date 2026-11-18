// E2E: the settings view's 懂你 section (planner RFC §15.15 ⑦, D126). Every
// project's auto-memory on a node, grouped (the Planner's own, registered
// workspaces, others, temporary ones folded), the last 7 days on top, a
// filter; an entry opens rendered, edits whole with the version it was
// opened at (a 409 loads the latest and keeps the draft, which a theme click
// does not drop either), deletes after 5 s with 撤销 (a refused delete puts
// the row back). Another node's memories are read through /api/remote and
// are read-only (decision B); a node too old to serve them says so. The 记忆
// kind of the asset browser and the Planner page link here.
const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const NODE = 'pc';
const now = Date.now();
const iso = (ms) => new Date(ms).toISOString();
const DAY = 86400000;

function memories() {
  const styleText = '---\nname: 回复风格\ndescription: 先结论\nmetadata:\n  type: feedback\n  source: naozhi-planner\n  remembered_at: ' + iso(now - 3600000) + '\n---\n\n回复**先结论**，再细节。\n';
  const azText = '---\nname: 法兰克福可用区\ndescription: 用 euc1-az2\ntype: project\n---\n\n别用 az1，容量不够。\n';
  return {
    writable: true,
    groups: [
      { project: '-Users-me--naozhi-planner-main', label: 'Planner 自己的', kind: 'planner', workspace_id: 'global', modified: iso(now - 3600000),
        entries: [{ name: 'reply-style', title: '回复风格', type: 'feedback', description: '先结论', snippet: '回复先结论，再细节。', source: 'naozhi-planner', remembered_at: iso(now - 3600000), modified: iso(now - 3600000), size: 120, version: 'v1', indexed: true }] },
      { project: '-Users-me-workspace-naozhi', label: 'naozhi', kind: 'workspace', workspace_id: 'ws_aaaaaaaa', workspace_name: 'naozhi', modified: iso(now - 2 * DAY),
        index: { lines: 3, size: 90, modified: iso(now - 20 * DAY) }, dangling: 1,
        entries: [
          { name: 'frankfurt-az', title: '法兰克福可用区', type: 'project', description: '用 euc1-az2', snippet: '别用 az1，容量不够。', modified: iso(now - 2 * DAY), size: 80, version: 'v1', indexed: false },
          { name: 'old-thing', title: '很久以前', type: 'reference', description: '旧的', snippet: '旧', modified: iso(now - 30 * DAY), size: 20, version: 'v1', indexed: true },
        ] },
      { project: '-private-tmp-iso1', label: '-private-tmp-iso1', kind: 'temp', modified: iso(now - 40 * DAY),
        entries: [{ name: 't', title: '临时', snippet: 't', modified: iso(now - 40 * DAY), size: 2, version: 'v1', indexed: true }] },
    ],
    files: {
      '-Users-me--naozhi-planner-main/reply-style': { project: '-Users-me--naozhi-planner-main', name: 'reply-style', title: '回复风格', source: 'naozhi-planner', text: styleText, body: '回复**先结论**，再细节。\n', version: 'v1', modified: iso(now - 3600000), size: styleText.length },
      '-Users-me-workspace-naozhi/frankfurt-az': { project: '-Users-me-workspace-naozhi', name: 'frankfurt-az', title: '法兰克福可用区', text: azText, body: '别用 az1，容量不够。\n', version: 'v1', modified: iso(now - 2 * DAY), size: azText.length },
      '-Users-me-workspace-naozhi/MEMORY': { project: '-Users-me-workspace-naozhi', name: 'MEMORY', text: '- [很久以前](old-thing.md)\n', body: '- [很久以前](old-thing.md)\n', version: 'v1', modified: iso(now - 20 * DAY), size: 30 },
    },
  };
}

async function openSettings(page) {
  await page.click('#abnav-settings');
  await expect(page.locator('#settings-memory .mem-grp').first()).toBeVisible();
}

const row = (page, name) => page.locator('#settings-memory .mem-grp .mem-row[data-name="' + name + '"]');

test('groups, the last 7 days and the filter', async ({ page }) => {
  const mock = await startMockServer({ memories: memories() });
  try {
    await page.goto(mock.url + '/dashboard');
    await openSettings(page);
    const sec = page.locator('#settings-memory');
    await expect(sec.locator('h2')).toHaveText('懂你');
    await expect(sec.locator('.mem-ro')).toBeHidden();
    // The section sits under 主题 (the phone's 系统 row above it is hidden here).
    const order = await page.locator('#settings-main .settings-sec:visible').evaluateAll((els) => els.map((e) => e.id || e.querySelector('h2')?.textContent));
    expect(order.slice(0, 2)).toEqual(['主题', 'settings-memory']);
    const heads = sec.locator('.mem-grp-h .mem-grp-l');
    await expect(heads).toHaveText([/^Planner 自己的 · 1 条/, /^naozhi · 2 条 · 索引 3 行/, /^-private-tmp-iso1 · 1 条/]);
    await expect(sec.locator('.mem-grp').nth(1)).toContainText('索引里 1 行指向不存在的文件');
    await expect(sec.locator('.mem-grp').nth(2)).not.toHaveAttribute('open', /.*/);
    await expect(row(page, 'frankfurt-az')).toContainText('没进索引');
    await expect(row(page, 'frankfurt-az')).toContainText('项目');
    await expect(row(page, 'reply-style')).toContainText('Planner 记的');
    await expect(row(page, 'reply-style')).toContainText('偏好');
    // 最近 7 天: the two entries written this week, newest first.
    await expect(sec.locator('.mem-recent .mem-row .mem-title')).toHaveText(['回复风格', '法兰克福可用区']);

    await sec.locator('.mem-filter').fill('az1');
    await expect(row(page, 'frankfurt-az')).toBeVisible();
    await expect(row(page, 'old-thing')).toBeHidden();
    await expect(sec.locator('.mem-grp').first()).toBeHidden();
    await sec.locator('.mem-filter').fill('');
    await expect(row(page, 'old-thing')).toBeVisible();

    // A 最近 7 天 row opens the entry where it lives.
    await sec.locator('.mem-recent .mem-row[data-name="reply-style"] .mem-row-h').click();
    await expect(row(page, 'reply-style').locator('.mem-md strong')).toHaveText('先结论');
  } finally {
    mock.server.close();
  }
});

test('edit: the version goes with the save, a 409 keeps the draft, a theme click too', async ({ page }) => {
  const data = memories();
  const mock = await startMockServer({ memories: data });
  try {
    await page.goto(mock.url + '/dashboard');
    await openSettings(page);
    const r = row(page, 'frankfurt-az');
    await r.locator('.mem-row-h').click();
    await expect(r.locator('.mem-md')).toContainText('别用 az1');
    await r.locator('[data-mem="edit"]').click();
    const ta = r.locator('textarea.mem-edit');
    await expect(ta).toHaveValue(/别用 az1/);
    await ta.fill('---\nname: 法兰克福可用区\n---\n\n用 az3。\n');

    // A theme click repaints the settings view around the section.
    await page.locator('.settings-theme-opt').nth(1).click();
    await expect(ta).toHaveValue(/用 az3/);
    // Leaving and coming back does not re-read over the draft.
    await page.click('#abnav-chat');
    await page.click('#abnav-settings');
    await expect(r.locator('textarea.mem-edit')).toHaveValue(/用 az3/);

    // A CLI session wrote it meanwhile.
    data.files['-Users-me-workspace-naozhi/frankfurt-az'] = { ...data.files['-Users-me-workspace-naozhi/frankfurt-az'], text: '会话写的新内容\n', body: '会话写的新内容\n', version: 'v7', modified: iso(Date.now()) };
    await r.locator('[data-mem="save"]').click();
    await expect(r.locator('.mem-note')).toContainText('这条在你打开后被改过');
    await expect(r.locator('.mem-latest')).toHaveText('会话写的新内容\n');
    await expect(r.locator('textarea.mem-edit')).toHaveValue(/用 az3/);
    expect(mock.memoryCalls.map((c) => c.method + ' ' + c.version)).toEqual(['PUT v1']);

    await r.locator('[data-mem="save"]').click();
    await expect(r.locator('.mem-note')).toHaveText('已保存');
    await expect(r.locator('.mem-md')).toContainText('用 az3。');
    expect(mock.memoryCalls.map((c) => c.method + ' ' + c.version)).toEqual(['PUT v1', 'PUT v7']);
    expect(mock.memoryCalls[1].text).toBe('---\nname: 法兰克福可用区\n---\n\n用 az3。\n');

    // MEMORY.md edits but does not delete.
    const idx = row(page, 'MEMORY');
    await idx.locator('.mem-row-h').click();
    await expect(idx.locator('[data-mem="edit"]')).toBeVisible();
    await expect(idx.locator('[data-mem="delete"]')).toHaveCount(0);
  } finally {
    mock.server.close();
  }
});

test('delete: 撤销 within 5 s sends nothing; otherwise one DELETE', async ({ page }) => {
  const mock = await startMockServer({ memories: memories() });
  try {
    await page.goto(mock.url + '/dashboard');
    await openSettings(page);
    const r = row(page, 'frankfurt-az');
    await r.locator('.mem-row-h').click();
    await r.locator('[data-mem="delete"]').click();
    await expect(r).toBeHidden();
    const toast = page.locator('#mem-undo-toast');
    await expect(toast).toContainText('已删除「法兰克福可用区」');
    await toast.locator('button').click();
    await expect(r).toBeVisible();
    await page.waitForTimeout(5500);
    expect(mock.memoryCalls).toEqual([]);

    await r.locator('[data-mem="delete"]').click();
    await expect(r).toBeHidden();
    await expect.poll(() => mock.memoryCalls.length, { timeout: 8000 }).toBe(1);
    expect(mock.memoryCalls[0]).toMatchObject({ method: 'DELETE', project: '-Users-me-workspace-naozhi', name: 'frankfurt-az', version: 'v1' });
    await expect(r).toHaveCount(0);
    await expect(page.locator('#settings-memory .mem-grp').nth(1).locator('.mem-grp-l')).toHaveText(/^naozhi · 1 条/);
    await page.waitForTimeout(500);
    expect(mock.memoryCalls.length).toBe(1);
  } finally {
    mock.server.close();
  }
});

test('delete: a 409 puts the row back with the latest, and the next delete names it', async ({ page }) => {
  const data = memories();
  const mock = await startMockServer({ memories: data });
  try {
    await page.goto(mock.url + '/dashboard');
    await openSettings(page);
    const r = row(page, 'reply-style');
    await r.locator('.mem-row-h').click();
    await expect(r.locator('.mem-md')).toContainText('再细节');
    // A CLI session rewrites it after the page opened it.
    const f = data.files['-Users-me--naozhi-planner-main/reply-style'];
    f.version = 'v9';
    f.text = f.text.replace('再细节', '再给例子');
    f.body = '回复**先结论**，再给例子。\n';
    await r.locator('[data-mem="delete"]').click();
    await expect(r).toBeHidden();
    await expect.poll(() => mock.memoryCalls.length, { timeout: 8000 }).toBe(1);
    expect(mock.memoryCalls[0].version).toBe('v1');
    await expect(r).toBeVisible();
    await expect(page.locator('#toast')).toHaveText('这条刚被改过，没有删');
    await expect(r.locator('.mem-md')).toContainText('再给例子');
    // Collapsed and opened again it is still the latest, not the cached one.
    await r.locator('.mem-row-h').click();
    await r.locator('.mem-row-h').click();
    await expect(r.locator('.mem-md')).toContainText('再给例子');
    await r.locator('[data-mem="delete"]').click();
    await expect.poll(() => mock.memoryCalls.length, { timeout: 8000 }).toBe(2);
    expect(mock.memoryCalls[1].version).toBe('v9');
    await expect(page.locator('#settings-memory .mem-row[data-name="reply-style"]')).toHaveCount(0);
  } finally {
    mock.server.close();
  }
});

function remoteSessions() {
  const s = defaultSessions();
  s.nodes = {
    local: { display_name: 'EC2', status: 'ok' },
    [NODE]: { display_name: 'MBP', status: 'ok', caps: ['planner'] },
  };
  return s;
}

const VIEWPORTS = [
  { name: 'desktop', width: 1280, height: 800 },
  { name: 'phone', width: 390, height: 844, isMobile: true, hasTouch: true },
];

for (const vp of VIEWPORTS) {
  test(vp.name + ': another node is read through /api/remote and read-only, whatever it says', async ({ browser }) => {
    const ctx = await browser.newContext({ viewport: { width: vp.width, height: vp.height }, isMobile: !!vp.isMobile, hasTouch: !!vp.hasTouch });
    const page = await ctx.newPage();
    // writable:true as a node reached over a loopback URL answers: the
    // page stays read-only, the primary does not forward a write.
    const remote = memories();
    expect(remote.writable).toBe(true);
    const mock = await startMockServer({
      ws: true, sessions: remoteSessions(), planner: { enabled: false }, memories: { writable: true, groups: [], files: {} },
      remote: { [NODE]: { planner: { enabled: true, stopped: false, key: 'planner:main', alive: true }, memories: remote } },
    });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');
      await page.click('#abnav-settings');
      const sec = page.locator('#settings-memory');
      // The Planner page shows the node's Planner (this one's is off), so
      // the section starts there; this node stays one pick away.
      const pick = sec.locator('select.mem-node');
      await expect(pick).toBeVisible();
      await expect(pick.locator('option')).toHaveText(['本机', 'MBP']);
      await expect(pick).toHaveValue(NODE);
      await expect(sec.locator('.mem-ro')).toHaveText('只读：在那台机器本机用 localhost 打开 dashboard 才能改');
      await expect(sec.locator('.mem-intro')).toContainText('对 Planner 说"记下来"');
      const r = row(page, 'frankfurt-az');
      await r.locator('.mem-row-h').click();
      await expect(r.locator('.mem-md')).toContainText('别用 az1');
      await expect(r.locator('[data-mem="edit"]')).toHaveCount(0);
      await expect(r.locator('[data-mem="delete"]')).toHaveCount(0);
      const paths = mock.remoteCalls.filter((c) => c.node === NODE).map((c) => c.method + ' ' + c.path);
      expect(paths).toContain('GET /api/memories');
      expect(paths).toContain('GET /api/memories/file');

      await pick.selectOption('local');
      await expect(sec.locator('.mem-empty')).toHaveText('这台机器上还没有记忆');
      await expect(sec.locator('.mem-ro')).toBeHidden();
      await expect(sec.locator('.mem-intro')).toContainText('各项目会话的 auto-memory');
    } finally {
      mock.server.close();
      await ctx.close();
    }
  });
}

test('a node too old to serve 懂你 says so', async ({ page }) => {
  const mock = await startMockServer({
    ws: true, sessions: remoteSessions(), planner: { enabled: false },
    remote: { [NODE]: { planner: { enabled: true, stopped: false, key: 'planner:main', alive: true } } },
  });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click('#abnav-settings');
    await expect(page.locator('#settings-memory .mem-err')).toHaveText('那台机器的 naozhi 版本还不支持懂你，升级后可用');
  } finally {
    mock.server.close();
  }
});

test('entries: the 记下了 card, the Planner "⋯" menu and the asset browser', async ({ page }) => {
  const block = (a) => '```naozhi-action\n' + JSON.stringify(a) + '\n```';
  const mock = await startMockServer({
    ws: true,
    memories: memories(),
    planner: { enabled: true, stopped: false, key: 'planner:main', alive: true },
    plannerEvents: [{ type: 'text', time: now - 5000, uuid: 'mv-t1', detail: '好，记下了。\n\n' + block({ type: 'remember', workspace: 'global', name: 'reply-style', text: '回复先结论，再细节。' }) }],
  });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click('#abnav-planner');
    const card = page.locator('#pl-stream .pl-act-remember').first();
    await card.locator('summary').click();
    await card.locator('[data-action="memory-open"]').click();
    await expect(page.locator('#settings-main')).toBeVisible();
    const r = row(page, 'reply-style');
    await expect(r).toHaveClass(/mem-row-focus/);
    await expect(r.locator('.mem-md')).toContainText('再细节');

    await page.click('#abnav-planner');
    await page.locator('#pl-more > summary').click();
    await page.locator('#pl-tools [data-action="memory-open"]').click();
    await expect(page.locator('#settings-main')).toBeVisible();
    await expect(page.locator('#pl-more')).not.toHaveAttribute('open', /.*/);

    await page.click('#abnav-assets');
    await page.locator('#asset-kindtabs [data-k="memory"]').click();
    await page.locator('.asset-mem-open').click();
    await expect(page.locator('#settings-main')).toBeVisible();
    await expect(page.locator('#settings-memory .mem-grp').first()).toBeVisible();
  } finally {
    mock.server.close();
  }
});
