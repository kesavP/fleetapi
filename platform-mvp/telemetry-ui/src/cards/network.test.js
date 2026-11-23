import { describe, expect, it } from 'vitest'
import { network } from './network'

const POD = '/k8s/shop/cart-7c9d8f6b5d-bk7q2/server'
const OTHER = '/k8s/shop/frontend-5d6f8c7b9-xk2lp/server'
const ID = '/k8s/shop/cart'
const r = (container, rate, extra = {}) => ({ labels: { container_id: container, destination: '10.0.0.9:80', ...extra }, rate })
const v = (container, value, ip) => ({ labels: { container_id: container, destination_ip: ip }, value })
const row = (card, label) => card.rows.find((x) => x.label === label)
const build = (series) => network.build({ sent: [], received: [], rtt: [], ...series }, { id: ID, seconds: 600 })

describe('network card', () => {
  it('reads the bytes and round-trip metrics of the node-agent, the round trip being optional', () => {
    expect(network.id).toBe('network')
    const m = Object.fromEntries(Object.entries(network.metrics).map(([k, x]) => [x.metric, { k, kind: x.kind, optional: !!x.optional }]))
    expect(m['container_net_tcp_bytes_sent_total'].kind).toBe('rate')
    expect(m['container_net_tcp_bytes_received_total'].kind).toBe('rate')
    expect(m['container_net_latency_seconds']).toMatchObject({ kind: 'mean', optional: true })
  })

  it('adds the bytes of every peer into a rate a second', () => {
    const c = build({ sent: [r(POD, 1024), r(POD, 2048)], received: [r(POD, 1024 * 1024)] })
    expect(row(c, 'Sent').value).toBe('3 KiB/s')
    expect(row(c, 'Received').value).toBe('1 MiB/s')
  })

  it('shows the busiest peer by the bytes it moved, both ways', () => {
    const c = build({
      sent: [r(POD, 100, { destination: 'a:1' }), r(POD, 900, { destination: 'b:2' })],
      received: [r(POD, 4900, { destination: 'a:1' })],
    })
    expect(row(c, 'Busiest peer')).toMatchObject({ value: 'a:1 (4.9 KiB/s)' })
  })

  it('does not name a peer when there is only one or none', () => {
    expect(row(build({ sent: [r(POD, 1)] }), 'Busiest peer')).toBeUndefined()
  })

  it('shows the mean round trip and the slowest peer, in milliseconds', () => {
    const c = build({ sent: [r(POD, 1)], rtt: [v(POD, 0.002, '10.0.0.1'), v(POD, 0.006, '10.0.0.2')] })
    expect(row(c, 'Round trip (mean)').value).toBe('4 ms')
    expect(row(c, 'Slowest peer')).toMatchObject({ value: '10.0.0.2 (6 ms)' })
  })

  it('marks a slow round trip: caution from 100 ms, bad from 500 ms', () => {
    expect(row(build({ rtt: [v(POD, 0.05, 'a')] }), 'Round trip (mean)').level).toBeUndefined()
    expect(row(build({ rtt: [v(POD, 0.2, 'a')] }), 'Round trip (mean)').level).toBe('caution')
    expect(row(build({ rtt: [v(POD, 0.8, 'a')] }), 'Round trip (mean)').level).toBe('bad')
  })

  it('leaves out the round trip when the metric has no reading for this application', () => {
    const c = build({ sent: [r(POD, 1)], rtt: [v(OTHER, 0.9, 'x')] })
    expect(row(c, 'Round trip (mean)')).toBeUndefined()
    expect(row(c, 'Slowest peer')).toBeUndefined()
  })

  it('counts only this application', () => {
    const c = build({ sent: [r(POD, 10), r(OTHER, 10000)] })
    expect(row(c, 'Sent').value).toBe('10 B/s')
  })

  it('is null when the application has no series', () => {
    expect(build({ sent: [r(OTHER, 5)] })).toBeNull()
    expect(build({})).toBeNull()
  })
})
