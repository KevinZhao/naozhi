// E2E: the session pane follows its newest content until the reader scrolls
// up, and again once they are back at the bottom (scroll_follow.js). Following
// is the pane's state, not a distance measured as each event arrives: the
// bottom moving away between two events (the banner, a render finishing) must
// not end it. A reader who moved up stays put through replies and through a
// message sent from elsewhere; their own send brings them back. Unfolding a
// tool result at the bottom leaves the reader on it.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const A = 'dashboard:direct:2026-01-01-120000-1:myproject';
const B = 'dashboard:direct:2026-01-01-120002-3:myproject';
const now = Date.now();
const T = (sec) => now - sec * 1000;
const para = (n, tag) => Array.from({ length: n }, (_, i) => tag + ' 第 ' + (i + 1) + ' 段。').join('\n\n');

const EVENTS = [];
for (let i = 0; i < 12; i++) {
  EVENTS.push({ type: 'user', detail: 'iso 问题 ' + i, time: T(4000 - i * 100), uuid: 'u' + i });
  EVENTS.push({ type: 'text', detail: para(6, 'iso 回答 ' + i), time: T(3990 - i * 100), uuid: 't' + i });
  EVENTS.push({ type: 'result', summary: 'done', time: T(3989 - i * 100), uuid: 'r' + i });
}

const pane = (page) => page.locator('#events-scroll');
const gap = (page) => pane(page).evaluate((el) => el.scrollHeight - el.scrollTop - el.clientHeight);
const top = (page) => pane(page).evaluate((el) => el.scrollTop);
const atBottom = (page) => expect.poll(() => gap(page)).toBeLessThanOrEqual(2);

async function open(page) {
  const mock = await startMockServer({ ws: true });
  await page.goto(mock.url + '/dashboard');
  await waitForWs(page, 'CONNECTED');
  await page.click(`.session-card[data-key="${A}"]`);
  const conn = mock.wsConnections[mock.wsConnections.length - 1];
  await expect.poll(() => conn.messages.filter((m) => m.type === 'subscribe' && m.key === A).length).toBe(1);
  conn.send({ type: 'subscribed', key: A, state: 'ready' });
  conn.send({ type: 'history', key: A, initial: true, events: EVENTS });
  await expect(pane(page).locator('.event')).not.toHaveCount(0);
  await atBottom(page);
  return { mock, conn };
}

let seq = 0;
function reply(conn, text) {
  seq++;
  conn.send({ type: 'history', key: A, events: [{ type: 'text', detail: text, time: now + seq * 1000, uuid: 'live-' + seq }] });
}

test('the pane follows replies though the bottom moved away between them', async ({ page }) => {
  const { mock, conn } = await open(page);
  try {
    // Content that grows in place is followed as it grows.
    await pane(page).evaluate((el) => {
      const d = document.createElement('div');
      d.style.height = '400px';
      el.lastElementChild.appendChild(d);
    });
    await atBottom(page);

    // The running banner takes its height off the pane.
    conn.send({ type: 'session_state', key: A, state: 'running' });
    await expect(page.locator('#running-banner')).toBeVisible();
    await atBottom(page);

    // The bottom moves away with nothing to tell the pane (a restyle): the
    // reader did not scroll, so the next reply is still followed.
    await pane(page).evaluate((el) => { el.lastElementChild.style.minHeight = '900px'; });
    expect(await gap(page)).toBeGreaterThan(100);
    reply(conn, 'iso 跟随中的回复');
    await expect(pane(page).getByText('iso 跟随中的回复')).toBeVisible();
    await atBottom(page);
  } finally {
    mock.server.close();
  }
});

