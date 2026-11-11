// The read API of telemetry-ingest, as the browser uses it:
//   GET /api/v1/projects/{project}/...   with the project's read key in the X-API-Key header.
//
// Three rules this file exists to keep:
//   - the key travels in a header, never in a URL, so it cannot end up in a proxy's access log, the browser
//     history or a Referer;
//   - an error message never contains the key;
//   - every failure becomes an ApiError whose message can be shown to a person as it is.

export class ApiError extends Error {
  constructor(message, { status = 0, kind = 'error' } = {}) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.kind = kind // 'auth' | 'request' | 'unavailable' | 'timeout' | 'network' | 'error'
  }
}

// The same rule the service applies to project names: a DNS label.
const PROJECT = /^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$/
export const validProject = (name) => PROJECT.test(name)

const DEFAULT_TIMEOUT_MS = 60000 // the service itself gives a query 30 s

export function createClient({ session, fetchImpl = (...a) => fetch(...a), base = '', timeoutMs = DEFAULT_TIMEOUT_MS }) {
  async function get(path, params = {}) {
    const s = session.get()
    if (!s || !s.project || !s.key) {
      throw new ApiError('Not connected: enter a project and a read key.', { kind: 'auth' })
    }
    if (!validProject(s.project)) {
      throw new ApiError('The project name must be lowercase letters, digits and dashes.', { kind: 'request' })
    }
    const query = new URLSearchParams(Object.entries(params).filter(([, v]) => v !== undefined && v !== null && v !== ''))
    const url = `${base}/api/v1/projects/${encodeURIComponent(s.project)}/${path}${query.size ? '?' + query : ''}`

    const controller = new AbortController()
    const timer = setTimeout(() => controller.abort(), timeoutMs)
    let res
    try {
      res = await fetchImpl(url, {
        headers: { 'X-API-Key': s.key, Accept: 'application/json' },
        signal: controller.signal,
        // Same-origin cookies only: the app sets none and the key is never a cookie, but a proxy in front of it
        // (Cloud Shell's web preview, IAP, oauth2-proxy) authenticates with one, and a request without it gets
        // redirected to a login page that the CSP then blocks.
        credentials: 'same-origin',
        cache: 'no-store',
      })
    } catch {
      if (controller.signal.aborted) throw new ApiError('The request timed out.', { kind: 'timeout' })
      throw new ApiError('Cannot reach the server. Is it running, and are you online?', { kind: 'network' })
    } finally {
      clearTimeout(timer)
    }

    let body = null
    try {
      body = await res.json()
    } catch {
      // a non-JSON body (a proxy's error page, say): the status alone has to do
    }
    if (res.ok) return body ? body.data : null

    const detail = body && typeof body.error === 'string' ? body.error : ''
    switch (res.status) {
      case 401:
        throw new ApiError('A read key is required.', { status: 401, kind: 'auth' })
      case 403:
        throw new ApiError(`This key cannot read project "${s.project}". Check the key and the project name.`, { status: 403, kind: 'auth' })
      case 400:
        throw new ApiError(detail || 'The request was not accepted.', { status: 400, kind: 'request' })
      case 404:
        throw new ApiError('Not found.', { status: 404, kind: 'request' })
      case 502:
      case 503:
      case 504:
        throw new ApiError('The telemetry service is not reachable right now.', { status: res.status, kind: 'unavailable' })
      default:
        throw new ApiError(`The server answered ${res.status}.`, { status: res.status })
    }
  }

  const ACTOR = /^[A-Za-z0-9][A-Za-z0-9 ._@+-]{0,63}$/

  /**
   * A call that changes something (POST, DELETE), which needs a MANAGE key: a different key from the one the session holds, given for this one call
   * and kept nowhere. `actor`, when it is a valid name, is recorded as who did it. The key goes in a header and in no message, like the read key.
   */
  async function send(method, path, { key, body, actor } = {}) {
    const s = session.get()
    if (!s || !s.project) throw new ApiError('Not connected: enter a project and a read key.', { kind: 'auth' })
    if (!validProject(s.project)) throw new ApiError('The project name must be lowercase letters, digits and dashes.', { kind: 'request' })
    if (typeof key !== 'string' || !key.trim()) throw new ApiError('A manage key is required for this.', { kind: 'auth' })
    const headers = { 'X-API-Key': key.trim(), Accept: 'application/json' }
    if (body !== undefined) headers['Content-Type'] = 'application/json'
    if (actor && ACTOR.test(actor)) headers['X-Actor'] = actor
    const controller = new AbortController()
    const timer = setTimeout(() => controller.abort(), timeoutMs)
    let res
    try {
      res = await fetchImpl(`${base}/api/v1/projects/${encodeURIComponent(s.project)}/${path}`, {
        method,
        headers,
        body: body === undefined ? undefined : JSON.stringify(body),
        signal: controller.signal,
        credentials: 'same-origin',
        cache: 'no-store',
      })
    } catch {
      if (controller.signal.aborted) throw new ApiError('The request timed out.', { kind: 'timeout' })
      throw new ApiError('Cannot reach the server. Is it running, and are you online?', { kind: 'network' })
    } finally {
      clearTimeout(timer)
    }
    let out = null
    try {
      out = await res.json()
    } catch {
      // a non-JSON body (a proxy's error page): the status alone has to do
    }
    if (res.ok) return out ? out.data : null
    const detail = out && typeof out.error === 'string' ? out.error : ''
    switch (res.status) {
      case 400:
        throw new ApiError(detail || 'The request was not accepted.', { status: 400, kind: 'request' })
      case 401:
        throw new ApiError('A manage key is required for this.', { status: 401, kind: 'auth' })
      case 403:
        throw new ApiError(`This key cannot change project "${s.project}". It must be a manage key of that project, not the read key.`, { status: 403, kind: 'auth' })
      case 404:
        throw new ApiError(detail || 'Not found.', { status: 404, kind: 'request' })
      case 405:
        throw new ApiError('The UI\'s proxy does not let this through. Is the UI image older than the service?', { status: 405, kind: 'request' })
      case 413:
        throw new ApiError('The request was too large.', { status: 413, kind: 'request' })
      case 429:
        throw new ApiError(detail || 'Too many requests; try again in a moment.', { status: 429, kind: 'request' })
      case 502:
      case 503:
      case 504:
        throw new ApiError('The telemetry service is not reachable right now.', { status: res.status, kind: 'unavailable' })
      default:
        throw new ApiError(`The server answered ${res.status}.`, { status: res.status })
    }
  }

  return {
    get,
    send,
    /** The names of the metrics the project has. */
    metrics: () => get('metrics'),
  }
}
