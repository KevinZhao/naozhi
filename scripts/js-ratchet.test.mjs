// node --test scripts/js-ratchet.test.mjs — what js-ratchet counts as a
// function and how it compares against the baseline.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { compare, measureSource, raisedMetrics, loadCaps, capProblems, sccs, importsOfSource } from './js-ratchet.mjs';

// body returns n lines of statements, so a function around it spans n + 2.
const body = (n) => Array.from({ length: n }, (_, i) => `  x(${i});`).join('\n');

test('every function form is measured', () => {
  const forms = {
    declaration: `function f() {\n${body(10)}\n}\n`,
    expression: `const f = function () {\n${body(10)}\n};\n`,
    arrow: `const f = () => {\n${body(10)}\n};\n`,
    'object method': `const o = {\n  m() {\n${body(10)}\n  },\n};\n`,
    'object property function': `const o = {\n  m: function () {\n${body(10)}\n  },\n};\n`,
    'class method': `class C {\n  m() {\n${body(10)}\n  }\n}\n`,
  };
  for (const [name, src] of Object.entries(forms)) {
    assert.equal(measureSource(src).maxFnLines, 12, name);
  }
});

test('a function over 100 lines is counted, nested ones on their own', () => {
  const src = `function outer() {\n  function inner() {\n${body(120)}\n  }\n}\n`;
  const m = measureSource(src);
  assert.equal(m.maxFnLines, 124);
  assert.equal(m.fnOver100, 2);
});

test('a top-level IIFE is a scope, not a function', () => {
  for (const src of [
    `(function () {\n${body(150)}\n  function g() {\n${body(5)}\n  }\n})();\n`,
    `!function () {\n${body(150)}\n}();\n`,
    `(() => {\n${body(150)}\n})();\n`,
  ]) {
    const m = measureSource(src);
    assert.equal(m.fnOver100, 0, src.slice(0, 20));
    assert.ok(m.maxFnLines < 100, src.slice(0, 20));
  }
  // The inner function still counts.
  assert.equal(measureSource(`(function () {\n  function g() {\n${body(5)}\n  }\n})();\n`).maxFnLines, 7);
});

test('lines and top-level let/var', () => {
  const m = measureSource('let a = 1;\nvar b = 2;\nconst c = 3;\n  let d = 4;\n');
  assert.equal(m.lines, 4);
  assert.equal(m.topLevelLetVar, 2);
});

test('compare flags growth, improvement, and metrics the baseline does not hold', () => {
  const base = { 'a.js': { lines: 10, maxFnLines: 5 } };
  assert.deepEqual(compare({ 'a.js': { lines: 10, maxFnLines: 5 } }, base), []);
  assert.match(compare({ 'a.js': { lines: 11, maxFnLines: 5 } }, base)[0], /grew 10 -> 11/);
  assert.match(compare({ 'a.js': { lines: 9, maxFnLines: 5 } }, base)[0], /improved/);
  assert.match(compare({ 'a.js': { lines: 10, maxFnLines: 5, fnOver100: 0 } }, base)[0], /not in the baseline/);
  assert.match(compare({ 'a.js': { lines: 10 } }, base)[0], /no longer measured/);
  assert.match(compare({ 'a.js': { lines: 10, maxFnLines: 5 }, 'b.js': { lines: 1, maxFnLines: 0 } }, base)[0], /b.js: not in baseline/);
  assert.match(compare({}, base)[0], /gone from static/);
});

test('over 100 means 101 lines and more', () => {
  assert.equal(measureSource(`function f() {\n${body(98)}\n}\n`).fnOver100, 0); // 100 lines
  assert.equal(measureSource(`function f() {\n${body(99)}\n}\n`).fnOver100, 1); // 101 lines
});

test('--write refuses to raise, and only to raise', () => {
  const base = { 'a.js': { lines: 10, maxFnLines: 5 } };
  assert.deepEqual(raisedMetrics({ 'a.js': { lines: 11, maxFnLines: 4 } }, base), ['a.js lines 10 -> 11']);
  assert.deepEqual(raisedMetrics({ 'a.js': { lines: 9, maxFnLines: 5 }, 'n.js': { lines: 50 } }, base), []);
});

