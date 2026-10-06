// node --test scripts/dashboard-leaves.test.mjs
// The dashboard's pure leaf helpers: file_ref_parse.js, nz_util.js,
// cron_format.js and session_ident.js (caps.leaves). They load in node with
// window, a MutationObserver and a bare document stubbed before the import.
// TZ is pinned to a zone with DST so the calendar-day and clock-time cases
// are the same on every machine; Date.now is replaced per test where a
// formatter reads it.
import { test, afterEach } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

process.env.TZ = 'America/New_York';
globalThis.window = globalThis;
globalThis.MutationObserver = class { observe() {} disconnect() {} };
globalThis.document = { documentElement: {}, addEventListener() {}, getElementById: () => null };

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const STATIC = '../internal/server/static/';
const { FILE_REF_HAS_EXT, fencedPathList, fileRefCode, isFileRefCandidate, splitPathLine } = await import(STATIC + 'file_ref_parse.js');
const nzu = await import(STATIC + 'nz_util.js');
const cf = await import(STATIC + 'cron_format.js');
const si = await import(STATIC + 'session_ident.js');
const { cronJobCostCache } = await import(STATIC + 'cron_state.js');
const { sessionList } = await import(STATIC + 'state.js');
const { wsm, WS_STATES } = await import(STATIC + 'ws_manager.js');
const { NZ_CONTRACT } = await import(STATIC + 'contract.js');

const realNow = Date.now;
afterEach(() => { Date.now = realNow; });
const at = (y, mo, d, h = 0, mi = 0, s = 0) => new Date(y, mo - 1, d, h, mi, s).getTime();
const freeze = (ms) => { Date.now = () => ms; };
const MIN = 60 * 1000, HOUR = 60 * MIN;

// goConsts returns [name, value] for every `Name Type = "value"` constant in a
// Go source file, so a wire value added on the backend reaches these tables.
function goConsts(rel, type) {
  const src = fs.readFileSync(path.join(ROOT, rel), 'utf8');
  const out = [...src.matchAll(new RegExp('^\\s*(\\w+)\\s+' + type + '\\s*=\\s*"([^"]*)"', 'gm'))].map((m) => [m[1], m[2]]);
  assert.ok(out.length >= 5, `found only ${out.length} ${type} constants in ${rel}: the scan lost the file`);
  return out;
}

// --- file_ref_parse.js ---

test('isFileRefCandidate takes slash paths and bare names with a line suffix', () => {
  for (const s of ['src/foo.go', './a/b.ts:42', '../x/y.js', '/abs/p.go', 'manifests/ec2nodeclass.yaml:9',
    'a/b.go:10-20', 'a/b.go:10:5', 'option_install_gpu_nodegroups.sh:1838-1883', 'foo.go:12:3', '语文/诊断.md']) {
    assert.equal(isFileRefCandidate(s), true, s);
  }
  for (const s of ['', 'foo.go', 'word', 'http://x/y', 'https://x/y.go', 'a b/c.go', 'a/b.go:', 'foo.go:abc',
    'a/b:c/d.go', 'a/b.go:1-2-3', 'Makefile:12']) {
    assert.equal(isFileRefCandidate(s), false, s);
  }
});

test('splitPathLine keeps a line or range and drops a trailing column', () => {
  assert.deepEqual(splitPathLine('a/b.go:42'), { path: 'a/b.go', line: '42' });
  assert.deepEqual(splitPathLine('a/b.go:10-20'), { path: 'a/b.go', line: '10-20' });
  assert.deepEqual(splitPathLine('a/b.go:10:5'), { path: 'a/b.go', line: '10' });
  assert.deepEqual(splitPathLine('a/b.go'), { path: 'a/b.go', line: '' });
  assert.deepEqual(splitPathLine('a/b.go:x'), { path: 'a/b.go:x', line: '' });
});

