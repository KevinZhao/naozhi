// @ts-check
// Lightbox gallery navigation E2E (RFC lightbox-gallery-nav).
// Covers: group collection from .event-images, n/N counter, prev/next
// buttons with boundary aria-disabled, ←/→ keyboard nav, Escape close,
// single-image .lb-single mode, toolbar zoom buttons, and the
// attachment-404 → thumbnail fallback path.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const desktop = { viewport: { width: 1280, height: 800 } };
const SESSION_KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';

// Distinct 1x1 PNG data URIs (red / green / blue) so each thumb is unique.
const THUMB_R = 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==';
const THUMB_G = 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==';
const THUMB_B = 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNgYPj/HwADAgH/p6FmwQAAAABJRU5ErkJggg==';

function galleryEvents() {
  return [
    { type: 'system', summary: 'session started', time: Date.now() - 10000 },
    {
      type: 'user',
      detail: 'three screenshots',
      time: Date.now() - 8000,
      images: [THUMB_R, THUMB_G, THUMB_B],
      image_paths: [
        '.naozhi/attachments/2026-01-01/a.png',
        '.naozhi/attachments/2026-01-01/b.png',
        '.naozhi/attachments/2026-01-01/c.png',
      ],
    },
    {
      type: 'user',
      detail: 'single shot',
      time: Date.now() - 5000,
      images: [THUMB_R],
      image_paths: ['.naozhi/attachments/2026-01-01/solo.png'],
    },
    {
      type: 'user',
      detail: 'expired attachment',
      time: Date.now() - 3000,
      images: [THUMB_G],
      image_paths: ['.naozhi/attachments/2026-01-01/missing.png'],
    },
  ];
}

