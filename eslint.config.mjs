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
  'dashboard.js': {
    ...ro([
      'appendEventsToContainer',
      'authHeaders',
      'cronApplyRunEnded',
      'cronApplyRunStarted',
      'cronJobs',
      'cronTimelineRefreshHeadDebounced',
      'ensureCronLiveSubscription',
      'esc',
      'escAttr',
      'fetchCronJobs',
      'fetchJSON',
      'findAgentByTaskId',
      'findAgentByToolUseId',
      'formatCostUSD',
      'formatDurationShort',
      'formatRunDuration',
      'initAgentsFromSession',
      'isCronLiveKey',
      'isCronSessionFrozen',
      'isCronSessionKey',
      'nz',
      'openCronPanel',
      'renderAgentRows',
      'renderCronPanel',
      'repaintCronLive',
      'setCronLiveStatus',
      'showToast',
      'trapFocus',
      'updateCronLiveTruncated',
    ]),
    // Lazy-loaded vendor libraries (script tags injected at render time).
    ...ro(['mermaid', 'katex']),
  },
  'cron_view.js': {
    ...ro([
      'CRON_LIVE_AGENT_ONLY_HTML',
      'CRON_LIVE_MAX_EVENTS',
      'EVENT_DIVIDER_GAP_MS',
      'INTERNAL_EVENT_TYPES',
      'appendEvents',
      'confirmDialog',
      'defaultWorkspace',
      'esc',
      'escAttr',
      'escJs',
      'eventHtml',
      'fetchCLIBackends',
      'fetchJSON',
      'formatAbsTime',
      'getToken',
      'isInternalEvent',
      'lastDividerTime',
      'lsGet',
      'lsSet',
      'mobileBack',
      'navUserEls',
      'nz',
      'processEventsForDisplay',
      'projectsData',
      'regroupAvatars',
      'renderBackendPicker',
      'renderEventsWithDividers',
      'renderMd',
      'runPendingAsync',
      'selectSession',
      'sending',
      'sessionsData',
      'setActiveSessionCard',
      'setActivityView',
      'shortPath',
      'showAPIError',
      'showAuthModal',
      'showNetworkError',
      'showToast',
      'timeDividerHtml',
      'trapFocus',
      'turnState',
      'wsm',
    ]),
    // cron_view.js assigns these dashboard.js `let` bindings directly.
    ...rw(['activeView', 'eventTimer', 'selectedKey']),
    // Implicit window.event, used behind a typeof guard in inline-onclick
    // handlers (cronDrawerSpecPromptToggle).
    ...ro(['event']),
  },
  'agent_view.js': {
    ...ro([
      'esc',
      'escAttr',
      'fmtDuration',
      'refreshBanner',
      'selectedKey',
      'selectedNode',
      'sessionScrollPos',
      'sessionsData',
      'showToast',
      'sid',
      'turnState',
      'wsm',
    ]),
  },
  'asset_browser.js': {
    ...ro(['esc', 'fetchJSON', 'nz']),
  },
  'files_view.js': {
    ...ro([
      'esc',
      'escAttr',
      'fetchJSON',
      'fileApiUrl',
      'nz',
      'renderSandboxedBlob',
    ]),
  },
};

const perFile = Object.entries(deps).map(([file, globals]) => ({
  files: [`internal/server/static/${file}`],
  languageOptions: { globals },
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
