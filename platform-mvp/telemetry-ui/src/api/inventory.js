// What the cluster collector reports about the cluster (GET /inventory, /inventory/workloads, /inventory/pods), as far as the Applications page
// needs it: what each workload is SET to run and how many are ready. That is what tells "scaled down" from "should be running and is not",
// which the node-agent's telemetry alone cannot: a workload that stopped sending data looks the same either way.
//
// Pure functions over the rows the API returns; loadInventory is the only one that calls it.

export const WORKLOAD_PATH_LIMIT = 1000
/** A collector that has not reported for this long is not trusted: what it last said may no longer be so. */
export const STALE_SECONDS = 600

/** The application id of a workload: the node-agent's name for it, "/k8s/<namespace>/<name>" ("/k8s-cronjob/..." for a cron job). */
export const appIdOf = (kind, namespace, name) => (kind === 'cronjob' ? `/k8s-cronjob/${namespace}/${name}` : `/k8s/${namespace}/${name}`)

const int = (v) => (Number.isFinite(v) ? v : 0)

/**
 * Map(application id -> {kind, desired, ready, available, observed}) from the rows of /inventory/workloads. Two clusters with a workload of the
 * same name are one application here (the page has one row per id), so their numbers are added.
 */
export function workloadsOf(rows) {
  const out = new Map()
  for (const r of rows || []) {
    if (!r || !r.namespace || !r.name || !r.data) continue
    const id = appIdOf(r.kind, r.namespace, r.name)
    const w = { kind: r.kind, desired: int(r.data.desired), ready: int(r.data.ready), available: int(r.data.available), observed: Date.parse(r.observed) || 0, clusters: 1 }
    const prev = out.get(id)
    if (prev) {
      prev.desired += w.desired
      prev.ready += w.ready
      prev.available += w.available
      prev.observed = Math.max(prev.observed, w.observed)
      prev.clusters += 1
    } else out.set(id, w)
  }
  return out
}

/** Why a pod is not ready, in one word: its own reason (Unschedulable, Evicted), or what its containers wait for or died of. */
export function podReason(p) {
  const d = (p && p.data) || {}
  if (d.reason) return String(d.reason)
  for (const c of d.containers || []) {
    if (c.state === 'waiting' && c.reason) return c.reason === 'CrashLoopBackOff' && c.last_reason ? `CrashLoopBackOff (${c.last_reason})` : c.reason
    if (c.state === 'terminated' && c.reason) return c.reason
  }
  return d.phase && d.phase !== 'Running' ? String(d.phase) : 'not ready'
}

/**
 * Map(application id -> ['CrashLoopBackOff ×2', 'Unschedulable']) from the rows of /inventory/pods: why the pods of each workload that is not
 * ready are not, the commonest first, at most three.
 */
export function podReasons(rows) {
  const per = new Map()
  for (const p of rows || []) {
    const d = p && p.data
    if (!d || d.ready === true || d.phase === 'Succeeded' || !d.workload_kind || !d.workload_name || d.workload_kind === 'pod') continue
    const id = appIdOf(d.workload_kind, p.namespace, d.workload_name)
    const reason = podReason(p)
    if (!per.has(id)) per.set(id, new Map())
    per.get(id).set(reason, (per.get(id).get(reason) || 0) + 1)
  }
  const out = new Map()
  for (const [id, m] of per) {
    out.set(id, [...m].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0])).slice(0, 3).map(([r, n]) => (n > 1 ? `${r} ×${n}` : r)))
  }
  return out
}

const notFound = (e) => e && (e.status === 404 || e.status === 405)

/**
 * What the cluster collectors say now, or null when there are none (the project has no inventory, or the hub is older than the endpoint).
 * -> {workloads: Map, reasons: Map, clusters, truncated} when at least one collector reported recently;
 *    {workloads: null, reasons: empty, stale: {seconds}} when they all have not: what they last said is not trusted.
 */
export async function loadInventory(client, { now = () => Date.now() } = {}) {
  let summary
  try {
    summary = await client.get('inventory')
  } catch (e) {
    if (notFound(e)) return null
    throw e
  }
  if (!Array.isArray(summary) || !summary.length) return null
  const fresh = summary.filter((c) => now() - (Date.parse(c.last_received) || 0) <= STALE_SECONDS * 1000)
  if (!fresh.length) {
    const newest = Math.max(...summary.map((c) => Date.parse(c.last_received) || 0))
    return { workloads: null, reasons: new Map(), clusters: summary, truncated: false, stale: { seconds: Math.max(0, Math.round((now() - newest) / 1000)) } }
  }
  const names = new Set(fresh.map((c) => c.cluster))
  const rows = ((await client.get('inventory/workloads', { limit: WORKLOAD_PATH_LIMIT })) || []).filter((r) => names.has(r.cluster))
  const workloads = workloadsOf(rows)
  // pods are only read when something is not ready: they are the most rows, and only needed to say why
  let reasons = new Map()
  if ([...workloads.values()].some((w) => w.desired > 0 && w.ready < w.desired)) {
    const pods = ((await client.get('inventory/pods', { limit: WORKLOAD_PATH_LIMIT })) || []).filter((r) => names.has(r.cluster))
    reasons = podReasons(pods)
  }
  return { workloads, reasons, clusters: summary, truncated: rows.length >= WORKLOAD_PATH_LIMIT, stale: null }
}
