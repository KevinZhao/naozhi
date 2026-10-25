// e2e-shim.js — the e2e suite's instrumentation surface (#2941). Served by
// mock-server.js only; production has no such route and no test surface.
//
// The legacy suite probes dashboard bindings as bare identifiers inside
// page.evaluate, so every name here is mirrored onto window as well as
// window.nz.test. Functions and constants come straight from the modules that
// export them; state lives on exported const objects (state.js, voice's
// voiceRec), so those names are read/write accessors onto
// the object its owner reads. Prefer shrinking this list as tests migrate to
// first-class assertions.
import * as authModal from '/static/auth_modal.js';
import * as composerFiles from '/static/composer_files.js';
import * as dashboard from '/static/dashboard.js';
import * as msgNav from '/static/msg_nav.js';
import * as nzUtil from '/static/nz_util.js';
import * as runningBanner from '/static/running_banner.js';
import * as sessionListModule from '/static/session_list.js';
import * as sessionIdent from '/static/session_ident.js';
import * as sidebarProject from '/static/sidebar_project.js';
import * as utilities from '/static/utilities.js';
import * as voice from '/static/voice.js';
import * as wsManager from '/static/ws_manager.js';
import { composer, selection, sessionList } from '/static/state.js';

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

expose(authModal, ['createNewSession', 'doCreateInProject', 'highlight', 'openProjectPalette', 'pickPaletteCustom', 'renderNodePicker']);
expose(composerFiles, ['handleFiles']);
expose(dashboard, ['maybeShowOnboarding', 'selectSession', 'toggleHistory']);
expose(msgNav, ['navDismissPopover']);
expose(nzUtil, ['showToast']);
expose(runningBanner, ['interruptSession']);
expose(sessionIdent, ['getNodeStatus', 'sid']);
expose(sessionListModule, ['fetchSessions', 'updateStatusBar']);
expose(sidebarProject, ['showGitRemote', 'toggleProjectCollapsed']);
expose(utilities, ['closeHistoryPopover', 'setMsgValue']);
expose(voice, ['MAX_REC_SECS', 'updateVoiceTimer']);
expose(wsManager, ['WS_STATES']);

// wsm is strict: reading or writing a field the object does not declare (one
// that moved to its owner) throws instead of reading undefined, so a test left
// on the old home fails loudly (S18, #3024).
const strictWsm = new Proxy(wsManager.wsm, {
  get(t, k) {
    if (typeof k === 'string' && !(k in t)) throw new Error('e2e-shim: wsm declares no field ' + k);
    return t[k];
  },
  set(t, k, v) {
    if (!(k in t)) throw new Error('e2e-shim: wsm declares no field ' + String(k));
    t[k] = v;
    return true;
  },
});
Object.defineProperty(surface, 'wsm', { get: () => strictWsm, enumerable: true, configurable: true });

stateField('voiceCancelled', voice.voiceRec, 'cancelled');
stateField('voiceRecStart', voice.voiceRec, 'start');
stateField('voiceRecTimer', voice.voiceRec, 'timer');
stateField('voiceState', voice.voiceRec, 'state');
stateField('pendingFiles', composer, 'pendingFiles');
stateField('projectsData', sessionList, 'projectsData');
stateField('selectedKey', selection, 'key');
stateField('sending', composer, 'sending');
stateField('sessionsData', sessionList, 'sessionsData');

window.nz.test = surface;
for (const name of Object.keys(surface)) {
  const d = Object.getOwnPropertyDescriptor(surface, name);
  Object.defineProperty(window, name, { get: d.get, set: d.set, configurable: true });
}
