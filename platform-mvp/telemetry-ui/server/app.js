import { keyMatches } from './projects.js'

// The HTTP side. One authenticated route, shaped like the read API's so the UI's own client (and its proxy) can use it:
//   GET /api/v1/projects/{project}/alerts      X-API-Key: that project's READ key
// A key that is valid for another project, or for none, gets the same 403 for any project path, so the answer does not reveal
// which projects exist. The rest is for the platform: /healthz, /readyz and Prometheus /metrics (no project data in them
// beyond names and counts).

const ROUTE = /^\/api\/v1\/projects\/([^/]+)\/alerts\/?$/

function send(res, status, body, type = 'application/json') {
  const text = typeof body === 'string' ? body : JSON.stringify(body)
  res.writeHead(status, { 'Content-Type': type, 'Cache-Control': 'no-store', 'X-Content-Type-Options': 'nosniff' })
  res.end(text)
}

export function createHandler({ evaluator, log = () => {} }) {
  return (req, res) => {
    const url = new URL(req.url, 'http://x')
    if (req.method !== 'GET' && req.method !== 'HEAD') return send(res, 405, { error: 'method not allowed' })

    if (url.pathname === '/healthz') return send(res, 200, 'ok\n', 'text/plain')
    if (url.pathname === '/readyz') return evaluator.ready() ? send(res, 200, 'ok\n', 'text/plain') : send(res, 503, 'not ready\n', 'text/plain')
    if (url.pathname === '/metrics') return send(res, 200, evaluator.metrics(), 'text/plain; version=0.0.4')

    const m = ROUTE.exec(url.pathname)
    if (!m) return send(res, 404, { error: 'not found' })
    const key = req.headers['x-api-key']
    if (!key || Array.isArray(key)) return send(res, 401, { error: 'missing API key' })
    let name
    try {
      name = decodeURIComponent(m[1])
    } catch {
      return send(res, 403, { error: 'this key cannot read that project' })
    }
    const project = evaluator.projects().find((p) => p.name === name)
    if (!project || !keyMatches(project, key)) {
      log({ level: 'info', msg: 'rejected', route: 'alerts', status: 403 }) // never the key, and not the project asked for
      return send(res, 403, { error: 'this key cannot read that project' })
    }
    return send(res, 200, { data: evaluator.snapshot(name) })
  }
}
