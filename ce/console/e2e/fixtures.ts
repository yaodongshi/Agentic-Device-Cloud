// Shared API fixtures for E2E route mocks. Field names mirror the
// Admin API wire contract (design/33, src/api/types.ts) so the mock
// responses exercise the same parsing paths as a real backend.
export const loginResponse = {
  token: 'e2e-token-abc123',
  user_id: 'u-admin-1',
  display_name: '平台管理员',
  tenant_id: 'tenant-cn-01',
  role: 'platform_admin',
  expires_at: '2026-09-15T00:00:00Z',
}

export const devicesPage = {
  items: [
    {
      device_id: 'dev-cnc-01',
      device_code: 'cnc-lathe-01',
      name: '一号车床',
      device_type: 'cnc',
      auth_type: 'token',
      status: 'online',
      last_heartbeat: '2026-08-15T08:00:00Z',
      sdk_version: '0.3.1',
      created_at: '2026-07-01T02:00:00Z',
    },
    {
      device_id: 'dev-plc-02',
      device_code: 'plc-line-02',
      name: '二号产线 PLC',
      device_type: 'plc',
      auth_type: 'hmac',
      status: 'offline',
      last_heartbeat: '2026-08-14T20:00:00Z',
      sdk_version: '0.3.0',
      created_at: '2026-07-02T02:00:00Z',
    },
  ],
  total: 2,
  page: 1,
  page_size: 20,
}

export const registeredDevice = {
  device_id: 'dev-test-03',
  device_code: 'TEST-CNC-01',
  name: '测试机床',
  device_type: 'cnc',
  auth_type: 'token',
  status: 'offline',
  last_heartbeat: null,
  created_at: '2026-08-15T09:00:00Z',
  credential: {
    secret: 'adc-sk-test-secret-123',
  },
}

export const toolsPage = {
  items: [
    {
      name: 'set_spindle_speed',
      description: '设置主轴转速',
      input_schema: { type: 'object', properties: { rpm: { type: 'number' } } },
      risk_level: 2,
      is_enabled: true,
      schema_version: 1,
      updated_at: '2026-08-10T03:00:00Z',
    },
    {
      name: 'get_status',
      description: '查询设备状态',
      input_schema: { type: 'object', properties: {} },
      risk_level: 0,
      is_enabled: true,
      schema_version: 1,
      updated_at: '2026-08-10T03:00:00Z',
    },
  ],
  total: 2,
  page: 1,
  page_size: 20,
}
