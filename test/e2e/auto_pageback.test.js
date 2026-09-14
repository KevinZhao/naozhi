// @ts-check
// The initial history page is the newest INITIAL_HISTORY_LIMIT (100) entries.
// dashboard.js hides the INTERNAL_EVENT_TYPES, so a session whose last 100
// entries are all internal (a parallel agent team's tool_use / task_progress
// churn) renders a blank transcript and strands the operator on the
// "该会话最近仅有 agent 活动" placeholder. maybeAutoPageBack recovers by paging
// backwards until a visible bubble appears, bounded by AUTO_PAGEBACK_MAX.
//
// This replaced the second half of
// internal/server/static_visible_events_contract_test.go, which listed six
// substrings of dashboard.js: `let _autoPageBackCount = 0;`,
// `const AUTO_PAGEBACK_MAX = 3;`, `function maybeAutoPageBack(`,
// `if (_autoPageBackCount >= AUTO_PAGEBACK_MAX) return;`,
// `if (!html && events.length > 0) maybeAutoPageBack();`, and — a comment —
// `_autoPageBackCount = 0; // reset the blank-page recovery budget per session`
// (#2547). Pinning a comment's text cannot fail for any reason that matters,
// and the five code substrings say the machinery is spelled out, not that a
// blank page ever recovers or that the recovery ever stops.
//
// That file's first test, TestInternalEventTypes_JSGoParity, stays: it compares
// the JS hidden-type Set against clievent.IsInternalEventType element by
// element in both directions, which is the cheap real-drift guard #2547 lists
// as worth keeping.
//
// 跑法：cd test/e2e && npx playwright test auto_pageback.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const desktop = { viewport: { width: 1280, height: 800 } };
const KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';
const PAGE = 100; // INITIAL_HISTORY_LIMIT
const MAX = 3;    // AUTO_PAGEBACK_MAX
const BASE = 1750000000000;

/**
 * internalRun builds n consecutive internal-only entries. tool_use is in
 * INTERNAL_EVENT_TYPES, so none of these renders a bubble.
 * @param {number} n @param {number} from @param {string} tag
 */
function internalRun(n, from, tag) {
  return Array.from({ length: n }, (_, i) => ({
    type: 'tool_use',
    tool: 'Read',
    detail: 'internal ' + tag + '-' + i,
    time: BASE + from + i,
    uuid: 'int-' + tag + '-' + i,
  }));
}

/** @param {number} at @param {string} text */
function visible(at, text) {
  return { type: 'text', detail: text, time: BASE + at, uuid: 'vis-' + at };
}

/**
 * @param {import('@playwright/test').Browser} browser
 * @param {any[]} events
 */
async function openSession(browser, events) {
  const mock = await startMockServer({ eventsByKey: { [KEY]: events } });
  const ctx = await browser.newContext({ ...desktop });
  const page = await ctx.newPage();
  /** @type {string[]} */
  const pageBacks = [];
  page.on('request', r => {
    const u = new URL(r.url());
    if (u.pathname === '/api/sessions/events' && u.searchParams.get('before')) pageBacks.push(u.search);
  });
  const cleanup = async () => {
    await ctx.close();
    mock.server.close();
  };
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.click(`.session-card[data-key="${KEY}"]`);
  await page.waitForSelector('#events-scroll');
  return { page, pageBacks, cleanup };
}

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'recovery is viewport-independent; desktop-chrome only');
  }
});

test.describe('全内部事件的首屏自动回翻', () => {
  test('首屏全是内部事件时回翻到有可见气泡的那一页', async ({ browser }) => {
    // Oldest first: one visible bubble, then enough internal churn that the
    // newest PAGE entries contain nothing renderable.
    const events = [
      visible(0, 'the answer you are looking for'),
      ...internalRun(PAGE + 20, 10, 'a'),
    ];
    const { page, pageBacks, cleanup } = await openSession(browser, events);
    try {
      // Recovery must happen on its own — no scroll, no click.
      const bubble = page.locator('#events-scroll .event');
      await expect(bubble).toHaveCount(1, { timeout: 10000 });
      await expect(bubble).toContainText('the answer you are looking for');
      // And it got there by paging back, not by rendering the first page.
      expect(pageBacks.length).toBeGreaterThan(0);
      // The placeholder is gone once a bubble exists.
      await expect(page.locator('#events-scroll')).not.toContainText('仅有 agent 活动');
    } finally {
      await cleanup();
    }
  });

  test('整段历史都是内部事件时回翻有上限，不无限拉', async ({ browser }) => {
    // No visible entry anywhere: recovery can never succeed, so the only
    // correct behaviour is to give up. An unbounded loop would walk the whole
    // history one page at a time on every session open.
    const { page, pageBacks, cleanup } = await openSession(browser, internalRun(PAGE * 6, 0, 'b'));
    try {
      // The placeholder is what the operator sees, and it stays.
      await expect(page.locator('#events-scroll')).toContainText('仅有 agent 活动', { timeout: 10000 });
      await expect(page.locator('#events-scroll .event')).toHaveCount(0);

      // Give the loop room to misbehave before counting.
      await page.waitForTimeout(2500);
      expect(pageBacks.length, `page-backs must stop at AUTO_PAGEBACK_MAX (${MAX})`)
        .toBeLessThanOrEqual(MAX);
      // …and it must have actually tried, or the bound above is vacuous.
      expect(pageBacks.length).toBeGreaterThan(0);
    } finally {
      await cleanup();
    }
  });

  test('首屏本来就有可见气泡时完全不回翻', async ({ browser }) => {
    const events = [
      ...internalRun(20, 0, 'c'),
      visible(500, 'already visible on the first page'),
    ];
    const { page, pageBacks, cleanup } = await openSession(browser, events);
    try {
      await expect(page.locator('#events-scroll .event')).toHaveCount(1);
      await page.waitForTimeout(1500);
      expect(pageBacks, 'a non-blank first page must not trigger recovery').toHaveLength(0);
    } finally {
      await cleanup();
    }
  });
});
