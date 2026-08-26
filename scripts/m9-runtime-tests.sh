#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"

if ! command -v docker >/dev/null 2>&1 && [[ -x /Applications/Docker.app/Contents/Resources/bin/docker ]]; then
  export PATH="$PATH:/Applications/Docker.app/Contents/Resources/bin"
fi

PASS=0
FAIL=0
SKIP=0
BASE=${ADC_BASE_URL:-http://127.0.0.1:18080}
ADMIN_USER=${ADC_M9_ADMIN_USER:-admin}
ADMIN_PASSWORD=${ADC_M9_ADMIN_PASSWORD:-${ADC_SMOKE_ADMIN_PASSWORD:-}}
APPROVER_USER=${ADC_M9_APPROVER_USER:-approver}
APPROVER_PASSWORD=${ADC_M9_APPROVER_PASSWORD:-}
TENANT_ADMIN_USER=${ADC_M9_TENANT_ADMIN_USER:-tenant-admin}
TENANT_ADMIN_PASSWORD=${ADC_M9_TENANT_ADMIN_PASSWORD:-}
AGENT_API_KEY=${ADC_M9_AGENT_API_KEY:-${ADC_SMOKE_AGENT_API_KEY:-}}
PG_USER=${ADC_PG_USER:-adc}
PG_PASSWORD=${ADC_PG_PASSWORD:-adc_dev_only}
PG_DBNAME=${ADC_PG_DBNAME:-adc}
GO_IMAGE=${ADC_M9_GO_IMAGE:-golang:1.26-alpine@sha256:28d89ee9cc0ff9fec75c82ca201e6bf7fdf9a679d4b7b24dfa04f2bb766bb468}
COMPOSE=(docker compose -f deploy/compose.yaml)
TMP=$(mktemp -d)
CREATED_APPS=()
ADMIN_TOKEN=
TENANT_ID=
APPROVER_TOKEN=
TENANT_ADMIN_TOKEN=
LLM_CRED=
LLM_CRED_ID=
EVAL_CRED=
EVAL_CRED_ID=
A2A_READ_CRED=
A2A_READ_CRED_ID=
A2A_RW_CRED=
A2A_RW_CRED_ID=
TASK_ID=
TASK_VERSION=
CONCURRENT_TASK_ID=

ok() {
  printf 'm9-runtime: PASS %s\n' "$1"
  PASS=$((PASS + 1))
}

bad() {
  printf 'm9-runtime: FAIL %s\n' "$1" >&2
  FAIL=$((FAIL + 1))
}

run_gate() {
  local label=$1
  shift
  if "$@"; then
    ok "$label"
  else
    bad "$label"
  fi
}

json_field() {
  local field=$1
  python3 -c 'import json,sys
field=sys.argv[1]
value=json.load(sys.stdin)
for part in field.split("."):
    value=value[int(part)] if isinstance(value,list) else value.get(part)
    if value is None: break
print("" if value is None else value)' "$field"
}

http() {
  local method=$1 url=$2 output=$3
  shift 3
  curl --silent --show-error --max-time 15 -o "$output" -w '%{http_code}' -X "$method" "$@" "$url" || printf '000'
}

expect_http() {
  local label=$1 expected=$2 method=$3 url=$4
  shift 4
  local body="$TMP/http-${PASS}-${FAIL}.json" code
  code=$(http "$method" "$url" "$body" "$@")
  if [[ "$code" == "$expected" ]]; then
    ok "$label: HTTP $code body=$(tr '\n' ' ' <"$body")"
  else
    bad "$label: expected HTTP $expected, got $code body=$(tr '\n' ' ' <"$body")"
  fi
}

wait_container_health() {
  local name=$1 state
  for _ in $(seq 1 30); do
    state=$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$name" 2>/dev/null || true)
    [[ "$state" == healthy || "$state" == running ]] && return 0
    sleep 2
  done
  return 1
}

cleanup() {
  local app_id
  if [[ -n ${ADMIN_TOKEN:-} ]]; then
    for app_id in "${CREATED_APPS[@]}"; do
      curl --silent --max-time 10 -o /dev/null -X DELETE \
        -H "Authorization: Bearer $ADMIN_TOKEN" \
        "$BASE/v1/admin/developer-applications/$app_id" || true
    done
  fi
  rm -rf "$TMP"
}
trap cleanup EXIT

if ! command -v docker >/dev/null 2>&1; then
  bad 'docker CLI unavailable, including the macOS Docker Desktop standard path'
  printf 'm9-runtime: PASS=%d FAIL=%d SKIP=%d\n' "$PASS" "$FAIL" "$SKIP"
  exit 1
fi
if ! docker info >/dev/null 2>&1; then
  bad 'Docker daemon unavailable'
  printf 'm9-runtime: PASS=%d FAIL=%d SKIP=%d\n' "$PASS" "$FAIL" "$SKIP"
  exit 1
fi
ok "Docker CLI $(docker version --format '{{.Client.Version}}') and daemon available"

for dependency in curl python3 go; do
  if command -v "$dependency" >/dev/null 2>&1; then
    ok "host dependency available: $dependency"
  else
    bad "host dependency unavailable: $dependency"
  fi
done
for container in adc-postgres adc-app adc-py-agent adc-gateway adc-console; do
  if wait_container_health "$container"; then
    ok "$container is running and healthy"
  else
    bad "$container is not running and healthy"
  fi
done
for name in ADC_M9_ADMIN_PASSWORD ADC_M9_APPROVER_PASSWORD ADC_M9_TENANT_ADMIN_PASSWORD ADC_M9_AGENT_API_KEY; do
  case "$name" in
    ADC_M9_ADMIN_PASSWORD) value=$ADMIN_PASSWORD ;;
    ADC_M9_APPROVER_PASSWORD) value=$APPROVER_PASSWORD ;;
    ADC_M9_TENANT_ADMIN_PASSWORD) value=$TENANT_ADMIN_PASSWORD ;;
    ADC_M9_AGENT_API_KEY) value=$AGENT_API_KEY ;;
  esac
  if [[ -n "$value" ]]; then ok "$name supplied"; else bad "$name is required"; fi
