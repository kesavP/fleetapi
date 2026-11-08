import { describe, expect, it, vi } from 'vitest'
import { hasMetrics, loadServiceMetrics, summarizeResources, trafficFor } from './serviceMetrics'

describe('hasMetrics', () => {
  it('is true only for names the node-agent gives a pod workload', () => {
    for (const n of ['/k8s/shop/frontend', '/k8s/data/redis', '/k8s-cronjob/ops/nightly']) expect(hasMetrics(n)).toBe(true)
    for (const n of ['frontend', '/system.slice/sshd.service', '']) expect(hasMetrics(n)).toBe(false)
  })
})

const edge = (from, to, o = {}) => ({ from, to, rps: 10, connPerSec: 1, errPct: 0, attempts: 10, failures: 0, errBasis: 'http', p95ms: null, protocol: 'http', protocols: ['http'], unit: 'req/s', ...o })
const GRAPH = {
  nodes: [
    { id: '/k8s/shop/a', label: 'shop/a' },
    { id: '/k8s/shop/b', label: 'shop/b' },
    { id: '/k8s/shop/c', label: 'shop/c' },
  ],
  edges: [
    edge('/k8s/shop/a', '/k8s/shop/b', { rps: 30, attempts: 30, failures: 3, errPct: 10 }),
    edge('/k8s/shop/c', '/k8s/shop/b', { rps: 10, attempts: 10, failures: 0 }),
    edge('/k8s/shop/b', '/k8s/shop/c', { rps: 5, attempts: 5, failures: 0 }),
  ],
}

describe('trafficFor', () => {
  it('splits the edges into callers and targets of the service', () => {
    const t = trafficFor(GRAPH, '/k8s/shop/b')
    expect(t.inbound.map((e) => e.peer)).toEqual(['/k8s/shop/a', '/k8s/shop/c']) // the failing one first
    expect(t.outbound.map((e) => e.peer)).toEqual(['/k8s/shop/c'])
    expect(t.inbound[0].peerLabel).toBe('shop/a')
  })

  it('totals from counts, not by adding percentages', () => {
    const { inbound } = trafficFor(GRAPH, '/k8s/shop/b').totals
    // 3 failures of 40 attempts, not (10% + 0%) or their mean.
    expect(inbound).toMatchObject({ rps: 40, edges: 2 })
    expect(inbound.errPct).toBeCloseTo(7.5, 9)
  })

  it('is empty for a service that is not on the map', () => {
    const t = trafficFor(GRAPH, '/k8s/shop/zzz')
    expect(t.inbound).toEqual([])
    expect(t.totals.inbound).toEqual({ rps: 0, errPct: 0, edges: 0 })
  })

  it('counts a connection-only edge (no calls seen) by its attempts', () => {
    const g = { nodes: [], edges: [edge('/k8s/shop/a', '/k8s/shop/b', { rps: null, attempts: 4, failures: 3, errPct: 75 })] }
    expect(trafficFor(g, '/k8s/shop/b').totals.inbound).toMatchObject({ rps: 0, errPct: 75 })
  })
})

describe('summarizeResources', () => {
  const POD1 = '/k8s/shop/b-5d6f8c7b9-xk2lp/app'
  const POD2 = '/k8s/shop/b-5d6f8c7b9-bk7q2/app'
  const OTHER = '/k8s/shop/c-5d6f8c7b9-xk2lp/app'
  const g = (c, value) => ({ labels: { container_id: c }, value })
  const r = (c, rate) => ({ labels: { container_id: c }, rate })

  it('sums this workload\'s pods and ignores other workloads', () => {
    const out = summarizeResources('/k8s/shop/b', {
      cpu: [r(POD1, 0.1), r(POD2, 0.2), r(OTHER, 9)],
      cpuLimit: [g(POD1, 0.5), g(POD2, 0.5), g(OTHER, 4)],
      rss: [g(POD1, 100), g(POD2, 50), g(OTHER, 999)],
      memLimit: [g(POD1, 200), g(POD2, 200)],
      restarts: [r(POD1, 1 / 600), r(OTHER, 1)],
      oom: [r(POD2, 0)],
    }, 600)
    expect(out).toMatchObject({ instances: 2, cpuLimitCores: 1, memoryBytes: 150, memoryLimitBytes: 400, restarts: 1, oomKills: 0 })
    expect(out.cpuCores).toBeCloseTo(0.3, 9)
  })

  it('treats a limit of 0 (none set) as no limit', () => {
    const out = summarizeResources('/k8s/shop/b', { rss: [g(POD1, 1)], cpuLimit: [g(POD1, 0)], memLimit: [g(POD1, 0)] }, 60)
    expect(out.cpuLimitCores).toBeNull()
    expect(out.memoryLimitBytes).toBeNull()
  })

  it('is null for a metric the project does not have, and for a workload with no series', () => {
    const none = summarizeResources('/k8s/shop/b', {}, 60)
    expect(Object.values(none).every((v) => v === null)).toBe(true)
    const other = summarizeResources('/k8s/shop/b', { rss: [g(OTHER, 5)], cpu: [r(OTHER, 1)] }, 60)
    expect(other).toMatchObject({ instances: null, cpuCores: null, memoryBytes: null })
  })
})

