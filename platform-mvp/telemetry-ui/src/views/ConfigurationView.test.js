import { describe, expect, it } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import ConfigurationView from './ConfigurationView.vue'

const tabStub = { template: '<div class="ch-stub" />' }
const rulesStub = { template: '<div class="rules-stub" />' }
const connStub = { template: '<div class="conn-stub" />' }
const clustersStub = { template: '<div class="clusters-stub" />' }
async function mountAt(path) {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/configuration/connection', name: 'connection', component: ConfigurationView, meta: { tab: 'connection' } },
      { path: '/configuration/clusters', name: 'clusters', component: ConfigurationView, meta: { tab: 'clusters' } },
      { path: '/configuration/incidents', name: 'incident-rules', component: ConfigurationView, meta: { tab: 'incidents' } },
      { path: '/configuration/clickhouse', name: 'clickhouse', component: ConfigurationView, meta: { tab: 'clickhouse' } },
    ],
  })
  router.push(path)
  await router.isReady()
  const w = mount(ConfigurationView, { global: { plugins: [router], stubs: { ClickHouseTab: tabStub, ConnectionView: connStub, IncidentSettingsTab: rulesStub, ClustersTab: clustersStub } } })
  await flushPromises()
  return { w, router }
}

describe('ConfigurationView', () => {
  it('has a Connection and a ClickHouse tab, and shows the connection one at /configuration/connection', async () => {
    const { w } = await mountAt('/configuration/connection')
    expect(w.get('h1').text()).toBe('Configuration')
    expect(w.findAll('.tab').map((t) => t.text())).toEqual(['Connection', 'Clusters', 'Incidents', 'ClickHouse'])
    expect(w.findAll('.tab.active').map((t) => t.text())).toEqual(['Connection'])
    expect(w.find('.conn-stub').exists()).toBe(true)
    expect(w.find('.ch-stub').exists()).toBe(false)
  })

  it('shows the ClickHouse tab at /configuration/clickhouse, and marks it as the current page', async () => {
    const { w } = await mountAt('/configuration/clickhouse')
    expect(w.find('.ch-stub').exists()).toBe(true)
    expect(w.find('.conn-stub').exists()).toBe(false)
    const active = w.findAll('.tab.active')
    expect(active.map((t) => t.text())).toEqual(['ClickHouse'])
    expect(active[0].attributes('aria-current')).toBe('page')
  })

  it('shows the clusters at /configuration/clusters', async () => {
    const { w } = await mountAt('/configuration/clusters')
    expect(w.find('.clusters-stub').exists()).toBe(true)
    expect(w.find('.conn-stub').exists()).toBe(false)
    expect(w.findAll('.tab.active').map((t) => t.text())).toEqual(['Clusters'])
  })

  it('shows the incident rules at /configuration/incidents', async () => {
    const { w } = await mountAt('/configuration/incidents')
    expect(w.find('.rules-stub').exists()).toBe(true)
    expect(w.find('.ch-stub').exists()).toBe(false)
    expect(w.findAll('.tab.active').map((t) => t.text())).toEqual(['Incidents'])
  })

  it('switches tab by link', async () => {
    const { w, router } = await mountAt('/configuration/connection')
    await w.findAll('.tab')[3].trigger('click')
    await flushPromises()
    expect(router.currentRoute.value.fullPath).toBe('/configuration/clickhouse')
    expect(w.find('.ch-stub').exists()).toBe(true)
  })
})
