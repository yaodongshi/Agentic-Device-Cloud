#!/usr/bin/env bash
set -euo pipefail

ROOT=${1:-.}
ROOT=$(cd "$ROOT" && pwd)
REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)
FAIL=0

report() {
  printf 'immutable-check: %s\n' "$1" >&2
  FAIL=$((FAIL + 1))
}

check_dockerfiles() {
  local file line ref
  while IFS= read -r -d '' file; do
    [[ "$ROOT" == "$REPO_ROOT" && "$file" == "$REPO_ROOT/scripts/fixtures/immutable/"* ]] && continue
    while IFS= read -r line; do
      ref=${line#FROM }
      ref=${ref#--platform=* }
      ref=${ref%% *}
      [[ "$ref" =~ ^[^:@[:space:]]+([^@[:space:]]*)?:[^@[:space:]]+@sha256:[0-9a-f]{64}$ ]] || report "$file has mutable FROM: $ref"
    done < <(awk 'toupper($1)=="FROM" {print}' "$file")
  done < <(find "$ROOT" -type f \( -name 'Dockerfile' -o -name 'Dockerfile.*' \) -print0)
}

check_compose() {
  local file line ref previous
  while IFS= read -r -d '' file; do
    [[ "$ROOT" == "$REPO_ROOT" && "$file" == "$REPO_ROOT/scripts/fixtures/immutable/"* ]] && continue
    previous=
    while IFS= read -r line; do
      if [[ "$line" =~ ^[[:space:]]*build: ]]; then
        previous=build
        continue
      fi
      if [[ "$line" =~ ^[[:space:]]*image:[[:space:]]*([^#[:space:]]+) ]]; then
        ref=${BASH_REMATCH[1]}
        if [[ "$ref" =~ ^\$\{ADC_(CE|PY_AGENT|CONSOLE)_IMAGE:-adc/(ce|py-agent|console):dev\}$ ]]; then
          previous=
          continue
        fi
        [[ "$ref" =~ ^[^@[:space:]]+:[^@[:space:]]+@sha256:[0-9a-f]{64}$ ]] || report "$file has mutable image: $ref"
      fi
      [[ -z "${line//[[:space:]]/}" ]] || previous=
    done < "$file"
  done < <(find "$ROOT" -type f \( -name 'compose.yml' -o -name 'compose.yaml' -o -name 'docker-compose.yml' -o -name 'docker-compose.yaml' \) -print0)
}

check_workflows() {
  local file line ref
  while IFS= read -r -d '' file; do
    grep -Eq '^permissions:' "$file" || report "$file lacks workflow permissions"
    if ! awk '
      /^jobs:/ { in_jobs=1; next }
      in_jobs && /^  [A-Za-z0-9_-]+:$/ {
        if (job != "" && ! permission) exit 1
        job=$1
        permission=0
        next
      }
      in_jobs && job != "" && /^    permissions:/ { permission=1 }
      END { if (job != "" && ! permission) exit 1 }
    ' "$file"; then
      report "$file has a job without explicit permissions"
    fi
    while IFS= read -r line; do
      ref=${line#*uses: }
      ref=${ref%% *}
      [[ "$ref" =~ ^[^@[:space:]]+@[0-9a-f]{40}$ ]] || report "$file has mutable action: $ref"
    done < <(grep -E '^[[:space:]]*-?[[:space:]]*uses:[[:space:]]*' "$file" || true)
    if grep -Eq 'permissions:[[:space:]]*(write-all|read-all)' "$file"; then
      report "$file uses broad permissions"
    fi
  done < <(find "$ROOT/.github/workflows" -type f \( -name '*.yml' -o -name '*.yaml' \) -print0 2>/dev/null || true)
}

check_dockerfiles
check_compose
check_workflows

if (( FAIL > 0 )); then
  printf 'immutable-check: FAIL=%d\n' "$FAIL" >&2
  exit 1
fi
printf 'immutable-check: PASS\n'