test('a reader who scrolled up stays put, and is followed again from the bottom', async ({ page }) => {
  const { mock, conn } = await open(page);
  try {
    await pane(page).hover();
    await page.mouse.wheel(0, -700);
    await expect.poll(() => gap(page)).toBeGreaterThan(300);
    const held = await top(page);

    reply(conn, para(5, 'iso 翻上去时来的回复'));
    await expect(pane(page).locator('.event[data-uuid="live-' + seq + '"]')).toHaveCount(1);
    expect(await top(page)).toBe(held);

    // A message sent from elsewhere (IM, another tab) does not move them either.
    conn.send({ type: 'history', key: A, events: [{ type: 'user', detail: 'iso 别处发来的消息', time: now + 500000, uuid: 'peer-1' }] });
    await expect(pane(page).locator('.event[data-uuid="peer-1"]')).toHaveCount(1);
    seq = 600;
    reply(conn, 'iso 对别处消息的回复');
    await expect(pane(page).locator('.event[data-uuid="live-' + seq + '"]')).toHaveCount(1);
    expect(await top(page)).toBe(held);

    // Back at the bottom, the pane follows again.
    await page.mouse.wheel(0, 100000);
    await atBottom(page);
    reply(conn, para(5, 'iso 回到底部后的回复'));
    await expect(pane(page).locator('.event[data-uuid="live-' + seq + '"]')).toBeVisible();
    await atBottom(page);

    // Their own send brings a reader who moved up back to the bottom.
    await page.mouse.wheel(0, -700);
    await expect.poll(() => gap(page)).toBeGreaterThan(300);
    await page.locator('#msg-input').click();
    await page.keyboard.type('iso 我自己发的');
    await page.keyboard.press('Enter');
    await expect(pane(page).locator('.optimistic-msg')).toBeVisible();
    await atBottom(page);
  } finally {
    mock.server.close();
  }
});

test('unfolding a tool result at the bottom leaves the reader on it', async ({ page }) => {
  const { mock, conn } = await open(page);
  try {
    conn.send({ type: 'history', key: A, events: [{ type: 'tool_result', summary: 'iso 工具输出', detail: para(60, 'iso 输出'), time: now + 1000, uuid: 'tr-1' }] });
    const fold = pane(page).locator('details.tr-wrap');
    await expect(fold).toBeVisible();
    await atBottom(page);

    await fold.locator('summary').click();
    await expect.poll(() => gap(page)).toBeGreaterThan(300);
    const held = await top(page);
    seq = 900;
    reply(conn, 'iso 展开后来的回复');
    await expect(pane(page).locator('.event[data-uuid="live-' + seq + '"]')).toHaveCount(1);
    expect(await top(page)).toBe(held);
    await expect(fold.locator('summary')).toBeInViewport();
  } finally {
    mock.server.close();
  }
});

test('a session left and opened again: followed if it was, back mid-history if the reader was there', async ({ page }) => {
  const { mock, conn } = await open(page);
  const reopen = async (events) => {
    await page.click(`.session-card[data-key="${B}"]`);
    await expect.poll(() => conn.messages.filter((m) => m.type === 'subscribe' && m.key === B).length).toBeGreaterThan(0);
    const subs = conn.messages.filter((m) => m.type === 'subscribe' && m.key === A).length;
    await page.click(`.session-card[data-key="${A}"]`);
    await expect.poll(() => conn.messages.filter((m) => m.type === 'subscribe' && m.key === A).length).toBe(subs + 1);
    conn.send({ type: 'subscribed', key: A, state: 'ready' });
    conn.send({ type: 'history', key: A, initial: true, events });
    await expect(pane(page).locator('.event')).not.toHaveCount(0);
  };
  try {
    await reopen(EVENTS);
    await atBottom(page);
    seq = 1200;
    reply(conn, para(5, 'iso 切回来后的回复'));
    await expect(pane(page).locator('.event[data-uuid="live-' + seq + '"]')).toBeVisible();
    await atBottom(page);

    // A reader who moved up comes back to the bubble they were at, and new
    // replies leave them there.
    await pane(page).hover();
    await page.mouse.wheel(0, -700);
    await expect.poll(() => gap(page)).toBeGreaterThan(300);
    const anchor = await pane(page).evaluate((el) => {
      const t = el.getBoundingClientRect().top;
      const a = [...el.querySelectorAll(':scope > .event[data-uuid]')].find((b) => b.getBoundingClientRect().bottom > t);
      return { uuid: a.getAttribute('data-uuid'), off: Math.round(a.getBoundingClientRect().top - t) };
    });
    const offset = () => pane(page).evaluate((el, uuid) => {
      const a = el.querySelector(':scope > .event[data-uuid="' + uuid + '"]');
      return Math.round(a.getBoundingClientRect().top - el.getBoundingClientRect().top);
    }, anchor.uuid);
    await reopen(EVENTS.concat([{ type: 'text', detail: para(5, 'iso 切走时来的回复'), time: now + 1300000, uuid: 'away-1' }]));
    await expect.poll(async () => Math.abs((await offset()) - anchor.off)).toBeLessThanOrEqual(2);
    seq = 1400;
    reply(conn, para(5, 'iso 回到原位后的回复'));
    await expect(pane(page).locator('.event[data-uuid="live-' + seq + '"]')).toHaveCount(1);
    expect(Math.abs((await offset()) - anchor.off)).toBeLessThanOrEqual(2);
  } finally {
    mock.server.close();
  }
});

