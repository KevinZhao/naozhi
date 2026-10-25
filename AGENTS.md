# AGENTS.md

**Canonical agent instructions for this repository live in [`CLAUDE.md`](./CLAUDE.md). Read that file.**

This file is deliberately a one-line pointer rather than a copy.

## Why not a copy

`AGENTS.md` used to be a full duplicate of `CLAUDE.md`. It silently rotted:
it drifted ~279 lines behind, and a global find/replace left it asserting
things that were never true of this codebase — `~/.Codex/sessions` (the code
reads `~/.claude/projects/<slug>/<sessionId>.jsonl`), a `cli.backend` value of
`Codex` (the real values are `claude` / `kiro` / `codex`), a retired 6-step
shutdown order, and an outdated `Protocol` signature.

The root cause was governance, not wording: `CLAUDE.md` was CI-gated at the
time (a test kept its Module Dependency list in sync with `internal/`), while
nothing gated `AGENTS.md`. Any duplicate of a maintained file will drift, so fixing the prose would only have reset the clock.
