import { RULES } from './rules'
import { formatDuration, severityClass } from '../api/serviceStats'
import { formatBytes } from '../format'

// The text a person pastes into Claude (or any assistant) to work out why a service is not serving. Nothing here calls an AI
// service: it only gathers what the telemetry shows about the alert and arranges it so the cause can be reasoned about.
//
// The text is for a third party, so it is shaped to be small (similar log lines and failing spans are grouped and counted,
// bodies are cut) and scrubbed on a best-effort basis (see redact). Telemetry can hold things nobody meant to share, a SQL
// statement or a URL with a token in it, so the page tells the reader to look before pasting.

export const MAX_TEXT = 300 // characters of one log body or status message
export const MAX_GROUPS = 12 // failing-span groups and log groups

const SECRET_KEY = /(pass(?:word|wd)?|secret|token|api[_-]?key|authorization|auth|credential|private[_-]?key|session)/i

/** Best-effort scrubbing of the usual ways a secret ends up in a line. Not a guarantee: it cannot know what is sensitive. */
export function redact(text) {
  return String(text ?? '')
    .replace(/(\b(?:Bearer|Basic)\s+)[A-Za-z0-9._~+/=-]{8,}/g, '$1[redacted]')
    .replace(/(:\/\/)[^\s/@:]+:[^\s/@]+@/g, '$1[redacted]@') // user:password@host in a URL
    .replace(new RegExp(`(["']?${SECRET_KEY.source}["']?\\s*[:=]\\s*)("[^"]*"|'[^']*'|[^\\s,;&"']+)`, 'gi'), (_, lead) => `${lead}[redacted]`)
    .replace(/\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]*/g, '[redacted jwt]')
    .replace(/\b(?:AKIA|ASIA)[A-Z0-9]{16}\b/g, '[redacted aws key]')
}

const clip = (s, n = MAX_TEXT) => {
  const t = redact(s).replace(/\s+/g, ' ').trim()
  return t.length > n ? t.slice(0, n - 1) + '…' : t
}
const fmtTime = (ms) => new Date(ms).toISOString().replace('.000Z', 'Z')
const rate = (x) => (x == null ? '–' : x >= 100 ? String(Math.round(x)) : String(Math.round(x * 100) / 100))
const cell = (s) => String(s).replace(/\|/g, '\\|')

/** "took 1234 ms for user 8841" -> "took # ms for user #": lines that differ only in numbers or ids are one kind of line. */
export function shape(text) {
  return clip(text, 160)
    .replace(/\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b/gi, '<uuid>')
    .replace(/\b[0-9a-f]{16,}\b/gi, '<hex>')
    .replace(/\b\d+(?:\.\d+)?\b/g, '#')
}

/** Groups failing spans by what they were and how they failed: [{name, message, service, count, meanNs, lastTime, traceId}]. */
export function groupFailingSpans(spans) {
  const by = new Map()
  for (const s of spans.filter((x) => x.status_code === 'ERROR')) {
    const key = `${s.service}\n${s.name}\n${shape(s.status_message || '')}`
    const g = by.get(key) || { name: s.name, message: clip(s.status_message || ''), service: s.service, count: 0, totalNs: 0, lastTime: 0, traceId: s.trace_id }
    g.count++
    g.totalNs += Number(s.duration_ns) || 0
    const t = Date.parse(s.time)
    if (t > g.lastTime) {
      g.lastTime = t
      g.traceId = s.trace_id
    }
    by.set(key, g)
  }
  return [...by.values()]
    .map((g) => ({ ...g, meanNs: g.count ? g.totalNs / g.count : 0 }))
    .sort((a, b) => b.count - a.count || b.lastTime - a.lastTime)
    .slice(0, MAX_GROUPS)
}

/** Groups error and warning log lines by their shape: [{text, severity, count, first, last}], most frequent first. */
export function groupLogs(logs) {
  const by = new Map()
  for (const l of logs) {
    const cls = severityClass(l)
    if (cls !== 'error' && cls !== 'warn') continue
    const key = `${cls}\n${shape(l.body)}`
    const t = Date.parse(l.time)
    const g = by.get(key) || { text: clip(l.body), severity: cls, count: 0, first: t, last: t }
    g.count++
    g.first = Math.min(g.first, t)
    g.last = Math.max(g.last, t)
    by.set(key, g)
  }
  return [...by.values()].sort((a, b) => b.count - a.count || b.last - a.last).slice(0, MAX_GROUPS)
}

function table(rows, header) {
  if (!rows.length) return '_none_\n'
  return [`| ${header.join(' | ')} |`, `| ${header.map(() => '---').join(' | ')} |`, ...rows.map((r) => `| ${r.map(cell).join(' | ')} |`)].join('\n') + '\n'
}

/**
 * The report, as Markdown. `context` is what was gathered for the alert: {project, now, spans, logs, traffic, resources,
 * errors} (any part may be missing: say so rather than leave it out silently), `alerts` the other alerts at the time.
 */
