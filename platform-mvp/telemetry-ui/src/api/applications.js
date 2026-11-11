import { gaugeSeries, gauges, loadMap, rates } from '../map/load.js'
import { instanceOf, workloadForId, workloadOf } from '../map/workload.js'
import { shape } from '../alerts/report.js'
import { DNS_METRIC, summarizeDns } from './dns.js'
import { serviceNames } from './search.js'
import { loadInventory } from './inventory.js'
import { silenceOf } from './silence.js'
import { CPU, CPU_LIMIT, MEM_LIMIT, OOM, RESTARTS, RSS, trafficFor } from './serviceMetrics.js'

// The Applications page: every application (workload) of the project with how it is doing, in one table.
//
// An application is any workload the node-agent reports a container for (the memory gauge has one series per container, and a pod that
// stopped keeps its last reading, which is how a workload with no running pod is told from one that never existed), plus any workload
// the cluster collector's inventory lists (so one scaled to zero long ago is still here), any service the traces or logs name and any
// node of the call map. Its
// numbers come from the same counters the map and the service page use, read once for ALL applications and grouped by workload,
// so the page costs about 25 reads however many applications there are.

const APP_TYPE = 'container_application_type'

/** "/k8s/shop/frontend" -> "shop"; a unit or container on a plain host ("/system.slice/nginx.service") -> "host"; any other name -> "other". */
export function namespaceOf(id) {
  const m = /^\/k8s(?:-cronjob)?\/([a-z0-9-]+)\//.exec(String(id || ''))
  if (m) return m[1]
  const w = workloadForId(id)
  return w && w.host ? 'host' : 'other'
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
export function buildApplications({ inventory = null, cpu = null, cpuLimit = null, memLimit = null, restarts = null, oom = null, appTypes = null, dns = null, graph = null, errorLogs = null, names = [], cluster = null, seconds = 3600 }) {
  const ids = new Set()
  const workloads = cluster && cluster.workloads ? cluster.workloads : null
  // Is anything sending? If nothing is, the agent (or the way to the hub) is the problem, and no application is called silent for it.
  const agentAlive = !!(inventory && inventory.some((s) => s.fresh))
  for (const id of workloads ? workloads.keys() : []) ids.add(id)
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
    const w = workloadForId(id)
    const name = w ? w.name : id
    const ns = namespaceOf(id)

    const mine = inventory ? by(inventory, id) : null
    const pods = mine ? new Set(mine.map((s) => instanceOf(s.labels))) : null
    const running = mine ? new Set(mine.filter((s) => s.fresh).map((s) => instanceOf(s.labels))) : null
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

    const wl = workloads ? workloads.get(id) : null
    const workload = wl ? { ...wl, reasons: (cluster.reasons && cluster.reasons.get(id)) || [] } : null
    const instances = pods && pods.size ? { running: running.size, seen: pods.size } : null

    const traffic = graph ? trafficFor(graph, id) : null
    const inb = traffic ? traffic.totals.inbound : null
    const out = traffic ? traffic.outbound : []
    const rows = logsBy.get(id) || []
    const unique = new Set(rows.map((l) => shape(l.body)))

    apps.push({
      silence: silenceOf({ instances, workload }, { agentAlive, hasInventory: !!workloads }),
      workload,
      id,
      name,
      namespace: ns,
      category: categoryOf(ns),
      label: w ? w.label : id,
      type: typeOf(appTypes || [], id, nodeOf.get(id) && nodeOf.get(id).serves),
      instances,
      cpuCores: cpuS && cpuS.length ? sum(cpuS, (s) => s.rate) : null,
      cpuLimit: lim(cpuLimit),
      memoryBytes: fresh && fresh.length ? sum(fresh, (s) => s.value) : null,
      memoryLimit: lim(memLimit),
      restarts: total(restarts),
      oomKills: total(oom),
      traffic: inb && traffic.inbound.length ? { rps: inb.rps, errPct: inb.errPct, callers: traffic.inbound.length } : null,
      upstreams: out.length ? { ok: out.filter((e) => e.errPct < 1).length, total: out.length } : null,
      dns: dns ? summarizeDns(dns, id) : null,
      errorLines: rows.length,
      uniqueErrors: unique.size,
      hasMetrics: !!(mine && mine.length) || !!(traffic && (traffic.inbound.length || traffic.outbound.length)),
    })
  }
  return apps.sort((a, b) => a.namespace.localeCompare(b.namespace) || a.name.localeCompare(b.name))
}

export const STATUSES = [
  { key: 'critical', label: 'Critical' },
  { key: 'silent', label: 'Silent' },
  { key: 'warning', label: 'Warning' },
  { key: 'logs', label: 'Errors in logs' },
  { key: 'nodata', label: 'No data' },
  { key: 'ok', label: 'OK' },
]

/**
 * How an application is doing, worst first: critical when an alert is firing at that level; silent when it has stopped sending data (see
 * silence.js: whether that was meant is in `app.silence`, and zero running instances is not itself judged a failure); warning for a warning
 * alert, a restart or OOM kill in the window, or at least 1% of the calls it answers failing; "errors in logs" when only its logs show
 * errors; "no data" when nothing but a name is known; else ok. `alert` is the worst firing alert's severity.
 */
