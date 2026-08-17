import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import { api } from '@/api/request'
import { useBrandingStore, DEFAULT_BRANDING } from './branding'

vi.mock('@/api/request', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/request')>()
  return { ...actual, api: { ...actual.api, get: vi.fn() } }
})

const getMock = vi.mocked(api.get)

beforeEach(() => {
  localStorage.clear()
  setActivePinia(createPinia())
  getMock.mockReset()
  // Branding writes the CSS variable onto the root element; reset it so
  // tests start from a clean document.
  document.documentElement.style.removeProperty('--adc-brand')
  document.title = ''
})

describe('branding store', () => {
  it('starts with the platform defaults before loading', () => {
    const store = useBrandingStore()
    expect(store.config).toEqual(DEFAULT_BRANDING)
    expect(store.loaded).toBe(false)
  })

  it('load applies the configured brand: CSS variable and document title', async () => {
    getMock.mockResolvedValue({
      title: 'Acme Console',
      logo_url: 'https://cdn.acme.test/logo.png',
      primary_color: '#1d4ed8',
      is_default: false,
    })
    const store = useBrandingStore()
    await store.load()
    expect(getMock).toHaveBeenCalledWith('/v1/admin/branding')
    expect(document.documentElement.style.getPropertyValue('--adc-brand')).toBe('#1d4ed8')
    expect(document.title).toBe('Acme Console')
    expect(store.logoUrl).toBe('https://cdn.acme.test/logo.png')
    expect(store.failed).toBe(false)
  })

  it('renders the i18n app name when the platform default is returned', async () => {
    getMock.mockResolvedValue({
      title: 'ADC Console',
      logo_url: '',
      primary_color: '#2563eb',
      is_default: true,
    })
    const store = useBrandingStore()
    await store.load()
    expect(document.title).toBe('ADC 控制台')
    expect(document.documentElement.style.getPropertyValue('--adc-brand')).toBe('#2563eb')
  })

  it('falls back to defaults when the fetch fails', async () => {
    getMock.mockRejectedValue(new Error('network down'))
    const store = useBrandingStore()
    await store.load()
    expect(store.config).toEqual(DEFAULT_BRANDING)
    expect(store.failed).toBe(true)
    expect(store.loaded).toBe(true)
    expect(document.title).toBe('ADC 控制台')
    expect(document.documentElement.style.getPropertyValue('--adc-brand')).toBe('#2563eb')
  })

  it('ignores a malformed primary_color from the server', async () => {
    getMock.mockResolvedValue({
      title: 'Evil Console',
      logo_url: '',
      primary_color: 'red; background: url(x)',
      is_default: false,
    })
    const store = useBrandingStore()
    await store.load()
    // Defense in depth: a config that fails the client-side color check
    // falls back to the built-in defaults wholesale.
    expect(document.documentElement.style.getPropertyValue('--adc-brand')).toBe('#2563eb')
    expect(document.title).toBe('ADC 控制台')
  })

  it('apply can be re-run to refresh the title after a locale switch', async () => {
    getMock.mockResolvedValue({ ...DEFAULT_BRANDING })
    const store = useBrandingStore()
    await store.load()
    expect(document.title).toBe('ADC 控制台')
    store.apply()
    expect(document.title).toBe('ADC 控制台')
  })
})
