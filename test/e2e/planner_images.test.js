// E2E: images to the Planner (planner_chat.js over composer_files.js's upload
// tray; planner RFC §10.1 输入): an image pasted, dropped or picked into the
// Planner page's input box uploads like the chat view's and goes out as
// file_ids with POST /api/planner/send, also with no text; your bubble shows
// its thumbnail, from the stream and from the history a reload reads; the
// drawer's input box is the same box; 交给 Planner carries the chat view's
// input's attachments with the session.
//
// 跑法：cd test/e2e && npx playwright test planner_images.test.js --project=desktop-chrome
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const SESSION = 'dashboard:direct:2026-01-01-120000-1:myproject';
const PLANNER = 'planner:main';
// A 1x1 PNG, so the image path's decode and re-encode both succeed.
const PNG_1X1 = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=';
const THUMB = 'data:image/png;base64,' + PNG_1X1;
const SNAP = '<naozhi-snapshot at="x">\n</naozhi-snapshot>';
const now = Date.now();

// The history holds one message of yours with a picture: a reload shows it.
const plannerEvents = () => [
  { type: 'user', time: now - 120000, uuid: 'pi-u1', detail: SNAP + '\n\niso 昨天那张图', summary: 'iso 昨天那张图 [+1 image(s)]', images: [THUMB] },
  { type: 'text', time: now - 110000, uuid: 'pi-t1', detail: 'iso 看到了，是一张登录页截图。' },
];

const planner = () => ({ enabled: true, stopped: false, key: PLANNER, alive: true });

// paste puts a PNG on the clipboard of the element sel names.
async function paste(page, sel) {
  return page.evaluate(([b64, s]) => {
    const bytes = Uint8Array.from(atob(b64), (c) => c.charCodeAt(0));
    const dt = new DataTransfer();
    dt.items.add(new File([bytes], 'shot.png', { type: 'image/png' }));
    const el = document.querySelector(s);
    el.focus();
    return el.dispatchEvent(new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true }));
  }, [PNG_1X1, sel]);
}

async function openPlanner(page, mock) {
  await page.goto(mock.url + '/dashboard');
  await waitForWs(page, 'CONNECTED');
  await page.locator('#abnav-planner').click();
  await expect(page.locator('#pl-stream .event.user')).toHaveCount(1);
  const conn = mock.wsConnections[mock.wsConnections.length - 1];
  await expect.poll(() => conn.messages.some((m) => m.type === 'subscribe' && m.key === PLANNER)).toBe(true);
  conn.send({ type: 'subscribed', key: PLANNER, state: 'ready' });
  return conn;
}

