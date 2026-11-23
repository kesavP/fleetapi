import { describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { createRouter, createMemoryHistory } from 'vue-router'
import ProfilesView from './ProfilesView.vue'

const stub = { template: '<i/>' }
const session = (v = { project: 'gke', key: 'k' }) => ({ get: () => v })
const TARGETS = [
  { service: '/k8s/shop/cart', type: 'alloc_space:bytes', last_seen: 't' },
  { service: '/k8s/shop/cart', type: 'cpu:nanoseconds', last_seen: 't' },
  { service: '/k8s/shop/frontend', type: 'cpu:nanoseconds', last_seen: 't' },
]
// Leaf first. 6 s in parse, 3 s in send, 1 s in gc: 10 s in all.
const STACKS = [
  { frames: ['parse', 'handle', 'main'], value: 6e9 },
  { frames: ['send', 'handle', 'main'], value: 3e9 },
  { frames: ['gc', 'main'], value: 1e9 },
]
const clientOf = ({ targets = TARGETS, stacks = STACKS, fail = {} } = {}) => ({
  get: vi.fn(async (path) => {
    if (fail[path]) throw new Error(fail[path])
    if (path === 'profiles') return targets
    if (path === 'profiles/top') return stacks
    throw new Error('unexpected ' + path)
  }),
})
const tops = (c) => c.get.mock.calls.filter(([p]) => p === 'profiles/top')

async function mountAt(query = '', props = {}) {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [{ path: '/', component: stub }, { path: '/connection', component: stub }, { path: '/profiles', name: 'profiles', component: ProfilesView }],
  })
  router.push('/profiles' + query)
  await router.isReady()
  const w = mount(ProfilesView, { props: { client: clientOf(), session: session(), ...props }, global: { plugins: [router] } })
  await flushPromises()
  return { w, router }
}
const nodes = (w) => w.findAll('.flame-node')
const node = (w, name) => nodes(w).find((n) => n.text() === name)

