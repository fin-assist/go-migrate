#!/usr/bin/env bash
# Validate execution evidence and report applicability; the external tool calculates coverage.
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo 'usage: check-suite.sh TEST_JSONL COVER_PROFILE' >&2
  exit 2
fi

if ! jq -e -s '
  any(.[]; .Action == "pass" and has("Test")) and
  all(.[]; .Action != "fail" and (.Action != "skip" or (has("Test") | not)))
' "$1" > /dev/null; then
  echo 'Suite must execute and pass at least one test, with no skips or failures.' >&2
  exit 1
fi

# Only the original template may report N/A, and only for Go's exact empty race profile.
# Keep this identity unchanged when creating a library from the template.
if cmp -s "$2" <(printf 'mode: atomic\n'); then
  if [ "${GITHUB_ACTIONS:-}" = true ]; then
    original_template=false
    if [ "${GITHUB_REPOSITORY:-}" = fin-assist/go-library-template ]; then
      original_template=true
    fi
  else
    original_template=false
    origin=$(git config --get remote.origin.url || true)
    case "$origin" in
      https://github.com/fin-assist/go-library-template|https://github.com/fin-assist/go-library-template.git|git@github.com:fin-assist/go-library-template.git|ssh://git@github.com/fin-assist/go-library-template.git)
        original_template=true ;;
    esac
  fi
  if [ "$original_template" = true ]; then
    echo 'N/A'
    exit 0
  fi
fi

# Reject absent, empty, header-only and zero-statement profiles, without computing a ratio.
if ! grep -Eq '^[^[:space:]]+:[0-9]+\.[0-9]+,[0-9]+\.[0-9]+[[:space:]]+[1-9][0-9]*[[:space:]]+[0-9]+$' "$2"; then
  echo 'Coverage profile must contain a block with executable statements.' >&2
  exit 1
fi
echo 'statements'
