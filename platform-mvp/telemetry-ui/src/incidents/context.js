// The context of a recorded incident: what the application's traffic, resources, dependencies and logs and traces looked like around it, picked from the
// incident itself (its application and its time) so nobody writes a query. Pure shaping, and one loader that reads with the existing read API only.
//
// Every panel stands on its own: one that cannot be read says why (status "unavailable"), one that does not apply says so (status "none"), and the others
// are still shown. Times are epoch milliseconds in what this returns; the window is given in seconds, as the rest of the UI does.

import { gauges, loadMap } from '../map/load.js'
import { CPU, CPU_LIMIT, MEM_LIMIT, OOM, RESTARTS, RSS, hasMetrics, summarizeResources, trafficFor } from '../api/serviceMetrics.js'
import { SPAN_LIMIT, percentile } from '../api/serviceStats.js'
import { loadEvidence } from './evidence.js'
import { workloadOf as defaultWorkloadOf } from '../map/workload.js'

export const PAD_BEFORE_SECONDS = 1800 // shown from this long before the incident started
export const PAD_AFTER_SECONDS = 900 // and, once it is resolved, this long after
export const MIN_SPAN_SECONDS = 3600
export const DEFAULT_SPAN_SECONDS = 6 * 3600 // when the incident has no start time
export const MAX_SPAN_SECONDS = 7 * 86400
export const BASELINE_SHIFT_MS = 86400000 // "the same time yesterday"
export const BUCKETS = 40
const ENDED = ['resolved', 'closed']

const ms = (iso) => {
  const t = Date.parse(iso)
  return Number.isFinite(t) ? t : null
}

/**
 * The window to show for an incident: {start, end} in epoch SECONDS. From before it started to now (or a little after it was resolved), at least an hour
 * and at most a week. `span` (seconds), when given, is the length the person picked, counted back from the end.
 */
export function contextWindow(incident, { now = Date.now(), span = 0 } = {}) {
  const nowS = now / 1000
  const started = ms(incident && (incident.startedAt || incident.createdAt))
  const resolved = incident && ENDED.includes(incident.state) ? ms(incident.resolvedAt) : null
  const end = resolved !== null ? Math.min(resolved / 1000 + PAD_AFTER_SECONDS, nowS) : nowS
  let start = started !== null ? started / 1000 - PAD_BEFORE_SECONDS : end - DEFAULT_SPAN_SECONDS
  if (span > 0) start = end - span
  start = Math.min(start, end - MIN_SPAN_SECONDS)
  start = Math.max(start, end - MAX_SPAN_SECONDS)
  return { start, end }
}

/** The series of a metric whose container belongs to the workload `id`, added up per instant: [{t, v}] by time. */
export function sumByTime(series, id, workloadOf) {
  const at = new Map()
  for (const s of Array.isArray(series) ? series : []) {
    const w = workloadOf(s.labels && s.labels.container_id)
    if (!w || w.id !== id) continue
    for (const p of s.points || []) {
      if (!Number.isFinite(p.t) || !Number.isFinite(p.v)) continue
      at.set(p.t, (at.get(p.t) || 0) + p.v)
    }
  }
  return [...at.entries()].sort((a, b) => a[0] - b[0]).map(([t, v]) => ({ t, v }))
}

/**
 * Root spans (newest first, as the API answers) -> {points, total, truncated, from}. Each point is one slice of the window: {t, count, errors, rps, errorPct,
 * p95Ms}, with nothing (null) for the figures that need spans in a slice that has none. With a full page the answer stops where its oldest span is, and
 * only that part is described (`from` says where it starts), never extrapolated.
 */
