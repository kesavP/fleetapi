// The incidents the HUB has recorded (GET .../incidents, .../incidents/{id}, .../incidents/{id}/events), as the pages show them.
//
// They are not the ones the Incidents page works out for itself in the browser: those come and go with the metrics and the rules saved in this
// browser, while these are written by the service's detector (or by a person, through the API), have a state that people move on, a timeline and
// evidence, and stay until closed. This file only READS them: the UI's proxy lets nothing but GET through to incidents, because changing one
// takes a manage key, and the key a browser holds can look and never touch.
//
// Everything the service sends is checked on the way in. A state or severity it does not know is shown as the safe default, never as free text
// that a page could use as a class name; an address in a piece of evidence becomes a link only when it is http(s).

export const STATES = ['open', 'acknowledged', 'investigating', 'mitigated', 'resolved', 'closed']
export const SEVERITIES = ['critical', 'warning', 'info']
export const DEFAULT_LIMIT = 50
const EVENT_LIMIT = 500

const ID = /^[a-z0-9]{8}$/
/** The service's incident ids are 8 lower-case letters and digits. Checked before an id is put into a path. */
export const validId = (id) => typeof id === 'string' && ID.test(id)

const isObject = (v) => v !== null && typeof v === 'object' && !Array.isArray(v)
const str = (v) => (typeof v === 'string' ? v : '')

function evidenceOf(e) {
  if (!isObject(e) || typeof e.id !== 'string' || !isObject(e.ref) || typeof e.ref.kind !== 'string') return null
  return { id: e.id, ref: e.ref, addedBy: str(e.added_by), addedAt: str(e.added_at) }
}

/** One incident as the pages use it, or null for a row that is not one. */
export function incidentOf(r) {
  if (!isObject(r) || !validId(r.id) || typeof r.title !== 'string' || !r.title) return null
  return {
    id: r.id,
    title: r.title,
    app: str(r.app),
    severity: SEVERITIES.includes(r.severity) ? r.severity : 'info',
    state: STATES.includes(r.state) ? r.state : 'open',
    source: r.source === 'detected' || r.source === 'manual' ? r.source : '',
    summary: str(r.summary),
    assignee: str(r.assignee),
    resolution: str(r.resolution),
    startedAt: str(r.started_at),
    createdAt: str(r.created_at),
    updatedAt: str(r.updated_at),
    resolvedAt: str(r.resolved_at),
    reopened: Number.isFinite(r.reopened) ? r.reopened : 0,
    version: Number.isFinite(r.version) ? r.version : 0,
    evidence: (Array.isArray(r.evidence) ? r.evidence : []).map(evidenceOf).filter(Boolean),
    allowed: (Array.isArray(r.allowed_transitions) ? r.allowed_transitions : []).filter((s) => STATES.includes(s)),
  }
}

const UPDATED_FIELDS = ['title', 'severity', 'summary', 'assignee']

/** One line of the timeline: {seq, when, actor, text}. */
export function describeEvent(ev) {
  const e = isObject(ev) ? ev : {}
  const d = isObject(e.data) ? e.data : {}
  let text
  switch (e.type) {
    case 'created':
      text = 'Opened'
      break
    case 'state_changed':
      text = d.from && d.to ? `${d.from} → ${d.to}${d.resolution ? ` (${d.resolution})` : ''}${d.note ? `: ${d.note}` : ''}` : 'State changed'
      break
    case 'updated': {
      const changed = UPDATED_FIELDS.filter((f) => d[f] !== undefined)
      text = changed.length ? `Changed ${changed.join(', ')}` : 'Changed'
      break
    }
    case 'commented':
      text = d.text ? `Comment: ${d.text}` : 'Comment'
      break
    case 'evidence_added':
      text = isObject(d.ref) && d.ref.kind ? `Evidence added: ${d.ref.kind}` : 'Evidence added'
      break
    case 'evidence_removed':
      text = d.reason ? `Evidence removed: ${d.reason}` : 'Evidence removed'
      break
    default:
      text = typeof e.type === 'string' && e.type ? e.type : 'Event'
  }
  // `body` is a comment's text as it was written, with its line breaks, for the pages that show a comment as more than a line
  return { seq: Number.isFinite(e.seq) ? e.seq : 0, when: str(e.time), actor: str(e.actor) || 'system', kind: str(e.type), text, body: e.type === 'commented' ? str(d.text) : '' }
}

/** The newest comment claude made: the analysis the hub kept (see analyseIncident), or null. */
export function latestAnalysis(events) {
  const mine = (Array.isArray(events) ? events : []).filter((e) => e && e.actor === 'claude' && e.kind === 'commented' && e.body)
  return mine.length ? mine.reduce((a, b) => (b.seq >= a.seq ? b : a)) : null
}

