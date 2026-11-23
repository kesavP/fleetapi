import { describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import RecordedIncidentsView from './RecordedIncidentsView.vue'

const stub = { template: '<i/>' }
const session = (v = { project: 'us', key: 'k' }) => ({ get: () => v })
const INC = {
  id: 'vcxxe33p', title: 'monitoring/telemetry-ui: availability objective burning', app: '/k8s/monitoring/telemetry-ui', severity: 'critical', state: 'open', source: 'detected',
  summary: '', assignee: '', resolution: '', startedAt: '2026-11-16T05:23:00Z', createdAt: '2026-11-16T06:00:50Z', updatedAt: '2026-11-16T06:00:51Z', reopened: 0, version: 3, evidence: [], allowed: [],
}
const result = (o = {}) => ({ incidents: [INC], skipped: 0, ...o })

const STATS = {
  from: '', to: '', opened: 5, resolved: 3, notCounted: 1, stillOpen: 2, reopened: 1, partial: false,
  mttd: { count: 4, mean: 40, median: 30, p90: 60, max: 60 }, mtta: { count: 3, mean: 300, median: 240, p90: 600, max: 600 },
  mttm: { count: 0, mean: null, median: null, p90: null, max: null }, mttr: { count: 3, mean: 2000, median: 1800, p90: 3600, max: 3600 },
  bySeverity: { warning: { count: 1, median: 3600 }, critical: { count: 2, median: 600 } },
}

async function mountAt(query = '', { load = vi.fn(async () => result()), stats = vi.fn(async () => STATS), sess = session() } = {}) {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: stub }, { path: '/configuration/connection', component: stub }, { path: '/incidents', component: stub },
      { path: '/recorded', name: 'recorded-incidents', component: RecordedIncidentsView },
      { path: '/recorded/:id', name: 'recorded-incident', component: stub },
    ],
  })
  router.push('/recorded' + query)
  await router.isReady()
  const w = mount(RecordedIncidentsView, { props: { client: {}, session: sess, loaders: { loadRecorded: load, loadStats: stats } }, global: { plugins: [router] } })
  await flushPromises()
  return { w, router, load, stats }
}

