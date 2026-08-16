// Alert rule configuration and fired-alert history client (FR-017,
// design/82 B2). Both endpoints are admin-only on the backend (the RBAC
// matrix covers admin-tier paths), so the Monitor view gates the editor by
// role and degrades the lists to an error state for auditors.

import { api } from './request'

export type AlertSeverity = 'P1' | 'P2' | 'P3'

export type AlertOperator = '>' | '<' | '>='

/** Threshold alert rule, GET/PUT /v1/admin/alerts/rules (snake_case wire). */
export interface AlertRule {
  id: string
  name: string
  metric: string
  operator: AlertOperator
  threshold: number
  duration_sec: number
  severity: AlertSeverity
  enabled: boolean
  labels?: Record<string, string>
  created_at: string
  updated_at: string
}

/** Fired alert, GET /v1/admin/alerts/events. */
export interface AlertEvent {
  id: string
  rule_id: string
  rule_name: string
  metric: string
  operator: AlertOperator
  threshold: number
  observed: number
  severity: AlertSeverity
  fired_at: string
}

/** Body of PUT /v1/admin/alerts/rules (full replace + audit reason). */
export interface SaveAlertRulesPayload {
  rules: Array<Partial<AlertRule>>
  change_reason: string
}

interface AlertRulesResponse {
  rules: AlertRule[]
}

interface AlertEventsResponse {
  events: AlertEvent[]
}

/** Metric families a rule may target (mirrors the backend whitelist). */
export const ALERT_METRIC_OPTIONS: Array<{ name: string; hint: string }> = [
  { name: 'adc_device_online_total', hint: 'online device count (gauge, per tenant)' },
  { name: 'adc_agent_calls_total', hint: 'agent tool calls (counter)' },
  { name: 'adc_hitl_intercepted_total', hint: 'HITL intercepted calls (counter)' },
  { name: 'adc_hitl_approved_total', hint: 'HITL approved calls (counter)' },
  { name: 'adc_hitl_rejected_total', hint: 'HITL rejected calls (counter)' },
  { name: 'adc_http_requests_total', hint: 'HTTP requests (counter)' },
  { name: 'adc_hitl_notify_failures_total', hint: 'approval push failures (counter)' },
  { name: 'adc_pg_pool_connections', hint: 'PG pool state (gauge)' },
]

export async function fetchAlertRules(): Promise<AlertRule[]> {
  const res = await api.get<AlertRulesResponse>('/v1/admin/alerts/rules')
  return res.rules ?? []
}

export async function saveAlertRules(payload: SaveAlertRulesPayload): Promise<AlertRule[]> {
  const res = await api.put<AlertRulesResponse>('/v1/admin/alerts/rules', payload)
  return res.rules ?? []
}

export async function fetchAlertEvents(limit = 20): Promise<AlertEvent[]> {
  const res = await api.get<AlertEventsResponse>('/v1/admin/alerts/events', { limit })
  return res.events ?? []
}
