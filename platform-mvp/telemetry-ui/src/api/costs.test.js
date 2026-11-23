import { describe, expect, it, vi } from 'vitest'
import { COST_WINDOWS, barPercent, groupByCategory, lifecycleText, loadCosts, memoryText, money, parseReport, shareText, sourceText, vcpuText } from './costs.js'

const GIB = 1024 ** 3

const node = (over = {}) => ({
  name: 'aks-n1', machine_id: 'm1', provider: 'Azure', region: 'SouthIndia', instance_type: 'Standard_D4s_v6', lifecycle: 'on-demand', lifecycle_assumed: true,
  cpu: { capacity: 4, usage: 0.8, requests: 2 }, memory: { capacity: 16 * GIB, usage: 4 * GIB, requests: 8 * GIB }, cpu_share: 0.65,
  price_monthly: 146, price_source: 'override', used_monthly: 36.5, overprovisioned_monthly: 36.5, idle_monthly: 73, ...over,
})
const app = (over = {}) => ({
  app: '/k8s/shop/web', label: 'shop/web', namespace: 'shop', cpu: { usage: 0.5, requests: 1.5 }, memory: { usage: 2 * GIB, requests: 6 * GIB },
  usage_monthly: 20, allocation_monthly: 54, overprovisioning_monthly: 34, ...over,
})
const finding = (over = {}) => ({ id: 'idle-capacity', kind: 'saving', title: 'Nobody requested 50%', detail: 'd', monthly_saving: 73, items: [{ name: 'aks-n1', monthly: 73 }], ...over })
const report = (over = {}) => ({
  currency: 'USD', window_seconds: 3600, generated_at: '2026-11-18T10:00:00Z', has_requests: true, nodes: [node()], applications: [app()],
  totals: { price_monthly: 146, used_monthly: 36.5, overprovisioned_monthly: 36.5, idle_monthly: 73, unattributed_monthly: 5, nodes_without_price: 0, app_usage_monthly: 20, app_allocation_monthly: 54, app_overprovisioning_monthly: 34 },
  analysis: [finding()], warnings: ['a note'], ...over,
})

describe('loadCosts', () => {
  it('asks for the window and returns the report as the page uses it', async () => {
    const client = { get: vi.fn(async () => report()) }
    const r = await loadCosts(client, 86400)
    expect(client.get).toHaveBeenCalledWith('costs', { window: 86400 })
    expect(r.windowSeconds).toBe(3600)
    expect(r.nodes[0].name).toBe('aks-n1')
  })
  it('lets a failure through as the client words it, and refuses an answer that is not a report', async () => {
    await expect(loadCosts({ get: async () => { throw new Error('The costs are not switched on for this hub') } }, 3600)).rejects.toThrow('not switched on')
    await expect(loadCosts({ get: async () => null }, 3600)).rejects.toThrow('not a cost report')
    await expect(loadCosts({ get: async () => [] }, 3600)).rejects.toThrow('not a cost report')
  })
})

