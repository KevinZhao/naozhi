// E2E: the gateway's side notes in the session view (session_notes.js,
// planner RFC §15.3 ③): GET /api/sessions/notes places them among the
// bubbles by time, a session_note frame adds one live, neither starts a turn,
// a health transition and the item it opened are one note naming the cause,
// no note links back to the Planner page (its card only opens this session
// again), and a new socket lists the notes pushed while it was down.
// data-inbox-item anchors a note to its item for a notification's click.
// A note the Planner wrote (§15.22 ②) is marked Planner, shows its words,
// folds what it goes by behind 依据, and takes 没用: marked at once, posted
// to /api/sessions/notes/useless, taken back when the post fails. The notes
// of a session on a node running its own Planner show without 没用.
// A progress note (§15.22 ④) says how far the background work got, takes no
// 没用, and a stretch of background work shows its latest one only.
const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const A = 'dashboard:direct:2026-01-01-120000-1:myproject';
const now = Date.now();
const iso = (ms) => new Date(ms).toISOString();
const T = (min) => now - min * 60000;

const EVENTS = [
  { type: 'user', detail: '跑一下测试', time: T(30), uuid: 'u1' },
  { type: 'text', detail: '测试开始跑了', time: T(29), uuid: 't1' },
  { type: 'result', summary: 'done', time: T(29), uuid: 'r1' },
  { type: 'user', detail: '再试一次', time: T(10), uuid: 'u2' },
  { type: 'text', detail: '这次通过了', time: T(9), uuid: 't2' },
  { type: 'result', summary: 'done', time: T(9), uuid: 'r2' },
];
const NOTES = [
  { id: 'h1', key: A, time: iso(T(20)), kind: 'health', from: 'active', to: 'failed', error_class: 'rate_limit', evidence: 'last turn ended in an API error' },
  { id: 'open-in_1', key: A, time: iso(T(20)), kind: 'inbox', event: 'open', item: 'in_1', item_kind: 'error', title: '会话失败：myproject' },
  { id: 'h2', key: A, time: iso(T(5)), kind: 'health', from: 'failed', to: 'idle' },
];
const ITEM = { id: 'in_1', kind: 'error', level: 'timely', unit: A, title: '会话失败：myproject', why: '失败', created: iso(T(20)), state: 'open', source: 'health:' + A };

