// E2E: the v0.4.3 interface leftovers (planner RFC §15.8, D92). Names, not
// IDs: an inbox card about a unit no list holds names it by the health
// table's name or its kind (定时任务, 派出的任务) and its workspace by name; a
// dispatch card written with a workspace ID names the workspace from GET
// /api/planner; a dispatched session opened that no list names is 派出的任务
// in the breadcrumb. The sidebar's second line of a dispatched session is
// what it was dispatched for, never the reply's head. The CLI's api_retry
// line is not in the Planner conversation and reads 接口重试 in a session.
// The Planner conversation pages back as it is scrolled to the top, with
// the journaled notes and the briefs of the older span in place.
const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const PLANNER = 'planner:main';
const TASK = 'task:0a1b2c3d4e5f';
const RENAMED = 'task:1a2b3c4d5e6f';
const GONE = 'task:9f8e7d6c5b4a';
const WS = 'ws_abcd1234';
const now = Date.now();
const iso = (ms) => new Date(ms).toISOString();
const VIEWPORTS = [
  { name: 'desktop', viewport: { width: 1440, height: 900 } },
  { name: 'phone', viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true },
];

function sessions() {
  const s = defaultSessions();
  const task = { state: 'ready', platform: 'task', agent: 'general', cli_name: 'claude', purpose: 'task', workspace: '/home/user/workspace/myproject', project: 'myproject', node: 'local' };
  s.sessions.push(
    { ...task, key: TASK, user_label: 'iso 写 hello', purpose_title: 'iso 写 hello', last_response: '/home/user/workspace/myproject/hello.md 已写好', last_active: now - 10000 },
    { ...task, key: RENAMED, user_label: 'iso 你改的名字', purpose_title: 'iso 整理周报', last_response: '/home/user/workspace/myproject/weekly.md 已更新', last_active: now - 20000 },
  );
  return s;
}

function inbox() {
  return {
    open: [
      { id: 'in_cron', kind: 'error', level: 'timely', unit: 'cron:nightly', title: 'iso 构建失败', created: iso(now - 3600000), state: 'open' },
      { id: 'in_cron2', kind: 'error', level: 'timely', unit: 'cron:weekly', title: 'iso 周任务失败', created: iso(now - 3000000), state: 'open' },
      { id: 'in_task', kind: 'error', level: 'timely', unit: GONE, ws: WS, title: 'iso 会话失败', created: iso(now - 600000), state: 'open' },
    ],
    closed: [],
    counts: { open: 3, badge: 3 },
  };
}

