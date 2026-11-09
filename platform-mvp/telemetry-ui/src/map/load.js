import { buildGraph } from './graph.js'
import { L7_PROTOCOLS, workloadOf } from './workload.js'

const CONNECTS = 'container_net_tcp_successful_connects_total'
const FAILED_CONNECTS = 'container_net_tcp_failed_connects_total'
const HTTP_REQUESTS = 'container_http_requests_total'
const LISTEN = 'container_net_tcp_listen_info'

/**
 * Mean per-second rate of each series of `metric` between two epoch times (seconds). The service computes a rate per
 * bucket with counter resets handled, and returns a point only for a bucket in which the series had readings. The rate
 * over the range is the total of those points divided by ALL the buckets of the range, so a quiet stretch counts as zero
 * traffic. Averaging only the points that exist skips every quiet moment, and counts a restart twice (the pod IP is a
 * label, so the new pod is a new series, each averaged over its own short life and the two added together): an outage
 * would read as busier than normal. from_zero counts a counter's first appearance as an increase (the agent creates
 * these counters when the first event happens, so their first value IS that event), only for series born in the range.
 * Returns {series: [{labels, rate}], truncated}.
 */
export async function rates(client, metric, start, end) {
  // About 40 buckets however long the range, so a busy cluster stays within the service's per-call point limit.
  const step = Math.max(15, Math.floor((end - start) / 40))
  const from = Math.round(start * 1000) / 1000
  const to = Math.round(end * 1000) / 1000
  // The service buckets on the epoch grid and a rate needs a previous reading, so the first bucket has no point.
  const buckets = Math.max(Math.floor(to / step) - Math.floor(from / step), 1)
  const data =
    (await client.get('metrics/query_range', { name: metric, from: from.toFixed(3), to: to.toFixed(3), step, rate: true, from_zero: true })) || {}
  const series = []
  for (const s of data.series || []) {
    const pts = (s.points || []).map((p) => p.v)
    if (pts.length) series.push({ labels: s.labels || {}, rate: pts.reduce((a, b) => a + b, 0) / buckets })
  }
  return { series, truncated: !!data.truncated }
}

/** The addresses each container listened on during the range, with when each was last reported (ms). */
export async function listenAddresses(client, start, end) {
  const step = Math.max(15, Math.floor((end - start) / 40))
  const data = (await client.get('metrics/query_range', { name: LISTEN, from: start.toFixed(3), to: end.toFixed(3), step })) || {}
  const series = []
  for (const s of data.series || []) {
    const times = (s.points || []).map((p) => p.t)
    if (times.length) series.push({ labels: s.labels || {}, last: Math.max(...times) })
  }
  return { series, truncated: !!data.truncated }
}

/**
 * (`present`, the set of metric names, can be passed in by a caller that has already fetched it.)
 * Loads the counters for the last `seconds` seconds and builds the graph. A counter that cannot be read leaves the others
 * and adds a warning; if the metric list itself or every counter fails, that is an error (usually a bad key).
 */
