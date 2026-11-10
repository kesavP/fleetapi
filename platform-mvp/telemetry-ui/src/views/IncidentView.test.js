import { describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import IncidentView from './IncidentView.vue'
import { defaultSettings } from '../incidents/settings'

const stub = { template: '<i/>' }
const session = (v = { project: 'gke', key: 'k' }) => ({ get: () => v })
const FE = '/k8s/shop/frontend'
const CART = '/k8s/shop/cartservice'
const REDIS = '/k8s/shop/redis-cart'
const INCIDENT = {
  id: 'k3j2h1g0', app: FE, label: 'shop/frontend', kind: ['availability', 'latency'], start: Math.floor(Date.now() / 1000) - 6 * 3600, end: Math.floor(Date.now() / 1000) - 6 * 3600 + 540, open: false,
  slo: { availability: { objective: 0.999, compliance: 0.9705, burn1h: 30, burn5m: 28 }, latency: { objective: 0.999, compliance: 1, burn1h: 2, burn5m: 2 }, latencySeconds: 0.5 },
}
const issue = (o) => ({ scope: 'edge', from: CART, to: REDIS, kind: 'rtt', severity: 'critical', before: 0.4, during: 210, ratio: 525, unit: 'ms', ...o })
const ANALYSIS = {
  incident: INCIDENT,
  findings: {
    app: FE,
    issues: [issue(), { scope: 'app', app: FE, kind: 'errors', severity: 'warning', before: 0, during: 12, ratio: null, unit: '%' }],
    nodes: [
      { id: FE, name: 'frontend', label: 'shop/frontend', depth: 0, issues: [{ scope: 'app', app: FE, kind: 'errors' }] },
      { id: CART, name: 'cartservice', label: 'shop/cartservice', depth: 1, issues: [issue()] },
      { id: REDIS, name: 'redis-cart', label: 'shop/redis-cart', depth: 2, issues: [] },
    ],
    edges: [{ from: FE, to: CART, issues: [] }, { from: CART, to: REDIS, issues: [issue()] }],
    rootCause: { kind: 'rtt', scope: 'edge', from: CART, to: REDIS, confidence: 'likely', issue: issue(), path: [FE, CART, REDIS] },
    checked: { applications: 3, calls: 2, signals: ['request errors', 'network round trip'] },
  },
  narrative: {
    title: '`frontend` errors/latency caused by increased network round-trip time between `cartservice` and `redis-cart`.',
    summary: ['Elevated latency and errors in `frontend` trace back through `cartservice` to `redis-cart`. The `cartservice` → `redis-cart` network round-trip time rose from 0.4 ms to 210 ms (525x).'],
    propagation: 'The anomaly in `frontend` (12% of requests failing) propagates down the dependency chain, not from `frontend` itself.',
    path: [FE, CART, REDIS],
  },
  evidence: {
    traces: { failed: [{ traceId: 'a'.repeat(32), name: 'GET /cart', status: 'ERROR', message: 'context canceled', durationMs: 2100, time: '2026-01-01T10:00:00Z' }], slow: [] },
    logs: [{ service: CART, count: 12, sample: 'redis: timeout' }],
    profile: null,
    warnings: [],
  },
  windows: { before: [0, 3600], during: [3720, 4260] },
  warnings: [],
}

async function mountAt(id = 'k3j2h1g0', { list = vi.fn(async () => ({ incidents: [INCIDENT] })), analyse = vi.fn(async () => ANALYSIS), sess = session(), settings = { get: () => defaultSettings() } } = {}) {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: stub }, { path: '/configuration/connection', component: stub }, { path: '/incidents', name: 'incidents', component: stub },
      { path: '/incidents/:id', name: 'incident', component: IncidentView },
      { path: '/applications/:name', name: 'application', component: stub }, { path: '/traces/:id', name: 'trace', component: stub },
    ],
  })
  router.push(`/incidents/${id}`)
  await router.isReady()
  const w = mount(IncidentView, { props: { client: {}, session: sess, settings, loaders: { listIncidents: list, loadIncident: analyse } }, global: { plugins: [router] } })
  await flushPromises()
  return { w, list, analyse }
}

