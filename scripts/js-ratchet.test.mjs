// node --test scripts/js-ratchet.test.mjs — what js-ratchet counts as a
// function and how it compares against the baseline.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {
  compare, measureSource, raisedMetrics, loadCaps, capProblems, sccs, importsOfSource,
  analyzeSources, measureGlobal, INJECTION_ALLOW, GLOBAL,
} from './js-ratchet.mjs';

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
    JSON.stringify({ maxFn: { default: 120, exempt: [] }, lines: {}, sideEffectLegacy: [], cycleLegacy: [], leaves: [] }),
    (file) => {
      const { errors } = loadCaps(file);
      assert.ok(errors.some((e) => /maxFnLines/.test(e)), errors);
    },
  );
});

test('loadCaps fails closed when default is not a number', () => {
  withCapsFile(
    JSON.stringify({ maxFnLines: { default: '120', exempt: [] }, lines: {}, sideEffectLegacy: [], cycleLegacy: [], leaves: [] }),
    (file) => {
      const { errors } = loadCaps(file);
      assert.ok(errors.some((e) => /default must be a positive safe integer/.test(e)), errors);
    },
  );
});

test('loadCaps fails closed on a default or a line cap that is not a safe integer', () => {
  // 1e19 is a JSON number, but no int64 holds it: tools/ratchet-raises must
  // not be the only side that notices (#3057 review).
  for (const d of [1e19, 120.5, 0, -1]) {
    withCapsFile(
      JSON.stringify({ maxFnLines: { default: d, exempt: [] }, lines: {}, sideEffectLegacy: [], cycleLegacy: [], leaves: [] }),
      (file) => {
        const { errors } = loadCaps(file);
        assert.ok(errors?.some((e) => /default must be a positive safe integer/.test(e)), `default ${d}: ${errors}`);
      },
    );
  }
  for (const v of [1e19, 6011.5, -1, '6011', null]) {
    withCapsFile(
      JSON.stringify({ maxFnLines: { default: 120, exempt: [] }, lines: { 'dashboard.js': v }, sideEffectLegacy: [], cycleLegacy: [], leaves: [] }),
      (file) => {
        const { errors } = loadCaps(file);
        assert.ok(errors?.some((e) => /caps\.lines\.dashboard\.js must be a non-negative safe integer/.test(e)), `lines ${v}: ${errors}`);
      },
    );
  }
});

test('loadCaps accepts a well-formed caps file', () => {
  withCapsFile(
    JSON.stringify({ maxFnLines: { default: 120, exempt: ['a.js'] }, lines: { 'dashboard.js': 100 }, sideEffectLegacy: [], cycleLegacy: [], leaves: [] }),
    (file) => {
      const { errors, caps } = loadCaps(file);
      assert.equal(errors, undefined);
      assert.equal(caps.maxFnLines.default, 120);
    },
  );
});

// --- caps: cross-checked against a measurement and the import graph -------

test('capProblems: a new file over the default cap fails', () => {
  const caps = { maxFnLines: { default: 120, exempt: [] }, lines: {}, sideEffectLegacy: [], cycleLegacy: [], leaves: [] };
  const current = { 'n.js': { lines: 10, maxFnLines: 121 } };
  const problems = capProblems(current, {}, caps);
  assert.ok(problems.some((p) => /n\.js: maxFnLines 121 exceeds the cap/.test(p)), problems);
});

test('capProblems: an exempt file already at or under the default must move out', () => {
  const caps = { maxFnLines: { default: 120, exempt: ['a.js'] }, lines: {}, sideEffectLegacy: [], cycleLegacy: [], leaves: [] };
  const current = { 'a.js': { lines: 10, maxFnLines: 90 } };
  const problems = capProblems(current, {}, caps);
  assert.ok(problems.some((p) => /a\.js is already <= 120/.test(p)), problems);
});

