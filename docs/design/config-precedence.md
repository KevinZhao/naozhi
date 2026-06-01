# Config precedence

> ARCH5 / #385. naozhi resolves runtime configuration from three sources.
> Until now the precedence rules lived only in inline comments; this document
> is the single source of truth.

## The three sources

| # | Source | Loaded by | Scope |
|---|--------|-----------|-------|
| 1 | `config.yaml` | `config.Load()` (`internal/config/config.go`) | naozhi's own settings — server addr, channels, session policy, node pool |
| 2 | `~/.claude/settings.json` (`env` block) | `applyClaudeEnvSettings()` (`cmd/naozhi/main_claude_settings.go`) | Claude/Bedrock env injected into the naozhi process before any CLI spawn |
| 3 | Process environment (`os.Getenv`) | individual call sites | OS-level vars (`HOME`, `PATH`, `XDG_RUNTIME_DIR`, …) and a small set of documented overrides |

These sources are **not** a single override stack — each owns a distinct slice
of the configuration surface. The precedence rules below apply *within* the
narrow places where they overlap.

## Per-field precedence

| Field / concern | Source of truth | Override order | Notes |
|-----------------|-----------------|----------------|-------|
| Server addr, log, session policy, channels | `config.yaml` | yaml only | `applyDefaults()` fills gaps; see `config.go`. |
| Remote node pool (`nodes` / `workspaces`) | `config.yaml` | `workspaces` wins over `nodes` | `Config.Normalize()` reconciles the alias; conflict logs via `slog.Warn`. R71-ARCH-L1. |
| `${VAR}` placeholders inside `config.yaml` | process env | env expands into yaml at load | Secret-prefixed vars (`ANTHROPIC_`, `AWS_`, `GITHUB_TOKEN`, …) are **refused** so credentials never enter the in-memory Config. R240-SEC-16 / #1047, see `envExpansionDenyPrefixes`. |
| Claude / Bedrock env (`ANTHROPIC_MODEL`, `AWS_REGION`, `CLAUDE_CODE_USE_BEDROCK`, …) | `~/.claude/settings.json` `env` block | **shell env wins** — `applyClaudeEnvSettings` only sets a var when it is not already present in the process env | An operator who exports the var in the shell keeps control; the settings file is the default, not an override. See `main_settings_test.go` (`from-shell` case). |
| `CLAUDE_PROJECTS_DIR` (transcripts root) | process env, else `~/.claude/projects` | env wins | Single resolver: `resolveClaudeProjectsDir()` in `internal/server/claude_paths.go`. R222-ARCH-9 / #724. |
| `~/.claude` location | `os.UserHomeDir()` | OS only | Single resolver: `resolveClaudeDir()`. |
| `NOTIFY_SOCKET`, `XDG_RUNTIME_DIR`, `PATH`, `HOME`, `APPDATA`, `SUDO_USER`, `USER` | process env | OS only | Genuinely OS-level; intentionally read via `os.Getenv` and not modelled in `config.yaml`. |

## Rules of thumb for new config

1. naozhi-owned knobs go in `config.yaml`, with a default in `applyDefaults()`
   and validation in `validateConfig()`.
2. Claude/Bedrock env belongs in the `~/.claude/settings.json` `env` block;
   never re-read it with a fresh `os.Getenv` after `applyClaudeEnvSettings`
   has run — read the already-injected process env, not the file.
3. A direct `os.Getenv` is acceptable **only** for genuinely OS-level vars
   (table row 7) or a documented override that has a single resolver function
   (rows 5–6). Do not scatter ad-hoc `os.Getenv` reads of naozhi config across
   call sites — that is the bypass ARCH5 / #385 tracks. The
   `internal/server` package enforces this with a regression test
   (`TestNoStrayConfigGetenv`).

## Why no global override stack?

A flat "env beats file beats default" stack was rejected because sources 1 and
2 own disjoint surfaces (naozhi settings vs. Claude env) and conflating them
would let a stray `ANTHROPIC_*` shell var silently reshape naozhi behaviour.
Keeping the surfaces explicit (and gating secret expansion) is the safer DX.
