// E2E: the Planner drawer (planner_drawer.js, planner RFC §15.3 ②): Alt+P
// from any view, an input included, pulls the Planner conversation over the
// right side and Alt+P or Esc puts it away; it and the workbench never show
// together; a send in it goes through /api/planner/send and its receipt
// renders there; a session card leaves it for that session; the rail's
// Planner icon takes the conversation back to the page; openPlannerDrawer
// marks a 等你 item; planner.enabled off shows the explanation; on phone the
// session header's Planner button opens it full screen.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const SESSION = 'dashboard:direct:2026-01-01-120000-1:myproject';
const PLANNER = 'planner:main';
const now = Date.now();
const iso = (ms) => new Date(ms).toISOString();

const plannerEvents = () => [
  { type: 'user', time: now - 120000, uuid: 'pd-u1', detail: '<naozhi-snapshot at="x">\n</naozhi-snapshot>\n\niso 早上好' },
  { type: 'text', time: now - 110000, uuid: 'pd-t1', detail: '`' + SESSION + '` 在等你回复。' },
];

const inbox = () => ({
  open: [{ id: 'in_n', kind: 'notice', level: 'timely', unit: '', title: 'iso 磁盘快满了', why: '剩余不到 5%', created: iso(now - 300000), state: 'open', source: 'disk' }],
  closed: [],
  counts: { open: 1, badge: 0 },
});

const planner = (over) => Object.assign({ enabled: true, stopped: false, key: PLANNER, alive: true }, over);

async function openChat(page, mock) {
  await page.goto(mock.url + '/dashboard');
  await waitForWs(page, 'CONNECTED');
  await page.click('.session-card[data-key="' + SESSION + '"]');
  await expect(page.locator('#msg-input')).toBeVisible();
}

