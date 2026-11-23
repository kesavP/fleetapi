import { describe, expect, it, vi } from 'vitest'
import { duration, loadStats, loadSummary, statsOf, summaryOf, timingOf } from './mttr'

describe('duration', () => {
  it('words seconds as people say them', () => {
    expect([0, 59, 60, 3599, 3600, 3660, 86400, 90000].map(duration)).toEqual(['0 s', '59 s', '1 min', '59 min', '1 h', '1 h 1 min', '1 d', '1 d 1 h'])
  })
  it('says – for nothing', () => {
    expect([null, undefined, -1, NaN, 'x'].map(duration)).toEqual(['–', '–', '–', '–', '–'])
  })
})

describe('timingOf', () => {
  it('keeps numbers and drops the rest', () => {
    expect(timingOf({ detect_seconds: 30, acknowledge_seconds: -5, resolve_seconds: '9' })).toEqual({ detect: 30, acknowledge: null, mitigate: null, resolve: null })
    expect(timingOf(null)).toEqual({ detect: null, acknowledge: null, mitigate: null, resolve: null })
  })
})

describe('statsOf', () => {
  it('reads the counts and the durations', () => {
    const s = statsOf({
      opened: 5, resolved: 3, not_counted: 1, still_open: 1, reopened: 1, partial: true,
      mttr: { count: 3, mean_seconds: 2000, median_seconds: 1800, p90_seconds: 3600, max_seconds: 3600 },
      mttr_by_severity: { critical: { count: 2, median_seconds: 600 }, '<b>': { count: 1 } },
    })
    expect(s.opened).toBe(5)
    expect(s.partial).toBe(true)
    expect(s.mttr).toEqual({ count: 3, mean: 2000, median: 1800, p90: 3600, max: 3600 })
    expect(s.mtta).toEqual({ count: 0, mean: null, median: null, p90: null, max: null })
    expect(Object.keys(s.bySeverity)).toEqual(['critical'])
  })
  it('survives nonsense', () => {
    expect(statsOf('x').opened).toBe(0)
  })
})

describe('summaryOf', () => {
  it('keeps what it knows and words the unknown safely', () => {
    const s = summaryOf({
      headline: 'Critical: x — open for 5 min.', elapsed_seconds: 300, timing: { detect_seconds: 120 },
      findings: [{ at: '2026-11-23T10:00:00Z', kind: 'rollout', text: 'deployment shop/checkout rolled out', score: 92 }, { kind: 'evil class', text: 'other' }, { kind: 'process' }, 7],
      timeline: [{ at: 't', source: 'kubernetes', text: 'BackOff' }, { source: 'x', text: 'y' }, { source: 'incident' }],
      gaps: ['Process lineage: none', 3, ''],
    })
    expect(s.headline).toBe('Critical: x — open for 5 min.')
    expect(s.timing.detect).toBe(120)
    expect(s.findings.map((f) => f.kind)).toEqual(['rollout', 'other'])
    expect(s.timeline.map((e) => e.source)).toEqual(['kubernetes', 'other'])
    expect(s.gaps).toEqual(['Process lineage: none'])
  })
})

describe('loaders', () => {
  it('asks for the stats of a known window, and 7 days for any other', async () => {
    const client = { get: vi.fn(async () => ({ opened: 1 })) }
    await loadStats(client, 30)
    await loadStats(client, 12)
    expect(client.get.mock.calls).toEqual([['incidents/stats', { days: 30 }], ['incidents/stats', { days: 7 }]])
  })
  it('asks for a summary by a valid id only', async () => {
    const client = { get: vi.fn(async () => ({ headline: 'h' })) }
    expect((await loadSummary(client, 'abcd1234')).headline).toBe('h')
    expect(client.get).toHaveBeenCalledWith('incidents/abcd1234/summary')
    await expect(loadSummary(client, '../x')).rejects.toThrow('not an incident id')
  })
})