const WEB = /^https?:\/\/\S+$/i

/** A piece of evidence: {label, text, to (a route, or null), href (an http(s) address, or null), note, addedBy}. */
export function describeEvidence(e) {
  const r = (e && e.ref) || {}
  const out = { label: str(r.kind) || 'Evidence', text: '', to: null, href: null, note: str(r.note), addedBy: str(e && (e.addedBy ?? e.added_by)) }
  switch (r.kind) {
    case 'application':
      out.label = 'Application'
      out.text = str(r.service)
      if (out.text) out.to = { name: 'application', params: { name: out.text } }
      break
    case 'trace':
      out.label = 'Trace'
      out.text = str(r.trace_id)
      if (out.text) out.to = { name: 'trace', params: { id: out.text } }
      break
    case 'logs': {
      out.label = 'Logs'
      const query = {}
      if (str(r.service)) query.service = r.service
      if (str(r.severity)) query.severity = r.severity
      if (str(r.query)) query.q = r.query
      out.text = [str(r.service), str(r.severity) && `severity ${r.severity}`, str(r.query) && `"${r.query}"`].filter(Boolean).join(' · ')
      out.to = { name: 'logs', query }
      break
    }
    case 'metric': {
      out.label = 'Metric'
      const match = isObject(r.match) ? Object.keys(r.match).sort().map((k) => `${k}="${r.match[k]}"`) : []
      out.text = `${str(r.metric)}${match.length ? `{${match.join(',')}}` : ''}`
      break
    }
    case 'profile':
      out.label = 'Profile'
      out.text = [str(r.service), str(r.profile_type)].filter(Boolean).join(' ')
      break
    case 'url':
      out.label = 'Link'
      out.text = str(r.url)
      if (WEB.test(out.text)) out.href = out.text
      break
    default:
      break
  }
  return out
}

/**
 * The project's recorded incidents, newest first: {incidents, skipped}. `state` may be several, comma separated. A filter that is empty is not sent.
 * A failure of the call is thrown, as the client words it.
 */
export async function loadRecorded(client, { state = '', severity = '', app = '', limit = DEFAULT_LIMIT } = {}) {
  const params = { limit }
  if (state) params.state = state
  if (severity) params.severity = severity
  if (app) params.app = app
  const data = await client.get('incidents', params)
  const rows = Array.isArray(data) ? data : []
  const incidents = rows.map(incidentOf).filter(Boolean)
  incidents.sort((a, b) => (Date.parse(b.createdAt) || 0) - (Date.parse(a.createdAt) || 0))
  return { incidents, skipped: rows.length - incidents.length }
}

/** One incident and its timeline: {incident, events (oldest first), warning}. The timeline failing does not take the incident with it. */
export async function loadRecordedIncident(client, id) {
  if (!validId(id)) throw new Error(`"${String(id).slice(0, 40)}" is not an incident id.`)
  const [incident, tl] = await Promise.all([
    client.get(`incidents/${id}`),
    client.get(`incidents/${id}/events`, { limit: EVENT_LIMIT }).then(
      (rows) => ({ rows: Array.isArray(rows) ? rows : [], warning: '' }),
      (e) => ({ rows: [], warning: `The timeline could not be read: ${e && e.message ? e.message : 'unknown error'}` }),
    ),
  ])
  const inc = incidentOf(incident)
  if (!inc) throw new Error('The service answered with something that is not an incident.')
  const events = tl.rows.filter(isObject).map((r) => ({ ...describeEvent(r), id: str(r.id) }))
  events.sort((a, b) => a.seq - b.seq)
  return { incident: inc, events, warning: tl.warning }
}

/** How long to wait for an analysis: the hub gives the model three minutes, and the proxy in front of it 200 seconds. */
export const ANALYSIS_TIMEOUT_MS = 200000

/**
 * Asks the hub to have Claude analyse the incident, which keeps the resolution steps as a comment on it; returns the incident as it is then. The hub
 * sends Claude the incident's title, severity, summary and where its evidence is, and no logs or traces. It needs a MANAGE key, given for this one call
 * and kept nowhere (the page does not hold one), and it costs money, so the hub allows one analysis of an incident every two minutes.
 */
export async function analyseIncident(client, id, key) {
  if (!validId(id)) throw new Error(`"${String(id).slice(0, 40)}" is not an incident id.`)
  const inc = incidentOf(await client.send('POST', `incidents/${id}/analysis`, { key, timeoutMs: ANALYSIS_TIMEOUT_MS }))
  if (!inc) throw new Error('The service answered with something that is not an incident.')
  return inc
}