export function statusOf(app, alert = null) {
  if (alert === 'critical') return 'critical'
  if (app.silence) return 'silent'
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
  add('dns', DNS_METRIC, () => rates(client, DNS_METRIC, start, end))
  add('cluster', null, () => loadInventory(client, { now }))
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
      // the cluster inventory is an extra: it failing is a warning, and not one of the telemetry reads that all failing means a bad key
      if (k !== 'cluster') failures.push(why)
      warnings.push(`${k === 'cluster' ? 'cluster inventory' : jobs[k].metric}: ${why}`)
      return
    }
    if (r.value && r.value.truncated) warnings.push(`${jobs[k].metric} returned more series than one call allows, so some applications may be missing or partial; use a shorter window`)
    got[k] = r.value && r.value.series !== undefined ? r.value.series : r.value
  })
  const telemetry = keys.filter((k) => k !== 'cluster').length
  if (telemetry && failures.length === telemetry) throw new Error(failures[0])

  const graph = got.graph || null
  if (graph) warnings.push(...graph.warnings)
  const cluster = got.cluster || null
  if (cluster && cluster.stale) warnings.push(`The cluster collector last reported ${Math.max(1, Math.round(cluster.stale.seconds / 60))} minutes ago, so what each workload is set to run is not shown: whether an application that stopped was scaled down cannot be said. Is it running?`)
  if (cluster && cluster.truncated) warnings.push('The cluster has more workloads than one call returns, so some are missing from this list.')
  const apps = buildApplications({ ...got, inventory: got.inventory || null, graph, names: got.names || [], cluster, seconds })
  return { apps, graph, warnings, from: start * 1000, to: end * 1000, hasInventory: !!got.inventory, hasCluster: !!(cluster && cluster.workloads) }
}

/**
 * The aspects of an application the Map's hover menu lists, each with a status (ok, caution, bad, or unknown when it is not known) and a short
 * reason: instances, CPU, memory, DNS and logs. Each is also a place on the application's page (its `anchor`).
 */
export function healthItems(app) {
  const ratio = (used, limit) => (used != null && limit ? used / limit : null)
  const tone = { bad: 'bad', caution: 'caution', info: 'unknown' }
  const instances = app.silence
    ? [tone[app.silence.tone] || 'caution', `${app.silence.label}: ${app.silence.detail}`]
    : app.instances
      ? app.instances.running < app.instances.seen ? ['caution', `${app.instances.running} of ${app.instances.seen} running`] : ['ok', `${app.instances.running} running`]
      : ['unknown', 'not reported']
  const cpu = ratio(app.cpuCores, app.cpuLimit)
  const mem = ratio(app.memoryBytes, app.memoryLimit)
  const cpuItem = cpu == null ? (app.cpuCores != null ? ['ok', 'no limit set'] : ['unknown', 'not reported']) : cpu >= 1 ? ['bad', 'at its limit'] : cpu >= 0.8 ? ['caution', `${Math.round(cpu * 100)}% of its limit`] : ['ok', `${Math.round(cpu * 100)}% of its limit`]
  const memItem = app.oomKills > 0 ? ['bad', `${app.oomKills} out-of-memory kill${app.oomKills === 1 ? '' : 's'}`]
    : mem == null ? (app.memoryBytes != null ? ['ok', 'no limit set'] : ['unknown', 'not reported'])
    : mem >= 0.95 ? ['bad', `${Math.round(mem * 100)}% of its limit`] : mem >= 0.8 ? ['caution', `${Math.round(mem * 100)}% of its limit`] : ['ok', `${Math.round(mem * 100)}% of its limit`]
  const dnsItem = !app.dns ? ['unknown', 'no lookups']
    : app.dns.failedShare >= 0.05 ? ['bad', `${Math.round(app.dns.failedShare * 1000) / 10}% of lookups fail`]
    : app.dns.failedShare >= 0.01 ? ['caution', `${Math.round(app.dns.failedShare * 1000) / 10}% of lookups fail`] : ['ok', 'lookups succeed']
  const logsItem = app.uniqueErrors > 0 ? ['caution', `${app.uniqueErrors} unique error${app.uniqueErrors === 1 ? '' : 's'}`] : ['ok', 'no errors']
  const item = (key, label, [status, hint]) => ({ key, label, status, hint, anchor: key })
  return [item('instances', 'Instances', instances), item('cpu', 'CPU', cpuItem), item('memory', 'Memory', memItem), item('dns', 'DNS', dnsItem), item('logs', 'Logs', logsItem)]
}

/**
 * A short-lived cache of the applications, shared by the pages that need them (the Applications page, and the Map's hover menu), so moving
 * between them does not read about 25 metrics each time. Keyed by project and window; a load in progress is shared, not repeated; a failed
 * one is forgotten so the next call tries again. `force` skips it (a Refresh).
 */
export function createApplicationsCache({ ttlMs = 30_000, now = () => Date.now() } = {}) {
  const entries = new Map()
  return {
    get(client, { project, seconds, force = false, load = loadApplications }) {
      const key = `${project}|${seconds}`
      const hit = entries.get(key)
      if (!force && hit && now() - hit.at < ttlMs) return hit.promise
      const promise = load(client, { seconds })
      entries.set(key, { at: now(), promise })
      promise.catch(() => {
        if (entries.get(key) && entries.get(key).promise === promise) entries.delete(key)
      })
      return promise
    },
    clear: () => entries.clear(),
  }
}
