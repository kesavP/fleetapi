import { describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { createRouter, createMemoryHistory } from 'vue-router'
import ServiceView from './ServiceView.vue'

const stub = { template: '<i/>' }
const session = (v = { project: 'gke', key: 'k' }) => ({ get: () => v })
const NOW = Date.now()
const iso = (agoSec) => new Date(NOW - agoSec * 1000).toISOString()

const spans = [
  { span_id: 'a', trace_id: 'a'.repeat(32), time: iso(5), name: 'GET /cart', kind: 'SERVER', duration_ns: 20e6, status_code: 'OK' },
  { span_id: 'b', trace_id: 'b'.repeat(32), time: iso(10), name: 'GET /cart', kind: 'SERVER', duration_ns: 80e6, status_code: 'ERROR', status_message: 'boom' },
  { span_id: 'c', trace_id: 'c'.repeat(32), time: iso(20), name: 'redis GET', kind: 'CLIENT', duration_ns: 1e6, status_code: 'OK' },
]
const logs = [
  { time: iso(8), severity: 'ERROR', severity_number: 17, body: 'cart failed' },
  { time: iso(30), severity: 'INFO', severity_number: 9, body: 'cart ok' },
]
const answers = { traces: spans, logs, profiles: [{ service: 'cartservice', type: 'cpu' }] }
const clientOf = (fail = {}, over = {}) => ({
  get: vi.fn(async (path) => {
    if (fail[path]) throw new Error(fail[path])
    return path in over ? over[path] : answers[path]
  }),
})

async function mountAt(name, props = {}) {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: stub }, { path: '/connection', component: stub },
      { path: '/applications', component: stub },
      { path: '/applications/:name', name: 'application', component: ServiceView },
      { path: '/traces', name: 'traces', component: stub },
      { path: '/traces/:id', name: 'trace', component: stub },
      { path: '/logs', name: 'logs', component: stub },
      { path: '/profiles', name: 'profiles', component: stub },
    ],
  })
  router.push(`/applications/${name}`)
  await router.isReady()
  const w = mount(ServiceView, { props: { client: clientOf(), session: session(), ...props }, global: { plugins: [router] } })
  await flushPromises()
  return { w, router }
}