test('capProblems: an exempt file still over the default is fine', () => {
  const caps = { maxFnLines: { default: 120, exempt: ['a.js'] }, lines: {}, sideEffectLegacy: [], cycleLegacy: [], leaves: [] };
  const current = { 'a.js': { lines: 10, maxFnLines: 200 } };
  assert.deepEqual(capProblems(current, {}, caps), []);
});

test('capProblems: a cap pointing at a file not in static/ fails', () => {
  const caps = { maxFnLines: { default: 120, exempt: ['missing.js'] }, lines: {}, sideEffectLegacy: [], cycleLegacy: [], leaves: [] };
  const current = { 'a.js': { lines: 10, maxFnLines: 10 } };
  const problems = capProblems(current, {}, caps);
  assert.ok(problems.some((p) => /missing\.js does not exist/.test(p)), problems);
});

test('capProblems: an over-cap dashboard.js line count fails', () => {
  const caps = { maxFnLines: { default: 120, exempt: [] }, lines: { 'dashboard.js': 100 }, sideEffectLegacy: [], cycleLegacy: [], leaves: [] };
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
    "import { e } from '/static/e.js';", // the absolute form e2e-shim uses: the same node
    "const local = 1;",
    "export { local };", // no source: not an edge
    "export const other = 2;",
    "import bare from 'bare-pkg';", // outside static/: not an edge
    "const lazy = () => import('./lazy.js');", // runs later: not an edge
  ].join('\n');
  assert.deepEqual(importsOfSource(src), ['a.js', 'side.js', 'b.js', 'c.js', 'd.js', 'e.js']);
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
  const caps = { maxFnLines: { default: 120, exempt: [] }, lines: {}, sideEffectLegacy: [], cycleLegacy: [], leaves: [] };
  const current = { 'a.js': { lines: 1, maxFnLines: 1 }, 'b.js': { lines: 1, maxFnLines: 1 } };
  const graph = { 'a.js': ['b.js'], 'b.js': ['a.js'] };
  const problems = capProblems(current, graph, caps);
  assert.ok(problems.some((p) => /a\.js is part of an import cycle/.test(p)), problems);
  assert.ok(problems.some((p) => /b\.js is part of an import cycle/.test(p)), problems);
});

test('capProblems: cycleLegacy exactly matching the real cycle is clean', () => {
  const caps = { maxFnLines: { default: 120, exempt: [] }, lines: {}, sideEffectLegacy: [], cycleLegacy: ['a.js', 'b.js'], leaves: [] };
  const current = { 'a.js': { lines: 1, maxFnLines: 1 }, 'b.js': { lines: 1, maxFnLines: 1 } };
  const graph = { 'a.js': ['b.js'], 'b.js': ['a.js'] };
  assert.deepEqual(capProblems(current, graph, caps), []);
});

test('capProblems: a cycleLegacy entry no longer in a cycle must move out', () => {
  const caps = { maxFnLines: { default: 120, exempt: [] }, lines: {}, sideEffectLegacy: [], cycleLegacy: ['a.js', 'b.js'], leaves: [] };
  const current = { 'a.js': { lines: 1, maxFnLines: 1 }, 'b.js': { lines: 1, maxFnLines: 1 } };
  const graph = { 'a.js': [], 'b.js': [] }; // cycle fixed
  const problems = capProblems(current, graph, caps);
  assert.ok(problems.some((p) => /a\.js is no longer part of an import cycle/.test(p)), problems);
  assert.ok(problems.some((p) => /b\.js is no longer part of an import cycle/.test(p)), problems);
});

test('loadCaps fails closed without a leaves list', () => {
  for (const leaves of [undefined, 'contract.js', null]) {
    const raw = { maxFnLines: { default: 120, exempt: [] }, lines: {}, sideEffectLegacy: [], cycleLegacy: [] };
    if (leaves !== undefined) raw.leaves = leaves;
    withCapsFile(JSON.stringify(raw), (file) => {
      const { errors } = loadCaps(file);
      assert.ok(errors?.some((e) => /leaves/.test(e)), `leaves ${leaves}: ${errors}`);
    });
  }
});

