// "Service not serving": rules over two looks at the same project, a RECENT window and a longer BASELINE that ends where the
// recent one begins. Pure: it takes the two map results (see map/load.js) and returns the conditions that hold, so the same code
// can run in the browser now and in a service later.
//
// What each rule means, and what it does not:
//   no-traffic     a service that was getting calls now gets (almost) none. It may be down, or its callers may be.
//   refused        a high share of the connections into it fail (refused, reset, timed out): up but not accepting.
//   errors         a high share of the calls it answers fail with a 5xx or a gRPC server failure.
//   no-instances   it was listening during the baseline or recent window and no longer is: its pods are gone.
//   telemetry-gone ALL traffic disappeared at once. That points at the agents or the pipeline, not at every service, so it
//                  replaces the per-service "absence" rules instead of adding to them.
//
// Absence is the hard part: "no traffic" is also what a failed read looks like. A rule that depends on something being absent
// is skipped when either look was incomplete (a counter failed or was cut short), and nothing here treats a missing read as zero.

import { workloadForId } from '../map/workload.js'

export const DEFAULTS = {
  recentSeconds: 300, //          the window being judged
  baselineSeconds: 3600, //       what "normal" is, ending where the recent window starts
  minBaselineRate: 0.1, //        calls+connections per second a service must have had to be expected to have traffic
  dropRatio: 0.05, //             "no traffic" when the recent rate is below this share of the baseline's
  minFailedConnects: 5, //        failed connections in the window before a share means anything
  failedConnectShare: 20, //      percent of connection attempts failing
  criticalConnectShare: 90,
  minCalls: 20, //                calls in the window before an error share means anything
  errorShare: 5, //               percent of calls failing
  criticalErrorShare: 50,
}

/** A config with only known, positive, finite numbers; anything else keeps the value it had (`base`). */
export function cleanConfig(c, base = DEFAULTS) {
  const out = { ...DEFAULTS, ...base }
  for (const k of Object.keys(DEFAULTS)) {
    const v = c && Number(c[k])
    if (c && c[k] !== undefined && c[k] !== null && c[k] !== '' && Number.isFinite(v) && v > 0) out[k] = v
  }
  return out
}

export const RULES = {
  'no-traffic': 'Stopped receiving traffic',
  refused: 'Connections failing',
  errors: 'Calls failing',
  'no-instances': 'No running instances',
  'telemetry-gone': 'No telemetry',
}

const sum = (xs, f) => xs.reduce((a, x) => a + f(x), 0)
const activity = (e) => (e.rps ?? 0) + e.connPerSec
const round = (n, d = 3) => Math.round(n * 10 ** d) / 10 ** d
// "shop/cartservice" for a Kubernetes workload, "nginx" for a unit on a plain host, the id itself for anything else
const labelOf = (id) => (id.startsWith('/k8s/') ? id.split('/').slice(2).join('/') : (workloadForId(id) || { label: id }).label)

function inboundOf(result, id) {
  const edges = result.edges.filter((e) => e.to === id)
  const calls = sum(edges, (e) => e.rps ?? 0)
  const conn = sum(edges, (e) => e.connPerSec)
  const failedConnects = sum(edges, (e) => e.failedConnects)
  return {
    edges,
    activity: sum(edges, activity),
    calls,
    callErrors: sum(edges, (e) => e.callErrors),
    conn,
    failedConnects,
    // callers, busiest first: the first thing to look at when a service stops getting traffic
    callers: edges.map((e) => ({ id: e.from, label: labelOf(e.from), rate: round(activity(e)) })).sort((a, b) => b.rate - a.rate).slice(0, 8),
  }
}

/**
 * -> {alerts, checked, skipped}: the conditions that hold now, how many services were looked at, and what could not be judged (and why).
 * An alert is {id, rule, service, label, severity, summary, evidence}; `id` is stable across evaluations.
 */