export function bucketSpans(spans, { from, to, buckets = BUCKETS, limit = SPAN_LIMIT }) {
  const list = Array.isArray(spans) ? spans : []
  const times = list.map((s) => Date.parse(s.time)).filter((t) => Number.isFinite(t))
  const truncated = list.length >= limit
  const covered = truncated && times.length ? Math.min(Math.max(from, Math.min(...times)), to) : from
  const width = (to - covered) / buckets
  if (!(width > 0)) return { points: [], total: list.length, truncated, from: covered }
  const slots = Array.from({ length: buckets }, (_, i) => ({ t: covered + i * width, count: 0, errors: 0, durations: [] }))
  for (const s of list) {
    const t = Date.parse(s.time)
    if (!(t >= covered && t <= to)) continue
    const b = slots[Math.min(buckets - 1, Math.floor((t - covered) / width))]
    b.count++
    if (s.status_code === 'ERROR') b.errors++
    const d = Number(s.duration_ns)
    if (Number.isFinite(d)) b.durations.push(d / 1e6)
  }
  const points = slots.map((b) => {
    const sorted = b.durations.sort((x, y) => x - y)
    return { t: b.t, count: b.count, errors: b.errors, rps: b.count / (width / 1000), errorPct: b.count ? (100 * b.errors) / b.count : null, p95Ms: percentile(sorted, 95) }
  })
  return { points, total: list.length, truncated, from: covered }
}

const failure = (e) => (e && e.message ? e.message : 'failed')
const unavailable = (what, e) => ({ status: 'unavailable', reason: `${what} could not be read: ${failure(e)}` })

async function pointSeries(client, metric, start, end, rate) {
  const step = Math.max(15, Math.floor((end - start) / 60))
  const params = { name: metric, from: start.toFixed(3), to: end.toFixed(3), step }
  if (rate) Object.assign(params, { rate: true, from_zero: true })
  const data = (await client.get('metrics/query_range', params)) || {}
  return { series: data.series || [], truncated: !!data.truncated, step }
}

async function loadSignals(client, app, start, end, compare) {
  const range = (a, b) => ({ from: Math.floor(a), to: Math.ceil(b) })
  const read = (a, b) => client.get('traces', { ...range(a, b), service: app, root: true, limit: SPAN_LIMIT })
  const baselineStart = start - BASELINE_SHIFT_MS / 1000
  const [now, before] = await Promise.allSettled([read(start, end), compare ? read(baselineStart, end - BASELINE_SHIFT_MS / 1000) : Promise.resolve(null)])
  if (now.status !== 'fulfilled') return unavailable('The traffic', now.reason)
  const current = bucketSpans(now.value, { from: start * 1000, to: end * 1000 })
  const out = { status: 'ok', ...current, baseline: null, warnings: [] }
  if (current.truncated) out.warnings.push('The window holds more requests than one call returns, so the traffic shown covers only its newest part.')
  if (before.status === 'fulfilled' && before.value) {
    const b = bucketSpans(before.value, { from: baselineStart * 1000, to: (end * 1000) - BASELINE_SHIFT_MS })
    out.baseline = { ...b, points: b.points.map((p) => ({ ...p, t: p.t + BASELINE_SHIFT_MS })) }
  } else if (before.status === 'rejected') out.warnings.push(`The same time yesterday could not be read: ${failure(before.reason)}`)
  return out
}

async function loadResources(client, app, start, end, present, workloadOf) {
  const wanted = [['cpu', CPU, true], ['memory', RSS, false], ['restarts', RESTARTS, true], ['oom', OOM, true]].filter(([, m]) => present.has(m))
  const limits = [['cpuLimit', CPU_LIMIT], ['memLimit', MEM_LIMIT]].filter(([, m]) => present.has(m))
  if (!wanted.length && !limits.length) return { status: 'none', reason: 'The project has no resource metrics for this application.' }
  const settled = await Promise.allSettled([
    ...wanted.map(([, m, rate]) => pointSeries(client, m, start, end, rate)),
    ...limits.map(([, m]) => gauges(client, m, start, end)),
  ])
  const out = { status: 'ok', cpu: [], memory: [], restarts: [], oom: [], cpuLimitCores: null, memoryLimitBytes: null, step: 0, warnings: [] }
  const limitSeries = {}
  settled.forEach((r, i) => {
    const isLimit = i >= wanted.length
    const [key, metric] = isLimit ? limits[i - wanted.length] : wanted[i]
    if (r.status !== 'fulfilled') return out.warnings.push(`${metric}: ${failure(r.reason)}`)
    if (r.value.truncated) out.warnings.push(`${metric} returned more series than one call allows, so its figures may be partial`)
    if (isLimit) limitSeries[key] = r.value.series
    else {
      out[key] = sumByTime(r.value.series, app, workloadOf)
      out.step = Math.max(out.step, r.value.step)
    }
  })
  const lim = summarizeResources(app, limitSeries, end - start)
  out.cpuLimitCores = lim.cpuLimitCores
  out.memoryLimitBytes = lim.memoryLimitBytes
  // A counter's rate times the step is how many happened in that slice: a slice with one or more is a marker.
  out.markers = [
    ...out.restarts.map((p) => ({ t: p.t, kind: 'restart', count: Math.round(p.v * out.step) })),
    ...out.oom.map((p) => ({ t: p.t, kind: 'oom', count: Math.round(p.v * out.step) })),
  ].filter((m) => m.count >= 1)
  if (!out.cpu.length && !out.memory.length && !out.markers.length) return { status: 'none', reason: 'This application reported no resource use in the window.', warnings: out.warnings }
  return out
}

