// A demo of telemetry-ingest's read API: a small shop with a few things wrong, so every page of the UI has something to show with
// no cluster. For screenshots and trying the UI out; not part of the service image.
//
//   KEY=demo-read-key PROJECT=demo PORT=8080 node server/testing/demo-read-api.js
//   then point the UI at it:  make ui-dev UI_API=http://host.docker.internal:8080   (project "demo", key "demo-read-key")
//
// What is wrong: cartservice restarted and its calls to redis-cart fail; adservice logs errors; checkout has a pod that stopped; the DNS
// server fails some of cartservice's lookups. Beside the Kubernetes shop there is a small estate of VMs: nginx and a shop API run as systemd
// units (the API on two machines), postgres in Docker on loopback and redis in Docker on the second machine.

import { createServer } from 'node:http'

const KEY = process.env.KEY || 'demo-read-key'
const PROJECT = process.env.PROJECT || 'demo'
const pod = (ns, name, hash, suffix, c = 'server') => `/k8s/${ns}/${name}-${hash}-${suffix}/${c}`

// [namespace, name, pod count, application type, memory MiB, cpu cores, restarts per hour]
const APPS = [
  ['shop', 'frontend', 2, 'unknown', 96, 0.18, 0],
  ['shop', 'cartservice', 1, 'unknown', 120, 0.31, 3],
  ['shop', 'checkoutservice', 1, 'unknown', 54, 0.05, 0],
  ['shop', 'currencyservice', 2, 'unknown', 71, 0.22, 0],
  ['shop', 'productcatalogservice', 1, 'unknown', 66, 0.09, 0],
  ['shop', 'adservice', 1, 'unknown', 188, 0.12, 0],
  ['shop', 'redis-cart', 1, 'redis', 28, 0.02, 0],
  ['payments', 'ledger', 2, 'postgres', 310, 0.4, 0],
  ['payments', 'gateway', 1, 'unknown', 90, 0.1, 0],
  ['monitoring', 'prometheus', 1, 'prometheus', 820, 0.5, 0],
  ['monitoring', 'telemetry-ingest', 1, 'unknown', 140, 0.2, 0],
  ['kube-system', 'coredns', 2, 'unknown', 24, 0.03, 0],
]
const HASH = ['5d6f8c7b9', '7c9d8f6b5d', '6c5d8f7b9d', '5b8d7f6c9']
const SUFFIX = ['xk2lp', 'bk7q2', 'zq9wn', 'mn4pq', 'tb8qs'] // five characters from the set Kubernetes uses for a ReplicaSet's pods
const containers = []
APPS.forEach(([ns, name, n, type, mem, cpu, restarts], a) => {
  for (let i = 0; i < n; i++) containers.push({ ns, name, id: pod(ns, name, HASH[a % 4], SUFFIX[(a + i) % 5], name), type, mem: mem * 1024 * 1024, cpu: cpu / n, restarts: restarts / n, ip: `10.4.${a}.${10 + i}` })
})
// checkoutservice had a second pod that stopped reporting, so it shows 1/2
// Service (cluster) addresses: what an app dials, which the kernel translates to a pod. Redis is reached through one.
const SERVICE_IP = { redisCart: '10.96.0.50:8080' }
const GONE = { id: pod('shop', 'checkoutservice', HASH[2], 'gn7kq', 'checkoutservice'), ns: 'shop', name: 'checkoutservice', ip: '10.4.2.99' }
const idOf = (ns, name) => containers.find((c) => c.ns === ns && c.name === name)
const ip = (ns, name) => idOf(ns, name).ip

const CALLS = [
  // from, to, calls/s, 5xx/s, protocol
  [['shop', 'frontend'], ['shop', 'productcatalogservice'], 40, 0, 'http'],
  [['shop', 'frontend'], ['shop', 'currencyservice'], 55, 0, 'http'],
  [['shop', 'frontend'], ['shop', 'cartservice'], 22, 0.3, 'http'],
  [['shop', 'frontend'], ['shop', 'checkoutservice'], 6, 0, 'http'],
  [['shop', 'frontend'], ['shop', 'adservice'], 18, 1.4, 'http'],
  [['shop', 'checkoutservice'], ['shop', 'cartservice'], 6, 0, 'http'],
  [['shop', 'checkoutservice'], ['payments', 'gateway'], 5, 0, 'http'],
  [['payments', 'gateway'], ['payments', 'ledger'], 5, 0, 'postgres'],
  [['shop', 'cartservice'], ['shop', 'redis-cart'], 30, 12, 'redis'],
]

