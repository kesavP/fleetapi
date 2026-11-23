// Finding incidents and analysing one, from the read API. The logic is in detect.js, analyze.js and narrative.js; this reads what they need.

import { loadApplications } from '../api/applications.js'
import { SLO_DEFAULTS, burnRate, findIncidents, sloStanding } from './detect.js'
import { analyze } from './analyze.js'
import { loadEvidence } from './evidence.js'
import { loadInbound, loadInboundTimeline } from './inbound.js'
import { narrate } from './narrative.js'
import { workloadForId } from '../map/workload.js'

export const LOOKBACKS = [
  { label: '6 hours', seconds: 6 * 3600 },
  { label: '12 hours', seconds: 12 * 3600 },
  { label: '24 hours', seconds: 24 * 3600 },
]
const STEP = 60

/** A short stable name for an incident, the same every time it is computed: 8 base-36 characters of a hash of the application and the start. */
export function incidentId(app, start) {
  let h = 2166136261
  for (const ch of `${app}|${Math.round(start)}`) {
    h ^= ch.charCodeAt(0)
    h = Math.imul(h, 16777619) >>> 0
  }
  return h.toString(36).padStart(8, '0')
}

const maxBurn = (app, bad, objective, from, to, steps, c) => {
  let m = 0
  for (let i = from; i <= to; i++) m = Math.max(m, burnRate(app.total, bad, i, steps, STEP, objective, c.minRequests))
  return m
}

/**
 * The incidents of the last `seconds`, newest first: [{id, app, label, kind, start, end, open, slo}] with `slo` the standing of each
 * objective over the incident ({objective, compliance, burn1h, burn5m} or null when it cannot be said). `appConfig` is {application id:
 * {availability, latency}}: objectives of one application that differ from `config`'s.
 * -> {incidents, from, to, hasData, warnings}.
 */
export async function listIncidents(client, { seconds = LOOKBACKS[0].seconds, now = () => Date.now(), config = {}, appConfig = {} } = {}) {
  const c = { ...SLO_DEFAULTS, ...config }
  const end = Math.floor(now() / 1000 / STEP) * STEP
  const start = end - seconds
  const tl = await loadInboundTimeline(client, start, end, { step: STEP, thresholdSeconds: c.latencySeconds })
  const warnings = []
  if (tl.truncated) warnings.push('The request metrics returned more series than one call allows, so some applications are missing; use a shorter lookback.')
  if (tl.has && tl.bound === null) warnings.push('The project has no request-duration histogram, so only failed requests are judged, not slow ones.')
  else if (tl.bound !== null && tl.bound !== c.latencySeconds) warnings.push(`The latency objective is ${c.latencySeconds * 1000} ms, but the agent's histogram has no such bound; ${tl.bound * 1000} ms is used.`)
  const shortN = Math.max(Math.round(c.shortSeconds / STEP), 1)
  const longN = Math.max(Math.round(c.longSeconds / STEP), shortN)
  const incidents = []
  for (const a of tl.apps.values()) {
    const own = Object.prototype.hasOwnProperty.call(appConfig, a.id) ? { ...c, ...appConfig[a.id] } : c
    for (const inc of findIncidents(a, tl, own)) {
      const [first, last] = inc.steps
      const standing = sloStanding(a, tl, first, last, own)
      const slo = (kind, bad, objective) =>
        standing[kind].compliance === null
          ? null
          : { objective, compliance: standing[kind].compliance, burn1h: burnRate(a.total, bad, last, longN, STEP, objective, c.minRequests), burn5m: maxBurn(a, bad, objective, first, last, shortN, c) }
      incidents.push({
        id: incidentId(a.id, inc.start),
        app: a.id,
        label: (workloadForId(a.id) || { label: a.id }).label,
        kind: inc.kind,
        start: inc.start,
        end: inc.end,
        open: inc.open,
        slo: { availability: slo('availability', a.errors, own.availability), latency: a.slow ? slo('latency', a.slow, own.latency) : null, latencySeconds: tl.bound ?? c.latencySeconds, burnThreshold: c.burnThreshold },
      })
    }
  }
  incidents.sort((x, y) => y.start - x.start || x.app.localeCompare(y.app))
  return { incidents, from: start * 1000, to: end * 1000, hasData: !!tl.has, warnings }
}

/** The two windows an incident is judged over: during it, and the hour (at least) before it, leaving out the minutes the trouble was starting. */
export function windowsFor(incident, { minDuring = 180, gap = 120, minBefore = 3600 } = {}) {
  const during = [incident.start, Math.max(incident.end, incident.start + minDuring)]
  const length = Math.max(minBefore, 3 * (during[1] - during[0]))
  const beforeEnd = incident.start - gap
  return { during, before: [beforeEnd - length, beforeEnd] }
}

async function snapshot(client, [start, end]) {
  const seconds = Math.max(Math.round(end - start), 60)
  const [apps, inbound] = await Promise.all([loadApplications(client, { seconds, now: () => end * 1000 }), loadInbound(client, start, end)])
  return {
    snap: { graph: apps.graph || { nodes: [], edges: [] }, apps: new Map(apps.apps.map((a) => [a.id, a])), inbound: inbound.apps },
    seconds,
    warnings: [...apps.warnings, ...(inbound.truncated ? ['The request metrics were cut short, so the figures for this window are partial.'] : [])],
  }
}

/** The full analysis of one incident (from listIncidents): {incident, findings, narrative, evidence, windows, warnings}. `deps` is for tests. */
export async function loadIncident(client, incident, { config = {}, deps = {} } = {}) {
  const get = deps.snapshot || snapshot
  const getEvidence = deps.evidence || loadEvidence
  const w = windowsFor(incident)
  const [during, before] = await Promise.all([get(client, w.during), get(client, w.before)])
  const findings = analyze({ app: incident.app, before: before.snap, during: during.snap, seconds: { before: before.seconds, during: during.seconds }, config })
  const evidence = await getEvidence(client, { app: incident.app, cause: findings.rootCause, start: incident.start, end: incident.end, thresholdMs: ((incident.slo && incident.slo.latencySeconds) || SLO_DEFAULTS.latencySeconds) * 1000 })
  return {
    incident,
    findings,
    narrative: narrate(incident, findings),
    evidence,
    windows: w,
    warnings: [...new Set([...during.warnings, ...before.warnings, ...evidence.warnings])],
  }
}
