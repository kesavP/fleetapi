// Root cause analysis of one incident. Pure.
//
// Input: two looks at the same project, BEFORE the incident (what normal was) and DURING it, each {graph, apps, inbound}:
//   graph    the service map of that window (map/load.js), whose edges carry call rates, error share, p95, connect time and round trip;
//   apps     the applications of that window (api/applications.js): CPU, limits, restarts, out-of-memory kills, error log lines;
//   inbound  what each application served (incidents/inbound.js): requests per second, error share, p95.
// Output: the FINDINGS: what changed (issues), where (a graph of the affected applications and the dependencies that carry the damage),
// and the most likely cause, with the facts it rests on. The words are made from this by narrative.js; nothing here is prose, so the
// same findings can be shown, tested, or handed to something else to explain (a language model, later).
//
// How the cause is chosen. Trouble travels UP the call chain: a slow database slows everything that calls it. So among the things that
// changed, the one furthest from the application that was hurt, along the calls it makes, is the likely origin, unless something nearer
// explains it. In order of how directly they point at a cause:
//   1. a call whose network round trip or connection time rose: the path between two applications is the problem;
//   2. a resource problem inside an application (CPU at its limit, restarts, out-of-memory kills) that no slower callee explains;
//   3. a call that got slower or started failing, whose callee has no such trouble further down.
// Where nothing is found it says so, and says what was checked: "no cause found" is an answer, a guessed cause is not.

import { workloadForId } from '../map/workload.js'

export const ANALYSIS_DEFAULTS = {
  minRps: 0.5, //       calls a second an edge must have had in the incident to be judged
  rttRatio: 3, //       times larger than before
  rttMinRiseMs: 2, //   and at least this much larger, so 0.1 ms -> 0.4 ms is not an event
  connRatio: 3,
  connMinRiseMs: 5,
  latencyRatio: 2,
  latencyMinRiseMs: 50,
  errorPct: 5, //       share of calls failing, and
  errorMinRisePct: 3, //  at least this much more than before
  cpuShareOfLimit: 0.9, // at its limit now ...
  cpuWasBelow: 0.8, //    ... and was not before
  cpuRatio: 2, //       with no limit: at least this many times as many cores,
  cpuMinRiseCores: 0.2, // and this many more
  logRatio: 3, //       error log lines a second, times as many as before
  logMinLines: 5, //    and at least this many in the incident
}

const nameOf = (id) => {
  const w = workloadForId(id)
  return w ? w.name : id
}
const labelOf = (id) => {
  const w = workloadForId(id)
  return w ? w.label : id
}
const round = (n, d = 1) => (n == null ? null : Math.round(n * 10 ** d) / 10 ** d)
const ratio = (now, was) => (was > 0 ? now / was : null)
const key = (from, to) => `${from}\n${to}`

/** Issues in an application itself. `before` and `during` are snapshots; `seconds` the length of each window, for rates of counts. */
function appIssues(id, before, during, c, seconds) {
  const out = []
  const issue = (kind, severity, b, d, extra = {}) => out.push({ scope: 'app', app: id, kind, severity, before: b, during: d, ratio: ratio(d, b), ...extra })
  const inb = during.inbound.get(id)
  const inb0 = before.inbound.get(id)
  if (inb && inb.errPct >= c.errorPct && inb.errPct >= (inb0 ? inb0.errPct : 0) + c.errorMinRisePct) {
    issue('errors', inb.errPct >= 50 ? 'critical' : 'warning', round(inb0 ? inb0.errPct : 0), round(inb.errPct), { unit: '%' })
  }
  if (inb && inb.p95ms != null && inb0 && inb0.p95ms != null && inb.p95ms >= inb0.p95ms * c.latencyRatio && inb.p95ms - inb0.p95ms >= c.latencyMinRiseMs) {
    issue('latency', 'warning', round(inb0.p95ms), round(inb.p95ms), { unit: 'ms' })
  }
  const a = during.apps.get(id)
  const a0 = before.apps.get(id)
  if (a && a.cpuCores != null) {
    const share = a.cpuLimit ? a.cpuCores / a.cpuLimit : null
    const share0 = a0 && a0.cpuLimit && a0.cpuCores != null ? a0.cpuCores / a0.cpuLimit : null
    if (share != null) {
      if (share >= c.cpuShareOfLimit && (share0 == null || share0 < c.cpuWasBelow)) issue('cpu', 'warning', round((share0 ?? 0) * 100, 0), round(share * 100, 0), { unit: '% of its limit' })
    } else if (a0 && a0.cpuCores > 0 && a.cpuCores >= a0.cpuCores * c.cpuRatio && a.cpuCores - a0.cpuCores >= c.cpuMinRiseCores) {
      issue('cpu', 'warning', round(a0.cpuCores, 2), round(a.cpuCores, 2), { unit: 'cores' })
    }
  }
  if (a && a.errorLines >= c.logMinLines && seconds.during > 0) {
    const was = a0 && seconds.before > 0 ? a0.errorLines / seconds.before : 0
    const now = a.errorLines / seconds.during
    if (now >= was * c.logRatio) issue('logs', 'warning', a0 ? a0.errorLines : 0, a.errorLines, { unit: 'error lines' })
  }
  if (a && a.restarts > 0 && !(a0 && a0.restarts > 0)) issue('restarts', 'warning', 0, a.restarts, { unit: 'restarts' })
  if (a && a.oomKills > 0 && !(a0 && a0.oomKills > 0)) issue('oom', 'critical', 0, a.oomKills, { unit: 'out-of-memory kills' })
  return out
}

