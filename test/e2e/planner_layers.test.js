// E2E: the Planner timeline's two layers (planner RFC §15.14 ②, D113). 对话,
// the default, shows the turns you started (your message, your answer to a
// question) with the Planner's whole reply; a receipt, a report line, a
// session's SendMessage, a scout run and a receipt from before entries
// carried their source fold into one 动态 row between two of yours, which
// unfolds to one line each (time, task or source, the reply's first
// sentence, "→ 等你" for a turn that asked you) and a line to its whole
// reply; the shift change is one separator, not your bubble, its fold the
// handoff; the Planner on a turn you did not start says 处理汇报中 and
// writes nothing into the timeline; a session you opened ending a turn
// (§15.22 ①, D138) is an entry only when the Planner had something to say.
// 全部 is the whole timeline, remembered
// on this device. The page, the Alt+P drawer and a phone alike.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const PLANNER = 'planner:main';
const FIX = 'task:fix00001';
const DOCS = 'task:docs0002';
// The fixture's times sit in one hour of today, so the rows' spans read the
// same whatever the clock says (a turn of yesterday reads 昨天 HH:MM).
const base = (() => { const d = new Date(); d.setHours(10, 0, 0, 0); return Math.min(d.getTime(), Date.now() - 3600000); })();
const at = (min) => base + min * 60000;
const hm = (t) => { const d = new Date(t); return String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0'); };
const fence = (body) => '```naozhi-action\n' + JSON.stringify(body) + '\n```';
const snap = '<naozhi-snapshot at="x">\n</naozhi-snapshot>\n\n';
const receipt = (text) => '<naozhi-receipt>\n' + text + '\n</naozhi-receipt>';

const SUMMARY = '## 进行中\niso 修复批次已合并。\n## 答应用户的事\n无\n## 用户新说的偏好与决定\n无\n## 待跟进\n无';

function plannerEvents() {
  return [
    { type: 'user', detail: snap + 'iso 把修复批次派出去', time: at(0), uuid: 'ly-u1' },
    { type: 'text', detail: 'iso 好，派出去了。', time: at(0.2), uuid: 'ly-t1' },
    { type: 'result', time: at(0.3), uuid: 'ly-r1' },
    // A report: the gateway's line and the receipt turn that tells the Planner.
    { type: 'system', summary: '📨 ' + FIX + ' 汇报：完成 · iso PR 已合并', time: at(20), uuid: 'ly-s1' },
    { type: 'user', source: 'receipt', detail: receipt(FIX + '（iso 修复批次）汇报：完成 · iso PR 已合并'), time: at(20.1), uuid: 'ly-u2' },
    { type: 'text', detail: 'iso 修复批次完成了，PR 已合并。后面没有别的事。', time: at(20.3), uuid: 'ly-t2' },
    { type: 'result', time: at(20.4), uuid: 'ly-r2' },
    // A session's SendMessage, whose turn asked you.
    { type: 'peer_message', peer: 'iso-docs', peer_key: DOCS, detail: 'iso 文档写好了。要不要发到 wiki？', summary: 'iso 文档写好了。', time: at(30), uuid: 'ly-p1' },
    { type: 'text', detail: 'iso 文档那边问要不要发 wiki。\n\n' + fence({ type: 'ask', question: 'iso 发到 wiki 吗？', options: ['发', '先别'] }), time: at(30.3), uuid: 'ly-t3' },
    { type: 'result', time: at(30.4), uuid: 'ly-r3' },
    // A scout run's turn.
    { type: 'user', source: 'scout', detail: '<naozhi-scout findings="1" run="20261116T110000-ab12">\n{"title":"iso 机密"}\n</naozhi-scout>\n\n' + snap, time: at(40), uuid: 'ly-u3' },
    { type: 'text', detail: 'iso 巡检没有要你处理的。', time: at(40.2), uuid: 'ly-t4' },
    { type: 'result', time: at(40.3), uuid: 'ly-r4' },
    // A receipt from before entries carried their source.
    { type: 'user', detail: receipt(FIX + '（iso 修复批次）已收起'), time: at(45), uuid: 'ly-u4' },
    { type: 'text', detail: 'iso 收起了。', time: at(45.2), uuid: 'ly-t5' },
    { type: 'result', time: at(45.3), uuid: 'ly-r5' },
    { type: 'user', detail: snap + 'iso 接着呢', time: at(50), uuid: 'ly-u5' },
    { type: 'text', detail: 'iso 没有别的了。', time: at(50.2), uuid: 'ly-t6' },
    { type: 'result', time: at(50.3), uuid: 'ly-r6' },
    // Your answer to a question.
    { type: 'user', source: 'answer', detail: receipt('你问用户的「iso 发到 wiki 吗？」，用户的回答：发。网关已把这个回答原样送到 ' + DOCS + '（iso 写文档），不用再转达；它做完会写汇报块，网关转给你。'), time: at(52), uuid: 'ly-u6' },
    { type: 'text', detail: 'iso 好，已经转给文档那边。', time: at(52.2), uuid: 'ly-t7' },
    { type: 'result', time: at(52.3), uuid: 'ly-r7' },
    // The shift change: the handoff asked of the old conversation, then
    // given to the new one.
    { type: 'user', source: 'shift', detail: '现在换班：请写交接摘要，交给接班的新会话。', time: at(55), uuid: 'ly-u7' },
    { type: 'text', detail: SUMMARY, time: at(55.5), uuid: 'ly-t8' },
    { type: 'result', time: at(55.6), uuid: 'ly-r8' },
    { type: 'user', source: 'shift', detail: '<naozhi-handoff day="2026-11-16">\n' + SUMMARY + '\n</naozhi-handoff>\n这是上一班会话留下的交接摘要。', time: at(56), uuid: 'ly-u8' },
    { type: 'text', detail: '已接班', time: at(56.2), uuid: 'ly-t9' },
    { type: 'result', time: at(56.3), uuid: 'ly-r9' },
  ];
}

const tasks = () => ({ tasks: [
  { id: FIX, kind: 'session', title: 'iso 修复批次', state: 'done', at: new Date(at(20)).toISOString(), created: new Date(at(0)).toISOString() },
  { id: DOCS, kind: 'session', title: 'iso 写文档', state: 'doing', at: new Date(at(30)).toISOString(), created: new Date(at(0)).toISOString() },
] });

const planner = { enabled: true, stopped: false, key: PLANNER, alive: true };

async function openPlanner(page, mock) {
  await page.goto(mock.url + '/dashboard');
  await waitForWs(page, 'CONNECTED');
  await page.evaluate(() => document.getElementById('abnav-planner').click());
  await expect(page.locator('body')).toHaveClass(/nz-view-planner/);
}

for (const vp of [{ name: 'desktop', width: 1280, height: 900 }, { name: 'phone', width: 390, height: 844 }]) {
  test(`${vp.name}: 对话 folds what you did not start into 动态, 全部 shows it all`, async ({ browser }) => {
    test.setTimeout(60000);
    const ctx = await browser.newContext({ viewport: { width: vp.width, height: vp.height } });
    const page = await ctx.newPage();
    const errors = [];
    page.on('pageerror', (e) => errors.push(e.message));
    const mock = await startMockServer({ ws: true, plannerEvents: plannerEvents(), tasks: tasks(), planner });
    try {
      await openPlanner(page, mock);
      const stream = page.locator('#pl-stream');
      // The switch is in the header, on a phone in "⋯".
      const phone = vp.name === 'phone';
      const sw = phone ? page.locator('#pl-tools .pl-layer') : page.locator('.pl-head > .pl-layer');
      await expect(page.locator('.pl-head > .pl-layer')).toBeVisible({ visible: !phone });
      if (phone) await page.click('#pl-more > summary');
      await expect(sw.locator('[data-layer="talk"]')).toHaveAttribute('aria-pressed', 'true');
      if (phone) await page.click('#pl-more > summary');
      // Yours: two messages and the answer, the answer as your bubble.
      await expect(stream.locator('.event.user:visible')).toHaveCount(3);
      const answer = stream.locator('.event.user:visible').nth(2);
      await expect(answer).toContainText('iso 发到 wiki 吗？');
      await expect(answer).toContainText('发');
      await expect(answer).not.toContainText('网关已把');
      await expect(stream.getByText('iso 好，已经转给文档那边。')).toBeVisible();
      // Not yours: none of it in view, one row for the four between.
      for (const t of ['iso 修复批次完成了', 'iso 文档那边问要不要发 wiki', 'iso 巡检没有要你处理的', 'iso 收起了', '来自巡检']) {
        await expect(stream.getByText(t, { exact: false }).first()).toBeHidden();
      }
      await expect(stream.locator('.event.peer_message:visible')).toHaveCount(0);
      const rows = stream.locator('.pl-dyn');
      await expect(rows).toHaveCount(1);
      const head = rows.locator('.pl-dyn-h');
      await expect(head).toHaveText('▸ ' + hm(at(20)) + '–' + hm(at(45)) + ' · 4 条动态（iso 修复批次 2 条 · iso 写文档 · 巡检）');
      // The row sits between the two messages of yours.
      const order = await stream.evaluate((box) => [...box.children].filter((c) => c.offsetParent !== null)
        .map((c) => (c.matches('.pl-dyn') ? 'row' : c.matches('.pl-shift') ? 'shift' : c.matches('.event.user') ? 'you' : c.matches('.event.text') ? 'reply' : '')).filter(Boolean));
      expect(order).toEqual(['you', 'reply', 'row', 'you', 'reply', 'you', 'reply', 'shift']);

      // Unfolded: one line each, the report's line and its receipt one entry.
      await head.click();
      await expect(head).toHaveAttribute('aria-expanded', 'true');
      const lines = rows.locator('.pl-dyn-line');
      await expect(lines).toHaveCount(4);
      await expect(lines.nth(0).locator('.pl-dyn-t')).toHaveText(hm(at(20)));
      await expect(lines.nth(0).locator('.pl-dyn-src')).toHaveText('iso 修复批次');
      await expect(lines.nth(0).locator('.pl-dyn-one')).toHaveText('iso 修复批次完成了，PR 已合并。');
      await expect(lines.nth(1).locator('.pl-dyn-src')).toHaveText('iso 写文档');
      await expect(lines.nth(1).locator('.pl-dyn-wait')).toHaveText('→ 等你');
      await expect(lines.nth(2).locator('.pl-dyn-src')).toHaveText('巡检');
      await expect(lines.nth(2).locator('.pl-dyn-wait')).toHaveCount(0);
      await expect(lines.nth(3).locator('.pl-dyn-one')).toHaveText('iso 收起了。');
      // A line opens the turn's whole reply, its cards with it.
      await lines.nth(1).click();
      const full = rows.locator('.pl-dyn-item').nth(1).locator('.pl-dyn-full');
      await expect(full).toBeVisible();
      await expect(full.locator('.pl-act-ask')).toContainText('iso 发到 wiki 吗？');
      await expect(full.locator('.event.peer_message')).toHaveCount(1);
      await page.screenshot({ path: test.info().outputPath(`planner-layers-${vp.name}.png`), fullPage: false });
      // The report's line, unfolded, opens its 任务 card as in 全部.
      if (!phone) {
        await lines.nth(0).click();
        const report = rows.locator('.pl-dyn-item').nth(0).locator('.pl-dyn-full .pl-report');
        await expect(report).toContainText('iso 修复批次 汇报：完成');
        await report.click();
        const card = page.locator('#pl-task .tk-card[data-id="' + FIX + '"]');
        await expect(card).toHaveClass(/inbox-lit/);
        await expect(card).toBeInViewport();
      }

      // The shift change: a separator, no bubble of yours; its fold is the handoff.
      const shift = stream.locator('.pl-shift');
      await expect(shift).toHaveCount(1);
      await expect(shift.locator('.pl-shift-h')).toHaveText('—— 11-16 换班 · 看交接 ▸ ——');
      await expect(stream.getByText('现在换班', { exact: false })).toBeHidden();
      await expect(stream.getByText('已接班', { exact: true })).toBeHidden();
      await shift.locator('.pl-shift-h').click();
      await expect(shift.locator('.pl-shift-body')).toContainText('iso 修复批次已合并。');

      expect(errors).toEqual([]);

      // 全部: everything, as the timeline was; remembered on reload.
      if (phone) await page.click('#pl-more > summary');
      await sw.locator('[data-layer="all"]').click();
      await expect(sw.locator('[data-layer="all"]')).toHaveAttribute('aria-pressed', 'true');
      await expect(stream.locator('.pl-dyn')).toHaveCount(0);
      await expect(stream.locator('.pl-shift')).toHaveCount(0);
      await expect(stream.getByText('iso 修复批次完成了，PR 已合并。')).toBeVisible();
      await expect(stream.getByText('iso 巡检没有要你处理的。')).toBeVisible();
      await expect(stream.getByText('来自巡检：1 条新发现')).toBeVisible();
      await page.reload();
      await waitForWs(page, 'CONNECTED');
      await page.evaluate(() => document.getElementById('abnav-planner').click());
      await expect(sw.locator('[data-layer="all"]')).toHaveAttribute('aria-pressed', 'true');
      await expect(page.locator('#pl-stream').getByText('iso 巡检没有要你处理的。')).toBeVisible();
      await expect(page.locator('#pl-stream .pl-dyn')).toHaveCount(0);
      expect(errors).toEqual([]);
    } finally {
      await ctx.close();
      await new Promise((r) => mock.server.close(r));
    }
  });
}

test('a turn you did not start says 处理汇报中 and adds to its row, yours streams as before', async ({ browser }) => {
  test.setTimeout(60000);
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  const page = await ctx.newPage();
  const events = plannerEvents().slice(0, 3);
  const mock = await startMockServer({ ws: true, plannerEvents: events, tasks: tasks(), planner });
  try {
    await openPlanner(page, mock);
    const stream = page.locator('#pl-stream');
    await expect(page.locator('#pl-status')).toHaveText('');
    const t0 = Date.now();
    events.push({ type: 'user', source: 'receipt', detail: receipt(FIX + '（iso 修复批次）汇报：进行中 · iso 在跑测试'), time: t0, uuid: 'ly-live-u' },
      { type: 'text', detail: 'iso 修复批次还在跑测试。', time: t0 + 1, uuid: 'ly-live-t' });
    await expect(page.locator('#pl-status')).toHaveText('处理汇报中');
    await expect(stream.locator('.pl-dyn .pl-dyn-h')).toContainText('1 条动态（iso 修复批次）');
    await expect(stream.locator('.event.text', { hasText: 'iso 修复批次还在跑测试。' })).toBeHidden();
    events.push({ type: 'result', time: t0 + 2, uuid: 'ly-live-r' });
    await expect(page.locator('#pl-status')).toHaveText('');
    // Your message's answer streams into the timeline as before.
    events.push({ type: 'user', detail: snap + 'iso 现在呢', time: t0 + 3, uuid: 'ly-live-u2' },
      { type: 'text', detail: 'iso 我看一下。', time: t0 + 4, uuid: 'ly-live-t2' });
    await expect(stream.getByText('iso 我看一下。')).toBeVisible();
    await expect(page.locator('#pl-status')).toHaveText('');
  } finally {
    await ctx.close();
    await new Promise((r) => mock.server.close(r));
  }
});

test('the drawer folds as the page does', async ({ browser }) => {
  test.setTimeout(60000);
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  const page = await ctx.newPage();
  const mock = await startMockServer({ ws: true, plannerEvents: plannerEvents(), tasks: tasks(), planner });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.keyboard.press('Alt+KeyP');
    const drawer = page.locator('#planner-drawer');
    await expect(drawer).toBeVisible();
    await expect(drawer.locator('.pl-head > .pl-layer [data-layer="talk"]')).toBeVisible();
    await expect(drawer.locator('#pl-stream .pl-dyn')).toHaveCount(1);
    await expect(drawer.locator('#pl-stream .pl-shift')).toHaveCount(1);
    await expect(drawer.locator('#pl-stream .event.user:visible')).toHaveCount(3);
    await drawer.locator('.pl-head > .pl-layer [data-layer="all"]').click();
    await expect(drawer.locator('#pl-stream .pl-dyn')).toHaveCount(0);
    await expect(drawer.getByText('iso 修复批次完成了，PR 已合并。')).toBeVisible();
  } finally {
    await ctx.close();
    await new Promise((r) => mock.server.close(r));
  }
});

