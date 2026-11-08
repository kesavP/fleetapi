import { describe, expect, it, vi } from 'vitest'
import { createAlertStore } from './store'

const A = '/k8s/shop/cartservice'
const B = '/k8s/shop/frontend'
const edge = (from, to, o = {}) => ({ from, to, rps: 10, connPerSec: 0, failedConnects: 0, callErrors: 0, ...o })
const look = (edges, listeners) => ({ edges, nodes: [...new Set(edges.flatMap((e) => [e.from, e.to]))].map((id) => ({ id, external: false })), listeners, incomplete: false })
const live = { seen: [A, B], fresh: [A, B] }
const HEALTHY = look([edge(B, A)], live)
const BROKEN = look([edge(B, A, { callErrors: 8 })], live) // 80% failing

function memoryStorage() {
  const m = new Map()
  return { getItem: (k) => (m.has(k) ? m.get(k) : null), setItem: (k, v) => m.set(k, v), removeItem: (k) => m.delete(k), m }
}
function setup({ recent = HEALTHY, baseline = HEALTHY, project = 'gke' } = {}) {
  let t = 1_700_000_000_000
  const calls = []
  const ctx = {
    recent, baseline, project, fail: '',
    serverAnswer: () => { throw Object.assign(new Error('The telemetry service is not reachable right now.'), { kind: 'unavailable', status: 502 }) }, // no alert service
    storage: memoryStorage(),
    clock: { advance: (ms) => (t += ms) },
  }
  const sessionObj = { get: () => (ctx.project ? { project: ctx.project, key: 'k' } : null) }
  const client = { get: vi.fn(async (path) => (path === 'alerts' ? ctx.serverAnswer() : ['container_net_tcp_listen_info'])) }
  const load = vi.fn(async (c, { seconds, now }) => {
    calls.push({ seconds, end: now() })
    if (ctx.fail) throw new Error(ctx.fail)
    return seconds <= 300 ? ctx.recent : ctx.baseline
  })
  const timers = { set: vi.fn((f, ms) => ({ f, ms })), clear: vi.fn() }
  ctx.store = createAlertStore({ client, session: sessionObj, storage: ctx.storage, now: () => t, load, timers })
  ctx.calls = calls
  ctx.timers = timers
  ctx.client = client
  ctx.load = load
  ctx.step = async () => {
    ctx.clock.advance(60_000)
    await ctx.store.evaluate()
  }
  return ctx
}