/** Issues on a call from one application to another. */
function edgeIssues(e, e0, c) {
  const out = []
  const issue = (kind, severity, b, d, unit) => out.push({ scope: 'edge', from: e.from, to: e.to, kind, severity, before: b, during: d, ratio: ratio(d, b), unit })
  if (!((e.rps ?? 0) + e.connPerSec >= c.minRps)) return out
  if (e.rttMs != null && e0 && e0.rttMs != null && e.rttMs >= e0.rttMs * c.rttRatio && e.rttMs - e0.rttMs >= c.rttMinRiseMs) issue('rtt', 'critical', e0.rttMs, e.rttMs, 'ms')
  if (e.connMs != null && e0 && e0.connMs != null && e.connMs >= e0.connMs * c.connRatio && e.connMs - e0.connMs >= c.connMinRiseMs) issue('connect', 'critical', e0.connMs, e.connMs, 'ms')
  if (e.p95ms != null && e0 && e0.p95ms != null && e.p95ms >= e0.p95ms * c.latencyRatio && e.p95ms - e0.p95ms >= c.latencyMinRiseMs) issue('latency', 'warning', round(e0.p95ms), round(e.p95ms), 'ms')
  if (e.errPct >= c.errorPct && e.errPct >= (e0 ? e0.errPct : 0) + c.errorMinRisePct) issue('errors', e.errPct >= 50 ? 'critical' : 'warning', round(e0 ? e0.errPct : 0), round(e.errPct), '%')
  return out
}

/** Shortest list of application ids from `from` to `to` along `edges` ([{from, to}]); null if none. */
export function pathBetween(edges, from, to) {
  if (from === to) return [from]
  const next = new Map()
  for (const e of edges) {
    if (!next.has(e.from)) next.set(e.from, [])
    next.get(e.from).push(e.to)
  }
  const prev = new Map([[from, null]])
  const queue = [from]
  while (queue.length) {
    const at = queue.shift()
    for (const n of next.get(at) || []) {
      if (prev.has(n)) continue
      prev.set(n, at)
      if (n === to) {
        const path = [n]
        for (let p = prev.get(n); p !== null; p = prev.get(p)) path.unshift(p)
        return path
      }
      queue.push(n)
    }
  }
  return null
}

/** How far each application is from `app` along calls: Map(id -> number of calls away). */
function depthsFrom(edges, app) {
  const next = new Map()
  for (const e of edges) {
    if (!next.has(e.from)) next.set(e.from, [])
    next.get(e.from).push(e.to)
  }
  const depth = new Map([[app, 0]])
  const queue = [app]
  while (queue.length) {
    const at = queue.shift()
    for (const n of next.get(at) || []) {
      if (!depth.has(n)) {
        depth.set(n, depth.get(at) + 1)
        queue.push(n)
      }
    }
  }
  return depth
}

/**
 * -> findings {app, issues, nodes, edges, rootCause, checked}.
 *   issues      every change found, apps' and edges' (see appIssues/edgeIssues);
 *   nodes/edges the affected part of the map: the hurt application, whatever it depends on that changed, and the calls between;
 *   rootCause   null or {kind, scope, app | from/to, issue, path, via: [...]} (see above); `confidence` is "likely" for a network or
 *               resource cause and "possible" for a call that merely got slower;
 *   checked     what was looked at, so that "nothing found" can be told from "nothing looked at".
 */
