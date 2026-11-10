import { describe, expect, it } from 'vitest'
import { SLO_DEFAULTS, burnRate, findIncidents, sloStanding } from './detect'

const STEP = 60
const N = 240 // four hours at one-minute steps
const mk = (fn) => Array.from({ length: N }, (_, i) => fn(i))
const grid = { start: 1_000_000, step: STEP, n: N }
// 10 requests a second all the time; `bad(i)` of them failing
const app = (bad, slow = null) => ({ total: mk(() => 10), errors: mk(bad), slow: slow && mk(slow) })

describe('burnRate', () => {
  it('is how many times faster than the budget allows the bad share is', () => {
    const total = mk(() => 10)
    const bad = mk(() => 0.144) // 1.44% of requests
    expect(burnRate(total, bad, 100, 5, STEP, 0.999, 1)).toBeCloseTo(14.4, 6)
    expect(burnRate(total, mk(() => 0), 100, 5, STEP, 0.999, 1)).toBe(0)
  })
  it('is 0 when the window holds too few requests to judge (a quiet service is not burning its budget)', () => {
    const total = mk(() => 0.01) // 0.6 requests a minute
    expect(burnRate(total, mk(() => 0.01), 100, 5, STEP, 0.999, 20)).toBe(0)
  })
  it('does not read before the start of the data', () => {
    expect(burnRate(mk(() => 10), mk(() => 10), 0, 5, STEP, 0.999, 1)).toBeGreaterThan(0)
  })
})

describe('findIncidents', () => {
  it('finds nothing in healthy traffic', () => {
    expect(findIncidents(app(() => 0), grid)).toEqual([])
  })

  it('finds a nine minute outage, with its start and end at the step it began and ended', () => {
    // every request fails from minute 100 to 108 inclusive
    const found = findIncidents(app((i) => (i >= 100 && i <= 108 ? 10 : 0)), grid)
    expect(found).toHaveLength(1)
    expect(found[0].kind).toEqual(['availability'])
    expect(found[0].start).toBe(grid.start + 100 * STEP)
    expect(found[0].end).toBe(grid.start + 109 * STEP)
    expect(found[0].open).toBe(false)
  })

  it('ignores a blip shorter than two minutes and a few failures', () => {
    expect(findIncidents(app((i) => (i === 100 ? 10 : 0)), grid)).toEqual([]) // one minute
    expect(findIncidents(app((i) => (i >= 50 && i < 200 ? 0.05 : 0)), grid)).toEqual([]) // 0.5% failing: under 14.4x of a 0.1% budget
  })

  it('needs the long window too: a few bad minutes inside hours of traffic that is otherwise fine is judged on both', () => {
    // 1.5 minutes of total failure in an hour is 2.5% of the hour: 25x the budget, so it counts. 20 seconds of it is 0.55%: 5.5x, it does not.
    expect(findIncidents(app((i) => (i === 100 ? 3.4 : 0)), grid)).toEqual([])
  })

  it('finds latency as its own kind, and both kinds in one incident when they overlap', () => {
    const slowOnly = findIncidents(app(() => 0, (i) => (i >= 100 && i <= 110 ? 10 : 0)), grid)
    expect(slowOnly.map((x) => x.kind)).toEqual([['latency']])
    const both = findIncidents(app((i) => (i >= 102 && i <= 106 ? 10 : 0), (i) => (i >= 100 && i <= 110 ? 10 : 0)), grid)
    expect(both).toHaveLength(1)
    expect(both[0].kind.sort()).toEqual(['availability', 'latency'])
  })

  it('does not look for latency when there is no histogram', () => {
    expect(findIncidents(app(() => 0, null), grid)).toEqual([])
  })

  it('joins two breaches a few minutes apart into one incident, and keeps two far apart as two', () => {
    const near = findIncidents(app((i) => ((i >= 100 && i <= 104) || (i >= 107 && i <= 112) ? 10 : 0)), grid)
    expect(near).toHaveLength(1)
    const far = findIncidents(app((i) => ((i >= 30 && i <= 38) || (i >= 150 && i <= 158) ? 10 : 0)), grid)
    expect(far).toHaveLength(2)
  })

  it('says an incident is open when the data ends in it', () => {
    const found = findIncidents(app((i) => (i >= 230 ? 10 : 0)), grid)
    expect(found).toHaveLength(1)
    expect(found[0].open).toBe(true)
  })

  it('does not report an incident for an application nobody calls', () => {
    expect(findIncidents({ total: mk(() => 0), errors: mk(() => 0), slow: null }, grid)).toEqual([])
  })

  it('takes the objective and threshold from the config', () => {
    const bad = (i) => (i >= 100 && i <= 120 ? 0.5 : 0) // 5% failing for 21 minutes: 50x a 0.1% budget, 5x a 1% budget
    expect(findIncidents(app(bad), grid)).toHaveLength(1)
    expect(findIncidents(app(bad), grid, { availability: 0.99 })).toEqual([])
  })
})

describe('sloStanding', () => {
  it('gives the compliance and burn rate over a stretch', () => {
    const a = app((i) => (i >= 100 && i <= 108 ? 10 : 0), (i) => (i >= 100 && i <= 108 ? 10 : 0))
    const s = sloStanding(a, grid, 100, 108)
    expect(s.availability.compliance).toBe(0)
    expect(s.availability.burn).toBeCloseTo(1000, 3)
    const ok = sloStanding(a, grid, 0, 50)
    expect(ok.availability.compliance).toBe(1)
    expect(ok.latency.burn).toBe(0)
  })
  it('is null where it cannot be said: no histogram, no requests', () => {
    expect(sloStanding(app(() => 0), grid, 0, 10).latency).toEqual({ compliance: null, burn: null })
    expect(sloStanding({ total: mk(() => 0), errors: mk(() => 0), slow: null }, grid, 0, 10).availability).toEqual({ compliance: null, burn: null })
  })
  it('exposes the defaults the page quotes', () => {
    expect(SLO_DEFAULTS.availability).toBe(0.999)
    expect(SLO_DEFAULTS.latencySeconds).toBe(0.5)
  })
})