export function evaluate({ recent, baseline }, config = {}) {
  const c = { ...DEFAULTS, ...config }
  const alerts = []
  const skipped = []
  const add = (rule, service, severity, summary, evidence) =>
    alerts.push({ id: `${rule}|${service}`, rule, service, label: service === '*' ? 'project' : labelOf(service), severity, summary, evidence })

  const complete = !recent.incomplete && !baseline.incomplete
  if (!complete) skipped.push('Rules that depend on something being absent were not judged: a counter could not be read in full.')

  // Everything vanished at once: the agents or the pipeline, not the services.
  const baselineTraffic = baseline.edges.length > 0
  const gone = baselineTraffic && recent.edges.length === 0 && complete
  if (gone) {
    add('telemetry-gone', '*', 'critical', 'No calls between services were recorded in the last ' + Math.round(c.recentSeconds / 60) + ' minutes, though there were before. The node-agents or the ingest path may have stopped; per-service absence alerts are suppressed.', {
      baselineEdges: baseline.edges.length,
      recentEdges: 0,
    })
  }

  const ids = new Set()
  for (const r of [recent, baseline]) {
    for (const n of r.nodes) if (!n.external) ids.add(n.id)
    for (const id of r.listeners ? r.listeners.seen : []) ids.add(id)
  }

  for (const id of [...ids].sort()) {
    const now = inboundOf(recent, id)
    const before = inboundOf(baseline, id)

    // up but failing
    const attempts = now.conn + now.failedConnects
    const failedShare = attempts > 0 ? (100 * now.failedConnects) / attempts : 0
    if (now.failedConnects * c.recentSeconds >= c.minFailedConnects && failedShare >= c.failedConnectShare) {
      add('refused', id, failedShare >= c.criticalConnectShare ? 'critical' : 'warning',
        `${round(failedShare, 1)}% of connections into ${labelOf(id)} are failing (${round(now.failedConnects * c.recentSeconds, 0)} failed in ${Math.round(c.recentSeconds / 60)} min).`,
        { failedConnectsPerSec: round(now.failedConnects), connectionsPerSec: round(now.conn), failedSharePct: round(failedShare, 1), callers: now.callers })
    }
    const errShare = now.calls > 0 ? (100 * now.callErrors) / now.calls : 0
    if (now.calls * c.recentSeconds >= c.minCalls && errShare >= c.errorShare) {
      add('errors', id, errShare >= c.criticalErrorShare ? 'critical' : 'warning',
        `${round(errShare, 1)}% of the calls ${labelOf(id)} answers are failing (${round(now.callErrors * c.recentSeconds, 0)} of ${round(now.calls * c.recentSeconds, 0)} in ${Math.round(c.recentSeconds / 60)} min).`,
        { callsPerSec: round(now.calls), errorsPerSec: round(now.callErrors), errorSharePct: round(errShare, 1), callers: now.callers })
    }

    // gone: needs both looks to be complete
    if (!complete || gone) continue
    let noInstances = false
    if (recent.listeners && baseline.listeners) {
      const was = recent.listeners.seen.includes(id) || baseline.listeners.seen.includes(id)
      if (was && !recent.listeners.fresh.includes(id)) {
        noInstances = true
        add('no-instances', id, 'critical', `${labelOf(id)} was listening and no instance has reported a listen address for the last ${Math.round(c.recentSeconds / 60)} minutes: its pods may be gone.`, {
          lastTrafficPerSec: round(before.activity),
          baselineCallers: before.callers,
        })
      }
    } else {
      skipped.push(`Instance presence was not judged for ${labelOf(id)}: the project has no listen-address metric in one of the windows.`)
    }
    if (noInstances) continue // the traffic stopping is the symptom of the instances being gone, not a second problem
    if (before.activity >= c.minBaselineRate && now.activity < before.activity * c.dropRatio) {
      add('no-traffic', id, now.activity === 0 ? 'critical' : 'warning',
        `${labelOf(id)} stopped receiving traffic: ${round(now.activity)}/s now, ${round(before.activity)}/s before. It may be down, or its callers may be.`,
        { recentPerSec: round(now.activity), baselinePerSec: round(before.activity), baselineCallers: before.callers, recentCallers: now.callers })
    }
  }
  // listen-metric skips repeat per service; one line says it
  return { alerts, checked: ids.size, skipped: [...new Set(skipped.map((s) => (s.startsWith('Instance presence') ? 'Instance presence was not judged: the project has no listen-address metric.' : s)))] }
}
