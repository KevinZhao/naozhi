// @ts-check
// E2E: session-header meta row must stay inside a phone-width viewport.
//
// Production repro (390px, live session on `us.anthropic.claude-fable-5-1`
// with a git chip and recorded runs): .detail-runstats painted past the right
// edge of the screen and the git chip text overlapped "N 轮". Root cause was
// the cli/model label (.detail-left) refusing to shrink (no min-width:0) plus
// the git chip having no overflow clip. Runstats are dropped at ≤480px.
const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');

// Linked-worktree session: the "⑂ feat-x · worktree-feat-x" chip is the long
// git text that spilled over the runstats in production.
const KEY = 'dashboard:direct:2026-01-01-120001-2:otherproject';

let mock;

test.beforeAll(async () => {
  const sessions = defaultSessions();
  // Long, unstripped Bedrock inference-profile model id — the shape that
  // overflowed in production.
  sessions.sessions[1].model = 'us.anthropic.claude-fable-5-1[1m]';
  sessions.sessions[1].cli_version = '2.1.226';
  mock = await startMockServer({
    sessions,
    sessionRuns: {
      [KEY]: {
        runs: [
          { run_id: 'r1', started_at: Date.now() - 600000, duration_ms: 123456, outcome: 'ok', cost_usd: 0.42 },
          { run_id: 'r2', started_at: Date.now() - 300000, duration_ms: 65432, outcome: 'ok', cost_usd: 0.31 },
          { run_id: 'r3', started_at: Date.now() - 100000, duration_ms: 9876, outcome: 'timeout', cost_usd: 0.05 },
        ],
        stats: { count: 3, total_ms: 198764, total_cost_usd: 0.78, timeout_count: 1 },
      },
    },
  });
});

test.afterAll(async () => {
  await new Promise(r => mock.server.close(r));
});

async function openSession(page) {
  await page.goto(`${mock.url}/dashboard`);
  await page.waitForSelector(`.session-card[data-key="${KEY}"]`, { timeout: 5000 });
  const gitSettled = page.waitForResponse(r => r.url().includes('/api/sessions/git'));
  const runsSettled = page.waitForResponse(r => r.url().includes('/api/sessions/runs'));
  await page.locator(`.session-card[data-key="${KEY}"]`).click();
  await gitSettled;
  await runsSettled;
  await expect(page.locator('#header-git .git-chip')).toHaveCount(1);
}

test('390px: header meta row children stay inside the viewport', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await openSession(page);

  const detail = page.locator('.main-header .detail');
  await expect(detail).toBeVisible();

  const geom = await page.evaluate(() => {
    const row = document.querySelector('.main-header .detail');
    const r = row.getBoundingClientRect();
    const kids = Array.from(row.children).map(el => {
      const b = el.getBoundingClientRect();
      const cs = getComputedStyle(el);
      return { cls: el.className, id: el.id, left: b.left, right: b.right, width: b.width, display: cs.display };
    });
    const chipText = document.querySelector('#header-git .git-chip-text');
    const ct = chipText ? chipText.getBoundingClientRect() : null;
    return {
      innerWidth: window.innerWidth,
      row: { left: r.left, right: r.right, scrollWidth: row.scrollWidth, clientWidth: row.clientWidth },
      kids,
      chipText: ct && { left: ct.left, right: ct.right },
    };
  });

  // The row itself is not wider than its box (no horizontal overflow).
  expect(geom.row.scrollWidth).toBeLessThanOrEqual(geom.row.clientWidth + 1);

  // Every rendered child ends inside the viewport.
  for (const k of geom.kids) {
    if (k.display === 'none' || k.width === 0) continue;
    expect(k.right, `${k.cls}#${k.id} right=${k.right} > innerWidth=${geom.innerWidth}`).toBeLessThanOrEqual(geom.innerWidth);
    expect(k.left, `${k.cls}#${k.id} left=${k.left} < 0`).toBeGreaterThanOrEqual(0);
  }

  // The runstats slot specifically (the element that painted off-screen).
  const runstats = geom.kids.find(k => k.id === 'header-runstats');
  expect(runstats).toBeTruthy();
  expect(runstats.right).toBeLessThanOrEqual(geom.innerWidth);
  // At phone width the overview is dropped; it must not render as a clipped
  // fragment in the header (the collapsed run panel still carries it).
  expect(runstats.display).toBe('none');

  // The git chip and the cli/model label do not overlap.
  const left = geom.kids.find(k => k.cls.includes('detail-left'));
  const git = geom.kids.find(k => k.id === 'header-git');
  expect(left).toBeTruthy();
  expect(git).toBeTruthy();
  expect(git.left).toBeGreaterThanOrEqual(left.right - 0.5);

  // The production symptom: .detail-git shrank (min-width:0) but had no
  // overflow clip, so its text painted past the chip's own box and over the
  // "N 轮" runstats. The text must now end inside the chip box.
  expect(geom.chipText).toBeTruthy();
  expect(geom.chipText.right, `git chip text right=${geom.chipText.right} spills past chip box right=${git.right}`)
    .toBeLessThanOrEqual(git.right + 0.5);
  // …and must not reach into any later sibling (runstats / effort / timer).
  for (const k of geom.kids) {
    if (k === git || k.display === 'none' || k.width === 0 || k.left < git.left) continue;
    expect(geom.chipText.right, `git chip text overlaps ${k.cls}#${k.id} (left=${k.left})`).toBeLessThanOrEqual(k.left + 0.5);
  }
});

test('model label strips the us.anthropic. inference-profile prefix', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(`${mock.url}/dashboard`);
  await page.waitForSelector(`.session-card[data-key="${KEY}"]`, { timeout: 5000 });
  await page.locator(`.session-card[data-key="${KEY}"]`).click();
  // renderMainShell paints #header-model synchronously on click; read it in
  // one round-trip.
  const label = await page.evaluate(() => {
    const el = document.getElementById('header-model');
    return el && { text: el.textContent, title: el.getAttribute('title') };
  });
  expect(label).toBeTruthy();
  expect(label.text).toMatch(/claude-fable-5\.1 1m/);
  expect(label.text).not.toContain('us.anthropic');
  // Raw id preserved for debugging.
  expect(label.title).toMatch(/us\.anthropic\.claude-fable-5-1\[1m\]/);
});

test('desktop: runstats remain visible and inside the viewport', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await openSession(page);
  const runstats = page.locator('#header-runstats');
  await expect(runstats).toContainText('3 轮');
  const r = await runstats.evaluate(el => {
    const b = el.getBoundingClientRect();
    return { right: b.right, innerWidth: window.innerWidth };
  });
  expect(r.right).toBeLessThanOrEqual(r.innerWidth);
});
