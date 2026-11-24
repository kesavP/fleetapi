// How long incidents take (GET .../incidents/stats) and one incident's instant summary (GET .../incidents/{id}/summary), as the pages show them.
//
// Both are worked out by the hub from what it holds; nothing here calls a model. As with the rest of the recorded incidents, everything the service
// sends is checked on the way in: a number that is not one is left out, a finding of a kind this page does not know is shown as plain text.

import { validId } from './recorded'

const isObject = (v) => v !== null && typeof v === 'object' && !Array.isArray(v)
const str = (v) => (typeof v === 'string' ? v : '')
const num = (v) => (Number.isFinite(v) && v >= 0 ? v : null)

export const STATS_DAYS = [7, 30, 90]
export const FINDING_KINDS = ['rollout', 'kubernetes', 'process']
const ENTRY_SOURCES = ['incident', ...FINDING_KINDS]

/** A number of seconds as people say it: 45 s, 12 min, 3 h 20 min, 2 d 4 h; '–' for none. */
export function duration(sec) {
  if (!Number.isFinite(sec) || sec < 0) return '–'
  const s = Math.floor(sec)
  if (s < 60) return `${s} s`
  if (s < 3600) return `${Math.floor(s / 60)} min`
  if (s < 86400) {
    const m = Math.floor((s % 3600) / 60)
    return m ? `${Math.floor(s / 3600)} h ${m} min` : `${Math.floor(s / 3600)} h`
  }
  const h = Math.floor((s % 86400) / 3600)
  return h ? `${Math.floor(s / 86400)} d ${h} h` : `${Math.floor(s / 86400)} d`
}

/** An incident's timing (detect, acknowledge, mitigate, resolve), each a number of seconds or null. */
export function timingOf(t) {
  const o = isObject(t) ? t : {}
  return { detect: num(o.detect_seconds), acknowledge: num(o.acknowledge_seconds), mitigate: num(o.mitigate_seconds), resolve: num(o.resolve_seconds) }
}

function durationsOf(d) {
  const o = isObject(d) ? d : {}
  return { count: num(o.count) ?? 0, mean: num(o.mean_seconds), median: num(o.median_seconds), p90: num(o.p90_seconds), max: num(o.max_seconds) }
}

/** The stats of a window, as the page shows them. */
export function statsOf(r) {
  const o = isObject(r) ? r : {}
  const bySeverity = {}
  if (isObject(o.mttr_by_severity)) for (const sev of ['critical', 'warning', 'info']) if (o.mttr_by_severity[sev]) bySeverity[sev] = durationsOf(o.mttr_by_severity[sev])
  return {
    from: str(o.from), to: str(o.to),
    opened: num(o.opened) ?? 0, resolved: num(o.resolved) ?? 0, notCounted: num(o.not_counted) ?? 0, stillOpen: num(o.still_open) ?? 0, reopened: num(o.reopened) ?? 0,
    mttd: durationsOf(o.mttd), mtta: durationsOf(o.mtta), mttm: durationsOf(o.mttm), mttr: durationsOf(o.mttr), bySeverity,
    partial: o.partial === true,
  }
}

/** One incident's instant summary, as the page shows it. */
export function summaryOf(r) {
  const o = isObject(r) ? r : {}
  const findings = (Array.isArray(o.findings) ? o.findings : []).filter(isObject).map((f) => ({
    at: str(f.at), kind: FINDING_KINDS.includes(f.kind) ? f.kind : 'other', text: str(f.text), score: num(f.score) ?? 0,
  })).filter((f) => f.text)
  const timeline = (Array.isArray(o.timeline) ? o.timeline : []).filter(isObject).map((e) => ({
    at: str(e.at), source: ENTRY_SOURCES.includes(e.source) ? e.source : 'other', text: str(e.text),
  })).filter((e) => e.text)
  return {
    headline: str(o.headline),
    elapsed: num(o.elapsed_seconds),
    timing: timingOf(o.timing),
    from: str(o.from), to: str(o.to),
    findings, timeline,
    gaps: (Array.isArray(o.gaps) ? o.gaps : []).filter((g) => typeof g === 'string' && g),
  }
}

/** The project's stats over the last `days` (7, 30 or 90). A failure is thrown, as the client words it. */
export async function loadStats(client, days = 7) {
  const d = STATS_DAYS.includes(days) ? days : 7
  return statsOf(await client.get('incidents/stats', { days: d }))
}

/** One incident's instant summary. */
export async function loadSummary(client, id) {
  if (!validId(id)) throw new Error(`"${String(id).slice(0, 40)}" is not an incident id.`)
  return summaryOf(await client.get(`incidents/${id}/summary`))
}
