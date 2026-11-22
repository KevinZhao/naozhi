// E2E: the Planner page's writes show at once, ahead of the gateway, and a
// failure puts back what was shown with a toast saying why: closing and
// answering a 等你 card, the one pause of pushes and custody and its
// 恢复, a line of work's 编辑 and 结束, and the session header's 托管 and
// 观察 chips. Each request is held (page.route) so what shows before it
// answers is told apart from what its answer repaints; a read of the list
// let through meanwhile still says what was, and must not put it back.
const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const PLANNER = 'planner:main';
const A = 'dashboard:direct:2026-01-01-120000-1:myproject';
const now = Date.now();
const iso = (ms) => new Date(ms).toISOString();
const planner = { enabled: true, stopped: false, key: PLANNER, alive: true };

function inbox() {
  return {
    open: [
      { id: 'in_err', kind: 'error', level: 'timely', unit: 'cron:nightly', title: 'iso nightly 失败', why: '定时任务运行失败', created: iso(now - 3 * 3600000), state: 'open', source: 'health:cron:nightly' },
      { id: 'in_q', kind: 'question', level: 'timely', unit: A, title: 'iso 会话在等你回复', why: '有提问等你回答超过 10 分钟', created: iso(now - 12 * 60000), state: 'open', source: 'waiting:' + A },
      { id: 'in_d', kind: 'decision', level: 'timely', title: 'iso 周报用哪个模板？', why: 'Planner 问你', created: iso(now - 5 * 60000), state: 'open', source: 'planner:d1', decision: { handler: 'ask', actions: ['模板 A', '模板 B'] } },
    ],
    closed: [],
    counts: { open: 3, badge: 3 },
  };
}

// holdPath holds the page's method requests to path until release(); with
// status set it answers the next ones with that instead, the gateway left
// untouched. held counts the requests taken. The latest one set up takes a
// request first; another method goes on to the ones before it.
async function holdPath(page, path, method) {
  const h = { status: 0, held: 0 };
  let open;
  h.gate = new Promise((r) => { open = r; });
  h.release = () => open();
  await page.route((u) => u.pathname === path, async (route) => {
    if (route.request().method() !== method) { await route.fallback(); return; }
    if (h.status) { await route.fulfill({ status: h.status, contentType: 'text/plain', body: 'boom' }); return; }
    h.held++;
    await h.gate;
    await route.continue();
  });
  return h;
}
async function openPlanner(page, overrides) {
  const mock = await startMockServer({ ws: true, planner, plannerEvents: [], tasks: { tasks: [] }, ...overrides });
  await page.goto(mock.url + '/dashboard');
  await waitForWs(page, 'CONNECTED');
  await page.click('#abnav-planner');
  return mock;
}

// read runs one of the page's own reads of a list to its end: what a poll,
// a count frame or a turn's end would start.
const read = (page, mod, fn) => page.evaluate(async ([m, f]) => { await (await import('/static/' + m))[f](); }, [mod, fn]);

