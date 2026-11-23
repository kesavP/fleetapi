import { describe, expect, it, vi } from 'vitest'
import { ANALYSIS_TIMEOUT_MS, analyseIncident, describeEvent, describeEvidence, incidentOf, latestAnalysis, loadRecorded, loadRecordedIncident, validId } from './recorded.js'

const ROW = {
  id: 'vcxxe33p',
  project: 'us',
  title: 'monitoring/telemetry-ui: availability objective burning',
  app: '/k8s/monitoring/telemetry-ui',
  severity: 'critical',
  state: 'open',
  source: 'detected',
  summary: 'Opened automatically.',
  started_at: '2026-11-16T05:23:00Z',
  created_at: '2026-11-16T06:00:50.974Z',
  updated_at: '2026-11-16T06:00:51.003Z',
  reopened: 0,
  version: 3,
  evidence: [{ id: 'ev_1', ref: { kind: 'application', service: '/k8s/monitoring/telemetry-ui' }, added_by: 'detector', added_at: '2026-11-16T06:00:50.997Z' }],
  allowed_transitions: ['acknowledged', 'resolved'],
}

describe('validId', () => {
  it('accepts what the service makes (8 lower-case letters and digits) and nothing else, so an id cannot change the path it is put in', () => {
    expect(validId('vcxxe33p')).toBe(true)
    for (const bad of ['', 'VCXXE33P', 'vcxxe33', 'vcxxe33pp', '../x/y/z0', 'a/b', 'a b c d e', null, undefined, 12345678]) expect(validId(bad)).toBe(false)
  })
})

describe('incidentOf', () => {
  it('keeps what the page shows, with the times as the service sent them', () => {
    expect(incidentOf(ROW)).toMatchObject({
      id: 'vcxxe33p', title: ROW.title, app: ROW.app, severity: 'critical', state: 'open', source: 'detected', summary: 'Opened automatically.',
      startedAt: '2026-11-16T05:23:00Z', updatedAt: '2026-11-16T06:00:51.003Z', version: 3, allowed: ['acknowledged', 'resolved'],
    })
    expect(incidentOf(ROW).evidence).toHaveLength(1)
  })
  it('fills what is missing with something safe to show, and skips a row that is not an incident at all', () => {
    const bare = incidentOf({ id: 'abcd1234', title: 'x' })
    expect(bare).toMatchObject({ app: '', summary: '', assignee: '', resolution: '', evidence: [], allowed: [], severity: 'info', state: 'open' })
    for (const bad of [null, undefined, 'x', 5, {}, { id: 'abcd1234' }, { title: 'x' }, { id: '../../x', title: 'x' }]) expect(incidentOf(bad)).toBeNull()
  })
  it('does not trust a state or a severity it does not know: they are shown as the safe default, not as free text styled as a class name', () => {
    const o = incidentOf({ ...ROW, state: 'on-fire" onclick="x', severity: 'catastrophic' })
    expect(o.state).toBe('open')
    expect(o.severity).toBe('info')
  })
  it('keeps only the evidence and transitions that are well formed', () => {
    const o = incidentOf({ ...ROW, evidence: [null, { id: 'e', ref: null }, { id: 'e2', ref: { kind: 'trace', trace_id: 'a' } }, 7], allowed_transitions: ['resolved', 'bogus', 5] })
    expect(o.evidence.map((e) => e.id)).toEqual(['e2'])
    expect(o.allowed).toEqual(['resolved'])
  })
})

