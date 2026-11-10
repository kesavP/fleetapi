// Finds incidents: stretches of time in which an application burned its error budget too fast. Pure.
//
// An objective such as "99.9% of requests should not fail" leaves a budget of 0.1%: the share of requests allowed to be bad. The BURN RATE
// is how many times faster than allowed the budget is being used: a window in which 1.44% of requests fail burns it at 14.4x. A breach is
// declared when a short window (5 minutes) AND a long one (an hour) both burn at the threshold or more: the long window says it matters,
// the short one says it is still happening, so an incident ends soon after the trouble does. (The usual multi-window alert.)
// The start and end given are not the windows' but the first and last STEP that was itself burning that fast, so they are as exact as the
// data's step allows.

export const SLO_DEFAULTS = {
  availability: 0.999, // share of requests that should not fail
  latency: 0.999, //      share of requests that should be faster than latencySeconds
  latencySeconds: 0.5,
  burnThreshold: 14.4,
  shortSeconds: 300,
  longSeconds: 3600,
  minRequests: 20, //     requests a window must hold before its share means anything
  joinSeconds: 300, //    two breaches closer than this are one incident
  minSeconds: 120, //     a breach shorter than this is noise
}

const sum = (a, from, to) => {
  let s = 0
  for (let i = Math.max(from, 0); i <= to; i++) s += a[i] || 0
  return s
}

/** Burn rate of a window ending at step `i` and `steps` long: bad share over the budget; 0 when too few requests to judge. */
export function burnRate(total, bad, i, steps, step, objective, minRequests) {
  const t = sum(total, i - steps + 1, i) * step
  if (t < minRequests) return 0
  const b = sum(bad, i - steps + 1, i) * step
  return b / t / (1 - objective)
}

/** The `[first, last]` step indexes of each stretch where `breach[i]` holds, joining stretches at most `join` steps apart. */
function stretches(flags, join) {
  const out = []
  for (let i = 0; i < flags.length; i++) {
    if (!flags[i]) continue
    const last = out[out.length - 1]
    if (last && i - last[1] <= join + 1) last[1] = i
    else out.push([i, i])
  }
  return out
}

/**
 * Incidents of one application. `app` is {total, errors, slow} arrays (see inbound.js); `slow` may be null. Each is
 * {kind: ['availability' | 'latency', ...], start, end (epoch seconds; end is the end of the last bad step), steps: [first, last], open}.
 * `open` is true when the application is still breaching at the last step of the data.
 */
export function findIncidents(app, { start, step, n }, config = {}) {
  const c = { ...SLO_DEFAULTS, ...config }
  const shortN = Math.max(Math.round(c.shortSeconds / step), 1)
  const longN = Math.max(Math.round(c.longSeconds / step), shortN)
  const join = Math.round(c.joinSeconds / step)
  const kinds = [['availability', app.errors, c.availability]]
  if (app.slow) kinds.push(['latency', app.slow, c.latency])
  const perKind = []
  for (const [kind, bad, objective] of kinds) {
    // judged at the step itself, over the short and the long window
    const hot = (i, steps) => burnRate(app.total, bad, i, steps, step, objective, c.minRequests) >= c.burnThreshold
    const breach = Array.from({ length: n }, (_, i) => hot(i, shortN) && hot(i, longN))
    // a step is "burning" when its own few steps are: that is where an incident really begins and ends
    const burning = (i) => burnRate(app.total, bad, i, 1, step, objective, 0) >= c.burnThreshold && (app.total[i] || 0) * step >= 1
    for (const [a, b] of stretches(breach, join)) {
      let first = a
      let last = b
      while (first - 1 >= Math.max(a - shortN, 0) && burning(first - 1)) first--
      while (first < last && !burning(first)) first++
      while (last > first && !burning(last)) last--
      perKind.push({ kind, first, last })
    }
  }
  // availability and latency stretches that overlap are one incident with both kinds
  perKind.sort((x, y) => x.first - y.first)
  const merged = []
  for (const k of perKind) {
    const m = merged[merged.length - 1]
    if (m && k.first <= m.last + join + 1) {
      m.last = Math.max(m.last, k.last)
      if (!m.kinds.includes(k.kind)) m.kinds.push(k.kind)
    } else merged.push({ first: k.first, last: k.last, kinds: [k.kind] })
  }
  return merged
    .filter((m) => (m.last - m.first + 1) * step >= c.minSeconds)
    .map((m) => ({ kind: m.kinds, start: start + m.first * step, end: start + (m.last + 1) * step, steps: [m.first, m.last], open: m.last >= n - 1 }))
}

/** The objective's standing over steps [from, to]: {availability, latency} compliance (0..1, null when too few requests) and burn rates. */
export function sloStanding(app, { step }, from, to, config = {}) {
  const c = { ...SLO_DEFAULTS, ...config }
  const total = sum(app.total, from, to) * step
  const one = (bad, objective) => {
    if (!bad || total < 1) return { compliance: null, burn: null }
    const b = sum(bad, from, to) * step
    return { compliance: 1 - b / total, burn: b / total / (1 - objective) }
  }
  return { requests: total, availability: one(app.errors, c.availability), latency: one(app.slow, c.latency) }
}
