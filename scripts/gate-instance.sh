#!/usr/bin/env bash
# gate-instance.sh — boot a throwaway naozhi instance for the screenshot gate,
# run scripts/release-gate.mjs against it, and tear everything down.
#
# Used by CI (release.yml e2e-gate job) where there is no systemd deployment to
# target. naozhi runs in "dashboard-only mode" with no Feishu credentials and a
# stub CLI binary — enough to serve and render the dashboard, which is all the
# screenshot gate exercises.
#
# It does NOT touch the real deployment. To gate an already-running instance
# (e.g. local `make release-gate` after `make deploy`), call release-gate.mjs
# directly with NAOZHI_BASE_URL pointing at it instead of using this script.
#
# Env:
#   NAOZHI_GATE_BIN   path to a built naozhi binary (default: bin/naozhi)
#   NAOZHI_GATE_PORT  port for the throwaway instance (default: 18180)
#
# Exit code propagates from release-gate.mjs (0 pass, non-zero fail).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

BIN="${NAOZHI_GATE_BIN:-bin/naozhi}"
PORT="${NAOZHI_GATE_PORT:-18180}"
TOKEN="release-gate-token-$$"
WORK="$(mktemp -d)"
PID=""

cleanup() {
  [ -n "$PID" ] && kill "$PID" 2>/dev/null || true
  [ -n "$PID" ] && wait "$PID" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

if [ ! -x "$BIN" ]; then
  echo "[fatal] naozhi binary not found/executable at $BIN (set NAOZHI_GATE_BIN)" >&2
  exit 3
fi

# A stub CLI that only needs to answer --version so the backend probe passes;
# the gate never sends a real prompt, so it never has to behave like claude.
# /bin/false fatally fails the probe, so we cannot reuse it (see probe notes).
cat > "$WORK/stub-cli" <<'STUB'
#!/usr/bin/env bash
case "$1" in
  --version) echo "0.0.0 (release-gate stub)"; exit 0 ;;
  *) exec sleep 86400 ;;
esac
STUB
chmod +x "$WORK/stub-cli"

# Config must be 0600 or naozhi refuses to load it (group/world-accessible
# check). HOME is redirected into the temp dir so we never read or write the
# operator's ~/.claude or ~/.naozhi state.
cat > "$WORK/config.yaml" <<CFG
server:
  addr: "127.0.0.1:${PORT}"
  dashboard_token: "${TOKEN}"
workspace:
  id: "release-gate"
  name: "Release Gate"
cli:
  path: "$WORK/stub-cli"
  args: []
session:
  max_procs: 2
  ttl: "30m"
  cwd: "$WORK"
  store_path: "$WORK/sessions.json"
CFG
chmod 0600 "$WORK/config.yaml"

echo "[gate-instance] starting $BIN on 127.0.0.1:${PORT}"
HOME="$WORK" NAOZHI_DASHBOARD_TOKEN="$TOKEN" \
  "$BIN" --config "$WORK/config.yaml" > "$WORK/naozhi.log" 2>&1 &
PID=$!

# release-gate.mjs polls /health itself, so we just hand off. If the process
# died on boot, surface its log before the gate's health timeout obscures it.
sleep 2
if ! kill -0 "$PID" 2>/dev/null; then
  echo "[fatal] naozhi exited during boot:" >&2
  cat "$WORK/naozhi.log" >&2
  exit 3
fi

# `|| rc=$?` keeps set -e from aborting before we can print the instance log;
# without it the diagnostic tail below would be dead code on failure.
rc=0
NAOZHI_BASE_URL="http://127.0.0.1:${PORT}" \
NAOZHI_DASHBOARD_TOKEN="$TOKEN" \
  node scripts/release-gate.mjs || rc=$?

if [ "$rc" -ne 0 ]; then
  echo "[gate-instance] gate failed (rc=$rc) — instance log tail:" >&2
  tail -20 "$WORK/naozhi.log" >&2
fi
exit "$rc"
