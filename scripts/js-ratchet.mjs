#!/usr/bin/env node
// js-ratchet.mjs — growth ratchet for the dashboard's scripts
// (internal/server/static/*.js). Per file, these numbers may only go down:
//
//   lines           total line count
//   maxFnLines      longest function, of every form: declarations, expressions,
//                   arrows, object and class methods (nested ones counted on
//                   their own); a top-level IIFE is a module scope, not a function
//   maxIifeLines    longest top-level IIFE, (function(){…})(), arrow and unary
//                   (!function(){…}()) forms: the module scope maxFnLines skips
//   fnOver100       count of such functions longer than 100 lines
//   topLevelLetVar  column-0 `let` / `var` declarations (mutable globals)
//   configureDeps   dependencies the module receives by injection instead of
//                   import, counted where they land (S20a, #3026): the keys
//                   an exported function (or a helper it hands the
//                   parameter to) copies out of its parameter into module
//                   scope, whatever the function is called, plus one per
//                   shell.X upcall slot the module uses
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
//                      link error: the whole page fails to load), and
//                      namespace imports of a table module read as a whole
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
// below it (the improving PR must ship the tightened baseline — run --write),
// and on an injection receiver outside the closed set (receiverProblems).
// --write refuses to raise a value: deliberate growth requires hand-editing
// scripts/js-ratchet.baseline.json and an approved scripts/ratchet-raises.jsonl
// entry (tools/ratchet-raises). That tool also holds the sums across files
// (lines, fnOver100, configureDeps, deadInjections, innerHTMLAssign,
// htmlInsert, lateBindings), the maxima of maxFnLines and maxIifeLines and
// every _global metric, so a new file cannot absorb growth; lines and the
// injection / HTML / late-binding counts are judged only as sums there, since
// moving code between files is a refactor, not a raise (js-ratchet --check
// still holds every file's own values).

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

// functions returns every function in the program with its line span (fns),
// except top-level IIFEs, whose bodies are walked but which are not functions
// a reader has to hold in their head at once; their spans are iifes.
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
  const span = (n) => n.loc.end.line - n.loc.start.line + 1;
  return { fns: out, iifes: [...scopes].map(span) };
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

// staticString is the string a literal spells: 'p', 1, or a template with
// no expressions (`p`); anything else is null.
function staticString(n) {
  if (n?.type === 'Literal') return String(n.value);
  if (n?.type === 'TemplateLiteral' && !n.expressions.length) return n.quasis[0].value.cooked;
  return null;
}