// --- caps: fail-closed schema checks (S19-0, #3025) -----------------------

function withCapsFile(content, fn) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'js-ratchet-caps-'));
  const file = path.join(dir, 'caps.json');
  if (content !== null) fs.writeFileSync(file, content);
  try {
    return fn(file);
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
}

test('loadCaps fails closed when the file is missing', () => {
  withCapsFile(null, (file) => {
    const { errors, caps } = loadCaps(file);
    assert.equal(caps, undefined);
    assert.match(errors[0], /missing/);
  });
});

test('loadCaps fails closed on a top-level key rename', () => {
  withCapsFile(
    JSON.stringify({ maxFn: { default: 120, exempt: [] }, lines: {}, sideEffectLegacy: [], cycleLegacy: [] }),
    (file) => {
      const { errors } = loadCaps(file);
      assert.ok(errors.some((e) => /maxFnLines/.test(e)), errors);
    },
  );
});

test('loadCaps fails closed when default is not a number', () => {
  withCapsFile(
    JSON.stringify({ maxFnLines: { default: '120', exempt: [] }, lines: {}, sideEffectLegacy: [], cycleLegacy: [] }),
    (file) => {
      const { errors } = loadCaps(file);
      assert.ok(errors.some((e) => /default must be a number/.test(e)), errors);
    },
  );
});

test('loadCaps accepts a well-formed caps file', () => {
  withCapsFile(
    JSON.stringify({ maxFnLines: { default: 120, exempt: ['a.js'] }, lines: { 'dashboard.js': 100 }, sideEffectLegacy: [], cycleLegacy: [] }),
    (file) => {
      const { errors, caps } = loadCaps(file);
      assert.equal(errors, undefined);
      assert.equal(caps.maxFnLines.default, 120);
    },
  );
});

// --- caps: cross-checked against a measurement and the import graph -------

test('capProblems: a new file over the default cap fails', () => {
  const caps = { maxFnLines: { default: 120, exempt: [] }, lines: {}, sideEffectLegacy: [], cycleLegacy: [] };
  const current = { 'n.js': { lines: 10, maxFnLines: 121 } };
  const problems = capProblems(current, {}, caps);
  assert.ok(problems.some((p) => /n\.js: maxFnLines 121 exceeds the cap/.test(p)), problems);
});

test('capProblems: an exempt file already at or under the default must move out', () => {
  const caps = { maxFnLines: { default: 120, exempt: ['a.js'] }, lines: {}, sideEffectLegacy: [], cycleLegacy: [] };
  const current = { 'a.js': { lines: 10, maxFnLines: 90 } };
  const problems = capProblems(current, {}, caps);
  assert.ok(problems.some((p) => /a\.js is already <= 120/.test(p)), problems);
});

test('capProblems: an exempt file still over the default is fine', () => {
  const caps = { maxFnLines: { default: 120, exempt: ['a.js'] }, lines: {}, sideEffectLegacy: [], cycleLegacy: [] };
  const current = { 'a.js': { lines: 10, maxFnLines: 200 } };
  assert.deepEqual(capProblems(current, {}, caps), []);
});

test('capProblems: a cap pointing at a file not in static/ fails', () => {
  const caps = { maxFnLines: { default: 120, exempt: ['missing.js'] }, lines: {}, sideEffectLegacy: [], cycleLegacy: [] };
  const current = { 'a.js': { lines: 10, maxFnLines: 10 } };
  const problems = capProblems(current, {}, caps);
  assert.ok(problems.some((p) => /missing\.js does not exist/.test(p)), problems);
});

test('capProblems: an over-cap dashboard.js line count fails', () => {
  const caps = { maxFnLines: { default: 120, exempt: [] }, lines: { 'dashboard.js': 100 }, sideEffectLegacy: [], cycleLegacy: [] };
  const current = { 'dashboard.js': { lines: 101, maxFnLines: 10 } };
  const problems = capProblems(current, {}, caps);
  assert.ok(problems.some((p) => /dashboard\.js is 101 lines, over the cap of 100/.test(p)), problems);
});

test('sccs finds a two-file import cycle', () => {
  const graph = { 'a.js': ['b.js'], 'b.js': ['a.js'], 'c.js': [] };
  assert.deepEqual(
    sccs(graph).map((c) => [...c].sort()),
    [['a.js', 'b.js']],
  );
});