describe('parseReport', () => {
  it('keeps what the page shows, with the names the page uses', () => {
    const r = parseReport(report())
    expect(r).toMatchObject({ currency: 'USD', windowSeconds: 3600, hasRequests: true, warnings: ['a note'] })
    const n = r.nodes[0]
    expect(n).toMatchObject({ name: 'aks-n1', instanceType: 'Standard_D4s_v6', lifecycle: 'on-demand', lifecycleAssumed: true, priceMonthly: 146, priceSource: 'override', usedMonthly: 36.5, overprovisionedMonthly: 36.5, idleMonthly: 73 })
    expect(n.cpu).toEqual({ capacity: 4, usage: 0.8, requests: 2 })
    expect(n.memory.requests).toBe(8 * GIB)
    expect(r.applications[0]).toMatchObject({ app: '/k8s/shop/web', label: 'shop/web', namespace: 'shop', usageMonthly: 20, allocationMonthly: 54, overprovisioningMonthly: 34 })
    expect(r.totals).toMatchObject({ priceMonthly: 146, idleMonthly: 73, appAllocationMonthly: 54, nodesWithoutPrice: 0, unattributedMonthly: 5 })
    expect(r.analysis[0]).toEqual({ id: 'idle-capacity', kind: 'saving', title: 'Nobody requested 50%', detail: 'd', monthlySaving: 73, items: [{ name: 'aks-n1', monthly: 73 }] })
  })
  it('keeps "not known" as null and never as zero: a price that is missing, and requests that are', () => {
    const r = parseReport(report({
      has_requests: false,
      nodes: [node({ price_monthly: null, cpu: { capacity: 4, usage: 1 }, memory: { capacity: GIB, usage: 1 } })],
      applications: [app({ allocation_monthly: null, overprovisioning_monthly: null, cpu: { usage: 1 }, memory: { usage: 1 } })],
      totals: { price_monthly: 0, app_allocation_monthly: null, app_overprovisioning_monthly: null },
    }))
    expect(r.nodes[0].priceMonthly).toBeNull()
    expect(r.nodes[0].cpu.requests).toBeNull()
    expect(r.applications[0].allocationMonthly).toBeNull()
    expect(r.applications[0].overprovisioningMonthly).toBeNull()
    expect(r.applications[0].cpu.requests).toBeNull()
    expect(r.totals.appAllocationMonthly).toBeNull()
  })
  it('makes a number of what is not one, and an empty list of what is not a list, so a bad answer cannot break the page', () => {
    const r = parseReport({ nodes: 'x', applications: null, analysis: 5, warnings: {}, totals: { price_monthly: 'NaN' }, window_seconds: 'a' })
    expect(r.nodes).toEqual([])
    expect(r.applications).toEqual([])
    expect(r.analysis).toEqual([])
    expect(r.warnings).toEqual([])
    expect(r.totals.priceMonthly).toBe(0)
    expect(r.windowSeconds).toBe(0)
    expect(r.currency).toBe('USD')
    const n = parseReport(report({ nodes: [node({ idle_monthly: 'lots', cpu: { capacity: Infinity, usage: NaN } })] })).nodes[0]
    expect(n.idleMonthly).toBe(0)
    expect(n.cpu.capacity).toBe(0)
    expect(n.cpu.usage).toBe(0)
  })
  it('leaves out a row that has no name, and a finding that says nothing, and treats a kind it does not know as information', () => {
    const r = parseReport(report({ nodes: [node(), null, 'x', {}, node({ name: '' })], applications: [app(), { app: '' }, 5], analysis: [finding({ kind: 'alarm' }), finding({ title: '' }), null, finding({ items: 'x' })] }))
    expect(r.nodes).toHaveLength(1)
    expect(r.applications).toHaveLength(1)
    expect(r.analysis).toHaveLength(2)
    expect(r.analysis[0].kind).toBe('info')
    expect(r.analysis[1].items).toEqual([])
  })
  it('keeps only words in the text that comes back, never anything that is not text', () => {
    const r = parseReport(report({ warnings: ['ok', 5, null, { a: 1 }, ''], analysis: [finding({ title: 7 })] }))
    expect(r.warnings).toEqual(['ok'])
    expect(r.analysis).toEqual([])
  })
})

describe('money', () => {
  it('writes dollars: cents under a hundred, whole dollars above, a thousands separator, and a dash for what is not known', () => {
    expect(money(110.16)).toBe('$110')
    expect(money(34.48)).toBe('$34.48')
    expect(money(0)).toBe('$0.00')
    expect(money(1234.56)).toBe('$1,235')
    expect(money(99.99)).toBe('$99.99')
    expect(money(100)).toBe('$100')
    for (const v of [null, undefined, NaN, Infinity, 'x']) expect(money(v)).toBe('—')
  })
  it('says a small negative number as zero, never as minus zero', () => {
    expect(money(-0.001)).toBe('$0.00')
    expect(money(-0)).toBe('$0.00')
  })
})

