// @ts-check
// event_render.js — one EventEntry to one transcript bubble (eventHtml and its
// EVENT_WHOLE/EVENT_CONTENT/EVENT_ICONS tables), plus the time-divider and
// dedup helpers the transcript, cron and agent views share.
import { NZ_CONTRACT } from './contract.js';
import { selection, serverInfo, sessionList } from './state.js';
import { esc, escAttr } from './nz_util.js';
import { renderMd } from './render_md.js';
import { EVENT_DIVIDER_GAP_MS, formatTimeFull, timeDividerHtml } from './utilities.js';
import { renderAskQuestionCard } from './ask_card.js';
import { CLAWD_SVG, ICONS } from './icons.js';
import { pendingBackendID } from './features.js';
import { isInternalEvent, sid } from './session_ident.js';

// renderTodoList parses the JSON todos payload stored on EventEntry.detail and
// emits a checklist block. Falls back to the summary line when detail is
// malformed so a parse failure never produces an empty bubble.
function renderTodoList(detail, summary) {
  let todos = null;
  if (detail) {
    try { todos = JSON.parse(detail); } catch (_) { todos = null; }
  }
  if (!Array.isArray(todos) || todos.length === 0) return esc(summary || 'Todos');
  let done = 0, active = 0, pending = 0;
  const items = todos.map(t => {
    const status = (t && t.status) || 'pending';
    let cls = 'todo-pending';
    let mark = '\u25cb'; // ○ pending
    let text = (t && t.content) || '';
    if (status === 'completed') {
      cls = 'todo-done';
      mark = '\u2714'; // ✔
      done++;
    } else if (status === 'in_progress') {
      cls = 'todo-active';
      mark = '\u25b8'; // ▸
      if (t && t.activeForm) text = t.activeForm;
      active++;
    } else {
      pending++;
    }
    return '<li class="todo-item ' + cls + '"><span class="todo-mark">' + mark + '</span><span class="todo-text">' + esc(text) + '</span></li>';
  }).join('');
  const total = todos.length;
  const counts =
    '<span class="todo-count">' + total + ' 项</span>' +
    (done > 0 ? '<span class="todo-count done">' + done + ' 完成</span>' : '') +
    (active > 0 ? '<span class="todo-count active">' + active + ' 进行中</span>' : '') +
    (pending > 0 ? '<span class="todo-count">' + pending + ' 待办</span>' : '');
  const header =
    '<div class="todo-header">' +
      '<span class="todo-title">任务清单</span>' +
      '<span class="todo-counts">' + counts + '</span>' +
    '</div>';
  return header + '<ul class="todo-list">' + items + '</ul>';
}

// LEAKED_TOOLCALL_RE anchors the start of a tool-call block that an LLM
// emitted as *prose* instead of a structured tool_use content block. The
// claude/anthropic harness expresses a real tool call as a dedicated
// content block (type:"tool_use") which naozhi surfaces as its own
// tool_use event and filters out of the main transcript (isInternalEvent).
// But the model occasionally regresses and writes the call syntax —
//   call
//   <invoke name="Bash">
//   <parameter name="command">…</parameter>
//   </invoke>
// — verbatim into an assistant *text* block. That text flows through the
// pipeline untouched (process_event_format.go stores it as type:"text")
// and lands in the bubble as a literal wall of XML, which is noise to the
// operator (the call was never executed — it's a malformed turn).
//
// The anchor REQUIRES a `call` or `<function_calls>` marker alone on its
// own line immediately preceding `<invoke name="`. This is deliberately
// strict: a bare `<invoke …>` must NOT trip the detector, because operators
// legitimately quote tool-call syntax inside backticks when discussing it
// (e.g. this very bug report). Validated against 667 real text/user events:
// 9 genuine leaks caught, 0 false positives on quoted-syntax discussions.
const LEAKED_TOOLCALL_RE = /(?:^|\n)[ \t]*(?:call|<function_calls>)[ \t]*\n[ \t]*<invoke name="/;

