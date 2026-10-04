// e2e-shim.js — the e2e suite's instrumentation surface (#2941). Served by
// mock-server.js only; production has no such route and no test surface.
//
// The legacy suite probes dashboard bindings as bare identifiers inside
// page.evaluate, so every name here is mirrored onto window as well as
// window.nz.test. Functions and constants come straight from the modules that
// export them; state lives on exported const objects (state.js, msg_nav's
// navState, voice's voiceRec), so those names are read/write accessors onto
// the object its owner reads. This list may only shrink as tests migrate to
// first-class assertions.
import * as authModal from '/static/auth_modal.js';
import * as composerFiles from '/static/composer_files.js';
import * as dashboard from '/static/dashboard.js';
import * as discovery from '/static/discovery.js';
import * as eventRender from '/static/event_render.js';
import * as eventStream from '/static/event_stream.js';
import * as fileRefParse from '/static/file_ref_parse.js';
import * as mobileNav from '/static/mobile_nav.js';
import * as msgNav from '/static/msg_nav.js';
import * as nzUtil from '/static/nz_util.js';
import * as renderMd from '/static/render_md.js';
import * as runningBanner from '/static/running_banner.js';
import * as sendMessage from '/static/send_message.js';
import * as sessionListModule from '/static/session_list.js';
import * as sessionIdent from '/static/session_ident.js';
import { sessionStream } from '/static/session_stream.js';
import * as sessionHeader from '/static/session_header.js';
import * as sidebarProject from '/static/sidebar_project.js';
import * as systemView from '/static/system_view.js';
import * as tuning from '/static/tuning.js';
import * as utilities from '/static/utilities.js';
import * as voice from '/static/voice.js';
import * as wsManager from '/static/ws_manager.js';
import { composer, perSession, selection, sessionList, timers, transcript, ui } from '/static/state.js';

const surface = {};

/** Expose a module's live exports under their own names. */
function expose(mod, names) {
  for (const name of names) {
    if (!(name in mod)) throw new Error('e2e-shim: module does not export ' + name);
    Object.defineProperty(surface, name, { get: () => mod[name], enumerable: true, configurable: true });
  }
}

/** Expose one field of a module's exported state object, readable and writable. */
function stateField(name, obj, key) {
  Object.defineProperty(surface, name, {
    get: () => obj[key],
    set: (v) => { obj[key] = v; },
    enumerable: true,
    configurable: true,
  });
}

expose(authModal, ['createNewSession', 'doCreateInProject', 'getSelectedNode', 'highlight', 'openProjectPalette', 'pickPaletteCustom', 'renderNodePicker', 'wireNodePicker']);
expose(composerFiles, ['ORIENT_MAX_WAIT_MS', 'awaitPendingOrients', 'handleFiles', 'maybeAutoOrient', 'openFilePicker', 'removeFile', 'renderFilePreviews', 'retryUpload']);
expose(dashboard, ['maybeShowOnboarding', 'renderMainShell', 'selectSession', 'sessionCardKey', 'setActivityView', 'toggleHistory', 'updateHeaderCLI']);
expose(discovery, ['scanDiscovered']);
expose(eventRender, ['eventAlreadyRendered', 'eventHtml']);
expose(eventStream, ['appendEvents', 'fetchEvents', 'renderEvents', 'trimEventsScroll']);
expose(fileRefParse, ['isFileRefCandidate', 'splitPathLine']);
expose(mobileNav, ['mobileShowList', 'toggleSidebarCollapsed']);
expose(msgNav, ['navDismissPopover']);
expose(nzUtil, ['showToast']);
expose(renderMd, ['BLOCK_SPLIT_RE', 'LIST_ITEM_RE', 'LIST_SHAPE_RE', 'MAX_LIST_DEPTH', '_mdCache', 'isMathDisplay', 'isMathInline', 'katexPending', 'parseListItem', 'renderKatex', 'renderMd', 'renderTable']);
expose(runningBanner, ['fmtDuration', 'interruptSession', 'scrollSlackPx', 'turnState']);
expose(sendMessage, ['clearPendingFiles', 'markSessionOptimisticRunning', 'renderOptimisticUserMsg', 'sendMessage']);
expose(sessionIdent, ['getNodeStatus', 'sid']);
expose(sessionHeader, ['renderSessionRunsPanel', 'setHeaderRunStats']);
expose(sessionListModule, ['debouncedFetchSessions', 'fetchSessions', 'renderSidebar', 'restorePending', 'updateStatusBar']);
expose(sidebarProject, ['showGitRemote', 'toggleProjectCollapsed']);
expose(systemView, ['reconcileSelectedNode']);
expose(tuning, ['dismissSession', 'renameSession']);
expose(utilities, ['MAX_LIVE_DOM_EVENTS', 'applyFeatureGates', 'closeHistoryPopover', 'getMsgValue', 'isMobile', 'mobileEnterChat', 'promptDialog', 'removeSidebarCard', 'setMsgValue']);
expose(voice, ['MAX_REC_SECS', 'updateVoiceTimer']);
expose(wsManager, ['WS_STATES']);

