import { describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import IncidentsView from './IncidentsView.vue'
import { defaultSettings } from '../incidents/settings'

const stub = { template: '<i/>' }
const session = (v = { project: 'gke', key: 'k' }) => ({ get: () => v })
const INCIDENT = { id: 'k3j2h1g0', app: '/k8s/shop/frontend', label: 'shop/frontend', kind: ['availability', 'latency'], start: 1_800_000_000, end: 1_800_000_540, open: false }
const settingsOf = (s = defaultSettings()) => ({ get: () => s })
const result = (o = {}) => ({ incidents: [INCIDENT], from: 0, to: 0, hasData: true, warnings: [], ...o })

async function mountAt(query = '', { list = vi.fn(async () => result()), sess = session(), settings = settingsOf() } = {}) {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: stub }, { path: '/configuration/connection', component: stub },
      { path: '/incidents', name: 'incidents', component: IncidentsView },
      { path: '/incidents/:id', name: 'incident', component: stub },
      { path: '/applications/:name', name: 'application', component: stub },
    ],
  })
  router.push('/incidents' + query)
  await router.isReady()
  const w = mount(IncidentsView, { props: { client: {}, session: sess, settings, loaders: { listIncidents: list } }, global: { plugins: [router] } })
  await flushPromises()
  return { w, router, list }
}

describe('IncidentsView', () => {
  it('lists the incidents with their application, duration and the objectives they broke, each linking to its analysis', async () => {
    const { w } = await mountAt()
    const row = w.findAll('table.incidents tbody tr')[0]
    const cells = row.findAll('td').map((c) => c.text())
    expect(cells[0]).toBe('k3j2h1g0')
    expect(cells[1]).toBe('shop/frontend')
    expect(cells[3]).toBe('9 min')
    expect(cells[4]).toContain('Availability')
    expect(cells[4]).toContain('Latency')
    expect(cells[5]).toBe('Resolved')
    expect(row.get('a.name').attributes('href')).toBe('/incidents/k3j2h1g0?window=21600')
    expect(row.findAll('a')[1].attributes('href')).toBe('/applications/%2Fk8s%2Fshop%2Ffrontend')
  })
  it('says an incident that has not ended is ongoing', async () => {
    const { w } = await mountAt('', { list: async () => result({ incidents: [{ ...INCIDENT, open: true }] }) })
    expect(w.get('table.incidents tbody tr').findAll('td')[5].text()).toBe('Ongoing')
  })
  it('asks for the look-back in the address, and for a new one when it is changed', async () => {
    const { w, list, router } = await mountAt('?window=43200')
    expect(list.mock.calls[0][1]).toMatchObject({ seconds: 43200 })
    await w.get('#window').setValue(86400)
    await flushPromises()
    expect(list.mock.calls[1][1]).toMatchObject({ seconds: 86400 })
    expect(router.currentRoute.value.query.window).toBe('86400')
  })
  it('ignores a look-back it does not offer', async () => {
    const { list } = await mountAt('?window=5')
    expect(list.mock.calls[0][1]).toMatchObject({ seconds: 21600 })
  })
  it('says so when there are none, and when there is nothing to judge', async () => {
    const none = await mountAt('', { list: async () => result({ incidents: [] }) })
    expect(none.w.get('[data-testid=none]').text()).toContain('No incidents in the last 6 hours')
    const nodata = await mountAt('', { list: async () => result({ incidents: [], hasData: false }) })
    expect(nodata.w.text()).toContain('no inbound request metrics')
    expect(nodata.w.find('[data-testid=none]').exists()).toBe(false) // "no incidents" would be a false comfort
  })
  it('shows what the service warned about, and a failure instead of a table', async () => {
    const warned = await mountAt('', { list: async () => result({ warnings: ['No request-duration histogram.'] }) })
    expect(warned.w.get('[role=note]').text()).toContain('histogram')
    const failed = await mountAt('', { list: async () => { throw new Error('The read key was not accepted.') } })
    expect(failed.w.get('[role=alert]').text()).toContain('not accepted')
    expect(failed.w.find('table').exists()).toBe(false)
  })
  it('asks nothing when not connected', async () => {
    const { w, list } = await mountAt('', { sess: session(null) })
    expect(list).not.toHaveBeenCalled()
    expect(w.text()).toContain('Not connected')
  })

  it('says which objectives it judges by, and offers to change them', async () => {
    const { w } = await mountAt()
    expect(w.get('[data-testid=rules]').text()).toContain('99.9% of requests should not fail, and 99.9% should be served in under 500 ms')
    const link = w.get('[data-testid=rules] a')
    expect(link.attributes('href')).toBe('/configuration/incidents')
    expect(link.text()).toBe('Change the rules')
  })
  it('hands the saved rules to the search, as fractions and seconds, with each application\'s own', async () => {
    const s = defaultSettings()
    Object.assign(s.slo, { availability: 99, latencyMs: 250, burnThreshold: 6 })
    s.apps = { '/k8s/shop/frontend': { availability: 99.99 } }
    const { w, list } = await mountAt('', { settings: settingsOf(s) })
    const opts = list.mock.calls[0][1]
    expect(opts.config).toMatchObject({ availability: 0.99, latencySeconds: 0.25, burnThreshold: 6 })
    expect(opts.appConfig).toEqual({ '/k8s/shop/frontend': { availability: 0.9999 } })
    const text = w.get('[data-testid=rules]').text()
    expect(text).toContain('99% of requests should not fail')
    expect(text).toContain('under 250 ms')
    expect(text).toContain('1 application has its own')
    expect(w.get('[data-testid=rules] a').text()).toContain('your settings')
  })
})
