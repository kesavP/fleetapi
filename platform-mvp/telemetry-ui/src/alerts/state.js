// Alert lifecycle across evaluations. A condition must hold on two evaluations in a row before it is FIRING (until then it is
// PENDING, so one odd reading does not raise anything), and must be absent on two in a row before it is RESOLVED, so an alert
// does not flap while a service wobbles. State is a plain list so it can be kept in sessionStorage.
//
// An alert is {id, key, rule, service, label, severity, summary, evidence, status, firstSeen, lastSeen, resolvedAt?, seen, missed}.
// `id` names the condition (the same for every episode of it); `key` names one episode (id plus when it began), and is unique.

export const CONFIRM = 2 // evaluations a condition must hold to fire
export const CLEAR = 2 //   evaluations it must be absent to resolve
export const KEEP_RESOLVED_MS = 24 * 3600 * 1000
export const MAX_RESOLVED = 50

/** The new list of alerts, from the previous list, the conditions that hold now, and the time (ms). Pure. */
export function reconcile(previous, conditions, now) {
  const by = new Map(previous.filter((a) => a.status !== 'resolved').map((a) => [a.id, a])) // the episode in progress, if any
  const out = []
  const holding = new Set()

  for (const c of conditions) {
    holding.add(c.id)
    const old = by.get(c.id)
    if (old && old.status !== 'resolved') {
      const seen = old.seen + 1
      out.push({ ...old, ...c, seen, missed: 0, lastSeen: now, status: seen >= CONFIRM ? 'firing' : 'pending' })
    } else {
      // new, or back after being resolved: a new episode, with its own start
      out.push({ ...c, key: `${c.id}@${now}`, seen: 1, missed: 0, firstSeen: now, lastSeen: now, status: CONFIRM <= 1 ? 'firing' : 'pending' })
    }
  }

  for (const old of previous) {
    if (old.status === 'resolved') {
      if (now - old.resolvedAt < KEEP_RESOLVED_MS) out.push(old) // past episodes stay in the history, even when the condition is back
    } else if (holding.has(old.id)) {
      continue // carried forward above
    } else if (old.status === 'pending') {
      // never fired: it was a blip, and leaves no trace
    } else {
      const missed = old.missed + 1
      out.push(missed >= CLEAR ? { ...old, status: 'resolved', resolvedAt: now, missed } : { ...old, missed })
    }
  }

  const active = out.filter((a) => a.status !== 'resolved')
  const resolved = out.filter((a) => a.status === 'resolved').sort((a, b) => b.resolvedAt - a.resolvedAt).slice(0, MAX_RESOLVED)
  return [...active, ...resolved]
}

const ORDER = { critical: 0, warning: 1 }
/** Firing alerts first, critical before warning, longest-running first; then pending; then resolved, newest first. */
export function sorted(alerts) {
  const rank = { firing: 0, pending: 1, resolved: 2 }
  return [...alerts].sort(
    (a, b) =>
      rank[a.status] - rank[b.status] ||
      (a.status === 'resolved' ? b.resolvedAt - a.resolvedAt : (ORDER[a.severity] ?? 9) - (ORDER[b.severity] ?? 9) || a.firstSeen - b.firstSeen) ||
      a.id.localeCompare(b.id),
  )
}
