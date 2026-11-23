import { describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import RecordedIncidentView from './RecordedIncidentView.vue'

const stub = { template: '<i/>' }
const session = (v = { project: 'us', key: 'k' }) => ({ get: () => v })
const incident = (o = {}) => ({
  id: 'vcxxe33p', title: 'monitoring/telemetry-ui: availability objective burning', app: '/k8s/monitoring/telemetry-ui', severity: 'critical', state: 'open', source: 'detected',
  summary: 'Opened automatically.\nAvailability: 3% of requests failed.', assignee: '', resolution: '', startedAt: '2026-11-16T05:23:00Z', createdAt: '2026-11-16T06:00:50Z',
  updatedAt: '2026-11-16T06:00:51Z', reopened: 0, version: 3,
  evidence: [{ id: 'ev_1', ref: { kind: 'application', service: '/k8s/monitoring/telemetry-ui' }, addedBy: 'detector', addedAt: '' }],
  allowed: ['acknowledged', 'resolved'], ...o,
})
const EVENTS = [
  { seq: 1, id: 'e1', when: '2026-11-16T06:00:50Z', actor: 'detector', text: 'Opened' },
  { seq: 2, id: 'e2', when: '2026-11-16T06:10:00Z', actor: 'sam', text: 'Comment: looking now' },
]
const data = (o = {}) => ({ incident: incident(), events: EVENTS, warning: '', ...o })

const SUMMARY = {
  headline: 'Critical: x (monitoring/telemetry-ui) — open for 38 min, not yet taken on. Most likely related: deployment monitoring/telemetry-ui rolled out 4 min before the trouble began.',
  elapsed: 2280, timing: { detect: 2270, acknowledge: null, mitigate: null, resolve: null }, from: '', to: '',
  findings: [{ at: '2026-11-16T05:19:00Z', kind: 'rollout', text: 'deployment monitoring/telemetry-ui rolled out 4 min before the trouble began', score: 92 },
    { at: '2026-11-16T05:24:00Z', kind: 'kubernetes', text: 'BackOff ×12 on pod telemetry-ui-1: Back-off', score: 90 }],
  timeline: [{ at: '2026-11-16T05:19:00Z', source: 'rollout', text: 'deployment rolled out' }, { at: '2026-11-16T06:00:50Z', source: 'incident', text: 'Opened by detection' }],
  gaps: ['Process lineage: nothing unusual was recorded'],
}

async function mountAt(id = 'vcxxe33p', { load = vi.fn(async () => data()), analyse = vi.fn(async () => incident()), summary = vi.fn(async () => SUMMARY), sess = session() } = {}) {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/', component: stub }, { path: '/configuration/connection', component: stub }, { path: '/incidents', component: stub }, { path: '/recorded', component: stub },
      { path: '/recorded/:id', name: 'recorded-incident', component: RecordedIncidentView },
      { path: '/applications/:name', name: 'application', component: stub }, { path: '/traces/:id', name: 'trace', component: stub }, { path: '/logs', name: 'logs', component: stub },
    ],
  })
  router.push('/recorded/' + id)
  await router.isReady()
  const w = mount(RecordedIncidentView, { props: { client: {}, session: sess, loaders: { loadRecordedIncident: load, analyseIncident: analyse, loadSummary: summary } }, global: { plugins: [router] } })
  await flushPromises()
  return { w, load, analyse, summary }
}

