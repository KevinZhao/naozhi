// node --test scripts/check-ws-receivers.test.mjs
// One fixture per rule: a rule that stops matching would pass every file
// silently, so each is shown to fire here, and the clean shape to pass.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { check, checkModules, checkSource, MANAGED } from './check-ws-receivers.mjs';

const OUT = new Set(['history', 'pong']);

// A clean pair of modules: a claim and a fallback for history, a no-op for pong.
const CLEAN = `
const sessionFrames = { onHistory(msg) { return msg.key; } };
const live = (msg) => msg.key === 'cron:x';
function onLive(msg) { return msg.events; }
wsm.on(NZ_CONTRACT.WS.history, onLive, live);
wsm.on(NZ_CONTRACT.WS.history, (msg) => sessionFrames.onHistory(msg));
wsm.on(NZ_CONTRACT.WS.pong, () => {});
`;

const problemsOf = (src, outbound = OUT) => check([['a.js', src]], outbound).problems;
const only = (problems, rule) => {
  assert.ok(problems.length > 0, `expected a ${rule} problem, got none`);
  for (const p of problems) assert.match(p, new RegExp('\\(' + rule + '\\b'), p);
};

test('the clean shape passes and its registrations are collected', () => {
  const { problems, regs } = check([['a.js', CLEAN]], OUT);
  assert.deepEqual(problems, []);
  assert.deepEqual(regs.map((r) => [r.key, r.claim]), [['history', true], ['history', false], ['pong', false]]);
});

test('a top-level IIFE body is module scope', () => {
  const src = CLEAN.replace(/^wsm\.on/gm, '  wsm.on');
  assert.deepEqual(problemsOf(`(function () {\n${src}\n})();`), []);
});

test('R1: a switch on .type with an outbound case, literal or constant', () => {
  only(problemsOf(CLEAN + "function f(msg) { switch (msg.type) { case 'pong': break; } }"), 'R1');
  only(problemsOf(CLEAN + 'function f(m) { switch (m.type) { case NZ_CONTRACT.WS.history: break; } }'), 'R1');
});

test('R1: a .type comparison against an outbound type, either side', () => {
  only(problemsOf(CLEAN + "const a = (msg) => msg.type === 'history';"), 'R1');
  only(problemsOf(CLEAN + 'const b = (x) => NZ_CONTRACT.WS.pong !== x.type;'), 'R1');
});

test('R1 leaves non-outbound types and non-type discriminants alone', () => {
  assert.deepEqual(problemsOf(CLEAN + `
function f(e) { switch (e.type) { case 'text': break; } return e.kind === 'history'; }
const g = (b) => b.type === 'image/png';`), []);
});

test('R2: the type must be NZ_CONTRACT.WS.<outbound key>', () => {
  const strType = problemsOf(CLEAN + "wsm.on('pong', () => {}, () => true);");
  assert.ok(strType.some((p) => /must name an outbound type/.test(p)), strType.join('\n'));
  const inbound = problemsOf(CLEAN + 'wsm.on(NZ_CONTRACT.WS.subscribe, () => {}, () => true);');
  assert.ok(inbound.some((p) => /must name an outbound type/.test(p)), inbound.join('\n'));
});

test('R2: two or three arguments', () => {
  const p = problemsOf(CLEAN + 'wsm.on(NZ_CONTRACT.WS.pong);');
  assert.ok(p.some((x) => /takes \(type, handler\[, when\]\)/.test(x)), p.join('\n'));
});

test('R2: registration inside a function, a branch or a nested IIFE is refused', () => {
  for (const src of [
    'function later() { wsm.on(NZ_CONTRACT.WS.pong, () => {}, () => true); }',
    'if (window.x) { wsm.on(NZ_CONTRACT.WS.pong, () => {}, () => true); }',
    'const r = wsm.on(NZ_CONTRACT.WS.pong, () => {}, () => true);',
    '(function () { (function () { wsm.on(NZ_CONTRACT.WS.pong, () => {}, () => true); })(); })();',
  ]) {
    const p = problemsOf(CLEAN + src);
    assert.ok(p.some((x) => /statement at module scope \(R2\)/.test(x)), src + '\n' + p.join('\n'));
  }
});

test('R3: an outbound type with no registration', () => {
  const p = problemsOf(CLEAN.replace('wsm.on(NZ_CONTRACT.WS.pong, () => {});', 'wsm.on(NZ_CONTRACT.WS.history, () => {}, live);'));
  assert.ok(p.some((x) => /^pong: no wsm.on handler/.test(x)), p.join('\n'));
});

test('R3: a second unconditional handler, across files', () => {
  const { problems } = check([['a.js', CLEAN], ['b.js', 'wsm.on(NZ_CONTRACT.WS.pong, () => {});']], OUT);
  assert.ok(problems.some((x) => /^pong: 2 unconditional handlers \(a\.js:\d+, b\.js:1\)/.test(x)), problems.join('\n'));
});