describe('describeEvent', () => {
  const at = (type, data, over = {}) => ({ seq: 2, id: 'x', time: '2026-11-16T06:00:51Z', type, actor: 'detector', data, ...over })
  it('says who did what and when, for each kind of event', () => {
    expect(describeEvent(at('created', { title: 'T', severity: 'critical' }))).toMatchObject({ actor: 'detector', text: 'Opened' })
    expect(describeEvent(at('state_changed', { from: 'open', to: 'resolved', resolution: 'fixed', note: 'rolled back' })).text).toBe('open → resolved (fixed): rolled back')
    expect(describeEvent(at('state_changed', { from: 'open', to: 'acknowledged' })).text).toBe('open → acknowledged')
    expect(describeEvent(at('commented', { text: 'looking now' })).text).toBe('Comment: looking now')
    expect(describeEvent(at('evidence_added', { id: 'ev_1', ref: { kind: 'logs' } })).text).toBe('Evidence added: logs')
    expect(describeEvent(at('evidence_removed', { id: 'ev_1', reason: 'wrong app' })).text).toBe('Evidence removed: wrong app')
    expect(describeEvent(at('evidence_removed', { id: 'ev_1' })).text).toBe('Evidence removed')
  })
  it('lists what an update changed', () => {
    expect(describeEvent(at('updated', { severity: 'warning', assignee: 'sam' })).text).toBe('Changed severity, assignee')
    expect(describeEvent(at('updated', {})).text).toBe('Changed')
  })
  it('shows an event it does not know by its type, and survives data that is missing or is not an object', () => {
    expect(describeEvent(at('mystery', { a: 1 })).text).toBe('mystery')
    for (const data of [undefined, null, 'str', 5, []]) expect(describeEvent(at('state_changed', data)).text).toBe('State changed')
    expect(describeEvent({}).text).toBe('Event')
  })
  it('keeps the time as sent, and the person or program that acted ("system" when it is not said)', () => {
    expect(describeEvent(at('created', {})).when).toBe('2026-11-16T06:00:51Z')
    expect(describeEvent(at('created', {}, { actor: '' })).actor).toBe('system')
  })
})

describe('describeEvidence', () => {
  const ref = (r, over = {}) => ({ id: 'ev', ref: r, added_by: 'detector', added_at: '2026-11-16T06:00:51Z', ...over })
  it('links an application and a trace to their pages', () => {
    expect(describeEvidence(ref({ kind: 'application', service: '/k8s/shop/web' }))).toMatchObject({ label: 'Application', text: '/k8s/shop/web', to: { name: 'application', params: { name: '/k8s/shop/web' } } })
    const id = 'a'.repeat(32)
    expect(describeEvidence(ref({ kind: 'trace', trace_id: id }))).toMatchObject({ label: 'Trace', to: { name: 'trace', params: { id } } })
  })
  it('links a log search with what it was filtered by', () => {
    const d = describeEvidence(ref({ kind: 'logs', service: '/k8s/shop/web', severity: 'error', query: 'timeout', from: '2026-11-16T05:18:00Z', to: '2026-11-16T06:00:00Z' }))
    expect(d.to).toEqual({ name: 'logs', query: { service: '/k8s/shop/web', severity: 'error', q: 'timeout' } })
    expect(d.text).toContain('/k8s/shop/web')
    expect(d.text).toContain('error')
    expect(d.text).toContain('timeout')
  })
  it('describes a metric and a profile without a link', () => {
    expect(describeEvidence(ref({ kind: 'metric', metric: 'container_http_inbound_requests_total', match: { status: '5xx' } }))).toMatchObject({ label: 'Metric', to: null, text: 'container_http_inbound_requests_total{status="5xx"}' })
    expect(describeEvidence(ref({ kind: 'profile', service: '/k8s/a/b', profile_type: 'cpu' }))).toMatchObject({ label: 'Profile', to: null })
  })
  it('links an address only when it is http or https, so a stored javascript: address is shown as text and never becomes a link', () => {
    expect(describeEvidence(ref({ kind: 'url', url: 'https://runbook.example/x' }))).toMatchObject({ label: 'Link', href: 'https://runbook.example/x' })
    for (const url of ['javascript:alert(1)', 'data:text/html,x', 'ftp://x', '//evil.example', ' https://x']) {
      const d = describeEvidence(ref({ kind: 'url', url }))
      expect(d.href).toBeNull()
      expect(d.text).toBe(url)
    }
  })
  it('puts the note after the description, and names who added it', () => {
    const d = describeEvidence(ref({ kind: 'application', service: '/k8s/a/b', note: 'the one that broke' }))
    expect(d.note).toBe('the one that broke')
    expect(d.addedBy).toBe('detector')
  })
  it('does not fail on a kind it does not know', () => {
    expect(describeEvidence(ref({ kind: 'hologram' }))).toMatchObject({ label: 'hologram', to: null, href: null })
  })
})