test('importsOf: an import, a named re-export and export * are all edges', () => {
  const src = [
    "import { a } from './a.js';",
    "import './side.js';",
    "export { b } from './b.js';",
    "export * from './c.js';",
    "export * as ns from '../d.js';",
    "const local = 1;",
    "export { local };", // no source: not an edge
    "export const other = 2;",
    "import bare from 'bare-pkg';", // outside static/: not an edge
    "const lazy = () => import('./lazy.js');", // runs later: not an edge
  ].join('\n');
  assert.deepEqual(importsOfSource(src), ['a.js', 'side.js', 'b.js', 'c.js', 'd.js']);
});

test('sccs finds a cycle that closes through a re-export', () => {
  // utilities.js imports nz_util.js; nz_util.js re-exports from utilities.js.
  const graph = {
    'utilities.js': importsOfSource("import { esc } from './nz_util.js';\nexport function configureUtilities() {}\n"),
    'nz_util.js': importsOfSource("export { configureUtilities } from './utilities.js';\nexport const esc = 1;\n"),
    'star.js': importsOfSource("export * from './nz_util.js';\n"),
  };
  assert.deepEqual(sccs(graph).map((c) => [...c].sort()), [['nz_util.js', 'utilities.js']]);
});

test('capProblems: an import cycle not listed in cycleLegacy fails', () => {
  const caps = { maxFnLines: { default: 120, exempt: [] }, lines: {}, sideEffectLegacy: [], cycleLegacy: [] };
  const current = { 'a.js': { lines: 1, maxFnLines: 1 }, 'b.js': { lines: 1, maxFnLines: 1 } };
  const graph = { 'a.js': ['b.js'], 'b.js': ['a.js'] };
  const problems = capProblems(current, graph, caps);
  assert.ok(problems.some((p) => /a\.js is part of an import cycle/.test(p)), problems);
  assert.ok(problems.some((p) => /b\.js is part of an import cycle/.test(p)), problems);
});

test('capProblems: cycleLegacy exactly matching the real cycle is clean', () => {
  const caps = { maxFnLines: { default: 120, exempt: [] }, lines: {}, sideEffectLegacy: [], cycleLegacy: ['a.js', 'b.js'] };
  const current = { 'a.js': { lines: 1, maxFnLines: 1 }, 'b.js': { lines: 1, maxFnLines: 1 } };
  const graph = { 'a.js': ['b.js'], 'b.js': ['a.js'] };
  assert.deepEqual(capProblems(current, graph, caps), []);
});

test('capProblems: a cycleLegacy entry no longer in a cycle must move out', () => {
  const caps = { maxFnLines: { default: 120, exempt: [] }, lines: {}, sideEffectLegacy: [], cycleLegacy: ['a.js', 'b.js'] };
  const current = { 'a.js': { lines: 1, maxFnLines: 1 }, 'b.js': { lines: 1, maxFnLines: 1 } };
  const graph = { 'a.js': [], 'b.js': [] }; // cycle fixed
  const problems = capProblems(current, graph, caps);
  assert.ok(problems.some((p) => /a\.js is no longer part of an import cycle/.test(p)), problems);
  assert.ok(problems.some((p) => /b\.js is no longer part of an import cycle/.test(p)), problems);
});

test('capProblems: a sideEffectLegacy entry pointing at a missing file fails', () => {
  const caps = { maxFnLines: { default: 120, exempt: [] }, lines: {}, sideEffectLegacy: ['missing.js'], cycleLegacy: [] };
  const current = { 'a.js': { lines: 1, maxFnLines: 1 } };
  const problems = capProblems(current, {}, caps);
  assert.ok(problems.some((p) => /sideEffectLegacy: missing\.js does not exist/.test(p)), problems);
});

test('configureDeps counts what configure calls inject', () => {
  const src = [
    'configureA({ a, b, c });',
    'configureB({ ...shared, d: () => d() });',
    'configure({ x, y });', // not configure[A-Z]
    'setup({ p, q });',
    'function f() { configureC({ e }); }',
  ].join('\n');
  assert.equal(measureSource(src).configureDeps, 6);
});
