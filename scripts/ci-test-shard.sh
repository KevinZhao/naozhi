#!/usr/bin/env bash
# ci-test-shard.sh — the package set one CI test shard runs (#2828).
#
#   scripts/ci-test-shard.sh a|b
#
# Shard a is the packages with the longest -race runs; shard b is every other
# package, derived by exclusion so a package added anywhere is tested by b
# rather than by nobody. Together they run exactly `go test -race ./...`.
set -euo pipefail

heavy='^github.com/naozhi/naozhi/internal/(cli|cron|server|upstream|history|dashboard|session)(/|$)'

case "${1:-}" in
  a) pkgs=$(go list ./... | grep -E "$heavy") ;;
  b) pkgs=$(go list ./... | grep -vE "$heavy") ;;
  *) echo "usage: $0 a|b" >&2; exit 2 ;;
esac

# shellcheck disable=SC2086 # one package per word
go test -race -timeout 300s $pkgs

# Tests write only below their temp dirs. A file left in the checkout is a
# fixture writing relative to the package directory, and `git add -A` would
# commit it.
leftover=$(git status --porcelain --untracked-files=all)
if [ -n "$leftover" ]; then
  echo "tests left files in the checkout:" >&2
  echo "$leftover" >&2
  exit 1
fi
