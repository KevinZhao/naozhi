// @ts-check
// E2E: project section-header favorite + GitHub icons.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

let mock;

test.beforeAll(async () => {
  mock = await startMockServer();
});

test.afterAll(async () => {
  await new Promise(r => mock.server.close(r));
});

test.beforeEach(async ({ page }) => {
  mock.resetCalls();
  await page.goto(`${mock.url}/dashboard`);
  // Give the initial fetchSessions a chance to run.
  await page.waitForSelector('.session-card', { timeout: 5000 });
});

test('GitHub icon renders for github-hosted project only', async ({ page }) => {
  // myproject has github: true with a remote URL.
  const ghOnMyproject = page.locator('.section-header', { hasText: 'myproject' }).locator('.sh-btn.github-on');
  await expect(ghOnMyproject).toHaveCount(1);

  // otherproject is github: false, no GitHub icon.
  const ghOnOther = page.locator('.section-header', { hasText: 'otherproject' }).locator('.sh-btn.github-on');
  await expect(ghOnOther).toHaveCount(0);
});

test('clicking GitHub icon shows remote URL toast', async ({ page }) => {
  const ghBtn = page.locator('.section-header', { hasText: 'myproject' }).locator('.sh-btn.github-on');
  await ghBtn.click();
  const toast = page.locator('#toast.show');
  await expect(toast).toContainText('GitHub remote:');
  await expect(toast).toContainText('github.com/acme/myproject.git');
});

test('favorite star toggles and triggers API call', async ({ page }) => {
  const header = page.locator('.section-header', { hasText: 'otherproject' });
  const star = header.locator('.sh-btn').first();
  // Initial: not favorited.
  await expect(star).not.toHaveClass(/star-on/);
  await star.click();

  // API call was sent.
  await expect.poll(() => mock.favoriteCalls.length).toBeGreaterThan(0);
  expect(mock.favoriteCalls[0]).toMatchObject({ name: 'otherproject', favorite: true });

  // After poll completes the star should become active.
  await expect(header.locator('.sh-btn.star-on')).toHaveCount(1, { timeout: 5000 });
});

test('favorited project with no sessions still renders header + sh-new CTA', async ({ page }) => {
  // pinned-empty has favorite: true in the mock but no sessions.
  const header = page.locator('.section-header', { hasText: 'pinned-empty' });
  await expect(header).toHaveCount(1);
  // Star is active.
  await expect(header.locator('.sh-btn.star-on')).toHaveCount(1);
  // The header's compact `+` button is now the sole per-project create
  // affordance — the old full-width "New session in pinned-empty" row below
  // the header was removed as redundant once the header carried its own `+`.
  await expect(header.locator('.sh-btn.sh-new')).toHaveCount(1);
  await expect(page.locator('.section-empty', { hasText: 'pinned-empty' })).toHaveCount(0);
});

test('favorited groups sort before non-favorite groups', async ({ page }) => {
  // Record the full order of section headers.
  const names = await page.locator('.section-header .sh-name').allTextContents();
  // Collect favorite-state from star buttons; favorites must precede non-favorites.
  const stars = await page.locator('.section-header .sh-btn').evaluateAll(
    (els) => els.map((e) => e.classList.contains('star-on'))
  );
  let seenNonFav = false;
  for (const isFav of stars) {
    if (!isFav) {
      seenNonFav = true;
    } else if (seenNonFav) {
      throw new Error('Favorite appeared after a non-favorite in order: ' + JSON.stringify(names));
    }
  }
  // Sanity: pinned-empty is always favorited in this mock.
  expect(names).toContain('pinned-empty');
});
