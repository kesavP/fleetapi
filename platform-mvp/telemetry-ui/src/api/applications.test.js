import { describe, expect, it, vi } from 'vitest'
import { alertLevels, buildApplications, categoryOf, loadApplications, namespaceOf, namespacesOf, statusOf } from './applications'

const FE_POD = '/k8s/shop/frontend-5d6f8c7b9-xk2lp'
const FE = `${FE_POD}/server`
const FE_SIDE = `${FE_POD}/istio-proxy`
const FE2 = '/k8s/shop/frontend-5d6f8c7b9-bk7q2/server'
const CART = '/k8s/shop/cartservice-7c9d8f6b5d-bk7q2/server'
const PG = '/k8s/data/postgres-0/postgres'
const OLD = '/k8s/shop/legacy-7c9d8f6b5d-zzzzz/app' // a workload whose pod has stopped reporting

const g = (container_id, value, fresh = true, extra = {}) => ({ labels: { container_id, ...extra }, value, last: 1, fresh })
const r = (container_id, rate) => ({ labels: { container_id }, rate })
const edge = (from, to, o = {}) => ({ from, to, rps: 10, connPerSec: 1, errPct: 0, attempts: 10, failures: 0, failedConnects: 0, callErrors: 0, errBasis: 'http', p95ms: null, protocol: 'http', protocols: ['http'], unit: 'req/s', ...o })

describe('namespaces', () => {
  it('reads the namespace from a workload id, and puts anything else under "other"', () => {
    expect(namespaceOf('/k8s/shop/frontend')).toBe('shop')
    expect(namespaceOf('/k8s-cronjob/ops/nightly')).toBe('ops')
    expect(namespaceOf('plain-service')).toBe('other')
    expect(namespaceOf('')).toBe('other')
  })
  it('sorts namespaces into the platform\'s, the watchers\', and the user\'s', () => {
    for (const n of ['kube-system', 'kube-public', 'gke-managed-system', 'gmp-system', 'istio-system', 'ingress-nginx']) expect(categoryOf(n)).toBe('control-plane')
    for (const n of ['monitoring', 'observability', 'prometheus-stack', 'telemetry-agent', 'coroot']) expect(categoryOf(n)).toBe('monitoring')
    for (const n of ['shop', 'default', 'payments', 'kubernetes-dashboard-app', 'other']) expect(categoryOf(n)).toBe('application')
  })
  it('counts applications per namespace', () => {
    expect(namespacesOf([{ namespace: 'b' }, { namespace: 'a' }, { namespace: 'b' }])).toEqual([
      { name: 'a', category: 'application', count: 1 },
      { name: 'b', category: 'application', count: 2 },
    ])
  })
})