const clientOf = (handler) => ({ get: vi.fn(async (path, params) => handler(path, params)) })

describe('loadRecorded', () => {
  it('asks for the incidents with the filters that are set, newest first, and drops what is not an incident', async () => {
    const client = clientOf(() => [{ ...ROW, id: 'aaaaaaaa', created_at: '2026-11-16T06:00:00Z' }, { ...ROW, id: 'bbbbbbbb', created_at: '2026-11-16T07:00:00Z' }, { nonsense: true }])
    const out = await loadRecorded(client, { state: 'open,acknowledged', severity: 'critical', app: '', limit: 50 })
    expect(client.get).toHaveBeenCalledWith('incidents', { state: 'open,acknowledged', severity: 'critical', limit: 50 })
    expect(out.incidents.map((i) => i.id)).toEqual(['bbbbbbbb', 'aaaaaaaa'])
    expect(out.skipped).toBe(1)
  })
  it('sends nothing for a filter that is not set', async () => {
    const client = clientOf(() => [])
    await loadRecorded(client, {})
    expect(client.get).toHaveBeenCalledWith('incidents', { limit: 50 })
  })
  it('treats an answer that is not a list as none', async () => {
    expect((await loadRecorded(clientOf(() => null), {})).incidents).toEqual([])
    expect((await loadRecorded(clientOf(() => ({ not: 'a list' })), {})).incidents).toEqual([])
  })
  it('lets a failure of the call through, for the page to say', async () => {
    const client = { get: async () => { throw new Error('The telemetry service is not reachable right now.') } }
    await expect(loadRecorded(client, {})).rejects.toThrow('not reachable')
  })
})

describe('loadRecordedIncident', () => {
  const EVENTS = [
    { seq: 2, id: 'e2', time: '2026-11-16T06:10:00Z', type: 'commented', actor: 'sam', data: { text: 'hi' } },
    { seq: 1, id: 'e1', time: '2026-11-16T06:00:51Z', type: 'created', actor: 'detector', data: {} },
  ]
  const router = (path) => (path === 'incidents/vcxxe33p' ? ROW : path === 'incidents/vcxxe33p/events' ? EVENTS : null)

  it('reads the incident and its timeline, oldest event first', async () => {
    const client = clientOf(router)
    const out = await loadRecordedIncident(client, 'vcxxe33p')
    expect(out.incident.id).toBe('vcxxe33p')
    expect(out.events.map((e) => e.seq)).toEqual([1, 2])
    expect(out.warning).toBe('')
    expect(client.get).toHaveBeenCalledWith('incidents/vcxxe33p/events', { limit: 500 })
  })
  it('refuses an id that is not one before asking anything', async () => {
    const client = clientOf(router)
    await expect(loadRecordedIncident(client, '../../metrics')).rejects.toThrow('not an incident id')
    expect(client.get).not.toHaveBeenCalled()
  })
  it('still shows the incident when the timeline cannot be read, and says so', async () => {
    const client = clientOf((path) => {
      if (path.endsWith('/events')) throw new Error('boom')
      return ROW
    })
    const out = await loadRecordedIncident(client, 'vcxxe33p')
    expect(out.incident.id).toBe('vcxxe33p')
    expect(out.events).toEqual([])
    expect(out.warning).toMatch(/timeline/i)
  })
  it('fails when the incident itself cannot be read, or is not one', async () => {
    await expect(loadRecordedIncident({ get: async () => { throw new Error('Not found.') } }, 'vcxxe33p')).rejects.toThrow('Not found')
    await expect(loadRecordedIncident(clientOf(() => ({ nonsense: 1 })), 'vcxxe33p')).rejects.toThrow('not an incident')
  })
})

