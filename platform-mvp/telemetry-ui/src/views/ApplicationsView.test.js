import { describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { createRouter, createMemoryHistory } from 'vue-router'
import { reactive } from 'vue'
import ApplicationsView from './ApplicationsView.vue'

const stub = { template: '<i/>' }
const session = (v = { project: 'gke', key: 'k' }) => ({ get: () => v })
const NOW = Date.now()
const last = (v) => [{ t: NOW - 1000, v }]
const flat = (v) => Array.from({ length: 40 }, (_, i) => ({ t: NOW - (40 - i) * 15000, v }))
const pod = (ns, name, hash = '5d6f8c7b9', suffix = 'xk2lp', container = 'app') => `/k8s/${ns}/${name}-${hash}-${suffix}/${container}`
const FE = pod('shop', 'frontend')
const CART = pod('shop', 'cartservice', '7c9d8f6b5d', 'bk7q2')
const PAY = pod('payments', 'ledger', '6c5d8f7b9d', 'zq9wn')
const PROM = pod('monitoring', 'prometheus', '5b8d7f6c9', 'mn4pq')
const KUBE = pod('kube-system', 'coredns', '6d4cf56db6', 'tb8qs')

const rss = (c, v = 1024 * 1024 * 64) => ({ labels: { container_id: c }, points: last(v) })
const DATA = {
  container_resources_memory_rss_bytes: [rss(FE), rss(CART), rss(PAY), rss(PROM), rss(KUBE)],
  container_resources_cpu_usage_seconds_total: [{ labels: { container_id: FE }, points: flat(0.25) }],
  container_restarts_total: [{ labels: { container_id: CART }, points: flat(0.01) }],
  container_application_type: [{ labels: { container_id: PROM, application_type: 'prometheus' }, points: last(1) }],
}
const clientOf = ({ data = DATA, names = Object.keys(data), fail = {}, logs = [] } = {}) => ({
  get: vi.fn(async (path, params) => {
    if (fail[path] || (params && fail[params.name])) throw new Error(fail[path] || fail[params.name])
    if (path === 'metrics') return names
    if (path === 'metrics/query_range') return { series: data[params.name] || [] }
    if (path === 'traces/services') return []
    if (path === 'logs/services') return []
    if (path === 'logs') return logs
    throw new Error('unexpected ' + path)
  }),
})

async function mountAt(query = '', props = {}) {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: stub }, { path: '/connection', component: stub },
      { path: '/connection', component: stub },
      { path: '/applications', name: 'applications', component: ApplicationsView },
      { path: '/applications/:name', name: 'application', component: stub },
      { path: '/logs', name: 'logs', component: stub },
    ],
  })
  router.push('/applications' + query)
  await router.isReady()
  const w = mount(ApplicationsView, { props: { client: clientOf(), session: session(), ...props }, global: { plugins: [router] }, attachTo: document.body })
  await flushPromises()
  return { w, router }
}
const names = (w) => w.findAll('tbody tr .app-cell a').map((a) => a.text())
const chip = (w, key) => w.get(`[data-status=${key}].chip`)
const counts = (w) => Object.fromEntries(['critical', 'warning', 'logs', 'nodata', 'ok'].map((k) => [k, Number(chip(w, k).get('.chip-count').text())]))

