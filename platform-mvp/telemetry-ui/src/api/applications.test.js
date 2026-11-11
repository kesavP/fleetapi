import { describe, expect, it, vi } from 'vitest'
import { alertLevels, buildApplications, categoryOf, createApplicationsCache, healthItems, loadApplications, namespaceOf, namespacesOf, statusOf, STATUSES } from './applications'
import { ApiError } from './client.js'

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

describe('applications on plain hosts', () => {
  const NGINX = '/system.slice/nginx.service'
  const PGD = '/docker/postgres'
  const gm = (container_id, machine_id, value, fresh = true) => ({ labels: { container_id, machine_id }, value, last: 1, fresh })
  const inventory = [gm(NGINX, 'vm-1', 100), gm(NGINX, 'vm-2', 100), gm(PGD, 'vm-1', 400), gm('/system.slice/old.service', 'vm-1', 5, false)]
  const apps = buildApplications({ inventory, seconds: 600 })
  const find = (id) => apps.find((a) => a.id === id)

  it('lists a unit or container on a host as an application, in the "host" namespace, named without the suffix', () => {
    expect(find(NGINX)).toMatchObject({ name: 'nginx', namespace: 'host', category: 'application', label: 'nginx' })
    expect(find(PGD)).toMatchObject({ name: 'postgres', namespace: 'host' })
  })

  it('counts machines as instances: one unit on two machines is two instances of one application', () => {
    expect(find(NGINX).instances).toEqual({ running: 2, seen: 2 })
    expect(find(NGINX).memoryBytes).toBe(200)
    expect(find(PGD).instances).toEqual({ running: 1, seen: 1 })
  })

  it('shows a unit that has stopped on every machine as down', () => {
    expect(find('/system.slice/old.service').instances).toEqual({ running: 0, seen: 1 })
    // not "critical" for having no instance: it is silent, and without an inventory all that can be said is that it stopped
    expect(statusOf(find('/system.slice/old.service'))).toBe('silent')
    expect(find('/system.slice/old.service').silence).toMatchObject({ cause: 'stopped' })
  })

  it('knows the namespace of a host application id, and keeps "other" for a plain name', () => {
    expect(namespaceOf(NGINX)).toBe('host')
    expect(namespaceOf('/docker/redis')).toBe('host')
    expect(namespaceOf('plain-service')).toBe('other')
    expect(categoryOf('host')).toBe('application')
  })
})

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
  it('does not judge zero running instances a failure by itself: only a silence found by silence.js is flagged', () => {
    expect(statusOf({ ...base, instances: { running: 0, seen: 2 } })).toBe('ok')
    expect(statusOf({ ...base, instances: null })).toBe('ok')
    expect(statusOf({ ...base, silence: { cause: 'scaled-to-zero', tone: 'info', label: 'Scaled to zero', detail: '' } })).toBe('silent')
  })
  it('puts silent under a critical alert and over everything else', () => {
    const silence = { cause: 'stopped', tone: 'caution', label: 'Stopped sending', detail: '' }
    expect(statusOf({ ...base, silence }, 'critical')).toBe('critical')
    expect(statusOf({ ...base, silence }, 'warning')).toBe('silent')
    expect(statusOf({ ...base, silence, restarts: 3, oomKills: 1, uniqueErrors: 5, traffic: { errPct: 50, rps: 1 } })).toBe('silent')
  })
  it('lists silent second, after critical, so the worst come first', () => {
    expect(STATUSES.map((x) => x.key)).toEqual(['critical', 'silent', 'warning', 'logs', 'nodata', 'ok'])
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
  const clientOf = ({ data = DATA, names = Object.keys(DATA), fail = {}, services = ['/k8s/shop/frontend'], logs = [], cluster = null, clusterError = null } = {}) => ({
    get: vi.fn(async (path, params) => {
      if (fail[path] || (params && fail[params.name])) throw new Error(fail[path] || fail[params.name])
      if (path.startsWith('inventory') && clusterError) throw clusterError
      if (path === 'inventory') return cluster ? cluster.summary : []
      if (path === 'inventory/workloads') return cluster.workloads
      if (path === 'inventory/pods') return cluster.pods || []
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

  describe('with the cluster inventory', () => {
    // the cart's last reading is five minutes old (not fresh); the frontend is sending
    const STALE_CART = { ...DATA, container_resources_memory_rss_bytes: [{ labels: { container_id: FE }, points: last(100) }, { labels: { container_id: CART }, points: [{ t: (END - 500) * 1000, v: 50 }] }] }
    const iso = (agoSeconds) => new Date(END * 1000 - agoSeconds * 1000).toISOString()
    const wl = (name, desired, ready) => ({ cluster: 'gke', kind: 'deployment', uid: name, namespace: 'shop', name, observed: iso(10), data: { desired, ready, available: ready } })
    const cluster = (workloads, pods = [], agoSeconds = 20) => ({ summary: [{ cluster: 'gke', last_received: iso(agoSeconds), last_collected: iso(agoSeconds), counts: {} }], workloads, pods })

    it('says why an application is silent, and lists a workload that has no series at all', async () => {
      const c = cluster([wl('frontend', 1, 1), wl('cartservice', 0, 0), wl('adservice', 1, 1), wl('paymentservice', 2, 0)], [
        { cluster: 'gke', kind: 'pod', namespace: 'shop', name: 'p', data: { ready: false, phase: 'Running', workload_kind: 'deployment', workload_name: 'paymentservice', containers: [{ state: 'waiting', reason: 'CrashLoopBackOff' }] } },
      ])
      const r2 = await loadApplications(clientOf({ data: STALE_CART, cluster: c }), { seconds: 600, now: NOW })
      const by = Object.fromEntries(r2.apps.map((a) => [a.name, a]))
      expect(by.frontend.silence).toBeNull()
      expect(by.cartservice.silence).toMatchObject({ cause: 'scaled-to-zero' })
      expect(by.adservice.silence).toMatchObject({ cause: 'no-data' }) // no series at all, but the cluster says it is ready
      expect(by.paymentservice.silence).toMatchObject({ cause: 'not-ready', detail: '0 of 2 ready: CrashLoopBackOff' })
      expect(by.cartservice.workload).toMatchObject({ desired: 0, ready: 0 })
      expect(r2.hasCluster).toBe(true)
      expect(r2.apps.map((a) => statusOf(a))).toEqual(r2.apps.map((a) => (a.silence ? 'silent' : statusOf(a))))
    })

    it('says an application that is no longer in the cluster was removed, not that it failed', async () => {
      const r2 = await loadApplications(clientOf({ data: STALE_CART, cluster: cluster([wl('frontend', 1, 1)]) }), { seconds: 600, now: NOW })
      expect(r2.apps.find((a) => a.name === 'cartservice').silence).toMatchObject({ cause: 'removed', tone: 'info' })
    })

    it('does not trust a collector that has gone quiet, and says so', async () => {
      const r2 = await loadApplications(clientOf({ data: STALE_CART, cluster: cluster([wl('frontend', 1, 1), wl('cartservice', 0, 0)], [], 1500) }), { seconds: 600, now: NOW })
      expect(r2.hasCluster).toBe(false)
      expect(r2.warnings.some((w) => w.includes('cluster collector last reported 25 minutes ago'))).toBe(true)
      expect(r2.apps.find((a) => a.name === 'cartservice').silence).toMatchObject({ cause: 'stopped' }) // all that can be said without it
      expect(r2.apps.map((a) => a.name)).not.toContain('adservice')
    })

    it('says nothing of silence for any application when none is sending: that is the agent, not the applications', async () => {
      const allStale = { ...DATA, container_resources_memory_rss_bytes: [{ labels: { container_id: FE }, points: [{ t: (END - 500) * 1000, v: 1 }] }, { labels: { container_id: CART }, points: [{ t: (END - 500) * 1000, v: 1 }] }] }
      const r2 = await loadApplications(clientOf({ data: allStale, cluster: cluster([wl('frontend', 1, 1), wl('cartservice', 3, 0)]) }), { seconds: 600, now: NOW })
      expect(r2.apps.every((a) => a.silence === null)).toBe(true)
    })

    it('works against a hub that does not have the inventory yet, with no warning for it', async () => {
      const r2 = await loadApplications(clientOf({ data: STALE_CART, clusterError: new ApiError('not found', { status: 404 }) }), { seconds: 600, now: NOW })
      expect(r2.hasCluster).toBe(false)
      expect(r2.warnings.some((w) => /inventory|collector/i.test(w))).toBe(false)
      expect(r2.apps.find((a) => a.name === 'cartservice').silence).toMatchObject({ cause: 'stopped' })
    })

    it('shows any other failure to read the inventory as a warning, and still shows the applications', async () => {
      const r2 = await loadApplications(clientOf({ data: STALE_CART, clusterError: new ApiError('the service failed', { status: 500 }) }), { seconds: 600, now: NOW })
      expect(r2.warnings).toContain('cluster inventory: the service failed')
      expect(r2.apps.length).toBe(2)
    })
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
    // the inventory failing as well changes nothing: it is an extra, not one of the reads that all failing means a bad key
    await expect(loadApplications(clientOf({ names, fail: all, clusterError: new Error('inventory down') }), { seconds: 600, now: NOW })).rejects.toThrow('denied')
  })

  it('warns that the list may be partial when a metric hit the series cap', async () => {
    const client = { get: async (p) => (p === 'metrics' ? ['container_resources_memory_rss_bytes'] : p === 'metrics/query_range' ? { truncated: true, series: [] } : []) }
    const r2 = await loadApplications(client, { seconds: 600, now: NOW })
    expect(r2.warnings.some((w) => w.includes('some applications may be missing'))).toBe(true)
  })
})

describe('DNS in the applications', () => {
  const dns = (container_id, domain, status, rate) => ({ labels: { container_id, domain, status, request_type: 'TypeA' }, rate })
  it('summarises the lookups of each application, and is null for one that made none', () => {
    const apps = buildApplications({ inventory: [g(FE, 1), g(CART, 1)], dns: [dns(FE, 'a.example', 'ok', 9), dns(FE, 'a.example', 'servfail', 1)], seconds: 60 })
    const fe = apps.find((a) => a.id === '/k8s/shop/frontend')
    expect(fe.dns).toMatchObject({ rps: 10, failedRps: 1 })
    expect(apps.find((a) => a.id === '/k8s/shop/cartservice').dns).toBeNull()
    expect(buildApplications({ inventory: [g(FE, 1)], seconds: 60 })[0].dns).toBeNull() // not read at all
  })
})

describe('healthItems', () => {
  const base = { instances: { running: 2, seen: 2 }, cpuCores: 0.2, cpuLimit: 1, memoryBytes: 100, memoryLimit: 1000, oomKills: 0, dns: { failedShare: 0 }, uniqueErrors: 0 }
  const by = (app) => Object.fromEntries(healthItems(app).map((i) => [i.key, i]))

  it('lists instances, CPU, memory, DNS and logs, each a place on the application\'s page', () => {
    const items = healthItems(base)
    expect(items.map((i) => [i.key, i.label, i.anchor])).toEqual([['instances', 'Instances', 'instances'], ['cpu', 'CPU', 'cpu'], ['memory', 'Memory', 'memory'], ['dns', 'DNS', 'dns'], ['logs', 'Logs', 'logs']])
    expect(items.every((i) => i.status === 'ok')).toBe(true)
  })

  it('judges instances: some missing is a caution, and none running is not called bad by itself', () => {
    expect(by({ ...base, instances: { running: 0, seen: 2 } }).instances).toMatchObject({ status: 'caution', hint: '0 of 2 running' })
    expect(by({ ...base, instances: { running: 1, seen: 2 } }).instances).toMatchObject({ status: 'caution', hint: '1 of 2 running' })
    expect(by({ ...base, instances: null }).instances.status).toBe('unknown')
  })

  it('takes what the silence says: why an application sends nothing, coloured by how much it needs looking at', () => {
    const silent = (tone, label, detail) => by({ ...base, silence: { tone, label, detail } }).instances
    expect(silent('bad', 'Not ready', '0 of 3 ready')).toMatchObject({ status: 'bad', hint: 'Not ready: 0 of 3 ready' })
    expect(silent('caution', 'Stopped sending', 'x')).toMatchObject({ status: 'caution', hint: 'Stopped sending: x' })
    expect(silent('info', 'Scaled to zero', 'y')).toMatchObject({ status: 'unknown', hint: 'Scaled to zero: y' })
  })

  it('judges CPU and memory against their limits, and says so when there is no limit', () => {
    expect(by({ ...base, cpuCores: 0.85 }).cpu).toMatchObject({ status: 'caution', hint: '85% of its limit' })
    expect(by({ ...base, cpuCores: 1.2 }).cpu.status).toBe('bad')
    expect(by({ ...base, cpuLimit: null }).cpu).toMatchObject({ status: 'ok', hint: 'no limit set' })
    expect(by({ ...base, cpuCores: null, cpuLimit: null }).cpu.status).toBe('unknown')
    expect(by({ ...base, memoryBytes: 850 }).memory.status).toBe('caution')
    expect(by({ ...base, memoryBytes: 960 }).memory.status).toBe('bad')
    expect(by({ ...base, memoryLimit: null }).memory.hint).toBe('no limit set')
  })

  it('treats an out-of-memory kill as bad whatever the usage', () => {
    expect(by({ ...base, oomKills: 2 }).memory).toMatchObject({ status: 'bad', hint: '2 out-of-memory kills' })
    expect(by({ ...base, oomKills: 1 }).memory.hint).toBe('1 out-of-memory kill')
  })

  it('judges DNS by the share of lookups that fail, and logs by unique errors', () => {
    expect(by({ ...base, dns: { failedShare: 0.02 } }).dns).toMatchObject({ status: 'caution', hint: '2% of lookups fail' })
    expect(by({ ...base, dns: { failedShare: 0.2 } }).dns.status).toBe('bad')
    expect(by({ ...base, dns: null }).dns.status).toBe('unknown')
    expect(by({ ...base, uniqueErrors: 1 }).logs).toMatchObject({ status: 'caution', hint: '1 unique error' })
    expect(by({ ...base, uniqueErrors: 3 }).logs.hint).toBe('3 unique errors')
  })
})

describe('createApplicationsCache', () => {
  const key = (seconds = 3600, project = 'gke') => ({ project, seconds })
  const setup = () => {
    let t = 1000
    const calls = []
    const load = vi.fn(async (client, { seconds }) => { calls.push(seconds); return { apps: [], seconds } })
    return { cache: createApplicationsCache({ ttlMs: 30_000, now: () => t }), load, calls, tick: (ms) => (t += ms) }
  }

  it('reads once and then answers from memory, for the same project and window', async () => {
    const { cache, load } = setup()
    const a = await cache.get({}, { ...key(), load })
    const b = await cache.get({}, { ...key(), load })
    expect(load).toHaveBeenCalledTimes(1)
    expect(b).toBe(a)
  })

  it('shares a load that is still going instead of starting another', async () => {
    const { cache, load } = setup()
    await Promise.all([cache.get({}, { ...key(), load }), cache.get({}, { ...key(), load })])
    expect(load).toHaveBeenCalledTimes(1)
  })

  it('keeps projects and windows apart', async () => {
    const { cache, load } = setup()
    await cache.get({}, { ...key(3600, 'gke'), load })
    await cache.get({}, { ...key(3600, 'dc1'), load })
    await cache.get({}, { ...key(900, 'gke'), load })
    expect(load).toHaveBeenCalledTimes(3)
  })

  it('reads again after it has expired, and when forced', async () => {
    const { cache, load, tick } = setup()
    await cache.get({}, { ...key(), load })
    tick(29_999)
    await cache.get({}, { ...key(), load })
    expect(load).toHaveBeenCalledTimes(1)
    tick(2)
    await cache.get({}, { ...key(), load })
    expect(load).toHaveBeenCalledTimes(2)
    await cache.get({}, { ...key(), load, force: true })
    expect(load).toHaveBeenCalledTimes(3)
  })

  it('forgets a load that failed, so the next call tries again', async () => {
    const { cache } = setup()
    const load = vi.fn().mockRejectedValueOnce(new Error('down')).mockResolvedValue({ apps: [] })
    await expect(cache.get({}, { ...key(), load })).rejects.toThrow('down')
    await Promise.resolve()
    await expect(cache.get({}, { ...key(), load })).resolves.toEqual({ apps: [] })
    expect(load).toHaveBeenCalledTimes(2)
  })

  it('can be emptied', async () => {
    const { cache, load } = setup()
    await cache.get({}, { ...key(), load })
    cache.clear()
    await cache.get({}, { ...key(), load })
    expect(load).toHaveBeenCalledTimes(2)
  })
})

describe('buildApplications with a cluster inventory', () => {
  const series = (c, fresh = true) => ({ labels: { container_id: c }, value: 1, last: 1, fresh })
  const FEP = '/k8s/shop/frontend-5d6f8c7b9-xk2lp/app'
  const OLD = '/k8s/shop/old-5d6f8c7b9-xk2lp/app'
  const cluster = (entries, reasons = {}) => ({ workloads: new Map(Object.entries(entries)), reasons: new Map(Object.entries(reasons)) })
  const w = (desired, ready, kind = 'deployment') => ({ kind, desired, ready, available: ready, observed: 1, clusters: 1 })

  it('includes a workload the metrics have never named, and its numbers', () => {
    const apps = buildApplications({ inventory: [series(FEP)], cluster: cluster({ '/k8s/shop/frontend': w(1, 1), '/k8s/shop/cartservice': w(0, 0) }) })
    const cart = apps.find((a) => a.name === 'cartservice')
    expect(cart).toMatchObject({ namespace: 'shop', instances: null, workload: { desired: 0, ready: 0, reasons: [] }, silence: { cause: 'scaled-to-zero' } })
    expect(apps.find((a) => a.name === 'frontend').silence).toBeNull()
  })

  it('carries the reasons the pods are not ready', () => {
    const apps = buildApplications({ inventory: [series(FEP), series('/k8s/shop/pay-5d6f8c7b9-xk2lp/app', false)], cluster: cluster({ '/k8s/shop/pay': w(2, 0) }, { '/k8s/shop/pay': ['ImagePullBackOff ×2'] }) })
    expect(apps.find((a) => a.name === 'pay')).toMatchObject({ silence: { cause: 'not-ready', detail: '0 of 2 ready: ImagePullBackOff ×2' }, workload: { reasons: ['ImagePullBackOff ×2'] } })
  })

  it('knows nothing is sending only from the container series: with none, nothing is silent', () => {
    expect(buildApplications({ inventory: null, cluster: cluster({ '/k8s/shop/cart': w(3, 0) }) })[0].silence).toBeNull()
    expect(buildApplications({ inventory: [series(FEP, false)], cluster: cluster({ '/k8s/shop/cart': w(3, 0) }) }).every((a) => a.silence === null)).toBe(true)
  })

  it('tells removed from stopped by whether there is an inventory', () => {
    const withCluster = buildApplications({ inventory: [series(FEP), series(OLD, false)], cluster: cluster({ '/k8s/shop/frontend': w(1, 1) }) })
    expect(withCluster.find((a) => a.name === 'old').silence).toMatchObject({ cause: 'removed' })
    const without = buildApplications({ inventory: [series(FEP), series(OLD, false)] })
    expect(without.find((a) => a.name === 'old').silence).toMatchObject({ cause: 'stopped' })
    // a stale collector is no inventory
    const stale = buildApplications({ inventory: [series(FEP), series(OLD, false)], cluster: { workloads: null, reasons: new Map(), stale: { seconds: 900 } } })
    expect(stale.find((a) => a.name === 'old').silence).toMatchObject({ cause: 'stopped' })
  })

  it('never calls a cron job silent', () => {
    const apps = buildApplications({ inventory: [series(FEP)], cluster: cluster({ '/k8s-cronjob/shop/cleanup': w(0, 0, 'cronjob') }) })
    expect(apps.find((a) => a.name === 'cleanup').silence).toBeNull()
  })
})
