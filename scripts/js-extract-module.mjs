#!/usr/bin/env node
// js-extract-module.mjs — move one or more dashboard.js regions into their own
// ES module (#2558 D4), computing the module boundary from real scope analysis
// instead of regex guesswork.
//
//   node scripts/js-extract-module.mjs --out voice --regions "Voice recording,Touch handlers" --dry-run
//   node scripts/js-extract-module.mjs --out voice --regions "Voice recording,Touch handlers"
//
// Why a tool: the first three D4 batches showed that hand-maintained
// needs/exposed/writes lists are unreliable (a 436-line region had seven
// dependencies the region scan missed), and iterating with per-name regex
// patches corrupts the import/export blocks. This computes the three sets with
// espree + eslint-scope and writes every file exactly once.
//
// What it does:
//   1. Splits dashboard.js at the region-comment boundaries (the same `// ---`
//      / `/* =====` markers the file already carries).
//   2. Scope-analyses the moved text and the remainder to derive:
//        needs    — names the moved code reads that stay in dashboard
//        exposed  — names the moved code declares that dashboard still reads
//        writes   — of `needs`, the reassignable ones the moved code assigns
//   3. Classifies `needs` into nz.state reads (mutable dashboard state, listed
//      in STATE below), nz_util imports, and injected deps (everything else).
//   4. Emits the module (header + deps + configureX + body + export block) and
//      rewrites dashboard.js with the import block, the configureX call and the
//      moved text removed — one atomic write per file.
//
// It does NOT touch Go embeds/routes, dashboard.html, baselines or contract
// tests; --dry-run prints the plan so those follow-ups are known up front.

import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';
import { createRequire } from 'node:module';

// espree + eslint-scope come from the e2e install (the same place the lint
// gate's eslint lives). This is a dev-only tool, never part of CI's default
// path, so borrowing that node_modules is fine — scripts/js-*.mjs gates stay
// dependency-free.
const require_ = createRequire(import.meta.url);
const espree = require_('../test/e2e/node_modules/espree/dist/espree.cjs');
const eslintScope = require_('../test/e2e/node_modules/eslint-scope/lib/index.js');

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const STATIC_DIR = path.join(ROOT, 'internal', 'server', 'static');
const DASH = path.join(STATIC_DIR, 'dashboard.js');

// Mutable dashboard state exposed through nz.state accessors. Read from
// dashboard.js itself rather than hard-coded: the set grows every time a batch
// promotes another shared binding, and a stale copy here would silently
// classify a state read as an injected dep (which then can't be written).
function readStateNames(src) {
  const block = /Object\.defineProperties\(nzState, \{\n([\s\S]*?)\n\}\);/.exec(src);
  const out = new Map(); // name -> hasSetter
  for (const line of (block?.[1] ?? '').split('\n')) {
    const m = /^\s*([A-Za-z_$][\w$]*): \{(.*)\}/.exec(line);
    if (m) out.set(m[1], m[2].includes('set:'));
  }
  return out;
}

// Names nz_util exports — a moved region referencing these imports them
// directly instead of taking an injected dep.
const NZ_UTIL = new Set([
  'esc', 'escAttr', 'fetchJSON', 'showToast', 'trapFocus', 'nzState', 'nzBus',
  'nzViews', 'nzTest', 'nzActions', 'registerActions', 'formatCostUSD',
  'formatDurationShort', 'formatRunDuration', 'isCronSessionKey',
]);

// stripExports removes `export { … };` re-export blocks before parsing. The
// remainder of dashboard.js re-exports names that live in other modules, so a
// module-mode parse of the leftover text alone would fail on the undefined
// export binding — the block carries no scope information we need anyway.
function stripExportBlocks(src) {
  return src.replace(/^export \{[\s\S]*?\};$/gm, '');
}

function parse(src) {
  return espree.parse(stripExportBlocks(src), {
    ecmaVersion: 2022,
    sourceType: 'module',
    loc: true,
    range: true,
  });
}

// topLevelNames returns the module-scope bindings a source declares.
function topLevelNames(src) {
  const ast = parse(src);
  const scopeManager = eslintScope.analyze(ast, { ecmaVersion: 2022, sourceType: 'module' });
  const names = new Map(); // name -> 'let' | 'const' | 'var' | 'function' | 'class'
  for (const v of scopeManager.globalScope.childScopes[0]?.variables ?? scopeManager.globalScope.variables) {
    const def = v.defs[0];
    if (!def) continue;
    let kind = def.type === 'FunctionName' ? 'function'
      : def.type === 'ClassName' ? 'class'
        : def.parent?.kind ?? 'var';
    names.set(v.name, kind);
  }
  return names;
}

