// What each application serves, from the node-agent's INBOUND HTTP metrics: how many requests, how many fail, how many are slow.
// This is the server's own view of its users, the thing a service level objective is about. Two shapes of the same data:
//   - a snapshot over a window (inboundSnapshot): requests per second, error share, p95: for comparing a bad window with a good one;
//   - a timeline per step (inboundTimeline): totals, failures and slow requests at each step, for finding when an objective was missed.
// Pure functions over the series the read API returns; loadInbound* are the only ones that call it.

import { histogramQuantile, isServerError, parseLe, workloadOf } from '../map/workload.js'

export const REQUESTS = 'container_http_inbound_requests_total'
export const LATENCY = 'container_http_inbound_requests_duration_seconds_total_bucket'

const appOf = (labels) => workloadOf(labels && labels.container_id)

/**
 * -> Map(application id -> {id, label, rps, errPct, p95ms}). `reqs` is [{labels:{container_id, status}, rate}] and `buckets` is
 * [{labels:{container_id, le}, rate}] (cumulative per-second rates). p95ms is null when there is no histogram for the application.
 */
export function inboundSnapshot({ reqs = [], buckets = [] }) {
  const apps = new Map()
  const get = (w) => {
    if (!apps.has(w.id)) apps.set(w.id, { id: w.id, label: w.label, rps: 0, failed: 0, hist: new Map() })
    return apps.get(w.id)
  }
  for (const { labels, rate } of reqs) {
    const w = appOf(labels)
    if (!w || !(rate > 0)) continue
    const a = get(w)
    a.rps += rate
    if (isServerError(labels.status)) a.failed += rate
  }
  for (const { labels, rate } of buckets) {
    const w = appOf(labels)
    const le = parseLe(labels.le)
    if (!w || Number.isNaN(le) || !(rate >= 0)) continue
    const h = get(w).hist
    h.set(le, (h.get(le) || 0) + rate)
  }
  const out = new Map()
  for (const a of apps.values()) {
    if (!(a.rps > 0)) continue
    const q = histogramQuantile(0.95, a.hist)
    out.set(a.id, { id: a.id, label: a.label, rps: a.rps, errPct: (100 * a.failed) / a.rps, p95ms: q === null ? null : Math.round(q * 1e6) / 1e3 })
  }
  return out
}

/**
 * Per application, per step: {total, errors, slow} arrays of requests per second. `slow` is what took longer than `thresholdSeconds`,
 * read from the smallest histogram bound at or above it (so a threshold between two bounds is judged by the larger: stated in `bound`).
 * `series` are raw query_range series: [{labels, points:[{t (ms), v}]}], v being a per-second rate; a step with no point had no requests.
 * -> {start, step, n, apps: Map(id -> {id, label, total[], errors[], slow[] | null}), bound}; `slow` is null when no bound is at or above
 * the threshold, since then nothing can be said about it.
 */
export function inboundTimeline({ reqs = [], buckets = [] }, { start, step, n, thresholdSeconds = 0.5 }) {
  const slot = (t) => Math.floor((t / 1000 - start) / step)
  const zeros = () => new Array(n).fill(0)
  const apps = new Map()
  const get = (w) => {
    if (!apps.has(w.id)) apps.set(w.id, { id: w.id, label: w.label, total: zeros(), errors: zeros(), le: new Map() })
    return apps.get(w.id)
  }
  const add = (arr, s) => {
    for (const p of s.points || []) {
      const i = slot(p.t)
      if (i >= 0 && i < n && p.v > 0) arr[i] += p.v
    }
  }
  for (const s of reqs) {
    const w = appOf(s.labels)
    if (!w) continue
    const a = get(w)
    add(a.total, s)
    if (isServerError(s.labels.status)) add(a.errors, s)
  }
  let bound = null
  for (const s of buckets) {
    const le = parseLe(s.labels && s.labels.le)
    if (Number.isNaN(le) || le === Infinity || le < thresholdSeconds) continue
    if (bound === null || le < bound) bound = le
  }
  for (const s of buckets) {
    const w = appOf(s.labels)
    const le = parseLe(s.labels && s.labels.le)
    if (!w || le !== bound) continue
    add((get(w).fast ||= zeros()), s)
  }
  const out = new Map()
  for (const a of apps.values()) {
    const slow = bound !== null && a.fast ? a.total.map((t, i) => Math.max(0, t - a.fast[i])) : null
    out.set(a.id, { id: a.id, label: a.label, total: a.total, errors: a.errors, slow })
  }
  return { start, step, n, apps: out, bound }
}

const stepOf = (start, end) => Math.max(15, Math.floor((end - start) / 40))
async function range(client, name, start, end, step) {
  const data = (await client.get('metrics/query_range', { name, from: start.toFixed(3), to: end.toFixed(3), step, rate: true })) || {}
  return { series: data.series || [], truncated: !!data.truncated }
}

/** The snapshot of the window [start, end] (epoch seconds), from the read API. A metric the project does not have gives an empty answer. */
export async function loadInbound(client, start, end) {
  const present = new Set((await client.get('metrics')) || [])
  const step = stepOf(start, end)
  const buckets_ = (n) => (present.has(n) ? range(client, n, start, end, step) : { series: [], truncated: false })
  const [r, b] = await Promise.all([buckets_(REQUESTS), buckets_(LATENCY)])
  const buckets = (s) => {
    const n = Math.max(Math.floor(end / step) - Math.floor(start / step), 1)
    return s.series.map((x) => ({ labels: x.labels || {}, rate: (x.points || []).reduce((a, p) => a + p.v, 0) / n }))
  }
  return { apps: inboundSnapshot({ reqs: buckets(r), buckets: buckets(b) }), truncated: r.truncated || b.truncated, has: present.has(REQUESTS) }
}

/** The timeline of [start, end] at `step` seconds. */
export async function loadInboundTimeline(client, start, end, { step = 60, thresholdSeconds = 0.5 } = {}) {
  const present = new Set((await client.get('metrics')) || [])
  const none = { series: [], truncated: false }
  const [r, b] = await Promise.all([present.has(REQUESTS) ? range(client, REQUESTS, start, end, step) : none, present.has(LATENCY) ? range(client, LATENCY, start, end, step) : none])
  const n = Math.max(Math.floor((end - start) / step), 1)
  return { ...inboundTimeline({ reqs: r.series, buckets: b.series }, { start, step, n, thresholdSeconds }), truncated: r.truncated || b.truncated, has: present.has(REQUESTS) }
}