describe('buildApplications', () => {
  const inventory = [g(FE, 100), g(FE_SIDE, 20), g(FE2, 100), g(CART, 50), g(PG, 400), g(OLD, 30, false)]
  const graph = {
    nodes: [
      { id: '/k8s/shop/frontend', external: false, serves: [] },
      { id: '/k8s/shop/cartservice', external: false, serves: [] },
      { id: '/k8s/data/postgres', external: false, serves: ['postgres'] },
      { id: 'external:1.2.3.4:443', external: true, serves: [] },
    ],
    edges: [
      edge('/k8s/shop/frontend', '/k8s/shop/cartservice', { rps: 30, attempts: 30, failures: 3, errPct: 10 }),
      edge('/k8s/shop/cartservice', '/k8s/data/postgres', { rps: 5, protocols: ['postgres'], protocol: 'postgres', unit: 'queries/s' }),
      edge('/k8s/shop/frontend', 'external:1.2.3.4:443', { rps: 1 }),
    ],
  }
  const build = (o = {}) => buildApplications({ inventory, graph, seconds: 600, ...o })
  const app = (apps, id) => apps.find((a) => a.id === id)

  it('lists every workload the node-agent has a container for, plus the call map\'s and the traces\' and logs\' names, once', () => {
    const apps = build({ names: ['/k8s/shop/frontend', '/k8s/shop/traced-only'] })
    expect(apps.map((a) => a.id)).toEqual(['/k8s/data/postgres', '/k8s/shop/cartservice', '/k8s/shop/frontend', '/k8s/shop/legacy', '/k8s/shop/traced-only'])
    expect(apps.some((a) => a.id.startsWith('external:'))).toBe(false)
  })

  it('counts pods, not containers, and tells a pod that stopped from one that is running', () => {
    const apps = build()
    expect(app(apps, '/k8s/shop/frontend').instances).toEqual({ running: 2, seen: 2 }) // two pods; the sidecar is not a third
    expect(app(apps, '/k8s/shop/legacy').instances).toEqual({ running: 0, seen: 1 })
    expect(app(apps, '/k8s/shop/traced-only')).toBeUndefined()
  })

  it('adds up memory of the running containers only, and CPU as mean cores', () => {
    const apps = build({ cpu: [r(FE, 0.1), r(FE2, 0.2), r(CART, 0.5)], memLimit: [g(FE, 200), g(FE2, 200)], cpuLimit: [g(FE, 0)] })
    const fe = app(apps, '/k8s/shop/frontend')
    expect(fe.memoryBytes).toBe(220) // 100 + 20 + 100
    expect(fe.cpuCores).toBeCloseTo(0.3, 9)
    expect(fe.memoryLimit).toBe(400)
    expect(fe.cpuLimit).toBeNull() // a limit of 0 is none
    expect(app(apps, '/k8s/shop/legacy').memoryBytes).toBeNull() // nothing running, so nothing in use
  })

  it('reports restarts and OOM kills as counts in the window', () => {
    const apps = build({ restarts: [r(FE, 2 / 600), r(CART, 0)], oom: [r(FE2, 1 / 600)] })
    expect(app(apps, '/k8s/shop/frontend')).toMatchObject({ restarts: 2, oomKills: 1 })
    expect(app(apps, '/k8s/shop/cartservice')).toMatchObject({ restarts: 0, oomKills: null })
  })

  it('takes traffic and error share from the calls into the application, and upstreams from the calls out of it', () => {
    const apps = build()
    expect(app(apps, '/k8s/shop/cartservice').traffic).toMatchObject({ rps: 30, callers: 1 })
    expect(app(apps, '/k8s/shop/cartservice').traffic.errPct).toBeCloseTo(10, 9)
    expect(app(apps, '/k8s/shop/frontend').upstreams).toEqual({ ok: 1, total: 2 }) // the call to cartservice is failing (10%)
    expect(app(apps, '/k8s/shop/frontend').traffic).toBeNull() // nothing calls the frontend
    expect(app(apps, '/k8s/shop/cartservice').upstreams).toEqual({ ok: 1, total: 1 })
  })

  it('counts an upstream that is failing as not ok', () => {
    const g2 = { ...graph, edges: [edge('/k8s/shop/frontend', '/k8s/shop/cartservice', { errPct: 40 }), edge('/k8s/shop/frontend', '/k8s/data/postgres')] }
    expect(app(build({ graph: g2 }), '/k8s/shop/frontend').upstreams).toEqual({ ok: 1, total: 2 })
  })

  it('names the application\'s type from the node-agent, else from the protocol it serves', () => {
    const apps = build({ appTypes: [g(PG, 1, true, { application_type: 'postgres' }), g(CART, 1, true, { application_type: 'unknown' })] })
    expect(app(apps, '/k8s/data/postgres').type).toBe('postgres')
    expect(app(apps, '/k8s/shop/cartservice').type).toBe('') // "unknown" is not a type
    expect(app(build(), '/k8s/data/postgres').type).toBe('postgres') // from what it serves on the map
  })

  it('counts unique errors in the logs, grouping lines that differ only in numbers', () => {
    const lines = [
      { service: '/k8s/shop/cartservice', body: 'timeout after 30 ms for user 5' },
      { service: '/k8s/shop/cartservice', body: 'timeout after 41 ms for user 9' },
      { service: '/k8s/shop/cartservice', body: 'connection refused' },
      { service: '/k8s/shop/frontend', body: 'boom' },
      { body: 'no service' },
    ]
    const apps = build({ errorLogs: lines })
    expect(app(apps, '/k8s/shop/cartservice')).toMatchObject({ errorLines: 3, uniqueErrors: 2 })
    expect(app(apps, '/k8s/shop/frontend')).toMatchObject({ errorLines: 1, uniqueErrors: 1 })
    expect(app(apps, '/k8s/data/postgres')).toMatchObject({ errorLines: 0, uniqueErrors: 0 })
  })

  it('shows "–" (null), never a made-up zero, for what was not read', () => {
    const apps = buildApplications({ inventory: [g(FE, 1)], seconds: 60 })
    expect(apps[0]).toMatchObject({ cpuCores: null, restarts: null, oomKills: null, traffic: null, upstreams: null, memoryLimit: null })
  })

  it('is empty for nothing', () => {
    expect(buildApplications({})).toEqual([])
  })
})

