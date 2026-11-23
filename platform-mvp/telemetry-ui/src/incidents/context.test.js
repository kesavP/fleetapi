import { describe, expect, it, vi } from 'vitest'
import { bucketSpans, contextWindow, incidentMarkers, loadContext, sumByTime } from './context.js'

const APP = '/k8s/shop/frontend'
const POD = (n) => `${APP}-5d6f8c7b9-xk2l${n}/server`
const NOW = Date.parse('2026-11-18T12:00:00Z')
const iso = (m) => new Date(NOW - m * 60000).toISOString()
const incident = (o = {}) => ({ id: 'abcdefgh', app: APP, state: 'open', startedAt: iso(60), createdAt: iso(60), resolvedAt: '', ...o })

describe('contextWindow', () => {
  it('runs from half an hour before the incident started to now while it is open', () => {
    const w = contextWindow(incident(), { now: NOW })
    expect(w.end).toBe(NOW / 1000)
    expect(w.start).toBe((NOW - 60 * 60000) / 1000 - 1800)
  })
  it('ends a quarter of an hour after it was resolved, and not later than now', () => {
    const r = incident({ state: 'resolved', resolvedAt: iso(30) })
    expect(contextWindow(r, { now: NOW }).end).toBe((NOW - 30 * 60000) / 1000 + 900)
    const late = incident({ state: 'closed', resolvedAt: iso(1) })
    expect(contextWindow(late, { now: NOW }).end).toBe(NOW / 1000)
  })
  it('is at least an hour and at most a week long', () => {
    const fresh = contextWindow(incident({ startedAt: iso(1) }), { now: NOW })
    expect(fresh.end - fresh.start).toBe(3600)
    const old = contextWindow(incident({ startedAt: iso(60 * 24 * 30) }), { now: NOW })
    expect(old.end - old.start).toBe(7 * 86400)
  })
  it('uses six hours when there is no start, and the length that was picked when there is one', () => {
    const none = contextWindow(incident({ startedAt: '', createdAt: '' }), { now: NOW })
    expect(none.end - none.start).toBe(6 * 3600)
    const picked = contextWindow(incident(), { now: NOW, span: 24 * 3600 })
    expect(picked.end - picked.start).toBe(24 * 3600)
  })
})

describe('sumByTime', () => {
  const w = () => (cid) => (cid && cid.startsWith('/k8s/shop/frontend') ? { id: APP } : cid ? { id: '/k8s/shop/other' } : null)
  it('adds the pods of the application up per instant and leaves the others out', () => {
    const series = [
      { labels: { container_id: POD(2) }, points: [{ t: 2000, v: 3 }, { t: 1000, v: 1 }] },
      { labels: { container_id: POD(4) }, points: [{ t: 1000, v: 2 }] },
      { labels: { container_id: '/k8s/shop/other-aaaaaaaaaa-bbbbb/x' }, points: [{ t: 1000, v: 100 }] },
      { labels: {}, points: [{ t: 1000, v: 100 }] },
    ]
    expect(sumByTime(series, APP, w())).toEqual([{ t: 1000, v: 3 }, { t: 2000, v: 3 }])
  })
  it('skips points that are not numbers', () => {
    const series = [{ labels: { container_id: POD(2) }, points: [{ t: 1, v: NaN }, { t: NaN, v: 1 }, { t: 2, v: 5 }] }]
    expect(sumByTime(series, APP, w())).toEqual([{ t: 2, v: 5 }])
  })
})

