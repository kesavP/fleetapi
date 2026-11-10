import { describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import ApplicationsView from './ApplicationsView.vue'
import LogsView from './LogsView.vue'
import MapView from './MapView.vue'
import ProfilesView from './ProfilesView.vue'
import TracesView from './TracesView.vue'

const stub = { template: '<i/>' }
const NOW = Date.now()
const sessionOf = (v = { project: 'gke', key: 'k' }) => ({ get: () => v, subscribe: () => () => {} })

// A project whose metrics are `names`. Every other read fails, as the pages cope with: the notice must not depend on them.
const clientOf = (names, fresh = true) => ({
  get: vi.fn(async (path) => {
    if (path === 'metrics') return names
    if (path === 'metrics/query_range') return fresh ? { series: [{ labels: {}, points: [{ t: NOW, v: 1 }] }] } : { series: [] }
    throw new Error('not available in this test')
  }),
})

const VIEWS = { Applications: ApplicationsView, Map: MapView, Profiles: ProfilesView, Traces: TracesView, Logs: LogsView }

async function mountView(view, client, session = sessionOf()) {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: stub }, { path: '/connection', component: stub }, { path: '/configuration/connection', component: stub },
      { path: '/applications', name: 'applications', component: stub }, { path: '/applications/:name', name: 'application', component: stub },
      { path: '/map', name: 'map', component: stub }, { path: '/traces', name: 'traces', component: stub }, { path: '/traces/:id', name: 'trace', component: stub },
      { path: '/logs', name: 'logs', component: stub }, { path: '/profiles', name: 'profiles', component: stub },
    ],
  })
  router.push('/')
  await router.isReady()
  const w = mount(view, {
    props: { client, session },
    global: { plugins: [router], provide: { client, session, alerts: null, categories: null, appCache: null }, stubs: { RouterLink: { template: '<a><slot/></a>' } } },
  })
  await flushPromises()
  return w
}

describe.each(Object.entries(VIEWS))('%s page', (_, view) => {
  it('tells you the node-agent is not set up when the project has no telemetry', async () => {
    const w = await mountView(view, clientOf([]))
    const n = w.find('[data-testid=telemetry-notice]')
    expect(n.exists()).toBe(true)
    expect(n.text()).toContain('No telemetry has arrived for the project “gke”')
    expect(n.text()).toContain('kubectl -n telemetry-agent get daemonset node-agent')
  })

  it('says so when only other metrics arrive, and when the node-agent has stopped', async () => {
    expect((await mountView(view, clientOf(['up']))).get('[data-testid=telemetry-notice]').attributes('data-state')).toBe('other')
    expect((await mountView(view, clientOf(['container_resources_memory_rss_bytes'], false))).get('[data-testid=telemetry-notice]').attributes('data-state')).toBe('stale')
  })

  it('has no notice when the node-agent is reporting, and none when not connected', async () => {
    expect((await mountView(view, clientOf(['container_resources_memory_rss_bytes']))).find('[data-testid=telemetry-notice]').exists()).toBe(false)
    expect((await mountView(view, clientOf([]), sessionOf(null))).find('[data-testid=telemetry-notice]').exists()).toBe(false)
  })
})
