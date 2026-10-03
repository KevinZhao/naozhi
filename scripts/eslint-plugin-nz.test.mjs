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

tester.run('shell-bindings', nz.rules['shell-bindings'], {
  valid: [
    // Functions, consts, classes and imports are shared, never copied stale.
    "import { a } from './x.js'; function f() {} const o = {}; class C {} configureX({ a, f, o, C });",
    "configureX({ g: () => 1, h: function () {} });",
    // Non-configure calls are out of scope.
    "let s = 1; register({ s });",
    "let s = 1; reconfigureX({ s });",
    // A const inside a function body is still a const.
    "function boot() { const local = {}; configureX({ local }); }",
    // registerShell slots follow the same rule, bare or through a namespace.
    "import * as S from './shell.js'; function f() {} const g = () => 1; registerShell({ f, g }); S.registerShell({ f });",
    // Only the shell's own name: another method called registerX is out of scope.
    'let s = 1; S.registerOther({ s });',
  ],
  invalid: [
    { code: 'let s = 1; configureX({ s });', errors: [{ messageId: 'mutable', data: { name: 's', kind: 'let', callee: 'configureX' } }] },
    { code: 'var v = 1; configureX({ v });', errors: [{ messageId: 'mutable', data: { name: 'v', kind: 'var', callee: 'configureX' } }] },
    { code: 'function boot(p) { configureX({ p }); }', errors: [{ messageId: 'mutable', data: { name: 'p', kind: 'Parameter', callee: 'configureX' } }] },
    { code: 'configureX({ undeclared });', errors: [{ messageId: 'mutable', data: { name: 'undeclared', kind: 'global', callee: 'configureX' } }] },
    { code: 'const o = { m: {} }; configureX({ m: o.m });', errors: [{ messageId: 'notBinding', data: { callee: 'configureX', type: 'MemberExpression' } }] },
    { code: 'configureX({ n: 3 });', errors: [{ messageId: 'notBinding', data: { callee: 'configureX', type: 'Literal' } }] },
    { code: 'const o = {}; configureX({ ...o });', errors: [{ messageId: 'notBinding', data: { callee: 'configureX', type: 'SpreadElement' } }] },
    // A registered shell slot that is a let would be called stale after the
    // root reassigns it.
    { code: 'let current = () => 1; registerShell({ current });', errors: [{ messageId: 'mutable', data: { name: 'current', kind: 'let', callee: 'registerShell' } }] },
    { code: "import * as S from './shell.js'; let current = () => 1; S.registerShell({ current });", errors: [{ messageId: 'mutable', data: { name: 'current', kind: 'let', callee: 'registerShell' } }] },
    { code: 'const o = { f() {} }; registerShell({ f: o.f });', errors: [{ messageId: 'notBinding', data: { callee: 'registerShell', type: 'MemberExpression' } }] },
    // Every offending property is reported, not just the first.
    { code: 'let a = 1; let b = 2; function f() {} configureX({ a, f, b });', errors: [{ messageId: 'mutable' }, { messageId: 'mutable' }] },
    { code: 'const o = {}; let b = 2; configureX({ ...o, b });', errors: [{ messageId: 'notBinding' }, { messageId: 'mutable' }] },
  ],
});

// S20k: no module takes a new dependency table; the legacy option names the
// receivers that still do, by file and function.
const legacy = [{ legacy: ['tuning.js:configureTuning'] }];
tester.run('shell-bindings: no new configureX export', nz.rules['shell-bindings'], {
  valid: [
    { code: 'export function configureTuning(impl) { return impl; }', filename: 'static/tuning.js', options: legacy },
    // Calling a legacy receiver, and a name that only starts like one, are out of scope.
    { code: "import { configureTuning } from './tuning.js'; configureTuning({});", filename: 'static/dashboard.js', options: legacy },
    { code: 'export function configured() {} export const configure = 1; export function reconfigureX() {}', filename: 'static/view.js', options: legacy },
  ],
  invalid: [
    { code: 'export function configureFoo(impl) { return impl; }', filename: 'static/view.js', options: legacy, errors: [{ messageId: 'newConfigure', data: { name: 'configureFoo' } }] },
    // The legacy name is allowed only in its own file, and a second one in that file is new.
    { code: 'export function configureTuning(impl) { return impl; }', filename: 'static/view.js', options: legacy, errors: [{ messageId: 'newConfigure' }] },
    { code: 'export function configureMore(impl) { return impl; }', filename: 'static/tuning.js', options: legacy, errors: [{ messageId: 'newConfigure' }] },
    // Every export spelling, and no option at all.
    { code: 'export const configureFoo = (impl) => impl;', filename: 'static/view.js', errors: [{ messageId: 'newConfigure' }] },
    { code: 'function configureFoo() {} export { configureFoo };', filename: 'static/view.js', errors: [{ messageId: 'newConfigure' }] },
    { code: 'function wire() {} export { wire as configureFoo };', filename: 'static/view.js', errors: [{ messageId: 'newConfigure', data: { name: 'configureFoo' } }] },
    { code: 'export default function configureFoo() {}', filename: 'static/view.js', errors: [{ messageId: 'newConfigure' }] },
  ],
});

