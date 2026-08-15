// Type definitions mirroring the Admin API contract in design/33.
// All Admin endpoints are reached through the unified gateway with the
// /v1/admin prefix (ADR-19, design/30); field names stay snake_case to
// match the wire format 1:1.

/** Admin API roles from design/33 1.2 (RBAC matrix). */
export type Role = 'platform_admin' | 'tenant_admin' | 'approver' | 'auditor'

/** Response of POST /v1/admin/auth/login (design/33 3.1.1 + B-01 session token). */
export interface LoginResponse {
  token: string
  user_id: string
  display_name: string
  tenant_id: string
  role: Role
  expires_at: string
}

/** Offset pagination envelope (design/33 1.6). */
export interface Page<T> {
  items: T[]
  total: number
  page: number
  page_size: number
}

export type DeviceStatus = 'online' | 'offline' | 'error' | 'frozen' | 'retired'

export type DeviceAuthType = 'token' | 'hmac' | 'mtls'

/** Device ledger row, GET /v1/admin/devices (design/33 3.1.7). */
export interface Device {
  device_id: string
  device_code: string
  name: string
  device_type: string
  auth_type: DeviceAuthType
  status: DeviceStatus
  last_heartbeat?: string | null
  sdk_version?: string
  metadata?: Record<string, string>
  created_at: string
}

/** Credential shown exactly once on issue/reset (NFR-004). */
export interface DeviceCredential {
  secret: string
  cert_url?: string
}

/** Body of POST /v1/admin/devices (design/33 3.1.6). */
export interface RegisterDevicePayload {
  device_code: string
  name: string
  device_type: string
  auth_type: DeviceAuthType
}

/** Response of POST /v1/admin/devices: device row plus the one-time credential. */
export interface RegisteredDevice extends Device {
  credential: DeviceCredential
}

/** `op` enum of PATCH /v1/admin/devices/{id} (design/33 3.1.8). */
export type DevicePatchOp =
  | 'freeze'
  | 'unfreeze'
  | 'revoke_credential'
  | 'reset_credential'
  | 'update_meta'

export interface DevicePatchPayload {
  op: DevicePatchOp
  change_reason: string
}

/** Response of PATCH /v1/admin/devices/{id}; reset_credential carries a new credential. */
export interface DevicePatchResponse {
  device_id: string
  device_code: string
  name?: string
  status?: DeviceStatus
  credential?: DeviceCredential
  updated_at: string
}

export type ApiKeyStatus = 'active' | 'revoked' | 'expired'

/** Tool permission scope (design/33 3.1.12). */
export interface ApiKeyScopes {
  allowed_tools: string[]
}

/** API Key list row, GET /v1/admin/agent-keys (design/33 3.1.13). */
export interface ApiKey {
  key_id: string
  name: string
  key_prefix: string
  scopes: ApiKeyScopes
  status: ApiKeyStatus
  last_used_at?: string | null
  expires_at?: string | null
  created_at: string
}

/** Response of POST /v1/admin/agent-keys: the full key appears exactly once. */
export interface CreatedApiKey {
  key_id: string
  name: string
  key: string
  key_prefix: string
  scopes: ApiKeyScopes
  status: ApiKeyStatus
  expires_at?: string | null
  created_at: string
}

/** Body of POST /v1/admin/agent-keys (design/33 3.1.12). */
export interface CreateApiKeyPayload {
  name: string
  scopes: ApiKeyScopes
  expires_at?: string | null
}

/** Tenant quota block (design/33 3.1.3). */
export interface TenantQuota {
  max_devices: number
  max_agent_keys: number
  monthly_call_limit: number
  audit_retention_days: number
}

/** Tenant row (design/33 3.1.2/3.1.3). */
export interface Tenant {
  tenant_id: string
  name: string
  code?: string
  status: 'active' | 'disabled'
  device_count?: number
  agent_key_count?: number
  quota?: TenantQuota
  created_at: string
}

/**
 * Tool risk level (FR-006): 0 read-only / 1 low / 2 high (HITL) /
 * 3 critical (physical loop). DB is authoritative over device reports
 * (design/32 3.6, SEC-09).
 */
export type ToolRiskLevel = 0 | 1 | 2 | 3

/** Device tool row, GET /v1/admin/devices/{id}/tools (design/33 3.1.10). */
export interface DeviceTool {
  name: string
  description: string
  input_schema: Record<string, unknown>
  risk_level: ToolRiskLevel
  is_enabled: boolean
  schema_version: number
  updated_at: string
}

/** One tool change in the PATCH /v1/admin/devices/{id}/tools batch (design/33 3.1.11). */
export interface ToolChange {
  name: string
  risk_level?: ToolRiskLevel
  is_enabled?: boolean
}

export interface ToolPatchPayload {
  changes: ToolChange[]
  change_reason: string
}

/** Response of PATCH /v1/admin/devices/{id}/tools. */
export interface ToolPatchResponse {
  changed: number
  failed: Array<{ name: string; reason: string }>
}

export type ApprovalTicketStatus = 'pending' | 'approved' | 'rejected' | 'expired'

/** Approval ticket row, GET /v1/admin/approval-tickets (design/33 3.1.15). */
export interface ApprovalTicket {
  ticket_id: string
  device_id: string
  device_code: string
  tool_name: string
  arguments: Record<string, unknown>
  risk_level: ToolRiskLevel
  status: ApprovalTicketStatus
  approver?: string | null
  comment?: string | null
  created_at: string
  expire_at: string
  resolved_at?: string | null
}

export type HitlDecision = 'approve' | 'reject'

/** Body of POST /v1/hitl/callback (design/33 3.4.1). */
export interface HitlCallbackPayload {
  ticket_id: string
  decision: HitlDecision
  approver: string
  comment?: string
}

/** Response of POST /v1/hitl/callback. */
export interface HitlCallbackResponse {
  ticket_id: string
  status: ApprovalTicketStatus
}

export type AuditLogStatus = 'success' | 'failed' | 'blocked_by_hitl'

/** Audit log row, GET /v1/admin/audit-logs (design/33 3.1.16). */
export interface AuditLog {
  log_id: string
  event_type: string
  trace_id: string
  agent_id?: string | null
  key_id?: string | null
  device_id?: string | null
  device_code?: string | null
  tool_name?: string | null
  request_params?: Record<string, unknown>
  response_payload?: Record<string, unknown>
  status: AuditLogStatus
  hitl_approver?: string | null
  hitl_comment?: string | null
  exempt_reason?: string | null
  execution_duration_ms?: number | null
  created_at: string
}

/** Cursor pagination envelope for audit logs (design/33 1.6). */
export interface AuditLogPage {
  items: AuditLog[]
  next_cursor: string | null
}

/** Summary returned by GET /v1/hitl/action for a pending ticket (landing page, F-11). */
export interface HitlActionTicket {
  ticket_id: string
  device_code?: string
  tool_name?: string
  risk_level?: ToolRiskLevel
  expire_at?: string
  status?: ApprovalTicketStatus
}