// freeNames returns every identifier a source references but does not declare
// anywhere in its own scope chain (i.e. its unresolved references), plus the
// subset it assigns to.
function freeNames(src) {
  const ast = parse(src);
  const scopeManager = eslintScope.analyze(ast, { ecmaVersion: 2022, sourceType: 'module' });
  const reads = new Set();
  const writes = new Set();
  const visit = (scope) => {
    for (const ref of scope.through) {
      reads.add(ref.identifier.name);
      if (ref.isWrite()) writes.add(ref.identifier.name);
    }
  };
  // `through` on the module scope already aggregates unresolved refs from every
  // nested scope, but walk all scopes so a stray `var` in a block still counts.
  visit(scopeManager.globalScope);
  for (const s of scopeManager.globalScope.childScopes) visit(s);
  return { reads, writes };
}

// regions splits dashboard.js on its region-comment markers.
function regions(lines) {
  const marks = [];
  lines.forEach((l, i) => {
    if (/^(\/\/|\/\*) (-{3,}|={5})/.test(l)) marks.push({ line: i + 1, label: l });
  });
  return marks.map((m, i) => ({
    label: m.label,
    start: m.line,
    end: i + 1 < marks.length ? marks[i + 1].line - 1 : lines.length,
  }));
}

function fail(msg) {
  console.error('js-extract-module: ' + msg);
  process.exit(1);
}

// ── CLI ────────────────────────────────────────────────────────────────────

const argv = process.argv.slice(2);
const getArg = (name) => {
  const i = argv.indexOf('--' + name);
  return i >= 0 ? argv[i + 1] : null;
};
const out = getArg('out');
const regionArg = getArg('regions');
const dryRun = argv.includes('--dry-run');
if (!out || !regionArg) fail('usage: --out <module-name> --regions "<marker>,<marker>" [--dry-run]');

const dashSrc = fs.readFileSync(DASH, 'utf8');
const STATE_ACCESSORS = readStateNames(dashSrc);
const STATE = new Set(STATE_ACCESSORS.keys());
const lines = dashSrc.split('\n');
const regs = regions(lines);
const markers = regionArg.split(',').map((s) => s.trim()).filter(Boolean);
const picked = markers.map((m) => {
  const hits = regs.filter((r) => r.label.includes(m));
  if (hits.length !== 1) fail(`marker ${JSON.stringify(m)} matched ${hits.length} regions`);
  return hits[0];
});
picked.sort((a, b) => a.start - b.start);

const movedLines = new Set();
for (const r of picked) for (let i = r.start; i <= r.end; i++) movedLines.add(i);
const movedText = picked.map((r) => lines.slice(r.start - 1, r.end).join('\n')).join('\n\n');
const restText = lines.filter((_, i) => !movedLines.has(i + 1)).join('\n');

// Scope analysis. The moved text alone is not a valid module (it references
// dashboard names freely) but espree parses it fine — free references are
// exactly what we want.
const movedDecls = topLevelNames(movedText);
const { reads: movedFree, writes: movedWrites } = freeNames(movedText);
const restDecls = topLevelNames(restText);
const { reads: restFree, writes: restWrites } = freeNames(restText);

// A "need" is a free name in the moved text that the remainder declares.
const needs = [...movedFree].filter((n) => restDecls.has(n)).sort();
// "exposed" is a moved declaration the remainder still references.
const exposed = [...movedDecls.keys()].filter((n) => restFree.has(n)).sort();
// …and of those, the ones dashboard ASSIGNS. An import binding is read-only,
// so leaving such a name in the module makes dashboard throw "Assignment to
// constant variable" at runtime — silent in lint, and in the browser it only
// shows up as the surrounding feature quietly not working (the first attempt
// at moving Message navigation broke session switching this way: 26 e2e specs
// failed on navUserEls / navIdx / _lastAppliedMainState).
// nz.state-backed names are excluded: the module reads/writes them through
// the accessor, so dashboard keeps the binding and both sides stay in sync.
const exposedWrittenByDash = exposed.filter((n) => restWrites.has(n) && !STATE.has(n));
// Writes that land on a dashboard binding.
const writesToDash = needs.filter((n) => movedWrites.has(n));

// Late-bound hooks: dashboard declares the binding (`let x = null;`) but the
// moved region is what ASSIGNS it — dashboard only reads it later, at event
// time. Those belong to the module: it declares and exports them, and
// dashboard's declaration goes away. Detected as "the region writes it AND
// dashboard's declaration is a bare `let <name> = null;`".
const lateBound = writesToDash.filter((n) => !STATE.has(n)
  && restDecls.get(n) === 'let'
  && new RegExp(`^let ${n} = null;$`, 'm').test(restText));