test('names, not IDs: inbox cards, the dispatch card, the breadcrumb and the sidebar', async ({ browser }) => {
  for (const vp of VIEWPORTS) {
    const ctx = await browser.newContext({ viewport: vp.viewport, isMobile: !!vp.isMobile, hasTouch: !!vp.hasTouch });
    const page = await ctx.newPage();
    const dispatch = { type: 'dispatch', workspace: WS, title: 'iso 写 hello', model: '', message: '在工作区里写一个 hello.md' };
    const mock = await startMockServer({
      ws: true, inbox: inbox(), sessions: sessions(),
      planner: { enabled: true, stopped: false, key: PLANNER, alive: true, workspaces: { [WS]: 'iso-scratch' } },
      systemHealth: { units: [{ id: 'cron:nightly', kind: 'cron', state: 'failed', name: 'iso 夜间构建' }] },
      plannerEvents: [
        { type: 'text', detail: '派一个会话去写。\n\n```naozhi-action\n' + JSON.stringify(dispatch) + '\n```', time: now - 5000, uuid: 'pl-t1' },
        { type: 'system', summary: 'api_retry', time: now - 4000, uuid: 'pl-s1' },
        { type: 'text', detail: 'iso 旧任务（`' + GONE + '`）写完了。', time: now - 3000, uuid: 'pl-t2' },
      ],
      keyEvents: {
        [GONE]: [
          { type: 'user', detail: '写一个 hello.md', time: now - 9000, uuid: 'g-u1' },
          { type: 'system', summary: 'api_retry', time: now - 8000, uuid: 'g-s1' },
          { type: 'text', detail: '写好了。', time: now - 7000, uuid: 'g-t1' },
        ],
      },
    });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');
      // The sidebar: a dispatched session's second line is what it was
      // dispatched for, left out where its title already says it.
      if (vp.name === 'desktop') {
        const list = page.locator('#session-list');
        await expect(list.locator('.session-card[data-key="' + RENAMED + '"] .sc-response')).toHaveText('iso 整理周报');
        await expect(list.locator('.session-card[data-key="' + TASK + '"] .sc-response')).toHaveCount(0);
        await expect(list).not.toContainText('/home/user/workspace/myproject/');
        await list.screenshot({ path: test.info().outputPath('leftovers-sidebar.png') });
      }

      await page.click('#abnav-planner');
      const stream = page.locator('#pl-stream');
      // The dispatch card names the workspace the Planner wrote by its ID.
      const card = stream.locator('.pl-act-dispatch');
      await expect(card.locator('dt').first()).toHaveText('工作区');
      await expect(card.locator('dd').first()).toHaveText('iso-scratch');
      // The CLI's retry line is not part of the conversation.
      await expect(stream.locator('.event')).toHaveCount(2);
      await expect(stream).not.toContainText('api_retry');
      await expect(stream).not.toContainText('接口重试');
      await expect(page.locator('#pl-center')).not.toContainText(WS);

      // Inbox cards about units no list holds: the health table's name, else
      // the kind; the workspace by name.
      if (vp.isMobile) await page.click('#pl-left-toggle');
      const wait = page.locator('#pl-wait');
      await expect(wait.locator('.pl-card[data-id="in_cron"] .pl-card-meta')).toHaveText('iso 夜间构建');
      await expect(wait.locator('.pl-card[data-id="in_cron2"] .pl-card-meta')).toHaveText('定时任务');
      await expect(wait.locator('.pl-card[data-id="in_task"] .pl-card-meta')).toHaveText('iso-scratch · 派出的任务');
      for (const raw of ['cron:nightly', 'cron:weekly', GONE, WS]) await expect(wait).not.toContainText(raw);
      await page.screenshot({ path: test.info().outputPath('leftovers-inbox-' + vp.name + '.png') });
      if (vp.isMobile) await page.click('#pl-left-toggle');

      // A dispatched session no list names: 派出的任务 in the breadcrumb, its
      // retry line in words.
      await stream.locator('.pl-scard[data-key="' + GONE + '"]').click();
      await expect(page.locator('#pl-sess')).toBeVisible();
      await expect(page.locator('#pl-sess-name')).toHaveText('派出的任务');
      await expect(page.locator('#pl-sess-stream .event.system')).toHaveText(/接口重试/);
      await expect(page.locator('#pl-sess')).not.toContainText(GONE);
      await page.screenshot({ path: test.info().outputPath('leftovers-session-' + vp.name + '.png') });
    } finally {
      await ctx.close();
      mock.server.close();
    }
  }
});

