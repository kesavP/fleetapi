import { describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { reactive } from 'vue'
import CostsView from './CostsView.vue'
import { parseReport } from '../api/costs'
import { createCategoryStore } from '../categories'

const GIB = 1024 ** 3
const session = (v = { project: 'us', key: 'k' }) => ({ get: () => v })

const rawNode = (over = {}) => ({
  name: 'aks-n1', machine_id: 'm1', provider: 'Azure', region: 'SouthIndia', instance_type: 'Standard_D4s_v6', lifecycle: 'on-demand', lifecycle_assumed: true,
  cpu: { capacity: 4, usage: 0.8, requests: 2 }, memory: { capacity: 16 * GIB, usage: 4 * GIB, requests: 8 * GIB }, cpu_share: 0.65,
  price_monthly: 146, price_source: 'azure-retail', used_monthly: 36.5, overprovisioned_monthly: 36.5, idle_monthly: 73, ...over,
})
const rawApp = (over = {}) => ({
  app: '/k8s/shop/web', label: 'shop/web', namespace: 'shop', cpu: { usage: 0.5, requests: 1.5 }, memory: { usage: 2 * GIB, requests: 6 * GIB },
  usage_monthly: 20, allocation_monthly: 54, overprovisioning_monthly: 34, ...over,
})
const rawReport = (over = {}) => ({
  currency: 'USD', window_seconds: 3600, generated_at: '2026-11-18T10:00:00Z', has_requests: true,
  nodes: [rawNode(), rawNode({ name: 'aks-n2', machine_id: 'm2', idle_monthly: 40, used_monthly: 50, overprovisioned_monthly: 56, cpu: { capacity: 4, usage: 1, requests: 3 } })],
  applications: [rawApp(), rawApp({ app: '/k8s/kube-system/coredns', label: 'kube-system/coredns', namespace: 'kube-system', usage_monthly: 5, allocation_monthly: 8, overprovisioning_monthly: 3 })],
  totals: { price_monthly: 292, used_monthly: 86.5, overprovisioned_monthly: 92.5, idle_monthly: 113, unattributed_monthly: 61.5, nodes_without_price: 0, app_usage_monthly: 25, app_allocation_monthly: 62, app_overprovisioning_monthly: 37 },
  analysis: [{ id: 'idle-capacity', kind: 'saving', title: 'Nobody requested 39% of what the nodes cost: $113 a month', detail: 'The most is on aks-n1.', monthly_saving: 113, items: [{ name: 'aks-n1', monthly: 73 }, { name: 'aks-n2', monthly: 40 }] },
    { id: 'lifecycle-assumed', kind: 'info', title: 'Nodes are priced as on-demand', detail: 'Spot nodes cost much less.', monthly_saving: null, items: [] }],
  warnings: [], ...over,
})

const features = (costs = true, loaded = true) => reactive({ state: { costs, loaded }, refresh: vi.fn() })

function make({ raw = rawReport, load, sess = session(), feat = features(), categories } = {}) {
  const loadCosts = load || vi.fn(async () => parseReport(typeof raw === 'function' ? raw() : raw))
  const w = mount(CostsView, {
    props: { client: {}, session: sess, features: feat, categories: categories || createCategoryStore({ session: sess, storage: null }), loaders: { loadCosts } },
  })
  return { w, loadCosts, feat }
}
async function ready(o) {
  const m = make(o)
  await flushPromises()
  return m
}

describe('CostsView', () => {
  it('reads the report of the last hour, and says what it is', async () => {
    const { w, loadCosts } = await ready()
    expect(loadCosts).toHaveBeenCalledTimes(1)
    expect(loadCosts.mock.calls[0][1]).toBe(3600)
    expect(w.get('h1').text()).toBe('Costs')
    expect(w.get('#window').element.value).toBe('3600')
  })

  it('shows the cluster in four figures: what the nodes cost, and how much of it was used, requested and not used, and requested by nobody', async () => {
    const { w } = await ready()
    expect(w.get('[data-testid=tile-price]').text()).toContain('$292')
    expect(w.get('[data-testid=tile-used]').text()).toContain('$86.50')
    expect(w.get('[data-testid=tile-used]').text()).toContain('30%')
    expect(w.get('[data-testid=tile-overprovisioned]').text()).toContain('$92.50')
    expect(w.get('[data-testid=tile-idle]').text()).toContain('$113')
    expect(w.get('[data-testid=tile-idle]').text()).toContain('39%')
    expect(w.get('[data-testid=tile-idle]').text()).toContain('requested by nobody')
    const split = w.get('[data-testid=split]')
    expect(split.findAll('.seg').map((s) => s.attributes('style'))).toEqual([expect.stringContaining('width: 29.6'), expect.stringContaining('width: 31.6'), expect.stringContaining('width: 38.6')])
  })

  it('lists the nodes with their size, what they cost, and what is idle, the most idle first, with a total', async () => {
    const { w } = await ready()
    const rows = w.findAll('[data-testid=nodes] tbody tr[data-node]')
    expect(rows).toHaveLength(2)
    const first = rows[0]
    expect(first.text()).toContain('aks-n1')
    expect(first.text()).toContain('Standard_D4s_v6 / 4 vCPU / 16 GiB')
    expect(first.get('[data-testid=price]').text()).toContain('$146')
    expect(first.get('[data-testid=price]').text()).toContain('/mo')
    expect(first.get('[data-testid=price]').text()).toContain('on-demand (assumed)')
    expect(first.get('[data-testid=price]').text()).toContain('list price')
    expect(first.get('[data-testid=idle]').text()).toContain('$73.00')
    const total = w.get('[data-testid=nodes-total]')
    expect(total.text()).toContain('$292')
    expect(total.text()).toContain('$113')
  })

  it('draws what a node used and what was requested against what it has, as bars of the right width', async () => {
    const { w } = await ready()
    const row = w.get('[data-node="aks-n1"]')
    const width = (sel) => row.get(sel).attributes('style')
    expect(width('[data-testid=cpu-usage] .fill')).toContain('width: 20%') // 0.8 of 4
    expect(width('[data-testid=cpu-requests] .fill')).toContain('width: 50%') // 2 of 4
    expect(width('[data-testid=mem-usage] .fill')).toContain('width: 25%')
    expect(width('[data-testid=mem-requests] .fill')).toContain('width: 50%')
    expect(row.get('[data-testid=cpu-usage]').text()).toContain('0.8')
  })

  it('shows a node whose price is not known as such, and leaves its money out', async () => {
    const { w } = await ready({ raw: () => rawReport({ nodes: [rawNode({ price_monthly: null, price_source: '', used_monthly: 0, overprovisioned_monthly: 0, idle_monthly: 0 })] }) })
    const row = w.get('[data-node="aks-n1"]')
    expect(row.get('[data-testid=price]').text()).toContain('no price')
    expect(row.get('[data-testid=idle]').text()).toBe('—')
    expect(row.get('[data-testid=price]').text()).not.toContain('$0')
  })

  it('reads the applications as categories with what each used, was allocated and over-provisioned, and opens a category to its applications', async () => {
    const { w } = await ready()
    const cats = w.findAll('[data-testid=categories] tbody tr[data-category]')
    expect(cats.map((c) => c.attributes('data-category'))).toEqual(['application', 'control-plane'])
    expect(cats[0].text()).toContain('$20.00')
    expect(cats[0].text()).toContain('$54.00')
    expect(cats[0].text()).toContain('$34.00')
    expect(w.findAll('[data-app]')).toHaveLength(0)
    await cats[0].get('[data-testid=cat-toggle]').trigger('click')
    expect(w.findAll('[data-app]').map((r) => r.attributes('data-app'))).toEqual(['/k8s/shop/web'])
    expect(cats[0].get('[data-testid=cat-toggle]').attributes('aria-expanded')).toBe('true')
    await cats[0].get('[data-testid=cat-toggle]').trigger('click')
    expect(w.findAll('[data-app]')).toHaveLength(0)
    const total = w.get('[data-testid=categories-total]')
    expect(total.text()).toContain('$25.00')
    expect(total.text()).toContain('$62.00')
  })

  it('puts an application in the category the person moved it to', async () => {
    const sess = session()
    const store = createCategoryStore({ session: sess, storage: null })
    store.add('batch')
    store.move('/k8s/shop/web', 'batch')
    const { w } = await ready({ categories: store, sess })
    expect(w.findAll('[data-category]').map((c) => c.attributes('data-category'))).toEqual(['batch', 'control-plane'])
  })

  it('says what the nodes used that no Kubernetes workload did', async () => {
    const { w } = await ready()
    expect(w.get('[data-testid=unattributed]').text()).toContain('$61.50')
  })

  it('without requests shows usage and idle, and says allocation is unknown and why, and shows no request bars', async () => {
    const raw = () => rawReport({
      has_requests: false,
      nodes: [rawNode({ cpu: { capacity: 4, usage: 0.8 }, memory: { capacity: 16 * GIB, usage: 4 * GIB }, overprovisioned_monthly: 0, idle_monthly: 109.5 })],
      applications: [rawApp({ allocation_monthly: null, overprovisioning_monthly: null, cpu: { usage: 0.5 }, memory: { usage: 2 * GIB } })],
      totals: { price_monthly: 146, used_monthly: 36.5, overprovisioned_monthly: 0, idle_monthly: 109.5, app_usage_monthly: 20, app_allocation_monthly: null, app_overprovisioning_monthly: null },
      analysis: [{ id: 'requests-unknown', kind: 'info', title: 'What pods request is not known', detail: 'Deploy the cluster collector.', monthly_saving: null, items: [] }],
    })
    const { w } = await ready({ raw })
    expect(w.get('[data-node="aks-n1"]').find('[data-testid=cpu-requests] .fill').exists()).toBe(false)
    expect(w.get('[data-node="aks-n1"]').get('[data-testid=cpu-requests]').text()).toContain('unknown')
    expect(w.get('[data-testid=tile-overprovisioned]').text()).toContain('—')
    expect(w.get('[data-testid=tile-idle]').text()).toContain('not used')
    const cat = w.get('[data-category="application"]')
    expect(cat.findAll('td').slice(2).map((c) => c.text())).toEqual(['—', '—'])
    expect(w.get('[data-testid=requests-note]').text()).toContain('cluster collector')
  })

  it('shows the analysis: each finding with its kind, what it says, what it could save and where', async () => {
    const { w } = await ready()
    const items = w.findAll('[data-testid=analysis] article.finding')
    expect(items).toHaveLength(2)
    expect(items[0].classes()).toContain('saving')
    expect(items[0].get('h3').text()).toContain('Nobody requested 39%')
    expect(items[0].text()).toContain('The most is on aks-n1.')
    expect(items[0].get('[data-testid=saving]').text()).toContain('$113')
    expect(items[0].findAll('li').map((l) => l.text())).toEqual(['aks-n1 $73.00', 'aks-n2 $40.00'])
    expect(items[1].classes()).toContain('info')
    expect(items[1].find('[data-testid=saving]').exists()).toBe(false)
  })

  it('says so when the analysis has nothing to report', async () => {
    const { w } = await ready({ raw: () => rawReport({ analysis: [] }) })
    expect(w.get('[data-testid=analysis]').text()).toContain('Nothing to report')
  })

  it('puts what limits the figures in the bell and not on the page', async () => {
    const { w } = await ready({ raw: () => rawReport({ warnings: ['No price for Standard_X in SouthIndia'] }) })
    expect(w.text()).not.toContain('No price for Standard_X')
    expect(w.get('[data-testid=bell-count]').text()).toBe('1')
    await w.get('[data-testid=bell]').trigger('click')
    expect(w.get('[data-testid=bell-list]').text()).toContain('No price for Standard_X')
  })

  it('reads again for another window, and when Refresh is pressed', async () => {
    const { w, loadCosts } = await ready()
    await w.get('#window').setValue('86400')
    await flushPromises()
    expect(loadCosts).toHaveBeenCalledTimes(2)
    expect(loadCosts.mock.calls[1][1]).toBe(86400)
    await w.findAll('.head-right button').find((b) => b.text() === 'Refresh').trigger('click')
    await flushPromises()
    expect(loadCosts).toHaveBeenCalledTimes(3)
  })

  it('offers the windows a person can pick', async () => {
    const { w } = await ready()
    expect(w.findAll('#window option').map((o) => o.text())).toEqual(['Last hour', 'Last 6 hours', 'Last 24 hours', 'Last 7 days'])
  })

  it('says the costs are switched off, and how, and reads nothing, when the hub has not switched them on', async () => {
    const { w, loadCosts } = await ready({ feat: features(false, true) })
    expect(w.get('[data-testid=costs-off]').text()).toContain('switched off')
    expect(w.get('[data-testid=costs-off]').text()).toContain('telemetryIngest.costs.enabled')
    expect(loadCosts).not.toHaveBeenCalled()
    expect(w.find('[data-testid=nodes]').exists()).toBe(false)
  })

  it('waits for the hub to say whether the costs are on before it reads them, and then reads', async () => {
    const feat = features(true, false)
    const { w, loadCosts } = await ready({ feat })
    expect(loadCosts).not.toHaveBeenCalled()
    expect(w.find('[data-testid=costs-off]').exists()).toBe(false)
    feat.state.loaded = true
    await flushPromises()
    expect(loadCosts).toHaveBeenCalledTimes(1)
    expect(w.find('[data-testid=nodes]').exists()).toBe(true)
  })

  it('reads nothing without a connection', async () => {
    const { w, loadCosts } = await ready({ sess: session(null) })
    expect(w.text()).toContain('Not connected')
    expect(loadCosts).not.toHaveBeenCalled()
  })

  it('says why the report could not be read, and shows nothing else', async () => {
    const { w } = await ready({ load: async () => { throw new Error('The report could not be made.') } })
    expect(w.get('[data-testid=costs-error]').text()).toContain('could not be made')
    expect(w.find('[data-testid=nodes]').exists()).toBe(false)
  })

  it('says so when no node has sent anything in the window', async () => {
    const { w } = await ready({ raw: () => rawReport({ nodes: [], applications: [], analysis: [], totals: {} }) })
    expect(w.get('[data-testid=no-nodes]').text()).toContain('No node')
  })

  it('shows what the hub sent as text, never as markup', async () => {
    const { w } = await ready({ raw: () => rawReport({ nodes: [rawNode({ name: '<img src=x onerror=alert(1)>' })] }) })
    expect(w.find('img').exists()).toBe(false)
    expect(w.get('[data-testid=nodes]').text()).toContain('<img src=x onerror=alert(1)>')
  })

  it('says what the figures are: list prices, per month, at the rate of the window', async () => {
    const { w } = await ready()
    const notes = w.get('[data-testid=costs-notes]').text()
    for (const want of ['list prices', 'a month', 'idle']) expect(notes).toContain(want)
  })
})
