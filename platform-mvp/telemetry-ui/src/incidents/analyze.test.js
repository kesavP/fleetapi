import { describe, expect, it } from 'vitest'
import { analyze, pathBetween } from './analyze'

const FE = '/k8s/shop/frontend'
const CART = '/k8s/shop/cartservice'
const REDIS = '/k8s/shop/redis-cart'
const ADS = '/k8s/shop/adservice'
const OTHER = '/k8s/ops/batch'

const edge = (from, to, o = {}) => ({ from, to, rps: 20, connPerSec: 2, errPct: 0, p95ms: 10, connMs: 1, rttMs: 0.4, ...o })
const app = (o = {}) => ({ cpuCores: 0.1, cpuLimit: 1, restarts: 0, oomKills: 0, errorLines: 0, ...o })
// a project at one moment: the shop's calls, each application's resources, and what the frontend serves
const snap = ({ edges = [], apps = {}, inbound = {} } = {}) => ({
  graph: { edges, nodes: [] },
  apps: new Map(Object.entries({ [FE]: app(), [CART]: app(), [REDIS]: app(), [ADS]: app(), ...apps })),
  inbound: new Map(Object.entries({ [FE]: { rps: 30, errPct: 0, p95ms: 40 }, ...inbound })),
})
const NORMAL_EDGES = [edge(FE, CART), edge(FE, ADS), edge(CART, REDIS)]
const normal = () => snap({ edges: NORMAL_EDGES })
const run = (during, over = {}) => analyze({ app: FE, before: normal(), during, ...over })
const kinds = (f, scope) => f.issues.filter((i) => i.scope === scope).map((i) => `${i.scope === 'edge' ? i.from.split('/').pop() + '>' + i.to.split('/').pop() : i.app.split('/').pop()}:${i.kind}`).sort()

describe('analyze: a network delay between two applications', () => {
  const during = snap({
    edges: [edge(FE, CART, { p95ms: 900 }), edge(FE, ADS), edge(CART, REDIS, { rttMs: 210, connMs: 400, p95ms: 800 })],
    inbound: { [FE]: { rps: 30, errPct: 12, p95ms: 1500 } },
  })
  const f = run(during)

  it('names the slow path as the cause, ahead of the slow calls it makes slow', () => {
    expect(f.rootCause).toMatchObject({ kind: 'rtt', scope: 'edge', from: CART, to: REDIS, confidence: 'likely' })
    expect(f.rootCause.path).toEqual([FE, CART, REDIS])
    expect(f.rootCause.issue).toMatchObject({ before: 0.4, during: 210 })
  })
  it('lists what changed, on the application that suffers it', () => {
    expect(kinds(f, 'app')).toEqual(['frontend:errors', 'frontend:latency'])
    expect(kinds(f, 'edge')).toEqual(['cartservice>redis-cart:connect', 'cartservice>redis-cart:latency', 'cartservice>redis-cart:rtt', 'frontend>cartservice:latency'])
  })
  it('shows only the applications that matter: the hurt one, the path and the dependency, not a healthy sibling', () => {
    expect(f.nodes.map((n) => n.id)).toEqual([FE, CART, REDIS])
    expect(f.nodes.map((n) => n.depth)).toEqual([0, 1, 2])
    expect(f.edges.map((e) => e.from.split('/').pop() + '>' + e.to.split('/').pop())).toEqual(['frontend>cartservice', 'cartservice>redis-cart'])
    expect(f.nodes.find((n) => n.id === CART).issues.map((i) => i.kind).sort()).toEqual(['connect', 'latency', 'rtt'])
  })
  it('says what it looked at', () => {
    expect(f.checked.calls).toBe(3)
    expect(f.checked.signals).toContain('network round trip')
  })
})