test('a SendMessage that came into your turn stays out of your answer', async ({ browser }) => {
  test.setTimeout(60000);
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  const page = await ctx.newPage();
  // The CLI delivered the session's SendMessage while it was on your turn
  // (a queued_command): the rest of the turn answers you.
  const events = [
    { type: 'user', detail: snap + 'iso 任务都怎么样了', time: at(0), uuid: 'mt-u1' },
    { type: 'text', detail: 'iso 我先查一下。', time: at(0.1), uuid: 'mt-t1' },
    { type: 'peer_message', peer: 'iso-docs', peer_key: DOCS, detail: 'iso 文档写到一半。', summary: 'iso 文档写到一半。', time: at(0.2), uuid: 'mt-p1' },
  ];
  const mock = await startMockServer({ ws: true, plannerEvents: events, tasks: tasks(), planner: { ...planner, running: true } });
  try {
    await openPlanner(page, mock);
    const stream = page.locator('#pl-stream');
    await expect(stream.getByText('iso 我先查一下。')).toBeVisible();
    await expect(stream.locator('.pl-dyn .pl-dyn-h')).toContainText('1 条动态（iso 写文档）');
    await expect(stream.locator('.event.peer_message:visible')).toHaveCount(0);
    // Your turn, still under way: it streams, the header says nothing.
    events.push({ type: 'text', detail: 'iso 结论：三个任务都在跑，没有卡住。', time: at(0.3), uuid: 'mt-t2' });
    await expect(stream.getByText('iso 结论：三个任务都在跑，没有卡住。')).toBeVisible();
    await expect(page.locator('#pl-status')).toHaveText('');
    events.push({ type: 'result', time: at(0.4), uuid: 'mt-r1' });
    // After the result, a SendMessage starts a turn of its own: 动态.
    events.push({ type: 'peer_message', peer: 'iso-docs', peer_key: DOCS, detail: 'iso 文档写完了。', summary: 'iso 文档写完了。', time: at(5), uuid: 'mt-p2' },
      { type: 'text', detail: 'iso 文档那边写完了。', time: at(5.1), uuid: 'mt-t3' });
    await expect(stream.locator('.pl-dyn')).toHaveCount(2);
    await expect(stream.locator('.event.text', { hasText: 'iso 文档那边写完了。' })).toBeHidden();
    await expect(page.locator('#pl-status')).toHaveText('处理汇报中');
    const order = await stream.evaluate((box) => [...box.children].filter((c) => c.offsetParent !== null)
      .map((c) => (c.matches('.pl-dyn') ? 'row' : c.matches('.event.user') ? 'you' : c.matches('.event.text') ? 'reply' : '')).filter(Boolean));
    expect(order).toEqual(['you', 'reply', 'row', 'reply', 'row']);
  } finally {
    await ctx.close();
    await new Promise((r) => mock.server.close(r));
  }
});

