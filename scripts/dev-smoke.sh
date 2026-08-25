#!/usr/bin/env bash
# ADC 全链路 smoke 测试（容器版）：经统一网关（gateway:18080）走全部业务流量，
# dev 决策注入直连 adc 容器端口 18082；mock 设备 WSS 经网关透传。
# 前置：deploy/compose.yaml 已启动（docker compose -f deploy/compose.yaml up -d --build）
set -euo pipefail

if ! command -v docker >/dev/null 2>&1 && [ -x /Applications/Docker.app/Contents/Resources/bin/docker ]; then
  export PATH="$PATH:/Applications/Docker.app/Contents/Resources/bin"
fi
command -v docker >/dev/null 2>&1 || { echo "未找到 docker CLI"; exit 1; }
shopt -s nullglob
MIGRATION_FILES=(ce/migrations/*.up.sql)
[ "${#MIGRATION_FILES[@]}" -gt 0 ] || { echo "未找到迁移文件"; exit 1; }
LATEST_MIGRATION="${MIGRATION_FILES[${#MIGRATION_FILES[@]}-1]##*/}"
TARGET_MIGRATION_VERSION=$((10#${LATEST_MIGRATION%%_*}))

BASE="${ADC_BASE_URL:-http://127.0.0.1:18080}"          # 统一 API 网关（唯一业务入口）
DECIDE_BASE="${ADC_DECIDE_URL:-http://127.0.0.1:18082}" # adc 直连（仅 dev 决策注入）
if [ -z "${ADC_DEVICE_SECRET:-}" ]; then
  ADC_DEVICE_SECRET=$(docker exec adc-app cat /tmp/adc-demo-secret 2>/dev/null || echo "")
fi
[ -n "$ADC_DEVICE_SECRET" ] || { echo "无法获取设备密钥（容器未运行或未 seed）"; exit 1; }
PASS=0; FAIL=0; SKIP=0

step() { echo "--- $1"; }
ok()   { echo "  PASS: $1"; PASS=$((PASS+1)); }
bad()  { echo "  FAIL: $1"; FAIL=$((FAIL+1)); }
skip() { echo "  SKIP: $1"; SKIP=$((SKIP+1)); }

# 1. 健康检查
step "1. 网关健康检查"
if curl -sf "$BASE/healthz" | grep -q ok; then ok "healthz"; else bad "healthz"; fi

# 2. mock 设备接入（后台运行，经网关 WSS 透传）
step "2. mock 设备接入隧道"
go run ./ce/cmd/mockdevice -url "${BASE/http:/ws:}/v1/devices/tunnel" -device cnc-demo-01 -secret "$ADC_DEVICE_SECRET" >/tmp/adc-mock.log 2>&1 &
MOCK_PID=$!
trap 'kill $MOCK_PID 2>/dev/null || true' EXIT
sleep 2
if kill -0 $MOCK_PID 2>/dev/null; then ok "mock 设备在线（经网关 WSS 透传）"; else bad "mock 设备掉线: $(tail -3 /tmp/adc-mock.log)"; fi

# 3. Agent 查询工具列表
step "3. Agent tools/list"
TOOLS=$(curl -sf -H "X-ADC-Key: dev-agent-key" "$BASE/v1/agent/mcp/tools")
echo "$TOOLS" | grep -q "cnc-demo-01__set_spindle_speed" && ok "聚合工具可见" || bad "聚合工具缺失: $TOOLS"

# 4. 高危调用 → 202 + ticket
step "4. 高危调用触发 HITL"
RESP=$(curl -s -H "X-ADC-Key: dev-agent-key" -H "Content-Type: application/json" \
  -d '{"name":"cnc-demo-01::set_spindle_speed","arguments":{"rpm":3000}}' \
  "$BASE/v1/agent/mcp/tools/call")
echo "$RESP"
REQ_ID=$(echo "$RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('request_id',''))" 2>/dev/null || echo "")
TICKET_ID=$(echo "$RESP" | python3 -c "import json,sys; print(json.load(sys.stdin).get('ticket_id',''))" 2>/dev/null || echo "")
[ -n "$REQ_ID" ] && ok "返回 request_id=$REQ_ID" || bad "未返回 request_id"

# 5. 审批前轮询应保持 pending
step "5. 审批前轮询"
CODE=$(curl -s -o /dev/null -w '%{http_code}' -H "X-ADC-Key: dev-agent-key" "$BASE/v1/agent/mcp/tools/call/$REQ_ID")
[ "$CODE" = "202" ] && ok "pending 状态 202" || bad "期望 202 得到 $CODE"

# 6. 审批同意（dev 决策注入直连 adc）
step "6. 审批同意"
curl -sf -X POST -H "Content-Type: application/json" \
  -d "{\"ticket_id\":\"$TICKET_ID\",\"decision\":\"approve\"}" "$DECIDE_BASE/dev/decide" && ok "决策注入" || bad "决策注入失败"

# 7. 轮询直到执行完成
step "7. 轮询执行结果"
RESULT=""
for i in $(seq 1 20); do
  CODE=$(curl -s -o /tmp/adc-call-result.json -w '%{http_code}' -H "X-ADC-Key: dev-agent-key" "$BASE/v1/agent/mcp/tools/call/$REQ_ID")
  if [ "$CODE" = "200" ]; then RESULT=$(cat /tmp/adc-call-result.json); break; fi
  sleep 0.2
done
echo "$RESULT" | grep -q "set_spindle_speed" && ok "执行结果返回: $RESULT" || bad "未获得执行结果: $RESULT"

# 8. 拒绝路径验证（第二次调用 → 拒绝 → BLOCKED_BY_HITL）
step "8. 拒绝路径 BLOCKED_BY_HITL"
RESP2=$(curl -s -H "X-ADC-Key: dev-agent-key" -H "Content-Type: application/json" \
  -d '{"name":"cnc-demo-01::set_spindle_speed","arguments":{"rpm":9999}}' \
  "$BASE/v1/agent/mcp/tools/call")
REQ2=$(echo "$RESP2" | python3 -c "import json,sys; print(json.load(sys.stdin).get('request_id',''))" 2>/dev/null || echo "")
TICKET2=$(echo "$RESP2" | python3 -c "import json,sys; print(json.load(sys.stdin).get('ticket_id',''))" 2>/dev/null || echo "")
curl -sf -X POST -H "Content-Type: application/json" \
  -d "{\"ticket_id\":\"$TICKET2\",\"decision\":\"reject\"}" "$DECIDE_BASE/dev/decide" >/dev/null
sleep 0.5
BODY=$(curl -s -H "X-ADC-Key: dev-agent-key" "$BASE/v1/agent/mcp/tools/call/$REQ2")
echo "$BODY" | grep -q "BLOCKED_BY_HITL" && ok "拒绝后 BLOCKED_BY_HITL" || bad "拒绝路径失败: $BODY"

# 9. 鉴权失败（SEC-02）
step "9. 非法 API Key 拒绝"
CODE=$(curl -s -o /dev/null -w '%{http_code}' -H "X-ADC-Key: bad-key" "$BASE/v1/agent/mcp/tools")
[ "$CODE" = "401" ] && ok "401 拒绝" || bad "期望 401 得到 $CODE"

# 10. 审计落库验证（SEC-07）
step "10. 审计日志落库"
sleep 1
CNT=$(docker exec adc-postgres psql -U adc -d adc -t -c "SELECT count(*) FROM adc_audit_logs;" 2>/dev/null | tr -d ' ')
[ "$CNT" -ge 1 ] && ok "审计记录 $CNT 条" || bad "审计表为空"

# 11. 数据库迁移版本（V2.1 M8）
step "11. 数据库迁移版本"
MIGRATION_TABLE=$(docker exec adc-postgres psql -U "${ADC_PG_USER:-adc}" -d "${ADC_PG_DBNAME:-adc}" -Atc \
  "SELECT to_regclass('public.adc_schema_migrations') IS NOT NULL;" 2>/dev/null || true)
if [ "$MIGRATION_TABLE" = "t" ]; then
  MIGRATION_LEDGER=$(docker exec adc-postgres psql -U "${ADC_PG_USER:-adc}" -d "${ADC_PG_DBNAME:-adc}" -AtF '|' -c \
    "SELECT COUNT(*), COALESCE(MAX(version), 0), COALESCE(MIN(version), 0) FROM adc_schema_migrations;" 2>/dev/null || true)
  MIGRATION_COUNT=${MIGRATION_LEDGER%%|*}
  MIGRATION_REMAINDER=${MIGRATION_LEDGER#*|}
  MIGRATION_VERSION=${MIGRATION_REMAINDER%%|*}
  MIGRATION_MINIMUM=${MIGRATION_LEDGER##*|}
  if [ "${MIGRATION_MINIMUM:-0}" -eq 1 ] && \
     [ "${MIGRATION_COUNT:-0}" -eq "$TARGET_MIGRATION_VERSION" ] && \
     [ "${MIGRATION_VERSION:-0}" -eq "$TARGET_MIGRATION_VERSION" ]; then
    ok "迁移账本连续且已达目标版本 ${TARGET_MIGRATION_VERSION}（MIN=1，COUNT=MAX）"
  else
    bad "迁移账本异常：MIN=${MIGRATION_MINIMUM:-不可读} COUNT=${MIGRATION_COUNT:-不可读} MAX=${MIGRATION_VERSION:-不可读} TARGET=${TARGET_MIGRATION_VERSION}"
  fi
else
  bad "adc_schema_migrations 不存在，既有卷升级门禁未就绪"
fi

# 12. OIDC 配置状态：302 表示已启用，404 表示显式禁用，其他状态异常。
step "12. OIDC 配置状态"
OIDC_CODE=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/v1/admin/auth/oidc/start" || true)
case "$OIDC_CODE" in
  302) ok "OIDC 已启用，授权入口可重定向" ;;
  404) ok "OIDC 已显式禁用（开发环境允许）" ;;
  *) bad "OIDC 状态端点异常，HTTP $OIDC_CODE" ;;
esac

# 后续管理面专项检查复用演示管理员会话。
LOGIN=$(curl -sf -H "Content-Type: application/json" \
  -d "{\"username\":\"${ADC_SMOKE_ADMIN_USER:-admin}\",\"password\":\"${ADC_SMOKE_ADMIN_PASSWORD:-admin123!}\"}" \
  "$BASE/v1/admin/auth/login" || true)
ADMIN_TOKEN=$(echo "$LOGIN" | python3 -c "import json,sys; print(json.load(sys.stdin).get('token',''))" 2>/dev/null || true)
TENANT_ID=$(echo "$LOGIN" | python3 -c "import json,sys; print(json.load(sys.stdin).get('tenant_id',''))" 2>/dev/null || true)
if [ -n "$ADMIN_TOKEN" ] && [ -z "$TENANT_ID" ]; then
  TENANTS=$(curl -sf -H "Authorization: Bearer $ADMIN_TOKEN" "$BASE/v1/admin/tenants?page=1&page_size=1" || true)
  TENANT_ID=$(echo "$TENANTS" | python3 -c "import json,sys; items=json.load(sys.stdin).get('items',[]); print(items[0].get('id','') if items else '')" 2>/dev/null || true)
fi

# 13. 工具市场列表
step "13. 工具市场列表"
if [ -z "$ADMIN_TOKEN" ] || [ -z "$TENANT_ID" ]; then
  bad "缺少管理员会话或租户 ID，无法验证工具市场"
else
  MARKET=$(curl -sf -H "Authorization: Bearer $ADMIN_TOKEN" "$BASE/v1/admin/tool-packages?tenant_id=$TENANT_ID&page=1&page_size=20" || true)
  echo "$MARKET" | python3 -c "import json,sys; d=json.load(sys.stdin); assert isinstance(d.get('items'), list) and isinstance(d.get('total'), int)" 2>/dev/null \
    && ok "工具市场列表契约可用" || bad "工具市场列表异常: $MARKET"
fi

# 14. 预算状态
step "14. 租户预算状态"
if [ -z "$ADMIN_TOKEN" ] || [ -z "$TENANT_ID" ]; then
  bad "缺少管理员会话或租户 ID，无法验证预算状态"
else
  BUDGET=$(curl -sf -H "Authorization: Bearer $ADMIN_TOKEN" "$BASE/v1/admin/tenants/$TENANT_ID/budget-status" || true)
  echo "$BUDGET" | python3 -c "import json,sys; d=json.load(sys.stdin); assert d.get('status') in ('OK','WARN','EXCEEDED') and isinstance(d.get('usage_percent'), int)" 2>/dev/null \
    && ok "预算状态契约可用" || bad "预算状态异常: $BUDGET"
fi

# 15. 参数校验：运行态无法在不修改 seed 的前提下稳定注入 schema 边界，使用专门单测门禁。
step "15. 工具参数校验"
if (cd ce && go test ./internal/agentapi -run 'TestParamGuardRejects(MissingRequired|TypeError|RangeViolation)$' -count=1 >/dev/null); then
  skip "未构造运行态 schema 边界；参数缺失、类型和范围拒绝已由 agentapi 单测门禁验证"
else
  bad "参数校验单测门禁失败"
fi

echo
echo "========== SMOKE 结果: PASS=$PASS FAIL=$FAIL SKIP=$SKIP =========="
[ "$FAIL" -eq 0 ] || exit 1
