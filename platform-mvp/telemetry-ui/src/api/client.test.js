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

describe('send (a call that changes something)', () => {
  const MANAGE = 'us-manage-key-0123456789abc'

  it('uses the manage key it is given, never the session\'s, in a header and not in the URL', async () => {
    const f = fakeFetch(201, { data: { id: 'a1' } })
    const out = await clientWith(f).send('POST', 'enrollments', { key: MANAGE, body: { cluster: 'gke' }, actor: 'ana@example.com' })
    expect(out).toEqual({ id: 'a1' })
    const { url, init } = f.calls[0]
    expect(url).toBe('/api/v1/projects/gke/enrollments')
    expect(url).not.toContain(MANAGE)
    expect(init.method).toBe('POST')
    expect(init.headers['X-API-Key']).toBe(MANAGE)
    expect(init.headers['X-API-Key']).not.toBe(KEY)
    expect(init.headers['Content-Type']).toBe('application/json')
    expect(init.headers['X-Actor']).toBe('ana@example.com')
    expect(init.body).toBe('{"cluster":"gke"}')
    expect(init.credentials).toBe('same-origin')
    expect(init.cache).toBe('no-store')
  })

  it('sends no body and no content type for a DELETE, and leaves out an actor that is not valid', async () => {
    const f = fakeFetch(200, { data: {} })
    await clientWith(f).send('DELETE', 'enrollments/0123456789ab', { key: MANAGE, actor: 'a;b' })
    const { init } = f.calls[0]
    expect(init.method).toBe('DELETE')
    expect(init.body).toBeUndefined()
    expect(init.headers['Content-Type']).toBeUndefined()
    expect(init.headers['X-Actor']).toBeUndefined()
  })

  it('refuses without asking when there is no manage key, or no session', async () => {
    const f = fakeFetch(200, { data: {} })
    for (const key of [undefined, '', '   ', 42]) {
      await expect(clientWith(f).send('POST', 'enrollments', { key, body: {} })).rejects.toMatchObject({ kind: 'auth', message: expect.stringContaining('manage key') })
    }
    await expect(clientWith(f, sessionOf(null)).send('POST', 'enrollments', { key: MANAGE, body: {} })).rejects.toMatchObject({ kind: 'auth' })
    await expect(clientWith(f, sessionOf({ project: 'Bad Name', key: KEY })).send('POST', 'x', { key: MANAGE })).rejects.toMatchObject({ kind: 'request' })
    expect(f).not.toHaveBeenCalled()
  })

  it('says what went wrong in words a person can use, and never repeats the key', async () => {
    const cases = [
      [400, { error: 'cluster must be lower-case' }, 'cluster must be lower-case'],
      [401, null, 'manage key is required'],
      [403, null, 'must be a manage key'],
      [404, { error: 'no such enrollment' }, 'no such enrollment'],
      [405, null, 'proxy does not let this through'],
      [413, null, 'too large'],
      [429, { error: 'a project may have 20 unredeemed tokens at once' }, '20 unredeemed'],
      [502, null, 'not reachable'],
      [500, null, 'answered 500'],
    ]
    for (const [status, body, want] of cases) {
      const err = await clientWith(fakeFetch(status, body)).send('POST', 'enrollments', { key: MANAGE, body: {} }).catch((e) => e)
      expect(err).toBeInstanceOf(ApiError)
      expect(err.status).toBe(status)
      expect(err.message).toContain(want)
      expect(err.message).not.toContain(MANAGE)
    }
  })

  it('tells a timeout and an unreachable server apart, and survives a body that is not JSON', async () => {
    const down = vi.fn(async () => { throw new TypeError('failed to fetch ' + MANAGE) })
    const e1 = await clientWith(down).send('POST', 'enrollments', { key: MANAGE, body: {} }).catch((e) => e)
    expect(e1.kind).toBe('network')
    expect(e1.message).not.toContain(MANAGE)
    const slow = vi.fn((url, init) => new Promise((_, reject) => init.signal.addEventListener('abort', () => reject(new Error('aborted')))))
    const e2 = await clientWith(slow, connected, { timeoutMs: 5 }).send('POST', 'enrollments', { key: MANAGE, body: {} }).catch((e) => e)
    expect(e2.kind).toBe('timeout')
    expect(await clientWith(fakeFetch(200, null, { text: 'html' })).send('DELETE', 'enrollments/0123456789ab', { key: MANAGE })).toBeNull()
  })
})