// wsm and sessionStream are strict: reading or writing a field the object does
// not declare (one that moved to its owner) throws instead of reading
// undefined, so a test left on the old home fails loudly (S18, #3024).
const strict = (obj, name) => new Proxy(obj, {
  get(t, k) {
    if (typeof k === 'string' && !(k in t)) throw new Error('e2e-shim: ' + name + ' declares no field ' + k);
    return t[k];
  },
  set(t, k, v) {
    if (!(k in t)) throw new Error('e2e-shim: ' + name + ' declares no field ' + String(k));
    t[k] = v;
    return true;
  },
});
for (const [name, obj] of [['wsm', wsManager.wsm], ['sessionStream', sessionStream]]) {
  const p = strict(obj, name);
  Object.defineProperty(surface, name, { get: () => p, enumerable: true, configurable: true });
}

stateField('navIdx', msgNav.navState, 'idx');
stateField('navPopoverOpen', msgNav.navState, 'popoverOpen');
stateField('navUserEls', msgNav.navState, 'userEls');
stateField('voiceCancelled', voice.voiceRec, 'cancelled');
stateField('voiceRecStart', voice.voiceRec, 'start');
stateField('voiceRecTimer', voice.voiceRec, 'timer');
stateField('voiceState', voice.voiceRec, 'state');
stateField('_lastSidebarData', sessionList, 'lastSidebarData');
stateField('activeView', ui, 'activeView');
stateField('discoveredItems', sessionList, 'discoveredItems');
stateField('discoveredPollTimer', timers, 'discoveredPoll');
stateField('historyTag', sessionList, 'historyTag');
stateField('lastEventTime', transcript, 'lastEventTime');
stateField('lastRenderedEventTime', transcript, 'lastRenderedEventTime');
stateField('lastVersion', sessionList, 'lastVersion');
stateField('pendingFiles', composer, 'pendingFiles');
stateField('projectsData', sessionList, 'projectsData');
stateField('selectedKey', selection, 'key');
stateField('selectedNode', selection, 'node');
stateField('sending', composer, 'sending');
stateField('sessionPollTimer', timers, 'sessionPoll');
stateField('sessionsData', sessionList, 'sessionsData');
Object.defineProperty(surface, '_askAnswered', { get: () => transcript.askAnswered, enumerable: true, configurable: true });
Object.defineProperty(surface, 'sessionDrafts', { get: () => perSession.drafts, enumerable: true, configurable: true });
Object.defineProperty(surface, 'sessionNodes', { get: () => perSession.nodes, enumerable: true, configurable: true });
Object.defineProperty(surface, 'sessionWorkspaces', { get: () => perSession.workspaces, enumerable: true, configurable: true });

window.nz.test = surface;
for (const name of Object.keys(surface)) {
  const d = Object.getOwnPropertyDescriptor(surface, name);
  Object.defineProperty(window, name, { get: d.get, set: d.set, configurable: true });
}
