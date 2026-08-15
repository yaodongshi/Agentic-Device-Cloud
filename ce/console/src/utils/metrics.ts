// Prometheus text exposition format parser (F-10). The console reads GET
// /metrics on the gateway (design/33 1.2 endpoint 27: unauthenticated,
// plain text, internal network only) and turns the `adc_*` families
// (design/60 6.1) into a typed snapshot for the monitoring dashboard.
//
// Exposition format reference:
// https://prometheus.io/docs/instrumenting/exposition_formats/

export interface MetricSample {
  labels: Record<string, string>
  value: number
}

export interface MetricFamily {
  name: string
  help: string
  type: string
  samples: MetricSample[]
}

export type ParsedMetrics = Map<string, MetricFamily>

export interface HistogramBucket {
  le: number
  count: number
}

/** One refresh cycle of the monitoring dashboard (design/20 4.12). */
export interface MonitorSnapshot {
  deviceOnlineTotal: number
  deviceOnlineByTenant: Record<string, number>
  agentCallsTotal: number
  agentCallsByTenant: Record<string, number>
  hitlByState: Record<string, number>
  hitlBlockedTotal: number
  approvalP95Seconds: number | null
  approvalTotal: number
}

const HELP_RE = /^#\s+HELP\s+(\S+)\s*(.*)$/
const TYPE_RE = /^#\s+TYPE\s+(\S+)\s+(\S+)$/
const SAMPLE_RE = /^([a-zA-Z_:][a-zA-Z0-9_:]*)\s*(?:\{([^}]*)\})?\s+([^\s]+)/

/** Sentinel label used when a metric sample has no `tenant` label. */
export const ALL_TENANTS_LABEL = '__all__'

