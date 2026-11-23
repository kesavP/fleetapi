// Naming and address helpers for the service map. Everything here is pure.

// The node-agent names a service from its container id with these patterns (common/otel.go there), and the same
// name is the service name on its traces. Using them here keeps a map node and a traces/logs service identical.
const DEPLOYMENT = /^(\/k8s\/[a-z0-9-]+\/[a-z0-9-]+)-[0-9a-f]{1,10}-[bcdfghjklmnpqrstvwxz2456789]{5}\/.+/
const DAEMONSET = /^(\/k8s\/[a-z0-9-]+\/[a-z0-9-]+)-[bcdfghjklmnpqrstvwxz2456789]{5}\/.+/
const STATEFULSET = /^(\/k8s\/[a-z0-9-]+\/[a-z0-9-]+)-\d+\/.+/
const CRONJOB = /^(\/k8s-cronjob\/[a-z0-9-]+\/[a-z0-9-]+)\/.+/
const BARE_POD = /^(\/k8s\/[a-z0-9-]+\/[a-z0-9-]+)\/.+/

// Containers the node-agent names for orchestrators this map does not group (a Nomad job, an ECS service, a Swarm service). They are left
// out rather than mis-grouped as hosts.
const UNSUPPORTED = /^\/(nomad|ecs|swarm)\//

/**
 * A process or container on a plain host (a VM, a bare-metal machine, plain Docker): the node-agent's container id is its cgroup path, such as
 * "/system.slice/nginx.service" or "/docker/postgres", the same on every host that runs it, so those hosts are instances of one application.
 * That id is also the service name on its traces and logs, so it is kept as the application id. The name is the last part without ".service".
 */
function hostWorkload(id) {
  if (!id.startsWith('/') || id === '/' || UNSUPPORTED.test(id) || /^\/k8s(-cronjob)?\//.test(id)) return null // a Kubernetes id is never a host's
  const last = id.split('/').filter(Boolean).pop() || ''
  const name = last.replace(/\.(service|slice)$/, '') || last
  return name ? { id, ns: 'host', name, label: name, host: true } : null
}

/**
 * The application a container id belongs to. "/k8s/shop/frontend-5d6f8c7b9-xk2lp/server" -> {id: "/k8s/shop/frontend", ns, name, label}; a
 * container on a plain host ("/system.slice/nginx.service") is an application of its own with `host: true`; null for anything else.
 */
export function workloadOf(containerId) {
  const id = String(containerId || '')
  for (const re of [DEPLOYMENT, DAEMONSET, STATEFULSET, CRONJOB, BARE_POD]) {
    const m = re.exec(id)
    if (m) {
      const [, , ns, name] = m[1].split('/')
      return { id: m[1], ns, name, label: `${ns}/${name}` }
    }
  }
  return hostWorkload(id)
}

/** The application an APPLICATION id stands for (as opposed to a container id): the same, for "/k8s/shop/frontend" or "/system.slice/nginx.service". */
export function workloadForId(applicationId) {
  const id = String(applicationId || '')
  const k = workloadOf(`${id}/x`) // a Kubernetes application id has no container part: give it one
  if (k && !k.host && k.id === id) return k
  const h = hostWorkload(id)
  return h && h.id === id ? h : null
}

/**
 * The pod a container id belongs to ("/k8s/shop/frontend-5d6f8c7b9-xk2lp/server" -> "/k8s/shop/frontend-5d6f8c7b9-xk2lp"), so a
 * pod with a sidecar counts once. A cronjob's id has no pod part and stands for itself; null for a host process.
 */
export function podOf(containerId) {
  const m = /^(\/k8s(?:-cronjob)?\/[a-z0-9-]+\/[a-z0-9-]+)\/.+/.exec(String(containerId || ''))
  return m ? m[1] : null
}

/**
 * Which instance of its application a series is from: the pod for a Kubernetes container (so a sidecar is not a second instance), the
 * machine for a container on a plain host (every series the agent sends carries `machine_id`, and the same unit on two machines is two
 * instances). null for what is neither.
 */
export function instanceOf(labels) {
  const pod = podOf(labels.container_id)
  if (pod) return pod
  const w = workloadOf(labels.container_id)
  return w && w.host ? `${w.id}@${labels.machine_id || 'unknown-machine'}` : null
}

/** The port of "ip:port" or "[ipv6]:port", as text; '' when there is none. */
export function portOf(address) {
  const a = String(address || '').trim()
  const m = a.startsWith('[') ? /\]:(\d+)$/.exec(a) : (a.match(/:/g) || []).length === 1 ? /:(\d+)$/.exec(a) : null
  return m ? m[1] : ''
}

/** The host part of "ip:port", "[ipv6]:port" or "name:port"; a bare address is returned as it is. */
export function hostOf(address) {
  const a = String(address || '').trim()
  if (a.startsWith('[')) return a.slice(1).split(']')[0]
  if ((a.match(/:/g) || []).length === 1) return a.split(':')[0]
  return a
}

