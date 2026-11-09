// A demo of telemetry-ingest's read API: a small shop with a few things wrong, so every page of the UI has something to show with
// no cluster. For screenshots and trying the UI out; not part of the service image.
//
//   KEY=demo-read-key PROJECT=demo PORT=8080 node server/testing/demo-read-api.js
//   then point the UI at it:  make ui-dev UI_API=http://host.docker.internal:8080   (project "demo", key "demo-read-key")
//
// What is wrong: cartservice restarted and its calls to redis-cart fail; adservice logs errors; checkout has a pod that stopped.

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

function series(name, from, to) {
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
    for (const [f, t, rps] of CALLS) out.push(counter({ container_id: idOf(...f).id, destination: `${ip(...t)}:8080`, actual_destination: `${ip(...t)}:8080` }, from, to, rps / 20))
  } else if (name === 'container_net_tcp_failed_connects_total') {
    out.push(counter({ container_id: idOf('shop', 'cartservice').id, destination: `${ip('shop', 'redis-cart')}:8080`, actual_destination: `${ip('shop', 'redis-cart')}:8080` }, from, to, 0.4))
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
      const l = { container_id: idOf(...f).id, destination: `${ip(...t)}:8080`, actual_destination: `${ip(...t)}:8080` }
      out.push(counter({ ...l, status: 'ok' }, from, to, rps - bad))
      if (bad) out.push(counter({ ...l, status: 'failed' }, from, to, bad))
    }
  }
  return out
}
const METRICS = ['container_resources_memory_rss_bytes', 'container_resources_memory_limit_bytes', 'container_resources_cpu_usage_seconds_total', 'container_restarts_total',
  'container_application_type', 'container_net_tcp_listen_info', 'container_net_tcp_successful_connects_total', 'container_net_tcp_failed_connects_total',
  'container_http_requests_total', 'container_redis_queries_total', 'container_postgres_queries_total']

const SERVICES = APPS.map(([ns, name]) => `/k8s/${ns}/${name}`)
const iso = (ago) => new Date(Date.now() - ago * 1000).toISOString()
const LOGS = [
  ...Array.from({ length: 14 }, (_, i) => ({ time: iso(20 + i * 40), severity: 'ERROR', severity_number: 17, service: '/k8s/shop/adservice', body: `failed to fetch ads for session ${1000 + i}: upstream timeout after ${3000 + i} ms` })),
  ...Array.from({ length: 5 }, (_, i) => ({ time: iso(60 + i * 90), severity: 'ERROR', severity_number: 17, service: '/k8s/shop/adservice', body: 'ad cache unavailable' })),
  ...Array.from({ length: 6 }, (_, i) => ({ time: iso(30 + i * 100), severity: 'ERROR', severity_number: 17, service: '/k8s/shop/cartservice', body: `redis: connection refused (attempt ${i + 1})` })),
  { time: iso(400), severity: 'ERROR', severity_number: 17, service: '/k8s/monitoring/prometheus', body: 'scrape failed: context deadline exceeded' },
]

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
    if (path === 'metrics') return send(200, { data: METRICS })
    if (path === 'metrics/query_range') return send(200, { data: { series: series(q.get('name'), Number(q.get('from')), Number(q.get('to'))) } })
    if (path === 'traces/services') return send(200, { data: SERVICES })
    if (path === 'logs/services') return send(200, { data: ['/k8s/shop/adservice', '/k8s/shop/cartservice', '/k8s/monitoring/prometheus'] })
    if (path === 'logs') {
      const svc = q.get('service')
      const sev = (q.get('severity') || '').toUpperCase()
      return send(200, { data: LOGS.filter((l) => (!svc || l.service === svc) && (!sev || l.severity === sev)).slice(0, Number(q.get('limit') || 100)) })
    }
    if (path === 'traces') return send(200, { data: [] })
    if (path === 'profiles') return send(200, { data: [] })
    if (path === 'alerts') return send(404, { error: 'not found' }) // no alert service in the demo: the page checks for itself
    return send(404, { error: 'not found' })
  }
}

import { pathToFileURL } from 'node:url'
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  createServer(createDemoApi()).listen(Number(process.env.PORT || 8080), () => console.log(`demo read API on ${process.env.PORT || 8080}: project "${PROJECT}", key "${KEY}"`))
}
