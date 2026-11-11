// Enrolling a cluster from the Configuration page.
//
// A person with a MANAGE key makes an enrollment: a one-time token that lasts minutes. The page shows ONE command to run on the cluster, which
// trades the token for an ingest key of the cluster's own and applies it as the Secret the node-agent reads. The key never reaches the browser:
// the cluster comes to the hub with the token, and the hub never reaches into the cluster.
//
// Pure functions, and the three calls.

export const DEFAULT_NAMESPACE = 'telemetry-agent'
export const TTL_CHOICES = [
  { minutes: 15, label: '15 minutes' },
  { minutes: 60, label: '1 hour' },
  { minutes: 24 * 60, label: '24 hours' },
]

// the same shapes the service holds to: a cluster name, a token, an enrollment id, a Kubernetes name
const CLUSTER = /^[a-z0-9]([a-z0-9._-]{0,62}[a-z0-9])?$/
const TOKEN = /^enr_[A-Za-z0-9_-]{43}$/
const ID = /^[0-9a-f]{12}$/
const K8S_NAME = /^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$/

export const validCluster = (s) => CLUSTER.test(String(s || ''))
export const validId = (s) => ID.test(String(s || ''))

/** What is wrong with a cluster name, or ''. */
export function clusterProblem(s) {
  if (!s) return 'Give the cluster a name.'
  return validCluster(s) ? '' : 'Lower-case letters, digits and . _ - only, starting and ending with a letter or digit, at most 64 characters.'
}

/**
 * The address the cluster reaches the hub's ingest service at, tidied: no trailing slash. {url} when usable, {error} when not. It must be plain
 * http(s) with nothing after the host and port that a shell or a proxy could take another way: no credentials, query, fragment or quote.
 */
export function hubUrl(input) {
  const raw = String(input || '').trim()
  if (!raw) return { error: 'Give the address the cluster reaches the hub at, such as http://hub:30081.' }
  if (/[\s'"`$\\<>|;&(){}]/.test(raw)) return { error: 'The address has a character that does not belong in one.' }
  let u
  try {
    u = new URL(raw)
  } catch {
    return { error: 'That is not an address. Start it with http:// or https://.' }
  }
  if (u.protocol !== 'http:' && u.protocol !== 'https:') return { error: 'Start the address with http:// or https://.' }
  if (u.username || u.password) return { error: 'The address must not carry a user name or password.' }
  if (u.search || u.hash) return { error: 'The address must have no query or fragment; the path is added.' }
  return { url: raw.replace(/\/+$/, '') }
}

/**
 * The command to run on the cluster. It asks the hub for the Secret manifest, giving the token, and hands it to kubectl: the key goes through
 * the pipe and is never printed. Every part that goes into it has been checked against a pattern first, so that a token or an address from a
 * service that was not what it seemed cannot put anything else on the line. null when something does not pass.
 */
export function enrollCommand({ url, token, namespace = DEFAULT_NAMESPACE }) {
  const h = hubUrl(url)
  if (h.error || !TOKEN.test(String(token || ''))) return null
  if (!K8S_NAME.test(namespace)) return null
  const body = namespace === DEFAULT_NAMESPACE ? { token } : { token, namespace }
  return `curl -fsS -X POST '${h.url}/v1/enroll?format=secret' -H 'Content-Type: application/json' -d '${JSON.stringify(body)}' | kubectl apply -f -`
}

/** The spoke chart's values that use the Secret the command makes, to put in the values file of that cluster. */
export function valuesSnippet({ cluster, url }) {
  const h = hubUrl(url)
  if (!validCluster(cluster) || h.error) return null
  return `nodeAgent:
  enabled: true
  collectorURL: ${h.url}
  # apiKeySecret defaults to the Secret the command above made
clusterCollector:
  enabled: true
  clusterName: ${cluster}
  image: { repository: <registry>/cluster-collector, tag: <tag> }`
}

export const STATUS = {
  pending: { label: 'Waiting for the cluster', tone: 'caution' },
  redeemed: { label: 'Enrolled', tone: 'ok' },
  expired: { label: 'Expired', tone: 'muted' },
  revoked: { label: 'Revoked', tone: 'bad' },
}

/** Whole seconds until `iso`, never negative; 0 when it is not a time. */
export function secondsLeft(iso, now = Date.now()) {
  const t = Date.parse(iso)
  return Number.isFinite(t) ? Math.max(0, Math.round((t - now) / 1000)) : 0
}

export function clock(seconds) {
  const s = Math.max(0, Math.floor(seconds))
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`
}

/** The enrollments of the project, newest first. */
export async function listEnrollments(client) {
  const rows = await client.get('enrollments')
  return Array.isArray(rows) ? rows : []
}

/** Makes one. -> {id, cluster, status, created_at, expires_at, token}: the only time the token is available. */
export function createEnrollment(client, { cluster, ttlMinutes, manageKey, actor }) {
  const body = { cluster }
  if (ttlMinutes) body.ttl_minutes = ttlMinutes
  return client.send('POST', 'enrollments', { key: manageKey, body, actor })
}

/** Cuts one off: a token that was not redeemed can no longer be, and a key that was issued stops working. */
export function revokeEnrollment(client, id, { manageKey, actor }) {
  if (!validId(id)) return Promise.reject(new Error('That is not an enrollment.'))
  return client.send('DELETE', `enrollments/${id}`, { key: manageKey, actor })
}
