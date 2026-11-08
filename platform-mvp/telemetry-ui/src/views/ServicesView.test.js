import { describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { createRouter, createMemoryHistory } from 'vue-router'
import ServicesView from './ServicesView.vue'

const router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/', component: { template: '<i/>' } }, { path: '/services/:name', name: 'service', component: { template: '<i/>' } }] })
const session = (v = { project: 'gke', key: 'k' }) => ({ get: () => v })
const answers = {
  'traces/services': ['frontend', 'cartservice'],
  'logs/services': ['frontend', 'redis'],
  profiles: [{ service: 'cartservice', type: 'cpu' }],
}
const clientOf = (fail = {}) => ({
  get: vi.fn(async (path) => {
    if (fail[path]) throw new Error(fail[path])
    return answers[path]
  }),
})

async function mountView(props = {}) {
  const w = mount(ServicesView, { props: { client: clientOf(), session: session(), ...props }, global: { plugins: [router] } })
  await flushPromises()
  return w
}
const names = (w) => w.findAll('td.name').map((n) => n.text())

describe('ServicesView', () => {
  it('lists the services of the connected project', async () => {
    const w = await mountView()
    expect(names(w)).toEqual(['cartservice', 'frontend', 'redis'])
    expect(w.text()).toContain('gke')
    expect(w.text()).toContain('3 of 3 services')
    expect(w.findAll('tbody tr')[0].text()).toContain('cpu')
  })

  it('does not call the API when not connected, and points to the connection', async () => {
    const client = clientOf()
    const w = await mountView({ client, session: session(null) })
    expect(client.get).not.toHaveBeenCalled()
    expect(w.text()).toContain('Not connected')
    expect(w.find('table').exists()).toBe(false)
  })

  it('filters by name and says when nothing matches', async () => {
    const w = await mountView()
    await w.get('#filter').setValue('RED')
    expect(names(w)).toEqual(['redis'])
    await w.get('#filter').setValue('zzz')
    expect(w.find('table').exists()).toBe(false)
    expect(w.text()).toContain('No service matches "zzz"')
  })

  it('reloads with the chosen window', async () => {
    const client = clientOf()
    const w = await mountView({ client })
    client.get.mockClear()
    await w.get('#window').setValue(String(24 * 3600))
    await flushPromises()
    const [, range] = client.get.mock.calls.find(([p]) => p === 'traces/services')
    expect(range.to - range.from).toBe(24 * 3600)
  })

  it('shows what loaded and warns about the source that did not', async () => {
    const w = await mountView({ client: clientOf({ 'logs/services': 'boom' }) })
    expect(names(w)).toEqual(['cartservice', 'frontend'])
    expect(w.get('[role=status]').text()).toContain('logs')
  })

  it('shows the reason when everything fails', async () => {
    const f = 'This key cannot read project "gke". Check the key and the project name.'
    const w = await mountView({ client: clientOf({ 'traces/services': f, 'logs/services': f, profiles: f }) })
    expect(w.get('[role=alert]').text()).toContain('This key cannot read project')
    expect(w.find('table').exists()).toBe(false)
  })

  it('explains an empty project', async () => {
    const empty = { get: vi.fn(async () => []) }
    const w = await mountView({ client: empty })
    expect(w.text()).toContain('No services in this window')
  })
})