test('side notes sit among the bubbles by time, arrive live and start no turn', async ({ page }) => {
  const mock = await startMockServer({
    ws: true,
    notes: { [A]: NOTES },
    inbox: { open: [ITEM], closed: [], counts: { open: 1, badge: 1 } },
    planner: { enabled: true, stopped: false, key: 'planner:main', alive: true },
  });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click(`.session-card[data-key="${A}"]`);
    const conn = mock.wsConnections[mock.wsConnections.length - 1];
    await expect.poll(() => conn.messages.filter((m) => m.type === 'subscribe' && m.key === A).length).toBe(1);
    conn.send({ type: 'subscribed', key: A, state: 'ready' });
    conn.send({ type: 'history', key: A, initial: true, events: EVENTS });

    const pane = page.locator('#events-scroll');
    await expect(pane.locator('.gw-note')).toHaveCount(2);
    // Between the first exchange and the second, and after the last. The
    // failure and the item it opened are one note, the item's.
    const order = await pane.evaluate((el) => [...el.children]
      .filter((c) => c.matches('.event.user, .event.text, .gw-note'))
      .map((c) => c.dataset.noteId || c.dataset.uuid));
    expect(order).toEqual(['u1', 't1', 'open-in_1', 'u2', 't2', 'h2']);
    await expect(pane.locator('[data-note-id="open-in_1"]')).toHaveText(/进入「失败」：限流/);
    await expect(pane.locator('[data-note-id="h2"]')).toContainText('已从「失败」恢复');
    await expect(pane.locator('[data-inbox-item="in_1"]')).toHaveAttribute('data-note-id', 'open-in_1');
    // It sits in the session the item is about: no link to the Planner page.
    await expect(pane.locator('.gw-note button')).toHaveCount(0);
    // Notes are not bubbles: the transcript's own bookkeeping never sees them.
    await expect(pane.locator('.event.gw-note')).toHaveCount(0);

    // A live note lands at the end, and nothing reads as a running turn.
    conn.send({ type: 'session_note', key: A, note: { id: 'h3', key: A, time: iso(Date.now()), kind: 'health', from: 'idle', to: 'waiting' } });
    await expect(pane.locator('.gw-note').last()).toHaveAttribute('data-note-id', 'h3');
    await expect(pane.locator('[data-note-id="h3"]')).toContainText('进入「等你」');
    await expect(page.locator('#running-banner')).toHaveClass(/nz-hidden/);
    await expect(page.locator('#btn-stop')).toBeHidden();
    await expect(page.locator('#rb-elapsed')).toHaveText('');
    // A note for another session is not placed here.
    conn.send({ type: 'session_note', key: 'dashboard:direct:other:general', note: { id: 'x1', key: 'dashboard:direct:other:general', time: iso(Date.now()), kind: 'dispatch' } });

    // Live, the failure's note comes first and its item a frame later: the
    // two still end as one note, the item's, naming the death in words.
    const at = Date.now();
    conn.send({ type: 'session_note', key: A, note: { id: 'h5', key: A, time: iso(at), kind: 'health', from: 'idle', to: 'failed', error_class: 'other', evidence: 'death_reason=cli_exited_code_-1' } });
    await expect(pane.locator('[data-note-id="h5"]')).toHaveText(/进入「失败」：CLI 进程被信号终止/);
    conn.send({ type: 'session_note', key: A, note: { id: 'open-in_2', key: A, time: iso(at + 5), kind: 'inbox', event: 'open', item: 'in_2', item_kind: 'error', title: 'myproject 失败' } });
    await expect(pane.locator('[data-note-id="h5"]')).toHaveCount(0);
    await expect(pane.locator('[data-note-id="open-in_2"]')).toHaveText(/进入「失败」：CLI 进程被信号终止/);
    await expect(pane.locator('[data-note-id="open-in_2"]')).not.toContainText('其他');
    await expect(pane.locator('[data-note-id="open-in_2"]')).toHaveAttribute('title', / · CLI 进程被信号终止$/);

    // The notes survive a repaint, and a close note is its own.
    await page.click('#abnav-planner');
    await page.click('#abnav-chat');
    await page.click(`.session-card[data-key="${A}"]`);
    await expect(pane.locator('.gw-note')).toHaveCount(4);
    conn.send({ type: 'session_note', key: A, note: { id: 'close-in_1', key: A, time: iso(Date.now()), kind: 'inbox', event: 'close', item: 'in_1', item_kind: 'error', closed_by: 'user', title: '会话失败：myproject' } });
    await expect(pane.locator('[data-note-id="close-in_1"]')).toContainText('通知 · 已关闭 · 会话失败：myproject');
    await expect(pane.locator('[data-note-id="x1"]')).toHaveCount(0);
    await expect(page.locator('#running-banner')).toHaveClass(/nz-hidden/);

    // A note pushed while the socket is down comes with the next one's list.
    NOTES.push({ id: 'h4', key: A, time: iso(Date.now()), kind: 'health', from: 'waiting', to: 'stalled' });
    const conns = mock.wsConnections.length;
    conn.close();
    await expect.poll(() => mock.wsConnections.length).toBeGreaterThan(conns);
    await expect(pane.locator('[data-note-id="h4"]')).toContainText('进入「停滞」');
    await expect(page.locator('#running-banner')).toHaveClass(/nz-hidden/);
  } finally {
    mock.server.close();
  }
});

// openSession opens A and feeds its history; it returns the socket.
async function openSession(page, mock) {
  await page.goto(mock.url + '/dashboard');
  await waitForWs(page, 'CONNECTED');
  await page.click(`.session-card[data-key="${A}"]`);
  const conn = mock.wsConnections[mock.wsConnections.length - 1];
  await expect.poll(() => conn.messages.filter((m) => m.type === 'subscribe' && m.key === A).length).toBe(1);
  conn.send({ type: 'subscribed', key: A, state: 'ready' });
  conn.send({ type: 'history', key: A, initial: true, events: EVENTS });
  return conn;
}

const P1 = 'p0123456789ab', P2 = 'pfedcba987654', P3 = 'p00000000000c';