test('FILE_REF_HAS_EXT wants a dot and an alphanumeric tail', () => {
  for (const s of ['a.go', 'index.html', '.bashrc', 'x.H2']) assert.equal(FILE_REF_HAS_EXT.test(s), true, s);
  for (const s of ['Makefile', 'a.', 'a.go~', 'v1']) assert.equal(FILE_REF_HAS_EXT.test(s), false, s);
});

test('fencedPathList returns rows only when every line is a path with an extension', () => {
  assert.deepEqual(fencedPathList('src/a.go\n\n  docs/b.md:12  \n'), [
    { path: 'src/a.go', note: '' }, { path: 'docs/b.md:12', note: '' },
  ]);
  assert.deepEqual(fencedPathList('语文/诊断.md   ← 待生成\nbar/q.html  # 答案'), [
    { path: '语文/诊断.md', note: '← 待生成' }, { path: 'bar/q.html', note: '# 答案' },
  ]);
  assert.deepEqual(fencedPathList('only/one.ts'), [{ path: 'only/one.ts', note: '' }]);
  for (const code of ['', '\n  \n', 'src/a.go\nfunc main() {}', '@angular/core', 'github.com/gin-gonic/gin',
    '/api/v1/users', '2024/01/02', '1/2', 'src/a.go\nsrc/Makefile', 'a/' + 'x'.repeat(512) + '.go']) {
    assert.equal(fencedPathList(code), null, JSON.stringify(code.slice(0, 40)));
  }
});

test('fileRefCode wraps without escaping, md-code by default and bare for an empty class', () => {
  assert.equal(fileRefCode('a/b.go'), '<code class="md-code">a/b.go</code>');
  assert.equal(fileRefCode('a/b.go', ''), '<code>a/b.go</code>');
  assert.equal(fileRefCode('a/b.go', 'x'), '<code class="x">a/b.go</code>');
  assert.equal(fileRefCode('&lt;b&gt;'), '<code class="md-code">&lt;b&gt;</code>');
});

// --- nz_util.js ---

test('esc escapes &, < and > only; escAttr adds both quotes without double-escaping', () => {
  assert.equal(nzu.esc('<a href="x" title=\'y\'>&amp;</a>'), '&lt;a href="x" title=\'y\'&gt;&amp;amp;&lt;/a&gt;');
  assert.equal(nzu.escAttr('"\'<&>'), '&quot;&#39;&lt;&amp;&gt;');
  for (const v of [undefined, null, '', 0]) {
    assert.equal(nzu.esc(v), '');
    assert.equal(nzu.escAttr(v), '');
  }
  assert.equal(nzu.esc(42), '42');
});

test('formatCostUSD shows four decimals under a cent and nothing for no spend', () => {
  for (const v of [undefined, 0, -1]) assert.equal(nzu.formatCostUSD(v), '');
  assert.equal(nzu.formatCostUSD(0.0044), '$0.0044');
  assert.equal(nzu.formatCostUSD(0.0001), '$0.0001');
  assert.equal(nzu.formatCostUSD(0.01), '$0.01');
  assert.equal(nzu.formatCostUSD(12.3), '$12.30');
});

test('formatDurationShort switches unit at each boundary and pads the second field', () => {
  const cases = [
    [undefined, '—'], [0, '—'], [-5, '—'], [850, '850ms'], [1000, '1.0s'], [9500, '9.5s'], [10000, '10s'],
    [59000, '59s'], [60000, '1m 00s'], [65000, '1m 05s'], [195000, '3m 15s'], [3599000, '59m 59s'],
    [3600000, '1h 00m'], [3720000, '1h 02m'], [36000000 + 15 * MIN, '10h 15m'],
  ];
  for (const [ms, want] of cases) assert.equal(nzu.formatDurationShort(ms), want, String(ms));
});

// 119.6s rounds to 120s: 2 minutes, not "1m 60s".
test('formatDurationShort and formatRunDuration carry a rounded-up second into the minute', () => {
  assert.equal(nzu.formatDurationShort(119600), '2m 00s');
  assert.equal(nzu.formatDurationShort(3599600), '1h 00m');
  assert.equal(nzu.formatRunDuration(119600), '2m 0s');
  assert.equal(nzu.formatRunDuration(179700), '3m 0s');
});

