---
name: dev-workflow
description: Engineering workflow gate for the naozhi repo. Enforces a hard "isolated worktree → independent review → PR" loop on every code-touching task: every feature or bug fix starts on a fresh git worktree (never main / never the current working branch), runs a code review before the PR opens, and ships through a PR — no direct push to the main branch. Also enforces design-doc requirements for features, classifies bug fixes by size and risk, blocks PRs that lack tests/regression evidence/rollback plans, and routes deployment through the `naozhi upgrade` release pipeline. TRIGGER when working in the naozhi repo and the user asks to implement a feature, refactor, fix a bug, or release a new version; or when about to write code that touches >20 lines, >3 files, or any item on the risk checklist. DO NOT TRIGGER for typo fixes, comment-only edits, or pure documentation changes.
---

# naozhi Dev Workflow

Project-scoped gate for the naozhi repo. Decides whether a change can go straight to a PR or must first produce a design + independent review, and how a tagged release reaches each host. Apply at the start of any code-touching task, before writing the first line.

## Mandatory loop (every code-touching task)

Every feature or bug fix — regardless of bucket (A, B, C, or E) — MUST run through this loop end-to-end. Trivial Bucket D changes (typos, comments, pure docs) are the only exception.

1. **New worktree.** Open an isolated git worktree before writing any code. Do not edit on `master`/`main` and do not reuse the current working branch. Use the `EnterWorktree` tool, or `git worktree add .claude/worktrees/<topic> -b <branch>` from the main branch tip. Each task gets its own worktree; do not bundle two unrelated tasks into one branch.
2. **Implement + tests.** Per Step 2 below.
3. **Code review before PR.** Run a review pass on the diff (not just the design). Prefer the `ecc:code-review` skill, or spawn parallel reviewer agents per the "Recommended agent team layouts" table below. Self-review is allowed only with written angle-by-angle notes. Address every blocking comment in the worktree before opening the PR; record agent verdicts and reconciled disagreements in the PR description.
4. **PR.** Open via `ecc:prp-pr` or `ecc:git-workflow`. Direct push to the main branch is forbidden — even for one-line fixes. Merge happens only after CI is green and the PR readiness gate (§Step 3) passes.

Never skip a step "because it's small". The loop is the rule; bucket classification only changes how heavy step 2 (design + tests) is, not whether the loop runs.

## Step 0 — Classify the task

Pick exactly one bucket. If two apply, pick the stricter one.

| Bucket | Definition |
|---|---|
| **A. Feature / refactor** | New behavior, new module, removing/changing existing behavior, or any work the user describes as a "feature" |
| **B. Bug fix — small** | Fix that is ALL of: ≤20 lines changed, ≤3 files touched, AND triggers no item on the Risk Checklist |
| **C. Bug fix — large** | Bug fix that violates ANY of: >20 lines, >3 files, or any Risk Checklist item |
| **D. Trivial** | Typo, comment, formatting, doc-only — skip this skill |
| **E. Hotfix** | Live incident or user-authorized emergency fix. Bypass design path to stop the bleeding. Required afterwards within 24h: write the design retroactively, add a regression test, and open a GitHub issue with `priority:p0` + appropriate `type:` label for the postmortem trail. Commit message must use `hotfix:` prefix. |

### Risk Checklist (any one match → escalate to "needs design")

A change is risky regardless of size if it:

- [ ] Touches a naozhi core path: CLI wrapper, session router, persistence, scheduler, dispatcher, channel adapter (Feishu/etc.), dashboard hub
- [ ] Involves concurrency, locks, channel ordering, or fsync sequencing
- [ ] Modifies a public API, IM message protocol, on-disk format, or migration boundary
- [ ] Changes observable behavior (output, response shape, error code) even if 1 line
- [ ] Security surface: auth, input validation, secrets, command injection, path traversal, webhook signature verification
- [ ] Performance hot path (per-request alloc, I/O in tight loops, per-event handlers)
- [ ] Affects multiple subsystems or crosses a module boundary

**Bucket A and C MUST follow the design path. Bucket B skips the design-doc path and may proceed directly to Step 2, but the Mandatory loop (worktree → implement+tests → code review → PR) still applies in full and a regression test is still required.**

## Step 1 — Design path (Bucket A and C)

### 1a. Feasibility spike (only if any apply)

- New external dependency or new protocol
- Performance ceiling unknown
- Cross-subsystem change
- Change estimated >300 lines or >10 files

A spike is a throwaway branch that proves the unknown. Time-box it. Record findings in the design doc, do not merge spike code.

### 1b. Design document

For Bucket A: write to `docs/rfc/<topic>.md` and add the entry to `docs/rfc/README.md`.
For Bucket C: design may live inline in the PR description instead of a standalone file, but must cover every required section in full — "shorter" means location, not content.

Use `ecc:plan` or `ecc:prp-plan` to draft the design.

**Required sections (no skipping):**

