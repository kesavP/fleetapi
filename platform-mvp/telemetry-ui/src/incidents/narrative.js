// The words of an incident, made from its findings (analyze.js) with fixed sentence shapes. Nothing is inferred here: every number and
// name in the text is in the findings, so the text can be checked against them, and the same findings can be given to something else to
// word differently (a language model, say) without that thing being the only source of the facts.
//
// Names and numbers are wrapped in `backticks` for the page to show as code; see `segments`.

import { workloadForId } from '../map/workload.js'

const nameOf = (id) => {
  const w = workloadForId(id)
  return w ? w.name : String(id)
}
const code = (id) => '`' + nameOf(id) + '`'

/** 0.4 -> "0.4 ms", 210 -> "210 ms", 1500 -> "1.5 s", 90000 -> "90 s". Milliseconds in. */
export function formatMs(ms) {
  if (ms == null || !Number.isFinite(ms)) return '–'
  if (ms >= 10000) return `${Math.round(ms / 1000)} s`
  if (ms >= 1000) return `${Math.round(ms / 100) / 10} s`
  if (ms >= 100) return `${Math.round(ms)} ms`
  if (ms >= 10) return `${Math.round(ms * 10) / 10} ms`
  return `${Math.round(ms * 100) / 100} ms`
}
const times = (r) => (r && r >= 1.5 ? ` (${r >= 10 ? Math.round(r) : Math.round(r * 10) / 10}x)` : '')
const pct = (n) => `${Math.round(n * 10) / 10}%`

/** The short label of an issue, as the graph lists it under an application. */
export function issueLabel(i) {
  if (i.scope === 'edge') {
    const to = nameOf(i.to)
    return { rtt: `TCP network latency to ${to}`, connect: `TCP connection latency to ${to}`, latency: `Latency of calls to ${to}`, errors: `Errors calling ${to}` }[i.kind] || i.kind
  }
  return { errors: 'Errors', latency: 'Latency', cpu: 'CPU', logs: 'Log: errors', restarts: 'Restarts', oom: 'Out of memory' }[i.kind] || i.kind
}

/** One sentence of what changed, with the numbers. */
export function issueSentence(i) {
  const was = i.before
  const now = i.during
  switch (`${i.scope}:${i.kind}`) {
    case 'edge:rtt':
      return `The ${code(i.from)} → ${code(i.to)} network round-trip time rose from ${formatMs(was)} to ${formatMs(now)}${times(i.ratio)}.`
    case 'edge:connect':
      return `Opening a TCP connection from ${code(i.from)} to ${code(i.to)} took ${formatMs(now)} on average, against ${formatMs(was)} before${times(i.ratio)}.`
    case 'edge:latency':
      return `The p95 latency of ${code(i.from)} → ${code(i.to)} calls rose from ${formatMs(was)} to ${formatMs(now)}${times(i.ratio)}.`
    case 'edge:errors':
      return `${pct(now)} of ${code(i.from)} → ${code(i.to)} calls failed, against ${pct(was)} before.`
    case 'app:errors':
      return `${pct(now)} of the requests ${code(i.app)} served failed, against ${pct(was)} before.`
    case 'app:latency':
      return `The p95 latency of ${code(i.app)} rose from ${formatMs(was)} to ${formatMs(now)}${times(i.ratio)}.`
    case 'app:cpu':
      return i.unit === 'cores'
        ? `${code(i.app)} used ${now} CPU cores, against ${was} before${times(i.ratio)}.`
        : `${code(i.app)} used ${now}% of its CPU limit, against ${was}% before.`
    case 'app:logs':
      return `${code(i.app)} logged ${now} error lines in the incident, against ${was} in the period before.`
    case 'app:restarts':
      return `${code(i.app)} restarted ${now} time${now === 1 ? '' : 's'}.`
    case 'app:oom':
      return `${code(i.app)} was killed ${now} time${now === 1 ? '' : 's'} for running out of memory.`
    default:
      return `${code(i.app || i.from)}: ${i.kind} changed.`
  }
}