describe('statusOf / alertLevels', () => {
  const base = { instances: { running: 1, seen: 1 }, restarts: 0, oomKills: 0, traffic: null, uniqueErrors: 0, hasMetrics: true }
  it('is worst first', () => {
    expect(statusOf(base)).toBe('ok')
    expect(statusOf({ ...base, uniqueErrors: 2 })).toBe('logs')
    expect(statusOf({ ...base, uniqueErrors: 2, restarts: 1 })).toBe('warning')
    expect(statusOf({ ...base, oomKills: 1 })).toBe('warning')
    expect(statusOf({ ...base, traffic: { errPct: 1, rps: 5 } })).toBe('warning')
    expect(statusOf({ ...base, traffic: { errPct: 0.9, rps: 5 } })).toBe('ok')
    expect(statusOf(base, 'warning')).toBe('warning')
    expect(statusOf({ ...base, restarts: 3 }, 'critical')).toBe('critical')
  })
  it('is critical when it has pods and none are running, without waiting for an alert', () => {
    expect(statusOf({ ...base, instances: { running: 0, seen: 2 } })).toBe('critical')
    expect(statusOf({ ...base, instances: null })).toBe('ok')
  })
  it('is "no data" when only a name is known', () => {
    expect(statusOf({ ...base, instances: null, hasMetrics: false })).toBe('nodata')
  })
  it('takes the worst firing alert per application, ignoring pending, resolved and project-wide ones', () => {
    const m = alertLevels([
      { service: 'a', severity: 'warning', status: 'firing' },
      { service: 'a', severity: 'critical', status: 'firing' },
      { service: 'b', severity: 'warning', status: 'firing' },
      { service: 'c', severity: 'critical', status: 'pending' },
      { service: 'd', severity: 'critical', status: 'resolved' },
      { service: '*', severity: 'critical', status: 'firing' },
    ])
    expect([...m]).toEqual([['a', 'critical'], ['b', 'warning']])
  })
})

