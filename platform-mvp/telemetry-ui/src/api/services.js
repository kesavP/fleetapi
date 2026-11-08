// The services a project has, put together from what the API offers: it has no list of its own, so the names
// come from the traces, the logs and the profiles. A service seen in any of them is listed, with a flag for
// each signal it has. One source failing does not hide the others: its error is returned beside the list.

export const WINDOWS = [
  { label: '15 minutes', seconds: 15 * 60 },
  { label: '1 hour', seconds: 60 * 60 },
  { label: '6 hours', seconds: 6 * 60 * 60 },
  { label: '24 hours', seconds: 24 * 60 * 60 },
]

const SOURCES = ['traces', 'logs', 'profiles']

function entry(map, name) {
  if (!map.has(name)) map.set(name, { name, traces: false, logs: false, profiles: [] })
  return map.get(name)
}

/** The merged, name-sorted list from three source answers (each an array, null or missing). Pure. */
export function mergeServices({ traces, logs, profiles }) {
  const map = new Map()
  for (const name of traces || []) if (name) entry(map, name).traces = true
  for (const name of logs || []) if (name) entry(map, name).logs = true
  for (const p of profiles || []) {
    if (!p || !p.service) continue
    const e = entry(map, p.service)
    if (p.type && !e.profiles.includes(p.type)) e.profiles.push(p.type)
  }
  for (const e of map.values()) e.profiles.sort()
  return [...map.values()].sort((a, b) => a.name.localeCompare(b.name))
}

/**
 * Loads the three sources for the last `seconds` seconds (profiles are not time-bound in the API: every
 * profiled service is listed). Returns {services, errors}, where errors maps a source to its message.
 */
export async function loadServices(client, { seconds, now = () => Date.now() } = {}) {
  const to = Math.floor(now() / 1000)
  const range = { from: to - seconds, to }
  const calls = {
    traces: () => client.get('traces/services', range),
    logs: () => client.get('logs/services', range),
    profiles: () => client.get('profiles'),
  }
  const settled = await Promise.allSettled(SOURCES.map((s) => calls[s]()))
  const answers = {}
  const errors = {}
  settled.forEach((r, i) => {
    if (r.status === 'fulfilled') answers[SOURCES[i]] = r.value
    else errors[SOURCES[i]] = r.reason && r.reason.message ? r.reason.message : 'failed'
  })
  return { services: mergeServices(answers), errors }
}

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
