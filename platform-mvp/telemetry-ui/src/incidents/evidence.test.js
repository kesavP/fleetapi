import { describe, expect, it, vi } from 'vitest'
import { loadEvidence, summarizeLogs, summarizeSpans } from './evidence'

const span = (id, ms, status = 'OK', o = {}) => ({ trace_id: id.repeat(32).slice(0, 32), name: 'GET /cart', service: '/k8s/shop/frontend', duration_ns: ms * 1e6, status_code: status, time: 't', ...o })

describe('summarizeSpans', () => {
  it('lists the worst failed requests and the slowest requests, each at most `limit`', () => {
    const rows = [span('a', 900, 'ERROR'), span('b', 100, 'ERROR'), span('c', 2000), span('d', 700), span('e', 5)]
    const s = summarizeSpans(rows, 2)
    expect(s.failed.map((x) => x.durationMs)).toEqual([900, 100])
    expect(s.slow.map((x) => x.durationMs)).toEqual([2000, 900])
    expect(s.slow[0]).toMatchObject({ traceId: 'c'.repeat(32), name: 'GET /cart' })
  })
  it('copes with nothing, and with rows that are not spans', () => {
    expect(summarizeSpans(null)).toEqual({ failed: [], slow: [] })
    expect(summarizeSpans([null, {}, { trace_id: '' }])).toEqual({ failed: [], slow: [] })
  })
})

describe('summarizeLogs', () => {
  const row = (service, body, time = '2026-01-01T10:00:00Z') => ({ service, body, time })
  it('groups lines of one shape, counts them and keeps a sample, most frequent first', () => {
    const s = summarizeLogs([row('a', 'timeout after 31 ms'), row('a', 'timeout after 5 ms', '2026-01-01T10:05:00Z'), row('b', 'boom')])
    expect(s).toHaveLength(2)
    expect(s[0]).toMatchObject({ service: 'a', count: 2, sample: 'timeout after 31 ms' })
    expect(s[0].last - s[0].first).toBe(5 * 60 * 1000)
  })
  it('keeps the same message from two applications apart', () => {
    expect(summarizeLogs([row('a', 'boom'), row('b', 'boom')])).toHaveLength(2)
  })
  it('ignores lines with no body and stops at the limit', () => {
    expect(summarizeLogs([row('a', ''), null, row('a', 'x')])).toHaveLength(1)
    const many = Array.from({ length: 20 }, (_, i) => row('a', `distinct message number ${'x'.repeat(i + 1)}`))
    expect(summarizeLogs(many, 3).length).toBeLessThanOrEqual(3)
  })
})

describe('loadEvidence', () => {
  const FE = '/k8s/shop/frontend'
  const CART = '/k8s/shop/cartservice'
  const clientOf = (answers) => ({ get: vi.fn(async (path, params) => { const a = answers[path]; if (a instanceof Error) throw a; return typeof a === 'function' ? a(params) : a }) })

  it('reads the hurt application\'s root spans and the error logs of it and of the cause, in the incident\'s window', async () => {
    const client = clientOf({ traces: [], logs: [] })
    await loadEvidence(client, { app: FE, cause: { scope: 'edge', kind: 'rtt', from: CART, to: '/k8s/shop/redis-cart' }, start: 1000.4, end: 1600.2 })
    const calls = client.get.mock.calls
    const traces = calls.filter(([p]) => p === 'traces').map(([, q]) => q)
    expect(traces).toHaveLength(2)
    for (const q of traces) expect(q).toMatchObject({ service: FE, root: true, from: 1000, to: 1601 })
    expect(traces.find((q) => q.status === 'error')).toBeTruthy()
    expect(traces.find((q) => q.min_duration_ms === 500)).toBeTruthy()
    const logs = calls.filter(([p]) => p === 'logs').map(([, q]) => q.service).sort()
    expect(logs).toEqual([FE, '/k8s/shop/redis-cart'].sort())
    expect(calls.some(([p]) => p === 'profiles/top')).toBe(false) // not a CPU cause
  })

  it('reads the CPU profile of a CPU cause, and gives its hottest functions', async () => {
    const client = clientOf({
      traces: [], logs: [],
      'profiles/top': [{ frames: ['regex.match', 'handle', 'main'], value: 8e9 }, { frames: ['gc', 'main'], value: 2e9 }],
    })
    const e = await loadEvidence(client, { app: FE, cause: { scope: 'app', kind: 'cpu', app: CART }, start: 1000, end: 1600 })
    expect(client.get.mock.calls.find(([p]) => p === 'profiles/top')[1]).toMatchObject({ service: CART, type: 'cpu:nanoseconds' })
    expect(e.profile.service).toBe(CART)
    expect(e.profile.functions[0]).toMatchObject({ name: 'regex.match', self: 8e9 })
  })

  it('shows what it could read when one read fails, and says which', async () => {
    const client = clientOf({ traces: new Error('too many spans'), logs: [{ service: FE, body: 'boom', time: '2026-01-01T10:00:00Z' }] })
    const e = await loadEvidence(client, { app: FE, cause: null, start: 1000, end: 1600 })
    expect(e.warnings).toEqual(['traces: too many spans', 'traces: too many spans'])
    expect(e.logs).toHaveLength(1)
    expect(e.traces).toEqual({ failed: [], slow: [] })
  })

  it('does not read profiles for a cause that is not CPU, nor fail on an empty profile', async () => {
    const client = clientOf({ traces: [], logs: [], 'profiles/top': [] })
    const e = await loadEvidence(client, { app: FE, cause: { scope: 'app', kind: 'cpu', app: CART }, start: 1000, end: 1600 })
    expect(e.profile).toBeNull()
  })
})
