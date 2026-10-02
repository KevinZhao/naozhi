// eslint-plugin-nz.mjs — dashboard-specific lint rules, loaded by the root
// eslint.config.mjs as a local plugin (no npm dependency).
//
// Shared state between the dashboard's ES modules has one mechanism: the
// owning module exports a const object and every reader dereferences its
// fields at the use site. These rules close the two other routes that copy a
// value instead of sharing it:
//
//   nz/configure-deps          configureX({ … }) may only inject functions
//                               and const bindings. A let, an object field or
//                               a literal is copied into the callee's deps
//                               table once and goes stale when the owner
//                               reassigns it.
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
    docs: { description: 'configureX deps may only be functions or const bindings' },
    schema: [],
    messages: {
      mutable: "'{{name}}' is a {{kind}}: configure{{target}} would keep a copy. Inject a function, or move the value into a const state object the module imports.",
      notBinding: 'configure{{target}} deps must be functions or const bindings; a {{type}} is copied once. Read shared state from its const state object instead.',
    },
  },
  create(context) {
    const sourceCode = context.sourceCode;
    return {
      CallExpression(node) {
        if (node.callee.type !== 'Identifier' || !CONFIGURE_RE.test(node.callee.name)) return;
        const arg = node.arguments[0];
        if (!arg || arg.type !== 'ObjectExpression') return;
        const target = node.callee.name.slice('configure'.length);
        for (const prop of arg.properties) {
          if (prop.type !== 'Property') {
            context.report({ node: prop, messageId: 'notBinding', data: { target, type: prop.type } });
            continue;
          }
          const value = prop.value;
          if (value.type === 'FunctionExpression' || value.type === 'ArrowFunctionExpression') continue;
          if (value.type !== 'Identifier') {
            context.report({ node: value, messageId: 'notBinding', data: { target, type: value.type } });
            continue;
          }
          const variable = findVariable(sourceCode.getScope(value), value.name);
          if (isSharedBinding(variable)) continue;
          const def = variable && variable.defs[0];
          const kind = !def ? 'global' : def.type === 'Variable' ? def.parent.kind : def.type;
          context.report({ node: value, messageId: 'mutable', data: { name: value.name, kind, target } });
        }
      },
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

// isPureExpr reports whether evaluating node can only ever bind a name — no
// DOM/network/global touch, no function call escaping the small whitelist
// below. Deliberately recursive on object/array literals and new/Object.*
// so `Object.freeze({ a: new Set([1]) })` is still pure. The sub-expressions
// that run at evaluation time are checked: computed member properties and
// object keys, a class's heritage, computed member keys, static field
// initialisers and static blocks, and (isPurePattern) a destructuring
// pattern's defaults and computed keys. Object.assign/freeze mutate their
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
      return isPureExpr(node.argument);
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

// isPureObjectCall reports Object.create(...), or Object.assign/freeze whose
// target is a literal created right there (so nothing outside is mutated).
// Object.assign reads every source's properties and writes the target's, so
// a source literal's getter, or a target literal's setter, would run.
function isPureObjectCall(node) {
  const c = node.callee;
  if (c.type !== 'MemberExpression' || c.computed || c.object.type !== 'Identifier' || c.object.name !== 'Object') return false;
  if (c.property.name === 'create') return true;
  if (c.property.name !== 'assign' && c.property.name !== 'freeze') return false;
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
// `wsm.on(X, init())` would evaluate to build the handler.
function isWsmRegistration(expr) {
  if (expr.type !== 'CallExpression') return false;
  const c = expr.callee;
  return c.type === 'MemberExpression' && !c.computed && c.object.type === 'Identifier' && c.object.name === 'wsm'
    && /^(on|onReady|onStateChange|onAuthFail)$/.test(c.property.name)
    && expr.arguments.every(isPureExpr);
}

const noModuleSideEffects = {
  meta: {
    type: 'problem',
    docs: { description: 'a module may declare state at load time but may not run anything (D-S19)' },
    schema: [],
    messages: {
      sideEffect: 'top-level {{what}} runs code at import time; a module may only declare functions, classes, imports/exports, and const bindings whose initialiser is a pure expression (object/array/new Set|Map|RegExp/Object.freeze|create|assign).',
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
          if (isWsmRegistration(st.expression)) return;
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
    'no-exported-let': noExportedLet,
    'no-module-side-effects': noModuleSideEffects,
  },
};
