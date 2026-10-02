// node --test scripts/eslint-config.test.mjs — every dashboard script is
// linted as an ES module with no per-file globals (#2907).
//
// A module can reach another file's names only by import or through a global.
// no-undef turns every other bare reference into an error, but only while each
// file is checked as a module and its globals are the shared browser list: a
// per-file globals entry, or a file linted as a classic script, reopens the
// implicit cross-file reference that js-deps-freeze was built to count.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';
import nz from './eslint-plugin-nz.mjs';
import { loadCaps } from './js-ratchet.mjs';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const STATIC_DIR = path.join(ROOT, 'internal', 'server', 'static');
const e2eRequire = createRequire(path.join(ROOT, 'test', 'e2e', 'package.json'));
const { ESLint, Linter } = e2eRequire('eslint');
const espree = e2eRequire('espree');

// sw.js is a service worker with its own scope; contract.js is generated (and
// ignored by eslint), though it is a module like the rest.
const NOT_MODULES = new Set(['sw.js', 'contract.js']);

export async function effectiveConfigs(configFile) {
  const eslint = new ESLint({ cwd: ROOT, overrideConfigFile: configFile });
  const files = fs.readdirSync(STATIC_DIR).filter((f) => f.endsWith('.js') && !NOT_MODULES.has(f)).sort();
  const out = {};
  for (const f of files) out[f] = await eslint.calculateConfigForFile(path.join(STATIC_DIR, f));
  return out;
}

// problems lists every file linted as a script, and every global a file gets
// beyond the shared browser list.
export function problems(configs, shared) {
  const want = new Set(Object.keys(shared));
  const out = [];
  for (const [f, cfg] of Object.entries(configs)) {
    const lo = cfg?.languageOptions ?? {};
    if (lo.sourceType !== 'module') out.push(`${f}: linted as ${lo.sourceType ?? 'nothing'}, not as a module`);
    const extra = Object.keys(lo.globals ?? {}).filter((g) => !want.has(g));
    if (extra.length) out.push(`${f}: globals beyond the shared browser list: ${extra.join(', ')}`);
  }
  return out;
}

test('every dashboard script is a module with only the shared globals', async () => {
  const config = (await import(path.join(ROOT, 'eslint.config.mjs'))).default;
  const shared = config.find((b) => b.files?.includes('internal/server/static/*.js')).languageOptions.globals;
  const configs = await effectiveConfigs(path.join(ROOT, 'eslint.config.mjs'));
  assert.ok(Object.keys(configs).length >= 25, `only ${Object.keys(configs).length} scripts found`);
  assert.deepEqual(problems(configs, shared), []);
});

// nz/no-module-side-effects applies to every file except sw.js, contract.js
// (generated, parsed as a script) and caps.sideEffectLegacy (S19-0, #3025).
// The legacy list may only shrink — a file that is already clean has to move
// out, so this also actually lints every legacy file and requires it to
// still be dirty, or the list has gone stale. Membership is per file: a
// legacy file may gain more side effects without failing here (S19's
// migration PRs shrink the list; a per-file count is not ratcheted).
// legacyProblem judges one legacy file's lint messages. Only the rule's own
// findings prove the file dirty: a parse error (or any other rule's message)
// would otherwise keep a clean file on the list forever.
export function legacyProblem(f, messages) {
  const fatal = messages.filter((m) => m.fatal);
  if (fatal.length) return `${f}: does not parse: ${fatal.map((m) => m.message).join('; ')}`;
  if (!messages.some((m) => m.ruleId === 'nz/no-module-side-effects')) {
    return `${f}: in caps.sideEffectLegacy but clean — move it out`;
  }
  return null;
}

test('legacyProblem: only the rule\'s own finding keeps a file on the legacy list', () => {
  assert.equal(legacyProblem('a.js', [{ ruleId: 'nz/no-module-side-effects', message: 'x' }]), null);
  assert.match(legacyProblem('a.js', []), /clean — move it out/);
  assert.match(legacyProblem('a.js', [{ ruleId: 'no-undef', message: 'x' }]), /clean — move it out/);
  assert.match(legacyProblem('a.js', [{ ruleId: null, fatal: true, message: 'Parsing error: x' }]), /does not parse: Parsing error/);
});