test('the Planner conversation follows the same way', async ({ page }) => {
  const PLANNER = 'planner:main';
  const events = [];
  for (let i = 0; i < 10; i++) {
    events.push({ type: 'user', detail: 'iso 问 Planner ' + i, time: T(4000 - i * 100), uuid: 'pu' + i });
    events.push({ type: 'text', detail: para(6, 'iso Planner 答 ' + i), time: T(3990 - i * 100), uuid: 'pt' + i });
    events.push({ type: 'result', summary: 'done', time: T(3989 - i * 100), uuid: 'pr' + i });
  }
  const mock = await startMockServer({
    ws: true, plannerEvents: events, tasks: { tasks: [] }, inbox: { open: [], closed: [], counts: { open: 0, badge: 0 } },
    planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
  });
  const pl = page.locator('#pl-events');
  const plGap = () => pl.evaluate((el) => el.scrollHeight - el.scrollTop - el.clientHeight);
  const say = (conn, text, uuid) => conn.send({ type: 'event', key: PLANNER, event: { type: 'text', detail: text, time: Date.now(), uuid } });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click('#abnav-planner');
    await expect(page.locator('#pl-stream .event.text')).toHaveCount(10);
    const conn = mock.wsConnections[mock.wsConnections.length - 1];
    await expect.poll(() => conn.messages.some((m) => m.type === 'subscribe' && m.key === PLANNER)).toBe(true);
    conn.send({ type: 'subscribed', key: PLANNER, state: 'ready' });
    await expect.poll(plGap).toBeLessThanOrEqual(2);

    // The bottom moved away with no scroll of the reader's: still followed.
    await page.locator('#pl-stream .event.text').last().evaluate((el) => { el.style.minHeight = '900px'; });
    expect(await plGap()).toBeGreaterThan(100);
    say(conn, 'iso Planner 跟随中的回复', 'pl-live-1');
    await expect(page.locator('#pl-stream').getByText('iso Planner 跟随中的回复')).toBeVisible();
    await expect.poll(plGap).toBeLessThanOrEqual(2);

    // Scrolled up, the reader stays; back at the bottom, followed again.
    await pl.hover();
    await page.mouse.wheel(0, -700);
    await expect.poll(plGap).toBeGreaterThan(300);
    const held = await pl.evaluate((el) => el.scrollTop);
    say(conn, para(5, 'iso Planner 翻上去时的回复'), 'pl-live-2');
    await expect(page.locator('#pl-stream .event[data-uuid="pl-live-2"]')).toHaveCount(1);
    expect(await pl.evaluate((el) => el.scrollTop)).toBe(held);
    await page.mouse.wheel(0, 100000);
    await expect.poll(plGap).toBeLessThanOrEqual(2);
    say(conn, para(5, 'iso Planner 回到底部后的回复'), 'pl-live-3');
    await expect(page.locator('#pl-stream .event[data-uuid="pl-live-3"]')).toBeVisible();
    await expect.poll(plGap).toBeLessThanOrEqual(2);
  } finally {
    mock.server.close();
  }
});
