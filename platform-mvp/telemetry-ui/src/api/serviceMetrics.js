import { gauges, loadMap, rates } from '../map/load'
import { DNS_METRIC, summarizeDns } from './dns'
import { instanceOf, workloadForId, workloadOf } from '../map/workload'

// The metrics side of a service's page: who calls it and whom it calls (the map's edges for it), and what it uses (CPU,
// memory, restarts). They are keyed by container, and the node-agent's naming rule turns a container id into the service name
// that traces use, so a service's metrics are the series whose container id names it.

export const CPU = 'container_resources_cpu_usage_seconds_total'
export const CPU_LIMIT = 'container_resources_cpu_limit_cores'
export const RSS = 'container_resources_memory_rss_bytes'
export const MEM_LIMIT = 'container_resources_memory_limit_bytes'
export const RESTARTS = 'container_restarts_total'
export const OOM = 'container_oom_kills_total'

/** Only an application id (a Kubernetes workload, or a unit or container on a plain host) has metrics keyed this way. */
export const hasMetrics = (name) => workloadForId(name) !== null

const mine = (id) => (s) => {
  const w = workloadOf(s.labels.container_id)
  return !!w && w.id === id
}
const sum = (xs, f) => xs.reduce((a, x) => a + f(x), 0)

/** The edges into and out of one node of the map, with the totals. Percentages cannot be added, so totals come from counts. */
export function trafficFor(graph, id) {
  const label = (n) => (graph.nodes.find((x) => x.id === n) || { label: n }).label
  const shape = (e, peer) => ({ ...e, peer, peerLabel: label(peer) })
  const inbound = graph.edges.filter((e) => e.to === id).map((e) => shape(e, e.from))
  const outbound = graph.edges.filter((e) => e.from === id).map((e) => shape(e, e.to))
  const total = (edges) => {
    const attempts = sum(edges, (e) => e.attempts)
    return {
      rps: sum(edges, (e) => e.rps ?? 0),
      errPct: attempts > 0 ? (100 * sum(edges, (e) => e.failures)) / attempts : 0,
      edges: edges.length,
    }
  }
  const worst = (a, b) => b.errPct - a.errPct || (b.rps ?? 0) - (a.rps ?? 0)
  return { inbound: inbound.sort(worst), outbound: outbound.sort(worst), totals: { inbound: total(inbound), outbound: total(outbound) } }
}

/**
 * CPU, memory, restarts and OOM kills of one workload. Counters are totals over the window (`seconds` long) or, for CPU,
 * the mean cores used; gauges are the latest values of the instances still reporting. A limit is the sum over those
 * instances, so it compares with the sum of their use. null for what the project has no metric for.
 */
export function summarizeResources(id, { cpu, cpuLimit, rss, memLimit, restarts, oom }, seconds) {
  const m = mine(id)
  const pick = (r) => (r ? r.filter(m) : null)
  const [cpuS, cpuL, rssS, memL, rest, oomS] = [cpu, cpuLimit, rss, memLimit, restarts, oom].map(pick)
  const limit = (s) => (s && s.length ? sum(s, (x) => x.value) : null)
  return {
    instances: rssS && rssS.length ? new Set(rssS.map((s) => instanceOf(s.labels))).size : null, // pods or machines: a sidecar is not a second instance
    cpuCores: cpuS && cpuS.length ? sum(cpuS, (s) => s.rate) : null,
    cpuLimitCores: limit(cpuL && cpuL.filter((s) => s.value > 0)),
    memoryBytes: rssS && rssS.length ? sum(rssS, (s) => s.value) : null,
    memoryLimitBytes: limit(memL && memL.filter((s) => s.value > 0)),
    restarts: rest && rest.length ? Math.round(sum(rest, (s) => s.rate) * seconds) : null,
    oomKills: oomS && oomS.length ? Math.round(sum(oomS, (s) => s.rate) * seconds) : null,
  }
}

/**
 * Loads everything the metrics section needs: {traffic, resources, warnings, found}. A metric that cannot be read leaves the
 * rest and adds a warning; if the metric list itself fails that is an error (usually a bad key).
 */
export async function loadServiceMetrics(client, name, { seconds, now = () => Date.now() } = {}) {
  const end = now() / 1000
  const start = end - seconds
  const present = new Set((await client.get('metrics')) || [])
  const id = name
  const warnings = []

  const reads = {
    cpu: [CPU, () => rates(client, CPU, start, end)],
    cpuLimit: [CPU_LIMIT, () => gauges(client, CPU_LIMIT, start, end)],
    rss: [RSS, () => gauges(client, RSS, start, end)],
    memLimit: [MEM_LIMIT, () => gauges(client, MEM_LIMIT, start, end)],
    restarts: [RESTARTS, () => rates(client, RESTARTS, start, end)],
    oom: [OOM, () => rates(client, OOM, start, end)],
    dns: [DNS_METRIC, () => rates(client, DNS_METRIC, start, end)],
  }
  const keys = Object.keys(reads).filter((k) => present.has(reads[k][0]))
  const [mapResult, ...settled] = await Promise.allSettled([
    loadMap(client, { seconds, now, present }),
    ...keys.map((k) => reads[k][1]()),
  ])

  const series = {}
  settled.forEach((r, i) => {
    const metric = reads[keys[i]][0]
    if (r.status !== 'fulfilled') return warnings.push(`${metric}: ${r.reason && r.reason.message ? r.reason.message : 'failed'}`)
    if (r.value.truncated) warnings.push(`${metric} returned more series than one call allows, so its figures may be partial`)
    series[keys[i]] = r.value.series
  })

  let traffic = null
  if (mapResult.status === 'fulfilled') {
    traffic = trafficFor(mapResult.value, id)
    warnings.push(...mapResult.value.warnings)
  } else {
    warnings.push(`calls: ${mapResult.reason && mapResult.reason.message ? mapResult.reason.message : 'failed'}`)
  }
  // Nothing could be read at all (the counters and every resource metric failed): usually a bad key, so say that.
  if (!traffic && !Object.keys(series).length && warnings.length) throw new Error(warnings[0])

  const resources = summarizeResources(id, series, seconds)
  const dns = series.dns ? summarizeDns(series.dns, id) : null
  const found = !!(dns) || !!(traffic && (traffic.inbound.length || traffic.outbound.length)) || Object.values(resources).some((v) => v !== null)
  return { traffic, resources, dns, warnings, found }
}
