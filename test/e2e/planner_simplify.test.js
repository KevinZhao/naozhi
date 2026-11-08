// E2E: the Planner conversation keeps to what matters (planner RFC §15.12 ⑤,
// D103; UX review 9): a dispatch card folds to one line — 派出 · 标题 · 工作区
// · 目标 (its goal, else its message's first sentence) — with its rows in the
// fold; a status 进行中 leaves nothing in the conversation (a reply of only
// that is no empty bubble), done / failed one line with the reason in its
// tooltip; the gateway's report line "📨 <key> 汇报：…" reads by the
// session's name and opens its 任务 card, expanded and lit (on a phone in
// the unfolded left block; a session 任务 does not list opens in the middle
// column); a SendMessage from a session folds to its sender and first
// sentence on the Planner page only (peer_source.test.js: the 会话 view
// keeps it whole); the header pins "要你决定 N", the questions at the
// conversation's end, not red, also over an opened session and in the
// drawer; the desktop left column keeps one line for the Planner's own
// question, pointing there, while a phone and the 会话 sidebar keep the card.
// Jumping to a question focuses its first answer; a report line from the
// drawer goes to the page first, ends "只看新结果" for a card it hides and
// marks read a card with no digest; hidden empty replies double no robot
// and leave no divider over nothing; a goal is cut as the gateway cuts it.
// Screenshots of a crowded conversation go to test-results, and to
// $NZ_SHOTS_DIR when set.
const fs = require('fs');
const path = require('path');
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const PLANNER = 'planner:main';
const HELLO = 'task:aaaa1111';
const WEEKLY = 'task:bbbb2222';
const GONE = 'task:gone0000';
const now = Date.now();
const iso = (ms) => new Date(ms).toISOString();
const fence = (body) => '```naozhi-action\n' + JSON.stringify(body) + '\n```';

const GOAL_DISPATCH = { type: 'dispatch', workspace: 'ws_iso00001', title: 'iso 整理周报', goal: 'iso 周五前出一版周报初稿', message: '把本周合并的 PR 整理成周报，按模块分组。写在 docs/weekly.md。' };
// An old block, from before dispatch had a goal: its message's first sentence.
const OLD_DISPATCH = { type: 'dispatch', workspace: 'iso-scratch', title: 'iso 写 hello', message: '在工作区里写一个 hello.md。写完跑一下测试。' };
// A goal past the gateway's 60 characters, an emoji where it is cut, in a
// workspace written as a path.
const LONG_GOAL = '长'.repeat(59) + '😀😀';
const LONG_DISPATCH = { type: 'dispatch', workspace: '/Users/iso/work/proj-long', title: 'iso 长目标', goal: LONG_GOAL, message: '照做。' };

function plannerEvents() {
  return [
    { type: 'user', detail: 'iso 把周报和 hello 都安排一下', time: now - 600000, uuid: 'ps-u1' },
    { type: 'text', detail: '好，派三个会话：\n\n' + fence(GOAL_DISPATCH) + '\n\n' + fence(OLD_DISPATCH) + '\n\n' + fence(LONG_DISPATCH), time: now - 590000, uuid: 'ps-t1' },
    // Replies of only a 进行中: nothing to show, and the reply after them
    // goes on under the robot of the one before.
    { type: 'text', detail: fence({ type: 'status', task: HELLO, state: 'doing', why: 'iso 它开始写了' }), time: now - 580000, uuid: 'ps-t2' },
    { type: 'text', detail: fence({ type: 'status', task: WEEKLY, state: 'doing', why: 'iso 它也开始了' }), time: now - 575000, uuid: 'ps-t2b' },
    { type: 'text', detail: 'iso 周报那边开始了。\n\n' + fence({ type: 'status', task: WEEKLY, state: 'doing', why: 'iso 它在读 PR' }) + '\n\niso 有结果我告诉你。', time: now - 570000, uuid: 'ps-t3' },
    { type: 'system', summary: '📨 ' + HELLO + ' 汇报：完成 · iso hello.md 写好了，测试全部通过。', time: now - 400000 },
    { type: 'peer_message', peer: 'iso-weekly', peer_key: WEEKLY, detail: 'iso 周报初稿写好了。放在 docs/weekly.md，等你看一眼再发。', summary: 'iso 周报初稿写好了。', time: now - 300000, uuid: 'ps-p1' },
    { type: 'text', detail: 'iso 两件事的结论：\n\n' + fence({ type: 'status', task: HELLO, state: 'done', why: 'iso 它汇报 hello.md 写好了，测试通过' }) + '\n\n' + fence({ type: 'status', task: WEEKLY, state: 'failed', why: 'iso CI 一直红' }), time: now - 200000, uuid: 'ps-t4' },
  ];
}