describe('alert store', () => {
  it('reads the project once and judges a recent window against a baseline that ends where it begins', async () => {
    const c = setup()
    await c.store.evaluate()
    expect(c.client.get.mock.calls.filter(([p]) => p === 'metrics')).toHaveLength(1)
    const [recent, baseline] = c.calls
    expect(recent.seconds).toBe(300)
    expect(baseline.seconds).toBe(3600)
    expect(baseline.end).toBe(recent.end - 300_000)
    expect(c.store.state.alerts).toEqual([])
    expect(c.store.state.checked).toBe(2)
    expect(c.store.state.lastRun).toBeGreaterThan(0)
  })

  it('holds a new problem as pending and fires it on the second evaluation', async () => {
    const c = setup({ recent: BROKEN })
    await c.store.evaluate()
    expect(c.store.state.alerts.map((a) => [a.rule, a.status])).toEqual([['errors', 'pending']])
    expect(c.store.firing()).toEqual([])
    await c.step()
    expect(c.store.state.alerts[0].status).toBe('firing')
    expect(c.store.firing()).toHaveLength(1)
  })

  it('resolves it after the problem has been gone for two evaluations', async () => {
    const c = setup({ recent: BROKEN })
    await c.store.evaluate()
    await c.step()
    c.recent = HEALTHY
    await c.step()
    expect(c.store.state.alerts[0].status).toBe('firing')
    await c.step()
    expect(c.store.state.alerts[0].status).toBe('resolved')
    expect(c.store.firing()).toEqual([])
  })

  it('keeps what was firing, and says it could not look, when an evaluation fails', async () => {
    const c = setup({ recent: BROKEN })
    await c.store.evaluate()
    await c.step()
    c.fail = 'The telemetry service is not reachable right now.'
    await c.step()
    await c.step()
    await c.step()
    expect(c.store.state.alerts[0].status).toBe('firing') // not resolved by an outage of the read path
    expect(c.store.state.lastError).toContain('not reachable')
    expect(c.store.state.errorSince).toBeGreaterThan(0)
    c.fail = ''
    await c.step()
    expect(c.store.state.lastError).toBe('')
  })

  it('does nothing when not connected', async () => {
    const c = setup({ project: '' })
    await c.store.evaluate()
    expect(c.client.get).not.toHaveBeenCalled()
  })

  describe('with the alert service', () => {
    const SERVER = (o = {}) => ({
      alerts: [{ id: 'errors|x', key: 'errors|x@1', rule: 'errors', service: 'x', label: 'x', severity: 'critical', summary: 's', evidence: {}, status: 'firing', firstSeen: 5, lastSeen: 9 }],
      checked: 7, skipped: ['a note'], lastRun: 1234, lastError: '', errorSince: 0, config: { errorShare: 5 }, intervalSeconds: 60, evaluatedBy: 'server', ...o,
    })

    it('shows what the service found and does no checking of its own', async () => {
      const c = setup()
      c.serverAnswer = () => SERVER()
      await c.store.evaluate()
      expect(c.store.state).toMatchObject({ source: 'server', checked: 7, lastRun: 1234, serverInterval: 60, skipped: ['a note'] })
      expect(c.store.state.alerts.map((a) => a.status)).toEqual(['firing'])
      expect(c.store.state.serverConfig).toEqual({ errorShare: 5 })
      expect(c.load).not.toHaveBeenCalled()
      expect(c.client.get.mock.calls.filter(([p]) => p === 'metrics')).toHaveLength(0)
      expect(c.store.firing()).toHaveLength(1)
    })

    it('shows the service\'s own "could not look" and keeps nothing in the tab', async () => {
      const c = setup()
      c.serverAnswer = () => SERVER({ lastError: 'The telemetry service is not reachable right now.', errorSince: 99 })
      await c.store.evaluate()
      expect(c.store.state).toMatchObject({ lastError: 'The telemetry service is not reachable right now.', errorSince: 99 })
      expect(c.storage.m.size).toBe(0)
    })

    it('falls back to checking for itself when the service is not there (502 or 404)', async () => {
      const c = setup({ recent: BROKEN })
      await c.store.evaluate() // the default answer is a 502
      expect(c.store.state.source).toBe('browser')
      expect(c.load).toHaveBeenCalledTimes(2)
      c.serverAnswer = () => { throw Object.assign(new Error('Not found.'), { kind: 'request', status: 404 }) }
      await c.step()
      expect(c.store.state.source).toBe('browser')
      expect(c.store.state.alerts[0].status).toBe('firing')
    })

    it('does not fall back on a refused key or an unreachable app: that is an error to show', async () => {
      for (const err of [
        Object.assign(new Error('This key cannot read project "gke". Check the key and the project name.'), { kind: 'auth', status: 403 }),
        Object.assign(new Error('Cannot reach the server. Is it running, and are you online?'), { kind: 'network', status: 0 }),
      ]) {
        const c = setup()
        c.serverAnswer = () => { throw err }
        await c.store.evaluate()
        expect(c.store.state.lastError).toBe(err.message)
        expect(c.load).not.toHaveBeenCalled()
        expect(c.store.state.source).toBe('')
      }
    })

    it('uses its own checking when the answer is not the service\'s (a path that returns something else)', async () => {
      const c = setup()
      c.serverAnswer = () => ({ series: [] })
      await c.store.evaluate()
      expect(c.store.state.source).toBe('browser')
    })

    it('goes back to what the tab had kept if the service is removed, and takes over again when it returns', async () => {
      const c = setup({ recent: BROKEN })
      await c.store.evaluate()
      await c.step() // the tab's own alert is firing
      c.serverAnswer = () => SERVER({ alerts: [] })
      await c.step()
      expect(c.store.state).toMatchObject({ source: 'server', alerts: [] })
      c.serverAnswer = () => { throw Object.assign(new Error('x'), { kind: 'unavailable', status: 502 }) }
      await c.step()
      expect(c.store.state.source).toBe('browser')
      expect(c.store.state.alerts.length).toBeGreaterThan(0) // the tab's own record, restored
      c.serverAnswer = () => SERVER()
      await c.step()
      expect(c.store.state.source).toBe('server')
    })

    it('does not mix projects when the service answers after a switch', async () => {
      const c = setup()
      c.serverAnswer = () => { c.project = 'other'; return SERVER() }
      await c.store.evaluate()
      expect(c.store.state.alerts).toEqual([]) // the answer was for the project that was connected when it was asked
    })
  })

  it('does not run two evaluations at once', async () => {
    const c = setup()
    await Promise.all([c.store.evaluate(), c.store.evaluate()])
    expect(c.load).toHaveBeenCalledTimes(2) // one pair of reads, not two
  })

  it('keeps alerts per project and starts clean for another', async () => {
    const c = setup({ recent: BROKEN })
    await c.store.evaluate()
    await c.step()
    expect(c.store.state.alerts).toHaveLength(1)
    c.project = 'other'
    c.recent = HEALTHY
    await c.step()
    expect(c.store.state.project).toBe('other')
    expect(c.store.state.alerts).toEqual([])
    c.project = 'gke'
    await c.step() // back: what was kept for gke is restored, then judged again
    expect(c.store.state.project).toBe('gke')
    expect(c.store.state.alerts.length).toBeGreaterThan(0)
  })

  it('survives a reload: alerts and thresholds come back from the tab\'s storage', async () => {
    const c = setup({ recent: BROKEN })
    await c.store.evaluate()
    await c.step()
    c.store.setConfig({ errorShare: 20 })
    const again = createAlertStore({ client: c.client, session: { get: () => ({ project: 'gke', key: 'k' }) }, storage: c.storage, now: () => 1_700_000_000_000 + 999_999, load: async () => HEALTHY, timers: c.timers })
    await again.evaluate()
    expect(again.state.config.errorShare).toBe(20)
    expect(again.state.alerts[0]).toMatchObject({ rule: 'errors', firstSeen: 1_700_000_000_000 })
  })

  it('never puts a key in storage', async () => {
    const c = setup({ recent: BROKEN })
    await c.store.evaluate()
    await c.step()
    expect(JSON.stringify([...c.storage.m.values()])).not.toContain('"k"')
  })

  it('applies a threshold change on the next evaluation, and ignores a bad value', async () => {
    const c = setup({ recent: BROKEN })
    c.store.setConfig({ errorShare: 90 }) // 80% is now below the line
    await c.store.evaluate()
    expect(c.store.state.alerts).toEqual([])
    c.store.setConfig({ errorShare: -4, bogus: 1 })
    expect(c.store.state.config.errorShare).toBe(90)
    expect(c.store.state.config.bogus).toBeUndefined()
    c.store.resetConfig()
    expect(c.store.state.config.errorShare).toBe(5)
  })

  it('starts one timer, evaluates at once, and stops it', async () => {
    const c = setup()
    c.store.start()
    c.store.start()
    expect(c.timers.set).toHaveBeenCalledTimes(1)
    expect(c.timers.set.mock.calls[0][1]).toBe(60_000)
    await new Promise((r) => setTimeout(r, 0))
    expect(c.load).toHaveBeenCalled()
    c.store.stop()
    expect(c.timers.clear).toHaveBeenCalledTimes(1)
  })

  it('forgets everything on reset', async () => {
    const c = setup({ recent: BROKEN })
    await c.store.evaluate()
    await c.step()
    c.store.reset()
    expect(c.store.state.alerts).toEqual([])
    expect(c.storage.getItem('fleet-ui.alerts.gke')).toBeNull() // the project's alerts are gone; the thresholds are a preference and stay
  })

  it('works without storage', async () => {
    const c = setup({ recent: BROKEN })
    const s = createAlertStore({ client: c.client, session: { get: () => ({ project: 'gke', key: 'k' }) }, storage: null, now: () => 1, load: async () => BROKEN, timers: c.timers })
    await s.evaluate()
    expect(s.state.alerts).toHaveLength(1)
  })
})