test('等你: a card closes and an answer goes at once; a failure puts the card back', async ({ page }) => {
  const box = inbox();
  const mock = await openPlanner(page, { inbox: box });
  try {
    const cards = page.locator('#pl-wait .pl-card');
    await expect(cards).toHaveCount(3);
    await expect(page.locator('#abnav-planner-count')).toHaveText('3');

    // Closing: gone with its count before the gateway answers, and a list
    // read meanwhile (still holding it) does not bring it back.
    const close = await holdPath(page, '/api/inbox/close', 'POST');
    await page.click('.pl-card[data-id="in_err"] .pl-close');
    await expect(page.locator('.pl-card[data-id="in_err"]')).toHaveCount(0);
    await expect(page.locator('#pl-wait-h')).toHaveText('等你 2');
    await expect(page.locator('#abnav-planner-count')).toHaveText('2');
    expect(close.held).toBe(1);
    expect(mock.inboxCalls).toEqual([]);
    await read(page, 'planner_view.js', 'fetchInbox');
    expect(box.open.map((it) => it.id)).toContain('in_err');
    await expect(page.locator('.pl-card[data-id="in_err"]')).toHaveCount(0);
    await expect(page.locator('#abnav-planner-count')).toHaveText('2');
    close.release();
    await expect.poll(() => mock.inboxCalls).toEqual([{ path: '/api/inbox/close', method: 'POST', body: { id: 'in_err' } }]);
    await expect(cards).toHaveCount(2);
    await expect(page.locator('#pl-wait-h')).toHaveText('等你 2');

    // Answering: the decision is gone at once, both answers with it.
    const decide = await holdPath(page, '/api/inbox/decide', 'POST');
    await page.click('.pl-card[data-id="in_d"] [data-act="模板 B"]');
    await expect(page.locator('.pl-card[data-id="in_d"]')).toHaveCount(0);
    await expect(page.locator('#pl-wait-h')).toHaveText('等你 1');
    await expect(page.locator('#abnav-planner-count')).toHaveText('1');
    expect(decide.held).toBe(1);
    await read(page, 'planner_view.js', 'fetchInbox');
    await expect(page.locator('.pl-card[data-id="in_d"]')).toHaveCount(0);
    decide.release();
    await expect.poll(() => mock.inboxCalls.length).toBe(2);
    expect(mock.inboxCalls[1]).toEqual({ path: '/api/inbox/decide', method: 'POST', body: { id: 'in_d', action: '模板 B' } });
    await expect(cards).toHaveCount(1);

    // A close that fails: the card comes back as it was, with its count,
    // and the toast says so.
    close.status = 500;
    await page.click('.pl-card[data-id="in_q"] .pl-close');
    await expect(page.locator('#toast')).toHaveText(/关闭失败：/);
    await expect(page.locator('.pl-card[data-id="in_q"]')).toHaveCount(1);
    await expect(page.locator('.pl-card[data-id="in_q"] .pl-close')).toBeEnabled();
    await expect(page.locator('#pl-wait-h')).toHaveText('等你 1');
    await expect(page.locator('#abnav-planner-count')).toHaveText('1');

    // Closed on the gateway already (404: another tab closed it, no frame
    // told this one yet): what was asked, no error.
    box.open = box.open.filter((it) => it.id !== 'in_q');
    box.counts = { open: 0, badge: 0 };
    close.status = 404;
    await page.evaluate(() => { document.getElementById('toast').className = 'toast'; });
    await page.click('.pl-card[data-id="in_q"] .pl-close');
    await expect(page.locator('.pl-card[data-id="in_q"]')).toHaveCount(0);
    await expect(page.locator('#pl-wait .pl-empty')).toHaveText('没有等你的事');
    await expect(page.locator('#toast')).not.toHaveClass(/show/);
  } finally {
    mock.server.close();
  }
});

test('等你: an answer that fails puts the decision back, its answers live', async ({ page }) => {
  const mock = await openPlanner(page, { inbox: inbox() });
  try {
    await expect(page.locator('#pl-wait .pl-card')).toHaveCount(3);
    const decide = await holdPath(page, '/api/inbox/decide', 'POST');
    decide.status = 500;
    await page.click('.pl-card[data-id="in_d"] [data-act="模板 A"]');
    await expect(page.locator('#toast')).toHaveText(/没有生效：/);
    const card = page.locator('#pl-wait .pl-card[data-id="in_d"]');
    await expect(card).toHaveCount(1);
    await expect(card.locator('.pl-card-acts button')).toHaveText(['模板 A', '模板 B']);
    await expect(card.locator('.pl-card-acts button').first()).toBeEnabled();
    await expect(page.locator('#pl-wait-h')).toHaveText('等你 3');
    await expect(page.locator('#abnav-planner-count')).toHaveText('3');
  } finally {
    mock.server.close();
  }
});

