import { describe, expect, it } from 'vitest'
import { DEFAULTS, evaluate } from './rules'

const A = '/k8s/shop/cartservice'
const B = '/k8s/shop/frontend'
const C = '/k8s/shop/checkout'
const edge = (from, to, o = {}) => ({ from, to, rps: 10, connPerSec: 0, failedConnects: 0, callErrors: 0, ...o })
const node = (id, external = false) => ({ id, external })
const look = (edges, { listeners = null, incomplete = false, extra = [] } = {}) => ({
  edges,
  nodes: [...new Set([...edges.flatMap((e) => [e.from, e.to]), ...extra])].map((id) => node(id)),
  listeners,
  incomplete,
})
const listening = (seen, fresh = seen) => ({ seen, fresh })
const rules = (r) => r.alerts.map((a) => `${a.rule}|${a.service}`).sort()
const NORMAL = look([edge(B, A, { rps: 10 }), edge(B, C, { rps: 5 })], { listeners: listening([A, B, C]) })

describe('evaluate: healthy', () => {
  it('raises nothing when traffic is as before, and says how many services it looked at', () => {
    const r = evaluate({ recent: NORMAL, baseline: NORMAL })
    expect(r.alerts).toEqual([])
    expect(r.checked).toBe(3)
  })
})

describe('no-traffic', () => {
  it('fires when a service that had traffic gets none, naming who used to call it', () => {
    const recent = look([edge(B, C, { rps: 5 })], { listeners: listening([A, B, C]) })
    const r = evaluate({ recent, baseline: NORMAL })
    expect(rules(r)).toEqual([`no-traffic|${A}`])
    const a = r.alerts[0]
    expect(a).toMatchObject({ severity: 'critical', label: 'shop/cartservice' })
    expect(a.evidence).toMatchObject({ recentPerSec: 0, baselinePerSec: 10 })
    expect(a.evidence.baselineCallers).toEqual([{ id: B, label: 'shop/frontend', rate: 10 }])
    expect(a.summary).toContain('callers may be')
  })

  it('is a warning when traffic fell to almost nothing but not zero', () => {
    const recent = look([edge(B, A, { rps: 0.2 }), edge(B, C, { rps: 5 })], { listeners: listening([A, B, C]) })
    expect(evaluate({ recent, baseline: NORMAL }).alerts.find((x) => x.rule === 'no-traffic').severity).toBe('warning')
  })

  it('ignores a service that never had much traffic', () => {
    const quiet = look([edge(B, A, { rps: 0.01 }), edge(B, C, { rps: 5 })], { listeners: listening([A, B, C]) })
    const recent = look([edge(B, C, { rps: 5 })], { listeners: listening([A, B, C]) })
    expect(evaluate({ recent, baseline: quiet }).alerts).toEqual([])
  })

  it('does not fire for a service that simply gets less traffic', () => {
    const recent = look([edge(B, A, { rps: 4 }), edge(B, C, { rps: 5 })], { listeners: listening([A, B, C]) })
    expect(evaluate({ recent, baseline: NORMAL }).alerts).toEqual([])
  })

  it('counts connections as traffic, for a service with no call counters', () => {
    const base = look([edge(B, A, { rps: null, connPerSec: 2 })], { listeners: listening([A, B]) })
    const recent = look([edge(C, B, { rps: 1 })], { listeners: listening([A, B, C]) })
    expect(rules(evaluate({ recent, baseline: base }))).toContain(`no-traffic|${A}`)
  })
})

describe('no-instances', () => {
  it('fires when a service listened and no longer does, and holds back the no-traffic that follows from it', () => {
    const recent = look([edge(B, C, { rps: 5 })], { listeners: listening([A, B, C], [B, C]) })
    const r = evaluate({ recent, baseline: NORMAL })
    expect(rules(r)).toEqual([`no-instances|${A}`])
    expect(r.alerts[0].severity).toBe('critical')
    expect(r.alerts[0].evidence.lastTrafficPerSec).toBe(10)
  })

  it('catches a service that died inside the baseline, before the recent window', () => {
    const baseline = look([edge(B, C, { rps: 5 })], { listeners: listening([A, B, C], [B, C]) })
    const recent = look([edge(B, C, { rps: 5 })], { listeners: listening([B, C]) })
    expect(rules(evaluate({ recent, baseline }))).toEqual([`no-instances|${A}`])
  })

  it('is not raised for a service that never listened, such as an external endpoint', () => {
    const recent = look([edge(B, 'external:34.1.2.3:443')], { listeners: listening([B]) })
    recent.nodes.find((n) => n.id.startsWith('external:')).external = true
    expect(evaluate({ recent, baseline: recent }).alerts).toEqual([])
  })

  it('says it was not judged when the project has no listen metric', () => {
    const base = look(NORMAL.edges)
    const r = evaluate({ recent: look(NORMAL.edges), baseline: base })
    expect(r.alerts).toEqual([])
    expect(r.skipped).toEqual(['Instance presence was not judged: the project has no listen-address metric.'])
  })
})

