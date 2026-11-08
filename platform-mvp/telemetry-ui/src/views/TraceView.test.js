import { describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { createRouter, createMemoryHistory } from 'vue-router'
import TraceView from './TraceView.vue'

const stub = { template: '<i/>' }
const session = (v = { project: 'gke', key: 'k' }) => ({ get: () => v })
const ID = 'ab'.repeat(16)
const T = Date.parse('2026-01-01T12:00:00Z')
const sp = (id, parent, off, ms, o = {}) => ({ span_id: id, parent_span_id: parent, trace_id: ID, name: `op-${id}`, kind: 'SERVER', service: 'shop/' + id, time: new Date(T + off).toISOString(), duration_ns: ms * 1e6, status_code: 'OK', ...o })

const clientOf = (rows, fail) => ({
  get: vi.fn(async () => {
    if (fail) throw new Error(fail)
    return rows
  }),
})
async function mountAt(id, props = {}) {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [{ path: '/', component: stub }, { path: '/traces', component: stub }, { path: '/logs', name: 'logs', component: stub }, { path: '/services/:name', name: 'service', component: stub }, { path: '/traces/:id', name: 'trace', component: TraceView }],
  })
  router.push(`/traces/${id}`)
  await router.isReady()
  const w = mount(TraceView, { props: { client: clientOf([]), session: session(), ...props }, global: { plugins: [router] } })
  await flushPromises()
  return w
}

describe('TraceView', () => {
  const NESTED = [sp('b', 'a', 10, 20), sp('a', '', 0, 100, { span_attributes: { 'http.url': 'http://x/y', 'http.method': 'GET' } }), sp('c', 'b', 12, 5, { status_code: 'ERROR', status_message: 'boom' })]

  it('draws the spans nested under their parents, in order, with durations', async () => {
    const w = await mountAt(ID, { client: clientOf(NESTED) })
    const rows = w.findAll('.wf-row')
    expect(rows.map((r) => r.find('.name').text())).toEqual(['op-a', 'op-b', 'op-c'])
    expect(rows[0].get('.wf-dur').text()).toBe('100 ms')
    expect(rows[1].get('.wf-label').attributes('style')).toContain('padding-left: 14px')
    expect(rows[2].get('.wf-label').attributes('style')).toContain('padding-left: 28px')
    expect(rows[2].get('.wf-bar').classes()).toContain('bad')
    expect(w.text()).not.toContain('no parent links')
  })

  it('opens the first span, and shows another when it is selected, with its attributes', async () => {
    const w = await mountAt(ID, { client: clientOf(NESTED) })
    expect(w.get('.detail').text()).toContain('op-a')
    expect(w.get('.detail').text()).toContain('http.url')
    await w.findAll('.wf-row')[2].trigger('click')
    const d = w.get('.detail').text()
    expect(d).toContain('op-c')
    expect(d).toContain('boom')
    expect(d).not.toContain('http.url')
    expect(w.findAll('.wf-row')[2].attributes('aria-pressed')).toBe('true')
  })

  it('explains a flat trace, as the node-agent produces, and does not nest it', async () => {
    const flat = [sp('a', '', 0, 5), sp('b', '', 50, 5)]
    const w = await mountAt(ID, { client: clientOf(flat) })
    expect(w.text()).toContain('no parent links')
    expect(w.findAll('.wf-label').every((l) => l.attributes('style').includes('padding-left: 0px'))).toBe(true)
  })

  it('needs no explanation for a single span, and links to the trace\'s logs', async () => {
    const w = await mountAt(ID, { client: clientOf([sp('a', '', 0, 5)]) })
    expect(w.text()).not.toContain('no parent links')
    expect(w.text()).toContain('1 span in 1 service')
    expect(w.get('a[href*="/logs"]').attributes('href')).toBe(`/logs?trace_id=${ID}`)
  })

  it('reads the trace by the id in the address, lower-cased', async () => {
    const client = clientOf([sp('a', '', 0, 5)])
    await mountAt(ID.toUpperCase(), { client })
    expect(client.get).toHaveBeenCalledWith(`traces/${ID}`)
  })

  it('refuses a bad id without calling the API', async () => {
    const client = clientOf([])
    const w = await mountAt('not-a-trace-id', { client })
    expect(client.get).not.toHaveBeenCalled()
    expect(w.get('[role=alert]').text()).toContain('32 hex')
  })

  it('says so for a trace with no spans, and shows the reason when the read fails', async () => {
    expect((await mountAt(ID, { client: clientOf([]) })).text()).toContain('No spans found')
    const failed = await mountAt(ID, { client: clientOf([], 'The telemetry service is not reachable right now.') })
    expect(failed.get('[role=alert]').text()).toContain('not reachable')
  })

  it('does not call the API when not connected', async () => {
    const client = clientOf([])
    const w = await mountAt(ID, { client, session: session(null) })
    expect(client.get).not.toHaveBeenCalled()
    expect(w.text()).toContain('Not connected')
  })
})
