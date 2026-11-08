import { describe, expect, it } from 'vitest'
import { MAX_GROUPS, MAX_TEXT, buildReport, groupFailingSpans, groupLogs, redact, shape } from './report'

describe('redact', () => {
  it.each([
    ['Authorization: Bearer abcdef1234567890abcdef', 'Authorization: [redacted]'],
    ['curl -H "Authorization: Basic dXNlcjpwYXNzd29yZA=="', '[redacted]'],
    ['connect postgres://admin:s3cr3t@db:5432/app failed', 'postgres://[redacted]@db:5432/app'],
    ['password=hunter2&user=bob', 'password=[redacted]&user=bob'],
    ['{"api_key": "sk-live-123456", "ok": true}', '"api_key": [redacted]'],
    ['token: abc123', 'token: [redacted]'],
    ['jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.c2lnbmF0dXJl end', 'jwt [redacted jwt] end'],
    ['key AKIAABCDEFGHIJKLMNOP here', 'key [redacted aws key] here'],
  ])('%s', (input, expected) => {
    const out = redact(input)
    expect(out).toContain(expected)
    for (const secret of ['abcdef1234567890abcdef', 'dXNlcjpwYXNzd29yZA', 's3cr3t', 'hunter2', 'sk-live-123456', 'abc123', 'c2lnbmF0dXJl', 'AKIAABCDEFGHIJKLMNOP']) expect(out).not.toContain(secret)
  })
  it('leaves ordinary text, trace ids and numbers alone', () => {
    const t = 'GET /cart took 1234 ms for trace 4bf92f3577b34da6a3ce929d0e0e4736 (status 503)'
    expect(redact(t)).toBe(t)
    expect(redact(undefined)).toBe('')
  })
})

describe('shape', () => {
  it('makes lines that differ only in numbers and ids the same', () => {
    expect(shape('took 1234 ms for user 8841')).toBe(shape('took 17 ms for user 5'))
    expect(shape('order 3f2504e0-4f89-11d3-9a0c-0305e82c3301 failed')).toBe('order <uuid> failed')
    expect(shape('conn ab12cd34ef56ab12cd34 closed')).toBe('conn <hex> closed')
    expect(shape('timeout')).not.toBe(shape('refused'))
  })
})

const T = Date.parse('2026-01-01T12:00:00Z')
const span = (o) => ({ service: 's', name: 'GET /x', status_code: 'ERROR', status_message: 'boom 1', duration_ns: 2e6, time: new Date(T).toISOString(), trace_id: 'a'.repeat(32), ...o })

describe('groupFailingSpans', () => {
  it('counts failing spans that are the same kind of failure together, with the latest trace and a mean', () => {
    const g = groupFailingSpans([
      span({ status_message: 'boom 1', duration_ns: 2e6, time: new Date(T).toISOString(), trace_id: '1'.repeat(32) }),
      span({ status_message: 'boom 22', duration_ns: 4e6, time: new Date(T + 5000).toISOString(), trace_id: '2'.repeat(32) }),
      span({ name: 'GET /y', status_message: 'other' }),
      span({ status_code: 'OK' }),
    ])
    expect(g).toHaveLength(2)
    expect(g[0]).toMatchObject({ name: 'GET /x', count: 2, meanNs: 3e6, traceId: '2'.repeat(32) })
    expect(g[1]).toMatchObject({ name: 'GET /y', count: 1 })
  })
  it('keeps the report small', () => {
    const many = Array.from({ length: 100 }, (_, i) => span({ name: `op${i}` }))
    expect(groupFailingSpans(many)).toHaveLength(MAX_GROUPS)
  })
  it('redacts and cuts a message', () => {
    const g = groupFailingSpans([span({ status_message: 'password=hunter2 ' + 'x'.repeat(1000) })])
    expect(g[0].message).not.toContain('hunter2')
    expect(g[0].message.length).toBeLessThanOrEqual(MAX_TEXT)
  })
})