function parseLabels(raw: string): Record<string, string> {
  const labels: Record<string, string> = {}
  if (!raw) return labels
  // Split on commas that are not inside double quotes; backslash escapes
  // inside quoted values keep the following quote from acting as a
  // delimiter.
  const parts: string[] = []
  let current = ''
  let inQuote = false
  let escaped = false
  for (const ch of raw) {
    if (inQuote && ch === '\\') {
      current += ch
      escaped = true
      continue
    }
    if (escaped) {
      current += ch
      escaped = false
      continue
    }
    if (ch === '"') inQuote = !inQuote
    if (ch === ',' && !inQuote) {
      parts.push(current)
      current = ''
      continue
    }
    current += ch
  }
  parts.push(current)
  for (const part of parts) {
    const eq = part.indexOf('=')
    if (eq < 0) continue
    const key = part.slice(0, eq).trim()
    let value = part.slice(eq + 1).trim()
    if (value.startsWith('"') && value.endsWith('"')) {
      value = value.slice(1, -1).replace(/\\(["\\])/g, '$1')
    }
    if (key) labels[key] = value
  }
  return labels
}

function parseValue(raw: string): number {
  switch (raw) {
    case 'NaN':
      return NaN
    case '+Inf':
      return Infinity
    case '-Inf':
      return -Infinity
    default:
      return Number(raw)
  }
}

/** Parse a Prometheus text exposition payload into name -> family entries. */
export function parseMetrics(text: string): ParsedMetrics {
  const metrics: ParsedMetrics = new Map()
  for (const line of text.split('\n')) {
    const trimmed = line.trim()
    if (!trimmed || trimmed.startsWith('#')) {
      const help = trimmed.match(HELP_RE)
      if (help) {
        const family = metrics.get(help[1]) ?? { name: help[1], help: '', type: '', samples: [] }
        family.help = family.help ? `${family.help}\n${help[2]}` : help[2]
        metrics.set(help[1], family)
      }
      const type = trimmed.match(TYPE_RE)
      if (type) {
        const family = metrics.get(type[1]) ?? { name: type[1], help: '', type: '', samples: [] }
        family.type = type[2]
        metrics.set(type[1], family)
      }
      continue
    }
    const match = trimmed.match(SAMPLE_RE)
    if (!match) continue
    const [, name, labelText, valueText] = match
    const family = metrics.get(name) ?? { name, help: '', type: '', samples: [] }
    family.samples.push({ labels: parseLabels(labelText ?? ''), value: parseValue(valueText) })
    metrics.set(name, family)
  }
  return metrics
}

/** Sum of all samples of a metric family (counters/gauges without labels). */
export function sumSamples(family: MetricFamily | undefined): number {
  if (!family) return 0
  return family.samples.reduce((acc, s) => acc + (Number.isFinite(s.value) ? s.value : 0), 0)
}

/** Sum samples grouped by the `tenant` label. */
function sumByTenant(family: MetricFamily | undefined): Record<string, number> {
  if (!family) return {}
  const out: Record<string, number> = {}
  for (const s of family.samples) {
    const tenant = s.labels.tenant ?? ALL_TENANTS_LABEL
    out[tenant] = (out[tenant] ?? 0) + (Number.isFinite(s.value) ? s.value : 0)
  }
  return out
}

/**
 * Estimate a percentile from a Prometheus histogram's cumulative `le`
 * buckets using linear interpolation within the target bucket. Returns null
 * when the histogram is empty or has no observations.
 */
export function percentileFromHistogram(buckets: HistogramBucket[], percentile: number): number | null {
  if (!buckets.length) return null
  const sorted = [...buckets].sort((a, b) => a.le - b.le)
  const total = sorted[sorted.length - 1]?.count ?? 0
  if (!total || total <= 0) return null
  const target = (percentile / 100) * total
  let lowerLe = 0
  let lowerCount = 0
  for (const bucket of sorted) {
    if (bucket.count >= target) {
      if (!Number.isFinite(bucket.le)) {
        // The percentile falls above the largest finite bucket: per
        // Prometheus conventions return the previous bucket bound.
        return lowerLe
      }
      const inBucket = bucket.count - lowerCount
      if (inBucket <= 0) return lowerLe
      const ratio = (target - lowerCount) / inBucket
      return lowerLe + ratio * (bucket.le - lowerLe)
    }
    lowerLe = bucket.le
    lowerCount = bucket.count
  }
  return null
}

/** Collect the `{base}_bucket` samples of a histogram family into buckets. */
function histogramOf(metrics: ParsedMetrics, base: string): HistogramBucket[] {
  const family = metrics.get(`${base}_bucket`)
  if (!family) return []
  return family.samples
    .filter((s) => s.labels.le !== undefined)
    .map((s) => ({
      le: s.labels.le === '+Inf' ? Infinity : Number(s.labels.le),
      count: s.value,
    }))
    .filter((b) => Number.isFinite(b.count))
}

/**
 * Build the dashboard snapshot from parsed metrics. Metric names come from
 * design/60 6.1; `adc_agent_calls_total` falls back to `adc_tool_call_total`
 * so the dashboard keeps rendering before the gateway exposes the canonical
 * agent-call counter.
 */
export function buildMonitorSnapshot(metrics: ParsedMetrics): MonitorSnapshot {
  const deviceFamily = metrics.get('adc_device_online_total')
  const callsFamily = metrics.get('adc_agent_calls_total') ?? metrics.get('adc_tool_call_total')
  const ticketFamily = metrics.get('adc_hitl_ticket')
  const blockedFamily = metrics.get('adc_hitl_blocked_total') ?? metrics.get('adc_hitl_intercepted_total')

  const hitlByState: Record<string, number> = {}
  if (ticketFamily) {
    for (const s of ticketFamily.samples) {
      const state = s.labels.state ?? 'other'
      hitlByState[state] = (hitlByState[state] ?? 0) + (Number.isFinite(s.value) ? s.value : 0)
    }
  }

  const buckets = histogramOf(metrics, 'adc_hitl_approve_seconds')
  const approvalTotal = sumSamples(metrics.get('adc_hitl_approve_seconds_count'))
  const approvalP95 = buckets.length ? percentileFromHistogram(buckets, 95) : null

  return {
    deviceOnlineTotal: sumSamples(deviceFamily),
    deviceOnlineByTenant: sumByTenant(deviceFamily),
    agentCallsTotal: sumSamples(callsFamily),
    agentCallsByTenant: sumByTenant(callsFamily),
    hitlByState,
    hitlBlockedTotal: sumSamples(blockedFamily),
    approvalP95Seconds: approvalP95,
    approvalTotal,
  }
}

/**
 * Fetch and parse the gateway metrics endpoint. /metrics is unauthenticated
 * plain text (design/33 1.2), so it bypasses the JSON `request` wrapper; in
 * dev the vite proxy must forward `/metrics` to the gateway (dev-only gap,
 * production serves it same-origin per design/30).
 */
export async function fetchMetricsSnapshot(metricsPath = '/metrics'): Promise<MonitorSnapshot> {
  const res = await fetch(metricsPath, { headers: { Accept: 'text/plain' } })
  if (!res.ok) {
    throw new Error(`metrics request failed: HTTP ${res.status}`)
  }
  return buildMonitorSnapshot(parseMetrics(await res.text()))
}
