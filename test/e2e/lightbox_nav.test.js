// Lightbox gallery navigation E2E (RFC lightbox-gallery-nav).
// Covers: group collection from .event-images, n/N counter, prev/next
// buttons with boundary aria-disabled, ←/→ keyboard nav, Escape close, and
// the attachment-404 → thumbnail fallback path.
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
    const w = await page.$eval('.lightbox-overlay img', el => el.naturalWidth);
    expect(w).toBeGreaterThan(0);

    await page.keyboard.press('Escape');
    await ctx.close();
  });
});