const fromUtil = needs.filter((n) => NZ_UTIL.has(n)).sort();
const fromState = needs.filter((n) => STATE.has(n)).sort();
const asDeps = needs.filter((n) => !NZ_UTIL.has(n) && !STATE.has(n) && !lateBound.includes(n)).sort();
const stateWrites = writesToDash.filter((n) => STATE.has(n));
const otherWrites = writesToDash.filter((n) => !STATE.has(n) && !lateBound.includes(n));

const pascal = out.split('_').map((w) => w[0].toUpperCase() + w.slice(1)).join('');
const configureName = 'configure' + pascal;

console.log(`# extract ${out}.js`);
for (const r of picked) console.log(`  region ${r.start}-${r.end} (${r.end - r.start + 1} lines) ${r.label.slice(0, 56)}`);
console.log(`  moved lines: ${movedLines.size}`);
console.log(`  nz_util imports (${fromUtil.length}): ${fromUtil.join(', ') || '-'}`);
console.log(`  nz.state reads (${fromState.length}): ${fromState.join(', ') || '-'}`);
console.log(`  injected deps  (${asDeps.length}): ${asDeps.join(', ') || '-'}`);
console.log(`  exports back   (${exposed.length}): ${exposed.join(', ') || '-'}`);
if (lateBound.length) console.log(`  late-bound hooks moved with the region (${lateBound.length}): ${lateBound.join(', ')}`);
if (stateWrites.length) console.log(`  ! nz.state SETTERS required in dashboard.js: ${stateWrites.join(', ')}`);
if (otherWrites.length) console.log(`  ! writes a non-state dashboard binding (NOT movable as-is): ${otherWrites.join(', ')}`);

// nz.state accessor check: a state read only works if dashboard actually
// registers a getter (and a state write needs a setter). The first D4-4 batch
// shipped a read of `nodesData` with no accessor — every fetchSessions threw
// and the sidebar silently kept its skeleton, which only the 15 s e2e timeout
// surfaced. Fail early instead.
{
  const declared = STATE_ACCESSORS;
  const missing = fromState.filter((n) => !declared.has(n));
  const needSetter = stateWrites.filter((n) => declared.get(n) === false);
  if (missing.length) console.log(`  ! nz.state GETTERS missing in dashboard.js: ${missing.join(', ')}`);
  if (needSetter.length) console.log(`  ! nz.state accessors need a setter: ${needSetter.join(', ')}`);
  if (!dryRun && (missing.length || needSetter.length)) {
    fail('refusing to move: add the nz.state accessors listed above to dashboard.js first');
  }
}

if (exposedWrittenByDash.length) {
  console.log(`  ! dashboard ASSIGNS these moved names (import bindings are read-only): ${exposedWrittenByDash.join(', ')}`);
  if (!dryRun) fail('refusing to move: keep those declarations in dashboard (or promote them to nz.state with setters) and re-run');
}

if (otherWrites.length) fail('refusing to move: the region assigns a dashboard binding that is not nz.state-backed — promote it to nz.state (or move its owner too) first');

// ── emit ───────────────────────────────────────────────────────────────────

const rewriteRefs = (text) => {
  // Prefix state reads and injected deps. Word-boundary replace with a
  // property-access guard; the moved text is JS, so a preceding `.` or `$`
  // means it is already a member access.
  let t = text;
  for (const n of fromState) t = t.replace(new RegExp(`(?<![.\\w$])${n}(?![\\w$])`, 'g'), `nzState.${n}`);
  for (const n of asDeps) t = t.replace(new RegExp(`(?<![.\\w$])${n}(?![\\w$])`, 'g'), `deps.${n}`);
  return t;
};

const utilImports = new Set(fromUtil);
if (fromState.length) utilImports.add('nzState');
const header = `// ${out}.js — extracted from dashboard.js (#2558 D4).
//
// Verbatim region move: \`git diff --color-moved\` shows the body as a pure
// move; the import block, the deps table and the export block below are the
// only additions.
//
// Layering (D4-1 rule): a module dashboard imports must NOT import dashboard
// back — that cycle puts dashboard's own top-level consts in TDZ while this
// module evaluates. Dashboard state is read through nz.state; its helpers are
// injected once via ${configureName}(), called from dashboard's module body.
${utilImports.size ? `import { ${[...utilImports].sort().join(', ')} } from './nz_util.js';\n` : ''}`;