describe('loadServiceMetrics', () => {
  const START = 1_700_000_040
  const END = START + 600
  const NOW = () => END * 1000
  const POD = '/k8s/shop/b-5d6f8c7b9-xk2lp/app'
  const flat = (n) => Array.from({ length: 40 }, (_, i) => ({ t: (START + i * 15) * 1000, v: n }))
  const fresh = (v) => [{ t: (END - 5) * 1000, v }]
  const DATA = {
    container_resources_cpu_usage_seconds_total: [{ labels: { container_id: POD }, points: flat(0.5) }],
    container_resources_memory_rss_bytes: [{ labels: { container_id: POD }, points: fresh(1024) }],
    container_resources_memory_limit_bytes: [{ labels: { container_id: POD }, points: fresh(2048) }],
    container_restarts_total: [{ labels: { container_id: POD }, points: flat(0) }],
  }
  const clientOf = ({ data = DATA, fail = {}, names = Object.keys(data) } = {}) => ({
    get: vi.fn(async (path, params) => {
      if (path === 'metrics') {
        if (fail.metrics) throw new Error(fail.metrics)
        return names
      }
      if (fail[params.name]) throw new Error(fail[params.name])
      return { series: data[params.name] || [] }
    }),
  })

  it('reads the resources of the workload the name points to', async () => {
    const r = await loadServiceMetrics(clientOf(), '/k8s/shop/b', { seconds: 600, now: NOW })
    expect(r.found).toBe(true)
    expect(r.resources).toMatchObject({ instances: 1, memoryBytes: 1024, memoryLimitBytes: 2048, restarts: 0 })
    expect(r.resources.cpuCores).toBeCloseTo(0.5, 9)
    expect(r.traffic).toEqual({ inbound: [], outbound: [], totals: { inbound: { rps: 0, errPct: 0, edges: 0 }, outbound: { rps: 0, errPct: 0, edges: 0 } } })
  })

  it('fetches the metric list once and queries only what is in it', async () => {
    const client = clientOf()
    await loadServiceMetrics(client, '/k8s/shop/b', { seconds: 600, now: NOW })
    expect(client.get.mock.calls.filter(([p]) => p === 'metrics')).toHaveLength(1)
    const asked = new Set(client.get.mock.calls.filter(([p]) => p === 'metrics/query_range').map(([, q]) => q.name))
    expect([...asked].every((n) => n in DATA)).toBe(true)
  })

  it('leaves out a pod that stopped reporting', async () => {
    const stale = { ...DATA, container_resources_memory_rss_bytes: [{ labels: { container_id: POD }, points: [{ t: START * 1000, v: 999 }] }] }
    const r = await loadServiceMetrics(clientOf({ data: stale }), '/k8s/shop/b', { seconds: 600, now: NOW })
    expect(r.resources.memoryBytes).toBeNull()
  })

  it('keeps the rest and warns when one metric cannot be read', async () => {
    const r = await loadServiceMetrics(clientOf({ fail: { container_restarts_total: 'down' } }), '/k8s/shop/b', { seconds: 600, now: NOW })
    expect(r.resources.restarts).toBeNull()
    expect(r.resources.memoryBytes).toBe(1024)
    expect(r.warnings).toContain('container_restarts_total: down')
  })

  it('fails with the reason when the metric list fails', async () => {
    await expect(loadServiceMetrics(clientOf({ fail: { metrics: 'This key cannot read project "x".' } }), '/k8s/shop/b', { seconds: 600, now: NOW })).rejects.toThrow('cannot read project')
  })

  it('fails when the call counters fail and there is nothing else to show', async () => {
    const client = clientOf({ names: ['container_http_requests_total'], fail: { container_http_requests_total: 'denied' }, data: {} })
    await expect(loadServiceMetrics(client, '/k8s/shop/b', { seconds: 600, now: NOW })).rejects.toThrow('denied')
  })

  it('shows what it has, with the warnings, when every resource metric fails but the map could be built', async () => {
    const all = Object.fromEntries(Object.keys(DATA).map((n) => [n, 'denied']))
    const r = await loadServiceMetrics(clientOf({ fail: all }), '/k8s/shop/b', { seconds: 600, now: NOW })
    expect(r.found).toBe(false)
    expect(r.warnings.filter((w) => w.endsWith(': denied'))).toHaveLength(Object.keys(DATA).length)
  })

  it('says nothing was found for a workload with no metrics', async () => {
    const r = await loadServiceMetrics(clientOf(), '/k8s/shop/ghost', { seconds: 600, now: NOW })
    expect(r.found).toBe(false)
  })
})