test('what is opened in an unfolded entry stays open as the page refreshes', async ({ browser }) => {
  test.setTimeout(60000);
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  const page = await ctx.newPage();
  const mock = await startMockServer({ ws: true, plannerEvents: plannerEvents(), tasks: tasks(), planner });
  try {
    await openPlanner(page, mock);
    const row = page.locator('#pl-stream .pl-dyn');
    await row.locator('.pl-dyn-h').click();
    await row.locator('.pl-dyn-line').nth(1).click();
    const peer = row.locator('.pl-dyn-item').nth(1).locator('.pl-dyn-full details.pl-peer');
    await peer.locator('summary').click();
    await expect(peer).toHaveAttribute('open', '');
    await peer.evaluate((el) => { el.dataset.mark = 'kept'; });
    // The stream is folded again for every repaint: the state frames, the
    // session list's poll, a 任务 list, a new event.
    for (const c of mock.wsConnections) {
      c.send({ type: 'session_state', key: PLANNER, state: 'running' });
      c.send({ type: 'session_state', key: PLANNER, state: 'ready' });
      c.send({ type: 'event', key: PLANNER, event: { type: 'user', detail: snap + 'iso 再看看', time: at(57), uuid: 'keep-u' } });
    }
    await expect(page.locator('#pl-stream').getByText('iso 再看看')).toBeVisible();
    await expect(row.locator('details.pl-peer[data-mark="kept"]')).toHaveAttribute('open', '');
  } finally {
    await ctx.close();
    await new Promise((r) => mock.server.close(r));
  }
});