describe('bucketSpans', () => {
  const span = (min, status = 'OK', ms = 10) => ({ time: new Date(NOW - min * 60000).toISOString(), status_code: status, duration_ns: ms * 1e6 })
  const range = { from: NOW - 60 * 60000, to: NOW, buckets: 6 }
  it('counts requests, errors and the 95th percentile per slice', () => {
    const r = bucketSpans([span(5, 'ERROR', 900), span(6, 'OK', 100), span(55, 'OK', 20)], range)
    expect(r.points).toHaveLength(6)
    const last = r.points[5]
    expect(last).toMatchObject({ count: 2, errors: 1, errorPct: 50, p95Ms: 900 })
    expect(last.rps).toBeCloseTo(2 / 600)
    expect(r.points[0]).toMatchObject({ count: 1, errors: 0, errorPct: 0, p95Ms: 20 })
    expect(r.truncated).toBe(false)
  })
  it('has no percentage or latency for a slice with no requests, not a zero', () => {
    const p = bucketSpans([span(5)], range).points[2]
    expect(p).toMatchObject({ count: 0, errorPct: null, p95Ms: null })
  })
  it('describes only the newest part when a full page came back', () => {
    const spans = [span(5), span(10), span(20)]
    const r = bucketSpans(spans, { ...range, limit: 3 })
    expect(r.truncated).toBe(true)
    expect(r.from).toBe(NOW - 20 * 60000)
    expect(r.points[0].t).toBe(NOW - 20 * 60000)
  })
  it('ignores spans outside the window and answers nothing for an empty window', () => {
    expect(bucketSpans([span(500)], range).points.every((p) => p.count === 0)).toBe(true)
    expect(bucketSpans([], { from: 5, to: 5 }).points).toEqual([])
    expect(bucketSpans(null, range).total).toBe(0)
  })
})

describe('incidentMarkers', () => {
  it('marks the start, the resolution and the events, in time order, and not the opening event twice', () => {
    const inc = incident({ state: 'resolved', resolvedAt: iso(10) })
    const m = incidentMarkers(inc, [{ when: iso(20), type: 'comment', text: 'Looking' }, { when: iso(60), type: 'created', text: 'Opened' }, { when: 'bad', type: 'x', text: 'y' }])
    expect(m.map((x) => [x.kind, x.label])).toEqual([['started', 'Incident started'], ['event', 'Looking'], ['resolved', 'Resolved']])
  })
  it('does not mark a resolution on an incident that is still open', () => {
    expect(incidentMarkers(incident({ resolvedAt: iso(5) })).map((x) => x.kind)).toEqual(['started'])
  })
})

// A read API with only what the loader asks of it.
function api({ metrics = ['container_resources_cpu_usage_seconds_total', 'container_resources_memory_rss_bytes', 'container_restarts_total'], fail = {} } = {}) {
  const calls = []
  return {
    calls,
    get: vi.fn(async (path, params = {}) => {
      calls.push([path, params])
      if (fail[path] || (params.name && fail[params.name])) throw new Error(fail[path] || fail[params.name])
      if (path === 'metrics') return metrics
      if (path === 'traces') return params.status === 'error' || params.min_duration_ms ? [] : [{ trace_id: 't1', time: iso(5), status_code: 'ERROR', duration_ns: 5e8, name: 'GET /', service: APP }]
      if (path === 'logs') return []
      if (path === 'metrics/query_range') {
        const t = NOW - 5 * 60000
        if (params.name === 'container_restarts_total') return { series: [{ labels: { container_id: POD(2) }, points: [{ t, v: 1 / params.step }] }] }
        if (params.name === 'container_resources_memory_rss_bytes') return { series: [{ labels: { container_id: POD(2) }, points: [{ t, v: 100 }] }, { labels: { container_id: POD(4) }, points: [{ t, v: 50 }] }] }
        if (params.name === 'container_resources_cpu_usage_seconds_total') return { series: [{ labels: { container_id: POD(2) }, points: [{ t, v: 0.5 }] }] }
        return { series: [] }
      }
      return []
    }),
  }
}

