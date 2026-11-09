import { gaugeSeries, gauges, loadMap, rates } from '../map/load.js'
import { podOf, workloadOf } from '../map/workload.js'
import { shape } from '../alerts/report.js'
import { serviceNames } from './search.js'
import { CPU, CPU_LIMIT, MEM_LIMIT, OOM, RESTARTS, RSS, trafficFor } from './serviceMetrics.js'

// The Applications page: every application (workload) of the project with how it is doing, in one table.
//
// There is no inventory in the API, so it is assembled. An application is any workload the node-agent reports a container for
// (the memory gauge has one series per container, and a pod that stopped keeps its last reading, which is how a workload with no
// running pod is told from one that never existed), plus any service the traces or logs name and any node of the call map. Its
// numbers come from the same counters the map and the service page use, read once for ALL applications and grouped by workload,
// so the page costs about 25 reads however many applications there are.

const APP_TYPE = 'container_application_type'

/** "/k8s/shop/frontend" -> "shop"; a name that is not a Kubernetes workload belongs to "other". */
export function namespaceOf(id) {
  const m = /^\/k8s(?:-cronjob)?\/([a-z0-9-]+)\//.exec(String(id || ''))
  return m ? m[1] : 'other'
}

// Namespaces that are the platform's, not the user's applications; and the ones that watch them. They are hidden until asked for,
// as the reference page does, so the first view is the applications themselves.
const CONTROL_PLANE = /^(kube-|gke-|gmp-|gatekeeper|config-management|istio-system$|asm-|anthos|cert-manager$|ingress-nginx$|linkerd|calico|tigera|cilium|flux-system$|argocd$|olm$)/
const MONITORING = /^(monitoring$|observability$|prometheus|grafana|loki|tempo|jaeger|logging$|elastic|coroot|telemetry|otel|opentelemetry|datadog|newrelic)/

export function categoryOf(ns) {
  if (CONTROL_PLANE.test(ns)) return 'control-plane'
  if (MONITORING.test(ns)) return 'monitoring'
  return 'application'
}

const sum = (xs, f) => xs.reduce((a, x) => a + f(x), 0)
const by = (list, id) => list.filter((s) => {
  const w = workloadOf(s.labels.container_id)
  return !!w && w.id === id
})

/** The commonest application type among an application's containers ("postgres", "redis", ...), or ''. */
function typeOf(typeSeries, id, serves) {
  const counts = new Map()
  for (const s of by(typeSeries, id)) {
    const t = s.labels.application_type
    if (t && t !== 'unknown') counts.set(t, (counts.get(t) || 0) + 1)
  }
  if (counts.size) return [...counts].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))[0][0]
  return serves && serves.length ? serves[0] : ''
}

/**
 * Applications from what was read. Pure. Every input may be missing (null): the project may lack a metric, or a read may have
 * failed, and an application is then shown with `–` for that, never with a made-up zero.
 */
export function buildApplications({ inventory = null, cpu = null, cpuLimit = null, memLimit = null, restarts = null, oom = null, appTypes = null, graph = null, errorLogs = null, names = [], seconds = 3600 }) {
  const ids = new Set()
  for (const s of inventory || []) {
    const w = workloadOf(s.labels.container_id)
    if (w) ids.add(w.id)
  }
  for (const n of (graph && graph.nodes) || []) if (!n.external) ids.add(n.id)
  for (const n of names) if (n) ids.add(n)

  const nodeOf = new Map(((graph && graph.nodes) || []).map((n) => [n.id, n]))
  const logsBy = new Map()
  for (const l of errorLogs || []) {
    if (!l.service) continue
    if (!logsBy.has(l.service)) logsBy.set(l.service, [])
    logsBy.get(l.service).push(l)
  }

  const apps = []
  for (const id of ids) {
    const w = workloadOf(`${id}/x`)
    const name = w ? w.name : id
    const ns = namespaceOf(id)

    const mine = inventory ? by(inventory, id) : null
    const pods = mine ? new Set(mine.map((s) => podOf(s.labels.container_id))) : null
    const running = mine ? new Set(mine.filter((s) => s.fresh).map((s) => podOf(s.labels.container_id))) : null
    const fresh = mine ? mine.filter((s) => s.fresh) : null
    const lim = (series) => {
      const v = series ? by(series, id).filter((s) => s.value > 0) : []
      return v.length ? sum(v, (s) => s.value) : null
    }
    const cpuS = cpu ? by(cpu, id) : null
    const total = (series) => {
      const s = series ? by(series, id) : []
      return s.length ? Math.round(sum(s, (x) => x.rate) * seconds) : null
    }

    const traffic = graph ? trafficFor(graph, id) : null
    const inb = traffic ? traffic.totals.inbound : null
    const out = traffic ? traffic.outbound : []
    const rows = logsBy.get(id) || []
    const unique = new Set(rows.map((l) => shape(l.body)))

    apps.push({
      id,
      name,
      namespace: ns,
      category: categoryOf(ns),
      label: w ? w.label : id,
      type: typeOf(appTypes || [], id, nodeOf.get(id) && nodeOf.get(id).serves),
      instances: pods && pods.size ? { running: running.size, seen: pods.size } : null,
      cpuCores: cpuS && cpuS.length ? sum(cpuS, (s) => s.rate) : null,
      cpuLimit: lim(cpuLimit),
      memoryBytes: fresh && fresh.length ? sum(fresh, (s) => s.value) : null,
      memoryLimit: lim(memLimit),
      restarts: total(restarts),
      oomKills: total(oom),
      traffic: inb && traffic.inbound.length ? { rps: inb.rps, errPct: inb.errPct, callers: traffic.inbound.length } : null,
      upstreams: out.length ? { ok: out.filter((e) => e.errPct < 1).length, total: out.length } : null,
      errorLines: rows.length,
      uniqueErrors: unique.size,
      hasMetrics: !!(mine && mine.length) || !!(traffic && (traffic.inbound.length || traffic.outbound.length)),
    })
  }
  return apps.sort((a, b) => a.namespace.localeCompare(b.namespace) || a.name.localeCompare(b.name))
}