describe('analyze: the other causes', () => {
  it('a slow callee with a normal network: the deepest slow call, as a possible cause', () => {
    const f = run(snap({
      edges: [edge(FE, CART, { p95ms: 900 }), edge(FE, ADS), edge(CART, REDIS, { p95ms: 800 })],
      inbound: { [FE]: { rps: 30, errPct: 0, p95ms: 1500 } },
    }))
    expect(f.rootCause).toMatchObject({ kind: 'latency', from: CART, to: REDIS, confidence: 'possible' })
  })
  it('an application at its CPU limit, whose own calls are fine, is the cause (a resource problem)', () => {
    const f = run(snap({
      edges: [edge(FE, CART, { p95ms: 900 }), edge(FE, ADS), edge(CART, REDIS)],
      apps: { [CART]: app({ cpuCores: 0.98 }) },
      inbound: { [FE]: { rps: 30, errPct: 0, p95ms: 1500 } },
    }))
    expect(f.rootCause).toMatchObject({ kind: 'cpu', scope: 'app', app: CART, confidence: 'likely' })
    expect(f.rootCause.issue).toMatchObject({ before: 10, during: 98 })
  })
  it('an application that is slow because of a slow callee is not blamed for its CPU: the callee is', () => {
    const f = run(snap({
      edges: [edge(FE, CART), edge(CART, REDIS, { p95ms: 900 })],
      apps: { [CART]: app({ cpuCores: 0.98 }) },
      inbound: { [FE]: { rps: 30, errPct: 0, p95ms: 1500 } },
    }))
    expect(f.rootCause).toMatchObject({ kind: 'latency', to: REDIS })
  })
  it('out-of-memory kills and restarts that began in the incident are issues, an old crash loop is not', () => {
    const during = snap({ edges: NORMAL_EDGES, apps: { [CART]: app({ oomKills: 2, restarts: 2 }) }, inbound: { [FE]: { rps: 30, errPct: 30, p95ms: 40 } } })
    expect(kinds(run(during), 'app')).toEqual(['cartservice:oom', 'cartservice:restarts', 'frontend:errors'])
    const before = snap({ edges: NORMAL_EDGES, apps: { [CART]: app({ oomKills: 5, restarts: 5 }) } })
    expect(kinds(analyze({ app: FE, before, during }), 'app')).toEqual(['frontend:errors'])
  })
  it('a callee that fails on a call it answers is a cause of errors', () => {
    const f = run(snap({ edges: [edge(FE, CART), edge(CART, REDIS, { errPct: 60 })], inbound: { [FE]: { rps: 30, errPct: 40, p95ms: 40 } } }))
    expect(f.rootCause).toMatchObject({ kind: 'errors', from: CART, to: REDIS })
    expect(f.rootCause.issue.during).toBe(60)
  })
  it('error log lines are compared as rates, as the two windows are not the same length', () => {
    const base = snap({ edges: NORMAL_EDGES, apps: { [CART]: app({ errorLines: 60 }) } }) // 60 lines in an hour: 1 a minute
    const quiet = analyze({ app: FE, before: base, during: snap({ edges: NORMAL_EDGES, apps: { [CART]: app({ errorLines: 10 }) } }), seconds: { before: 3600, during: 600 } })
    expect(kinds(quiet, 'app')).toEqual([]) // 10 in ten minutes is the same rate
    const loud = analyze({ app: FE, before: base, during: snap({ edges: NORMAL_EDGES, apps: { [CART]: app({ errorLines: 60 }) } }), seconds: { before: 3600, during: 600 } })
    expect(kinds(loud, 'app')).toEqual(['cartservice:logs'])
  })
})