// stripLeakedToolCalls splits an assistant text body into the real prose
// that precedes a leaked tool-call block and the leaked block itself.
// Returns null when no leak is detected (the overwhelmingly common case —
// keep this cheap so every text bubble can call it). On a hit it returns
// { prose, leaked } where `prose` is the body with the leaked region
// removed (trailing whitespace trimmed) and `leaked` is the raw XML for the
// fold-away <details>. The region runs from the anchor's `call` /
// `<function_calls>` line to the final `</invoke>` (plus an optional
// trailing `</function_calls>`), so multiple chained <invoke> blocks under
// one marker collapse into a single fold.
function stripLeakedToolCalls(text) {
  if (!text || text.indexOf('</invoke>') === -1) return null;
  const m = LEAKED_TOOLCALL_RE.exec(text);
  if (!m) return null;
  // Anchor start = the `call` / `<function_calls>` line, not the regex's
  // leading \n. m.index points at the char before that line when the
  // alternation matched `\n`; step over it so the marker stays in `leaked`.
  let start = m.index;
  if (text[start] === '\n') start += 1;
  // The leaked region ends at the LAST </invoke> — a single prose turn that
  // leaks more than one call writes them consecutively, and everything from
  // the marker to the last close tag is the malformed payload.
  let end = text.lastIndexOf('</invoke>') + '</invoke>'.length;
  const tail = text.slice(end);
  const fc = /^\s*<\/function_calls>/.exec(tail);
  if (fc) end += fc[0].length;
  return { prose: text.slice(0, start).replace(/\s+$/, ''), leaked: text.slice(start, end) };
}

// EVENT_WHOLE renders a type's entire bubble, bypassing the generic
// icon/content/time wrapper below (ask_question is its own card). A Map, not
// a plain object: an unlisted e.type like 'constructor' must resolve to
// undefined, not an inherited Object.prototype member (#3025 S19-2; same
// reason for EVENT_CONTENT/EVENT_ICONS).
const EVENT_WHOLE = new Map([['ask_question', renderAskQuestionCard]]);

// EVENT_CONTENT fills div.event-content for a type still using the generic
// wrapper. agent/result/task_* are plain escaped text (defaultTextContentHtml);
// persist_gap shares the unrecognised-type chip (defaultEventChip).
const EVENT_CONTENT = new Map([
  ['system', (e) => esc(e.summary || e.type)],
  ['text', textOrUserContentHtml],
  ['user', textOrUserContentHtml],
  ['todo', (e) => renderTodoList(e.detail, e.summary)],
  ['tool_use', (e) => (e.tool_call ? toolUseHtml(e) : defaultTextContentHtml(e))],
  ['tool_result', toolResultHtml],
  ['agent', defaultTextContentHtml],
  ['result', defaultTextContentHtml],
  ['task_start', defaultTextContentHtml],
  ['task_progress', defaultTextContentHtml],
  ['task_done', defaultTextContentHtml],
  ['persist_gap', defaultEventChip],
]);

// EVENT_ICONS: the .event-icon glyph for a handful of known types; anything
// else (including the clawd-mascot override in eventIconHtml, which is
// dynamic) stays blank.
const EVENT_ICONS = new Map([
  ['system', ICONS.gear],
  ['user', ICONS.user],
  ['text', ICONS.spark],
  ['todo', ICONS.todo],
]);

// shouldHideEvent is eventHtml's pre-filter, run before any table lookup: a
// NO_BUBBLE kind in every view (the server's visible count skips the same
// kindTable column), an internal type without includeInternal, injected
// system XML, and the CLI's own SIGINT interrupt marker.
const NO_BUBBLE_EVENT_TYPES = new Set(NZ_CONTRACT.ENUMS.EVENT_TYPE_NO_BUBBLE);
function shouldHideEvent(e, includeInternal) {
  if (NO_BUBBLE_EVENT_TYPES.has(e.type) || (!includeInternal && isInternalEvent(e))) return true;
  const raw = e.detail || e.summary || '';
  if (e.type === 'user' && /^<(task-notification|system-reminder|local-command|command-name|available-deferred-tools)[\s>]/.test(raw)) return true;
  if (e.type === 'user' && (raw === '[Request interrupted by user]' || raw === '[Request interrupted by user for tool use]')) return true;
  return false;
}