1. **Background & problem** — reproducible symptom, data, link to incident/ticket
2. **Goals & non-goals** — explicit non-goals prevent scope creep
3. **Alternatives considered** — at least 2 candidates + why the chosen one wins
4. **Test strategy** — unit / integration / manual / regression points; name the specific tests to add
5. **Risk & rollback** — what breaks if this is wrong, how to revert
6. **Observability** — logs, metrics, dashboards added or changed
7. **Compatibility & migration** — backward compat, on-disk format, config flags
8. **Rollout plan** — flag-gated, staged, full-cutover

If a section truly does not apply, write "N/A — <one-line reason>". Do not silently omit.

### 1c. Independent review

Run a dedicated review pass before coding starts. The reviewer can be:

- **Preferred — an agent team / specialized subagent.** Spawn parallel reviewers per angle (e.g. `ecc:code-reviewer`, `ecc:security-reviewer`, `ecc:performance-optimizer`, `ecc:database-reviewer`, `ecc:go-reviewer`). Multi-angle parallel review surfaces blind spots a single pass misses and is the default for any non-trivial design.
- A different session or a human collaborator.
- The author themselves, IF they perform a deliberate angle-by-angle pass with fresh eyes (e.g. after a break or in a separate session) and write the review notes down explicitly. Self-review is acceptable but weaker than agent-team review — escalate to agents whenever the change is risky or cross-subsystem.

**Reviewer(s) must check from these angles (one per heading in the review notes):**

- Architecture, naming, abstraction level
- Test sufficiency: coverage, boundary cases, concurrency, failure paths
- Performance: hot-path allocation, I/O, locks
- Security: OWASP, input boundaries, log leakage, secrets
- Observability: are failures debuggable from logs/metrics alone?
- Backward compatibility & migration safety
- Failure modes & graceful degradation
- Documentation sync — produce `docs/review/R{N}-raw.md` and run the `triage-findings` skill, which routes findings to GitHub Issues / `docs/cosmetic-backlog.md` / discard. (`docs/TODO.md` was deleted 2026-05-26 after the migration; do NOT recreate it.)

Save review evidence to `docs/reviews/<topic>-<date>.md` if the repo uses that pattern; otherwise paste into the PR description. When using an agent team, capture each agent's verdict and note whether disagreements were resolved.

For convergence-style review (two independent reviewers must agree), use `ecc:santa-loop`. For a single-pass review, use `ecc:code-review`.

The design doc must be revised until the review is signed off (by the agent team's consensus, the human reviewer, or — for self-review — by the author's own written angle-by-angle notes). Do not start coding until then.

## Step 2 — Implementation

- Per the Mandatory loop step 1, work in a fresh worktree branched from the main branch tip. One worktree per task; two unrelated tasks → two worktrees → two PRs.
- Keep refactor and bug fix in **separate PRs**. Do not bundle "while I was here" cleanup into a fix PR.
- Add tests **before or alongside** the code change. For Go projects, use `ecc:tdd-workflow` and `ecc:golang-testing` to drive the test-first loop.
- **Regression test acceptance for Bucket B/C:** at least one test must fail on the old code and pass on the new. A test that only asserts the post-fix behavior without reproducing the original bug does not qualify.
- **Scope re-classification.** If during implementation the change breaks Bucket B's thresholds (>20 lines, >3 files) or hits any item on the Risk Checklist, STOP and re-classify to Bucket C. Keep already-written code as a spike and produce the design doc before continuing.
- Do not break existing functionality:
  - Run the full test suite before pushing, not just the new tests.
  - For UI/runtime changes, exercise the feature end-to-end, not just type-check.

## Step 3 — PR readiness gate

Use `ecc:prp-pr` or `ecc:git-workflow` to drive the PR creation flow. **PRs must originate from a dedicated worktree branch, never directly from `master`/`main`. Direct push to the main branch is forbidden — every change ships through a PR, even one-line fixes.**

A PR may be opened only when ALL of these hold:

- [ ] Worktree branched from the main branch tip; no edits made on `master`/`main` or on an unrelated topic branch
- [ ] Build passes: `go build ./...`
- [ ] Full test suite passes: `go test ./...`
- [ ] `go vet ./...` clean — no new warnings introduced
- [ ] Coverage on changed files did not drop
- [ ] **Code review on the diff completed before push.** Use `ecc:code-review` (or parallel reviewer agents per the table below). PR description must record reviewer verdicts and how blocking comments were addressed.
- [ ] For Bucket A/C: design doc linked in PR description, design-stage reviewer sign-off recorded (agent team verdicts attached if used)
- [ ] For Bucket B: regression test present, root cause stated in commit message
- [ ] PR description contains: Summary, Why, Test plan, Risk, Rollback
- [ ] No disabled tests without a tracking issue. `--no-verify` and skipped hooks are allowed only when the user has explicitly authorized them or hook failure is unrelated and tracked separately.

## Recommended agent team layouts

Use these as starting points; spawn the relevant subagents in parallel via the `Agent` tool.