// propName is a member's static property name: x.p, x['p'] and x[`p`] are
// all p (so is an object key, 'p': v included).
function propName(m) {
  if (!m.computed && m.property.type === 'Identifier') return m.property.name;
  return staticString(m.property);
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

// staticKeys: the keys an object pattern takes (const { a, b: x } = t is
// a, b), or null when one is computed or a rest element takes the rest.
function staticKeys(p) {
  if (p.type !== 'ObjectPattern') return null;
  const keys = p.properties.map((q) => (q.type === 'Property' ? propName({ computed: q.computed, property: q.key }) : null));
  return keys.includes(null) ? null : keys;
}

const IMPORT_SPECIFIER = new Set(['ImportSpecifier', 'ImportDefaultSpecifier', 'ImportNamespaceSpecifier']);

// htmlSinks returns the HTML sinks node is: for each, the arguments that
// land in the DOM as HTML and the metric (kind) it counts in. A property
// sink is one whether it is assigned (el.innerHTML = s), copied in by a
// literal key (Object.assign(el, { innerHTML: s })) or set by name
// (Reflect.set(el, 'innerHTML', s)); srcdoc also as an attribute
// (setAttribute('srcdoc', s)). A property named by a runtime value
// (el[k] = s) is not resolved.
// Maps, not object literals: `p in {…}` would also match toString.
const HTML_PROP = new Map([['innerHTML', 'innerHTMLAssign'], ['outerHTML', 'htmlInsert'], ['srcdoc', 'htmlInsert']]);
const HTML_CALL_ARG = new Map([['insertAdjacentHTML', 1], ['setHTMLUnsafe', 0], ['createContextualFragment', 0], ['parseFromString', 0]]);
function htmlSinks(node) {
  if (node.type === 'AssignmentExpression' && node.left.type === 'MemberExpression') {
    const kind = HTML_PROP.get(propName(node.left));
    return kind ? [{ kind, args: [node.right] }] : [];
  }
  if (node.type !== 'CallExpression' || node.callee.type !== 'MemberExpression') return [];
  const callee = memberPath(node.callee);
  if (callee === 'Object.assign') {
    return node.arguments.slice(1).filter((a) => a.type === 'ObjectExpression').flatMap((a) => a.properties
      .filter((q) => q.type === 'Property' && HTML_PROP.has(propName({ computed: q.computed, property: q.key })))
      .map((q) => ({ kind: HTML_PROP.get(propName({ computed: q.computed, property: q.key })), args: [q.value] })));
  }
  if (callee === 'Reflect.set') {
    const k = HTML_PROP.get(staticString(node.arguments[1]));
    return k ? [{ kind: k, args: node.arguments.slice(2, 3) }] : [];
  }
  const p = propName(node.callee);
  // srcdoc is an attribute too; HTML attribute names are case-insensitive.
  const attr = p === 'setAttribute' ? 0 : p === 'setAttributeNS' ? 1 : -1;
  if (attr >= 0 && staticString(node.arguments[attr])?.toLowerCase() === 'srcdoc') {
    return [{ kind: 'htmlInsert', args: node.arguments.slice(attr + 1, attr + 2) }];
  }
  if (HTML_CALL_ARG.has(p)) return [{ kind: 'htmlInsert', args: node.arguments.slice(HTML_CALL_ARG.get(p), HTML_CALL_ARG.get(p) + 1) }];
  if ((p === 'write' || p === 'writeln') && node.callee.object.type === 'Identifier' && node.callee.object.name === 'document') {
    return [{ kind: 'htmlInsert', args: node.arguments }];
  }
  return [];
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

// The analysis reads three lists from caps.json, where tools/ratchet-raises
// sees them (as constants here, each could zero a count without a raise):
//   lateBindingTables  the late-bound function tables, by the module that
//                      exports each (nz_util.js nzViews; state.js hooks,
//                      which must stay absent: the entry counts any revived
//                      export); a dropped table is a raise there
//   injectionAllow     "file:fn" receivers that copy a parameter's fields
//                      into module scope on purpose and are not dependency
//                      injection (registerActions: the data-action registry).
//                      Each must still match a receiver: a stale one fails
//                      the check. A new entry is a raise there
//   shellRoots         the modules allowed to registerShell (a new one is a
//                      raise there)
//   injectionLegacy    "file:fn" receivers of injected dependencies left over
//                      from before shell.js, shrink-only (a new entry is a
//                      raise there; a stale one fails the check). Any other
//                      receiver fails the check, whatever its count
// NO_CAPS is the empty set of each, for analysing a fixture.
export const NO_CAPS = Object.freeze({ lateBindingTables: {}, injectionAllow: [], injectionLegacy: [], shellRoots: [], leaves: [] });

// SHELL_MODULE — the upcall table (S20f): the root modules register their
// orchestration functions once with registerShell({...}) and lower modules
// call shell.X(...).
export const SHELL_MODULE = 'shell.js';

// desugarNamespaces: a namespace import of shell.js or of a late-binding
// table's module (import * as S) reaches the table through a member: S.hooks,
// S.shell, S.registerShell, or one destructured (const { hooks: H } = S).
// Each member becomes a binding named "S.hooks" (no identifier has a dot, so
// it shadows nothing), recorded as an import of hooks like the named form, so
// every check after this sees it the same way. Any other read of S (S[k], an
// alias, S passed on, a rest element), or a dynamic import() of the
// module, hides which table it reaches: for
// shell.js an opaque shell read (upcallNotUp), for a table module an
// nsOpaque entry (unresolvedImports). The AST is rewritten in place.
function desugarNamespaces(program, facts, caps) {
  const wanted = new Map([[SHELL_MODULE, new Set(['shell', 'registerShell'])]]); // module -> export names
  for (const [name, module] of Object.entries(caps.lateBindingTables)) wanted.set(module, new Set([...(wanted.get(module) ?? []), name]));
  const ns = new Map(facts.imports.filter((i) => i.imported === '*' && wanted.has(i.from)).map((i) => [i.local, i.from]));
  const opaque = (local, from, line) => {
    if (from === SHELL_MODULE) facts.shellOpaque.push(line);
    else facts.nsOpaque.push({ local, from, line });
  };
  const members = []; // [MemberExpression, binding name, imported, from]
  visit(program, (n, parent, key) => {
    // import('./shell.js') hands over the same namespace, but as a value
    // this analysis does not follow.
    if (n.type === 'ImportExpression') {
      const from = moduleFile(staticString(n.source) ?? '');
      if (wanted.has(from)) opaque(null, from, n.loc.start.line);
    }
    if (n.type !== 'Identifier' || !ns.has(n.name) || !isReference(n, parent, key) || IMPORT_SPECIFIER.has(parent?.type)) return;
    const from = ns.get(n.name);
    const names = wanted.get(from);
    const line = n.loc.start.line;
    if (parent?.type === 'MemberExpression' && key === 'object') {
      const p = propName(parent);
      if (p === null) opaque(n.name, from, line);
      else if (names.has(p)) members.push([parent, `${n.name}.${p}`, p, from]);
      return;
    }
    if (parent?.type === 'VariableDeclarator' && key === 'init' && staticKeys(parent.id)) {
      for (const q of parent.id.properties) {
        const p = propName({ computed: q.computed, property: q.key });
        if (!names.has(p)) continue;
        const v = q.value.type === 'AssignmentPattern' ? q.value.left : q.value;
        if (v.type === 'Identifier') { facts.imports.push({ local: v.name, imported: p, from, line }); facts.nsBound.add(v); }
        else opaque(n.name, from, line); // a nested pattern (const { shell: { x } } = S): not followed, so a finding
      }
      return;
    }
    opaque(n.name, from, line);
  });
  for (const [m, name, imported, from] of members) {
    for (const k of Object.keys(m)) if (!SKIP_KEYS.has(k)) delete m[k];
    Object.assign(m, { type: 'Identifier', name });
    if (!facts.imports.some((i) => i.local === name)) facts.imports.push({ local: name, imported, from, line: m.loc.start.line });
  }
}

// analyzeProgram collects everything the per-file metrics and the _global
// checks need from one module.
function analyzeProgram(program, file, caps = NO_CAPS) {
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
    receivers: new Map(), // receiver function name -> { line, keys }, besides injectionAllow's
    deadInjections: [],
    shellUses: new Set(),
    shellOpaque: [], // lines reading shell other than by a static slot
    shellRegs: [], // ObjectExpression args of registerShell(...)
    tables: new Set(), // local names of the late-binding tables
    shellNames: new Set(), // local names of shell.js's shell
    nsOpaque: [], // { local, from, line }: a table module's namespace read as a whole
    nsBound: new Set(), // the pattern identifiers desugarNamespaces bound (declarations, not reads)
  };
  const exportedLocal = new Set();
  let defaultExport = null; // export default function (…) {…} / { … }
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
    if (st.type === 'ExportDefaultDeclaration') {
      facts.exportNames.add('default');
      const d = st.declaration;
      if (d.type === 'Identifier') exportedLocal.add(d.name);
      else if (isFn(d) || d.type === 'ObjectExpression') defaultExport = { name: d.id?.name ?? 'default', node: d };
    }
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
  desugarNamespaces(program, facts, caps);
  const localOf = (module, name) => facts.imports.filter((i) => i.from === module && i.imported === name).map((i) => i.local);
  const tables = facts.tables;
  for (const [name, module] of Object.entries(caps.lateBindingTables)) {
    if (file === module) tables.add(name);
    for (const l of localOf(module, name)) tables.add(l);
  }
  // A local alias of a table (const H = hooks) writes into the table too.
  for (let grew = true; grew;) {
    grew = false;
    visit(program, (n) => {
      if (n.type === 'VariableDeclarator' && n.id.type === 'Identifier' && n.init?.type === 'Identifier' &&
          tables.has(n.init.name) && !tables.has(n.id.name)) { tables.add(n.id.name); grew = true; }
    });
  }
  const shellNames = facts.shellNames;
  for (const l of localOf(SHELL_MODULE, 'shell')) shellNames.add(l);
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
    for (const sink of htmlSinks(n)) facts.sinks.push({ ...sink, line: n.loc.start.line, inFns: [...stack] });
    if (n.type === 'CallExpression') facts.calls.push({ node: n, inFns: [...stack] });
    if (n.type === 'AssignmentExpression' && n.left.type === 'MemberExpression' &&
        n.left.object.type === 'Identifier' && tables.has(n.left.object.name)) {
      facts.lateBindings += n.right.type === 'ObjectExpression' ? n.right.properties.length : 1;
    }
    const tableCall = n.type === 'CallExpression' && n.arguments[0]?.type === 'Identifier' && tables.has(n.arguments[0].name)
      ? memberPath(n.callee) : null;
    if (tableCall === 'Object.assign' || tableCall === 'Object.defineProperties') {
      for (const a of n.arguments.slice(1)) facts.lateBindings += a.type === 'ObjectExpression' ? a.properties.length : 1;
    }
    if (tableCall === 'Object.defineProperty' || tableCall === 'Reflect.set') facts.lateBindings++;
    // A shell slot is used as shell.X or destructured by its static key
    // (const { X } = shell). Any other read of shell (shell[k], an alias, a
    // rest element, shell passed on) hides which slot it reaches: a finding.
    if (n.type === 'Identifier' && shellNames.has(n.name) && isReference(n, parent, key) && !IMPORT_SPECIFIER.has(parent?.type) && !facts.nsBound.has(n)) {
      const slot = parent?.type === 'MemberExpression' && key === 'object' ? propName(parent) : null;
      const slots = parent?.type === 'VariableDeclarator' && key === 'init' ? staticKeys(parent.id) : null;
      if (slot !== null) facts.shellUses.add(slot);
      else if (slots) slots.forEach((x) => facts.shellUses.add(x));
      else facts.shellOpaque.push(n.loc.start.line);
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
  findInjections(program, facts, exportedLocal, defaultExport, caps.injectionAllow);
  return facts;
}

// findInjections: a function another module can call (an export, the default
// export, a method of an exported object literal) that copies its parameter's
// fields (p.x, p[k], a destructured field, a spread of p, Object.assign from
// p) into a module-scope binding is receiving injected dependencies, whatever
// it is called. So is one that stores a whole parameter, or anything made from
// one, in a binding the module then calls. A parameter (or a field of one)
// handed to a function of this module by a bare call makes that function a
// receiver on that parameter position, to a fixed point: a thin export over a
// private helper lands the same keys. The bindings of a for-of / for-in over a
// parameter and the parameters of a callback given to a call on one
// (Object.entries(p).forEach(([k, v]) => …)) carry its fields too. The keys
// landed are counted (a whole-table copy counts the table's keys); a key the
// module never reads is dead. A use of the table counts however it is
// spelt: deps.f(), const { f } = deps; f(), const f = deps.f; f(),
// const d = deps; d.f().
//
// Known gaps: a class constructor as the receiver; a parameter handed on
// through .call / .apply or a member call (helpers.set(p)); a callback
// stored and only passed on (listeners.push(cb)); a field copied by a static
// copy into a slot the module never calls; a table slot reached only through
// a function's return value or another object's member (o.t = deps; o.t.f()).
function findInjections(program, facts, exportedLocal, defaultExport, allow) {
  const receivers = new Map(); // fn node -> { name, fn, at: receiving parameter indices }
  const receive = (name, fn, indices) => {
    if (!receivers.has(fn)) receivers.set(fn, { name, fn, at: new Set() });
    const r = receivers.get(fn);
    let grew = false;
    for (const i of indices) if (i < fn.params.length && !r.at.has(i)) { r.at.add(i); grew = true; }
    return grew;
  };
  const every = (fn) => fn.params.map((_, i) => i);
  // An exported function, or a function-valued property of an exported
  // object literal (api.wire).
  const exported = (name, node) => {
    if (isFn(node)) { receive(name, node, every(node)); return; }
    if (node?.type !== 'ObjectExpression') return;
    for (const q of node.properties) {
      const k = q.type === 'Property' && isFn(q.value) ? propName({ computed: q.computed, property: q.key }) : null;
      if (k !== null) receive(`${name}.${k}`, q.value, every(q.value));
    }
  };
  for (const name of exportedLocal) {
    const d = facts.top.get(name);
    exported(name, d?.type === 'VariableDeclarator' ? d.init : d);
  }
  if (defaultExport) exported(defaultExport.name, defaultExport.node);
  // The functions of this module a bare call reaches, by name.
  const local = new Map();
  for (const f of facts.fns) if (f.bound && f.name) local.set(f.name, [...(local.get(f.name) ?? []), f.node]);
  // Every static read of a module binding's member, and every call path.
  const reads = new Set();
  const calledPaths = new Set();
  const calledRoots = new Set(); // bindings something is called through: t.f(), t[k](), const f = t[k]; f()
  // aliases: [local, path, root] for a local bound to a member of root, or
  // to root itself: const f = t.x, const f = t[k] (no path), const d = t,
  // const { x, y: { z } } = t, ({ x } = t). Calling through the local calls
  // through root, and reading the local reads the path.
  const aliases = [];
  // elementCalled: the lists an element of which is called directly (X[i](),
  // X.find(f)(), X.forEach((h) => h()), for (const h of X) h(), const f =
  // X[i]; f()); elementAliases: [local, list] for const f = X[i] / X.find(f).
  const elementCalled = new Set();
  const elementAliases = [];
  const alias = (pattern, path, root) => {
    if (pattern.type === 'AssignmentPattern') return alias(pattern.left, path, root);
    if (pattern.type === 'Identifier') { aliases.push([pattern.name, path, root]); return; }
    if (pattern.type !== 'ObjectPattern') return;
    for (const q of pattern.properties) {
      if (q.type === 'RestElement') { alias(q.argument, path, root); continue; }
      const k = propName({ computed: q.computed, property: q.key });
      alias(q.value, k !== null && path !== null ? `${path}.${k}` : null, root);
    }
  };
  visit(program, (n, parent, key) => {
    if (n.type === 'MemberExpression') {
      const writeTarget = parent?.type === 'AssignmentExpression' && key === 'left';
      const path = memberPath(n);
      if (path && !writeTarget) reads.add(path);
    }
    const bind = n.type === 'VariableDeclarator' ? [n.id, n.init]
      : n.type === 'AssignmentExpression' && n.operator === '=' && n.left.type === 'ObjectPattern' ? [n.left, n.right] : null;
    if (bind?.[1] && (bind[1].type === 'Identifier' || bind[1].type === 'MemberExpression')) {
      const root = rootIdent(bind[1]);
      if (root) alias(bind[0], memberPath(bind[1]), root);
    }
    const list = bind?.[0].type === 'Identifier' && bind[1] ? elementOf(bind[1]) : null;
    if (list) elementAliases.push([bind[0].name, list]);
    if (n.type === 'ForOfStatement' && memberPath(n.right)) {
      const names = n.left.type === 'VariableDeclaration' ? n.left.declarations.flatMap((d) => patternNames(d.id)) : patternNames(n.left);
      if (callsDirectly(n.body, names)) elementCalled.add(memberPath(n.right));
    }
    if (isReference(n, parent, key) && !(parent?.type === 'AssignmentExpression' && key === 'left') &&
        !(parent?.type === 'VariableDeclarator' && key === 'id')) reads.add(n.name);
    if (n.type === 'CallExpression') {
      const path = memberPath(n.callee);
      if (path) calledPaths.add(path);
      if (n.callee.type === 'MemberExpression') calledRoots.add(rootIdent(n.callee));
      const direct = n.callee.type === 'MemberExpression' && ['call', 'apply'].includes(propName(n.callee)) ? n.callee.object : n.callee;
      const list = elementOf(direct);
      if (list) elementCalled.add(list);
      const on = n.callee.type === 'MemberExpression' ? memberPath(n.callee.object) : null;
      if (on && n.arguments.some((a) => isFn(a) && callsDirectly(a.body, a.params.flatMap((q) => patternNames(q))))) elementCalled.add(on);
    }
  });
  // Resolve the aliases to a fixed point (an alias of an alias).
  const under = (set, l) => [...set].filter((c) => c === l || c.startsWith(l + '.')).map((c) => c.slice(l.length));
  for (let grew = true; grew;) {
    grew = false;
    const add = (set, x) => { if (!set.has(x)) { set.add(x); grew = true; } };
    for (const [l, path, root] of aliases) {
      if (l === root) continue;
      if (calledRoots.has(l)) add(calledRoots, root);
      if (path !== null && elementCalled.has(l)) add(elementCalled, path);
      const called = under(calledPaths, l);
      if (called.length) add(calledRoots, root);
      if (path === null) continue;
      for (const rest of called) add(calledPaths, path + rest);
      for (const rest of under(reads, l)) add(reads, path + rest);
    }
  }
  for (const [l, list] of elementAliases) if (['', '.call', '.apply'].some((s) => calledPaths.has(l + s))) elementCalled.add(list);
  // listOfData: base is a list the module initialises as an array literal,
  // calls nothing through but the list's own methods (jobs.find, jobs.filter),
  // and never calls an element of. A runtime-keyed write into one is an
  // element of data (jobs[i] = job), not an injected table.
  const listOfData = (base) => {
    const p = memberPath(base);
    return isListLiteral(facts, base) && !elementCalled.has(p) &&
      [...calledPaths].every((c) => !(c === p || c.startsWith(p + '.')) || ARRAY_METHODS.has(c.slice(p.length + 1)));
  };
  const tableKeys = (t) => {
    const d = facts.top.get(t);
    const literal = d?.type === 'VariableDeclarator' && d.init?.type === 'ObjectExpression'
      ? d.init.properties.filter((q) => q.type === 'Property').map((q) => propName({ computed: q.computed, property: q.key }) ?? '?')
      : [];
    if (literal.length) return literal;
    const read = [...reads].filter((r) => r.startsWith(t + '.') && !r.slice(t.length + 1).includes('.')).map((r) => r.slice(t.length + 1));
    return read.length ? read : ['*']; // a registry with no static keys: one table
  };
  // carriesOf: does an expression carry what the receiver's receiving
  // parameters hold (the parameter itself, a field of it, or a binding that
  // carries one)?
  const carriesOf = ({ fn, at }) => {
    const params = new Set();
    const fields = new Set();
    fn.params.forEach((p, i) => {
      if (!at.has(i)) return;
      if (p.type === 'Identifier') params.add(p.name);
      else patternNames(p).forEach((x) => fields.add(x));
    });
    const carries = (e) => {
      let hit = false;
      visit(e, (n, parent, key) => {
        if (!hit && isReference(n, parent, key) && (params.has(n.name) || fields.has(n.name))) hit = true;
      });
      return hit;
    };
    for (let grew = true; grew;) {
      grew = false;
      const add = (names) => { for (const x of names) if (!fields.has(x) && !params.has(x)) { fields.add(x); grew = true; } };
      visit(fn.body, (n) => {
        if (n.type === 'VariableDeclarator' && n.init && carries(n.init)) add(patternNames(n.id));
        if ((n.type === 'ForOfStatement' || n.type === 'ForInStatement') && carries(n.right)) {
          add(n.left.type === 'VariableDeclaration' ? n.left.declarations.flatMap((d) => patternNames(d.id)) : patternNames(n.left));
        }
        if (n.type === 'CallExpression' && (carries(n.callee) || n.arguments.some((a) => !isFn(a) && carries(a)))) {
          for (const a of n.arguments) if (isFn(a)) add(a.params.flatMap((q) => patternNames(q)));
        }
      });
    }
    return carries;
  };
  // Delegation: a bare call to a function of this module with an argument
  // that carries a receiving parameter.
  for (let grew = true; grew;) {
    grew = false;
    for (const r of [...receivers.values()]) {
      const carries = carriesOf(r);
      visit(r.fn.body, (n) => {
        if (n.type !== 'CallExpression' || n.callee.type !== 'Identifier') return;
        for (const callee of local.get(n.callee.name) ?? []) {
          const at = [];
          n.arguments.forEach((a, i) => {
            if (a.type !== 'SpreadElement') { if (carries(a)) at.push(i); } else if (carries(a.argument)) for (let j = i; j < callee.params.length; j++) at.push(j);
          });
          if (receive(n.callee.name, callee, at)) grew = true;
        }
      });
    }
  }
  for (const r of receivers.values()) {
    const { name, fn } = r;
    if (!r.at.size) continue;
    const carries = carriesOf(r);
    const own = new Set(fn.params.flatMap((p) => patternNames(p)));
    const locals = new Set();
    visit(fn.body, (n) => { if (n.type === 'VariableDeclarator') patternNames(n.id).forEach((x) => locals.add(x)); });
    const keys = [];
    const whole = (t) => keys.push(...tableKeys(t).map((k) => `${t}.${k}`));
    const moduleBinding = (t) => t && facts.top.has(t) && !locals.has(t) && !own.has(t);
    // Whatever lands is injection only when the module calls what landed
    // there; otherwise it is state taken from a data argument (turnState.tool
    // = ev.tool, pending[id] = { tex }). A copy keyed by a runtime value
    // (deps[k] = impl[k]) lands the whole table, if anything is called
    // through it. A copy to a static target (slot = x.f, deps.a = x.a) lands
    // its path; a whole binding (deps = x, deps = { ...x }) the whole table,
    // when a function is called straight off it (deps.f(), not el.classList.add()).
    // A runtime-keyed write into a list of data (listOfData: jobs[i] = job,
    // store.jobs[i] = job) is an element, not the table landing.
    const land = (target) => {
      const t = rootIdent(target);
      if (!moduleBinding(t)) return;
      if (target.type === 'MemberExpression' && propName(target) === null) { if ((calledRoots.has(t) || elementCalled.has(memberPath(target.object))) && !listOfData(target.object)) whole(t); return; }
      const path = memberPath(target);
      if (path && calledPaths.has(path)) keys.push(path);
      else if (target.type === 'Identifier' && [...calledPaths].some((c) => c.startsWith(path + '.') && !c.slice(path.length + 1).includes('.'))) whole(t);
    };
    visit(fn.body, (n) => {
      if (n.type === 'AssignmentExpression' && carries(n.right)) land(n.left);
      // Object.assign(T, p) copies p's runtime keys: the whole table; so does
      // a spread of p in a literal source. Any other literal property is a
      // static copy to T.key.
      if (n.type === 'CallExpression' && memberPath(n.callee) === 'Object.assign' && n.arguments.length > 1) {
        const t = rootIdent(n.arguments[0]);
        if (!moduleBinding(t)) return;
        for (const src of n.arguments.slice(1)) {
          if (src.type !== 'ObjectExpression') {
            if (carries(src)) whole(t);
            continue;
          }
          for (const q of src.properties) {
            if (q.type === 'SpreadElement') { if (carries(q.argument)) whole(t); continue; }
            const k = propName({ computed: q.computed, property: q.key });
            const path = `${memberPath(n.arguments[0]) ?? t}.${k}`;
            if (k !== null && carries(q.value) && calledPaths.has(path)) keys.push(path);
          }
        }
      }
    });
    if (!keys.length) continue;
    // The shell table is counted where it is used (one per shell.X slot a
    // module calls), not again where registerShell fills it.
    if (facts.file === SHELL_MODULE && name === 'registerShell') continue;
    if (allow.includes(`${facts.file}:${name}`)) { facts.allowHits.add(`${facts.file}:${name}`); continue; }
    if (!facts.receivers.has(name)) facts.receivers.set(name, { line: fn.loc.start.line, keys: [] });
    facts.receivers.get(name).keys.push(...keys);
    for (const k of keys) if (!facts.injections.has(k)) facts.injections.set(k, name);
  }
  // A registry (T.*) is read by runtime key; it has no static key to miss.
  for (const k of facts.injections.keys()) if (!k.endsWith('.*') && !reads.has(k)) facts.deadInjections.push(k);
}

const ARRAY_METHODS = new Set(Object.getOwnPropertyNames(Array.prototype).filter((k) => k !== 'constructor' && typeof Array.prototype[k] === 'function'));

// elementOf: the list path an expression takes an element of, by a runtime
// key or a call on the list: X[i], S.hs[i], X.find(f), X.at(0). A static
// index (X[0]) is a member path, which listOfData's method check rejects.
function elementOf(n) {
  if (n.type === 'MemberExpression' && propName(n) === null) return memberPath(n.object);
  if (n.type === 'CallExpression' && n.callee.type === 'MemberExpression') return memberPath(n.callee.object);
  return null;
}

// callsDirectly: body calls one of names (h(), h.call(), h.apply()).
function callsDirectly(body, names) {
  let hit = false;
  visit(body, (n) => {
    if (hit || n.type !== 'CallExpression') return;
    const c = n.callee.type === 'MemberExpression' && ['call', 'apply'].includes(propName(n.callee)) ? n.callee.object : n.callee;
    if (c.type === 'Identifier' && names.includes(c.name)) hit = true;
  });
  return hit;
}

// isListLiteral: n is a module binding, or a static member path into one,
// whose initialiser is an array literal (const jobs = [], const s = { jobs: [] }).
function isListLiteral(facts, n) {
  const path = memberPath(n)?.split('.');
  let v = path && facts.top.get(path[0])?.init;
  for (const k of path?.slice(1) ?? []) {
    v = v?.type === 'ObjectExpression' ? v.properties.find((q) => q.type === 'Property' && propName({ computed: q.computed, property: q.key }) === k)?.value : undefined;
  }
  return v?.type === 'ArrayExpression';
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
export function analyzeSources(sources, espree = loadEspree(), caps = NO_CAPS) {
  const out = {};
  for (const [f, src] of Object.entries(sources)) out[f] = analyzeProgram(espree.parse(src, PARSE), f, caps);
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
// an injected table, a late-binding table or the shell (deps.f(...),
// hooks.f(...), shell.f(...)) resolves by the property name, the only handle
// such a call has. Any other member call (el.f(...), obj.render(...)) is not
// resolved: a same-named wrapper elsewhere says nothing about it.
function sinkWrappers(all) {
  const key = (f, fn) => `${f}:${fn.name ?? '@' + fn.node.loc.start.line + ':' + fn.node.loc.start.column}`;
  const wrappers = new Map(); // key -> { file, fn, html: Set of parameter indices }
  const byNode = new Map();
  for (const [f, facts] of Object.entries(all)) for (const fn of facts.fns) byNode.set(fn.node, fn);
  const find = (pred) => { for (const w of wrappers.values()) if (pred(w)) return w; return null; };
  // byName: per file, the bindings a call resolves through by property name.
  const byName = new Map(Object.entries(all).map(([f, facts]) => [f, new Set([
    ...facts.tables, ...facts.shellNames, ...[...facts.injections.keys()].map((k) => k.split('.')[0]),
  ])]));
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
    return byName.get(f).has(rootIdent(callee.object)) ? find((w) => w.fn.name === p) : null;
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
function upcalls(all, graph, shellRoots) {
  const forwarders = [];
  const notUp = [];
  const owner = new Map();
  for (const [f, facts] of Object.entries(all)) {
    for (const { arg, line } of facts.shellRegs) {
      if (!shellRoots.includes(f)) { forwarders.push(`${f}:${line}: registerShell outside the root modules (${shellRoots.join(', ')})`); continue; }
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
    for (const line of facts.shellOpaque) notUp.push(`${f}:${line}: shell read other than as shell.X or const { X } = shell, so its slot is unknown`);
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
export function measureGlobal(all, graph, caps = NO_CAPS) {
  const { leaves } = caps;
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
    for (const { local, from, line } of facts.nsOpaque) {
      const how = local === null ? `import() of ${from} read as a value` : `${local} (import * from ${from}) read other than as ${local}.X or const { X } = ${local}`;
      details.unresolvedImports.push(`${f}:${line}: ${how}, so the late-binding table it reaches is unknown`);
    }
  }
  const leafSet = new Set(leaves);
  details.leafImports = [];
  for (const f of leaves) {
    for (const to of graph[f] ?? []) if (graph[to] && !leafSet.has(to)) details.leafImports.push(`${f} imports ${to}, which is not a leaf`);
  }
  const { forwarders, notUp } = upcalls(all, graph, caps.shellRoots);
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

export function measureSource(src, espree = loadEspree(), file = '', caps = NO_CAPS) {
  const lines = src.split('\n');
  const total = lines.length - (src.endsWith('\n') ? 1 : 0);
  const program = espree.parse(src, PARSE);
  const { fns, iifes } = functions(program);
  let letVar = 0;
  for (const line of lines) if (/^(?:let|var)\s/.test(line)) letVar++;
  return {
    lines: total,
    maxFnLines: fns.reduce((m, f) => Math.max(m, f.lines), 0),
    maxIifeLines: Math.max(0, ...iifes),
    fnOver100: fns.filter((f) => f.lines > 100).length,
    topLevelLetVar: letVar,
    ...perFileMetrics(analyzeProgram(program, file, caps)),
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
// findings that fail the run outright (receiverProblems); details back each
// _global count.
function snapshot(caps) {
  const espree = loadEspree();
  const sources = readSources();
  const all = analyzeSources(sources, espree, caps);
  const out = {};
  for (const f of Object.keys(sources)) out[f] = measureSource(sources[f], espree, f, caps);
  const graph = importGraph(espree);
  const { metrics, details } = measureGlobal(all, graph, caps);
  out[GLOBAL] = metrics;
  return { current: out, graph, details, problems: receiverProblems(all, caps) };
}

// receiverProblems closes the set of injection receivers (S20k): besides
// shell.js's registerShell, only a caps.injectionAllow or caps.injectionLegacy
// entry may receive one, and an entry no receiver matches is stale.
export function receiverProblems(all, caps) {
  const legacy = caps.injectionLegacy;
  const allowHits = new Set(Object.values(all).flatMap((a) => [...a.allowHits]));
  const problems = caps.injectionAllow.filter((a) => !allowHits.has(a)).map((a) => `caps.injectionAllow: ${a} no longer receives an injection — drop it from the list`);
  const legacyHits = new Set();
  for (const [f, facts] of Object.entries(all)) {
    for (const [name, { line, keys }] of facts.receivers) {
      const id = `${f}:${name}`;
      if (legacy.includes(id)) { legacyHits.add(id); continue; }
      problems.push(`${f}:${line}: ${name} receives injected dependencies (${keys.join(', ')}); import them, or upcall through shell.X (caps.injectionLegacy is shrink-only)`);
    }
  }
  for (const a of legacy) if (!legacyHits.has(a)) problems.push(`caps.injectionLegacy: ${a} no longer receives an injection — drop it from the list`);
  return problems;
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
  for (const key of ['maxFnLines', 'lines', 'sideEffectLegacy', 'cycleLegacy', 'leaves', 'lateBindingTables', 'injectionAllow', 'injectionLegacy', 'shellRoots']) {
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
  const tables = raw.lateBindingTables;
  if (typeof tables !== 'object' || tables === null || Array.isArray(tables) || !Object.values(tables).every((m) => typeof m === 'string')) {
    errors.push('caps.lateBindingTables must map a table name to the module that exports it');
  }
  const receiverList = (v) => Array.isArray(v) && v.every((a) => typeof a === 'string' && /^[^:]+\.js:[A-Za-z_$][\w$]*$/.test(a));
  if (!receiverList(raw.injectionAllow)) {
    errors.push('caps.injectionAllow must be an array of "file.js:function"');
  }
  // Required even once drained ([]): a dropped section would come back as a
  // new one, which tools/ratchet-raises records without a raise.
  if (!receiverList(raw.injectionLegacy)) {
    errors.push('caps.injectionLegacy must be an array of "file.js:function"');
  }
  if (!Array.isArray(raw.shellRoots)) errors.push('caps.shellRoots must be an array');
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
  for (const f of caps.shellRoots ?? []) {
    if (!exists(f)) problems.push(`caps.shellRoots: ${f} does not exist in static/`);
  }
  for (const [t, f] of Object.entries(caps.lateBindingTables ?? {})) {
    if (!exists(f)) problems.push(`caps.lateBindingTables.${t}: ${f} does not exist in static/`);
  }
  for (const list of ['injectionAllow', 'injectionLegacy']) {
    for (const a of caps[list] ?? []) {
      const f = a.slice(0, a.indexOf(':'));
      if (!exists(f)) problems.push(`caps.${list}: ${f} does not exist in static/`);
    }
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
  const snap = snapshot(caps ?? NO_CAPS);
  const mode = process.argv[2] ?? '--check';
  if (mode === '--check') process.exit(check(snap, caps, errors));
  else if (mode === '--write') process.exit(write(snap, caps, errors));
  else {
    console.error(`unknown mode ${mode}; use --check | --write`);
    process.exit(2);
  }
}
