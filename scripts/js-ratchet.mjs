#!/usr/bin/env node
// js-ratchet.mjs — growth ratchet for the dashboard's scripts
// (internal/server/static/*.js). Per file, these numbers may only go down:
//
//   lines           total line count
//   maxFnLines      longest function, of every form: declarations, expressions,
//                   arrows, object and class methods (nested ones counted on
//                   their own); a top-level IIFE is a module scope, not a function
//   fnOver100       count of such functions longer than 100 lines
//   topLevelLetVar  column-0 `let` / `var` declarations (mutable globals)
//   configureDeps   dependencies the module receives by injection instead of
//                   import, counted where they land (S20a, #3026): the keys
//                   an exported function copies out of its parameter into
//                   module scope, whatever the function is called, plus one
//                   per shell.X upcall slot the module uses
//   deadInjections  of those keys, the ones the module never reads
//   innerHTMLAssign assignments to .innerHTML (any operator, ['innerHTML'] too)
//   htmlInsert      the other HTML-string sinks: outerHTML assignments,
//                   insertAdjacentHTML, setHTMLUnsafe, createContextualFragment,
//                   parseFromString, document.write / writeln
//   lateBindings    hooks.X / nzViews.X writes: functions published at load
//                   time for other modules to call, outside the import graph
//
// and across all files (the "_global" entry of the baseline):
//
//   htmlSinks          innerHTMLAssign + htmlInsert + the call sites of every
//                      function whose parameter reaches a sink (a wrapper does
//                      not hide the sink it wraps)
//   importCycles       strongly-connected components of the import graph
//   unresolvedImports  imports naming an export the target does not have (a
//                      link error: the whole page fails to load)
//   reexports          `export … from` relays
//   leafImports        imports from a caps.leaves module to a non-leaf
//   upcallForwarders   shell slots not backed by a root module's own code
//   upcallNotUp        shell.X uses that could have been a plain import
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
// entry (tools/ratchet-raises). That tool also holds the sums across files
// (lines, fnOver100, configureDeps, deadInjections, innerHTMLAssign,
// htmlInsert, lateBindings), the maximum maxFnLines and every _global metric,
// so a new file cannot absorb growth; lines and the injection / HTML /
// late-binding counts are judged only as sums there, since moving code
// between files is a refactor, not a raise (js-ratchet --check still holds
// every file's own values).

import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const STATIC_DIR = path.join(ROOT, 'internal', 'server', 'static');
const BASELINE_PATH = path.join(ROOT, 'scripts', 'js-ratchet.baseline.json');
const CAPS_PATH = path.join(ROOT, 'scripts', 'js-ratchet.caps.json');

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

// importsOf lists the relative module specifiers a file loads (its own
// import graph edges): every `import … from`, and every re-export
// (`export { x } from`, `export * from`), which loads and evaluates the
// target exactly like an import does — a cycle closed through a re-export is
// still a cycle. A dynamic import() runs later, not at evaluation, and is not
// an edge. Any other absolute or bare specifier would be a dependency outside
// static/, which is out of scope for the cycle check.
function importsOf(program) {
  const out = [];
  for (const st of program.body) {
    const loads = st.type === 'ImportDeclaration' || st.type === 'ExportAllDeclaration'
      || (st.type === 'ExportNamedDeclaration' && st.source);
    if (!loads) continue;
    const f = moduleFile(st.source.value);
    if (f) out.push(f);
  }
  return out;
}

// moduleFile maps a specifier to the static/ file it names: './x.js',
// '../x.js' and '/static/x.js' (the absolute form e2e-shim uses) are all
// x.js, so one module is one graph node however it is spelt. An absolute or
// bare specifier elsewhere is outside static/ and returns null.
function moduleFile(spec) {
  if (spec.startsWith('./') || spec.startsWith('../') || spec.startsWith('/static/')) return path.basename(spec);
  return null;
}

// ---- AST facts: HTML sinks, injections, late bindings, imports (S20a) ----

const PARSE = { ecmaVersion: 'latest', sourceType: 'module', loc: true };
const SKIP_KEYS = new Set(['loc', 'range', 'parent']);

// visit calls fn(node, parent, key) on every node under root, depth first.
function visit(root, fn, parent = null, key = null) {
  if (!root || typeof root.type !== 'string') return;
  fn(root, parent, key);
  for (const [k, v] of Object.entries(root)) {
    if (SKIP_KEYS.has(k)) continue;
    if (Array.isArray(v)) for (const c of v) visit(c, fn, root, k);
    else if (v && typeof v === 'object') visit(v, fn, root, k);
  }
}

const isFn = (n) => n?.type === 'FunctionDeclaration' || n?.type === 'FunctionExpression' || n?.type === 'ArrowFunctionExpression';

// propName is a member's static property name: x.p and x['p'] are both p.
function propName(m) {
  if (!m.computed) return m.property.type === 'Identifier' ? m.property.name : null;
  return m.property.type === 'Literal' ? String(m.property.value) : null;
}

// memberPath spells an Identifier / static member chain: a, a.b, a.b.c.
function memberPath(n) {
  if (n.type === 'Identifier') return n.name;
  if (n.type !== 'MemberExpression') return null;
  const p = propName(n);
  const o = memberPath(n.object);
  return p !== null && o !== null ? `${o}.${p}` : null;
}

function rootIdent(n) {
  while (n.type === 'MemberExpression') n = n.object;
  return n.type === 'Identifier' ? n.name : null;
}

