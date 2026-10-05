// node --test scripts/release-gate-static.test.mjs — the release gate's /static
// load rules (#3330) against a recorded load of the rendered dashboard, and one
// broken load per rule.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import { IMMUTABLE_CACHE, recordStatic, staticLoadProblems } from './release-gate-static.mjs';

const BASE = 'http://127.0.0.1:18180';

// A page as renderDashboardHTML writes it: two modules in the import map (one
// an entry module behind an inline loader), one versioned stylesheet.
function rendered() {
  return {
    maps: 1,
    imports: { '/static/dashboard.js': '/static/dashboard.js?v=aaaa', '/static/state.js': '/static/state.js?v=bbbb' },
    urls: ['/static/dashboard.js?v=aaaa', '/static/state.js?v=bbbb', '/static/css/views.css?v=cccc',
      '/static/dashboard.js?v=aaaa', '/static/state.js?v=bbbb'],
    entries: ['/static/dashboard.js'],
    violations: [],
  };
}

function loaded(urls = ['/static/css/views.css?v=cccc', '/static/dashboard.js?v=aaaa', '/static/state.js?v=bbbb']) {
  return {
    requests: urls.map((u) => BASE + u),
    failed: [],
    responses: urls.map((u) => ({ url: BASE + u, status: 200, cacheControl: IMMUTABLE_CACHE })),
  };
}

test('a load of the rendered page breaks no rule', () => {
  assert.deepEqual(staticLoadProblems(rendered(), loaded()), []);
});

test('a page without its import map is not the rendered page', () => {
  const named = { ...rendered(), maps: 0, imports: {} };
  const got = staticLoadProblems(named, loaded());
  assert.ok(got.includes('page has 0 import maps, want 1'), got.join('\n'));
  assert.ok(got.includes('module /static/state.js is not in the import map'), got.join('\n'));
});

test('a CSP violation fails', () => {
  const named = { ...rendered(), violations: ['script-src-elem inline'] };
  assert.deepEqual(staticLoadProblems(named, loaded()), ['CSP violation: script-src-elem inline']);
});

test('an entry tag left unrewritten is an unversioned URL with no loader', () => {
  const named = { ...rendered(), entries: [], urls: [...rendered().urls, '/static/dashboard.js'] };
  assert.deepEqual(staticLoadProblems(named, loaded()), [
    'page has no inline module loader',
    'page names /static/dashboard.js as v=aaaa and unversioned',
  ]);
});

test('a module the page names under two versions fails', () => {
  const named = { ...rendered(), urls: [...rendered().urls, '/static/state.js?v=zzzz'] };
  assert.deepEqual(staticLoadProblems(named, loaded()), ['page names /static/state.js as v=bbbb and v=zzzz']);
});

test('a stylesheet the page names without a version fails', () => {
  const named = { ...rendered(), urls: [...rendered().urls, '/static/css/new.css'] };
  const rec = loaded(['/static/css/views.css?v=cccc', '/static/css/new.css', '/static/dashboard.js?v=aaaa', '/static/state.js?v=bbbb']);
  rec.responses[1].cacheControl = 'no-cache, must-revalidate';
  assert.deepEqual(staticLoadProblems(named, rec), [
    'page names /static/css/new.css as unversioned',
    '/static/css/new.css sent Cache-Control "no-cache, must-revalidate", want "' + IMMUTABLE_CACHE + '"',
  ]);
});

test('a request for a URL the page does not name fails', () => {
  const rec = loaded(['/static/css/views.css?v=cccc', '/static/dashboard.js?v=aaaa', '/static/state.js']);
  rec.responses[2].cacheControl = 'no-cache, must-revalidate';
  assert.deepEqual(staticLoadProblems(rendered(), rec), [
    'requested /static/state.js, not the URL the page names',
    '/static/state.js sent Cache-Control "no-cache, must-revalidate", want "' + IMMUTABLE_CACHE + '"',
  ]);
});

test('a module fetched under a second URL is requested twice', () => {
  const rec = loaded(['/static/css/views.css?v=cccc', '/static/dashboard.js?v=aaaa', '/static/state.js?v=bbbb', '/static/state.js']);
  rec.responses.pop();
  assert.deepEqual(staticLoadProblems(rendered(), rec), [
    'requested /static/state.js, not the URL the page names',
    '/static/state.js requested 2 times',
  ]);
});

test('an entry module never loaded fails', () => {
  const got = staticLoadProblems(rendered(), loaded(['/static/css/views.css?v=cccc', '/static/state.js?v=bbbb']));
  assert.deepEqual(got, ['entry module /static/dashboard.js never loaded']);
});

test('no /static request at all fails', () => {
  assert.ok(staticLoadProblems(rendered(), loaded([])).includes('no /static request seen'));
});

test('a versioned response without the immutable policy fails', () => {
  const rec = loaded();
  rec.responses[0].cacheControl = 'no-cache, must-revalidate';
  assert.deepEqual(staticLoadProblems(rendered(), rec), [
    '/static/css/views.css?v=cccc sent Cache-Control "no-cache, must-revalidate", want "' + IMMUTABLE_CACHE + '"',
  ]);
});

test('a non-200 or failed /static request fails', () => {
  const rec = loaded();
  rec.responses[1].status = 404;
  rec.responses[2].status = 304;
  rec.failed.push(BASE + '/static/state.js?v=bbbb');
  assert.deepEqual(staticLoadProblems(rendered(), rec), [
    'request failed: /static/state.js',
    '404 for /static/dashboard.js?v=aaaa',
    '304 for /static/state.js?v=bbbb',
  ]);
});

test('recordStatic keeps /static traffic only, until stop', () => {
  const page = new EventEmitter();
  const req = (u) => ({ url: () => BASE + u });
  const resp = (u, cc) => ({ url: () => BASE + u, status: () => 200, headers: () => (cc ? { 'cache-control': cc } : {}) });
  const rec = recordStatic(page);
  page.emit('request', req('/static/state.js?v=bbbb'));
  page.emit('request', req('/api/sessions'));
  page.emit('requestfailed', req('/static/x.js'));
  page.emit('response', resp('/static/state.js?v=bbbb', IMMUTABLE_CACHE));
  page.emit('response', resp('/static/y.js'));
  rec.stop();
  page.emit('request', req('/static/late.js'));
  assert.deepEqual(rec.requests, [BASE + '/static/state.js?v=bbbb']);
  assert.deepEqual(rec.failed, [BASE + '/static/x.js']);
  assert.deepEqual(rec.responses, [
    { url: BASE + '/static/state.js?v=bbbb', status: 200, cacheControl: IMMUTABLE_CACHE },
    { url: BASE + '/static/y.js', status: 200, cacheControl: '' },
  ]);
  assert.equal(page.listenerCount('request') + page.listenerCount('requestfailed') + page.listenerCount('response'), 0);
});
