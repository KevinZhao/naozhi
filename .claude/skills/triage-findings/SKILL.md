---
name: triage-findings
description: Triage raw review-agent findings before they become noise. Takes a `docs/review/R{ROUND}-raw.md` file (one finding per `- ` bullet, with anchor like `R249-GO-3`) and routes each to one of three buckets - GitHub Issue (real problems), `docs/cosmetic-backlog.md` (godoc/naming/comment-only), or discarded (false positive / already fixed / duplicate). Verifies each finding against the current code (grep file paths, line numbers, function names) before opening anything. TRIGGER when a review skill produces `docs/review/R*-raw.md`, when the user says "triage these findings" / "process this review output", or when about to bulk-import historical findings into issues. DO NOT TRIGGER for one-off bug reports the user types directly - those go straight to a normal bug-fix workflow.
---

# triage-findings

Filters raw review-agent output so only verified, non-duplicate, non-cosmetic problems reach the GitHub issue tracker. Reduces issue-tracker noise; preserves audit trail for everything that gets dropped.

## Why this skill exists

`docs/TODO.md` was the previous dump target for review findings. It grew to 2649 lines / 1054 open / 65 review rounds because every finding was appended without verification, dedup, or categorization. PRs touching TODO.md conflicted constantly. As of 2026-05-25 TODO.md is FROZEN; this skill is the gate for everything new.

## Inputs

A single file under `docs/review/R{ROUND}-raw.md` produced by a review agent / skill. Each finding is a bullet:

```markdown
- **R249-GO-3 — short title (P2)** [REFACTOR]: symptom + location (`internal/server/wshub.go:485`) + proposal.
```

The anchor `R{ROUND}-{CAT}-{IDX}` is REQUIRED. If a finding has no anchor, ask the user to assign one before proceeding (this is the cross-round dedup key).

## Three-bucket routing

For each finding, decide exactly one bucket:

### Bucket A — Open a GitHub Issue
Open if ALL of:
1. **Verified to still exist** in the current code (grep file path / line / function name; if file moved, find new location; if symbol removed, this is bucket C)
2. **Not a duplicate** of an existing issue (`gh issue list --search "<anchor or symptom>"`) AND not already covered by an existing TODO.md anchor that's marked `[~]` or has a "降级" / "已实施" annotation
3. **Has a real functional impact** — could plausibly change runtime behavior, perf, or security posture. This includes P3 correctness items (e.g. rare-race / edge-case-only). **Priority does NOT gate issue creation**; even P3 correctness gets an issue. Tag with `priority:p{0..3}` so we can filter later.
4. NOT purely a godoc/naming/comment suggestion (those are bucket B regardless of priority). If a finding mixes a godoc suggestion AND a functional claim (e.g. "rename and fix the lock"), the functional claim wins — bucket A. The "purely" qualifier is the gate.

### Bucket B — Cosmetic backlog
Append to `docs/cosmetic-backlog.md` if BOTH:
- The change has **no observable runtime impact** — no behavior, perf, security, or output difference
- It's purely about how the source reads: godoc/comment wording, variable/function rename for readability, file split/move that preserves behavior

A "P3 REFACTOR" is bucket B only if it really has zero functional impact. If it removes a subtle correctness foot-gun (even rarely triggered), it goes to bucket A.

Cosmetic backlog is read-only most of the time; once or twice a quarter someone can sweep it.

### Bucket C — Discard (audit trail only)
Drop if any of:
- **Already fixed**: grep shows the proposed change is already in the code (review was stale)
- **False positive**: invariant the finding claims to break is actually maintained by a lock / contract the reviewer missed
- **Out of scope**: depends on naozhi being deployed differently than it actually is (multi-tenant / different OS / etc.)
- **Superseded**: another finding in this same R{ROUND}-raw.md or an existing issue covers the same root cause

Drops are NOT silent. Append to the bottom of the same `docs/review/R{ROUND}-raw.md` under a `## Discarded` section with a one-line reason per anchor. Future reviewers grep for these reasons.

## Workflow per finding

