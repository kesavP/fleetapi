// The geometry of a small time-series chart, kept apart from the component so it can be tested without a browser: points in, SVG path text out.
// Values start at zero (a chart of a resource that does not show zero misleads), and a gap in the data is a gap in the line, not a slope across it.

export const W = 600
export const H = 140

/** A round upper bound for the value axis: 1, 2, 5 or 10 times a power of ten. */
export function niceMax(v) {
  if (!(v > 0) || !Number.isFinite(v)) return 1
  const p = 10 ** Math.floor(Math.log10(v))
  const n = v / p
  return (n <= 1 ? 1 : n <= 2 ? 2 : n <= 5 ? 5 : 10) * p
}

const r = (x) => Math.round(x * 10) / 10
const median = (xs) => {
  const s = [...xs].sort((a, b) => a - b)
  return s.length ? s[Math.floor(s.length / 2)] : 0
}

/**
 * series: [{name, points: [{t, v}]}], markers: [{t, kind, label}], from/to: epoch ms, limit: {value, label} | null.
 * -> {empty, yMax, limitY, lines: [{name, d, dots: [{x, y}]}], marks: [{x, kind, label, t}], stats: [{name, latest, max, min, count}]}
 * A run of points is a line; a point with no neighbour in reach is a dot. "In reach" is two and a half times the usual spacing of the series.
 */
export function chartModel({ series = [], markers = [], from, to, limit = null, width = W, height = H } = {}) {
  const span = to - from
  const empty = { empty: true, yMax: 1, limitY: null, lines: [], marks: [], stats: [] }
  if (!(span > 0)) return empty
  const x = (t) => ((t - from) / span) * width
  const inRange = (t) => t >= from && t <= to
  const kept = series.map((s) => ({ name: s.name, points: (s.points || []).filter((p) => Number.isFinite(p.t) && Number.isFinite(p.v) && inRange(p.t)).sort((a, b) => a.t - b.t) }))
  const values = kept.flatMap((s) => s.points.map((p) => p.v))
  const limitValue = limit && Number.isFinite(limit.value) && limit.value > 0 ? limit.value : null
  if (!values.length) return { ...empty, marks: markers.filter((m) => inRange(m.t)).map((m) => ({ x: r(x(m.t)), kind: m.kind, label: m.label, t: m.t })) }
  const yMax = niceMax(Math.max(...values, limitValue || 0))
  const y = (v) => r(height - (Math.min(Math.max(v, 0), yMax) / yMax) * height)
  const lines = kept.map((s) => {
    const reach = median(s.points.slice(1).map((p, i) => p.t - s.points[i].t)) * 2.5
    const runs = []
    for (const p of s.points) {
      const last = runs[runs.length - 1]
      if (last && p.t - last[last.length - 1].t <= reach) last.push(p)
      else runs.push([p])
    }
    return {
      name: s.name,
      d: runs.filter((run) => run.length > 1).map((run) => run.map((p, i) => `${i ? 'L' : 'M'}${r(x(p.t))} ${y(p.v)}`).join(' ')).join(' '),
      dots: runs.filter((run) => run.length === 1).map(([p]) => ({ x: r(x(p.t)), y: y(p.v) })),
    }
  })
  const stats = kept.map((s) => ({
    name: s.name,
    count: s.points.length,
    latest: s.points.length ? s.points[s.points.length - 1].v : null,
    max: s.points.length ? Math.max(...s.points.map((p) => p.v)) : null,
    min: s.points.length ? Math.min(...s.points.map((p) => p.v)) : null,
  }))
  return {
    empty: false,
    yMax,
    limitY: limitValue ? y(limitValue) : null,
    lines,
    marks: markers.filter((m) => inRange(m.t)).map((m) => ({ x: r(x(m.t)), kind: m.kind, label: m.label, t: m.t })),
    stats,
  }
}