// isReference: an Identifier that reads a binding, not a property name or
// an object key.
function isReference(n, parent, key) {
  if (n.type !== 'Identifier') return false;
  if (parent?.type === 'MemberExpression' && key === 'property' && !parent.computed) return false;
  if ((parent?.type === 'Property' || parent?.type === 'MethodDefinition') && key === 'key' && !parent.computed) return false;
  return true;
}

// mentions reports whether node reads any of names.
function mentions(node, names) {
  let hit = false;
  visit(node, (n, parent, key) => {
    if (!hit && isReference(n, parent, key) && names.has(n.name)) hit = true;
  });
  return hit;
}

function patternNames(p, out = []) {
  if (!p) return out;
  if (p.type === 'Identifier') out.push(p.name);
  else if (p.type === 'AssignmentPattern') patternNames(p.left, out);
  else if (p.type === 'RestElement') patternNames(p.argument, out);
  else if (p.type === 'ArrayPattern') p.elements.forEach((e) => patternNames(e, out));
  else if (p.type === 'ObjectPattern') p.properties.forEach((q) => patternNames(q.type === 'RestElement' ? q : q.value, out));
  return out;
}

// htmlSinkArgs returns the arguments of node that land in the DOM as HTML,
// or null when node is not a sink. kind says which metric it counts in.
// A Map, not an object literal: `p in {…}` would also match toString.
const HTML_CALL_ARG = new Map([['insertAdjacentHTML', 1], ['setHTMLUnsafe', 0], ['createContextualFragment', 0], ['parseFromString', 0]]);
function htmlSink(node) {
  if (node.type === 'AssignmentExpression' && node.left.type === 'MemberExpression') {
    const p = propName(node.left);
    if (p === 'innerHTML') return { kind: 'innerHTMLAssign', args: [node.right] };
    if (p === 'outerHTML') return { kind: 'htmlInsert', args: [node.right] };
  }
  if (node.type === 'CallExpression' && node.callee.type === 'MemberExpression') {
    const p = propName(node.callee);
    if (HTML_CALL_ARG.has(p)) return { kind: 'htmlInsert', args: node.arguments.slice(HTML_CALL_ARG.get(p), HTML_CALL_ARG.get(p) + 1) };
    if ((p === 'write' || p === 'writeln') && node.callee.object.type === 'Identifier' && node.callee.object.name === 'document') {
      return { kind: 'htmlInsert', args: node.arguments };
    }
  }
  return null;
}

// fnName names a function the way its callers reach it: a declaration or a
// `const f = …` by its binding, a method, an object property or a member
// assignment (hooks.f = …) by its key. Anything else is anonymous (null).
// bound says the name is a binding (a bare call f() reaches it) rather than
// a key (only o.f() does).
function fnName(fn, parent, key) {
  if (fn.type === 'FunctionDeclaration') return fn.id ? { name: fn.id.name, bound: true } : null;
  if (parent?.type === 'VariableDeclarator' && key === 'init' && parent.id.type === 'Identifier') return { name: parent.id.name, bound: true };
  if ((parent?.type === 'Property' || parent?.type === 'MethodDefinition') && key === 'value') {
    const name = propName({ computed: parent.computed, property: parent.key });
    return name === null ? null : { name, bound: false };
  }
  if (parent?.type === 'AssignmentExpression' && key === 'right' && parent.left.type === 'MemberExpression') {
    const name = propName(parent.left);
    return name === null ? null : { name, bound: false };
  }
  return null;
}

// LATE_BINDING_TABLES: the late-bound function tables, by the module that
// exports each (state.js hooks, nz_util.js nzViews).
const LATE_BINDING_TABLES = { hooks: 'state.js', nzViews: 'nz_util.js' };

// INJECTION_ALLOW: receiver functions that copy a parameter's fields into
// module scope on purpose and are not dependency injection. Each entry must
// still match a receiver: a stale one fails the check (rather than allowing
// whatever lands under that name next).
//   registerActions  the data-action registry (name -> click handler)
export const INJECTION_ALLOW = ['nz_util.js:registerActions'];

// SHELL_* — the upcall table (S20f): the root modules register their
// orchestration functions once with registerShell({...}) and lower modules
// call shell.X(...).
export const SHELL_MODULE = 'shell.js';
export const SHELL_ROOTS = ['cron_view.js', 'dashboard.js'];