test('a turn cut off without a result is over once the Planner is idle', async ({ browser }) => {
  test.setTimeout(60000);
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  const page = await ctx.newPage();
  // The last turn, a receipt's, has no result: the watchdog killed it.
  const events = plannerEvents().slice(0, 3).concat([
    { type: 'user', source: 'receipt', detail: receipt(FIX + '（iso 修复批次）汇报：进行中 · iso 在跑测试'), time: at(20), uuid: 'cut-u' },
    { type: 'text', detail: 'iso 修复批次还在跑测试。', time: at(20.1), uuid: 'cut-t' },
  ]);
  const status = { ...planner, running: false };
  const mock = await startMockServer({ ws: true, plannerEvents: events, tasks: tasks(), planner: status });
  try {
    await openPlanner(page, mock);
    const stream = page.locator('#pl-stream');
    await expect(stream.locator('.pl-dyn .pl-dyn-h')).toContainText('1 条动态（iso 修复批次）');
    await expect(page.locator('#pl-status')).toHaveText('');
    await stream.locator('.pl-dyn-h').click();
    await expect(stream.locator('.pl-dyn-one')).toHaveText('iso 修复批次还在跑测试。');
    // On a turn again, and idle: as the server says.
    for (const c of mock.wsConnections) c.send({ type: 'session_state', key: PLANNER, state: 'running' });
    await expect(page.locator('#pl-status')).toHaveText('处理汇报中');
    for (const c of mock.wsConnections) c.send({ type: 'session_state', key: PLANNER, state: 'ready' });
    await expect(page.locator('#pl-status')).toHaveText('');
  } finally {
    await ctx.close();
    await new Promise((r) => mock.server.close(r));
  }
});

