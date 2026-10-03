// eslint-plugin-nz.mjs — dashboard-specific lint rules, loaded by the root
// eslint.config.mjs as a local plugin (no npm dependency).
//
// Shared state between the dashboard's ES modules has one mechanism: the
// owning module exports a const object and every reader dereferences its
// fields at the use site. These rules close the two other routes that copy a
// value instead of sharing it:
//
//   nz/configure-deps          configureX({ … }) and registerShell({ … })
//                               may only hand over functions and const
//                               bindings. A let, an object field or a literal
//                               is copied into the callee's table once and
//                               goes stale when the owner reassigns it.
//   nz/deps-keys               a module's `deps = { … }` literal is the list
//                               of what it is handed; deps.X (or a
//                               destructured X) naming no key of it is a call
//                               that throws only when its path runs.
//   nz/no-exported-let         an exported let is a second sharing mechanism
//                               (an ES live binding importers can read but
//                               not write); export a const state object or a
//                               function instead.
//   nz/no-module-side-effects  a module may declare state (functions,
//                               classes, const bindings whose initialiser is
//                               a pure expression) at load time, but may not
//                               run anything — a module graph where "import"
//                               means "run arbitrary code in this order" is
//                               not one a reader can hold in their head.
//                               wsm.on / onReady / onStateChange / onAuthFail
//                               are the one standing exception (D2, #3024
//                               R2): the WS dispatch table's registration
//                               calls, whose own shape check-ws-receivers.mjs
//                               owns. The caller passes the files this rule
//                               should skip (scripts/js-ratchet.caps.json's
//                               sideEffectLegacy — eslint.config.mjs reads it).
//
// Tests: node --test scripts/eslint-plugin-nz.test.mjs

const CONFIGURE_RE = /^configure[A-Z]/;

// tableCallee names the call when it hands a table over: configureX(…), or
// registerShell(…) called bare or through a namespace (S.registerShell).
function tableCallee(callee) {
  if (callee.type === 'Identifier') return CONFIGURE_RE.test(callee.name) || callee.name === 'registerShell' ? callee.name : null;
  if (callee.type === 'MemberExpression' && !callee.computed && callee.property.type === 'Identifier' && callee.property.name === 'registerShell') return 'registerShell';
  return null;
}

function findVariable(scope, name) {
  for (let s = scope; s; s = s.upper) {
    const v = s.set.get(name);
    if (v) return v;
  }
  return null;
}

// isSharedBinding reports whether an injected identifier names something that
// cannot go stale: a function, a class, a const, or an import (the exporting
// module cannot export a let — see no-exported-let).
function isSharedBinding(variable) {
  if (!variable || variable.defs.length === 0) return false;
  const def = variable.defs[0];
  switch (def.type) {
    case 'FunctionName':
    case 'ClassName':
    case 'ImportBinding':
      return true;
    case 'Variable':
      return def.parent.kind === 'const';
    default:
      return false;
  }
}

const configureDeps = {
  meta: {
    type: 'problem',
    docs: { description: 'configureX deps and registerShell slots may only be functions or const bindings' },
    schema: [],
    messages: {
      mutable: "'{{name}}' is a {{kind}}: {{callee}} would keep a copy. Inject a function, or move the value into a const state object the module imports.",
      notBinding: '{{callee}} values must be functions or const bindings; a {{type}} is copied once. Read shared state from its const state object instead.',
    },
  },
  create(context) {
    const sourceCode = context.sourceCode;
    return {
      CallExpression(node) {
        const callee = tableCallee(node.callee);
        if (!callee) return;
        const arg = node.arguments[0];
        if (!arg || arg.type !== 'ObjectExpression') return;
        for (const prop of arg.properties) {
          if (prop.type !== 'Property') {
            context.report({ node: prop, messageId: 'notBinding', data: { callee, type: prop.type } });
            continue;
          }
          const value = prop.value;
          if (value.type === 'FunctionExpression' || value.type === 'ArrowFunctionExpression') continue;
          if (value.type !== 'Identifier') {
            context.report({ node: value, messageId: 'notBinding', data: { callee, type: value.type } });
            continue;
          }
          const variable = findVariable(sourceCode.getScope(value), value.name);
          if (isSharedBinding(variable)) continue;
          const def = variable && variable.defs[0];
          const kind = !def ? 'global' : def.type === 'Variable' ? def.parent.kind : def.type;
          context.report({ node: value, messageId: 'mutable', data: { name: value.name, kind, callee } });
        }
      },
    };
  },
};

// propKey: the static name of a non-computed (or literal-keyed) property, or
// null.
function propKey(p) {
  if (p.computed && p.key.type !== 'Literal') return null;
  if (p.key.type === 'Identifier' || p.key.type === 'PrivateIdentifier') return p.key.name;
  return p.key.type === 'Literal' ? String(p.key.value) : null;
}

