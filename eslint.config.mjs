// eslint.config.mjs — no-undef gate for the dashboard's classic scripts
// (internal/server/static/*.js, loaded as plain <script defer> sharing one
// global scope).
//
// Each file's `globals` block is its explicit dependency whitelist: every
// cross-file name it may reference bare (top-level declarations and window.X
// exports of the other files). The lists are frozen output of
// `node scripts/js-deps-freeze.mjs --globals` — when a cross-file reference
// is removed, delete it here (and refresh scripts/js-deps-baseline.json);
// new entries need the same scrutiny as a new API.
//
// Dependency-free on purpose: the npm install lives in test/e2e, so this root
// config cannot import packages (run it as
// `test/e2e/node_modules/.bin/eslint internal/server/static`).

const ro = (names) => Object.fromEntries(names.map((n) => [n, 'readonly']));
const rw = (names) => Object.fromEntries(names.map((n) => [n, 'writable']));

// Browser APIs the dashboard actually uses. ECMAScript built-ins (Promise,
// JSON, Math, …) come from languageOptions.ecmaVersion and are not listed.
const browserGlobals = ro([
  'window',
  'document',
  'navigator',
  'location',
  'history',
  'localStorage',
  'sessionStorage',
  'fetch',
  'WebSocket',
  'EventSource',
  'URL',
  'URLSearchParams',
  'setTimeout',
  'clearTimeout',
  'setInterval',
  'clearInterval',
  'requestAnimationFrame',
  'cancelAnimationFrame',
  'queueMicrotask',
  'console',
  'alert',
  'confirm',
  'prompt',
  'getComputedStyle',
  'matchMedia',
  'performance',
  'crypto',
  'atob',
  'btoa',
  'structuredClone',
  'AbortController',
  'CustomEvent',
  'EventTarget',
  'Event',
  'KeyboardEvent',
  'MouseEvent',
  'PointerEvent',
  'TouchEvent',
  'ClipboardItem',
  'DOMParser',
  'FormData',
  'Blob',
  'File',
  'FileReader',
  'Image',
  'Audio',
  'AudioContext',
  'MediaRecorder',
  'Notification',
  'IntersectionObserver',
  'ResizeObserver',
  'MutationObserver',
  'TextEncoder',
  'TextDecoder',
  'XMLHttpRequest',
  'HTMLElement',
  'Element',
  'Node',
  'NodeFilter',
  'Option',
  'visualViewport',
  'speechSynthesis',
  'SpeechSynthesisUtterance',
  'webkitSpeechRecognition',
  'caches',
  'indexedDB',
  'CSS',
  'createImageBitmap',
  // Generated contract global (contract.js loads first, #2539).
  'NZ_CONTRACT',
]);

// Cross-file whitelists (js-deps-freeze --globals output, frozen 2026-09-05).
// `writable` marks names another file assigns (shared mutable state — the
// most dangerous edges; D3 aims to drive these to zero).
const deps = {
  'nz_util.js': {},
  'render_md.js': {},
  'self_update.js': {},
  'voice.js': {},
  'session_header.js': {},
  'composer_files.js': {},
  'mobile_nav.js': {},
  'dashboard.js': {},
  'cron_view.js': {},
  // ES module since D3 PR-B: utilities and nz.state come in via import;
  // dashboard globals are window.* dereferences.
  'agent_view.js': {},
  // ES modules since D3 PR-A: cross-file consumption is explicit (import /
  // window.* deref), so no bare-global whitelist.
  'asset_browser.js': {},
  'files_view.js': {},
};

// Files migrated to ES modules (D3, docs/rfc/dashboard-es-modules.md).
// sourceType 'module' makes no-undef a real scope check for them.
const moduleFiles = new Set(['nz_util.js', 'render_md.js', 'self_update.js', 'voice.js', 'session_header.js', 'composer_files.js', 'mobile_nav.js', 'dashboard.js', 'agent_view.js', 'asset_browser.js', 'files_view.js', 'cron_view.js']);

const perFile = Object.entries(deps).map(([file, globals]) => ({
  files: [`internal/server/static/${file}`],
  languageOptions: {
    globals,
    ...(moduleFiles.has(file) ? { sourceType: 'module' } : {}),
  },
}));

export default [
  {
    files: ['internal/server/static/*.js'],
    ignores: ['internal/server/static/sw.js', 'internal/server/static/contract.js'],
    languageOptions: {
      ecmaVersion: 2022,
      sourceType: 'script',
      globals: browserGlobals,
    },
    linterOptions: {
      reportUnusedDisableDirectives: 'error',
    },
    rules: {
      'no-undef': 'error',
      // vars:'local' skips top-level (shared-scope) names — other files may be
      // their only consumer; locals inside functions/IIFEs are still checked.
      'no-unused-vars': ['error', { vars: 'local', args: 'none', caughtErrors: 'none' }],
      // Backend API paths come from the generated NZ_CONTRACT.API table
      // (#2539); a new hardcoded '/api/…' literal bypasses the contract.
      'no-restricted-syntax': ['error', {
        selector: "Literal[value=/^\\u002Fapi\\u002F/]",
        message: 'use NZ_CONTRACT.API.* (generated contract.js) instead of a hardcoded /api/ path',
      }, {
        // D3 bridge rule: a top-level `const x = window.y` snapshots the
        // value at load time — for primitives that dashboard.js reassigns
        // (selectedKey et al.) the copy silently goes stale. Bridges must
        // dereference at the call site (`window.fn(...)`, `window.x` inline).
        selector: "Program > VariableDeclaration > VariableDeclarator[init.type='MemberExpression'][init.object.name='window']",
        message: 'no top-level window.* snapshots — dereference window.<name> at the use site (D3 bridge rule)',
      }],
    },
  },
  // sw.js is a service worker: its own scope, no cross-file references.
  {
    files: ['internal/server/static/sw.js'],
    languageOptions: {
      ecmaVersion: 2022,
      sourceType: 'script',
      globals: ro(['self', 'caches', 'fetch', 'console', 'URL']),
    },
    rules: {
      'no-undef': 'error',
      'no-unused-vars': ['error', { vars: 'local', args: 'none', caughtErrors: 'none' }],
    },
  },
  ...perFile,
];