// eventIconHtml: EVENT_ICONS, plus the clawd mascot for a claude-backend
// assistant turn in place of the default glyph (other backends keep theirs).
function eventIconHtml(e) {
  let icon = EVENT_ICONS.get(e.type) || '';
  if (e.type === 'text') {
    const sess = sessionList.sessionsData[sid(selection.key, selection.node)] || {};
    const backendID = sess.backend || pendingBackendID(selection.key, selection.node) || (serverInfo.cliBackends && serverInfo.cliBackends.default) || '';
    if (backendID === 'claude' || backendID === '') icon = CLAWD_SVG;
  }
  return icon;
}

// defaultEventChip: the fallback .event-content for persist_gap and any
// unrecognised type — a chip with the literal type string, its title the
// detail/summary, and the summary repeated after so it stays visible.
function defaultEventChip(e) {
  const title = escAttr(e.detail || e.summary || '');
  const tail = e.summary ? ' ' + esc(e.summary) : '';
  return '<span class="event-chip" title="' + title + '">' + esc(e.type || '') + '</span>' + tail;
}

// defaultTextContentHtml: plain-text content for a known type with no
// structure of its own (agent, result, task_*).
function defaultTextContentHtml(e) {
  return esc(e.detail || e.summary || e.type);
}

// textOrUserContentHtml folds a leaked tool-call block (the model wrote
// <invoke …> into a text turn) behind a collapsed warning instead of
// breaking the bubble; esc() keeps it inert.
function textOrUserContentHtml(e, cleanRaw) {
  const leak = stripLeakedToolCalls(cleanRaw);
  if (!leak) return renderMd(cleanRaw || e.type);
  return renderMd(leak.prose || e.type) +
    '<details class="leaked-toolcall"><summary class="leaked-toolcall-summary">' +
    '⚠ 模型输出了未执行的工具调用（已折叠）</summary>' +
    '<pre class="leaked-toolcall-body">' + esc(leak.leaked) + '</pre></details>';
}

// toolUseHtml: the ACP rich tool progress row (kiro, includeInternal-only).
// Output extraction tries the kiro {items:[{Json:{stdout}}]} shape first,
// then falls back to pretty-printed JSON.
function toolUseHtml(e) {
  const tc = e.tool_call;
  const status = tc.status || '';
  const kind = tc.kind || '';
  const title = tc.title || tc.name || tc.id || '(tool)';
  const statusClass = 'tc-status tc-status-' + (status || 'pending');
  const statusLabel = status || 'pending';
  let bodyText = '';
  if (tc.output_json) {
    try {
      const parsed = JSON.parse(tc.output_json);
      if (parsed && Array.isArray(parsed.items) && parsed.items.length > 0 &&
          parsed.items[0] && parsed.items[0].Json && typeof parsed.items[0].Json.stdout === 'string') {
        bodyText = parsed.items[0].Json.stdout;
      } else {
        bodyText = JSON.stringify(parsed, null, 2);
      }
    } catch { bodyText = tc.output_json; }
  } else if (tc.input_json) {
    try {
      bodyText = JSON.stringify(JSON.parse(tc.input_json), null, 2);
    } catch { bodyText = tc.input_json; }
  }
  const bodyHtml = bodyText
    ? '<pre class="tc-body">' + esc(bodyText.length > 8000 ? bodyText.slice(0, 8000) + '\n…' : bodyText) + '</pre>'
    : '';
  const kindBadge = kind ? '<span class="tc-kind">' + esc(kind) + '</span>' : '';
  return '<details class="tc-wrap"' + (status === 'failed' ? ' open' : '') + '>' +
    '<summary class="tc-summary">' +
    '<span class="tc-icon" aria-hidden="true">🛠</span>' +
    '<span class="tc-title">' + esc(title) + '</span>' +
    kindBadge +
    '<span class="' + statusClass + '">' + esc(statusLabel) + '</span>' +
    '</summary>' + bodyHtml + '</details>';
}

