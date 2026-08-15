#!/usr/bin/env bash
# ADC 全链路 smoke 测试（容器版）：经统一网关（gateway:18080）走全部业务流量，
# dev 决策注入直连 adc 容器端口 18082；mock 设备 WSS 经网关透传。
# 前置：deploy/compose.yaml 已启动（docker compose -f deploy/compose.yaml up -d --build）
set -euo pipefail

BASE="${ADC_BASE_URL:-http://127.0.0.1:18080}"          # 统一 API 网关（唯一业务入口）
DECIDE_BASE="${ADC_DECIDE_URL:-http://127.0.0.1:18082}" # adc 直连（仅 dev 决策注入）
if [ -z "${ADC_DEVICE_SECRET:-}" ]; then
  ADC_DEVICE_SECRET=$(docker exec adc-app cat /tmp/adc-demo-secret 2>/dev/null || echo "")
fi
[ -n "$ADC_DEVICE_SECRET" ] || { echo "无法获取设备密钥（容器未运行或未 seed）"; exit 1; }
PASS=0; FAIL=0

step() { echo "--- $1"; }
ok()   { echo "  PASS: $1"; PASS=$((PASS+1)); }
bad()  { echo "  FAIL: $1"; FAIL=$((FAIL+1)); }

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

echo
echo "========== SMOKE 结果: PASS=$PASS FAIL=$FAIL =========="
[ "$FAIL" -eq 0 ] || exit 1
