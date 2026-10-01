// node --test scripts/check-ws-receivers.test.mjs
// One fixture per rule: a rule that stops matching would pass every file
// silently, so each is shown to fire here, and the clean shape to pass.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { check, checkSource } from './check-ws-receivers.mjs';

const OUT = new Set(['history', 'pong']);

// A clean pair of modules: a claim and a fallback for history, a no-op for pong.
const CLEAN = `
const wsm = { onHistory(msg) { return msg.key; } };
const live = (msg) => msg.key === 'cron:x';
function onLive(msg) { return msg.events; }
wsm.on(NZ_CONTRACT.WS.history, onLive, live);
wsm.on(NZ_CONTRACT.WS.history, (msg) => wsm.onHistory(msg));
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
  const h = problemsOf(CLEAN.replace('(msg) => wsm.onHistory(msg)', '(frame) => wsm.onHistory(frame)'));
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