describe('analyze: leaving things alone', () => {
  it('finds nothing when nothing changed, and says so without a made-up cause', () => {
    const f = run(normal())
    expect(f.issues).toEqual([])
    expect(f.rootCause).toBeNull()
    expect(f.nodes.map((n) => n.id)).toEqual([FE])
  })
  it('does not call a small rise an event: 0.4 ms to 1.5 ms is three times, but not two milliseconds more', () => {
    const f = run(snap({ edges: [edge(FE, CART), edge(CART, REDIS, { rttMs: 1.5 })] }))
    expect(f.issues).toEqual([])
  })
  it('does not judge a call that hardly happens', () => {
    const f = run(snap({ edges: [edge(FE, CART), edge(CART, REDIS, { rttMs: 300, rps: 0.01, connPerSec: 0.01 })] }))
    expect(f.issues).toEqual([])
  })
  it('does not compare with a "before" that did not have the reading (a new call, or no metric)', () => {
    const before = snap({ edges: [edge(FE, CART), edge(CART, REDIS, { rttMs: null })] })
    const f = analyze({ app: FE, before, during: snap({ edges: [edge(FE, CART), edge(CART, REDIS, { rttMs: 300 })] }) })
    expect(f.issues.filter((i) => i.kind === 'rtt')).toEqual([])
  })
  it('ignores trouble in applications the hurt one does not call, even if they have it', () => {
    const before = snap({ edges: [...NORMAL_EDGES, edge(OTHER, REDIS)] }) // it had a baseline, so it is judged if it is looked at
    const f = analyze({ app: FE, before, during: snap({ edges: [...NORMAL_EDGES, edge(OTHER, REDIS, { rttMs: 500 })], apps: { [OTHER]: app({ cpuCores: 1 }) } }) })
    expect(f.issues).toEqual([])
    expect(f.nodes.map((n) => n.id)).toEqual([FE])
    expect(f.checked).toMatchObject({ applications: 4, calls: 3 }) // what the frontend reaches, not every application in the project
  })
  it('takes thresholds from the config', () => {
    const during = snap({ edges: [edge(FE, CART), edge(CART, REDIS, { rttMs: 1.5 })] })
    const f = run(during, { config: { rttRatio: 2, rttMinRiseMs: 1 } })
    expect(kinds(f, 'edge')).toEqual(['cartservice>redis-cart:rtt'])
  })
  it('copes with a cycle in the calls', () => {
    const f = run(snap({ edges: [edge(FE, CART), edge(CART, FE), edge(CART, REDIS, { rttMs: 300 })] }))
    expect(f.rootCause.to).toBe(REDIS)
    expect(f.nodes.length).toBe(3)
  })
})

describe('analyze: checks that are turned off', () => {
  const during = snap({
    edges: [edge(FE, CART, { p95ms: 900 }), edge(CART, REDIS, { rttMs: 210, p95ms: 800 })],
    inbound: { [FE]: { rps: 30, errPct: 12, p95ms: 1500 } },
  })
  it('leaves a check out of the analysis altogether: it neither shows nor is the cause', () => {
    const f = run(during, { config: { disabled: ['rtt'] } })
    expect(f.issues.some((i) => i.kind === 'rtt')).toBe(false)
    expect(f.rootCause).toMatchObject({ kind: 'latency', to: REDIS, confidence: 'possible' }) // the next best explanation, said as less sure
  })
  it('can turn off everything, and then finds nothing and says no cause', () => {
    const f = run(during, { config: { disabled: ['rtt', 'connect', 'latency', 'errors', 'cpu', 'logs', 'restarts', 'oom'] } })
    expect(f.issues).toEqual([])
    expect(f.rootCause).toBeNull()
  })
  it('ignores a name it does not know', () => {
    expect(run(during, { config: { disabled: ['nonsense'] } }).rootCause.kind).toBe('rtt')
  })
})

describe('pathBetween', () => {
  const e = (from, to) => ({ from, to })
  it('is the shortest list of applications along calls', () => {
    expect(pathBetween([e('a', 'b'), e('b', 'c'), e('a', 'c')], 'a', 'c')).toEqual(['a', 'c'])
    expect(pathBetween([e('a', 'b'), e('b', 'c')], 'a', 'c')).toEqual(['a', 'b', 'c'])
  })
  it('is null when there is none, and the application itself for no distance', () => {
    expect(pathBetween([e('a', 'b')], 'b', 'a')).toBeNull()
    expect(pathBetween([], 'a', 'a')).toEqual(['a'])
  })
  it('does not loop on a cycle', () => {
    expect(pathBetween([e('a', 'b'), e('b', 'a')], 'a', 'z')).toBeNull()
  })
})
