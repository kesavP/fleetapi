import { describe, expect, it } from 'vitest'
import { addressIndex, buildGraph } from './graph'

const FE = '/k8s/shop/frontend-5d6f8c7b9-xk2lp/server'
const CART = '/k8s/shop/cartservice-7c9d8f6b5d-bk7q2/server'
const PG = '/k8s/data/postgres-0/postgres'
const listen = (container_id, listen_addr, last = 1000) => ({ labels: { container_id, listen_addr }, last })
const s = (labels, rate) => ({ labels, rate })

// frontend (10.4.0.5) calls cartservice: the Service address 10.0.0.9 was translated to the pod 10.4.1.9.
const LISTEN = [listen(FE, '10.4.0.5:8080'), listen(CART, '10.4.1.9:7070'), listen(PG, '10.4.2.2:5432')]
const call = { container_id: FE, destination: '10.0.0.9:7070', actual_destination: '10.4.1.9:7070' }
const edge = (g, from, to) => g.edges.find((e) => e.from === from && e.to === to)

describe('addressIndex', () => {
  it('maps a pod address to its workload', () => {
    expect(addressIndex(LISTEN).get('10.4.1.9').id).toBe('/k8s/shop/cartservice')
  })
  it('ignores loopback and wildcard listeners', () => {
    expect(addressIndex([listen(CART, '127.0.0.1:9000'), listen(CART, '0.0.0.0:80'), listen(CART, '[::]:80')]).size).toBe(0)
  })
  it('gives an address to the workload that reported it last', () => {
    const ix = addressIndex([listen(CART, '10.4.1.9:7070', 1000), listen(FE, '10.4.1.9:8080', 2000)])
    expect(ix.get('10.4.1.9').id).toBe('/k8s/shop/frontend')
  })
  it('drops an address two workloads claim at the same time (host networking)', () => {
    expect(addressIndex([listen(CART, '10.0.0.1:1', 1000), listen(FE, '10.0.0.1:2', 1000)]).has('10.0.0.1')).toBe(false)
  })
})

