// icons.js — the dashboard's glyph set: ICONS and the Clawd mascot SVG. A
// leaf (caps.leaves) with no imports.

// "Clawd" pixel mascot for claude-backend assistant turns. Sourced from
// the Custom Brand Icons set, icon `cbi:claude-clawd`
// (https://github.com/elax46/custom-brand-icons), licensed CC BY-NC-SA
// 4.0. Naozhi ships under BSL 1.1 (non-commercial Additional Use Grant
// through 2030-03-21), so the NC clause is compatible for the current
// licensed term — see ATTRIBUTIONS.md. Fill flows from currentColor so
// the rust hex lives once in dashboard.html as --nz-clawd-rust (CSS sets
// .cc-clawd { color: var(--nz-clawd-rust) }) — no inline hex in JS.
export const CLAWD_SVG = '<svg class="cc-clawd" viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg" aria-hidden="true"><path fill="currentColor" d="M4.5 6h15v5H22v2h-2.5v3h-1v2H17v-2h-1v2h-1.5v-2h-5v2H8v-2H7v2H5.5v-2h-1v-3H2v-2h2.5ZM7 8v3h1V8Zm9 0v3h1V8Z"/></svg>';

// ICONS — single source of truth for the non-SVG glyph set (#2027). Before
// this map the dashboard carried four parallel icon notations (SVG consts,
// `&#x..;` / `&#..;` HTML entities, bare Unicode glyphs like `✎ ⎘`, and raw
// `\u{..}` escapes) with the same semantic icon spelled differently at each
// call site. Centralising here means one icon → one definition → one notation.
//
// Notation rule: literal Unicode glyph characters (one form, no mixing decimal
// `&#128277;` with hex `&#x2328;`, no mixing `\u{1f464}` lowercase with
// `\u{1F916}` uppercase). Literal glyphs are the only notation that renders
// correctly in BOTH consumption contexts used here: dropped raw into innerHTML
// / template strings, and passed through esc() (which would HTML-escape an
// entity's `&` into `&amp;` and show it verbatim). SVG-backed icons
// keep dedicated *_SVG consts: CLAWD_SVG above, the rest in sidebar_project.js.
export const ICONS = {
  close:    '×', // dismiss / close affordance
  back:     '←', // mobile back
  navUp:    '▲', // previous user message
  navDown:  '▼', // next user message
  edit:     '✎', // rename / edit
  copy:     '⎘', // copy key
  trash:    '🗑', // delete
  attach:   '📎', // attach file
  mic:      '🎤', // voice input
  keyboard: '⌨', // keyboard input
  send:     '➤', // send message
  stop:     '■', // interrupt turn
  download: '⬇', // download
  downArrow:'↓', // file-row download (thinner, paired with ↗)
  preview:  '↗', // preview / ask-aside
  gear:     '⚙', // init / system event
  user:     '&gt;_', // user event — brand ">_" terminal prompt mark (rust mono, see .event.user .event-icon)
  spark:    '✦', // assistant text event (non-claude backends)
  todo:     '☰', // todo event
  robot:    '🤖', // subagent badge / agent count
  galleryPrev:  '‹', // lightbox previous image
  galleryNext:  '›', // lightbox next image
  zoomOut:      '−', // lightbox zoom out
  zoomIn:       '+', // lightbox zoom in
  rotateLeft:   '↺', // lightbox rotate left
  rotateRight:  '↻', // lightbox rotate right
};
