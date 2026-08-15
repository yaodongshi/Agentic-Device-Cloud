import { expect, test } from '@playwright/test'
import { devicesPage, registeredDevice, toolsPage } from './fixtures'

// Seed a platform_admin session before every test: the router guard reads
// the token from localStorage, so requests to the console pages must be
// authenticated up front. All API traffic is intercepted with page.route.
const sessionUser = {
  userId: 'u-admin-1',
  displayName: '平台管理员',
  tenantId: 'tenant-cn-01',
  role: 'platform_admin',
}

test.beforeEach(async ({ page }) => {
  await page.addInitScript(
    (init: { token: string; user: string }) => {
      localStorage.setItem('adc_token', init.token)
      localStorage.setItem('adc_user', init.user)
    },
    { token: 'e2e-token-abc123', user: JSON.stringify(sessionUser) },
  )
})

// One handler for the whole /v1/admin/devices tree; the console hits the
// collection endpoint (list + register) and the per-device tools endpoint.
async function mockDevicesApi(page: import('@playwright/test').Page) {
  let lastToolPatch: { headers: Record<string, string>; body: unknown } | null = null
  await page.route(/\/v1\/admin\/devices($|[/?])/, async (route) => {
    const url = new URL(route.request().url())
    const method = route.request().method()
    if (url.pathname === '/v1/admin/devices' && method === 'GET') {
      return route.fulfill({ json: devicesPage })
    }
    if (url.pathname === '/v1/admin/devices' && method === 'POST') {
      return route.fulfill({ json: registeredDevice })
    }
    if (url.pathname === '/v1/admin/devices/dev-cnc-01/tools') {
      if (method === 'GET') return route.fulfill({ json: toolsPage })
      if (method === 'PATCH') {
        lastToolPatch = {
          headers: route.request().headers(),
          body: route.request().postDataJSON(),
        }
        return route.fulfill({ json: { changed: 1, failed: [] } })
      }
    }
    return route.fulfill({ status: 404, json: { code: '10004', message: 'not found' } })
  })
  return () => lastToolPatch
}

test.describe('devices', () => {
  test('renders the device list from the mocked API', async ({ page }) => {
    await mockDevicesApi(page)
    await page.goto('/devices')
    // Scope to table rows: filter dropdown options reuse the same labels.
    const rowCnc = page.locator('tr', { hasText: 'cnc-lathe-01' })
    await expect(rowCnc.getByText('一号车床')).toBeVisible()
    await expect(rowCnc.getByText('在线')).toBeVisible()
    const rowPlc = page.locator('tr', { hasText: 'plc-line-02' })
    await expect(rowPlc.getByText('二号产线 PLC')).toBeVisible()
    await expect(rowPlc.getByText('离线')).toBeVisible()
    await expect(rowCnc.getByText('0.3.1')).toBeVisible()
  })

  test('register dialog creates a device and shows the one-time credential', async ({ page }) => {
    await mockDevicesApi(page)
    await page.goto('/devices')
    await page.getByRole('button', { name: '注册设备' }).first().click()

    const dialog = page.locator('.el-dialog', { hasText: '注册设备' })
    await expect(dialog).toBeVisible()

    await dialog.locator('.el-form-item', { hasText: '设备码' }).locator('input').fill('TEST-CNC-01')
    await dialog.locator('.el-form-item', { hasText: '设备名称' }).locator('input').fill('测试机床')

    // Pick a device type from the select (auth_type stays on the token default).
    await dialog.locator('.el-form-item', { hasText: '设备类型' }).locator('.el-select').click()
    await page.locator('.el-select-dropdown__item:visible', { hasText: 'cnc' }).last().click()

    await dialog.getByRole('button', { name: '注册设备' }).click()

    // The one-time credential dialog must render before it can be closed.
    const credentialDialog = page.locator('.el-dialog', { hasText: '设备凭证（仅显示一次）' })
    await expect(credentialDialog).toBeVisible()
    await expect(credentialDialog.locator('.credential-box input')).toHaveValue('adc-sk-test-secret-123')

    const closeButton = credentialDialog.getByRole('button', { name: '关闭' })
    await expect(closeButton).toBeDisabled()
    await credentialDialog.getByText('我已复制并妥善保存凭证').click()
    await expect(closeButton).toBeEnabled()
    await closeButton.click()
    await expect(credentialDialog).toBeHidden()
  })

  test('risk level downgrade sends the double-confirm header and audited reason', async ({ page }) => {
    const getLastPatch = await mockDevicesApi(page)
    await page.goto('/tools')

    // Remote device selector feeds the tool catalog.
    await page.locator('.device-select').click()
    await page.locator('.el-select-dropdown__item:visible', { hasText: 'cnc-lathe-01 / 一号车床' }).click()
    await expect(page.getByText('set_spindle_speed')).toBeVisible()

    // Open the level editor for the level-2 tool.
    await page.locator('tr', { hasText: 'set_spindle_speed' }).getByText('修改等级').click()
    const editDialog = page.locator('.el-dialog', { hasText: '修改风险等级' })
    await expect(editDialog).toBeVisible()
    await expect(editDialog.locator('.risk-tag')).toHaveText('2 级（高危）')

    // Downgrade 2 -> 0: warning + mandatory 10-200 char reason appear.
    await editDialog.locator('.el-select').click()
    await page.locator('.el-select-dropdown__item:visible', { hasText: '0 级（只读）' }).last().click()
    await expect(editDialog.getByText('等级下调后，该工具调用将不再触发 HITL 人工审批，直接执行，请谨慎操作。')).toBeVisible()
    await editDialog.locator('textarea').fill('生产排产已迁移至新方案，该工具降级为只读配置')

    await editDialog.getByRole('button', { name: '确认下调' }).click()

    await expect(editDialog).toBeHidden()
    const patch = getLastPatch()
    expect(patch).not.toBeNull()
    expect(patch?.headers['x-adc-confirm']).toBe('true')
    expect(patch?.body).toMatchObject({
      changes: [{ name: 'set_spindle_speed', risk_level: 0 }],
      change_reason: '生产排产已迁移至新方案，该工具降级为只读配置',
    })
  })
})
