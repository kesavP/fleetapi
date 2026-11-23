import { describe, expect, it, vi } from 'vitest'
import { loadService } from './services'

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
