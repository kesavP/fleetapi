import { describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { createRouter, createMemoryHistory } from 'vue-router'
import MapView from './MapView.vue'
import { createCategoryStore } from '../categories'

import { reactive } from 'vue'

const stub = { template: '<i/>' }
// A router per mount: the page keeps its choices in the address, and a shared router would carry one test's into the next.
const makeRouter = () => createRouter({
  history: createMemoryHistory(),
  routes: [{ path: '/', component: stub }, { path: '/connection', component: stub }, { path: '/map', name: 'map', component: stub }, { path: '/applications/:name', name: 'application', component: stub }],
})
const session = (v = { project: 'gke', key: 'k' }) => ({ get: () => v })

const FE = '/k8s/shop/frontend-5d6f8c7b9-xk2lp/server'
const CART = '/k8s/shop/cartservice-7c9d8f6b5d-bk7q2/server'
const PG = '/k8s/data/postgres-0/postgres'
const NOW = Date.now()
const flat = (n) => Array.from({ length: 40 }, (_, i) => ({ t: NOW - (40 - i) * 15000, v: n }))
const calls = (dest, actual) => ({ container_id: FE, destination: dest, actual_destination: actual })
const cartCall = calls('10.0.0.9:7070', '10.4.1.9:7070')

const DATA = {
  container_net_tcp_listen_info: [
    { labels: { container_id: CART, listen_addr: '10.4.1.9:7070' }, points: [{ t: NOW, v: 1 }] },
    { labels: { container_id: PG, listen_addr: '10.4.2.2:5432' }, points: [{ t: NOW, v: 1 }] },
  ],
  container_net_tcp_successful_connects_total: [
    { labels: cartCall, points: flat(2) },
    { labels: calls('34.1.2.3:443'), points: flat(1) },
  ],
  container_http_requests_total: [
    { labels: { ...cartCall, status: '2xx' }, points: flat(9) },
    { labels: { ...cartCall, status: '500' }, points: flat(1) },
  ],
}
const clientOf = ({ data = DATA, names = Object.keys(data), fail = {} } = {}) => ({
  get: vi.fn(async (path, params) => {
    if (path === 'metrics') {
      if (fail.metrics) throw new Error(fail.metrics)
      return names
    }
    if (fail[params.name]) throw new Error(fail[params.name])
    return { series: data[params.name] || [] }
  }),
})
async function mountMap(props = {}, query = '') {
  const router = makeRouter()
  router.push('/map' + query)
  await router.isReady()
  const w = mount(MapView, { props: { client: clientOf(), session: session(), ...props }, global: { plugins: [router] }, attachTo: document.body })
  await flushPromises()
  w.router = router
  return w
}
const nodeNames = (w) => w.findAll('.node .node-name').map((n) => n.text()).sort()

describe('MapView', () => {
  it('draws the services and who calls whom, with external endpoints marked', async () => {
    const w = await mountMap()
    expect(nodeNames(w)).toEqual(['34.1.2.3:443', 'cartservice', 'frontend'])
    expect(w.findAll('.edge')).toHaveLength(2)
    expect(w.findAll('.node.ext')).toHaveLength(1)
  })

  it('colours a failing edge and says why in its tooltip', async () => {
    const w = await mountMap()
    const bad = w.get('.edge.bad')
    expect(bad.text()).toContain('10.0/s · 10% err')
    expect(bad.get('title').text()).toContain('shop/frontend → shop/cartservice')
    expect(bad.get('title').text()).toContain('10.0 req/s')
    expect(bad.get('title').text()).toContain('10.0% errors (http)')
    expect(w.findAll('.edge.bad')).toHaveLength(1)
  })

  it('lists every call in a table, worst first', async () => {
    const w = await mountMap()
    const rows = w.findAll('table.calls tbody tr')
    expect(rows).toHaveLength(2)
    expect(rows[0].text()).toContain('shop/cartservice')
    expect(rows[0].text()).toContain('10.0%')
    expect(rows[1].text()).toContain('34.1.2.3:443')
  })

  it('selecting a service shows its calls and links to its page; selecting again clears it', async () => {
    const w = await mountMap()
    const fe = w.findAll('.node').find((n) => n.text().includes('frontend'))
    await fe.trigger('click')
    const detail = w.get('.detail')
    expect(detail.text()).toContain('shop/frontend')
    expect(detail.text()).toContain('Calls out')
    expect(detail.text()).toContain('shop/cartservice')
    expect(detail.text()).not.toContain('Called by')
    expect(detail.get('a').attributes('href')).toBe('/applications/%2Fk8s%2Fshop%2Ffrontend')
    expect(fe.get('.node-select').attributes('aria-pressed')).toBe('true')
    await fe.trigger('click')
    expect(w.find('.detail').exists()).toBe(false)
  })

  it('an external endpoint has no service page', async () => {
    const w = await mountMap()
    await w.findAll('.node').find((n) => n.text().includes('34.1.2.3')).trigger('click')
    expect(w.get('.detail').text()).toContain('external')
    expect(w.get('.detail').find('a').exists()).toBe(false)
  })

  it('can be operated from the keyboard: each card has a real button to select it and a real link to open it', async () => {
    const w = await mountMap()
    const fe = nodeOf(w, 'frontend')
    const btn = fe.get('button.node-select')
    expect(btn.attributes('aria-label')).toBe('Show the calls of shop/frontend')
    expect(btn.attributes('aria-pressed')).toBe('false')
    await btn.trigger('click')
    expect(w.find('.detail').exists()).toBe(true)
    expect(btn.attributes('aria-pressed')).toBe('true')
    expect(fe.get('a.node-name').attributes('href')).toBe('/applications/%2Fk8s%2Fshop%2Ffrontend')
  })

  it('reloads for the chosen window', async () => {
    const client = clientOf()
    const w = await mountMap({ client })
    client.get.mockClear()
    await w.get('#window').setValue(String(24 * 3600))
    await flushPromises()
    const q = client.get.mock.calls.find(([p]) => p === 'metrics/query_range')[1]
    expect(Number(q.to) - Number(q.from)).toBeCloseTo(24 * 3600, 0)
  })

  it('shows a database as the node that serves it', async () => {
    const q = { container_id: CART, destination: '10.0.0.50:5432', actual_destination: '10.4.2.2:5432' }
    const data = { ...DATA, container_postgres_queries_total: [{ labels: { ...q, status: 'ok' }, points: flat(5) }] }
    const w = await mountMap({ client: clientOf({ data }) })
    expect(nodeNames(w)).toContain('postgres')
    expect(w.findAll('.node').find((n) => n.text().includes('postgres')).text()).toContain('postgres')
    expect(w.text()).toContain('queries/s')
  })

  it('warns when something could not be read, and still draws', async () => {
    const w = await mountMap({ client: clientOf({ fail: { container_net_tcp_successful_connects_total: 'down' } }) })
    expect(w.get('[role=status]').text()).toContain('container_net_tcp_successful_connects_total: down')
    expect(w.findAll('.edge').length).toBeGreaterThan(0)
  })

  it('shows the reason when the data cannot be read at all', async () => {
    const f = 'This key cannot read project "gke". Check the key and the project name.'
    const w = await mountMap({ client: clientOf({ fail: { metrics: f } }) })
    expect(w.get('[role=alert]').text()).toContain('This key cannot read project')
    expect(w.find('svg.map').exists()).toBe(false)
  })

  it('draws refused connections to a Service address against the application behind it, and says how it knew', async () => {
    const data = {
      ...DATA,
      container_net_tcp_failed_connects_total: [{ labels: { container_id: FE, destination: '10.0.0.9:7070' }, points: flat(1) }], // only the dialled address
    }
    const w = await mountMap({ client: clientOf({ data }) })
    expect(nodeNames(w)).toEqual(['34.1.2.3:443', 'cartservice', 'frontend']) // no extra external endpoint for the Service address
    expect(w.findAll('.node.ext')).toHaveLength(1)
    expect(w.get('[data-testid=learned]').text()).toContain('1 address')
    expect(w.get('[data-testid=learned]').text()).toContain('matched from the successful connections')
    const row = w.findAll('table.calls tbody tr').find((r) => r.text().includes('shop/cartservice'))
    expect(row.text()).toContain('(http+tcp)') // the refusals count against it
  })

  it('does not mention Service addresses when none were needed', async () => {
    const w = await mountMap()
    expect(w.find('[data-testid=learned]').exists()).toBe(false)
  })

  it('explains an empty project, and a quiet one', async () => {
    const w = await mountMap({ client: clientOf({ names: ['up'], data: {} }) })
    expect(w.text()).toContain('are the node-agents sending?')
    const quiet = await mountMap({ client: clientOf({ data: { ...DATA, container_net_tcp_successful_connects_total: [], container_http_requests_total: [] } }) })
    expect(quiet.text()).toContain('No calls between services')
    expect(quiet.text()).toContain('Widen the window')
  })

  it('does not call the API when not connected', async () => {
    const client = clientOf()
    const w = await mountMap({ client, session: session(null) })
    expect(client.get).not.toHaveBeenCalled()
    expect(w.text()).toContain('Not connected')
  })
})

// A wider project: platform and monitoring namespaces beside the application ones.
const PROM = '/k8s/monitoring/prometheus-5b8d7f6c9-mn4pq/server'
const KUBE = '/k8s/kube-system/coredns-6d4cf56db6-tb8qs/coredns'
const WIDE = {
  container_net_tcp_listen_info: [
    { labels: { container_id: FE, listen_addr: '10.4.0.5:8080' }, points: [{ t: NOW, v: 1 }] },
    { labels: { container_id: CART, listen_addr: '10.4.1.9:7070' }, points: [{ t: NOW, v: 1 }] },
    { labels: { container_id: PG, listen_addr: '10.4.2.2:5432' }, points: [{ t: NOW, v: 1 }] },
    { labels: { container_id: PROM, listen_addr: '10.4.5.5:9090' }, points: [{ t: NOW, v: 1 }] },
    { labels: { container_id: KUBE, listen_addr: '10.4.6.6:53' }, points: [{ t: NOW, v: 1 }] },
  ],
  container_net_tcp_successful_connects_total: [
    { labels: { container_id: FE, destination: '10.0.0.9:7070', actual_destination: '10.4.1.9:7070' }, points: flat(2) },
    { labels: { container_id: FE, destination: '34.1.2.3:443' }, points: flat(1) },
    { labels: { container_id: FE, destination: '10.4.6.6:53', actual_destination: '10.4.6.6:53' }, points: flat(3) },
    { labels: { container_id: PROM, destination: '10.4.0.5:8080', actual_destination: '10.4.0.5:8080' }, points: flat(1) },
  ],
  container_http_requests_total: [
    { labels: { container_id: FE, destination: '10.0.0.9:7070', actual_destination: '10.4.1.9:7070', status: '2xx' }, points: flat(9) },
    { labels: { container_id: FE, destination: '10.0.0.9:7070', actual_destination: '10.4.1.9:7070', status: '500' }, points: flat(1) },
  ],
  container_postgres_queries_total: [{ labels: { container_id: CART, destination: '10.4.2.2:5432', actual_destination: '10.4.2.2:5432', status: 'ok' }, points: flat(5) }],
}
const wide = () => clientOf({ data: WIDE })
const translate = (w, name) => {
  const g = w.findAll('.node').find((n) => n.get('.node-name').text() === name)
  const style = g.attributes('style')
  return { x: Number(/left:\s*([-\d.]+)px/.exec(style)[1]), y: Number(/top:\s*([-\d.]+)px/.exec(style)[1]) }
}
const nodeOf = (w, name) => w.findAll('.node').find((n) => n.get('.node-name').text() === name)

describe('MapView: filters', () => {
  it('draws the applications of the application namespaces, and only the calls among them, until other kinds are asked for', async () => {
    const w = await mountMap({ client: wide() })
    expect(nodeNames(w)).toEqual(['34.1.2.3:443', 'cartservice', 'frontend', 'postgres'])
    expect(w.findAll('.edge')).toHaveLength(3) // frontend to cartservice and to the external endpoint, cartservice to postgres
    expect(w.findAll('.cat input').map((b) => b.element.checked)).toEqual([true, false, false])
  })

  it('shows monitoring and control-plane applications, and their calls, when their kinds are ticked', async () => {
    const w = await mountMap({ client: wide() })
    await w.findAll('.cat input')[2].setValue(true)
    expect(nodeNames(w)).toContain('prometheus')
    expect(w.findAll('.edge')).toHaveLength(4) // prometheus scraping the frontend appears
    await w.findAll('.cat input')[1].setValue(true)
    expect(nodeNames(w)).toContain('coredns')
    expect(w.findAll('.edge')).toHaveLength(5)
  })

  it('does not let the last kind be switched off', async () => {
    const w = await mountMap({ client: wide() })
    await w.findAll('.cat input')[0].setValue(false)
    expect(w.findAll('.cat input')[0].element.checked).toBe(true)
    expect(nodeNames(w).length).toBe(4)
  })

  it('narrows to the namespaces chosen, and offers only the namespaces of the kinds that are on', async () => {
    const w = await mountMap({ client: wide() })
    await w.get('.ns-toggle').trigger('click')
    expect(w.findAll('.ns-option').map((o) => o.text())).toEqual(['data 1', 'shop 2'])
    await w.findAll('.ns-option input')[1].setValue(true) // shop
    expect(nodeNames(w)).toEqual(['34.1.2.3:443', 'cartservice', 'frontend']) // postgres is in "data"; the external endpoint is still called from shop
    expect(w.findAll('.edge')).toHaveLength(2)
  })

  it('forgets a namespace choice when its kind is switched off, as the picker does, instead of drawing nothing', async () => {
    const w = await mountMap({ client: wide() }, '?cat=application,monitoring&ns=monitoring')
    expect(nodeNames(w)).toEqual(['prometheus'])
    await w.findAll('.cat input')[2].setValue(false)
    expect(w.get('.ns-all').exists()).toBe(true)
    expect(nodeNames(w)).toEqual(['34.1.2.3:443', 'cartservice', 'frontend', 'postgres'])
  })

  it('dims what a search does not match, says how many match, and does not move anything', async () => {
    const w = await mountMap({ client: wide() })
    const before = translate(w, 'frontend')
    await w.get('.search-box input').setValue('CART')
    expect(w.get('[data-testid=matches]').text()).toBe('1 match')
    expect(nodeOf(w, 'cartservice').classes()).not.toContain('dim')
    expect(nodeOf(w, 'frontend').classes()).toContain('dim')
    expect(translate(w, 'frontend')).toEqual(before)
  })

  it('says when the filters leave nothing to draw', async () => {
    // every application here is in a platform or monitoring namespace, so with only "application" on there is nothing to draw
    const hidden = {
      container_net_tcp_listen_info: [WIDE.container_net_tcp_listen_info[3], WIDE.container_net_tcp_listen_info[4]],
      container_net_tcp_successful_connects_total: [{ labels: { container_id: PROM, destination: '10.4.6.6:53', actual_destination: '10.4.6.6:53' }, points: flat(1) }],
    }
    const w = await mountMap({ client: clientOf({ data: hidden }) })
    expect(w.get('[data-testid=hidden]').text()).toContain('Nothing to show with these filters')
    expect(w.find('svg.map').exists()).toBe(false)
    await w.findAll('.cat input')[2].setValue(true) // monitoring: prometheus appears (and the platform's coredns it calls stays out)
    expect(w.find('[data-testid=hidden]').exists()).toBe(false)
    expect(nodeNames(w)).toEqual(['prometheus'])
  })

  it('keeps the choices in the address and starts from them, ignoring junk', async () => {
    const w = await mountMap({ client: wide() }, '?q=front&ns=shop&cat=application,monitoring&dir=tb&window=900')
    expect(w.get('.search-box input').element.value).toBe('front')
    await vi.waitFor(() => expect(w.router.currentRoute.value.query).toEqual({ window: '900', q: 'front', ns: 'shop', cat: 'application,monitoring', dir: 'tb' }))
    const junk = await mountMap({ client: wide() }, '?cat=nope&dir=sideways&window=7')
    expect(junk.findAll('.cat input').map((b) => b.element.checked)).toEqual([true, false, false])
    expect(junk.findAll('.seg-btn')[0].attributes('aria-pressed')).toBe('true')
  })
})

describe('MapView: how it is drawn', () => {
  it('runs left to right by default and top to bottom when asked, and says which is on', async () => {
    const w = await mountMap({ client: wide() })
    const lr = { fe: translate(w, 'frontend'), cart: translate(w, 'cartservice') }
    expect(lr.cart.x).toBeGreaterThan(lr.fe.x)
    expect(w.findAll('.seg-btn')[0].attributes('aria-pressed')).toBe('true')
    await w.get('[aria-label="Top to bottom"]').trigger('click')
    await flushPromises() // the address is updated by the router, which takes a moment
    const tb = { fe: translate(w, 'frontend'), cart: translate(w, 'cartservice') }
    expect(tb.cart.y).toBeGreaterThan(tb.fe.y)
    expect(w.findAll('.seg-btn')[1].attributes('aria-pressed')).toBe('true')
    await vi.waitFor(() => expect(w.router.currentRoute.value.query.dir).toBe('tb'))
    await w.get('[aria-label="Left to right"]').trigger('click')
    await flushPromises()
    await vi.waitFor(() => expect(w.router.currentRoute.value.query.dir).toBeUndefined()) // the default is not written
  })

  it('shows each application as a card: a status dot, its name, its kind and namespace, and an icon for what it is', async () => {
    const w = await mountMap({ client: wide() })
    expect(nodeOf(w, 'frontend').get('.node-sub').text()).toBe('ns:shop')
    expect(nodeOf(w, 'postgres').get('.node-sub').text()).toBe('postgres / ns:data')
    expect(nodeOf(w, '34.1.2.3:443').get('.node-sub').text()).toBe('external endpoint')
    const iconOf = (name) => nodeOf(w, name).get('.node-icon path').attributes('d')
    expect(new Set([iconOf('frontend'), iconOf('postgres'), iconOf('34.1.2.3:443')]).size).toBe(3) // workload, database, external
  })

  it('colours the dot by health: failing calls into it, a firing alert, and a hollow dot for an external endpoint', async () => {
    const store = { state: reactive({ alerts: [] }) }
    const w = await mountMap({ client: wide(), alerts: store })
    expect(nodeOf(w, 'cartservice').attributes('data-status')).toBe('bad') // 1 in 10 of the calls it answers fail
    expect(nodeOf(w, 'frontend').attributes('data-status')).toBe('ok')
    expect(nodeOf(w, '34.1.2.3:443').attributes('data-status')).toBe('external')
    expect(nodeOf(w, '34.1.2.3:443').get('.dot').classes()).toContain('external')
    expect(nodeOf(w, 'cartservice').attributes('aria-label')).toContain('failing')
    store.state.alerts = [{ service: '/k8s/shop/frontend', severity: 'warning', status: 'firing' }, { service: '/k8s/data/postgres', severity: 'critical', status: 'pending' }]
    await flushPromises()
    expect(nodeOf(w, 'frontend').attributes('data-status')).toBe('caution')
    expect(nodeOf(w, 'postgres').attributes('data-status')).toBe('ok') // pending is not firing
  })

  it('draws a healthy line plainly, and labels only the failing ones until something is selected', async () => {
    const w = await mountMap({ client: wide() })
    const labelled = w.findAll('.edge').filter((e) => e.find('.edge-label').exists())
    expect(labelled).toHaveLength(1)
    expect(labelled[0].classes()).toContain('bad')
    await nodeOf(w, 'frontend').trigger('click')
    expect(w.findAll('.edge').filter((e) => e.find('.edge-label').exists()).length).toBe(2) // and now the lines of the selected node
  })

  it('draws a straight line for a forward call', async () => {
    const w = await mountMap({ client: wide() })
    for (const e of w.findAll('.edge .line')) expect(e.attributes('d')).toMatch(/^M [\d.]+ [\d.]+ L [\d.]+ [\d.]+$/)
  })

  it('shows the selected application\'s status in its panel, and closes it', async () => {
    const w = await mountMap({ client: wide() })
    await nodeOf(w, 'cartservice').trigger('click')
    expect(w.get('.detail').text()).toContain('failing')
    await w.get('.detail .close').trigger('click')
    expect(w.find('.detail').exists()).toBe(false)
  })

  it('opens an application from its name, and an external endpoint has nothing to open', async () => {
    const w = await mountMap({ client: wide() })
    const link = nodeOf(w, 'cartservice').get('a.node-name')
    expect(link.attributes('href')).toBe('/applications/%2Fk8s%2Fshop%2Fcartservice')
    await link.trigger('click')
    await flushPromises()
    await vi.waitFor(() => expect(w.router.currentRoute.value.fullPath).toBe('/applications/%2Fk8s%2Fshop%2Fcartservice'))
    const w2 = await mountMap({ client: wide() })
    const ext = nodeOf(w2, '34.1.2.3:443')
    expect(ext.find('a').exists()).toBe(false)
    expect(ext.find('.kebab').exists()).toBe(false) // nor a menu: it is not an application of the project
    expect(ext.find('.node-menu').exists()).toBe(false)
  })
})

describe('MapView: size', () => {
  let observed
  const original = globalThis.ResizeObserver
  const install = () => {
    observed = null
    globalThis.ResizeObserver = class {
      constructor(cb) { observed = { cb, disconnected: false } }
      observe() {}
      disconnect() { if (observed) observed.disconnected = true }
    }
  }
  const uninstall = () => { globalThis.ResizeObserver = original }
  const svgOf = (w) => w.get('svg.map')
  const view = (w) => svgOf(w).attributes('viewBox').split(' ').map(Number)
  const widthOf = (w) => Number(/width:\s*(\d+)px/.exec(w.get('.map-surface').attributes('style'))[1])

  it('scales the picture down to the width of the canvas, never up, and not below the size where its text stays readable', async () => {
    install()
    try {
      const w = await mountMap({ client: wide() })
      const [, , vw] = view(w)
      observed.cb([{ contentRect: { width: 100000 } }])
      await flushPromises()
      expect(widthOf(w)).toBe(vw) // plenty of room: natural size, not bigger
      observed.cb([{ contentRect: { width: vw * 0.75 } }])
      await flushPromises()
      expect(widthOf(w)).toBe(Math.round(vw * 0.75))
      observed.cb([{ contentRect: { width: 50 } }])
      await flushPromises()
      expect(widthOf(w)).toBe(Math.round(vw * 0.65)) // too narrow to fit: the smallest it is drawn at, and it scrolls
    } finally {
      uninstall()
    }
  })

  it('zooms in and out and goes back to fit, within limits', async () => {
    install()
    try {
      const w = await mountMap({ client: wide() })
      const [, , vw] = view(w)
      observed.cb([{ contentRect: { width: 100000 } }])
      await flushPromises()
      const width = () => widthOf(w)
      await w.get('[aria-label="Zoom in"]').trigger('click')
      expect(width()).toBe(Math.round(vw * 1.25))
      await w.get('[aria-label="Zoom out"]').trigger('click')
      await w.get('[aria-label="Zoom out"]').trigger('click')
      expect(width()).toBe(Math.round(vw * 0.8 * 1.25 * 0.8))
      await w.get('[aria-label="Fit to the page"]').trigger('click')
      expect(width()).toBe(vw)
      for (let i = 0; i < 20; i++) await w.get('[aria-label="Zoom in"]').trigger('click')
      expect(width()).toBe(Math.round(vw * 3)) // capped
      for (let i = 0; i < 40; i++) await w.get('[aria-label="Zoom out"]').trigger('click')
      expect(width()).toBe(Math.round(vw * 0.4))
    } finally {
      uninstall()
    }
  })

  it('opens a node\'s details below the picture, and does not change the size of the picture', async () => {
    install()
    try {
      const w = await mountMap({ client: wide() })
      const [, , vw] = view(w)
      observed.cb([{ contentRect: { width: vw * 0.8 } }])
      await flushPromises()
      const before = widthOf(w)
      await nodeOf(w, 'frontend').trigger('click')
      const panel = w.get('.map-detail')
      expect(w.get('.map-canvas').element.contains(panel.element)).toBe(false) // below it, not over it or beside it
      expect(widthOf(w)).toBe(before) // so selecting never shrinks what you are looking at
    } finally {
      uninstall()
    }
  })

  it('stops watching the canvas size when the page goes away', async () => {
    install()
    try {
      const w = await mountMap({ client: wide() })
      w.unmount()
      expect(observed.disconnected).toBe(true)
    } finally {
      uninstall()
    }
  })

  it('draws at natural size where the browser cannot report a size', async () => {
    const w = await mountMap({ client: wide() })
    expect(widthOf(w)).toBe(view(w)[2])
  })
})

describe('MapView: categories the user chose', () => {
  const memory = () => { const m = new Map(); return { getItem: (k) => (m.has(k) ? m.get(k) : null), setItem: (k, v) => m.set(k, String(v)), removeItem: (k) => m.delete(k) } }
  const storeWith = (setup) => { const c = createCategoryStore({ session: session(), storage: memory() }); setup(c); return c }

  it('takes an application out of the picture when it is moved to a category that is not shown, and brings it back when that is', async () => {
    const categories = storeWith((c) => c.move('/k8s/shop/cartservice', 'monitoring', 'application'))
    const w = await mountMap({ client: wide(), categories })
    expect(nodeNames(w)).not.toContain('cartservice')
    expect(w.findAll('.edge')).toHaveLength(1) // only frontend to the external endpoint is left among what is shown
    await w.findAll('.cat input')[2].setValue(true)
    expect(nodeNames(w)).toContain('cartservice')
  })

  it('draws an application moved into a shown category, whatever its namespace', async () => {
    const categories = storeWith((c) => c.move('/k8s/monitoring/prometheus', 'application', 'monitoring'))
    const w = await mountMap({ client: wide(), categories })
    expect(nodeNames(w)).toContain('prometheus')
    expect(w.findAll('.edge')).toHaveLength(4) // and its call to the frontend
  })

  it('makes a category from the "+", ticks it, and shows applications that are moved to it', async () => {
    const categories = storeWith(() => {})
    const w = await mountMap({ client: wide(), categories })
    await w.get('.cat-plus').trigger('click')
    await w.get('.cat-form input').setValue('edge tier')
    await w.get('.cat-form').trigger('submit')
    await flushPromises()
    expect(w.findAll('.cat input').map((b) => b.element.checked)).toEqual([true, false, false, true])
    await vi.waitFor(() => expect(w.router.currentRoute.value.query.cat).toBe('application,edge tier'))
  })
})

// What the Applications page reads, for the hover menu: memory against its limit, DNS lookups, and so on.
const MB = 1024 * 1024
const WITH_APPS = {
  ...WIDE,
  container_resources_memory_rss_bytes: [
    { labels: { container_id: FE }, points: [{ t: NOW - 1000, v: 100 * MB }] },
    { labels: { container_id: CART }, points: [{ t: NOW - 1000, v: 970 * MB }] }, // 97% of its limit
    { labels: { container_id: PG }, points: [{ t: NOW - 1000, v: 50 * MB }] },
  ],
  container_resources_memory_limit_bytes: [
    { labels: { container_id: FE }, points: [{ t: NOW - 1000, v: 1000 * MB }] },
    { labels: { container_id: CART }, points: [{ t: NOW - 1000, v: 1000 * MB }] },
  ],
  container_dns_requests_total: [
    { labels: { container_id: CART, domain: 'redis.shop.svc', status: 'ok', request_type: 'TypeA' }, points: flat(8) },
    { labels: { container_id: CART, domain: 'redis.shop.svc', status: 'servfail', request_type: 'TypeA' }, points: flat(2) },
  ],
}
const withApps = () => clientOf({ data: WITH_APPS })
const items = (w, name) => nodeOf(w, name).findAll('.node-menu-item').map((a) => ({ text: a.text().replace(/\s+/g, ' '), href: a.attributes('href'), status: a.get('.dot').classes().find((c) => ['ok', 'caution', 'bad', 'unknown'].includes(c)) }))

describe('MapView: the hover menu', () => {
  it('lists instances, CPU, memory, DNS and logs for an application, each a link to that part of its page', async () => {
    const w = await mountMap({ client: withApps() })
    const list = items(w, 'frontend')
    expect(list.map((i) => i.text.split(' ')[0])).toEqual(['Instances', 'CPU', 'Memory', 'DNS', 'Logs'])
    const base = '/applications/%2Fk8s%2Fshop%2Ffrontend'
    expect(list.map((i) => i.href)).toEqual(['instances', 'cpu', 'memory', 'dns', 'logs'].map((a) => `${base}#${a}`))
    expect(nodeOf(w, 'frontend').get('.node-menu').attributes('role')).toBe('menu')
  })

  it('shows how each is doing, from the applications\' own data', async () => {
    const w = await mountMap({ client: withApps() })
    const cart = Object.fromEntries(items(w, 'cartservice').map((i) => [i.text.split(' ')[0], i]))
    expect(cart.Memory.status).toBe('bad') // 97% of its limit
    expect(cart.Memory.text).toContain('97% of its limit')
    expect(cart.DNS.status).toBe('bad') // 2 of 10 lookups fail
    expect(cart.DNS.text).toContain('20% of lookups fail')
    expect(cart.Instances.status).toBe('ok')
    const fe = Object.fromEntries(items(w, 'frontend').map((i) => [i.text.split(' ')[0], i]))
    expect(fe.Memory.status).toBe('ok')
    expect(fe.DNS.status).toBe('unknown') // it made no lookups
  })

  it('marks the aspects as not loaded when there is no data for the application', async () => {
    const w = await mountMap({ client: wide() }) // no container metrics at all
    expect(items(w, 'frontend').every((i) => i.status === 'unknown' || i.status === 'ok')).toBe(true)
    expect(items(w, 'postgres').map((i) => i.text).join(' ')).not.toContain('undefined')
  })

  it('has no menu for an external endpoint', async () => {
    const w = await mountMap({ client: withApps() })
    expect(nodeOf(w, '34.1.2.3:443').find('.node-menu').exists()).toBe(false)
  })

  it('says the menu\'s data could not be loaded, and still draws the map and lists the aspects', async () => {
    const appCache = { get: () => Promise.reject(new Error('The telemetry service is not reachable right now.')) }
    const w = await mountMap({ client: wide(), appCache })
    expect(w.get('[data-testid=apps-error]').text()).toContain('not reachable')
    expect(nodeNames(w)).toContain('frontend')
    expect(items(w, 'frontend').every((i) => i.status === 'unknown')).toBe(true)
    expect(items(w, 'frontend')[0].text).toContain('not loaded')
  })

  it('reads the applications through the cache it was given, and Refresh forces a new read', async () => {
    const get = vi.fn(async () => ({ apps: [] }))
    const w = await mountMap({ client: wide(), appCache: { get } })
    expect(get).toHaveBeenCalledTimes(1)
    expect(get.mock.calls[0][1]).toMatchObject({ project: 'gke', force: false })
    await w.get('.head-right button').trigger('click')
    await flushPromises()
    expect(get).toHaveBeenCalledTimes(2)
    expect(get.mock.calls[1][1].force).toBe(true)
  })
})

describe('MapView: moving an application to a category', () => {
  const memory = () => { const m = new Map(); return { getItem: (k) => (m.has(k) ? m.get(k) : null), setItem: (k, v) => m.set(k, String(v)), removeItem: (k) => m.delete(k) } }
  const fresh = () => createCategoryStore({ session: session(), storage: memory() })
  const kebab = (w, name) => nodeOf(w, name).get('.kebab')
  const menuOf = (w) => w.find('.move-menu')

  it('opens "Move the app to a category" from the three dots, listing the other categories, and closes it again', async () => {
    const w = await mountMap({ client: wide(), categories: fresh() })
    expect(menuOf(w).exists()).toBe(false)
    await kebab(w, 'cartservice').trigger('click')
    expect(kebab(w, 'cartservice').attributes('aria-expanded')).toBe('true')
    expect(menuOf(w).get('.move-title').text()).toBe('Move the app to a category')
    expect(menuOf(w).findAll('.move-item').map((b) => b.text())).toEqual(['+ a new category', '→ control-plane', '→ monitoring'])
    await kebab(w, 'cartservice').trigger('click')
    expect(menuOf(w).exists()).toBe(false)
  })

  it('opens one menu at a time, and not for a click elsewhere on the page', async () => {
    const w = await mountMap({ client: wide(), categories: fresh() })
    await kebab(w, 'cartservice').trigger('click')
    await kebab(w, 'frontend').trigger('click')
    expect(w.findAll('.move-menu')).toHaveLength(1)
    expect(nodeOf(w, 'frontend').find('.move-menu').exists()).toBe(true)
    document.body.click() // a click anywhere else
    await flushPromises()
    expect(menuOf(w).exists()).toBe(false)
  })

  it('does not select the card when its menu is used', async () => {
    const w = await mountMap({ client: wide(), categories: fresh() })
    await kebab(w, 'cartservice').trigger('click')
    expect(w.find('.detail').exists()).toBe(false)
    await menuOf(w).get('.move-title').trigger('click')
    expect(w.find('.detail').exists()).toBe(false)
  })

  it('moves the application, and it leaves the picture when its new category is not shown, saying so', async () => {
    const categories = fresh()
    const w = await mountMap({ client: wide(), categories })
    await kebab(w, 'cartservice').trigger('click')
    await menuOf(w).findAll('.move-item')[2].trigger('click') // monitoring
    expect(categories.categoryFor('/k8s/shop/cartservice', 'application')).toBe('monitoring')
    expect(nodeNames(w)).not.toContain('cartservice')
    expect(menuOf(w).exists()).toBe(false)
    const n = w.get('[data-testid=notice]').text()
    expect(n).toContain('Moved shop/cartservice to monitoring.')
    expect(n).toContain('hidden')
    expect(n).toContain('Show monitoring')
  })

  it('shows the category the application went to, when asked', async () => {
    const w = await mountMap({ client: wide(), categories: fresh() })
    await kebab(w, 'cartservice').trigger('click')
    await menuOf(w).findAll('.move-item')[2].trigger('click')
    await w.findAll('[data-testid=notice] .link').find((b) => b.text() === 'Show monitoring').trigger('click')
    expect(w.findAll('.cat input')[2].element.checked).toBe(true)
    expect(nodeNames(w)).toContain('cartservice')
    expect(w.find('[data-testid=notice]').exists()).toBe(false)
  })

  it('undoes the move', async () => {
    const categories = fresh()
    const w = await mountMap({ client: wide(), categories })
    await kebab(w, 'cartservice').trigger('click')
    await menuOf(w).findAll('.move-item')[2].trigger('click')
    await w.findAll('[data-testid=notice] .link').find((b) => b.text() === 'Undo').trigger('click')
    expect(categories.moved('/k8s/shop/cartservice')).toBe(false)
    expect(nodeNames(w)).toContain('cartservice')
    expect(w.find('[data-testid=notice]').exists()).toBe(false)
  })

  it('does not call it hidden when the category it went to is shown', async () => {
    const w = await mountMap({ client: wide(), categories: fresh() }, '?cat=application,monitoring')
    await kebab(w, 'cartservice').trigger('click')
    await menuOf(w).findAll('.move-item')[2].trigger('click')
    expect(w.get('[data-testid=notice]').text()).not.toContain('hidden')
    expect(nodeNames(w)).toContain('cartservice')
  })

  it('makes a new category from the menu and moves the application to it', async () => {
    const categories = fresh()
    const w = await mountMap({ client: wide(), categories })
    await kebab(w, 'cartservice').trigger('click')
    await menuOf(w).findAll('.move-item')[0].trigger('click')
    await menuOf(w).get('.cat-form input').setValue('Payments Tier')
    await menuOf(w).get('.cat-form').trigger('submit')
    expect(categories.all()).toContain('payments tier')
    expect(categories.categoryFor('/k8s/shop/cartservice', 'application')).toBe('payments tier')
    expect(w.get('[data-testid=notice]').text()).toContain('Moved shop/cartservice to payments tier.')
    expect(w.findAll('.cat').map((l) => l.text())).toContain('payments tier') // and it has a checkbox now
  })

  it('refuses a bad new name and keeps the menu open', async () => {
    const categories = fresh()
    const w = await mountMap({ client: wide(), categories })
    await kebab(w, 'cartservice').trigger('click')
    await menuOf(w).findAll('.move-item')[0].trigger('click')
    await menuOf(w).get('.cat-form input').setValue('a,b')
    await menuOf(w).get('.cat-form').trigger('submit')
    expect(menuOf(w).get('[role=alert]').text()).toContain('lowercase letters')
    expect(menuOf(w).exists()).toBe(true)
    expect(categories.moved('/k8s/shop/cartservice')).toBe(false)
  })

  it('offers to move it back when it has been moved, and moving it back forgets the move', async () => {
    const categories = fresh()
    categories.move('/k8s/shop/cartservice', 'monitoring', 'application')
    const w = await mountMap({ client: wide(), categories }, '?cat=application,monitoring')
    await kebab(w, 'cartservice').trigger('click')
    expect(menuOf(w).findAll('.move-item').map((b) => b.text())).toEqual(['+ a new category', '→ application', '→ control-plane'])
    await menuOf(w).findAll('.move-item')[1].trigger('click')
    expect(categories.moved('/k8s/shop/cartservice')).toBe(false)
  })

  it('opens away from the edge of the picture, so the popup is not cut off', async () => {
    const w = await mountMap({ client: wide(), categories: fresh() })
    await kebab(w, 'cartservice').trigger('click')
    expect(menuOf(w).classes()).toContain('right') // cartservice is in the middle of the picture
    await kebab(w, 'cartservice').trigger('click')
    await kebab(w, 'postgres').trigger('click') // postgres is in the last layer
    expect(menuOf(w).classes()).toContain('left')
  })

  it('is a menu for an application of the project, never for an external endpoint', async () => {
    const w = await mountMap({ client: wide(), categories: fresh() })
    expect(nodeOf(w, '34.1.2.3:443').find('.kebab').exists()).toBe(false)
    expect(w.findAll('.kebab').length).toBe(3) // frontend, cartservice, postgres
  })
})