```
1. Parse anchor + claim from the bullet.
2. Verify:
   - If file path is referenced: Read or Grep that file. Confirm symbol/line still relevant.
   - If lock / invariant claim: Grep adjacent code, confirm the claim is correct.
3. Dedup:
   - gh issue list --search "<anchor>" --state all
   - gh issue list --search "<key symptom phrase>" --state all
   - If duplicate: comment on existing issue with this round's anchor as a "re-discovered in R{N}" note, mark this finding bucket C with reason "dup of #N".
4. Decide bucket A / B / C using rules above.
5. Action:
   - Bucket A: `gh issue create` with the body filled per the `finding.yml` template structure:
     - **Anchor** field ← parsed from the raw bullet (e.g. `R249-GO-3`)
     - **Symptom** ← the bullet's symptom/title text
     - **Location** ← file:line + code snippet from the bullet (or grepped if bullet lacks it)
     - **Proposal** ← the bullet's "方案" / "fix" text if present, else blank
     - **Triage notes** ← skill-generated: which file was grepped to verify, dedup search results, severity rationale
     - Attach labels: `priority:p{0..3}`, `area:{cron|wshub|dashboard|cli|dispatch|server|session|adapter|shim|persistence}`, `type:{correctness|perf|sec|refactor|ux|feature}`, `source:R{ROUND}` (create the `source:R{ROUND}` label on first use of each round). Do NOT use `needs-triage` — that label is for manually-filed issues that bypassed this skill.
   - Bucket B: append to docs/cosmetic-backlog.md as a single line: `- [R{ROUND}-{CAT}-{IDX}] <one-line> — file:line`
   - Bucket C: append to ## Discarded section in R{ROUND}-raw.md with reason.
6. Annotate the original bullet in R{ROUND}-raw.md with the outcome: `→ #123` (issue) / `→ cosmetic` / `→ discarded:<reason>`.
```

## Mode selection

- **Default mode (interactive)**: process findings one at a time with brief reasoning visible to the user. Good for the first time the skill runs in a session, when the user wants to spot-check the triage.
- **Batch mode**: when processing > 30 findings (e.g. historical R215-R248 backfill), summarize per-finding decisions in a markdown table, only stop for ambiguous cases. Use batch mode when the user says "process all of R249" or invokes after a multi-round bulk import.

## Quality bar

- **Never open an issue without grepping the code first.** A stale finding from 3 months ago might already be fixed; opening it wastes the user's triage time downstream.
- **Always include the anchor in the issue title** as the prefix `[R249-GO-3]`. This is how cross-round dedup works for future rounds.
- **Severity calibration** (only used to set the issue's `priority:p{N}` label, NOT to gate whether to open):
  - P0: data loss, security exposure, deadlock, panic on prod path
  - P1: race / wrong behavior under realistic concurrency / hot-path leak
  - P2: edge-case correctness / perf in non-hot-path / DX nuisance with functional impact
  - P3: rare correctness foot-gun, low-impact perf, niche edge case — still bucket A if functional, bucket B if pure cosmetic
- **When uncertain whether something is real**, lean toward bucket A with `needs-design` label rather than bucket C — losing a real issue is worse than carrying an extra triage note.
- **When uncertain whether something has functional impact** (A vs B borderline), default to A. False positives in A get closed in 30 seconds; misclassified to B can sit in cosmetic-backlog forever.

## Output to user

Per run, print a one-line summary table:

```
R249 triage: 76 findings → 50 issues opened, 11 cosmetic, 15 discarded
  Issues: #401-#450 (priority breakdown: 2 P0 / 8 P1 / 27 P2 / 13 P3)
  Discarded reasons: 9 already-fixed, 4 dup, 2 out-of-scope
```

Priority distribution is informational only — every functional finding becomes an issue regardless of priority.

Do not narrate every finding to the user; the annotations in R{ROUND}-raw.md are the audit trail.

## Edge cases

- **Anchor collision across rounds** (e.g. R246-CR-11 and R247-CR-11 about different things): namespace by full `R{ROUND}-{CAT}-{IDX}`; the round prefix prevents collision.
- **Finding with no anchor**: assign one using the next sequence in the round, e.g. if R249-GO has 1..7, assign R249-GO-8. Note in the body that this was a triage-assigned anchor.
- **Multi-finding from same root cause** (very common in 5-agent parallel review): cluster by file/function, open ONE issue with all anchors in the body, the rest go bucket C with reason "rolled into #N".
- **Historical backfill (TODO.md → issues)**: when running on `docs/TODO.md` (not a R{ROUND}-raw.md file), treat each `- [ ]` bullet as a finding, use the existing anchor in the bullet. **Verify each via grep before deciding**; many items are 2-6 weeks old and silently fixed since. Staleness is NOT the same as low-priority — a P0 finding from 4 weeks ago that's still in the code is still bucket A. The bias for stale items should only express itself in: (a) double-checking already-fixed status with extra care, (b) treating ambiguous "降级 (deferred)" notes as bucket C unless the underlying claim still grep-verifies.

## Linked skills

- Produced by: `dev-workflow` skill's review step (the `ecc:code-review` invocation).
- Output consumed by: GitHub issue tracker + `docs/cosmetic-backlog.md`.
- Never write to: `docs/TODO.md` (FROZEN as of 2026-05-25).
