// @ts-check
//
// Waiting on the dashboard through the e2e shim. The shim (e2e-shim.js) is a
// module the mock appends to the page, and the bare names it mirrors onto
// window (wsm, WS_STATES, sessionsData, ...) do not exist until it has run. A
// waitForFunction predicate that throws is not retried, so reading one before
// then fails the wait at once with a ReferenceError. waitForShim polls until
// the surface is installed; it stays for the life of the document, so bare
// reads after it are safe until the next goto or reload.

/** @typedef {import('@playwright/test').Page} Page */
/** @typedef {{ timeout?: number }} WaitOpts */

// Well inside the 30 s test timeout, so a shim that never loads fails here
// with the diagnosis below rather than as a bare "Test ended".
const SHIM_TIMEOUT_MS = 10000;

/**
 * Resolves once window.nz.test is installed. On timeout the error says how
 * far the page got, the shim's HTTP status, and the page errors and console
 * errors the current document has produced (load-time ones included).
 * @param {Page} page
 * @param {WaitOpts} [opts]
 */
async function waitForShim(page, opts = {}) {
  try {
    await page.waitForFunction(() => {
      const w = /** @type {any} */ (window);
      return !!(w.nz && w.nz.test && w.nz.test.wsm);
    }, undefined, { timeout: opts.timeout ?? SHIM_TIMEOUT_MS });
  } catch (err) {
    const seen = await page.evaluate(() => {
      const w = /** @type {any} */ (window);
      return {
        url: location.pathname,
        shimTag: !!document.querySelector('script[src="/e2e-shim.js"]'),
        nz: !!w.nz,
        nzTest: !!(w.nz && w.nz.test),
      };
    }).catch((e) => ({ evaluateFailed: String(e) }));
    // page.requests() keeps the last 100 requests across navigations; the
    // newest shim request is this document's unless it has aged out.
    const shimReq = (await page.requests().catch(() => []))
      .filter((r) => r.url().endsWith('/e2e-shim.js')).pop();
    const shimResp = shimReq && await shimReq.response().catch(() => null);
    const shimStatus = !shimReq ? 'not requested' : shimResp ? shimResp.status() : 'no response';
    const since = /** @type {const} */ ({ filter: 'since-navigation' });
    const errors = (await page.pageErrors(since).catch(() => [])).map((e) => e.message);
    const consoleErrors = [...new Set((await page.consoleMessages(since).catch(() => []))
      .filter((m) => m.type() === 'error').map((m) => m.text()))];
    throw new Error('e2e shim not installed: ' + JSON.stringify({ ...seen, shimStatus }) +
      '; page errors: ' + JSON.stringify(errors) +
      '; console errors: ' + JSON.stringify(consoleErrors) + '; ' + String(err), { cause: err });
  }
}

/**
 * Waits for the shim, then for wsm to reach `state`: a WS_STATES key
 * ('CONNECTED') or value ('connected'). A name that is neither throws.
 * @param {Page} page
 * @param {string} [state]
 * @param {WaitOpts} [opts] - applies to the state wait; the shim wait keeps its own bound
 */
async function waitForWs(page, state = 'CONNECTED', opts = {}) {
  await waitForShim(page);
  await page.waitForFunction((s) => {
    // Guarded again: a reload between the two waits swaps in a document
    // whose shim may not have run yet.
    const w = /** @type {any} */ (window);
    const t = w.nz && w.nz.test;
    if (!t) return false;
    const states = t.WS_STATES;
    const want = s in states ? states[s] : s;
    if (!Object.values(states).includes(want)) throw new Error('waitForWs: no WS state ' + s);
    return t.wsm.state === want;
  }, state, opts);
}

/**
 * Waits for the shim, then runs page.waitForFunction(fn, arg, opts) as given:
 * for predicates that read more than wsm.state, or whose return value the
 * caller needs.
 * @template A
 * @param {Page} page
 * @param {(arg: A) => any} fn
 * @param {A} [arg]
 * @param {WaitOpts} [opts]
 */
async function waitForWsWhere(page, fn, arg, opts) {
  await waitForShim(page);
  return page.waitForFunction(fn, arg, opts);
}

module.exports = { waitForShim, waitForWs, waitForWsWhere };
