// node --test scripts/ts-check.test.mjs
// The dashboard type-checks file by file (#3439): internal/server/static/
// tsconfig.json has checkJs off, a file opts in with a `// @ts-check` first
// line, and lint-js runs tsc over the directory, so an opted-in file must
// have zero errors. CHECKED is the floor: a file cannot drop its pragma (and
// with it every check) or silence lines with @ts-ignore / @ts-expect-error /
// @ts-nocheck without failing here, and a new opt-in is listed too. The
// probes prove wire.d.ts reaches a handler through wsm.on; the plants prove
// the annotations type the frames and bodies the modules read; wireReach
// keeps tsc the whole field check on frames.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import { createRequire } from 'node:module';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = path.join(path.dirname(fileURLToPath(import.meta.url)), '..');
const STATIC = path.join(ROOT, 'internal', 'server', 'static');
const TSC = path.join(ROOT, 'test', 'e2e', 'node_modules', 'typescript', 'bin', 'tsc');
const PRAGMA = '// @ts-check\n';

const CHECKED = [
  'agent_view.js',
  'cron_live.js',
  'cron_view.js',
  'dashboard.js',
  'event_render.js',
  'event_stream.js',
  'self_update.js',
  'session_ident.js',
  'session_list.js',
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

test('an opted-in file carries no directive that switches checks back off', () => {
  const hits = CHECKED.flatMap((f) => fs.readFileSync(path.join(STATIC, f), 'utf8').split('\n')
    .flatMap((line, i) => (/@ts-(nocheck|ignore|expect-error)\b/.test(line) ? [`${f}:${i + 1}`] : [])));
  assert.deepEqual(hits, [], 'fix the type error or leave the file out of CHECKED');
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

// Each anchor is the line that types a value the root modules read; the plant
// after it reads a field no wire type has. Without the annotation the value is
// `any` and tsc says nothing, so every plant must come back as an error.
const PLANTS = [
  ['event_stream.js', "onHistory(/** @type {WsFrames['history']} */ msg) {", 'msg', 'WsFrame_history'],
  ['event_stream.js', "onEvent(/** @type {WsFrames['event']} */ msg) {", 'msg', 'WsFrame_event'],
  ['event_stream.js', "onSendAck(/** @type {Omit<WsFrames['send_ack'], 'type'>} */ msg) {", 'msg', "Omit<WsFrame_send_ack, \"type\">"],
  ['event_stream.js', "onInterruptAck(/** @type {WsFrames['interrupt_ack']} */ msg) {", 'msg', 'WsFrame_interrupt_ack'],
  ['event_stream.js', "onSendError(/** @type {WsFrames['send_error']} */ msg) {", 'msg', 'WsFrame_send_error'],
  ['event_stream.js', 'function renderInitialHistory(', 'msg', 'WsFrame_history'],
  ['session_list.js', "function onSessionState(/** @type {WsFrames['session_state']} */ msg) {", 'msg', 'WsFrame_session_state'],
  ['session_list.js', 'function settleTurnBoundary(', 'msg', 'WsFrame_session_state'],
  ['session_list.js', 'function paintSessionCardState(', 'msg', 'WsFrame_session_state'],
  ['session_list.js', 'function resubscribeOnRunning(', 'msg', 'WsFrame_session_state'],
  ['session_list.js', 'let data = got.data;', 'data', 'RestResponse_sessions'],
  ['session_list.js', 'load.then(h => {', 'h', 'RestResponse_sessions_history'],
  ['session_list.js', 'function sessionCardHtml(', 's', 'SessionSnapshot'],
  ['self_update.js', 'wsm.onReady((msg) => {', 'msg', 'WsFrame_auth_ok'],
  ['dashboard.js', 'wsm.onAuthFail((msg) => {', 'msg', 'WsFrame_auth_fail'],
];

// staticCopy copies what tsc reads from static/ into a temp dir.
function staticCopy(prefix) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), prefix));
  for (const f of fs.readdirSync(STATIC)) {
    if (f.endsWith('.js') || f === 'wire.d.ts' || f === 'tsconfig.json') fs.copyFileSync(path.join(STATIC, f), path.join(dir, f));
  }
  return dir;
}

