// Shared formatting helpers. Timestamps arrive as RFC3339 UTC (design/33 1.1)
// and are rendered in the browser locale/timezone.
export function formatTime(iso?: string | null): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return new Intl.DateTimeFormat(undefined, {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
  }).format(d)
}

export function formatDate(iso?: string | null): string {
  if (!iso) return ''
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return new Intl.DateTimeFormat(undefined, {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
  }).format(d)
}

/** Compact number for quota counters (e.g. 1000000 -> 1M / 100万). */
export function formatCompact(n?: number | null): string {
  if (n === null || n === undefined) return '—'
  if (!Number.isFinite(n)) return '—'
  return new Intl.NumberFormat(undefined, { notation: 'compact', maximumFractionDigits: 1 }).format(n)
}
