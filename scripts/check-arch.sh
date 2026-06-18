#!/usr/bin/env bash
# Architectural file-length gate — golangci has no native file-line linter, so this
# enforces the web's `max-lines: 500` rule for Go. Like .golangci.arch.yml, it is scoped
# to NEW/CHANGED .go files vs a base rev (default origin/main) so the ~11 pre-existing
# god-files are grandfathered. New/touched files that cross 500 lines fail the gate.
#
# Usage: bash scripts/check-arch.sh [base-rev]   (default base: origin/main)
# Exit 0 = clean, 1 = a changed file exceeds the limit.
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
