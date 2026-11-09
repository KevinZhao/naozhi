// relay.js — a message the Planner relayed with the user's own words
// (source "relay", docs/rfc/planner.md D98). The gateway's envelope leads it:
//   <naozhi-relay at="M/D HH:mm">
//   用户 M/D HH:mm 在 Planner 对话里说：「…」   (or: Planner 在通知里问用户「…」，用户 … 选了「…」。)
//   </naozhi-relay>
//   Planner 的说明：
//   …
// The bubble shows the words as a quote and the Planner's text after them, so
// it reads whose is which; anything else renders as written.

import { esc } from './nz_util.js';

const ENVELOPE_RE = /^<naozhi-relay[^>\n]*>\n([\s\S]*?)\n<\/naozhi-relay>(?:\nPlanner 的说明：\n([\s\S]*))?$/;

// parseRelay reads the envelope: the lines of the user's words and the
// Planner's text ('' for none), or null when text is not one.
export function parseRelay(text) {
  const m = ENVELOPE_RE.exec(String(text || '').trim());
  if (!m) return null;
  return { words: m[1].split('\n').filter((l) => l.trim()), note: (m[2] || '').trim() };
}

// NOTE_LINE_MAX caps the Planner's text in the folded line, in characters.
const NOTE_LINE_MAX = 60;

// noteLine is the first line of the Planner's text for the fold's summary.
function noteLine(note) {
  const line = note.split('\n').map((l) => l.trim()).find(Boolean) || '';
  const chars = Array.from(line);
  return chars.length > NOTE_LINE_MAX ? chars.slice(0, NOTE_LINE_MAX - 1).join('') + '…' : line;
}

// relayBubbleHtml renders a relayed message; renderBody renders markdown.
// The user's words stay in view (they are what the session may act on);
// the Planner's text folds to its first line, opened on a click.
export function relayBubbleHtml(text, renderBody) {
  const r = parseRelay(text);
  if (!r) return '<div class="custody-source">Planner 转达</div>' + renderBody(text);
  return '<div class="custody-source">Planner 转达 · 附你的原话</div>' +
    '<blockquote class="relay-words">' + r.words.map((l) => '<div>' + esc(l) + '</div>').join('') + '</blockquote>' +
    (r.note ? '<details class="relay-note"><summary class="relay-note-h">Planner 的说明 · ' + esc(noteLine(r.note)) + '</summary>' +
      '<div class="relay-note-body">' + renderBody(r.note) + '</div></details>' : '');
}