test('formatRunDuration drops a .0 and has no unit past minutes', () => {
  const cases = [
    [undefined, ''], [0, ''], [-1, ''], [999, '999ms'], [1000, '1s'], [1500, '1.5s'], [59900, '59.9s'],
    [60000, '1m 0s'], [61000, '1m 1s'], [7200000, '120m 0s'],
  ];
  for (const [ms, want] of cases) assert.equal(nzu.formatRunDuration(ms), want, String(ms));
});

test('formatBytes picks the largest unit the count reaches', () => {
  const cases = [
    [undefined, ''], [0, ''], [-1, ''], [1, '1 B'], [1023, '1023 B'], [1024, '1 KiB'], [1536, '2 KiB'],
    [1 << 20, '1 MiB'], [5.4 * (1 << 20), '5 MiB'], [1 << 30, '1.0 GiB'], [1.5 * (1 << 30), '1.5 GiB'],
    [4 * (1 << 30), '4.0 GiB'],
  ];
  for (const [n, want] of cases) assert.equal(nzu.formatBytes(n), want, String(n));
});

test('isCronSessionKey is a case-sensitive cron: prefix on a string', () => {
  assert.equal(nzu.isCronSessionKey('cron:job1'), true);
  assert.equal(nzu.isCronSessionKey('cron:'), true);
  for (const k of ['xcron:job', 'CRON:job', 'cron', '', null, undefined, 42]) assert.equal(nzu.isCronSessionKey(k), false, String(k));
});

test('every backend RunState and running has its own dot and label', () => {
  const states = goConsts('internal/runtelemetry/state.go', 'RunState').map(([, v]) => v).concat('running');
  const dots = states.map(nzu.runStateDot), labels = states.map(nzu.runStateLabel);
  for (const [i, s] of states.entries()) {
    assert.notEqual(dots[i], 'unk', `RunState ${s} has no dot`);
    assert.notEqual(labels[i], s, `RunState ${s} has no label`);
  }
  assert.equal(new Set(dots).size, states.length, 'two states share a dot');
  assert.equal(new Set(labels).size, states.length, 'two states share a label');
  assert.equal(nzu.runStateDot('weird'), 'unk');
  assert.equal(nzu.runStateLabel('weird'), 'weird');
  assert.equal(nzu.runStateLabel(''), '未知');
});

test('sessionExit is null for a live session and names every contract death_reason', () => {
  for (const st of NZ_CONTRACT.ENUMS.SESSION_STATE.filter((s) => s !== 'dead')) {
    assert.equal(nzu.sessionExit(st, 'cli_exited'), null, st);
  }
  const reclaimed = ['evicted', 'idle_timeout', 'released'];
  for (const r of NZ_CONTRACT.ENUMS.DEATH_REASON) {
    const x = nzu.sessionExit('dead', r);
    assert.ok(!x.text.startsWith('进程已退出'), `death_reason ${r} has no wording`);
    assert.equal(x.crashed, !reclaimed.includes(r), r);
  }
});

test('sessionExit reads an exit code or signal off cli_exited and falls back to the raw reason', () => {
  const { CODE, SIGNAL } = NZ_CONTRACT.DEATH_REASON_PREFIX;
  const text = (r) => nzu.sessionExit('dead', r).text;
  assert.equal(text(CODE + '-1'), 'CLI 进程被信号终止');
  assert.equal(text(CODE + '2'), 'CLI 进程异常退出（退出码 2）');
  assert.equal(text(SIGNAL + 'SIGKILL'), 'CLI 进程被信号 SIGKILL 终止');
  assert.equal(text(CODE + 'x'), '进程已退出（' + CODE + 'x）');
  assert.equal(text(SIGNAL), '进程已退出（' + SIGNAL + '）');
  assert.equal(text('__proto__'), '进程已退出（__proto__）');
  assert.equal(text('toString'), '进程已退出（toString）');
  assert.equal(text(''), '进程已退出');
  assert.equal(nzu.sessionExit('dead', CODE + '137').crashed, true);
});