test('the one pause shows at once, the gateway\'s end then; 恢复 too; a failure puts it back', async ({ page }) => {
  const box = inbox();
  const mock = await openPlanner(page, { inbox: box, custody: { paused: false, sessions: [] } });
  try {
    await expect(page.locator('#pl-wait .pl-card')).toHaveCount(3);
    const paused = page.locator('#pl-paused');
    await expect(paused).toBeHidden();

    // 今天: until the coming midnight as soon as it is clicked, the menu
    // folded; a list read meanwhile (no pause yet) leaves it.
    const pause = await holdPath(page, '/api/inbox/pause', 'PUT');
    await page.click('#pl-more > summary');
    await page.click('[data-action="planner-pause"][data-for="today"]');
    await expect(paused).toBeVisible();
    await expect(page.locator('#pl-tools')).toBeHidden();
    const midnight = await page.evaluate(() => { const d = new Date(); const m = new Date(d.getFullYear(), d.getMonth(), d.getDate() + 1); return (m.getMonth() + 1) + '/' + m.getDate() + ' 00:00'; });
    await expect(paused.locator('.pl-paused-to')).toHaveText('到 ' + midnight);
    expect(pause.held).toBe(1);
    await read(page, 'planner_view.js', 'fetchInbox');
    await read(page, 'custody.js', 'refreshCustody');
    await expect(paused.locator('.pl-paused-to')).toHaveText('到 ' + midnight);
    // The gateway's end is what shows once it answers (the mock pauses 6
    // hours for 今天).
    pause.release();
    await expect.poll(() => !!box.paused_until).toBe(true);
    const end = await page.evaluate((t) => { const d = new Date(t); const p = (n) => String(n).padStart(2, '0'); return p(d.getHours()) + ':' + p(d.getMinutes()); }, box.paused_until);
    await expect(paused.locator('.pl-paused-to')).toContainText(end);
    if (!midnight.endsWith(end)) await expect(paused.locator('.pl-paused-to')).not.toHaveText('到 ' + midnight);

    // 恢复: the line goes at once; neither list read meanwhile (both still
    // paused) brings it back.
    const resume = await holdPath(page, '/api/inbox/pause', 'DELETE');
    await page.click('#pl-paused [data-action="planner-pause-resume"]');
    await expect(paused).toBeHidden();
    expect(resume.held).toBe(1);
    await read(page, 'planner_view.js', 'fetchInbox');
    await read(page, 'custody.js', 'refreshCustody');
    await expect(paused).toBeHidden();
    resume.release();
    await expect.poll(() => mock.inboxCalls.map((c) => c.method)).toEqual(['PUT', 'DELETE']);
    await expect(paused).toBeHidden();

    // A pause that fails: shown, then taken back with a toast; nothing paused.
    pause.status = 500;
    await page.click('#pl-more > summary');
    await page.click('[data-action="planner-pause"][data-for="1h"]');
    await expect(page.locator('#toast')).toHaveText(/暂停失败：/);
    await expect(paused).toBeHidden();
    await page.click('#pl-more > summary');
    await expect(page.locator('[data-action="planner-pause"][data-for="1h"]')).toBeVisible();
    await page.click('#pl-stream');

    // A 恢复 that fails: the pause is back, until when it was.
    pause.status = 0;
    await page.click('#pl-more > summary');
    await page.click('[data-action="planner-pause"][data-for="1h"]');
    await expect(paused).toBeVisible();
    await expect.poll(() => mock.inboxCalls.length).toBe(3);
    const was = await paused.locator('.pl-paused-to').textContent();
    resume.status = 500;
    await page.click('#pl-paused [data-action="planner-pause-resume"]');
    await expect(page.locator('#toast')).toHaveText(/恢复失败：/);
    await expect(paused).toBeVisible();
    await expect(paused.locator('.pl-paused-to')).toHaveText(was);
  } finally {
    mock.server.close();
  }
});