// analyzeProgram collects everything the per-file metrics and the _global
// checks need from one module.
function analyzeProgram(program, file) {
  const facts = {
    file,
    imports: [], // { local, imported ('default' | '*' | name), from, line }
    exportNames: new Set(),
    starFrom: [],
    reexports: [], // line numbers
    top: new Map(), // top-level non-import binding -> declaration node (fn / declarator / class)
    fns: [], // { node, name, bound, paramNames: [Set per parameter] }
    calls: [], // { node, inFns: [fn nodes] }
    sinks: [], // { kind, args, line }
    lateBindings: 0,
    injections: new Map(), // "T.k" -> receiver function name
    allowHits: new Set(),
    deadInjections: [],
    shellUses: new Set(),
    shellRegs: [], // ObjectExpression args of registerShell(...)
  };
  const exportedLocal = new Set();
  for (const st of program.body) {
    if (st.type === 'ImportDeclaration') {
      const from = moduleFile(st.source.value);
      for (const sp of st.specifiers) {
        const imported = sp.type === 'ImportDefaultSpecifier' ? 'default'
          : sp.type === 'ImportNamespaceSpecifier' ? '*' : (sp.imported.name ?? sp.imported.value);
        facts.imports.push({ local: sp.local.name, imported, from, line: st.loc.start.line });
      }
      if (!st.specifiers.length) facts.imports.push({ local: null, imported: null, from, line: st.loc.start.line });
      continue;
    }
    if (st.type === 'ExportAllDeclaration') {
      facts.reexports.push(st.loc.start.line);
      if (st.exported) facts.exportNames.add(st.exported.name ?? st.exported.value);
      else facts.starFrom.push(moduleFile(st.source.value));
      continue;
    }
    if (st.type === 'ExportDefaultDeclaration') facts.exportNames.add('default');
    if (st.type === 'ExportNamedDeclaration') {
      for (const sp of st.specifiers) {
        facts.exportNames.add(sp.exported.name ?? sp.exported.value);
        if (st.source) {
          const imported = sp.local.name ?? sp.local.value;
          facts.imports.push({ local: null, imported, from: moduleFile(st.source.value), line: st.loc.start.line });
        } else exportedLocal.add(sp.local.name);
      }
      if (st.source) facts.reexports.push(st.loc.start.line);
    }
    const d = st.type === 'ExportNamedDeclaration' || st.type === 'ExportDefaultDeclaration' ? st.declaration : st;
    if (!d) continue;
    const exported = st.type === 'ExportNamedDeclaration'; // export default f() {} exports "default", not f
    if ((d.type === 'FunctionDeclaration' || d.type === 'ClassDeclaration') && d.id) {
      facts.top.set(d.id.name, d);
      if (exported) { facts.exportNames.add(d.id.name); exportedLocal.add(d.id.name); }
    }
    if (d.type === 'VariableDeclaration') {
      for (const decl of d.declarations) {
        for (const name of patternNames(decl.id)) {
          facts.top.set(name, decl);
          if (exported) { facts.exportNames.add(name); exportedLocal.add(name); }
        }
      }
    }
  }
  const localOf = (module, name) => facts.imports.filter((i) => i.from === module && i.imported === name).map((i) => i.local);
  const tables = new Set();
  for (const [name, module] of Object.entries(LATE_BINDING_TABLES)) {
    if (file === module) tables.add(name);
    for (const l of localOf(module, name)) tables.add(l);
  }
  const shellNames = new Set(localOf(SHELL_MODULE, 'shell'));
  const registerShellNames = new Set(localOf(SHELL_MODULE, 'registerShell'));

  // One walk: functions (with the chain of enclosing ones), calls, sinks,
  // late bindings, shell uses.
  const stack = [];
  const walk = (n, parent, key) => {
    if (!n || typeof n.type !== 'string') return;
    let pushed = false;
    if (isFn(n)) {
      const nm = fnName(n, parent, key);
      const paramNames = n.params.map((p) => new Set(patternNames(p)));
      facts.fns.push({ node: n, name: nm?.name ?? null, bound: nm?.bound ?? false, paramNames });
      stack.push(n);
      pushed = true;
    }
    const sink = htmlSink(n);
    if (sink) facts.sinks.push({ ...sink, line: n.loc.start.line, inFns: [...stack] });
    if (n.type === 'CallExpression') facts.calls.push({ node: n, inFns: [...stack] });
    if (n.type === 'AssignmentExpression' && n.left.type === 'MemberExpression' &&
        n.left.object.type === 'Identifier' && tables.has(n.left.object.name)) {
      facts.lateBindings += n.right.type === 'ObjectExpression' ? n.right.properties.length : 1;
    }
    if (n.type === 'CallExpression' && memberPath(n.callee) === 'Object.assign' &&
        n.arguments[0]?.type === 'Identifier' && tables.has(n.arguments[0].name)) {
      for (const a of n.arguments.slice(1)) facts.lateBindings += a.type === 'ObjectExpression' ? a.properties.length : 1;
    }
    if (n.type === 'MemberExpression' && n.object.type === 'Identifier' && shellNames.has(n.object.name)) {
      const p = propName(n);
      if (p !== null) facts.shellUses.add(p);
    }
    if (n.type === 'CallExpression' && n.callee.type === 'Identifier' && registerShellNames.has(n.callee.name)) {
      facts.shellRegs.push({ arg: n.arguments[0], line: n.loc.start.line });
    }
    for (const [k, v] of Object.entries(n)) {
      if (SKIP_KEYS.has(k)) continue;
      if (Array.isArray(v)) for (const c of v) walk(c, n, k);
      else if (v && typeof v === 'object') walk(v, n, k);
    }
    if (pushed) stack.pop();
  };
  walk(program, null, null);
  findInjections(program, facts, exportedLocal);
  return facts;
}

