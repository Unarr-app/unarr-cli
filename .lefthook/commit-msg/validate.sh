#!/bin/bash
# Validate commit message follows Conventional Commits format.
# Allowed types: feat, fix, docs, test, chore, refactor, ci, style, perf, build
#
# Valid examples:
#   feat: add search by genre
#   fix(client): handle nil response
#   docs: update README
#   feat!: breaking change in API

commit_msg_file="$1"
commit_msg=$(head -1 "$commit_msg_file")

# Allow merge commits
if echo "$commit_msg" | grep -qE '^Merge '; then
  exit 0
fi

# Conventional Commits regex:
#   type(optional-scope)optional-!: description
pattern='^(feat|fix|docs|test|chore|refactor|ci|style|perf|build)(\([a-zA-Z0-9_-]+\))?!?: .+'

if ! echo "$commit_msg" | grep -qE "$pattern"; then
  echo "ERROR: Commit message does not follow Conventional Commits format."
  echo ""
  echo "  Expected: <type>[optional scope]: <description>"
  echo ""
  echo "  Allowed types: feat, fix, docs, test, chore, refactor, ci, style, perf, build"
  echo ""
  echo "  Examples:"
  echo "    feat: add search by genre"
  echo "    fix(client): handle nil response body"
  echo "    docs: update API reference"
  echo ""
  echo "  Your message: $commit_msg"
  exit 1
fi