async function loadDependencies(client, app, start, end, present) {
  const graph = await loadMap(client, { seconds: end - start, now: () => end * 1000, present })
  const t = trafficFor(graph, app)
  if (!t.inbound.length && !t.outbound.length) return { status: 'none', reason: 'No calls to or from this application were seen in the window.', warnings: graph.warnings }
  return { status: 'ok', ...t, inbound: t.inbound.slice(0, 8), outbound: t.outbound.slice(0, 8), warnings: graph.warnings }
}

/** Where to mark the incident's own history on the charts. */
export function incidentMarkers(incident, events = []) {
  const out = []
  const started = ms(incident.startedAt || incident.createdAt)
  if (started !== null) out.push({ t: started, kind: 'started', label: 'Incident started' })
  const resolved = ENDED.includes(incident.state) ? ms(incident.resolvedAt) : null
  if (resolved !== null) out.push({ t: resolved, kind: 'resolved', label: 'Resolved' })
  for (const e of events) {
    const t = ms(e.when)
    if (t !== null && e.type !== 'created') out.push({ t, kind: 'event', label: e.text })
  }
  return out.sort((a, b) => a.t - b.t)
}

/**
 * The context of one recorded incident: {app, window: {start, end} (seconds), panels: {signals, resources, dependencies, evidence}, markers, warnings}.
 * Each panel is {status: 'ok' | 'none' | 'unavailable', ...}. An incident with no application has nothing to show; an application that is not a Kubernetes
 * workload has no resource or call metrics (they are keyed by container) but its requests, logs and traces are still read.
 * `workloadOf` is the map's by default (a test may pass another).
 */
export async function loadContext(client, incident, { events = [], now = Date.now(), span = 0, compare = true, workloadOf = defaultWorkloadOf } = {}) {
  const window = contextWindow(incident, { now, span })
  const app = incident.app
  const markers = incidentMarkers(incident, events)
  const base = { app, window, markers, warnings: [] }
  if (!app) {
    const none = { status: 'none', reason: 'This incident names no application, so there is nothing to look up.' }
    return { ...base, panels: { signals: none, resources: none, dependencies: none, evidence: none } }
  }
  const { start, end } = window
  const metrics = hasMetrics(app)
  const presentP = metrics ? client.get('metrics').then((l) => new Set(l || [])) : null
  const noMetrics = { status: 'none', reason: 'Resource and call metrics are kept for Kubernetes workloads; this application is not one.' }
  const guarded = (what, fn) => (metrics ? presentP.then(fn).catch((e) => unavailable(what, e)) : Promise.resolve(noMetrics))
  const [signals, resources, dependencies, evidence] = await Promise.all([
    loadSignals(client, app, start, end, compare).catch((e) => unavailable('The traffic', e)),
    guarded('The resource use', (present) => loadResources(client, app, start, end, present, workloadOf)),
    guarded('The calls', (present) => loadDependencies(client, app, start, end, present)),
    loadEvidence(client, { app, cause: null, start, end }).then(
      (e) => ({ status: 'ok', ...e }),
      (e) => unavailable('The logs and traces', e),
    ),
  ])
  const panels = { signals, resources, dependencies, evidence }
  const markersAll = resources.status === 'ok' ? [...markers, ...resources.markers.map((m) => ({ ...m, label: m.kind === 'oom' ? `${m.count} OOM kill${m.count === 1 ? '' : 's'}` : `${m.count} restart${m.count === 1 ? '' : 's'}` }))].sort((a, b) => a.t - b.t) : markers
  return { ...base, markers: markersAll, panels, warnings: Object.values(panels).flatMap((p) => p.warnings || []) }
}