test('sessionExit\'s title says what the next send does for each startup_failure class', () => {
  const title = (f, detail) => nzu.sessionExit('dead', 'cli_exited', detail, f).title;
  const head = 'CLI 进程退出，';
  const operator = { auth: '后端认证失败', mcp_config: 'CLI 配置错误', missing_runtime: 'CLI 运行环境缺失' };
  const automatic = ['unknown', 'resume_not_found'];
  assert.deepEqual([...NZ_CONTRACT.ENUMS.STARTUP_FAILURE_CLASS].sort(), [...Object.keys(operator), ...automatic].sort(),
    'a startup_failure class was added: decide whether it needs an operator');
  for (const [cls, cause] of Object.entries(operator)) assert.equal(title({ class: cls }), head + cause + '，需管理员修复后重试');
  for (const cls of [...automatic, '__proto__', 'toString', undefined]) assert.equal(title({ class: cls }), head + '下次发送时自动恢复', String(cls));
  assert.equal(title(undefined), head + '下次发送时自动恢复');
  assert.equal(title({ class: 'auth', new_session: true }), head + '下次发送将开启新会话（上次会话无法恢复）');
  assert.equal(title(undefined, 'exit status 1'), head + '下次发送时自动恢复\nexit status 1');
  freeze(at(2026, 6, 15, 10, 0));
  const retry = { class: 'auth', streak: 3, retry_at: at(2026, 6, 15, 10, 5, 7) };
  assert.equal(title(retry), head + 'CLI 连续启动失败（3 次），已暂停自动重试；10:05:07 后可重试，或发送 /new 立即重试');
  assert.equal(title({ ...retry, retry_at: Date.now() }), head + '后端认证失败，需管理员修复后重试');
});

test('sessionExitChipHtml marks a crash, mutes a reclaim and escapes the title', () => {
  assert.equal(nzu.sessionExitChipHtml('ready', 'killed'), '');
  assert.equal(nzu.sessionExitChipHtml('dead', 'killed', '"<x>'),
    '<span class="sc-exit sc-exit-crashed" title="进程被终止，下次发送时自动恢复\n&quot;&lt;x&gt;">⚠ 异常退出</span>');
  assert.equal(nzu.sessionExitChipHtml('dead', 'evicted'),
    '<span class="sc-exit sc-exit-reclaimed" title="为腾出容量，进程已回收，下次发送时自动恢复">已回收</span>');
});

// --- cron_format.js ---

test('firstNonEmptyLine trims to the first non-blank line and cuts by code point', () => {
  for (const v of [undefined, null, '', '  \n\t\n']) assert.equal(cf.firstNonEmptyLine(v), '');
  assert.equal(cf.firstNonEmptyLine('\n   \n  hi there  \nnext'), 'hi there');
  assert.equal(cf.firstNonEmptyLine(123), '123');
  assert.equal(cf.firstNonEmptyLine('a'.repeat(60)), 'a'.repeat(60));
  assert.equal(cf.firstNonEmptyLine('a'.repeat(61)), 'a'.repeat(60) + '…');
  assert.equal(cf.firstNonEmptyLine('abcdef', 0), 'abcdef');
  assert.equal(cf.firstNonEmptyLine('😀😀😀', 2), '😀😀…');
  assert.equal(cf.firstNonEmptyLine('😀😀', 2), '😀😀');
});

