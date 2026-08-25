import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import ElementPlus from 'element-plus'
import { i18n } from '@/i18n'
import { api, ApiError } from '@/api/request'
import Login from './Login.vue'

// Keep the real ApiError/errorMessage chain but stub the network calls, and
// stub the router push so the redirect can be asserted without mounting the
// full app router.
const { pushMock } = vi.hoisted(() => ({ pushMock: vi.fn() }))

vi.mock('vue-router', () => ({
  useRouter: () => ({ push: pushMock }),
}))

vi.mock('@/api/request', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/request')>()
  return { ...actual, api: { ...actual.api, get: vi.fn(), post: vi.fn() } }
})

const getMock = vi.mocked(api.get)
const postMock = vi.mocked(api.post)

const loginResponse = {
  token: 'tok-login',
  user_id: 'u-1',
  display_name: '平台管理员',
  tenant_id: 't-1',
  role: 'platform_admin' as const,
  expires_at: '2026-08-15T16:00:00Z',
}

class ResizeObserverStub {
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
}

function mountLogin() {
  const pinia = createPinia()
  setActivePinia(pinia)
  return mount(Login, {
    global: { plugins: [pinia, i18n, ElementPlus] },
  })
}

// Element Plus validation renders async-validator errors on a real timer
// tick, so assertions on error text poll instead of relying on microtasks.
async function expectText(wrapper: ReturnType<typeof mountLogin>, text: string) {
  await vi.waitFor(() => expect(wrapper.text()).toContain(text))
}

async function expectNoText(wrapper: ReturnType<typeof mountLogin>, text: string) {
  await vi.waitFor(() => expect(wrapper.text()).not.toContain(text))
}

beforeEach(() => {
  localStorage.clear()
  pushMock.mockReset()
  postMock.mockReset()
	getMock.mockReset()
	getMock.mockResolvedValue({ enabled: false })
  if (typeof window.ResizeObserver === 'undefined') {
    Object.defineProperty(window, 'ResizeObserver', {
      configurable: true,
      writable: true,
      value: ResizeObserverStub,
    })
  }
  if (typeof window.matchMedia !== 'function') {
    Object.defineProperty(window, 'matchMedia', {
      configurable: true,
      writable: true,
      value: (query: string) => ({
        matches: false,
        media: query,
        onchange: null,
        addListener: () => {},
        removeListener: () => {},
        addEventListener: () => {},
        removeEventListener: () => {},
        dispatchEvent: () => false,
      }),
    })
  }
})

describe('Login', () => {
  it('renders the login title and form fields', () => {
    const wrapper = mountLogin()
    expect(wrapper.text()).toContain('登录 ADC')
    expect(wrapper.find('input[autocomplete="username"]').exists()).toBe(true)
    expect(wrapper.find('input[autocomplete="current-password"]').exists()).toBe(true)
  })

	it('enables SSO after the status probe reports OIDC available', async () => {
		getMock.mockResolvedValue({ enabled: true })
		const wrapper = mountLogin()
		await flushPromises()
		expect(getMock).toHaveBeenCalledWith('/v1/admin/auth/oidc/status')
		expect(wrapper.get('.sso').attributes('disabled')).toBeUndefined()
		expect(wrapper.get('.sso').attributes('href')).toBe('/v1/admin/auth/oidc/start')
	})

  it('shows validation errors and does not call the API on an empty submit', async () => {
    const wrapper = mountLogin()
    await wrapper.get('.submit').trigger('click')
    await expectText(wrapper, '请输入用户名')
    await expectText(wrapper, '请输入密码')
    expect(postMock).not.toHaveBeenCalled()
  })

  it('submits credentials, persists the session and redirects to /dashboard', async () => {
    postMock.mockResolvedValue(loginResponse)
    const wrapper = mountLogin()
    await wrapper.find('input[autocomplete="username"]').setValue('admin')
    await wrapper.find('input[autocomplete="current-password"]').setValue('secret')
    await wrapper.get('.submit').trigger('click')
    await flushPromises()
    expect(postMock).toHaveBeenCalledWith('/v1/admin/auth/login', {
      username: 'admin',
      password: 'secret',
    })
    expect(localStorage.getItem('adc_token')).toBe('tok-login')
    expect(JSON.parse(localStorage.getItem('adc_user') ?? 'null')).toMatchObject({
      userId: 'u-1',
      tenantId: 't-1',
    })
    expect(pushMock).toHaveBeenCalledWith('/dashboard')
  })

  it('renders the mapped server error when login fails', async () => {
    postMock.mockRejectedValue(new ApiError('10002', '用户名或密码错误', 401))
    const wrapper = mountLogin()
    await wrapper.find('input[autocomplete="username"]').setValue('admin')
    await wrapper.find('input[autocomplete="current-password"]').setValue('wrong')
    await wrapper.get('.submit').trigger('click')
    await expectText(wrapper, '用户名或密码错误')
    expect(localStorage.getItem('adc_token')).toBeNull()
    expect(pushMock).not.toHaveBeenCalled()
  })

  it('clears a previous error message on the next submit', async () => {
    postMock
      .mockRejectedValueOnce(new ApiError('10002', '用户名或密码错误', 401))
      .mockResolvedValueOnce(loginResponse)
    const wrapper = mountLogin()
    await wrapper.find('input[autocomplete="username"]').setValue('admin')
    await wrapper.find('input[autocomplete="current-password"]').setValue('wrong')
    await wrapper.get('.submit').trigger('click')
    await expectText(wrapper, '用户名或密码错误')
    await wrapper.find('input[autocomplete="current-password"]').setValue('secret')
    await wrapper.get('.submit').trigger('click')
    await flushPromises()
    await expectNoText(wrapper, '用户名或密码错误')
    expect(pushMock).toHaveBeenCalledWith('/dashboard')
  })
})