const depsBlock = asDeps.length
  ? `
const deps = {
${asDeps.map((n) => `  ${n}: null,`).join('\n')}
};
export function ${configureName}(impl) {
  for (const k of Object.keys(deps)) {
    if (typeof impl[k] === 'undefined') throw new Error('${out} dep missing: ' + k);
    deps[k] = impl[k];
  }
}
`
  : '';

const allExports = [...new Set([...exposed, ...lateBound])].sort();
const lateBoundDecl = lateBound.length
  ? `
// Late-bound hooks: assigned by the code below, read by other modules at event
// time (never at load time) — the shape they had as dashboard module-scope
// lets before this extraction.
${lateBound.map((n) => `let ${n} = null;`).join('\n')}
`
  : '';

const exportBlock = allExports.length
  ? `
export {
${allExports.map((n) => `  ${n},`).join('\n')}
};
`
  : '';

const moduleSrc = header + depsBlock + lateBoundDecl + '\n' + rewriteRefs(movedText) + '\n' + exportBlock;

// dashboard.js: insert the import block after the last existing import, add the
// configureX call next to the other configure* calls, drop the moved lines and
// drop the moved names from dashboard's own export block (they live here now).
let newDash = restText;
const importNames = [...(asDeps.length ? [configureName] : []), ...allExports].sort();
const importBlock = `import {\n${importNames.map((n) => `  ${n},`).join('\n')}\n} from './${out}.js';\n`;
const lastImportEnd = (() => {
  const re = /^import [\s\S]*?from '\.\/[^']+';$/gm;
  let m, last = null;
  while ((m = re.exec(newDash))) last = m;
  if (!last) fail('no existing import block found in dashboard.js');
  return last.index + last[0].length + 1;
})();
newDash = newDash.slice(0, lastImportEnd) + importBlock + newDash.slice(lastImportEnd);

if (asDeps.length) {
  const callLine = `${configureName}({ ${asDeps.join(', ')} });\n`;
  const anchor = /^configure[A-Za-z]+\(\{/m.exec(newDash);
  if (!anchor) fail('no existing configure* call site found in dashboard.js');
  newDash = newDash.slice(0, anchor.index) + callLine + newDash.slice(anchor.index);
}

// The late-bound hooks now live in the module: drop dashboard's placeholder
// declarations (and the explanatory comment block above them, if any).
for (const n of lateBound) {
  newDash = newDash.replace(new RegExp(`^let ${n} = null;\\n`, 'm'), '');
}

// Remove moved names from dashboard's export block, if present there.
newDash = newDash.replace(/^export \{\n([\s\S]*?)\};$/m, (whole, body) => {
  const kept = body.split('\n').filter((l) => {
    const n = l.trim().replace(/,$/, '');
    return !movedDecls.has(n);
  });
  return `export {\n${kept.join('\n')}\n};`;
});

if (dryRun) {
  console.log(`\n(dry run) would write internal/server/static/${out}.js (${moduleSrc.split('\n').length} lines)`);
  console.log(`(dry run) dashboard.js ${lines.length} -> ${newDash.split('\n').length} lines`);
  console.log(`\nfollow-ups this tool does NOT do:`);
  console.log(`  - internal/server/static_assets.go: embed + asset table + handler`);
  console.log(`  - internal/server/routes.go: GET /static/${out}.js route (+ lint baseline)`);
  console.log(`  - internal/server/routes_snapshot_test.go: handler name, then UPDATE_GOLDEN=1`);
  console.log(`  - dashboard.html: <script type="module"> tag`);
  console.log(`  - scripts/js-*.mjs baselines (--write), Playwright + Go contract tests`);
  process.exit(0);
}

fs.writeFileSync(path.join(STATIC_DIR, `${out}.js`), moduleSrc);
fs.writeFileSync(DASH, newDash);

// eslint.config.mjs: register the new file as a module with an empty
// cross-file whitelist (imports make its dependencies explicit).
const CFG = path.join(ROOT, 'eslint.config.mjs');
let cfg = fs.readFileSync(CFG, 'utf8');
if (!cfg.includes(`'${out}.js': {}`)) {
  cfg = cfg.replace("  'dashboard.js': {},", `  '${out}.js': {},\n  'dashboard.js': {},`);
  cfg = cfg.replace("const moduleFiles = new Set(['nz_util.js',", `const moduleFiles = new Set(['nz_util.js', '${out}.js',`);
  fs.writeFileSync(CFG, cfg);
}
console.log(`\nwrote internal/server/static/${out}.js, rewrote dashboard.js, registered in eslint.config.mjs`);
