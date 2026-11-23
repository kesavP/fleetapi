import { describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import IncidentSettingsTab from './IncidentSettingsTab.vue'
import { KEY, createIncidentSettingsStore, defaultSettings } from '../incidents/settings'

const memoryStorage = () => { const m = new Map(); return { getItem: (k) => (m.has(k) ? m.get(k) : null), setItem: (k, v) => m.set(k, String(v)), removeItem: (k) => m.delete(k), m } }
const sessionOf = (project = 'gke') => ({ get: () => (project ? { project, key: 'k' } : null), subscribe: () => () => {} })
const APPS = [{ id: '/k8s/shop/frontend', label: 'shop/frontend', hasMetrics: true }, { id: '/k8s/shop/cartservice', label: 'shop/cartservice', hasMetrics: true }, { id: '/k8s/x/silent', label: 'x/silent', hasMetrics: false }]

async function mountTab({ project = 'gke', storage = memoryStorage(), apps = async () => ({ apps: APPS }) } = {}) {
  const session = sessionOf(project)
  const store = createIncidentSettingsStore({ session, storage })
  const appCache = { get: vi.fn(apps) }
  const w = mount(IncidentSettingsTab, { props: { settings: store, session, client: {}, appCache }, global: { stubs: { RouterLink: { template: '<a><slot/></a>' } } } })
  await flushPromises()
  return { w, store, storage, appCache }
}
const input = (w, id) => w.get('#' + id)
const save = (w) => w.get('button[type=submit]')

describe('IncidentSettingsTab', () => {
  it('shows the current rules, and says they are kept in this browser, for this project', async () => {
    const { w } = await mountTab()
    expect(w.get('[data-testid=where]').text()).toContain('kept in this browser')
    expect(w.get('[data-testid=where]').text()).toContain('gke')
    expect(input(w, 'slo-availability').element.value).toBe('99.9')
    expect(input(w, 'slo-latencyMs').element.value).toBe('500')
    expect(input(w, 'slo-burnThreshold').element.value).toBe('14.4')
    expect(input(w, 'analysis-rttRatio').element.value).toBe('3')
    expect(w.findAll('.check input').every((c) => c.element.checked)).toBe(true)
  })

  it('has nothing to save until something is changed, then saves it in this browser for this project only', async () => {
    const { w, store, storage } = await mountTab()
    expect(save(w).attributes('disabled')).toBeDefined()
    await input(w, 'slo-availability').setValue('99.5')
    expect(save(w).attributes('disabled')).toBeUndefined()
    await w.get('form').trigger('submit')
    await flushPromises()
    expect(store.get().slo.availability).toBe(99.5)
    expect(JSON.parse(storage.m.get(KEY + 'gke')).slo.availability).toBe(99.5)
    expect(w.get('[role=status]').text()).toContain('Saved in this browser')
    expect(save(w).attributes('disabled')).toBeDefined() // nothing new to save
  })

  it('refuses a value that is not a number or is out of range, says which and why, and keeps nothing', async () => {
    const { w, store, storage } = await mountTab()
    await input(w, 'slo-latencyMs').setValue('5')
    expect(w.get('#slo-latencyMs-e').text()).toContain('between 10 and 60000')
    expect(input(w, 'slo-latencyMs').attributes('aria-invalid')).toBe('true')
    expect(save(w).attributes('disabled')).toBeDefined()
    await w.get('form').trigger('submit')
    expect(store.get().slo.latencyMs).toBe(500)
    expect(storage.m.size).toBe(0)
    expect(w.get('[role=status]').text()).toContain('not valid')
    await input(w, 'slo-latencyMs').setValue('')
    expect(w.get('#slo-latencyMs-e').text()).toContain('enter a number')
    await input(w, 'slo-latencyMs').setValue('250')
    expect(w.get('#slo-latencyMs-e').text()).toBe('')
    expect(save(w).attributes('disabled')).toBeUndefined()
  })

  it('refuses a long window shorter than the short one', async () => {
    const { w } = await mountTab()
    await input(w, 'slo-shortMinutes').setValue('30')
    await input(w, 'slo-longMinutes').setValue('10')
    expect(w.get('#slo-longMinutes-e').text()).toContain('must not be shorter')
    expect(save(w).attributes('disabled')).toBeDefined()
  })

  it('turns checks on and off', async () => {
    const { w, store } = await mountTab()
    const boxes = w.findAll('.check')
    const logs = boxes.find((b) => b.text().includes('Error log lines'))
    await logs.get('input').setValue(false)
    await w.get('form').trigger('submit')
    await flushPromises()
    expect(store.get().checks.logs).toBe(false)
    expect(store.get().checks.rtt).toBe(true)
  })

  it('lets an application have its own objective: chosen from the project\'s applications, with the rest left to the general one', async () => {
    const { w, store, appCache } = await mountTab()
    expect(appCache.get).toHaveBeenCalledWith({}, { project: 'gke', seconds: 3600 })
    const options = w.findAll('#pick-app option').map((o) => o.text())
    expect(options).toContain('shop/frontend')
    expect(options).not.toContain('x/silent') // one that reports nothing has no objective to hold it to
    await w.get('#pick-app').setValue('/k8s/shop/frontend')
    await w.get('.add-app button').trigger('click')
    expect(w.findAll('#pick-app option').map((o) => o.text())).not.toContain('shop/frontend') // already has one
    await w.get('input[aria-label="shop/frontend availability objective"]').setValue('99.99')
    await w.get('form').trigger('submit')
    await flushPromises()
    expect(store.get().apps).toEqual({ '/k8s/shop/frontend': { availability: 99.99 } }) // the empty latency box is not an override
  })

  it('refuses an application objective that is out of range, naming the application', async () => {
    const { w } = await mountTab()
    await w.get('#pick-app').setValue('/k8s/shop/cartservice')
    await w.get('.add-app button').trigger('click')
    await w.get('input[aria-label="shop/cartservice latency objective"]').setValue('20')
    expect(w.get('table.overrides').text()).toContain('between 50 and 99.999')
    expect(save(w).attributes('disabled')).toBeDefined()
  })

  it('removes an application\'s own objective', async () => {
    const storage = memoryStorage()
    storage.m.set(KEY + 'gke', JSON.stringify({ apps: { '/k8s/shop/frontend': { availability: 99.99 } } }))
    const { w, store } = await mountTab({ storage })
    expect(w.get('table.overrides').text()).toContain('shop/frontend')
    await w.get('button[aria-label="Remove shop/frontend"]').trigger('click')
    expect(w.find('table.overrides').exists()).toBe(false)
    await w.get('form').trigger('submit')
    await flushPromises()
    expect(store.get().apps).toEqual({})
  })

  it('still shows and edits the overrides already set when the application list cannot be read', async () => {
    const storage = memoryStorage()
    storage.m.set(KEY + 'gke', JSON.stringify({ apps: { '/k8s/shop/frontend': { availability: 99.99 } } }))
    const { w } = await mountTab({ storage, apps: async () => { throw new Error('metrics unavailable') } })
    expect(w.get('table.overrides').text()).toContain('/k8s/shop/frontend') // by id, since there is no label to be had
    expect(w.get('#pick-app').attributes('disabled')).toBeDefined()
  })

  it('resets to the defaults and forgets what was kept, and has nothing to reset when it is already the defaults', async () => {
    const storage = memoryStorage()
    storage.m.set(KEY + 'gke', JSON.stringify({ slo: { burnThreshold: 6 } }))
    const { w, store } = await mountTab({ storage })
    expect(input(w, 'slo-burnThreshold').element.value).toBe('6')
    const reset = w.findAll('button').find((b) => b.text().startsWith('Reset'))
    await reset.trigger('click')
    await flushPromises()
    expect(input(w, 'slo-burnThreshold').element.value).toBe('14.4')
    expect(store.get()).toEqual(defaultSettings())
    expect(storage.m.size).toBe(0)
    expect(reset.attributes('disabled')).toBeDefined()
  })

  it('does not keep rules for another project: each project has its own', async () => {
    const storage = memoryStorage()
    storage.m.set(KEY + 'dc1', JSON.stringify({ slo: { burnThreshold: 6 } }))
    const { w } = await mountTab({ project: 'gke', storage })
    expect(input(w, 'slo-burnThreshold').element.value).toBe('14.4')
  })

  it('asks nothing and points to the connection page when there is no connection', async () => {
    const { w, appCache } = await mountTab({ project: '' })
    expect(w.text()).toContain('Not connected')
    expect(appCache.get).not.toHaveBeenCalled()
    expect(w.find('form').exists()).toBe(false)
  })
})
