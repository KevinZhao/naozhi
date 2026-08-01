// effort_tag.test.js — header effort tag rendering (kiro thinking-effort tier).
//
// Covers what the Go source-greps in static_effort_tag_contract_test.go cannot:
// that the tag actually reaches the DOM, carries the raw kiro tier as its text,
// emphasises only the top two tiers, and stays absent for backends that report
// no effort. docs/rfc/kiro-effort-visibility.md
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

// Four sessions: a kiro one per interesting tier plus a claude one that reports
// none. Keys are shaped like the dashboard's own so nothing else in the UI
// treats them specially.
function effortSessions() {
  const base = {
    state: 'ready',
    platform: 'dashboard',
    agent: 'general',
    workspace: '/home/user/workspace/myproject',
    last_active: Date.now() - 60000,
    node: 'local',
    project: 'myproject',
  };
  return {
    sessions: [
      {
        ...base,
        // This exact key carries a git state in mock-server's defaultGitStates,
        // so it exercises the effort tag ALONGSIDE the git chip — both want the
        // row's free space via margin-right:auto, and only one may win.
        key: 'dashboard:direct:2026-01-01-120000-1:myproject',
        cli_name: 'kiro',
        cli_version: '2.16.0',
        backend: 'kiro',
        model: 'claude-fable-5',
        effort: 'max',
      },
      {
        ...base,
        key: 'dashboard:direct:2026-01-01-120001-2:kiro-xhigh',
        cli_name: 'kiro',
        cli_version: '2.16.0',
        backend: 'kiro',
        model: 'claude-fable-5',
        effort: 'xhigh',
      },
      {
        ...base,
        key: 'dashboard:direct:2026-01-01-120002-3:kiro-medium',
        cli_name: 'kiro',
        cli_version: '2.16.0',
        backend: 'kiro',
        model: 'claude-fable-5',
        effort: 'medium',
      },
      {
        ...base,
        key: 'dashboard:direct:2026-01-01-120003-4:claude-none',
        cli_name: 'claude',
        cli_version: '2.1.143',
        backend: 'claude',
        model: 'claude-opus-4.7',
        // no effort field at all — claude reports none
      },
    ],
    stats: { version: 1, running: 0, ready: 4, max_procs: 10 },
    nodes: {},
    history_sessions: [],
  };
}

// selectByKey clicks the sidebar card for a key and waits for the header to
// settle on that session.
async function selectByKey(page, keySuffix) {
  const card = page.locator(`.session-card[data-key*="${keySuffix}"]`).first();
  await card.click();
  await page.waitForTimeout(350);
}

