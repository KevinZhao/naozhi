// E2E: the Planner page's header on a phone with everything in it at once
// (v0.7.0 integration review): the machine switch of D97's two Planners,
// "要你决定 N" (§15.12 ⑤), "N 件新结果" (§15.12 ④) and 已暂停. It keeps
// 要你决定 and N 件新结果 whole, the switch at least its first letters (it
// stands in for the title "Planner"), 已暂停 without its until time; none
// of them runs under 工作台 or pushes "⋯" off the screen. Screenshots go to
// test-results, and to $NZ_SHOTS_DIR when set.
const fs = require('fs');
const path = require('path');
const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const SHOTS = process.env.NZ_SHOTS_DIR || '';
const PLANNER = 'planner:main';
const NODE = 'ec2';
const now = Date.now();
const iso = (ms) => new Date(ms).toISOString();

const ask = (id, title, ago) => ({
  id, kind: 'question', level: 'timely', unit: PLANNER, title, why: 'Planner 问你', created: iso(now - ago),
  state: 'open', source: 'ask:' + id, decision: { handler: 'ask', actions: ['好', '先别'] },
});

// inbox has the Planner's questions and a pause until tomorrow 18:00.
function inbox(asks) {
  const until = new Date(now + 86400000);
  until.setHours(18, 0, 0, 0);
  const open = asks.map(([id, title], i) => ask(id, title, (i + 2) * 60000));
  return { open, closed: [], counts: { open: open.length, badge: open.length }, paused_until: until.toISOString() };
}

function tasks(n) {
  return { tasks: Array.from({ length: n }, (_, i) => ({
    id: 'task:new00000000' + i, kind: 'session', title: 'iso 新结果 ' + i, state: 'done', by: 'planner', at: iso(now - (i + 1) * 60000),
    created: iso(now - 3600000), new: true, result_at: iso(now - (i + 1) * 60000),
  })) };
}

// events asks the user the questions at the conversation's end.
const events = (asks) => [
  { type: 'user', detail: 'iso 今天的事', time: now - 600000, uuid: 'ph-u1' },
  { type: 'text', detail: 'iso 有两件事要你拿主意。\n\n' + asks.map(([, q]) => '```naozhi-action\n' + JSON.stringify({ type: 'ask', question: q, options: ['好', '先别'] }) + '\n```').join('\n\n'), time: now - 590000, uuid: 'ph-t1' },
];

async function shot(page, name) {
  const file = test.info().outputPath(name + '.png');
  await page.screenshot({ path: file });
  if (SHOTS) {
    fs.mkdirSync(SHOTS, { recursive: true });
    fs.copyFileSync(file, path.join(SHOTS, name + '.png'));
  }
}

// layout is the header's visible items, left to right, with their boxes and
// whether their text overflows them.
const layout = (head) => head.evaluate((el) => [...el.children]
  .filter((c) => c.getClientRects().length && getComputedStyle(c).visibility !== 'hidden')
  .map((c) => {
    const r = c.getBoundingClientRect();
    return { id: c.id || c.className, left: r.left, right: r.right, cut: c.scrollWidth > c.clientWidth + 1 };
  }));

for (const width of [390, 360]) {
  test(`phone ${width}: the header with the switch, 要你决定, 新结果 and 已暂停`, async ({ browser }) => {
    const ctx = await browser.newContext({ viewport: { width, height: 844 }, isMobile: true, hasTouch: true });
    const page = await ctx.newPage();
    const s = defaultSessions();
    s.nodes = { local: { display_name: 'Mac', status: 'ok' }, [NODE]: { display_name: 'EC2', status: 'ok', caps: ['files', 'planner', 'subscribe-history'] } };
    const asks = [['in_a1', 'iso 今天发 v0.7.0 吗？'], ['in_a2', 'iso 周报先发给谁？']];
    const remote = { planner: { enabled: true, stopped: false, key: PLANNER, alive: true }, tasks: tasks(12), inbox: inbox(asks),
      custody: { paused: true, sessions: [] }, events: { [PLANNER]: events(asks) } };
    const mock = await startMockServer({
      ws: true, sessions: s, tasks: tasks(2), plannerEvents: events(asks), inbox: inbox(asks),
      planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
      custody: { paused: true, sessions: [] }, remote: { [NODE]: remote },
    });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');
      await page.click('#abnav-planner');
      const head = page.locator('#planner-main .pl-center > .pl-head');
      for (const [node, news] of [['local', '2 件新结果'], [NODE, '12 件新结果']]) {
        if (node !== 'local') await page.locator('#pl-host select').selectOption(node);
        await expect(page.locator('#pl-host select')).toHaveValue(node);
        await expect(page.locator('#pl-pin')).toHaveText('要你决定 2');
        await expect(page.locator('#pl-pin')).toBeVisible();
        await expect(page.locator('#pl-news [data-action="tasks-only-new"]')).toHaveText(news);
        await expect(page.locator('#pl-paused')).toBeVisible();
        // The switch stands in for the title; the pin says "决定 2", the
        // pause no time (its title has it).
        await expect(head.locator('.pl-title')).toBeHidden();
        expect(await page.locator('#pl-pin').evaluate((el) => el.innerText.trim())).toBe('决定 2');
        expect(await page.locator('#pl-paused > span:not([aria-hidden])').evaluate((el) => el.innerText.trim())).toBe('已暂停');
        await expect(page.locator('#pl-paused')).toHaveAttribute('title', /推送与托管已暂停，到 /);

        const items = await layout(head);
        const [sw, cw] = await head.evaluate((el) => [el.scrollWidth, el.clientWidth]);
        expect(sw, JSON.stringify(items)).toBeLessThanOrEqual(cw);
        for (let i = 1; i < items.length; i++) {
          expect(items[i - 1].right, JSON.stringify(items)).toBeLessThanOrEqual(items[i].left + 0.5);
        }
        const byId = Object.fromEntries(items.map((it) => [it.id, it]));
        // What is acted on is whole; the switch keeps its first letters.
        expect(byId['pl-pin'].cut).toBe(false);
        expect(byId['pl-news'].cut).toBe(false);
        expect(byId['pl-host'].right - byId['pl-host'].left).toBeGreaterThanOrEqual(56);
        const more = await page.locator('#pl-more > summary').boundingBox();
        expect(more.x + more.width).toBeLessThanOrEqual(width);
        await shot(page, 'planner-head-phone-' + width + '-' + node);
      }
      // Paused alone, the header keeps the time whole (planner_page.test.js).
      await page.locator('#pl-host select').selectOption('local');
      // Its pin and news still count: tapping them works.
      await page.locator('#pl-news [data-action="tasks-only-new"]').click();
      await expect(page.locator('#planner-main')).toHaveClass(/pl-left-open/);
    } finally {
      await ctx.close();
      await mock.server.close();
    }
  });
}