test('capProblems: a leaves entry pointing at a missing file fails', () => {
  const caps = { maxFnLines: { default: 120, exempt: [] }, lines: {}, sideEffectLegacy: [], cycleLegacy: [], leaves: ['gone.js'] };
  const problems = capProblems({ 'a.js': { lines: 1, maxFnLines: 1 } }, {}, caps);
  assert.ok(problems.some((p) => /caps\.leaves: gone\.js does not exist/.test(p)), problems);
});

test('capProblems: a sideEffectLegacy entry pointing at a missing file fails', () => {
  const caps = { maxFnLines: { default: 120, exempt: [] }, lines: {}, sideEffectLegacy: ['missing.js'], cycleLegacy: [], leaves: [] };
  const current = { 'a.js': { lines: 1, maxFnLines: 1 } };
  const problems = capProblems(current, {}, caps);
  assert.ok(problems.some((p) => /sideEffectLegacy: missing\.js does not exist/.test(p)), problems);
});

// --- S20a (#3026): HTML sinks, injections, late bindings, the module graph --

const js = (...lines) => lines.join('\n') + '\n';
const global = (sources, leaves = []) => {
  const all = analyzeSources(sources);
  const graph = Object.fromEntries(Object.entries(sources).map(([f, src]) => [f, importsOfSource(src)]));
  return measureGlobal(all, graph, leaves);
};

test('innerHTMLAssign counts every assignment operator and the computed form', () => {
  const m = measureSource(js(
    'el.innerHTML = a;',
    'el.innerHTML ||= b;',
    'el.innerHTML ??= c;',
    'el.innerHTML += d;',
    "el['innerHTML'] = e;",
    'const x = el.innerHTML;', // a read, not a sink
    'el.textContent = f;',
  ));
  assert.equal(m.innerHTMLAssign, 5);
  assert.equal(m.htmlInsert, 0);
});

test('htmlInsert counts the other HTML-string sinks, and only those', () => {
  const m = measureSource(js(
    "el.insertAdjacentHTML('beforeend', h);",
    'el.outerHTML = h;',
    'el.setHTMLUnsafe(h);',
    'range.createContextualFragment(h);',
    "new DOMParser().parseFromString(h, 'text/html');",
    'document.write(h);',
    'document.writeln(h);',
    'x.toString();', // inherited Object.prototype names are not sinks
    'x.constructor(h);',
    'log.write(h);', // write on anything but document
  ));
  assert.equal(m.htmlInsert, 7);
  assert.equal(m.innerHTMLAssign, 0);
});

test('htmlSinks: a wrapper does not hide the sink it wraps', () => {
  const base = { 'a.js': js('el.innerHTML = s;') };
  assert.equal(global(base).metrics.htmlSinks, 1);
  // One assignment inside setHTML plus three calls: +4 over a bare sink-free file.
  const wrapped = {
    'a.js': js(
      'export function setHTML(el, s) { el.innerHTML = s; }',
      "setHTML(a, '<b>');",
      "setHTML(b, '<i>');",
    ),
    'b.js': js("import { setHTML } from './a.js';", 'setHTML(c, x);'),
  };
  assert.equal(global(wrapped).metrics.htmlSinks, 4);
  // A wrapper of the wrapper, an import alias and a call through an injected
  // table are all calls to a sink.
  const deeper = {
    'a.js': js(
      'export function setHTML(el, s) { el.innerHTML = s; }',
      'export const paint = (el, s) => setHTML(el, s);',
    ),
    'b.js': js(
      "import { paint as p } from './a.js';",
      'p(c, x);',
      'p(d, y);',
      'deps.setHTML(e, z);',
    ),
  };
  // 1 assignment + setHTML() inside paint + p() twice + deps.setHTML().
  assert.equal(global(deeper).metrics.htmlSinks, 5);
  // Passing only the element a wrapper writes to does not make the caller a
  // wrapper: frame() and its two calls add nothing over setHTML's one call.
  const element = {
    'a.js': js(
      'function setHTML(el, s) { el.innerHTML = s; }',
      "function frame(el) { setHTML(el, '<b>'); }",
      'frame(a);',
      'frame(b);',
    ),
  };
  assert.equal(global(element).metrics.htmlSinks, 2);
  // A function that writes a constant is not a wrapper: its calls add nothing.
  const constant = { 'a.js': js("function clear(el) { el.innerHTML = ''; }", 'clear(a);', 'clear(b);') };
  assert.equal(global(constant).metrics.htmlSinks, 1);
});

