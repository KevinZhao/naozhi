---
name: dev-workflow
description: Engineering workflow for the naozhi repo — every code change goes worktree → PR → green aggregate `test` → merge, and every deployment goes through a tagged release plus `naozhi upgrade`. TRIGGER when working in the naozhi repo and the user asks to implement a feature, refactor, fix a bug, or release a new version. DO NOT TRIGGER for typo fixes, comment-only edits, or pure documentation changes.
---

# naozhi Dev Workflow

## Changes

1. Open a fresh worktree from the `master` tip (`EnterWorktree`, or `git worktree add .claude/worktrees/<topic> -b <branch> origin/master`). One task per worktree/branch; never edit on `master` and never push to it directly.
2. Implement with tests. A bug fix should come with a test that fails on the old code. Never make CI green by deleting a failing test.
3. Before pushing run `go build ./...`, `go vet ./...`, `gofmt -l .` (must be empty) and the full `go test ./...`.
4. Open a PR (conventional-commit title). Merge once the aggregate `test` check is green.

## Releasing & upgrading naozhi

1. Merge the PR to `master`.
2. Tag the commit on `master`: `git tag vX.Y.Z && git push origin vX.Y.Z` — `release.yml` refuses tags that are not on `master`. Semver: fixes bump patch, compatible features bump minor, breaking changes bump major.
3. CI cross-compiles the binaries, runs the screenshot release gate (`e2e-gate`: boots the binary in dashboard-only mode and asserts every view renders with zero console errors) and publishes the signed GitHub Release only if the gate passes.
4. On each host run `naozhi upgrade`; it downloads the matching binary, verifies it, replaces the running executable and restarts the service.

Rules:

- `naozhi upgrade` from a tagged release is the only way to deploy. No `scp` of local builds, no `git pull && go build` on a host. If the pipeline fails, fix the pipeline.
- For UI-touching changes run `make release-gate` locally before tagging (see `docs/ops/release-gate.md`).
- Shim state golden (#2543): before tagging, check whether the release changed the shape of `State` / `SpawnOverlay` in `internal/shim/state.go` (added/renamed/removed fields, json tag changes). If it did, add `internal/shim/testdata/state_v<new tag>.json` and the matching expectations in `internal/shim/state_golden_test.go` (every omitempty field set to a non-zero value). Fixture values must be explicit placeholders (`FIXTURE-NOT-A-REAL-TOKEN` style), never copied from a real state file. If the shape did not change, skip — the golden matrix only grows on shape changes. This matrix is what keeps shim reattach working across versions.

## Repo conventions

- Project-scoped skills live under `.claude/skills/` and are tracked in git (whitelisted in `.gitignore`); worktrees under `.claude/worktrees/` are ignored.
- Large designs go in `docs/rfc/`, indexed in `docs/rfc/README.md`. Outstanding work is tracked in GitHub Issues — do not recreate `docs/TODO.md`.