// One Planner, no switch: the title stays, "决定 N" beside N 件新结果,
// 已暂停 without its time; nothing overlaps.
for (const width of [390, 360]) {
  test(`phone ${width}: one Planner's header with 要你决定, 新结果 and 已暂停`, async ({ browser }) => {
    const ctx = await browser.newContext({ viewport: { width, height: 844 }, isMobile: true, hasTouch: true });
    const page = await ctx.newPage();
    const asks = [['in_a1', 'iso 今天发 v0.7.0 吗？'], ['in_a2', 'iso 周报先发给谁？']];
    const mock = await startMockServer({
      ws: true, tasks: tasks(12), plannerEvents: events(asks), inbox: inbox(asks),
      planner: { enabled: true, stopped: false, key: PLANNER, alive: true }, custody: { paused: true, sessions: [] },
    });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');
      await page.click('#abnav-planner');
      const head = page.locator('#planner-main .pl-center > .pl-head');
      await expect(page.locator('#pl-news [data-action="tasks-only-new"]')).toHaveText('12 件新结果');
      await expect(page.locator('#pl-pin')).toBeVisible();
      await expect(page.locator('#pl-paused')).toBeVisible();
      await expect(page.locator('#pl-host')).toBeHidden();
      await expect(head.locator('.pl-title')).toBeVisible();
      expect(await page.locator('#pl-pin').evaluate((el) => el.innerText.trim())).toBe('决定 2');
      expect(await page.locator('#pl-paused > span:not([aria-hidden])').evaluate((el) => el.innerText.trim())).toBe('已暂停');
      const items = await layout(head);
      const [sw, cw] = await head.evaluate((el) => [el.scrollWidth, el.clientWidth]);
      expect(sw, JSON.stringify(items)).toBeLessThanOrEqual(cw);
      for (let i = 1; i < items.length; i++) {
        expect(items[i - 1].right, JSON.stringify(items)).toBeLessThanOrEqual(items[i].left + 0.5);
      }
      const byId = Object.fromEntries(items.map((it) => [it.id, it]));
      expect(byId['pl-pin'].cut).toBe(false);
      expect(byId['pl-news'].cut).toBe(false);
      const more = await page.locator('#pl-more > summary').boundingBox();
      expect(more.x + more.width).toBeLessThanOrEqual(width);
      await shot(page, 'planner-head-phone-' + width + '-one');
    } finally {
      await ctx.close();
      await mock.server.close();
    }
  });
}

// Desktop: the same header has room for all of it in full.
test('desktop: the header with the switch, 要你决定, 新结果 and 已暂停 in full', async ({ browser }) => {
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const page = await ctx.newPage();
  const s = defaultSessions();
  s.nodes = { local: { display_name: 'Mac', status: 'ok' }, [NODE]: { display_name: 'EC2', status: 'ok', caps: ['files', 'planner', 'subscribe-history'] } };
  const asks = [['in_a1', 'iso 今天发 v0.7.0 吗？'], ['in_a2', 'iso 周报先发给谁？']];
  const mock = await startMockServer({
    ws: true, sessions: s, tasks: tasks(2), plannerEvents: events(asks), inbox: inbox(asks),
    planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
    custody: { paused: true, sessions: [] },
    remote: { [NODE]: { planner: { enabled: true, stopped: false, key: PLANNER, alive: true }, tasks: tasks(1), inbox: inbox([]), custody: { paused: false, sessions: [] }, events: { [PLANNER]: [] } } },
  });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click('#abnav-planner');
    const head = page.locator('#planner-main .pl-center > .pl-head');
    await expect(head.locator('.pl-title')).toBeVisible();
    expect(await page.locator('#pl-pin').evaluate((el) => el.innerText.trim())).toBe('要你决定 2');
    await expect(page.locator('#pl-paused > span:not([aria-hidden])')).toHaveText(/^已暂停到 /);
    await expect(page.locator('#pl-news .pl-news-all')).toBeVisible();
    await shot(page, 'planner-head-desktop');
  } finally {
    await ctx.close();
    await mock.server.close();
  }
});