test('R4: fewer registrations than outbound types means the scan went blind', () => {
  // R3 names the uncovered type; R4 says the collector itself saw too little.
  const two = check([['a.js', 'wsm.on(NZ_CONTRACT.WS.pong, () => {}, () => true);']], new Set(['pong', 'history'])).problems;
  assert.ok(two.some((x) => /only 1 wsm.on registrations for 2 outbound types .*\(R4\)/.test(x)), two.join('\n'));
});

test('R6: a handler or claim parameter not named msg', () => {
  const h = problemsOf(CLEAN.replace('(msg) => sessionFrames.onHistory(msg)', '(frame) => sessionFrames.onHistory(frame)'));
  assert.ok(h.some((x) => /handler must name its frame parameter msg/.test(x)), h.join('\n'));
  const c = problemsOf(CLEAN.replace("const live = (msg) => msg.key === 'cron:x';", "const live = (f) => f.key === 'cron:x';"));
  assert.ok(c.some((x) => /claim must name its frame parameter msg/.test(x)), c.join('\n'));
});

test('R6: msg forwarded to a same-file function or method whose parameter is not msg', () => {
  const m = problemsOf(CLEAN.replace('onHistory(msg) { return msg.key; }', 'onHistory(frame) { return frame.key; }'));
  assert.ok(m.some((x) => /forwarded to a parameter not named msg/.test(x)), m.join('\n'));
  const f = problemsOf(CLEAN.replace('function onLive(msg) { return msg.events; }', 'function onLive(f) { return f.events; }'));
  assert.ok(f.some((x) => /handler must name its frame parameter msg/.test(x)), f.join('\n'));
  const fwd = problemsOf(CLEAN + "function helper(frame) { return frame; }\nwsm.on(NZ_CONTRACT.WS.pong, (msg) => helper(msg), live);");
  assert.ok(fwd.some((x) => /forwarded to a parameter not named msg/.test(x)), fwd.join('\n'));
});

test('R6: msg forwarded to a method its same-file object does not define', () => {
  const p = problemsOf(CLEAN.replace('(msg) => sessionFrames.onHistory(msg)', '(msg) => sessionFrames.onEvent(msg)'));
  assert.ok(p.some((x) => /forwarded to sessionFrames.onEvent, which sessionFrames does not define/.test(x)), p.join('\n'));
});

test('R6: msg forwarded to an imported binding is refused', () => {
  const p = problemsOf("import { cronLiveEvent } from './cron_view.js';\n" + CLEAN +
    'wsm.on(NZ_CONTRACT.WS.pong, (msg) => cronLiveEvent(msg), live);');
  assert.ok(p.some((x) => /forwarded to imported cronLiveEvent/.test(x)), p.join('\n'));
});

test('R6: a named handler must be a function declared in the file', () => {
  const p = problemsOf(CLEAN + 'wsm.on(NZ_CONTRACT.WS.pong, elsewhere, live);');
  assert.ok(p.some((x) => /handler elsewhere must be a function declared in this file/.test(x)), p.join('\n'));
});

test('checkSource reports the file and line of a problem', () => {
  const { problems } = checkSource('x.js', "\n\nfunction f(msg) { return msg.type === 'pong'; }", OUT);
  assert.deepEqual(problems, ['x.js:3: .type compared with outbound frame type (R1: register it with wsm.on)']);
});

test('R6: a wsm.onAuthFail callback names its parameter msg', () => {
  const p = problemsOf(CLEAN + 'wsm.onAuthFail((frame) => frame.error);');
  assert.ok(p.some((x) => /onAuthFail callback must name its frame parameter msg/.test(x)), p.join('\n'));
  assert.deepEqual(problemsOf(CLEAN + 'wsm.onAuthFail((msg) => msg.error);'), []);
});

// R5 / R7 fixtures: a two-object world with its own owners, core set and leaf.
const CFG = { managed: { wsm: 'ws.js', stream: 'stream.js' }, core: { wsm: ['conn', 'on', 'send'] }, leaves: { 'ws.js': ['./contract.js'] } };
const WORLD = {
  'ws.js': "import { NZ_CONTRACT } from './contract.js';\nexport const wsm = { conn: null, on() {}, send(m) { return this.conn && m; } };\n",
  'stream.js': "import { wsm } from './ws.js';\nexport const stream = { key: null, sub(k) { this.key = k; wsm.send(k); } };\n",
  'user.js': "import { wsm } from './ws.js';\nimport { stream } from './stream.js';\nwsm.on(); stream.sub(stream.key);\n",
};
const modulesOf = (edits = {}) => checkModules(Object.entries({ ...WORLD, ...edits }), CFG);
const user = (line) => modulesOf({ 'user.js': WORLD['user.js'] + line + '\n' });
const fires = (problems, rule, re) => {
  assert.ok(problems.some((p) => p.includes('(' + rule + ')') && re.test(p)), `expected ${rule} ${re}, got:\n${problems.join('\n')}`);
};

test('R5/R7: the clean world passes', () => {
  assert.deepEqual(modulesOf(), []);
});