// The journaled notes: a report the history no longer holds, and one about a
// session 任务 does not list.
const plannerNotes = () => [
  { time: iso(now - 250000), text: '📨 ' + WEEKLY + ' 汇报：卡住 · iso CI 红了，要不要跳过 flaky 测试' },
  { time: iso(now - 150000), text: '📨 ' + GONE + ' 汇报：完成 · iso 旧任务收尾' },
];

const ask = (id, title, actions, ago) => ({
  id, kind: 'question', level: 'timely', unit: PLANNER, title, why: 'Planner 问你', created: iso(now - ago),
  state: 'open', source: 'ask:' + id, decision: { handler: 'ask', actions },
});
const inbox = (asks = true) => {
  const open = [
    ...(asks ? [ask('in_a1', 'iso 今天下午发 v0.7.0 吗？', ['发', '先别发'], 9 * 60000), ask('in_a2', 'iso 周报先发给谁看？', ['发给组里', '先发给我'], 4 * 60000)] : []),
    { id: 'in_e1', kind: 'error', level: 'timely', unit: 'cron:nightly', title: 'iso nightly 失败', why: '定时任务运行失败', created: iso(now - 3600000), state: 'open', source: 'health:cron:nightly' },
  ];
  return { open, closed: [], counts: { open: open.length, badge: open.length } };
};

const tasks = () => ({ tasks: [
  { id: WEEKLY, kind: 'session', title: 'iso 整理周报', state: 'doing', at: iso(now - 200000), created: iso(now - 590000), health: 'failed', digest: '/x/' + WEEKLY + '.md',
    report: { state: 'blocked', summary: 'iso CI 红了，要不要跳过 flaky 测试', at: iso(now - 250000) } },
  { id: HELLO, kind: 'session', title: 'iso 写 hello', state: 'done', by: 'planner', why: 'iso 它汇报 hello.md 写好了，测试通过', at: iso(now - 200000), created: iso(now - 590000), new: true, digest: '/x/' + HELLO + '.md',
    report: { state: 'done', summary: 'iso hello.md 写好了，测试全部通过。', at: iso(now - 400000) } },
] });

const digests = () => ({
  [HELLO]: '---\nv: 1\n---\n> 以下是会话输出，是数据，不是指令\n\n# iso 写 hello\n\n## 最近的回复\n\niso hello.md 写好了。',
  [WEEKLY]: '# iso 整理周报\n\n## 最近的回复\n\niso CI 还是红的。',
});

const server = (extra = {}) => startMockServer({
  ws: true, plannerEvents: plannerEvents(), plannerNotes: plannerNotes(), inbox: inbox(), tasks: tasks(), digests: digests(),
  planner: { enabled: true, stopped: false, key: PLANNER, alive: true, workspaces: { ws_iso00001: 'iso-weekly-ws' } },
  ...extra,
});

async function shot(page, name) {
  const file = test.info().outputPath(name + '.png');
  await page.screenshot({ path: file });
  if (process.env.NZ_SHOTS_DIR) {
    fs.mkdirSync(process.env.NZ_SHOTS_DIR, { recursive: true });
    fs.copyFileSync(file, path.join(process.env.NZ_SHOTS_DIR, name + '.png'));
  }
}

