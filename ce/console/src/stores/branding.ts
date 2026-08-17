import { defineStore } from 'pinia'
import { api } from '@/api/request'
import { i18n } from '@/i18n'

// White-label branding store (C2.1, design/83): the console title / logo /
// primary color come from GET /v1/admin/branding (a public endpoint, so
// the login page can brand before authentication). The response is
// applied at runtime: --adc-brand CSS variable override (tokens.css) plus
// document.title. An unconfigured platform answers is_default=true and
// the UI keeps the i18n app name; a failed fetch keeps the built-in
// defaults so branding can never block the console.

export interface BrandingConfig {
  title: string
  logo_url: string
  primary_color: string
  is_default: boolean
}

// Built-in platform defaults mirror tokens.css --adc-brand and
// index.html <title> (the GET failure fallback).
export const DEFAULT_BRANDING: BrandingConfig = {
  title: 'ADC Console',
  logo_url: '',
  primary_color: '#2563eb',
  is_default: true,
}

const BRAND_VAR = '--adc-brand'

const COLOR_RE = /^#[0-9a-fA-F]{6}$/

export const useBrandingStore = defineStore('branding', {
  state: () => ({
    config: { ...DEFAULT_BRANDING },
    loaded: false,
    failed: false,
  }),
  getters: {
    // A platform-default brand renders the i18n app name so the zh-CN /
    // en locale names survive; a configured brand shows its own title.
    displayTitle(state): string {
      if (state.config.is_default || !state.config.title) {
        return i18n.global.t('common.appName')
      }
      return state.config.title
    },
    logoUrl(state): string {
      return state.config.logo_url
    },
  },
  actions: {
    // apply pushes the current config into the document: the brand CSS
    // variable and the tab title. It is idempotent and cheap, so the
    // layout re-runs it after a locale switch.
    apply() {
      const color = COLOR_RE.test(this.config.primary_color)
        ? this.config.primary_color
        : DEFAULT_BRANDING.primary_color
      document.documentElement.style.setProperty(BRAND_VAR, color)
      document.title = this.displayTitle
    },
    // load fetches the brand config (tenant-scoped by the request layer
    // when a session user exists) and applies it. Failures fall back to
    // the built-in defaults and are recorded, never thrown.
    async load() {
      try {
        const res = await api.get<BrandingConfig>('/v1/admin/branding')
        if (res && COLOR_RE.test(res.primary_color ?? '')) {
          this.config = {
            title: res.title ?? DEFAULT_BRANDING.title,
            logo_url: res.logo_url ?? '',
            primary_color: res.primary_color,
            is_default: Boolean(res.is_default),
          }
        } else {
          this.config = { ...DEFAULT_BRANDING }
        }
        this.failed = false
      } catch {
        this.config = { ...DEFAULT_BRANDING }
        this.failed = true
      } finally {
        this.loaded = true
        this.apply()
      }
    },
  },
})
