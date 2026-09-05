#!/usr/bin/env node
// js-deps-freeze.mjs — freeze the implicit cross-file dependency graph of the
// dashboard's classic <script defer> files (internal/server/static/*.js).
//
// The six files share one global scope; the only "dependency declaration" is
// the <script> order in dashboard.html. This script makes that graph explicit
// and enforces that it only ever shrinks:
//
//   node scripts/js-deps-freeze.mjs --check    # compare against baseline (CI)
//   node scripts/js-deps-freeze.mjs --write    # regenerate the baseline
//   node scripts/js-deps-freeze.mjs --globals  # print per-file eslint globals
//
// Method (matches the arch-review-20260905 audit): a file's top-level names
// are its column-0 function/let/const/var/class declarations plus its
// `window.X =` exports; a bare reference is that name appearing in ANOTHER
// file without a leading `.` — string contents count (onclick="fn()" wiring),
// comments do not.
//
// --check fails on: any new bare reference (matrix growth), any new
// reverse-order let/const reference (TDZ hazard, the PR#1954 shape), and any
// typeof-guard count increase. Orphan references (#2184 shape — a name no
// file defines) are eslint no-undef's job, not this script's. Entries that
// disappear are reported as stale with a hint to rerun --write, but do not
// fail the check.

import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const STATIC_DIR = path.join(ROOT, 'internal', 'server', 'static');
const BASELINE_PATH = path.join(ROOT, 'scripts', 'js-deps-baseline.json');

// <script defer> order from dashboard.html — the de-facto dependency order.
// sw.js is a service worker with its own scope and is deliberately excluded.
const LOAD_ORDER = [
  'nz_util.js',
  'dashboard.js',
  'cron_view.js',
  'agent_view.js',
  'asset_browser.js',
  'files_view.js',
];

// ── source scanning ────────────────────────────────────────────────────────

// Strip // and /* */ comments while preserving string and template-literal
// contents (inline onclick="fn()" handlers live inside strings and must keep
// counting as references). Regex literals are not tracked; a `//` inside one
// drops the rest of that line, which at worst under-counts — never a false
// growth failure.
function stripComments(src) {
  let out = '';
  let i = 0;
  const n = src.length;
  let mode = 'code'; // code | line | block | squote | dquote | template
  while (i < n) {
    const c = src[i];
    const next = src[i + 1];
    if (mode === 'code') {
      if (c === '/' && next === '/') {
        mode = 'line';
        i += 2;
        continue;
      }
      if (c === '/' && next === '*') {
        mode = 'block';
        i += 2;
        continue;
      }
      if (c === "'") mode = 'squote';
      else if (c === '"') mode = 'dquote';
      else if (c === '`') mode = 'template';
      out += c;
      i++;
    } else if (mode === 'line') {
      if (c === '\n') {
        mode = 'code';
        out += c;
      }
      i++;
    } else if (mode === 'block') {
      if (c === '*' && next === '/') {
        mode = 'code';
        i += 2;
        continue;
      }
      if (c === '\n') out += c; // keep line numbers stable
      i++;
    } else {
      // inside a string/template: copy verbatim, honour escapes
      if (c === '\\') {
        out += c + (next ?? '');
        i += 2;
        continue;
      }
      if (
        (mode === 'squote' && c === "'") ||
        (mode === 'dquote' && c === '"') ||
        (mode === 'template' && c === '`')
      ) {
        mode = 'code';
      }
      out += c;
      i++;
    }
  }
  return out;
}

// Column-0 declarations. The static files only use single-name declarators at
// the top level (verified: no `let a, b` / destructuring at column 0).
function topLevelDecls(stripped) {
  const decls = new Map(); // name -> kind
  const re = /^(?:(async\s+)?function\s+([A-Za-z_$][\w$]*)|(let|const|var)\s+([A-Za-z_$][\w$]*)|class\s+([A-Za-z_$][\w$]*))/gm;
  for (const m of stripped.matchAll(re)) {
    if (m[2]) decls.set(m[2], 'function');
    else if (m[4]) decls.set(m[4], m[3]);
    else if (m[5]) decls.set(m[5], 'class');
  }
  return decls;
}

function windowExports(stripped) {
  const names = new Set();
  const re = /window\.([A-Za-z_$][\w$]*)\s*=(?!=)/g;
  for (const m of stripped.matchAll(re)) names.add(m[1]);
  return names;
}

// All identifiers not preceded by `.` (property access / optional chaining).
function bareIdents(stripped) {
  const idents = new Set();
  const re = /[A-Za-z_$][\w$]*/g;
  for (const m of stripped.matchAll(re)) {
    const prev = stripped[m.index - 1];
    if (prev === '.' || prev === '$') continue;
    idents.add(m[0]);
  }
  return idents;
}