test.describe('header effort tag', () => {
  let server;
  let baseURL;

  test.beforeAll(async () => {
    server = await startMockServer({ sessions: effortSessions() });
    baseURL = server.url;
  });

  test.afterAll(async () => {
    if (server) await new Promise(r => server.server.close(r));
  });

  test('renders the raw kiro tier and emphasises max', async ({ page }) => {
    await page.goto(baseURL + '/dashboard');
    await page.waitForSelector('.session-card', { timeout: 8000 });
    await selectByKey(page, '120000-1:myproject');

    const tag = page.locator('.main-header .detail-effort .effort-tag');
    await expect(tag).toHaveCount(1);
    // Raw kiro token, NOT a translation — the operator has to be able to map
    // what they read here onto /effort and ~/.kiro/settings/cli.json.
    await expect(tag).toHaveText('max');
    await expect(tag).toHaveClass(/effort-hot/);
    // Tooltip carries the Chinese gloss.
    await expect(tag).toHaveAttribute('title', /thinking effort: max/);
  });

  test('xhigh is emphasised, medium is not', async ({ page }) => {
    await page.goto(baseURL + '/dashboard');
    await page.waitForSelector('.session-card', { timeout: 8000 });

    await selectByKey(page, 'kiro-xhigh');
    const xhigh = page.locator('.main-header .detail-effort .effort-tag');
    await expect(xhigh).toHaveText('xhigh');
    await expect(xhigh).toHaveClass(/effort-hot/);

    await selectByKey(page, 'kiro-medium');
    const medium = page.locator('.main-header .detail-effort .effort-tag');
    await expect(medium).toHaveText('medium');
    await expect(medium).not.toHaveClass(/effort-hot/);
  });

  // The .detail row is justify-content:space-between, so without
  // margin-right:auto the tag gets flung to the horizontal centre, visually
  // divorced from the model label it qualifies. Caught by screenshot during
  // implementation; pinned here because a CSS tidy-up would regress it
  // silently — every assertion above still passes with the tag mid-header.
  test('tag sits next to the model label, not centred', async ({ page }) => {
    await page.goto(baseURL + '/dashboard');
    await page.waitForSelector('.session-card', { timeout: 8000 });
    await selectByKey(page, 'kiro-medium');

    // Anchor on .detail-left (cli + model labels), which always renders —
    // .model-label is absent when the backend never reported a model.
    await expect(page.locator('.main-header .detail-effort .effort-tag')).toHaveCount(1);
    const detail = await page.locator('.main-header .detail').boundingBox();
    const left = await page.locator('.main-header .detail-left').boundingBox();
    const tag = await page.locator('.main-header .detail-effort').boundingBox();

    // Close behind the cli/model labels rather than adrift in the middle.
    const gap = tag.x - (left.x + left.width);
    expect(gap, `effort tag is ${gap}px from .detail-left`).toBeLessThan(24);
    // And well left of centre, which is where space-between would park it.
    expect(tag.x).toBeLessThan(detail.x + detail.width / 2);
  });

  // The git chip also claims the row's free space via margin-right:auto, and in
  // flex the FIRST auto margin absorbs all of it. Without the :has() override
  // that makes the git chip yield, the effort tag lands at the far right —
  // past the row's centre, i.e. the very failure the test above screens for,
  // but only reproducible when a git state exists for the selected session.
  test('stays anchored left when the git chip is also present', async ({ page }) => {
    await page.goto(baseURL + '/dashboard');
    await page.waitForSelector('.session-card', { timeout: 8000 });
    await selectByKey(page, '120000-1:myproject');

    // Wait for the async git chip to fill in — the layout conflict only exists
    // once both chips are on the row.
    await expect(page.locator('.main-header .git-chip')).toHaveCount(1);

    const detail = await page.locator('.main-header .detail').boundingBox();
    const left = await page.locator('.main-header .detail-left').boundingBox();
    const tag = await page.locator('.main-header .detail-effort').boundingBox();

    const gap = tag.x - (left.x + left.width);
    expect(gap, `effort tag is ${gap}px from .detail-left with a git chip present`)
      .toBeLessThan(180); // git chip sits between them, so a wider bound than the no-git case
    expect(tag.x, 'effort tag drifted past the row centre').toBeLessThan(detail.x + detail.width / 2);
  });

  // This is the regression that a version-gated repaint hides: a turn boundary
  // changes the tier WITHOUT advancing stats.version, so a repaint placed after
  // fetchSessions' short-circuit never runs. Every other assertion in this file
  // passes in that broken state because they all navigate (which rebuilds the
  // header via renderMainShell).
  test('picks up a tier change that does not bump stats.version', async ({ page }) => {
    const KEY = 'dashboard:direct:2026-01-01-120002-3:kiro-medium';
    try {
      await page.goto(baseURL + '/dashboard');
      await page.waitForSelector('.session-card', { timeout: 8000 });
      await selectByKey(page, 'kiro-medium');
      await expect(page.locator('.main-header .detail-effort .effort-tag')).toHaveText('medium');

      // Simulate the turn boundary: tier flips, version stays put.
      server.setSessionEffortWithoutVersionBump(KEY, 'max');

      // No navigation, no reload — just wait for the 5 s poll to come round.
      const tag = page.locator('.main-header .detail-effort .effort-tag');
      await expect(tag).toHaveText('max', { timeout: 12000 });
      await expect(tag).toHaveClass(/effort-hot/);
    } finally {
      // The mock server is shared across this file's tests, so put the tier
      // back or every later case reads 'max' for this session.
      server.setSessionEffortWithoutVersionBump(KEY, 'medium');
    }
  });

  test('absent for a backend that reports no effort', async ({ page }) => {
    await page.goto(baseURL + '/dashboard');
    await page.waitForSelector('.session-card', { timeout: 8000 });
    await selectByKey(page, 'claude-none');

    // Mount node exists but stays empty, so :empty collapses it.
    await expect(page.locator('.main-header .detail-effort .effort-tag')).toHaveCount(0);
    const mount = page.locator('.main-header #header-effort');
    await expect(mount).toHaveCount(1);
    await expect(mount).not.toBeVisible(); // :empty{display:none}
  });

  test('switching sessions replaces the tier rather than stacking', async ({ page }) => {
    await page.goto(baseURL + '/dashboard');
    await page.waitForSelector('.session-card', { timeout: 8000 });

    await selectByKey(page, '120000-1:myproject');
    await selectByKey(page, 'kiro-medium');
    // Exactly one tag, showing the newly selected session's tier — a stale
    // repaint would leave two, or leave "max" behind.
    const tags = page.locator('.main-header .detail-effort .effort-tag');
    await expect(tags).toHaveCount(1);
    await expect(tags).toHaveText('medium');

    // And back to a session with no effort clears it rather than inheriting.
    await selectByKey(page, 'claude-none');
    await expect(page.locator('.main-header .detail-effort .effort-tag')).toHaveCount(0);
  });

  test('no script errors while rendering the tag', async ({ page }) => {
    // Only uncaught script errors matter here. The mock server serves no
    // favicon / optional assets and has no /ws endpoint, so resource 404s and
    // the WebSocket handshake failure are expected noise in this harness.
    const errors = [];
    page.on('pageerror', e => errors.push(String(e)));

    await page.goto(baseURL + '/dashboard');
    await page.waitForSelector('.session-card', { timeout: 8000 });
    await selectByKey(page, '120000-1:myproject');
    await selectByKey(page, 'claude-none');

    expect(errors, 'script errors: ' + errors.join(' | ')).toHaveLength(0);
  });
});
