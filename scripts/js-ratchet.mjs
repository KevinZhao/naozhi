#!/usr/bin/env node
// js-ratchet.mjs — growth ratchet for the dashboard's scripts
// (internal/server/static/*.js). Per file, four numbers may only go down:
//
//   lines           total line count
//   maxFnLines      longest function, of every form: declarations, expressions,
//                   arrows, object and class methods (nested ones counted on
//                   their own); a top-level IIFE is a module scope, not a function
//   fnOver100       count of such functions longer than 100 lines
//   topLevelLetVar  column-0 `let` / `var` declarations (mutable globals)
//   configureDeps   properties passed in `configureX({ ... })` calls: the
//                   dependencies a module receives by injection instead of
//                   import, invisible to the module graph (a spread counts 1)
//
// Function lengths come from espree, the parser eslint installs into
// test/e2e/node_modules (npm install there first). typeof-guard counts are
// ratcheted by scripts/js-deps-freeze.mjs already and are not duplicated here.
//
//   node scripts/js-ratchet.mjs --check   # CI gate
//   node scripts/js-ratchet.mjs --write   # ratchet the baseline down
//
// --check fails on any metric above its baseline (growth) AND on any metric
// below it (the improving PR must ship the tightened baseline — run --write).
// --write refuses to raise a value: deliberate growth requires hand-editing
// scripts/js-ratchet.baseline.json and an approved scripts/ratchet-raises.jsonl
// entry (tools/ratchet-raises), which also holds the sum of lines and
// fnOver100 and configureDeps and the maximum maxFnLines across files, so a
// new file cannot absorb growth.

import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const STATIC_DIR = path.join(ROOT, 'internal', 'server', 'static');
const BASELINE_PATH = path.join(ROOT, 'scripts', 'js-ratchet.baseline.json');

// sw.js is a 27-line service worker with its own scope; not worth ratcheting.
const EXCLUDE = new Set(['sw.js']);

function loadEspree() {
  const req = createRequire(path.join(ROOT, 'test', 'e2e', 'package.json'));
  try {
    return req('espree');
  } catch {
    console.error('js-ratchet: espree not found; run `npm install` in test/e2e');
    process.exit(2);
  }
}

// functions returns every function in the program with its line span, except
// top-level IIFEs, whose bodies are walked but which are not functions a
// reader has to hold in their head at once.
function functions(program) {
  const scopes = new Set();
  for (const st of program.body) {
    let e = st.type === 'ExpressionStatement' ? st.expression : null;
    if (e && e.type === 'UnaryExpression') e = e.argument;
    if (e && e.type === 'CallExpression' && /Function/.test(e.callee.type)) scopes.add(e.callee);
  }
  const out = [];
  const walk = (n) => {
    if (!n || typeof n.type !== 'string') return;
    if (/Function/.test(n.type) && !scopes.has(n)) {
      out.push({ start: n.loc.start.line, lines: n.loc.end.line - n.loc.start.line + 1 });
    }
    for (const [k, v] of Object.entries(n)) {
      if (k === 'loc' || k === 'range' || k === 'parent') continue;
      if (Array.isArray(v)) v.forEach(walk);
      else if (v && typeof v === 'object') walk(v);
    }
  };
  walk(program);
  return out;
}

// configureDeps counts the properties of the object literal handed to each
// configure[A-Z]… call.
function configureDeps(program) {
  let n = 0;
  const walk = (node) => {
    if (!node || typeof node.type !== 'string') return;
    if (node.type === 'CallExpression' && node.callee.type === 'Identifier' &&
        /^configure[A-Z]/.test(node.callee.name) && node.arguments[0]?.type === 'ObjectExpression') {
      n += node.arguments[0].properties.length;
    }
    for (const [k, v] of Object.entries(node)) {
      if (k === 'loc' || k === 'range') continue;
      if (Array.isArray(v)) v.forEach(walk);
      else if (v && typeof v === 'object') walk(v);
    }
  };
  walk(program);
  return n;
}

