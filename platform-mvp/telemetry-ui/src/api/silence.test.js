import { describe, expect, it } from 'vitest'
import { silenceOf } from './silence.js'

const wl = (over = {}) => ({ kind: 'deployment', desired: 2, ready: 2, available: 2, observed: 1, reasons: [], ...over })
const quiet = { running: 0, seen: 1 }
const alive = { agentAlive: true, hasInventory: true }

describe('silenceOf', () => {
  it('says nothing about an application that is sending data', () => {
    expect(silenceOf({ instances: { running: 1, seen: 2 }, workload: wl() }, alive)).toBeNull()
    expect(silenceOf({ instances: { running: 3, seen: 3 }, workload: wl({ desired: 0 }) }, alive)).toBeNull() // pods still shutting down are still sending
  })

  it('says nothing for anything when nothing at all is sending: that is the agent or the way to the hub, said once elsewhere', () => {
    expect(silenceOf({ instances: quiet, workload: wl({ desired: 0 }) }, { agentAlive: false, hasInventory: true })).toBeNull()
    expect(silenceOf({ instances: quiet, workload: null }, { agentAlive: false, hasInventory: false })).toBeNull()
  })

  it('tells a workload set to run nothing from one that should be running', () => {
    expect(silenceOf({ instances: quiet, workload: wl({ desired: 0, ready: 0 }) }, alive)).toMatchObject({ cause: 'scaled-to-zero', tone: 'info', label: 'Scaled to zero' })
    expect(silenceOf({ instances: null, workload: wl({ desired: 0, ready: 0 }) }, alive)).toMatchObject({ cause: 'scaled-to-zero' }) // scaled down so long ago it has no series left
  })

  it('is not ready when fewer are ready than it is set to run, and says why when it knows', () => {
    const s = silenceOf({ instances: quiet, workload: wl({ desired: 3, ready: 0, reasons: ['CrashLoopBackOff (OOMKilled) ×2', 'Unschedulable'] }) }, alive)
    expect(s).toMatchObject({ cause: 'not-ready', tone: 'bad', label: 'Not ready' })
    expect(s.detail).toBe('0 of 3 ready: CrashLoopBackOff (OOMKilled) ×2, Unschedulable')
    expect(silenceOf({ instances: quiet, workload: wl({ desired: 3, ready: 1 }) }, alive).detail).toBe('1 of 3 ready')
  })

  it('says it is ready and nothing arrives from it, when that is so', () => {
    expect(silenceOf({ instances: null, workload: wl({ desired: 2, ready: 2 }) }, alive)).toMatchObject({ cause: 'no-data', tone: 'caution' })
    expect(silenceOf({ instances: quiet, workload: wl({ desired: 2, ready: 3 }) }, alive)).toMatchObject({ cause: 'no-data' })
  })

  it('does not call a cron job silent: idle between runs is how it is', () => {
    expect(silenceOf({ instances: quiet, workload: wl({ kind: 'cronjob', desired: 0, ready: 0 }) }, alive)).toBeNull()
    expect(silenceOf({ instances: null, workload: wl({ kind: 'cronjob', desired: 1, ready: 0 }) }, alive)).toBeNull()
  })

  it('without an inventory entry: removed from a cluster that has an inventory, stopped where there is none', () => {
    expect(silenceOf({ instances: quiet, workload: null }, { agentAlive: true, hasInventory: true })).toMatchObject({ cause: 'removed', tone: 'info' })
    const s = silenceOf({ instances: quiet, workload: null }, { agentAlive: true, hasInventory: false })
    expect(s).toMatchObject({ cause: 'stopped', tone: 'caution', label: 'Stopped sending' })
    expect(s.detail).toContain('cluster collector')
  })

  it('says nothing about what was never seen sending: a name from a trace, a node of the call map', () => {
    expect(silenceOf({ instances: null, workload: null }, alive)).toBeNull()
    expect(silenceOf({ instances: null, workload: null }, { agentAlive: true, hasInventory: false })).toBeNull()
  })
})
