import { describe, expect, it, vi } from 'vitest'
import { buildWaterfall, filterProblem, getTrace, logParams, searchLogs, searchSpans, serviceNames, spanParams, validTraceId } from './search'

const R = { from: 100, to: 200 }

describe('spanParams', () => {
  it('leaves out empty filters and sends the rest', () => {
    expect(spanParams({ service: ' /k8s/a/b ', name: '', status: 'error', minDurationMs: '250', rootOnly: false, limit: 500 }, R)).toEqual({
      from: 100, to: 200, service: '/k8s/a/b', status: 'error', min_duration_ms: '250', limit: 500,
    })
  })
  it('asks for roots only when told, and defaults the page size', () => {
    expect(spanParams({ rootOnly: true }, R)).toEqual({ from: 100, to: 200, root: true, limit: 100 })
  })
})

describe('logParams', () => {
  it('sends the text as q and lower-cases a trace id', () => {
    const id = 'ABCDEF0123456789ABCDEF0123456789'
    expect(logParams({ service: 's', severity: 'ERROR', q: ' timeout ', traceId: id, limit: 1000 }, R)).toEqual({
      from: 100, to: 200, service: 's', severity: 'ERROR', q: 'timeout', trace_id: id.toLowerCase(), limit: 1000,
    })
  })
  it('sends nothing for blanks', () => {
    expect(logParams({ q: '  ', traceId: ' ' }, R)).toEqual({ from: 100, to: 200, limit: 100 })
  })
})

describe('validation', () => {
  it('knows a trace id', () => {
    expect(validTraceId('a'.repeat(32))).toBe(true)
    expect(validTraceId('A'.repeat(32))).toBe(true)
    for (const bad of ['', 'a'.repeat(31), 'g'.repeat(32), undefined, '../etc/passwd']) expect(validTraceId(bad)).toBe(false)
  })
  it('names the problem with a filter set', () => {
    expect(filterProblem({})).toBe('')
    expect(filterProblem({ minDurationMs: '', traceId: '' })).toBe('')
    expect(filterProblem({ minDurationMs: '12' })).toBe('')
    expect(filterProblem({ minDurationMs: '1.5' })).toContain('whole number')
    expect(filterProblem({ minDurationMs: '-3' })).toContain('whole number')
    expect(filterProblem({ traceId: 'xyz' })).toContain('32 hex')
  })
})

describe('calls', () => {
  const NOW = () => 1_700_000_000_000
  it('searches over the window ending now', async () => {
    const get = vi.fn(async () => [{ span_id: 'a' }])
    expect(await searchSpans({ get }, { service: 's' }, { seconds: 600, now: NOW })).toEqual([{ span_id: 'a' }])
    expect(get).toHaveBeenCalledWith('traces', { from: 1_700_000_000 - 600, to: 1_700_000_000, service: 's', limit: 100 })
    await searchLogs({ get }, { q: 'x' }, { seconds: 60, now: NOW })
    expect(get).toHaveBeenLastCalledWith('logs', { from: 1_700_000_000 - 60, to: 1_700_000_000, q: 'x', limit: 100 })
  })
  it('treats a null answer as no rows', async () => {
    const client = { get: async () => null }
    expect(await searchSpans(client, {}, { seconds: 60 })).toEqual([])
    expect(await searchLogs(client, {}, { seconds: 60 })).toEqual([])
    expect(await getTrace(client, 'a'.repeat(32))).toEqual([])
  })
  it('reads a trace by its lower-case id', async () => {
    const get = vi.fn(async () => [])
    await getTrace({ get }, 'AB'.repeat(16))
    expect(get).toHaveBeenCalledWith(`traces/${'ab'.repeat(16)}`)
  })
  it('lists service names from traces and logs, sorted, once each, and survives one failing', async () => {
    const get = async (p) => {
      if (p === 'logs/services') throw new Error('down')
      return ['b', 'a', 'b', '']
    }
    expect(await serviceNames({ get }, { seconds: 60 })).toEqual(['a', 'b'])
    const both = async (p) => (p === 'logs/services' ? ['c', 'a'] : ['b', 'a'])
    expect(await serviceNames({ get: both }, { seconds: 60 })).toEqual(['a', 'b', 'c'])
  })
})

describe('buildWaterfall', () => {
  const T = Date.parse('2026-01-01T00:00:00Z')
  const sp = (id, parent, startMs, ms, name = id) => ({ span_id: id, parent_span_id: parent, name, time: new Date(T + startMs).toISOString(), duration_ns: ms * 1e6 })

  it('nests a span under its parent and orders siblings by start', () => {
    const w = buildWaterfall([sp('c2', 'a', 60, 10), sp('a', '', 0, 100), sp('c1', 'a', 10, 20), sp('g', 'c1', 12, 5)])
    expect(w.rows.map((r) => [r.span.span_id, r.depth])).toEqual([['a', 0], ['c1', 1], ['g', 2], ['c2', 1]])
    expect(w.linked).toBe(true)
  })

  it('places bars as a share of the trace', () => {
    const w = buildWaterfall([sp('a', '', 0, 100), sp('b', 'a', 25, 50)])
    expect(w.totalMs).toBe(100)
    expect(w.rows[0]).toMatchObject({ left: 0, width: 100 })
    expect(w.rows[1].left).toBeCloseTo(25, 9)
    expect(w.rows[1].width).toBeCloseTo(50, 9)
  })

  it('is a flat list by start time when nothing links to anything (what the node-agent records)', () => {
    const w = buildWaterfall([sp('b', '', 50, 5), sp('a', '', 0, 5), sp('c', '', 20, 5)])
    expect(w.rows.map((r) => r.span.span_id)).toEqual(['a', 'c', 'b'])
    expect(w.rows.every((r) => r.depth === 0)).toBe(true)
    expect(w.linked).toBe(false)
  })

  it('treats a parent that is not in the trace as no parent', () => {
    const w = buildWaterfall([sp('a', 'missing', 0, 5)])
    expect(w.rows[0].depth).toBe(0)
  })

  it('keeps a sliver for an instant span and never leaves the bar', () => {
    const w = buildWaterfall([sp('a', '', 0, 100), sp('z', 'a', 100, 0)])
    const z = w.rows[1]
    expect(z.width).toBeGreaterThan(0)
    expect(z.left + z.width).toBeLessThanOrEqual(100.000001)
  })

  it('shows every span even if the data has a parent cycle', () => {
    const w = buildWaterfall([sp('a', 'b', 0, 5), sp('b', 'a', 1, 5), sp('s', 's', 2, 5)])
    expect(w.rows.map((r) => r.span.span_id).sort()).toEqual(['a', 'b', 's'])
  })

  it('reads a nanosecond timestamp and handles nothing and junk', () => {
    const ns = { span_id: 'x', parent_span_id: '', name: 'n', time: '2026-01-01T00:00:00.123456789Z', duration_ns: 1e6 }
    expect(buildWaterfall([ns]).startMs).toBe(Date.parse('2026-01-01T00:00:00.123Z'))
    expect(buildWaterfall([])).toEqual({ rows: [], startMs: 0, totalMs: 0, linked: false })
    expect(buildWaterfall(null).rows).toEqual([])
    expect(buildWaterfall([{ span_id: 'j', time: 'nope', duration_ns: 'x' }]).rows).toHaveLength(1)
  })
})
