import { describe, expect, it, vi } from 'vitest'
import { loadService, loadServices, mergeServices } from './services'

describe('mergeServices', () => {
  it('lists a service seen in any signal, flags each, and sorts by name', () => {
    const out = mergeServices({
      traces: ['frontend', 'cart'],
      logs: ['frontend', 'redis'],
      profiles: [
        { service: 'cart', type: 'cpu' },
        { service: 'cart', type: 'alloc' },
        { service: 'cart', type: 'cpu' },
      ],
    })
    expect(out).toEqual([
      { name: 'cart', traces: true, logs: false, profiles: ['alloc', 'cpu'] },
      { name: 'frontend', traces: true, logs: true, profiles: [] },
      { name: 'redis', traces: false, logs: true, profiles: [] },
    ])
  })

  it('treats null, missing and empty names as nothing', () => {
    expect(mergeServices({ traces: null, logs: undefined, profiles: [null, { service: '' }] })).toEqual([])
    expect(mergeServices({ traces: ['', 'a'] }).map((s) => s.name)).toEqual(['a'])
  })
})

describe('loadServices', () => {
  const NOW = 1_700_000_000_000
  const answers = { 'traces/services': ['a'], 'logs/services': ['b'], profiles: [{ service: 'c', type: 'cpu' }] }
  const clientOf = (fail = {}) => ({
    get: vi.fn(async (path) => {
      if (fail[path]) throw new Error(fail[path])
      return answers[path]
    }),
  })

  it('asks traces and logs for the window ending now, and profiles for the project', async () => {
    const client = clientOf()
    const r = await loadServices(client, { seconds: 3600, now: () => NOW })
    expect(client.get).toHaveBeenCalledWith('traces/services', { from: 1_700_000_000 - 3600, to: 1_700_000_000 })
    expect(client.get).toHaveBeenCalledWith('logs/services', { from: 1_700_000_000 - 3600, to: 1_700_000_000 })
    expect(client.get).toHaveBeenCalledWith('profiles')
    expect(r.errors).toEqual({})
    expect(r.services.map((s) => s.name)).toEqual(['a', 'b', 'c'])
  })

  it('keeps what loaded when one source fails, and reports the failure', async () => {
    const r = await loadServices(clientOf({ 'logs/services': 'The telemetry service is not reachable right now.' }), { seconds: 60, now: () => NOW })
    expect(r.services.map((s) => s.name)).toEqual(['a', 'c'])
    expect(r.errors).toEqual({ logs: 'The telemetry service is not reachable right now.' })
  })

  it('reports every failure when all fail', async () => {
    const f = 'This key cannot read project "x".'
    const r = await loadServices(clientOf({ 'traces/services': f, 'logs/services': f, profiles: f }), { seconds: 60, now: () => NOW })
    expect(r.services).toEqual([])
    expect(Object.keys(r.errors).sort()).toEqual(['logs', 'profiles', 'traces'])
  })
})

describe('loadService', () => {
  const NOW = 1_700_000_000_000
  const spans = [{ span_id: 's1' }]
  const logs = [{ body: 'x' }]
  const profiles = [{ service: 'cart', type: 'cpu' }, { service: 'other', type: 'alloc' }]
  const answer = { traces: spans, logs, profiles }
  const clientOf = (fail = {}) => ({
    get: vi.fn(async (path) => {
      if (fail[path]) throw new Error(fail[path])
      return answer[path]
    }),
  })

  it('asks for this service over the window, with the largest page', async () => {
    const client = clientOf()
    const r = await loadService(client, 'cart', { seconds: 600, now: () => NOW })
    const range = { from: 1_700_000_000 - 600, to: 1_700_000_000 }
    expect(client.get).toHaveBeenCalledWith('traces', { service: 'cart', ...range, limit: 1000 })
    expect(client.get).toHaveBeenCalledWith('logs', { service: 'cart', ...range, limit: 1000 })
    expect(r).toMatchObject({ spans, logs, errors: {}, from: range.from * 1000, to: range.to * 1000 })
  })

  it('keeps only this service\'s profiles', async () => {
    const r = await loadService(clientOf(), 'cart', { seconds: 60, now: () => NOW })
    expect(r.profiles).toEqual([{ service: 'cart', type: 'cpu' }])
  })

  it('keeps the rest when one call fails', async () => {
    const r = await loadService(clientOf({ logs: 'down' }), 'cart', { seconds: 60, now: () => NOW })
    expect(r.errors).toEqual({ logs: 'down' })
    expect(r.logs).toEqual([])
    expect(r.spans).toEqual(spans)
  })

  it('treats null answers as empty', async () => {
    const client = { get: vi.fn(async () => null) }
    const r = await loadService(client, 'cart', { seconds: 60, now: () => NOW })
    expect(r).toMatchObject({ spans: [], logs: [], profiles: [], errors: {} })
  })
})
