#!/usr/bin/env bash
set -euo pipefail

if ! command -v docker >/dev/null 2>&1 && [[ -x /Applications/Docker.app/Contents/Resources/bin/docker ]]; then
  export PATH="$PATH:/Applications/Docker.app/Contents/Resources/bin"
fi
command -v docker >/dev/null 2>&1 || { printf 'guard-smoke: FAIL docker unavailable\n' >&2; exit 1; }

BASE=${ADC_BASE_URL:-http://127.0.0.1:18080}
AGENT_API_KEY=${ADC_SMOKE_AGENT_API_KEY:-dev-agent-key}
PASS=0
FAIL=0

ok() { printf 'guard-smoke: PASS %s\n' "$1"; PASS=$((PASS + 1)); }
bad() { printf 'guard-smoke: FAIL %s\n' "$1" >&2; FAIL=$((FAIL + 1)); }
ticket_count() {
  docker exec adc-postgres psql -U "${ADC_PG_USER:-adc}" -d "${ADC_PG_DBNAME:-adc}" -Atc \
    "SELECT count(*) FROM adc_approval_tickets;" 2>/dev/null || printf 'unavailable'
}
request() {
  curl -s -o /tmp/adc-guard-response.json -w '%{http_code}' \
    -H "X-ADC-Key: $AGENT_API_KEY" -H 'Content-Type: application/json' \
    -d "$1" "$BASE/v1/agent/mcp/tools/call" || true
}
reject_case() {
  local label=$1 payload=$2 before after code
  before=$(ticket_count)
  code=$(request "$payload")
  after=$(ticket_count)
  if [[ "$code" == 400 && "$before" == "$after" ]]; then
    ok "$label returned 400 without creating an approval ticket"
  else
    bad "$label expected HTTP 400 and unchanged ticket count; got HTTP $code, $before -> $after"
  fi
}

reject_case missing-required '{"name":"cnc-demo-01::set_spindle_speed","arguments":{}}'
reject_case type-error '{"name":"cnc-demo-01::set_spindle_speed","arguments":{"rpm":"fast"}}'
reject_case range-violation '{"name":"cnc-demo-01::set_spindle_speed","arguments":{"rpm":12001}}'

before=$(ticket_count)
valid_rpm=$(docker exec adc-postgres psql -U "${ADC_PG_USER:-adc}" -d "${ADC_PG_DBNAME:-adc}" -Atc \
  "SELECT rpm FROM generate_series(1,12000) AS rpm
   WHERE NOT EXISTS (
     SELECT 1 FROM adc_approval_tickets
     WHERE status='PENDING' AND tool_name='set_spindle_speed'
       AND (arguments->>'rpm')::numeric=rpm
   ) LIMIT 1;" 2>/dev/null || true)
if [[ -z "$valid_rpm" ]]; then
  bad 'valid path could not allocate an unused rpm'
else
  code=$(request "{\"name\":\"cnc-demo-01::set_spindle_speed\",\"arguments\":{\"rpm\":${valid_rpm}}}")
  after=$(ticket_count)
  if [[ "$code" == 202 && "$before" != unavailable && "$after" -eq $((before + 1)) ]]; then
    ok 'valid high-risk request returned 202 and created one approval ticket'
  else
    bad "valid path expected HTTP 202 and one ticket; got HTTP $code, $before -> $after"
  fi
fi

printf 'guard-smoke: PASS=%d FAIL=%d SKIP=0\n' "$PASS" "$FAIL"
(( FAIL == 0 ))