function counter(labels, from, to, perSec) {
  const step = Math.max(15, Math.floor((to - from) / 40))
  const n = Math.max(Math.floor(to / step) - Math.floor(from / step), 1)
  return { labels, points: Array.from({ length: n }, (_, i) => ({ t: (from + i * step) * 1000, v: perSec })) }
}
const gauge = (labels, to, v, fresh = true) => ({ labels, points: [{ t: (fresh ? to - 1 : to - 2000) * 1000, v }] })

// ---- plain hosts: systemd units and Docker containers, told apart by machine, address AND port ----
// [container id (the cgroup path), machine, listen address, memory MiB, cpu cores, application type]
const HOSTS = [
  ['/system.slice/nginx.service', 'vm-01', '10.1.0.7:80', 40, 0.05, ''],
  ['/system.slice/shop-api.service', 'vm-01', '10.1.0.7:8080', 220, 0.3, ''],
  ['/system.slice/shop-api.service', 'vm-02', '10.1.0.8:8080', 210, 0.28, ''],
  ['/docker/postgres', 'vm-01', '127.0.0.1:5432', 380, 0.2, 'postgres'], // listens on loopback: only processes on vm-01 can reach it
  ['/docker/redis', 'vm-02', '10.1.0.8:6379', 30, 0.02, 'redis'],
]
// [from container, from machine, destination, protocol, calls a second, failing a second]
const HOST_CALLS = [
  ['/system.slice/nginx.service', 'vm-01', '10.1.0.7:8080', 'http', 12, 0],
  ['/system.slice/nginx.service', 'vm-01', '10.1.0.8:8080', 'http', 11, 0],
  ['/system.slice/shop-api.service', 'vm-01', '127.0.0.1:5432', 'postgres', 9, 0], // a process calling another on the same machine
  ['/system.slice/shop-api.service', 'vm-01', '10.1.0.8:6379', 'redis', 14, 0],
  ['/system.slice/shop-api.service', 'vm-02', '10.1.0.8:6379', 'redis', 15, 0],
]
function hostSeries(name, from, to) {
  const out = []
  const lbl = (c, machine) => ({ container_id: c, machine_id: machine })
  if (name === 'container_resources_memory_rss_bytes') {
    for (const [c, m, , mem] of HOSTS) out.push(gauge(lbl(c, m), to, mem * 1024 * 1024))
  } else if (name === 'container_resources_cpu_usage_seconds_total') {
    for (const [c, m, , , cpu] of HOSTS) out.push(counter(lbl(c, m), from, to, cpu))
  } else if (name === 'container_application_type') {
    for (const [c, m, , , , t] of HOSTS) {
      if (t) out.push(gauge({ ...lbl(c, m), application_type: t }, to, 1))
    }
  } else if (name === 'container_net_tcp_listen_info') {
    for (const [c, m, addr] of HOSTS) out.push(gauge({ ...lbl(c, m), listen_addr: addr }, to, 1))
  } else if (name === 'container_net_tcp_successful_connects_total') {
    for (const [c, m, dest, , rps] of HOST_CALLS) out.push(counter({ ...lbl(c, m), destination: dest, actual_destination: dest }, from, to, rps / 20))
  } else if (name === 'container_http_requests_total') {
    for (const [c, m, dest, proto, rps] of HOST_CALLS) {
      if (proto === 'http') out.push(counter({ ...lbl(c, m), destination: dest, actual_destination: dest, status: '2xx' }, from, to, rps))
    }
  } else if (name === 'container_postgres_queries_total' || name === 'container_redis_queries_total') {
    const want = name.includes('redis') ? 'redis' : 'postgres'
    for (const [c, m, dest, proto, rps] of HOST_CALLS) {
      if (proto === want) out.push(counter({ ...lbl(c, m), destination: dest, actual_destination: dest, status: 'ok' }, from, to, rps))
    }
  }
  return out
}

