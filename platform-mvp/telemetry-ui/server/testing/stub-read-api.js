// A stand-in for telemetry-ingest's read API (and for Alertmanager), for tests. It serves the counters of a small shop whose
// frontend calls cartservice, and can make those calls fail. Not part of the service image.
//
//   as a module:  createStubReadApi({key}) -> {handler, state}
//   standalone:   KEY=... PROJECT=gke PORT=8080 node server/testing/stub-read-api.js
//                 POST /__control?broken=true|false   switches the calls between failing and healthy
//                 GET  /__posts                       what was posted to /api/v2/alerts (as Alertmanager)

import { createServer } from 'node:http'
import { pathToFileURL } from 'node:url'

export const FE = '/k8s/shop/frontend-5d6f8c7b9-xk2lp/server'
export const CART = '/k8s/shop/cartservice-7c9d8f6b5d-bk7q2/server'
const call = { container_id: FE, destination: '10.0.0.9:7070', actual_destination: '10.4.1.9:7070' }

function counter(labels, from, to, perSec) {
  const step = Math.max(15, Math.floor((to - from) / 40))
  const n = Math.max(Math.floor(to / step) - Math.floor(from / step), 1)
  return { labels, points: Array.from({ length: n }, (_, i) => ({ t: (from + i * step) * 1000, v: perSec })) }
}
const gauge = (labels, to) => ({ labels, points: [{ t: (to - 1) * 1000, v: 1 }] })

export function createStubReadApi({ key, now = () => Date.now() / 1000 }) {
  const state = { broken: true, down: false, requests: [], posts: [] }

  const handler = (req, res) => {
    const url = new URL(req.url, 'http://x')
    const send = (code, body) => {
      res.writeHead(code, { 'Content-Type': 'application/json' })
      res.end(JSON.stringify(body))
    }
    if (url.pathname === '/__control') {
      state.broken = url.searchParams.get('broken') !== 'false'
      return send(200, { broken: state.broken })
    }
    if (url.pathname === '/__posts') return send(200, state.posts)
    if (url.pathname === '/api/v2/alerts' && req.method === 'POST') {
      let body = ''
      req.on('data', (c) => (body += c))
      req.on('end', () => {
        state.posts.push({ url: req.url, body: JSON.parse(body) })
        send(200, {})
      })
      return
    }
    state.requests.push({ path: url.pathname, key: req.headers['x-api-key'] })
    if (state.down) return send(503, { error: 'unavailable' })
    if (req.headers['x-api-key'] !== key) return send(403, { error: 'this key cannot read that project' })
    if (url.pathname.endsWith('/metrics')) {
      return send(200, { data: ['container_net_tcp_listen_info', 'container_net_tcp_successful_connects_total', 'container_http_requests_total'] })
    }
    const name = url.searchParams.get('name')
    const from = Number(url.searchParams.get('from'))
    const to = Number(url.searchParams.get('to'))
    const recent = to > now() - 120 // the recent window ends now; the baseline ends five minutes before
    let series = []
    if (name === 'container_net_tcp_listen_info') {
      series = [gauge({ container_id: CART, listen_addr: '10.4.1.9:7070' }, to), gauge({ container_id: FE, listen_addr: '10.4.0.5:8080' }, to)]
    } else if (name === 'container_net_tcp_successful_connects_total') {
      series = [counter(call, from, to, 1)]
    } else if (name === 'container_http_requests_total') {
      series = recent && state.broken
        ? [counter({ ...call, status: '2xx' }, from, to, 2), counter({ ...call, status: '500' }, from, to, 8)] // 80% failing now
        : [counter({ ...call, status: '2xx' }, from, to, 10)]
    }
    return send(200, { data: { series } })
  }
  return { handler, state }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const { handler } = createStubReadApi({ key: process.env.KEY })
  createServer(handler).listen(Number(process.env.PORT || 8080), () => console.log('stub read API on', process.env.PORT || 8080))
}