test('calendarDayDelta counts local midnights, across a DST change too', () => {
  assert.equal(cf.calendarDayDelta(at(2026, 6, 14, 23, 59), at(2026, 6, 15, 0, 1)), 1);
  assert.equal(cf.calendarDayDelta(at(2026, 6, 15, 0, 1), at(2026, 6, 15, 23, 59)), 0);
  assert.equal(cf.calendarDayDelta(at(2026, 6, 15, 1), at(2026, 6, 15, 1) - 26 * HOUR), -2);
  assert.equal(cf.calendarDayDelta(at(2026, 12, 31, 22), at(2027, 1, 1, 2)), 1);
  // 2026-03-08 is a 23-hour day in New York and 2026-11-01 a 25-hour one.
  assert.equal(at(2026, 3, 9) - at(2026, 3, 8), 23 * HOUR, 'TZ did not take effect');
  assert.equal(cf.calendarDayDelta(at(2026, 3, 8), at(2026, 3, 9)), 1);
  assert.equal(cf.calendarDayDelta(at(2026, 3, 7, 12), at(2026, 3, 9, 12)), 2);
  assert.equal(cf.calendarDayDelta(at(2026, 11, 1), at(2026, 11, 2)), 1);
  assert.equal(cf.calendarDayDelta(at(2026, 10, 31, 12), at(2026, 11, 2, 12)), 2);
});

test('formatWhenColloquial buckets a future time from minutes to days', () => {
  const now = at(2026, 6, 15, 10, 0);
  freeze(now);
  const cases = [
    [0, '—', false], [now - 1, '即将', true], [now + 30 * 1000, '片刻后', true], [now + MIN, '1 分钟后', true],
    [now + 10 * MIN - 1, '9 分钟后', true], [now + 10 * MIN, '10 分钟后', false], [now + 59 * MIN, '59 分钟后', false],
    [now + HOUR, '约 1 小时后', false], [at(2026, 6, 15, 23, 59), '约 13 小时后', false],
    [at(2026, 6, 16, 0, 0), '明早 00:00', false], [at(2026, 6, 16, 11, 59), '明早 11:59', false],
    [at(2026, 6, 16, 12, 0), '明日 12:00', false], [at(2026, 6, 18, 2, 5), '3 天后 · 02:05', false],
  ];
  for (const [ms, label, imminent] of cases) assert.deepEqual(cf.formatWhenColloquial(ms), { label, imminent }, label);
});

test('formatAgoColloquial says yesterday by calendar date, not by 24 hours', () => {
  const now = at(2026, 6, 15, 1, 0);
  freeze(now);
  const cases = [
    [0, ''], [now - 59 * 1000, '刚刚'], [now - MIN, '1 分钟前'], [now - 59 * MIN, '59 分钟前'],
    [now - HOUR, '1 小时前'], [now - HOUR - MIN, '昨天 23:59'], [now - 25 * HOUR, '昨天 00:00'],
    [now - 25 * HOUR - MIN, '2 天前'], [at(2026, 6, 8, 12), '7 天前'],
  ];
  for (const [ms, want] of cases) assert.equal(cf.formatAgoColloquial(ms), want, want);
});

test('formatRunningElapsed counts up from the start and wraps at minutes and hours', () => {
  const now = at(2026, 6, 15, 10, 0);
  freeze(now);
  const cases = [
    [0, '正在运行'], [now + 1000, '正在运行'], [now, '运行中 0s'], [now - 59999, '运行中 59s'],
    [now - 60000, '运行中 1m 0s'], [now - 61000, '运行中 1m 1s'], [now - HOUR + 1000, '运行中 59m 59s'], [now - HOUR - 2 * MIN, '运行中 1h 2m'],
  ];
  for (const [startedAt, want] of cases) assert.equal(cf.formatRunningElapsed(startedAt), want, want);
});

test('cronErrorClassLabel names every cron error class and passes others through', () => {
  const classes = goConsts('internal/runtelemetry/state.go', 'ErrorClass')
    .filter(([name, v]) => v !== '' && !name.startsWith('ErrClassSysession'));
  for (const [name, v] of classes) assert.notEqual(cf.cronErrorClassLabel(v), v, `${name} (${v}) has no label`);
  assert.equal(cf.cronErrorClassLabel('turn_failed'), '后端报错');
  for (const v of ['brand_new', 'constructor', 'toString', '__proto__']) assert.equal(cf.cronErrorClassLabel(v), v);
  for (const v of ['', undefined, null]) assert.equal(cf.cronErrorClassLabel(v), '');
});