test('lateBindings counts hooks.X / nzViews.X writes, an object literal by its keys', () => {
  const src = js(
    "import { hooks as h } from './state.js';",
    "import { nzViews } from './nz_util.js';",
    'h.openLightbox = function () {};',
    'nzViews.agent = { show, hide };',
    'Object.assign(h, { a, b, c });',
    'other.x = 1;',
    'const y = h.openLightbox;',
  );
  assert.equal(measureSource(src).lateBindings, 6);
  // Not imported from state.js / nz_util.js: some other object called hooks.
  assert.equal(measureSource(js('const hooks = {};', 'hooks.x = 1;')).lateBindings, 0);
});

test('configureDeps counts injections where they land, whatever the receiver is called', () => {
  // The configure loop over a deps table: the table's keys.
  const loop = js(
    'const deps = { a: null, b: null, c: null };',
    'export function wireFoo(impl) {',
    '  for (const k of Object.keys(deps)) deps[k] = impl[k];',
    '}',
    'export function use() { deps.a(); deps.b(); return deps.c; }',
  );
  assert.equal(measureSource(loop).configureDeps, 3);
  assert.equal(measureSource(loop).deadInjections, 0);
  // Object.assign from the parameter, a field into a slot the module calls,
  // a destructured field, and a whole parameter stored and then called.
  const forms = js(
    'const tbl = { x: null, y: null };',
    'let slot = null;',
    'let cb = null;',
    'let hook = null;',
    'export function initA(o) { Object.assign(tbl, o); }',
    'export function initB(x) { slot = x.f; }',
    'export function initC({ g }) { hook = g; }',
    'export const initD = (fn) => { cb = fn; };',
    'export function run() { tbl.x(); tbl.y(); slot(); hook(); cb(); }',
  );
  assert.equal(measureSource(forms).configureDeps, 5);
  // State taken from a data argument is not injection: nothing calls it.
  const state = js(
    'const live = { jobId: null };',
    'const turn = { tool: null, n: 0 };',
    'let zone = "";',
    'export function subscribe(jobId) { live.jobId = jobId; }',
    'export function apply(ev) { turn.tool = ev.tool; Object.assign(turn, { n: ev.n }); }',
    'export function setZone(meta) { zone = meta.timezone; }',
    'export function show() { return live.jobId + turn.tool + zone; }',
  );
  assert.equal(measureSource(state).configureDeps, 0);
  // Not exported: not a receiver another module can inject through.
  assert.equal(measureSource(js('const deps = { a: null };', 'function wire(impl) { deps.a = impl.a; }', 'deps.a();')).configureDeps, 0);
});

test('configureDeps no longer depends on how the caller spells the injection', () => {
  const receiver = js(
    'const deps = { a: null, b: null };',
    'export function configureVoice(impl) { for (const k of Object.keys(deps)) deps[k] = impl[k]; }',
    'export function go() { deps.a(); deps.b(); }',
  );
  // Passing a variable instead of a literal used to count 0 at the caller.
  const caller = js("import { configureVoice } from './voice.js';", 'const d = { a, b };', 'configureVoice(d);');
  assert.equal(measureSource(caller).configureDeps, 0);
  assert.equal(measureSource(receiver).configureDeps, 2);
});

