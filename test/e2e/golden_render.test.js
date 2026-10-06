// @ts-check
// Golden renderings of the dashboard's three big renderers — eventHtml,
// renderMd and the sidebar (fetchSessions → renderSidebar). A split or move
// of these renderers has to reproduce the markup byte for byte (see #3025).
//
//   golden/event_render_known.json    every kind the backend sends, and the
//                                     variants a branch keys on (ask cards:
//                                     multi_select, answered, a backend
//                                     without askuser)
//   golden/event_render_unknown.json  persist_gap and types no renderer
//                                     knows, including Object.prototype names
//                                     (S19-2 changes these on purpose)
//   golden/render_md.json             every fence language, prototype-named
//                                     fences, unclosed fences, CRLF, lists,
//                                     tables, quotes, math
//   golden/sidebar.json               the sidebar rows plus the order of
//                                     sessionList.allSessionsCache (msg_nav's
//                                     session order), over managed, discovered
//                                     and pending sessions, with unread chips,
//                                     agent and workflow badges and last
//                                     responses
//   golden/sidebar_empty.json         the empty sidebar
//
// golden/pins.json holds each file's sha256 and every run checks the file
// against its pin before comparing. UPDATE_GOLDEN=1 rewrites the golden files
// and prints their new sha256 but never touches pins.json: a re-recorded
// golden fails the next run until its pin is changed by hand, and
// tools/ratchet-raises counts a changed pin as a raise that needs a ledger
// entry (scripts/ratchet-raises.jsonl). So a rendering change cannot be
// re-recorded quietly.
//
// Determinism: the timezone is UTC and the clock is fixed, so formatTimeFull
// and the cards' relative times do not depend on the machine; /static/vendor/
// is blocked so KaTeX and mermaid never load and math stays on the pending
// path; mmd-N / ktx-N ids come from module counters and are normalised.
//
//   cd test/e2e && npx playwright test golden_render.test.js --project=desktop-chrome
//   UPDATE_GOLDEN=1 npx playwright test golden_render.test.js --project=desktop-chrome
const fs = require('node:fs');
const path = require('node:path');
const crypto = require('node:crypto');
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const GOLDEN_DIR = path.join(__dirname, 'golden');
const PINS = path.join(GOLDEN_DIR, 'pins.json');
const UPDATE = process.env.UPDATE_GOLDEN === '1';

// Fixed instants: fixture times hang off T0; the page clock reads NOW.
const T0 = Date.UTC(2026, 0, 1, 12, 0, 0);
const NOW = T0 + 2 * 3600 * 1000;
const MIN = 60 * 1000;

test.use({ timezoneId: 'UTC', viewport: { width: 1280, height: 800 } });

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'renderer output does not depend on the viewport; desktop-chrome records it once');
  }
});

const sha256 = (data) => crypto.createHash('sha256').update(data).digest('hex');
const serialize = (obj) => JSON.stringify(obj, null, 2) + '\n';
const normalize = (html) => html.replace(/\bmmd-\d+\b/g, 'mmd-N').replace(/\bktx-\d+\b/g, 'ktx-N');

function readPins() {
  return JSON.parse(fs.readFileSync(PINS, 'utf8'));
}

// checkGolden compares actual with golden/<name>, after checking the file on
// disk against its pin. Under UPDATE_GOLDEN=1 it only writes the file.
function checkGolden(name, actual) {
  const file = path.join(GOLDEN_DIR, name);
  const text = serialize(actual);
  if (UPDATE) {
    fs.writeFileSync(file, text);
    console.log(`golden: wrote ${name}, sha256 ${sha256(text)} (pins.json is not updated; change the pin by hand)`);
    return;
  }
  const onDisk = fs.readFileSync(file);
  expect(sha256(onDisk), `golden/${name} does not match its sha256 in golden/pins.json: it was re-recorded without changing the pin`)
    .toBe(readPins()[name]);
  expect(actual, `the rendering differs from golden/${name}`).toEqual(JSON.parse(onDisk.toString('utf8')));
  expect(text, `golden/${name} is not byte-identical to the serialised rendering`).toBe(onDisk.toString('utf8'));
}

