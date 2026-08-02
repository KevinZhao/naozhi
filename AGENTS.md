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

The root cause was governance, not wording: `CLAUDE.md` is CI-gated
(`cmd/naozhi/claude_md_modules_test.go` keeps its Module Dependency list in
sync with `internal/`), while nothing gated `AGENTS.md`. Any duplicate of a
CI-gated file will drift, so fixing the prose would only have reset the clock.

## Why not a symlink

A symlink (`AGENTS.md -> CLAUDE.md`) was tried first. It is correct on
Linux/macOS, but Git on Windows without `core.symlinks=true` or Developer Mode
materialises it as a ~10-byte text file whose entire content is the string
`CLAUDE.md`. The repo IS checked out on Windows — `.github/workflows/ci.yml`
runs a `build-windows` job on a `windows-latest` runner — so an agent reading
`AGENTS.md` there would get nothing usable. `go build` was unaffected (this
file is not compiled), which is exactly why the breakage would have gone
unnoticed.

A real file with a pointer behaves identically on every platform.
