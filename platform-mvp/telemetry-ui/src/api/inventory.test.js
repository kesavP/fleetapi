import { describe, expect, it, vi } from 'vitest'
import { ApiError } from './client.js'
import { STALE_SECONDS, WORKLOAD_PATH_LIMIT, appIdOf, loadInventory, podReason, podReasons, workloadsOf } from './inventory.js'

const NOW = Date.UTC(2026, 10, 10, 12, 0, 0)
const iso = (agoSeconds) => new Date(NOW - agoSeconds * 1000).toISOString()
const row = (kind, ns, name, data, over = {}) => ({ cluster: 'gke', kind, uid: `${kind}-${ns}-${name}`, namespace: ns, name, labels: {}, created: iso(9999), observed: iso(30), data, ...over })
const dep = (ns, name, desired, ready, over) => row('deployment', ns, name, { desired, ready, available: ready }, over)
const pod = (ns, workload, data, over) => row('pod', ns, `${workload}-abc-xyz`, { phase: 'Running', ready: false, workload_kind: 'deployment', workload_name: workload, containers: [], ...data }, over)

describe('appIdOf', () => {
  it('is the name the node-agent gives a workload', () => {
    expect(appIdOf('deployment', 'shop', 'cart')).toBe('/k8s/shop/cart')
    expect(appIdOf('statefulset', 'data', 'db')).toBe('/k8s/data/db')
    expect(appIdOf('daemonset', 'kube-system', 'agent')).toBe('/k8s/kube-system/agent')
    expect(appIdOf('cronjob', 'shop', 'cleanup')).toBe('/k8s-cronjob/shop/cleanup')
  })
})

describe('workloadsOf', () => {
  it('reads what each workload is set to run and how many are ready', () => {
    const m = workloadsOf([dep('shop', 'cart', 0, 0), dep('shop', 'web', 3, 2), row('cronjob', 'shop', 'cleanup', { schedule: '* * * * *' })])
    expect(m.get('/k8s/shop/cart')).toMatchObject({ kind: 'deployment', desired: 0, ready: 0 })
    expect(m.get('/k8s/shop/web')).toMatchObject({ desired: 3, ready: 2, available: 2 })
    expect(m.get('/k8s-cronjob/shop/cleanup')).toMatchObject({ kind: 'cronjob', desired: 0, ready: 0 })
    expect(m.get('/k8s/shop/web').observed).toBe(NOW - 30000)
  })
  it('adds the numbers of the same workload in two clusters: the page has one row for it', () => {
    const m = workloadsOf([dep('shop', 'web', 2, 2, { cluster: 'a' }), dep('shop', 'web', 3, 1, { cluster: 'b' })])
    expect(m.get('/k8s/shop/web')).toMatchObject({ desired: 5, ready: 3, clusters: 2 })
  })
  it('skips rows that are not usable and takes null', () => {
    expect(workloadsOf(null).size).toBe(0)
    expect(workloadsOf([null, { kind: 'deployment' }, { kind: 'deployment', namespace: 'a', name: 'b' }, dep('shop', 'ok', 1, 1)]).size).toBe(1)
    expect(workloadsOf([row('deployment', 'shop', 'x', { desired: 'many', ready: null })]).get('/k8s/shop/x')).toMatchObject({ desired: 0, ready: 0 })
  })
})

describe('podReason', () => {
  const p = (data) => ({ data })
  it('prefers what the pod itself says, then what its containers wait for or died of', () => {
    expect(podReason(p({ reason: 'Unschedulable', containers: [{ state: 'waiting', reason: 'x' }] }))).toBe('Unschedulable')
    expect(podReason(p({ containers: [{ state: 'waiting', reason: 'ImagePullBackOff' }] }))).toBe('ImagePullBackOff')
    expect(podReason(p({ containers: [{ state: 'waiting', reason: 'CrashLoopBackOff', last_reason: 'OOMKilled' }] }))).toBe('CrashLoopBackOff (OOMKilled)')
    expect(podReason(p({ containers: [{ state: 'waiting', reason: 'CrashLoopBackOff' }] }))).toBe('CrashLoopBackOff')
    expect(podReason(p({ containers: [{ state: 'terminated', reason: 'Error' }] }))).toBe('Error')
    expect(podReason(p({ phase: 'Pending', containers: [] }))).toBe('Pending')
    expect(podReason(p({ phase: 'Running', containers: [{ state: 'running' }] }))).toBe('not ready')
    expect(podReason(null)).toBe('not ready')
  })
})