test('pins.json pins exactly the golden files, each to its current sha256', () => {
  test.skip(UPDATE, 'UPDATE_GOLDEN=1 only writes the golden files');
  const pins = readPins();
  const files = fs.readdirSync(GOLDEN_DIR).filter((f) => f.endsWith('.json') && f !== 'pins.json').sort();
  expect(Object.keys(pins).sort()).toEqual(files);
  expect(files).toEqual(['event_render_known.json', 'event_render_unknown.json', 'render_md.json', 'sidebar.json', 'sidebar_empty.json']);
  for (const f of files) {
    expect(pins[f], `pin for ${f}`).toMatch(/^[0-9a-f]{64}$/);
    expect(sha256(fs.readFileSync(path.join(GOLDEN_DIR, f))), `golden/${f} vs its pin`).toBe(pins[f]);
  }
});

/** @param {import('@playwright/test').Page} page */
async function openDashboard(page, mock, ready = '.session-card') {
  await page.route(/\/static\/vendor\//, (route) => route.abort());
  await page.clock.setFixedTime(NOW);
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector(ready);
}

// ─── eventHtml ────────────────────────────────────────────────────────────────

const SEL = 'dashboard:direct:2026-01-01-120000-1:myproject';
const LONG = 'This paragraph is long enough to expose the bubble toolbar. '.repeat(10).trim();
const LEAK = 'Let me check the file.\ncall\n<invoke name="Read">\n<parameter name="file_path">/tmp/a.txt</parameter>\n</invoke>';
const PNG = 'data:image/png;base64,iVBORw0KGgo=';
const ASK = {
  tool_use_id: 'toolu_ask_1',
  items: [{
    question: 'Which approach?', header: 'Approach', multi_select: false,
    options: [{ label: 'Fast', description: 'ship today' }, { label: 'Safe', description: 'one more review' }],
  }],
};
const ASK_MULTI = {
  tool_use_id: 'toolu_ask_2',
  items: [
    { question: 'Which checks?', header: 'Checks', multi_select: true, options: [{ label: 'lint' }, { label: 'race', description: '-race' }] },
    { question: 'Ship <now>?', options: [{ label: 'yes' }, { label: 'no' }] },
  ],
};
// Two backends, only claude declares askuser: featureForCurrent then keys on
// the selected session's backend instead of answering true.
const BACKENDS = {
  default: 'claude',
  backends: [{ id: 'claude', features: { askuser: true } }, { id: 'kiro', features: {} }],
};

// Each case: { name, e, opts?, selected?, backends?, answered? }. selected
// sets selection.key for that case only (the attachment / persisted-output
// links and the text icon read it); backend overrides the selected session's
// backend. backends installs BACKENDS as serverInfo.cliBackends; answered
// marks the ask card's tool_use_id as answered.
const KNOWN = [
  { name: 'system', e: { type: 'system', summary: 'session started', time: T0, uuid: 'u-sys' } },
  { name: 'system without summary', e: { type: 'system', time: T0 } },
  { name: 'user markdown', e: { type: 'user', detail: 'hello **world** and `code`', time: T0 + 1000, uuid: 'u-usr' } },
  { name: 'user summary only', e: { type: 'user', summary: 'from summary', time: T0 } },
  { name: 'user system-reminder is hidden', e: { type: 'user', detail: '<system-reminder>x</system-reminder>', time: T0 } },
  { name: 'user task-notification is hidden', e: { type: 'user', detail: '<task-notification>\nx', time: T0 } },
  { name: 'user interrupt marker is hidden', e: { type: 'user', detail: '[Request interrupted by user]', time: T0 } },
  { name: 'user interrupt for tool use is hidden', e: { type: 'user', detail: '[Request interrupted by user for tool use]', time: T0 } },
  { name: 'user long', e: { type: 'user', detail: LONG, time: T0 } },
  { name: 'user images, unselected', e: { type: 'user', detail: 'look [+2 image(s)]', images: [PNG, PNG], image_paths: ['a/1.png', 'a/2.png'], time: T0 } },
  { name: 'user images, selected', selected: {}, e: { type: 'user', detail: 'look [+2 image(s)]', images: [PNG, PNG], image_paths: ['a/1.png'], time: T0 } },
  { name: 'user images without time', selected: {}, e: { type: 'user', detail: 'look', images: [PNG], image_paths: ['a/1.png'] } },
  { name: 'text, no selection', e: { type: 'text', detail: '# Title\n\nSome *prose*.', time: T0 } },
  { name: 'text, claude session', selected: { backend: 'claude' }, e: { type: 'text', detail: 'claude says hi', time: T0 } },
  { name: 'text, kiro session', selected: { backend: 'kiro' }, e: { type: 'text', detail: 'kiro says hi', time: T0 } },
  { name: 'text long', e: { type: 'text', detail: LONG, time: T0 } },
  { name: 'text leaked tool call', e: { type: 'text', detail: LEAK, time: T0 } },
  { name: 'text leaked tool call, no prose', e: { type: 'text', detail: 'call\n<invoke name="Bash">\n</invoke>\n</function_calls>', time: T0 } },
  { name: 'text empty', e: { type: 'text', time: T0 } },
  { name: 'todo', e: { type: 'todo', summary: '2 todos', detail: JSON.stringify([{ content: 'write tests', status: 'completed' }, { content: 'ship', status: 'in_progress', activeForm: 'Shipping' }, { content: 'rest', status: 'pending' }]), time: T0 } },
  { name: 'todo malformed', e: { type: 'todo', summary: 'todos', detail: '{not json', time: T0 } },
  { name: 'thinking renders nothing', e: { type: 'thinking', detail: 'hmm', time: T0 } },
  { name: 'ask_question', e: { type: 'ask_question', summary: 'Which approach?', ask_question: ASK, time: T0 } },
  { name: 'ask_question without items', e: { type: 'ask_question', summary: 'Ask', time: T0 } },
  { name: 'ask_question with an option-less item', e: { type: 'ask_question', summary: 'Ask', ask_question: { tool_use_id: 't2', items: [{ question: 'q', options: [] }] }, time: T0 } },
  { name: 'ask_question multi_select', e: { type: 'ask_question', summary: 'Which checks?', ask_question: ASK_MULTI, time: T0 } },
  { name: 'ask_question already answered', answered: true, e: { type: 'ask_question', summary: 'Which approach?', ask_question: ASK, time: T0 } },
  { name: 'ask_question, backend with askuser', selected: { backend: 'claude' }, backends: true, e: { type: 'ask_question', summary: 'Which approach?', ask_question: ASK, time: T0 } },
  { name: 'ask_question, backend without askuser (degraded)', selected: { backend: 'kiro' }, backends: true, e: { type: 'ask_question', summary: 'Which checks?', ask_question: ASK_MULTI, time: T0 } },
  { name: 'tool_use hidden by default', e: { type: 'tool_use', summary: 'Bash', tool: 'Bash', time: T0 } },
  { name: 'tool_use internal, no tool_call', opts: { includeInternal: true }, e: { type: 'tool_use', summary: 'Bash: ls', tool: 'Bash', time: T0 } },
  { name: 'tool_use internal, kiro stdout', opts: { includeInternal: true }, e: { type: 'tool_use', summary: 'shell', time: T0, tool_call: { id: 'tc1', title: 'ls -la', kind: 'execute', status: 'completed', output_json: JSON.stringify({ items: [{ Json: { exit_status: '0', stdout: 'a\nb' } }] }) } } },
  { name: 'tool_use internal, failed, other output', opts: { includeInternal: true }, e: { type: 'tool_use', time: T0, tool_call: { id: 'tc2', name: 'fetch', status: 'failed', output_json: '{"error":"boom"}' } } },
  { name: 'tool_use internal, bad output json', opts: { includeInternal: true }, e: { type: 'tool_use', time: T0, tool_call: { id: 'tc3', output_json: 'not json' } } },
  { name: 'tool_use internal, input only', opts: { includeInternal: true }, e: { type: 'tool_use', time: T0, tool_call: { id: 'tc4', status: 'in_progress', input_json: '{"path":"/x"}' } } },
  { name: 'tool_use internal, bad input json', opts: { includeInternal: true }, e: { type: 'tool_use', time: T0, tool_call: { id: 'tc5', input_json: '{' } } },
  { name: 'tool_use internal, huge output', opts: { includeInternal: true }, e: { type: 'tool_use', time: T0, tool_call: { id: 'tc6', title: 'cat', output_json: JSON.stringify({ items: [{ Json: { stdout: 'y'.repeat(8010) } }] }) } } },
  { name: 'tool_result', e: { type: 'tool_result', summary: 'first line', detail: 'first line\nsecond <b>line</b>', time: T0 } },
  { name: 'tool_result empty', e: { type: 'tool_result', time: T0 } },
  { name: 'tool_result persisted, unselected', e: { type: 'tool_result', summary: 'big', tool: 'persisted:tool-results/abc.txt', time: T0 } },
  { name: 'tool_result persisted, selected', selected: {}, e: { type: 'tool_result', summary: 'big', detail: 'head', tool: 'persisted:tool-results/abc.txt', time: T0 } },
  { name: 'agent hidden by default', e: { type: 'agent', summary: 'reviewer started', time: T0 } },
  { name: 'agent internal', opts: { includeInternal: true }, e: { type: 'agent', summary: 'reviewer started', detail: 'Review the diff', time: T0 } },
  { name: 'task_start internal', opts: { includeInternal: true }, e: { type: 'task_start', summary: 'task 1', time: T0 } },
  { name: 'task_progress internal', opts: { includeInternal: true }, e: { type: 'task_progress', summary: 'step 2/3', time: T0 } },
  { name: 'task_done internal', opts: { includeInternal: true }, e: { type: 'task_done', summary: 'done', detail: 'all good', time: T0 } },
  { name: 'task_done hidden by default', e: { type: 'task_done', summary: 'done', time: T0 } },
  { name: 'result internal', opts: { includeInternal: true }, e: { type: 'result', summary: 'turn complete', time: T0 } },
  { name: 'result hidden by default', e: { type: 'result', summary: 'turn complete', time: T0 } },
  { name: 'escaping in type and uuid', e: { type: 'system', summary: '<img src=x onerror=1>', uuid: 'u"><b>', time: T0 } },
];

// The Object.prototype names are what a plain-object lookup (icons[e.type])
// resolves to something other than undefined; S19-2's Map tables fix them.
const UNKNOWN = [
  { name: 'persist_gap', e: { type: 'persist_gap', summary: '12 events were not persisted', detail: 'eventlog: disk full', time: T0 } },
  { name: 'persist_gap without detail', e: { type: 'persist_gap', summary: '3 events were not persisted', time: T0 } },
  { name: 'zz_future', e: { type: 'zz_future', summary: 'a kind from a newer server', detail: 'payload', time: T0 } },
  { name: 'zz_future bare', e: { type: 'zz_future', time: T0 } },
  { name: 'type constructor', e: { type: 'constructor', summary: 'proto name', detail: 'detail', time: T0 } },
  { name: 'type __proto__', e: { type: '__proto__', summary: 'proto name', time: T0 } },
  { name: 'type toString', e: { type: 'toString', summary: 'proto name', time: T0 } },
  { name: 'type toString, internal', opts: { includeInternal: true }, e: { type: 'toString', detail: 'd', time: T0 } },
  // defaultEventChip writes all three into innerHTML: each needs its esc/escAttr.
  { name: 'markup in type, summary and detail', e: { type: '<img src=x onerror=alert(1)>', summary: '<b>s</b>', detail: '"><svg onload=1>', time: T0 } },
  { name: 'markup in type and summary, no detail', e: { type: 'x"><i>', summary: '"><svg onload=2>', time: T0 } },
];

async function renderEvents(page, cases) {
  return page.evaluate(async ({ cases, sel, backends }) => {
    const t = window.nz.test;
    const { selection, sessionList, serverInfo, transcript } = await import('/static/state.js');
    const prev = { key: selection.key, node: selection.node, cliBackends: serverInfo.cliBackends };
    const out = [];
    // No await below: the cases render in one task, so no poll can interleave
    // with the temporary selection.
    for (const c of cases) {
      let saved;
      const sk = t.sid(sel, 'local');
      if (c.selected) {
        selection.key = sel;
        selection.node = 'local';
        saved = sessionList.sessionsData[sk];
        if (c.selected.backend) sessionList.sessionsData[sk] = Object.assign({}, saved, { backend: c.selected.backend });
      }
      if (c.backends) serverInfo.cliBackends = backends;
      const tuid = c.answered ? c.e.ask_question.tool_use_id : '';
      if (tuid) transcript.askAnswered.add(tuid);
      try {
        out.push({ name: c.name, input: c.e, opts: c.opts || null, html: t.eventHtml(c.e, c.opts) });
      } finally {
        if (c.selected) {
          selection.key = prev.key;
          selection.node = prev.node;
          sessionList.sessionsData[sk] = saved;
        }
        serverInfo.cliBackends = prev.cliBackends;
        if (tuid) transcript.askAnswered.delete(tuid);
      }
    }
    return out;
  }, { cases, sel: SEL, backends: BACKENDS });
}

test.describe('golden: eventHtml', () => {
  test('known kinds and their variants', async ({ page }) => {
    const mock = await startMockServer();
    try {
      await openDashboard(page, mock);
      const out = await renderEvents(page, KNOWN);
      checkGolden('event_render_known.json', out.map((c) => ({ ...c, html: normalize(c.html) })));
    } finally { mock.server.close(); }
  });

  test('persist_gap, unknown and Object.prototype-named types', async ({ page }) => {
    const mock = await startMockServer();
    try {
      await openDashboard(page, mock);
      const out = await renderEvents(page, UNKNOWN);
      checkGolden('event_render_unknown.json', out.map((c) => ({ ...c, html: normalize(c.html) })));
    } finally { mock.server.close(); }
  });
});

// ─── renderMd ─────────────────────────────────────────────────────────────────

const fence = (info, body) => '```' + info + '\n' + body + '\n```';
const MD = [
  ['plain', 'just a sentence'],
  ['empty', ''],
  ['html is escaped', '<script>alert(1)</script> & <b>bold</b>'],
  ['__bold__ at word boundaries only', 'a __bold__ b, __all__ = [], snake_case__name__x, pkg/__init__.py'],
  ['inline', '**bold** *em* _em_ ~~strike~~ `code` [link](https://example.com/a?b=1&c=2) <https://example.com> https://example.com/x'],
  ['headings', '# h1\n## h2\n### h3\n#### h4\n##### h5\n###### h6\n####### seven'],
  ['paragraphs and breaks', 'line one\nline two\n\nnew paragraph'],
  ['blockquote', '> quoted **text**\n> second line\n\nafter'],
  ['nested quote', '> outer\n> > inner'],
  ['hr', 'above\n\n---\n\nbelow'],
  ['unordered list', '- a\n- b\n  - b1\n  - b2\n- c'],
  ['ordered list', '1. one\n2. two\n   1. two.one\n3. three'],
  ['mixed list', '1. first\n   - bullet\n   - bullet\n2. second\n- loose'],
  ['task list', '- [x] done\n- [ ] todo'],
  ['deep list', '- 1\n  - 2\n    - 3\n      - 4\n        - 5\n          - 6\n            - 7'],
  ['same depth, different column', '- a\n - b\n1. c\n 2. d'],
  ['opposite kind at the depth cap', '- 1\n  - 2\n    - 3\n      - 4\n        - 5\n          - 6\n            - 7\n            1. 8'],
  ['table', '| Col A | Col B |\n|:------|------:|\n| 1 | **2** |\n| `x` | y |'],
  ['table without leading pipe', 'a | b\n--- | ---\n1 | 2'],
  ['CRLF', '# title\r\n\r\n- a\r\n- b\r\n\r\n| x | y |\r\n|---|---|\r\n| 1 | 2 |\r\nend\rline'],
  ['fence js', fence('js', 'const a = 1 < 2;')],
  ['fence js with braces', fence('js {1,3}', 'let x;')],
  ['fence python with file', fence('python:main.py', 'print("hi")')],
  ['fence c++', fence('c++', 'int main() {}')],
  ['fence c#', fence('c#', 'class A {}')],
  ['fence objective-c', fence('objective-c', '@interface A @end')],
  ['fence stray punctuation', fence('we!rd$lang', 'x')],
  ['fence go', fence('go', 'func main() {}')],
  ['fence bash', fence('bash', 'echo $HOME')],
  ['fence json', fence('json', '{"a": 1}')],
  ['fence diff', fence('diff', '- a\n+ b')],
  ['fence mermaid', fence('mermaid', 'graph TD\n  A-->B')],
  ['two mermaid fences', fence('mermaid', 'graph LR\n  X-->Y') + '\n\n' + fence('mermaid', 'graph LR\n  Y-->Z')],
  ['fence math', fence('math', 'E = mc^2')],
  ['fence latex', fence('latex', '\\frac{a}{b}')],
  ['fence tex', fence('tex', 'x^2')],
  ['fence without language', fence('', 'some code\nmore code')],
  ['fence that is a path list', fence('', '/home/user/workspace/myproject/a.go\n/home/user/workspace/myproject/b.go  # the second')],
  ['fence constructor', fence('constructor', 'x')],
  ['fence __proto__', fence('__proto__', 'x')],
  ['fence toString', fence('toString', 'x')],
  ['fence hasOwnProperty', fence('hasOwnProperty', 'x')],
  ['single-line fence', '```ls -la```'],
  ['unclosed fence', 'before\n```js\nconst streaming = true;\nnot closed'],
  ['unclosed fence without language', '```\npartial'],
  ['display math $$', 'before\n\n$$\\sum_{i=1}^n i$$\n\nafter'],
  ['display math \\[', '\\[ a^2 + b^2 = c^2 \\]'],
  ['begin environment', '\\begin{aligned} a &= b \\\\ c &= d \\end{aligned}'],
  ['inline math \\(', 'where \\(x + y\\) holds'],
  ['inline math across lines', 'where \\(x +\ny\\) holds'],
  ['inline math $', 'cost $x^2 + y$ here'],
  ['dollar amounts are not math', 'it costs $5 and $10'],
  ['code span keeps \\(', 'regex `\\(\\d+\\)` matches'],
  ['everything', '# Report\n\nIntro with **bold**.\n\n- item\n- item\n\n' + fence('js', 'x()') + '\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\n> note\n\n$$x$$'],
];

test.describe('golden: renderMd', () => {
  test('fences, prototype names, CRLF, lists, tables, quotes, math', async ({ page }) => {
    const mock = await startMockServer();
    try {
      await openDashboard(page, mock);
      const out = await page.evaluate((cases) => cases.map(([name, src]) => ({ name, input: src, html: window.nz.test.renderMd(src) })), MD);
      checkGolden('render_md.json', out.map((c) => ({ ...c, html: normalize(c.html) })));
    } finally { mock.server.close(); }
  });
});

// ─── sidebar ──────────────────────────────────────────────────────────────────

// The server's list is deliberately out of created_at order, so the sidebar
// (oldest first) and allSessionsCache only come out ordered if renderSidebar
// sorts the list it caches.
function sidebarSessions() {
  const base = { platform: 'dashboard', agent: 'general', cli_name: 'claude', cli_version: '1.0.30', last_prompt: '', node: 'local' };
  const s = (key, extra) => ({ ...base, key, state: 'ready', last_active: T0 + 30 * MIN, ...extra });
  const wf = (task_id, name, status, done, total) => ({ task_id, name, status, epoch: '00000000000000a1', version: 3, started_at: T0, counts: { total, queued: total - done, running: 0, done, failed: 0, skipped: 0, stopped: 0 } });
  return {
    sessions: [
      s('dashboard:direct:2026-01-01-120300-a:general', { workspace: '/home/user/workspace/myproject', project: 'myproject', created_at: T0 + 3 * MIN, last_prompt: 'third in myproject', last_response: 'Done: the "fix" & <its> tests pass, and the race run is clean too.' }),
      s('dashboard:direct:2026-01-01-120100-b:general', { workspace: '/home/user/workspace/myproject', project: 'myproject', created_at: T0 + 1 * MIN, last_prompt: 'first in myproject', workflows: [wf('w0', 'finished', 'completed', 2, 2)] }),
      s('dashboard:direct:2026-01-01-120200-c:reviewer', { agent: 'reviewer', workspace: '/home/user/workspace/myproject', project: 'myproject', created_at: T0 + 2 * MIN, last_prompt: 'second in myproject', state: 'running', subagents: [{ name: 'explore' }, { name: 'review', background: true }], workflows: [wf('w1', 'fan-out', 'running', 1, 2)] }),
      s('dashboard:direct:2026-01-01-120150-d:general', { workspace: '/home/user/workspace/otherproject', project: 'otherproject', created_at: T0 + 90 * 1000, last_prompt: 'other', last_response: 'short reply', workflows: [wf('w2', 'probe "q" <a&b>', 'running', 5, 8), wf('w3', '', 'paused', 0, 3), wf('w4', 'done', 'completed', 1, 1)] }),
      s('dashboard:direct:2026-01-01-120050-e:general', { node: 'remote1', workspace: '/srv/myproject', project: 'myproject', created_at: T0 + 50 * 1000, last_prompt: 'on remote1', workflows: [wf('w5', 'remote', 'running', 1, 4)] }),
      s('dashboard:direct:2026-01-01-120400-f:general', { workspace: '/tmp/x/scratch', project: 'scratch', project_fallback: true, created_at: T0 + 4 * MIN, last_prompt: 'fallback group' }),
      s('dashboard:direct:2026-01-01-120450-g:general', { workspace: '', project: '', created_at: T0 + 270 * 1000, last_prompt: 'no project at all' }),
      // Also pending in this browser: the backend copy wins, no second card.
      s('dashboard:direct:2026-01-01-120230-p:general', { workspace: '/home/user/workspace/myproject', project: 'myproject', created_at: T0 + 150 * 1000, last_prompt: 'promoted from pending' }),
      // Same created_at as b: the key breaks the tie.
      s('dashboard:direct:2026-01-01-120100-a:general', { workspace: '/home/user/workspace/myproject', project: 'myproject', created_at: T0 + 1 * MIN, last_active: 0, last_prompt: 'tie on created_at' }),
      // No created_at: sorted by last_active.
      s('dashboard:direct:2026-01-01-115900-h:general', { workspace: '/home/user/workspace/otherproject', project: 'otherproject', last_active: T0 - 5 * MIN, last_prompt: 'pre-feature payload' }),
    ],
    stats: {
      total: 10, running: 1, ready: 9, active: 10, uptime: '2h0m0s', backend: 'cc', max_procs: 20,
      default_workspace: '/home/user/workspace', agents: ['general', 'reviewer'],
      projects: [
        { name: 'myproject', path: '/home/user/workspace/myproject', favorite: false, github: true, git_remote_url: 'https://github.com/acme/myproject.git', created_at: T0 - 60 * MIN },
        { name: 'otherproject', path: '/home/user/workspace/otherproject', favorite: false, github: false, git_remote_url: '', created_at: T0 - 120 * MIN },
        { name: 'pinned-empty', path: '/home/user/workspace/pinned-empty', favorite: true, github: false, git_remote_url: '' },
      ],
      version: 7,
    },
    history_sessions: [],
    nodes: { local: { display_name: 'Local', status: 'ok' }, remote1: { display_name: 'Remote 1', status: 'ok' } },
  };
}

function sidebarDiscovered() {
  return [
    { pid: 777, session_id: 'disc-local', cwd: '/home/user/workspace/myproject', proc_start_time: 100, node: 'local', cli_name: 'claude-code', state: 'ready', started_at: T0 + 5 * MIN, summary: 'terminal session' },
    { pid: 777, session_id: 'disc-remote', cwd: '/srv/elsewhere', proc_start_time: 200, node: 'remote1', cli_name: 'claude-code', state: 'running', started_at: T0 + 20 * 1000, last_active: T0 + 6 * MIN, summary: 'remote terminal', type_label: 'kiro' },
  ];
}

// Unread completed turns, by session key (local node): one under the 99 cap,
// one over it.
const UNREAD = {
  'dashboard:direct:2026-01-01-120100-b:general': 3,
  'dashboard:direct:2026-01-01-115900-h:general': 120,
};

// Pending sessions this browser created: one the backend does not list yet
// (an unregistered workspace, so it groups by basename) and one it already
// lists (reconciled away).
const PENDING = {
  'dashboard:direct:2026-01-01-130000-n:researcher': '/home/user/scratchpad/brand-new',
  'dashboard:direct:2026-01-01-120230-p:general': '/home/user/workspace/myproject',
  'dashboard:direct:2026-01-01-130100-q:general': '/home/user/workspace/otherproject',
};

test.describe('golden: sidebar', () => {
  test('managed, discovered and pending sessions across nodes and groups', async ({ page }) => {
    const mock = await startMockServer({ sessions: sidebarSessions(), discovered: sidebarDiscovered() });
    try {
      await openDashboard(page, mock);
      await expect(page.locator('.session-card[data-key^="_discovered:"]')).toHaveCount(2);
      const out = await page.evaluate(async ({ pending, unread }) => {
        const t = window.nz.test;
        const { sessionList, perSession } = await import('/static/state.js');
        for (const [k, ws] of Object.entries(pending)) t.sessionWorkspaces[k] = ws;
        for (const [k, n] of Object.entries(unread)) perSession.unread[t.sid(k, 'local')] = n;
        await t.fetchSessions();
        const list = document.getElementById('session-list');
        return {
          rows: [...list.children].map((el) => el.outerHTML),
          allSessionsCache: sessionList.allSessionsCache.map((s) => (s.node || 'local') + ' ' + s.key),
          pendingLeft: Object.keys(t.sessionWorkspaces).sort(),
        };
      }, { pending: PENDING, unread: UNREAD });
      checkGolden('sidebar.json', { ...out, rows: out.rows.map(normalize) });
    } finally { mock.server.close(); }
  });

  test('no sessions, no projects', async ({ page }) => {
    const mock = await startMockServer({
      sessions: {
        sessions: [],
        stats: { total: 0, running: 0, ready: 0, active: 0, uptime: '0s', backend: 'cc', max_procs: 10, default_workspace: '/tmp', agents: ['general'], projects: [], version: 1 },
        nodes: { local: { display_name: 'Local', status: 'ok' } },
      },
      discovered: [],
    });
    try {
      await openDashboard(page, mock, '#session-list .no-sessions');
      const rows = await page.evaluate(() => [...document.getElementById('session-list').children].map((el) => el.outerHTML));
      checkGolden('sidebar_empty.json', { rows: rows.map(normalize) });
    } finally { mock.server.close(); }
  });
});