describe('refused', () => {
  const recent = (o) => look([edge(B, A, { rps: null, connPerSec: 1, failedConnects: 4, ...o })], { listeners: listening([A, B]) })
  it('fires when a large share of the connections into a service fail', () => {
    const r = evaluate({ recent: recent(), baseline: NORMAL })
    const a = r.alerts.find((x) => x.rule === 'refused')
    expect(a).toMatchObject({ service: A, severity: 'warning' }) // 80% of 5/s
    expect(a.evidence).toMatchObject({ failedSharePct: 80, failedConnectsPerSec: 4 })
    expect(a.summary).toContain('80%')
  })
  it('is critical when nearly all fail', () => {
    expect(evaluate({ recent: recent({ connPerSec: 0.1, failedConnects: 4 }), baseline: NORMAL }).alerts.find((x) => x.rule === 'refused').severity).toBe('critical')
  })
  it('needs enough failures to mean anything', () => {
    // 100% failing, but 1 failure in 5 minutes
    expect(evaluate({ recent: recent({ connPerSec: 0, failedConnects: 1 / 300 }), baseline: NORMAL }).alerts.find((x) => x.rule === 'refused')).toBeUndefined()
  })
  it('stays quiet for a few failures among many connections', () => {
    expect(evaluate({ recent: recent({ connPerSec: 50, failedConnects: 1 }), baseline: NORMAL }).alerts.find((x) => x.rule === 'refused')).toBeUndefined()
  })
})

describe('errors', () => {
  const recent = (o) => look([edge(B, A, { rps: 10, ...o })], { listeners: listening([A, B]) })
  it('fires on the share of calls failing, as a warning then critical', () => {
    const w = evaluate({ recent: recent({ callErrors: 1 }), baseline: NORMAL }).alerts.find((x) => x.rule === 'errors')
    expect(w).toMatchObject({ service: A, severity: 'warning' })
    expect(w.evidence.errorSharePct).toBe(10)
    expect(evaluate({ recent: recent({ callErrors: 6 }), baseline: NORMAL }).alerts.find((x) => x.rule === 'errors').severity).toBe('critical')
  })
  it('stays quiet under the threshold, and for too few calls', () => {
    expect(evaluate({ recent: recent({ callErrors: 0.2 }), baseline: NORMAL }).alerts.find((x) => x.rule === 'errors')).toBeUndefined() // 2%
    expect(evaluate({ recent: recent({ rps: 0.05, callErrors: 0.05 }), baseline: NORMAL }).alerts.find((x) => x.rule === 'errors')).toBeUndefined() // 15 calls
  })
})

describe('telemetry-gone', () => {
  it('replaces the per-service absence alerts when everything vanishes at once', () => {
    const recent = look([], { listeners: listening([], []) })
    const r = evaluate({ recent, baseline: NORMAL })
    expect(rules(r)).toEqual(['telemetry-gone|*'])
    expect(r.alerts[0]).toMatchObject({ severity: 'critical', label: 'project' })
  })
  it('does not fire for a project that never had traffic', () => {
    expect(evaluate({ recent: look([]), baseline: look([]) }).alerts).toEqual([])
  })
})

describe('incomplete reads', () => {
  it('does not turn a failed read into an outage', () => {
    const recent = look([], { incomplete: true, listeners: listening([], []) })
    const r = evaluate({ recent, baseline: NORMAL })
    expect(r.alerts).toEqual([])
    expect(r.skipped[0]).toContain('not judged')
  })
  it('still judges what is present: errors do not depend on absence', () => {
    const recent = look([edge(B, A, { rps: 10, callErrors: 6 })], { incomplete: true, listeners: listening([A, B]) })
    expect(rules(evaluate({ recent, baseline: NORMAL }))).toEqual([`errors|${A}`])
  })
})

describe('config', () => {
  it('takes thresholds from the caller', () => {
    const recent = look([edge(B, A, { rps: 10, callErrors: 1 }), edge(B, C, { rps: 5 })], { listeners: listening([A, B, C]) })
    expect(rules(evaluate({ recent, baseline: NORMAL }))).toEqual([`errors|${A}`]) // 10% is over the default 5%
    expect(evaluate({ recent, baseline: NORMAL }, { errorShare: 20 }).alerts).toEqual([])
    expect(DEFAULTS.errorShare).toBe(5) // the defaults are not changed by a caller's config
  })
  it('gives stable ids', () => {
    const recent = look([edge(B, C, { rps: 5 })], { listeners: listening([A, B, C]) })
    expect(evaluate({ recent, baseline: NORMAL }).alerts[0].id).toBe(evaluate({ recent, baseline: NORMAL }).alerts[0].id)
  })
})
