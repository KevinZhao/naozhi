#!/usr/bin/env bash
# ci-test-shard.sh — the package set one CI test shard runs (#2828).
#
#   scripts/ci-test-shard.sh a|b
#
# Shard a is the packages with the longest -race runs; shard b is every other
# package, derived by exclusion so a package added anywhere is tested by b
# rather than by nobody. Together they run exactly `go test -race ./...`,
# with the quarantine list (.github/quarantine.txt, #2537) skipped as before.
set -euo pipefail

heavy='^github.com/naozhi/naozhi/internal/(cli|cron|server|upstream|history|dashboard)(/|$)'

case "${1:-}" in
  a) pkgs=$(go list ./... | grep -E "$heavy") ;;
  b) pkgs=$(go list ./... | grep -vE "$heavy") ;;
  *) echo "usage: $0 a|b" >&2; exit 2 ;;
esac

pattern=$(grep -v '^#' .github/quarantine.txt 2>/dev/null | grep -v '^$' | paste -sd'|' - || true)
skip=()
if [ -n "$pattern" ]; then
  skip=(-skip "$pattern")
fi

# shellcheck disable=SC2086 # one package per word
go test -race -timeout 300s "${skip[@]}" $pkgs