// DNS lookups: the frontend's mostly succeed (and its short names each miss a search domain first, which is normal); cartservice's server fails some.
function dnsSeries(from, to) {
  const l = (c, domain, status) => ({ container_id: c, domain, status, request_type: 'TypeA' })
  const fe = idOf('shop', 'frontend').id
  const cart = idOf('shop', 'cartservice').id
  return [
    counter(l(fe, 'productcatalogservice.shop.svc.cluster.local', 'ok'), from, to, 4),
    counter(l(fe, 'productcatalogservice.shop.svc.cluster.local.shop.svc.cluster.local', 'nxdomain'), from, to, 4),
    counter(l(fe, 'ads.example.com', 'ok'), from, to, 1),
    counter(l(cart, 'redis-cart.shop.svc.cluster.local', 'ok'), from, to, 6),
    counter(l(cart, 'redis-cart.shop.svc.cluster.local', 'servfail'), from, to, 0.9),
    counter(l(cart, 'metadata.google.internal', 'ok'), from, to, 0.3),
  ]
}

// ---- an incident: a nine-minute network delay between cartservice and redis-cart, an hour and a half before the demo started ----
// cartservice's redis calls take 0.2 s a round trip instead of 0.4 ms, so its own requests fail and slow down, and so do the frontend's.
const INC_START = Math.floor((Date.now() / 1000 - 5400) / 60) * 60
const INC_END = INC_START + 540
const inIncident = (t) => t >= INC_START && t < INC_END
// one point per grid step of the range, as the real service answers a rate query
function sampled(from, to, step, f) {
  const first = Math.floor(from / step) + 1
  const n = Math.max(Math.floor(to / step) - Math.floor(from / step), 1)
  return Array.from({ length: n }, (_, i) => ({ t: (first + i) * step * 1000, v: f((first + i) * step) }))
}
// [namespace, name, requests a second, failing share and slow share while the incident lasts]
const SERVED = [['shop', 'frontend', 30, 0.4, 0.8], ['shop', 'cartservice', 28, 0.25, 0.7]]
function incidentSeries(name, from, to, step) {
  const out = []
  const served = (ns, app, fn) => containers.filter((c) => c.ns === ns && c.name === app).forEach((c, _i, all) => fn(c, all.length))
  if (name === 'container_http_inbound_requests_total') {
    for (const [ns, app, rps, bad] of SERVED) {
      served(ns, app, (c, pods) => {
        out.push({ labels: { container_id: c.id, status: '200' }, points: sampled(from, to, step, (t) => (rps / pods) * (inIncident(t) ? 1 - bad : 1)) })
        out.push({ labels: { container_id: c.id, status: '502' }, points: sampled(from, to, step, (t) => (inIncident(t) ? (rps / pods) * bad : 0)) })
      })
    }
  } else if (name === 'container_http_inbound_requests_duration_seconds_total_bucket') {
    for (const [ns, app, rps, , slow] of SERVED) {
      served(ns, app, (c, pods) => {
        const r = rps / pods
        // cumulative: how many a second took at most that long. Healthy: nearly all under 100 ms. In the incident `slow` of them take over a second.
        for (const [le, healthy, hurt] of [['0.1', 0.92, 0.05], ['0.5', 0.995, 1 - slow], ['1', 1, 1 - slow + 0.05], ['2.5', 1, 1], ['+Inf', 1, 1]]) {
          out.push({ labels: { container_id: c.id, le }, points: sampled(from, to, step, (t) => r * (inIncident(t) ? hurt : healthy)) })
        }
      })
    }
  } else if (name === 'container_net_latency_seconds') {
    for (const [f, t] of CALLS) {
      const delayed = f[1] === 'cartservice' && t[1] === 'redis-cart'
      out.push({ labels: { container_id: idOf(...f).id, destination_ip: ip(...t) }, points: sampled(from, to, step, (x) => (delayed && inIncident(x) ? 0.21 : 0.0004)) })
    }
  } else if (name === 'container_net_tcp_connection_time_seconds_total') {
    for (const [f, t, rps] of CALLS) {
      const delayed = f[1] === 'cartservice' && t[1] === 'redis-cart'
      const dialled = t[1] === 'redis-cart' ? SERVICE_IP.redisCart : `${ip(...t)}:8080`
      out.push({ labels: { container_id: idOf(...f).id, destination: dialled, actual_destination: `${ip(...t)}:8080` }, points: sampled(from, to, step, (x) => (rps / 20) * (delayed && inIncident(x) ? 0.4 : 0.001)) })
    }
  } else if (name === 'container_redis_queries_duration_seconds_total_bucket') {
    for (const [f, t, rps, bad, proto] of CALLS) {
      if (proto !== 'redis') continue
      const dialled = SERVICE_IP.redisCart
      const labels = (le) => ({ container_id: idOf(...f).id, destination: dialled, actual_destination: `${ip(...t)}:8080`, le })
      for (const [le, healthy, hurt] of [['0.005', 0.9, 0], ['0.025', 1, 0.05], ['1', 1, 1], ['+Inf', 1, 1]]) {
        out.push({ labels: labels(le), points: sampled(from, to, step, (x) => (rps - bad) * (inIncident(x) ? hurt : healthy)) })
      }
    }
  } else return null
  return out
}

