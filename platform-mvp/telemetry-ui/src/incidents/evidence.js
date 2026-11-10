// What an incident's findings rest on, from the other signals: the failed and the slow requests that went through the hurt
// application (traces), the error lines of it and of the cause (logs), and where a CPU cause spends its time (profiles).
// Each read is on its own: one that fails is a warning and the rest is shown.

import { shape } from '../alerts/report.js'
import { topFunctions, loadStacks } from '../api/profiles.js'

const CPU_TYPE = 'cpu:nanoseconds'

/** Error log lines as distinct messages: [{service, count, sample, first, last}], the most frequent first. */
export function summarizeLogs(rows, limit = 8) {
  const by = new Map()
  for (const r of Array.isArray(rows) ? rows : []) {
    if (!r || !r.body) continue
    const key = `${r.service}\n${shape(r.body)}`
    const t = Date.parse(r.time)
    const e = by.get(key) || { service: r.service, count: 0, sample: String(r.body), first: t, last: t }
    e.count++
    if (Number.isFinite(t)) {
      e.first = Math.min(e.first, t)
      e.last = Math.max(e.last, t)
    }
    by.set(key, e)
  }
  return [...by.values()].sort((a, b) => b.count - a.count || a.service.localeCompare(b.service) || a.sample.localeCompare(b.sample)).slice(0, limit)
}

/** The requests of the hurt application to show: root spans that failed, and root spans over the objective, each the worst few. */
export function summarizeSpans(rows, limit = 5) {
  const list = (Array.isArray(rows) ? rows : []).filter((r) => r && r.trace_id)
  const ms = (r) => Number(r.duration_ns) / 1e6
  const pick = (xs) => xs.slice(0, limit).map((r) => ({ traceId: r.trace_id, name: r.name, service: r.service, status: r.status_code, message: r.status_message || '', durationMs: ms(r), time: r.time }))
  return { failed: pick(list.filter((r) => r.status_code === 'ERROR').sort((a, b) => ms(b) - ms(a))), slow: pick([...list].sort((a, b) => ms(b) - ms(a))) }
}

/**
 * -> {traces: {failed, slow}, logs: [...], profile: {service, functions: [...]} | null, warnings[]}.
 * `apps` are the application ids whose logs to read: the hurt one and whatever the cause is in. `cause` is the findings' rootCause.
 */
export async function loadEvidence(client, { app, cause, start, end, thresholdMs = 500 }) {
  const range = { from: Math.floor(start), to: Math.ceil(end) }
  const apps = [...new Set([app, ...(cause ? [cause.scope === 'edge' ? cause.to : cause.app] : [])])]
  const jobs = {
    spans: () => client.get('traces', { ...range, service: app, root: true, min_duration_ms: Math.round(thresholdMs), limit: 200 }),
    failed: () => client.get('traces', { ...range, service: app, root: true, status: 'error', limit: 200 }),
    logs: async () => (await Promise.all(apps.map((s) => client.get('logs', { ...range, service: s, severity: 'ERROR', limit: 200 })))).flat(),
  }
  const wantsProfile = cause && cause.scope === 'app' && cause.kind === 'cpu'
  if (wantsProfile) jobs.profile = () => loadStacks(client, { service: cause.app, type: CPU_TYPE }, { seconds: Math.max(Math.ceil(end - start), 60), now: () => end * 1000 })
  const names = Object.keys(jobs)
  const settled = await Promise.allSettled(names.map((k) => jobs[k]()))
  const got = {}
  const warnings = []
  settled.forEach((r, i) => {
    if (r.status === 'fulfilled') got[names[i]] = r.value
    else warnings.push(`${names[i] === 'spans' || names[i] === 'failed' ? 'traces' : names[i]}: ${r.reason && r.reason.message ? r.reason.message : 'failed'}`)
  })
  const slow = summarizeSpans(got.spans).slow
  const failed = summarizeSpans(got.failed).failed
  let profile = null
  if (wantsProfile && Array.isArray(got.profile) && got.profile.length) {
    const top = topFunctions(got.profile, 5)
    profile = { service: cause.app, total: top.all, functions: top.functions }
  }
  return { traces: { failed, slow }, logs: summarizeLogs(got.logs), profile, warnings }
}