// toolResultHtml folds long outputs by default; a <persisted-output> Tool
// field of "persisted:tool-results/<id>.ext" gets a fetch-full button.
function toolResultHtml(e) {
  const summary = e.summary || '(tool result)';
  const detail = e.detail || '';
  let persistedPath = '';
  if (typeof e.tool === 'string' && e.tool.indexOf('persisted:') === 0) {
    persistedPath = e.tool.slice('persisted:'.length);
  }
  const detailHtml = detail ? '<pre class="tr-detail">' + esc(detail) + '</pre>' : '';
  let persistedBtn = '';
  if (persistedPath && selection.key) {
    const toolURL = NZ_CONTRACT.API.sessions_tool_result + '?key=' + encodeURIComponent(selection.key) +
      '&node=' + encodeURIComponent(selection.node || 'local') +
      '&path=' + encodeURIComponent(persistedPath);
    persistedBtn = '<a class="tr-persisted" href="' + escAttr(toolURL) +
      '" target="_blank" rel="noopener noreferrer" title="查看完整输出">📎 打开完整输出</a>';
  }
  return '<details class="tr-wrap"><summary class="tr-summary">' +
    esc(summary) + '</summary>' + detailHtml + persistedBtn + '</details>';
}

// eventImagesHtml: user-message thumbnails. The click target is the
// full-size attachment URL when image_paths is populated, else the data URI;
// `?v=<time>` cache-busts a GC'd attachment. Click handling is delegated
// (lightbox.js), so it survives innerHTML re-renders.
function eventImagesHtml(e) {
  if (!e.images || e.images.length === 0) return '';
  const paths = e.image_paths || [];
  const cacheBust = e.time ? ('&v=' + e.time) : '';
  return '<div class="event-images">' + e.images.map((src, i) => {
    const p = paths[i] || '';
    let full = src;
    if (p && selection.key) {
      full = NZ_CONTRACT.API.sessions_attachment + '?key=' + encodeURIComponent(selection.key) +
        '&path=' + encodeURIComponent(p) + cacheBust;
    }
    return '<img src="' + escAttr(src) + '" loading="lazy" ' +
      'data-full="' + escAttr(full) + '" ' +
      'data-thumb="' + escAttr(src) + '">';
  }).join('') + '</div>';
}

// eventActionsHtml: copy / ask-aside buttons, one shared >500-char gate so
// they cannot drift apart (both hover-revealed via .hover-only).
function eventActionsHtml(e, cleanRaw) {
  const isLong = !!cleanRaw && cleanRaw.length > 500;
  const copyBtn = isLong && (e.type === 'text' || e.type === 'user')
    ? '<button class="event-copy-btn hover-only" type="button" data-raw="' + escAttr(cleanRaw) + '" data-action="event-copy" title="复制" aria-label="复制消息">复制</button>'
    : '';
  const askBtn = isLong && e.type === 'text'
    ? '<button class="event-ask-btn hover-only" type="button" data-raw="' + escAttr(cleanRaw) + '" data-msg-time="' + (e.time || 0) + '" data-action="ask-aside" title="基于此内容追问">' + ICONS.preview + ' 追问</button>'
    : '';
  return { copyBtn, askBtn };
}

// eventHtml renders one EventEntry bubble via EVENT_WHOLE or EVENT_CONTENT.
// opts.includeInternal=true keeps tool_use/task_*/agent/result events the
// parent view hides — agent_view.js's sub-agent panel needs them, since a
// team member's work is almost entirely tool_use.
export function eventHtml(/** @type {EventEntry} */ e, opts) {
  const includeInternal = !!(opts && opts.includeInternal);
  if (shouldHideEvent(e, includeInternal)) return '';
  if (EVENT_WHOLE.has(e.type)) return EVENT_WHOLE.get(e.type)(e);
  const icon = eventIconHtml(e);

  // Strip redundant "[+N image(s)]" suffix when thumbnails are present.
  let cleanRaw = e.detail || e.summary || '';
  if (e.images && e.images.length > 0) cleanRaw = cleanRaw.replace(/ \[\+\d+ image\(s\)\]$/, '');

  const content = (EVENT_CONTENT.get(e.type) || defaultEventChip)(e, cleanRaw);
  const imgHtml = eventImagesHtml(e);
  const { copyBtn, askBtn } = eventActionsHtml(e, cleanRaw);

  const timeAttr = e.time ? ' data-time="' + e.time + '" title="' + escAttr(formatTimeFull(e.time)) + '"' : '';
  // data-uuid: the backend's entry identity, the dedup key for a restart's
  // history replay (docs/rfc/dashboard-event-uuid-idempotent-render.md).
  const uuidAttr = e.uuid ? ' data-uuid="' + escAttr(e.uuid) + '"' : '';
  return '<div class="event ' + escAttr(e.type || '') + '"' + timeAttr + uuidAttr + '>' +
    '<span class="event-icon">' + icon + '</span>' +
    '<div class="event-content">' + content + imgHtml + copyBtn + askBtn + '</div></div>';
}

