// How the card plugins write numbers, so every card reads the same.

const trim = (x, digits) => Number(x.toFixed(digits))

/** A rate a second: 0.25 -> "0.25/s", 12.34 -> "12.3/s", 400.2 -> "400/s". */
export function perSecond(x) {
  const v = !Number.isFinite(x) ? 0 : x >= 100 ? Math.round(x) : x >= 10 ? trim(x, 1) : trim(x, 2)
  return `${v}/s`
}

/** A time in seconds: 0.0002 -> "0.2 ms", 0.004 -> "4 ms", 0.35 -> "350 ms", 1.5 -> "1.5 s". */
export function duration(seconds) {
  const ms = seconds * 1000
  if (ms >= 1000) return `${trim(ms / 1000, 1)} s`
  if (ms >= 10) return `${Math.round(ms)} ms`
  return `${trim(ms, 1)} ms`
}

/** A share (0 to 1) as a percentage: 0.005 -> "0.5%", 0.1 -> "10%". */
export const percent = (share) => `${trim(share * 100, 1)}%`

export const sum = (xs, f) => xs.reduce((a, x) => a + f(x), 0)
