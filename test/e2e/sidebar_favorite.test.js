// E2E: the project star is optimistic. The star, the favorites-first order
// and an empty favorite group change at the click; a sessions poll answering
// from before the POST does not undo them, and a refused POST puts back what
// was there and says why. Each test gets a fresh mock: the star's state
// persists in the mock across tests.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

let mock;

test.beforeEach(async ({ page }) => {
  mock = await startMockServer();
  await page.goto(`${mock.url}/dashboard`);
  await page.waitForSelector('.session-card', { timeout: 5000 });
});

test.afterEach(async () => {
  await new Promise(r => mock.server.close(r));
});

// holdFavorite parks every POST /api/projects/favorite until release() and
// then answers it with answer(route) (the mock by default), so the page's
// state while the request is out is what the assertions see.
async function holdFavorite(page, answer) {
  let release;
  const held = new Promise((r) => { release = r; });
  await page.route((url) => url.pathname === '/api/projects/favorite', async (route) => {
    await held;
    if (answer) await answer(route); else await route.fallback();
  });
  return () => release();
}

const headerOrder = (page) => page.locator('.section-header .sh-btn[data-action="project-favorite"]').evaluateAll(
  (els) => els.map((e) => e.dataset.name),
);

test('favorite: the star and the order move before the POST answers, and a poll meanwhile does not undo them', async ({ page }) => {
  const release = await holdFavorite(page);
  const header = page.locator('.section-header', { hasText: 'otherproject' });
  const star = header.locator('.sh-btn[data-action="project-favorite"]');
  await expect(star).not.toHaveClass(/star-on/);
  const before = await headerOrder(page);
  expect(before.indexOf('otherproject')).toBeGreaterThan(before.indexOf('myproject'));

  await star.click();
  await expect(star).toHaveClass(/star-on/, { timeout: 1000 });
  const after = await headerOrder(page);
  expect(after.indexOf('otherproject')).toBeLessThan(after.indexOf('myproject'));
  expect(mock.favoriteCalls).toEqual([]);

  // The 5 s poll still answers favorite:false (the mock has not seen the POST).
  const polled = await page.waitForResponse((r) => new URL(r.url()).pathname === '/api/sessions', { timeout: 8000 });
  expect((await polled.json()).stats.projects.find((p) => p.name === 'otherproject').favorite).toBe(false);
  await page.waitForTimeout(300);
  await expect(star).toHaveClass(/star-on/);

  release();
  await expect.poll(() => mock.favoriteCalls.length).toBe(1);
  expect(mock.favoriteCalls[0]).toMatchObject({ name: 'otherproject', favorite: true });
  await expect(page.locator('#toast')).toHaveText('已收藏 otherproject');
  await expect(star).toHaveClass(/star-on/);
});

test('favorite: a refused unfavorite brings the empty favorite group back and says why', async ({ page }) => {
  const release = await holdFavorite(page, (route) => route.fulfill({ status: 500, contentType: 'application/json', body: '{"error":"boom"}' }));
  const header = page.locator('.section-header', { hasText: 'pinned-empty' });
  await expect(header).toHaveCount(1);
  const before = await headerOrder(page);
  await header.locator('.sh-btn[data-action="project-favorite"]').click();
  // pinned-empty has no session: unstarred, its group leaves at once.
  await expect(header).toHaveCount(0, { timeout: 1000 });

  release();
  await expect(page.locator('#toast')).toContainText('取消收藏失败');
  await expect(header).toHaveCount(1);
  await expect(header.locator('.sh-btn.star-on')).toHaveCount(1);
  expect(await headerOrder(page)).toEqual(before);
  expect(mock.favoriteCalls).toEqual([]);
});
