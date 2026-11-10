import { describe, expect, it, vi } from 'vitest'
import { incidentId, listIncidents, loadIncident, windowsFor } from './load'

const FE = '/k8s/shop/frontend-5d6f8c7b9-xk2lp/server'
const CART = '/k8s/shop/cartservice-7c9d8f6b5d-bk7q2/server'
const NOW = 1_800_000_000 // a multiple of 60
const nowMs = () => NOW * 1000

// a client whose request counters show frontend failing completely for nine minutes, an hour and a half before NOW
const OUTAGE = [NOW - 5400, NOW - 5400 + 9 * 60]
function client({ outage = OUTAGE, histogram = true, present = null } = {}) {
  const names = present || ['container_http_inbound_requests_total', ...(histogram ? ['container_http_inbound_requests_duration_seconds_total_bucket'] : [])]
  const pts = (from, to, f) => {
    const out = []
    for (let t = Math.ceil(from / 60) * 60; t < to; t += 60) out.push({ t: t * 1000, v: f(t) })
    return out
  }
  const bad = (t) => outage && t >= outage[0] && t < outage[1]
  return {
    get: vi.fn(async (path, q) => {
      if (path === 'metrics') return names
      if (path !== 'metrics/query_range') throw new Error('unexpected ' + path)
      const from = Number(q.from)
      const to = Number(q.to)
      if (q.name === 'container_http_inbound_requests_total') {
        return { series: [
          { labels: { container_id: FE, status: '200' }, points: pts(from, to, (t) => (bad(t) ? 0 : 10)) },
          { labels: { container_id: FE, status: '502' }, points: pts(from, to, (t) => (bad(t) ? 10 : 0)) },
          { labels: { container_id: CART, status: '200' }, points: pts(from, to, () => 5) },
        ] }
      }
      return { series: [
        { labels: { container_id: FE, le: '0.5' }, points: pts(from, to, (t) => (bad(t) ? 0 : 10)) },
        { labels: { container_id: FE, le: '+Inf' }, points: pts(from, to, () => 10) },
      ] }
    }),
  }
}

describe('incidentId', () => {
  it('is the same for the same incident, and differs between incidents', () => {
    expect(incidentId('/k8s/shop/frontend', 1000)).toBe(incidentId('/k8s/shop/frontend', 1000))
    expect(incidentId('/k8s/shop/frontend', 1000)).not.toBe(incidentId('/k8s/shop/frontend', 1060))
    expect(incidentId('/k8s/shop/frontend', 1000)).not.toBe(incidentId('/k8s/shop/cartservice', 1000))
    expect(incidentId('a', 1)).toMatch(/^[0-9a-z]{8}$/)
  })
})

describe('listIncidents', () => {
  it('finds the outage, with when it began and ended, which objective it broke and how badly', async () => {
    const r = await listIncidents(client(), { now: nowMs })
    expect(r.incidents).toHaveLength(1)
    const i = r.incidents[0]
    expect(i).toMatchObject({ app: '/k8s/shop/frontend', label: 'shop/frontend', open: false })
    expect(i.start).toBe(OUTAGE[0])
    expect(i.end).toBe(OUTAGE[1])
    expect(i.kind.sort()).toEqual(['availability', 'latency'])
    expect(i.slo.availability.objective).toBe(0.999)
    expect(i.slo.availability.compliance).toBe(0) // every request in the incident failed
    expect(i.slo.availability.burn5m).toBeGreaterThan(14.4)
    expect(i.slo.availability.burn1h).toBeGreaterThan(14.4) // 9 bad minutes in the hour
    expect(i.id).toBe(incidentId('/k8s/shop/frontend', OUTAGE[0]))
    expect(r.hasData).toBe(true)
    expect(r.warnings).toEqual([])
  })
  it('finds nothing in a quiet project, and does not report an application that is only called', async () => {
    const r = await listIncidents(client({ outage: null }), { now: nowMs })
    expect(r.incidents).toEqual([])
  })
  it('without a histogram it finds the failures and says latency was not judged', async () => {
    const r = await listIncidents(client({ histogram: false }), { now: nowMs })
    expect(r.incidents[0].kind).toEqual(['availability'])
    expect(r.incidents[0].slo.latency).toBeNull()
    expect(r.warnings.join(' ')).toContain('no request-duration histogram')
  })
  it('says there is no data when the project has no request metrics, rather than that all is well', async () => {
    const r = await listIncidents(client({ present: ['container_cpu'] }), { now: nowMs })
    expect(r.hasData).toBe(false)
    expect(r.incidents).toEqual([])
  })
  it('asks for the lookback it was given, one point a minute', async () => {
    const c = client()
    await listIncidents(c, { now: nowMs, seconds: 12 * 3600 })
    const q = c.get.mock.calls.find(([p]) => p === 'metrics/query_range')[1]
    expect(Number(q.to) - Number(q.from)).toBe(12 * 3600)
    expect(q.step).toBe(60)
  })
  it('takes the objectives from the config: a looser one is not broken by the same outage, a stricter one is broken sooner', async () => {
    const loose = await listIncidents(client(), { now: nowMs, config: { availability: 0.5, latency: 0.5 } })
    expect(loose.incidents).toEqual([]) // nine bad minutes in an hour is 15%: well inside a 50% budget
    const strict = await listIncidents(client(), { now: nowMs, config: { availability: 0.99999, latency: 0.99999 } })
    expect(strict.incidents).toHaveLength(1)
    expect(strict.incidents[0].slo.availability.objective).toBe(0.99999)
  })
})

