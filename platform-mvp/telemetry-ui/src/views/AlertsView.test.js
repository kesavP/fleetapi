import { afterEach, describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { createRouter, createMemoryHistory } from 'vue-router'
import AlertsView from './AlertsView.vue'
import { createAlertStore } from '../alerts/store'

const stub = { template: '<i/>' }
const A = '/k8s/shop/cartservice'
const B = '/k8s/shop/frontend'
const edge = (from, to, o = {}) => ({ from, to, rps: 10, connPerSec: 0, failedConnects: 0, callErrors: 0, ...o })
const look = (edges, listeners = { seen: [A, B], fresh: [A, B] }) => ({ edges, nodes: [...new Set(edges.flatMap((e) => [e.from, e.to]))].map((id) => ({ id, external: false })), listeners, incomplete: false })
const HEALTHY = look([edge(B, A)])
const BROKEN = look([edge(B, A, { callErrors: 8 })])

const session = (v = { project: 'gke', key: 'k' }) => ({ get: () => v })

function clientOf({ fail = {}, server = null } = {}) {
  return {
    get: vi.fn(async (path, params) => {
      if (fail[path]) throw new Error(fail[path])
      if (path === 'alerts') {
        if (server) return server
        throw Object.assign(new Error('The telemetry service is not reachable right now.'), { kind: 'unavailable', status: 502 }) // no alert service
      }
      if (path === 'metrics') return []
      if (path === 'traces') return [{ span_id: 's', trace_id: 'a'.repeat(32), service: params.service, name: 'GET /cart', status_code: 'ERROR', status_message: 'upstream connect error password=hunter2', duration_ns: 2e6, time: new Date().toISOString() }]
      if (path === 'logs') return [{ body: 'cart failed: connection refused', severity: 'ERROR', severity_number: 17, time: new Date().toISOString() }]
      if (path === 'profiles') return []
      return { series: [] }
    }),
  }
}

async function mountView({ recent = HEALTHY, baseline = HEALTHY, sess = session(), client = clientOf(), steps = 2 } = {}) {
  const ctx = { recent, baseline, fail: '' }
  let t = Date.now()
  const store = createAlertStore({
    client, session: sess, storage: null, now: () => t,
    load: async (c, { seconds }) => {
      if (ctx.fail) throw new Error(ctx.fail)
      return seconds <= 300 ? ctx.recent : ctx.baseline
    },
    timers: { set: () => 1, clear: () => {} },
  })
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [{ path: '/', component: stub }, { path: '/connection', component: stub }, { path: '/applications/:name', name: 'application', component: stub }],
  })
  router.push('/')
  await router.isReady()
  const w = mount(AlertsView, { props: { client, session: sess, store }, global: { plugins: [router] } })
  await flushPromises()
  for (let i = 1; i < steps; i++) {
    t += 60_000
    await store.evaluate()
  }
  await flushPromises()
  return { w, store, ctx, client, step: async () => ((t += 60_000), await store.evaluate(), await flushPromises()) }
}

afterEach(() => vi.restoreAllMocks())

