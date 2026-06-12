// @ts-check
// E2E for the uuid-idempotent event render fix that stops a post-restart
// re-subscribe history replay from painting the same user message twice.
// See docs/rfc/dashboard-event-uuid-idempotent-render.md.
//
// The WS handlers (onEvent/onHistory) read module-private state
// (selectedKey/lastRenderedEventTime) not reachable from page.evaluate, so
// these tests drive the *dedup layer* directly in a real browser DOM:
// window.eventHtml (already exposed for agent_view.js) + the global
// eventAlreadyRendered helper. This exercises the actual rendered markup and
// the actual querySelector path — the mechanism the bug fix relies on.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const desktop = { viewport: { width: 1280, height: 800 } };

test.describe('Event uuid idempotent render', () => {
  let mock;

  test.beforeAll(async () => { mock = await startMockServer(); });
  test.afterAll(() => mock.server.close());

  async function open(browser) {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForLoadState('networkidle');
    return { ctx, page };
  }

  test('eventHtml stamps data-uuid on the .event element', async ({ browser }) => {
    const { ctx, page } = await open(browser);

    const attr = await page.evaluate(() => {
      // @ts-ignore — eventHtml is exposed on window for agent_view.js
      const html = window.eventHtml({ type: 'user', detail: 'hi', uuid: 'abc123', time: 1700000000000 });
      const tmp = document.createElement('div');
      tmp.innerHTML = html;
      const ev = tmp.querySelector('.event');
      return ev ? ev.getAttribute('data-uuid') : null;
    });
    expect(attr).toBe('abc123');

    await ctx.close();
  });

  test('uuid-less event omits data-uuid (not empty string)', async ({ browser }) => {
    const { ctx, page } = await open(browser);

    const hasAttr = await page.evaluate(() => {
      // @ts-ignore
      const html = window.eventHtml({ type: 'user', detail: 'no uuid here', time: 1700000000000 });
      const tmp = document.createElement('div');
      tmp.innerHTML = html;
      const ev = tmp.querySelector('.event');
      return ev ? ev.hasAttribute('data-uuid') : null;
    });
    expect(hasAttr).toBe(false);

    await ctx.close();
  });

  test('eventAlreadyRendered: present uuid matches, absent does not', async ({ browser }) => {
    const { ctx, page } = await open(browser);

    const result = await page.evaluate(() => {
      const scroll = document.createElement('div');
      // @ts-ignore
      scroll.innerHTML = window.eventHtml({ type: 'user', detail: 'hello', uuid: 'U1', time: 1 });
      // @ts-ignore
      return {
        present: eventAlreadyRendered(scroll, 'U1'),
        absent: eventAlreadyRendered(scroll, 'U2'),
        empty: eventAlreadyRendered(scroll, ''),
      };
    });
    expect(result.present).toBe(true);
    expect(result.absent).toBe(false);
    // Empty/absent uuid must NEVER match — otherwise uuid-less events get swallowed.
    expect(result.empty).toBe(false);

    await ctx.close();
  });

  test('eventAlreadyRendered tolerates selector-special uuid (CSS.escape)', async ({ browser }) => {
    const { ctx, page } = await open(browser);

    // A uuid containing quote/bracket chars would break a naive attribute
    // selector; CSS.escape must keep the query well-formed and correct.
    const result = await page.evaluate(() => {
      const weird = 'a"]b[c';
      const scroll = document.createElement('div');
      // @ts-ignore
      scroll.innerHTML = window.eventHtml({ type: 'user', detail: 'x', uuid: weird, time: 1 });
      // @ts-ignore
      return { match: eventAlreadyRendered(scroll, weird), other: eventAlreadyRendered(scroll, 'zzz') };
    });
    expect(result.match).toBe(true);
    expect(result.other).toBe(false);

    await ctx.close();
  });

  test('dedup is DOM-sourced: a trimmed element stops matching', async ({ browser }) => {
    const { ctx, page } = await open(browser);

    // Emulate trimEventsScroll eviction: once the .event[data-uuid] node is
    // removed from the DOM, eventAlreadyRendered must report it as gone. This
    // pins the "no parallel JS Set" invariant — DOM is the single source of
    // truth, so eviction and full rebuilds keep dedup automatically consistent.
    const result = await page.evaluate(() => {
      const scroll = document.createElement('div');
      // @ts-ignore
      scroll.innerHTML = window.eventHtml({ type: 'user', detail: 'temp', uuid: 'TRIM1', time: 1 });
      // @ts-ignore
      const before = eventAlreadyRendered(scroll, 'TRIM1');
      const node = scroll.querySelector('.event[data-uuid="TRIM1"]');
      if (node) node.remove();
      // @ts-ignore
      const after = eventAlreadyRendered(scroll, 'TRIM1');
      return { before, after };
    });
    expect(result.before).toBe(true);
    expect(result.after).toBe(false);

    await ctx.close();
  });

  test('replay scenario: rendering the same user uuid is detectable as a dup', async ({ browser }) => {
    const { ctx, page } = await open(browser);

    // This mirrors the bug's core: a real user bubble (uuid=U) is on screen,
    // then a restart re-subscribe replays the same uuid. The render guard must
    // see it as already-present so the caller skips the second append.
    const result = await page.evaluate(() => {
      const scroll = document.createElement('div');
      // first render (the live onEvent path)
      // @ts-ignore
      scroll.insertAdjacentHTML('beforeend', window.eventHtml({ type: 'user', detail: '升级到最新版本', uuid: 'REPLAY', time: 1781266649481 }));

      // replay arrives (the onHistory path): the guard the fix adds
      // @ts-ignore
      const seen = eventAlreadyRendered(scroll, 'REPLAY');
      if (!seen) {
        // only the buggy code path would re-append here
        // @ts-ignore
        scroll.insertAdjacentHTML('beforeend', window.eventHtml({ type: 'user', detail: '升级到最新版本', uuid: 'REPLAY', time: 1781266649481 }));
      }
      return {
        seen,
        count: scroll.querySelectorAll('.event.user[data-uuid="REPLAY"]').length,
      };
    });
    expect(result.seen).toBe(true);
    // The whole point: exactly one bubble, never two.
    expect(result.count).toBe(1);

    await ctx.close();
  });
});
