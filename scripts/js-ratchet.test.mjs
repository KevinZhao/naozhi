// node --test scripts/js-ratchet.test.mjs — what js-ratchet counts as a
// function and how it compares against the baseline.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { compare, measureSource, raisedMetrics } from './js-ratchet.mjs';

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
