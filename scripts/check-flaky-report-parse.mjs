#!/usr/bin/env node
// check-flaky-report-parse.mjs — the flaky-report job in .github/workflows/ci.yml
// turns failed-job logs into `[flaky] <test>` issue titles with three regexes.
// Nothing else exercises them: they run only on a master red, and a regex that
// stops matching fails open — the job succeeds and files nothing, which reads
// exactly like a green week.
//
// This pulls the regex literals out of the workflow (so what is checked is what
// ships) and runs them over log lines copied verbatim from real runs. It also
// checks that flaky-report waits on every job that runs tests: a job missing
// from its needs files nothing when it alone is red, and otherwise is reported
// only if it happened to finish first.
//
//   node scripts/check-flaky-report-parse.mjs
import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const WORKFLOW = path.join(ROOT, '.github', 'workflows', 'ci.yml');
const yml = fs.readFileSync(WORKFLOW, 'utf8');

// Anchored on the call sites rather than the pattern bodies: a rewritten regex
// is still found, a deleted one fails here instead of silently at 3am.
function literal(re, what) {
  const m = yml.match(re);
  if (!m) {
    console.error(`check-flaky-report-parse: ${what} not found in ci.yml — did the flaky-report script change shape?`);
    process.exit(1);
  }
  return new Function('return ' + m[1])();
}
const ansi = literal(/log\.replace\((\/.+?\/g)/, 'the ANSI strip');
const goRe = literal(/clean\.matchAll\((\/--- FAIL.+?\/g)\)/, "go test's FAIL matcher");
const pwRe = literal(/clean\.matchAll\((\/✘.+?\/g)\)/, "Playwright's ✘ matcher");
// Also read from the workflow, not re-typed here: a copy would agree with a
// broken original and this check would pass on it.
const durTrim = literal(/m\[2\]\.replace\((\/.+?\/)/, "Playwright's trailing-duration trim");

// names() drives the workflow's own two loops over one job log, deduping by
// name exactly as the reporter's Map does — a name that appears twice must file
// one issue, not two.
function names(log) {
  const clean = log.replace(ansi, '');
  const out = new Set();
  for (const m of clean.matchAll(goRe)) out.add(m[1]);
  for (const m of clean.matchAll(pwRe)) {
    out.add(m[1] + ' › ' + m[2].replace(durTrim, '').trim());
  }
  return [...out];
}

const ESC = '\u001b';
const cases = [
  {
    what: 'go test failure (job `test`, run 33974275550)',
    log: '2026-09-09T04:11:02.1234567Z --- FAIL: TestReverseServer_Reconnect_closesOldConn (2.01s)\n',
    want: ['TestReverseServer_Reconnect_closesOldConn'],
  },
  {
    what: 'go test subtest failure',
    log: '--- FAIL: TestWaitServiceActive/slow_cold_start (0.30s)\n',
    want: ['TestWaitServiceActive/slow_cold_start'],
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

let bad = 0;
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
  const got = names(c.log);
  const same = got.length === c.want.length && got.every((g, i) => g === c.want[i]);
  if (!same) {
    bad++;
    console.error(`check-flaky-report-parse: ${c.what}\n  want ${JSON.stringify(c.want)}\n  got  ${JSON.stringify(got)}`);
  }
}
if (bad > 0) {
  console.error(`check-flaky-report-parse: ${bad} check(s) failed — flaky-report would file the wrong issues, or none`);
  process.exit(1);
}
console.log(`check-flaky-report-parse: OK (${cases.length} log cases, ${needsCases.length} needs cases, ${testJobs.length} test jobs: ${testJobs.join(', ')})`);
