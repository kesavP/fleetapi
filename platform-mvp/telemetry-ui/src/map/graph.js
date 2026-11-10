import { FAILED_STATUS, HTTP_UNIT, histogramQuantile, hostOf, isLoopback, isServerError, isUnspecified, parseLe, portOf, workloadOf } from './workload.js'

// Builds the service map from the agents' counters. Input series are {labels, rate} (mean per-second rate over the
// window); `listen` is {labels, last} from the listen-address gauge, `last` being when it was last reported (ms).
//
// There is no Kubernetes API here, so a destination is recognised the way the agent itself exposes it: every container
// reports the addresses it listens on (container_net_tcp_listen_info, one per pod IP) and the call counters carry the
// address the call actually reached (actual_destination, the pod after the Service address was translated). Matching
// that pod address to a listening container names the callee. An address no container claims is External.
//
// One case the agent cannot help with: a connection that FAILS carries only the address that was dialled (the Service's cluster
// IP), never the pod it would have reached, because there was no connection to look up. So a refused or timed-out connect to a
// Service would show as an external endpoint, which is exactly when the callee matters most. The successful connections tell us
// what such an address leads to (each pairs "dialled" with "reached", and the reached pod is a known application), so the map
// learns that table from them (learnServiceTable) and uses it for the connections that failed.

// How long an address's listeners are considered current, for deciding whether an IP belongs to ONE pod: a pod IP that was reused after
// churn has an old pod that stopped reporting and a new one that did not, and only the new one counts. Listeners that reported within this
// of the freshest are all current.
const CURRENT_MS = 2 * 60 * 1000

function vote(map, key, w, last) {
  const cur = map.get(key)
  if (!cur || last > cur.last) map.set(key, { w, last, tie: false })
  else if (last === cur.last && cur.w.id !== w.id) cur.tie = true
}

/**
 * What a listening address belongs to, as the node-agent reports it (container_net_tcp_listen_info). Three ways to resolve an address, the
 * way Coroot's server does it, strongest first:
 *   1. the exact "ip:port": a service listening there. This is the only way that works on a host, where one IP carries many services. An
 *      address two applications claim at the same time is ambiguous and resolves to nothing, never to a guess; one claimed again later (a
 *      pod IP reused) goes to the latest.
 *   2. the IP alone, but only for Kubernetes pods, where an IP is one pod's and any port on it is that pod. Not for a host: a connection to
 *      some other port of a machine is not evidence about whichever service happens to listen on one port.
 *   3. a loopback address, for a process on a plain host: 127.0.0.1:5432 on machine A is the service that listens there on machine A, and
 *      on no other. (Inside a pod, loopback stays inside the pod and is not a call.)
 * Returns {resolve(addr), resolveLocal(machineId, addr), size}.
 */
export function addressIndex(listen) {
  const exact = new Map() // "ip:port" -> {w, last, tie}
  const local = new Map() // "machine|port" -> {w, last, tie}: listeners on loopback
  const byIp = new Map() // ip -> Map(application id -> {w, last}): pods only
  for (const s of listen) {
    const w = workloadOf(s.labels.container_id)
    const addr = s.labels.listen_addr
    if (!w || !addr || isUnspecified(addr)) continue
    const port = portOf(addr)
    if (isLoopback(addr)) {
      if (w.host && port && s.labels.machine_id) vote(local, `${s.labels.machine_id}|${port}`, w, s.last)
      continue
    }
    const ip = hostOf(addr).toLowerCase()
    if (port) vote(exact, `${ip}:${port}`, w, s.last)
    if (!w.host) {
      if (!byIp.has(ip)) byIp.set(ip, new Map())
      const cur = byIp.get(ip).get(w.id)
      if (!cur || s.last > cur.last) byIp.get(ip).set(w.id, { w, last: s.last })
    }
  }
  const ipOnly = new Map() // ip -> the one pod application that is current on it
  for (const [ip, by] of byIp) {
    const newest = Math.max(...[...by.values()].map((v) => v.last))
    const current = [...by.values()].filter((v) => v.last >= newest - CURRENT_MS)
    if (current.length === 1) ipOnly.set(ip, current[0].w)
  }
  return {
    size: exact.size + ipOnly.size,
    resolve(addr) {
      const ip = hostOf(addr).toLowerCase()
      const port = portOf(addr)
      const e = port ? exact.get(`${ip}:${port}`) : null
      if (e) return e.tie ? null : e.w // listed, and either known or ambiguous: the IP does not get a say
      return ipOnly.get(ip) || null
    },
    resolveLocal(machineId, addr) {
      const e = local.get(`${machineId}|${portOf(addr)}`)
      return e && !e.tie ? e.w : null
    },
  }
}

/**
 * What each Service address (as dialled, "ip:port") leads to, learned from successful connections: every series that carries both the
 * dialled and the reached address, where the reached pod is a known application, is one vote for that application, weighted by its rate.
 * Returns {table: Map(dialled -> workload), ambiguous: [{destination, workloads}]}.
 *
 * An address whose votes go to more than one application (a Service whose selector matches two workloads) goes to the one with the most
 * traffic, ties by name so the answer does not depend on the order of the data, and is listed as ambiguous so it can be said.
 * Nothing is learned from an address that was not translated (dialled equals reached: a pod called directly), from loopback, or from a
 * reached pod no container claims. And only the exact "ip:port" is learned: a Service's other ports may lead somewhere else.
 */
