import { describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { createRouter, createMemoryHistory } from 'vue-router'
import MapView from './MapView.vue'

const stub = { template: '<i/>' }
const router = createRouter({
  history: createMemoryHistory(),
  routes: [{ path: '/', component: stub }, { path: '/connection', component: stub }, { path: '/applications/:name', name: 'application', component: stub }],
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
async function mountMap(props = {}) {
  const w = mount(MapView, { props: { client: clientOf(), session: session(), ...props }, global: { plugins: [router] } })
  await flushPromises()
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
    expect(fe.attributes('aria-pressed')).toBe('true')
    await fe.trigger('click')
    expect(w.find('.detail').exists()).toBe(false)
  })

  it('an external endpoint has no service page', async () => {
    const w = await mountMap()
    await w.findAll('.node').find((n) => n.text().includes('34.1.2.3')).trigger('click')
    expect(w.get('.detail').text()).toContain('external')
    expect(w.get('.detail').find('a').exists()).toBe(false)
  })

  it('can be operated from the keyboard', async () => {
    const w = await mountMap()
    const n = w.findAll('.node')[0]
    expect(n.attributes('tabindex')).toBe('0')
    await n.trigger('keydown', { key: 'Enter' })
    expect(w.find('.detail').exists()).toBe(true)
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
