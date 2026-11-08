import { describe, expect, it, vi } from 'vitest'
import { ApiError, createClient, validProject } from './client'

const KEY = 'us-read-key-0123456789abcdef'

const sessionOf = (value) => ({ get: () => value })
const connected = sessionOf({ project: 'gke', key: KEY })

/** A fetch that answers with `status` and `body`, and remembers how it was called. */
function fakeFetch(status, body, { text } = {}) {
  const calls = []
  const fn = vi.fn(async (url, init) => {
    calls.push({ url, init })
    return {
      ok: status >= 200 && status < 300,
      status,
      json: async () => {
        if (text !== undefined) throw new SyntaxError('not json')
        return body
      },
    }
  })
  fn.calls = calls
  return fn
}

const clientWith = (fetchImpl, session = connected, extra = {}) => createClient({ session, fetchImpl, ...extra })

describe('a request', () => {
  it('carries the key in a header, never in the URL, and sends cookies only to its own origin', async () => {
    const f = fakeFetch(200, { data: ['a'] })
    await clientWith(f).get('metrics')
    const { url, init } = f.calls[0]
    expect(url).toBe('/api/v1/projects/gke/metrics')
    expect(url).not.toContain(KEY)
    expect(init.headers['X-API-Key']).toBe(KEY)
    expect(init.credentials).toBe('same-origin')
    expect(init.cache).toBe('no-store')
  })

  it('returns the data of the answer', async () => {
    expect(await clientWith(fakeFetch(200, { data: { series: [] } })).get('metrics/query_range')).toEqual({ series: [] })
  })

  it('builds the query string and leaves out empty values', async () => {
    const f = fakeFetch(200, { data: [] })
    await clientWith(f).get('metrics/query_range', { name: 'up', step: 15, rate: true, from: '', to: undefined, x: null })
    expect(f.calls[0].url).toBe('/api/v1/projects/gke/metrics/query_range?name=up&step=15&rate=true')
  })

  it('encodes what goes in the URL', async () => {
    const f = fakeFetch(200, { data: [] })
    await clientWith(f).get('logs', { q: 'a b&c=d' })
    expect(f.calls[0].url).toBe('/api/v1/projects/gke/logs?q=a+b%26c%3Dd')
  })

  it('goes under a base path when the UI is mounted under one', async () => {
    const f = fakeFetch(200, { data: [] })
    await clientWith(f, connected, { base: '/telemetry' }).get('metrics')
    expect(f.calls[0].url).toBe('/telemetry/api/v1/projects/gke/metrics')
  })

  it('tolerates an empty answer', async () => {
    expect(await clientWith(fakeFetch(200, null, { text: 'html' })).get('metrics')).toBeNull()
  })
})

describe('before any request', () => {
  it('refuses when there is no session, without calling the server', async () => {
    const f = fakeFetch(200, { data: [] })
    for (const s of [sessionOf(null), sessionOf({ project: 'gke', key: '' }), sessionOf({ project: '', key: KEY })]) {
      await expect(clientWith(f, s).get('metrics')).rejects.toMatchObject({ kind: 'auth' })
    }
    expect(f).not.toHaveBeenCalled()
  })

  it('refuses a project name that could change the path', async () => {
    const f = fakeFetch(200, { data: [] })
    for (const project of ['../other', 'a/b', 'UPPER', 'a b', '-lead', 'x?y=1']) {
      await expect(clientWith(f, sessionOf({ project, key: KEY })).get('metrics')).rejects.toMatchObject({ kind: 'request' })
    }
    expect(f).not.toHaveBeenCalled()
  })

  it('knows what a valid project name is', () => {
    for (const ok of ['gke', 'us-east-1', 'a', 'dc1']) expect(validProject(ok)).toBe(true)
    for (const bad of ['', 'A', '-a', 'a-', 'a_b', 'a.b', 'x'.repeat(64)]) expect(validProject(bad)).toBe(false)
  })
})

describe('a failure', () => {
  const failures = [
    [401, {}, 'auth', /read key is required/i],
    [403, { error: 'this key cannot read that project' }, 'auth', /cannot read project "gke"/],
    [400, { error: 'rate=true needs a step (seconds)' }, 'request', /rate=true needs a step/],
    [400, {}, 'request', /not accepted/],
    [404, {}, 'request', /not found/i],
    [502, {}, 'unavailable', /not reachable/],
    [503, {}, 'unavailable', /not reachable/],
    [504, {}, 'unavailable', /not reachable/],
    [500, {}, 'error', /answered 500/],
  ]
  for (const [status, body, kind, message] of failures) {
    it(`${status} becomes a readable ${kind} error`, async () => {
      const e = await clientWith(fakeFetch(status, body)).get('metrics').catch((x) => x)
      expect(e).toBeInstanceOf(ApiError)
      expect(e.status).toBe(status)
      expect(e.kind).toBe(kind)
      expect(e.message).toMatch(message)
    })
  }

  it('never puts the key in a message, whatever the server says', async () => {
    for (const status of [400, 401, 403, 404, 500, 502]) {
      const e = await clientWith(fakeFetch(status, { error: 'nothing secret here' })).get('metrics').catch((x) => x)
      expect(e.message).not.toContain(KEY)
      expect(String(e.stack)).not.toContain(KEY)
    }
  })

  it('copes with a proxy error page instead of JSON', async () => {
    const e = await clientWith(fakeFetch(502, null, { text: '<html>Bad gateway</html>' })).get('metrics').catch((x) => x)
    expect(e.kind).toBe('unavailable')
  })

  it('reports an unreachable server', async () => {
    const f = vi.fn(async () => {
      throw new TypeError('Failed to fetch')
    })
    const e = await clientWith(f).get('metrics').catch((x) => x)
    expect(e.kind).toBe('network')
    expect(e.message).toMatch(/cannot reach the server/i)
  })

  it('gives up on a request that takes too long', async () => {
    vi.useFakeTimers()
    try {
      const hangs = vi.fn((url, { signal }) => new Promise((_, reject) => signal.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')))))
      const pending = clientWith(hangs, connected, { timeoutMs: 5000 }).get('metrics').catch((x) => x)
      await vi.advanceTimersByTimeAsync(5001)
      const e = await pending
      expect(e.kind).toBe('timeout')
    } finally {
      vi.useRealTimers()
    }
  })
})

describe('metrics()', () => {
  it('asks for the metric names', async () => {
    const f = fakeFetch(200, { data: ['up', 'down'] })
    expect(await clientWith(f).metrics()).toEqual(['up', 'down'])
    expect(f.calls[0].url).toBe('/api/v1/projects/gke/metrics')
  })
})