function series(name, from, to, step = 15) {
  const incident = incidentSeries(name, from, to, step)
  if (incident) return incident
  const out = []
  if (name === 'container_resources_memory_rss_bytes') {
    for (const c of containers) out.push(gauge({ container_id: c.id }, to, c.mem))
    out.push(gauge({ container_id: GONE.id }, to, 50 * 1024 * 1024, false))
  } else if (name === 'container_resources_memory_limit_bytes') {
    for (const c of containers) out.push(gauge({ container_id: c.id }, to, c.mem * 2))
  } else if (name === 'container_resources_cpu_usage_seconds_total') {
    for (const c of containers) out.push(counter({ container_id: c.id }, from, to, c.cpu))
  } else if (name === 'container_restarts_total') {
    for (const c of containers) out.push(counter({ container_id: c.id }, from, to, c.restarts / 3600))
  } else if (name === 'container_application_type') {
    for (const c of containers) if (c.type !== 'unknown') out.push(gauge({ container_id: c.id, application_type: c.type }, to, 1))
  } else if (name === 'container_net_tcp_listen_info') {
    for (const c of containers) out.push(gauge({ container_id: c.id, listen_addr: `${c.ip}:8080` }, to, 1))
  } else if (name === 'container_net_tcp_successful_connects_total') {
    for (const [f, t, rps] of CALLS) {
      const dialled = t[1] === 'redis-cart' ? SERVICE_IP.redisCart : `${ip(...t)}:8080`
      out.push(counter({ container_id: idOf(...f).id, destination: dialled, actual_destination: `${ip(...t)}:8080` }, from, to, rps / 20))
    }
  } else if (name === 'container_net_tcp_failed_connects_total') {
    // A failed connect carries only what was dialled (the Service's cluster IP), never the pod: there was no connection to look up.
    out.push(counter({ container_id: idOf('shop', 'cartservice').id, destination: SERVICE_IP.redisCart }, from, to, 0.4))
  } else if (name === 'container_http_requests_total') {
    for (const [f, t, rps, bad, proto] of CALLS) {
      if (proto !== 'http') continue
      const l = { container_id: idOf(...f).id, destination: `${ip(...t)}:8080`, actual_destination: `${ip(...t)}:8080` }
      out.push(counter({ ...l, status: '2xx' }, from, to, rps - bad))
      if (bad) out.push(counter({ ...l, status: '500' }, from, to, bad))
    }
  } else if (name === 'container_redis_queries_total' || name === 'container_postgres_queries_total') {
    const want = name.includes('redis') ? 'redis' : 'postgres'
    for (const [f, t, rps, bad, proto] of CALLS) {
      if (proto !== want) continue
      const dialled = want === 'redis' ? SERVICE_IP.redisCart : `${ip(...t)}:8080`
      const l = { container_id: idOf(...f).id, destination: dialled, actual_destination: `${ip(...t)}:8080` }
      out.push(counter({ ...l, status: 'ok' }, from, to, rps - bad))
      if (bad) out.push(counter({ ...l, status: 'failed' }, from, to, bad))
    }
  }
  out.push(...hostSeries(name, from, to))
  if (name === 'container_dns_requests_total') out.push(...dnsSeries(from, to))
  return out
}
const METRICS = ['container_resources_memory_rss_bytes', 'container_resources_memory_limit_bytes', 'container_resources_cpu_usage_seconds_total', 'container_restarts_total',
  'container_application_type', 'container_net_tcp_listen_info', 'container_net_tcp_successful_connects_total', 'container_net_tcp_failed_connects_total',
  'container_http_requests_total', 'container_redis_queries_total', 'container_postgres_queries_total', 'container_dns_requests_total',
  'container_http_inbound_requests_total', 'container_http_inbound_requests_duration_seconds_total_bucket', 'container_net_latency_seconds',
  'container_net_tcp_connection_time_seconds_total', 'container_redis_queries_duration_seconds_total_bucket']

