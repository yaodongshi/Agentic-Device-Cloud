import { describe, expect, it } from 'vitest'
import { formatDate, formatTime } from './format'

describe('formatTime', () => {
  it('renders an em dash for missing values', () => {
    expect(formatTime(undefined)).toBe('—')
    expect(formatTime(null)).toBe('—')
    expect(formatTime('')).toBe('—')
  })

  it('returns the raw string for an invalid timestamp', () => {
    expect(formatTime('not-a-date')).toBe('not-a-date')
  })

  it('formats a valid RFC3339 timestamp as a locale datetime', () => {
    const out = formatTime('2026-08-15T07:30:00Z')
    expect(out).not.toContain('NaN')
    expect(out).toMatch(/\d{4}/)
    expect(out).toMatch(/\d{2}:\d{2}/)
  })
})

describe('formatDate', () => {
  it('returns an empty string for missing values', () => {
    expect(formatDate(undefined)).toBe('')
    expect(formatDate(null)).toBe('')
    expect(formatDate('')).toBe('')
  })

  it('returns the raw string for an invalid timestamp', () => {
    expect(formatDate('not-a-date')).toBe('not-a-date')
  })

  it('formats a valid RFC3339 timestamp as a locale date', () => {
    const out = formatDate('2026-08-15T07:30:00Z')
    expect(out).not.toContain('NaN')
    expect(out).toMatch(/\d{4}/)
  })
})