function fronts() {
  const t = Date.now();
  return {
    writable: true,
    fronts: [
      { id: 'jd-tgw', name: 'JD TGW 迁移', state: 'EC2 VXLAN 方案被否', next: '改迁移路线图', created: iso(t - 2 * 86400e3), updated: iso(t - 3600e3) },
      { id: 'blog', name: 'trn2 blog', state: '草稿完成', created: iso(t - 9 * 86400e3), updated: iso(t - 2 * 3600e3) },
      { id: 'eval', name: '搜索评测', state: '跑完一轮', created: iso(t - 9 * 86400e3), updated: iso(t - 3 * 3600e3) },
    ],
  };
}

test('工作线: an edit and 结束 show at once; a failure puts the line back', async ({ page }) => {
  const data = fronts();
  const mock = await openPlanner(page, { fronts: data });
  try {
    const group = page.locator('#pl-front');
    const cards = group.locator('.fr-card');
    await expect(cards).toHaveCount(3);
    const post = await holdPath(page, '/api/planner/fronts', 'POST');

    // 编辑 of the second line: saved, it shows the edit and goes first (the
    // latest updated), the editor closed, before the gateway answers; a
    // read meanwhile (the line as it was) does not undo it.
    const blog = group.locator('.fr-card[data-id="blog"]');
    await blog.locator('.tk-more').click();
    await blog.locator('[data-action="front-edit"]').click();
    await blog.locator('textarea[data-field="state"]').fill('发出去了');
    await blog.locator('[type="submit"]').click();
    await expect(blog.locator('.fr-edit')).toHaveCount(0);
    await expect(blog.locator('.fr-state')).toHaveText('发出去了');
    await expect(cards.nth(0)).toHaveAttribute('data-id', 'blog');
    expect(post.held).toBe(1);
    await read(page, 'fronts.js', 'refreshFronts');
    await expect(blog.locator('.fr-state')).toHaveText('发出去了');
    await expect(cards.nth(0)).toHaveAttribute('data-id', 'blog');
    post.release();
    await expect.poll(() => mock.frontsCalls.length).toBe(1);
    await expect(blog.locator('.fr-state')).toHaveText('发出去了');

    // 结束: off the list with its count at once, and a read meanwhile
    // does not bring it back.
    const evalCard = group.locator('.fr-card[data-id="eval"]');
    await evalCard.locator('.tk-more').click();
    const post2 = await holdPath(page, '/api/planner/fronts', 'POST');
    await evalCard.locator('[data-action="front-close"]').click();
    await expect(evalCard).toHaveCount(0);
    await expect(group.locator('.pl-group-h')).toHaveText('工作线 2');
    expect(post2.held).toBe(1);
    await read(page, 'fronts.js', 'refreshFronts');
    await expect(evalCard).toHaveCount(0);
    await expect(group.locator('.pl-group-h')).toHaveText('工作线 2');
    post2.release();
    await expect.poll(() => mock.frontsCalls.length).toBe(2);
    expect(mock.frontsCalls[1]).toEqual({ id: 'eval', close: true });
    await expect(cards).toHaveCount(2);

    // An edit that fails: the line as it was, where it was, the editor back
    // on what was typed, and a toast.
    await page.unrouteAll({ behavior: 'ignoreErrors' });
    await page.route((u) => u.pathname === '/api/planner/fronts', (route) => (route.request().method() === 'POST'
      ? route.fulfill({ status: 500, contentType: 'text/plain', body: 'boom' }) : route.continue()));
    const jd = group.locator('.fr-card[data-id="jd-tgw"]');
    await jd.locator('.tk-more').click();
    await jd.locator('[data-action="front-edit"]').click();
    await jd.locator('textarea[data-field="next"]').fill('写新路线图');
    await jd.locator('[type="submit"]').click();
    await expect(page.locator('#toast')).toHaveText(/没有保存：/);
    await expect(jd.locator('textarea[data-field="next"]')).toHaveValue('写新路线图');
    await jd.locator('[data-action="front-edit-cancel"]').click();
    await expect(jd.locator('.fr-meta')).toHaveText('→ 改迁移路线图');

    // A 结束 that fails: the line is back, its controls open (still open
    // from the edit).
    await jd.locator('[data-action="front-close"]').click();
    await expect(page.locator('#toast')).toHaveText(/没有结束：/);
    await expect(jd).toHaveCount(1);
    await expect(jd.locator('[data-action="front-close"]')).toBeVisible();
    await expect(group.locator('.pl-group-h')).toHaveText('工作线 2');
  } finally {
    mock.server.close();
  }
});

