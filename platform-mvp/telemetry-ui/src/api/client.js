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

  return {
    get,
    /** The names of the metrics the project has. */
    metrics: () => get('metrics'),
  }
}
