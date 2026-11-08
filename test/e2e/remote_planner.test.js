// E2E: the Planner of another node (docs/rfc/remote-planner.md). A primary
// whose own Planner is off shows the Planner of the node advertising one:
// the session list groups by node, that node first, its dispatched session
// in its workspace's group;
// the Planner page reads and writes the node's routes through
// /api/remote/<node>/, subscribes to the node's planner:main, and a message
// sent there is dispatched and reported on the node and shown here; the
// node's 等你 items act on the node; a node that went offline says so and
// takes no message. Desktop and phone.
const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const PLANNER = 'planner:main';
const NODE = 'pc';
const OLD_TASK = 'task:aaaaaaaaaaaa';
const NEW_TASK = 'task:bbbbbbbbbbbb';
const MAC_WS = '/Users/me/workspace/naozhi';
const now = Date.now();
const iso = (ms) => new Date(ms).toISOString();

function sessions(status) {
  const s = defaultSessions();
  const remote = (key, extra) => Object.assign({
    key, node: NODE, state: 'ready', platform: 'dashboard', agent: 'general', cli_name: 'claude',
    workspace: MAC_WS, project: 'naozhi', last_active: now - 60000, last_prompt: 'iso remote ' + key,
  }, extra);
  s.sessions.push(
    remote(OLD_TASK, { platform: 'task', purpose: 'task', purpose_title: 'iso 整理 README', user_label: 'iso 整理 README', state: 'running', created_at: now - 90000 }),
    remote('dashboard:direct:2026-01-02-090000-1:general', { created_at: now - 80000 }),
  );
  s.nodes = {
    local: { display_name: 'EC2', status: 'ok' },
    [NODE]: { display_name: 'MBP', status: status || 'ok', caps: ['files', 'planner', 'subscribe-history'] },
  };
  return s;
}

function remote() {
  const r = {
    planner: { enabled: true, stopped: false, key: PLANNER, alive: true, workspaces: { ws_naozhi01: 'naozhi' } },
    tasks: { tasks: [{ id: OLD_TASK, kind: 'session', title: 'iso 整理 README', state: 'doing', at: iso(now - 90000), created: iso(now - 90000) }] },
    inbox: {
      open: [{ id: 'in_q1', kind: 'question', level: 'timely', unit: OLD_TASK, title: 'iso 要不要顺手改 CHANGELOG？', why: '会话在问你', created: iso(now - 120000), state: 'open', source: 'health:' + OLD_TASK }],
      closed: [], counts: { open: 1, badge: 1 },
    },
    custody: { paused: false, sessions: [{ key: OLD_TASK, on: true, state: 'on', by: 'auto', sent: 0, max: 8 }] },
    events: {
      [PLANNER]: [{ type: 'text', detail: 'iso 早上好，昨天的 README 还在整理。', time: now - 50000, uuid: 'r-t0' }],
      [NEW_TASK]: [
        { type: 'user', detail: '在 naozhi 里跑一遍 go vet', time: now - 1000, uuid: 'n-u1' },
        { type: 'text', detail: 'iso go vet 没有报错。', time: now - 500, uuid: 'n-t1' },
      ],
    },
  };
  // The node's Planner takes the message, dispatches, the session reports.
  r.onSend = (body) => {
    const t = Date.now();
    const ev = r.events[PLANNER];
    ev.push({ type: 'user', detail: body.text, time: t, uuid: 'r-u1' });
    ev.push({ type: 'text', detail: '好，派一个会话去跑。\n\n```naozhi-action\n' + JSON.stringify({ type: 'dispatch', workspace: 'ws_naozhi01', title: 'iso 跑 go vet', message: '跑一遍 go vet' }) + '\n```', time: t + 1, uuid: 'r-t1' });
    ev.push({ type: 'peer_message', detail: 'iso go vet 没有报错，什么都没改。', summary: 'iso go vet 没有报错，什么都没改。', from: NEW_TASK, time: t + 2, uuid: 'r-p1' });
    ev.push({ type: 'text', detail: '「iso 跑 go vet」（`' + NEW_TASK + '`）做完了：go vet 没有报错。', time: t + 3, uuid: 'r-t2' });
    ev.push({ type: 'result', time: t + 4, uuid: 'r-r1' });
    r.tasks.tasks.push({ id: NEW_TASK, kind: 'session', title: 'iso 跑 go vet', state: 'done', by: 'planner', why: '会话汇报 go vet 通过', at: iso(t + 3), created: iso(t + 1) });
  };
  return r;
}

const VIEWPORTS = [
  { name: 'desktop', width: 1280, height: 800 },
  { name: 'phone', width: 390, height: 844, isMobile: true, hasTouch: true },
];

