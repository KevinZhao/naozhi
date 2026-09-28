// node --test scripts/check-enum-literals.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { deathReasonKeys, literalHits, run } from './check-enum-literals.mjs';

const nzUtil = `
const OTHER = { a: 1 };
const DEATH_REASONS = {
  idle_timeout: { crashed: false, text: 'x' },
  evicted: { crashed: false, text: 'y' },
  cli_exited: { crashed: true, text: 'z' },
};
function sessionExit() {}
`;

test('deathReasonKeys reads the object literal, not string contents', () => {
  assert.deepEqual(deathReasonKeys(nzUtil), ['idle_timeout', 'evicted', 'cli_exited']);
  assert.equal(deathReasonKeys('const X = 1;'), null);
});

test('literalHits finds a whole-string match and ignores a substring', () => {
  const files = {
    'a.js': "if (reason === 'idle_timeout') foo();",
    'b.js': "// evicted from the cache long ago\nconst x = 'evictedFromCache';",
    'nz_util.js': "idle_timeout: {},",
    'contract.js': "idle_timeout: 'idle_timeout',",
  };
  const hits = literalHits(files, ['idle_timeout', 'evicted']);
  assert.deepEqual(hits, [{ file: 'a.js', reasons: ['idle_timeout'] }]);
});

test('literalHits skips a match inside a comment', () => {
  const hits = literalHits({ 'a.js': "// once 'idle_timeout' meant something\nconst y = 1;" }, ['idle_timeout']);
  assert.deepEqual(hits, []);
});

test('run flags a missing key, an extra key, and a stray literal, and passes a clean set', () => {
  const contractReasons = ['idle_timeout', 'evicted', 'cli_exited'];
  assert.deepEqual(run({ 'nz_util.js': nzUtil, 'a.js': 'const x = 1;' }, contractReasons), []);

  const missing = run({ 'nz_util.js': nzUtil.replace("cli_exited: { crashed: true, text: 'z' },\n", ''), 'a.js': '' }, contractReasons);
  assert.equal(missing.length, 1);
  assert.match(missing[0], /missing "cli_exited"/);

  const extra = run({ 'nz_util.js': nzUtil.replace('cli_exited:', 'killed:'), 'a.js': '' }, contractReasons);
  assert.ok(extra.some((p) => /has "killed"/.test(p)));

  const stray = run({ 'nz_util.js': nzUtil, 'a.js': "reason === 'evicted'" }, contractReasons);
  assert.ok(stray.some((p) => /hardcodes death_reason literal\(s\) evicted/.test(p)));
});

test('run reports a blind check when nz_util.js has no DEATH_REASONS block', () => {
  const problems = run({ 'nz_util.js': 'const X = 1;' }, ['idle_timeout']);
  assert.ok(problems.some((p) => /gone blind/.test(p)));
});