async function openSession(page, mock, key) {
  await page.evaluate((k) => window.nz.test.selectSession(k, 'local'), key);
  const conn = mock.wsConnections[mock.wsConnections.length - 1];
  await expect.poll(() => conn.messages.some((m) => m.type === 'subscribe' && m.key === key)).toBe(true);
  conn.send({ type: 'subscribed', key, state: 'ready' });
  conn.send({ type: 'history', key, initial: true, events: [{ type: 'text', detail: 'iso 跑着', time: now - 20000, uuid: 'o-t1' }] });
}

test('托管 and 观察 switch at once; the gateway\'s answer then; a failure puts the chip back', async ({ page }) => {
  const custody = { paused: false, sessions: [{ key: A, on: false, state: 'stopped', why: '连续 3 次没有进展', sent: 3, max: 8 }] };
  const mock = await startMockServer({ ws: true, sessions: defaultSessions(), planner, plannerEvents: [], inbox: { open: [], closed: [], counts: { open: 0, badge: 0 } }, custody });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await openSession(page, mock, A);
    const chip = page.locator('#header-custody .custody-chip');
    const watch = page.locator('#header-custody .watch-chip');
    await expect(chip).toHaveText('托管已停');
    await expect(watch).toHaveText('观察开');

    // A stopped one turns on afresh, said at once; a list read meanwhile
    // (still stopped) does not flip it back.
    const put = await holdPath(page, '/api/custody', 'PUT');
    await chip.click();
    await expect(chip).toHaveText('托管开');
    await expect(chip).toHaveAttribute('aria-pressed', 'true');
    await expect(chip).toBeEnabled();
    await expect(page.locator('#toast')).toHaveText('已重新开始托管，重新计数');
    expect(put.held).toBe(1);
    await read(page, 'custody.js', 'refreshCustody');
    expect(custody.sessions[0].state).toBe('stopped');
    await expect(chip).toHaveText('托管开');
    put.release();
    await expect.poll(() => mock.custodyCalls).toEqual([{ key: A, on: true }]);
    await expect(chip).toHaveText('托管开');
    await read(page, 'custody.js', 'refreshCustody');
    await expect(chip).toHaveText('托管开');

    // 观察 likewise.
    const put2 = await holdPath(page, '/api/custody', 'PUT');
    await watch.click();
    await expect(watch).toHaveText('观察关');
    await expect(watch).toHaveAttribute('aria-pressed', 'false');
    await expect(page.locator('#toast')).toHaveText('观察已关：不进任务、不写摘要（托管不受影响）');
    expect(put2.held).toBe(1);
    await read(page, 'custody.js', 'refreshCustody');
    await expect(watch).toHaveText('观察关');
    put2.release();
    await expect.poll(() => mock.custodyCalls.length).toBe(2);
    expect(mock.custodyCalls[1]).toEqual({ key: A, watch: false });
    await expect(watch).toHaveText('观察关');

    // A switch that fails: the chip as it was, and why.
    put2.status = 500;
    await chip.click();
    await expect(page.locator('#toast')).toHaveText(/没有关掉托管：/);
    await expect(chip).toHaveText('托管开');
    await expect(chip).toHaveAttribute('aria-pressed', 'true');
    await watch.click();
    await expect(page.locator('#toast')).toHaveText(/没有打开观察：/);
    await expect(watch).toHaveText('观察关');
    expect(mock.custodyCalls.length).toBe(2);
  } finally {
    mock.server.close();
  }
});