/** True for 127.0.0.0/8, ::1 (also as an IPv4-mapped address) and "localhost"; 128.0.0.1 and 127.example.com are not. */
export function isLoopback(address) {
  let h = hostOf(address).toLowerCase()
  if (h === 'localhost' || h === '::1') return true
  const mapped = /^::ffff:(\d+\.\d+\.\d+\.\d+)$/.exec(h)
  if (mapped) h = mapped[1]
  return /^127\.\d{1,3}\.\d{1,3}\.\d{1,3}$/.test(h)
}

export const isUnspecified = (address) => ['0.0.0.0', '::', ''].includes(hostOf(address))

const GRPC_PREFIX = 'grpc:'
// gRPC runs over HTTP/2 and is counted with status "grpc:<CODE>". A server-side failure is the counterpart of a 5xx; the
// codes a caller causes (INVALID_ARGUMENT, NOT_FOUND, ...) are not counted, as 4xx is not. This is the set that
// OpenTelemetry's gRPC conventions treat as errors for a server span.
const GRPC_SERVER_ERRORS = new Set(['UNKNOWN', 'DEADLINE_EXCEEDED', 'UNIMPLEMENTED', 'INTERNAL', 'UNAVAILABLE', 'DATA_LOSS'])

/** True for an HTTP 5xx ("500" or the "5xx" class form) and for a gRPC server-side failure code. */
export function isServerError(status) {
  const s = String(status || '')
  if (s.startsWith(GRPC_PREFIX)) return GRPC_SERVER_ERRORS.has(s.slice(GRPC_PREFIX.length))
  return /^5..$/.test(s)
}

/**
 * The q-quantile of a latency histogram as Prometheus computes it. `buckets` maps each upper bound (seconds, Infinity for
 * the overflow bucket) to the cumulative per-second rate at or under it. Interpolated linearly in the bucket that holds the
 * rank; a rank in the overflow bucket gives the largest finite bound ("at least this"). null when there is nothing.
 */
export function histogramQuantile(q, buckets) {
  const pts = [...buckets.entries()].sort((a, b) => a[0] - b[0])
  if (!pts.length) return null
  let running = 0
  // Bucket rates are summed independently, so they can dip; a cumulative histogram never does.
  const cum = pts.map(([bound, count]) => [bound, (running = Math.max(running, count))])
  const total = cum[cum.length - 1][1]
  if (!(total > 0)) return null
  const rank = q * total
  let prevBound = 0
  let prevCount = 0
  for (const [bound, count] of cum) {
    if (count >= rank) {
      if (bound === Infinity) return prevBound > 0 ? prevBound : null
      if (count === prevCount) return bound
      return prevBound + ((bound - prevBound) * (rank - prevCount)) / (count - prevCount)
    }
    prevBound = bound
    prevCount = count
  }
  return null
}

/** A histogram "le" label to a number: "0.5", "+Inf" (also "Inf"). NaN when it is neither. */
export function parseLe(le) {
  const s = String(le ?? '').trim()
  if (/^\+?inf$/i.test(s)) return Infinity
  return /^[0-9.eE+-]+$/.test(s) ? Number(s) : NaN
}

// Data stores and brokers the node-agent counts as it does HTTP: per destination, with a status of ok, failed or unknown,
// and (for most) a latency histogram. Only the protocols a project reports are queried. The latency is the CLIENT's
// measurement and is only as good as the agent's matching of a request to its response; ClickHouse has none because that
// disagreed with the server's own histogram by two orders of magnitude (p95 8.7 s there against 22 ms), while Redis,
// checked the same way, agreed to the digit. Put a histogram name back only after checking it the same way.
export const L7_PROTOCOLS = [
  { name: 'postgres', calls: 'container_postgres_queries_total', latency: 'container_postgres_queries_duration_seconds_total', unit: 'queries/s' },
  { name: 'mysql', calls: 'container_mysql_queries_total', latency: 'container_mysql_queries_duration_seconds_total', unit: 'queries/s' },
  { name: 'redis', calls: 'container_redis_queries_total', latency: 'container_redis_queries_duration_seconds_total', unit: 'queries/s' },
  { name: 'memcached', calls: 'container_memcached_queries_total', latency: 'container_memcached_queries_duration_seconds_total', unit: 'queries/s' },
  { name: 'mongo', calls: 'container_mongo_queries_total', latency: 'container_mongo_queries_duration_seconds_total', unit: 'queries/s' },
  { name: 'clickhouse', calls: 'container_clickhouse_queries_total', latency: null, unit: 'queries/s' },
  { name: 'cassandra', calls: 'container_cassandra_queries_total', latency: 'container_cassandra_queries_duration_seconds_total', unit: 'queries/s' },
  { name: 'foundationdb', calls: 'container_foundationdb_requests_total', latency: 'container_foundationdb_requests_duration_seconds_total', unit: 'req/s' },
  { name: 'kafka', calls: 'container_kafka_requests_total', latency: 'container_kafka_requests_duration_seconds_total', unit: 'req/s' },
  { name: 'rabbitmq', calls: 'container_rabbitmq_messages_total', latency: null, unit: 'msgs/s' },
  { name: 'nats', calls: 'container_nats_messages_total', latency: null, unit: 'msgs/s' },
]
export const HTTP_UNIT = 'req/s'
export const FAILED_STATUS = 'failed' // what the agent labels a failed query, as 5xx labels a failed HTTP request