test('desktop: Alt+P and Esc, the workbench, a send with its receipt, a card, the full page', async ({ browser }) => {
  test.setTimeout(60000);
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  const page = await ctx.newPage();
  const mock = await startMockServer({ ws: true, plannerEvents: plannerEvents(), inbox: inbox(), planner: planner() });
  try {
    await openChat(page, mock);
    const drawer = page.locator('#planner-drawer');
    await expect(drawer).toBeHidden();

    // From inside the chat input: nothing typed, the drawer opens on the
    // conversation with the focus in its input box.
    await page.click('#msg-input');
    await page.keyboard.press('Alt+KeyP');
    await expect(drawer).toBeVisible();
    await expect(drawer.locator('#pl-center')).toHaveCount(1);
    await expect(drawer.locator('#pl-stream .event')).toHaveCount(2);
    await expect(drawer.locator('#pl-status')).toHaveText('');
    await expect(page.locator('#pl-input')).toBeFocused();
    await expect(page.locator('#msg-input')).toHaveText('');
    await expect(drawer.locator('.pl-page-only:visible')).toHaveCount(0);
    await expect(drawer.locator('[data-action="planner-drawer-close"]')).toBeVisible();

    // Esc puts it away and gives the focus back; Alt+P toggles.
    await page.keyboard.press('Escape');
    await expect(drawer).toBeHidden();
    await expect(page.locator('#planner-main #pl-center')).toHaveCount(1);
    await expect(page.locator('#msg-input')).toBeFocused();
    await page.keyboard.press('Alt+KeyP');
    await expect(drawer).toBeVisible();
    await page.keyboard.press('Alt+KeyP');
    await expect(drawer).toBeHidden();

    // The workbench and the drawer put each other away.
    await page.click('.main-header .btn-workbench');
    await expect(page.locator('#fv-drawer')).toHaveClass(/fv-open/);
    await page.keyboard.press('Alt+KeyP');
    await expect(drawer).toBeVisible();
    await expect(page.locator('#fv-drawer')).not.toHaveClass(/fv-open/);
    await page.locator('.main-header .btn-workbench').dispatchEvent('click');
    await expect(page.locator('#fv-drawer')).toHaveClass(/fv-open/);
    await expect(drawer).toBeHidden();
    await page.keyboard.press('Escape');
    await expect(page.locator('#fv-drawer')).not.toHaveClass(/fv-open/);

    // A send from the drawer: the pending bubble, the pushed receipt.
    await page.keyboard.press('Alt+KeyP');
    const conn = mock.wsConnections[mock.wsConnections.length - 1];
    await expect.poll(() => conn.messages.filter((m) => m.type === 'subscribe' && m.key === PLANNER).length).toBeGreaterThan(0);
    conn.send({ type: 'subscribed', key: PLANNER, state: 'ready' });
    await expect(page.locator('#pl-target-to')).toHaveText('');
    await page.fill('#pl-input', 'iso 现在都在干什么？');
    await page.press('#pl-input', 'Enter');
    await expect.poll(() => mock.plannerSendCalls).toEqual([{ text: 'iso 现在都在干什么？' }]);
    await expect(drawer.locator('#pl-stream .pl-pending')).toHaveText(/iso 现在都在干什么？/);
    conn.send({ type: 'event', key: PLANNER, event: { type: 'system', summary: '收到，正在看', time: Date.now() } });
    await expect(drawer.locator('#pl-stream .event.system')).toHaveText(/收到，正在看/);
    expect(mock.sendCalls).toHaveLength(0);
    await page.screenshot({ path: test.info().outputPath('planner-drawer-desktop.png'), animations: 'disabled' });

    // A session card leaves the drawer for the session.
    await drawer.locator('#pl-stream .pl-scard[data-key="' + SESSION + '"]').click();
    await expect(drawer).toBeHidden();
    await expect(page.locator('body')).not.toHaveClass(/nz-view-planner/);
    await expect(page.locator('.main-header h2')).toContainText('hello world');

    // The rail icon still opens the full page, taking the conversation back.
    await page.keyboard.press('Alt+KeyP');
    await expect(drawer).toBeVisible();
    await page.click('#abnav-planner');
    await expect(page.locator('body')).toHaveClass(/nz-view-planner/);
    await expect(drawer).toBeHidden();
    await expect(page.locator('#planner-main #pl-center')).toBeVisible();
    await expect(page.locator('#pl-stream .event.user')).toHaveCount(1);
    // On the page Alt+P is the conversation already in view.
    await page.keyboard.press('Alt+KeyP');
    await expect(drawer).toBeHidden();
    await expect(page.locator('#pl-input')).toBeFocused();
    // With a session open in the middle column, openPlannerDrawer({ item })
    // goes back to the Planner conversation to mark it.
    await page.locator('#pl-stream .pl-scard[data-key="' + SESSION + '"]').click();
    await expect(page.locator('#planner-main')).toHaveClass(/pl-sess-open/);
    await page.evaluate(() => import('/static/planner_drawer.js').then((m) => m.openPlannerDrawer({ item: 'in_n' })));
    await expect(page.locator('#planner-main')).not.toHaveClass(/pl-sess-open/);
    await expect(page.locator('#planner-main #pl-focus .pl-card[data-id="in_n"]')).toBeVisible();
    await expect(page.locator('#pl-target-to')).toHaveText('');
    await expect(page.locator('#pl-input')).toBeFocused();
    await expect(drawer).toBeHidden();

    // openPlannerDrawer marks a 通知 item above the conversation.
    await page.click('#abnav-chat');
    await page.evaluate(() => import('/static/planner_drawer.js').then((m) => m.openPlannerDrawer({ item: 'in_n' })));
    await expect(drawer).toBeVisible();
    const marked = drawer.locator('#pl-focus .pl-card[data-id="in_n"]');
    await expect(marked).toContainText('iso 磁盘快满了');
    await marked.locator('[data-action="planner-inbox-close"]').click();
    await expect.poll(() => mock.inboxCalls.map((c) => c.path)).toContain('/api/inbox/close');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

// The drawer and the workbench are not modal: Esc in the chat input stays
// the input's (it arms an interrupt of the running turn), while Esc from
// elsewhere still closes the panel.
test('Esc in the chat input is the input\'s while the drawer or the workbench is open', async ({ page }) => {
  const RUNNING = 'dashboard:direct:2026-01-01-120001-2:otherproject';
  const mock = await startMockServer({ ws: true, plannerEvents: plannerEvents(), inbox: inbox(), planner: planner() });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click('.session-card[data-key="' + RUNNING + '"]');
    const drawer = page.locator('#planner-drawer');
    const wb = page.locator('#fv-drawer');
    for (const panel of ['drawer', 'workbench']) {
      // Past the 600ms an armed Esc waits for its second press.
      await page.waitForTimeout(700);
      if (panel === 'drawer') {
        await page.keyboard.press('Alt+KeyP');
        await expect(drawer).toBeVisible();
      } else {
        await page.click('.main-header .btn-workbench');
        await expect(wb).toHaveClass(/fv-open/);
      }
      // The drawer may cover the input's middle: focus it as a click on its
      // visible part would.
      await page.focus('#msg-input');
      await page.evaluate(() => { document.getElementById('toast').textContent = ''; });
      await page.keyboard.press('Escape');
      await expect(page.locator('#toast')).toHaveText('再按一次 Esc 发送中断');
      if (panel === 'drawer') await expect(drawer).toBeVisible();
      else await expect(wb).toHaveClass(/fv-open/);
      // Esc from outside an input closes it.
      await page.locator('#msg-input').blur();
      await page.keyboard.press('Escape');
      if (panel === 'drawer') await expect(drawer).toBeHidden();
      else await expect(wb).not.toHaveClass(/fv-open/);
    }
  } finally {
    mock.server.close();
  }
});

test('planner.enabled off: the drawer explains it', async ({ page }) => {
  const mock = await startMockServer({ ws: true, plannerEvents: [], inbox: { open: [], closed: [], counts: { open: 0, badge: 0 } }, planner: planner({ enabled: false, alive: false }) });
  try {
    await openChat(page, mock);
    await expect(page.locator('#btn-pld')).toBeHidden();
    await page.keyboard.press('Alt+KeyP');
    await expect(page.locator('#planner-drawer #pl-note')).toContainText('Planner 没有开启');
    await expect(page.locator('#planner-drawer #pl-note')).toContainText('Planner 页面左栏的"通知"');
    await expect(page.locator('#pl-input')).toBeDisabled();
  } finally {
    mock.server.close();
  }
});

test('phone: the session header\'s Planner button opens it full screen', async ({ browser }) => {
  const ctx = await browser.newContext({ viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });
  const page = await ctx.newPage();
  // A gateway line longer than the phone is wide wraps inside it.
  const events = plannerEvents().concat({ type: 'system', time: now - 100000, summary: '派出「iso 列出 work 目录文件」失败：同时运行的会话已满（max_procs 3），等有会话结束再派' });
  const mock = await startMockServer({ ws: true, plannerEvents: events, inbox: inbox(), planner: planner() });
  try {
    await openChat(page, mock);
    const btn = page.locator('#btn-pld');
    await expect(btn).toBeInViewport();
    await page.screenshot({ path: test.info().outputPath('planner-drawer-entry-mobile.png'), animations: 'disabled' });
    await btn.tap();
    const drawer = page.locator('#planner-drawer');
    await expect(drawer).toBeVisible();
    const box = await drawer.boundingBox();
    expect(Math.round(box.width)).toBe(390);
    await expect(page.locator('#pl-input')).toBeInViewport();
    await expect(drawer.locator('#pl-stream .event')).toHaveCount(3);
    const line = drawer.locator('#pl-stream .event.system .event-content');
    await expect.poll(async () => { const b = await line.boundingBox(); return b.x >= 0 && b.x + b.width <= 390; }).toBe(true);
    await page.screenshot({ path: test.info().outputPath('planner-drawer-mobile.png'), animations: 'disabled' });
    await drawer.locator('[data-action="planner-drawer-close"]').tap();
    await expect(drawer).toBeHidden();
    await expect(page.locator('#msg-input')).toBeVisible();
  } finally {
    await ctx.close();
    mock.server.close();
  }
});
