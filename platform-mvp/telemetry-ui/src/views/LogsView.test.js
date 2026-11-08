import { describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { createRouter, createMemoryHistory } from 'vue-router'
import LogsView from './LogsView.vue'

const stub = { template: '<i/>' }
const session = (v = { project: 'gke', key: 'k' }) => ({ get: () => v })
const TID = 'cd'.repeat(16)
const log = (n, o = {}) => ({ time: '2026-01-01T12:00:00Z', severity: 'INFO', severity_number: 9, service: '/k8s/shop/frontend', body: `line ${n}`, ...o })
const ROWS = [log(1), log(2, { severity: 'ERROR', severity_number: 17, body: 'cart failed\nsecond line', trace_id: TID, log_attributes: { 'log.file': '/var/log/x' } })]

const clientOf = ({ rows = ROWS, fail } = {}) => ({
  get: vi.fn(async (path) => {
    if (path === 'traces/services') return ['/k8s/shop/frontend']
    if (path === 'logs/services') return ['/k8s/shop/cart']
    if (path === 'logs') {
      if (fail) throw new Error(fail)
      return rows
    }
    throw new Error('unexpected ' + path)
  }),
})
const searches = (c) => c.get.mock.calls.filter(([p]) => p === 'logs')

async function mountAt(query = '', props = {}) {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [{ path: '/', component: stub }, { path: '/logs', name: 'logs', component: LogsView }, { path: '/traces/:id', name: 'trace', component: stub }, { path: '/services/:name', name: 'service', component: stub }],
  })
  router.push('/logs' + query)
  await router.isReady()
  const w = mount(LogsView, { props: { client: clientOf(), session: session(), ...props }, global: { plugins: [router] } })
  await flushPromises()
  return { w, router }
}

describe('LogsView', () => {
  it('lists the newest lines with severity, service, body and a link to the trace', async () => {
    const { w } = await mountAt()
    const items = w.findAll('.log-lines li')
    expect(items).toHaveLength(2)
    expect(items[0].get('.log-body').text()).toBe('line 1')
    expect(items[1].get('.tag').attributes('data-sev')).toBe('error')
    expect(items[1].get('.log-body').text()).toBe('cart failed\nsecond line') // kept as logged
    expect(items[1].find('a[href^="/traces/"]').attributes('href')).toBe(`/traces/${TID}`)
    expect(items[0].find('a[href^="/traces/"]').exists()).toBe(false)
    expect(w.text()).toContain('2 lines')
  })

  it('shows a line\'s attributes on request', async () => {
    const { w } = await mountAt()
    expect(w.findAll('.log-lines li')[0].find('details').exists()).toBe(false)
    expect(w.findAll('.log-lines li')[1].get('details').text()).toContain('log.file')
  })

  it('does not interpret markup in a log body', async () => {
    const { w } = await mountAt('', { client: clientOf({ rows: [log(1, { body: '<img src=x onerror=alert(1)>' })] }) })
    expect(w.find('.log-body img').exists()).toBe(false)
    expect(w.get('.log-body').text()).toBe('<img src=x onerror=alert(1)>')
  })

  it('sends the filters typed, lower-casing the trace id', async () => {
    const client = clientOf()
    const { w } = await mountAt('', { client })
    await w.get('#service').setValue('/k8s/shop/cart')
    await w.get('#severity').setValue('ERROR')
    await w.get('#text').setValue('timeout')
    await w.get('#trace').setValue(TID.toUpperCase())
    await w.get('#limit').setValue('1000')
    await w.get('form').trigger('submit')
    await flushPromises()
    expect(searches(client)[1][1]).toMatchObject({ service: '/k8s/shop/cart', severity: 'ERROR', q: 'timeout', trace_id: TID, limit: 1000 })
  })

  it('opens already filtered when linked from a trace or a service', async () => {
    const client = clientOf()
    const { w, router } = await mountAt(`?trace_id=${TID}&service=%2Fk8s%2Fshop%2Ffrontend`, { client })
    expect(searches(client)[0][1]).toMatchObject({ trace_id: TID, service: '/k8s/shop/frontend' })
    expect(w.get('#trace').element.value).toBe(TID)
    expect(router.currentRoute.value.query).toEqual({ trace_id: TID, service: '/k8s/shop/frontend' })
  })

  it('refuses a malformed trace id without calling the API', async () => {
    const client = clientOf()
    const { w } = await mountAt('', { client })
    await w.get('#trace').setValue('xyz')
    expect(w.get('[role=alert]').text()).toContain('32 hex')
    await w.get('form').trigger('submit')
    expect(searches(client)).toHaveLength(1)
  })

  it('says when nothing matches, when the page is full, and why a search failed', async () => {
    expect((await mountAt('', { client: clientOf({ rows: [] }) })).w.text()).toContain('No log lines match')
    const rows = Array.from({ length: 100 }, (_, i) => log(i))
    expect((await mountAt('', { client: clientOf({ rows }) })).w.get('[role=status]').text()).toContain('newest 100 lines')
    const failed = (await mountAt('', { client: clientOf({ fail: 'The telemetry service is not reachable right now.' }) })).w
    expect(failed.get('[role=alert]').text()).toContain('not reachable')
  })

  it('suggests services from both traces and logs', async () => {
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