// depsKeys looks only at a top-level binding named `deps` initialised with an
// object literal of static keys (every receiver module's convention); a
// spread or computed key leaves the key set unknown and the rule silent. A
// read through a shadowing `deps` (a parameter, an inner binding) is not the
// table's and is skipped. Computed reads (deps[k] = impl[k], the receiver's
// copy loop) have no static key to check.
const depsKeys = {
  meta: {
    type: 'problem',
    docs: { description: "deps.X must name a key of the module's deps table" },
    schema: [],
    messages: {
      unknownKey: "'{{key}}' is not a key of this module's deps table ({{keys}}): declare and inject it, or import it.",
    },
  },
  create(context) {
    const sourceCode = context.sourceCode;
    let table = null; // { variable, keys: Set }
    const check = (node, scopeNode, key) => {
      if (!table || findVariable(sourceCode.getScope(scopeNode), 'deps') !== table.variable) return;
      if (key !== null && !table.keys.has(key)) context.report({ node, messageId: 'unknownKey', data: { key, keys: [...table.keys].join(', ') || 'empty' } });
    };
    const checkPattern = (pattern, init) => {
      if (pattern?.type !== 'ObjectPattern' || init?.type !== 'Identifier' || init.name !== 'deps') return;
      for (const p of pattern.properties) if (p.type === 'Property') check(p.key, init, propKey(p));
    };
    return {
      Program(program) {
        for (const st of program.body) {
          const decl = st.type === 'ExportNamedDeclaration' ? st.declaration : st;
          if (decl?.type !== 'VariableDeclaration') continue;
          for (const d of decl.declarations) {
            if (d.id.type !== 'Identifier' || d.id.name !== 'deps' || d.init?.type !== 'ObjectExpression') continue;
            const keys = d.init.properties.map((p) => (p.type === 'Property' ? propKey(p) : null));
            if (keys.includes(null)) return;
            table = { variable: sourceCode.getDeclaredVariables(d)[0], keys: new Set(keys) };
          }
        }
      },
      MemberExpression(node) {
        if (node.object.type !== 'Identifier' || node.object.name !== 'deps') return;
        if (node.computed && node.property.type !== 'Literal') return;
        check(node.property, node, node.computed ? String(node.property.value) : node.property.name);
      },
      VariableDeclarator(node) { checkPattern(node.id, node.init); },
      AssignmentExpression(node) { checkPattern(node.left, node.right); },
    };
  },
};

function boundNames(pattern, out) {
  switch (pattern.type) {
    case 'Identifier': out.push(pattern); break;
    case 'ObjectPattern': for (const p of pattern.properties) boundNames(p.type === 'RestElement' ? p.argument : p.value, out); break;
    case 'ArrayPattern': for (const e of pattern.elements) if (e) boundNames(e.type === 'RestElement' ? e.argument : e, out); break;
    case 'AssignmentPattern': boundNames(pattern.left, out); break;
    default: break;
  }
  return out;
}

const noExportedLet = {
  meta: {
    type: 'problem',
    docs: { description: 'modules export const state objects or functions, never a let' },
    schema: [],
    messages: {
      exportedLet: "'{{name}}' is an exported {{kind}}. Export a const state object (or a function) and mutate its fields instead.",
    },
  },
  create(context) {
    return {
      Program(program) {
        const mutable = new Map(); // name -> kind, top-level non-const bindings
        for (const stmt of program.body) {
          const exported = stmt.type === 'ExportNamedDeclaration';
          const decl = exported ? stmt.declaration : stmt;
          if (!decl || decl.type !== 'VariableDeclaration' || decl.kind === 'const') continue;
          for (const d of decl.declarations) {
            for (const id of boundNames(d.id, [])) {
              if (exported) context.report({ node: id, messageId: 'exportedLet', data: { name: id.name, kind: decl.kind } });
              else mutable.set(id.name, decl.kind);
            }
          }
        }
        for (const stmt of program.body) {
          if (stmt.type !== 'ExportNamedDeclaration' || stmt.source) continue;
          for (const spec of stmt.specifiers) {
            const kind = mutable.get(spec.local.name);
            if (kind) context.report({ node: spec, messageId: 'exportedLet', data: { name: spec.local.name, kind } });
          }
        }
      },
    };
  },
};