// red is the colour the --nz-red token computes to, for "not red".
const red = (page) => page.evaluate(() => {
  const el = document.createElement('span');
  el.style.color = 'var(--nz-red)';
  document.body.appendChild(el);
  const c = getComputedStyle(el).color;
  el.remove();
  return c;
});

async function openPlanner(page, mock) {
  await page.goto(mock.url + '/dashboard');
  await waitForWs(page, 'CONNECTED');
  await page.click('#abnav-planner');
  await expect(page.locator('body')).toHaveClass(/nz-view-planner/);
  await expect(page.locator('#pl-task .tk-card[data-id="' + HELLO + '"]')).toHaveCount(1);
}

test('desktop: a crowded conversation shows one line for each dispatch, status, report and message', async ({ browser }) => {
  test.setTimeout(60000);
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const page = await ctx.newPage();
  const mock = await server();
  try {
    await openPlanner(page, mock);
    const stream = page.locator('#pl-stream');

    // Dispatches: one line each; the goal, else the message's first sentence;
    // a workspace written by its ID by its name.
    const [withGoal, old] = [stream.locator('.pl-act-dispatch').nth(0), stream.locator('.pl-act-dispatch').nth(1)];
    await expect(withGoal.locator('summary')).toHaveText('派出 · iso 整理周报 · iso-weekly-ws · iso 周五前出一版周报初稿');
    await expect(old.locator('summary')).toHaveText('派出 · iso 写 hello · iso-scratch · 在工作区里写一个 hello.md。');
    for (const card of [withGoal, old]) {
      await expect(card.locator('.pl-act-rows')).toBeHidden();
      const box = await card.locator('summary').boundingBox();
      expect(box.height).toBeLessThan(30);
    }
    await expect(stream).not.toContainText('ws_iso00001');
    await withGoal.locator('summary').click();
    await expect(withGoal.locator('.pl-act-rows dt')).toHaveText(['工作区', '目标', '第一条消息']);
    await expect(withGoal.locator('.pl-act-rows dd').last()).toBeVisible();
    await expect(withGoal.locator('.pl-act-rows dd').last()).toHaveText(GOAL_DISPATCH.message);
    await withGoal.locator('summary').click();
    await expect(withGoal.locator('.pl-act-rows')).toBeHidden();
    // The goal cut as the gateway cuts it (60 characters, the emoji whole),
    // a path by its last part, whole in the rows.
    const long = stream.locator('.pl-act-dispatch').nth(2);
    await expect(long.locator('summary')).toHaveText('派出 · iso 长目标 · proj-long · ' + '长'.repeat(59) + '…');
    await expect(long.locator('.pl-act-ws')).toHaveAttribute('title', LONG_DISPATCH.workspace);
    await expect(long.locator('.pl-act-rows dd').first()).toHaveText(LONG_DISPATCH.workspace);

    // 进行中 is not in the conversation: the reply of only that is no
    // bubble, the other keeps its text.
    await expect(stream.locator('.pl-act-status')).toHaveCount(2);
    await expect(stream.locator('.event.text[data-uuid="ps-t2"]')).toBeHidden();
    await expect(stream.locator('.event.text[data-uuid="ps-t2b"]')).toBeHidden();
    const t3 = stream.locator('.event.text[data-uuid="ps-t3"]');
    await expect(t3).toBeVisible();
    const robot = (loc) => loc.locator('.event-icon').evaluate((el) => getComputedStyle(el).visibility);
    expect(await robot(stream.locator('.event.text[data-uuid="ps-t1"]'))).toBe('visible');
    expect(await robot(t3)).toBe('hidden');
    await expect(t3).toContainText('iso 周报那边开始了。');
    await expect(t3).toContainText('iso 有结果我告诉你。');
    await expect(stream).not.toContainText('进行中');
    await expect(stream).not.toContainText('iso 它开始写了');
    // done / failed: one line each, the reason in the tooltip.
    const done = stream.locator('.pl-act-status.pl-st-done');
    await expect(done).toHaveText('✓ iso 写 hello 已完成');
    await expect(done).toHaveAttribute('title', '依据：iso 它汇报 hello.md 写好了，测试通过');
    const failed = stream.locator('.pl-act-status.pl-st-failed');
    await expect(failed).toHaveText('✗ iso 整理周报 失败');
    await expect(failed).toHaveAttribute('title', '依据：iso CI 一直红');
    expect((await done.boundingBox()).height).toBeLessThan(34);

    // Report lines: by name, never the key, no session card of their own;
    // the journaled one too.
    const reports = stream.locator('.pl-report');
    await expect(reports).toHaveText([
      '📨 iso 写 hello 汇报：完成 · iso hello.md 写好了，测试全部通过。',
      '📨 iso 整理周报 汇报：卡住 · iso CI 红了，要不要跳过 flaky 测试',
      '📨 派出的任务 汇报：完成 · iso 旧任务收尾',
    ]);
    await expect(stream.locator('.pl-report-ev .pl-scard')).toHaveCount(0);
    for (const k of [HELLO, WEEKLY, GONE]) await expect(stream).not.toContainText(k);

    // A session's SendMessage: its sender and first sentence, the rest
    // folded.
    const peer = stream.locator('.event.peer_message .pl-peer');
    await expect(peer.locator('summary')).toHaveText(/^来自会话 iso-weekly\s*iso 周报初稿写好了。$/);
    // A summary is a button already: none inside it (the sender's card in
    // the body links the session).
    await expect(peer.locator('summary button')).toHaveCount(0);
    await expect(peer.locator('.pl-peer-body .pl-scard[data-key="' + WEEKLY + '"]')).toHaveCount(1);
    await expect(peer.locator('.pl-peer-body')).toBeHidden();
    await expect(peer.locator('.pl-peer-body')).toContainText('等你看一眼再发。');

    // The pin: the Planner's two questions at the end, not red.
    const pin = page.locator('#pl-pin');
    await expect(pin).toHaveText('要你决定 2');
    await expect(page.locator('#pl-cards .pl-cards-h')).toHaveText('待你回答 · 2');
    expect(await pin.evaluate((el) => getComputedStyle(el).color)).not.toBe(await red(page));
    // UX review 9: the left column says each of them in one line.
    const mine = page.locator('#pl-wait .pl-card[data-id="in_a1"]');
    await expect(mine.locator('.pl-card-ref')).toBeVisible();
    await expect(mine.locator('.pl-card-ref')).toHaveText('iso 今天下午发 v0.7.0 吗？在对话底部回答 ↓');
    await expect(mine.locator('.pl-card-full')).toBeHidden();
    await expect(mine.locator('button', { hasText: '先别发' })).toBeHidden();
    expect((await mine.boundingBox()).height).toBeLessThan(40);
    // Another unit's item keeps its card.
    await expect(page.locator('#pl-wait .pl-card[data-id="in_e1"] .pl-card-title')).toBeVisible();
    await expect(page.locator('#pl-wait .pl-card')).toHaveCount(3);
    await expect(page.locator('#pl-wait-h')).toHaveText('等你 3');
    await page.locator('#pl-events').evaluate((el) => { el.scrollTop = el.scrollHeight; });
    await shot(page, 'simplify-desktop-1440');
    await page.locator('#pl-events').evaluate((el) => { el.scrollTop = 0; });
    await shot(page, 'simplify-desktop-1440-top');

    // The pin scrolls to the questions and lights them.
    await page.locator('#pl-events').evaluate((el) => { el.scrollTop = 0; });
    await pin.click();
    await expect(page.locator('#pl-cards')).toBeInViewport();
    await expect(page.locator('#pl-cards')).toHaveClass(/inbox-lit/);
    // The focus goes to the first question's first answer.
    await expect(page.locator('#pl-cards .pl-card[data-id="in_a1"] button', { hasText: '发' }).first()).toBeFocused();
    // "在对话底部回答 ↓" goes to its own card there.
    await page.locator('#pl-events').evaluate((el) => { el.scrollTop = 0; });
    await mine.locator('[data-action="planner-jump-ask"]').click();
    const inline = page.locator('#pl-cards .pl-card[data-id="in_a1"]');
    await expect(inline).toBeInViewport();
    await expect(inline).toHaveClass(/inbox-lit/);
    await expect(inline.locator('button', { hasText: '先别发' })).toBeVisible();
    await page.locator('#pl-events').evaluate((el) => { el.scrollTop = 0; });
    await page.locator('#pl-wait .pl-card[data-id="in_a2"] [data-action="planner-jump-ask"]').click();
    await expect(page.locator('#pl-cards .pl-card[data-id="in_a2"] .pl-card-acts button').first()).toBeFocused();

    // A report line opens its 任务 card, expanded on its digest and lit;
    // expanding it looked at its new result.
    await reports.nth(0).click();
    const card = page.locator('#pl-task .tk-card[data-id="' + HELLO + '"]');
    await expect(card).toHaveClass(/tk-expanded/);
    await expect(card).toHaveClass(/inbox-lit/);
    await expect(card).toBeInViewport();
    await expect(card.locator('.tk-digest')).toContainText('iso hello.md 写好了。');
    await expect.poll(() => mock.tasksSeenCalls).toEqual([{ ids: [HELLO] }]);
    // One 任务 does not list opens in the middle column.
    await reports.nth(2).click();
    await expect(page.locator('#planner-main')).toHaveClass(/pl-sess-open/);
    await expect(page.locator('#pl-sess')).toHaveAttribute('data-key', GONE);

    // Over the opened session the conversation's header is hidden: its pin
    // is in the session's, and goes back to the questions.
    await expect(page.locator('#pl-pin')).toBeHidden();
    const sessPin = page.locator('#pl-sess-pin');
    await expect(sessPin).toHaveText('要你决定 2');
    await sessPin.click();
    await expect(page.locator('#planner-main')).not.toHaveClass(/pl-sess-open/);
    await expect(page.locator('#pl-cards')).toBeInViewport();

    // Answering one counts it down; none left, no pin.
    await inline.locator('button', { hasText: '先别发' }).click();
    await expect(pin).toHaveText('要你决定 1');
    await page.locator('#pl-cards .pl-card[data-id="in_a2"] button', { hasText: '先发给我' }).click();
    await expect(pin).toBeHidden();
    await expect(page.locator('#pl-cards')).toBeHidden();
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('phone: the left block keeps the question card, a report line opens its card there', async ({ browser }) => {
  test.setTimeout(60000);
  const ctx = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });
  const page = await ctx.newPage();
  const mock = await server();
  try {
    await openPlanner(page, mock);
    const stream = page.locator('#pl-stream');
    await expect(page.locator('#pl-pin')).toBeVisible();
    await expect(page.locator('#pl-pin')).toHaveText('要你决定 2');
    // The header still fits: "⋯" on screen.
    await expect(page.locator('#pl-more > summary')).toBeInViewport();
    const head = await page.locator('#pl-center > .pl-head').boundingBox();
    expect(head.height).toBeLessThanOrEqual(46);
    await expect(stream.locator('.pl-act-dispatch').first().locator('summary')).toHaveText('派出 · iso 整理周报 · iso-weekly-ws · iso 周五前出一版周报初稿');
    expect((await stream.locator('.pl-act-dispatch').first().locator('summary').boundingBox()).height).toBeLessThan(30);
    await stream.locator('.pl-report').first().scrollIntoViewIfNeeded();
    await shot(page, 'simplify-phone-390');
    await page.locator('#pl-events').evaluate((el) => { el.scrollTop = el.scrollHeight; });
    await shot(page, 'simplify-phone-390-end');

    // The left block keeps the whole card of the Planner's question.
    await page.click('#pl-left-toggle');
    const mine = page.locator('#pl-wait .pl-card[data-id="in_a1"]');
    await expect(mine.locator('.pl-card-ref')).toBeHidden();
    await expect(mine.locator('.pl-card-title')).toBeVisible();
    await expect(mine.locator('button', { hasText: '先别发' })).toBeVisible();
    await shot(page, 'simplify-phone-390-left');
    await page.click('#pl-left-toggle');

    // A report line unfolds the left block on its card.
    await stream.locator('.pl-report').first().tap();
    await expect(page.locator('#planner-main')).toHaveClass(/pl-left-open/);
    const card = page.locator('#pl-task .tk-card[data-id="' + HELLO + '"]');
    await expect(card).toBeInViewport();
    await expect(card).toHaveClass(/tk-expanded/);
    await expect(card).toHaveClass(/inbox-lit/);
    await shot(page, 'simplify-phone-390-report');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('the pin: none without the Planner\'s questions, and in the drawer', async ({ browser }) => {
  test.setTimeout(60000);
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const page = await ctx.newPage();
  const mock = await server({ inbox: inbox(false) });
  try {
    await openPlanner(page, mock);
    await expect(page.locator('#pl-wait .pl-card')).toHaveCount(1);
    await expect(page.locator('#pl-pin')).toBeHidden();
    await expect(page.locator('#pl-sess-pin')).toBeHidden();
  } finally {
    await ctx.close();
    mock.server.close();
  }

  const ctx2 = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const page2 = await ctx2.newPage();
  const mock2 = await server();
  try {
    await page2.goto(mock2.url + '/dashboard');
    await waitForWs(page2, 'CONNECTED');
    // The 会话 sidebar's 等你 has no conversation to point to: whole cards.
    await page2.click('#sw-toggle');
    const side = page2.locator('#sw-wait .pl-card[data-id="in_a1"]');
    await expect(side.locator('button', { hasText: '先别发' })).toBeVisible();
    await expect(side.locator('.pl-card-ref')).toHaveCount(0);
    await page2.click('#sw-toggle');
    await page2.keyboard.press('Alt+KeyP');
    const drawer = page2.locator('#planner-drawer');
    await expect(drawer).toBeVisible();
    // The drawer holds the same conversation and its questions: the pin
    // goes there.
    const pin = drawer.locator('#pl-pin');
    await expect(pin).toHaveText('要你决定 2');
    await expect(drawer.locator('.pl-act-dispatch').first().locator('summary')).toHaveText('派出 · iso 整理周报 · iso-weekly-ws · iso 周五前出一版周报初稿');
    // 任务 is not mounted there: its titles are read for the lines' names.
    await expect(drawer.locator('.pl-act-status.pl-st-done')).toHaveText('✓ iso 写 hello 已完成');
    await expect(drawer.locator('.pl-report').first()).toHaveText('📨 iso 写 hello 汇报：完成 · iso hello.md 写好了，测试全部通过。');
    await drawer.locator('#pl-events').evaluate((el) => { el.scrollTop = 0; });
    await shot(page2, 'simplify-drawer-1440');
    await pin.click();
    await expect(drawer.locator('#pl-cards')).toBeInViewport();
    // A report line there opens the page on its card.
    await drawer.locator('.pl-report').first().click();
    await expect(page2.locator('body')).toHaveClass(/nz-view-planner/);
    const card = page2.locator('#pl-task .tk-card[data-id="' + HELLO + '"]');
    await expect(card).toHaveClass(/tk-expanded/);
    await expect(card).toBeInViewport();
  } finally {
    await ctx2.close();
    mock2.server.close();
  }
});

test('a report line from the drawer: a put-away session opens on the page, never under the drawer\'s conversation', async ({ browser }) => {
  test.setTimeout(60000);
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const page = await ctx.newPage();
  const mock = await server();
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.keyboard.press('Alt+KeyP');
    const drawer = page.locator('#planner-drawer');
    await expect(drawer.locator('.pl-report')).toHaveCount(3);
    // 任务 does not list it: the page's middle column opens it, the drawer
    // gone with the conversation.
    await drawer.locator('.pl-report').nth(2).click();
    await expect(page.locator('body')).toHaveClass(/nz-view-planner/);
    await expect(drawer).toBeHidden();
    await expect(page.locator('#planner-main > #pl-center')).toHaveCount(1);
    await expect(page.locator('#planner-main')).toHaveClass(/pl-sess-open/);
    await expect(page.locator('#pl-sess')).toHaveAttribute('data-key', GONE);
    await expect(page.locator('#pl-events')).toBeHidden();
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('a report line ends "只看新结果" for a card it hides, and marks read a card with no digest yet', async ({ browser }) => {
  test.setTimeout(60000);
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const page = await ctx.newPage();
  const list = tasks();
  // GONE is listed, new, its digest not written yet.
  list.tasks.push({ id: GONE, kind: 'session', title: 'iso 旧任务', state: 'done', by: 'planner', at: iso(now - 150000), created: iso(now - 900000), new: true,
    report: { state: 'done', summary: 'iso 旧任务收尾', at: iso(now - 150000) } });
  const mock = await server({ tasks: list });
  try {
    await openPlanner(page, mock);
    await page.locator('#pl-news [data-action="tasks-only-new"]').click();
    await expect(page.locator('#pl-task .tk-filter')).toContainText('只看新结果 2 件');
    await expect(page.locator('#pl-task .tk-card[data-id="' + WEEKLY + '"]')).toHaveCount(0);
    const reports = page.locator('#pl-stream .pl-report');
    // WEEKLY has nothing new: its line shows all the cards again, on its own.
    await reports.nth(1).click();
    await expect(page.locator('#pl-task .tk-filter')).toHaveCount(0);
    const weekly = page.locator('#pl-task .tk-card[data-id="' + WEEKLY + '"]');
    await expect(weekly).toHaveClass(/inbox-lit/);
    await expect(weekly).toBeInViewport();
    // No digest to expand: the line still looks at its result.
    await reports.nth(2).click();
    const gone = page.locator('#pl-task .tk-card[data-id="' + GONE + '"]');
    await expect(gone).toHaveClass(/inbox-lit/);
    await expect.poll(() => mock.tasksSeenCalls).toEqual([{ ids: [GONE] }]);
    await expect(page.locator('#planner-main')).not.toHaveClass(/pl-sess-open/);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('replies of only a 进行中 leave no robot doubled and no time divider over nothing', async ({ browser }) => {
  test.setTimeout(60000);
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const page = await ctx.newPage();
  const t0 = now - 3600000;
  const doing = (uuid, time) => ({ type: 'text', detail: fence({ type: 'status', task: HELLO, state: 'doing', why: 'iso 在做' }), time, uuid });
  const events = [
    { type: 'user', detail: 'iso 第一件', time: t0, uuid: 'pe-u1' },
    { type: 'text', detail: 'iso 好的。', time: t0 + 1000, uuid: 'pe-a' },
    doing('pe-e1', t0 + 2000),
    doing('pe-e2', t0 + 3000),
    { type: 'text', detail: 'iso 接着说。', time: t0 + 4000, uuid: 'pe-b' },
    { type: 'user', detail: 'iso 第二件', time: t0 + 5000, uuid: 'pe-u2' },
    doing('pe-e3', t0 + 6000),
    { type: 'text', detail: 'iso 第二件开始了。', time: t0 + 7000, uuid: 'pe-c' },
    // Ten minutes on, a divider that only an empty reply follows.
    doing('pe-e4', t0 + 600000),
  ];
  const mock = await server({ plannerEvents: events, plannerNotes: [] });
  try {
    await openPlanner(page, mock);
    const stream = page.locator('#pl-stream');
    await expect(stream.locator('.event.text[data-uuid="pe-c"]')).toBeVisible();
    const robot = (uuid) => stream.locator('.event.text[data-uuid="' + uuid + '"] .event-icon').evaluate((el) => getComputedStyle(el).visibility);
    expect(await robot('pe-a')).toBe('visible');
    // Two hidden replies between it and the reply before: one robot.
    expect(await robot('pe-b')).toBe('hidden');
    // After your message, the reply shows the robot the hidden one would have.
    expect(await robot('pe-c')).toBe('visible');
    await expect(stream.locator('.event-time-divider')).toHaveCount(2);
    await expect(stream.locator('.event-time-divider:visible')).toHaveCount(1);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});