describe('loadContext', () => {
  it('reads the four panels for the incident application and its window', async () => {
    const client = api()
    const c = await loadContext(client, incident(), { now: NOW, events: [{ when: iso(30), type: 'comment', text: 'Looking' }] })
    expect(c.app).toBe(APP)
    expect(c.panels.signals).toMatchObject({ status: 'ok', total: 1 })
    expect(c.panels.signals.baseline).not.toBeNull()
    expect(c.panels.resources.status).toBe('ok')
    expect(c.panels.resources.memory).toEqual([{ t: NOW - 5 * 60000, v: 150 }])
    expect(c.panels.resources.cpu).toEqual([{ t: NOW - 5 * 60000, v: 0.5 }])
    expect(c.panels.evidence.status).toBe('ok')
    const trace = client.calls.find(([p, q]) => p === 'traces' && q.root && !q.status && !q.min_duration_ms)
    expect(trace[1]).toMatchObject({ service: APP, root: true, from: Math.floor(c.window.start), to: Math.ceil(c.window.end) })
    expect(c.markers.map((m) => m.kind)).toEqual(['started', 'event', 'restart'])
    expect(c.markers.find((m) => m.kind === 'restart').label).toBe('1 restart')
  })
  it('reads the same window of yesterday for the traffic, shifted onto today for overlaying', async () => {
    const client = api()
    const c = await loadContext(client, incident(), { now: NOW })
    const reads = client.calls.filter(([p, q]) => p === 'traces' && q.root && !q.status && !q.min_duration_ms)
    expect(reads).toHaveLength(2)
    expect(reads[1][1].from).toBe(reads[0][1].from - 86400)
    expect(c.panels.signals.baseline.points[0].t).toBeGreaterThanOrEqual(c.window.start * 1000 - 1)
  })
  it('does not read yesterday when told not to', async () => {
    const client = api()
    const c = await loadContext(client, incident(), { now: NOW, compare: false })
    expect(client.calls.filter(([p, q]) => p === 'traces' && q.root && !q.status && !q.min_duration_ms)).toHaveLength(1)
    expect(c.panels.signals.baseline).toBeNull()
  })
  it('shows what it could read when one panel fails, and says why for that one', async () => {
    const client = api({ fail: { 'container_resources_memory_rss_bytes': 'boom' } })
    const c = await loadContext(client, incident(), { now: NOW })
    expect(c.panels.signals.status).toBe('ok')
    expect(c.panels.resources.status).toBe('ok')
    expect(c.panels.resources.memory).toEqual([])
    expect(c.warnings.join(' ')).toContain('container_resources_memory_rss_bytes: boom')
    const down = await loadContext(api({ fail: { traces: 'no traces' } }), incident(), { now: NOW })
    expect(down.panels.signals).toEqual({ status: 'unavailable', reason: 'The traffic could not be read: no traces' })
    expect(down.panels.resources.status).toBe('ok')
  })
  it('says so when the list of metrics cannot be read, and still shows the traffic', async () => {
    const c = await loadContext(api({ fail: { metrics: 'bad key' } }), incident(), { now: NOW })
    expect(c.panels.resources).toEqual({ status: 'unavailable', reason: 'The resource use could not be read: bad key' })
    expect(c.panels.dependencies.status).toBe('unavailable')
    expect(c.panels.signals.status).toBe('ok')
  })
  it('says the project has no resource metrics when it has none', async () => {
    const c = await loadContext(api({ metrics: [] }), incident(), { now: NOW })
    expect(c.panels.resources.status).toBe('none')
  })
  it('reads no metrics for an application that is not a Kubernetes workload, but still its traffic', async () => {
    const client = api()
    const c = await loadContext(client, incident({ app: 'checkout-api' }), { now: NOW })
    expect(c.panels.resources.status).toBe('none')
    expect(c.panels.dependencies.status).toBe('none')
    expect(client.calls.some(([p]) => p === 'metrics' || p === 'metrics/query_range')).toBe(false)
    expect(c.panels.signals.status).toBe('ok')
  })
  it('has nothing to look up for an incident with no application, and asks for nothing', async () => {
    const client = api()
    const c = await loadContext(client, incident({ app: '' }), { now: NOW })
    expect(Object.values(c.panels).map((p) => p.status)).toEqual(['none', 'none', 'none', 'none'])
    expect(client.calls).toEqual([])
  })
  it('warns that the traffic is partial when a full page of requests came back', async () => {
    const full = Array.from({ length: 1000 }, (_, i) => ({ trace_id: `t${i}`, time: iso(5 + (i % 40)), status_code: 'OK', duration_ns: 1e6 }))
    const client = api()
    const get = client.get
    client.get = vi.fn(async (p, q) => (p === 'traces' && q.root && !q.status && !q.min_duration_ms ? full : get(p, q)))
    const c = await loadContext(client, incident(), { now: NOW, compare: false })
    expect(c.panels.signals.truncated).toBe(true)
    expect(c.warnings.join(' ')).toContain('newest part')
  })
})