test('cronJobLedgerCostHtml renders the 30-day figure and notes dropped entries', () => {
  delete cronJobCostCache.j1;
  assert.equal(cf.cronJobLedgerCostHtml('j1'), '');
  cronJobCostCache.j1 = { usd: 0, entries: 0, dropped: 0 };
  assert.equal(cf.cronJobLedgerCostHtml('j1'), '');
  cronJobCostCache.j1 = { usd: 1.5, entries: 4, dropped: 0 };
  const plain = cf.cronJobLedgerCostHtml('j1');
  assert.match(plain, /^<span class="ct-cost-ledger" title="近 30 天账本合计：4 条账本记录[^"]*">30 天 \$1\.50<\/span>$/);
  assert.doesNotMatch(plain, /丢弃/);
  cronJobCostCache.j1 = { usd: 0.002, entries: 4, dropped: 2 };
  assert.match(cf.cronJobLedgerCostHtml('j1'), /；账本曾丢弃 2 条，可能偏低">30 天 \$0\.0020</);
  delete cronJobCostCache.j1;
});

// --- session_ident.js ---

test('sid and discoveredKey default the node to local; the pid round-trips', () => {
  assert.equal(si.sid('k', undefined), 'k\tlocal');
  assert.equal(si.sid('k', 'n1'), 'k\tn1');
  for (const [pid, node] of [[123, undefined], [1, 'local'], [98765, 'n:with:colons'], [7, '']]) {
    const key = si.discoveredKey(pid, node);
    assert.equal(si.isDiscoveredKey(key), true, key);
    assert.equal(si.parseDiscoveredPid(key), pid, key);
  }
  assert.equal(si.discoveredKey(5), '_discovered:5:local');
  for (const k of ['x_discovered:1', 'local:1', '', null, 42]) assert.equal(si.isDiscoveredKey(k), false, String(k));
});

test('discovered items are found and dropped by pid and node together', () => {
  sessionList.discoveredItems = [{ pid: 1 }, { pid: 1, node: 'n1' }, { pid: 2, node: 'local' }];
  assert.equal(si.sameDiscovered({ pid: 2 }, 2, 'local'), true);
  assert.equal(si.sameDiscovered({ pid: 2, node: 'n1' }, 2), false);
  assert.equal(si.findDiscovered(1, 'n1').node, 'n1');
  assert.equal(si.findDiscovered(2), sessionList.discoveredItems[2]);
  assert.equal(si.findDiscovered(3), null);
  si.dropDiscovered(1, 'local');
  assert.deepEqual(sessionList.discoveredItems, [{ pid: 1, node: 'n1' }, { pid: 2, node: 'local' }]);
  sessionList.discoveredItems = [];
});

test('nodeColor is the same palette entry for an id on every load', () => {
  const cases = [['', '#1f6feb'], ['a', '#0550ae'], ['local', '#0550ae'], ['node-eu-west-1', '#0550ae'],
    ['macbook-pro.internal.example.com', '#1f6feb'], ['香港节点', '#6e40c9']];
  for (const [id, want] of cases) assert.equal(si.nodeColor(id), want, id);
});

test('isMultiNode is true once any remote node is known', () => {
  const cases = [[{}, false], [{ local: {} }, false], [{ n1: {} }, true], [{ local: {}, n1: {} }, true]];
  for (const [nodes, want] of cases) {
    sessionList.nodesData = nodes;
    assert.equal(si.isMultiNode(), want, JSON.stringify(nodes));
  }
  sessionList.nodesData = {};
});

test('projectDisplayLabel prefers a trimmed display_name; the prefix carries its own space', () => {
  assert.equal(si.projectDisplayLabel(null), '');
  assert.equal(si.projectDisplayLabel({ name: 'dir', config: { display_name: '  Shown  ' } }), 'Shown');
  assert.equal(si.projectDisplayLabel({ name: 'dir', config: { display_name: '   ' } }), 'dir');
  assert.equal(si.projectDisplayLabel({ name: '<b>' }), '<b>');
  assert.equal(si.projectDisplayLabel({}), '');
  assert.equal(si.projectDisplayPrefix(null), '');
  assert.equal(si.projectDisplayPrefix({ config: { emoji: ' 🚀 ' } }), '🚀 ');
  assert.equal(si.projectDisplayPrefix({ config: { emoji: ' ' } }), '');
  assert.equal(si.projectDisplayPrefix({ name: 'x' }), '');
});

test('matchProject takes the longest project path that contains the workspace', () => {
  sessionList.projectsData = [];
  assert.equal(si.matchProject('/w/a'), '');
  sessionList.projectsData = [{ name: 'wa', path: '/w/a/' }, { name: 'w', path: '/w' }, { name: 'wab', path: '/w/ab' }];
  assert.equal(si.matchProject('/w/a/x'), 'wa');
  assert.equal(si.matchProject('/w/a'), 'wa');
  assert.equal(si.matchProject('/w/abc'), 'w');
  assert.equal(si.matchProject('/w/ab/'), 'wab');
  assert.equal(si.matchProject('/other'), '');
  assert.equal(si.matchProject(''), '');
  sessionList.projectsData = [];
});

test('sessionTypeTag escapes the label and defaults to CLI', () => {
  assert.equal(si.sessionTypeTag(''), '<span class="sc-type-tag">CLI</span>');
  assert.equal(si.sessionTypeTag('<Kiro>'), '<span class="sc-type-tag">&lt;Kiro&gt;</span>');
});

test('node name and status: local follows the WS state, a remote its snapshot', () => {
  sessionList.nodesData = { n1: { display_name: 'Tokyo', status: 'unreachable' }, n2: {} };
  assert.equal(si.getNodeDisplayName(undefined), '本地');
  assert.equal(si.getNodeDisplayName('local'), '本地');
  assert.equal(si.getNodeDisplayName('n1'), 'Tokyo');
  assert.equal(si.getNodeDisplayName('n2'), 'n2');
  assert.equal(si.getNodeDisplayName('n3'), 'n3');
  const wsCases = [[WS_STATES.CONNECTED, 'ok'], [WS_STATES.CONNECTING, 'connecting'], [WS_STATES.AUTH, 'connecting'],
    [WS_STATES.OFF, 'offline'], [WS_STATES.DISCONNECTED, 'offline']];
  const saved = wsm.state;
  for (const [st, want] of wsCases) {
    wsm.state = st;
    assert.equal(si.getNodeStatus('local'), want, st);
    assert.equal(si.getNodeStatus(''), want, st);
  }
  wsm.state = saved;
  assert.equal(si.getNodeStatus('n1'), 'unreachable');
  assert.equal(si.getNodeStatus('n2'), 'offline');
  assert.equal(si.getNodeStatus('n3'), 'offline');
  sessionList.nodesData = {};
  assert.equal(si.statusLabelForNode('ok'), 'connected');
  assert.equal(si.statusLabelForNode('authenticating'), 'authenticating');
  assert.equal(si.statusLabelForNode('weird'), 'weird');
});

test('isInternalEvent hides exactly the contract\'s internal event types', () => {
  const internal = new Set(NZ_CONTRACT.ENUMS.EVENT_TYPE_INTERNAL);
  for (const t of NZ_CONTRACT.ENUMS.EVENT_TYPE) assert.equal(si.isInternalEvent({ type: t }), internal.has(t), t);
  assert.equal(si.isInternalEvent(null), false);
  assert.equal(si.isInternalEvent({ type: 'brand_new' }), false);
});
