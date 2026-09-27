// eslint-plugin-nz.mjs — dashboard-specific lint rules, loaded by the root
// eslint.config.mjs as a local plugin (no npm dependency).
//
// Shared state between the dashboard's ES modules has one mechanism: the
// owning module exports a const object and every reader dereferences its
// fields at the use site. These rules close the two other routes that copy a
// value instead of sharing it:
//
//   nz/configure-deps   configureX({ … }) may only inject functions and const
//                       bindings. A let, an object field or a literal is
//                       copied into the callee's deps table once and goes
//                       stale when the owner reassigns it.
//   nz/no-exported-let  an exported let is a second sharing mechanism (an ES
//                       live binding importers can read but not write); export
//                       a const state object or a function instead.
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

export default {
  meta: { name: 'eslint-plugin-nz' },
  rules: {
    'configure-deps': configureDeps,
    'no-exported-let': noExportedLet,
  },
};