describe('ServiceView', () => {
  it('shows the service, its figures per span kind, recent errors, spans, logs and profiles', async () => {
    const { w } = await mountAt('cartservice')
    expect(w.get('.svc-title').text()).toBe('cartservice')
    const server = w.get('[data-group=server]').text()
    expect(server).toContain('Spans2')
    expect(server).toContain('1 (50%)')
    expect(w.get('[data-group=client]').text()).toContain('Spans1')
    expect(w.text()).toContain('cart failed')
    expect(w.text()).toContain('boom')
    expect(w.text()).toContain('1 error, 0 warn, 1 info')
    expect(w.text()).toContain('cpu')
    expect(w.findAll('tbody tr')).toHaveLength(3)
  })

  it('links the service to all its traces and logs, and a span to its trace', async () => {
    const { w } = await mountAt('cartservice')
    const hrefs = w.findAll('a').map((a) => a.attributes('href'))
    expect(hrefs).toContain('/traces?service=cartservice')
    expect(hrefs).toContain('/logs?service=cartservice')
    expect(hrefs).toContain('/profiles?service=cartservice&type=cpu')
    expect(hrefs).toContain(`/traces/${'a'.repeat(32)}`)
  })

  it('asks for the service named in the address, and decodes it', async () => {
    const client = clientOf()
    await mountAt('my%20svc', { client })
    expect(client.get.mock.calls.find(([p]) => p === 'traces')[1].service).toBe('my svc')
  })

  it('does not call the API when not connected', async () => {
    const client = clientOf()
    const { w } = await mountAt('x', { client, session: session(null) })
    expect(client.get).not.toHaveBeenCalled()
    expect(w.text()).toContain('Not connected')
  })

  it('warns that figures are partial when the page is full', async () => {
    const many = Array.from({ length: 1000 }, (_, i) => ({ span_id: String(i), trace_id: '0'.repeat(32), time: iso(i * 0.5), name: 'n', kind: 'SERVER', duration_ns: 1e6, status_code: 'OK' }))
    const { w } = await mountAt('busy', { client: clientOf({}, { traces: many }) })
    expect(w.text()).toContain('more than 1000 spans')
    expect(w.text()).toContain('only the newest')
  })

  it('shows what loaded, and warns about what did not', async () => {
    const { w } = await mountAt('x', { client: clientOf({ logs: 'down' }) })
    expect(w.get('[role=status]').text()).toContain('logs')
    expect(w.find('[data-group=server]').exists()).toBe(true)
  })

  it('shows the reason when everything fails', async () => {
    const f = 'This key cannot read project "gke". Check the key and the project name.'
    const { w } = await mountAt('x', { client: clientOf({ traces: f, logs: f, profiles: f }) })
    expect(w.get('[role=alert]').text()).toContain('This key cannot read project')
  })

  it('says so for a service with nothing', async () => {
    const { w } = await mountAt('ghost', { client: clientOf({}, { traces: [], logs: [], profiles: [] }) })
    expect(w.text()).toContain('Nothing for this service')
    expect(w.find('.cards').exists()).toBe(false)
  })

  it('shows no figures when only logs exist', async () => {
    const { w } = await mountAt('logsonly', { client: clientOf({}, { traces: [] }) })
    expect(w.find('.cards').exists()).toBe(false)
    expect(w.text()).toContain('cart failed')
  })

  describe('metrics', () => {
    const SVC = '/k8s/shop/cartservice'
    const POD = '/k8s/shop/cartservice-7c9d8f6b5d-bk7q2/server'
    const FE = '/k8s/shop/frontend-5d6f8c7b9-xk2lp/server'
    const call = { container_id: FE, destination: '10.0.0.9:7070', actual_destination: '10.4.1.9:7070' }
    const flat = (n) => Array.from({ length: 40 }, (_, i) => ({ t: NOW - (40 - i) * 15000, v: n }))
    const last = (v) => [{ t: NOW - 1000, v }]
    const MET = {
      container_net_tcp_listen_info: [{ labels: { container_id: POD, listen_addr: '10.4.1.9:7070' }, points: last(1) }],
      container_net_tcp_successful_connects_total: [{ labels: call, points: flat(2) }],
      container_http_requests_total: [{ labels: { ...call, status: '2xx' }, points: flat(9) }, { labels: { ...call, status: '500' }, points: flat(1) }],
      container_resources_cpu_usage_seconds_total: [{ labels: { container_id: POD }, points: flat(0.25) }],
      container_resources_memory_rss_bytes: [{ labels: { container_id: POD }, points: last(64 * 1024 * 1024) }],
      container_resources_memory_limit_bytes: [{ labels: { container_id: POD }, points: last(128 * 1024 * 1024) }],
      container_restarts_total: [{ labels: { container_id: POD }, points: flat(0) }],
    }
    const metricsClient = (fail = {}, data = MET) => ({
      get: vi.fn(async (path, params) => {
        if (path === 'metrics') return fail.metrics ? Promise.reject(new Error(fail.metrics)) : Object.keys(data)
        if (path === 'metrics/query_range') return { series: data[params.name] || [] }
        return answers[path]
      }),
    })

    it('shows who calls the service, its error share, and its resources', async () => {
      const { w } = await mountAt(encodeURIComponent(SVC), { client: metricsClient() })
      const traffic = w.get('[data-section=traffic]').text()
      expect(traffic).toContain('10.0/s')
      expect(traffic).toContain('10.0%')
      const row = w.get('[data-table=in] tbody tr').text()
      expect(row).toContain('shop/frontend')
      expect(row).toContain('http')
      const res = w.get('[data-section=resources]').text()
      expect(res).toContain('64 MiB')
      expect(res).toContain('128 MiB')
      expect(res).toContain('0.25 cores')
      expect(res).toContain('Running1')
    })

    it('links a caller to its own page', async () => {
      const { w } = await mountAt(encodeURIComponent(SVC), { client: metricsClient() })
      expect(w.get('[data-table=in] a').attributes('href')).toBe('/applications/%2Fk8s%2Fshop%2Ffrontend')
    })

    it('does not query metrics for a name that is not a Kubernetes workload, and says why', async () => {
      const client = metricsClient()
      const { w } = await mountAt('plainname', { client })
      expect(client.get.mock.calls.some(([p]) => p === 'metrics')).toBe(false)
      expect(w.text()).toContain('not one')
    })

    it('shows the reason when the metrics cannot be read, and still shows spans and logs', async () => {
      const { w } = await mountAt(encodeURIComponent(SVC), { client: metricsClient({ metrics: 'The telemetry service is not reachable right now.' }) })
      expect(w.text()).toContain('The telemetry service is not reachable right now.')
      expect(w.find('[data-group=server]').exists()).toBe(true)
    })

    it('says so when the workload has no metrics', async () => {
      const { w } = await mountAt(encodeURIComponent('/k8s/shop/ghost'), { client: metricsClient() })
      expect(w.text()).toContain('No call or resource metrics')
    })
  })
})
