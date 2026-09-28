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

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const STATIC_DIR = path.join(ROOT, 'internal', 'server', 'static');
const { ESLint } = createRequire(path.join(ROOT, 'test', 'e2e', 'package.json'))('eslint');

// sw.js is a service worker with its own scope; contract.js is the generated
// classic script the modules read NZ_CONTRACT from.
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