done

run_gate 'immutable checker self-test completed with no skip' bash scripts/immutable-check-selftest.sh
run_gate 'repository immutable input check completed' bash scripts/immutable-check.sh
run_gate 'runtime seed/guard/redaction check completed with no skip' bash scripts/runtime-security-check.sh
run_gate 'four-path runtime parameter guard completed with no skip' env ADC_SMOKE_AGENT_API_KEY="$AGENT_API_KEY" bash scripts/guard-smoke.sh

expect_http 'Python health is public' 200 GET "$BASE/healthz"
expect_http 'Agent Card is public' 200 GET "$BASE/.well-known/agent-card.json"
expect_http 'anonymous LLM route is rejected' 401 GET "$BASE/v2/agents/llm/models"
expect_http 'anonymous Eval route is rejected' 401 GET "$BASE/v2/agents/evals/suites"
expect_http 'anonymous A2A route is rejected' 401 GET "$BASE/v2/agents/a2a/tasks"

login() {
  local username=$1 password=$2 output=$3 code
  code=$(http POST "$BASE/v1/admin/auth/login" "$output" \
    -H 'Content-Type: application/json' \
    --data "$(python3 -c 'import json,sys; print(json.dumps({"username":sys.argv[1],"password":sys.argv[2]}))' "$username" "$password")")
  [[ "$code" == 200 ]]
}

if login "$ADMIN_USER" "$ADMIN_PASSWORD" "$TMP/admin-login.json"; then
  ADMIN_TOKEN=$(json_field token <"$TMP/admin-login.json")
  TENANT_ID=$(json_field tenant_id <"$TMP/admin-login.json")
  if [[ -n "$ADMIN_TOKEN" && -n "$TENANT_ID" ]]; then
    ok "platform admin session issued for tenant $TENANT_ID"
  else
    bad 'platform admin login response omitted token or tenant_id'
  fi