const SERVICES = [...APPS.map(([ns, name]) => `/k8s/${ns}/${name}`), ...new Set(HOSTS.map((h) => h[0]))]
const iso = (ago) => new Date(Date.now() - ago * 1000).toISOString()
const LOGS = [
  ...Array.from({ length: 14 }, (_, i) => ({ time: iso(20 + i * 40), severity: 'ERROR', severity_number: 17, service: '/k8s/shop/adservice', body: `failed to fetch ads for session ${1000 + i}: upstream timeout after ${3000 + i} ms` })),
  ...Array.from({ length: 5 }, (_, i) => ({ time: iso(60 + i * 90), severity: 'ERROR', severity_number: 17, service: '/k8s/shop/adservice', body: 'ad cache unavailable' })),
  ...Array.from({ length: 6 }, (_, i) => ({ time: iso(30 + i * 100), severity: 'ERROR', severity_number: 17, service: '/k8s/shop/cartservice', body: `redis: connection refused (attempt ${i + 1})` })),
  { time: iso(400), severity: 'ERROR', severity_number: 17, service: '/k8s/monitoring/prometheus', body: 'scrape failed: context deadline exceeded' },
  ...Array.from({ length: 30 }, (_, i) => ({ time: new Date((INC_START + 20 + i * 15) * 1000).toISOString(), severity: 'ERROR', severity_number: 17, service: '/k8s/shop/cartservice', body: `redis: i/o timeout reading reply for key cart:${4000 + i} after 200 ms` })),
  ...Array.from({ length: 12 }, (_, i) => ({ time: new Date((INC_START + 40 + i * 40) * 1000).toISOString(), severity: 'ERROR', severity_number: 17, service: '/k8s/shop/frontend', body: 'could not retrieve cart: rpc error: code = DeadlineExceeded' })),
]
// root spans of the frontend: ordinary ones all the time, failing and slow ones in the incident
const SPANS = [
  ...Array.from({ length: 40 }, (_, i) => {
    const t = INC_START + 10 + i * 12
    return { time: new Date(t * 1000).toISOString(), trace_id: (0xabc000 + i).toString(16).padStart(32, '0'), span_id: (i + 1).toString(16).padStart(16, '0'), name: 'GET /cart', kind: 'SERVER', service: '/k8s/shop/frontend', duration_ns: (1500 + (i % 7) * 120) * 1e6, status_code: i % 3 === 0 ? 'ERROR' : 'OK', status_message: i % 3 === 0 ? 'context deadline exceeded' : '' }
  }),
  ...Array.from({ length: 10 }, (_, i) => ({ time: new Date((INC_START - 3000 + i * 200) * 1000).toISOString(), trace_id: (0xdef000 + i).toString(16).padStart(32, '0'), span_id: (i + 100).toString(16).padStart(16, '0'), name: 'GET /cart', kind: 'SERVER', service: '/k8s/shop/frontend', duration_ns: 30e6, status_code: 'OK' })),
]