function typeofGuardCount(stripped) {
  return [...stripped.matchAll(/typeof\s+[A-Za-z_$][\w$]*\s*[=!]==?\s*['"]function['"]/g)].length;
}

// ── analysis ───────────────────────────────────────────────────────────────

function analyze() {
  const files = {};
  for (const name of LOAD_ORDER) {
    const stripped = stripComments(
      fs.readFileSync(path.join(STATIC_DIR, name), 'utf8')
    );
    files[name] = {
      decls: topLevelDecls(stripped),
      exports: windowExports(stripped),
      idents: bareIdents(stripped),
      typeofGuards: typeofGuardCount(stripped),
    };
  }

  // referencing file -> defining file -> sorted names
  const matrix = {};
  const tdz = {}; // "referencer -> definer" -> let/const names (referencer loads first)
  const orphans = {}; // file -> names it references that NO file defines… filled below
  for (const from of LOAD_ORDER) {
    for (const to of LOAD_ORDER) {
      if (from === to) continue;
      const defined = new Set([...files[to].decls.keys(), ...files[to].exports]);
      // A name the referencing file itself declares or exports resolves to its
      // own binding — that is not a cross-file dependency.
      const hits = [...files[from].idents]
        .filter((id) => defined.has(id))
        .filter((id) => !files[from].decls.has(id) && !files[from].exports.has(id))
        .sort();
      if (hits.length === 0) continue;
      (matrix[from] ??= {})[to] = hits;
      if (LOAD_ORDER.indexOf(from) < LOAD_ORDER.indexOf(to)) {
        const hazard = hits.filter((id) => {
          const kind = files[to].decls.get(id);
          return kind === 'let' || kind === 'const';
        });
        if (hazard.length) (tdz[from] ??= {})[to] = hazard;
      }
    }
  }
  return { files, matrix, tdz };
}

function buildSnapshot({ files, matrix, tdz }) {
  const typeofGuards = {};
  for (const name of LOAD_ORDER) typeofGuards[name] = files[name].typeofGuards;
  return { matrix, tdz, typeofGuards };
}

// eslint per-file globals: everything this file references that another file
// defines (top-level decl or window export), i.e. its row of the matrix.
function eslintGlobals(matrix) {
  const out = {};
  for (const from of LOAD_ORDER) {
    const names = new Set();
    for (const to of Object.keys(matrix[from] ?? {})) {
      for (const n of matrix[from][to]) names.add(n);
    }
    out[from] = [...names].sort();
  }
  return out;
}

// ── check / write ──────────────────────────────────────────────────────────

function flatten(obj) {
  // {from: {to: [names]}} -> Set("from|to|name")
  const set = new Set();
  for (const from of Object.keys(obj ?? {})) {
    for (const to of Object.keys(obj[from])) {
      for (const n of obj[from][to]) set.add(`${from} -> ${to}: ${n}`);
    }
  }
  return set;
}

function check(current) {
  if (!fs.existsSync(BASELINE_PATH)) {
    console.error(`missing baseline ${path.relative(ROOT, BASELINE_PATH)}; run --write first`);
    return 1;
  }
  const baseline = JSON.parse(fs.readFileSync(BASELINE_PATH, 'utf8'));
  let failures = 0;
  let stale = 0;

  for (const [label, cur, base] of [
    ['cross-file bare reference', flatten(current.matrix), flatten(baseline.matrix)],
    ['TDZ-hazard reverse reference', flatten(current.tdz), flatten(baseline.tdz)],
  ]) {
    for (const entry of cur) {
      if (!base.has(entry)) {
        console.error(`NEW ${label}: ${entry}`);
        failures++;
      }
    }
    for (const entry of base) {
      if (!cur.has(entry)) {
        console.log(`stale baseline entry (${label}): ${entry}`);
        stale++;
      }
    }
  }

  for (const file of LOAD_ORDER) {
    const cur = current.typeofGuards[file] ?? 0;
    const base = baseline.typeofGuards?.[file] ?? 0;
    if (cur > base) {
      console.error(`typeof-guard count grew in ${file}: ${base} -> ${cur}`);
      failures++;
    } else if (cur < base) {
      console.log(`typeof-guard count dropped in ${file}: ${base} -> ${cur} (refresh with --write)`);
      stale++;
    }
  }

  if (stale) console.log(`${stale} stale baseline entr${stale === 1 ? 'y' : 'ies'}; run --write to shrink the baseline`);
  if (failures) {
    console.error(`js-deps-freeze: ${failures} new implicit cross-file dependenc${failures === 1 ? 'y' : 'ies'} (baseline is only allowed to shrink)`);
    return 1;
  }
  console.log('js-deps-freeze: OK');
  return 0;
}

const analysis = analyze();
const snapshot = buildSnapshot(analysis);
const mode = process.argv[2] ?? '--check';

if (mode === '--write') {
  fs.writeFileSync(BASELINE_PATH, JSON.stringify(snapshot, null, 2) + '\n');
  console.log(`wrote ${path.relative(ROOT, BASELINE_PATH)}`);
} else if (mode === '--globals') {
  console.log(JSON.stringify(eslintGlobals(snapshot.matrix), null, 2));
} else if (mode === '--check') {
  process.exit(check(snapshot));
} else {
  console.error(`unknown mode ${mode}; use --check | --write | --globals`);
  process.exit(2);
}
