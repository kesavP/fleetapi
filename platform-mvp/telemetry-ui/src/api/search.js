// Searching spans and logs, and reading one trace. The query builders and the waterfall are pure.
//
// What the API matches: a span name and a log severity are exact (the severity is upper-cased by the server), the log text
// is a case-insensitive substring, a trace id is exactly 32 hex characters. Answers come newest first, at most `limit`
// rows, and there is no way to ask for the next page, so a full page means "the newest N", and the views say so.

const MIN_BAR = 0.4 // percent: the least a bar shows
export const SPAN_LIMITS = [100, 500, 1000]
export const LOG_LIMITS = [100, 500, 1000]

export const validTraceId = (id) => /^[0-9a-f]{32}$/.test(String(id || '').toLowerCase())

const put = (o, k, v) => {
  if (v !== undefined && v !== null && String(v).trim() !== '') o[k] = typeof v === 'string' ? v.trim() : v
}

/** The problem with a filter set, or '' when it is fine. */
export function filterProblem({ minDurationMs, traceId } = {}) {
  if (minDurationMs !== undefined && String(minDurationMs).trim() !== '' && !/^\d+$/.test(String(minDurationMs).trim())) {
    return 'The minimum duration is a whole number of milliseconds.'
  }
  if (traceId && String(traceId).trim() && !validTraceId(String(traceId).trim())) return 'A trace id is 32 hex characters.'
  return ''
}

/** Query parameters for a span search: empty filters are left out. */
export function spanParams(f, { from, to }) {
  const p = { from, to }
  put(p, 'service', f.service)
  put(p, 'name', f.name)
  put(p, 'status', f.status)
  put(p, 'min_duration_ms', f.minDurationMs)
  if (f.rootOnly) p.root = true
  p.limit = f.limit || 100
  return p
}

/** Query parameters for a log search. */
export function logParams(f, { from, to }) {
  const p = { from, to }
  put(p, 'service', f.service)
  put(p, 'severity', f.severity)
  put(p, 'q', f.q)
  if (f.traceId && f.traceId.trim()) p.trace_id = f.traceId.trim().toLowerCase()
  p.limit = f.limit || 100
  return p
}

export const windowRange = (seconds, now = () => Date.now()) => {
  const to = Math.floor(now() / 1000)
  return { from: to - seconds, to }
}

export async function searchSpans(client, filters, { seconds, now } = {}) {
  const rows = await client.get('traces', spanParams(filters, windowRange(seconds, now)))
  return Array.isArray(rows) ? rows : []
}

export async function searchLogs(client, filters, { seconds, now } = {}) {
  const rows = await client.get('logs', logParams(filters, windowRange(seconds, now)))
  return Array.isArray(rows) ? rows : []
}

export async function getTrace(client, id) {
  const rows = await client.get(`traces/${String(id).toLowerCase()}`)
  return Array.isArray(rows) ? rows : []
}

/** The names a filter can offer: services with traces or logs in the window. Either source failing leaves the other. */
export async function serviceNames(client, { seconds, now } = {}) {
  const range = windowRange(seconds, now)
  const settled = await Promise.allSettled([client.get('traces/services', range), client.get('logs/services', range)])
  const names = new Set()
  for (const r of settled) if (r.status === 'fulfilled' && Array.isArray(r.value)) r.value.forEach((n) => n && names.add(n))
  return [...names].sort((a, b) => a.localeCompare(b))
}

/**
 * The rows of a trace as a waterfall. A span sits under its parent when the parent is in the trace; the rest are roots, so
 * a trace with no parent links (the node-agent records each request as its own span) is a flat list by start time.
 * `left` and `width` are percentages of the trace's span of time; a span too short to see keeps a visible sliver.
 * Times are read to the millisecond (the text carries nanoseconds, the browser keeps milliseconds), so sub-millisecond
 * ordering between spans is not guaranteed; the durations are exact.
 */
export function buildWaterfall(spans) {
  const list = (Array.isArray(spans) ? spans : []).map((s) => {
    const start = Date.parse(s.time)
    return { s, start: Number.isFinite(start) ? start : 0, ms: (Number(s.duration_ns) || 0) / 1e6 }
  })
  if (!list.length) return { rows: [], startMs: 0, totalMs: 0, linked: false }
  const t0 = Math.min(...list.map((x) => x.start))
  const t1 = Math.max(...list.map((x) => x.start + x.ms))
  const total = Math.max(t1 - t0, 0.001)

  const byId = new Map(list.map((x) => [x.s.span_id, x]))
  const kids = new Map()
  const roots = []
  for (const x of list) {
    const parent = x.s.parent_span_id
    if (parent && parent !== x.s.span_id && byId.has(parent)) {
      if (!kids.has(parent)) kids.set(parent, [])
      kids.get(parent).push(x)
    } else {
      roots.push(x)
    }
  }
  const order = (a, b) => a.start - b.start || String(a.s.name).localeCompare(String(b.s.name))
  const rows = []
  const seen = new Set()
  const walk = (x, depth) => {
    if (seen.has(x)) return
    seen.add(x)
    const left = Math.min(100 - MIN_BAR, ((x.start - t0) / total) * 100) // room is kept for the sliver of an instant span at the end
    const width = Math.min(100 - left, Math.max((x.ms / total) * 100, MIN_BAR))
    rows.push({ span: x.s, depth, left, width })
    for (const k of (kids.get(x.s.span_id) || []).sort(order)) walk(k, depth + 1)
  }
  roots.sort(order).forEach((x) => walk(x, 0))
  list.filter((x) => !seen.has(x)).sort(order).forEach((x) => walk(x, 0)) // a parent cycle in bad data: still shown
  return { rows, startMs: t0, totalMs: total, linked: kids.size > 0 }
}