describe('ApplicationsView', () => {
  it('lists the applications of the application namespaces, hiding the platform\'s and the monitoring ones until asked for', async () => {
    const { w } = await mountAt()
    expect(new Set(names(w))).toEqual(new Set(['frontend', 'cartservice', 'ledger']))
    expect(names(w)).not.toContain('prometheus')
    expect(names(w)).not.toContain('coredns')
    expect(w.get('[data-testid=count]').text()).toBe('3 of 5 applications')
  })

  it('shows the kinds of namespace as checkboxes, application on by default, and reveals the others when ticked', async () => {
    const { w } = await mountAt()
    const boxes = w.findAll('.cat input')
    expect(boxes.map((b) => b.element.checked)).toEqual([true, false, false])
    await boxes[2].setValue(true) // monitoring
    expect(names(w)).toContain('prometheus')
    await boxes[1].setValue(true) // control-plane
    expect(names(w)).toContain('coredns')
  })

  it('does not let the last kind of namespace be switched off', async () => {
    const { w } = await mountAt()
    await w.findAll('.cat input')[0].setValue(false)
    expect(w.findAll('.cat input')[0].element.checked).toBe(true)
    expect(names(w).length).toBe(3)
  })

  it('narrows to the namespaces chosen in the drop-down, and offers only those of the kinds that are on', async () => {
    const { w } = await mountAt()
    await w.get('.ns-toggle').trigger('click')
    expect(w.findAll('.ns-option').map((o) => o.text())).toEqual(['payments 1', 'shop 2'])
    await w.findAll('.ns-option input')[1].setValue(true) // shop
    expect(new Set(names(w))).toEqual(new Set(['frontend', 'cartservice']))
    expect(w.get('[data-testid=count]').text()).toBe('2 of 5 applications')
  })

  it('shows the namespace under a name only when the rows span several', async () => {
    const { w } = await mountAt()
    expect(w.findAll('tbody .ns').length).toBe(3)
    await w.get('.ns-toggle').trigger('click')
    await w.findAll('.ns-option input')[1].setValue(true)
    expect(w.findAll('tbody .ns').length).toBe(0)
  })

  it('searches by name', async () => {
    const { w } = await mountAt()
    await w.get('.search-box input').setValue('CART')
    expect(names(w)).toEqual(['cartservice'])
    await w.get('.search-box input').setValue('zzz')
    expect(w.get('[data-testid=no-match]').text()).toContain('No application matches')
  })

  it('counts applications per status and narrows to the statuses chosen', async () => {
    const { w } = await mountAt()
    // cartservice restarted (warning); frontend and ledger are fine
    expect(counts(w)).toEqual({ critical: 0, warning: 1, logs: 0, nodata: 0, ok: 2 })
    await chip(w, 'warning').trigger('click')
    expect(names(w)).toEqual(['cartservice'])
    expect(chip(w, 'warning').attributes('aria-pressed')).toBe('true')
    await chip(w, 'ok').trigger('click')
    expect(new Set(names(w))).toEqual(new Set(['cartservice', 'frontend', 'ledger']))
    await chip(w, 'warning').trigger('click')
    await chip(w, 'ok').trigger('click')
    expect(names(w).length).toBe(3)
  })

  it('sorts the worst first', async () => {
    const { w } = await mountAt()
    expect(names(w)[0]).toBe('cartservice') // the one with a restart
    expect(w.findAll('tbody tr')[0].attributes('data-status')).toBe('warning')
  })

  it('shows the figures of each application, and a dash for what is not known', async () => {
    const { w } = await mountAt('?cat=application,monitoring')
    const row = (n) => w.findAll('tbody tr').find((r) => r.get('.app-cell a').text() === n).findAll('td').map((c) => c.text().replace(/\s+/g, ' ').trim())
    const fe = row('frontend')
    expect(fe[0]).toContain('frontend')
    expect(fe[2]).toBe('–') // nothing calls it
    expect(fe[5]).toBe('1/1') // one pod running of one seen
    expect(fe[7]).toBe('0.25') // cores
    expect(fe[8]).toBe('64 MiB')
    expect(fe[9]).toBe('–')
    expect(row('prometheus')[1]).toBe('prometheus') // its type from the node-agent
    expect(row('cartservice')[6]).toBe('36') // 0.01 restarts a second, over the one-hour window
  })

  it('links a name to its page and the errors to its logs', async () => {
    const logs = [{ service: '/k8s/shop/frontend', body: 'boom 1' }, { service: '/k8s/shop/frontend', body: 'boom 22' }, { service: '/k8s/shop/frontend', body: 'other' }]
    const { w } = await mountAt('', { client: clientOf({ logs }) })
    const fe = w.findAll('tbody tr').find((r) => r.get('.app-cell a').text() === 'frontend')
    expect(fe.get('.app-cell a').attributes('href')).toBe('/applications/%2Fk8s%2Fshop%2Ffrontend')
    expect(fe.get('.log-link').text()).toBe('2 unique errors')
    expect(fe.get('.log-link').attributes('href')).toContain('/logs?service=/k8s/shop/frontend&severity=ERROR')
    expect(fe.attributes('data-status')).toBe('logs')
  })

  it('marks an application critical from a firing alert without a reload, and warning for a warning', async () => {
    const store = { state: reactive({ alerts: [] }) }
    const { w } = await mountAt('', { alerts: store })
    expect(counts(w).critical).toBe(0)
    store.state.alerts = [{ service: '/k8s/shop/frontend', severity: 'critical', status: 'firing' }, { service: '/k8s/payments/ledger', severity: 'warning', status: 'firing' }, { service: '/k8s/shop/cartservice', severity: 'critical', status: 'pending' }]
    await flushPromises()
    expect(counts(w)).toMatchObject({ critical: 1, warning: 2 }) // cartservice: pending is not firing, but it restarted
    expect(names(w)[0]).toBe('frontend')
  })

  it('marks an application with pods but none running as critical at once', async () => {
    const gone = { ...DATA, container_resources_memory_rss_bytes: [rss(FE), { labels: { container_id: CART }, points: [{ t: NOW - 600_000, v: 1 }] }] }
    const { w } = await mountAt('', { client: clientOf({ data: gone }) })
    const row = w.findAll('tbody tr').find((r) => r.get('.app-cell a').text() === 'cartservice')
    expect(row.attributes('data-status')).toBe('critical')
    expect(row.findAll('td')[5].text()).toBe('0/1')
  })

  it('keeps the choices in the address, and starts from them', async () => {
    const client = clientOf()
    const { w, router } = await mountAt('?ns=shop&status=ok&q=front&cat=application,monitoring&window=900&rows=25', { client })
    expect(names(w)).toEqual(['frontend'])
    expect(w.get('.search-box input').element.value).toBe('front')
    expect(router.currentRoute.value.query).toEqual({ window: '900', q: 'front', ns: 'shop', cat: 'application,monitoring', status: 'ok', rows: '25' })
    const q = client.get.mock.calls.find(([p]) => p === 'metrics/query_range')[1]
    expect(Number(q.to) - Number(q.from)).toBeCloseTo(900, 0)
  })

  it('ignores junk in the address', async () => {
    const { w } = await mountAt('?status=bogus&cat=nope&window=7&rows=3')
    expect(names(w).length).toBe(3)
    expect(w.findAll('.cat input')[0].element.checked).toBe(true)
  })

  it('reloads for the chosen time range', async () => {
    const client = clientOf()
    const { w } = await mountAt('', { client })
    client.get.mockClear()
    await w.get('#window').setValue(String(24 * 3600))
    await flushPromises()
    const q = client.get.mock.calls.find(([p]) => p === 'metrics/query_range')[1]
    expect(Number(q.to) - Number(q.from)).toBeCloseTo(24 * 3600, 0)
  })

  it('pages the table', async () => {
    const many = { container_resources_memory_rss_bytes: Array.from({ length: 60 }, (_, i) => rss(pod('shop', `svc${String(i).padStart(2, '0')}`))) }
    const { w } = await mountAt('?rows=25', { client: clientOf({ data: many }) })
    expect(names(w)).toHaveLength(25)
    expect(w.get('.pager').text()).toContain('1–25 of 60')
    await w.get('[aria-label="Next page"]').trigger('click')
    expect(w.get('.pager').text()).toContain('26–50 of 60')
    expect(names(w)[0]).toBe('svc25')
    await w.get('[aria-label="Next page"]').trigger('click')
    expect(names(w)).toHaveLength(10)
    expect(w.get('[aria-label="Next page"]').attributes('disabled')).toBeDefined()
    await w.get('.search-box input').setValue('svc0')
    expect(w.get('.pager').text()).toContain('1–10 of 10') // back to the first page when the filter changes
  })

  it('warns about what could not be read, and says what to do when there is nothing', async () => {
    const warned = await mountAt('', { client: clientOf({ fail: { container_restarts_total: 'down' } }) })
    expect(warned.w.get('[role=status]').text()).toContain('container_restarts_total: down')
    expect(names(warned.w).length).toBe(3)
    const none = await mountAt('', { client: clientOf({ data: {}, names: [] }) })
    expect(none.w.get('[data-testid=none]').text()).toContain('are the node-agents sending?')
  })

  it('says when every application is in a kind of namespace that is switched off', async () => {
    const onlyKube = { container_resources_memory_rss_bytes: [rss(KUBE)] }
    const { w } = await mountAt('', { client: clientOf({ data: onlyKube }) })
    expect(w.get('[data-testid=hidden]').text()).toContain('Tick control-plane or monitoring')
  })

  it('shows the reason when it cannot read, and does nothing when not connected', async () => {
    const f = 'This key cannot read project "gke". Check the key and the project name.'
    const bad = await mountAt('', { client: clientOf({ fail: { metrics: f } }) })
    expect(bad.w.get('[role=alert]').text()).toContain('cannot read project')
    const client = clientOf()
    const off = await mountAt('', { client, session: session(null) })
    expect(client.get).not.toHaveBeenCalled()
    expect(off.w.text()).toContain('Not connected')
  })
})
