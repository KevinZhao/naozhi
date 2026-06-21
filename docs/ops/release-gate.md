# Release Gate — post-deploy screenshot verification

The release gate boots a naozhi binary, drives every top-level dashboard view
with Playwright, and asserts each view actually renders before a release is
allowed to ship. It is the automated answer to "did this build break the
dashboard?" and it blocks the GitHub Release when it fails.

## What it checks

`scripts/release-gate.mjs` visits 9 views and, per view, asserts:

1. **Navigation succeeded** — `act()` ran without throwing.
2. **A load-bearing selector is visible** — proves the view rendered, not just
   that the shell exists. For the `<main>` container views (cron / system /
   settings) it additionally requires `≥1` mounted child, since those shells
   flip to `display:flex` the instant their nav class lands and would otherwise
   pass as empty shells.
3. **Zero console errors** — `console.error` + `pageerror` + surfaced
   `unhandledrejection` (an init script re-throws rejections so Playwright sees
   them). A short, documented allow-list covers known non-regressions (PWA
   manifest 404 in headless, the deprecated `require-sri-for` CSP directive, and
   the login page's one CSP-blocked inline style — the last scoped to the login
   view only).

Plus a `/health` precondition: the gate polls `/health` until `status:ok`
before driving any view.

Views: `login`, `chat-desktop`, `chat-mobile`, `assets`, `cron`, `system`,
`settings`, `history` (asserts the popover, not the trigger button),
`new-session` (asserts the produced modal/palette, not the trigger button).

Screenshots are written to `tmp/release-gate/` for archival and visual
inspection regardless of pass/fail.

## How it runs

### CI (authoritative gate) — `.github/workflows/release.yml`

On a `v*` tag push, the `e2e-gate` job downloads the freshly built
`naozhi-linux-amd64` artifact, installs Playwright + chromium, and runs
`scripts/gate-instance.sh`. The `release` job declares
`needs: [build, e2e-gate]`, so **a gate failure prevents the GitHub Release
from being published**. `naozhi upgrade` only ever sees tagged Release assets,
so a broken dashboard cannot reach an upgrading operator even though the tag
already exists. Screenshots upload as the `release-gate-screenshots` artifact
(`if: always()`).

The CI runner cannot reach an operator's systemd instance, so it validates the
binary's own embedded dashboard — which is byte-identical to what ships.

### Local — `make release-gate`

Builds the binary and runs the same throwaway-instance gate with no systemd
deployment required:

```
(cd test/e2e && npm install)   # one-time: install Playwright
make release-gate
```

### Local against a live instance — `make release-gate-live`

Gates the already-running systemd instance on `127.0.0.1:8180` (e.g. after
`make deploy`), reading the dashboard token from the systemd EnvironmentFile.
This is the only variant that exercises the real deployment (systemd unit,
credentials, real CLI), so it remains a useful pre-tag self-check even though
it is not enforced.

## Throwaway instance details (`scripts/gate-instance.sh`)

naozhi runs in **dashboard-only mode**: no Feishu credentials, and `cli.path`
points at a stub that only answers `--version` (a real `--version`-failing
binary like `/bin/false` aborts boot). `HOME` is redirected into a temp dir so
the operator's `~/.claude` / `~/.naozhi` state is never read or written, and the
generated config is `chmod 0600` (naozhi refuses group/world-readable configs).

## Exit codes

| code | meaning |
|------|---------|
| 0 | all views passed |
| 1 | `NAOZHI_DASHBOARD_TOKEN` not set |
| 2 | `/health` never ok or login failed (precondition) |
| 3 | Playwright unresolvable / chromium launch failure |
| 4 | one or more views failed an assertion |

## Tuning the gate

- Add a view: append to the `VIEWS` array in `scripts/release-gate.mjs` with a
  `selector` that proves real render (prefer produced artifacts over trigger
  controls; add `minChildren` for container shells).
- Tolerate new console noise: extend `IGNORED_CONSOLE` (every view) or
  `IGNORED_CONSOLE_PER_VIEW` (one view) — keep entries narrow and commented.
- `NAOZHI_GATE_ALLOW_CONSOLE_ERRORS=1` downgrades console-error assertions to
  warnings (still captured to `tmp/release-gate/console.log`) for debugging.
