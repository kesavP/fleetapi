// What a service's page shows, worked out from the spans and logs the API returns for it.
//
// The API answers newest-first and at most `limit` rows. When a window holds more than that, the answer covers
// only the latest part of it, and the numbers here describe that part, not the whole window. `truncated` and
// `coveredFrom` say so, and the page shows it. Nothing here is extrapolated.

export const SPAN_LIMIT = 1000
export const LOG_LIMIT = 1000

/** Nearest-rank percentile (p in 0..100) of an ascending array; null when empty. */
export function percentile(sorted, p) {
  if (!sorted.length) return null
  const rank = Math.max(1, Math.ceil((p / 100) * sorted.length))
  return sorted[Math.min(rank, sorted.length) - 1]
}

/** 1500000 -> "1.5 ms"; input in nanoseconds. */
export function formatDuration(ns) {
  if (ns == null) return '–'
  if (ns < 1e3) return `${Math.round(ns)} ns`
  if (ns < 1e6) return `${trim(ns / 1e3)} µs`
  if (ns < 1e9) return `${trim(ns / 1e6)} ms`
  return `${trim(ns / 1e9)} s`
}
const trim = (x) => (x >= 100 ? String(Math.round(x)) : String(Math.round(x * 10) / 10))

const GROUPS = ['server', 'client', 'other']
const groupOf = (kind) => (kind === 'SERVER' ? 'server' : kind === 'CLIENT' ? 'client' : 'other')

function summarize(spans) {
  const durations = spans.map((s) => Number(s.duration_ns) || 0).sort((a, b) => a - b)
  const errors = spans.filter((s) => s.status_code === 'ERROR').length
  return {
    count: spans.length,
    errors,
    errorShare: spans.length ? errors / spans.length : 0,
    p50: percentile(durations, 50),
    p95: percentile(durations, 95),
    p99: percentile(durations, 99),
  }
}

/**
 * Spans (newest first) -> {total, truncated, from, to, groups, primary, timeline}. `from` and `to` (ms) are what
 * the numbers cover: the window, or just its newest part when `limit` rows came back. Spans are grouped by kind
 * because a call is a CLIENT span in the caller and a SERVER span in the callee: mixed together they count twice.
 * `primary` is the group the timeline shows: SERVER when there is any, else CLIENT, else the rest.
 */
export function summarizeSpans(spans, { from, to, limit = SPAN_LIMIT, buckets = 30 }) {
  const list = Array.isArray(spans) ? spans : []
  const truncated = list.length >= limit
  const times = list.map((s) => Date.parse(s.time)).filter((t) => Number.isFinite(t))
  // With a full page the oldest span marks where the answer stops; the part before it was not returned.
  const coveredFrom = truncated && times.length ? Math.max(from, Math.min(...times)) : from

  const groups = {}
  for (const g of GROUPS) {
    const part = list.filter((s) => groupOf(s.kind) === g)
    if (part.length) groups[g] = summarize(part)
  }
  const primary = GROUPS.find((g) => groups[g]) || null

  const width = (to - coveredFrom) / buckets
  const timeline = Array.from({ length: buckets }, (_, i) => ({ t: coveredFrom + i * width, count: 0, errors: 0 }))
  if (primary && width > 0) {
    for (const s of list) {
      if (groupOf(s.kind) !== primary) continue
      const t = Date.parse(s.time)
      if (!(t >= coveredFrom && t <= to)) continue
      const b = timeline[Math.min(buckets - 1, Math.floor((t - coveredFrom) / width))]
      b.count++
      if (s.status_code === 'ERROR') b.errors++
    }
  }
  return { total: list.length, truncated, from: coveredFrom, to, groups, primary, timeline }
}

/** The severity class of a log record: its number when it has one, else its text. */
export function severityClass(log) {
  const n = Number(log.severity_number) || 0
  if (n >= 17) return 'error'
  if (n >= 13) return 'warn'
  if (n >= 9) return 'info'
  if (n >= 1) return 'debug'
  const t = String(log.severity || '').toUpperCase()
  if (/ERR|FATAL|CRIT/.test(t)) return 'error'
  if (/WARN/.test(t)) return 'warn'
  if (/INFO/.test(t)) return 'info'
  if (/DEBUG|TRACE/.test(t)) return 'debug'
  return 'other'
}

export function summarizeLogs(logs, { limit = LOG_LIMIT } = {}) {
  const list = Array.isArray(logs) ? logs : []
  const bySeverity = { error: 0, warn: 0, info: 0, debug: 0, other: 0 }
  for (const l of list) bySeverity[severityClass(l)]++
  return { total: list.length, truncated: list.length >= limit, bySeverity }
}
