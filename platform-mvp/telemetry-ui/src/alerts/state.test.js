import { describe, expect, it } from 'vitest'
import { CLEAR, CONFIRM, KEEP_RESOLVED_MS, MAX_RESOLVED, reconcile, sorted } from './state'

const cond = (id, o = {}) => ({ id, rule: 'errors', service: id, label: id, severity: 'warning', summary: 's', evidence: {}, ...o })
const run = (steps, start = []) => steps.reduce((s, [conds, t]) => reconcile(s, conds, t), start)
const get = (list, id) => list.find((a) => a.id === id)

describe('reconcile', () => {
  it('holds a new condition as pending, and fires it when it holds on the next evaluation too', () => {
    const one = run([[[cond('a')], 1000]])
    expect(get(one, 'a')).toMatchObject({ status: 'pending', firstSeen: 1000, seen: 1 })
    const two = reconcile(one, [cond('a')], 2000)
    expect(get(two, 'a')).toMatchObject({ status: 'firing', firstSeen: 1000, lastSeen: 2000, seen: 2 })
    expect(CONFIRM).toBe(2)
  })

  it('gives each episode a key of its own', () => {
    const list = run([[[cond('a')], 1000]])
    expect(get(list, 'a').key).toBe('a@1000')
    expect(reconcile(list, [cond('a')], 2000)[0].key).toBe('a@1000') // it does not change while the episode goes on
  })

  it('drops a condition that held once and went away, without a trace', () => {
    expect(run([[[cond('a')], 1000], [[], 2000]])).toEqual([])
  })

  it('keeps firing through one missed evaluation, and resolves after two', () => {
    const firing = run([[[cond('a')], 1], [[cond('a')], 2]])
    const missedOnce = reconcile(firing, [], 3)
    expect(get(missedOnce, 'a')).toMatchObject({ status: 'firing', missed: 1 })
    const back = reconcile(missedOnce, [cond('a')], 4)
    expect(get(back, 'a')).toMatchObject({ status: 'firing', missed: 0, firstSeen: 1 }) // the wobble did not restart it
    const gone = reconcile(reconcile(firing, [], 3), [], 4)
    expect(get(gone, 'a')).toMatchObject({ status: 'resolved', resolvedAt: 4 })
    expect(CLEAR).toBe(2)
  })

  it('starts a new episode when a resolved alert comes back', () => {
    const resolved = run([[[cond('a')], 1], [[cond('a')], 2], [[], 3], [[], 4]])
    const again = reconcile(resolved, [cond('a')], 10)
    expect(again.filter((a) => a.id === 'a' && a.status !== 'resolved')).toHaveLength(1)
    expect(again.find((a) => a.status === 'pending')).toMatchObject({ firstSeen: 10, seen: 1 })
    expect(again.find((a) => a.status === 'resolved')).toMatchObject({ resolvedAt: 4 }) // the earlier episode stays in the history
    expect(new Set(again.map((a) => a.key)).size).toBe(again.length) // and each episode has its own key
  })

  it('takes the latest summary, evidence and severity while it fires', () => {
    const list = run([[[cond('a', { severity: 'warning', evidence: { n: 1 } })], 1], [[cond('a', { severity: 'critical', evidence: { n: 2 } })], 2]])
    expect(get(list, 'a')).toMatchObject({ severity: 'critical', evidence: { n: 2 } })
  })

  it('keeps independent alerts independent', () => {
    const list = run([[[cond('a'), cond('b')], 1], [[cond('a')], 2]])
    expect(get(list, 'a').status).toBe('firing')
    expect(get(list, 'b')).toBeUndefined()
  })

  it('forgets resolved alerts after a day, and keeps a bounded history', () => {
    const resolved = run([[[cond('a')], 1], [[cond('a')], 2], [[], 3], [[], 4]])
    expect(reconcile(resolved, [], 4 + KEEP_RESOLVED_MS - 1)).toHaveLength(1)
    expect(reconcile(resolved, [], 4 + KEEP_RESOLVED_MS)).toHaveLength(0)
    const many = Array.from({ length: MAX_RESOLVED + 10 }, (_, i) => ({ ...cond(`r${i}`), status: 'resolved', resolvedAt: 100 + i, firstSeen: 1, lastSeen: 2, seen: 2, missed: 2 }))
    expect(reconcile(many, [], 1000)).toHaveLength(MAX_RESOLVED)
  })

  it('does not change what it was given', () => {
    const prev = run([[[cond('a')], 1]])
    const copy = JSON.parse(JSON.stringify(prev))
    reconcile(prev, [cond('a')], 2)
    expect(prev).toEqual(copy)
  })
})

describe('sorted', () => {
  it('puts firing critical first, then firing warnings, pending, then resolved newest first', () => {
    const mk = (id, status, severity, o = {}) => ({ ...cond(id), status, severity, firstSeen: 1, ...o })
    const list = sorted([
      mk('res-old', 'resolved', 'critical', { resolvedAt: 5 }),
      mk('pend', 'pending', 'critical'),
      mk('warn', 'firing', 'warning'),
      mk('res-new', 'resolved', 'warning', { resolvedAt: 9 }),
      mk('crit-late', 'firing', 'critical', { firstSeen: 50 }),
      mk('crit-early', 'firing', 'critical', { firstSeen: 10 }),
    ])
    expect(list.map((a) => a.id)).toEqual(['crit-early', 'crit-late', 'warn', 'pend', 'res-new', 'res-old'])
  })
})
