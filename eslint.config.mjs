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
// `test/e2e/node_modules/.bin/eslint internal/server/static`). The local
// rules in scripts/eslint-plugin-nz.mjs are plain objects, not a package.

import nz from './scripts/eslint-plugin-nz.mjs';
import { loadCaps } from './scripts/js-ratchet.mjs';

// js-ratchet.caps.json's sideEffectLegacy is read here, not duplicated: the
// legacy list for nz/no-module-side-effects must be the one js-ratchet.mjs
// --check also enforces shrink-only (S19-0, #3025).
const { caps, errors } = loadCaps();
if (errors) {
  throw new Error(`eslint.config.mjs: js-ratchet.caps.json is invalid: ${errors.join('; ')}`);
}
const sideEffectLegacy = new Set(caps.sideEffectLegacy);

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
  'split_view.js': {},
  'system_view.js': {},
  'running_banner.js': {},
  'file_refs.js': {},
  'utilities.js': {},
  'discovery.js': {},
  'tuning.js': {},
  'msg_nav.js': {},
  'sidebar_project.js': {},
  'state.js': {},
  'auth_modal.js': {},
  'send_message.js': {},
  'dashboard.js': {},
  'cron_view.js': {},
  'cron_schedule.js': {},
  'cron_timeline.js': {},
  'cron_attention.js': {},
  'cron_live.js': {},
  'platform.js': {},
  'ws_manager.js': {},
  'session_stream.js': {},
  'features.js': {},
  'ask_card.js': {},
  'event_render.js': {},
  'event_stream.js': {},
  'session_list.js': {},
  'session_ident.js': {},
  'icons.js': {},
  'file_ref_parse.js': {},
  'shell.js': {},
  'backend_catalog.js': {},
  'cron_state.js': {},
  'cron_format.js': {},
  'cron_drawer.js': {},
  'cron_trigger.js': {},
  // ES module since D3 PR-B: utilities and shared state come in via import;
  // dashboard globals are window.* dereferences.
  'agent_view.js': {},
  // ES modules since D3 PR-A: cross-file consumption is explicit (import /
  // window.* deref), so no bare-global whitelist.
  'asset_browser.js': {},
  'files_view.js': {},
};

// Files migrated to ES modules (D3, docs/rfc/dashboard-es-modules.md).
// sourceType 'module' makes no-undef a real scope check for them.
const moduleFiles = new Set(['nz_util.js', 'state.js', 'send_message.js', 'auth_modal.js', 'sidebar_project.js', 'msg_nav.js', 'tuning.js', 'discovery.js', 'utilities.js', 'file_refs.js', 'running_banner.js', 'system_view.js', 'split_view.js', 'render_md.js', 'self_update.js', 'voice.js', 'session_header.js', 'composer_files.js', 'mobile_nav.js', 'dashboard.js', 'agent_view.js', 'asset_browser.js', 'files_view.js', 'cron_view.js', 'cron_schedule.js', 'cron_timeline.js', 'cron_drawer.js', 'cron_trigger.js', 'cron_attention.js', 'cron_live.js', 'platform.js', 'ws_manager.js', 'session_stream.js', 'features.js', 'ask_card.js', 'event_render.js', 'event_stream.js', 'session_list.js', 'session_ident.js', 'icons.js', 'file_ref_parse.js', 'shell.js', 'backend_catalog.js', 'cron_state.js', 'cron_format.js']);

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
  // Shared state crosses module boundaries only as a const state object the
  // owner exports, and dependencies only as imports or shell.X upcalls: a
  // configureX export outside caps.injectionLegacy fails nz/shell-bindings
  // (scripts/eslint-plugin-nz.mjs).
  {
    files: [...moduleFiles].map((f) => `internal/server/static/${f}`),
    plugins: { nz },
    rules: {
      'nz/shell-bindings': ['error', { legacy: caps.injectionLegacy }],
      'nz/deps-keys': 'error',
      'nz/no-exported-let': 'error',
    },
  },
  // A module may declare state at load time but may not run anything
  // (D-S19). contract.js is excluded like sw.js (generated, and not in
  // moduleFiles — see the comment on contract.js below); dashboard.js (the
  // composition root) and sideEffectLegacy's other entries are today's real
  // violations (js-ratchet.caps.json's sideEffectLegacy, shrink-only — a
  // file moves out once it is clean). A caps.shellRoots file may fill its
  // shell slots with one top-level registerShell({ … }).
  {
    files: [...moduleFiles].filter((f) => f !== 'contract.js' && !sideEffectLegacy.has(f)).map((f) => `internal/server/static/${f}`),
    plugins: { nz },
    rules: {
      'nz/no-module-side-effects': ['error', { shellRoots: caps.shellRoots }],
    },
  },
];
