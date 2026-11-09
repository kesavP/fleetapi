import { describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import ConnectionView from './ConnectionView.vue'

const stub = { template: '<i/>' }
const panel = { name: 'ConnectionPanelStub', template: '<div class="panel-stub" />', emits: ['connected', 'disconnected'] }
async function mountView(alerts) {
  const router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/connection', component: ConnectionView }, { path: '/applications', component: stub }] })
  router.push('/connection')
  await router.isReady()
  const w = mount(ConnectionView, { global: { plugins: [router], provide: { alerts }, stubs: { ConnectionPanel: panel } } })
  await flushPromises()
  return { w, router }
}

describe('ConnectionView', () => {
  it('opens the applications, and has them judged, as soon as a connection is made', async () => {
    const alerts = { evaluate: vi.fn(), reset: vi.fn() }
    const { w, router } = await mountView(alerts)
    w.findComponent(panel).vm.$emit('connected')
    await flushPromises()
    expect(alerts.evaluate).toHaveBeenCalledTimes(1)
    expect(router.currentRoute.value.fullPath).toBe('/applications')
  })

  it('forgets the project\'s alerts on a disconnect, and stays on the page', async () => {
    const alerts = { evaluate: vi.fn(), reset: vi.fn() }
    const { w, router } = await mountView(alerts)
    w.findComponent(panel).vm.$emit('disconnected')
    await flushPromises()
    expect(alerts.reset).toHaveBeenCalledTimes(1)
    expect(router.currentRoute.value.fullPath).toBe('/connection')
  })

  it('works without an alert store', async () => {
    const { w, router } = await mountView(null)
    w.findComponent(panel).vm.$emit('connected')
    await flushPromises()
    expect(router.currentRoute.value.fullPath).toBe('/applications')
  })

  it('has a title', async () => {
    const { w } = await mountView(null)
    expect(w.get('h1').text()).toBe('Connection')
  })
})