describe('ProfilesView', () => {
  it('starts with the first service and its CPU profile, and says how much it holds', async () => {
    const client = clientOf()
    const { w } = await mountAt('', { client })
    expect(tops(client)).toHaveLength(1)
    expect(tops(client)[0][1]).toMatchObject({ service: '/k8s/shop/cart', type: 'cpu:nanoseconds', limit: 2000 })
    expect(w.get('.summary').text()).toContain('10 s in 3 stacks')
    expect(w.get('.summary').text()).toContain('cores on average') // 10 s over the 1 h window
  })

  it('draws the flame graph: callers below-to-above as rows, widths as shares', async () => {
    const { w } = await mountAt()
    expect(w.findAll('.flame-row').map((r) => r.findAll('.flame-node').map((n) => n.text()))).toEqual([['all'], ['main'], ['handle', 'gc'], ['parse', 'send']])
    expect(node(w, 'handle').attributes('style')).toContain('width: 90%')
    expect(node(w, 'gc').attributes('style')).toContain('left: 90%')
    expect(node(w, 'parse').attributes('style')).toContain('width: 60%')
  })

  it('describes a box on hover and in its title', async () => {
    const { w } = await mountAt()
    await node(w, 'handle').trigger('mouseenter')
    const text = w.get('.hover').text()
    expect(text).toContain('handle')
    expect(text).toContain('9 s')
    expect(text).toContain('90% of view')
    expect(node(w, 'handle').attributes('title')).toContain('self 0 ns')
  })

  it('zooms into a box, shows the path, and goes back', async () => {
    const { w } = await mountAt()
    await node(w, 'handle').trigger('click')
    expect(w.findAll('.flame-row').map((r) => r.findAll('.flame-node').map((n) => n.text()))).toEqual([['handle'], ['parse', 'send']])
    expect(node(w, 'handle').attributes('style')).toContain('width: 100%')
    expect(node(w, 'parse').attributes('style')).toContain('width: 66.666')
    expect(w.get('.crumbs').text()).toContain('main')
    await w.get('.crumbs button.link:not(:disabled)').trigger('click') // "all"
    expect(nodes(w).length).toBe(6)
  })

  it('highlights frames matching the text and counts them', async () => {
    const { w } = await mountAt()
    await w.get('#find').setValue('PARS')
    expect(node(w, 'parse').classes()).not.toContain('dim')
    expect(node(w, 'send').classes()).toContain('dim')
    expect(w.get('[role=status]').text()).toContain('1 match')
  })

  it('lists the hottest functions by self time, with self and total', async () => {
    const { w } = await mountAt()
    const rows = w.findAll('table.functions tbody tr').map((r) => r.findAll('td').map((c) => c.text()))
    expect(rows[0]).toEqual(['parse', '6 s', '60%', '6 s', '60%'])
    expect(rows[1]).toEqual(['send', '3 s', '30%', '3 s', '30%'])
    expect(rows.find((r) => r[0] === 'main')).toEqual(['main', '0 ns', '0%', '10 s', '100%'])
  })

  it('changes the profile list with the service and keeps a type the new service has', async () => {
    const client = clientOf()
    const { w } = await mountAt('?service=%2Fk8s%2Fshop%2Fcart&type=alloc_space%3Abytes', { client })
    expect(tops(client)[0][1].type).toBe('alloc_space:bytes')
    expect(w.findAll('#type option').map((o) => o.attributes('value'))).toEqual(['alloc_space:bytes', 'cpu:nanoseconds'])
    await w.get('#service').setValue('/k8s/shop/frontend')
    expect(w.get('#type').element.value).toBe('cpu:nanoseconds') // frontend has no alloc profile
    await w.get('form').trigger('submit')
    await flushPromises()
    expect(tops(client)[1][1]).toMatchObject({ service: '/k8s/shop/frontend', type: 'cpu:nanoseconds' })
  })

  it('formats the values in the profile\'s own unit', async () => {
    const stacks = [{ frames: ['alloc', 'main'], value: 64 * 1024 * 1024 }]
    const { w } = await mountAt('?service=%2Fk8s%2Fshop%2Fcart&type=alloc_space%3Abytes', { client: clientOf({ stacks }) })
    expect(w.get('.summary').text()).toContain('64 MiB')
    expect(w.get('.summary').text()).not.toContain('cores') // only a CPU profile has cores
  })

  it('keeps the choice in the address, and ignores junk there', async () => {
    const { router } = await mountAt('?service=%2Fk8s%2Fshop%2Ffrontend&type=cpu%3Ananoseconds&limit=5000&window=900')
    expect(router.currentRoute.value.query).toEqual({ service: '/k8s/shop/frontend', type: 'cpu:nanoseconds', window: '900', limit: '5000' })
    const client = clientOf()
    await mountAt('?limit=1&window=3', { client })
    expect(tops(client)[0][1].limit).toBe(2000)
  })

  it('falls back to the first service when the address names one that has no profiles', async () => {
    const client = clientOf()
    await mountAt('?service=nope', { client })
    expect(tops(client)[0][1].service).toBe('/k8s/shop/cart')
  })

  it('warns that the figures are of the top stacks only when the page is full', async () => {
    const stacks = Array.from({ length: 2000 }, (_, i) => ({ frames: [`f${i}`, 'main'], value: 1 + (i % 5) }))
    const { w } = await mountAt('', { client: clientOf({ stacks }) })
    expect(w.get('[role=status]').text()).toContain('top 2000 stacks')
  })

  it('says so when a service has no samples in the window, and when the project has no profiles', async () => {
    expect((await mountAt('', { client: clientOf({ stacks: [] }) })).w.text()).toContain('No samples for this service')
    const none = await mountAt('', { client: clientOf({ targets: [] }) })
    expect(none.w.text()).toContain('No profiles in this project yet')
    expect(none.w.find('form').exists()).toBe(false)
  })

  it('shows the reason when profiles cannot be read, at either step', async () => {
    const f = 'This key cannot read project "gke". Check the key and the project name.'
    expect((await mountAt('', { client: clientOf({ fail: { profiles: f } }) })).w.get('[role=alert]').text()).toContain('cannot read project')
    const second = await mountAt('', { client: clientOf({ fail: { 'profiles/top': 'The telemetry service is not reachable right now.' } }) })
    expect(second.w.get('[role=alert]').text()).toContain('not reachable')
    expect(second.w.find('.flame').exists()).toBe(false)
  })

  it('does not call the API when not connected', async () => {
    const client = clientOf()
    const { w } = await mountAt('', { client, session: session(null) })
    expect(client.get).not.toHaveBeenCalled()
    expect(w.text()).toContain('Not connected')
  })
})