// eventAlreadyRendered reports whether a .event with this uuid is already in
// the given scroll container. DOM is the single source of truth for render
// dedup — no parallel JS Set — so trimEventsScroll() eviction and full
// innerHTML rebuilds keep the dedup set automatically consistent (an element
// trimmed from the DOM stops matching, exactly as intended). Empty/absent
// uuid never matches (returns false) so uuid-less events are never swallowed.
// CSS.escape guards the attribute selector even though uuids are hex — keeps
// the "all selector inputs are escaped" invariant if the uuid source ever
// changes shape. See docs/rfc/dashboard-event-uuid-idempotent-render.md.
export function eventAlreadyRendered(scrollEl, uuid) {
  if (!scrollEl || !uuid) return false;
  const sel = (typeof CSS !== 'undefined' && CSS.escape) ? CSS.escape(uuid) : uuid;
  return !!scrollEl.querySelector('.event[data-uuid="' + sel + '"]');
}

// Expose the bubble renderer for agent_view.js (RFC v4 agent-team-ui §3.6).
// The sub-agent transcript panel must use the same layout as the parent view —
// tool_result folding, markdown, image thumbnails, copy/ask buttons — so one
// eventHtml is the source of truth (agent_view imports it; a past revision
// referenced a non-existent stub and silently lost the entire bubble UI).

// Walk a list of events and produce an HTML string with time dividers inserted
// whenever the gap between adjacent VISIBLE (non-null) bubbles exceeds
// EVENT_DIVIDER_GAP_MS. `prevTime` seeds the comparison against whatever is
// already rendered in the DOM (0 = always emit a leading divider for the first
// visible event).
export function renderEventsWithDividers(events, prevTime, opts) {
  let out = '';
  let lastTime = prevTime || 0;
  for (const e of events) {
    const h = eventHtml(e, opts);
    if (!h) continue;
    const t = e.time || 0;
    if (t && (lastTime === 0 || t - lastTime >= EVENT_DIVIDER_GAP_MS)) {
      out += timeDividerHtml(t);
    }
    out += h;
    if (t) lastTime = t;
  }
  return out;
}

// leadingTimeDivider returns the scroller's first time divider when it
// precedes every rendered bubble (the divider renderEventsWithDividers emits
// for prevTime=0); null when a bubble comes first or nothing is rendered.
export function leadingTimeDivider(el) {
  if (!el) return null;
  for (const c of el.children) {
    if (!c.classList) continue;
    if (c.classList.contains('event-time-divider')) return c;
    if (c.classList.contains('event')) return null;
  }
  return null;
}

// removeOptimisticMsg drops the optimistic user bubble a send rendered. With
// a send id (the WS send_ack echoes the `id` the send frame carried) only that
// send's bubble goes — a busy/error ack for the second of two in-flight sends
// must not eat the first one's bubble (#2430). Without an id (legacy servers,
// HTTP-path send_error) fall back to the oldest bubble on screen.
export function removeOptimisticMsg(sendId) {
  const root = document.getElementById('events-scroll') || document;
  let opt;
  if (sendId) {
    const sel = (typeof CSS !== 'undefined' && CSS.escape) ? CSS.escape(sendId) : sendId;
    opt = root.querySelector('.optimistic-msg[data-send-id="' + sel + '"]');
  } else {
    opt = root.querySelector('.optimistic-msg');
  }
  if (opt) opt.remove();
}
