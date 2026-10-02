// Tests for the dashboard lint rules in eslint-plugin-nz.mjs.
//
//   node --test scripts/eslint-plugin-nz.test.mjs
//
// eslint comes from the e2e install, the same place the lint gate runs it
// from.

import { describe, it } from 'node:test';
import { createRequire } from 'node:module';
import nz from './eslint-plugin-nz.mjs';

const require_ = createRequire(import.meta.url);
const { RuleTester } = require_('../test/e2e/node_modules/eslint');

RuleTester.describe = describe;
RuleTester.it = it;
RuleTester.itOnly = it.only;

const tester = new RuleTester({ languageOptions: { ecmaVersion: 2022, sourceType: 'module' } });

tester.run('configure-deps', nz.rules['configure-deps'], {
  valid: [
    // Functions, consts, classes and imports are shared, never copied stale.
    "import { a } from './x.js'; function f() {} const o = {}; class C {} configureX({ a, f, o, C });",
    "configureX({ g: () => 1, h: function () {} });",
    // Non-configure calls are out of scope.
    "let s = 1; register({ s });",
    "let s = 1; reconfigureX({ s });",
    // A const inside a function body is still a const.
    "function boot() { const local = {}; configureX({ local }); }",
  ],
  invalid: [
    { code: 'let s = 1; configureX({ s });', errors: [{ messageId: 'mutable', data: { name: 's', kind: 'let', target: 'X' } }] },
    { code: 'var v = 1; configureX({ v });', errors: [{ messageId: 'mutable', data: { name: 'v', kind: 'var', target: 'X' } }] },
    { code: 'function boot(p) { configureX({ p }); }', errors: [{ messageId: 'mutable', data: { name: 'p', kind: 'Parameter', target: 'X' } }] },
    { code: 'configureX({ undeclared });', errors: [{ messageId: 'mutable', data: { name: 'undeclared', kind: 'global', target: 'X' } }] },
    { code: 'const o = { m: {} }; configureX({ m: o.m });', errors: [{ messageId: 'notBinding', data: { target: 'X', type: 'MemberExpression' } }] },
    { code: 'configureX({ n: 3 });', errors: [{ messageId: 'notBinding', data: { target: 'X', type: 'Literal' } }] },
    { code: 'const o = {}; configureX({ ...o });', errors: [{ messageId: 'notBinding', data: { target: 'X', type: 'SpreadElement' } }] },
    // Every offending property is reported, not just the first.
    { code: 'let a = 1; let b = 2; function f() {} configureX({ a, f, b });', errors: [{ messageId: 'mutable' }, { messageId: 'mutable' }] },
    { code: 'const o = {}; let b = 2; configureX({ ...o, b });', errors: [{ messageId: 'notBinding' }, { messageId: 'mutable' }] },
  ],
});

tester.run('no-exported-let', nz.rules['no-exported-let'], {
  valid: [
    'export const state = { n: 0 };',
    'export function get() { return 1; }',
    // A private let is the owner's business.
    'let n = 0; export function bump() { n++; }',
    'const c = 1; export { c };',
    // Re-exports carry no local binding.
    "export { x } from './x.js';",
  ],
  invalid: [
    { code: 'export let n = 0;', errors: [{ messageId: 'exportedLet', data: { name: 'n', kind: 'let' } }] },
    { code: 'export var n = 0;', errors: [{ messageId: 'exportedLet', data: { name: 'n', kind: 'var' } }] },
    { code: 'let n = 0; export { n };', errors: [{ messageId: 'exportedLet', data: { name: 'n', kind: 'let' } }] },
    { code: 'let n = 0; export { n as count };', errors: [{ messageId: 'exportedLet', data: { name: 'n', kind: 'let' } }] },
    { code: 'let { a, b: [c] } = {}; export { a, c };', errors: [{ messageId: 'exportedLet', data: { name: 'a', kind: 'let' } }, { messageId: 'exportedLet', data: { name: 'c', kind: 'let' } }] },
  ],
});

tester.run('no-module-side-effects', nz.rules['no-module-side-effects'], {
  valid: [
    "import { a } from './x.js';",
    "export { a } from './x.js';",
    'export const o = {};',
    'function f() {}',
    'class C {}',
    // Pure-expression initialisers, recursively.
    "const RE = new RegExp('x' + 'y');",
    'const s = new Set([1, 2]);',
    'const frozen = Object.freeze({ a: new Set([1]), b: [1, 2] });',
    'const created = Object.create(null);',
    'const assigned = Object.assign({}, { a: 1 });',
    'const n = 1 + 2;',
    'const t = `a${1}b`;',
    // D2: the WS dispatch table's registration calls are the one exception.
    "wsm.on(NZ_CONTRACT.WS.history, (msg) => f(msg));",
    'wsm.onReady(() => f());',
    'wsm.onStateChange((s) => f(s));',
    'wsm.onAuthFail((msg) => f(msg));',
  ],
  invalid: [
    // A bare top-level call.
    { code: 'f();', errors: [{ messageId: 'sideEffect' }] },
    // Side effects laundered through a `const` initialiser.
    { code: "const t = setInterval(() => {}, 1000);", errors: [{ messageId: 'sideEffect' }] },
    { code: "const x = document.addEventListener('click', f);", errors: [{ messageId: 'sideEffect' }] },
    // Top-level control flow.
    { code: 'if (x) { f(); }', errors: [{ messageId: 'sideEffect' }] },
    { code: 'try { f(); } catch (e) {}', errors: [{ messageId: 'sideEffect' }] },
    // A call laundered through an export.
    { code: 'export const y = init();', errors: [{ messageId: 'sideEffect' }] },
    // A call on a managed object other than wsm's own methods is not the D2
    // exception.
    { code: 'sessionStream.reset();', errors: [{ messageId: 'sideEffect' }] },
    { code: 'wsm.connect();', errors: [{ messageId: 'sideEffect' }] },
  ],
});
