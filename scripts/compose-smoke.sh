#!/usr/bin/env bash
set -euo pipefail

if ! command -v docker >/dev/null 2>&1 && [[ -x /Applications/Docker.app/Contents/Resources/bin/docker ]]; then
  export PATH="$PATH:/Applications/Docker.app/Contents/Resources/bin"
fi
command -v docker >/dev/null 2>&1 || { printf 'compose-smoke: FAIL docker unavailable\n' >&2; exit 1; }
[[ ${ADC_COMPOSE_SMOKE_DESTRUCTIVE:-} == 1 ]] || {
  printf 'compose-smoke: FAIL set ADC_COMPOSE_SMOKE_DESTRUCTIVE=1 on an isolated runner\n' >&2
  exit 1
}

COMPOSE=(docker compose -f deploy/compose.yaml)
export ADC_ENV=dev
export ADC_DEVICE_KEK=${ADC_DEVICE_KEK:-aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899}
export ADC_DEV_SEED_CNC_SECRET=${ADC_DEV_SEED_CNC_SECRET:-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa}
export ADC_DEV_SEED_AGV_SECRET=${ADC_DEV_SEED_AGV_SECRET:-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb}
export ADC_DEV_SEED_ADMIN_PASSWORD=${ADC_DEV_SEED_ADMIN_PASSWORD:-compose-smoke-admin-password}
export ADC_DEV_SEED_TENANT_ADMIN_PASSWORD=${ADC_DEV_SEED_TENANT_ADMIN_PASSWORD:-compose-smoke-tenant-password}
export ADC_DEV_SEED_APPROVER_PASSWORD=${ADC_DEV_SEED_APPROVER_PASSWORD:-compose-smoke-approver-password}
export ADC_DEV_SEED_AGENT_API_KEY=${ADC_DEV_SEED_AGENT_API_KEY:-adc_1234abcd_cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc}

cleanup() {
  "${COMPOSE[@]}" --profile dev-seed down -v --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

# Fixed container names make project-name isolation insufficient. Destructive
# smoke is only valid on a fresh CI runner; never touch an existing ADC stack.
if docker ps -a --format '{{.Names}}' | grep -Eq '^adc-' || \
   docker volume ls --format '{{.Name}}' | grep -Eq '^adc_'; then
  printf 'compose-smoke: FAIL existing ADC containers or volumes detected; use a fresh isolated runner\n' >&2
  trap - EXIT
  exit 1
fi

cleanup
"${COMPOSE[@]}" build adc py-agent console
"${COMPOSE[@]}" up -d postgres valkey adc-migrate
if [[ $(docker exec adc-postgres psql -U "${ADC_PG_USER:-adc}" -d "${ADC_PG_DBNAME:-adc}" -Atc \
  "SELECT count(*) FROM adc_tenants WHERE code='tenant-demo';") != 0 ]]; then
  printf 'compose-smoke: FAIL default migration path created seed data\n' >&2
  exit 1
fi
"${COMPOSE[@]}" --profile dev-seed run --rm dev-seed

snapshot() {
  docker exec adc-postgres psql -U "${ADC_PG_USER:-adc}" -d "${ADC_PG_DBNAME:-adc}" -Atc \
    "SELECT string_agg(username||':'||password_hash||':'||status, E'\\n' ORDER BY username)
       FROM adc_users WHERE username IN ('admin','tenant-admin','approver');
     SELECT string_agg(device_code||':'||credential_hash||':'||status, E'\\n' ORDER BY device_code)
       FROM adc_devices WHERE device_code IN ('cnc-demo-01','agv-demo-01');"
}
before=$(snapshot)
canary='compose-seed-canary-secret-7f90c441'
seed_log=$(mktemp)
ADC_DEV_SEED_CNC_SECRET="${canary}aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" \
ADC_DEV_SEED_AGV_SECRET="${canary}bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" \
ADC_DEV_SEED_ADMIN_PASSWORD="$canary-admin" \
ADC_DEV_SEED_TENANT_ADMIN_PASSWORD="$canary-tenant" \
ADC_DEV_SEED_APPROVER_PASSWORD="$canary-approver" \
ADC_DEV_SEED_AGENT_API_KEY="adc_deadbeef_${canary}application" \
  "${COMPOSE[@]}" --profile dev-seed run --rm dev-seed >"$seed_log" 2>&1
after=$(snapshot)
[[ "$before" == "$after" ]] || { printf 'compose-smoke: FAIL repeated seed changed credentials or status\n' >&2; exit 1; }
canary_b64=$(printf '%s' "$canary" | base64 | tr -d '\n')
canary_sha=$(printf '%s' "$canary" | sha256sum | cut -d' ' -f1)
if grep -Fq "$canary" "$seed_log" || grep -Fq "$canary_b64" "$seed_log" || grep -Fq "$canary_sha" "$seed_log"; then
  printf 'compose-smoke: FAIL seed log leaked canary material\n' >&2
  exit 1
fi
rm -f "$seed_log"
"${COMPOSE[@]}" up -d adc py-agent gateway console

ADC_M9_ADMIN_PASSWORD="$ADC_DEV_SEED_ADMIN_PASSWORD" \
ADC_M9_APPROVER_PASSWORD="$ADC_DEV_SEED_APPROVER_PASSWORD" \
ADC_M9_TENANT_ADMIN_PASSWORD="$ADC_DEV_SEED_TENANT_ADMIN_PASSWORD" \
ADC_M9_AGENT_API_KEY="$ADC_DEV_SEED_AGENT_API_KEY" \
bash scripts/m9-runtime-tests.sh