describe('RecordedIncidentsView', () => {
  it('lists the recorded incidents, each linking to its page', async () => {
    const { w } = await mountAt()
    const row = w.get('table.incidents tbody tr')
    const cells = row.findAll('td').map((c) => c.text())
    expect(cells[0]).toBe('vcxxe33p')
    expect(cells[1]).toBe('monitoring/telemetry-ui: availability objective burning')
    expect(cells[2]).toBe('critical')
    expect(cells[3]).toBe('open')
    expect(row.get('a.name').attributes('href')).toBe('/recorded/vcxxe33p')
    expect(row.get('.pill.critical').exists()).toBe(true)
  })
  it('asks for the ones not yet resolved by default, with no severity', async () => {
    const { load } = await mountAt()
    expect(load.mock.calls[0][1]).toEqual({ state: 'open,acknowledged,investigating,mitigated', severity: '' })
  })
  it('asks again with the state and the severity that are chosen, and keeps them in the address', async () => {
    const { w, load, router } = await mountAt()
    await w.get('#state').setValue('resolved')
    await w.get('#severity').setValue('warning')
    await w.get('form.filters').trigger('submit')
    await flushPromises()
    expect(load.mock.calls.at(-1)[1]).toEqual({ state: 'resolved', severity: 'warning' })
    expect(router.currentRoute.value.query).toEqual({ state: 'resolved', severity: 'warning' })
  })
  it('asks for every state when told to, and keeps the default out of the address', async () => {
    const { w, load, router } = await mountAt()
    await w.get('#state').setValue('all')
    await w.get('form.filters').trigger('submit')
    await flushPromises()
    expect(load.mock.calls.at(-1)[1]).toEqual({ state: '', severity: '' })
    expect(router.currentRoute.value.query).toEqual({ state: 'all' })
    await w.get('#state').setValue('active')
    await w.get('form.filters').trigger('submit')
    await flushPromises()
    expect(router.currentRoute.value.query).toEqual({})
  })
  it('starts from the filters in the address, and ignores ones it does not offer', async () => {
    const a = await mountAt('?state=closed&severity=info')
    expect(a.load.mock.calls[0][1]).toEqual({ state: 'closed', severity: 'info' })
    const b = await mountAt('?state=bogus&severity=bogus')
    expect(b.load.mock.calls[0][1]).toEqual({ state: 'open,acknowledged,investigating,mitigated', severity: '' })
  })
  it('says so when none match', async () => {
    const { w } = await mountAt('', { load: async () => result({ incidents: [] }) })
    expect(w.get('[data-testid=none]').text()).toContain('No recorded incidents')
    expect(w.find('table.incidents').exists()).toBe(false)
  })
  it('says how many entries it could not read', async () => {
    const one = await mountAt('', { load: async () => result({ skipped: 1 }) })
    expect(one.w.text()).toContain('1 entry the service sent could not be read and is left out')
    const two = await mountAt('', { load: async () => result({ skipped: 2 }) })
    expect(two.w.text()).toContain('2 entries the service sent could not be read and are left out')
    const none = await mountAt()
    expect(none.w.find('p.warn').exists()).toBe(false)
  })
  it('shows a failure, with the way to the connection, and no table', async () => {
    const { w } = await mountAt('', { load: async () => { throw new Error('The telemetry service is not reachable right now.') } })
    expect(w.get('[role=alert]').text()).toContain('not reachable')
    expect(w.get('[role=alert] a').attributes('href')).toBe('/configuration/connection')
    expect(w.find('table').exists()).toBe(false)
  })
  it('asks for a connection first, and reads nothing without one', async () => {
    const { w, load } = await mountAt('', { sess: session(null) })
    expect(w.text()).toContain('Not connected')
    expect(load).not.toHaveBeenCalled()
  })
  it('puts a title that is markup on the page as text, never as markup', async () => {
    const { w } = await mountAt('', { load: async () => result({ incidents: [{ ...INC, title: '<img src=x onerror=alert(1)>' }] }) })
    expect(w.find('table img').exists()).toBe(false)
    expect(w.get('table.incidents tbody tr').findAll('td')[1].text()).toBe('<img src=x onerror=alert(1)>')
  })
  it('offers the two lists as tabs, with this one marked', async () => {
    const { w } = await mountAt()
    const tabs = w.findAll('nav.tabs a')
    expect(tabs.map((t) => t.text())).toEqual(['From the metrics', 'Recorded by the hub'])
    expect(tabs[1].attributes('aria-current')).toBe('page')
    expect(tabs[0].attributes('aria-current')).toBeUndefined()
  })
  describe('how long incidents take', () => {
    it('shows MTTR, MTTA, MTTD and the counts over 7 days by default', async () => {
      const { w, stats } = await mountAt()
      expect(stats.mock.calls[0][1]).toBe(7)
      expect(w.get('[data-testid=tile-mttr]').text()).toContain('30 min')
      expect(w.get('[data-testid=tile-mttr]').text()).toContain('p90 1 h · 3 resolved')
      expect(w.get('[data-testid=tile-mtta]').text()).toContain('4 min')
      expect(w.get('[data-testid=tile-mttd]').text()).toContain('30 s')
      expect(w.get('[data-testid=tile-counts]').text()).toContain('1 reopened')
      expect(w.get('[data-testid=by-severity]').text()).toBe('MTTR by severity: critical 10 min (2) · warning 1 h (1)')
      expect(w.find('[data-testid=partial]').exists()).toBe(false)
    })
    it('reads another window when it is picked', async () => {
      const { w, stats } = await mountAt()
      await w.get('#days').setValue('30')
      await flushPromises()
      expect(stats.mock.calls.at(-1)[1]).toBe(30)
    })
    it('says when the figures miss older incidents', async () => {
      const { w } = await mountAt('', { stats: vi.fn(async () => ({ ...STATS, partial: true })) })
      expect(w.get('[data-testid=partial]').text()).toContain('newest 500')
    })
    it('keeps the list when the figures fail', async () => {
      const { w } = await mountAt('', { stats: vi.fn(async () => { throw new Error('nope') }) })
      expect(w.get('[data-testid=stats-error]').text()).toContain('nope')
      expect(w.find('table.incidents').exists()).toBe(true)
    })
  })
})
