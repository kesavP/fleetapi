import { describe, expect, it, vi } from 'vitest'
import { checkProject } from './run'
import { DEFAULTS } from './rules'

// The whole check, over a client that answers like the read API. A Service (cartservice) that worked until five minutes ago and has
// refused every connection since: the window holds only failures, and a failed connect carries only the Service's cluster IP.
const END = 1_700_000_040
const NOW = () => END * 1000
const FE = '/k8s/shop/frontend-5d6f8c7b9-xk2lp/server'
const CART = '/k8s/shop/cartservice-7c9d8f6b5d-bk7q2/server'
const SVC = '10.0.0.9:7070'
const ok = { container_id: FE, destination: SVC, actual_destination: '10.4.1.9:7070' }
const failing = { container_id: FE, destination: SVC }

const counter = (labels, from, to, perSec) => {
  const step = Math.max(15, Math.floor((to - from) / 40))
  const n = Math.max(Math.floor(to / step) - Math.floor(from / step), 1)
  return { labels, points: Array.from({ length: n }, (_, i) => ({ t: (from + i * step) * 1000, v: perSec })) }
}
const gauge = (labels, to) => ({ labels, points: [{ t: (to - 1) * 1000, v: 1 }] })

function clientOf() {
  return {
    get: vi.fn(async (path, params) => {
      if (path === 'metrics') return ['container_net_tcp_listen_info', 'container_net_tcp_successful_connects_total', 'container_net_tcp_failed_connects_total']
      const from = Number(params.from)
      const to = Number(params.to)
      const recentOnly = to === END && to - from <= DEFAULTS.recentSeconds // the window being judged, not a longer look that includes before
      let series = []
      if (params.name === 'container_net_tcp_listen_info') {
        series = [gauge({ container_id: CART, listen_addr: '10.4.1.9:7070' }, to), gauge({ container_id: FE, listen_addr: '10.4.0.5:8080' }, to)]
      } else if (params.name === 'container_net_tcp_successful_connects_total') {
        series = recentOnly ? [] : [counter(ok, from, to, 1)]
      } else if (params.name === 'container_net_tcp_failed_connects_total') {
        series = recentOnly ? [counter(failing, from, to, 3)] : []
      }
      return { series }
    }),
  }
}

describe('checkProject: a Service that refuses every connection for the whole window', () => {
  it('raises the alert on the service behind the address, not on an external endpoint nobody watches', async () => {
    const r = await checkProject(clientOf(), DEFAULTS, { now: NOW })
    const refused = r.alerts.find((a) => a.rule === 'refused')
    expect(refused).toBeDefined()
    expect(refused).toMatchObject({ service: '/k8s/shop/cartservice', severity: 'critical', label: 'shop/cartservice' })
    expect(refused.evidence).toMatchObject({ failedSharePct: 100, failedConnectsPerSec: 3 })
    expect(refused.evidence.callers[0].label).toBe('shop/frontend') // and who was trying
    expect(r.alerts.some((a) => a.service.startsWith('external:'))).toBe(false)
  })

  it('also sees that it stopped getting traffic, since its callers can no longer reach it', async () => {
    const r = await checkProject(clientOf(), DEFAULTS, { now: NOW })
    expect(r.alerts.map((a) => a.rule).sort()).toEqual(['no-traffic', 'refused'])
  })
})