test('deadInjections: a deps key the module never reads', () => {
  const src = (extra) => js(
    `const deps = { a: null, b: null${extra} };`,
    'export function configureX(impl) { for (const k of Object.keys(deps)) deps[k] = impl[k]; }',
    'export function go() { deps.a(); const { b } = deps; return b; }',
  );
  assert.equal(measureSource(src('')).deadInjections, 0);
  const m = measureSource(src(', never: null'));
  assert.equal(m.deadInjections, 1);
  assert.equal(m.configureDeps, 3);
});

test('INJECTION_ALLOW: registerActions is the data-action registry, not injection', () => {
  const src = js(
    'const nzActions = Object.create(null);',
    'export function registerActions(map) { for (const k of Object.keys(map)) nzActions[k] = map[k]; }',
    "document.addEventListener('click', (e) => nzActions[e.target.dataset.action](e));",
  );
  assert.equal(measureSource(src, undefined, 'nz_util.js').configureDeps, 0);
  assert.deepEqual([...analyzeSources({ 'nz_util.js': src })['nz_util.js'].allowHits], ['nz_util.js:registerActions']);
  // The same function anywhere else is counted: one registry table, which
  // is read by runtime key and so is never dead.
  assert.equal(measureSource(src, undefined, 'other.js').configureDeps, 1);
  assert.equal(measureSource(src, undefined, 'other.js').deadInjections, 0);
  assert.deepEqual(INJECTION_ALLOW, ['nz_util.js:registerActions']);
});

test('importCycles: an edge through a re-export and a /static/ specifier closes a cycle', () => {
  const g = global({
    'a.js': js("export { x } from './b.js';"),
    'b.js': js("import { y } from '/static/a.js';", 'export const x = 1;'),
  });
  assert.equal(g.metrics.importCycles, 1);
  assert.equal(g.metrics.reexports, 1);
  // a.js re-exports x, but nothing named y.
  assert.equal(g.metrics.unresolvedImports, 1);
});

test('unresolvedImports: a missing export or module, through export * too', () => {
  const g = global({
    'a.js': js('export const one = 1;', 'export default 2;', 'export { one as uno };'),
    'star.js': js("export * from './a.js';"),
    'b.js': js(
      "import two, { one, uno } from './a.js';",
      "import { one as o } from './star.js';",
      "import * as ns from './a.js';",
      "import { gone } from './a.js';",
      "import { x } from './missing.js';",
    ),
  });
  assert.equal(g.metrics.unresolvedImports, 2, g.details.unresolvedImports.join('\n'));
  assert.match(g.details.unresolvedImports[0], /a\.js does not export gone/);
  assert.match(g.details.unresolvedImports[1], /missing\.js is not a static\/ module/);
});

test('leafImports: a leaf importing a non-leaf fails without any cycle', () => {
  const sources = {
    'contract.js': js('export const C = 1;'),
    'leaf.js': js("import { C } from './contract.js';", "import { show } from './view.js';"),
    'view.js': js('export function show() {}'),
  };
  const g = global(sources, ['contract.js', 'leaf.js']);
  assert.equal(g.metrics.importCycles, 0);
  assert.equal(g.metrics.leafImports, 1);
  assert.match(g.details.leafImports[0], /leaf\.js imports view\.js/);
  assert.equal(global(sources, ['contract.js', 'leaf.js', 'view.js']).metrics.leafImports, 0);
});

