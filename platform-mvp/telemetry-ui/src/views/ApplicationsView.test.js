import { describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { createRouter, createMemoryHistory } from 'vue-router'
import { reactive } from 'vue'
import ApplicationsView from './ApplicationsView.vue'
import { createCategoryStore } from '../categories'

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
const clientOf = ({ data = DATA, names = Object.keys(data), fail = {}, logs = [], cluster = null } = {}) => ({
  get: vi.fn(async (path, params) => {
    if (fail[path] || (params && fail[params.name])) throw new Error(fail[path] || fail[params.name])
    if (path === 'inventory') return cluster ? cluster.summary : []
    if (path === 'inventory/workloads') return cluster.workloads
    if (path === 'inventory/pods') return cluster.pods || []
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
const counts = (w) => Object.fromEntries(['critical', 'silent', 'warning', 'logs', 'nodata', 'ok'].map((k) => [k, Number(chip(w, k).get('.chip-count').text())]))

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

  it('forgets a namespace choice when its kind is switched off, as the picker does, instead of showing an empty table', async () => {
    const { w } = await mountAt('?cat=application,monitoring&ns=monitoring')
    expect(names(w)).toEqual(['prometheus']) // only the chosen namespace
    await w.findAll('.cat input')[2].setValue(false) // monitoring off: that namespace is no longer offered
    expect(w.get('.ns-all').text()).toBe('all (3)') // the picker says "all"
    expect(new Set(names(w))).toEqual(new Set(['frontend', 'cartservice', 'ledger'])) // and so does the table
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
    expect(counts(w)).toEqual({ critical: 0, silent: 0, warning: 1, logs: 0, nodata: 0, ok: 2 })
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

  it('marks an application that stopped sending as silent, not critical, and says it cannot tell why without an inventory', async () => {
    const gone = { ...DATA, container_resources_memory_rss_bytes: [rss(FE), { labels: { container_id: CART }, points: [{ t: NOW - 600_000, v: 1 }] }] }
    const { w } = await mountAt('', { client: clientOf({ data: gone }) })
    const row = w.findAll('tbody tr').find((r) => r.get('.app-cell a').text() === 'cartservice')
    expect(row.attributes('data-status')).toBe('silent')
    expect(row.get('[data-testid=silence]').text()).toBe('Stopped sending')
    expect(row.get('[data-testid=silence]').attributes('title')).toContain('cluster collector')
    expect(row.findAll('td')[5].text()).toBe('0/1')
    expect(row.findAll('td')[5].classes()).not.toContain('bad') // zero running is not painted as a failure by itself
    expect(counts(w)).toMatchObject({ critical: 0, silent: 1 })
  })

  describe('with the cluster inventory', () => {
    const iso = (agoMs) => new Date(NOW - agoMs).toISOString()
    const wl = (ns, name, desired, ready, kind = 'deployment') => ({ cluster: 'gke', kind, uid: `${ns}-${name}`, namespace: ns, name, observed: iso(10_000), data: { desired, ready, available: ready } })
    const cluster = (workloads, pods = [], agoMs = 20_000) => ({ summary: [{ cluster: 'gke', last_received: iso(agoMs), last_collected: iso(agoMs), counts: {} }], workloads, pods })
    // the frontend is sending; the cart and the ledger stopped five minutes ago
    const quiet = { ...DATA, container_resources_memory_rss_bytes: [rss(FE), { labels: { container_id: CART }, points: [{ t: NOW - 600_000, v: 1 }] }, { labels: { container_id: PAY }, points: [{ t: NOW - 600_000, v: 1 }] }] }
    const row = (w, name) => w.findAll('tbody tr').find((r) => r.get('.app-cell a').text() === name)

    it('says why each silent application is silent, and shows what it is set to run', async () => {
      const c = cluster([wl('shop', 'frontend', 1, 1), wl('shop', 'cartservice', 0, 0), wl('payments', 'ledger', 3, 0), wl('shop', 'adservice', 2, 2)], [
        { cluster: 'gke', kind: 'pod', namespace: 'payments', name: 'p1', data: { ready: false, phase: 'Running', workload_kind: 'deployment', workload_name: 'ledger', containers: [{ state: 'waiting', reason: 'CrashLoopBackOff', last_reason: 'OOMKilled' }] } },
        { cluster: 'gke', kind: 'pod', namespace: 'payments', name: 'p2', data: { ready: false, phase: 'Running', workload_kind: 'deployment', workload_name: 'ledger', containers: [{ state: 'waiting', reason: 'CrashLoopBackOff', last_reason: 'OOMKilled' }] } },
      ])
      const { w } = await mountAt('', { client: clientOf({ data: quiet, cluster: c }) })
      expect(counts(w)).toMatchObject({ critical: 0, silent: 3 })

      expect(row(w, 'frontend').attributes('data-status')).not.toBe('silent')
      const cart = row(w, 'cartservice')
      expect(cart.attributes('data-status')).toBe('silent')
      expect(cart.get('[data-testid=silence]').text()).toBe('Scaled to zero')
      expect(cart.get('[data-testid=silence]').classes()).toContain('tone-info')
      expect(cart.findAll('td')[5].text()).toBe('0/0')
      expect(cart.findAll('td')[5].classes()).not.toContain('bad')

      const ledger = row(w, 'ledger')
      expect(ledger.get('[data-testid=silence]').text()).toBe('Not ready · 0/3')
      expect(ledger.get('[data-testid=silence]').classes()).toContain('tone-bad')
      expect(ledger.get('[data-testid=silence]').attributes('title')).toBe('0 of 3 ready: CrashLoopBackOff (OOMKilled) ×2')
      expect(ledger.findAll('td')[5].text()).toBe('0/3')
      expect(ledger.findAll('td')[5].classes()).toContain('bad') // wants three and has none: a failure, said by the cluster

      const ad = row(w, 'adservice') // in the cluster, ready, and never sent a thing
      expect(ad.get('[data-testid=silence]').text()).toBe('Ready, no data')
      expect(ad.get('[data-testid=silence]').classes()).toContain('tone-caution')
    })

    it('lists the silent ones next to the critical ones, worst first, and filters by them', async () => {
      // the ledger is in neither list of the cluster: it is silent because it was removed
      const c = cluster([wl('shop', 'frontend', 1, 1), wl('shop', 'cartservice', 0, 0)])
      const { w } = await mountAt('', { client: clientOf({ data: quiet, cluster: c }) })
      expect(names(w).slice(0, 2).sort()).toEqual(['cartservice', 'ledger']) // the silent ones before the one that is fine
      expect(names(w)[2]).toBe('frontend')
      expect(row(w, 'ledger').get('[data-testid=silence]').text()).toBe('Removed')
      await chip(w, 'silent').trigger('click')
      expect(names(w).sort()).toEqual(['cartservice', 'ledger'])
      expect(w.findAll('tbody tr').every((r) => r.attributes('data-status') === 'silent')).toBe(true)
    })

    it('does not call anything silent when nothing at all is sending', async () => {
      const dead = { ...DATA, container_resources_memory_rss_bytes: [FE, CART, PAY].map((c) => ({ labels: { container_id: c }, points: [{ t: NOW - 600_000, v: 1 }] })) }
      const { w } = await mountAt('', { client: clientOf({ data: dead, cluster: cluster([wl('shop', 'frontend', 1, 1), wl('shop', 'cartservice', 2, 0)]) }) })
      expect(w.findAll('[data-testid=silence]')).toHaveLength(0)
      expect(counts(w).silent).toBe(0)
    })

    it('warns when the cluster collector has gone quiet and says only what the node-agent shows', async () => {
      const { w } = await mountAt('', { client: clientOf({ data: quiet, cluster: cluster([wl('shop', 'cartservice', 0, 0)], [], 25 * 60_000) }) })
      expect(w.get('.warn-list').text()).toContain('cluster collector last reported 25 minutes ago')
      expect(row(w, 'cartservice').get('[data-testid=silence]').text()).toBe('Stopped sending')
    })

    it('shows a firing critical alert above silence', async () => {
      const store = { state: reactive({ alerts: [{ service: '/k8s/shop/cartservice', severity: 'critical', status: 'firing' }] }) }
      const { w } = await mountAt('', { client: clientOf({ data: quiet, cluster: cluster([wl('shop', 'cartservice', 0, 0)]) }), alerts: store })
      expect(row(w, 'cartservice').attributes('data-status')).toBe('critical')
    })
  })

  it('keeps the choices in the address, and starts from them', async () => {
    const client = clientOf()
    const { w, router } = await mountAt('?ns=shop&status=ok&q=front&cat=application,monitoring&window=900&rows=25', { client })
    expect(names(w)).toEqual(['frontend'])
    expect(w.get('.search-box input').element.value).toBe('front')
    await vi.waitFor(() => expect(router.currentRoute.value.query).toEqual({ window: '900', q: 'front', ns: 'shop', cat: 'application,monitoring', status: 'ok', rows: '25' }))
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

  describe('categories the user chose', () => {
    const FRONT = '/k8s/shop/frontend'
    const memory = () => { const m = new Map(); return { getItem: (k) => (m.has(k) ? m.get(k) : null), setItem: (k, v) => m.set(k, String(v)), removeItem: (k) => m.delete(k) } }
    const storeWith = (setup) => { const c = createCategoryStore({ session: session(), storage: memory() }); setup(c); return c }

    it('shows an application where the user moved it, not where its namespace put it', async () => {
      const categories = storeWith((c) => c.move(FRONT, 'monitoring', 'application'))
      const { w } = await mountAt('', { categories })
      expect(names(w)).not.toContain('frontend') // moved out of "application"
      await w.findAll('.cat input')[2].setValue(true) // monitoring
      expect(names(w)).toContain('frontend')
      expect(names(w)).toContain('prometheus')
    })

    it('shows an application moved INTO application, though its namespace is a platform one', async () => {
      const categories = storeWith((c) => c.move('/k8s/monitoring/prometheus', 'application', 'monitoring'))
      const { w } = await mountAt('', { categories })
      expect(names(w)).toContain('prometheus')
    })

    it('offers a checkbox for a category of the user\'s own, and the apps moved to it', async () => {
      const categories = storeWith((c) => { c.add('payments tier'); c.move('/k8s/payments/ledger', 'payments tier', 'application') })
      const { w } = await mountAt('', { categories })
      expect(w.findAll('.cat').map((l) => l.text())).toEqual(['application', 'control-plane', 'monitoring', 'payments tier'])
      expect(names(w)).not.toContain('ledger')
      await w.findAll('.cat input')[3].setValue(true)
      expect(names(w)).toContain('ledger')
    })

    it('makes a category from the "+", shows it at once, and keeps it in the address', async () => {
      const categories = storeWith(() => {})
      const { w, router } = await mountAt('', { categories })
      await w.get('.cat-plus').trigger('click')
      await w.get('.cat-form input').setValue('Team A')
      await w.get('.cat-form').trigger('submit')
      await flushPromises()
      expect(categories.all()).toContain('team a')
      expect(w.findAll('.cat input').map((b) => b.element.checked)).toEqual([true, false, false, true]) // ticked: it is shown, not hidden
      await vi.waitFor(() => expect(router.currentRoute.value.query.cat).toBe('application,team a'))
    })

    it('starts from a category in the address when it exists, and ignores one that does not', async () => {
      const categories = storeWith((c) => c.add('team a'))
      const known = await mountAt('?cat=team%20a', { categories })
      expect(known.w.findAll('.cat input').map((b) => b.element.checked)).toEqual([false, false, false, true])
      const unknown = await mountAt('?cat=ghost', { categories })
      expect(unknown.w.findAll('.cat input')[0].element.checked).toBe(true)
    })
  })

  describe('the shared cache', () => {
    it('reads through the cache it was given, and Refresh forces a new read', async () => {
      const get = vi.fn(async (client, o) => (await import('../api/applications')).loadApplications(client, { seconds: o.seconds }))
      const { w } = await mountAt('', { appCache: { get } })
      expect(get).toHaveBeenCalledTimes(1)
      expect(get.mock.calls[0][1]).toMatchObject({ project: 'gke', seconds: 3600, force: false })
      expect(names(w).length).toBe(3)
      await w.get('.head-right button').trigger('click')
      await flushPromises()
      expect(get.mock.calls[1][1].force).toBe(true)
    })
    it('does not force a new read for a change of time range, which is a different key', async () => {
      const get = vi.fn(async () => ({ apps: [], warnings: [], hasInventory: true }))
      const { w } = await mountAt('', { appCache: { get } })
      await w.get('#window').setValue(String(900))
      await flushPromises()
      expect(get.mock.calls.at(-1)[1]).toMatchObject({ seconds: 900, force: false })
    })
  })
})
