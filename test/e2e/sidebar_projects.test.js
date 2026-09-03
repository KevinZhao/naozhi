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

// showGitRemote (dashboard.js) opens http(s)/git remotes in a new tab and
// only falls back to the "GitHub remote: …" toast for schemes it refuses to
// open (ssh / git@ — could embed credentials). Both branches were already in
// place at the history-squash commit a274e0cc.
test('clicking GitHub icon opens https remote in a new tab', async ({ page, context }) => {
  // Keep the popup offline — github.com must not be hit from the test runner.
  await context.route('https://github.com/**', route => route.fulfill({ status: 200, contentType: 'text/html', body: '<title>stub</title>' }));
  const ghBtn = page.locator('.section-header', { hasText: 'myproject' }).locator('.sh-btn.github-on');
  const [popup] = await Promise.all([
    page.waitForEvent('popup'),
    ghBtn.click(),
  ]);
  await popup.waitForLoadState();
  expect(popup.url()).toBe('https://github.com/acme/myproject.git');
  // No toast for the opened-in-tab path.
  await expect(page.locator('#toast.show')).toHaveCount(0);
  await popup.close();
});

test('clicking GitHub icon on an ssh remote shows the URL toast fallback', async ({ page }) => {
  // pinned-empty's remote is git@github.com:… — not openable, so the toast
  // surfaces the (truncated) URL instead.
  const ghBtn = page.locator('.section-header', { hasText: 'pinned-empty' }).locator('.sh-btn.github-on');
  await ghBtn.click();
  const toast = page.locator('#toast.show');
  await expect(toast).toContainText('GitHub remote:');
  await expect(toast).toContainText('git@github.com:acme/pinned.git');
});

test('favorite star toggles and triggers API call', async ({ page }) => {
  const header = page.locator('.section-header', { hasText: 'otherproject' });
  // The header's first .sh-btn is the collapse chevron (sh-collapse); target
  // the favorite button by its data-action instead of position.
  const star = header.locator('.sh-btn[data-action="project-favorite"]');
  // Initial: not favorited.
  await expect(star).not.toHaveClass(/star-on/);
  await star.click();

  // API call was sent.
  await expect.poll(() => mock.favoriteCalls.length).toBeGreaterThan(0);
  expect(mock.favoriteCalls[0]).toMatchObject({ name: 'otherproject', favorite: true });

  // After poll completes the star should become active.
  await expect(header.locator('.sh-btn.star-on')).toHaveCount(1, { timeout: 5000 });
});

test('favorited project with no sessions still renders header without per-project + button', async ({ page }) => {
  // pinned-empty has favorite: true in the mock but no sessions.
  const header = page.locator('.section-header', { hasText: 'pinned-empty' });
  await expect(header).toHaveCount(1);
  // Star is active.
  await expect(header.locator('.sh-btn.star-on')).toHaveCount(1);
  // The per-project compact `+` (sh-new) was removed — the top-right `+`
  // is the sole create affordance, so the header carries no sh-new icon
  // and no full-width "New session in X" row below it either.
  await expect(header.locator('.sh-btn.sh-new')).toHaveCount(0);
  await expect(page.locator('.section-empty', { hasText: 'pinned-empty' })).toHaveCount(0);
});

test('favorited groups sort before non-favorite groups', async ({ page }) => {
  // Record the full order of section headers.
  const names = await page.locator('.section-header .sh-name').allTextContents();
  // Collect favorite-state from the star buttons only (headers also carry
  // collapse / GitHub / ⚙ .sh-btn siblings); favorites must precede non-favorites.
  const stars = await page.locator('.section-header .sh-btn[data-action="project-favorite"]').evaluateAll(
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
