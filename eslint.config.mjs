// eslint.config.mjs — no-undef gate for the dashboard's ES modules
// (internal/server/static/*.js). Every file except the service worker and
// the generated contract.js is linted as a module with only the browser
// globals below, so a cross-file name must arrive through an import.
//
// Dependency-free on purpose: the npm install lives in test/e2e, so this root
// config cannot import packages (run it as
// `test/e2e/node_modules/.bin/eslint 'internal/server/static/*.js'`).

const ro = (names) => Object.fromEntries(names.map((n) => [n, 'readonly']));

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
  'CSSStyleSheet',
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
]);

export default [
  {
    files: ['internal/server/static/*.js'],
    ignores: ['internal/server/static/sw.js', 'internal/server/static/contract.js'],
    languageOptions: {
      ecmaVersion: 2022,
      sourceType: 'module',
      globals: browserGlobals,
    },
    linterOptions: {
      reportUnusedDisableDirectives: 'error',
    },
    rules: {
      'no-undef': 'error',
      // vars:'local' skips top-level names; locals inside functions are
      // still checked.
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
];
