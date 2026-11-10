// The ClickHouse tab's data: how the telemetry service is connected to its database, how much each kind of data takes, and (when the
// operator shares it) the server's disks. The answer comes from GET /api/v1/projects/<project>/clickhouse.
//
// What it never holds: the database's address, user or password. The service does not send them, and the page shows "<hidden>" itself.

export const HIDDEN = '<hidden>'

export async function loadClickHouse(client) {
  const info = await client.get('clickhouse')
  if (!info || typeof info !== 'object' || !info.connection) throw new Error('The telemetry service answered, but not with ClickHouse information. It may be an older version.')
  return {
    connection: info.connection,
    storage: Array.isArray(info.storage) ? info.storage : [],
    cluster: info.cluster && Array.isArray(info.cluster.servers) ? info.cluster : null,
    warnings: Array.isArray(info.warnings) ? info.warnings : [],
  }
}

/** 604800 -> "7d"; 43200 -> "12h"; 5400 -> "90m"; nothing -> "–". */
export function formatTtl(seconds) {
  const s = Number(seconds)
  if (!(s > 0)) return '–'
  if (s % 86400 === 0) return `${s / 86400}d`
  if (s % 3600 === 0) return `${s / 3600}h`
  if (s % 60 === 0) return `${s / 60}m`
  return `${Math.round(s)}s`
}

/** How long ago something is, in the unit that fits: "25m ago", "5h ago", "7d ago"; "–" for no date. */
export function formatSince(iso, now = Date.now()) {
  const t = Date.parse(iso)
  if (!Number.isFinite(t)) return '–'
  const minutes = Math.max(0, Math.floor((now - t) / 60000))
  if (minutes < 1) return 'just now'
  if (minutes < 60) return `${minutes}m ago`
  const hours = Math.floor(minutes / 60)
  if (hours < 48) return `${hours}h ago`
  return `${Math.floor(hours / 24)}d ago`
}

/** 11.04 -> "11.0x"; 0 or nothing (not known) -> "–". */
export const formatRatio = (x) => (Number(x) > 0 ? `${Number(x).toFixed(1)}x` : '–')

/** Share of a disk in use, 0 to 100 (whole numbers), or null when the size is not known. */
export function usedPercent(disk) {
  const total = Number(disk && disk.total)
  const free = Number(disk && disk.free)
  if (!(total > 0) || !(free >= 0)) return null
  return Math.min(100, Math.max(0, Math.round(((total - free) / total) * 100)))
}

/** How worried to be about a disk: full enough that something should be done soon. */
export const diskLevel = (pct) => (pct == null ? 'unknown' : pct >= 90 ? 'bad' : pct >= 75 ? 'caution' : 'ok')

export const KIND_LABEL = { metrics: 'metrics', traces: 'traces', logs: 'logs', profiles: 'profiling' }