async function openSession(page, mockUrl) {
  await page.goto(mockUrl + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.click(`.session-card[data-key="${SESSION_KEY}"]`);
  await page.waitForSelector('.event-images img');
}

test.describe('Lightbox gallery navigation', () => {
  let mock;

  test.beforeAll(async () => {
    mock = await startMockServer({ events: galleryEvents() });
  });
  test.afterAll(() => mock.server.close());

  test('clicking 2nd thumbnail opens group at 2/3 with full-size src', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await openSession(page, mock.url);

    await page.click('.event-images img:nth-child(2)');
    await page.waitForSelector('.lightbox-overlay.active');

    await expect(page.locator('.lb-counter')).toHaveText('2 / 3');
    // Full-size image is fetched from the attachment endpoint, not the thumb.
    const src = await page.$eval('.lightbox-overlay img', el => el.src);
    expect(src).toContain('/api/sessions/attachment');
    expect(src).toContain(encodeURIComponent('.naozhi/attachments/2026-01-01/b.png'));
    // Nav rails visible (not .lb-single), neither boundary disabled.
    await expect(page.locator('.lightbox-overlay')).not.toHaveClass(/lb-single/);
    await expect(page.locator('.lb-nav-prev')).toHaveAttribute('aria-disabled', 'false');
    await expect(page.locator('.lb-nav-next')).toHaveAttribute('aria-disabled', 'false');

    await ctx.close();
  });

  test('next/prev buttons navigate and disable at boundaries', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await openSession(page, mock.url);

    await page.click('.event-images img:nth-child(2)');
    await page.waitForSelector('.lightbox-overlay.active');

    await page.click('.lb-nav-next');
    await expect(page.locator('.lb-counter')).toHaveText('3 / 3');
    await expect(page.locator('.lb-nav-next')).toHaveAttribute('aria-disabled', 'true');
    const src3 = await page.$eval('.lightbox-overlay img', el => el.src);
    expect(src3).toContain(encodeURIComponent('.naozhi/attachments/2026-01-01/c.png'));

    await page.click('.lb-nav-prev');
    await page.click('.lb-nav-prev');
    await expect(page.locator('.lb-counter')).toHaveText('1 / 3');
    await expect(page.locator('.lb-nav-prev')).toHaveAttribute('aria-disabled', 'true');
    await expect(page.locator('.lb-nav-next')).toHaveAttribute('aria-disabled', 'false');

    await ctx.close();
  });

  test('ArrowRight/ArrowLeft navigate, Escape closes', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await openSession(page, mock.url);

    await page.click('.event-images img:nth-child(1)');
    await page.waitForSelector('.lightbox-overlay.active');
    await expect(page.locator('.lb-counter')).toHaveText('1 / 3');

    await page.keyboard.press('ArrowRight');
    await expect(page.locator('.lb-counter')).toHaveText('2 / 3');
    await page.keyboard.press('ArrowRight');
    await expect(page.locator('.lb-counter')).toHaveText('3 / 3');
    // Past the boundary: no wrap-around.
    await page.keyboard.press('ArrowRight');
    await expect(page.locator('.lb-counter')).toHaveText('3 / 3');
    await page.keyboard.press('ArrowLeft');
    await expect(page.locator('.lb-counter')).toHaveText('2 / 3');

    await page.keyboard.press('Escape');
    await expect(page.locator('.lightbox-overlay')).not.toHaveClass(/active/);

    await ctx.close();
  });

  test('single-image message hides nav rails and counter', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await openSession(page, mock.url);

    // The "single shot" event holds exactly one image.
    const solo = page.locator('.event', { hasText: 'single shot' }).locator('.event-images img');
    await solo.click();
    await page.waitForSelector('.lightbox-overlay.active');

    await expect(page.locator('.lightbox-overlay')).toHaveClass(/lb-single/);
    await expect(page.locator('.lb-nav-prev')).toBeHidden();
    await expect(page.locator('.lb-nav-next')).toBeHidden();
    await expect(page.locator('.lb-counter')).toBeHidden();

    await page.keyboard.press('Escape');
    await ctx.close();
  });

  test('toolbar zoom buttons change scale and show hint', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await openSession(page, mock.url);

    await page.click('.event-images img:nth-child(1)');
    await page.waitForSelector('.lightbox-overlay.active');

    await page.click('[data-lb-action="zoom-in"]');
    await expect(page.locator('.lb-zoom-hint')).toHaveClass(/visible/);
    await expect(page.locator('.lb-zoom-hint')).toHaveText('120%');
    // Zoomed state marks the overlay (grab cursor affordance).
    await expect(page.locator('.lightbox-overlay')).toHaveClass(/zoomed/);

    await page.click('[data-lb-action="zoom-out"]');
    await expect(page.locator('.lb-zoom-hint')).toHaveText('100%');
    await expect(page.locator('.lightbox-overlay')).not.toHaveClass(/zoomed/);

    await ctx.close();
  });

  test('zoom resets when navigating to the next image', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await openSession(page, mock.url);

    await page.click('.event-images img:nth-child(1)');
    await page.waitForSelector('.lightbox-overlay.active');

    await page.click('[data-lb-action="zoom-in"]');
    await expect(page.locator('.lightbox-overlay')).toHaveClass(/zoomed/);

    await page.click('.lb-nav-next');
    await expect(page.locator('.lb-counter')).toHaveText('2 / 3');
    await expect(page.locator('.lightbox-overlay')).not.toHaveClass(/zoomed/);

    await ctx.close();
  });

  test('GC-expired attachment falls back to thumbnail data URI', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await openSession(page, mock.url);

    const expired = page.locator('.event', { hasText: 'expired attachment' }).locator('.event-images img');
    await expired.click();
    await page.waitForSelector('.lightbox-overlay.active');

    // The attachment 404s (path contains "missing"); loadWithFallback must
    // silently swap to the embedded thumbnail instead of a broken image.
    await expect.poll(async () =>
      page.$eval('.lightbox-overlay img', el => el.src)
    ).toContain('data:image/png');
    const w = await page.$eval('.lightbox-overlay img', el => /** @type {HTMLImageElement} */(el).naturalWidth);
    expect(w).toBeGreaterThan(0);

    await page.keyboard.press('Escape');
    await ctx.close();
  });

  // ── Touch gesture arbitration (RFC §3) ──
  // webkit (mobile-safari project) lacks system libs on this host, so the
  // swipe contract is exercised on chromium with synthetic TouchEvents:
  // isTrusted=false but addEventListener-based handlers fire identically.
  /** Dispatch a touch sequence on the lightbox <img>. */
  async function dispatchSwipe(page, dx, opts = {}) {
    await page.evaluate(([dx, opts]) => {
      const img = document.querySelector('.lightbox-overlay img');
      const mk = (type, touches, changed) => {
        const toTouch = (p, i) => new Touch({
          identifier: i, target: img, clientX: p.x, clientY: p.y,
        });
        img.dispatchEvent(new TouchEvent(type, {
          bubbles: true, cancelable: true,
          touches: touches.map(toTouch),
          changedTouches: changed.map(toTouch),
        }));
      };
      const start = { x: 200, y: 300 };
      if (opts.pinchFirst) {
        // Two fingers down then up: gesture must be classified as pinch,
        // never as a navigation swipe, regardless of horizontal travel.
        mk('touchstart', [start, { x: 260, y: 300 }], [start, { x: 260, y: 300 }]);
        mk('touchend', [], [start, { x: 260, y: 300 }]);
        return;
      }
      const end = { x: start.x + dx, y: start.y + (opts.dy || 0) };
      mk('touchstart', [start], [start]);
      mk('touchend', [], [end]);
    }, [dx, opts]);
  }

  test('horizontal touch swipe navigates; vertical and pinch do not', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop, hasTouch: true });
    const page = await ctx.newPage();
    await openSession(page, mock.url);

    await page.click('.event-images img:nth-child(1)');
    await page.waitForSelector('.lightbox-overlay.active');
    await expect(page.locator('.lb-counter')).toHaveText('1 / 3');

    // Swipe left → next image.
    await dispatchSwipe(page, -120);
    await expect(page.locator('.lb-counter')).toHaveText('2 / 3');
    // Swipe right → back.
    await dispatchSwipe(page, 120);
    await expect(page.locator('.lb-counter')).toHaveText('1 / 3');
    // The synthesized-click guard must keep the overlay open after swipes.
    await expect(page.locator('.lightbox-overlay')).toHaveClass(/active/);

    // Mostly-vertical drag (dx 60 < dy 200 * 1.2): not a nav swipe.
    await dispatchSwipe(page, 60, { dy: 200 });
    await expect(page.locator('.lb-counter')).toHaveText('1 / 3');

    // Two-finger gesture: pinch classification wins, no navigation.
    await dispatchSwipe(page, 0, { pinchFirst: true });
    await expect(page.locator('.lb-counter')).toHaveText('1 / 3');

    await ctx.close();
  });

  test('swipe is disabled while zoomed (single finger pans instead)', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop, hasTouch: true });
    const page = await ctx.newPage();
    await openSession(page, mock.url);

    await page.click('.event-images img:nth-child(1)');
    await page.waitForSelector('.lightbox-overlay.active');

    await page.click('[data-lb-action="zoom-in"]');
    await page.click('[data-lb-action="zoom-in"]');
    await expect(page.locator('.lightbox-overlay')).toHaveClass(/zoomed/);

    // swipeScale is captured at touchstart (> 1.05): horizontal travel must
    // pan the zoomed image, not flip the page.
    await dispatchSwipe(page, -120);
    await expect(page.locator('.lb-counter')).toHaveText('1 / 3');

    await ctx.close();
  });

  test('legacy events without image_paths still open a navigable group', async ({ browser }) => {
    // Events persisted before image_paths existed carry only data URIs:
    // data-full === data-thumb, navigation must still work.
    const legacyMock = await startMockServer({
      events: [
        {
          type: 'user',
          detail: 'legacy pair',
          time: Date.now() - 5000,
          images: [THUMB_R, THUMB_B],
        },
      ],
    });
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await openSession(page, legacyMock.url);

    await page.click('.event-images img:nth-child(1)');
    await page.waitForSelector('.lightbox-overlay.active');
    await expect(page.locator('.lb-counter')).toHaveText('1 / 2');

    await page.keyboard.press('ArrowRight');
    await expect(page.locator('.lb-counter')).toHaveText('2 / 2');
    const src = await page.$eval('.lightbox-overlay img', el => el.src);
    expect(src).toContain('data:image/png');

    await ctx.close();
    legacyMock.server.close();
  });
});
