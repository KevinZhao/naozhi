// E2E: the Planner passing the user's decision on (planner RFC D98). In its
// conversation a relay block is a "转达你的决定" card naming the session,
// an ask naming a task says where the answer goes, a dispatch says it
// carries the user's words and the MCP it opens; in the session the relayed
// message shows the user's words as a quote, then the Planner's text, with
// none of the envelope's tags.
const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const PLANNER = 'planner:main';
const TASK = 'task:0a1b2c3d4e5f';
const now = Date.now();
const block = (a) => '```naozhi-action\n' + JSON.stringify(a) + '\n```';
const lastConn = (mock) => mock.wsConnections[mock.wsConnections.length - 1];

function sessions() {
  const s = defaultSessions();
  s.sessions.push({ key: TASK, state: 'ready', platform: 'task', agent: 'general', cli_name: 'claude', purpose: 'task', user_label: 'iso 合并发版',
    purpose_title: 'iso 合并发版', workspace: '/home/user/workspace/myproject', project: 'myproject', node: 'local', last_active: now - 10000 });
  return s;
}

const RELAYED = '<naozhi-relay at="11/7 21:28">\n用户 11/7 21:28 在 Planner 对话里说：「iso 都合并，发版」\n</naozhi-relay>\n' +
  'Planner 的说明：\n按 #1 → #2 的顺序合并，每合一个等 CI 绿。';

for (const [name, vp] of [['desktop', { viewport: { width: 1440, height: 900 } }], ['phone', { viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true }]]) {
  test(`relay cards and the relayed message (${name})`, async ({ browser }) => {
    test.setTimeout(60000);
    const ctx = await browser.newContext(vp);
    const page = await ctx.newPage();
    const mock = await startMockServer({
      ws: true, sessions: sessions(),
      planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
      plannerEvents: [
        { type: 'text', time: now - 5000, uuid: 'pr-t1', detail: '转达给它。\n\n' +
          block({ type: 'relay', key: TASK, text: 'iso 按顺序合并，每合一个等 CI 绿' }) + '\n\n' +
          block({ type: 'ask', question: 'iso 要发版吗？', options: ['发', '先不发'], task: TASK }) + '\n\n' +
          block({ type: 'dispatch', workspace: 'myproject', title: 'iso 建日程', model: '', message: 'iso 建两个日程', with_user_words: true,
            mcp_tools: { 'aws-outlook-mcp': ['calendar_meeting'] } }) },
      ],
    });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');
      await page.click('#abnav-planner');
      const reply = page.locator('#pl-stream .event.text').first();
      await expect(reply.locator('.pl-act-bad')).toHaveCount(0);
      const relay = reply.locator('.pl-act-relay');
      await expect(relay.locator('.pl-act-h')).toHaveText('转达你的决定');
      await expect(relay.locator('.pl-act-q')).toHaveText('iso 按顺序合并，每合一个等 CI 绿');
      await expect(relay.locator('.pl-scard[data-key="' + TASK + '"] .pl-scard-name')).toHaveText('iso 合并发版');
      await expect(reply.locator('.pl-act-ask')).toContainText('你的回答会直接送到');
      await expect(reply.locator('.pl-act-ask .pl-scard[data-key="' + TASK + '"]')).toHaveCount(1);
      const dispatch = reply.locator('.pl-act-dispatch');
      await expect(dispatch).toContainText('你这一轮的原话');
      await expect(dispatch).toContainText('放行写工具');
      await expect(dispatch).toContainText('aws-outlook-mcp：calendar_meeting');
      await expect(reply).not.toContainText(TASK);

      // The session: the relayed message reads whose words are whose.
      await page.evaluate((k) => window.nz.test.selectSession(k, 'local'), TASK);
      const conn = lastConn(mock);
      await expect.poll(() => conn.messages.some((m) => m.type === 'subscribe' && m.key === TASK)).toBe(true);
      conn.send({ type: 'subscribed', key: TASK, state: 'ready' });
      conn.send({ type: 'history', key: TASK, initial: true, events: [
        { type: 'user', source: 'planner', detail: 'iso 看一下 PR\n\n（网关附注：做完、卡住或判断做不了时，用 SendMessage 发给 planner-abc 汇报。）', time: now - 9000, uuid: 'pr-u1' },
        { type: 'user', source: 'relay', detail: RELAYED, time: now - 4000, uuid: 'pr-u2' },
      ] });
      const msg = page.locator('#events-scroll .event.user.relay-msg');
      await expect(msg.locator('.custody-source')).toHaveText('Planner 转达 · 附你的原话');
      await expect(msg.locator('.relay-words')).toHaveText('用户 11/7 21:28 在 Planner 对话里说：「iso 都合并，发版」');
      await expect(msg).toContainText('按 #1 → #2 的顺序合并');
      await expect(msg).not.toContainText('naozhi-relay');
      await msg.screenshot({ path: test.info().outputPath('relay-msg-' + name + '.png') });
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
}