// findInjections: an exported function that copies its parameter's fields
// (p.x, p[k], a destructured field, Object.assign from p) into a module-scope
// binding is receiving injected dependencies, whatever it is called. So is
// one that stores a whole parameter in a binding the module then calls. The
// keys landed are counted (a whole-table copy counts the table's keys); a
// key the module never reads is dead.
function findInjections(program, facts, exportedLocal) {
  const receivers = [];
  for (const name of exportedLocal) {
    const d = facts.top.get(name);
    const fn = d?.type === 'FunctionDeclaration' ? d : (d?.type === 'VariableDeclarator' && isFn(d.init) ? d.init : null);
    if (fn && fn.params.length) receivers.push({ name, fn });
  }
  // Every static read of a module binding's member, and every call path.
  const reads = new Set();
  const calledPaths = new Set();
  visit(program, (n, parent, key) => {
    if (n.type === 'MemberExpression') {
      const writeTarget = parent?.type === 'AssignmentExpression' && key === 'left';
      const path = memberPath(n);
      if (path && !writeTarget) reads.add(path);
    }
    if (n.type === 'VariableDeclarator' && n.id.type === 'ObjectPattern' && n.init?.type === 'Identifier') {
      for (const q of n.id.properties) {
        if (q.type === 'Property' && !q.computed && q.key.type === 'Identifier') reads.add(`${n.init.name}.${q.key.name}`);
      }
    }
    if (isReference(n, parent, key) && !(parent?.type === 'AssignmentExpression' && key === 'left') &&
        !(parent?.type === 'VariableDeclarator' && key === 'id')) reads.add(n.name);
    if (n.type === 'CallExpression') {
      const path = memberPath(n.callee);
      if (path) calledPaths.add(path);
    }
  });
  const tableKeys = (t) => {
    const d = facts.top.get(t);
    const literal = d?.type === 'VariableDeclarator' && d.init?.type === 'ObjectExpression'
      ? d.init.properties.filter((q) => q.type === 'Property').map((q) => propName({ computed: q.computed, property: q.key }) ?? '?')
      : [];
    if (literal.length) return literal;
    const read = [...reads].filter((r) => r.startsWith(t + '.') && !r.slice(t.length + 1).includes('.')).map((r) => r.slice(t.length + 1));
    return read.length ? read : ['*']; // a registry with no static keys: one table
  };
  for (const { name, fn } of receivers) {
    const params = new Set(fn.params.flatMap((p) => p.type === 'Identifier' ? [p.name] : []));
    const fields = new Set(fn.params.flatMap((p) => p.type === 'Identifier' ? [] : patternNames(p)));
    const locals = new Set();
    visit(fn.body, (n) => { if (n.type === 'VariableDeclarator') patternNames(n.id).forEach((x) => locals.add(x)); });
    const readsField = (e) => {
      let hit = false;
      visit(e, (n, parent, key) => {
        if (hit) return;
        if (n.type === 'MemberExpression' && n.object.type === 'Identifier' && params.has(n.object.name)) hit = true;
        else if (isReference(n, parent, key) && fields.has(n.name)) hit = true;
      });
      return hit;
    };
    // A local assigned from a parameter's field carries that field.
    for (let i = 0; i < 2; i++) {
      visit(fn.body, (n) => {
        if (n.type === 'VariableDeclarator' && n.id.type === 'Identifier' && n.init && readsField(n.init)) fields.add(n.id.name);
      });
    }
    const keys = [];
    // A copy keyed by a runtime value (deps[k] = impl[k]) lands the whole
    // table. A copy to a static target (slot = x.f, deps.a = x.a, deps = x)
    // is injection when the module calls what landed there; otherwise it is
    // state taken from a data argument (turnState.tool = ev.tool).
    const land = (target) => {
      const t = rootIdent(target);
      if (!t || !facts.top.has(t) || locals.has(t) || params.has(t)) return;
      if (target.type === 'MemberExpression' && propName(target) === null) { keys.push(...tableKeys(t).map((k) => `${t}.${k}`)); return; }
      const path = memberPath(target);
      if (path && calledPaths.has(path)) keys.push(path);
      else if (target.type === 'Identifier' && [...calledPaths].some((c) => c.startsWith(path + '.'))) keys.push(...tableKeys(t).map((k) => `${t}.${k}`));
    };
    visit(fn.body, (n) => {
      if (n.type === 'AssignmentExpression') {
        if (readsField(n.right) || (n.right.type === 'Identifier' && params.has(n.right.name))) land(n.left);
      }
      // Object.assign(T, p) copies p's runtime keys: the whole table. With
      // a literal source, each property is a static copy to T.key.
      if (n.type === 'CallExpression' && memberPath(n.callee) === 'Object.assign' && n.arguments.length > 1) {
        const t = rootIdent(n.arguments[0]);
        if (!t || !facts.top.has(t) || locals.has(t) || params.has(t)) return;
        for (const src of n.arguments.slice(1)) {
          if (src.type !== 'ObjectExpression') {
            if (mentions(src, params) || readsField(src)) keys.push(...tableKeys(t).map((k) => `${t}.${k}`));
            continue;
          }
          for (const q of src.properties) {
            const k = q.type === 'Property' ? propName({ computed: q.computed, property: q.key }) : null;
            const path = `${memberPath(n.arguments[0]) ?? t}.${k}`;
            if (k !== null && (readsField(q.value) || mentions(q.value, params)) && calledPaths.has(path)) keys.push(path);
          }
        }
      }
    });
    if (!keys.length) continue;
    if (INJECTION_ALLOW.includes(`${facts.file}:${name}`)) { facts.allowHits.add(`${facts.file}:${name}`); continue; }
    for (const k of keys) if (!facts.injections.has(k)) facts.injections.set(k, name);
  }
  // A registry (T.*) is read by runtime key; it has no static key to miss.
  for (const k of facts.injections.keys()) if (!k.endsWith('.*') && !reads.has(k)) facts.deadInjections.push(k);
}