const GB = 1024 ** 3
function demoClickHouse() {
  const ago = (ms) => new Date(Date.now() - ms).toISOString()
  const kinds = [
    ['metrics', 2_400_000_000, 11 * GB, 11.04, 336, 14 * 86400_000],
    ['traces', 310_000_000, 38 * GB, 5.95, 168, 7 * 86400_000],
    ['logs', 120_000_000, 9 * GB, 9.1, 168, 25 * 60_000],
    ['profiles', 800_000, 2 * GB, 4.2, 168, 7 * 86400_000],
  ]
  return {
    connection: { reachable: true, protocol: 'native', database: 'telemetry', tls: false, version: '24.8.14.39', ping_ms: 1.8 },
    storage: kinds.map(([type, rows, bytes, compression, hours, age]) => ({ type, rows, estimated_bytes: Math.round(bytes / 3), compression, ttl_seconds: hours * 3600, oldest: ago(age), table_bytes: bytes, table_rows: rows * 3 })),
    cluster: { servers: [{ host: 'chi-clickhouse-0-0-0', disks: [{ name: 'default', path: '/var/lib/clickhouse/', type: 'Local', free: 61 * GB, total: 100 * GB, used: 39 * GB }, { name: 'gcs', path: '', type: 'ObjectStorage', free: 0, total: 0, unlimited: true, used: 52 * GB }] }] },
    warnings: [],
  }
}

export function createDemoApi() {
  return (req, res) => {
    const url = new URL(req.url, 'http://x')
    const send = (code, body) => { res.writeHead(code, { 'Content-Type': 'application/json' }); res.end(JSON.stringify(body)) }
    const m = /^\/api\/v1\/projects\/([^/]+)\/(.*)$/.exec(url.pathname)
    if (!m) return send(404, { error: 'not found' })
    if (req.headers['x-api-key'] !== KEY) return send(403, { error: 'this key cannot read that project' })
    if (m[1] !== PROJECT) return send(403, { error: 'this key cannot read that project' })
    const path = m[2]
    const q = url.searchParams
    // NO_AGENT=1: a project nothing has reported to; NO_AGENT=other: one that only has metrics from something that is not the node-agent
    if (path === 'metrics') return send(200, { data: process.env.NO_AGENT === '1' ? [] : process.env.NO_AGENT === 'other' ? ['up', 'node_cpu_seconds_total'] : METRICS })
    if (path === 'metrics/query_range') return send(200, { data: { series: series(q.get('name'), Number(q.get('from')), Number(q.get('to')), Number(q.get('step')) || 15) } })
    if (path === 'traces/services') return send(200, { data: SERVICES })
    if (path === 'logs/services') return send(200, { data: ['/k8s/shop/adservice', '/k8s/shop/cartservice', '/k8s/monitoring/prometheus'] })
    if (path === 'logs') {
      const svc = q.get('service')
      const sev = (q.get('severity') || '').toUpperCase()
      const lo = q.get('from') ? Number(q.get('from')) * 1000 : -Infinity
      const hi = q.get('to') ? Number(q.get('to')) * 1000 : Infinity
      return send(200, { data: LOGS.filter((l) => (!svc || l.service === svc) && (!sev || l.severity === sev) && Date.parse(l.time) >= lo && Date.parse(l.time) <= hi).slice(0, Number(q.get('limit') || 100)) })
    }
    if (path === 'traces') {
      const svc = q.get('service')
      const status = (q.get('status') || '').toUpperCase()
      const min = Number(q.get('min_duration_ms') || 0) * 1e6
      const lo = q.get('from') ? Number(q.get('from')) * 1000 : -Infinity
      const hi = q.get('to') ? Number(q.get('to')) * 1000 : Infinity
      return send(200, { data: SPANS.filter((r) => (!svc || r.service === svc) && (!status || r.status_code === status) && r.duration_ns >= min && Date.parse(r.time) >= lo && Date.parse(r.time) <= hi).slice(0, Number(q.get('limit') || 100)) })
    }
    if (path === 'profiles') return send(200, { data: [] })
    if (path === 'clickhouse') return send(200, { data: demoClickHouse() })
    if (path === 'alerts') return send(404, { error: 'not found' }) // no alert service in the demo: the page checks for itself
    return send(404, { error: 'not found' })
  }
}

import { pathToFileURL } from 'node:url'
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  createServer(createDemoApi()).listen(Number(process.env.PORT || 8080), () => console.log(`demo read API on ${process.env.PORT || 8080}: project "${PROJECT}", key "${KEY}"`))
}
