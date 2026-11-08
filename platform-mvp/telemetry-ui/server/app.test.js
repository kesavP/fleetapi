// @vitest-environment node
import { createServer } from 'node:http'
import { afterAll, beforeAll, describe, expect, it } from 'vitest'
import { createHandler } from './app.js'

const KEY = 'gke-read-key-0123456789abcdef'
const SNAP = { alerts: [{ id: 'errors|x', status: 'firing' }], checked: 2, skipped: [], lastRun: 5, lastError: '', errorSince: 0, config: {}, intervalSeconds: 60, evaluatedBy: 'server' }

let server
let base
let ready = true
const logs = []
const evaluator = {
  ready: () => ready,
  projects: () => [{ name: 'gke', readKeys: [KEY] }, { name: 'dc1', readKeys: ['dc1-read-key-zzzzzzzzzzzzzzzz'] }],
  snapshot: (name) => (name === 'gke' ? SNAP : null),
  metrics: () => 'telemetry_alerts_last_check_ok{project="gke"} 1\n',
}
beforeAll(async () => {
  server = createServer(createHandler({ evaluator, log: (o) => logs.push(o) }))
  await new Promise((r) => server.listen(0, '127.0.0.1', r))
  base = `http://127.0.0.1:${server.address().port}`
})
afterAll(() => new Promise((r) => server.close(r)))

const get = (path, key, method = 'GET') => fetch(base + path, { method, headers: key ? { 'X-API-Key': key } : {} })

describe('alerts route', () => {
  it('serves a project\'s alerts to its read key, in the read API\'s envelope', async () => {
    const r = await get('/api/v1/projects/gke/alerts', KEY)
    expect(r.status).toBe(200)
    expect(r.headers.get('cache-control')).toBe('no-store')
    expect(await r.json()).toEqual({ data: SNAP })
  })

  it('asks for a key (401) and refuses a wrong one, another project\'s, or an unknown project (all the same 403)', async () => {
    expect((await get('/api/v1/projects/gke/alerts')).status).toBe(401)
    const bodies = []
    for (const [path, key] of [
      ['/api/v1/projects/gke/alerts', 'wrong-key'],
      ['/api/v1/projects/gke/alerts', 'dc1-read-key-zzzzzzzzzzzzzzzz'], // valid, for another project
      ['/api/v1/projects/nope/alerts', KEY], // no such project
      ['/api/v1/projects/%E0%A4%A/alerts', KEY], // not decodable
    ]) {
      const r = await get(path, key)
      expect(r.status).toBe(403)
      bodies.push(await r.text())
    }
    expect(new Set(bodies).size).toBe(1) // it does not say which of the cases it was
  })

  it('does not accept the key in the URL, and logs no key and no project name when it refuses', async () => {
    expect((await get(`/api/v1/projects/gke/alerts?key=${KEY}`)).status).toBe(401)
    await get('/api/v1/projects/secret-project/alerts', 'some-wrong-key-value')
    const text = JSON.stringify(logs)
    expect(text).not.toContain('some-wrong-key-value')
    expect(text).not.toContain('secret-project')
    expect(text).not.toContain(KEY)
  })

  it('answers only GET', async () => {
    expect((await get('/api/v1/projects/gke/alerts', KEY, 'POST')).status).toBe(405)
    expect((await get('/api/v1/projects/gke/alerts', KEY, 'DELETE')).status).toBe(405)
  })

  it('knows no other route under the API', async () => {
    expect((await get('/api/v1/projects/gke/metrics', KEY)).status).toBe(404)
    expect((await get('/api/v1/projects/gke/alerts/extra', KEY)).status).toBe(404)
    expect((await get('/', KEY)).status).toBe(404)
  })
})

describe('platform routes', () => {
  it('is alive, and ready once it has checked', async () => {
    expect((await get('/healthz')).status).toBe(200)
    ready = false
    expect((await get('/readyz')).status).toBe(503)
    ready = true
    expect((await get('/readyz')).status).toBe(200)
  })

  it('exposes metrics as Prometheus text, without a key', async () => {
    const r = await get('/metrics')
    expect(r.status).toBe(200)
    expect(r.headers.get('content-type')).toContain('text/plain')
    expect(await r.text()).toContain('telemetry_alerts_last_check_ok')
  })
})