function perFileMetrics(facts) {
  const injected = facts.file === SHELL_MODULE ? 0 : facts.shellUses.size;
  return {
    configureDeps: facts.injections.size + injected,
    deadInjections: facts.deadInjections.length,
    innerHTMLAssign: facts.sinks.filter((s) => s.kind === 'innerHTMLAssign').length,
    htmlInsert: facts.sinks.filter((s) => s.kind === 'htmlInsert').length,
    lateBindings: facts.lateBindings,
  };
}

// analyzeSources parses { file: source } and returns per-file facts.
export function analyzeSources(sources, espree = loadEspree()) {
  const out = {};
  for (const [f, src] of Object.entries(sources)) out[f] = analyzeProgram(espree.parse(src, PARSE), f);
  return out;
}

// exportsOf: a module's export names, following `export * from`.
function exportsOf(all, file, seen = new Set()) {
  const facts = all[file];
  if (!facts || seen.has(file)) return new Set();
  seen.add(file);
  const out = new Set(facts.exportNames);
  for (const f of facts.starFrom) for (const n of exportsOf(all, f, seen)) if (n !== 'default') out.add(n);
  return out;
}

// sinkWrappers finds, to a fixed point, every function whose parameter
// reaches an HTML sink's argument, directly or through the HTML argument of
// another wrapper; and counts the calls that reach one. A wrapper records
// which of its parameters carry the HTML, so passing the element it writes
// to does not make the caller a wrapper too. A call resolves through its
// binding (a local function, an import, a namespace member); a call through
// an injected table or a late binding (deps.f(...), hooks.f(...)) resolves by
// the property name, the only handle such a call has.
function sinkWrappers(all) {
  const key = (f, fn) => `${f}:${fn.name ?? '@' + fn.node.loc.start.line + ':' + fn.node.loc.start.column}`;
  const wrappers = new Map(); // key -> { file, fn, html: Set of parameter indices }
  const byNode = new Map();
  for (const [f, facts] of Object.entries(all)) for (const fn of facts.fns) byNode.set(fn.node, fn);
  const find = (pred) => { for (const w of wrappers.values()) if (pred(w)) return w; return null; };
  const resolve = (f, callee) => {
    const facts = all[f];
    if (callee.type === 'Identifier') {
      const local = find((w) => w.file === f && w.fn.bound && w.fn.name === callee.name);
      if (local) return local;
      const imp = facts.imports.find((i) => i.local === callee.name && i.imported !== '*');
      return imp && all[imp.from] ? find((w) => w.file === imp.from && w.fn.bound && w.fn.name === imp.imported) : null;
    }
    if (callee.type !== 'MemberExpression') return null;
    const p = propName(callee);
    if (p === null) return null;
    const ns = callee.object.type === 'Identifier' && facts.imports.find((i) => i.local === callee.object.name && i.imported === '*');
    if (ns) return find((w) => w.file === ns.from && w.fn.bound && w.fn.name === p);
    return find((w) => w.fn.name === p);
  };
  const htmlArgs = (w, call) => [...w.html].map((i) => call.arguments[i]).filter(Boolean);
  for (let changed = true; changed;) {
    changed = false;
    for (const [f, facts] of Object.entries(all)) {
      const flows = facts.sinks.map((s) => ({ args: s.args, inFns: s.inFns }));
      for (const c of facts.calls) {
        const w = resolve(f, c.node.callee);
        if (w) flows.push({ args: htmlArgs(w, c.node), inFns: c.inFns });
      }
      for (const { args, inFns } of flows) {
        for (const fnNode of inFns) {
          const fn = byNode.get(fnNode);
          const k = key(f, fn);
          fn.paramNames.forEach((names, i) => {
            if (!args.some((a) => mentions(a, names))) return;
            if (!wrappers.has(k)) wrappers.set(k, { file: f, fn, html: new Set() });
            const w = wrappers.get(k);
            if (!w.html.has(i)) { w.html.add(i); changed = true; }
          });
        }
      }
    }
  }
  let calls = 0;
  for (const [f, facts] of Object.entries(all)) for (const c of facts.calls) if (resolve(f, c.node.callee)) calls++;
  return { wrappers: [...wrappers.keys()].sort(), calls };
}

// reachable: every file f imports, transitively.
function reachable(graph, from) {
  const seen = new Set();
  const todo = [from];
  while (todo.length) {
    for (const w of graph[todo.pop()] ?? []) if (!seen.has(w)) { seen.add(w); todo.push(w); }
  }
  return seen;
}

