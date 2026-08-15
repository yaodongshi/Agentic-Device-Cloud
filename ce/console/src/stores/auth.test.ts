import { beforeEach, describe, expect, it } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import { useAuthStore } from './auth'

const user = {
  userId: 'u-1',
  displayName: '平台管理员',
  tenantId: 't-1',
  role: 'platform_admin' as const,
}

beforeEach(() => {
  localStorage.clear()
  setActivePinia(createPinia())
})

describe('auth store', () => {
  it('setToken updates state and persists to localStorage', () => {
    const store = useAuthStore()
    store.setToken('tok-1')
    expect(store.token).toBe('tok-1')
    expect(localStorage.getItem('adc_token')).toBe('tok-1')
  })

  it('setUser updates state and persists the user as JSON', () => {
    const store = useAuthStore()
    store.setUser(user)
    expect(store.user).toEqual(user)
    expect(JSON.parse(localStorage.getItem('adc_user') ?? 'null')).toEqual(user)
  })

  it('clear resets state and removes both localStorage keys', () => {
    const store = useAuthStore()
    store.setToken('tok-1')
    store.setUser(user)
    store.clear()
    expect(store.token).toBe('')
    expect(store.user).toBeNull()
    expect(localStorage.getItem('adc_token')).toBeNull()
    expect(localStorage.getItem('adc_user')).toBeNull()
  })

  it('initializes state from localStorage on store creation', () => {
    localStorage.setItem('adc_token', 'tok-seeded')
    localStorage.setItem('adc_user', JSON.stringify(user))
    setActivePinia(createPinia())
    const store = useAuthStore()
    expect(store.token).toBe('tok-seeded')
    expect(store.user).toEqual(user)
  })

  it('starts empty when localStorage holds nothing', () => {
    const store = useAuthStore()
    expect(store.token).toBe('')
    expect(store.user).toBeNull()
  })

  it('falls back to a null user when the stored JSON is corrupted', () => {
    localStorage.setItem('adc_token', 'tok')
    localStorage.setItem('adc_user', '{bad json')
    setActivePinia(createPinia())
    const store = useAuthStore()
    expect(store.token).toBe('tok')
    expect(store.user).toBeNull()
  })
})