test('shell: a slot that only forwards, and an upcall that could be an import', () => {
  const shell = js(
    'const slots = {};',
    'export const shell = new Proxy(slots, {});',
    'export function registerShell(t) { Object.assign(slots, t); }',
  );
  const util = js('export function sid(k) { return k; }');
  const good = {
    'shell.js': shell,
    'util.js': util,
    'dashboard.js': js(
      "import { registerShell } from './shell.js';",
      "import { sid } from './util.js';",
      "import './view.js';",
      'let current = null;',
      'function selectSession(k) { current = sid(k); }',
      'registerShell({ selectSession });',
    ),
    'view.js': js("import { shell } from './shell.js';", 'export function click(k) { shell.selectSession(k); }'),
  };
  let g = global(good);
  assert.equal(g.metrics.upcallForwarders, 0, g.details.upcallForwarders.join('\n'));
  assert.equal(g.metrics.upcallNotUp, 0, g.details.upcallNotUp.join('\n'));
  // Each (module, slot) pair a module uses is one injection.
  assert.equal(measureSource(good['view.js']).configureDeps, 1);

  // sidUp only forwards to an import: a disguised import.
  g = global({ ...good, 'dashboard.js': good['dashboard.js'].replace('registerShell({ selectSession });', 'function sidUp(k) { return sid(k); }\nregisterShell({ selectSession, sidUp });') });
  assert.equal(g.metrics.upcallForwarders, 1);
  assert.match(g.details.upcallForwarders[0], /sidUp .* only forwards/);
  // An inline function or an imported name is not the root's own code.
  g = global({ ...good, 'dashboard.js': good['dashboard.js'].replace('registerShell({ selectSession });', 'registerShell({ selectSession, sid, x: () => current });') });
  assert.equal(g.metrics.upcallForwarders, 2);
  // registerShell outside a root module.
  g = global({ ...good, 'view.js': good['view.js'] + js("import { registerShell } from './shell.js';", 'function f() { return 1; }', 'registerShell({ f });') });
  assert.equal(g.metrics.upcallForwarders, 1);

  // other.js is not reached from dashboard.js: it could import selectSession.
  g = global({ ...good, 'other.js': js("import { shell } from './shell.js';", 'shell.selectSession(1);', 'shell.nothing();') });
  assert.equal(g.metrics.upcallNotUp, 2);
  assert.match(g.details.upcallNotUp.join('\n'), /other\.js: shell\.nothing is not registered/);
  assert.match(g.details.upcallNotUp.join('\n'), /dashboard\.js does not reach other\.js/);
});

test('compare and raisedMetrics treat _global as the cross-file entry', () => {
  const base = { 'a.js': { lines: 1 }, [GLOBAL]: { importCycles: 0, htmlSinks: 5 } };
  assert.deepEqual(compare({ 'a.js': { lines: 1 }, [GLOBAL]: { importCycles: 0, htmlSinks: 5 } }, base), []);
  const out = compare({ 'a.js': { lines: 1 }, [GLOBAL]: { importCycles: 1, htmlSinks: 4 } }, base, { importCycles: ['a.js <-> b.js'] });
  assert.match(out[0], /^global: importCycles grew 0 -> 1/);
  assert.equal(out[1], '  a.js <-> b.js');
  assert.match(out[2], /^global: htmlSinks improved 5 -> 4/);
  assert.match(compare({ 'a.js': { lines: 1 } }, base)[0], /global: in baseline but no longer measured/);
  assert.deepEqual(raisedMetrics({ [GLOBAL]: { importCycles: 2, htmlSinks: 1 } }, base), ['global importCycles 0 -> 2']);
});

test('the INJECTION_ALLOW entry still matches a receiver in static/', () => {
  const dir = path.join(path.dirname(new URL(import.meta.url).pathname), '..', 'internal', 'server', 'static');
  const files = fs.readdirSync(dir).filter((f) => f.endsWith('.js') && f !== 'sw.js');
  const all = analyzeSources(Object.fromEntries(files.map((f) => [f, fs.readFileSync(path.join(dir, f), 'utf8')])));
  assert.ok(Object.values(all).some((a) => a.allowHits.has('nz_util.js:registerActions')));
});