describe('buildGraph', () => {
  it('names the callee from the address the call actually reached', () => {
    const g = buildGraph({ listen: LISTEN, conn: [s(call, 2)] })
    expect(g.edges).toHaveLength(1)
    expect(edge(g, '/k8s/shop/frontend', '/k8s/shop/cartservice')).toMatchObject({ connPerSec: 2, rps: null, errPct: 0, errBasis: 'tcp' })
    expect(g.nodes.map((n) => n.id)).toEqual(['/k8s/shop/cartservice', '/k8s/shop/frontend'])
    expect(g.nodes.every((n) => !n.external)).toBe(true)
  })

  it('falls back to the dialled address, and then to an external node', () => {
    const direct = buildGraph({ listen: LISTEN, conn: [s({ container_id: FE, destination: '10.4.1.9:7070' }, 1)] })
    expect(direct.edges[0].to).toBe('/k8s/shop/cartservice')
    const ext = buildGraph({ listen: LISTEN, conn: [s({ container_id: FE, destination: '34.1.2.3:443' }, 1)] })
    expect(ext.edges[0].to).toBe('external:34.1.2.3:443')
    expect(ext.nodes.find((n) => n.id === 'external:34.1.2.3:443').external).toBe(true)
  })

  it('counts a failed connect as a failed call in the error share', () => {
    const http = [s({ ...call, status: '200' }, 8), s({ ...call, status: '500' }, 1), s({ ...call, status: '2xx' }, 1)]
    const g = buildGraph({ listen: LISTEN, conn: [s(call, 3)], fail: [s(call, 1)], http })
    // 10 calls/s, 1 of them a 5xx, plus 1 failed connect: (1 + 1) / (10 + 1).
    expect(edge(g, '/k8s/shop/frontend', '/k8s/shop/cartservice')).toMatchObject({ rps: 10, errBasis: 'http+tcp' })
    expect(edge(g, '/k8s/shop/frontend', '/k8s/shop/cartservice').errPct).toBeCloseTo(100 * (2 / 11), 6)
  })

  it('reads a refused-connection edge from the TCP counters when no call got through', () => {
    const g = buildGraph({ listen: LISTEN, conn: [s(call, 1)], fail: [s(call, 3)] })
    expect(edge(g, '/k8s/shop/frontend', '/k8s/shop/cartservice')).toMatchObject({ rps: null, errPct: 75, errBasis: 'tcp' })
  })

  it('counts a gRPC server failure as an error and a caller mistake as none', () => {
    const http = [s({ ...call, status: 'grpc:UNAVAILABLE' }, 1), s({ ...call, status: 'grpc:NOT_FOUND' }, 1), s({ ...call, status: '2xx' }, 8)]
    expect(buildGraph({ listen: LISTEN, http }).edges[0].errPct).toBeCloseTo(10, 6)
  })

  it('drops loopback calls, host processes, and calls to itself', () => {
    const g = buildGraph({
      listen: LISTEN,
      conn: [
        s({ container_id: FE, destination: '127.0.0.1:9000' }, 1),
        s({ container_id: FE, destination: '10.0.0.9:80', actual_destination: '127.0.0.1:80' }, 1),
        s({ container_id: '/system.slice/sshd.service', destination: '10.4.1.9:7070' }, 1),
        s({ container_id: FE, destination: '10.4.0.5:8080' }, 1),
      ],
    })
    expect(g.edges).toEqual([])
    expect(g.nodes).toEqual([])
    expect(g.ignored.loopback).toBe(2)
  })

  it('sums the series of one edge, such as a restarted pod', () => {
    const g = buildGraph({ listen: LISTEN, conn: [s({ ...call, container_id: FE }, 1), s({ ...call, container_id: '/k8s/shop/frontend-5d6f8c7b9-zzzzz/server' }, 2)] })
    expect(g.edges).toHaveLength(1)
    expect(g.edges[0].connPerSec).toBe(3)
  })

  it('draws a database edge with its protocol, error share and p95, and marks the node as serving it', () => {
    const q = { container_id: CART, destination: '10.0.0.50:5432', actual_destination: '10.4.2.2:5432' }
    const protocols = [{
      name: 'postgres', unit: 'queries/s',
      calls: [s({ ...q, status: 'ok' }, 18), s({ ...q, status: 'failed' }, 2)],
      latency: [s({ ...q, le: '0.005' }, 10), s({ ...q, le: '0.1' }, 20), s({ ...q, le: '+Inf' }, 20)],
    }]
    const g = buildGraph({ listen: LISTEN, protocols })
    expect(edge(g, '/k8s/shop/cartservice', '/k8s/data/postgres')).toMatchObject({ rps: 20, errPct: 10, errBasis: 'calls', protocol: 'postgres', protocols: ['postgres'], unit: 'queries/s' })
    // p95 = rank 19 of 20: in (5 ms, 100 ms] -> 5 + 95 * (19-10)/(20-10) = 90.5 ms.
    expect(edge(g, '/k8s/shop/cartservice', '/k8s/data/postgres').p95ms).toBeCloseTo(90.5, 3)
    expect(g.nodes.find((n) => n.id === '/k8s/data/postgres').serves).toEqual(['postgres'])
    expect(g.nodes.find((n) => n.id === '/k8s/shop/cartservice').serves).toEqual([])
  })

  it('reports the latency of the protocol that carries most of a mixed edge, and calls/s as its unit', () => {
    const base = { container_id: FE, destination: '10.0.0.9:7070', actual_destination: '10.4.1.9:7070' }
    const g = buildGraph({
      listen: LISTEN,
      http: [s({ ...base, status: '2xx' }, 1)],
      protocols: [{ name: 'redis', unit: 'queries/s', calls: [s({ ...base, status: 'ok' }, 9)], latency: [s({ ...base, le: '0.01' }, 9), s({ ...base, le: '+Inf' }, 9)] }],
    })
    expect(g.edges[0]).toMatchObject({ rps: 10, protocol: 'redis', protocols: ['http', 'redis'], unit: 'calls/s' })
    expect(g.edges[0].p95ms).toBeCloseTo(9.5, 3)
  })

  it('does not count http as the basis when other protocols are present', () => {
    const base = { container_id: FE, destination: '10.0.0.9:7070', actual_destination: '10.4.1.9:7070' }
    const g = buildGraph({ listen: LISTEN, http: [s({ ...base, status: '2xx' }, 1)], protocols: [{ name: 'redis', unit: 'q', calls: [s({ ...base, status: 'ok' }, 1)], latency: null }] })
    expect(g.edges[0].errBasis).toBe('calls')
  })

  it('ignores a latency bucket with a bad bound', () => {
    const q = { container_id: CART, destination: '10.4.2.2:5432' }
    const g = buildGraph({ listen: LISTEN, protocols: [{ name: 'postgres', unit: 'q', calls: [s({ ...q, status: 'ok' }, 1)], latency: [s({ ...q, le: 'x' }, 1)] }] })
    expect(g.edges[0].p95ms).toBeNull()
  })

  it('is empty for no data', () => {
    expect(buildGraph({})).toEqual({ nodes: [], edges: [], ignored: { loopback: 0 } })
  })
})