test('a session you opened ending a turn: the Planner saying 无 leaves nothing in 对话, a note is one 动态 entry', async ({ browser }) => {
  test.setTimeout(60000);
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  const page = await ctx.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  // The mock's first listed session, titled by its last prompt.
  const YOURS = 'dashboard:direct:2026-01-01-120000-1:myproject';
  const watch = (last) => '<naozhi-watch>\n' + JSON.stringify({ key: YOURS, name: 'hello world', workspace: 'myproject', ended: '10:05', last }) + '\n</naozhi-watch>';
  const events = [
    { type: 'user', detail: snap + 'iso 早', time: at(0), uuid: 'wt-u0' },
    { type: 'text', detail: 'iso 早。', time: at(0.1), uuid: 'wt-t0' },
    { type: 'result', time: at(0.2), uuid: 'wt-r0' },
    // Nothing worth saying: the whole reply is 无.
    { type: 'user', source: 'watch', detail: watch('iso 登录页改好了。'), time: at(5), uuid: 'wt-u1' },
    { type: 'text', detail: '无', time: at(5.1), uuid: 'wt-t1' },
    { type: 'result', time: at(5.2), uuid: 'wt-r1' },
    { type: 'user', detail: snap + 'iso 接着呢', time: at(10), uuid: 'wt-u2' },
    { type: 'text', detail: 'iso 没有别的了。', time: at(10.1), uuid: 'wt-t2' },
    { type: 'result', time: at(10.2), uuid: 'wt-r2' },
    // Something to say: a side note in that session.
    { type: 'user', source: 'watch', detail: watch('iso 注册页也改了。'), time: at(20), uuid: 'wt-u3' },
    { type: 'text', detail: 'iso 这和修复批次的结论冲突。\n\n' + fence({ type: 'note', key: YOURS, text: 'iso 修复批次已经改成方案 B', why: 'iso 修复批次的摘要' }), time: at(20.2), uuid: 'wt-t3' },
    { type: 'result', time: at(20.3), uuid: 'wt-r3' },
    // An entry from before the source was written, told by its text.
    { type: 'user', detail: watch('iso 又一轮。'), time: at(25), uuid: 'wt-u4' },
    { type: 'text', detail: '无。', time: at(25.1), uuid: 'wt-t4' },
    { type: 'result', time: at(25.2), uuid: 'wt-r4' },
    // A turn that ended without a word.
    { type: 'user', source: 'watch', detail: watch('iso 没有回话的一轮。'), time: at(27), uuid: 'wt-u5' },
    { type: 'result', time: at(27.2), uuid: 'wt-r5' },
  ];
  const mock = await startMockServer({ ws: true, plannerEvents: events, tasks: tasks(), planner });
  try {
    await openPlanner(page, mock);
    const stream = page.locator('#pl-stream');
    await expect(stream.getByText('iso 没有别的了。')).toBeVisible();
    // Only your two messages are bubbles: the gateway's word is not one.
    await expect(stream.locator('.event.user:visible')).toHaveCount(2);
    await expect(stream.getByText('naozhi-watch', { exact: false })).toHaveCount(0);
    await expect(stream.locator('.event.text:visible', { hasText: /^无[。]?$/ })).toHaveCount(0);
    // One row, for the turn with the note; the two 无 turns and the one
    // without a word are no entry.
    const rows = stream.locator('.pl-dyn');
    await expect(rows).toHaveCount(1);
    await expect(rows.locator('.pl-dyn-h')).toHaveText('▸ ' + hm(at(20)) + ' · 1 条动态（hello world）');
    const order = await stream.evaluate((box) => [...box.children].filter((c) => c.offsetParent !== null)
      .map((c) => (c.matches('.pl-dyn') ? 'row' : c.matches('.event.user') ? 'you' : c.matches('.event.text') ? 'reply' : '')).filter(Boolean));
    expect(order).toEqual(['you', 'reply', 'you', 'reply', 'row']);
    await rows.locator('.pl-dyn-h').click();
    const line = rows.locator('.pl-dyn-line');
    await expect(line).toHaveCount(1);
    await expect(line.locator('.pl-dyn-one')).toHaveText('iso 这和修复批次的结论冲突。');
    await line.click();
    await expect(rows.locator('.pl-dyn-full .pl-act-note')).toContainText('iso 修复批次已经改成方案 B');
    await expect(page.locator('#pl-status')).toHaveText('');

    // A watch turn under way says 处理汇报中 and adds no entry until it has
    // something to show; 无 leaves the timeline as it was.
    const t0 = Date.now();
    events.push({ type: 'user', source: 'watch', detail: watch('iso 第四轮。'), time: t0, uuid: 'wt-live-u' });
    await expect(page.locator('#pl-status')).toHaveText('处理汇报中');
    await expect(rows).toHaveCount(1);
    // What it says on the way to 无 (before it reads the digest) is not
    // something to show either: no entry comes and then goes.
    events.push({ type: 'text', detail: 'iso 我先看一下摘要。', time: t0 + 1, uuid: 'wt-live-t0' });
    await expect(stream.locator('.event.text', { hasText: 'iso 我先看一下摘要。' })).toHaveCount(1);
    await expect(rows).toHaveCount(1);
    await expect(rows.locator('.pl-dyn-h')).toContainText('1 条动态（hello world）');
    events.push({ type: 'text', detail: '无', time: t0 + 2, uuid: 'wt-live-t' }, { type: 'result', time: t0 + 3, uuid: 'wt-live-r' });
    await expect(page.locator('#pl-status')).toHaveText('');
    await expect(rows).toHaveCount(1);
    await expect(rows.locator('.pl-dyn-h')).toContainText('1 条动态（hello world）');

    // 全部 shows every turn, the 无 ones too, still without a bubble of yours.
    await page.locator('.pl-head > .pl-layer [data-layer="all"]').click();
    await expect(stream.locator('.pl-dyn')).toHaveCount(0);
    await expect(stream.locator('.event.text:visible', { hasText: /^无[。]?$/ })).toHaveCount(3);
    await expect(stream.locator('.event.text:visible .pl-act-note')).toHaveCount(1);
    await expect(stream.locator('.event.user:visible')).toHaveCount(2);
    expect(errors).toEqual([]);
  } finally {
    await ctx.close();
    await new Promise((r) => mock.server.close(r));
  }
});

