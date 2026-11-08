import { describe, expect, it } from 'vitest'
import { formatDuration, percentile, severityClass, summarizeLogs, summarizeSpans } from './serviceStats'

describe('percentile', () => {
  it('is the nearest rank of a sorted array', () => {
    const xs = [1, 2, 3, 4, 5, 6, 7, 8, 9, 10]
    expect(percentile(xs, 50)).toBe(5)
    expect(percentile(xs, 95)).toBe(10)
    expect(percentile(xs, 90)).toBe(9)
    expect(percentile(xs, 0)).toBe(1)
    expect(percentile([7], 99)).toBe(7)
  })
  it('is null for nothing', () => expect(percentile([], 50)).toBeNull())
})

describe('formatDuration', () => {
  it('picks a unit', () => {
    expect(formatDuration(null)).toBe('–')
    expect(formatDuration(850)).toBe('850 ns')
    expect(formatDuration(1_500)).toBe('1.5 µs')
    expect(formatDuration(1_500_000)).toBe('1.5 ms')
    expect(formatDuration(250_000_000)).toBe('250 ms')
    expect(formatDuration(2_340_000_000)).toBe('2.3 s')
  })
})

const T0 = Date.parse('2026-01-01T00:00:00Z')
const at = (sec) => new Date(T0 + sec * 1000).toISOString()
const sp = (sec, kind, ms, status = 'OK') => ({ time: at(sec), kind, duration_ns: ms * 1e6, status_code: status })

describe('summarizeSpans', () => {
  const win = { from: T0, to: T0 + 600_000 }

  it('keeps client and server spans apart, so one call is not counted twice', () => {
    const r = summarizeSpans([sp(10, 'SERVER', 10), sp(11, 'CLIENT', 12), sp(12, 'SERVER', 30, 'ERROR')], win)
    expect(r.groups.server).toMatchObject({ count: 2, errors: 1, errorShare: 0.5 })
    expect(r.groups.client).toMatchObject({ count: 1, errors: 0 })
    expect(r.groups.other).toBeUndefined()
    expect(r.primary).toBe('server')
  })

  it('computes percentiles over durations', () => {
    const spans = Array.from({ length: 100 }, (_, i) => sp(i, 'SERVER', i + 1))
    const g = summarizeSpans(spans, win).groups.server
    expect([g.p50, g.p95, g.p99]).toEqual([50e6, 95e6, 99e6])
  })

  it('falls back to client, then other, for the timeline', () => {
    expect(summarizeSpans([sp(1, 'CLIENT', 1)], win).primary).toBe('client')
    expect(summarizeSpans([sp(1, 'INTERNAL', 1)], win).primary).toBe('other')
    expect(summarizeSpans([], win).primary).toBeNull()
  })

  it('puts each span in its time bucket and splits out errors', () => {
    // 600 s in 30 buckets: 20 s each.
    const r = summarizeSpans([sp(5, 'SERVER', 1), sp(15, 'SERVER', 1, 'ERROR'), sp(25, 'SERVER', 1), sp(599, 'SERVER', 1)], win)
    expect(r.timeline).toHaveLength(30)
    expect(r.timeline[0]).toMatchObject({ count: 2, errors: 1 })
    expect(r.timeline[1]).toMatchObject({ count: 1, errors: 0 })
    expect(r.timeline[29].count).toBe(1)
    expect(r.timeline.reduce((a, b) => a + b.count, 0)).toBe(4)
  })

  it('does not call a short page truncated, and covers the whole window', () => {
    const r = summarizeSpans([sp(5, 'SERVER', 1)], { ...win, limit: 10 })
    expect(r.truncated).toBe(false)
    expect(r.from).toBe(win.from)
  })

  it('on a full page, covers only back to the oldest span returned', () => {
    // Newest first: the page ends at 400 s, so the 400 s before it were not returned and must not be shown as empty.
    const spans = [sp(590, 'SERVER', 1), sp(500, 'SERVER', 1), sp(400, 'SERVER', 1)]
    const r = summarizeSpans(spans, { ...win, limit: 3 })
    expect(r.truncated).toBe(true)
    expect(r.from).toBe(T0 + 400_000)
    expect(r.timeline[0].t).toBe(T0 + 400_000)
    expect(r.timeline.reduce((a, b) => a + b.count, 0)).toBe(3)
  })

  it('tolerates null and junk', () => {
    expect(summarizeSpans(null, win).total).toBe(0)
    const r = summarizeSpans([{ time: 'not a time', kind: 'SERVER', duration_ns: 'x', status_code: 'OK' }], win)
    expect(r.groups.server.p50).toBe(0)
  })
})

describe('logs', () => {
  it('classes by number, then by text', () => {
    expect(severityClass({ severity_number: 17 })).toBe('error')
    expect(severityClass({ severity_number: 21 })).toBe('error')
    expect(severityClass({ severity_number: 13 })).toBe('warn')
    expect(severityClass({ severity_number: 9 })).toBe('info')
    expect(severityClass({ severity_number: 5 })).toBe('debug')
    expect(severityClass({ severity_number: 0, severity: 'ERROR' })).toBe('error')
    expect(severityClass({ severity_number: 0, severity: 'Warning' })).toBe('warn')
    expect(severityClass({ severity_number: 0, severity: '' })).toBe('other')
  })

  it('counts by class and notes a full page', () => {
    const logs = [{ severity_number: 17 }, { severity_number: 9 }, { severity_number: 9 }, { severity_number: 0, severity: '' }]
    expect(summarizeLogs(logs, { limit: 4 })).toEqual({ total: 4, truncated: true, bySeverity: { error: 1, warn: 0, info: 2, debug: 0, other: 1 } })
    expect(summarizeLogs(null).total).toBe(0)
  })
})