export function analyze({ app, before, during, seconds = { before: 3600, during: 600 }, config = {} }) {
  const c = { ...ANALYSIS_DEFAULTS, ...config }
  const edges0 = new Map(before.graph.edges.map((e) => [key(e.from, e.to), e]))
  const downstream = depthsFrom(during.graph.edges, app)
  const reachable = new Set(downstream.keys())

  let issues = []
  const ids = new Set([...during.apps.keys(), ...during.inbound.keys()])
  for (const id of ids) {
    if (reachable.has(id)) issues.push(...appIssues(id, before, during, c, seconds))
  }
  for (const e of during.graph.edges) {
    if (reachable.has(e.from)) issues.push(...edgeIssues(e, edges0.get(key(e.from, e.to)), c))
  }

  // checks the user has turned off (config.disabled: kinds of issue) are not part of the analysis at all, so they can neither show nor be a cause
  const off = new Set(c.disabled || [])
  if (off.size) issues = issues.filter((i) => !off.has(i.kind))

  // the affected part of the map: the hurt application, every application that has an issue (or calls or is called over an edge that
  // has one), and whatever lies on a call path between the hurt application and them
  const holders = new Set([app])
  for (const i of issues) {
    if (i.scope === 'app') holders.add(i.app)
    else {
      holders.add(i.from)
      holders.add(i.to)
    }
  }
  const reverse = new Map()
  for (const e of during.graph.edges) {
    if (!reachable.has(e.from)) continue
    if (!reverse.has(e.to)) reverse.set(e.to, [])
    reverse.get(e.to).push(e.from)
  }
  const keep = new Set()
  const stack = [...holders].filter((h) => reachable.has(h))
  while (stack.length) {
    const at = stack.pop()
    if (keep.has(at)) continue
    keep.add(at)
    for (const p of reverse.get(at) || []) stack.push(p)
  }
  keep.add(app)
  const shownEdges = during.graph.edges.filter((e) => keep.has(e.from) && keep.has(e.to) && reachable.has(e.from))
  const nodes = [...keep].map((id) => ({
    id,
    name: nameOf(id),
    label: labelOf(id),
    depth: downstream.get(id) ?? 0,
    // what is wrong with this node, in the reference's way of listing it: its own issues, then those of the calls it makes
    issues: issues.filter((i) => (i.scope === 'app' && i.app === id) || (i.scope === 'edge' && i.from === id)),
  }))
  const edgesOut = shownEdges.map((e) => ({ from: e.from, to: e.to, rps: e.rps, errPct: e.errPct, p95ms: e.p95ms, issues: issues.filter((i) => i.scope === 'edge' && i.from === e.from && i.to === e.to) }))

  const rootCause = chooseRootCause({ app, issues, edges: shownEdges, downstream })
  return {
    app,
    issues,
    nodes: nodes.sort((a, b) => a.depth - b.depth || a.id.localeCompare(b.id)),
    edges: edgesOut,
    rootCause,
    checked: {
      applications: reachable.size, // the hurt application and everything it calls, however far down
      calls: during.graph.edges.filter((e) => reachable.has(e.from)).length,
      signals: ['request errors', 'request latency', 'CPU', 'restarts and out-of-memory kills', 'error logs', 'call errors and latency', 'network round trip', 'connection time'],
    },
  }
}

function chooseRootCause({ app, issues, edges, downstream }) {
  const depthOf = (id) => downstream.get(id) ?? 0
  const trouble = (id) => edges.some((e) => e.from === id && issues.some((i) => i.scope === 'edge' && i.from === e.from && i.to === e.to && i.kind !== 'errors'))
  const rank = { critical: 2, warning: 1 }
  const best = (list, depthFn) => [...list].sort((a, b) => depthFn(b) - depthFn(a) || (rank[b.severity] || 0) - (rank[a.severity] || 0) || (b.ratio || 0) - (a.ratio || 0) || (a.from || a.app).localeCompare(b.from || b.app))[0]
  const result = (i, confidence) => {
    const target = i.scope === 'edge' ? i.to : i.app
    return {
      kind: i.kind,
      scope: i.scope,
      ...(i.scope === 'edge' ? { from: i.from, to: i.to } : { app: i.app }),
      issue: i,
      confidence,
      path: pathBetween(edges, app, target) || [app, target].filter((x, n, a) => a.indexOf(x) === n),
    }
  }

  // 1. the path between two applications: the connection itself is slow
  const net = issues.filter((i) => i.scope === 'edge' && (i.kind === 'rtt' || i.kind === 'connect'))
  if (net.length) return result(best(net, (i) => depthOf(i.to)), 'likely')

  // 2. a resource problem inside an application whose own callees are not slow
  const res = issues.filter((i) => i.scope === 'app' && ['cpu', 'oom', 'restarts'].includes(i.kind) && !trouble(i.app))
  if (res.length) return result(best(res, (i) => depthOf(i.app)), 'likely')

  // 3. the deepest call that got slower or failing: its callee has nothing below it that explains it
  const calls = issues.filter((i) => i.scope === 'edge' && (i.kind === 'latency' || i.kind === 'errors') && !edges.some((e) => e.from === i.to && issues.some((j) => j.scope === 'edge' && j.from === e.from && j.to === e.to)))
  if (calls.length) return result(best(calls, (i) => depthOf(i.to)), 'possible')

  // an application that logs errors or fails on its own, with nothing below it: it is the cause of itself
  const own = issues.filter((i) => i.scope === 'app' && (i.kind === 'logs' || i.kind === 'errors' || i.kind === 'latency') && i.app !== app && !trouble(i.app))
  if (own.length) return result(best(own, (i) => depthOf(i.app)), 'possible')
  return null
}