export function learnServiceTable(series, ips) {
  const votes = new Map() // dialled -> Map(workload id -> {w, weight})
  for (const { labels: m, rate } of series) {
    const dest = m.destination
    const actual = m.actual_destination
    if (!dest || !actual || dest === actual) continue
    if (isLoopback(dest) || isLoopback(actual)) continue
    const w = ips.resolve(actual)
    if (!w) continue
    if (!votes.has(dest)) votes.set(dest, new Map())
    const by = votes.get(dest)
    const v = by.get(w.id) || { w, weight: 0 }
    v.weight += Math.max(rate || 0, 0) + 1e-9 // a series that exists is a vote even at a rate that rounds to nothing
    by.set(w.id, v)
  }
  const table = new Map()
  const ambiguous = []
  for (const [dest, by] of [...votes].sort(([a], [b]) => a.localeCompare(b))) {
    const ranked = [...by.values()].sort((a, b) => b.weight - a.weight || a.w.id.localeCompare(b.w.id))
    table.set(dest, ranked[0].w)
    if (ranked.length > 1) ambiguous.push({ destination: dest, workloads: ranked.map((v) => v.w.id) })
  }
  return { table, ambiguous }
}

/**
 * -> {nodes, edges, ignored, learned}. An edge's error share counts a failed connect as one failed call, because a connect that
 * fails never becomes a request: a callee refusing connections would otherwise look healthy while some calls get
 * through. That is an approximation (a client that retries makes several attempts) but it keeps the failure visible.
 */
export function buildGraph({ listen = [], conn = [], fail = [], http = [], protocols = [], learn = [], connTime = [], rtt = [] }) {
  const ips = addressIndex(listen)
  // `learn` is more successful connections, from a longer look than the window: a Service that has been failing for the whole window
  // has no successful connection in it to learn from, but it did have before.
  const { table: services, ambiguous } = learnServiceTable([...conn, ...http, ...protocols.flatMap((p) => p.calls), ...learn], ips)
  const viaService = new Set() // the Service addresses that named a callee in this map
  const sources = [
    { kind: 'conn', proto: '', series: conn },
    { kind: 'fail', proto: '', series: fail },
    { kind: 'conntime', proto: '', series: connTime },
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
      let known = null
      if (isLoopback(dest) || isLoopback(actual)) {
        // Inside a pod, loopback stays in the pod (an app and its sidecar, a probe): not a call between services. On a plain host, a process
        // may be talking to another process on the same machine (a web server and its database): that is a call, if the machine has a
        // service listening there. Either address being loopback is enough: the peer dialled, or the one actually reached.
        if (src.host && m.machine_id) known = ips.resolveLocal(m.machine_id, actual && isLoopback(actual) ? actual : dest)
        if (!known) {
          loopback.add(`${m.container_id}|${dest}`)
          continue
        }
      } else {
        known = ips.resolve(actual) || ips.resolve(dest)
        if (!known && services.has(dest)) {
          known = services.get(dest) // a Service address the agent could not translate, as on a failed connect
          viaService.add(dest)
        }
      }
      const dst = known || { id: `external:${dest}`, ns: '', name: dest, label: dest, external: true }
      if (dst.id === src.id) continue
      node({ id: src.id, ns: src.ns, name: src.name, label: src.label, external: false })
      node({ id: dst.id, ns: dst.ns, name: dst.name, label: dst.label, external: !!dst.external })
      const key = `${src.id}\n${dst.id}`
      if (!edges.has(key)) edges.set(key, { from: src.id, to: dst.id, conn: 0, fail: 0, connTime: 0, rttSum: 0, rttN: 0, calls: {}, latency: {} })
      const e = edges.get(key)
      if (kind === 'conn' || kind === 'fail' || kind === 'conntime') {
        e[kind === 'conntime' ? 'connTime' : kind] += rate
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

  // Round-trip time is reported per remote IP, not per port: it can only be given to an edge whose callee is a pod (an IP that is one
  // application's), and only to an edge the calls above already made, so a reading never creates a connection that was not seen.
  for (const { labels: m, value } of rtt) {
    const src = workloadOf(m.container_id)
    const dst = m.destination_ip ? ips.resolve(m.destination_ip) : null
    const e = src && dst ? edges.get(`${src.id}\n${dst.id}`) : null
    if (e && Number.isFinite(value) && value >= 0) {
      e.rttSum += value
      e.rttN++
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
      // Mean time to open a connection, and mean round trip to the callee (milliseconds); null when the agent did not report them.
      connMs: e.conn > 0 && e.connTime > 0 ? Math.round((e.connTime / e.conn) * 1e5) / 100 : null,
      rttMs: e.rttN ? Math.round((e.rttSum / e.rttN) * 1e5) / 100 : null,
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
    // services: addresses learned; used: of those, how many named a callee here; ambiguous: addresses that lead to several applications
    learned: { services: services.size, used: viaService.size, ambiguous: ambiguous.filter((a) => viaService.has(a.destination)) },
  }
}