describe('podReasons', () => {
  it('counts why the pods of each workload are not ready, the commonest first, and leaves out the ready ones', () => {
    const crash = { containers: [{ state: 'waiting', reason: 'CrashLoopBackOff' }] }
    const m = podReasons([
      pod('shop', 'cart', crash), pod('shop', 'cart', crash), pod('shop', 'cart', { reason: 'Unschedulable', phase: 'Pending' }),
      pod('shop', 'cart', { ready: true }), pod('shop', 'cart', { phase: 'Succeeded' }),
      pod('shop', 'web', { reason: 'Evicted' }),
    ])
    expect(m.get('/k8s/shop/cart')).toEqual(['CrashLoopBackOff ×2', 'Unschedulable'])
    expect(m.get('/k8s/shop/web')).toEqual(['Evicted'])
  })
  it('names at most three, and ignores bare pods and pods without a workload', () => {
    const rows = ['a', 'b', 'c', 'd'].map((r) => pod('shop', 'x', { reason: r }))
    expect(podReasons(rows).get('/k8s/shop/x')).toEqual(['a', 'b', 'c'])
    expect(podReasons([pod('shop', 'solo', { workload_kind: 'pod' }), pod('shop', 'x', { workload_kind: '', workload_name: '' }), null]).size).toBe(0)
  })
  it('puts a cron job\'s pods under the cron job', () => {
    expect([...podReasons([pod('shop', 'cleanup', { workload_kind: 'cronjob', reason: 'Error' })]).keys()]).toEqual(['/k8s-cronjob/shop/cleanup'])
  })
})

const clientOf = ({ summary, workloads = [], pods = [], fail } = {}) => ({
  get: vi.fn(async (path) => {
    if (fail) throw fail
    if (path === 'inventory') return summary
    if (path === 'inventory/workloads') return workloads
    if (path === 'inventory/pods') return pods
    throw new Error('unexpected ' + path)
  }),
})
const reported = (agoSeconds, cluster = 'gke') => ({ cluster, last_received: iso(agoSeconds), last_collected: iso(agoSeconds), counts: {} })

describe('loadInventory', () => {
  it('is null when there is no collector, or the hub does not have the endpoint', async () => {
    expect(await loadInventory(clientOf({ summary: [] }), { now: () => NOW })).toBeNull()
    expect(await loadInventory(clientOf({ summary: null }), { now: () => NOW })).toBeNull()
    expect(await loadInventory(clientOf({ fail: new ApiError('not found', { status: 404 }) }), { now: () => NOW })).toBeNull()
  })
  it('lets any other failure through, to be shown as a warning', async () => {
    await expect(loadInventory(clientOf({ fail: new ApiError('boom', { status: 500 }) }), { now: () => NOW })).rejects.toThrow('boom')
  })
  it('reads the workloads, and the pods only when something is not ready', async () => {
    const ok = clientOf({ summary: [reported(20)], workloads: [dep('shop', 'web', 2, 2), dep('shop', 'cart', 0, 0)] })
    const r = await loadInventory(ok, { now: () => NOW })
    expect(r.workloads.size).toBe(2)
    expect(r.stale).toBeNull()
    expect(ok.get.mock.calls.map((c) => c[0])).toEqual(['inventory', 'inventory/workloads'])

    const bad = clientOf({ summary: [reported(20)], workloads: [dep('shop', 'web', 2, 0)], pods: [pod('shop', 'web', { reason: 'Unschedulable' })] })
    const r2 = await loadInventory(bad, { now: () => NOW })
    expect(bad.get.mock.calls.map((c) => c[0])).toEqual(['inventory', 'inventory/workloads', 'inventory/pods'])
    expect(r2.reasons.get('/k8s/shop/web')).toEqual(['Unschedulable'])
    // scaled to zero is not "not ready": nothing to explain
    const zero = clientOf({ summary: [reported(20)], workloads: [dep('shop', 'web', 0, 0)] })
    await loadInventory(zero, { now: () => NOW })
    expect(zero.get.mock.calls.map((c) => c[0])).not.toContain('inventory/pods')
  })
  it('does not trust a collector that has not reported lately, and says for how long', async () => {
    const r = await loadInventory(clientOf({ summary: [reported(STALE_SECONDS + 300)], workloads: [dep('shop', 'web', 2, 2)] }), { now: () => NOW })
    expect(r.workloads).toBeNull()
    expect(r.stale.seconds).toBe(STALE_SECONDS + 300)
  })
  it('uses the clusters that reported lately and leaves out the rows of one that did not', async () => {
    const r = await loadInventory(clientOf({
      summary: [reported(20, 'a'), reported(5000, 'dead')],
      workloads: [dep('shop', 'web', 2, 2, { cluster: 'a' }), dep('shop', 'old', 1, 1, { cluster: 'dead' })],
    }), { now: () => NOW })
    expect([...r.workloads.keys()]).toEqual(['/k8s/shop/web'])
  })
  it('says when it was cut short', async () => {
    const many = Array.from({ length: WORKLOAD_PATH_LIMIT }, (_, i) => dep('shop', `w${i}`, 1, 1))
    expect((await loadInventory(clientOf({ summary: [reported(10)], workloads: many }), { now: () => NOW })).truncated).toBe(true)
  })
})
