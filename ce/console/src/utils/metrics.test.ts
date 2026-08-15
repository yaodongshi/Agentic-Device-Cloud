// Unit tests for the Prometheus text parser and snapshot builder (F-10).
// Run with `npm run test`.
import { describe, expect, it } from 'vitest'
import { buildMonitorSnapshot, parseMetrics, percentileFromHistogram } from './metrics'

const SAMPLE = [
  '# HELP adc_device_online_total Online devices by tenant',
  '# TYPE adc_device_online_total gauge',
  'adc_device_online_total{tenant="t-01"} 12',
  'adc_device_online_total{tenant="t-02"} 8',
  '# HELP adc_agent_calls_total Agent tool calls',
  '# TYPE adc_agent_calls_total counter',
  'adc_agent_calls_total{tenant="t-01"} 23000',
  'adc_agent_calls_total{tenant="t-02"} 9000',
  '# TYPE adc_hitl_ticket gauge',
  'adc_hitl_ticket{state="pending"} 3',
  'adc_hitl_ticket{state="approved"} 41',
  'adc_hitl_ticket{state="rejected"} 7',
  '# TYPE adc_hitl_blocked_total counter',
  'adc_hitl_blocked_total 15',
  '# TYPE adc_hitl_approve_seconds histogram',
  'adc_hitl_approve_seconds_bucket{le="0.1"} 2',
  'adc_hitl_approve_seconds_bucket{le="0.5"} 8',
  'adc_hitl_approve_seconds_bucket{le="1"} 9',
  'adc_hitl_approve_seconds_bucket{le="5"} 10',
  'adc_hitl_approve_seconds_bucket{le="+Inf"} 10',
  'adc_hitl_approve_seconds_sum 6.2',
  'adc_hitl_approve_seconds_count 10',
  'process_cpu NaN',
  'something_infinite +Inf',
].join('\n')

describe('parseMetrics', () => {
  it('parses metric families, labels and values', () => {
    const parsed = parseMetrics(SAMPLE)
    const device = parsed.get('adc_device_online_total')
    expect(device?.type).toBe('gauge')
    expect(device?.help).toContain('Online devices')
    expect(device?.samples).toHaveLength(2)
    expect(device?.samples[1]).toEqual({ labels: { tenant: 't-02' }, value: 8 })
  })

  it('parses special float values', () => {
    const parsed = parseMetrics(SAMPLE)
    expect(parsed.get('process_cpu')?.samples[0]?.value).toBeNaN()
    expect(parsed.get('something_infinite')?.samples[0]?.value).toBe(Infinity)
  })

  it('handles quoted label values with escapes', () => {
    const parsed = parseMetrics('adc_x{device="cnc\\"01",tool="a,b"} 1')
    const sample = parsed.get('adc_x')?.samples[0]
    expect(sample?.labels).toEqual({ device: 'cnc"01', tool: 'a,b' })
  })
})

describe('percentileFromHistogram', () => {
  const buckets = [
    { le: 0.1, count: 2 },
    { le: 0.5, count: 8 },
    { le: 1, count: 9 },
    { le: 5, count: 10 },
    { le: Infinity, count: 10 },
  ]

  it('interpolates within the target bucket', () => {
    // p95 of 10 observations = target 9.5, inside the 1..5 bucket (counts 9..10).
    expect(percentileFromHistogram(buckets, 95)).toBeCloseTo(3, 5)
  })

  it('returns null for empty histograms', () => {
    expect(percentileFromHistogram([], 95)).toBeNull()
    expect(percentileFromHistogram([{ le: Infinity, count: 0 }], 95)).toBeNull()
  })
})

describe('buildMonitorSnapshot', () => {
  it('extracts all dashboard series from adc_* families', () => {
    const snapshot = buildMonitorSnapshot(parseMetrics(SAMPLE))
    expect(snapshot.deviceOnlineTotal).toBe(20)
    expect(snapshot.deviceOnlineByTenant).toEqual({ 't-01': 12, 't-02': 8 })
    expect(snapshot.agentCallsTotal).toBe(32000)
    expect(snapshot.hitlByState).toEqual({ pending: 3, approved: 41, rejected: 7 })
    expect(snapshot.hitlBlockedTotal).toBe(15)
    expect(snapshot.approvalTotal).toBe(10)
    expect(snapshot.approvalP95Seconds).toBeCloseTo(3, 5)
  })

  it('falls back to adc_tool_call_total when adc_agent_calls_total is missing', () => {
    const text = 'adc_tool_call_total{tenant="t-01"} 5\nadc_device_online_total 2'
    const snapshot = buildMonitorSnapshot(parseMetrics(text))
    expect(snapshot.agentCallsTotal).toBe(5)
    expect(snapshot.agentCallsByTenant).toEqual({ 't-01': 5 })
  })

  it('returns zeros and nulls when metrics are absent', () => {
    const snapshot = buildMonitorSnapshot(new Map())
    expect(snapshot.deviceOnlineTotal).toBe(0)
    expect(snapshot.agentCallsTotal).toBe(0)
    expect(snapshot.hitlBlockedTotal).toBe(0)
    expect(snapshot.approvalP95Seconds).toBeNull()
    expect(snapshot.approvalTotal).toBe(0)
  })
})