test('R5a: outside its owner a managed object is only name.<key>', () => {
  fires(user('const w = wsm;'), 'R5a', /^user\.js:4: wsm outside ws\.js only as wsm\.<key>/);
  fires(user('f(stream);'), 'R5a', /stream outside stream\.js/);
  fires(user("wsm['send'](1);"), 'R5a', /wsm outside ws\.js/);
  fires(user('const o = { ...stream };'), 'R5a', /stream outside stream\.js/);
  fires(user('export { wsm };'), 'R5a', /wsm outside ws\.js/);
  fires(user('if (wsm && wsm.conn) f();'), 'R5a', /wsm outside ws\.js/);
});

test('R5a: no property named after a managed object, and imports come from the owner', () => {
  fires(user('deps.wsm.send(1);'), 'R5a', /a property named wsm/);
  fires(user('configure({ wsm });'), 'R5a', /a property named wsm/);
  fires(user('configure({ wsm: null });'), 'R5a', /a property named wsm/);
  const p = modulesOf({ 'user.js': WORLD['user.js'].replace("from './ws.js'", "from './dashboard.js'") });
  fires(p, 'R5a', /wsm is imported from \.\/dashboard\.js, not its owner ws\.js/);
  assert.equal(p.filter((x) => /is imported from/.test(x)).length, 1, p.join('\n'));
  fires(modulesOf({ 'user.js': WORLD['user.js'].replace("import { wsm } from './ws.js';", "import { wsm as w } from './ws.js';") }), 'R5a', /wsm is imported as w; keep its name/);
});

test('R5b: an access names a declared key, here and in the owner', () => {
  fires(user('wsm.subscribedKey;'), 'R5b', /^user\.js:4: wsm\.subscribedKey is not a key wsm declares in ws\.js/);
  fires(modulesOf({ 'ws.js': WORLD['ws.js'].replace('return this.conn', 'return this.gone') }), 'R5b', /wsm\.gone is not a key/);
});

test('R5c: an assignment never creates a key', () => {
  fires(user('stream.extra = 1;'), 'R5c', /assigns stream\.extra, which stream does not declare/);
  fires(modulesOf({ 'stream.js': WORLD['stream.js'].replace('this.key = k;', 'this.key = k; this._pending = k;') }), 'R5c', /assigns stream\._pending/);
  // this inside a nested function is not the literal.
  assert.deepEqual(modulesOf({ 'stream.js': WORLD['stream.js'].replace('this.key = k;', 'this.key = k; [1].forEach(function () { this.other = 1; });') }), []);
});

test('R5d: wsm declares exactly its core set, both ways', () => {
  fires(modulesOf({ 'ws.js': WORLD['ws.js'].replace('conn: null,', 'conn: null, subscribedKey: null,') }), 'R5d', /wsm declares subscribedKey, which is not in its core set/);
  fires(checkModules(Object.entries(WORLD), { ...CFG, core: { wsm: ['conn', 'on', 'send', 'cronLive'] } }), 'R5d', /core set lists wsm\.cronLive, which wsm no longer declares — drop the entry/);
});

test('R5d: a declared key nobody references', () => {
  fires(modulesOf({ 'stream.js': WORLD['stream.js'].replace('key: null,', 'key: null, idle: 0,') }), 'R5d', /stream\.idle is declared but never referenced/);
});

test('R5: an owner without its literal is reported, not skipped', () => {
  fires(modulesOf({ 'stream.js': "export const stream = make();\nstream.sub();\n" }), 'R5', /stream\.js: no `const stream = \{ … \}` literal/);
});

test('R7: a leaf imports only its list, and must exist', () => {
  fires(modulesOf({ 'ws.js': "import { esc } from './nz_util.js';\n" + WORLD['ws.js'] }), 'R7', /ws\.js is a leaf and may import only \.\/contract\.js, not \.\/nz_util\.js/);
  fires(modulesOf({ 'ws.js': WORLD['ws.js'] + "export { esc } from './nz_util.js';\n" }), 'R7', /not \.\/nz_util\.js/);
  fires(modulesOf({ 'ws.js': WORLD['ws.js'] + "import('./nz_util.js');\n" }), 'R7', /may not import\(\)/);
  const gone = checkModules(Object.entries(WORLD), { ...CFG, leaves: { ...CFG.leaves, 'missing.js': [] } });
  fires(gone, 'R7', /^missing\.js: leaf not found/);
});

test('the production tree passes R5 and R7 with its real owners', async () => {
  const fs = await import('node:fs');
  const dir = new URL('../internal/server/static/', import.meta.url);
  const files = fs.readdirSync(dir).filter((f) => f.endsWith('.js') && f !== 'contract.js' && f !== 'sw.js')
    .map((f) => [f, fs.readFileSync(new URL(f, dir), 'utf8')]);
  assert.deepEqual(checkModules(files), []);
  assert.deepEqual(Object.keys(MANAGED), ['wsm', 'sessionStream', 'cronLive']);
});