// upcalls checks the shell table (S20f). A slot must be registered by a root
// module with one of its own top-level functions or consts (not an import,
// not an inline function), and that implementation must use some other
// top-level binding of the root (a slot that only forwards to something the
// caller could import is a disguised import). A module using shell.X must be
// one the root that registered X reaches through imports (else a plain
// import would not close a cycle, and the upcall is not one).
function upcalls(all, graph) {
  const forwarders = [];
  const notUp = [];
  const owner = new Map();
  for (const [f, facts] of Object.entries(all)) {
    for (const { arg, line } of facts.shellRegs) {
      if (!SHELL_ROOTS.includes(f)) { forwarders.push(`${f}:${line}: registerShell outside the root modules (${SHELL_ROOTS.join(', ')})`); continue; }
      if (arg?.type !== 'ObjectExpression') { forwarders.push(`${f}:${line}: registerShell needs an object literal`); continue; }
      for (const q of arg.properties) {
        const slot = q.type === 'Property' ? propName({ computed: q.computed, property: q.key }) : null;
        const v = q.type === 'Property' ? q.value : null;
        const decl = v?.type === 'Identifier' ? facts.top.get(v.name) : undefined;
        const impl = decl?.type === 'FunctionDeclaration' ? decl.body : decl?.type === 'VariableDeclarator' ? decl.init : null;
        if (!slot || !impl) { forwarders.push(`${f}:${q.loc.start.line}: shell slot ${slot ?? '?'} is not one of ${f}'s own top-level functions or consts`); continue; }
        owner.set(slot, f);
        const own = new Set([...facts.top.keys()].filter((n) => n !== v.name));
        if (!mentions(impl, own)) forwarders.push(`${f}:${q.loc.start.line}: shell slot ${slot} (${v.name}) uses nothing of ${f}'s own — it only forwards`);
      }
    }
  }
  for (const [f, facts] of Object.entries(all)) {
    if (f === SHELL_MODULE) continue;
    for (const slot of [...facts.shellUses].sort()) {
      const root = owner.get(slot);
      if (!root) notUp.push(`${f}: shell.${slot} is not registered by any root module`);
      else if (!reachable(graph, root).has(f)) notUp.push(`${f}: shell.${slot} — ${root} does not reach ${f} through imports, so import it directly`);
    }
  }
  return { forwarders, notUp };
}

// measureGlobal computes the _global entry from every file's facts and the
// import graph; details holds the finding behind each counted item.
export function measureGlobal(all, graph, leaves = []) {
  const details = {};
  const totals = { innerHTMLAssign: 0, htmlInsert: 0 };
  for (const facts of Object.values(all)) for (const s of facts.sinks) totals[s.kind]++;
  const { wrappers, calls } = sinkWrappers(all);
  details.htmlSinks = [`${calls} call(s) to ${wrappers.length} sink wrapper(s): ${wrappers.join(', ')}`];
  const cycles = sccs(graph);
  details.importCycles = cycles.map((c) => c.join(' <-> '));
  details.unresolvedImports = [];
  details.reexports = [];
  for (const [f, facts] of Object.entries(all)) {
    for (const line of facts.reexports) details.reexports.push(`${f}:${line}`);
    for (const i of facts.imports) {
      if (!i.from || i.imported === '*') continue;
      if (!all[i.from]) { if (!EXCLUDE.has(i.from)) details.unresolvedImports.push(`${f}:${i.line}: ${i.from} is not a static/ module`); continue; }
      if (i.imported !== null && !exportsOf(all, i.from).has(i.imported)) {
        details.unresolvedImports.push(`${f}:${i.line}: ${i.from} does not export ${i.imported}`);
      }
    }
  }
  const leafSet = new Set(leaves);
  details.leafImports = [];
  for (const f of leaves) {
    for (const to of graph[f] ?? []) if (graph[to] && !leafSet.has(to)) details.leafImports.push(`${f} imports ${to}, which is not a leaf`);
  }
  const { forwarders, notUp } = upcalls(all, graph);
  details.upcallForwarders = forwarders;
  details.upcallNotUp = notUp;
  return {
    metrics: {
      htmlSinks: totals.innerHTMLAssign + totals.htmlInsert + calls,
      importCycles: cycles.length,
      unresolvedImports: details.unresolvedImports.length,
      reexports: details.reexports.length,
      leafImports: details.leafImports.length,
      upcallForwarders: forwarders.length,
      upcallNotUp: notUp.length,
    },
    details,
  };
}

// importsOfSource is importsOf on source text (the unit tests' entry point;
// importGraph reads the same function over every static/*.js file).
export function importsOfSource(src, espree = loadEspree()) {
  return importsOf(espree.parse(src, PARSE));
}

export function measureSource(src, espree = loadEspree(), file = '') {
  const lines = src.split('\n');
  const total = lines.length - (src.endsWith('\n') ? 1 : 0);
  const program = espree.parse(src, PARSE);
  const fns = functions(program);
  let letVar = 0;
  for (const line of lines) if (/^(?:let|var)\s/.test(line)) letVar++;
  return {
    lines: total,
    maxFnLines: fns.reduce((m, f) => Math.max(m, f.lines), 0),
    fnOver100: fns.filter((f) => f.lines > 100).length,
    topLevelLetVar: letVar,
    ...perFileMetrics(analyzeProgram(program, file)),
  };
}

function staticFiles() {
  return fs
    .readdirSync(STATIC_DIR)
    .filter((f) => f.endsWith('.js') && !EXCLUDE.has(f))
    .sort();
}

function readSources() {
  const out = {};
  for (const f of staticFiles()) out[f] = fs.readFileSync(path.join(STATIC_DIR, f), 'utf8');
  return out;
}

// GLOBAL is the baseline key of the cross-file metrics; no static/ file can
// be named it (they all end in .js).
export const GLOBAL = '_global';

// snapshot measures every file, plus the _global entry. problems are the
// findings that fail the run outright (a stale INJECTION_ALLOW entry);
// details back each _global count.
function snapshot(leaves) {
  const espree = loadEspree();
  const sources = readSources();
  const all = analyzeSources(sources, espree);
  const out = {};
  for (const f of Object.keys(sources)) out[f] = measureSource(sources[f], espree, f);
  const graph = importGraph(espree);
  const { metrics, details } = measureGlobal(all, graph, leaves);
  out[GLOBAL] = metrics;
  const hits = new Set(Object.values(all).flatMap((a) => [...a.allowHits]));
  const problems = INJECTION_ALLOW.filter((a) => !hits.has(a)).map((a) => `INJECTION_ALLOW: ${a} no longer receives an injection — drop it from the allowlist`);
  return { current: out, graph, details, problems };
}

