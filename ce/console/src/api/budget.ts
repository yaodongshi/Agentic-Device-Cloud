// C5.1 tenant budget client (design/83): the monthly budget line
// (metadata.budget_monthly_cents) versus the current month's metered
// fee. Amounts travel as integer fen (1 yuan = 100 fen); fenToYuan in
// billing.ts is the single conversion point.

import { api } from './request'

export type BudgetStatusKind = 'OK' | 'WARN' | 'EXCEEDED'

/** Response of GET /v1/admin/tenants/{id}/budget-status. */
export interface BudgetStatus {
  tenant_id: string
  period: string
  budget_monthly_cents: number
  billed_fen: number
  usage_percent: number
  status: BudgetStatusKind
}

/** Response of PUT /v1/admin/tenants/{id}/budget. */
export interface BudgetLine {
  tenant_id: string
  budget_monthly_cents: number
}

export async function fetchBudgetStatus(tenantId: string): Promise<BudgetStatus> {
  return api.get<BudgetStatus>(`/v1/admin/tenants/${tenantId}/budget-status`)
}

export async function putBudgetLine(
  tenantId: string,
  budgetMonthlyCents: number,
  changeReason: string,
): Promise<BudgetLine> {
  return api.put<BudgetLine>(`/v1/admin/tenants/${tenantId}/budget`, {
    budget_monthly_cents: budgetMonthlyCents,
    change_reason: changeReason,
  })
}