describe('loadApplications', () => {
  const START = 1_700_000_040
  const END = START + 600
  const NOW = () => END * 1000
  const flat = (v) => Array.from({ length: 40 }, (_, i) => ({ t: (START + i * 15) * 1000, v }))
  const last = (v) => [{ t: (END - 5) * 1000, v }]
  const DATA = {
    container_resources_memory_rss_bytes: [{ labels: { container_id: FE }, points: last(100) }, { labels: { container_id: CART }, points: last(50) }],
    container_resources_cpu_usage_seconds_total: [{ labels: { container_id: FE }, points: flat(0.25) }],
    container_restarts_total: [{ labels: { container_id: CART }, points: flat(0) }],
  }
  const clientOf = ({ data = DATA, names = Object.keys(DATA), fail = {}, services = ['/k8s/shop/frontend'], logs = [] } = {}) => ({
    get: vi.fn(async (path, params) => {
      if (fail[path] || (params && fail[params.name])) throw new Error(fail[path] || fail[params.name])
      if (path === 'metrics') return names
      if (path === 'metrics/query_range') return { series: data[params.name] || [] }
      if (path === 'traces/services') return services
      if (path === 'logs/services') return []
      if (path === 'logs') return logs
      throw new Error('unexpected ' + path)
    }),
  })

  it('builds the applications from one read of each metric, however many applications there are', async () => {
    const client = clientOf()
    const r2 = await loadApplications(client, { seconds: 600, now: NOW })
    expect(r2.apps.map((a) => a.id)).toEqual(['/k8s/shop/cartservice', '/k8s/shop/frontend'])
    const fe = r2.apps.find((a) => a.name === 'frontend')
    expect(fe).toMatchObject({ instances: { running: 1, seen: 1 }, memoryBytes: 100 })
    expect(fe.cpuCores).toBeCloseTo(0.25, 9)
    const asked = client.get.mock.calls.filter(([p]) => p === 'metrics/query_range').map(([, q]) => q.name)
    expect(new Set(asked).size).toBe(asked.length) // each metric once
    expect(r2.hasInventory).toBe(true)
  })

  it('reads the error logs once, for the window', async () => {
    const client = clientOf({ logs: [{ service: '/k8s/shop/frontend', body: 'boom' }] })
    const r2 = await loadApplications(client, { seconds: 600, now: NOW })
    const call = client.get.mock.calls.find(([p]) => p === 'logs')
    expect(call[1]).toEqual({ severity: 'ERROR', from: START, to: END, limit: 1000 })
    expect(r2.apps.find((a) => a.name === 'frontend').uniqueErrors).toBe(1)
  })

  it('queries only the metrics the project has', async () => {
    const client = clientOf({ names: ['container_resources_memory_rss_bytes'] })
    await loadApplications(client, { seconds: 600, now: NOW })
    const asked = client.get.mock.calls.filter(([p]) => p === 'metrics/query_range').map(([, q]) => q.name)
    expect(asked).toEqual(['container_resources_memory_rss_bytes'])
  })

  it('still lists applications the traces name when the node-agent has no inventory', async () => {
    const r2 = await loadApplications(clientOf({ names: [], services: ['/k8s/shop/frontend'] }), { seconds: 600, now: NOW })
    expect(r2.apps.map((a) => [a.id, a.hasMetrics])).toEqual([['/k8s/shop/frontend', false]])
    expect(r2.hasInventory).toBe(false)
  })

  it('keeps the rest, with a warning, when one metric cannot be read', async () => {
    const r2 = await loadApplications(clientOf({ fail: { container_restarts_total: 'down' } }), { seconds: 600, now: NOW })
    expect(r2.warnings).toContain('container_restarts_total: down')
    expect(r2.apps.find((a) => a.name === 'cartservice').restarts).toBeNull()
    expect(r2.apps.find((a) => a.name === 'frontend').memoryBytes).toBe(100)
  })

  it('fails with the reason when the metric list cannot be read, or everything fails', async () => {
    await expect(loadApplications(clientOf({ fail: { metrics: 'This key cannot read project "x".' } }), { seconds: 600, now: NOW })).rejects.toThrow('cannot read project')
    const names = [...Object.keys(DATA), 'container_http_requests_total'] // so the call map has a counter to fail on too
    const all = { 'traces/services': 'denied', 'logs/services': 'denied', logs: 'denied', container_http_requests_total: 'denied', ...Object.fromEntries(Object.keys(DATA).map((k) => [k, 'denied'])) }
    await expect(loadApplications(clientOf({ names, fail: all }), { seconds: 600, now: NOW })).rejects.toThrow('denied')
  })

  it('warns that the list may be partial when a metric hit the series cap', async () => {
    const client = { get: async (p) => (p === 'metrics' ? ['container_resources_memory_rss_bytes'] : p === 'metrics/query_range' ? { truncated: true, series: [] } : []) }
    const r2 = await loadApplications(client, { seconds: 600, now: NOW })
    expect(r2.warnings.some((w) => w.includes('some applications may be missing'))).toBe(true)
  })
})
