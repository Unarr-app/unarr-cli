#!/usr/bin/env bash
# Architectural file-length gate — golangci has no native file-line linter, so this
# enforces the web's `max-lines: 500` rule for Go. Like .golangci.arch.yml, it is scoped
# to NEW/CHANGED .go files vs a base rev (default origin/main) so the ~11 pre-existing
# god-files are grandfathered. New/touched files that cross 500 lines fail the gate.
#
# Usage: bash scripts/check-arch.sh [base-rev]   (default base: origin/main)
# Exit 0 = clean, 1 = a changed file exceeds the limit.
#
# NEEDS A REAL BASE REV TO MEAN ANYTHING. With no resolvable base (a shallow clone, a
# detached checkout) collect() keeps only working-tree changes, so on a clean checkout it
# inspects ZERO files and still exits 0 with a tick. Verified on `git clone --depth 1`:
# "✓ file-length gate: 0 changed .go file(s)". A green here is only as meaningful as the
# base it was given — CI must fetch full history.
set -euo pipefail

LIMIT=500
BASE="${1:-origin/main}"

# Fall back gracefully if the base rev is missing (fresh clone / detached).
if ! git rev-parse --verify --quiet "$BASE" >/dev/null; then
  BASE="$(git rev-parse --verify --quiet main || true)"
fi

# Changed/added .go files: committed-vs-base + staged + unstaged + untracked (not yet
# git-added — git diff is blind to these, so list them explicitly). Exclude tests,
# generated, vendored, dist.
#
# Tests are excluded ON PURPOSE, matching the `path: _test\.go` exemption in
# .golangci.arch.yml: table-driven tests grow long by design (upgrade_test.go is 1236
# lines, parser_test.go 1050) and capping them would push authors toward fewer cases,
# which is the opposite of what this gate is for. Consequence worth knowing: NO linter
# and NO length check covers test code, including everything under test/e2e/ — a
# `golangci-lint ... --build-tags e2e` run still reports 0 issues there because the
# exemption, not the build tag, is what governs. That is the policy, not a gap.
#
# Build-constrained NON-test files are a different story and WERE a real gap: this
# script sees them (git does not care about //go:build), but golangci on a linux host
# does not. `make arch` now runs the golangci pass once per GOOS for that reason.
collect() {
  {
    [ -n "$BASE" ] && git diff --name-only --diff-filter=AM "$BASE"...HEAD 2>/dev/null || true
    git diff --name-only --diff-filter=AM 2>/dev/null || true
    git diff --name-only --cached --diff-filter=AM 2>/dev/null || true
    git ls-files --others --exclude-standard 2>/dev/null || true
  } | sort -u \
    | grep -E '\.go$' \
    | grep -vE '(_test\.go$|\.pb\.go$|_gen\.go$|mock_.*\.go$|^vendor/|^dist/)' || true
}

# Line count of a file at the base rev (0 if it didn't exist there). Captured into a var
# so a missing path (git show fails under pipefail) yields a clean single "0", never the
# "0\n0" that would break the numeric `[ -gt ]` test under `set -e`.
lines_at_base() {
  [ -z "$BASE" ] && { echo 0; return; }
  local n
  n=$(git show "$BASE:$1" 2>/dev/null | wc -l) || true
  echo "${n:-0}"
}

fail=0
checked=0
while IFS= read -r f; do
  [ -z "$f" ] && continue
  [ -f "$f" ] || continue
  checked=$((checked + 1))
  n=$(wc -l < "$f")
  [ "$n" -gt "$LIMIT" ] || continue
  # Grandfather: if the file was ALREADY over the limit at base, this change didn't
  # introduce the violation — skip it (mirrors eslint-suppressions.json). Only fail when
  # the file is new or the change pushed a previously-compliant file over the line.
  base_n=$(lines_at_base "$f")
  if [ "$base_n" -gt "$LIMIT" ]; then
    echo "· $f: $n lines (legacy god-file, grandfathered — was $base_n at base). Shrinking it is welcome."
    continue
  fi
  echo "✗ $f: $n lines (> $LIMIT) — this change pushed it over. Split by responsibility (SRP) into small, single-purpose files."
  fail=1
done < <(collect)

if [ "$fail" -eq 0 ]; then
  echo "✓ file-length gate: $checked changed .go file(s) all ≤ $LIMIT lines"
fi
exit "$fail"
