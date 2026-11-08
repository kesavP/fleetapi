// Small display helpers shared by the views.

/** An ISO time ("2026-01-01T12:34:56.789Z") or a millisecond timestamp -> the viewer's local date and time; the text itself when it is not a date. */
export function dateTime(iso) {
  const t = typeof iso === 'number' ? iso : Date.parse(iso)
  return Number.isFinite(t) ? new Date(t).toLocaleString() : String(iso ?? '')
}

export const shortId = (id) => (id ? String(id).slice(0, 8) : '')

/** 67108864 -> "64 MiB". */
export function formatBytes(b) {
  if (b == null || !Number.isFinite(Number(b))) return '–'
  let n = Math.abs(Number(b))
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let i = 0
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024
    i++
  }
  const text = n >= 100 || i === 0 ? String(Math.round(n)) : String(Math.round(n * 10) / 10)
  return `${Number(b) < 0 ? '-' : ''}${text} ${units[i]}`
}

/** 750000 -> "12 min"; 3 725 000 -> "1 h 2 min". For how long something has been going on. */
export function formatAge(ms) {
  const m = Math.max(0, Math.floor(ms / 60000))
  if (m < 1) return 'under a minute'
  if (m < 60) return `${m} min`
  const h = Math.floor(m / 60)
  if (h < 48) return m % 60 ? `${h} h ${m % 60} min` : `${h} h`
  return `${Math.floor(h / 24)} days`
}
