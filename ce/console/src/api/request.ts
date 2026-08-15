// API layer: single fetch wrapper per design/20 — errors, auth header and
// trace id are handled in one place; views never call fetch directly.
// BASE is always the unified gateway (ADR-19); dev uses the vite proxy.
const BASE = ''

export class ApiError extends Error {
  constructor(
    public code: string,
    message: string,
    public status: number,
    public traceId?: string,
  ) {
    super(message)
  }
}

export async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const token = localStorage.getItem('adc_token')
  const headers = new Headers(init.headers)
  if (!headers.has('Content-Type') && init.body) headers.set('Content-Type', 'application/json')
  if (token) headers.set('Authorization', `Bearer ${token}`)

  const res = await fetch(`${BASE}${path}`, { ...init, headers })
  const text = await res.text()
  const body = text ? JSON.parse(text) : undefined

  if (res.status === 401 && !window.location.pathname.startsWith('/login')) {
    // Session expired or revoked: drop local token and bounce to the login
    // page (F-02). Skip the redirect on the login page itself so a wrong
    // password can render its inline error instead of reloading.
    localStorage.removeItem('adc_token')
    localStorage.removeItem('adc_user')
    window.location.href = '/login'
    throw new ApiError('10002', 'unauthenticated', res.status, body?.trace_id)
  }
  if (!res.ok) {
    throw new ApiError(body?.code ?? String(res.status), body?.message ?? res.statusText, res.status, body?.trace_id)
  }
  return body as T
}

// Build a query string from a plain object, skipping empty values.
function withQuery(path: string, params?: Record<string, unknown>): string {
  if (!params) return path
  const qs = Object.entries(params)
    .filter(([, v]) => v !== undefined && v !== null && v !== '')
    .map(([k, v]) => `${encodeURIComponent(k)}=${encodeURIComponent(String(v))}`)
    .join('&')
  return qs ? `${path}?${qs}` : path
}

export const api = {
  get: <T>(path: string, params?: Record<string, unknown>) => request<T>(withQuery(path, params)),
  post: <T>(path: string, data?: unknown) => request<T>(path, { method: 'POST', body: JSON.stringify(data) }),
  patch: <T>(path: string, data?: unknown) => request<T>(path, { method: 'PATCH', body: JSON.stringify(data) }),
  delete: <T>(path: string) => request<T>(path, { method: 'DELETE' }),
}
