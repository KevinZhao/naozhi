// Tests for the scanners js-deps-freeze.mjs builds its dependency matrix from.
// The matrix is a growth gate that may only shrink, so under-counting is the
// failure that matters: a tokenizer that mistakes code for string text drops
// real references, and the gate then misses new coupling. Each case below is
// read through the bare-identifier scan, which is what the matrix consumes.
//
//   node --test scripts/js-deps-freeze.test.mjs

import assert from 'node:assert/strict';
import test from 'node:test';

import { bareIdents, stripComments, stripCommentsAndStrings } from './js-deps-freeze.mjs';

const refs = (src) => bareIdents(stripCommentsAndStrings(src));

test('a quote inside a regex literal does not swallow the code after it', () => {
  const src = "const r = /[&<>\"']/g;\nfetchJSON(url);\n";
  assert.ok(refs(src).has('fetchJSON'));
  // The regex copies through, so its own text is untouched.
  assert.ok(stripCommentsAndStrings(src).includes("/[&<>\"']/g"));
});

test('a regex after return / typeof is still a regex', () => {
  const src = 'function f(s) { return /"/.test(s) ? keep(s) : other(s); }\n';
  const got = refs(src);
  assert.ok(got.has('keep'));
  assert.ok(got.has('other'));
});

test('a slash after an operand is division, not a regex', () => {
  // After an identifier and after a closing paren: read as a regex, either
  // slash would run to the '/' in 'n/a' and leave its quote to open a string
  // that blanks the rest of the line.
  for (const src of [
    "const q = total / count; const label = 'n/a'; after(q);\n",
    "const q = (a + b) / count; const label = 'n/a'; after(q);\n",
  ]) {
    const got = refs(src);
    assert.ok(got.has('count'), src);
    assert.ok(got.has('after'), src);
  }
});

test('names inside string text are not references', () => {
  const src = "el.className = ' nz-hidden'; if (phase === 'sending') go();\nconst key = 'nz:cron_sort';\n";
  const got = refs(src);
  assert.ok(!got.has('nz'), 'a CSS class prefix is not a reference to nz');
  assert.ok(!got.has('sending'), 'a phase value is not a reference to a variable');
  assert.ok(got.has('go'));
});

test('template text is blanked but ${...} expressions stay code, nested ones too', () => {
  const src = 'const h = `<b class="nz-x">${esc(name)}</b> ${ok ? `in ${inner(v)}` : fallback}`;\n';
  const got = refs(src);
  for (const n of ['esc', 'name', 'ok', 'inner', 'v', 'fallback']) assert.ok(got.has(n), n);
  assert.ok(!got.has('nz'));
  assert.ok(!got.has('class'));
});

test('a brace inside a string inside ${...} does not end the expression early', () => {
  // Ended at the quoted '}', the rest of the expression (g(x)) would be read
  // as template text and blanked.
  const src = "const t = `${f('}') + g(x)} tail`; next();\n";
  const got = refs(src);
  for (const n of ['f', 'g', 'x', 'next']) assert.ok(got.has(n), n);
  assert.ok(!got.has('tail'));
});

test('comments are not references, and escapes do not end a string', () => {
  const src = "// ghost()\n/* phantom() */\nconst s = 'it\\'s fine'; real();\n";
  const got = refs(src);
  assert.ok(!got.has('ghost'));
  assert.ok(!got.has('phantom'));
  assert.ok(!got.has('fine'));
  assert.ok(got.has('real'));
});

test('line numbers survive stripping', () => {
  const src = "a();\nconst s = 'x\ny';\n// c\nb();\n";
  assert.equal(stripCommentsAndStrings(src).split('\n').length, src.split('\n').length);
});

test('the comment-only stripping still keeps strings for the scans that need them', () => {
  // typeof-guard counting matches the 'function' literal; import scanning
  // reads the specifier. Both read stripComments, which must keep them.
  const src = "if (typeof x === 'function') x();\nimport { a } from './m.js';\n";
  const kept = stripComments(src);
  assert.ok(kept.includes("'function'"));
  assert.ok(kept.includes("'./m.js'"));
});