// isPureExpr reports whether evaluating node runs no call escaping the small
// whitelist below and writes nothing (no assignment, update or `delete`). It
// is a syntactic check, not a proof of purity: reading a property, global or
// DOM (`document.title`) is allowed, and so are the forms that can reach user
// code only through a value the rule does not resolve — a getter behind a
// name, and implicit ToPrimitive (a template literal, a binary operator, a
// computed key or `new RegExp(o)` applied to an object whose toString /
// valueOf runs code). Deliberately recursive on object/array literals and new/Object.*
// so `Object.freeze({ a: new Set([1]) })` is still pure. The sub-expressions
// that run at evaluation time are checked: computed member properties and
// object keys, a class's heritage, computed member keys, static field
// initialisers and static blocks, and (isPurePattern) a destructuring
// pattern's defaults and computed keys. Object.assign/freeze/seal mutate their
// first argument, so it must be a fresh object or array literal. Reading a
// property can run a getter: a spread or Object.assign source written as a
// literal with get/set accessors is rejected (definesAccessor), but a
// property read through a name (`o.x`, `{ ...o }`) is not resolved to what
// the name holds — the rule is syntactic and does not track bindings.
function isPureExpr(node) {
  if (!node) return true;
  switch (node.type) {
    case 'Literal':
    case 'Identifier':
    case 'FunctionExpression':
    case 'ArrowFunctionExpression':
      return true;
    case 'ClassExpression':
    case 'ClassDeclaration':
      return isPureClass(node);
    case 'TemplateLiteral':
      return node.expressions.every(isPureExpr);
    case 'ObjectExpression':
      return node.properties.every((p) => (p.type === 'SpreadElement'
        ? isPureExpr(p.argument) && !definesAccessor(p.argument)
        : (!p.computed || isPureExpr(p.key)) && isPureExpr(p.value)));
    case 'ArrayExpression':
      return node.elements.every((e) => e === null || isPureExpr(e));
    case 'UnaryExpression':
      // `delete window.x` mutates whatever the operand names.
      return node.operator !== 'delete' && isPureExpr(node.argument);
    case 'BinaryExpression':
    case 'LogicalExpression':
      return isPureExpr(node.left) && isPureExpr(node.right);
    case 'ConditionalExpression':
      return isPureExpr(node.test) && isPureExpr(node.consequent) && isPureExpr(node.alternate);
    case 'MemberExpression':
      return isPureExpr(node.object) && (!node.computed || isPureExpr(node.property));
    case 'NewExpression':
      return node.callee.type === 'Identifier' && /^(Set|Map|WeakMap|WeakSet|RegExp)$/.test(node.callee.name)
        && node.arguments.every(isPureExpr);
    case 'CallExpression':
      return isPureObjectCall(node) && node.arguments.every(isPureExpr);
    default:
      return false;
  }
}

// isPureObjectCall reports Object.create(...), or Object.assign/freeze/seal
// whose target is a literal created right there (so nothing outside is
// mutated).
// Object.assign reads every source's properties and writes the target's, so
// a source literal's getter, or a target literal's setter, would run.
function isPureObjectCall(node) {
  const c = node.callee;
  if (c.type !== 'MemberExpression' || c.computed || c.object.type !== 'Identifier' || c.object.name !== 'Object') return false;
  if (c.property.name === 'create') return true;
  if (c.property.name !== 'assign' && c.property.name !== 'freeze' && c.property.name !== 'seal') return false;
  const target = node.arguments[0];
  if (c.property.name === 'assign' && node.arguments.some(definesAccessor)) return false;
  return !!target && (target.type === 'ObjectExpression' || target.type === 'ArrayExpression');
}

// definesAccessor reports an object literal (or a conditional / logical
// choice between expressions that may be one) with a get or set property:
// copying its properties — a spread, an Object.assign source — calls the
// getter, and Object.assign onto it calls the setter (a setter-only literal
// spread runs nothing, but is rejected too: one rule is easier to hold than
// the distinction). Binding such a literal to a name, or freezing it, runs
// nothing and stays pure.
function definesAccessor(node) {
  switch (node.type) {
    case 'ObjectExpression':
      return node.properties.some((p) => p.type === 'Property' && (p.kind === 'get' || p.kind === 'set'));
    case 'ConditionalExpression':
      return definesAccessor(node.consequent) || definesAccessor(node.alternate);
    case 'LogicalExpression':
      return definesAccessor(node.left) || definesAccessor(node.right);
    default:
      return false;
  }
}

// isPurePattern reports whether binding a declarator's id runs nothing
// beyond the initialiser: a default (`{ a = init() }`, `[b = init()]`) and a
// computed key (`{ [init()]: c }`) are evaluated at load time too.
function isPurePattern(node) {
  switch (node.type) {
    case 'Identifier':
      return true;
    case 'AssignmentPattern':
      return isPurePattern(node.left) && isPureExpr(node.right);
    case 'RestElement':
      return isPurePattern(node.argument);
    case 'ArrayPattern':
      return node.elements.every((e) => e === null || isPurePattern(e));
    case 'ObjectPattern':
      return node.properties.every((p) => (p.type === 'RestElement'
        ? isPurePattern(p)
        : (!p.computed || isPureExpr(p.key)) && isPurePattern(p.value)));
    default:
      return false;
  }
}