// importGraph maps every static/*.js file to the files it imports (D-S19:
// the module graph the no-module-side-effects / cycle checks both read).
export function importGraph(espree = loadEspree()) {
  const graph = {};
  for (const f of staticFiles()) {
    const src = fs.readFileSync(path.join(STATIC_DIR, f), 'utf8');
    graph[f] = importsOf(espree.parse(src, PARSE));
  }
  return graph;
}

// sccs returns every strongly-connected component of size > 1 in graph
// (Tarjan): a cycle, however many files long the loop runs through.
export function sccs(graph) {
  let index = 0;
  const stack = [];
  const onStack = new Set();
  const indices = new Map();
  const low = new Map();
  const out = [];
  const connect = (v) => {
    indices.set(v, index);
    low.set(v, index);
    index++;
    stack.push(v);
    onStack.add(v);
    for (const w of graph[v] ?? []) {
      if (!graph[w]) continue; // not a tracked file (excluded or external)
      if (!indices.has(w)) {
        connect(w);
        low.set(v, Math.min(low.get(v), low.get(w)));
      } else if (onStack.has(w)) {
        low.set(v, Math.min(low.get(v), indices.get(w)));
      }
    }
    if (low.get(v) === indices.get(v)) {
      const comp = [];
      let w;
      do {
        w = stack.pop();
        onStack.delete(w);
        comp.push(w);
      } while (w !== v);
      out.push(comp);
    }
  };
  for (const v of Object.keys(graph)) if (!indices.has(v)) connect(v);
  return out.filter((c) => c.length > 1);
}

// loadCaps reads scripts/js-ratchet.caps.json and validates its shape. Every
// failure here is fail-closed: --check (and --write) must not silently run
// with a broken or missing cap.
export function loadCaps(capsPath = CAPS_PATH) {
  if (!fs.existsSync(capsPath)) {
    return { errors: [`missing ${path.relative(ROOT, capsPath)}; the js-ratchet caps must exist`] };
  }
  let raw;
  try {
    raw = JSON.parse(fs.readFileSync(capsPath, 'utf8'));
  } catch (e) {
    return { errors: [`${path.relative(ROOT, capsPath)} is not valid JSON: ${e.message}`] };
  }
  const errors = [];
  for (const key of ['maxFnLines', 'lines', 'sideEffectLegacy', 'cycleLegacy', 'leaves']) {
    if (!(key in raw)) errors.push(`caps is missing the top-level key "${key}"`);
  }
  if (errors.length) return { errors };
  if (typeof raw.maxFnLines !== 'object' || raw.maxFnLines === null || Array.isArray(raw.maxFnLines)) {
    errors.push('caps.maxFnLines must be an object');
  } else {
    // A safe integer, like the int64 tools/ratchet-raises decodes it into:
    // 1e19 or 120.5 would otherwise pass here and read as anything there.
    if (!Number.isSafeInteger(raw.maxFnLines.default) || raw.maxFnLines.default < 1) {
      errors.push('caps.maxFnLines.default must be a positive safe integer');
    }
    if (!Array.isArray(raw.maxFnLines.exempt)) errors.push('caps.maxFnLines.exempt must be an array');
  }
  if (typeof raw.lines !== 'object' || raw.lines === null || Array.isArray(raw.lines)) {
    errors.push('caps.lines must be an object');
  } else {
    for (const [f, v] of Object.entries(raw.lines)) {
      if (!Number.isSafeInteger(v) || v < 0) errors.push(`caps.lines.${f} must be a non-negative safe integer`);
    }
  }
  if (!Array.isArray(raw.sideEffectLegacy)) errors.push('caps.sideEffectLegacy must be an array');
  if (!Array.isArray(raw.cycleLegacy)) errors.push('caps.cycleLegacy must be an array');
  if (!Array.isArray(raw.leaves)) errors.push('caps.leaves must be an array');
  if (errors.length) return { errors };
  return { caps: raw };
}

// capProblems checks the caps against the current measurement and the
// import graph: a cap pointing at a file that does not exist, an exempt or
// legacy entry that is already clean (must be moved out), a file over its
// cap (whether brand new or long-standing), and an import cycle not fully
// accounted for in cycleLegacy.
export function capProblems(current, graph, caps) {
  const problems = [];
  const exists = (f) => f in current;
  for (const f of caps.maxFnLines.exempt) {
    if (!exists(f)) { problems.push(`caps.maxFnLines.exempt: ${f} does not exist in static/`); continue; }
    if (current[f].maxFnLines <= caps.maxFnLines.default) {
      problems.push(`caps.maxFnLines.exempt: ${f} is already <= ${caps.maxFnLines.default} (${current[f].maxFnLines}) — move it out of exempt`);
    }
  }
  for (const f of Object.keys(caps.lines)) {
    if (!exists(f)) { problems.push(`caps.lines: ${f} does not exist in static/`); continue; }
    if (current[f].lines > caps.lines[f]) {
      problems.push(`caps.lines: ${f} is ${current[f].lines} lines, over the cap of ${caps.lines[f]}`);
    }
  }
  const exempt = new Set(caps.maxFnLines.exempt);
  for (const [f, m] of Object.entries(current)) {
    if (f === GLOBAL) continue;
    const limit = exempt.has(f) ? Infinity : caps.maxFnLines.default;
    if (m.maxFnLines > limit) {
      problems.push(`${f}: maxFnLines ${m.maxFnLines} exceeds the cap of ${limit}${exempt.has(f) ? '' : ' (not in caps.maxFnLines.exempt)'}`);
    }
  }
  for (const f of caps.sideEffectLegacy) {
    if (!exists(f)) problems.push(`caps.sideEffectLegacy: ${f} does not exist in static/`);
  }
  for (const f of caps.leaves) {
    if (!exists(f)) problems.push(`caps.leaves: ${f} does not exist in static/`);
  }
  const cyclic = new Set(sccs(graph).flat());
  for (const f of caps.cycleLegacy) {
    if (!exists(f)) { problems.push(`caps.cycleLegacy: ${f} does not exist in static/`); continue; }
    if (!cyclic.has(f)) problems.push(`caps.cycleLegacy: ${f} is no longer part of an import cycle — move it out`);
  }
  for (const f of cyclic) {
    if (!caps.cycleLegacy.includes(f)) {
      problems.push(`${f} is part of an import cycle (${[...sccs(graph).find((c) => c.includes(f))].join(' <-> ')}) and not in caps.cycleLegacy`);
    }
  }
  return problems;
}

