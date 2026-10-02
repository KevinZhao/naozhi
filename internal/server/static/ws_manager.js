// ws_manager.js — the dashboard's WebSocket (S18, #3024): dial, auth, backoff
// and reconnect, ping, send, and the receive dispatch table. Business state
// lives with its owner (session_stream.js, cron_live.js, dashboard.js), which
// registers frame handlers with wsm.on and lifecycle callbacks with onReady /
// onStateChange / onAuthFail. A leaf: it imports only contract.js and
// platform.js, so any module and node can import it (check-ws-receivers R7).
import { NZ_CONTRACT } from './contract.js';
import { getToken } from './platform.js';

export const WS_STATES = { OFF: 'off', CONNECTING: 'connecting', AUTH: 'authenticating', CONNECTED: 'connected', DISCONNECTED: 'disconnected' };

export const wsm = {
  conn: null,
  state: WS_STATES.OFF,
  backoff: 1000,
  maxBackoff: 30000,
  reconnectTimer: null,
  pingTimer: null,
  sendCounter: 0,
  // _everConnected tells the first handshake from a reconnect; setState sets
  // it after the CONNECTED listeners ran, so they see the old value.
  _everConnected: false,
  // _authBlockUntil is a unix-ms wall-clock deadline. While Date.now() <
  // _authBlockUntil, connect() skips dialing and scheduleReconnect() pushes
  // the next attempt to the deadline instead of its own exponential backoff.
  // Set by startWSAuthRetryCountdown when the server emits auth_fail with
  // retry_after=N (rate-limit lockout). Without this gate the default
  // reconnect loop would immediately dial a fresh WS, hit the same 429,
  // and rack up more lockout events in the journal.
  _authBlockUntil: 0,
  // R110-P1 WS outage duration display: wall-clock ms when the connection
  // first left the CONNECTED state (or 0 when connected). setState maintains
  // this: any CONNECTED→non-CONNECTED transition writes Date.now() if the
  // field is still 0 (first outage arm — don't stomp an earlier outage while
  // cycling connecting → auth → connecting during backoff); CONNECTED clears
  // it. updateStatusBar reads it to render "已断开 N 秒/分" inline hint so
  // users distinguish "just lost the WS 2s ago" from "dead for 10 min".
  _disconnectedSince: 0,
  // Lifecycle callbacks, each run in registration order: _ready after
  // auth_ok's core, _stateChange(s, prev) on every setState, _authFail(msg)
  // before an auth_fail closes the socket.
  _ready: [],
  _stateChange: [],
  _authFail: [],

  connect() {
    if (this.conn && (this.conn.readyState === WebSocket.OPEN || this.conn.readyState === WebSocket.CONNECTING)) return;
    // Respect the auth rate-limit gate: skip the dial if we're still within
    // the lockout window. scheduleReconnect re-arms a timer pointing at the
    // deadline so we come back exactly when the server says we can.
    if (this._authBlockUntil > 0 && Date.now() < this._authBlockUntil) {
      this.scheduleReconnect();
      return;
    }

    this.setState(WS_STATES.CONNECTING);
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    this.conn = new WebSocket(proto + '//' + location.host + '/ws');

    this.conn.onopen = () => {
      this.setState(WS_STATES.AUTH);
      const token = getToken();
      this.conn.send(JSON.stringify({ type: NZ_CONTRACT.WS.auth, token: token }));
    };

    this.conn.onmessage = (evt) => {
      try { this.onMessage(JSON.parse(evt.data)); }
      catch (err) { console.error('ws parse error:', err); }
    };

    this.conn.onclose = () => {
      this.cleanup();
      this.setState(WS_STATES.DISCONNECTED);
      this.scheduleReconnect();
    };

    this.conn.onerror = () => {};
  },

  cleanup() {
    if (this.pingTimer) { clearInterval(this.pingTimer); this.pingTimer = null; }
  },

  disconnect() {
    if (this.reconnectTimer) { clearTimeout(this.reconnectTimer); this.reconnectTimer = null; }
    this.cleanup();
    if (this.conn) { this.conn.close(); this.conn = null; }
    this.setState(WS_STATES.OFF);
  },

  scheduleReconnect() {
    if (this.reconnectTimer) return;
    // Pick the later of: the exponential-backoff delay, and the auth-block
    // deadline. If an auth rate-limit countdown is active, we must not
    // dial before it expires — the exponential curve would otherwise
    // happily re-try every 1-30s and wake the 429 bucket over and over.
    const now = Date.now();
    const authGap = Math.max(0, this._authBlockUntil - now);
    // RNEW-UX-001: add randomised jitter (0-500ms) on top of the computed
    // delay. Without jitter, N tabs that all dropped together on the same
    // server restart would redial on identical millisecond ticks, briefly
    // saturating the upgrade limiter and causing a thundering herd. The
    // jitter is additive (never shortens the gate) so the auth-block
    // invariant above is preserved.
    const jitter = Math.floor(Math.random() * 500);
    const delay = Math.max(this.backoff, authGap) + jitter;
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      this.connect();
    }, delay);
    this.backoff = Math.min(this.backoff * 2, this.maxBackoff);
  },

  // _frames is the receive dispatch table: an outbound NZ_CONTRACT.WS type to
  // its claims (handlers with a `when`, tried in registration order, the first
  // match wins) and its one unconditional handler, run when no claim matches.
  // No match, or an unregistered type, drops the frame.
  _frames: new Map(),

  on(type, fn, when) {
    if (!Object.hasOwn(NZ_CONTRACT.WS, type)) throw new Error('wsm.on: unknown frame type ' + type);
    const t = this._frames.get(type) || { claims: [], fallback: null };
    if (when) t.claims.push({ fn, when });
    else if (t.fallback) throw new Error('wsm.on: ' + type + ' already has a handler');
    else t.fallback = fn;
    this._frames.set(type, t);
  },

  onReady(fn) { this._ready.push(fn); },
  onStateChange(fn) { this._stateChange.push(fn); },
  onAuthFail(fn) { this._authFail.push(fn); },

  onMessage(msg) {
    const t = this._frames.get(msg.type);
    if (!t) return;
    const c = t.claims.find((x) => x.when(msg));
    if (c) c.fn(msg);
    else if (t.fallback) t.fallback(msg);
  },

  startPing() {
    if (this.pingTimer) clearInterval(this.pingTimer);
    this.pingTimer = setInterval(() => {
      if (this.conn && this.conn.readyState === WebSocket.OPEN) {
        this.conn.send(JSON.stringify({ type: NZ_CONTRACT.WS.ping }));
      }
    }, 30000);
  },

  send(msg) {
    if (this.conn && this.conn.readyState === WebSocket.OPEN) {
      this.conn.send(JSON.stringify(msg));
      return true;
    }
    return false;
  },

  setState(s) {
    const prev = this.state;
    this.state = s;
    // R110-P1 outage duration timestamp maintenance. Arm on first
    // transition OUT of CONNECTED (or from a cold OFF start that never
    // reached CONNECTED — treat any persistent non-CONNECTED as outage).
    // Guard with `=== 0` so a connecting→auth→connecting cycle during
    // backoff doesn't reset the clock to zero mid-outage. Clear on
    // entering CONNECTED so the next outage arms fresh.
    if (s === WS_STATES.CONNECTED) {
      this._disconnectedSince = 0;
    } else if (prev === WS_STATES.CONNECTED && this._disconnectedSince === 0) {
      // Just left a healthy connection — stamp the wall clock.
      this._disconnectedSince = Date.now();
    } else if (this._disconnectedSince === 0 && s !== WS_STATES.OFF) {
      // Cold-start / never-connected case: arm from the first
      // CONNECTING attempt so the user sees a duration even before the
      // first successful handshake. OFF (the initial synthetic state)
      // is excluded — pre-boot doesn't count as outage.
      this._disconnectedSince = Date.now();
    }
    this._stateChange.forEach((fn) => fn(s, prev));
    if (s === WS_STATES.CONNECTED) this._everConnected = true;
  },

  isConnected() { return this.state === WS_STATES.CONNECTED; }
};

wsm.on(NZ_CONTRACT.WS.auth_ok, () => {
  wsm.setState(WS_STATES.CONNECTED);
  wsm.backoff = 1000;
  wsm.startPing();
  wsm._ready.forEach((fn) => fn());
});
wsm.on(NZ_CONTRACT.WS.auth_fail, (msg) => {
  wsm._authFail.forEach((fn) => fn(msg));
  wsm.conn.close();
});
wsm.on(NZ_CONTRACT.WS.pong, () => {});