describe('text', () => {
  it('writes cores, memory, shares and bar widths the way the page shows them', () => {
    expect(vcpuText(4)).toBe('4 vCPU')
    expect(vcpuText(0.5)).toBe('0.5 vCPU')
    expect(memoryText(16 * GIB)).toBe('16 GiB')
    expect(memoryText(1.5 * GIB)).toBe('1.5 GiB')
    expect(memoryText(512 * 1024 ** 2)).toBe('512 MiB')
    expect(memoryText(0)).toBe('0 MiB')
    expect(shareText(25, 100)).toBe('25%')
    expect(shareText(1, 3)).toBe('33%')
    expect(shareText(5, 0)).toBe('—')
    expect(barPercent(1, 4)).toBe(25)
    expect(barPercent(9, 4)).toBe(100)
    expect(barPercent(-1, 4)).toBe(0)
    expect(barPercent(1, 0)).toBe(0)
    expect(barPercent(NaN, 4)).toBe(0)
  })
  it('says where a price came from and what a node is', () => {
    expect(sourceText('override')).toBe('your price')
    expect(sourceText('azure-retail')).toBe('list price')
    expect(sourceText('')).toBe('')
    expect(sourceText('mystery')).toBe('')
    expect(lifecycleText({ lifecycle: 'on-demand', lifecycleAssumed: true })).toBe('on-demand (assumed)')
    expect(lifecycleText({ lifecycle: 'on-demand', lifecycleAssumed: false })).toBe('on-demand')
    expect(lifecycleText({ lifecycle: 'spot', lifecycleAssumed: false })).toBe('spot')
    expect(lifecycleText({ lifecycle: '', lifecycleAssumed: false })).toBe('')
  })
  it('offers the windows the page lets a person pick', () => {
    expect(COST_WINDOWS.map((w) => w.seconds)).toEqual([3600, 21600, 86400, 604800])
    expect(COST_WINDOWS[0].label).toBe('hour')
  })
})

describe('groupByCategory', () => {
  const apps = [
    parseReport(report()).applications[0],
    ...parseReport(report({ applications: [
      app({ app: '/k8s/kube-system/coredns', label: 'kube-system/coredns', namespace: 'kube-system', usage_monthly: 5, allocation_monthly: 8, overprovisioning_monthly: 3 }),
      app({ app: '/k8s/shop/api', label: 'shop/api', usage_monthly: 12, allocation_monthly: 14, overprovisioning_monthly: 2 }),
    ] })).applications,
  ]
  const categoryFor = (a) => (a.namespace === 'kube-system' ? 'control-plane' : 'application')

  it('adds the applications of each category, the one that costs most first, with the applications in it by what they cost', () => {
    const g = groupByCategory(apps, categoryFor)
    expect(g.map((c) => c.category)).toEqual(['application', 'control-plane'])
    expect(g[0]).toMatchObject({ category: 'application', usage: 20 + 12 })
    expect(g[0].allocation).toBeCloseTo(54 + 14)
    expect(g[0].overprovisioning).toBeCloseTo(34 + 2)
    expect(g[0].apps.map((a) => a.label)).toEqual(['shop/web', 'shop/api'])
    expect(g[1]).toMatchObject({ category: 'control-plane', usage: 5, allocation: 8, overprovisioning: 3 })
  })
  it('has no allocation for a category none of whose applications has one', () => {
    const g = groupByCategory([{ ...apps[0], allocationMonthly: null, overprovisioningMonthly: null }], categoryFor)
    expect(g[0].allocation).toBeNull()
    expect(g[0].overprovisioning).toBeNull()
  })
  it('is empty for no applications', () => {
    expect(groupByCategory([], categoryFor)).toEqual([])
    expect(groupByCategory(null, categoryFor)).toEqual([])
  })
})
