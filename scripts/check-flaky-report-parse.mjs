#!/usr/bin/env node
// check-flaky-report-parse.mjs — the flaky-report job in .github/workflows/ci.yml
// turns failed-job logs into `[flaky] <test>` issue titles. Nothing else
// exercises that parsing: it runs only on a master red, and a regex that stops
// matching fails open — the job succeeds and files nothing, which reads
// exactly like a green week.
//
// This cuts the job's collectFailures and issueBody functions out of the
// workflow (so what is checked is what ships) and runs them over log lines
// copied verbatim from real runs. It also checks that flaky-report waits on every job that runs
// tests: a job missing from its needs files nothing when it alone is red, and
// otherwise is reported only if it happened to finish first. Last, it checks
// Playwright's retries and trace settings (config, projects, CI flags and
// per-spec overrides): a retry turns a flake green before flaky-report sees
// it, and with no retry a trace that records only on a retry records nothing.
//
// Loading the Playwright config needs test/e2e's npm install.
//
//   node scripts/check-flaky-report-parse.mjs
import fs from 'node:fs';
import { createRequire } from 'node:module';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const WORKFLOW = path.join(ROOT, '.github', 'workflows', 'ci.yml');
const yml = fs.readFileSync(WORKFLOW, 'utf8');

// The function runs from its declaration to the closing brace at the same
// indent; YAML indentation makes that boundary exact. A renamed or deleted
// function fails here instead of silently at 3am.
function workflowFunction(name) {
  const lines = yml.split('\n');
  const start = lines.findIndex(l => new RegExp(`^ *function ${name}\\(`).test(l));
  const indent = start < 0 ? '' : lines[start].match(/^ */)[0];
  const end = lines.findIndex((l, i) => i > start && l === `${indent}}`);
  if (start < 0 || end < 0) {
    console.error(`check-flaky-report-parse: function ${name} not found in ci.yml — did the flaky-report script change shape?`);
    process.exit(1);
  }
  return new Function(`${lines.slice(start, end + 1).join('\n')}\nreturn ${name};`)();
}
const collectFailures = workflowFunction('collectFailures');
const issueBody = workflowFunction('issueBody');

// failures() runs one job log through the reporter's own parser: each key is
// one issue title, so a name that appears twice must come back once.
function failures(log) {
  const out = new Map();
  collectFailures(log, 'job', out);
  return out;
}

