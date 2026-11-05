// E2E: the gateway's side notes in the session view (session_notes.js,
// planner RFC §15.3 ③): GET /api/sessions/notes places them among the
// bubbles by time, a session_note frame adds one live, neither starts a turn,
// a health transition and the item it opened are one note naming the cause,
// no note links back to the Planner page (its card only opens this session
// again), and a new socket lists the notes pushed while it was down.
// data-inbox-item anchors a note to its item for a notification's click.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
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
    inbox: { open: [ITEM], closed: [], counts: { open: 1, badge: 0 } },
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
    await expect(pane.locator('[data-note-id="h3"]')).toContainText('进入「待你回复」');
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
