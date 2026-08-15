import { ApiError } from './request'

// Unified error rendering: the server sends a code/message error body
// (design/33 1.5) and the console maps known business codes to i18n keys,
// falling back to the server message (V1.0 Chinese text) when unmapped.
export function errorMessage(err: unknown, t: (key: string) => string): string {
  if (err instanceof ApiError) {
    const key = `errors.${err.code}`
    const translated = t(key)
    return translated !== key ? translated : err.message
  }
  return t('errors.network')
}