describe('describeEvent: what a comment says', () => {
  const at = (type, data, over = {}) => ({ seq: 2, id: 'x', time: '2026-11-16T06:00:51Z', type, actor: 'sam', data, ...over })
  it('keeps the kind of the event, and for a comment the whole text, line breaks and all', () => {
    const d = describeEvent(at('commented', { text: 'Likely cause: x\n1. do this' }))
    expect(d.kind).toBe('commented')
    expect(d.body).toBe('Likely cause: x\n1. do this')
    expect(d.text).toBe('Comment: Likely cause: x\n1. do this')
    for (const type of ['created', 'state_changed', 'updated', 'evidence_added', 'mystery']) {
      expect(describeEvent(at(type, { text: 'not a comment' })).body).toBe('')
      expect(describeEvent(at(type, {})).kind).toBe(type)
    }
    expect(describeEvent(at('commented', {})).body).toBe('')
    expect(describeEvent({}).kind).toBe('')
  })
})

describe('latestAnalysis', () => {
  const ev = (seq, actor, kind = 'commented', body = `text ${seq}`) => ({ seq, actor, kind, body, when: `2026-11-16T06:0${seq}:00Z`, text: `Comment: ${body}` })
  it('is the newest comment made by claude', () => {
    expect(latestAnalysis([ev(1, 'detector', 'created'), ev(2, 'claude'), ev(3, 'sam'), ev(4, 'claude'), ev(5, 'sam')])).toMatchObject({ seq: 4, body: 'text 4' })
  })
  it('is null when claude has not commented, or only did something else, or there is nothing', () => {
    expect(latestAnalysis([ev(1, 'sam'), ev(2, 'claude', 'state_changed')])).toBeNull()
    expect(latestAnalysis([])).toBeNull()
    expect(latestAnalysis(null)).toBeNull()
    expect(latestAnalysis([ev(1, 'claude', 'commented', '')])).toBeNull() // a comment with no text is no analysis
  })
})

describe('analyseIncident', () => {
  const answer = { ...ROW, version: 4 }
  it('asks for the analysis with the manage key given for this call, waits as long as the hub may, and returns the incident with the comment on it', async () => {
    const client = { send: vi.fn(async () => answer) }
    const inc = await analyseIncident(client, 'vcxxe33p', ' my-manage-key ')
    expect(client.send).toHaveBeenCalledWith('POST', 'incidents/vcxxe33p/analysis', { key: ' my-manage-key ', timeoutMs: ANALYSIS_TIMEOUT_MS })
    expect(inc.id).toBe('vcxxe33p')
    expect(inc.version).toBe(4)
    expect(ANALYSIS_TIMEOUT_MS).toBeGreaterThan(180000) // the hub gives the model three minutes
  })
  it('refuses an id that is not one before sending anything, and an answer that is not an incident', async () => {
    const client = { send: vi.fn(async () => ({ nonsense: 1 })) }
    await expect(analyseIncident(client, '../../x', 'k')).rejects.toThrow('not an incident id')
    expect(client.send).not.toHaveBeenCalled()
    await expect(analyseIncident(client, 'vcxxe33p', 'k')).rejects.toThrow('not an incident')
  })
  it('lets a failure through as the client words it', async () => {
    const client = { send: async () => { throw new Error('Claude is rate limiting this key; try again in a minute') } }
    await expect(analyseIncident(client, 'vcxxe33p', 'k')).rejects.toThrow('rate limiting')
  })
})
