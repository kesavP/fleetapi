import { describe, expect, it } from 'vitest'
import { connections } from './connections'

const POD = '/k8s/shop/cart-7c9d8f6b5d-bk7q2/server'
const OTHER = '/k8s/shop/frontend-5d6f8c7b9-xk2lp/server'
const ID = '/k8s/shop/cart'
const s = (container, rate, extra = {}) => ({ labels: { container_id: container, destination: '10.0.0.9:80', ...extra }, rate })
const g = (container, value) => ({ labels: { container_id: container, destination: '10.0.0.9:80' }, value })
const row = (card, label) => card.rows.find((r) => r.label === label)
const build = (series) => connections.build({ connects: [], failed: [], time: [], active: [], retransmits: [], ...series }, { id: ID, seconds: 600 })

describe('connections card', () => {
  it('has a name, an id and reads the five metrics of the node-agent', () => {
    expect(connections.id).toBe('connections')
    expect(Object.values(connections.metrics).map((m) => m.metric).sort()).toEqual([
      'container_net_tcp_active_connections',
      'container_net_tcp_connection_time_seconds_total',
      'container_net_tcp_failed_connects_total',
      'container_net_tcp_retransmits_total',
      'container_net_tcp_successful_connects_total',
    ])
  })

  it('works out the mean setup time from time spent over connects made', () => {
    const c = build({ connects: [s(POD, 10), s(POD, 30)], time: [s(POD, 0.04), s(POD, 0.12)] })
    expect(row(c, 'Setup time (mean)').value).toBe('4 ms') // 0.16 s over 40 connects a second
    expect(row(c, 'Connects')).toMatchObject({ value: '40/s' })
  })

  it('shows sub-millisecond and slow setup times in a readable unit', () => {
    expect(row(build({ connects: [s(POD, 100)], time: [s(POD, 0.02)] }), 'Setup time (mean)').value).toBe('0.2 ms')
    expect(row(build({ connects: [s(POD, 1)], time: [s(POD, 1.5)] }), 'Setup time (mean)').value).toBe('1.5 s')
  })

  it('has no setup time without connects, or without time', () => {
    expect(row(build({ connects: [], failed: [s(POD, 1)] }), 'Setup time (mean)')).toBeUndefined()
    expect(row(build({ connects: [s(POD, 5)] }), 'Setup time (mean)')).toBeUndefined()
  })

  it('counts a failed connect against all attempts, and marks a share at 1% and 5%', () => {
    const quiet = build({ connects: [s(POD, 99.5)], failed: [s(POD, 0.5)] })
    expect(row(quiet, 'Failed')).toMatchObject({ value: '0.5/s (0.5%)', level: undefined })
    const caution = build({ connects: [s(POD, 98)], failed: [s(POD, 2)] })
    expect(row(caution, 'Failed').level).toBe('caution')
    const bad = build({ connects: [s(POD, 90)], failed: [s(POD, 10)] })
    expect(row(bad, 'Failed').level).toBe('bad')
    expect(row(bad, 'Failed').value).toBe('10/s (10%)')
  })

  it('reports an application that only fails to connect', () => {
    const c = build({ failed: [s(POD, 3)] })
    expect(row(c, 'Failed')).toMatchObject({ value: '3/s (100%)', level: 'bad' })
    expect(row(c, 'Setup time (mean)')).toBeUndefined()
  })

  it('sums the open connections of the application, and the retransmitted segments', () => {
    const c = build({ connects: [s(POD, 1)], active: [g(POD, 4), g(POD, 6)], retransmits: [s(POD, 0.25)] })
    expect(row(c, 'Open connections').value).toBe('10')
    expect(row(c, 'Retransmits').value).toBe('0.25/s')
  })

  it('counts only this application, never its neighbour', () => {
    const c = build({ connects: [s(POD, 5), s(OTHER, 500)], failed: [s(OTHER, 50)] })
    expect(row(c, 'Connects').value).toBe('5/s')
    expect(row(c, 'Failed')).toBeUndefined()
  })

  it('is null when the application has no series at all', () => {
    expect(build({ connects: [s(OTHER, 5)] })).toBeNull()
    expect(build({})).toBeNull()
  })

  it('shows zero retransmits when the counter exists and nothing was retransmitted', () => {
    const c = build({ connects: [s(POD, 1)], retransmits: [s(POD, 0)] })
    expect(row(c, 'Retransmits').value).toBe('0/s')
  })
})
