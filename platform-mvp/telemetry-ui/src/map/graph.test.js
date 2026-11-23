import { describe, expect, it } from 'vitest'
import { addressIndex, buildGraph, learnServiceTable } from './graph'

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
  it('maps a pod address to its application, by the exact address and by the pod\'s IP', () => {
    const ix = addressIndex(LISTEN)
    expect(ix.resolve('10.4.1.9:7070').id).toBe('/k8s/shop/cartservice')
    expect(ix.resolve('10.4.1.9:9999').id).toBe('/k8s/shop/cartservice') // any port on a pod's IP is that pod
    expect(ix.resolve('10.4.1.8:7070')).toBeNull()
    expect(ix.resolve('')).toBeNull()
  })

  it('ignores wildcard listeners, and loopback listeners of pods', () => {
    expect(addressIndex([listen(CART, '127.0.0.1:9000'), listen(CART, '0.0.0.0:80'), listen(CART, '[::]:80')]).size).toBe(0)
  })

  it('gives each port of an IP to the application that listens there, and no guess about the rest of the IP', () => {
    // two applications on one address, as on a host or with host networking: the port decides, and the IP alone decides nothing
    const ix = addressIndex([listen(CART, '10.0.0.1:1', 1000), listen(FE, '10.0.0.1:2', 1000)])
    expect(ix.resolve('10.0.0.1:1').id).toBe('/k8s/shop/cartservice')
    expect(ix.resolve('10.0.0.1:2').id).toBe('/k8s/shop/frontend')
    expect(ix.resolve('10.0.0.1:3')).toBeNull()
  })

  it('goes to the latest reporter when a pod IP is reused, and trusts the IP alone only for the one that is current', () => {
    const ix = addressIndex([listen(CART, '10.4.1.9:7070', 1000), listen(FE, '10.4.1.9:8080', 1000 + 10 * 60 * 1000)])
    expect(ix.resolve('10.4.1.9:8080').id).toBe('/k8s/shop/frontend')
    expect(ix.resolve('10.4.1.9:9999').id).toBe('/k8s/shop/frontend') // the old pod has been quiet for ten minutes
    expect(ix.resolve('10.4.1.9:7070').id).toBe('/k8s/shop/cartservice') // its exact address is still known
  })

  it('resolves an address two applications claim at the same time to nothing, and not to either', () => {
    const ix = addressIndex([listen(CART, '10.0.0.1:80', 1000), listen(FE, '10.0.0.1:80', 1000)])
    expect(ix.resolve('10.0.0.1:80')).toBeNull()
    expect(ix.resolve('10.0.0.1:81')).toBeNull() // and the IP does not get a say for a port that was listed
  })

  it('takes the later one when an exact address is claimed again', () => {
    expect(addressIndex([listen(CART, '10.0.0.1:80', 1000), listen(FE, '10.0.0.1:80', 2000)]).resolve('10.0.0.1:80').id).toBe('/k8s/shop/frontend')
  })

  it('does not trust the IP alone on a plain host, where one IP carries many services', () => {
    const NGINX = '/system.slice/nginx.service'
    const ix = addressIndex([{ labels: { container_id: NGINX, listen_addr: '10.1.0.7:80', machine_id: 'vm-1' }, last: 1000 }])
    expect(ix.resolve('10.1.0.7:80').name).toBe('nginx')
    expect(ix.resolve('10.1.0.7:22')).toBeNull() // some other service on the machine: not nginx
  })

  it('finds a loopback listener of a plain host from the same machine, and from no other', () => {
    const PG2 = '/docker/postgres'
    const ix = addressIndex([{ labels: { container_id: PG2, listen_addr: '127.0.0.1:5432', machine_id: 'vm-1' }, last: 1000 }])
    expect(ix.resolveLocal('vm-1', '127.0.0.1:5432').name).toBe('postgres')
    expect(ix.resolveLocal('vm-2', '127.0.0.1:5432')).toBeNull()
    expect(ix.resolveLocal('vm-1', '127.0.0.1:6379')).toBeNull()
    expect(ix.resolve('127.0.0.1:5432')).toBeNull() // loopback is never a global address
  })

  it('does not index a loopback listener of a pod, or one with no machine to place it on', () => {
    const ix = addressIndex([listen(CART, '127.0.0.1:9000'), { labels: { container_id: '/docker/x', listen_addr: '127.0.0.1:1' }, last: 1 }])
    expect(ix.resolveLocal('m', '127.0.0.1:9000')).toBeNull()
    expect(ix.resolveLocal('', '127.0.0.1:1')).toBeNull()
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

  it('drops loopback calls and calls to itself', () => {
    const g = buildGraph({
      listen: LISTEN,
      conn: [
        s({ container_id: FE, destination: '127.0.0.1:9000' }, 1),
        s({ container_id: FE, destination: '10.0.0.9:80', actual_destination: '127.0.0.1:80' }, 1),
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
    expect(buildGraph({})).toEqual({ nodes: [], edges: [], ignored: { loopback: 0 }, learned: { services: 0, used: 0, ambiguous: [] } })
  })
})

// The Service address (cluster IP) the frontend dials for cartservice, and the pod it reaches. A FAILED connect carries only the first.
const SVC = '10.0.0.9:7070'
const failedTo = (dest, container = FE) => ({ container_id: container, destination: dest })
const OTHER = '/k8s/shop/checkout-6c5d8f7b9d-zq9wn/server'
const LISTEN2 = [...LISTEN, listen(OTHER, '10.4.3.3:7070')]
const reached = (dest, actual, container = FE) => ({ container_id: container, destination: dest, actual_destination: actual })

describe('learnServiceTable', () => {
  const ips = addressIndex(LISTEN2)
  const table = (series) => learnServiceTable(series, ips)

  it('learns what a Service address leads to from a successful connection', () => {
    const { table: t, ambiguous } = table([s(reached(SVC, '10.4.1.9:7070'), 2)])
    expect(t.get(SVC).id).toBe('/k8s/shop/cartservice')
    expect(ambiguous).toEqual([])
  })

  it('learns nothing from what is not a translation, from loopback, or from a pod nobody claims', () => {
    expect(table([s(reached('10.4.1.9:7070', '10.4.1.9:7070'), 1)]).table.size).toBe(0) // a pod called directly
    expect(table([s(reached('127.0.0.1:80', '10.4.1.9:7070'), 1)]).table.size).toBe(0)
    expect(table([s(reached(SVC, '10.9.9.9:7070'), 1)]).table.size).toBe(0) // the reached pod is not a known application
    expect(table([s({ container_id: FE, destination: SVC }, 1)]).table.size).toBe(0) // no reached address at all: a failed connect
  })

  it('learns the exact address and port only', () => {
    const { table: t } = table([s(reached(SVC, '10.4.1.9:7070'), 1)])
    expect(t.has('10.0.0.9:80')).toBe(false)
    expect(t.has('10.0.0.9')).toBe(false)
  })

  it('goes to the busiest application when an address leads to several, says so, and breaks a tie by name', () => {
    const busy = table([s(reached(SVC, '10.4.1.9:7070'), 1), s(reached(SVC, '10.4.3.3:7070'), 9)])
    expect(busy.table.get(SVC).id).toBe('/k8s/shop/checkout')
    expect(busy.ambiguous).toEqual([{ destination: SVC, workloads: ['/k8s/shop/checkout', '/k8s/shop/cartservice'] }])
    const a = table([s(reached(SVC, '10.4.1.9:7070'), 5), s(reached(SVC, '10.4.3.3:7070'), 5)])
    const b = table([s(reached(SVC, '10.4.3.3:7070'), 5), s(reached(SVC, '10.4.1.9:7070'), 5)])
    expect(a.table.get(SVC).id).toBe('/k8s/shop/cartservice') // "cartservice" < "checkout"
    expect(b.table.get(SVC).id).toBe(a.table.get(SVC).id) // whatever the order of the data
  })

  it('counts a series as a vote even when its rate rounds to nothing', () => {
    expect(table([s(reached(SVC, '10.4.1.9:7070'), 0)]).table.get(SVC).id).toBe('/k8s/shop/cartservice')
  })
})

describe('buildGraph: a failed connect to a Service address', () => {
  const edgeOf = (g, to) => g.edges.find((e) => e.to === to)

  it('names the application behind the address, learned from the successful connections, instead of an external endpoint', () => {
    const g = buildGraph({ listen: LISTEN, conn: [s(reached(SVC, '10.4.1.9:7070'), 2)], fail: [s(failedTo(SVC), 3)] })
    expect(g.nodes.some((n) => n.external)).toBe(false)
    const e = edgeOf(g, '/k8s/shop/cartservice')
    expect(g.edges).toHaveLength(1)
    expect(e).toMatchObject({ from: '/k8s/shop/frontend', connPerSec: 2, failedConnects: 3 })
    expect(e.errPct).toBeCloseTo(60, 6) // 3 failed of 5 attempts: the refusals now count against cartservice
    expect(g.learned).toEqual({ services: 1, used: 1, ambiguous: [] })
  })

  it('names it from a longer look when the window has no successful connection to the address (it has been failing the whole time)', () => {
    const only = buildGraph({ listen: LISTEN, fail: [s(failedTo(SVC), 3)] })
    expect(only.edges[0].to).toBe(`external:${SVC}`) // nothing to learn from
    const g = buildGraph({ listen: LISTEN, fail: [s(failedTo(SVC), 3)], learn: [s(reached(SVC, '10.4.1.9:7070'), 1)] })
    expect(g.edges).toHaveLength(1)
    expect(g.edges[0]).toMatchObject({ to: '/k8s/shop/cartservice', failedConnects: 3, errPct: 100, errBasis: 'tcp' })
    expect(g.learned.used).toBe(1)
  })

  it('leaves an address that was never seen succeeding as external, and does not guess from another port', () => {
    const g = buildGraph({ listen: LISTEN, conn: [s(reached(SVC, '10.4.1.9:7070'), 1)], fail: [s(failedTo('10.0.0.9:80'), 1), s(failedTo('10.7.7.7:7070'), 1)] })
    expect(g.edges.filter((e) => e.to.startsWith('external:')).map((e) => e.to).sort()).toEqual(['external:10.0.0.9:80', 'external:10.7.7.7:7070'])
  })

  it('never lets what was learned override an address the connection actually reached', () => {
    // dialled SVC (learned: cartservice), but this connection's reached address is the checkout pod: believe the connection
    const g = buildGraph({ listen: LISTEN2, conn: [s(reached(SVC, '10.4.1.9:7070'), 5), s(reached(SVC, '10.4.3.3:7070', FE), 1)] })
    expect(g.edges.map((e) => e.to).sort()).toEqual(['/k8s/shop/cartservice', '/k8s/shop/checkout'])
    expect(g.learned.used).toBe(0) // the table was not needed
  })

  it('does not draw an application calling itself through its own Service', () => {
    const g = buildGraph({ listen: LISTEN, conn: [s(reached(SVC, '10.4.1.9:7070', CART), 1)], fail: [s(failedTo(SVC, CART), 1)] })
    expect(g.edges).toEqual([])
  })

  it('reports an ambiguous address only when it was used', () => {
    const conn = [s(reached(SVC, '10.4.1.9:7070'), 1), s(reached(SVC, '10.4.3.3:7070'), 9)]
    const used = buildGraph({ listen: LISTEN2, conn, fail: [s(failedTo(SVC), 1)] })
    expect(used.learned.ambiguous).toEqual([{ destination: SVC, workloads: ['/k8s/shop/checkout', '/k8s/shop/cartservice'] }])
    expect(edgeOf(used, '/k8s/shop/checkout').failedConnects).toBe(1)
    const unused = buildGraph({ listen: LISTEN2, conn })
    expect(unused.learned.ambiguous).toEqual([]) // known to be ambiguous, but nothing here depended on it
  })

  it('resolves the failed connects of a protocol too (a database Service that is refusing)', () => {
    const q = { container_id: CART, destination: '10.0.0.50:5432', actual_destination: '10.4.2.2:5432' }
    const g = buildGraph({
      listen: LISTEN,
      protocols: [{ name: 'postgres', unit: 'queries/s', calls: [s({ ...q, status: 'ok' }, 4)], latency: null }],
      fail: [s({ container_id: CART, destination: '10.0.0.50:5432' }, 2)],
    })
    expect(g.edges).toHaveLength(1)
    expect(g.edges[0]).toMatchObject({ from: '/k8s/shop/cartservice', to: '/k8s/data/postgres', failedConnects: 2, rps: 4 })
  })
})

describe('buildGraph: plain hosts', () => {
  const NGINX = '/system.slice/nginx.service'
  const API = '/system.slice/shop-api.service'
  const PGD = '/docker/postgres'
  const REDIS = '/docker/redis'
  const h = (container_id, listen_addr, machine_id, last = 1000) => ({ labels: { container_id, listen_addr, machine_id }, last })
  const call = (container_id, destination, machine_id, extra = {}) => ({ container_id, destination, actual_destination: destination, machine_id, ...extra })
  // vm-1 (10.1.0.7) runs nginx on :80, shop-api on :8080 and postgres on loopback :5432; vm-2 (10.1.0.8) runs redis on :6379
  const LISTEN_VM = [
    h(NGINX, '10.1.0.7:80', 'vm-1'), h(API, '10.1.0.7:8080', 'vm-1'), h(PGD, '127.0.0.1:5432', 'vm-1'), h(REDIS, '10.1.0.8:6379', 'vm-2'),
    h('/system.slice/sshd.service', '10.1.0.7:22', 'vm-1'),
  ]

  it('tells services on one IP apart by port, and names them as applications of the host', () => {
    const g = buildGraph({ listen: LISTEN_VM, conn: [s(call(NGINX, '10.1.0.7:8080', 'vm-1'), 2), s(call(NGINX, '10.1.0.7:22', 'vm-1'), 1)] })
    expect(g.edges.map((e) => [e.from, e.to]).sort()).toEqual([[NGINX, '/system.slice/shop-api.service'], [NGINX, '/system.slice/sshd.service']])
    const api = g.nodes.find((n) => n.id === API)
    expect(api).toMatchObject({ name: 'shop-api', ns: 'host', external: false })
    expect(g.nodes.some((n) => n.external)).toBe(false)
  })

  it('draws a call to a port nothing listens on as external, not as the other service on that IP', () => {
    const g = buildGraph({ listen: LISTEN_VM, conn: [s(call(NGINX, '10.1.0.7:9999', 'vm-1'), 1)] })
    expect(g.edges[0].to).toBe('external:10.1.0.7:9999')
  })

  it('links a process to one on the same machine through loopback, which inside a pod would not be a call', () => {
    const g = buildGraph({ listen: LISTEN_VM, conn: [s(call(API, '127.0.0.1:5432', 'vm-1'), 3)] })
    expect(g.edges).toHaveLength(1)
    expect(g.edges[0]).toMatchObject({ from: API, to: PGD, connPerSec: 3 })
    expect(g.ignored.loopback).toBe(0)
  })

  it('does not link a loopback call to a listener on another machine, and counts it as ignored', () => {
    const g = buildGraph({ listen: LISTEN_VM, conn: [s(call(API, '127.0.0.1:5432', 'vm-2'), 3)] })
    expect(g.edges).toEqual([])
    expect(g.ignored.loopback).toBe(1)
  })

  it('still ignores loopback inside a pod, even when a host has a listener on the same port', () => {
    const g = buildGraph({ listen: LISTEN_VM, conn: [s({ container_id: FE, destination: '127.0.0.1:5432', machine_id: 'vm-1' }, 1)] })
    expect(g.edges).toEqual([])
    expect(g.ignored.loopback).toBe(1)
  })

  it('does not draw a host process calling itself over loopback', () => {
    const SELF = [h(API, '127.0.0.1:9100', 'vm-1')]
    expect(buildGraph({ listen: SELF, conn: [s(call(API, '127.0.0.1:9100', 'vm-1'), 1)] }).edges).toEqual([])
  })

  it('links services on different machines, and one unit running on two machines is one application', () => {
    const two = [...LISTEN_VM, h(API, '10.1.0.8:8080', 'vm-2')]
    const g = buildGraph({ listen: two, conn: [s(call(NGINX, '10.1.0.8:8080', 'vm-1'), 1), s(call(NGINX, '10.1.0.7:8080', 'vm-1'), 1), s(call(API, '10.1.0.8:6379', 'vm-1'), 4)] })
    expect(g.edges.map((e) => [e.from, e.to]).sort()).toEqual([[API, REDIS], [NGINX, API]].sort())
    expect(g.edges.find((e) => e.to === API).connPerSec).toBe(2) // both machines' calls add up
  })

  it('learns a Service address on a host too: a failed connect to it is drawn against the service behind it', () => {
    const VIP = '10.1.0.100:6379' // a virtual IP in front of redis
    const ok = { container_id: API, destination: VIP, actual_destination: '10.1.0.8:6379', machine_id: 'vm-1' }
    const g = buildGraph({ listen: LISTEN_VM, conn: [s(ok, 2)], fail: [s({ container_id: API, destination: VIP, machine_id: 'vm-1' }, 1)] })
    expect(g.edges).toHaveLength(1)
    expect(g.edges[0]).toMatchObject({ from: API, to: REDIS, failedConnects: 1 })
  })
})

describe('connection timings on edges', () => {
  const conn = [s(call, 10)]
  const base = { listen: LISTEN, conn }
  it('gives an edge the mean time to open a connection: seconds spent over connections made', () => {
    const g = buildGraph({ ...base, connTime: [s(call, 0.5)] }) // 0.5 s a second spent connecting, 10 connections a second: 50 ms each
    expect(edge(g, '/k8s/shop/frontend', '/k8s/shop/cartservice').connMs).toBe(50)
  })
  it('has no connect time without connections to divide by, or without the metric', () => {
    expect(edge(buildGraph({ ...base }), '/k8s/shop/frontend', '/k8s/shop/cartservice').connMs).toBeNull()
    expect(edge(buildGraph({ listen: LISTEN, conn: [s(call, 0)], connTime: [s(call, 1)] }), '/k8s/shop/frontend', '/k8s/shop/cartservice')?.connMs ?? null).toBeNull()
  })
  it('gives an edge the mean round trip to its callee, by the callee\'s IP', () => {
    const g = buildGraph({ ...base, rtt: [{ labels: { container_id: FE, destination_ip: '10.4.1.9' }, value: 0.002 }, { labels: { container_id: FE, destination_ip: '10.4.1.9' }, value: 0.004 }] })
    expect(edge(g, '/k8s/shop/frontend', '/k8s/shop/cartservice').rttMs).toBe(3)
  })
  it('never makes an edge out of a round-trip reading, and ignores one for an IP no pod owns or a bad value', () => {
    const g = buildGraph({
      ...base,
      rtt: [
        { labels: { container_id: CART, destination_ip: '10.4.0.5' }, value: 0.001 }, // cartservice -> frontend was never called
        { labels: { container_id: FE, destination_ip: '10.9.9.9' }, value: 0.001 },
        { labels: { container_id: FE, destination_ip: '10.4.1.9' }, value: NaN },
        { labels: { container_id: FE, destination_ip: '10.4.1.9' }, value: -1 },
      ],
    })
    expect(g.edges).toHaveLength(1)
    expect(g.edges[0].rttMs).toBeNull()
  })
})