describe('listIncidents with an application\'s own objectives', () => {
  it('judges one application by its own objective and the rest by the general one', async () => {
    // frontend failing completely for nine minutes: with a 50% objective it is within budget, so it is not an incident
    const loose = await listIncidents(client(), { now: nowMs, appConfig: { '/k8s/shop/frontend': { availability: 0.5, latency: 0.5 } } })
    expect(loose.incidents).toEqual([])
    // another application's override does not touch it
    const other = await listIncidents(client(), { now: nowMs, appConfig: { '/k8s/shop/cartservice': { availability: 0.5, latency: 0.5 } } })
    expect(other.incidents).toHaveLength(1)
  })
  it('reports the objective it was judged by, and the burn threshold, in the incident', async () => {
    const r = await listIncidents(client(), { now: nowMs, config: { burnThreshold: 10 }, appConfig: { '/k8s/shop/frontend': { availability: 0.99 } } })
    expect(r.incidents[0].slo.availability.objective).toBe(0.99)
    expect(r.incidents[0].slo.latency.objective).toBe(0.999)
    expect(r.incidents[0].slo.burnThreshold).toBe(10)
  })
  it('does not take an override from an application name that happens to be an object property', async () => {
    const r = await listIncidents(client(), { now: nowMs, appConfig: JSON.parse('{"__proto__": {"availability": 0.01}}') })
    expect(r.incidents).toHaveLength(1)
  })
})

describe('windowsFor', () => {
  it('judges the incident itself and an hour before it, leaving out the minutes it was starting', () => {
    const w = windowsFor({ start: 10_000, end: 10_540 })
    expect(w.during).toEqual([10_000, 10_540])
    expect(w.before).toEqual([10_000 - 120 - 3600, 10_000 - 120])
  })
  it('stretches a very short incident to something that can be judged, and a long one to a longer baseline', () => {
    expect(windowsFor({ start: 10_000, end: 10_060 }).during).toEqual([10_000, 10_180])
    const long = windowsFor({ start: 100_000, end: 100_000 + 7200 })
    expect(long.before[1] - long.before[0]).toBe(21600)
  })
})

describe('loadIncident', () => {
  const incident = { id: 'x', app: '/k8s/shop/frontend', start: NOW - 5400, end: NOW - 5400 + 540, kind: ['availability'], slo: { latencySeconds: 0.5 } }
  const snapOf = (rttMs) => ({
    snap: {
      graph: { nodes: [], edges: [{ from: '/k8s/shop/frontend', to: '/k8s/shop/cartservice', rps: 20, connPerSec: 2, errPct: 0, p95ms: 10, connMs: 1, rttMs }] },
      apps: new Map(), inbound: new Map(),
    },
    seconds: 600, warnings: [],
  })
  const evidence = { traces: { failed: [], slow: [] }, logs: [], profile: null, warnings: [] }

  it('reads the two windows, analyses, narrates and gathers evidence for the cause it found', async () => {
    const snapshot = vi.fn(async (_c, [start]) => (start === incident.start ? snapOf(300) : snapOf(0.5)))
    const ev = vi.fn(async () => evidence)
    const r = await loadIncident({}, incident, { deps: { snapshot, evidence: ev } })
    expect(snapshot).toHaveBeenCalledTimes(2)
    expect(snapshot.mock.calls.map(([, w]) => w).sort((a, b) => a[0] - b[0])).toEqual([r.windows.before, r.windows.during])
    expect(r.findings.rootCause).toMatchObject({ kind: 'rtt', from: '/k8s/shop/frontend', to: '/k8s/shop/cartservice' })
    expect(r.narrative.title).toBe('`frontend` errors caused by increased network round-trip time between `frontend` and `cartservice`.')
    expect(ev).toHaveBeenCalledWith({}, expect.objectContaining({ app: '/k8s/shop/frontend', cause: expect.objectContaining({ kind: 'rtt' }), thresholdMs: 500 }))
    expect(r.evidence).toBe(evidence)
  })
  it('collects every warning once', async () => {
    const snapshot = async () => ({ ...snapOf(0.5), warnings: ['same warning'] })
    const r = await loadIncident({}, incident, { deps: { snapshot, evidence: async () => ({ ...evidence, warnings: ['same warning', 'logs: failed'] }) } })
    expect(r.warnings).toEqual(['same warning', 'logs: failed'])
  })
  it('fails as a whole when a window cannot be read, so a half-read analysis is not shown as an answer', async () => {
    const snapshot = vi.fn(async (_c, [start]) => { if (start === incident.start) throw new Error('metrics unavailable'); return snapOf(0.5) })
    await expect(loadIncident({}, incident, { deps: { snapshot, evidence: async () => evidence } })).rejects.toThrow('metrics unavailable')
  })
})
