import { describe, expect, it, vi } from 'vitest'
import { gatherContext } from './context'

const NOW = () => 1_700_000_000_000
const alert = (service) => ({ id: 'x', rule: 'errors', service })

function clientOf({ fail = {} } = {}) {
  return {
    get: vi.fn(async (path, params) => {
      if (path === 'metrics') return fail.metrics ? Promise.reject(new Error(fail.metrics)) : []
      if (fail[path]) throw new Error(fail[path])
      if (path === 'traces') return [{ span_id: 's', service: params.service }]
      if (path === 'logs') return [{ body: 'x' }]
      if (path === 'profiles') return []
      return { series: [] }
    }),
  }
}

describe('gatherContext', () => {
  it('reads the service\'s spans, logs and metrics over the same moment', async () => {
    const client = clientOf()
    const c = await gatherContext(client, alert('/k8s/shop/cart'), { project: 'gke', seconds: 1800, now: NOW })
    expect(c).toMatchObject({ project: 'gke', now: 1_700_000_000_000, errors: {} })
    expect(c.spans).toEqual([{ span_id: 's', service: '/k8s/shop/cart' }])
    expect(c.logs).toEqual([{ body: 'x' }])
    expect(c.traffic).not.toBeNull()
    const range = client.get.mock.calls.find(([p]) => p === 'traces')[1]
    expect(range.to - range.from).toBe(1800)
  })

  it('reads nothing for an alert on the whole project', async () => {
    const client = clientOf()
    const c = await gatherContext(client, alert('*'), { project: 'gke', now: NOW })
    expect(client.get).not.toHaveBeenCalled()
    expect(c.spans).toEqual([])
  })

  it('keeps the rest and names what failed', async () => {
    const c = await gatherContext(clientOf({ fail: { traces: 'denied', metrics: 'metrics down' } }), alert('/k8s/shop/cart'), { project: 'gke', now: NOW })
    expect(c.errors).toMatchObject({ spans: 'denied', metrics: 'metrics down' })
    expect(c.logs).toEqual([{ body: 'x' }])
    expect(c.traffic).toBeNull()
  })
})
