// What one application's page needs (its spans, logs and profiles), and the time windows every page offers.
// The list of applications is built in applications.js.

export const WINDOWS = [
  { label: '15 minutes', seconds: 15 * 60 },
  { label: '1 hour', seconds: 60 * 60 },
  { label: '6 hours', seconds: 6 * 60 * 60 },
  { label: '24 hours', seconds: 24 * 60 * 60 },
]

/**
 * Everything one service's page needs, for the last `seconds` seconds. The three calls are independent: one
 * failing leaves the others, and its message goes in `errors` (keyed spans, logs, profiles).
 */
export async function loadService(client, name, { seconds, now = () => Date.now() } = {}) {
  const toSec = Math.floor(now() / 1000)
  const range = { from: toSec - seconds, to: toSec }
  const calls = {
    spans: () => client.get('traces', { service: name, ...range, limit: 1000 }),
    logs: () => client.get('logs', { service: name, ...range, limit: 1000 }),
    profiles: () => client.get('profiles'),
  }
  const keys = Object.keys(calls)
  const settled = await Promise.allSettled(keys.map((k) => calls[k]()))
  const out = { spans: [], logs: [], profiles: [], errors: {}, from: range.from * 1000, to: range.to * 1000 }
  settled.forEach((r, i) => {
    const k = keys[i]
    if (r.status !== 'fulfilled') {
      out.errors[k] = r.reason && r.reason.message ? r.reason.message : 'failed'
    } else if (k === 'profiles') {
      out.profiles = (Array.isArray(r.value) ? r.value : []).filter((p) => p && p.service === name)
    } else {
      out[k] = Array.isArray(r.value) ? r.value : []
    }
  })
  return out
}