export const STATUSES = [
  { key: 'critical', label: 'Critical' },
  { key: 'warning', label: 'Warning' },
  { key: 'logs', label: 'Errors in logs' },
  { key: 'nodata', label: 'No data' },
  { key: 'ok', label: 'OK' },
]

/**
 * How an application is doing, worst first: critical when an alert is firing at that level or it has pods but none are running;
 * warning for a warning alert, a restart or OOM kill in the window, or at least 1% of the calls it answers failing; "errors in logs"
 * when only its logs show errors; "no data" when nothing but a name is known; else ok. `alert` is the worst firing alert's severity.
 */
export function statusOf(app, alert = null) {
  if (alert === 'critical' || (app.instances && app.instances.seen > 0 && app.instances.running === 0)) return 'critical'
  if (alert === 'warning' || app.restarts > 0 || app.oomKills > 0 || (app.traffic && app.traffic.errPct >= 1)) return 'warning'
  if (app.uniqueErrors > 0) return 'logs'
  if (!app.hasMetrics) return 'nodata'
  return 'ok'
}

/** The namespaces of the applications, with how many each has: [{name, category, count}], by name. */
export function namespacesOf(apps) {
  const by = new Map()
  for (const a of apps) by.set(a.namespace, (by.get(a.namespace) || 0) + 1)
  return [...by].map(([name, count]) => ({ name, category: categoryOf(name), count })).sort((a, b) => a.name.localeCompare(b.name))
}

/** The worst firing alert's severity for each application id: Map(id -> 'critical' | 'warning'). */
export function alertLevels(alerts) {
  const out = new Map()
  for (const a of alerts || []) {
    if (a.status !== 'firing' || a.service === '*') continue
    if (a.severity === 'critical' || !out.has(a.service)) out.set(a.service, a.severity === 'critical' ? 'critical' : 'warning')
  }
  return out
}

/**
 * Reads everything the table needs for the last `seconds` seconds and builds the applications. One source failing leaves the rest
 * (its name goes in `warnings`, and what it would have shown is `–`); the metric list failing is an error, as that is usually a
 * bad key.
 */
export async function loadApplications(client, { seconds, now = () => Date.now() } = {}) {
  const end = now() / 1000
  const start = end - seconds
  const present = new Set((await client.get('metrics')) || [])
  const has = (m) => present.has(m)

  const jobs = {}
  const add = (key, metric, run) => {
    if (metric === null || has(metric)) jobs[key] = { metric: metric || key, run }
  }
  add('inventory', RSS, () => gaugeSeries(client, RSS, start, end))
  add('memLimit', MEM_LIMIT, () => gauges(client, MEM_LIMIT, start, end))
  add('cpuLimit', CPU_LIMIT, () => gauges(client, CPU_LIMIT, start, end))
  add('appTypes', APP_TYPE, () => gaugeSeries(client, APP_TYPE, start, end))
  add('cpu', CPU, () => rates(client, CPU, start, end))
  add('restarts', RESTARTS, () => rates(client, RESTARTS, start, end))
  add('oom', OOM, () => rates(client, OOM, start, end))
  add('graph', null, () => loadMap(client, { seconds, now, present }))
  add('names', null, () => serviceNames(client, { seconds, now, strict: true }))
  add('errorLogs', null, async () => {
    const rows = await client.get('logs', { severity: 'ERROR', from: Math.floor(start), to: Math.floor(end), limit: 1000 })
    return Array.isArray(rows) ? rows : []
  })

  const keys = Object.keys(jobs)
  const settled = await Promise.allSettled(keys.map((k) => jobs[k].run()))
  const got = {}
  const warnings = []
  const failures = []
  settled.forEach((r, i) => {
    const k = keys[i]
    if (r.status !== 'fulfilled') {
      const why = r.reason && r.reason.message ? r.reason.message : 'failed'
      failures.push(why)
      warnings.push(`${jobs[k].metric}: ${why}`)
      return
    }
    if (r.value && r.value.truncated) warnings.push(`${jobs[k].metric} returned more series than one call allows, so some applications may be missing or partial; use a shorter window`)
    got[k] = r.value && r.value.series !== undefined ? r.value.series : r.value
  })
  if (keys.length && failures.length === keys.length) throw new Error(failures[0])

  const graph = got.graph || null
  if (graph) warnings.push(...graph.warnings)
  const apps = buildApplications({ ...got, inventory: got.inventory || null, graph, names: got.names || [], seconds })
  return { apps, warnings, from: start * 1000, to: end * 1000, hasInventory: !!got.inventory }
}
