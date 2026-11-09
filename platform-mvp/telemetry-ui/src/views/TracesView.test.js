import { describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { createRouter, createMemoryHistory } from 'vue-router'
import TracesView from './TracesView.vue'

const stub = { template: '<i/>' }
const session = (v = { project: 'gke', key: 'k' }) => ({ get: () => v })
const T = '2026-01-01T12:00:00Z'
const span = (n, o = {}) => ({ span_id: `s${n}`, trace_id: String(n).repeat(32).slice(0, 32), time: T, name: `GET /${n}`, kind: 'CLIENT', service: '/k8s/shop/frontend', duration_ns: 25e6, status_code: 'OK', ...o })
const ROWS = [span(1), span(2, { status_code: 'ERROR', duration_ns: 1.5e9 })]

const clientOf = ({ rows = ROWS, fail } = {}) => ({
  get: vi.fn(async (path) => {
    if (path === 'traces/services') return ['/k8s/shop/frontend', '/k8s/shop/cart']
    if (path === 'logs/services') return []
    if (path === 'traces') {
      if (fail) throw new Error(fail)
      return rows
    }
    throw new Error('unexpected ' + path)
  }),
})
const searches = (c) => c.get.mock.calls.filter(([p]) => p === 'traces')

async function mountAt(query = '', props = {}) {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [{ path: '/', component: stub }, { path: '/connection', component: stub }, { path: '/traces', name: 'traces', component: TracesView }, { path: '/traces/:id', name: 'trace', component: stub }, { path: '/applications/:name', name: 'application', component: stub }],
  })
  router.push('/traces' + query)
  await router.isReady()
  const w = mount(TracesView, { props: { client: clientOf(), session: session(), ...props }, global: { plugins: [router] } })
  await flushPromises()
  return { w, router }
}

describe('TracesView', () => {
  it('lists the newest spans with their status, duration, and links to the service and trace', async () => {
    const { w } = await mountAt()
    const rows = w.findAll('tbody tr')
    expect(rows).toHaveLength(2)
    expect(rows[0].text()).toContain('GET /1')
    expect(rows[0].text()).toContain('25 ms')
    expect(rows[1].text()).toContain('1.5 s')
    expect(rows[1].get('td.bad').text()).toBe('error')
    expect(rows[0].findAll('a')[0].attributes('href')).toBe('/applications/%2Fk8s%2Fshop%2Ffrontend')
    expect(rows[0].findAll('a')[1].attributes('href')).toBe(`/traces/${'1'.repeat(32)}`)
    expect(w.text()).toContain('2 spans')
  })

  it('searches on load with the defaults, then with the filters typed', async () => {
    const client = clientOf()
    const { w } = await mountAt('', { client })
    expect(searches(client)).toHaveLength(1)
    expect(searches(client)[0][1]).toMatchObject({ limit: 100 })
    expect(searches(client)[0][1].service).toBeUndefined()

    await w.get('#service').setValue('/k8s/shop/cart')
    await w.get('#name').setValue('GET /cart')
    await w.get('#status').setValue('error')
    await w.get('#min').setValue('250')
    await w.get('#root').setValue(true)
    await w.get('#limit').setValue('500')
    await w.get('form').trigger('submit')
    await flushPromises()
    expect(searches(client)[1][1]).toMatchObject({ service: '/k8s/shop/cart', name: 'GET /cart', status: 'error', min_duration_ms: '250', root: true, limit: 500 })
  })

  it('starts from the filters in the address, and keeps them there', async () => {
    const client = clientOf()
    const { w, router } = await mountAt('?service=%2Fk8s%2Fshop%2Fcart&status=error&min=100&root=true&limit=500', { client })
    expect(searches(client)[0][1]).toMatchObject({ service: '/k8s/shop/cart', status: 'error', min_duration_ms: '100', root: true, limit: 500 })
    expect(w.get('#service').element.value).toBe('/k8s/shop/cart')
    expect(router.currentRoute.value.query).toEqual({ service: '/k8s/shop/cart', status: 'error', min: '100', root: 'true', limit: '500' })
  })

  it('ignores junk in the address instead of sending it', async () => {
    const client = clientOf()
    await mountAt('?status=bogus&limit=99999&window=7', { client })
    const p = searches(client)[0][1]
    expect(p.status).toBeUndefined()
    expect(p.limit).toBe(100)
  })

  it('refuses a bad minimum duration without calling the API', async () => {
    const client = clientOf()
    const { w } = await mountAt('', { client })
    await w.get('#min').setValue('1.5')
    expect(w.get('[role=alert]').text()).toContain('whole number')
    expect(w.get('button[type=submit]').attributes('disabled')).toBeDefined()
    await w.get('form').trigger('submit')
    expect(searches(client)).toHaveLength(1)
  })

  it('says when nothing matches', async () => {
    const { w } = await mountAt('', { client: clientOf({ rows: [] }) })
    expect(w.text()).toContain('No spans match')
    expect(w.find('table').exists()).toBe(false)
  })

  it('says the list is only the newest page when it is full', async () => {
    const rows = Array.from({ length: 100 }, (_, i) => span(i % 9 + 1, { span_id: `x${i}` }))
    const { w } = await mountAt('', { client: clientOf({ rows }) })
    expect(w.get('[role=status]').text()).toContain('newest 100 spans')
  })

  it('shows the reason when the search fails', async () => {
    const { w } = await mountAt('', { client: clientOf({ fail: 'This key cannot read project "gke". Check the key and the project name.' }) })
    expect(w.get('[role=alert]').text()).toContain('This key cannot read project')
    expect(w.find('table').exists()).toBe(false)
  })

  it('offers the services as suggestions', async () => {
    const { w } = await mountAt()
    expect(w.findAll('#service-names option').map((o) => o.attributes('value'))).toEqual(['/k8s/shop/cart', '/k8s/shop/frontend'])
  })

  it('does not call the API when not connected', async () => {
    const client = clientOf()
    const { w } = await mountAt('', { client, session: session(null) })
    expect(client.get).not.toHaveBeenCalled()
    expect(w.text()).toContain('Not connected')
  })
})
