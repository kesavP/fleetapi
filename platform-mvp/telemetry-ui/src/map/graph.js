import { FAILED_STATUS, HTTP_UNIT, histogramQuantile, hostOf, isLoopback, isServerError, isUnspecified, parseLe, workloadOf } from './workload.js'

// Builds the service map from the agents' counters. Input series are {labels, rate} (mean per-second rate over the
// window); `listen` is {labels, last} from the listen-address gauge, `last` being when it was last reported (ms).
//
// There is no Kubernetes API here, so a destination is recognised the way the agent itself exposes it: every container
// reports the addresses it listens on (container_net_tcp_listen_info, one per pod IP) and the call counters carry the
// address the call actually reached (actual_destination, the pod after the Service address was translated). Matching
// that pod address to a listening container names the callee. An address no container claims is External.

/** pod IP -> workload. An IP claimed by two workloads (host networking, a reused address) goes to the one that reported last; a tie is dropped. */
export function addressIndex(listen) {
  const best = new Map() // ip -> {w, last, tie}
  for (const s of listen) {
    const w = workloadOf(s.labels.container_id)
    const addr = s.labels.listen_addr
    if (!w || !addr || isLoopback(addr) || isUnspecified(addr)) continue
    const ip = hostOf(addr)
    const cur = best.get(ip)
    if (!cur || s.last > cur.last) best.set(ip, { w, last: s.last, tie: false })
    else if (s.last === cur.last && cur.w.id !== w.id) cur.tie = true
  }
  const out = new Map()
  for (const [ip, v] of best) if (!v.tie) out.set(ip, v.w)
  return out
}

/**
 * -> {nodes, edges, ignored}. An edge's error share counts a failed connect as one failed call, because a connect that
 * fails never becomes a request: a callee refusing connections would otherwise look healthy while some calls get
 * through. That is an approximation (a client that retries makes several attempts) but it keeps the failure visible.
 */
export function buildGraph({ listen = [], conn = [], fail = [], http = [], protocols = [] }) {
  const ips = addressIndex(listen)
  const sources = [
    { kind: 'conn', proto: '', series: conn },
    { kind: 'fail', proto: '', series: fail },
    { kind: 'calls', proto: 'http', series: http },
    { kind: 'errors', proto: 'http', series: http.filter((s) => isServerError(s.labels.status || '')) },
  ]
  const units = { http: HTTP_UNIT }
  for (const p of protocols) {
    units[p.name] = p.unit
    sources.push({ kind: 'calls', proto: p.name, series: p.calls })
    sources.push({ kind: 'errors', proto: p.name, series: p.calls.filter((s) => s.labels.status === FAILED_STATUS) })
    if (p.latency) sources.push({ kind: 'latency', proto: p.name, series: p.latency })
  }

  const nodes = new Map()
  const edges = new Map()
  const serves = new Map()
  const loopback = new Set()
  const node = (n) => {
    if (!nodes.has(n.id)) nodes.set(n.id, n)
    return nodes.get(n.id)
  }

  for (const { kind, proto, series } of sources) {
    for (const { labels: m, rate } of series) {
      const src = workloadOf(m.container_id)
      if (!src) continue // a host process or systemd unit, not a pod
      const dest = m.destination || ''
      const actual = m.actual_destination || ''
      // A connection to loopback stays inside one host or one pod (an app and its sidecar, a probe): not a call between
      // services. Either address being loopback is enough: the peer dialled, or the one actually reached.
      if (isLoopback(dest) || isLoopback(actual)) {
        loopback.add(`${m.container_id}|${dest}`)
        continue
      }
      const known = ips.get(hostOf(actual)) || ips.get(hostOf(dest))
      const dst = known || { id: `external:${dest}`, ns: '', name: dest, label: dest, external: true }
      if (dst.id === src.id) continue
      node({ id: src.id, ns: src.ns, name: src.name, label: src.label, external: false })
      node({ id: dst.id, ns: dst.ns, name: dst.name, label: dst.label, external: !!dst.external })
      const key = `${src.id}\n${dst.id}`
      if (!edges.has(key)) edges.set(key, { from: src.id, to: dst.id, conn: 0, fail: 0, calls: {}, latency: {} })
      const e = edges.get(key)
      if (kind === 'conn' || kind === 'fail') {
        e[kind] += rate
      } else if (kind === 'calls') {
        const c = (e.calls[proto] ||= [0, 0])
        c[0] += rate
        if (proto !== 'http' && rate > 0) {
          if (!serves.has(dst.id)) serves.set(dst.id, new Set())
          serves.get(dst.id).add(proto)
        }
      } else if (kind === 'errors') {
        const c = (e.calls[proto] ||= [0, 0])
        c[1] += rate
      } else if (kind === 'latency') {
        const bound = parseLe(m.le)
        if (Number.isNaN(bound)) continue
        const b = (e.latency[proto] ||= new Map())
        b.set(bound, (b.get(bound) || 0) + rate)
      }
    }
  }

  const out = []
  for (const e of edges.values()) {
    const active = Object.entries(e.calls).filter(([, c]) => c[0] > 0)
    const total = active.reduce((a, [, c]) => a + c[0], 0)
    const failedCalls = active.reduce((a, [, c]) => a + c[1], 0)
    let attempts
    let failures
    let basis
    if (total > 0) {
      attempts = total + e.fail
      failures = failedCalls + e.fail
      basis = (active.length === 1 && active[0][0] === 'http' ? 'http' : 'calls') + (e.fail > 0 ? '+tcp' : '')
    } else if (e.conn + e.fail > 0) {
      attempts = e.conn + e.fail
      failures = e.fail
      basis = 'tcp'
    } else {
      attempts = 0
      failures = 0
      basis = 'tcp'
    }
    const errPct = attempts > 0 ? (100 * failures) / attempts : 0
    const dominant = active.length ? active.reduce((a, b) => (b[1][0] > a[1][0] ? b : a))[0] : null
    let p95ms = null
    if (dominant && e.latency[dominant]) {
      // One protocol's histogram only: HTTP and a database have different buckets and cannot be added, so an edge that
      // mixes protocols reports the latency of the one that carries most calls.
      const q = histogramQuantile(0.95, e.latency[dominant])
      if (q !== null) p95ms = Math.round(q * 1e6) / 1e3
    }
    out.push({
      from: e.from,
      to: e.to,
      rps: total > 0 ? total : null,
      connPerSec: e.conn,
      errPct,
      failedConnects: e.fail, // per second, on its own: a connect that was refused or timed out
      callErrors: failedCalls, // per second, on its own: 5xx, gRPC server failures, failed queries
      attempts, // calls + failed connects (or connects + failed connects when no call was seen), per second
      failures, // the failed part of attempts, per second: what summing edges needs, as percentages cannot be added
      errBasis: basis,
      p95ms,
      protocol: dominant,
      protocols: active.map(([p]) => p).sort(),
      unit: active.length ? (active.length === 1 ? units[dominant] : 'calls/s') : null,
    })
  }
  for (const n of nodes.values()) n.serves = [...(serves.get(n.id) || [])].sort()
  return {
    nodes: [...nodes.values()].sort((a, b) => a.id.localeCompare(b.id)),
    edges: out.sort((a, b) => (a.from + a.to).localeCompare(b.from + b.to)),
    ignored: { loopback: loopback.size },
  }
}
