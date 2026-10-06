// node --test scripts/ts-check.test.mjs
// The dashboard type-checks file by file (#3439): internal/server/static/
// tsconfig.json has checkJs off, a file opts in with a `// @ts-check` first
// line, and lint-js runs tsc over the directory, so an opted-in file must
// have zero errors. CHECKED is the floor: a file cannot drop its pragma (and
// with it every check) without that showing as a diff here, and a new opt-in
// is listed too. The probes prove wire.d.ts reaches a handler through wsm.on.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = path.join(path.dirname(fileURLToPath(import.meta.url)), '..');
const STATIC = path.join(ROOT, 'internal', 'server', 'static');
const TSC = path.join(ROOT, 'test', 'e2e', 'node_modules', 'typescript', 'bin', 'tsc');
const PRAGMA = '// @ts-check\n';

const CHECKED = [
  'session_stream.js',
  'ws_manager.js',
];

test('the files carrying // @ts-check are exactly the floor list', () => {
  const opted = fs.readdirSync(STATIC)
    .filter((f) => f.endsWith('.js') && fs.readFileSync(path.join(STATIC, f), 'utf8').startsWith(PRAGMA))
    .sort();
  assert.deepEqual(opted, [...CHECKED].sort(),
    'a file with the pragma must be listed in CHECKED, and a listed file must keep it as its first line');
});

test('no wire.d.ts global merges into a TypeScript lib declaration', () => {
  const names = [...fs.readFileSync(path.join(STATIC, 'wire.d.ts'), 'utf8').matchAll(/^ {2}interface (\w+) \{$/gm)].map((m) => m[1]);
  assert.ok(names.includes('EventEntry') && names.includes('WsFrames'), 'wire.d.ts lost its interfaces');
  const libDir = path.join(ROOT, 'test', 'e2e', 'node_modules', 'typescript', 'lib');
  const lib = fs.readdirSync(libDir).filter((f) => /^lib\..*\.d\.ts$/.test(f))
    .map((f) => fs.readFileSync(path.join(libDir, f), 'utf8')).join('\n');
  const merged = names.filter((n) => new RegExp(`\\b(interface|type|var|class) ${n}\\b`).test(lib));
  assert.deepEqual(merged, [], 'these names would extend the DOM/ES globals instead of declaring a wire type');
});

// tscProbe type-checks one @ts-check module against the real tsconfig,
// wire.d.ts and ws_manager.js, returning tsc's diagnostics ('' when clean).
function tscProbe(body) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'nz-ts-probe-'));
  try {
    const probe = path.join(dir, 'probe.js');
    fs.writeFileSync(probe, PRAGMA +
      `import { NZ_CONTRACT } from ${JSON.stringify(path.join(STATIC, 'contract.js'))};\n` +
      `import { wsm } from ${JSON.stringify(path.join(STATIC, 'ws_manager.js'))};\n` + body + '\n');
    fs.writeFileSync(path.join(dir, 'tsconfig.json'), JSON.stringify({
      extends: path.join(STATIC, 'tsconfig.json'),
      include: [probe, path.join(STATIC, 'wire.d.ts')],
    }));
    try {
      execFileSync(process.execPath, [TSC, '-p', dir], { encoding: 'utf8' });
      return '';
    } catch (err) {
      return String(err.stdout || err.message);
    }
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
}

test('wsm.on types each handler by its frame, and only outbound frame types register', () => {
  assert.equal(tscProbe([
    'wsm.on(NZ_CONTRACT.WS.session_state, (msg) => { const s = msg.state; return s; });',
    'wsm.on(NZ_CONTRACT.WS.event, (msg) => { const t = msg.event.type; return t; }, (msg) => msg.key === "k");',
  ].join('\n')), '');

  const typo = tscProbe('wsm.on(NZ_CONTRACT.WS.session_state, (msg) => { const s = msg.stat; return s; });');
  assert.match(typo, /Property 'stat' does not exist on type 'WsFrame_session_state'/);

  const crossFrame = tscProbe('wsm.on(NZ_CONTRACT.WS.pong, (msg) => { const e = msg.event; return e; });');
  assert.match(crossFrame, /Property 'event' does not exist on type 'WsFrame_pong'/);

  const kind = tscProbe('wsm.on(NZ_CONTRACT.WS.event, (msg) => { if (msg.event.type === "txt") return; });');
  assert.match(kind, /TS2367/, 'EventEntry.type is the closed kind union');

  // contract.js's frozen const literal is closed: a misspelt key is an error,
  // not an implicit any that would switch the handler's checks off.
  assert.match(tscProbe('wsm.on(NZ_CONTRACT.WS.evnt, () => {});'), /Property 'evnt' does not exist/);
  assert.match(tscProbe('const p = NZ_CONTRACT.API.sesions;'), /Property 'sesions' does not exist/);
  assert.match(tscProbe('wsm.on(NZ_CONTRACT.WS.subscribe, () => {});'), /not assignable to parameter of type 'keyof WsFrames'/);
});
