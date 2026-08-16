import { expect, test } from '@playwright/test'
import type { Page } from '@playwright/test'
import { devicesPage, loginResponse } from './fixtures'

// Login page + session flow (design/20 4.2): rendering, client-side
// validation and the happy-path login are covered. The backend is mocked
// with page.route so the suite is self-contained.
// The dashboard is the post-login landing page (f2cce63), so the happy
// paths assert /dashboard and mock the dashboard's three list endpoints.
function mockDashboardRoutes(page: Page) {
  const emptyPage = { items: [], total: 0, page: 1, page_size: 20 }
  void page.route('**/v1/admin/devices**', (route) => route.fulfill({ json: devicesPage }))
  void page.route('**/v1/admin/approval-tickets**', (route) => route.fulfill({ json: emptyPage }))
  void page.route('**/v1/admin/audit-logs**', (route) => route.fulfill({ json: emptyPage }))
}

test.describe('auth', () => {
  test('login page renders title, form and submit button', async ({ page }) => {
    await page.goto('/login')
    await expect(page.locator('h2')).toHaveText('登录 ADC')
    await expect(page.locator('input[autocomplete="username"]')).toBeVisible()
    await expect(page.locator('input[autocomplete="current-password"]')).toBeVisible()
    await expect(page.getByRole('button', { name: '登录' })).toBeVisible()
  })

  test('empty form shows validation errors and sends no login request', async ({ page }) => {
    let loginCalls = 0
    await page.route('**/v1/admin/auth/login', (route) => {
      loginCalls += 1
      return route.fulfill({ status: 400, json: { code: '10002', message: '用户名或密码错误' } })
    })
    await page.goto('/login')
    await page.getByRole('button', { name: '登录' }).click()
    await expect(page.getByText('请输入用户名')).toBeVisible()
    await expect(page.getByText('请输入密码')).toBeVisible()
    await expect(page).toHaveURL(/\/login/)
    expect(loginCalls).toBe(0)
  })

  test('wrong credentials render the mapped server error and stay on /login', async ({ page }) => {
    await page.route('**/v1/admin/auth/login', (route) =>
      route.fulfill({ status: 401, json: { code: '10002', message: '用户名或密码错误', trace_id: 'tr-401' } }),
    )
    await page.goto('/login')
    await page.locator('input[autocomplete="username"]').fill('admin')
    await page.locator('input[autocomplete="current-password"]').fill('wrong')
    await page.getByRole('button', { name: '登录' }).click()
    await expect(page.getByText('用户名或密码错误')).toBeVisible()
    await expect(page).toHaveURL(/\/login/)
    expect(await page.evaluate(() => localStorage.getItem('adc_token'))).toBeNull()
  })

  test('successful login stores the session and redirects to /dashboard', async ({ page }) => {
    await page.route('**/v1/admin/auth/login', (route) => route.fulfill({ json: loginResponse }))
    // The dashboard fires its list requests right after the redirect.
    mockDashboardRoutes(page)
    await page.goto('/login')
    await page.locator('input[autocomplete="username"]').fill('admin')
    await page.locator('input[autocomplete="current-password"]').fill('secret')
    await page.getByRole('button', { name: '登录' }).click()
    await expect(page).toHaveURL(/\/dashboard/)
    const token = await page.evaluate(() => localStorage.getItem('adc_token'))
    expect(token).toBe(loginResponse.token)
    const user = await page.evaluate(() => JSON.parse(localStorage.getItem('adc_user') ?? 'null'))
    expect(user).toMatchObject({ userId: 'u-admin-1', role: 'platform_admin' })
  })

  test('authenticated user visiting /login is bounced to /dashboard', async ({ page }) => {
    await page.addInitScript((init: { token: string; user: string }) => {
      localStorage.setItem('adc_token', init.token)
      localStorage.setItem('adc_user', init.user)
    }, {
      token: loginResponse.token,
      user: JSON.stringify({
        userId: 'u-admin-1',
        displayName: '平台管理员',
        tenantId: 'tenant-cn-01',
        role: 'platform_admin',
      }),
    })
    mockDashboardRoutes(page)
    await page.goto('/login')
    await expect(page).toHaveURL(/\/dashboard/)
  })
})