for (const [name, viewport] of [['desktop', { width: 1440, height: 900 }], ['phone', { width: 390, height: 844 }]]) {
  test(name + ': paste an image on the Planner page, send it with and without text, its bubble shows it', async ({ browser }) => {
    test.setTimeout(60000);
    const phone = name === 'phone';
    const ctx = await browser.newContext({ viewport, isMobile: phone, hasTouch: phone });
    const page = await ctx.newPage();
    const pageErrors = [];
    page.on('pageerror', (e) => pageErrors.push(String(e)));
    const mock = await startMockServer({ ws: true, uploads: true, plannerEvents: plannerEvents(), planner: planner() });
    try {
      const conn = await openPlanner(page, mock);
      // The history's picture shows on your bubble, the snapshot does not.
      const first = page.locator('#pl-stream .event.user').first();
      await expect(first.locator('.event-images img')).toHaveCount(1);
      await expect(first).toContainText('iso 昨天那张图');
      await expect(first).not.toContainText('naozhi-snapshot');
      await expect(first).not.toContainText('image(s)');

      // A pasted image: the input box's own tray, not the chat view's.
      await expect(page.locator('#pl-attach')).toBeVisible();
      expect(await paste(page, '#pl-input'), 'the paste must not also land in the textarea').toBe(false);
      const thumbs = page.locator('#pl-file-preview .file-thumb');
      await expect(thumbs).toHaveCount(1);
      await expect(page.locator('#pl-file-preview .file-thumb.ready')).toHaveCount(1);
      await expect(page.locator('#file-preview .file-thumb')).toHaveCount(0);
      await page.fill('#pl-input', 'iso 看这张图');
      await expect(page.locator('#pl-input')).toBeInViewport();
      await page.screenshot({ path: test.info().outputPath('planner-images-composer-' + name + '.png') });
      await page.press('#pl-input', 'Enter');
      await expect.poll(() => mock.plannerSendCalls).toEqual([{ text: 'iso 看这张图', file_ids: ['up-1'] }]);
      await expect(thumbs).toHaveCount(0);

      // The logged bubble carries the thumbnail.
      const t = Date.now();
      conn.send({ type: 'event', key: PLANNER, event: { type: 'user', detail: SNAP + '\n\niso 看这张图', summary: 'x [+1 image(s)]', images: [THUMB], time: t, uuid: 'pi-u2' } });
      const mine = page.locator('#pl-stream .event.user').last();
      await expect(mine).toContainText('iso 看这张图');
      await expect(mine.locator('.event-images img')).toHaveAttribute('src', THUMB);

      // Only an image, no text: it still goes out.
      await paste(page, '#pl-input');
      await expect(page.locator('#pl-file-preview .file-thumb.ready')).toHaveCount(1);
      await page.click('#pl-send');
      await expect.poll(() => mock.plannerSendCalls.length).toBe(2);
      expect(mock.plannerSendCalls[1]).toEqual({ text: '', file_ids: ['up-2'] });
      await expect(thumbs).toHaveCount(0);
      // The logged summary is the whole (short) snapshot and the image count.
      conn.send({ type: 'event', key: PLANNER, event: { type: 'user', detail: SNAP, summary: SNAP + ' [+1 image(s)]', images: [THUMB], time: t + 5, uuid: 'pi-u3' } });
      const only = page.locator('#pl-stream .event.user').last();
      await expect(only.locator('.event-images img')).toHaveCount(1);
      await expect(only).not.toContainText('naozhi-snapshot');
      await expect(only.locator('.event-content')).toHaveText('');
      await expect(page.locator('#pl-stream .event.user')).toHaveCount(3);
      await page.screenshot({ path: test.info().outputPath('planner-images-sent-' + name + '.png') });
      expect(pageErrors).toEqual([]);
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
}

test('desktop: the drawer input box sends an image too', async ({ browser }) => {
  test.setTimeout(60000);
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const page = await ctx.newPage();
  const mock = await startMockServer({ ws: true, uploads: true, plannerEvents: plannerEvents(), planner: planner() });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click('.session-card[data-key="' + SESSION + '"]');
    await expect(page.locator('#msg-input')).toBeVisible();
    await page.keyboard.press('Alt+KeyP');
    const drawer = page.locator('#planner-drawer');
    await expect(drawer).toBeVisible();
    await expect(drawer.locator('#pl-stream .event.user .event-images img')).toHaveCount(1);
    // A file the picker hands in.
    await drawer.locator('#pl-file-input').setInputFiles({ name: 'shot.png', mimeType: 'image/png', buffer: Buffer.from(PNG_1X1, 'base64') });
    await expect(drawer.locator('#pl-file-preview .file-thumb.ready')).toHaveCount(1);
    await page.fill('#pl-input', 'iso 抽屉里的图');
    await page.screenshot({ path: test.info().outputPath('planner-images-drawer-desktop.png'), animations: 'disabled' });
    await page.press('#pl-input', 'Enter');
    await expect.poll(() => mock.plannerSendCalls).toEqual([{ text: 'iso 抽屉里的图', file_ids: ['up-1'] }]);
    await expect(drawer.locator('#pl-file-preview .file-thumb')).toHaveCount(0);
    // The chat view's input kept nothing of it.
    await expect(page.locator('#file-preview .file-thumb')).toHaveCount(0);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('desktop: 交给 Planner carries the chat input\'s image with the session', async ({ browser }) => {
  test.setTimeout(60000);
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  const page = await ctx.newPage();
  const mock = await startMockServer({ ws: true, uploads: true, plannerEvents: plannerEvents(), planner: planner() });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click('.session-card[data-key="' + SESSION + '"]');
    await expect(page.locator('#msg-input')).toBeVisible();
    await paste(page, '#msg-input');
    await expect(page.locator('#file-preview .file-thumb.ready')).toHaveCount(1);
    await page.click('#msg-input');
    await page.keyboard.type('iso 这个页面不对');
    await expect(page.locator('#btn-handoff')).toBeEnabled();
    await page.click('#btn-handoff');
    await expect.poll(() => mock.plannerSendCalls).toEqual([{ text: 'iso 这个页面不对', ref: SESSION, file_ids: ['up-1'] }]);
    await expect(page.locator('#file-preview .file-thumb')).toHaveCount(0);
    await expect(page.locator('#msg-input')).toHaveText('');
    await expect(page.locator('#handoff-note')).toContainText('已交给 Planner');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});
