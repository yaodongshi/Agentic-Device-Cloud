# ADC 试点周报模板（A1.4）

> 用途：种子工厂试点期间每周向客户与内部管理层同步运行数据（design/82 A1.4）
> 使用方式：复制本模板填写；所有指标数据来源见附录 A；每项指标标注原始数据出处，不写无来源数字
> 试点目标基线（design/40、design/50）：设备在线率不低于 99%；HITL 拦截率 100%；免审任务成功率不低于 99%；指令下发 P95 不超过 500ms；审批时效 P95 不超过 5 分钟（观测项）

---

## 试点周报（第 __ 周）

| 项 | 内容 |
|---|---|
| 报告周期 | 2026-__-__ 至 2026-__-__ |
| 试点客户 | （客户名称 / 工厂代号） |
| 报告人 / 审核人 | |
| 数据截止时间 | 周报周期最后一日 24:00（Asia/Shanghai） |

## 一、核心指标总览

| 指标 | 目标 | 本周实测 | 上周 | 达标 | 数据来源 |
|---|---|---|---|---|---|
| 设备在线率 | 不低于 99% | __% | __% | 是/否 | /metrics 指标 `adc_device_online_total` ÷ 台账注册设备数 |
| Agent 调用量（周累计） | — | __ 次 | __ 次 | — | /metrics 指标 `adc_agent_calls_total` 周增量 |
| HITL 拦截率 | 100% | __% | __% | 是/否 | 等级 2+ 调用数对比 `adc_hitl_intercepted_total` |
| 免审任务成功率 | 不低于 99% | __% | __% | 是/否 | 审计查询 `status=success` ÷ 免审调用总数（附录 A.2） |
| 指令下发时延 P95 | 不超过 500ms | __ms | __ms | 是/否 | /metrics 直方图 `adc_tool_call_duration_seconds` 分位 |
| 审批时效 P95 | 不超过 5 分钟（观测） | __min | __min | — | 工单查询端点 decided_at - created_at（附录 A.3） |
| 审批卡片送达 | 不超过 30 秒 | __s | __s | 是/否 | 工单 created_at 与群机器人接收时间人工记录 |
| 通知推送失败数 | 0 | __ 次 | __ 次 | 是/否 | /metrics 指标 `adc_hitl_notify_failures_total` |

指标口径说明：

- 设备在线率 = `adc_device_online_total` 当前值 ÷ 控制台设备台账已注册设备数。
- HITL 拦截率 = 风险等级 2 及以上调用的拦截占比；任一高危调用未生成工单即不达标，须按 P0 上报。
- 免审任务成功率 = 免审（风险等级 0/1 或命中免审策略）调用中 status=success 的占比。

## 二、异常工单清单

| 工单号 | 设备 | 工具 | 状态 | 创建时间 | 决策时间 | 审批人 | 异常说明 | 处置状态 |
|---|---|---|---|---|---|---|---|---|
| | | | | | | | | |

说明：列出本周全部 EXPIRED 工单、推送失败工单、审批耗时超过 5 分钟的工单；无异常写"无"。工单清单来源：GET /v1/admin/approval-tickets（附录 A.3）。

## 三、本周问题与风险

| # | 问题描述 | 影响 | 根因分析 | 处理人 | 计划关闭时间 |
|---|---|---|---|---|---|
| 1 | | | | | |
| 2 | | | | | |

## 四、下周计划

| # | 事项 | 负责人 | 完成标志 |
|---|---|---|---|
| 1 | | | |
| 2 | | | |

---

## 附录 A：数据来源与采集方法

### A.1 /metrics 指标（Prometheus 格式）

网关指标端点（仅回环）：`http://127.0.0.1:19091/metrics`；经 Prometheus 采集后也可用 PromQL 查询（`http://127.0.0.1:19090`）或 Grafana ADC Overview 大盘（`http://127.0.0.1:19300`）。

与试点指标对应的指标名：

| 指标名 | 类型 | 含义 |
|---|---|---|
| adc_device_online_total | Gauge | 当前在线设备数 |
| adc_agent_calls_total | Counter | Agent 调用累计次数 |
| adc_hitl_intercepted_total | Counter | HITL 拦截累计次数 |
| adc_hitl_approved_total / adc_hitl_rejected_total | Counter | 审批通过 / 拒绝累计次数 |
| adc_tool_call_duration_seconds | Histogram | 工具调用端到端时延分布 |
| adc_hitl_notify_failures_total | Counter | 通知推送失败累计次数 |
| adc_http_requests_total | Counter | 服务请求量（按 service/code 标签） |
| adc_http_request_duration_seconds | Histogram | 服务请求时延分布 |
| adc_pg_pool_connections | Gauge | 数据库连接池水位 |

采集示例：

```bash
# 直读网关指标
curl -sf http://127.0.0.1:19091/metrics | grep -E '^adc_(device_online|agent_calls|hitl_intercepted|hitl_notify)'

# 经 Prometheus 查询在线设备数
curl -sf 'http://127.0.0.1:19090/api/v1/query?query=adc_device_online_total' | python3 -m json.tool
```

### A.2 审计查询与导出端点

- 在线查询：`GET /v1/admin/audit-logs`，游标分页，需携带管理员会话（浏览器登录控制台后同源请求，或带 Bearer Token）。
- CSV 导出：`POST /v1/admin/audit-logs/export`，条件放 JSON 请求体，同步返回 CSV。
- 过滤参数：`time_from`、`time_to`（RFC3339）、`device_code`、`tool_name`、`status`（success / failed / blocked_by_hitl）、`event_type`、`agent_id`、`keyword`。
- 返回字段含：device_code、tool_name、risk_level、status、hitl_ticket_id、hitl_approver、hitl_comment、request_params、response_payload、execution_duration_ms、request_id、created_at。

采集示例：

```bash
# 免审成功率：拉取本周免审调用并统计 status（经网关业务入口）
curl -s -H "Authorization: Bearer <admin-token>" \
  -G 'http://127.0.0.1:18080/v1/admin/audit-logs' \
  --data-urlencode 'time_from=2026-08-10T00:00:00+08:00' \
  --data-urlencode 'time_to=2026-08-16T23:59:59+08:00' \
  --data-urlencode 'status=success'
```

### A.3 工单查询端点（审批时效数据）

- 端点：`GET /v1/admin/approval-tickets`，参数 `status`（PENDING / APPROVED / REJECTED / EXPIRED）、`page`、`page_size`（1-100）。
- 返回字段含：tool_name、risk_level、status、approver_name、comment、decided_at、expires_at、created_at。
- 审批时效 = decided_at - created_at（APPROVED/REJECTED 工单），P95 取本周全部已决策工单的第 95 百分位。
- 异常工单清单：status=EXPIRED 全量 + 时效超 5 分钟工单。

采集示例：

```bash
curl -s -H "Authorization: Bearer <admin-token>" \
  'http://127.0.0.1:18080/v1/admin/approval-tickets?status=EXPIRED&page_size=100' | python3 -m json.tool
```

### A.4 人工记录项

审批卡片送达时长（工单创建到群内可见）与审批人在群内点卡的反馈，由试点值守人按工单号人工记录，周报归档到异常工单清单同一张表。

## 附录 B：周报提交与归档

- 每周一 12:00 前发出上一周周报；数据导出 CSV/JSON 与周报一并归档（审计留存要求，NFR-006）。
- 任一核心指标连续两周不达标，或 HITL 拦截率任一周低于 100%，触发试点升级评审（周报抄送项目组与客户 IT 负责人）。