test('a Planner note shows its words, folds its why, and takes 没用', async ({ page }) => {
  const notes = [
    { id: P1, key: A, time: iso(T(20)), kind: 'planner', text: '这个改法和 task:ab12 的结论冲突 <b>', why: 'task:ab12 的摘要：已改成方案 B <b>x</b>' },
    { id: P2, key: A, time: iso(T(5)), kind: 'planner', text: '你答应过今天回复张三', why: '承诺 pr_1', useless: true },
  ];
  const mock = await startMockServer({
    ws: true,
    notes: { [A]: notes },
    planner: { enabled: true, stopped: false, key: 'planner:main', alive: true },
  });
  try {
    const conn = await openSession(page, mock);
    const pane = page.locator('#events-scroll');
    const order = () => pane.evaluate((el) => [...el.children]
      .filter((c) => c.matches('.event.user, .event.text, .gw-note'))
      .map((c) => c.dataset.noteId || c.dataset.uuid));
    await expect.poll(order).toEqual(['u1', 't1', P1, 'u2', 't2', P2]);

    // Marked Planner, the words as text (never markup), the why folded.
    const n1 = pane.locator(`[data-note-id="${P1}"]`);
    await expect(n1).toHaveClass(/gw-note-pl/);
    await expect(n1.locator('.gw-note-by')).toHaveText('Planner');
    await expect(n1.locator('.gw-note-text')).toHaveText('这个改法和 task:ab12 的结论冲突 <b>');
    await expect(n1.locator('.gw-note-text b')).toHaveCount(0);
    const why = n1.locator('.gw-note-why');
    await expect(why.locator('summary')).toHaveText('依据');
    await expect(why.locator('div')).toBeHidden();
    await why.locator('summary').click();
    await expect(why.locator('div')).toBeVisible();
    // The why can quote outside content (a scout's turn): text, never markup.
    await expect(why.locator('div')).toHaveText('task:ab12 的摘要：已改成方案 B <b>x</b>');
    await expect(why.locator('b')).toHaveCount(0);
    // No inline handler: the button is a delegated data-action.
    await expect(n1.locator('button')).toHaveText('没用');
    await expect(n1.locator('[onclick]')).toHaveCount(0);

    // One listed as marked says so and has no button.
    const n2 = pane.locator(`[data-note-id="${P2}"]`);
    await expect(n2).toHaveClass(/gw-note-off/);
    await expect(n2.locator('.gw-note-done')).toHaveText('已标没用');
    await expect(n2.locator('button')).toHaveCount(0);

    // 没用: one POST naming the session and the note; the note stays where it
    // stood, marked.
    await n1.locator('button').click();
    await expect(n1.locator('.gw-note-done')).toHaveText('已标没用');
    await expect(n1.locator('button')).toHaveCount(0);
    await expect.poll(() => mock.noteUselessCalls).toEqual([{ key: A, id: P1 }]);
    expect(await order()).toEqual(['u1', 't1', P1, 'u2', 't2', P2]);

    // A live one lands at the end with its button, and starts no turn.
    conn.send({ type: 'session_note', key: A, note: { id: P3, key: A, time: iso(Date.now()), kind: 'planner', text: '它说做完了，PR 还没合', why: 'gh pr view 12：OPEN' } });
    const n3 = pane.locator(`[data-note-id="${P3}"]`);
    await expect(n3.locator('.gw-note-text')).toHaveText('它说做完了，PR 还没合');
    await expect(n3.locator('button')).toHaveText('没用');
    await expect(pane.locator('.gw-note').last()).toHaveAttribute('data-note-id', P3);
    await expect(page.locator('#running-banner')).toHaveClass(/nz-hidden/);
    await expect(pane.locator('.event.gw-note')).toHaveCount(0);

    // A mark made elsewhere comes with the next list read.
    notes.push({ id: P3, key: A, time: iso(Date.now()), kind: 'planner', text: '它说做完了，PR 还没合', why: 'gh pr view 12：OPEN', useless: true });
    const conns = mock.wsConnections.length;
    conn.close();
    await expect.poll(() => mock.wsConnections.length).toBeGreaterThan(conns);
    await expect(n3.locator('.gw-note-done')).toHaveText('已标没用');
    await expect(pane.locator('.gw-note')).toHaveCount(3);
  } finally {
    mock.server.close();
  }
});

test('没用 that is not kept is taken back', async ({ page }) => {
  const mock = await startMockServer({
    ws: true,
    notes: { [A]: [{ id: P1, key: A, time: iso(T(20)), kind: 'planner', text: '和你记下的决定相悖', why: '记忆 release_rule' }] },
    noteUselessFail: true,
    planner: { enabled: true, stopped: false, key: 'planner:main', alive: true },
  });
  try {
    await openSession(page, mock);
    const n1 = page.locator(`#events-scroll [data-note-id="${P1}"]`);
    await n1.locator('button').click();
    await expect.poll(() => mock.noteUselessCalls).toEqual([{ key: A, id: P1 }]);
    await expect(page.locator('#toast')).toHaveText('没记下，请再点一次');
    await expect(n1.locator('button')).toHaveText('没用');
    await expect(n1.locator('.gw-note-done')).toHaveCount(0);
    await expect(n1).not.toHaveClass(/gw-note-off/);
  } finally {
    mock.server.close();
  }
});

