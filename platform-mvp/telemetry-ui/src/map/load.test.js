import { describe, expect, it, vi } from 'vitest'
import { gaugeSeries, gauges, listenAddresses, loadMap, rates } from './load'

const START = 1_700_000_040 // a multiple of 15 s, so a 600 s range is exactly 40 buckets of 15 s
const END = START + 600
const pts = (...v) => v.map((x, i) => ({ t: (START + i * 15) * 1000, v: x }))

describe('rates', () => {
  it('asks for a per-bucket rate with from_zero, in about 40 buckets', async () => {
    const get = vi.fn(async () => ({ series: [] }))
    await rates({ get }, 'm', START, END)
    expect(get).toHaveBeenCalledWith('metrics/query_range', { name: 'm', from: `${START}.000`, to: `${END}.000`, step: 15, rate: true, from_zero: true })
  })

  it('divides by every bucket of the range, so quiet buckets count as zero', async () => {
    const client = { get: async () => ({ series: [{ labels: { a: '1' }, points: pts(...Array(40).fill(1)) }, { labels: { a: '2' }, points: pts(10, 10) }] }) }
    const r = await rates(client, 'm', START, END)
    expect(r.series[0].rate).toBeCloseTo(1, 9)
    expect(r.series[1].rate).toBeCloseTo(0.5, 9) // 20 over 40 buckets, not 10 over the 2 it was seen in
    expect(r.truncated).toBe(false)
  })

  it('a restarted pod (two short series) adds up to the traffic, not to double it', async () => {
    const client = { get: async () => ({ series: [{ labels: { pod: 'old' }, points: pts(...Array(20).fill(5)) }, { labels: { pod: 'new' }, points: pts(...Array(20).fill(5)) }] }) }
    const total = (await rates(client, 'm', START, END)).series.reduce((a, s) => a + s.rate, 0)
    expect(total).toBeCloseTo(5, 9)
  })

  it('scales the step with the range and keeps it at 15 s at least', async () => {
    const get = vi.fn(async () => ({ series: [] }))
    await rates({ get }, 'm', START, START + 24 * 3600)
    expect(get.mock.calls[0][1].step).toBe(Math.floor((24 * 3600) / 40))
    await rates({ get }, 'm', START, START + 60)
    expect(get.mock.calls[1][1].step).toBe(15)
  })

  it('skips a series with no points, reports truncation, and tolerates null', async () => {
    const client = { get: async () => ({ truncated: true, series: [{ labels: {}, points: [] }, { labels: { a: '1' } }] }) }
    expect(await rates(client, 'm', START, END)).toEqual({ series: [], truncated: true })
    expect(await rates({ get: async () => null }, 'm', START, END)).toEqual({ series: [], truncated: false })
  })
})

describe('listenAddresses', () => {
  it('keeps when each address was last reported', async () => {
    const client = { get: async () => ({ series: [{ labels: { listen_addr: 'x' }, points: [{ t: 5, v: 1 }, { t: 9, v: 1 }] }, { labels: {}, points: [] }] }) }
    expect((await listenAddresses(client, START, END)).series).toEqual([{ labels: { listen_addr: 'x' }, last: 9 }])
  })
})

