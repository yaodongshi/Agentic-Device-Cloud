#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
CHECK="$ROOT/scripts/immutable-check.sh"
FIXTURES="$ROOT/scripts/fixtures/immutable"
FAIL=0

expect() {
  local name=$1 expected=$2 status
  if bash "$CHECK" "$FIXTURES/$name" >/dev/null 2>&1; then status=0; else status=$?; fi
  if [[ "$expected" == pass && $status -ne 0 ]] || [[ "$expected" == fail && $status -eq 0 ]]; then
    printf 'immutable selftest: FAIL %s\n' "$name" >&2
    FAIL=$((FAIL + 1))
  else
    printf 'immutable selftest: PASS %s\n' "$name"
  fi
}

expect valid pass
expect tag-only fail
expect short-sha fail
expect bad-digest fail
printf 'immutable selftest: FAIL=%d SKIP=0\n' "$FAIL"
(( FAIL == 0 ))
