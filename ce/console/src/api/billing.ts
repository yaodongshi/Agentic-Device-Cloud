// Billing client (FR-016, design/82 B3.2). Amounts travel as integer fen
// (1 yuan = 100 fen) so the console renders money without floating point
// drift; fenToYuan is the single conversion point.

import { api } from './request'
import type { Page } from './types'

export type BillingStatus = 'GENERATED' | 'PAID' | 'OVERDUE'

/** Device ladder breakdown (doc/04: 20 included, 400/300/250 yuan tiers). */
export interface DeviceBreakdown {
  included_devices: number
  tier_400_devices: number
  tier_300_devices: number
  tier_250_devices: number
  annual_device_fee_fen: number
  monthly_device_fee_fen: number
}

/** Monthly bill row, GET /v1/admin/billing/statements. */
export interface BillingStatement {
  id: string
  tenant_id: string
  period: string
  device_peak: number
  tool_calls: number
  tokens: number
  subscription_fee_fen: number
  device_fee_fen: number
  token_fee_fen: number
  total_fee_fen: number
  status: BillingStatus
  paid_at?: string | null
  breakdown: DeviceBreakdown
  created_at: string
  updated_at: string
}

/** Audit reconciliation assertion, GET /v1/admin/billing/statements/{id}. */
export interface BillingReconciliation {
  period: string
  tool_calls_usage: number
  tool_calls_audit: number
  matched: boolean
}

/** Response of GET /v1/admin/billing/statements/{id}. */
export interface BillingDetail {
  statement: BillingStatement
  reconciliation: BillingReconciliation | null
}

/** Response of POST /v1/admin/billing/generate (idempotent). */
export interface BillingGenerateResult {
  statement: BillingStatement
  created: boolean
}

export async function fetchStatements(
  tenantId: string,
  page = 1,
  pageSize = 20,
): Promise<Page<BillingStatement>> {
  return api.get<Page<BillingStatement>>('/v1/admin/billing/statements', {
    tenant_id: tenantId,
    page,
    page_size: pageSize,
  })
}

export async function generateStatement(
  tenantId: string,
  period: string,
): Promise<BillingGenerateResult> {
  return api.post<BillingGenerateResult>('/v1/admin/billing/generate', {
    tenant_id: tenantId,
    period,
  })
}

export async function fetchStatementDetail(id: string): Promise<BillingDetail> {
  return api.get<BillingDetail>(`/v1/admin/billing/statements/${id}`)
}

/** Convert integer fen to a two-decimal yuan string, e.g. 331667 -> "3316.67". */
export function fenToYuan(fen: number): string {
  return (fen / 100).toFixed(2)
}
