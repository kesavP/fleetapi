import { describe, expect, it } from 'vitest'
import { analyze } from './analyze'
import { formatMs, issueLabel, issueSentence, narrate, segments } from './narrative'

const FE = '/k8s/shop/frontend'
const CART = '/k8s/shop/cartservice'
const REDIS = '/k8s/shop/redis-cart'
const edge = (from, to, o = {}) => ({ from, to, rps: 20, connPerSec: 2, errPct: 0, p95ms: 10, connMs: 1, rttMs: 0.4, ...o })
const app = (o = {}) => ({ cpuCores: 0.1, cpuLimit: 1, restarts: 0, oomKills: 0, errorLines: 0, ...o })
const snap = ({ edges = [], apps = {}, inbound = {} } = {}) => ({
  graph: { edges, nodes: [] },
  apps: new Map(Object.entries({ [FE]: app(), [CART]: app(), [REDIS]: app(), ...apps })),
  inbound: new Map(Object.entries({ [FE]: { rps: 30, errPct: 0, p95ms: 40 }, ...inbound })),
})
const before = snap({ edges: [edge(FE, CART), edge(CART, REDIS)] })
const findings = (during) => analyze({ app: FE, before, during })

describe('formatMs', () => {
  it('uses the unit that reads best', () => {
    expect([0.4, 0.456, 12.34, 210.4, 1500, 12000, null, NaN].map(formatMs)).toEqual(['0.4 ms', '0.46 ms', '12.3 ms', '210 ms', '1.5 s', '12 s', '–', '–'])
  })
})

describe('narrate: a network delay', () => {
  const during = snap({
    edges: [edge(FE, CART, { p95ms: 900 }), edge(CART, REDIS, { rttMs: 210, connMs: 400, p95ms: 800 })],
    inbound: { [FE]: { rps: 30, errPct: 12, p95ms: 1500 } },
  })
  const f = findings(during)
  const n = narrate({ app: FE, kind: ['availability', 'latency'] }, f)

  it('titles the incident with the symptom and the cause', () => {
    expect(n.title).toBe('`frontend` errors/latency caused by increased network round-trip time between `cartservice` and `redis-cart`.')
  })
  it('traces the path from the hurt application to the cause, and states the numbers', () => {
    expect(n.summary[0]).toContain('Elevated latency and errors in `frontend` trace back through `cartservice` to `redis-cart`.')
    expect(n.summary[0]).toContain('round-trip time rose from 0.4 ms to 210 ms (525x)')
    expect(n.path).toEqual([FE, CART, REDIS])
  })
  it('adds the other findings on the same call to the same paragraph', () => {
    expect(n.summary[0]).toContain('Opening a TCP connection from `cartservice` to `redis-cart` took 400 ms on average, against 1 ms before (400x)')
    expect(n.summary[0]).toContain('p95 latency of `cartservice` → `redis-cart` calls rose from 10 ms to 800 ms (80x)')
  })
  it('says the hurt application is not the origin, with what its users saw', () => {
    expect(n.propagation).toBe('The anomaly in `frontend` (12% of requests failing, p95 40 ms → 1.5 s) propagates down the dependency chain, not from `frontend` itself.')
  })
  it('contains no name or figure that is not in the findings', () => {
    const names = new Set(['frontend', 'cartservice', 'redis-cart'])
    const quoted = [...(n.title + n.summary.join(' ')).matchAll(/`([^`]+)`/g)].map((m) => m[1])
    for (const q of quoted) expect(names.has(q)).toBe(true)
  })
})

describe('narrate: other causes and no cause', () => {
  it('hedges a cause that is only the deepest slow call, and says to treat it as a lead', () => {
    const f = findings(snap({ edges: [edge(FE, CART), edge(CART, REDIS, { p95ms: 900 })], inbound: { [FE]: { rps: 30, errPct: 0, p95ms: 1500 } } }))
    const n = narrate({ app: FE, kind: ['latency'] }, f)
    expect(n.title).toBe('`frontend` latency possibly caused by slow calls from `cartservice` to `redis-cart`.')
    expect(n.summary[1]).toContain('lead, not a finding')
  })
  it('names a resource cause', () => {
    const f = findings(snap({ edges: [edge(FE, CART), edge(CART, REDIS)], apps: { [CART]: app({ cpuCores: 0.98 }) }, inbound: { [FE]: { rps: 30, errPct: 0, p95ms: 1500 } } }))
    const n = narrate({ app: FE, kind: ['latency'] }, f)
    expect(n.title).toBe('`frontend` latency caused by `cartservice` running out of CPU.')
    expect(n.summary[0]).toContain('`cartservice` used 98% of its CPU limit, against 10% before.')
  })
  it('says there is no cause, and what was compared, without inventing one', () => {
    const f = findings(snap({ edges: [edge(FE, CART), edge(CART, REDIS)], inbound: { [FE]: { rps: 30, errPct: 20, p95ms: 40 } } }))
    const n = narrate({ app: FE, kind: ['availability'] }, f)
    expect(n.title).toBe('`frontend` errors: no cause found among its dependencies.')
    expect(n.summary[0]).toContain('3 applications and 2 calls downstream of `frontend`')
    expect(n.summary[0]).toContain('network round trip')
    expect(n.summary[1]).toContain('20% of the requests `frontend` served failed')
    expect(n.propagation).toBeNull()
  })
  it('when the cause is the application itself, says so rather than "through"', () => {
    const f = findings(snap({ edges: [edge(FE, CART), edge(CART, REDIS)], apps: { [FE]: app({ cpuCores: 0.99 }) }, inbound: { [FE]: { rps: 30, errPct: 0, p95ms: 1500 } } }))
    const n = narrate({ app: FE, kind: ['latency'] }, f)
    expect(n.summary[0]).toContain('come from the application itself')
    expect(n.propagation).toBeNull()
  })
})

describe('labels and sentences', () => {
  it('lists an issue the way the graph shows it', () => {
    expect(issueLabel({ scope: 'edge', kind: 'rtt', to: REDIS })).toBe('TCP network latency to redis-cart')
    expect(issueLabel({ scope: 'edge', kind: 'connect', to: REDIS })).toBe('TCP connection latency to redis-cart')
    expect(issueLabel({ scope: 'app', kind: 'logs' })).toBe('Log: errors')
    expect(issueLabel({ scope: 'app', kind: 'cpu' })).toBe('CPU')
  })
  it('counts restarts and kills in the singular and the plural', () => {
    expect(issueSentence({ scope: 'app', kind: 'restarts', app: CART, during: 1 })).toContain('restarted 1 time.')
    expect(issueSentence({ scope: 'app', kind: 'oom', app: CART, during: 3 })).toContain('3 times')
  })
  it('leaves a ratio out when it is small', () => {
    expect(issueSentence({ scope: 'edge', kind: 'latency', from: FE, to: CART, before: 100, during: 140, ratio: 1.4 })).not.toContain('x)')
  })
})

describe('segments', () => {
  it('splits text into plain and code pieces', () => {
    expect(segments('a `b` c `d`')).toEqual([{ text: 'a ', code: false }, { text: 'b', code: true }, { text: ' c ', code: false }, { text: 'd', code: true }])
    expect(segments('plain')).toEqual([{ text: 'plain', code: false }])
    expect(segments(null)).toEqual([])
  })
  it('never turns text into markup: an angle bracket stays a character', () => {
    expect(segments('`<img onerror=x>`')).toEqual([{ text: '<img onerror=x>', code: true }])
  })
})
