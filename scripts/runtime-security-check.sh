#!/usr/bin/env bash
set -euo pipefail

if ! command -v docker >/dev/null 2>&1 && [[ -x /Applications/Docker.app/Contents/Resources/bin/docker ]]; then
  export PATH="$PATH:/Applications/Docker.app/Contents/Resources/bin"
fi
command -v docker >/dev/null 2>&1 || { printf 'runtime-security: FAIL docker unavailable\n' >&2; exit 1; }

COMPOSE=(docker compose -f deploy/compose.yaml)
PASS=0
FAIL=0
CANARY='cny-7f90c441'

ok() { printf 'runtime-security: PASS %s\n' "$1"; PASS=$((PASS + 1)); }
bad() { printf 'runtime-security: FAIL %s\n' "$1" >&2; FAIL=$((FAIL + 1)); }

default_services=$("${COMPOSE[@]}" config --services)
if grep -qx dev-seed <<<"$default_services"; then bad 'default topology contains dev-seed'; else ok 'default topology excludes dev-seed'; fi
profile_services=$("${COMPOSE[@]}" --profile dev-seed config --services)
if grep -qx dev-seed <<<"$profile_services"; then ok 'dev-seed profile exposes one-shot service'; else bad 'dev-seed profile missing'; fi

seed_log=$(mktemp)
guard_log=$(mktemp)
trap 'rm -f "$seed_log" "$guard_log"' EXIT
if ADC_ENV=production "${COMPOSE[@]}" --profile dev-seed run --rm --no-deps dev-seed >"$seed_log" 2>&1; then
  bad 'non-development seed was accepted'
elif grep -Fq 'seed: ADC_ENV rejected (development-only operation)' "$seed_log"; then
  ok 'non-development seed rejected'
else
  bad 'non-development seed failed without the environment guard reason'
fi

safe_guard=(
  ADC_ENV=production
  ADC_PG_PASSWORD=production-pg-secret-material-2026
  ADC_VALKEY_PASSWORD=production-valkey-secret-material-2026
  ADC_BOOTSTRAP_ADMIN_PASSWORD=production-admin-secret-material-2026
  ADC_BOOTSTRAP_DEVICE_SECRET=production-device-secret-material-2026
  ADC_BOOTSTRAP_APPLICATION_SECRET=production-application-secret-material-2026
  ADC_DEVICE_KEK=aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899
  ADC_NODE_KEY=0011aabbccddeeff0011aabbccddeeff
  ADC_HITL_CALLBACK_KEY=production-hitl-callback-material-2026
  ADC_CE_IMAGE=registry.example/adc/ce:2.2@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  ADC_PY_AGENT_IMAGE=registry.example/adc/py-agent:2.2@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
  ADC_CONSOLE_IMAGE=registry.example/adc/console:2.2@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
)
reject_guard() {
  local label=$1 override=$2
  if env "${safe_guard[@]}" "$override" "${COMPOSE[@]}" run --rm --no-deps runtime-guard >"$guard_log" 2>&1; then
    bad "$label was accepted"
  else
    ok "$label rejected"
  fi
}

reject_guard 'default PostgreSQL password' 'ADC_PG_PASSWORD=adc_dev_only'
reject_guard 'default Valkey password' 'ADC_VALKEY_PASSWORD=adc_dev_only'
reject_guard 'fixed development KEK' 'ADC_DEVICE_KEK=00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff'
reject_guard 'missing application credential' 'ADC_BOOTSTRAP_APPLICATION_SECRET='
reject_guard 'mutable production application image' 'ADC_CE_IMAGE=registry.example/adc/ce:2.2'
reject_guard 'canary invalid admin credential' "ADC_BOOTSTRAP_ADMIN_PASSWORD=$CANARY"
canary_sha=$(printf '%s' "$CANARY" | shasum -a 256 | cut -d' ' -f1)
if grep -Fq "$CANARY" "$guard_log" || grep -Fq "$(printf '%s' "$CANARY" | base64)" "$guard_log" || grep -Fq "$canary_sha" "$guard_log"; then
  bad 'guard log leaked canary secret'
else
  ok 'guard log redacts secret values'
fi

if env "${safe_guard[@]}" "${COMPOSE[@]}" run --rm --no-deps runtime-guard >/dev/null 2>&1; then
  ok 'valid production configuration accepted'
else
  bad 'valid production configuration rejected'
fi

rendered=$(env "${safe_guard[@]}" "${COMPOSE[@]}" config --format json)
if ADC_RENDERED_COMPOSE="$rendered" python3 -c '
import json, os
adc = json.loads(os.environ["ADC_RENDERED_COMPOSE"])["services"]["adc"]["environment"]
assert adc["ADC_NODE_KEY"] == "0011aabbccddeeff0011aabbccddeeff"
assert adc["ADC_HITL_CALLBACK_KEY"] == "production-hitl-callback-material-2026"
'; then
  ok 'validated node and HITL keys propagate to application topology'
else
  bad 'validated node or HITL key missing from application topology'
fi

printf 'runtime-security: PASS=%d FAIL=%d SKIP=0\n' "$PASS" "$FAIL"
(( FAIL == 0 ))
