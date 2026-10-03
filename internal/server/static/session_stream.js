// session_stream.js — the selected session's subscription (S18, #3024): the
// key and node the server streams to this tab, the subscribe in flight, and
// the event cursor a resubscribe resumes after. event_stream.js's sessionFrames
// and session_list.js's frame handlers keep it current. A leaf like
// ws_manager.js: it imports contract.js, state.js and ws_manager.js only (R7).
import { NZ_CONTRACT } from './contract.js';
import { selection, timers, transcript } from './state.js';
import { WS_STATES, wsm } from './ws_manager.js';

// INITIAL_HISTORY_LIMIT caps how many events the server sends on a fresh
// subscribe / first fetch. Keeps big sessions snappy on first paint; older
// pages load lazily via the "load earlier" button. Server caps at 500
// regardless (maxEventsPageLimit) so 100-500 is the effective window.
export const INITIAL_HISTORY_LIMIT = 100;

export const sessionStream = {
  subscribedKey: null,
  subscribedNode: null,
  lastEventTimeWs: 0,
  _initialSubscribe: false,
  _pendingSubscribeKey: null,
  _pendingSubscribeNode: null,
  _subscriptionSuspended: false,

  subscribe(key, node) {
    node = node || 'local';
    this._pendingSubscribeKey = key;
    this._pendingSubscribeNode = node;
    const msg = { type: NZ_CONTRACT.WS.subscribe, key: key };
    if (node && node !== 'local') msg.node = node;
    this._initialSubscribe = (this.lastEventTimeWs === 0);
    if (this.lastEventTimeWs > 0) {
      msg.after = this.lastEventTimeWs;
    } else {
      // Initial subscribe: ask for only the last INITIAL_HISTORY_LIMIT events.
      // Keeps the first frame fast on large sessions; older events are fetched
      // on demand via the "load earlier" button that calls GET
      // /api/sessions/events?before=..&limit=..
      msg.limit = INITIAL_HISTORY_LIMIT;
    }
    wsm.send(msg);
  },

  unsubscribe() {
    if (this.subscribedKey) {
      const msg = { type: NZ_CONTRACT.WS.unsubscribe, key: this.subscribedKey };
      if (this.subscribedNode && this.subscribedNode !== 'local') msg.node = this.subscribedNode;
      wsm.send(msg);
    }
    this.reset();
    this.lastEventTimeWs = 0;
  },

  // reset forgets the subscription and any subscribe in flight; the cursor
  // stays, so a later subscribe resumes after it.
  reset() {
    this.subscribedKey = null;
    this.subscribedNode = null;
    this._pendingSubscribeKey = null;
    this._pendingSubscribeNode = null;
  },
};

// A fresh socket carries no subscription: resume the selected session's
// stream after the last event this tab holds, else from an initial page.
wsm.onReady(() => {
  if (timers.events) { clearInterval(timers.events); timers.events = null; }
  if (selection.key) {
    if (transcript.lastEventTime > 0 && sessionStream.lastEventTimeWs === 0) {
      sessionStream.lastEventTimeWs = transcript.lastEventTime;
    }
    sessionStream.subscribe(selection.key, selection.node);
  }
});
// wsm.disconnect() (a token change) ends in OFF: the server dropped both.
wsm.onStateChange((s) => { if (s === WS_STATES.OFF) sessionStream.reset(); });
