// @vitest-environment node
import { describe, expect, it, vi } from 'vitest'
import { alertmanagerPayload, postToAlertmanager } from './notify.js'

const T = Date.parse('2026-01-01T12:00:00Z')
const MIN = 60_000
const alert = (o) => ({ rule: 'errors', service: '/k8s/shop/cart', label: 'shop/cart', severity: 'critical', summary: '80% failing', status: 'firing', firstSeen: T - 10 * MIN, ...o })

describe('alertmanagerPayload', () => {
  it('sends a firing alert with its start, labels, and an end time a few intervals ahead', () => {
    const [a] = alertmanagerPayload('gke', [alert()], T, MIN)
    expect(a.labels).toEqual({ alertname: 'ServiceCallsFailing', severity: 'critical', project: 'gke', workload: '/k8s/shop/cart', service: 'shop/cart', rule: 'errors' })
    expect(a.annotations.summary).toBe('80% failing')
    expect(a.startsAt).toBe('2025-01-01T12:00:00.000Z'.replace('2025', '2026').replace('12:00', '11:50'))
    expect(Date.parse(a.endsAt)).toBe(T + 3 * MIN)
  })

  it('does not send pending alerts, which have not fired', () => {
    expect(alertmanagerPayload('gke', [alert({ status: 'pending' })], T, MIN)).toEqual([])
  })

  it('sends a just-resolved alert once, with its real end, and not an old resolution', () => {
    const fresh = alert({ status: 'resolved', resolvedAt: T - 20_000 })
    const old = alert({ status: 'resolved', resolvedAt: T - 5 * MIN, rule: 'refused' })
    const out = alertmanagerPayload('gke', [fresh, old], T, MIN)
    expect(out).toHaveLength(1)
    expect(Date.parse(out[0].endsAt)).toBe(T - 20_000)
  })

  it('names each rule', () => {
    const names = ['no-traffic', 'refused', 'errors', 'no-instances', 'telemetry-gone'].map((rule) => alertmanagerPayload('p', [alert({ rule })], T, MIN)[0].labels.alertname)
    expect(names).toEqual(['ServiceStoppedReceivingTraffic', 'ServiceConnectionsFailing', 'ServiceCallsFailing', 'ServiceNoInstances', 'TelemetryMissing'])
  })

  it('is empty for nothing', () => {
    expect(alertmanagerPayload('p', [], T, MIN)).toEqual([])
  })
})

describe('postToAlertmanager', () => {
  const payload = [{ labels: { alertname: 'X' } }]
  it('posts JSON to the v2 API', async () => {
    const f = vi.fn(async () => ({ ok: true, status: 200 }))
    expect(await postToAlertmanager('http://am:9093', payload, f)).toEqual({ sent: 1, error: '' })
    expect(f.mock.calls[0][0]).toBe('http://am:9093/api/v2/alerts')
    expect(f.mock.calls[0][1]).toMatchObject({ method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload) })
  })
  it('does nothing without a URL or without alerts', async () => {
    const f = vi.fn()
    expect(await postToAlertmanager('', payload, f)).toEqual({ sent: 0, error: '' })
    expect(await postToAlertmanager('http://am', [], f)).toEqual({ sent: 0, error: '' })
    expect(f).not.toHaveBeenCalled()
  })
  it('reports a failure instead of throwing', async () => {
    expect((await postToAlertmanager('http://am', payload, async () => ({ ok: false, status: 503 }))).error).toContain('HTTP 503')
    const refused = await postToAlertmanager('http://am', payload, async () => { throw Object.assign(new Error('x'), { cause: { code: 'ECONNREFUSED' } }) })
    expect(refused).toEqual({ sent: 0, error: 'cannot reach Alertmanager (ECONNREFUSED)' })
  })
  it('gives up after the timeout', async () => {
    const hang = (url, { signal }) => new Promise((_, reject) => signal.addEventListener('abort', () => reject(new Error('aborted'))))
    expect((await postToAlertmanager('http://am', payload, hang, 20)).error).toContain('did not answer in time')
  })
})