const ESC = '\u001b';
const cases = [
  {
    what: 'go test failure (job `test`, run 33974275550)',
    log: '2026-09-09T04:11:02.1234567Z --- FAIL: TestReverseServer_Reconnect_closesOldConn (2.01s)\n',
    want: ['TestReverseServer_Reconnect_closesOldConn'],
  },
  {
    what: 'go test subtest failure files under its top-level test',
    log: '--- FAIL: TestWaitServiceActive/slow_cold_start (0.30s)\n',
    want: ['TestWaitServiceActive'],
    subtests: { TestWaitServiceActive: ['slow_cold_start'] },
  },
  {
    // go test prints a FAIL line for the parent and for each failed subtest;
    // keyed by the full path that is three issues for one flake (#3370,
    // #3371, #3393 were all TestReset_RetiresDeadCLIShim).
    what: 'a test and its failed subtests file one issue',
    log: '--- FAIL: TestReset_RetiresDeadCLIShim (4.02s)\n'
      + '    --- FAIL: TestReset_RetiresDeadCLIShim/no_process (2.01s)\n'
      + '    --- FAIL: TestReset_RetiresDeadCLIShim/dead_process (2.01s)\n',
    want: ['TestReset_RetiresDeadCLIShim'],
    subtests: { TestReset_RetiresDeadCLIShim: ['no_process', 'dead_process'] },
  },
  {
    what: 'nested and punctuated subtest names are kept whole',
    log: '--- FAIL: TestParse (0.00s)\n'
      + '    --- FAIL: TestParse/group/#01 (0.00s)\n'
      + '    --- FAIL: TestParse/key=a.b-c (0.00s)\n'
      + '    --- FAIL: TestParse/f(x) (0.00s)\n',
    want: ['TestParse'],
    subtests: { TestParse: ['group/#01', 'key=a.b-c', 'f(x)'] },
  },
  {
    what: 'a test whose name extends another is a separate issue',
    log: '--- FAIL: TestSend (0.00s)\n--- FAIL: TestSendAll (0.00s)\n',
    want: ['TestSend', 'TestSendAll'],
  },
  {
    what: 'Playwright spec failure (job `e2e`, run 34670407318)',
    log: '2026-09-12T03:28:39.5435848Z   ✘   44 [desktop-chrome] › dashboard.test.js:667:3 › Auth modal & login › Enter key in token input triggers login (30.0s)\n',
    want: ['dashboard.test.js › Auth modal & login › Enter key in token input triggers login'],
  },
  {
    what: 'Playwright spec failure with the reporter colouring the marker',
    log: `${ESC}[31m  ✘${ESC}[39m  7 [desktop-chrome] › events_panel.test.js:12:3 › events panel renders (1.2s)\n`,
    want: ['events_panel.test.js › events panel renders'],
  },
  {
    what: 'the run summary that repeats a failed spec must not file a second issue',
    log: '  ✘   44 [desktop-chrome] › dashboard.test.js:667:3 › a spec (30.0s)\n'
      + '  1) [desktop-chrome] › dashboard.test.js:667:3 › a spec\n'
      + '  1 failed\n'
      + '    [desktop-chrome] › dashboard.test.js:667:3 › a spec\n',
    want: ['dashboard.test.js › a spec'],
  },
  {
    what: 'Playwright spec failure in the mobile-safari project (job `e2e-mobile-safari`)',
    log: '2026-10-04T02:14:51.0712345Z   ✘   9 [mobile-safari] › mobile.test.js:51:3 › Mobile dashboard › on mobile: sidebar starts visible (list view) (5.3s)\n',
    want: ['mobile.test.js › Mobile dashboard › on mobile: sidebar starts visible (list view)'],
  },
  {
    what: 'a passing run files nothing',
    log: '  ✓  44 [desktop-chrome] › dashboard.test.js:667:3 › a spec (1.0s)\n  314 passed (41.9s)\n',
    want: [],
  },
  {
    // Run 34957007026: the runner logs the step's script before running it, so
    // both shapes appear twice — once quoted as source, once as output. Each
    // pair has to normalise to one name or the reporter files two issues for
    // one test (it filed the spurious #2723 before the quote was handled).
    what: 'the runner echoing the step script must not double-file',
    log: `${ESC}[36;1mecho "  ✘  1 [desktop-chrome] › probe_synthetic.test.js:1:1 › flaky probe synthetic spec (0.1s)"${ESC}[0m\n`
      + '  ✘  1 [desktop-chrome] › probe_synthetic.test.js:1:1 › flaky probe synthetic spec (0.1s)\n'
      + `${ESC}[36;1mecho "--- FAIL: TestFlakyProbeSynthetic (0.00s)"${ESC}[0m\n`
      + '--- FAIL: TestFlakyProbeSynthetic (0.00s)\n',
    want: ['TestFlakyProbeSynthetic', 'probe_synthetic.test.js › flaky probe synthetic spec'],
  },
  {
    what: 'the probe-fail job (both shapes in one dispatch)',
    log: yml.split('\n').filter(l => /^ *echo "(--- FAIL|  ✘)/.test(l))
      .map(l => l.replace(/^ *echo "/, '').replace(/"$/, '') + '\n').join(''),
    want: ['TestFlakyProbeSynthetic', 'probe_synthetic.test.js › flaky probe synthetic spec'],
  },
];

// The issue body names the run and, for a Go test, the subtests that failed:
// with the title keyed by the top-level test, the body is the only place the
// failing subtest shows.
const RUN = 'https://github.com/o/r/actions/runs/1';
const bodyCases = [
  {
    what: 'a test with no failed subtests gets the one-line body',
    args: ['0123456789abcdef', 'test (2)', RUN, new Set()],
    want: `master red on 01234567 — job \`test (2)\`, [run](${RUN}).`,
  },
  {
    what: 'the failed subtests are listed under the run line',
    args: ['0123456789abcdef', 'test (2)', RUN, new Set(['no_process', 'f(x)'])],
    want: `master red on 01234567 — job \`test (2)\`, [run](${RUN}).\n\nFailed subtests: \`no_process\`, \`f(x)\`.`,
  },
];

// jobs() maps each job key under `jobs:` to its body. Comment lines are
// dropped before anything else: a comment sits above the job it describes, so
// it would otherwise be read into the previous job's body or, at column 0, as
// the end of `jobs:`.
function jobs(text) {
  const head = text.match(/^jobs:\n/m);
  const out = new Map();
  if (!head) return out;
  let name = null;
  for (const line of text.slice(head.index + head[0].length).split('\n')) {
    if (/^\s*#/.test(line)) continue;
    if (/^\S/.test(line)) break;
    const m = line.match(/^  ([\w-]+):\s*$/);
    if (m) {
      name = m[1];
      out.set(name, '');
    } else if (name) {
      out.set(name, out.get(name) + line + '\n');
    }
  }
  return out;
}
function needsOf(body) {
  const m = (body ?? '').match(/^    needs:\s*(?:\[([^\]\n]*)\]|([\w-]+))\s*$/m);
  if (!m) return [];
  return (m[1] ?? m[2]).split(',').map(s => s.trim()).filter(Boolean);
}

// Test jobs flaky-report deliberately does not wait on, with the reason.
const UNREPORTED = {
  'test-quarantined': 'continue-on-error; it runs only tests already filed as flaky',
};
const TEST_JOB = /\bgo test\b|ci-test-shard\.sh|playwright test/;

// unwatched() lists the test jobs whose failure flaky-report would not see. An
// aggregate in its needs covers a job only if it runs on that job's failure
// (`if: always()`) and fails with it; otherwise the aggregate goes skipped or
// green and nothing is filed.
function unwatched(text, exempt) {
  const all = jobs(text);
  const reported = needsOf(all.get('flaky-report'));
  const aggregates = job => reported.some(r => needsOf(all.get(r)).includes(job)
    && /^    if:.*\balways\(\)/m.test(all.get(r)) && all.get(r).includes(`needs.${job}.result`));
  const testJobs = [...all].filter(([, body]) => TEST_JOB.test(body)).map(([name]) => name);
  const missing = testJobs.filter(job => !(job in exempt) && !reported.includes(job) && !aggregates(job));
  return { testJobs, missing };
}

const needsCases = [
  {
    what: 'a comment naming `go test` above a job is not read into the job before it',
    yml: 'on: push\njobs:\n  lint:\n    runs-on: x\n  # the go test suite\n  unit:\n    steps:\n      - run: go test ./...\n'
      + '  flaky-report:\n    needs: [unit]\n',
    want: [],
  },
  {
    what: 'a column-0 comment between jobs does not end the job list',
    yml: 'on: push\njobs:\n  flaky-report:\n    needs: [lint]\n  lint:\n    runs-on: x\n# the browser suite\n'
      + '  e2e:\n    steps:\n      - run: npx playwright test\n',
    want: ['e2e'],
  },
  {
    what: 'an aggregate that does not check the result does not cover its needs',
    yml: 'on: push\njobs:\n  shard:\n    steps:\n      - run: go test ./...\n  agg:\n    needs: shard\n    if: always()\n    steps:\n      - run: true\n'
      + '  flaky-report:\n    needs: [agg]\n',
    want: ['shard'],
  },
  {
    what: 'an aggregate that is skipped when its need fails does not cover it',
    yml: 'on: push\njobs:\n  shard:\n    steps:\n      - run: go test ./...\n  agg:\n    needs: shard\n'
      + '    steps:\n      - run: test "${{ needs.shard.result }}" = success\n  flaky-report:\n    needs: [agg]\n',
    want: ['shard'],
  },
  {
    what: 'an aggregate that fails on its need covers it',
    yml: 'on: push\njobs:\n  shard:\n    steps:\n      - run: npx playwright test\n  agg:\n    needs: shard\n    if: always()\n'
      + '    steps:\n      - run: test "${{ needs.shard.result }}" = success\n  flaky-report:\n    needs: agg\n',
    want: [],
  },
];

// Trace modes that record a test's first run, so a failure with no retry
// still leaves trace.zip under test-results/.
const RECORDS_FIRST_RUN = new Set(['on', 'retain-on-failure', 'retain-on-first-failure', 'retain-on-failure-and-retries']);

// playwrightProblems() checks the config and every `playwright test` command
// line, since a project or a CLI flag overrides the top-level setting.
function playwrightProblems(config, commands) {
  const out = [];
  const modeOf = t => (t !== null && typeof t === 'object' ? t.mode : t);
  if (!RECORDS_FIRST_RUN.has(modeOf(config.use?.trace))) {
    out.push(`playwright.config.js: use.trace is ${JSON.stringify(config.use?.trace)}, which records nothing without a retry`);
  }
  for (const [where, scope] of [['playwright.config.js', config], ...(config.projects ?? []).map(p => [`playwright.config.js project ${p.name}`, p])]) {
    if ((scope.retries ?? 0) !== 0) out.push(`${where}: retries is ${scope.retries}, want 0`);
    const trace = scope.use?.trace;
    if (scope !== config && trace !== undefined && !RECORDS_FIRST_RUN.has(modeOf(trace))) {
      out.push(`${where}: use.trace is ${JSON.stringify(trace)}, which records nothing without a retry`);
    }
  }
  for (const cmd of commands) {
    const retries = cmd.match(/--retries[= ](\S+)/);
    if (retries && retries[1] !== '0') out.push(`ci.yml: \`${cmd.trim()}\` passes --retries ${retries[1]}`);
    const trace = cmd.match(/--trace[= ](\S+)/);
    if (trace && !RECORDS_FIRST_RUN.has(trace[1])) out.push(`ci.yml: \`${cmd.trim()}\` passes --trace ${trace[1]}`);
  }
  return out;
}

const good = { retries: 0, use: { trace: 'retain-on-failure' }, projects: [{ name: 'desktop-chrome', use: {} }] };
const playwrightCases = [
  { what: 'retries 0 with a trace on failure passes', config: good, commands: ['npx playwright test --project=x'], want: 0 },
  { what: 'the trace object form is read by its mode', config: { ...good, use: { trace: { mode: 'on' } } }, commands: [], want: 0 },
  { what: "'on-first-retry' with no retry records nothing", config: { ...good, use: { trace: 'on-first-retry' } }, commands: [], want: 1 },
  { what: 'a missing trace defaults to off', config: { ...good, use: {} }, commands: [], want: 1 },
  { what: 'retries above 0 hides flakes from flaky-report', config: { ...good, retries: 1 }, commands: [], want: 1 },
  { what: 'a project cannot re-add retries', config: { ...good, projects: [{ name: 'p', retries: 2 }] }, commands: [], want: 1 },
  { what: 'a project cannot turn the trace off', config: { ...good, projects: [{ name: 'p', use: { trace: 'off' } }] }, commands: [], want: 1 },
  { what: 'a CI command cannot pass --retries', config: good, commands: ['npx playwright test --retries=2'], want: 1 },
  { what: 'a CI command cannot pass --trace off', config: good, commands: ['npx playwright test --trace off'], want: 1 },
];

// overrideProblems() checks the per-spec overrides: a spec's
// test.describe.configure({ retries }) or test.use({ trace }) beats the config.
// Each call's argument is cut out by balancing parentheses, so a nested
// `viewport: { ... }` does not end it early.
function overrideProblems(where, src) {
  const out = [];
  for (const m of src.matchAll(/\.(use|configure)\s*\(/g)) {
    let depth = 1;
    let i = m.index + m[0].length;
    for (; i < src.length && depth > 0; i++) {
      if (src[i] === '(') depth++;
      else if (src[i] === ')') depth--;
    }
    const arg = src.slice(m.index + m[0].length, i - 1);
    const call = `${where}: .${m[1]}(${arg.replace(/\s+/g, ' ').trim()})`;
    const retries = arg.match(/\bretries\s*:\s*([^,}\s]+)/);
    if (retries && retries[1] !== '0') out.push(`${call} sets retries ${retries[1]}, want 0`);
    if (/\btrace\s*:/.test(arg)) {
      const mode = arg.match(/\btrace\s*:\s*(?:\{[^}]*?\bmode\s*:\s*)?(['"])([\w-]+)\1/);
      if (!mode || !RECORDS_FIRST_RUN.has(mode[2])) out.push(`${call} overrides trace with one that may record nothing without a retry`);
    }
  }
  return out;
}

const overrideCases = [
  { what: 'a viewport override is fine', src: "test.use({ viewport: { width: 1280, height: 800 } });", want: 0 },
  { what: 'a spec cannot add retries', src: "test.describe.configure({ mode: 'serial', retries: 2 });", want: 1 },
  { what: 'a spec may pin retries to 0', src: 'test.describe.configure({ retries: 0 });', want: 0 },
  { what: 'a spec cannot turn the trace off past a nested object', src: "test.use({ viewport: { width: 1, height: 1 }, trace: 'off' });", want: 1 },
  { what: 'a spec trace in object form is read by its mode', src: "test.use({ trace: { mode: 'retain-on-failure', snapshots: true } });", want: 0 },
  { what: 'a spec trace set from a variable cannot be checked', src: 'test.use({ trace: mode });', want: 1 },
];

// specFiles() lists what Playwright's default testMatch picks up under testDir.
function specFiles(dir) {
  const out = [];
  for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
    if (e.name === 'node_modules') continue;
    const full = path.join(dir, e.name);
    if (e.isDirectory()) out.push(...specFiles(full));
    else if (/\.(spec|test)\.[cm]?[jt]sx?$/.test(e.name)) out.push(full);
  }
  return out;
}

let bad = 0;
for (const c of playwrightCases) {
  const got = playwrightProblems(c.config, c.commands);
  if (got.length !== c.want) {
    bad++;
    console.error(`check-flaky-report-parse: ${c.what}\n  want ${c.want} problem(s)\n  got  ${JSON.stringify(got)}`);
  }
}
for (const c of overrideCases) {
  const got = overrideProblems('case', c.src);
  if (got.length !== c.want) {
    bad++;
    console.error(`check-flaky-report-parse: ${c.what}\n  want ${c.want} problem(s)\n  got  ${JSON.stringify(got)}`);
  }
}
// Resolve the config as CI does, so a `process.env.CI ? 2 : 0` fails locally too.
process.env.CI ||= 'true';
const E2E = path.join(ROOT, 'test', 'e2e');
const pwConfig = createRequire(path.join(E2E, 'package.json'))('./playwright.config.js');
const specs = specFiles(path.resolve(E2E, pwConfig.testDir ?? '.'));
if (specs.length === 0) {
  bad++;
  console.error('check-flaky-report-parse: no spec files under test/e2e — did testDir move?');
}
for (const f of specs) {
  for (const p of overrideProblems(path.relative(ROOT, f), fs.readFileSync(f, 'utf8'))) {
    bad++;
    console.error(`check-flaky-report-parse: ${p}`);
  }
}
const pwCommands = yml.split('\n').filter(l => /\bplaywright test\b/.test(l));
if (pwCommands.length === 0) {
  bad++;
  console.error('check-flaky-report-parse: no `playwright test` command in ci.yml — did the e2e jobs change shape?');
}
for (const p of playwrightProblems(pwConfig, pwCommands)) {
  bad++;
  console.error(`check-flaky-report-parse: ${p}`);
}
for (const c of needsCases) {
  const got = unwatched(c.yml, {}).missing;
  if (got.join(',') !== c.want.join(',')) {
    bad++;
    console.error(`check-flaky-report-parse: ${c.what}\n  want ${JSON.stringify(c.want)}\n  got  ${JSON.stringify(got)}`);
  }
}
if (!jobs(yml).has('flaky-report')) {
  console.error('check-flaky-report-parse: no flaky-report job in ci.yml');
  process.exit(1);
}
// The rest of the github-script (log fetch, issue lookup and filing) runs only
// on a master red, so at least make it compile and call the two functions
// checked above.
{
  const lines = jobs(yml).get('flaky-report').split('\n');
  const at = lines.findIndex(l => /^ *script: \|\s*$/.test(l));
  const indent = at < 0 ? 0 : lines[at].match(/^ */)[0].length;
  const body = [];
  for (const l of lines.slice(at + 1)) {
    if (l.trim() !== '' && l.match(/^ */)[0].length <= indent) break;
    body.push(l);
  }
  try {
    if (at < 0) throw new Error('no `script: |` block');
    const AsyncFunction = (async () => {}).constructor;
    new AsyncFunction('github', 'context', 'core', body.join('\n'));
    if (!body.some(l => /^\s*collectFailures\(/.test(l))) throw new Error('nothing calls collectFailures');
    if (!body.some(l => /=\s*issueBody\(/.test(l))) throw new Error('nothing calls issueBody');
  } catch (e) {
    bad++;
    console.error(`check-flaky-report-parse: flaky-report's github-script: ${e.message}`);
  }
}
const { testJobs, missing } = unwatched(yml, UNREPORTED);
for (const job of missing) {
  bad++;
  console.error(`check-flaky-report-parse: job \`${job}\` runs tests but flaky-report does not wait on it — add it to flaky-report's needs (or to UNREPORTED with a reason)`);
}
for (const job of Object.keys(UNREPORTED)) {
  if (!testJobs.includes(job)) {
    bad++;
    console.error(`check-flaky-report-parse: UNREPORTED lists \`${job}\`, which is not a test job in ci.yml — drop the stale entry`);
  }
}

for (const c of cases) {
  const found = failures(c.log);
  const got = [...found.keys()];
  const gotSubtests = Object.fromEntries([...found].filter(([, f]) => f.subtests.size > 0).map(([n, f]) => [n, [...f.subtests]]));
  const wantSubtests = c.subtests ?? {};
  if (JSON.stringify(got) !== JSON.stringify(c.want) || JSON.stringify(gotSubtests) !== JSON.stringify(wantSubtests)) {
    bad++;
    console.error(`check-flaky-report-parse: ${c.what}\n  want ${JSON.stringify(c.want)} subtests ${JSON.stringify(wantSubtests)}\n  got  ${JSON.stringify(got)} subtests ${JSON.stringify(gotSubtests)}`);
  }
}
for (const c of bodyCases) {
  const got = issueBody(...c.args);
  if (got !== c.want) {
    bad++;
    console.error(`check-flaky-report-parse: ${c.what}\n  want ${JSON.stringify(c.want)}\n  got  ${JSON.stringify(got)}`);
  }
}
if (bad > 0) {
  console.error(`check-flaky-report-parse: ${bad} check(s) failed — flaky-report would file the wrong issues, or none`);
  process.exit(1);
}
console.log(`check-flaky-report-parse: OK (${cases.length} log cases, ${bodyCases.length} body cases, ${needsCases.length} needs cases, ${playwrightCases.length} playwright cases, ${overrideCases.length} override cases, ${specs.length} specs, ${testJobs.length} test jobs: ${testJobs.join(', ')})`);