export async function loadMap(client, { seconds, now = () => Date.now(), present: known } = {}) {
  const end = now() / 1000
  const start = end - seconds
  const present = known || new Set((await client.get('metrics')) || [])

  const jobs = [
    { name: LISTEN, run: () => listenAddresses(client, start, end), into: 'listen' },
    { name: CONNECTS, run: () => rates(client, CONNECTS, start, end), into: 'conn' },
    { name: FAILED_CONNECTS, run: () => rates(client, FAILED_CONNECTS, start, end), into: 'fail' },
    { name: HTTP_REQUESTS, run: () => rates(client, HTTP_REQUESTS, start, end), into: 'http' },
  ].filter((j) => present.has(j.name))
  const active = []
  for (const p of L7_PROTOCOLS) {
    if (!present.has(p.calls)) continue
    const entry = { name: p.name, unit: p.unit, calls: [], latency: null }
    active.push(entry)
    jobs.push({ name: p.calls, run: () => rates(client, p.calls, start, end), into: (r) => (entry.calls = r.series) })
    if (p.latency && present.has(p.latency + '_bucket')) {
      entry.latency = []
      jobs.push({ name: p.latency + '_bucket', run: () => rates(client, p.latency + '_bucket', start, end), into: (r) => (entry.latency = r.series) })
    }
  }

  const warnings = []
  let incomplete = false // a counter could not be read or was cut short: what is missing may only look absent
  if (!present.has(LISTEN)) warnings.push('The project has no listen-address metric, so no callee can be named: every destination shows as external.')
  const input = { listen: [], conn: [], fail: [], http: [], protocols: active }
  const settled = await Promise.allSettled(jobs.map((j) => j.run()))
  let listeners = null // which workloads listened during the range, and which still do at its end (null: no such metric)
  const failures = [] // why each failed, to name the reason when all do (warnings may start with something unrelated)
  settled.forEach((r, i) => {
    const j = jobs[i]
    if (r.status !== 'fulfilled') {
      const why = r.reason && r.reason.message ? r.reason.message : 'failed'
      failures.push(why)
      warnings.push(`${j.name}: ${why}`)
      incomplete = true
      return
    }
    if (r.value.truncated) incomplete = true
    if (j.into === 'listen') listeners = workloadsListening(r.value.series, start, end)
    if (r.value.truncated) warnings.push(`${j.name} returned more series than one call allows, so the map is partial; use a shorter window`)
    if (typeof j.into === 'function') j.into(r.value)
    else input[j.into] = r.value.series
  })
  if (jobs.length && failures.length === jobs.length) throw new Error(failures[0])

  const graph = buildGraph(input)
  return { ...graph, warnings, incomplete, listeners, from: start * 1000, to: end * 1000, hasData: present.has(CONNECTS) || present.has(HTTP_REQUESTS) }
}

/**
 * The latest value of every series of a gauge: [{labels, value, last, fresh}], `last` being when it was last reported (ms) and
 * `fresh` whether that was within three steps of the end of the range (it is still reporting). A series that stopped (a pod that
 * is gone) is KEPT here, marked not fresh: telling "gone" from "never existed" is what an inventory needs.
 */
export async function gaugeSeries(client, metric, start, end) {
  const step = Math.max(15, Math.floor((end - start) / 40))
  const data = (await client.get('metrics/query_range', { name: metric, from: start.toFixed(3), to: end.toFixed(3), step })) || {}
  const series = []
  for (const s of data.series || []) {
    const pts = s.points || []
    if (!pts.length) continue
    const lastPoint = pts.reduce((a, b) => (b.t > a.t ? b : a))
    series.push({ labels: s.labels || {}, value: lastPoint.v, last: lastPoint.t, fresh: lastPoint.t >= end * 1000 - 3 * step * 1000 })
  }
  return { series, truncated: !!data.truncated }
}

/**
 * The latest value of each series of a gauge that is still reporting: [{labels, value, last}]. A series that stopped is left out,
 * or its last value would be counted as if it were still there (see gaugeSeries to keep them).
 */
export async function gauges(client, metric, start, end) {
  const { series, truncated } = await gaugeSeries(client, metric, start, end)
  return { series: series.filter((s) => s.fresh).map(({ fresh, ...rest }) => (void fresh, rest)), truncated }
}

/**
 * The workloads that listened during the range (`seen`) and those still reporting at its end (`fresh`): a workload that was
 * seen but is not fresh has stopped, as when its pods are gone. A series counts as still reporting when its last reading is
 * within three steps of the end, as for any gauge here.
 */
export function workloadsListening(series, start, end) {
  const step = Math.max(15, Math.floor((end - start) / 40))
  const seen = new Set()
  const fresh = new Set()
  for (const s of series) {
    const w = workloadOf(s.labels.container_id)
    if (!w) continue
    seen.add(w.id)
    if (s.last >= end * 1000 - 3 * step * 1000) fresh.add(w.id)
  }
  return { seen: [...seen].sort(), fresh: [...fresh].sort() }
}