// isPureClass reports whether defining the class runs nothing: instance
// field initialisers and method bodies run later, but the heritage clause,
// computed member keys, static field initialisers and static blocks run now.
function isPureClass(node) {
  if (!isPureExpr(node.superClass)) return false;
  return node.body.body.every((m) => {
    if (m.type === 'StaticBlock') return false;
    if (m.computed && !isPureExpr(m.key)) return false;
    return !(m.type === 'PropertyDefinition' && m.static && !isPureExpr(m.value));
  });
}

// isWsmRegistration reports the one standing exception: a top-level call
// that registers a handler or lifecycle hook on the managed wsm object (D2).
// Its arguments must be pure too — the registration is exempt, not whatever
// `wsm.on(X, init())` would evaluate to build the handler. "The managed wsm
// object" is checked by binding, not by name: `wsm` must be
// `import { wsm } from './ws_manager.js'`, or ws_manager.js's own top-level
// `const wsm` — a module-local `const wsm = { on: init }` is not exempt.
function isWsmRegistration(expr, sourceCode, filename) {
  if (expr.type !== 'CallExpression') return false;
  const c = expr.callee;
  if (!(c.type === 'MemberExpression' && !c.computed && c.object.type === 'Identifier' && c.object.name === 'wsm'
    && /^(on|onReady|onStateChange|onAuthFail)$/.test(c.property.name)
    && expr.arguments.every(isPureExpr))) return false;
  const v = findVariable(sourceCode.getScope(c.object), 'wsm');
  const def = v?.defs[0];
  if (!def) return false;
  if (def.type === 'ImportBinding') {
    return def.node.type === 'ImportSpecifier' && def.node.imported.name === 'wsm'
      && /(^|\/)ws_manager\.js$/.test(def.parent.source.value);
  }
  return /(^|[\\/])ws_manager\.js$/.test(filename) && def.type === 'Variable' && def.parent.kind === 'const';
}

const noModuleSideEffects = {
  meta: {
    type: 'problem',
    docs: { description: 'a module may declare state at load time but may not run anything (D-S19)' },
    schema: [],
    messages: {
      sideEffect: 'top-level {{what}} runs code at import time; a module may only declare functions, classes, imports/exports, and const bindings whose initialiser is a pure expression (object/array/new Set|Map|RegExp/Object.freeze|seal|create|assign).',
    },
  },
  create(context) {
    // checkOne reports st (a Program-level statement, or the declaration an
    // `export` wraps — `export const y = init()` launders the same call
    // through a declaration ExportNamedDeclaration otherwise waves through).
    function checkOne(st) {
      switch (st.type) {
        case 'ImportDeclaration':
        case 'FunctionDeclaration':
          return;
        case 'ClassDeclaration':
          if (!isPureClass(st)) context.report({ node: st, messageId: 'sideEffect', data: { what: 'class definition' } });
          return;
        case 'VariableDeclaration': {
          const bad = st.declarations.find((d) => d.init !== null && !isPureExpr(d.init));
          if (bad) {
            context.report({ node: bad.init, messageId: 'sideEffect', data: { what: `'${st.kind}' initialiser` } });
            return;
          }
          const badPattern = st.declarations.find((d) => !isPurePattern(d.id));
          if (badPattern) context.report({ node: badPattern.id, messageId: 'sideEffect', data: { what: `'${st.kind}' destructuring default or computed key` } });
          return;
        }
        case 'ExpressionStatement':
          if (isWsmRegistration(st.expression, context.sourceCode, context.filename)) return;
          context.report({ node: st, messageId: 'sideEffect', data: { what: 'statement' } });
          return;
        default:
          context.report({ node: st, messageId: 'sideEffect', data: { what: st.type.replace(/Statement$/, '').toLowerCase() || st.type } });
      }
    }
    return {
      Program(program) {
        for (const st of program.body) {
          switch (st.type) {
            case 'ExportNamedDeclaration':
              if (st.declaration) checkOne(st.declaration);
              continue;
            case 'ExportDefaultDeclaration':
              if (/Function/.test(st.declaration.type)) continue;
              if (!isPureExpr(st.declaration)) context.report({ node: st.declaration, messageId: 'sideEffect', data: { what: 'default export' } });
              continue;
            case 'ExportAllDeclaration':
              continue;
            default:
              checkOne(st);
          }
        }
      },
    };
  },
};

export default {
  meta: { name: 'eslint-plugin-nz' },
  rules: {
    'configure-deps': configureDeps,
    'deps-keys': depsKeys,
    'no-exported-let': noExportedLet,
    'no-module-side-effects': noModuleSideEffects,
  },
};