// The primary's page lists the notes of a session on a node running its own
// Planner from that node's journal; 没用 is clicked on that node's own page,
// so none shows here and nothing is posted.
test('a remote node\'s Planner note shows without 没用', async ({ page }) => {
  const NODE = 'pc', R = 'dashboard:direct:2026-01-02-090000-1:general';
  const s = defaultSessions();
  s.sessions.push({
    key: R, node: NODE, state: 'ready', platform: 'dashboard', agent: 'general', cli_name: 'claude',
    workspace: '/Users/me/workspace/naozhi', project: 'naozhi', last_active: now - 60000, last_prompt: 'remote',
  });
  s.nodes = { local: { display_name: 'EC2', status: 'ok' }, [NODE]: { display_name: 'MBP', status: 'ok', caps: ['planner', 'subscribe-history'] } };
  const mock = await startMockServer({
    ws: true, sessions: s,
    planner: { enabled: true, stopped: false, key: 'planner:main', alive: true },
    remote: { [NODE]: {
      planner: { enabled: true, stopped: false, key: 'planner:main', alive: true },
      notes: { [R]: [{ id: P1, key: R, time: iso(T(20)), kind: 'planner', text: '它说做完了，PR 还没合', why: 'gh pr view 12：OPEN' }] },
    } },
  });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click(`.session-card[data-node="${NODE}"][data-key="${R}"]`);
    const n1 = page.locator(`#events-scroll [data-note-id="${P1}"]`);
    await expect(n1.locator('.gw-note-by')).toHaveText('Planner');
    await expect(n1.locator('.gw-note-text')).toHaveText('它说做完了，PR 还没合');
    await expect(n1.locator('.gw-note-why summary')).toHaveText('依据');
    await expect(n1.locator('button')).toHaveCount(0);
    await expect(n1.locator('.gw-note-done')).toHaveCount(0);
    expect(mock.noteUselessCalls).toEqual([]);
    expect(mock.remoteCalls.filter((c) => c.method !== 'GET')).toEqual([]);
  } finally {
    mock.server.close();
  }
});

// The gateway's progress notes: 后台中 and the line as text, no button; a
// later note of the same stretch (run) takes the earlier one's place, in the
// list read and live, and another stretch's note is its own.
test('a stretch of background work shows its latest progress note only', async ({ page }) => {
  const line = (min, done) => '已跑 ' + min + ' 分钟 · 工作流 review-all：12 个子代理完成 ' + done + ' 个，阶段「验证」';
  const notes = [
    { id: 'g1', key: A, time: iso(T(25)), kind: 'progress', run: 'r1', text: line(30, 3) + ' <b>x</b>' },
    { id: 'h1', key: A, time: iso(T(22)), kind: 'health', from: 'idle', to: 'waiting' },
    { id: 'g2', key: A, time: iso(T(15)), kind: 'progress', run: 'r1', text: line(40, 5) + ' <b>x</b>' },
    { id: 'g3', key: A, time: iso(T(5)), kind: 'progress', run: 'r2', text: '已跑 31 分钟 · 1 个后台任务：npm run bench' },
  ];
  const mock = await startMockServer({
    ws: true,
    notes: { [A]: notes },
    planner: { enabled: true, stopped: false, key: 'planner:main', alive: true },
  });
  try {
    const conn = await openSession(page, mock);
    const pane = page.locator('#events-scroll');
    const order = () => pane.evaluate((el) => [...el.children]
      .filter((c) => c.matches('.event.user, .event.text, .gw-note'))
      .map((c) => c.dataset.noteId || c.dataset.uuid));
    await expect.poll(order).toEqual(['u1', 't1', 'h1', 'g2', 'u2', 't2', 'g3']);
    const g2 = pane.locator('[data-note-id="g2"]');
    await expect(g2.locator('.gw-note-text')).toHaveText('后台中 · ' + line(40, 5) + ' <b>x</b>');
    await expect(g2.locator('.gw-note-text b')).toHaveCount(0);
    await expect(g2).not.toHaveClass(/gw-note-pl/);
    await expect(pane.locator('.gw-note button')).toHaveCount(0);
    await expect(pane.locator('.gw-note-by')).toHaveCount(0);

    // Live: the next note of r2 replaces g3 where the conversation ends,
    // and starts no turn; r1's stays where it was.
    conn.send({ type: 'session_note', key: A, note: { id: 'g4', key: A, time: iso(Date.now()), kind: 'progress', run: 'r2', text: '已跑 1 小时 1 分钟 · 1 个后台任务：npm run bench' } });
    await expect.poll(order).toEqual(['u1', 't1', 'h1', 'g2', 'u2', 't2', 'g4']);
    await expect(pane.locator('[data-note-id="g4"] .gw-note-text')).toHaveText('后台中 · 已跑 1 小时 1 分钟 · 1 个后台任务：npm run bench');
    await expect(page.locator('#running-banner')).toHaveClass(/nz-hidden/);
    // A note of a stretch after a new turn is one more.
    conn.send({ type: 'session_note', key: A, note: { id: 'g5', key: A, time: iso(Date.now() + 5), kind: 'progress', run: 'r3', text: '已跑 30 分钟 · 1 个后台任务：make' } });
    await expect.poll(order).toEqual(['u1', 't1', 'h1', 'g2', 'u2', 't2', 'g4', 'g5']);
    expect(mock.noteUselessCalls).toEqual([]);
  } finally {
    mock.server.close();
  }
});
