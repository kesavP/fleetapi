import { describe, expect, it } from 'vitest'
import { inboundSnapshot, inboundTimeline } from './inbound'

const FE = '/k8s/shop/frontend-5d6f8c7b9-xk2lp/server'
const FE2 = '/k8s/shop/frontend-5d6f8c7b9-bk7q2/server'
const CART = '/k8s/shop/cartservice-7c9d8f6b5d-zq9wn/server'
const req = (container_id, status, rate) => ({ labels: { container_id, status }, rate })
const bucket = (container_id, le, rate) => ({ labels: { container_id, le }, rate })

describe('inboundSnapshot', () => {
  it('adds the pods of an application, and works out its error share and p95', () => {
    const m = inboundSnapshot({
      reqs: [req(FE, '200', 8), req(FE2, '200', 8), req(FE, '502', 2), req(FE2, '500', 2), req(CART, '200', 5)],
      buckets: [bucket(FE, '0.1', 10), bucket(FE2, '0.1', 10), bucket(FE, '0.5', 20), bucket(FE2, '0.5', 20), bucket(FE, '+Inf', 20), bucket(FE2, '+Inf', 20)],
    })
    const fe = m.get('/k8s/shop/frontend')
    expect(fe.rps).toBe(20)
    expect(fe.errPct).toBe(20) // 4 of 20 are 5xx
    expect(fe.p95ms).toBeCloseTo(460, 0) // half the requests are under 100 ms, the rest under 500: p95 sits near the top of the second bucket
    expect(m.get('/k8s/shop/cartservice')).toMatchObject({ rps: 5, errPct: 0, p95ms: null })
  })
  it('leaves out an application with no requests, and what is not a pod', () => {
    const m = inboundSnapshot({ reqs: [req(FE, '200', 0), req('', '200', 5), req('garbage', '200', 5)] })
    expect(m.size).toBe(0) // an idle application, an empty container id and one the agent does not name are all left out
    const host = inboundSnapshot({ reqs: [req('/system.slice/nginx.service', '200', 4)] })
    expect(host.get('/system.slice/nginx.service')).toMatchObject({ rps: 4, label: 'nginx' }) // a unit on a plain host is an application too
  })
  it('counts 4xx as success, as the service map does', () => {
    expect(inboundSnapshot({ reqs: [req(FE, '404', 10)] }).get('/k8s/shop/frontend').errPct).toBe(0)
  })
  it('ignores a bucket with a bound that is not a number', () => {
    const m = inboundSnapshot({ reqs: [req(FE, '200', 10)], buckets: [bucket(FE, 'x', 10), bucket(FE, '0.5', 10), bucket(FE, '+Inf', 10)] })
    expect(m.get('/k8s/shop/frontend').p95ms).toBeCloseTo(475, 0)
  })
})

describe('inboundTimeline', () => {
  const grid = { start: 1000, step: 60, n: 5 }
  const pts = (vals) => vals.map((v, i) => ({ t: (1000 + i * 60) * 1000, v })).filter((p) => p.v !== null)
  it('lays each series on the step grid, adding the pods, and counts a step with no point as no requests', () => {
    const tl = inboundTimeline({
      reqs: [
        { labels: { container_id: FE, status: '200' }, points: pts([10, 10, null, 10, 10]) },
        { labels: { container_id: FE2, status: '200' }, points: pts([5, 5, null, 5, 5]) },
        { labels: { container_id: FE, status: '500' }, points: pts([null, 2, null, null, null]) },
      ],
    }, grid)
    const a = tl.apps.get('/k8s/shop/frontend')
    expect(a.total).toEqual([15, 17, 0, 15, 15])
    expect(a.errors).toEqual([0, 2, 0, 0, 0])
    expect(a.slow).toBeNull() // no histogram at all
  })
  it('reads slow requests as the total less those within the smallest bound at or above the threshold', () => {
    const tl = inboundTimeline({
      reqs: [{ labels: { container_id: FE, status: '200' }, points: pts([10, 10, 10, 10, 10]) }],
      buckets: [
        { labels: { container_id: FE, le: '0.1' }, points: pts([10, 10, 10, 10, 10]) },
        { labels: { container_id: FE, le: '0.5' }, points: pts([10, 10, 4, 4, 10]) },
        { labels: { container_id: FE, le: '1' }, points: pts([10, 10, 8, 10, 10]) },
        { labels: { container_id: FE, le: '+Inf' }, points: pts([10, 10, 10, 10, 10]) },
      ],
    }, { ...grid, thresholdSeconds: 0.5 })
    expect(tl.bound).toBe(0.5)
    expect(tl.apps.get('/k8s/shop/frontend').slow).toEqual([0, 0, 6, 6, 0])
  })
  it('uses the next bound up when the threshold is not one, and says which', () => {
    const tl = inboundTimeline({
      reqs: [{ labels: { container_id: FE, status: '200' }, points: pts([10, 10, 10, 10, 10]) }],
      buckets: [{ labels: { container_id: FE, le: '1' }, points: pts([5, 5, 5, 5, 5]) }, { labels: { container_id: FE, le: '0.25' }, points: pts([1, 1, 1, 1, 1]) }],
    }, { ...grid, thresholdSeconds: 0.5 })
    expect(tl.bound).toBe(1)
    expect(tl.apps.get('/k8s/shop/frontend').slow).toEqual([5, 5, 5, 5, 5])
  })
  it('has no slow series when every bound is below the threshold, rather than calling everything slow', () => {
    const tl = inboundTimeline({
      reqs: [{ labels: { container_id: FE, status: '200' }, points: pts([10, 10, 10, 10, 10]) }],
      buckets: [{ labels: { container_id: FE, le: '0.1' }, points: pts([10, 10, 10, 10, 10]) }],
    }, grid)
    expect(tl.bound).toBeNull()
    expect(tl.apps.get('/k8s/shop/frontend').slow).toBeNull()
  })
  it('drops a point outside the range and never goes negative when a bucket reads higher than the total', () => {
    const tl = inboundTimeline({
      reqs: [{ labels: { container_id: FE, status: '200' }, points: [...pts([10, 10, 10, 10, 10]), { t: 5000 * 1000, v: 99 }, { t: 0, v: 99 }] }],
      buckets: [{ labels: { container_id: FE, le: '0.5' }, points: pts([12, 12, 12, 12, 12]) }],
    }, grid)
    const a = tl.apps.get('/k8s/shop/frontend')
    expect(a.total).toEqual([10, 10, 10, 10, 10])
    expect(a.slow).toEqual([0, 0, 0, 0, 0])
  })
})