test('nz/no-module-side-effects covers every file but the legacy list, which is still genuinely dirty', async () => {
  const { caps, errors } = loadCaps();
  assert.equal(errors, undefined, errors);
  const legacy = new Set(caps.sideEffectLegacy);
  const configs = await effectiveConfigs(path.join(ROOT, 'eslint.config.mjs'));
  const enabledIn = (cfg) => {
    const r = cfg?.rules?.['nz/no-module-side-effects'];
    return Array.isArray(r) ? r[0] === 2 || r[0] === 'error' : r === 2 || r === 'error';
  };
  const problems = [];
  for (const [f, cfg] of Object.entries(configs)) {
    if (f === 'contract.js') continue; // generated, parsed as a script — see NOT_MODULES
    const want = !legacy.has(f);
    if (enabledIn(cfg) !== want) {
      problems.push(`${f}: nz/no-module-side-effects enabled=${enabledIn(cfg)}, want ${want} (legacy=${legacy.has(f)})`);
    }
  }
  const linter = new Linter();
  for (const f of legacy) {
    const src = fs.readFileSync(path.join(STATIC_DIR, f), 'utf8');
    const messages = linter.verify(src, {
      languageOptions: { ecmaVersion: 'latest', sourceType: 'module' },
      plugins: { nz },
      rules: { 'nz/no-module-side-effects': 'error' },
    }, { filename: path.join(STATIC_DIR, f) });
    const p = legacyProblem(f, messages);
    if (p) problems.push(p);
  }
  assert.deepEqual(problems, []);
});

// nzInlineOverrides lists every inline comment in src that switches an nz/*
// rule off: an `eslint-disable[-line|-next-line]` naming an nz/ rule or
// naming no rule at all (a blanket disable covers nz/* too), and an
// `/* eslint nz/…: … */` rule-config comment. The config-level scope above
// (and caps.sideEffectLegacy's shrink-only list) would otherwise be one
// comment away from moot.
export function nzInlineOverrides(src) {
  let comments;
  try {
    comments = espree.parse(src, { ecmaVersion: 'latest', sourceType: 'module', comment: true, loc: true }).comments;
  } catch {
    comments = espree.parse(src, { ecmaVersion: 'latest', sourceType: 'script', comment: true, loc: true }).comments;
  }
  const out = [];
  for (const c of comments) {
    const text = c.value.trim().split(/\s--\s/)[0].trim();
    const m = /^(eslint-disable(?:-next-line|-line)?|eslint)(?:\s+([\s\S]*))?$/.exec(text);
    if (!m) continue;
    const rules = (m[2] ?? '').trim();
    if (m[1] === 'eslint' ? /\bnz\//.test(rules) : rules === '' || /(^|[\s,])nz\//.test(rules)) {
      out.push(`line ${c.loc.start.line}: ${text}`);
    }
  }
  return out;
}

test('no static script switches an nz/* rule off inline', () => {
  const problems = [];
  for (const f of fs.readdirSync(STATIC_DIR).filter((x) => x.endsWith('.js')).sort()) {
    for (const p of nzInlineOverrides(fs.readFileSync(path.join(STATIC_DIR, f), 'utf8'))) problems.push(`${f} ${p}`);
  }
  assert.deepEqual(problems, []);
});

test('nzInlineOverrides names nz/* and blanket disables, not other rules', () => {
  const flagged = [
    '// eslint-disable-next-line nz/no-module-side-effects\nf();',
    'f(); // eslint-disable-line nz/configure-deps -- because',
    '/* eslint-disable no-console, nz/no-exported-let */',
    '/* eslint-disable */',
    '// eslint-disable-next-line',
    '/* eslint nz/no-module-side-effects: off */',
    "/* eslint nz/no-module-side-effects: 'off' */",
  ];
  for (const src of flagged) assert.equal(nzInlineOverrides(src).length, 1, src);
  assert.match(nzInlineOverrides('f();\n// eslint-disable-next-line nz/x\ng();')[0], /^line 2: /);
  const clean = [
    '// eslint-disable-next-line no-console\nf();',
    '/* eslint-disable no-unused-vars -- mentions nz/ only in the description */',
    '/* eslint no-console: off */',
    "// a comment about nz/no-module-side-effects",
    "const s = '// eslint-disable-next-line nz/x';",
  ];
  for (const src of clean) assert.deepEqual(nzInlineOverrides(src), [], src);
});

test('problems names a script-mode file and an extra global', () => {
  const shared = { window: 'readonly' };
  const got = problems({
    'a.js': { languageOptions: { sourceType: 'module', globals: { window: 'readonly' } } },
    'b.js': { languageOptions: { sourceType: 'script', globals: { window: 'readonly' } } },
    'c.js': { languageOptions: { sourceType: 'module', globals: { window: 'readonly', selectedKey: 'writable' } } },
    'd.js': undefined,
  }, shared);
  assert.equal(got.length, 3);
  assert.match(got[0], /b\.js: linted as script/);
  assert.match(got[1], /c\.js: globals beyond .*selectedKey/);
  assert.match(got[2], /d\.js: linted as nothing/);
});