export function buildReport(alert, context, alerts = []) {
  const c = context
  const out = []
  const line = (s = '') => out.push(s)

  line(`# Why is ${alert.label} not serving? (incident analysis request)`)
  line()
  line(`Project: ${c.project} · Generated: ${fmtTime(c.now)} · All times UTC`)
  line()
  line('## The alert')
  line()
  line(`- **${RULES[alert.rule] || alert.rule}** (${alert.severity}) on \`${alert.service === '*' ? 'the whole project' : alert.service}\``)
  line(`- Status: ${alert.status}, since ${fmtTime(alert.firstSeen)}${alert.resolvedAt ? `, resolved ${fmtTime(alert.resolvedAt)}` : ''}`)
  line(`- ${alert.summary}`)
  line('- Evidence:')
  for (const [k, v] of Object.entries(alert.evidence || {})) {
    if (Array.isArray(v)) {
      line(`  - ${k}: ${v.length ? v.map((x) => `${x.label} (${x.rate}/s)`).join(', ') : 'none'}`)
    } else {
      line(`  - ${k}: ${v}`)
    }
  }

  const others = alerts.filter((a) => a.id !== alert.id && a.status !== 'resolved').sort((a, b) => a.firstSeen - b.firstSeen)
  line()
  line('## Other alerts open at the same time (earliest first: a cause usually shows up before its effects)')
  line()
  line(others.length ? others.map((a) => `- ${fmtTime(a.firstSeen)} ${a.severity} **${RULES[a.rule] || a.rule}** ${a.label}: ${a.summary}`).join('\n') : '_none_')

  line()
  line('## Who calls it and whom it calls (last window)')
  line()
  if (c.traffic) {
    const row = (e) => [e.peerLabel, e.protocols.length ? e.protocols.join(',') : 'tcp', e.rps != null ? `${rate(e.rps)} ${e.unit}` : `${rate(e.connPerSec)} conn/s`, `${rate(e.errPct)}% (${e.errBasis})`, e.p95ms != null ? `${e.p95ms} ms` : '–']
    line('Called by:')
    line()
    line(table(c.traffic.inbound.map(row), ['caller', 'protocol', 'rate', 'errors', 'p95']))
    line('Calls out to:')
    line()
    line(table(c.traffic.outbound.map(row), ['target', 'protocol', 'rate', 'errors', 'p95']))
  } else {
    line('_not available_' + (c.errors && c.errors.metrics ? `: ${c.errors.metrics}` : ''))
  }

  line()
  line('## Resources')
  line()
  const r = c.resources
  if (r && Object.values(r).some((v) => v !== null)) {
    const kv = (k, v) => (v == null ? null : `- ${k}: ${v}`)
    line(
      [
        kv('instances reporting', r.instances),
        kv('CPU used (mean)', r.cpuCores != null ? `${rate(r.cpuCores)} cores${r.cpuLimitCores ? ` of ${rate(r.cpuLimitCores)} limit` : ''}` : null),
        kv('memory in use', r.memoryBytes != null ? `${formatBytes(r.memoryBytes)}${r.memoryLimitBytes ? ` of ${formatBytes(r.memoryLimitBytes)} limit` : ''}` : null),
        kv('restarts in window', r.restarts),
        kv('OOM kills in window', r.oomKills),
      ]
        .filter(Boolean)
        .join('\n'),
    )
  } else {
    line('_none reported for this service in the window_')
  }

  line()
  line('## Failing spans (grouped)')
  line()
  const failing = groupFailingSpans(c.spans || [])
  line(
    failing.length
      ? table(failing.map((g) => [g.name, g.message || '–', g.count, formatDuration(g.meanNs), fmtTime(g.lastTime), g.traceId.slice(0, 8)]), ['span', 'status message', 'count', 'mean duration', 'last seen', 'trace'])
      : '_no failing spans recorded for this service in the window_\n',
  )

  line('## Error and warning log lines (grouped; numbers and ids replaced by # so similar lines count together)')
  line()
  const logs = groupLogs(c.logs || [])
  line(logs.length ? table(logs.map((g) => [g.severity, g.count, `${fmtTime(g.first)} – ${fmtTime(g.last)}`, g.text]), ['level', 'count', 'between', 'line']) : '_no error or warning lines for this service in the window_\n')

  line('## How to read this')
  line()
  line('- The node-agent records each request as its own span with no parent link, so spans cannot be followed from one service to the next; use the call tables above for who depends on whom.')
  line(`- Span and log figures cover the newest ${c.limit || 1000} rows in the window; a busy service may have more.`)
  line('- "No traffic" can mean the service is down or that its callers are; check the callers listed above and the other alerts.')
  line('- Error share counts a failed connection as a failed call.')
  if (c.errors && Object.keys(c.errors).length) line(`- Not loaded: ${Object.entries(c.errors).map(([k, v]) => `${k} (${v})`).join('; ')}.`)

  line()
  line('## What I would like from you')
  line()
  line('Using only the data above, give me: (1) the most likely root causes, ranked, with the evidence for and against each; (2) what the data cannot tell us and the single most useful thing to check next (a command, a log, a dashboard); (3) whether this looks like a problem in this service, in something it depends on, or in something that calls it. Say plainly where you are guessing.')
  return out.join('\n')
}
