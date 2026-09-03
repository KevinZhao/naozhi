// @ts-check
// E2E: hold-to-talk after the 30s cap (#2435 item 5).
//
// updateVoiceTimer stops the recorder at MAX_REC_SECS while the finger is
// still down. Before the fix the trailing gesture kept driving the old
// send/cancel logic: an up-swipe flagged "cancel" but the transcript was
// already in flight and still got sent, the lift hid the "正在识别" overlay,
// the 200ms timer kept re-firing the "已达最长" toast until onstop landed, and
// the auto-send replaced a typed draft hidden behind voice mode.
//
// MediaRecorder / getUserMedia are stubbed via addInitScript; the cap is
// reached by rewinding voiceRecStart and invoking updateVoiceTimer directly
// (MAX_REC_SECS is a const, and waiting 30s would exceed the suite timeout).
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

let mock;
const SESSION = 'dashboard:direct:2026-01-01-120000-1:myproject';
const TRANSCRIBE_DELAY_MS = 800;

test.beforeAll(async () => { mock = await startMockServer(); });
test.afterAll(async () => { await new Promise(r => mock.server.close(r)); });

const stubMedia = () => {
  const fakeStream = {
    getTracks: () => [],
    getAudioTracks: () => [{ readyState: 'live', stop() {} }],
  };
  Object.defineProperty(navigator, 'mediaDevices', {
    configurable: true,
    value: { getUserMedia: () => Promise.resolve(fakeStream) },
  });
  class FakeRecorder {
    constructor(_stream, opts) { this.state = 'inactive'; this.mimeType = (opts && opts.mimeType) || 'audio/webm'; }
    static isTypeSupported() { return true; }
    start() { this.state = 'recording'; }
    stop() {
      this.state = 'inactive';
      // Real recorders deliver dataavailable + stop asynchronously.
      setTimeout(() => {
        if (this.ondataavailable) this.ondataavailable({ data: new Blob([new Uint8Array(4096)], { type: this.mimeType }) });
        if (this.onstop) this.onstop();
      }, 30);
    }
  }
  // @ts-ignore
  window.MediaRecorder = FakeRecorder;
  // Count toasts by message so the test can assert the cap toast fired once.
  window.__toasts = [];
  const origShow = window.showToast;
  const wrap = (fn) => (msg, type, dur) => { window.__toasts.push(String(msg)); return fn ? fn(msg, type, dur) : undefined; };
  let cur = wrap(origShow);
  Object.defineProperty(window, 'showToast', {
    configurable: true,
    get() { return cur; },
    set(fn) { cur = wrap(fn); },
  });
};

function touch(page, type, y) {
  return page.evaluate(({ type, y }) => {
    const btn = document.getElementById('btn-hold-talk');
    const t = new Touch({ identifier: 1, target: btn, clientX: 100, clientY: y });
    const ev = new TouchEvent(type, {
      touches: type === 'touchend' ? [] : [t], changedTouches: [t], targetTouches: type === 'touchend' ? [] : [t],
      bubbles: true, cancelable: true,
    });
    (type === 'touchstart' ? btn : document).dispatchEvent(ev);
  }, { type, y });
}

test('30s cap: trailing swipe/lift cannot cancel, hide the overlay or re-toast; draft is appended', async ({ browser }) => {
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, hasTouch: true });
  const page = await ctx.newPage();
  await page.addInitScript(stubMedia);
  mock.resetCalls();
  await page.route('**/api/transcribe', async (route) => {
    await new Promise(r => setTimeout(r, TRANSCRIBE_DELAY_MS));
    await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify({ text: '语音内容' }) });
  });

  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.click(`.session-card[data-key="${SESSION}"]`);
  await page.waitForSelector('#btn-mic');

  // Type a draft, then switch to voice mode (textarea hidden, value kept).
  await page.evaluate(() => setMsgValue(document.getElementById('msg-input'), '草稿'));
  await page.click('#btn-mic');
  await expect(page.locator('#btn-hold-talk')).toBeVisible();

  // Finger down → recording.
  await touch(page, 'touchstart', 600);
  await expect(page.locator('#voice-overlay')).toHaveClass(/show/);
  await expect.poll(() => page.evaluate(() => voiceState)).toBe('recording');

  // Hit the cap while the finger is still down. Tick twice: before the fix
  // the interval kept firing and every tick re-toasted + hid the overlay.
  await page.evaluate(() => { voiceRecStart = Date.now() - (MAX_REC_SECS + 1) * 1000; updateVoiceTimer(); updateVoiceTimer(); });
  expect(await page.evaluate(() => voiceRecTimer)).toBeNull();
  expect(await page.evaluate(() => window.__toasts.filter(m => m.indexOf('已达最长') === 0).length)).toBe(1);

  // Recorder finalizes → "正在识别" overlay.
  await expect(page.locator('#voice-overlay')).toHaveClass(/transcribing/);
  await expect(page.locator('#voice-overlay')).toHaveClass(/show/);

  // Still holding: swipe up (the "cancel" gesture) then lift.
  await touch(page, 'touchmove', 400);
  await touch(page, 'touchend', 400);

  const cls = await page.$eval('#voice-overlay', el => el.className);
  expect(cls).toContain('show');
  expect(cls).toContain('transcribing');
  expect(cls).not.toContain('cancel');
  expect(await page.evaluate(() => voiceCancelled)).toBe(false);
  expect(await page.$eval('#btn-hold-talk', el => el.classList.contains('active'))).toBe(false);
  expect(await page.$eval('#vo-hint', el => el.textContent)).toBe('正在识别...');

  // Transcription lands: exactly one send, draft + transcript, overlay gone.
  await expect.poll(() => mock.sendCalls.length, { timeout: 5000 }).toBe(1);
  expect(JSON.parse(mock.sendCalls[0]).text).toBe('草稿 语音内容');
  await expect(page.locator('#voice-overlay')).not.toHaveClass(/show/);
  expect(await page.evaluate(() => voiceState)).toBe('idle');
  expect(await page.evaluate(() => window.__toasts.filter(m => m.indexOf('已达最长') === 0).length)).toBe(1);

  await ctx.close();
});

test('normal hold: up-swipe still cancels while recording', async ({ browser }) => {
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, hasTouch: true });
  const page = await ctx.newPage();
  await page.addInitScript(stubMedia);
  mock.resetCalls();

  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.click(`.session-card[data-key="${SESSION}"]`);
  await page.waitForSelector('#btn-mic');
  await page.click('#btn-mic');

  await touch(page, 'touchstart', 600);
  await expect.poll(() => page.evaluate(() => voiceState)).toBe('recording');
  await touch(page, 'touchmove', 400);
  await expect(page.locator('#voice-overlay')).toHaveClass(/cancel/);
  await touch(page, 'touchend', 400);

  await expect(page.locator('#voice-overlay')).not.toHaveClass(/show/);
  await expect.poll(() => page.evaluate(() => voiceState)).toBe('idle');
  await page.waitForTimeout(300);
  expect(mock.sendCalls.length).toBe(0);

  await ctx.close();
});