describe('RecordedIncidentView', () => {
  it('reads the incident named in the address', async () => {
    const { load } = await mountAt()
    expect(load.mock.calls[0][1]).toBe('vcxxe33p')
  })
  it('shows the title, severity, state, who opened it and the summary as it was written', async () => {
    const { w } = await mountAt()
    expect(w.get('[data-testid=title]').text()).toBe('monitoring/telemetry-ui: availability objective burning')
    expect(w.get('.pill.critical').text()).toBe('critical')
    expect(w.get('.pill.state').text()).toBe('open')
    expect(w.text()).toContain('opened by the detector')
    expect(w.get('[data-testid=summary]').text()).toContain('3% of requests failed')
  })
  it('shows the facts, and leaves out the ones that do not apply', async () => {
    const { w } = await mountAt()
    const facts = w.get('[data-testid=facts]').text()
    expect(facts).toContain('Application')
    expect(facts).toContain('Started')
    expect(facts).not.toContain('Assignee')
    expect(facts).not.toContain('Resolution')
    expect(facts).not.toContain('Reopened')
    expect(w.get('[data-testid=facts] a').attributes('href')).toBe('/applications/%2Fk8s%2Fmonitoring%2Ftelemetry-ui')
    const more = await mountAt('vcxxe33p', { load: async () => data({ incident: incident({ assignee: 'sam', resolution: 'fixed', reopened: 2, state: 'resolved' }) }) })
    const text = more.w.get('[data-testid=facts]').text()
    expect(text).toContain('Assignee')
    expect(text).toContain('sam')
    expect(text).toContain('Resolution')
    expect(text).toContain('fixed')
    expect(text).toContain('Reopened')
    expect(text).toContain('2 times')
  })
  it('says what it can move to, and that changing it is not done here', async () => {
    const { w } = await mountAt()
    const t = w.get('[data-testid=transitions]').text()
    expect(t).toContain('It can move to: acknowledged, resolved')
    expect(t).toContain('manage key')
    // nothing on this page moves the incident: the one button asks for an analysis, which only adds a comment
    expect(w.findAll('button').map((b) => b.text())).toEqual(['Analyse with Claude'])
    expect(w.findAll('form')).toHaveLength(1)
    const closed = await mountAt('vcxxe33p', { load: async () => data({ incident: incident({ allowed: [] }) }) })
    expect(closed.w.get('[data-testid=transitions]').text()).not.toContain('It can move to')
  })
  it('links the evidence it points at to the pages that show it', async () => {
    const trace = 'a'.repeat(32)
    const { w } = await mountAt('vcxxe33p', {
      load: async () => data({
        incident: incident({
          evidence: [
            { id: 'a', ref: { kind: 'application', service: '/k8s/shop/web' }, addedBy: 'detector', addedAt: '' },
            { id: 'b', ref: { kind: 'trace', trace_id: trace, note: 'the slow one' }, addedBy: 'sam', addedAt: '' },
            { id: 'c', ref: { kind: 'logs', service: '/k8s/shop/web', severity: 'error' }, addedBy: '', addedAt: '' },
            { id: 'd', ref: { kind: 'url', url: 'https://runbook.example/x' }, addedBy: '', addedAt: '' },
            { id: 'e', ref: { kind: 'url', url: 'javascript:alert(1)' }, addedBy: '', addedAt: '' },
            { id: 'f', ref: { kind: 'metric', metric: 'm' }, addedBy: '', addedAt: '' },
          ],
        }),
      }),
    })
    const items = w.findAll('[data-testid=evidence] li')
    expect(items).toHaveLength(6)
    expect(items[0].get('a').attributes('href')).toBe('/applications/%2Fk8s%2Fshop%2Fweb')
    expect(items[1].get('a').attributes('href')).toBe(`/traces/${trace}`)
    expect(items[1].text()).toContain('the slow one')
    expect(items[1].text()).toContain('added by sam')
    expect(items[2].get('a').attributes('href')).toBe('/logs?service=/k8s/shop/web&severity=error')
    expect(items[3].get('a').attributes('href')).toBe('https://runbook.example/x')
    expect(items[3].get('a').attributes('rel')).toBe('noopener noreferrer')
    expect(items[4].find('a').exists()).toBe(false) // a javascript: address is text, not a link
    expect(items[4].text()).toContain('javascript:alert(1)')
    expect(items[5].find('a').exists()).toBe(false)
  })
  it('says when nothing is attached', async () => {
    const { w } = await mountAt('vcxxe33p', { load: async () => data({ incident: incident({ evidence: [] }) }) })
    expect(w.get('[data-testid=no-evidence]').exists()).toBe(true)
    expect(w.find('[data-testid=evidence]').exists()).toBe(false)
  })
  it('lists the timeline in order with who did what', async () => {
    const { w } = await mountAt()
    const rows = w.findAll('table[aria-label=Timeline] tbody tr').map((r) => r.findAll('td').map((c) => c.text()))
    expect(rows).toHaveLength(2)
    expect(rows[0].slice(1)).toEqual(['detector', 'Opened'])
    expect(rows[1].slice(1)).toEqual(['sam', 'Comment: looking now'])
  })
  it('says so when the timeline could not be read, and still shows the incident', async () => {
    const { w } = await mountAt('vcxxe33p', { load: async () => data({ events: [], warning: 'The timeline could not be read: boom' }) })
    expect(w.get('p.warn').text()).toContain('timeline could not be read')
    expect(w.find('table[aria-label=Timeline]').exists()).toBe(false)
    expect(w.get('[data-testid=title]').exists()).toBe(true)
  })
  it('says so when there are no events', async () => {
    const { w } = await mountAt('vcxxe33p', { load: async () => data({ events: [] }) })
    expect(w.get('[data-testid=no-events]').exists()).toBe(true)
  })
  it('shows a failure with the way back, and nothing else', async () => {
    const { w } = await mountAt('vcxxe33p', { load: async () => { throw new Error('Not found.') } })
    expect(w.get('[role=alert]').text()).toContain('Not found.')
    expect(w.get('[role=alert] a').attributes('href')).toBe('/recorded')
    expect(w.find('[data-testid=title]').exists()).toBe(false)
  })
  it('asks for a connection first, and reads nothing without one', async () => {
    const { w, load } = await mountAt('vcxxe33p', { sess: session(null) })
    expect(w.text()).toContain('Not connected')
    expect(load).not.toHaveBeenCalled()
  })
  it('puts a summary and a title that are markup on the page as text, never as markup', async () => {
    const { w } = await mountAt('vcxxe33p', { load: async () => data({ incident: incident({ title: '<b id=t>x</b>', summary: '<img src=x onerror=alert(1)>' }) }) })
    expect(w.find('#t').exists()).toBe(false)
    expect(w.find('img').exists()).toBe(false)
    expect(w.get('[data-testid=summary]').text()).toBe('<img src=x onerror=alert(1)>')
  })

  describe('analysing with Claude', () => {
    const CLAUDE_EVENTS = [
      ...EVENTS,
      { seq: 3, id: 'e3', when: '2026-11-16T06:20:00Z', actor: 'claude', kind: 'commented', body: 'Analysis by Claude (m)\n\nLikely cause: Mongo is scaled to zero.\n1. kubectl scale sts/mongo --replicas=1', text: 'Comment: ...' },
    ]
    const form = (w) => ({ key: w.get('[data-testid=analyse-key]'), button: w.get('[data-testid=analyse]') })

    it('offers the analysis with a password field for the manage key, and says what is sent and that the key is not kept', async () => {
      const { w } = await mountAt()
      const { key, button } = form(w)
      expect(key.attributes('type')).toBe('password')
      expect(key.attributes('autocomplete')).toBe('off')
      expect(button.text()).toBe('Analyse with Claude')
      const note = w.get('[data-testid=analyse-note]').text()
      for (const want of ['title', 'summary', 'where its evidence is', 'no logs or traces', 'manage key', 'not kept']) expect(note).toContain(want)
      // the Anthropic key is the hub's, in a Secret; this field is a different key, and mixing them up sends the Anthropic key to the hub
      expect(note).toContain('not an Anthropic key')
      expect(w.get('label[for=analyse-key]').text()).toBe('Manage key of this project (not an Anthropic key)')
    })
    it('asks for the key and calls nothing when there is none', async () => {
      const { w, analyse } = await mountAt()
      await form(w).button.trigger('click')
      await flushPromises()
      expect(w.get('[data-testid=analyse-error]').text()).toContain('manage key')
      expect(analyse).not.toHaveBeenCalled()
      await form(w).key.setValue('   ')
      await form(w).button.trigger('click')
      expect(analyse).not.toHaveBeenCalled()
    })
    it('sends the key for this one call, waits with the button off, then reads the incident again and forgets the key', async () => {
      let done
      const analyse = vi.fn(() => new Promise((resolve) => { done = resolve }))
      const load = vi.fn(async () => data())
      const { w } = await mountAt('vcxxe33p', { analyse, load })
      await form(w).key.setValue('the-manage-key')
      await form(w).button.trigger('click')
      expect(analyse).toHaveBeenCalledTimes(1)
      expect(analyse.mock.calls[0][1]).toBe('vcxxe33p')
      expect(analyse.mock.calls[0][2]).toBe('the-manage-key')
      expect(form(w).button.attributes('disabled')).toBeDefined()
      expect(w.get('[data-testid=analyse-busy]').text()).toContain('Claude is working')
      await form(w).button.trigger('click') // a second click while it works
      expect(analyse).toHaveBeenCalledTimes(1)
      load.mockImplementation(async () => data({ events: CLAUDE_EVENTS }))
      done(incident())
      await flushPromises()
      expect(load).toHaveBeenCalledTimes(2)
      expect(form(w).key.element.value).toBe('') // used, and not kept
      expect(form(w).button.attributes('disabled')).toBeUndefined()
      expect(w.find('[data-testid=analyse-busy]').exists()).toBe(false)
      expect(w.get('[data-testid=analysis]').text()).toContain('Likely cause: Mongo is scaled to zero.')
    })
    it('does not ask twice when the form is submitted again while it works (the Enter key), whatever the button says', async () => {
      const analyse = vi.fn(() => new Promise(() => {}))
      const { w } = await mountAt('vcxxe33p', { analyse })
      await form(w).key.setValue('the-manage-key')
      await w.get('form.analyse').trigger('submit')
      await w.get('form.analyse').trigger('submit')
      expect(analyse).toHaveBeenCalledTimes(1)
    })
    it('says why when it fails, keeps the incident on the page, forgets the key, and allows another try', async () => {
      const analyse = vi.fn(async () => { throw new Error('Claude is rate limiting this key; try again in a minute') })
      const { w, load } = await mountAt('vcxxe33p', { analyse })
      await form(w).key.setValue('the-manage-key')
      await form(w).button.trigger('click')
      await flushPromises()
      expect(w.get('[data-testid=analyse-error]').text()).toContain('rate limiting')
      expect(w.get('[data-testid=title]').exists()).toBe(true)
      expect(load).toHaveBeenCalledTimes(1) // nothing changed, so nothing is read again
      expect(form(w).key.element.value).toBe('')
      expect(form(w).button.attributes('disabled')).toBeUndefined()
      await form(w).key.setValue('again')
      await form(w).button.trigger('click')
      expect(analyse).toHaveBeenCalledTimes(2)
    })
    it('clears the last message when it is tried again', async () => {
      const analyse = vi.fn().mockRejectedValueOnce(new Error('boom')).mockResolvedValueOnce(incident())
      const { w } = await mountAt('vcxxe33p', { analyse })
      await form(w).key.setValue('k')
      await form(w).button.trigger('click')
      await flushPromises()
      expect(w.find('[data-testid=analyse-error]').exists()).toBe(true)
      await form(w).key.setValue('k')
      await form(w).button.trigger('click')
      await flushPromises()
      expect(w.find('[data-testid=analyse-error]').exists()).toBe(false)
    })
    it('shows the newest analysis above the timeline, as text with its line breaks and who made it and when', async () => {
      const { w } = await mountAt('vcxxe33p', { load: async () => data({ events: CLAUDE_EVENTS }) })
      const a = w.get('[data-testid=analysis]')
      expect(a.text()).toContain('Resolution steps from Claude')
      expect(a.get('pre').text()).toContain('1. kubectl scale sts/mongo --replicas=1')
      expect(a.get('pre').text().split('\n').length).toBeGreaterThan(3)
    })
    it('shows no analysis panel until there is one, and not for a comment a person made', async () => {
      expect((await mountAt()).w.find('[data-testid=analysis]').exists()).toBe(false)
      const person = [...EVENTS, { seq: 3, id: 'e3', when: '2026-11-16T06:20:00Z', actor: 'sam', kind: 'commented', body: 'Likely cause: me', text: 'Comment: Likely cause: me' }]
      expect((await mountAt('vcxxe33p', { load: async () => data({ events: person }) })).w.find('[data-testid=analysis]').exists()).toBe(false)
    })
    it('puts an analysis that is markup on the page as text, never as markup', async () => {
      const evil = [...EVENTS, { seq: 3, id: 'e3', when: '2026-11-16T06:20:00Z', actor: 'claude', kind: 'commented', body: '<img src=x onerror=alert(1)><b id=bold>x</b>', text: 'Comment: x' }]
      const { w } = await mountAt('vcxxe33p', { load: async () => data({ events: evil }) })
      expect(w.find('img').exists()).toBe(false)
      expect(w.find('#bold').exists()).toBe(false)
      expect(w.get('[data-testid=analysis] pre').text()).toBe('<img src=x onerror=alert(1)><b id=bold>x</b>')
    })
    it('keeps the line breaks of a comment in the timeline too', async () => {
      const { w } = await mountAt('vcxxe33p', { load: async () => data({ events: CLAUDE_EVENTS }) })
      const cell = w.findAll('table[aria-label=Timeline] tbody tr')[2].findAll('td')[2]
      expect(cell.classes()).toContain('what')
    })
    it('asks for nothing without a connection', async () => {
      const { w, analyse } = await mountAt('vcxxe33p', { sess: session(null) })
      expect(w.find('[data-testid=analyse]').exists()).toBe(false)
      expect(analyse).not.toHaveBeenCalled()
    })
  })
  describe('instant summary', () => {
    it('reads the summary of the same incident and shows its headline, timing, findings and gaps', async () => {
      const { w, summary } = await mountAt()
      expect(summary.mock.calls[0][1]).toBe('vcxxe33p')
      expect(w.get('[data-testid=summary-headline]').text()).toContain('Most likely related: deployment monitoring/telemetry-ui rolled out')
      const timing = w.get('[data-testid=timing]').text()
      expect(timing).toContain('Detected after37 min')
      expect(timing).toContain('Taken on afternot yet')
      const items = w.findAll('[data-testid=findings] li')
      expect(items).toHaveLength(2)
      expect(items[0].get('.pill.kind.rollout').text()).toBe('Rollout')
      expect(items[1].text()).toContain('BackOff ×12')
      expect(w.get('[data-testid=gaps]').text()).toContain('Process lineage')
      expect(w.get('[data-testid=summary-timeline] summary').text()).toBe('Everything around it (2)')
    })
    it('keeps the rest of the page when the summary fails', async () => {
      const { w } = await mountAt('vcxxe33p', { summary: vi.fn(async () => { throw new Error('service unavailable') }) })
      expect(w.get('[data-testid=summary-error]').text()).toContain('service unavailable')
      expect(w.get('[data-testid=title]').exists()).toBe(true)
    })
    it('shows no findings list when there are none', async () => {
      const { w } = await mountAt('vcxxe33p', { summary: vi.fn(async () => ({ ...SUMMARY, findings: [], gaps: [], timeline: [] })) })
      expect(w.find('[data-testid=findings]').exists()).toBe(false)
      expect(w.find('[data-testid=gaps]').exists()).toBe(false)
      expect(w.find('[data-testid=summary-timeline]').exists()).toBe(false)
    })
  })
})
