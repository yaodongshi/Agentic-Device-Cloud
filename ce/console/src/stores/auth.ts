import { defineStore } from 'pinia'
import type { Role } from '@/api/types'

// Session user mirror of the login response (design/33 3.1.1), persisted
// alongside the token so the layout can render the account and trim menus
// by role (F-03) without an extra round-trip.
export interface SessionUser {
  userId: string
  displayName: string
  tenantId: string
  role: Role
}

function readUser(): SessionUser | null {
  const raw = localStorage.getItem('adc_user')
  if (!raw) return null
  try {
    return JSON.parse(raw) as SessionUser
  } catch {
    return null
  }
}

export const useAuthStore = defineStore('auth', {
  state: () => ({
    token: localStorage.getItem('adc_token') ?? '',
    user: readUser(),
  }),
  actions: {
    setToken(token: string) {
      this.token = token
      localStorage.setItem('adc_token', token)
    },
    setUser(user: SessionUser) {
      this.user = user
      localStorage.setItem('adc_user', JSON.stringify(user))
    },
    clear() {
      this.token = ''
      this.user = null
      localStorage.removeItem('adc_token')
      localStorage.removeItem('adc_user')
    },
  },
})
