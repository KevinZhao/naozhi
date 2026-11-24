// E2E: the project patrol's weekly deep check (planner RFC §15.23 ②③, D142).
// A turn its findings started is not your bubble: in 对话 it folds into 动态
// as 项目巡查, in 全部 it is the line 来自项目巡查 · 工作区名, and the findings'
// JSON is never shown. The blocks that name a finding (dispatch, ask,
// notify) carry the 巡查 tag. Desktop and phone width.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const PLANNER = 'planner:main';
const WS = 'ws_ab12cd34';
const REF = WS + '/bug-reconnect-nil';
const base = (() => { const d = new Date(); d.setHours(10, 0, 0, 0); return Math.min(d.getTime(), Date.now() - 3600000); })();
const at = (min) => base + min * 60000;
const fence = (body) => '```naozhi-action\n' + JSON.stringify(body) + '\n```';
const snap = '<naozhi-snapshot at="x">\n</naozhi-snapshot>\n\n';

const PATROL_BLOCK = '<naozhi-patrol workspace="' + WS + '" name="iso 网关仓库" run="20261125T110412-9c1e" findings="2">\n' +
  '项目巡查的每周深查在这个工作区发现了下面的问题，每行一个 JSON。\n' +
  '{"ref":"' + REF + '","title":"iso 重连时会话为空会崩","kind":"bug","evidence":["internal/a.go:12"],"auto":true}\n' +
  '{"ref":"' + WS + '/debt-dup-backoff","title":"iso 退避有两份实现","kind":"debt","evidence":["internal/b.go:3"],"auto":false}\n' +
  '</naozhi-patrol>\n\n' + snap;

function plannerEvents() {
  return [
    { type: 'user', detail: snap + 'iso 今天先看文档', time: at(0), uuid: 'pa-u1' },
    { type: 'text', detail: 'iso 好。', time: at(0.2), uuid: 'pa-t1' },
    { type: 'result', time: at(0.3), uuid: 'pa-r1' },
    { type: 'user', source: 'patrol', detail: PATROL_BLOCK, summary: PATROL_BLOCK.slice(0, 120), time: at(10), uuid: 'pa-u2' },
    { type: 'text', detail: 'iso 巡查发现两处：重连的空指针小而明确，派出去修；退避的重复先问你。\n\n' +
      fence({ type: 'dispatch', workspace: WS, title: 'iso 修重连空指针', model: '', message: 'iso 修掉重连时的空指针，开 PR', finding: REF }) + '\n' +
      fence({ type: 'ask', question: 'iso 退避要不要统一成一个实现？', options: ['统一', '先不动'], finding: WS + '/debt-dup-backoff' }) + '\n' +
      fence({ type: 'notify', title: 'iso 修重连的 PR 已备好', why: 'CI 通过', level: 'timely', link: 'https://github.com/owner/repo/pull/31', id: '', finding: REF }) + '\n' +
      fence({ type: 'notify', title: 'iso 与巡查无关的提醒', why: '', level: 'brief', link: '', id: '' }),
    time: at(10.3), uuid: 'pa-t2' },
    { type: 'result', time: at(10.4), uuid: 'pa-r2' },
    { type: 'user', detail: snap + 'iso 接着呢', time: at(20), uuid: 'pa-u3' },
    { type: 'text', detail: 'iso 没有别的了。', time: at(20.2), uuid: 'pa-t3' },
    { type: 'result', time: at(20.3), uuid: 'pa-r3' },
  ];
}

const planner = { enabled: true, stopped: false, key: PLANNER, alive: true };

for (const vp of [{ name: 'desktop', width: 1280, height: 900 }, { name: 'phone', width: 390, height: 844 }]) {
  test(`${vp.name}: a patrol turn folds into 动态 as 项目巡查, its blocks tagged 巡查`, async ({ browser }) => {
    const ctx = await browser.newContext({ viewport: { width: vp.width, height: vp.height } });
    const page = await ctx.newPage();
    const errors = [];
    page.on('pageerror', (e) => errors.push(e.message));
    const mock = await startMockServer({ ws: true, plannerEvents: plannerEvents(), planner });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');
      await page.evaluate(() => document.getElementById('abnav-planner').click());
      await expect(page.locator('body')).toHaveClass(/nz-view-planner/);
      const stream = page.locator('#pl-stream');
      const phone = vp.name === 'phone';
      const sw = phone ? page.locator('#pl-tools .pl-layer') : page.locator('.pl-head > .pl-layer');

      // 对话: only what you typed is a bubble; the patrol's turn is one row.
      await expect(stream.locator('.event.user:visible')).toHaveCount(2);
      await expect(stream.getByText('iso 巡查发现两处', { exact: false }).first()).toBeHidden();
      const rows = stream.locator('.pl-dyn');
      await expect(rows).toHaveCount(1);
      const head = rows.locator('.pl-dyn-h');
      await expect(head).toContainText('1 条动态（项目巡查）');
      await head.click();
      const line = rows.locator('.pl-dyn-line');
      await expect(line).toHaveCount(1);
      await expect(line.locator('.pl-dyn-src')).toHaveText('项目巡查');
      await expect(line.locator('.pl-dyn-wait')).toHaveText('→ 等你');

      // 全部: the turn is marked by its source and workspace, never your
      // bubble, and the findings' data is not shown.
      if (phone) await page.click('#pl-more > summary');
      await sw.locator('[data-layer="all"]').click();
      await expect(stream.locator('.pl-dyn')).toHaveCount(0);
      await expect(stream.getByText('来自项目巡查 · iso 网关仓库：2 条新发现')).toBeVisible();
      await expect(stream.locator('.event.user')).toHaveCount(2);
      await expect(stream).not.toContainText('naozhi-patrol');
      await expect(stream).not.toContainText('iso 退避有两份实现');
      await expect(stream).not.toContainText('internal/a.go:12');

      // The blocks about a finding say so; one about none does not.
      const dispatch = stream.locator('.pl-act-dispatch');
      await expect(dispatch.locator('summary .pl-act-tag')).toHaveText('巡查');
      await expect(dispatch.locator('summary .pl-act-tag')).toHaveAttribute('title', '项目巡查的发现：' + REF);
      await expect(stream.locator('.pl-act-ask .pl-act-h .pl-act-tag')).toHaveText('巡查');
      const notices = stream.locator('.pl-act-notify');
      await expect(notices).toHaveCount(2);
      await expect(notices.nth(0).locator('.pl-act-tag')).toHaveText('巡查');
      await expect(notices.nth(1).locator('.pl-act-tag')).toHaveCount(0);
      await page.screenshot({ path: test.info().outputPath(`planner-patrol-${vp.name}.png`) });
      expect(errors).toEqual([]);
    } finally {
      await ctx.close();
      await new Promise((r) => mock.server.close(r));
    }
  });
}
