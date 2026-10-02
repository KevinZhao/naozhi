// node --test scripts/check-enum-literals.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { checkAll, contractKindProblems, deathReasonKeys, kindProblems, literalHits, run } from './check-enum-literals.mjs';

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

// --- EventEntry kinds (S13b-4) ---

const contract = {
  WS: { event: 'event', history: 'history' },
  ENUMS: { EVENT_TYPE: ['user', 'text', 'tool_use', 'result'], EVENT_TYPE_INTERNAL: ['tool_use', 'result'], EVENT_TYPE_MD_IGNORE: ['tool_use'] },
};
const other = { 'b.js': ['keydown'] };
const clean = {
  'a.js': "const S = new Set(NZ_CONTRACT.ENUMS.EVENT_TYPE_INTERNAL);\nconst icons = { user: 1, text: 2 };\nfunction f(e) { if (e.type === 'user' || e.type === 'event') return icons[e.type]; switch (e.type) { case 'text': return 3; } }\n",
  'b.js': "const k = ['user', 'other']; addEventListener('x', (ev) => { if (ev.type === 'keydown') k.push(ev.type); });\nif (e.type !== 'result') {}\n",
  'contract.js': "export const NZ_CONTRACT = { ENUMS: { EVENT_TYPE: ['user', 'text'] } };",
};
const check = (files, o = other, sentinels = ['a.js', 'b.js']) => kindProblems(files, contract, o, sentinels);

test('contractKindProblems wants three non-empty lists, the two columns inside EVENT_TYPE', () => {
  assert.deepEqual(contractKindProblems(contract.ENUMS), []);
  const missing = contractKindProblems({ EVENT_TYPE: ['user'], EVENT_TYPE_INTERNAL: [] });
  assert.deepEqual(missing, ['contract.js: ENUMS.EVENT_TYPE_INTERNAL is missing or empty', 'contract.js: ENUMS.EVENT_TYPE_MD_IGNORE is missing or empty']);
  const outside = contractKindProblems({ ...contract.ENUMS, EVENT_TYPE_MD_IGNORE: ['tool_use', 'txt'] });
  assert.deepEqual(outside, ['contract.js: ENUMS.EVENT_TYPE_MD_IGNORE has "txt", which ENUMS.EVENT_TYPE does not list']);
  assert.equal(contractKindProblems(undefined).length, 3);
});

test('kindProblems passes a clean tree and counts what it saw, contract.js aside', () => {
  const { problems, counts } = check(clean);
  assert.deepEqual(problems, []);
  assert.deepEqual(counts, { kindComparisons: 3, otherComparisons: 1, lookups: 1 });
});

const BAD = [
  ['a spread does not hide two restated kinds', "const X = []; const S2 = new Set([...X, 'tool_use', 'result']);", /a\.js:\d+: array restates 2 kinds \(tool_use, result\)/],
  ['a plain restated pair', "const S2 = ['tool_use','result'];", /array restates 2 kinds/],
  ['a template literal counts as a kind literal', 'const S2 = [`user`, `text`];', /array restates 2 kinds \(user, text\)/],
  ['a typo compared with .type', "if (e.type === 'tool_usee') {}", /\.type compared with "tool_usee", which is not/],
  ['a typo on the left', "if ('txt' == e.type) {}", /\.type compared with "txt"/],
  ['a switch case outside the vocabulary', "switch (ev.type) { case 'txt': break; }", /\.type compared with "txt"/],
  ['a template literal compared with .type', 'if (e.type !== `txt`) {}', /\.type compared with "txt"/],
  ['another file\'s OTHER entry', "if (ev.type === 'keydown') {}", /\.type compared with "keydown", which is not .* listed for a\.js/],
  ['a lookup-table key that is not a kind', "const icons2 = { user: 1, txt: 2 }; const i = icons2[e.type];", /a table looked up by \[\.type\] has key "txt"/],
  ['an inline lookup table', "const i = ({ user: 1, 'txt': 2 })[e.type];", /has key "txt"/],
  ['a computed lookup-table key', "const kk = 'user'; const icons2 = { [kk]: 1 }; icons2[e.type];", /a key that is not a static name/],
  ['a spread into a lookup table', "const icons2 = { ...base }; icons2[e.type];", /a key that is not a static name/],
  ['a file that does not parse', 'const = ;', /a\.js: does not parse/],
];
for (const [name, extra, want] of BAD) {
  test(`kindProblems rejects ${name}`, () => {
    const { problems } = check({ ...clean, 'a.js': clean['a.js'] + extra });
    assert.ok(problems.some((p) => want.test(p)), problems.join('\n') || '(no problems)');
  });
}

test('kindProblems fails a dead OTHER_TYPES entry, including one for a file it never saw', () => {
  const dead = check(clean, { 'b.js': ['keydown', 'click'], 'gone.js': ['quick'] }).problems;
  assert.deepEqual(dead, [
    "OTHER_TYPES['b.js'] lists \"click\", which no .type comparison in b.js uses — drop the dead entry",
    "OTHER_TYPES['gone.js'] lists \"quick\", which no .type comparison in gone.js uses — drop the dead entry",
  ]);
});

test('kindProblems fails a sentinel file that is gone or compares no kind', () => {
  const { 'b.js': _, ...withoutB } = clean;
  assert.ok(check(withoutB, {}).problems.includes('b.js: no .type comparison with a kind — the kind scan has gone blind'));
  const noKind = { ...clean, 'b.js': "if (ev.type === 'keydown') {}" };
  assert.deepEqual(check(noKind).problems, ['b.js: no .type comparison with a kind — the kind scan has gone blind']);
});

test('kindProblems leaves one kind literal, a non-.type comparison and a non-.type index alone', () => {
  const ok = { ...clean, 'a.js': clean['a.js'] + "const one = ['user', 'x']; if (e.kind === 'txt') {} const t = { txt: 1 }; t[e.kind]; t[type];" };
  assert.deepEqual(check(ok).problems, []);
});

test('checkAll reports every check, death_reason and kinds alike, over one tree', () => {
  const full = { ...contract, ENUMS: { ...contract.ENUMS, DEATH_REASON: ['idle_timeout', 'evicted', 'cli_exited'] } };
  const tree = { ...clean, 'nz_util.js': nzUtil };
  assert.deepEqual(checkAll(tree, full, other, ['a.js', 'b.js']).problems, []);
  const bad = { ...tree, 'a.js': clean['a.js'] + "if (e.type === 'txt' || r === 'evicted') {}" };
  const { problems } = checkAll(bad, { ...full, ENUMS: { ...full.ENUMS, EVENT_TYPE_MD_IGNORE: [] } }, other, ['a.js', 'b.js']);
  for (const want of [/a\.js: hardcodes death_reason literal\(s\) evicted/, /EVENT_TYPE_MD_IGNORE is missing or empty/, /a\.js:\d+: \.type compared with "txt"/]) {
    assert.ok(problems.some((p) => want.test(p)), `${want} not in:\n${problems.join('\n')}`);
  }
});