const log = (o) => ({ severity: 'ERROR', severity_number: 17, body: 'failed 1', time: new Date(T).toISOString(), ...o })
describe('groupLogs', () => {
  it('keeps only errors and warnings, groups by shape, most frequent first, with the span of time', () => {
    const g = groupLogs([
      log({ body: 'failed to reach 10.0.0.1 after 3 tries', time: new Date(T).toISOString() }),
      log({ body: 'failed to reach 10.0.0.2 after 5 tries', time: new Date(T + 60000).toISOString() }),
      log({ severity: 'WARN', severity_number: 13, body: 'slow' }),
      log({ severity: 'INFO', severity_number: 9, body: 'fine' }),
    ])
    expect(g.map((x) => [x.severity, x.count])).toEqual([['error', 2], ['warn', 1]])
    expect(g[0].last - g[0].first).toBe(60000)
  })
  it('redacts a secret in a line', () => {
    expect(groupLogs([log({ body: 'login failed token=abc123' })])[0].text).not.toContain('abc123')
  })
})

describe('buildReport', () => {
  const alert = { id: 'errors|/k8s/shop/cart', rule: 'errors', service: '/k8s/shop/cart', label: 'shop/cart', severity: 'critical', status: 'firing', firstSeen: T, summary: '60% of calls failing.', evidence: { errorSharePct: 60, callers: [{ id: 'x', label: 'shop/frontend', rate: 9 }] } }
  const other = { ...alert, id: 'refused|/k8s/shop/db', rule: 'refused', label: 'data/db', service: '/k8s/shop/db', severity: 'warning', firstSeen: T - 60000, summary: 'db refusing' }
  const context = {
    project: 'gke', now: T + 120000, limit: 1000, errors: {},
    spans: [span({ status_message: 'upstream connect error' })],
    logs: [log({ body: 'cart failed: dial tcp 10.1.1.1:5432: connection refused' })],
    traffic: { inbound: [{ peerLabel: 'shop/frontend', protocols: ['http'], rps: 10, unit: 'req/s', connPerSec: 1, errPct: 60, errBasis: 'http', p95ms: null }], outbound: [] },
    resources: { instances: 2, cpuCores: 0.25, cpuLimitCores: 1, memoryBytes: 64 * 1024 * 1024, memoryLimitBytes: null, restarts: 3, oomKills: 0 },
  }

  it('states the alert, the evidence and the other open alerts earliest first', () => {
    const t = buildReport(alert, context, [alert, other])
    expect(t).toContain('# Why is shop/cart not serving?')
    expect(t).toContain('Project: gke')
    expect(t).toContain('**Calls failing** (critical) on `/k8s/shop/cart`')
    expect(t).toContain('errorSharePct: 60')
    expect(t).toContain('shop/frontend (9/s)')
    expect(t).toContain('Other alerts open at the same time')
    expect(t).toContain('data/db: db refusing')
    expect(t).not.toMatch(/shop\/cart: 60% of calls failing\.\n- /) // the alert itself is not listed among the others
  })

  it('includes calls, resources, failing spans and log groups', () => {
    const t = buildReport(alert, context, [alert])
    expect(t).toContain('| shop/frontend | http | 10 req/s | 60% (http) | – |')
    expect(t).toContain('restarts in window: 3')
    expect(t).toContain('64 MiB')
    expect(t).toContain('upstream connect error')
    expect(t).toContain('connection refused')
    expect(t).toContain('What I would like from you')
  })

  it('says what is missing instead of leaving it out', () => {
    const t = buildReport(alert, { ...context, traffic: null, resources: null, spans: [], logs: [], errors: { metrics: 'down' } }, [])
    expect(t).toContain('_not available_: down')
    expect(t).toContain('_none reported for this service in the window_')
    expect(t).toContain('_no failing spans recorded')
    expect(t).toContain('_no error or warning lines')
    expect(t).toContain('Not loaded: metrics (down)')
  })

  it('never carries a secret out of a log, a span message or a status', () => {
    const t = buildReport(alert, { ...context, logs: [log({ body: 'call failed Authorization: Bearer abcdef1234567890abcdef' })], spans: [span({ status_message: 'dial postgres://u:hunter2@db/x' })] }, [alert])
    expect(t).not.toContain('abcdef1234567890abcdef')
    expect(t).not.toContain('hunter2')
  })

  it('does not break a table with a pipe in a value', () => {
    const t = buildReport(alert, { ...context, logs: [log({ body: 'a | b' })] }, [alert])
    expect(t).toContain('a \\| b')
  })

  it('describes a project-wide alert without a service', () => {
    const t = buildReport({ ...alert, id: 'telemetry-gone|*', rule: 'telemetry-gone', service: '*', label: 'project' }, context, [])
    expect(t).toContain('on `the whole project`')
  })
})