| Change shape | Suggested reviewers (parallel) |
|---|---|
| Default Go change in naozhi | `ecc:go-reviewer` + `ecc:code-reviewer` |
| Touches CLI wrapper / session router / dispatcher / hot path | add `ecc:performance-optimizer` |
| Touches auth, secrets, webhook signature, or input validation | add `ecc:security-reviewer` |
| Dashboard / frontend change (`internal/server/static/`) | add `ecc:typescript-reviewer` |
| Concurrency, locks, channel ordering, fsync | add `ecc:performance-optimizer` + `ecc:go-reviewer` (mandatory) |

After all parallel reviews return, reconcile disagreements explicitly in the design doc — do not silently pick one verdict. If reviewers split, the author decides and records the rationale.

## Anti-patterns (block on sight)

- Editing on `master`/`main` directly, or reusing an unrelated topic branch instead of opening a fresh worktree
- Pushing a fix straight to the main branch without a PR ("just one line, won't bother with a PR")
- Opening a PR without running a code review on the diff first
- PR with code but no background or motivation
- Change >20 lines with zero tests added
- "Drive-by" refactor mixed into a bug fix
- Self-review with no written angle-by-angle notes — claiming review without doing one
- Bug fix commit message that says "fix bug" without stating the root cause
- Deleting failing tests instead of fixing them
- Adding a feature flag without a removal plan

## Required commit message shape (bug fixes)

```
<type>(<scope>): <one-line fix summary>

Root cause: <why it broke>
Fix: <what changed and why this addresses the root cause>
Regression test: <test name or path>
```

## Decision flowchart

```
task arrives
  │
  ├─ doc / typo only?  ──► Bucket D — skip skill
  │
  └─ everything else
       │
       ├─ open fresh worktree from main branch tip   ◄── mandatory, every bucket
       │
       ├─ live incident or user-authorized emergency?
       │    └─► Bucket E — hotfix; backfill design + regression + postmortem within 24h
       │
       ├─ bug fix?
       │    ├─ ≤20 lines AND ≤3 files AND no risk-checklist hit?
       │    │     └─► Bucket B — code + regression test
       │    │           └─ scope grows mid-flight? → re-classify to C
       │    └─► Bucket C — design path (design + design-stage review)
       │
       ├─ feature / refactor / behavior change?
       │    └─► Bucket A — design path
       │           ├─ unknowns? → spike first
       │           ├─ write design doc (8 required sections)
       │           ├─ independent review across 8 angles (prefer agent team in parallel)
       │           └─ implement with tests
       │
       ├─ code review on the diff   ◄── mandatory before PR
       └─ PR readiness gate → open PR (never push directly to main)
```

## Repo conventions

- Project-scoped skills live under `.claude/skills/` and are tracked in git (whitelisted in `.gitignore`). Edits to a skill follow the same worktree → review → PR loop as code.
- Worktrees live under `.claude/worktrees/<name>/` and are ignored. Use `EnterWorktree` (or `git worktree add`) — never branch by editing the existing checkout in place.
- Large designs go in `docs/rfc/`, indexed in `docs/rfc/README.md`
- Outstanding work tracked in **GitHub Issues** (`label:priority:p0,priority:p1,priority:p2 is:open`); `docs/TODO.md` was deleted 2026-05-26 after the migration — do NOT recreate it. `docs/TODO-changelog.md` / `docs/TODO_ARCHIVE.md` are historical reference only.
- Cosmetic / godoc / naming suggestions go to `docs/cosmetic-backlog.md`, not issues
- Review findings flow: review agent → `docs/review/R{N}-raw.md` → `triage-findings` skill → issues / cosmetic-backlog / discarded
- Local restart after a build during development: `sudo systemctl restart naozhi` (see `docs/ops/naozhi-deploy-skill.md`); never hand-kill the process
- Prior review sweeps are referenced from the project memory `MEMORY.md`

## Releasing & upgrading naozhi

Distributing a new version to any machine running naozhi MUST go through the release pipeline. `sudo systemctl restart naozhi` is for local development only — never use it to ship a build to another host.

Release flow:

1. Merge the PR to the main branch.
2. Tag the commit: `git tag vX.Y.Z && git push origin vX.Y.Z`. The tag must live on the main branch — `release.yml` verifies this.
3. CI cross-compiles the 6-platform binaries and publishes them to a GitHub Release with checksums.
4. On each target host run `naozhi upgrade`. It checks the latest GitHub Release, downloads the matching binary, replaces the running one, and restarts the service.

Rules:

- Never deploy by copying a locally built `naozhi` binary to a host (no `scp`, no `git pull && go build` on a target host). The only sanctioned path is `naozhi upgrade` pulling from a tagged release.
- Tag versions follow semver: bug fixes bump patch, backwards-compatible features bump minor, breaking changes bump major.
- If the release pipeline fails, fix the pipeline — do not work around it with a manual binary copy.
- A PR that requires deployment to verify must call out the version it targets (`vX.Y.Z`) in the PR description so the tag step is not forgotten.

Reference: `docs/ops/deployment-strategy.md` (release pipeline + `naozhi upgrade` design).