test('the shift change is one line: the morning brief beside it, a handoff of an upgrade, a scout named by its source', async ({ browser }) => {
  test.setTimeout(60000);
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  const page = await ctx.newPage();
  // The snapshot a scout run carries lists the sessions under way.
  const fullSnap = '<naozhi-snapshot at="x">\n## 会话\n- ' + FIX + ' iso 修复批次 进行中\n</naozhi-snapshot>';
  const events = [
    { type: 'user', detail: snap + 'iso 早', time: at(0), uuid: 'sh-u0' },
    { type: 'text', detail: 'iso 早。', time: at(0.1), uuid: 'sh-t0' },
    { type: 'result', time: at(0.2), uuid: 'sh-r0' },
    { type: 'user', source: 'scout', detail: '<naozhi-scout findings="2" run="20261116T080000-ab12">\n{}\n</naozhi-scout>\n\n' + fullSnap, time: at(5), uuid: 'sh-u1' },
    { type: 'text', detail: 'iso 巡检两条都不急。', time: at(5.1), uuid: 'sh-t1' },
    { type: 'result', time: at(5.2), uuid: 'sh-r1' },
    // The brief is made while the old conversation writes its handoff.
    { type: 'user', source: 'shift', detail: '现在换班：请写交接摘要，交给接班的新会话。', time: at(10), uuid: 'sh-u2' },
    { type: 'text', detail: SUMMARY, time: at(10.6), uuid: 'sh-t2' },
    { type: 'result', time: at(10.7), uuid: 'sh-r2' },
    { type: 'user', source: 'shift', detail: '<naozhi-handoff day="2026-11-16">\n' + SUMMARY + '\n</naozhi-handoff>\n这是上一班会话留下的交接摘要。', time: at(11), uuid: 'sh-u3' },
    { type: 'text', detail: '已接班', time: at(11.1), uuid: 'sh-t3' },
    { type: 'result', time: at(11.2), uuid: 'sh-r3' },
    { type: 'user', detail: snap + 'iso 好', time: at(20), uuid: 'sh-u4' },
    { type: 'text', detail: 'iso 嗯。', time: at(20.1), uuid: 'sh-t4' },
    { type: 'result', time: at(20.2), uuid: 'sh-r4' },
    // An upgrade's change, its summary reused from its file: only the
    // handoff is sent, its day the summary's name (an older entry, no source).
    { type: 'user', detail: '<naozhi-handoff day="2026-11-16-v19">\niso 升级后的交接。\n</naozhi-handoff>\n这是上一班会话留下的交接摘要。', time: at(30), uuid: 'sh-u5' },
    { type: 'text', detail: '已接班', time: at(30.1), uuid: 'sh-t5' },
    { type: 'result', time: at(30.2), uuid: 'sh-r5' },
  ];
  const briefs = [{ day: '2026-11-16', at: new Date(at(10.3)).toISOString(), sent: true, markdown: 'iso 今天的早报' }];
  const mock = await startMockServer({ ws: true, plannerEvents: events, tasks: tasks(), planner, plannerBriefs: briefs });
  try {
    await openPlanner(page, mock);
    const stream = page.locator('#pl-stream');
    await expect(stream.locator('.pl-brief')).toHaveCount(1);
    const order = await stream.evaluate((box) => [...box.children].filter((c) => c.offsetParent !== null)
      .map((c) => (c.matches('.pl-dyn') ? 'row' : c.matches('.pl-shift') ? 'shift' : c.matches('.pl-brief') ? 'brief' : c.matches('.event.user') ? 'you' : c.matches('.event.text') ? 'reply' : '')).filter(Boolean));
    expect(order).toEqual(['you', 'reply', 'row', 'shift', 'brief', 'you', 'reply', 'shift']);
    await expect(stream.locator('.pl-dyn .pl-dyn-h')).toContainText('1 条动态（巡检）');
    const shifts = stream.locator('.pl-shift');
    await shifts.nth(0).locator('.pl-shift-h').click();
    await expect(shifts.nth(0).locator('.pl-shift-body')).toContainText('iso 修复批次已合并。');
    await expect(shifts.nth(1).locator('.pl-shift-h')).toHaveText('—— 11-16 换班 · 看交接 ▸ ——');
    await shifts.nth(1).locator('.pl-shift-h').click();
    await expect(shifts.nth(1).locator('.pl-shift-body')).toContainText('iso 升级后的交接。');
    await expect(shifts.nth(1).locator('.pl-shift-body')).not.toContainText('已接班');
    await expect(stream.getByText('iso 升级后的交接。', { exact: false }).first()).toBeVisible();
    await expect(stream.locator('.event.user:visible')).toHaveCount(2);
  } finally {
    await ctx.close();
    await new Promise((r) => mock.server.close(r));
  }
});