tester.run('deps-keys', nz.rules['deps-keys'], {
  valid: [
    // Declared keys, read every way the receivers spell them.
    "const deps = { a: null, 'b-c': null }; deps.a(); deps['b-c'](); const { a } = deps; let x; ({ a: x } = deps);",
    // The receiver's copy loop is computed: no static key to check.
    'const deps = { a: null }; export function configureX(impl) { for (const k of Object.keys(deps)) deps[k] = impl[k]; }',
    // A shadowing deps is not the table.
    'const deps = { a: null }; function f(deps) { deps.b(); } const g = () => { const deps = {}; deps.c(); };',
    // No literal of static keys: the key set is unknown, the rule is silent.
    'const extra = {}; const deps = { a: null, ...extra }; deps.z();',
    'const deps = makeDeps(); deps.z();',
    'deps.z();',
  ],
  invalid: [
    { code: 'const deps = { a: null }; deps.shortPath();', errors: [{ messageId: 'unknownKey', data: { key: 'shortPath', keys: 'a' } }] },
    { code: "const deps = { a: null }; deps['b']();", errors: [{ messageId: 'unknownKey', data: { key: 'b', keys: 'a' } }] },
    { code: 'const deps = { a: null }; function f() { const { a, b } = deps; return a + b; }', errors: [{ messageId: 'unknownKey', data: { key: 'b', keys: 'a' } }] },
    { code: 'const deps = { a: null }; let b; ({ b } = deps);', errors: [{ messageId: 'unknownKey', data: { key: 'b', keys: 'a' } }] },
    // A write to an undeclared key is not a slot either.
    { code: 'const deps = {}; deps.a = 1;', errors: [{ messageId: 'unknownKey', data: { key: 'a', keys: 'empty' } }] },
    // Inside a nested function, the module's deps is still the table.
    { code: 'const deps = { a: null }; export function f() { return () => deps.c(); }', errors: [{ messageId: 'unknownKey', data: { key: 'c', keys: 'a' } }] },
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

const WSM = "import { wsm } from './ws_manager.js'; ";

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
    "const k = { [NZ_CONTRACT.WS.history]: 1, ['a' + 'b']: 2 };",
    "const m = NZ_CONTRACT.WS['history'];",
    // A class definition runs nothing unless its heritage, computed keys,
    // static fields or static blocks do; instance fields run at construction.
    'class D extends Base { static n = 1; x = f(); m() { g(); } }',
    'export default class { static s = new Set(); }',
    'const E = class { static k = {}; };',
    // D2: the WS dispatch table's registration calls are the one exception,
    // on the wsm ws_manager.js exports (or, in ws_manager.js, its own const).
    `${WSM}wsm.on(NZ_CONTRACT.WS.history, (msg) => f(msg));`,
    `${WSM}wsm.onReady(() => f());`,
    `${WSM}wsm.onStateChange((s) => f(s));`,
    `${WSM}wsm.onAuthFail((msg) => f(msg));`,
    `${WSM}wsm.on(NZ_CONTRACT.WS.history, handleHistory);`,
    "import { WS_STATES, wsm } from '../static/ws_manager.js'; wsm.on('x', f);",
    { code: "export const wsm = { on() {} }; wsm.on('x', () => {});", filename: '/repo/internal/server/static/ws_manager.js' },
    // Destructuring with pure defaults and computed keys, and an accessor
    // literal bound to a name (its getter runs only when read later).
    "const { a = 1, ['k' + 'ey']: kk, ...rest } = {};",
    'const [b = new Set(), , ...more] = [];',
    'const { p: { q = [] } = {} } = {};',
    'const acc = { get x() { return init(); }, set x(v) { save(v); } };',
    'const spread = { ...{ a: 1 }, ...base };',
    'const merged = Object.assign({}, { a: 1 }, base);',
    'const frozenAcc = Object.freeze({ get x() { return init(); } });',
    // shell.js's slot table: sealing a fresh literal mutates nothing else.
    'export const shell = Object.seal({ selectSession: null });',
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
    // The D2 method names on any object other than wsm are not exempt.
    { code: "bus.on('x', f);", errors: [{ messageId: 'sideEffect' }] },
    { code: 'sessionStream.onReady(() => f());', errors: [{ messageId: 'sideEffect' }] },
    // A computed member names whatever the variable holds, not wsm's method.
    { code: "wsm[on]('x', f);", errors: [{ messageId: 'sideEffect' }] },
    // The registration is exempt; building its handler by a call is not.
    { code: `${WSM}wsm.on('x', init());`, errors: [{ messageId: 'sideEffect' }] },
    // The exception is the imported wsm, not any binding named wsm: a local
    // object, an unresolved global, a renamed import of something else, or
    // wsm from another module.
    { code: "const wsm = { on: init }; wsm.on('x');", errors: [{ messageId: 'sideEffect' }] },
    { code: "wsm.on('x', f);", errors: [{ messageId: 'sideEffect' }] },
    { code: "import { other as wsm } from './ws_manager.js'; wsm.on('x', f);", errors: [{ messageId: 'sideEffect' }] },
    { code: "import { wsm } from './fake_ws_manager.jsx'; wsm.on('x', f);", errors: [{ messageId: 'sideEffect' }] },
    { code: "import wsm from './ws_manager.js'; wsm.on('x', f);", errors: [{ messageId: 'sideEffect' }] },
    { code: "export const wsm = { on: init }; wsm.on('x');", filename: '/repo/internal/server/static/not_ws_manager.js', errors: [{ messageId: 'sideEffect' }] },
    { code: "let wsm = { on() {} }; wsm.on('x', () => {});", filename: '/repo/internal/server/static/ws_manager.js', errors: [{ messageId: 'sideEffect' }] },
    // `delete` mutates what its operand names.
    { code: 'const x = delete window.foo;', errors: [{ messageId: 'sideEffect' }] },
    // Object.assign/freeze/seal mutate their first argument; only a fresh literal
    // keeps them pure.
    { code: 'const g = Object.assign(window, { x: 1 });', errors: [{ messageId: 'sideEffect' }] },
    { code: 'const g = Object.freeze(window);', errors: [{ messageId: 'sideEffect' }] },
    { code: 'const g = Object.seal(window);', errors: [{ messageId: 'sideEffect' }] },
    { code: 'const g = Object[assign]({}, {});', errors: [{ messageId: 'sideEffect' }] },
    // Calls laundered through a computed member property or object key.
    { code: 'const z = window[setTimeout(() => {}, 0)];', errors: [{ messageId: 'sideEffect' }] },
    { code: "const z = { [fetch('/x')]: 1 };", errors: [{ messageId: 'sideEffect' }] },
    // Class definitions that run code at load time.
    { code: 'class Y { static t = setInterval(() => {}, 1000); }', errors: [{ messageId: 'sideEffect' }] },
    { code: 'class Y { static { setInterval(() => {}, 1000); } }', errors: [{ messageId: 'sideEffect' }] },
    { code: 'class Y { [f()]() {} }', errors: [{ messageId: 'sideEffect' }] },
    { code: 'class Y extends mixin(Base) {}', errors: [{ messageId: 'sideEffect' }] },
    { code: 'export class Y { static { f(); } }', errors: [{ messageId: 'sideEffect' }] },
    { code: 'export default class { static { f(); } }', errors: [{ messageId: 'sideEffect' }] },
    { code: 'const Y = class { static t = f(); };', errors: [{ messageId: 'sideEffect' }] },
    // A destructuring pattern's defaults and computed keys run at load time.
    { code: 'const { a = init() } = {};', errors: [{ messageId: 'sideEffect' }] },
    { code: 'const [b = init()] = [];', errors: [{ messageId: 'sideEffect' }] },
    { code: 'const { [init()]: c } = {};', errors: [{ messageId: 'sideEffect' }] },
    { code: 'const { p: { q = init() } } = { p: {} };', errors: [{ messageId: 'sideEffect' }] },
    { code: 'const [...[r = init()]] = [];', errors: [{ messageId: 'sideEffect' }] },
    { code: 'export const { zzTimer = setInterval(() => {}, 1000) } = {};', errors: [{ messageId: 'sideEffect' }] },
    // Copying a literal's accessors (spread, Object.assign source) calls the getter.
    { code: 'const d = { ...{ get x() { init(); return 1; } } };', errors: [{ messageId: 'sideEffect' }] },
    { code: 'const d = { ...(c ? { set x(v) {} } : {}) };', errors: [{ messageId: 'sideEffect' }] },
    { code: 'const f = Object.assign({}, { get x() { return init(); } });', errors: [{ messageId: 'sideEffect' }] },
    { code: 'const f = Object.assign({ set x(v) { init(v); } }, { x: 1 });', errors: [{ messageId: 'sideEffect' }] },
  ],
});