// label names a baseline entry in messages: a file, or the cross-file set.
const label = (file) => (file === GLOBAL ? 'global' : file);

// compare returns every way current is out of step with baseline. details
// (from measureGlobal) lists the findings behind a _global count that grew.
export function compare(current, baseline, details = {}) {
  const out = [];
  for (const [file, metrics] of Object.entries(current)) {
    const base = baseline[file];
    if (!base) {
      out.push(`${label(file)}: not in baseline — run --write to start tracking it`);
      continue;
    }
    for (const metric of Object.keys(base)) {
      if (!(metric in metrics)) out.push(`${label(file)}: baseline metric ${metric} is no longer measured — run --write`);
    }
    for (const [metric, cur] of Object.entries(metrics)) {
      const b = base[metric];
      if (b === undefined) {
        // A metric the baseline does not hold would compare false both ways
        // and pass unchecked.
        out.push(`${label(file)}: ${metric} is not in the baseline — run --write to start tracking it`);
      } else if (cur > b) {
        out.push(`${label(file)}: ${metric} grew ${b} -> ${cur} (ratchet: may only shrink)`);
        if (file === GLOBAL) for (const d of details[metric] ?? []) out.push(`  ${d}`);
      } else if (cur < b) {
        out.push(`${label(file)}: ${metric} improved ${b} -> ${cur} — ship the tightened baseline (run scripts/js-ratchet.mjs --write)`);
      }
    }
  }
  for (const file of Object.keys(baseline)) {
    if (!current[file]) out.push(`${label(file)}: in baseline but ${file === GLOBAL ? 'no longer measured' : 'gone from static/'} — run --write`);
  }
  return out;
}

// checkCaps returns loadCaps' errors, or, when the caps file is sound, the
// problems found checking it against current and the import graph. Shared by
// --check and --write: both read caps, and neither may run against a broken
// one.
function checkCaps(current, graph, caps, errors) {
  if (errors) return errors;
  return capProblems(current, graph, caps);
}

function check({ current, graph, details, problems: snapProblems }, caps, errors) {
  const capErrors = [...checkCaps(current, graph, caps, errors), ...snapProblems];
  if (capErrors.length) {
    for (const p of capErrors) console.error(`js-ratchet caps: ${p}`);
  }
  if (!fs.existsSync(BASELINE_PATH)) {
    console.error(`missing baseline ${path.relative(ROOT, BASELINE_PATH)}; run --write first`);
    return 1;
  }
  const problems = compare(current, JSON.parse(fs.readFileSync(BASELINE_PATH, 'utf8')), details);
  if (problems.length) {
    for (const p of problems) console.error(p);
    console.error(`js-ratchet: ${problems.filter((p) => !p.startsWith('  ')).length} metric(s) out of step with the baseline`);
  }
  if (capErrors.length || problems.length) return 1;
  console.log('js-ratchet: OK');
  return 0;
}

// raisedMetrics lists the values --write would raise; it refuses them.
export function raisedMetrics(current, baseline) {
  const out = [];
  for (const [file, metrics] of Object.entries(current)) {
    for (const [metric, cur] of Object.entries(metrics)) {
      const b = baseline[file]?.[metric];
      if (b !== undefined && cur > b) out.push(`${label(file)} ${metric} ${b} -> ${cur}`);
    }
  }
  return out;
}

// write never touches caps.json (comment at the top of this file): a cap
// problem here still fails the run, since --write reads the same caps and a
// broken or over-cap one must not be baselined silently.
function write({ current, graph, problems: snapProblems }, caps, errors) {
  const capErrors = [...checkCaps(current, graph, caps, errors), ...snapProblems];
  if (capErrors.length) {
    for (const p of capErrors) console.error(`js-ratchet caps: ${p}`);
    return 1;
  }
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
  const { caps, errors } = loadCaps();
  const snap = snapshot(caps?.leaves ?? []);
  const mode = process.argv[2] ?? '--check';
  if (mode === '--check') process.exit(check(snap, caps, errors));
  else if (mode === '--write') process.exit(write(snap, caps, errors));
  else {
    console.error(`unknown mode ${mode}; use --check | --write`);
    process.exit(2);
  }
}