describe('IncidentView', () => {
  it('finds the incident by its id, analyses it, and titles the page with the cause', async () => {
    const { w, analyse } = await mountAt()
    expect(analyse).toHaveBeenCalledWith({}, INCIDENT, { config: expect.objectContaining({ disabled: [], rttRatio: 3 }) })
    expect(w.get('[data-testid=title]').text()).toContain('frontend errors/latency caused by increased network round-trip time between cartservice and redis-cart.')
    expect(w.get('[data-testid=title]').findAll('code').map((c) => c.text())).toEqual(['frontend', 'cartservice', 'redis-cart'])
  })
  it('shows when it began and ended, how long it lasted, and the application', async () => {
    const { w } = await mountAt()
    const meta = w.get('.inc-meta').text()
    expect(meta).toContain('Started:')
    expect(meta).toContain('Resolved:')
    expect(meta).toContain('Duration: 9 min')
    expect(meta).toContain('6 h')
    expect(w.get('.inc-meta a').attributes('href')).toBe('/applications/%2Fk8s%2Fshop%2Ffrontend')
  })
  it('says an incident that has not ended is still ongoing, not resolved', async () => {
    const { w } = await mountAt('k3j2h1g0', { list: async () => ({ incidents: [{ ...INCIDENT, open: true }] }), analyse: async () => ({ ...ANALYSIS, incident: { ...INCIDENT, open: true } }) })
    expect(w.get('.inc-meta').text()).toContain('Still ongoing')
    expect(w.get('.inc-meta').text()).not.toContain('Resolved')
  })
  it('tabulates each objective with its compliance and burn rates, red where the objective or the threshold is missed', async () => {
    const { w } = await mountAt()
    const rows = w.findAll('table.slo tbody tr')
    expect(rows[0].text()).toContain('99.9% of requests should not fail')
    expect(rows[0].text()).toContain('97.05%')
    expect(rows[0].text()).toContain('1h: 30')
    expect(rows[0].text()).toContain('5m: 28')
    expect(rows[0].text()).toContain('threshold: 14.4') // INCIDENT has no threshold of its own: the default
    expect(rows[0].findAll('b.bad')).toHaveLength(2)
    expect(rows[1].text()).toContain('faster than 500ms')
    expect(rows[1].find('b.bad').exists()).toBe(false)
    expect(rows[1].find('td.bad').exists()).toBe(false) // 100% against 99.9%
  })
  it('writes the root cause, the request path and the graph, and lists what changed', async () => {
    const { w } = await mountAt()
    expect(w.get('[data-testid=summary]').text()).toContain('trace back through cartservice to redis-cart')
    expect(w.get('[data-testid=path]').text()).toBe('Request path: frontend → cartservice → redis-cart')
    expect(w.findAll('.ig-card').map((c) => c.find('strong').text())).toEqual(['frontend', 'cartservice', 'redis-cart'])
    expect(w.findAll('.ig-card.culprit')).toHaveLength(2) // both ends of the cause
    expect(w.findAll('.ig-card.hurt')).toHaveLength(1)
    expect(w.get('.ig-card:nth-child(3)').text()).toContain('TCP network latency to redis-cart') // listed under the caller, as the reference does
    expect(w.findAll('.ig-line.bad')).toHaveLength(1)
    expect(w.get('.changes').text()).toContain('rose from 0.4 ms to 210 ms')
    expect(w.text()).toContain('Checked 3 applications and 2 calls')
  })
  it('hides the details when asked, keeping the cause', async () => {
    const { w } = await mountAt()
    await w.get('button.link[aria-expanded]').trigger('click')
    expect(w.find('.ig').exists()).toBe(false)
    expect(w.find('[data-testid=summary]').exists()).toBe(true)
  })
  it('shows the error logs of the cause, and the failed requests on the Traces tab with a link to each trace', async () => {
    const { w } = await mountAt()
    expect(w.get('.logline').text()).toContain('redis: timeout')
    await w.findAll('[role=tab]')[1].trigger('click')
    const row = w.get('table[aria-label="Failed requests"] tbody tr')
    expect(row.text()).toContain('context canceled')
    expect(row.text()).toContain('2.1 s')
    expect(row.get('a').attributes('href')).toBe('/traces/' + 'a'.repeat(32))
    expect(w.text()).toContain('None in the incident.') // no slow requests
  })
  it('shows where a CPU cause spends its time', async () => {
    const a = { ...ANALYSIS, evidence: { ...ANALYSIS.evidence, profile: { service: CART, total: 10, functions: [{ name: 'regex.match', self: 8, total: 9 }] } } }
    const { w } = await mountAt('k3j2h1g0', { analyse: async () => a })
    const row = w.get('table[aria-label="Hottest functions"] tbody tr')
    expect(row.text()).toContain('regex.match')
    expect(row.text()).toContain('80%')
  })
  it('says the incident is not there when the id is unknown, and does not analyse anything', async () => {
    const { w, analyse } = await mountAt('nope0000')
    expect(w.get('[data-testid=missing]').text()).toContain('not in the last 6 hours')
    expect(analyse).not.toHaveBeenCalled()
  })
  it('shows a failure instead of a half-made analysis', async () => {
    const { w } = await mountAt('k3j2h1g0', { analyse: async () => { throw new Error('metrics unavailable') } })
    expect(w.get('[role=alert]').text()).toContain('metrics unavailable')
    expect(w.find('[data-testid=title]').exists()).toBe(false)
  })
  it('analyses again on request', async () => {
    const { w, analyse } = await mountAt()
    await w.get('button[aria-label="Analyse again"]').trigger('click')
    await flushPromises()
    expect(analyse).toHaveBeenCalledTimes(2)
  })
  it('puts a name from the data in as text, never as markup', async () => {
    const a = { ...ANALYSIS, narrative: { ...ANALYSIS.narrative, title: '`<img src=x onerror=alert(1)>` errors.' }, evidence: { ...ANALYSIS.evidence, logs: [{ service: CART, count: 1, sample: '<script>alert(1)</script>' }] } }
    const { w } = await mountAt('k3j2h1g0', { analyse: async () => a })
    expect(w.find('img').exists()).toBe(false)
    expect(w.find('script').exists()).toBe(false)
    expect(w.get('[data-testid=title] code').text()).toBe('<img src=x onerror=alert(1)>')
  })
  it('asks nothing when not connected', async () => {
    const { w, list } = await mountAt('k3j2h1g0', { sess: session(null) })
    expect(list).not.toHaveBeenCalled()
    expect(w.text()).toContain('Not connected')
  })

  it('searches and analyses with the saved rules: the same ones that found the incident', async () => {
    const s = defaultSettings()
    Object.assign(s.slo, { latencyMs: 250 })
    s.checks.logs = false
    const { list, analyse } = await mountAt('k3j2h1g0', { settings: { get: () => s } })
    expect(list.mock.calls[0][1].config.latencySeconds).toBe(0.25)
    expect(analyse.mock.calls[0][2].config.disabled).toEqual(['logs'])
  })
  it('judges the burn rates against the threshold the incident was found with', async () => {
    const inc = { ...INCIDENT, slo: { ...INCIDENT.slo, burnThreshold: 40 } } // 1h 30 and 5m 28 are under it
    const { w } = await mountAt('k3j2h1g0', { list: async () => ({ incidents: [inc] }), analyse: async () => ({ ...ANALYSIS, incident: inc }) })
    const row = w.findAll('table.slo tbody tr')[0]
    expect(row.text()).toContain('threshold: 40')
    expect(row.findAll('b.bad')).toHaveLength(0)
  })
})