describe('loadMap', () => {
  const FE = '/k8s/shop/frontend-5d6f8c7b9-xk2lp/server'
  const CART = '/k8s/shop/cartservice-7c9d8f6b5d-bk7q2/server'
  const call = { container_id: FE, destination: '10.0.0.9:7070', actual_destination: '10.4.1.9:7070' }
  const flat = (n) => pts(...Array(40).fill(n))
  const DATA = {
    container_net_tcp_listen_info: [{ labels: { container_id: CART, listen_addr: '10.4.1.9:7070' }, points: pts(1) }],
    container_net_tcp_successful_connects_total: [{ labels: call, points: flat(2) }],
    container_http_requests_total: [{ labels: { ...call, status: '2xx' }, points: flat(3) }, { labels: { ...call, status: '500' }, points: flat(1) }],
  }
  const clientOf = ({ names = Object.keys(DATA), fail = {}, data = DATA } = {}) => ({
    get: vi.fn(async (path, params) => {
      if (path === 'metrics') {
        if (fail.metrics) throw new Error(fail.metrics)
        return names
      }
      if (fail[params.name]) throw new Error(fail[params.name])
      return { series: data[params.name] || [] }
    }),
  })
  const NOW = () => END * 1000

  it('builds the map from the counters the project has', async () => {
    const r = await loadMap(clientOf(), { seconds: 600, now: NOW })
    expect(r.warnings).toEqual([])
    expect(r.edges).toHaveLength(1)
    expect(r.edges[0]).toMatchObject({ from: '/k8s/shop/frontend', to: '/k8s/shop/cartservice', rps: 4, connPerSec: 2 })
    expect(r.edges[0].errPct).toBeCloseTo(25, 6)
    expect(r.from).toBe(START * 1000)
    expect(r.to).toBe(END * 1000)
  })

  it('queries only the counters in the metric list', async () => {
    const client = clientOf()
    await loadMap(client, { seconds: 600, now: NOW })
    const asked = client.get.mock.calls.filter(([p]) => p === 'metrics/query_range').map(([, q]) => q.name).sort()
    expect(asked).toEqual(Object.keys(DATA).sort())
  })

  it('adds a protocol only when the project reports it, and its histogram only if present', async () => {
    const q = { container_id: CART, destination: '10.4.9.9:5432' }
    const data = { ...DATA, container_postgres_queries_total: [{ labels: { ...q, status: 'ok' }, points: flat(2) }], container_postgres_queries_duration_seconds_total_bucket: [{ labels: { ...q, le: '+Inf' }, points: flat(2) }] }
    const names = Object.keys(data)
    const r = await loadMap(clientOf({ names, data }), { seconds: 600, now: NOW })
    const pg = r.edges.find((e) => e.protocol === 'postgres')
    expect(pg).toMatchObject({ rps: 2, from: '/k8s/shop/cartservice', protocols: ['postgres'] })
    const none = clientOf({ names: Object.keys(DATA) })
    await loadMap(none, { seconds: 600, now: NOW })
    expect(none.get.mock.calls.some(([, q2]) => q2 && /postgres/.test(q2.name))).toBe(false)
  })

  it('keeps the map and warns when one counter cannot be read', async () => {
    const r = await loadMap(clientOf({ fail: { container_http_requests_total: 'down' } }), { seconds: 600, now: NOW })
    expect(r.edges).toHaveLength(1)
    expect(r.edges[0].rps).toBeNull()
    expect(r.warnings).toEqual(['container_http_requests_total: down'])
  })

  it('warns when the series cap was hit', async () => {
    const client = { get: async (p) => (p === 'metrics' ? ['container_http_requests_total'] : { truncated: true, series: [] }) }
    const r = await loadMap(client, { seconds: 600, now: NOW })
    expect(r.warnings.some((w) => w.includes('more series than one call allows'))).toBe(true)
  })

  it('warns that nothing can be named without the listen-address metric', async () => {
    const names = Object.keys(DATA).filter((n) => n !== 'container_net_tcp_listen_info')
    const r = await loadMap(clientOf({ names }), { seconds: 600, now: NOW })
    expect(r.warnings[0]).toContain('no listen-address metric')
    expect(r.edges[0].to).toBe('external:10.0.0.9:7070')
  })

  it('fails when the metric list fails or every counter fails, with the reason', async () => {
    await expect(loadMap(clientOf({ fail: { metrics: 'This key cannot read project "x".' } }), { seconds: 600, now: NOW })).rejects.toThrow('cannot read project')
    const all = Object.fromEntries(Object.keys(DATA).map((n) => [n, 'denied']))
    await expect(loadMap(clientOf({ fail: all }), { seconds: 600, now: NOW })).rejects.toThrow('denied')
  })

  it('names the failure, not an unrelated warning, when every counter fails', async () => {
    // No listen-address metric, so loadMap starts with a warning of its own; the error must still be the real reason.
    const names = ['container_http_requests_total']
    await expect(loadMap(clientOf({ names, fail: { container_http_requests_total: 'This key cannot read project "x".' } }), { seconds: 600, now: NOW })).rejects.toThrow('cannot read project')
  })

  it('is empty, without error, for a project with no counters', async () => {
    const r = await loadMap(clientOf({ names: ['up'] }), { seconds: 600, now: NOW })
    expect(r.edges).toEqual([])
    expect(r.hasData).toBe(false)
  })
})

describe('gaugeSeries / gauges', () => {
  const NOW_END = START + 600
  const at = (t) => [{ t: t * 1000, v: 7 }]
  const client = {
    get: async () => ({
      series: [
        { labels: { container_id: 'live' }, points: at(NOW_END - 5) },
        { labels: { container_id: 'gone' }, points: at(START + 30) }, // stopped reporting long before the end
        { labels: { container_id: 'empty' }, points: [] },
      ],
    }),
  }
  it('keeps every series, saying which are still reporting', async () => {
    const r = await gaugeSeries(client, 'm', START, NOW_END)
    expect(r.series.map((s) => [s.labels.container_id, s.fresh, s.value])).toEqual([['live', true, 7], ['gone', false, 7]])
  })
  it('gauges leaves out the ones that stopped, as before', async () => {
    const r = await gauges(client, 'm', START, NOW_END)
    expect(r.series.map((s) => s.labels.container_id)).toEqual(['live'])
    expect(r.series[0]).not.toHaveProperty('fresh')
    expect(r.series[0].last).toBe((NOW_END - 5) * 1000)
  })
})
