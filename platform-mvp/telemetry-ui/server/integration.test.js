// @vitest-environment node
import { createServer } from 'node:http'
import { afterAll, beforeAll, describe, expect, it } from 'vitest'
import { createEvaluator } from './evaluator.js'
import { createHandler } from './app.js'
import { loadConfig } from './config.js'
import { createStubReadApi } from './testing/stub-read-api.js'

// The real pieces end to end, over real sockets: a stub read API that serves counters the way telemetry-ingest does, the real
// evaluator and checks (the same rules the browser runs), the real HTTP handler, and a stub Alertmanager. A service whose
// calls start failing must fire, be served to a key that may read it, be refused to one that may not, and reach Alertmanager.

const READ_KEY = 'gke-read-key-0123456789abcdef'
let nowSec = Math.floor(Date.now() / 1000) // the clock of this test: one minute passes per check, for the evaluator and the stub alike
const stub = createStubReadApi({ key: READ_KEY, now: () => nowSec })
const state = stub.state

const servers = []
function listen(handler) {
  const s = createServer(handler)
  servers.push(s)
  return new Promise((r) => s.listen(0, '127.0.0.1', () => r(`http://127.0.0.1:${s.address().port}`)))
}

let evaluator
let alertsBase
beforeAll(async () => {
  const readUrl = await listen(stub.handler) // serves the read API, and takes the posts to /api/v2/alerts as Alertmanager
  const config = loadConfig({ READ_API: readUrl, ALERTMANAGER_URL: readUrl, INTERVAL_SECONDS: '60' })
  evaluator = createEvaluator({ config, loadProjects: () => [{ name: 'gke', readKeys: [READ_KEY] }], now: () => nowSec * 1000 })
  await evaluator.restore()
  alertsBase = await listen(createHandler({ evaluator }))
})
afterAll(() => Promise.all(servers.map((s) => new Promise((r) => s.close(r)))))

const getAlerts = (key) => fetch(`${alertsBase}/api/v1/projects/gke/alerts`, { headers: key ? { 'X-API-Key': key } : {} })

describe('alert service, end to end', () => {
  it('answers a valid key with "not checked yet" before its first check, not an error', async () => {
    const r = await getAlerts(READ_KEY)
    expect(r.status).toBe(200)
    expect((await r.json()).data).toMatchObject({ alerts: [], lastRun: 0, evaluatedBy: 'server' })
    expect((await fetch(`${alertsBase}/readyz`)).status).toBe(503)
  })

  it('finds a service whose calls are failing, first as pending and then firing', async () => {
    await evaluator.tick()
    expect((await fetch(`${alertsBase}/readyz`)).status).toBe(200)
    let alerts = (await (await getAlerts(READ_KEY)).json()).data.alerts
    expect(alerts.map((a) => [a.rule, a.service, a.status])).toEqual([['errors', '/k8s/shop/cartservice', 'pending']])
    nowSec += 60
    await evaluator.tick()
    const data = (await (await getAlerts(READ_KEY)).json()).data
    alerts = data.alerts
    expect(alerts).toHaveLength(1)
    expect(alerts[0]).toMatchObject({ rule: 'errors', status: 'firing', severity: 'critical', label: 'shop/cartservice' })
    expect(alerts[0].evidence.errorSharePct).toBeCloseTo(80, 1)
    expect(alerts[0].evidence.callers[0]).toMatchObject({ label: 'shop/frontend' })
    expect(data).toMatchObject({ lastError: '', checked: 2, evaluatedBy: 'server' })
    expect(data.config.errorShare).toBe(5)
  })

  it('reads with the project\'s read key and never with anything else', () => {
    expect(state.requests.length).toBeGreaterThan(0)
    expect(state.requests.every((r) => r.key === READ_KEY)).toBe(true)
    expect(state.requests.every((r) => r.path.startsWith('/api/v1/projects/gke/'))).toBe(true)
  })

  it('tells Alertmanager about the firing alert, and not about the pending one', () => {
    const posts = state.posts.filter((p) => p.url === '/api/v2/alerts')
    expect(posts).toHaveLength(1) // the first check had only a pending alert: nothing to send
    expect(posts[0].body[0].labels).toMatchObject({ alertname: 'ServiceCallsFailing', severity: 'critical', project: 'gke', service: 'shop/cartservice' })
  })

  it('serves the alert only to a key that may read the project', async () => {
    expect((await getAlerts()).status).toBe(401)
    expect((await getAlerts('some-other-key')).status).toBe(403)
  })

  it('exports what is firing as metrics', async () => {
    const m = await (await fetch(`${alertsBase}/metrics`)).text()
    expect(m).toContain('telemetry_alerts_firing{project="gke",rule="errors",severity="critical"} 1')
    expect(m).toContain('telemetry_alerts_last_check_ok{project="gke"} 1')
  })

  it('keeps the alert, and says it could not look, while the read API is down', async () => {
    state.down = true
    for (let i = 0; i < 3; i++) {
      nowSec += 60
      await evaluator.tick()
    }
    const data = (await (await getAlerts(READ_KEY)).json()).data
    expect(data.alerts[0].status).toBe('firing')
    expect(data.lastError).toContain('not reachable')
    const posts = state.posts.filter((p) => p.url === '/api/v2/alerts')
    expect(posts.length).toBeGreaterThan(1) // still being re-sent, so Alertmanager does not let go of it
    state.down = false
  })

  it('resolves after the problem is gone for two checks, and tells Alertmanager the end', async () => {
    state.broken = false
    nowSec += 60
    await evaluator.tick()
    expect((await (await getAlerts(READ_KEY)).json()).data.alerts[0].status).toBe('firing')
    nowSec += 60
    await evaluator.tick()
    const data = (await (await getAlerts(READ_KEY)).json()).data
    expect(data.alerts[0].status).toBe('resolved')
    const last = state.posts.filter((p) => p.url === '/api/v2/alerts').at(-1).body[0]
    expect(Date.parse(last.endsAt)).toBeLessThanOrEqual(nowSec * 1000)
  })
})