else
  ADMIN_TOKEN=
  TENANT_ID=
  bad "platform admin login failed for $ADMIN_USER"
fi

create_app() {
  local variable=$1 name=$2 scopes_json=$3 output="$TMP/app-$name.json" code app_id secret
  code=$(http POST "$BASE/v1/admin/developer-applications?tenant_id=$TENANT_ID" "$output" \
    -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' \
    --data "{\"name\":\"$name\",\"purpose\":\"M9 runtime test; deleted on exit\",\"scopes\":$scopes_json}")
  if [[ "$code" != 201 ]]; then
    bad "create $name: expected HTTP 201, got $code body=$(tr '\n' ' ' <"$output")"
    printf -v "$variable" '%s' ''
    return 0
  fi
  app_id=$(json_field application_id <"$output")
  secret=$(json_field secret <"$output")
  if [[ -z "$app_id" || -z "$secret" ]]; then
    bad "create $name: one-shot application id or secret missing"
    printf -v "$variable" '%s' ''
    return 0
  fi
  CREATED_APPS+=("$app_id")
  printf -v "$variable" '%s' "$secret"
  printf -v "${variable}_ID" '%s' "$app_id"
  ok "created temporary application $name id=$app_id scopes=$scopes_json"
}

run_id=$(date +%s)-$$
create_app LLM_CRED "m9-llm-$run_id" '["llm:invoke"]'
create_app EVAL_CRED "m9-eval-$run_id" '["evals:read","evals:write"]'
create_app A2A_READ_CRED "m9-a2a-read-$run_id" '["a2a.tasks:read"]'
create_app A2A_RW_CRED "m9-a2a-rw-$run_id" '["a2a.tasks:read","a2a.tasks:write"]'

app_header() { printf 'Authorization: Bearer %s' "$1"; }

expect_http 'LLM-only credential may list models without external model call' 200 GET "$BASE/v2/agents/llm/models" -H "$(app_header "$LLM_CRED")"
expect_http 'LLM-only credential cannot read Eval' 403 GET "$BASE/v2/agents/evals/suites" -H "$(app_header "$LLM_CRED")"
expect_http 'LLM-only credential cannot read A2A' 403 GET "$BASE/v2/agents/a2a/tasks" -H "$(app_header "$LLM_CRED")"
expect_http 'Eval credential may list suites' 200 GET "$BASE/v2/agents/evals/suites" -H "$(app_header "$EVAL_CRED")"
expect_http 'Eval credential cannot invoke LLM' 403 GET "$BASE/v2/agents/llm/models" -H "$(app_header "$EVAL_CRED")"
eval_code=$(http POST "$BASE/v2/agents/evals/runs" "$TMP/eval-run.json" \
  -H "$(app_header "$EVAL_CRED")" -H 'Content-Type: application/json' \
  --data '{"suite":"m9-runtime","cases":[{"name":"mock-ok","tool":"dev-001::get_telemetry","args":{},"expect_status":"ok"}]}')
eval_passed=$(json_field passed <"$TMP/eval-run.json" 2>/dev/null || true)
eval_failed=$(json_field failed <"$TMP/eval-run.json" 2>/dev/null || true)
if [[ "$eval_code" == 201 && "$eval_passed" == 1 && "$eval_failed" == 0 ]]; then
  ok 'Eval credential executed the in-process mock evaluation with passed=1 failed=0'
else
  bad "Eval mock execution expected HTTP 201/passed 1/failed 0, got HTTP $eval_code body=$(tr '\n' ' ' <"$TMP/eval-run.json")"
fi
expect_http 'A2A read credential may list tasks' 200 GET "$BASE/v2/agents/a2a/tasks" -H "$(app_header "$A2A_READ_CRED")"
expect_http 'A2A read credential cannot create tasks' 403 POST "$BASE/v2/agents/a2a/tasks" \
  -H "$(app_header "$A2A_READ_CRED")" -H 'Content-Type: application/json' -H 'Idempotency-Key: m9-read-denied' \
  --data '{"task_type":"maintain","goal":"denied","devices":["cnc-demo-01"]}'
expect_http 'A2A read credential cannot invoke LLM' 403 GET "$BASE/v2/agents/llm/models" -H "$(app_header "$A2A_READ_CRED")"

revoke_code=$(http POST "$BASE/v1/admin/developer-applications/$LLM_CRED_ID/credentials/revoke" "$TMP/revoke.json" \
  -H "Authorization: Bearer $ADMIN_TOKEN")
if [[ "$revoke_code" == 200 ]]; then
  ok "LLM application credential revoked through management API id=$LLM_CRED_ID"
else
  bad "credential revoke expected HTTP 200, got $revoke_code body=$(tr '\n' ' ' <"$TMP/revoke.json")"
fi
expect_http 'revoked credential is rejected on the next Python request' 401 GET "$BASE/v2/agents/llm/models" -H "$(app_header "$LLM_CRED")"

TASK_BODY='{"task_type":"maintain","goal":"M9 durable spindle inspection","devices":["cnc-demo-01"]}'
TASK_BODY_DIFFERENT='{"task_type":"maintain","goal":"M9 conflicting request","devices":["cnc-demo-01"]}'
IDEMPOTENCY_KEY="m9-$run_id"
create_code=$(http POST "$BASE/v2/agents/a2a/tasks" "$TMP/task-create.json" \
  -H "$(app_header "$A2A_RW_CRED")" -H 'Content-Type: application/json' -H "Idempotency-Key: $IDEMPOTENCY_KEY" --data "$TASK_BODY")
TASK_ID=$(json_field task_id <"$TMP/task-create.json" 2>/dev/null || true)
TASK_VERSION=$(json_field version <"$TMP/task-create.json" 2>/dev/null || true)
if [[ "$create_code" == 202 && -n "$TASK_ID" && "$TASK_VERSION" == 1 ]]; then
  ok "A2A task persisted id=$TASK_ID state=input-required version=$TASK_VERSION"
else
  bad "A2A create expected HTTP 202/version 1, got $create_code body=$(tr '\n' ' ' <"$TMP/task-create.json")"
fi

repeat_code=$(http POST "$BASE/v2/agents/a2a/tasks" "$TMP/task-repeat.json" \
  -H "$(app_header "$A2A_RW_CRED")" -H 'Content-Type: application/json' -H "Idempotency-Key: $IDEMPOTENCY_KEY" --data "$TASK_BODY")
repeat_id=$(json_field task_id <"$TMP/task-repeat.json" 2>/dev/null || true)
if [[ "$repeat_code" == 202 && -n "$TASK_ID" && "$repeat_id" == "$TASK_ID" ]]; then
  ok "same idempotency key and body returned original task id=$TASK_ID"
else
  bad "idempotent retry expected original task, got HTTP $repeat_code id=$repeat_id"
fi
expect_http 'same idempotency key with different body conflicts' 409 POST "$BASE/v2/agents/a2a/tasks" \
  -H "$(app_header "$A2A_RW_CRED")" -H 'Content-Type: application/json' -H "Idempotency-Key: $IDEMPOTENCY_KEY" --data "$TASK_BODY_DIFFERENT"
expect_http 'machine application cannot submit HITL decision' 403 POST "$BASE/v2/agents/a2a/tasks/$TASK_ID/decision" \
  -H "$(app_header "$A2A_RW_CRED")" -H 'Content-Type: application/json' --data '{"decision":"approve","reason":"machine self approval"}'

if docker restart adc-py-agent >/dev/null && wait_container_health adc-py-agent; then
  ok 'adc-py-agent restarted and returned healthy'
else
  bad 'adc-py-agent failed to restart healthy'
fi
restart_code=$(http GET "$BASE/v2/agents/a2a/tasks/$TASK_ID" "$TMP/task-after-restart.json" -H "$(app_header "$A2A_RW_CRED")")
restart_id=$(json_field task_id <"$TMP/task-after-restart.json" 2>/dev/null || true)
restart_version=$(json_field version <"$TMP/task-after-restart.json" 2>/dev/null || true)
if [[ "$restart_code" == 200 && "$restart_id" == "$TASK_ID" && "$restart_version" == "$TASK_VERSION" ]]; then
  ok "task survived Python restart id=$restart_id version=$restart_version"
else
  bad "task restart recovery failed: HTTP $restart_code id=$restart_id version=$restart_version"
fi

if login "$APPROVER_USER" "$APPROVER_PASSWORD" "$TMP/approver-login.json"; then
  APPROVER_TOKEN=$(json_field token <"$TMP/approver-login.json")
  ok "human approver session issued for $APPROVER_USER"
else
  APPROVER_TOKEN=
  bad "human approver login failed for $APPROVER_USER"
fi
decision_code=$(http POST "$BASE/v1/admin/a2a/tasks/$TASK_ID/decision" "$TMP/task-decision.json" \
  -H "Authorization: Bearer $APPROVER_TOKEN" -H 'Content-Type: application/json' \
  --data "{\"decision\":\"approve\",\"reason\":\"M9 runtime approval\",\"expected_version\":$TASK_VERSION}")
decision_state=$(json_field state <"$TMP/task-decision.json" 2>/dev/null || true)
decision_version=$(json_field version <"$TMP/task-decision.json" 2>/dev/null || true)
decision_actor=$(json_field approver_user_id <"$TMP/task-decision.json" 2>/dev/null || true)
if [[ "$decision_code" == 200 && "$decision_state" == working && "$decision_version" == 2 && -n "$decision_actor" ]]; then
  ok "human decision recorded actor=$decision_actor state=$decision_state version=$decision_version"
else
  bad "human decision expected HTTP 200/working/version 2, got $decision_code body=$(tr '\n' ' ' <"$TMP/task-decision.json")"
fi
post_decision_code=$(http GET "$BASE/v2/agents/a2a/tasks/$TASK_ID" "$TMP/task-working.json" -H "$(app_header "$A2A_RW_CRED")")
post_decision_state=$(json_field state <"$TMP/task-working.json" 2>/dev/null || true)
if [[ "$post_decision_code" == 200 && "$post_decision_state" == working ]]; then
  ok 'approved task remains working while awaiting a real execution result'
else
  bad "approved task did not remain working: HTTP $post_decision_code state=$post_decision_state"
fi

CONCURRENT_KEY="m9-concurrent-$run_id"
concurrent_create=$(http POST "$BASE/v2/agents/a2a/tasks" "$TMP/concurrent-task.json" \
  -H "$(app_header "$A2A_RW_CRED")" -H 'Content-Type: application/json' -H "Idempotency-Key: $CONCURRENT_KEY" --data "$TASK_BODY")
CONCURRENT_TASK_ID=$(json_field task_id <"$TMP/concurrent-task.json" 2>/dev/null || true)
if [[ "$concurrent_create" == 202 && -n "$CONCURRENT_TASK_ID" ]]; then
  ok "created task for concurrent human CAS id=$CONCURRENT_TASK_ID"
else
  bad "could not create concurrent CAS task: HTTP $concurrent_create"
fi
if login "$TENANT_ADMIN_USER" "$TENANT_ADMIN_PASSWORD" "$TMP/tenant-admin-login.json"; then
  TENANT_ADMIN_TOKEN=$(json_field token <"$TMP/tenant-admin-login.json")
  ok "second authorized human session issued for $TENANT_ADMIN_USER"
else
  TENANT_ADMIN_TOKEN=
  bad "tenant admin login failed for $TENANT_ADMIN_USER"
fi

curl --silent --show-error --max-time 15 -o "$TMP/cas-one.json" -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $APPROVER_TOKEN" -H 'Content-Type: application/json' \
  --data '{"decision":"approve","reason":"concurrent approver","expected_version":1}' \
  "$BASE/v1/admin/a2a/tasks/$CONCURRENT_TASK_ID/decision" >"$TMP/cas-one.code" &
cas_pid_one=$!
curl --silent --show-error --max-time 15 -o "$TMP/cas-two.json" -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $TENANT_ADMIN_TOKEN" -H 'Content-Type: application/json' \
  --data '{"decision":"reject","reason":"concurrent tenant admin","expected_version":1}' \
  "$BASE/v1/admin/a2a/tasks/$CONCURRENT_TASK_ID/decision" >"$TMP/cas-two.code" &
cas_pid_two=$!
if wait "$cas_pid_one" && wait "$cas_pid_two"; then
  cas_one=$(<"$TMP/cas-one.code")
  cas_two=$(<"$TMP/cas-two.code")
  if [[ "$cas_one $cas_two" == '200 409' || "$cas_one $cas_two" == '409 200' ]]; then
    ok "concurrent human CAS produced exactly one success and one conflict: $cas_one/$cas_two"
  else
    bad "concurrent human CAS expected 200/409, got $cas_one/$cas_two"
  fi
else
  bad 'one or both concurrent human decision HTTP requests failed at transport level'
fi

NETWORK=$(docker inspect adc-postgres --format '{{range $name, $_ := .NetworkSettings.Networks}}{{$name}}{{end}}' 2>/dev/null || true)
if [[ -z "$NETWORK" ]]; then
  bad 'could not discover the Compose network for isolated PostgreSQL integration tests'
else
  integration_log="$TMP/go-pg-integration.log"
  if docker run --rm --network "$NETWORK" -v "$ROOT:/src" -w /src/ce \
    -e "ADC_MIGRATION_TEST_DATABASE_URL=postgres://$PG_USER:$PG_PASSWORD@postgres:5432/$PG_DBNAME?sslmode=disable" \
    "$GO_IMAGE" go test ./migrations -run 'TestPostgres(A2AHumanDecisionCAS|ImmediateSessionRevocation)$' -count=1 -v >"$integration_log" 2>&1; then
    if grep -q -- '--- SKIP:' "$integration_log"; then
      bad "isolated real-PG authz/cross-tenant test produced SKIP: $(tr '\n' ' ' <"$integration_log")"
    elif grep -q -- '--- PASS: TestPostgresA2AHumanDecisionCAS' "$integration_log" && \
         grep -q -- '--- PASS: TestPostgresImmediateSessionRevocation' "$integration_log"; then
      ok "isolated real-PG session revocation and tenant-scoped A2A CAS tests passed: $(tr '\n' ' ' <"$integration_log")"
    else
      bad "isolated real-PG test output omitted a required test: $(tr '\n' ' ' <"$integration_log")"
    fi
  else
    bad "isolated real-PG authz/cross-tenant tests failed: $(tr '\n' ' ' <"$integration_log")"
  fi
fi

run_gate 'main dev smoke completed last with FAIL=0 SKIP=0' env \
  ADC_SMOKE_ADMIN_USER="$ADMIN_USER" ADC_SMOKE_ADMIN_PASSWORD="$ADMIN_PASSWORD" \
  ADC_SMOKE_AGENT_API_KEY="$AGENT_API_KEY" bash scripts/dev-smoke.sh

printf '\nm9-runtime: PASS=%d FAIL=%d SKIP=%d\n' "$PASS" "$FAIL" "$SKIP"
(( FAIL == 0 && SKIP == 0 ))