test('the modules type their frame handlers and session fetches', () => {
  const dir = staticCopy('nz-ts-plant-');
  try {
    PLANTS.forEach(([file, anchor, name], i) => {
      const lines = fs.readFileSync(path.join(dir, file), 'utf8').split('\n');
      const at = lines.findIndex((l) => l.includes(anchor));
      assert.ok(at !== -1 && lines.findLastIndex((l) => l.includes(anchor)) === at, `${file} must have exactly one ${anchor}`);
      lines.splice(at + 1, 0, `void ${name}.nzPlanted${i};`);
      fs.writeFileSync(path.join(dir, file), lines.join('\n'));
    });
    let out = '';
    try {
      execFileSync(process.execPath, [TSC, '-p', dir], { encoding: 'utf8' });
    } catch (err) {
      out = String(err.stdout || err.message);
    }
    PLANTS.forEach(([file, anchor, , type], i) => {
      assert.match(out, new RegExp(`${file}\\(\\d+,\\d+\\): error TS2339: Property 'nzPlanted${i}' does not exist on type '${type}`),
        `the value typed at ${file} "${anchor}" is untyped`);
    });
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});

// wireReach type-checks dir and returns what would let a frame field go
// unchecked: a file a wire.d.ts type reaches without the pragma (tsc reports
// nothing there), and a WS frame handed to an untyped parameter, as an
// argument or as the frame of a handler passed by name (its reads are `any`).
// typed counts the wire-typed identifiers in opted-in files.
function wireReach(dir) {
  const ts = createRequire(path.join(ROOT, 'test', 'e2e', 'package.json'))('typescript');
  const cfg = ts.getParsedCommandLineOfConfigFile(path.join(dir, 'tsconfig.json'), {}, { ...ts.sys, onUnRecoverableConfigFileDiagnostic: () => {} });
  const program = ts.createProgram(cfg.fileNames, cfg.options);
  const checker = program.getTypeChecker();
  const wire = path.join(dir, 'wire.d.ts');
  const ours = (f) => path.dirname(f) === dir && f.endsWith('.js');
  const wireName = (t, frame, seen = new Set()) => {
    if (!t || seen.has(t)) return null;
    seen.add(t);
    if (t.isUnionOrIntersection()) return t.types.map((x) => wireName(x, frame, seen)).find(Boolean) || null;
    const sym = t.aliasSymbol || t.getSymbol();
    const ok = sym && sym.declarations?.some((d) => d.getSourceFile().fileName === wire) && (!frame || sym.name.startsWith('WsFrame_'));
    return ok ? sym.name : null;
  };
  const isAny = (t) => (t.flags & ts.TypeFlags.Any) !== 0;
  const declaredHere = (sym) => sym.declarations?.some((d) => ours(d.getSourceFile().fileName));
  const problems = [];
  let typed = 0;
  for (const sf of program.getSourceFiles()) {
    if (!ours(sf.fileName)) continue;
    const file = path.basename(sf.fileName);
    const checked = sf.text.startsWith(PRAGMA);
    const at = (n) => `${file}:${sf.getLineAndCharacterOfPosition(n.getStart()).line + 1}`;
    let reached = null;
    const visit = (n) => {
      if (ts.isIdentifier(n) && wireName(checker.getTypeAtLocation(n), false)) {
        if (checked) typed++;
        else reached ??= n;
      }
      if (ts.isCallExpression(n)) {
        const sig = checker.getResolvedSignature(n);
        n.arguments.forEach((arg, i) => {
          const p = sig?.parameters[Math.min(i, sig.parameters.length - 1)];
          if (!p) return;
          const want = checker.getTypeOfSymbolAtLocation(p, n);
          const got = checker.getTypeAtLocation(arg);
          const frame = wireName(got, true);
          if (frame && isAny(want) && declaredHere(p)) problems.push(`${at(arg)}: a ${frame} reaches untyped parameter ${p.name}`);
          const [ws, gs] = [want.getCallSignatures()[0], got.getCallSignatures()[0]];
          ws?.parameters.forEach((wp, j) => {
            const gp = gs?.parameters[j];
            const f = gp && wireName(checker.getTypeOfSymbolAtLocation(wp, n), true);
            if (f && isAny(checker.getTypeOfSymbolAtLocation(gp, arg))) problems.push(`${at(arg)}: a ${f} reaches untyped parameter ${gp.name}`);
          });
        });
      }
      ts.forEachChild(n, visit);
    };
    visit(sf);
    if (reached) problems.push(`${at(reached)}: ${reached.text} has a wire.d.ts type but ${file} has no // @ts-check`);
  }
  return { problems, typed };
}

test('every frame read is typed: wire types reach only opted-in files, and no frame reaches an untyped parameter', () => {
  const { problems, typed } = wireReach(STATIC);
  assert.deepEqual(problems, []);
  assert.ok(typed > 0, 'no wire-typed identifier in an opted-in file: the reach check has gone blind');
});

test('wireReach catches an untyped forward, a handler passed by name and an unchecked consumer', () => {
  const dir = staticCopy('nz-ts-reach-');
  try {
    const av = path.join(dir, 'agent_view.js');
    const src = fs.readFileSync(av, 'utf8');
    const typedHandler = "function onAgentEvent(/** @type {WsFrames['agent_event']} */ msg) {";
    assert.ok(src.includes(typedHandler), 'agent_view.js lost its typed onAgentEvent');
    fs.writeFileSync(av, src.replace(typedHandler, 'function onAgentEvent(msg) {'));
    const head = "import { NZ_CONTRACT } from './contract.js';\nimport { wsm } from './ws_manager.js';\n";
    fs.writeFileSync(path.join(dir, 'probe_named.js'), PRAGMA + head +
      'function onPong(m) { return m.nope; }\nwsm.on(NZ_CONTRACT.WS.pong, onPong);\n');
    fs.writeFileSync(path.join(dir, 'probe_unchecked.js'), head +
      'wsm.on(NZ_CONTRACT.WS.pong, (msg) => msg.nope);\n');
    const { problems } = wireReach(dir);
    const want = [
      /^agent_view\.js:\d+: a WsFrame_agent_event reaches untyped parameter msg$/,
      /^probe_named\.js:5: a WsFrame_pong reaches untyped parameter m$/,
      /^probe_unchecked\.js:3: msg has a wire\.d\.ts type but probe_unchecked\.js has no \/\/ @ts-check$/,
    ];
    for (const re of want) assert.ok(problems.some((p) => re.test(p)), `${re} not in:\n${problems.join('\n')}`);
    assert.equal(problems.length, want.length, problems.join('\n'));
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
});
