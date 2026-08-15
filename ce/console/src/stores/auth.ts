import { defineStore } from 'pinia'

// Auth store: token persistence is a placeholder until the Admin API login
// endpoint (B-01) is wired in Sprint 4.
export const useAuthStore = defineStore('auth', {
  state: () => ({
    token: localStorage.getItem('adc_token') ?? '',
  }),
  actions: {
    setToken(token: string) {
      this.token = token
      localStorage.setItem('adc_token', token)
    },
    clear() {
      this.token = ''
      localStorage.removeItem('adc_token')
    },
  },
})