for (const vp of VIEWPORTS) {
  test(vp.name + ': the list groups by node, the Planner node first', async ({ browser }) => {
    const ctx = await browser.newContext({ viewport: { width: vp.width, height: vp.height }, isMobile: !!vp.isMobile, hasTouch: !!vp.hasTouch });
    const page = await ctx.newPage();
    const mock = await startMockServer({ ws: true, sessions: sessions(), remote: { [NODE]: remote() }, planner: { enabled: false } });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');
      const list = page.locator('#session-list');
      const nodes = list.locator('.node-header');
      await expect(nodes).toHaveCount(2);
      // The node the Planner runs on leads, tagged; this one follows.
      await expect(nodes.nth(0).locator('.sh-name')).toHaveText('MBP');
      await expect(nodes.nth(0).locator('.node-tag')).toHaveText('Planner');
      await expect(nodes.nth(1).locator('.sh-name')).toHaveText('本机 · EC2');
      await expect(nodes.nth(1).locator('.node-tag')).toHaveCount(0);
      // Under the node, its dispatched session sits in its workspace's
      // group (the remote naozhi project), marked 派出.
      // The 只看 Planner 派出 chip leads the list, above the nodes.
      await expect(list.locator(':scope > .sb-filter')).toHaveCount(1);
      const rows = await list.locator(':scope > :not(.sb-filter)').evaluateAll((els) => els.map((e) =>
        e.classList.contains('node-header') ? 'N:' + e.dataset.node
          : e.classList.contains('session-card') ? 'C:' + e.dataset.node + ':' + e.dataset.key
            : 'H:' + (e.querySelector('.sh-name')?.textContent || '')));
      const firstLocal = rows.indexOf('N:local');
      expect(rows[0]).toBe('N:' + NODE);
      expect(rows[1]).toBe('H:naozhi');
      expect(rows[2]).toBe('C:' + NODE + ':' + OLD_TASK);
      await expect(list.locator('.session-card[data-key="' + OLD_TASK + '"] .sc-origin.kind-planner')).toHaveText('派出');
      expect(rows.slice(1, firstLocal).every((r) => !r.startsWith('C:') || r.startsWith('C:' + NODE))).toBe(true);
      expect(rows.slice(firstLocal).every((r) => !r.startsWith('C:') || r.startsWith('C:local'))).toBe(true);
      await page.screenshot({ path: test.info().outputPath('remote-list-' + vp.name + '.png') });

      // Folding a node leaves its header and its count.
      await nodes.nth(0).locator('.sh-collapse').click();
      await expect(list.locator('.session-card[data-node="' + NODE + '"]')).toHaveCount(0);
      await expect(list.locator('.node-header[data-node="' + NODE + '"] .sh-count')).toHaveText('2');
      await expect(list.locator('.session-card[data-node="local"]').first()).toBeVisible();
      await page.reload();
      await waitForWs(page, 'CONNECTED');
      await expect(list.locator('.node-header')).toHaveCount(2);
      await expect(list.locator('.session-card[data-node="' + NODE + '"]')).toHaveCount(0);
      await list.locator('.node-header[data-node="' + NODE + '"] .sh-collapse').click();
      await expect(list.locator('.session-card[data-node="' + NODE + '"]')).toHaveCount(2);
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });

  test(vp.name + ': the node\'s Planner takes a message, dispatches there and reports here', async ({ browser }) => {
    test.setTimeout(60000);
    const ctx = await browser.newContext({ viewport: { width: vp.width, height: vp.height }, isMobile: !!vp.isMobile, hasTouch: !!vp.hasTouch });
    const page = await ctx.newPage();
    const node = remote();
    const mock = await startMockServer({ ws: true, sessions: sessions(), remote: { [NODE]: node }, planner: { enabled: false } });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');
      await page.click('#abnav-planner');
      await expect(page.locator('body')).toHaveClass(/nz-view-planner/);
      // The header names the node; the stream is the node's planner:main.
      await expect(page.locator('#pl-host')).toHaveText('MBP');
      const stream = page.locator('#pl-stream');
      await expect(stream).toContainText('iso 早上好');
      await expect.poll(() => mock.wsConnections.flatMap((c) => c.messages)
        .some((m) => m.type === 'subscribe' && m.key === PLANNER && m.node === NODE)).toBe(true);
      // Uploads stay here: no attachment to another node's Planner.
      await expect(page.locator('#pl-attach')).toBeDisabled();
      // The badge counts the node's question; 等你 lists it.
      await expect(page.locator('#abnav-planner-count')).toHaveText('1');
      if (vp.isMobile) await page.click('#pl-left-toggle');
      const card = page.locator('#pl-wait .pl-card[data-id="' + NODE + '|in_q1"]');
      await expect(card).toContainText('iso 要不要顺手改 CHANGELOG？');
      await expect(page.locator('#pl-task .tk-card[data-id="' + OLD_TASK + '"]')).toContainText('iso 整理 README');
      await page.screenshot({ path: test.info().outputPath('remote-planner-' + vp.name + '.png') });
      // Closing it closes the node's item.
      await card.locator('.pl-close').click();
      await expect.poll(() => mock.remoteCalls.filter((c) => c.method === 'POST').map((c) => [c.path, c.body]))
        .toEqual([['/api/inbox/close', { id: 'in_q1' }]]);
      if (vp.isMobile) await page.click('#pl-left-toggle');

      // A message goes to the node's Planner; its dispatch and the report come back.
      await page.fill('#pl-input', 'iso 在 naozhi 里跑一遍 go vet');
      await page.click('#pl-send');
      await expect.poll(() => mock.remoteCalls.filter((c) => c.path === '/api/planner/send').map((c) => c.body))
        .toEqual([{ text: 'iso 在 naozhi 里跑一遍 go vet' }]);
      await expect(stream.locator('.pl-act-dispatch .pl-act-h')).toHaveText('派出 · iso 跑 go vet · naozhi · 跑一遍 go vet', { timeout: 15000 });
      await expect(stream.locator('.pl-act-dispatch dd').first()).toHaveText('naozhi');
      await expect(stream).toContainText('go vet 没有报错', { timeout: 15000 });
      // The new task is the node's; opening it reads its events on the node.
      if (vp.isMobile) await page.click('#pl-left-toggle');
      const done = page.locator('#pl-task .tk-card[data-id="' + NEW_TASK + '"]');
      await expect(done).toContainText('iso 跑 go vet', { timeout: 15000 });
      await page.locator('#pl-task .tk-sec-done summary').click();
      await done.locator('.tk-open').click();
      await expect(page.locator('#pl-sess')).toHaveAttribute('data-node', NODE);
      await expect(page.locator('#pl-sess-stream')).toContainText('iso go vet 没有报错。');
      await page.screenshot({ path: test.info().outputPath('remote-task-' + vp.name + '.png') });
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
}

test('a node gone offline says so and takes no message', async ({ browser }) => {
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  const page = await ctx.newPage();
  const node = Object.assign(remote(), { offline: true });
  const mock = await startMockServer({ ws: true, sessions: sessions('offline'), remote: { [NODE]: node }, planner: { enabled: false } });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await expect(page.locator('.node-header[data-node="' + NODE + '"] .node-state')).toHaveText('离线');
    await page.click('#abnav-planner');
    await expect(page.locator('#pl-status')).toHaveText('离线');
    await expect(page.locator('#pl-note')).toContainText('MBP 离线');
    await expect(page.locator('#pl-input')).toBeDisabled();
    await expect(page.locator('#pl-send')).toBeDisabled();
    await page.screenshot({ path: test.info().outputPath('remote-offline.png') });
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

// Both nodes run a Planner (remote-planner.md §2: each manages its own
// node). The page opens on this node's; the header switches to the other,
// and the conversation, the message sent, 任务, 等你 and its count follow
// the Planner shown, never mixing the two; switching back returns them.
for (const vp of VIEWPORTS) {
  test(vp.name + ': with a Planner on each node the header switches between them, nothing crossing', async ({ browser }) => {
    test.setTimeout(60000);
    const ctx = await browser.newContext({ viewport: { width: vp.width, height: vp.height }, isMobile: !!vp.isMobile, hasTouch: !!vp.hasTouch });
    const page = await ctx.newPage();
    const node = remote();
    const LOCAL_TASK = 'task:cccccccccccc';
    const s = sessions();
    s.sessions.push({ key: LOCAL_TASK, node: 'local', state: 'ready', platform: 'task', agent: 'general', cli_name: 'claude', purpose: 'task', purpose_title: 'iso EC2 清日志', user_label: 'iso EC2 清日志', workspace: '/home/ec2-user/workspace/naozhi', last_active: now - 30000 });
    const mock = await startMockServer({
      ws: true, sessions: s, remote: { [NODE]: node },
      planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
      plannerEvents: [{ type: 'text', detail: 'iso 这里是 EC2 的 Planner。', time: now - 40000, uuid: 'l-t0' }],
      tasks: { tasks: [{ id: LOCAL_TASK, kind: 'session', title: 'iso EC2 清日志', state: 'doing', at: iso(now - 30000), created: iso(now - 30000) }] },
      inbox: {
        open: [
          { id: 'in_l1', kind: 'error', level: 'timely', unit: LOCAL_TASK, title: 'iso EC2 清日志 失败', why: '', created: iso(now - 60000), state: 'open', source: 'health:failed:' + LOCAL_TASK },
          { id: 'in_l2', kind: 'question', level: 'timely', unit: PLANNER, title: 'iso EC2 要重启吗？', why: 'Planner 问你', created: iso(now - 50000), state: 'open', source: 'ask:1', decision: { handler: 'ask', actions: ['重启', '不重启'] } },
        ],
        closed: [], counts: { open: 2, badge: 2 },
      },
    });
    const wsSubs = () => mock.wsConnections.flatMap((c) => c.messages).filter((m) => m.type === 'subscribe' && m.key === PLANNER);
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');
      // Both nodes are tagged in the list; this one (the Planner shown) leads.
      await expect(page.locator('#session-list .node-header .node-tag')).toHaveCount(2);
      await expect(page.locator('#session-list .node-header').first()).toHaveAttribute('data-node', 'local');

      await page.click('#abnav-planner');
      const pick = page.locator('#pl-host select');
      await expect(pick).toHaveValue('local');
      await expect(pick.locator('option')).toHaveText(['本机 · EC2', 'MBP']);
      const stream = page.locator('#pl-stream');
      await expect(stream).toContainText('iso 这里是 EC2 的 Planner。');
      await expect(page.locator('#abnav-planner-count')).toHaveText('2');
      if (vp.isMobile) await page.click('#pl-left-toggle');
      await expect(page.locator('#pl-wait .pl-card')).toHaveCount(2);
      await expect(page.locator('#pl-task .tk-card[data-id="' + LOCAL_TASK + '"]')).toHaveCount(1);
      if (vp.isMobile) await page.click('#pl-left-toggle');
      await page.screenshot({ path: test.info().outputPath('switch-local-' + vp.name + '.png') });

      // To MBP's: its conversation, its subscription, its 任务 and 等你 alone.
      await pick.selectOption(NODE);
      await expect(stream).toContainText('iso 早上好');
      await expect(stream).not.toContainText('iso 这里是 EC2 的 Planner。');
      await expect.poll(() => wsSubs().map((m) => m.node || 'local')).toEqual(['local', NODE]);
      await expect.poll(() => mock.wsConnections.flatMap((c) => c.messages).some((m) => m.type === 'unsubscribe' && m.key === PLANNER && !m.node)).toBe(true);
      await expect(page.locator('#abnav-planner-count')).toHaveText('1');
      if (vp.isMobile) await page.click('#pl-left-toggle');
      await expect(page.locator('#pl-wait .pl-card')).toHaveCount(1);
      await expect(page.locator('#pl-wait .pl-card[data-id="' + NODE + '|in_q1"]')).toHaveCount(1);
      await expect(page.locator('#pl-task .tk-card[data-id="' + OLD_TASK + '"]')).toHaveCount(1);
      await expect(page.locator('#pl-task .tk-card[data-id="' + LOCAL_TASK + '"]')).toHaveCount(0);
      if (vp.isMobile) await page.click('#pl-left-toggle');
      await page.screenshot({ path: test.info().outputPath('switch-remote-' + vp.name + '.png') });
      await page.fill('#pl-input', 'iso 给 MBP 的话');
      await page.click('#pl-send');
      await expect.poll(() => mock.remoteCalls.filter((c) => c.path === '/api/planner/send').map((c) => c.body.text)).toEqual(['iso 给 MBP 的话']);
      expect(mock.plannerSendCalls).toEqual([]);
      await expect(stream).toContainText('go vet 没有报错', { timeout: 15000 });

      // The pick holds across a reload; switching back returns this node's.
      await page.reload();
      await waitForWs(page, 'CONNECTED');
      await page.click('#abnav-planner');
      await expect(page.locator('#pl-host select')).toHaveValue(NODE);
      await page.locator('#pl-host select').selectOption('local');
      await expect(stream).toContainText('iso 这里是 EC2 的 Planner。');
      await expect(stream).not.toContainText('iso 早上好');
      await expect(page.locator('#abnav-planner-count')).toHaveText('2');
      await page.fill('#pl-input', 'iso 给 EC2 的话');
      await page.click('#pl-send');
      await expect.poll(() => mock.plannerSendCalls.map((b) => b.text)).toEqual(['iso 给 EC2 的话']);
      expect(mock.remoteCalls.filter((c) => c.path === '/api/planner/send')).toHaveLength(1);
      // Each node's inbox was written only through its own route.
      expect(mock.remoteCalls.filter((c) => c.method !== 'GET' && c.path !== '/api/planner/send')).toEqual([]);
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
}