export function measureSource(src, espree = loadEspree()) {
  const lines = src.split('\n');
  const total = lines.length - (src.endsWith('\n') ? 1 : 0);
  const program = espree.parse(src, { ecmaVersion: 'latest', sourceType: 'module', loc: true });
  const fns = functions(program);
  let letVar = 0;
  for (const line of lines) if (/^(?:let|var)\s/.test(line)) letVar++;
  return {
    lines: total,
    maxFnLines: fns.reduce((m, f) => Math.max(m, f.lines), 0),
    fnOver100: fns.filter((f) => f.lines > 100).length,
    topLevelLetVar: letVar,
    configureDeps: configureDeps(program),
  };
}

function measure(file, espree) {
  return measureSource(fs.readFileSync(path.join(STATIC_DIR, file), 'utf8'), espree);
}

function snapshot() {
  const espree = loadEspree();
  const out = {};
  const files = fs
    .readdirSync(STATIC_DIR)
    .filter((f) => f.endsWith('.js') && !EXCLUDE.has(f))
    .sort();
  for (const f of files) out[f] = measure(f, espree);
  return out;
}

// compare returns every way current is out of step with baseline.
export function compare(current, baseline) {
  const out = [];
  for (const [file, metrics] of Object.entries(current)) {
    const base = baseline[file];
    if (!base) {
      out.push(`${file}: not in baseline — run --write to start tracking it`);
      continue;
    }
    for (const metric of Object.keys(base)) {
      if (!(metric in metrics)) out.push(`${file}: baseline metric ${metric} is no longer measured — run --write`);
    }
    for (const [metric, cur] of Object.entries(metrics)) {
      const b = base[metric];
      if (b === undefined) {
        // A metric the baseline does not hold would compare false both ways
        // and pass unchecked.
        out.push(`${file}: ${metric} is not in the baseline — run --write to start tracking it`);
      } else if (cur > b) {
        out.push(`${file}: ${metric} grew ${b} -> ${cur} (ratchet: may only shrink)`);
      } else if (cur < b) {
        out.push(`${file}: ${metric} improved ${b} -> ${cur} — ship the tightened baseline (run scripts/js-ratchet.mjs --write)`);
      }
    }
  }
  for (const file of Object.keys(baseline)) {
    if (!current[file]) out.push(`${file}: in baseline but gone from static/ — run --write`);
  }
  return out;
}

function check(current) {
  if (!fs.existsSync(BASELINE_PATH)) {
    console.error(`missing baseline ${path.relative(ROOT, BASELINE_PATH)}; run --write first`);
    return 1;
  }
  const problems = compare(current, JSON.parse(fs.readFileSync(BASELINE_PATH, 'utf8')));
  if (problems.length) {
    for (const p of problems) console.error(p);
    console.error(`js-ratchet: ${problems.length} metric(s) out of step with the baseline`);
    return 1;
  }
  console.log('js-ratchet: OK');
  return 0;
}

// raisedMetrics lists the values --write would raise; it refuses them.
export function raisedMetrics(current, baseline) {
  const out = [];
  for (const [file, metrics] of Object.entries(current)) {
    for (const [metric, cur] of Object.entries(metrics)) {
      const b = baseline[file]?.[metric];
      if (b !== undefined && cur > b) out.push(`${file} ${metric} ${b} -> ${cur}`);
    }
  }
  return out;
}

function write(current) {
  if (fs.existsSync(BASELINE_PATH)) {
    const raised = raisedMetrics(current, JSON.parse(fs.readFileSync(BASELINE_PATH, 'utf8')));
    if (raised.length) {
      for (const r of raised) {
        console.error(`refusing to raise ${r}; growth requires hand-editing ${path.relative(ROOT, BASELINE_PATH)} and an approved scripts/ratchet-raises.jsonl entry`);
      }
      return 1;
    }
  }
  fs.writeFileSync(BASELINE_PATH, JSON.stringify(current, null, 2) + '\n');
  console.log(`wrote ${path.relative(ROOT, BASELINE_PATH)}`);
  return 0;
}

// Run as a script; imported by the tests for measureSource.
if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const current = snapshot();
  const mode = process.argv[2] ?? '--check';
  if (mode === '--check') process.exit(check(current));
  else if (mode === '--write') process.exit(write(current));
  else {
    console.error(`unknown mode ${mode}; use --check | --write`);
    process.exit(2);
  }
}
