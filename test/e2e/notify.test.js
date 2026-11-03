// The dashboard exit of the push pipeline (planner RFC §10.1, notify.js): a
// notify frame shows a browser notification only while the page is in the
// background and notifications are allowed, and a click opens the session the
// item is about; inbox_count frames reach nzBus for the Planner page's badge.
// Notification and document.hidden are stubbed so the test drives both.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const SESSION_KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';

function stubNotification() {
  window.__notes = [];
  window.__hidden = false;
  class FakeNotification {
    constructor(title, opts) {
      this.title = title;
      this.opts = opts || {};
      window.__notes.push(this);
    }
    close() { this.closed = true; }
  }
  FakeNotification.permission = 'granted';
  FakeNotification.requestPermission = async () => FakeNotification.permission;
  window.Notification = FakeNotification;
  Object.defineProperty(document, 'hidden', { configurable: true, get: () => window.__hidden });
}

for (const [name, viewport] of [['desktop', { width: 1280, height: 800 }], ['mobile', { width: 390, height: 844 }]]) {
  test.describe(`browser notifications (${name})`, () => {
    let mock;

    test.beforeAll(async () => { mock = await startMockServer({ ws: true }); });
    test.afterAll(() => mock.server.close());

    test('notify shows only in the background and a click opens the session', async ({ browser }) => {
      const ctx = await browser.newContext({ viewport });
      const page = await ctx.newPage();
      await page.addInitScript(stubNotification);
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page);
      await page.waitForFunction(
        (key) => !!window.nz.test.sessionsData[window.nz.test.sid(key, 'local')],
        SESSION_KEY
      );
      await page.evaluate(async () => {
        const { nzBus } = await import('/static/nz_util.js');
        window.__counts = [];
        nzBus.addEventListener('inbox:count', (e) => window.__counts.push([e.detail.open, e.detail.badge]));
      });
      const conn = mock.wsConnections[mock.wsConnections.length - 1];
      const frame = { type: 'notify', id: 'in_1', kind: 'question', level: 'timely', title: 'iso 会话 在等你回复', body: '有提问等你回答超过 10 分钟', key: SESSION_KEY };

      // In the foreground the inbox shows it; no notification.
      conn.send(frame);
      conn.send({ type: 'inbox_count', open: 1, badge: 1 });
      await page.waitForFunction(() => window.__counts.length === 1);
      expect(await page.evaluate(() => window.__notes.length)).toBe(0);

      await page.evaluate(() => { window.__hidden = true; });
      conn.send(frame);
      await page.waitForFunction(() => window.__notes.length === 1);
      const note = await page.evaluate(() => ({ title: window.__notes[0].title, ...window.__notes[0].opts }));
      expect(note).toEqual({ title: frame.title, body: frame.body, tag: 'in_1' });

      // Not allowed: nothing, even in the background.
      await page.evaluate(() => { window.Notification.permission = 'denied'; });
      conn.send({ ...frame, id: 'in_2' });
      conn.send({ type: 'inbox_count', open: 2, badge: 1 });
      await page.waitForFunction(() => window.__counts.length === 2);
      expect(await page.evaluate(() => window.__notes.length)).toBe(1);
      expect(await page.evaluate(() => window.__counts)).toEqual([[1, 1], [2, 1]]);

      await page.evaluate(() => { window.__hidden = false; window.__notes[0].onclick(); });
      await page.waitForFunction((key) => window.nz.test.selectedKey === key, SESSION_KEY);
      expect(await page.evaluate(() => window.__notes[0].closed)).toBe(true);

      await ctx.close();
    });
  });
}