/** What the incident looked like to its users, as the nouns the title uses. */
const symptom = (kinds) => (kinds.includes('availability') && kinds.includes('latency') ? 'errors/latency' : kinds.includes('latency') ? 'latency' : 'errors')

const CAUSE = {
  rtt: (r) => `increased network round-trip time between ${code(r.from)} and ${code(r.to)}`,
  connect: (r) => `slow TCP connections from ${code(r.from)} to ${code(r.to)}`,
  latency: (r) => (r.scope === 'edge' ? `slow calls from ${code(r.from)} to ${code(r.to)}` : `slowness in ${code(r.app)}`),
  errors: (r) => (r.scope === 'edge' ? `failing calls from ${code(r.from)} to ${code(r.to)}` : `errors in ${code(r.app)}`),
  cpu: (r) => `${code(r.app)} running out of CPU`,
  oom: (r) => `${code(r.app)} being killed for running out of memory`,
  restarts: (r) => `${code(r.app)} restarting`,
  logs: (r) => `errors in ${code(r.app)}`,
}

/**
 * -> {title, summary[], propagation, path, causeSentence, causeLabel}.
 * `incident` is {app, kind: ['availability'|'latency'...]}; `findings` is what analyze() returned.
 */
export function narrate(incident, findings) {
  const app = code(incident.app)
  const what = symptom(incident.kind || ['availability'])
  const root = findings.rootCause
  const own = findings.issues.filter((i) => i.scope === 'app' && i.app === incident.app)

  if (!root) {
    const looked = `${findings.checked.applications} application${findings.checked.applications === 1 ? '' : 's'} and ${findings.checked.calls} call${findings.checked.calls === 1 ? '' : 's'} downstream of ${app} were compared with the hour before, on ${findings.checked.signals.join(', ')}.`
    return {
      title: `${app} ${what}: no cause found among its dependencies.`,
      summary: [
        `${looked} Nothing changed enough to explain it.`,
        ...(own.length ? [`What changed in ${app} itself: ${own.map(issueSentence).join(' ')}`] : []),
      ],
      propagation: null,
      path: [incident.app],
      causeLabel: null,
    }
  }

  const cause = CAUSE[root.kind](root)
  const hedge = root.confidence === 'possible' ? 'possibly ' : ''
  const via = root.path.slice(1, -1).map(code)
  const reached = code(root.path[root.path.length - 1])
  const trace = root.path.length > 1 ? ` trace back${via.length ? ` through ${list(via)}` : ''} to ${reached}` : ' come from the application itself'
  const first = `Elevated ${what.replace('errors/latency', 'latency and errors')} in ${app}${trace}. ${issueSentence(root.issue)}`
  const extra = findings.issues
    .filter((i) => i !== root.issue && i.scope === 'edge' && root.scope === 'edge' && i.from === root.from && i.to === root.to)
    .map(issueSentence)
  const summary = [[first, ...extra].join(' ')]
  if (root.confidence === 'possible') summary.push('Nothing in the network or in resources explains it, so this is the deepest call that got worse; treat it as a lead, not a finding.')

  const ownBits = own.map((i) => {
    if (i.kind === 'errors') return `${pct(i.during)} of requests failing`
    if (i.kind === 'latency') return `p95 ${formatMs(i.before)} → ${formatMs(i.during)}`
    return null
  }).filter(Boolean)
  const propagation = root.path.length > 1
    ? `The anomaly in ${app}${ownBits.length ? ` (${ownBits.join(', ')})` : ''} propagates down the dependency chain, not from ${app} itself.`
    : null
  return { title: `${app} ${what} ${hedge}caused by ${cause}.`, summary, propagation, path: root.path, causeLabel: cause.replace(/`/g, '') }
}

function list(xs) {
  return xs.length <= 1 ? xs.join('') : `${xs.slice(0, -1).join(', ')} and ${xs[xs.length - 1]}`
}

/** Splits text on `backticks` into [{text, code}] so the page can show the names as code without building HTML from a string. */
export function segments(text) {
  const out = []
  String(text ?? '').split('`').forEach((part, i) => {
    if (part) out.push({ text: part, code: i % 2 === 1 })
  })
  return out
}