describe('AlertsView', () => {
  it('says all is well, and how many services it looked at, when nothing is wrong', async () => {
    const { w } = await mountView()
    expect(w.get('[data-testid=all-clear]').text()).toContain('Nothing is wrong')
    expect(w.get('[data-testid=last-run]').text()).toContain('2 services looked at')
    expect(w.get('[data-testid=last-run]').text()).not.toMatch(/\d{10,}/) // a readable time, not a millisecond count
    expect(w.findAll('[data-alert]')).toHaveLength(0)
  })

  it('shows a firing alert with its rule, service, summary, evidence and callers', async () => {
    const { w } = await mountView({ recent: BROKEN })
    const a = w.get('[data-alert]')
    expect(a.classes()).toContain('firing')
    expect(a.text()).toContain('Calls failing')
    expect(a.text()).toContain('shop/cartservice')
    expect(a.text()).toContain('80% of the calls')
    expect(a.text()).toContain('errorSharePct')
    expect(a.text()).toContain('Busiest callers: shop/frontend')
    expect(a.text()).not.toMatch(/\d{10,}/) // "since" is a readable time too
    expect(a.get('a.name').attributes('href')).toBe('/applications/%2Fk8s%2Fshop%2Fcartservice')
    expect(w.find('[data-testid=all-clear]').exists()).toBe(false)
  })

  it('marks a first sighting as confirming, not firing', async () => {
    const { w } = await mountView({ recent: BROKEN, steps: 1 })
    expect(w.get('[data-alert]').classes()).toContain('pending')
    expect(w.get('[data-alert]').text()).toContain('confirming')
  })

  it('lists a resolved alert under a heading, not among the active ones', async () => {
    const { w, ctx, step } = await mountView({ recent: BROKEN })
    ctx.recent = HEALTHY
    await step()
    await step()
    expect(w.findAll('[data-alert]')).toHaveLength(0)
    expect(w.get('details.resolved summary').text()).toContain('Resolved in the last day (1)')
    expect(w.findAll('[data-resolved]')).toHaveLength(1)
  })

  it('says it could not check, and that the alerts shown have not been cleared', async () => {
    const { w, ctx, step } = await mountView({ recent: BROKEN })
    ctx.fail = 'The telemetry service is not reachable right now.'
    await step()
    const err = w.get('[role=alert]').text()
    expect(err).toContain('Could not check')
    expect(err).toContain('not reachable')
    expect(err).toContain('have not been cleared')
    expect(w.findAll('[data-alert]')).toHaveLength(1)
  })

  it('does not call the API when not connected', async () => {
    const client = clientOf()
    const { w } = await mountView({ sess: session(null), client })
    expect(client.get).not.toHaveBeenCalled()
    expect(w.text()).toContain('Not connected')
  })

  it('edits a threshold, which takes effect on the next check', async () => {
    const { w, store, step } = await mountView({ recent: BROKEN })
    const input = w.get('#t-errorShare')
    expect(input.element.value).toBe('5')
    await input.setValue('95')
    await input.trigger('change')
    expect(store.state.config.errorShare).toBe(95)
    await step()
    expect(w.findAll('[data-alert]')).toHaveLength(1) // an alert that was firing needs two checks without the problem to resolve
    await step()
    expect(w.findAll('[data-alert]')).toHaveLength(0)
    await w.get('details.thresholds button').trigger('click')
    expect(store.state.config.errorShare).toBe(5)
  })

  it('says when the checking is done by this page, and that it stops when the app is closed', async () => {
    const { w } = await mountView()
    expect(w.get('[data-testid=source-browser]').text()).toContain('Nothing is checking while this app is closed')
    expect(w.find('[data-testid=source-server]').exists()).toBe(false)
    expect(w.find('#t-errorShare').exists()).toBe(true) // thresholds are editable here
  })

  describe('with the alert service', () => {
    const SERVER = {
      alerts: [{ id: 'no-instances|/k8s/shop/cartservice', key: 'k1', rule: 'no-instances', service: '/k8s/shop/cartservice', label: 'shop/cartservice', severity: 'critical', summary: 'cartservice was listening and no instance has reported.', evidence: { lastTrafficPerSec: 10 }, status: 'firing', firstSeen: Date.now() - 600_000, lastSeen: Date.now() }],
      checked: 4, skipped: [], lastRun: Date.now(), lastError: '', errorSince: 0, intervalSeconds: 60, evaluatedBy: 'server',
      config: { recentSeconds: 300, baselineSeconds: 7200, minBaselineRate: 0.1, dropRatio: 0.05, failedConnectShare: 20, minFailedConnects: 5, errorShare: 12, minCalls: 20 },
    }

    it('shows what the service found, says it checks whether or not the app is open, and shows its thresholds read-only', async () => {
      const client = clientOf({ server: SERVER })
      const { w } = await mountView({ client, steps: 1 })
      const text = w.get('[data-testid=source-server]').text()
      expect(text).toContain('alert service')
      expect(text).toContain('whether or not anyone has this app open')
      expect(text).toContain('2 h before them') // its own baseline, not this tab's
      expect(w.findAll('[data-alert]')).toHaveLength(1)
      expect(w.get('[data-alert]').text()).toContain('Stopped sending data')
      const th = w.get('[data-testid=thresholds-server]').text()
      expect(th).toContain('telemetryAlerts.thresholds')
      expect(th).toContain('12') // errorShare as the service has it
      expect(w.find('#t-errorShare').exists()).toBe(false)
      expect(w.findAll('button').find((b) => b.text() === 'Refresh')).toBeTruthy()
      expect(client.get.mock.calls.some(([p]) => p === 'metrics/query_range')).toBe(false) // no checking of its own
    })

    it('says the service has not finished its first check, and when it could not check', async () => {
      const fresh = await mountView({ client: clientOf({ server: { ...SERVER, alerts: [], lastRun: 0, checked: 0 } }), steps: 1 })
      expect(fresh.w.get('[data-testid=not-yet]').text()).toContain('alert service has not finished its first check')
      const failing = await mountView({ client: clientOf({ server: { ...SERVER, lastError: 'The telemetry service is not reachable right now.', errorSince: Date.now() - 60_000 } }), steps: 1 })
      const err = failing.w.get('[role=alert]').text()
      expect(err).toContain('The alert service could not check')
      expect(err).toContain('not reachable')
      expect(err).toContain('have not been cleared')
    })

    it('still builds the analysis report for an alert the service found', async () => {
      const { w } = await mountView({ client: clientOf({ server: SERVER }), steps: 1 })
      await w.get('[data-alert] button').trigger('click')
      await flushPromises()
      expect(w.get('textarea').element.value).toContain('# Why is shop/cartservice not serving?')
    })
  })

  describe('Analyze with Claude', () => {
    it('gathers the evidence and shows a report, with a warning to read it first, and sends nothing', async () => {
      const client = clientOf()
      const { w } = await mountView({ recent: BROKEN, client })
      await w.get('[data-alert] button').trigger('click')
      await flushPromises()
      const box = w.get('[data-analysis]')
      const report = box.get('textarea').element.value
      expect(report).toContain('# Why is shop/cartservice not serving?')
      expect(report).toContain('Calls failing')
      expect(report).toContain('connection refused')
      expect(report).toContain('upstream connect error')
      expect(box.text()).toContain('Nothing has been sent anywhere')
      expect(box.text()).toContain('Read it first')
      expect(box.text()).toMatch(/\d[\d,]* characters \(about \d[\d,]* tokens\)/)
      const reads = client.get.mock.calls.map(([p]) => p)
      expect(reads).toContain('traces')
      expect(reads.every((p) => ['alerts', 'metrics', 'metrics/query_range', 'traces', 'logs', 'profiles'].includes(p))).toBe(true) // reads only
    })

    it('masks a secret in the evidence before showing it', async () => {
      const { w } = await mountView({ recent: BROKEN })
      await w.get('[data-alert] button').trigger('click')
      await flushPromises()
      expect(w.get('textarea').element.value).not.toContain('hunter2')
    })

    it('copies to the clipboard', async () => {
      const writeText = vi.fn(async () => {})
      vi.stubGlobal('navigator', { clipboard: { writeText } })
      const { w } = await mountView({ recent: BROKEN })
      await w.get('[data-alert] button').trigger('click')
      await flushPromises()
      await w.findAll('[data-analysis] button').find((b) => b.text() === 'Copy').trigger('click')
      await flushPromises()
      expect(writeText).toHaveBeenCalledWith(expect.stringContaining('# Why is shop/cartservice not serving?'))
      expect(w.get('[data-analysis] [role=status]').text()).toContain('Copied')
      vi.unstubAllGlobals()
    })

    it('falls back to selecting the text when copying is refused', async () => {
      vi.stubGlobal('navigator', { clipboard: { writeText: async () => { throw new Error('denied') } } })
      const { w } = await mountView({ recent: BROKEN })
      await w.get('[data-alert] button').trigger('click')
      await flushPromises()
      await w.findAll('[data-analysis] button').find((b) => b.text() === 'Copy').trigger('click')
      await flushPromises()
      expect(w.get('[data-analysis] [role=status]').text()).toContain('press Ctrl+C')
      vi.unstubAllGlobals()
    })

    it('closes the report', async () => {
      const { w } = await mountView({ recent: BROKEN })
      await w.get('[data-alert] button').trigger('click')
      await flushPromises()
      await w.findAll('[data-analysis] button').find((b) => b.text() === 'Close').trigger('click')
      expect(w.find('[data-analysis]').exists()).toBe(false)
    })

    it('still produces a report, saying what is missing, when some reads fail', async () => {
      const { w } = await mountView({ recent: BROKEN, client: clientOf({ fail: { logs: 'logs down' } }) })
      // the checks themselves need only 'metrics'; the failing read is the evidence gathering
      await w.get('[data-alert] button').trigger('click')
      await flushPromises()
      const report = w.get('textarea').element.value
      expect(report).toContain('Not loaded: logs (logs down)')
      expect(report).toContain('upstream connect error')
    })
  })
})
