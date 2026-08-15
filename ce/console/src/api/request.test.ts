import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, api, download, request } from './request'

// fetch is mocked through vi.stubGlobal; window.location is replaced with a
// plain object so the 401 redirect can be asserted instead of triggering a
// jsdom navigation error.
const fetchMock = vi.fn()

function jsonResponse(body: unknown, status = 200, statusText = 'OK') {
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText,
    text: vi.fn().mockResolvedValue(JSON.stringify(body)),
  }
}

function stubLocation(pathname: string) {
  const loc = { href: '', pathname }
  vi.stubGlobal('location', loc)
  return loc
}

beforeEach(() => {
  vi.stubGlobal('fetch', fetchMock)
  fetchMock.mockReset()
  localStorage.clear()
})

afterEach(() => {
  vi.unstubAllGlobals()
  localStorage.clear()
})

describe('request', () => {
  it('parses a 200 JSON body', async () => {
    fetchMock.mockResolvedValue(jsonResponse({ items: [], total: 0 }))
    await expect(request('/v1/admin/devices')).resolves.toEqual({ items: [], total: 0 })
    expect(fetchMock).toHaveBeenCalledWith('/v1/admin/devices', expect.anything())
  })

  it('returns undefined for a 200 empty body', async () => {
    const res = { ok: true, status: 200, text: vi.fn().mockResolvedValue('') }
    fetchMock.mockResolvedValue(res)
    await expect(request('/v1/admin/auth/logout', { method: 'POST' })).resolves.toBeUndefined()
  })

  it('clears token and redirects to /login on 401 outside the login page', async () => {
    localStorage.setItem('adc_token', 'tok')
    localStorage.setItem('adc_user', JSON.stringify({ userId: 'u' }))
    const loc = stubLocation('/devices')
    fetchMock.mockResolvedValue(
      jsonResponse({ code: '10002', message: 'unauthenticated', trace_id: 'tr-1' }, 401, 'Unauthorized'),
    )
    await expect(request('/v1/admin/devices')).rejects.toMatchObject({
      code: '10002',
      status: 401,
      traceId: 'tr-1',
    })
    expect(localStorage.getItem('adc_token')).toBeNull()
    expect(localStorage.getItem('adc_user')).toBeNull()
    expect(loc.href).toBe('/login')
  })

  it('does not redirect on 401 when already on the login page', async () => {
    localStorage.setItem('adc_token', 'tok')
    const loc = stubLocation('/login')
    fetchMock.mockResolvedValue(jsonResponse({ code: '10002', message: 'bad credentials' }, 401))
    await expect(request('/v1/admin/auth/login', { method: 'POST', body: '{}' })).rejects.toMatchObject({
      status: 401,
    })
    expect(loc.href).toBe('')
    expect(localStorage.getItem('adc_token')).toBe('tok')
  })

  it('maps a server error body into ApiError with code/message/status/traceId', async () => {
    fetchMock.mockResolvedValue(
      jsonResponse({ code: '11008', message: '设备码已存在', trace_id: 'tr-2' }, 409, 'Conflict'),
    )
    await expect(
      request('/v1/admin/devices', { method: 'POST', body: JSON.stringify({}) }),
    ).rejects.toMatchObject({ code: '11008', message: '设备码已存在', status: 409, traceId: 'tr-2' })
  })

  it('falls back to status code/message when the error body is missing fields', async () => {
    fetchMock.mockResolvedValue(jsonResponse({}, 502, 'Bad Gateway'))
    await expect(request('/v1/admin/devices')).rejects.toMatchObject({ code: '502', status: 502 })
  })

  it('injects the Authorization header when a token is stored', async () => {
    localStorage.setItem('adc_token', 'tok-123')
    fetchMock.mockResolvedValue(jsonResponse({ items: [] }))
    await request('/v1/admin/devices')
    const init = fetchMock.mock.calls[0][1]
    const headers = new Headers(init.headers)
    expect(headers.get('Authorization')).toBe('Bearer tok-123')
  })

  it('does not inject Authorization when no token is stored', async () => {
    fetchMock.mockResolvedValue(jsonResponse({ items: [] }))
    await request('/v1/admin/devices')
    const init = fetchMock.mock.calls[0][1]
    const headers = new Headers(init.headers)
    expect(headers.has('Authorization')).toBe(false)
  })

  it('sets a default JSON Content-Type when a body is present and keeps custom headers', async () => {
    fetchMock.mockResolvedValue(jsonResponse({}))
    await request('/v1/admin/devices/d1/tools', {
      method: 'PATCH',
      headers: { 'X-ADC-Confirm': 'true' },
      body: JSON.stringify({ changes: [] }),
    })
    const init = fetchMock.mock.calls[0][1]
    const headers = new Headers(init.headers)
    expect(headers.get('Content-Type')).toBe('application/json')
    expect(headers.get('X-ADC-Confirm')).toBe('true')
  })
})

describe('api helpers', () => {
  it('post sends a JSON stringified body with POST method', async () => {
    fetchMock.mockResolvedValue(jsonResponse({}))
    await api.post('/v1/admin/auth/login', { username: 'admin', password: 'x' })
    const [url, init] = fetchMock.mock.calls[0]
    expect(url).toBe('/v1/admin/auth/login')
    expect(init.method).toBe('POST')
    expect(init.body).toBe(JSON.stringify({ username: 'admin', password: 'x' }))
  })

  it('get builds a query string and skips empty values', async () => {
    fetchMock.mockResolvedValue(jsonResponse({}))
    await api.get('/v1/admin/devices', { page: 2, keyword: '', status: undefined, device_type: 'cnc' })
    expect(fetchMock.mock.calls[0][0]).toBe('/v1/admin/devices?page=2&device_type=cnc')
  })

  it('get keeps the bare path when no params are given', async () => {
    fetchMock.mockResolvedValue(jsonResponse({}))
    await api.get('/v1/admin/devices')
    expect(fetchMock.mock.calls[0][0]).toBe('/v1/admin/devices')
  })

  it('patch and delete use their HTTP verbs', async () => {
    fetchMock.mockResolvedValue(jsonResponse({}))
    await api.patch('/v1/admin/devices/d1', { op: 'freeze' })
    await api.delete('/v1/admin/devices/d1')
    expect(fetchMock.mock.calls[0][1].method).toBe('PATCH')
    expect(fetchMock.mock.calls[1][1].method).toBe('DELETE')
  })
})

describe('download', () => {
  it('returns the response blob on success', async () => {
    const blob = { size: 42 }
    fetchMock.mockResolvedValue({
      ok: true,
      status: 200,
      blob: vi.fn().mockResolvedValue(blob),
      text: vi.fn(),
    })
    await expect(download('/v1/admin/audit-logs/export')).resolves.toBe(blob)
  })

  it('throws ApiError parsed from the error body', async () => {
    fetchMock.mockResolvedValue({
      ok: false,
      status: 400,
      text: vi.fn().mockResolvedValue(JSON.stringify({ code: '14002', message: 'over limit' })),
      blob: vi.fn(),
    })
    await expect(download('/v1/admin/audit-logs/export')).rejects.toBeInstanceOf(ApiError)
  })

  it('throws ApiError even when the error body is not JSON', async () => {
    fetchMock.mockResolvedValue({
      ok: false,
      status: 500,
      statusText: 'Internal Server Error',
      text: vi.fn().mockResolvedValue('boom'),
      blob: vi.fn(),
    })
    await expect(download('/v1/admin/audit-logs/export')).rejects.toMatchObject({
      code: '500',
      status: 500,
    })
  })
})