test('the Planner conversation loads older pages at its top', async ({ browser }) => {
  const t0 = now - 600000;
  const at = (i) => t0 + i * 1000;
  for (const vp of VIEWPORTS) {
    const ctx = await browser.newContext({ viewport: vp.viewport, isMobile: !!vp.isMobile, hasTouch: !!vp.hasTouch });
    const page = await ctx.newPage();
    // 260 events: the first page holds the newest 200.
    const events = [];
    for (let i = 0; i < 260; i++) events.push({ type: 'text', detail: 'iso 第 ' + i + ' 条', time: at(i), uuid: 'pl-' + i });
    const day = (ms) => { const d = new Date(ms); return d.getFullYear() + '-' + String(d.getMonth() + 1).padStart(2, '0') + '-' + String(d.getDate()).padStart(2, '0'); };
    const mock = await startMockServer({
      ws: true, inbox: { open: [], closed: [], counts: { open: 0, badge: 0 } },
      planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
      plannerEvents: events,
      plannerNotes: [{ time: iso(at(5) + 500), text: '到点提醒（承诺，到期 9:00）：iso 更早的提醒' }],
      plannerBriefs: [{ day: day(at(10)), at: iso(at(10) + 500), sent: true, counts: {}, markdown: '自 iso 以来：完成 1\n' }],
    });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');
      await page.click('#abnav-planner');
      const stream = page.locator('#pl-stream');
      await expect(stream.locator('.event.text')).toHaveCount(200);
      await expect(stream.locator('.event.text').first()).toHaveText(/iso 第 60 条/);
      // Neither the older note nor the brief belongs to this page yet.
      await expect(stream).not.toContainText('iso 更早的提醒');
      const content = (i) => stream.locator('.event.text .event-content', { hasText: new RegExp('^iso 第 ' + i + ' 条$') });
      const head = content(60);

      await page.locator('#pl-events').evaluate((el) => { el.scrollTop = 0; });
      await expect(stream.locator('.event.text')).toHaveCount(260);
      await expect(stream.locator('.event.text').first()).toHaveText(/iso 第 0 条/);
      // The reader stays on what they were reading.
      await expect(head).toBeInViewport();
      // The older span's note and brief sit where they were made.
      const order = await stream.locator(':scope > .event, :scope > .pl-brief').evaluateAll((els) => els.map((e) =>
        e.classList.contains('pl-brief') ? 'brief' : e.classList.contains('system') ? 'note' : (e.querySelector('.event-content')?.textContent || '').trim()));
      expect(order.indexOf('note')).toBe(order.indexOf('iso 第 5 条') + 1);
      expect(order.indexOf('brief')).toBe(order.indexOf('iso 第 10 条') + 1);
      await expect(stream.locator('.event.system')).toHaveText(/iso 更早的提醒/);
      await expect(content(5)).toHaveCount(1);
      await page.screenshot({ path: test.info().outputPath('leftovers-older-' + vp.name + '.png') });
    } finally {
      await ctx.close();
      mock.server.close();
    }
  }
});

// A brief older than the first page, opened by its push link: the pages
// loaded above it as it sits at the stream's top keep it in view and lit.
test('a brief link to before the first page keeps the brief in view as older pages load', async ({ browser }) => {
  const t0 = now - 260 * 600000;
  const at = (i) => t0 + i * 600000;
  const day = (ms) => { const d = new Date(ms); return d.getFullYear() + '-' + String(d.getMonth() + 1).padStart(2, '0') + '-' + String(d.getDate()).padStart(2, '0'); };
  for (const vp of VIEWPORTS) {
    const ctx = await browser.newContext({ viewport: vp.viewport, isMobile: !!vp.isMobile, hasTouch: !!vp.hasTouch });
    const page = await ctx.newPage();
    const events = [];
    for (let i = 0; i < 260; i++) events.push({ type: 'text', detail: 'iso 第 ' + i + ' 条', time: at(i), uuid: 'pl-' + i });
    const d = day(at(10) + 500);
    const mock = await startMockServer({
      ws: true, inbox: { open: [], closed: [], counts: { open: 0, badge: 0 } },
      planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
      plannerEvents: events,
      plannerBriefs: [{ day: d, at: iso(at(10) + 500), sent: true, counts: {}, markdown: '自 iso 以来：完成 1\n' }],
    });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');
      await page.evaluate((x) => { location.hash = 'planner&item=brief:' + x; }, d);
      const stream = page.locator('#pl-stream');
      await expect(stream.locator('.event.text')).toHaveCount(260);
      const brief = stream.locator(':scope > .pl-brief');
      const order = await stream.locator(':scope > .event, :scope > .pl-brief').evaluateAll((els) => els.map((e) =>
        e.classList.contains('pl-brief') ? 'brief' : (e.querySelector('.event-content')?.textContent || '').trim()));
      expect(order.indexOf('brief')).toBe(order.indexOf('iso 第 10 条') + 1);
      await expect(brief).toHaveAttribute('open', '');
      await expect(brief.locator('summary')).toBeInViewport();
      await expect(brief).toHaveClass(/inbox-lit/);
      await page.screenshot({ path: test.info().outputPath('leftovers-brief-link-' + vp.name + '.png') });
    } finally {
      await ctx.close();
      mock.server.close();
    }
  }
});
