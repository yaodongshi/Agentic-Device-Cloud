// Tool package marketplace client (design/83 C3.1/C3.2). The market
// surface mirrors the Admin API contract 1:1 (snake_case wire fields);
// packages are standard MCP tool bundles (core-sdk protocol.MCPTool)
// with a server-side signature for tamper detection.

import { api } from './request'
import type { Page, ToolRiskLevel } from './types'

export type ToolPackageScope = 'platform' | 'tenant'
export type ToolPackageStatus = 'PUBLISHED' | 'DEPRECATED'

/** One tool inside a package: standard MCP tool definition (design/83 C3.1). */
export interface MarketTool {
  name: string
  description: string
  input_schema: Record<string, unknown>
  risk_level?: ToolRiskLevel | null
  schema_version?: string
}

/** Market list row, GET /v1/admin/tool-packages. */
export interface ToolPackage {
  id: string
  name: string
  version: string
  description: string
  author: string
  scope: ToolPackageScope
  tool_count: number
  status: ToolPackageStatus
  installed: boolean
  installed_at?: string | null
  published_at: string
}

/** Detail / publish / install response: list row plus the tool array. */
export interface ToolPackageDetail extends ToolPackage {
  tools: MarketTool[]
}

/** Body of POST /v1/admin/tool-packages. */
export interface PublishToolPackagePayload {
  name: string
  version: string
  description: string
  author?: string
  tools: MarketTool[]
}

export async function fetchToolPackages(
  keyword = '',
  page = 1,
  pageSize = 20,
): Promise<Page<ToolPackage>> {
  return api.get<Page<ToolPackage>>('/v1/admin/tool-packages', {
    keyword,
    page,
    page_size: pageSize,
  })
}

export async function fetchToolPackageDetail(id: string): Promise<ToolPackageDetail> {
  return api.get<ToolPackageDetail>(`/v1/admin/tool-packages/${id}`)
}

export async function publishToolPackage(
  payload: PublishToolPackagePayload,
): Promise<ToolPackageDetail> {
  return api.post<ToolPackageDetail>('/v1/admin/tool-packages', payload)
}

export async function installToolPackage(id: string): Promise<ToolPackageDetail> {
  return api.post<ToolPackageDetail>(`/v1/admin/tool-packages/${id}/install`)
}

export async function uninstallToolPackage(id: string): Promise<ToolPackageDetail> {
  return api.post<ToolPackageDetail>(`/v1/admin/tool-packages/${id}/uninstall`)
}
