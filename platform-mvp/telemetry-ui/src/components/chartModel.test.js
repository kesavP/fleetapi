import { describe, expect, it } from 'vitest'
import { chartModel, niceMax } from './timeSeries.js'

const pts = (...vs) => vs.map(([t, v]) => ({ t, v }))

describe('niceMax', () => {
  it('rounds up to 1, 2, 5 or 10 times a power of ten', () => {
    expect([0.03, 1, 1.1, 2.1, 5, 5.1, 130, 99999].map(niceMax)).toEqual([0.05, 1, 2, 5, 5, 10, 200, 100000])
  })
  it('is 1 for nothing usable', () => {
    expect([0, -4, NaN, Infinity, undefined].map(niceMax)).toEqual([1, 1, 1, 1, 1])
  })
})

describe('chartModel', () => {
  const base = { from: 0, to: 1000, width: 100, height: 50 }
  it('draws a line from zero up, high values at the top', () => {
    const m = chartModel({ ...base, series: [{ name: 'cpu', points: pts([0, 0], [500, 2], [1000, 4]) }] })
    expect(m.yMax).toBe(5)
    expect(m.lines[0].d).toBe('M0 50 L50 30 L100 10')
    expect(m.stats[0]).toEqual({ name: 'cpu', count: 3, latest: 4, max: 4, min: 0 })
  })
  it('breaks the line at a gap instead of sloping across it, and draws a lone point as a dot', () => {
    const m = chartModel({ ...base, series: [{ name: 's', points: pts([0, 1], [100, 1], [200, 1], [800, 1], [900, 1], [1000, 1]) }] })
    expect(m.lines[0].d.match(/M/g)).toHaveLength(2)
    const lone = chartModel({ ...base, series: [{ name: 's', points: pts([0, 1], [100, 1], [200, 1], [900, 1]) }] })
    expect(lone.lines[0].dots).toEqual([{ x: 90, y: 0 }])
  })
  it('lets the limit lift the axis, so a line near its limit is shown near it', () => {
    const m = chartModel({ ...base, series: [{ name: 'mem', points: pts([0, 10], [1000, 20]) }], limit: { value: 100, label: 'limit' } })
    expect(m.yMax).toBe(100)
    expect(m.limitY).toBe(0)
  })
  it('ignores points outside the range and values that are not numbers, and sorts the rest', () => {
    const m = chartModel({ ...base, series: [{ name: 's', points: pts([2000, 9], [500, NaN], [800, 3], [200, 1]) }] })
    expect(m.stats[0].count).toBe(2)
    expect(m.lines[0].d).toBe('M20 40 L80 20')
  })
  it('keeps a negative reading on the axis rather than drawing below it', () => {
    const m = chartModel({ ...base, series: [{ name: 's', points: pts([0, -3], [500, 1], [1000, 1]) }] })
    expect(m.lines[0].d.startsWith('M0 50')).toBe(true)
  })
  it('places the markers that fall inside the range, and only those', () => {
    const m = chartModel({ ...base, series: [{ name: 's', points: pts([0, 1], [1000, 1]) }], markers: [{ t: 250, kind: 'started', label: 'a' }, { t: 5000, kind: 'event', label: 'b' }] })
    expect(m.marks).toEqual([{ x: 25, kind: 'started', label: 'a', t: 250 }])
  })
  it('is empty with no points or no range, and still places markers when there are no points', () => {
    expect(chartModel({ ...base, series: [{ name: 's', points: [] }] }).empty).toBe(true)
    expect(chartModel({ from: 5, to: 5, series: [] }).empty).toBe(true)
    expect(chartModel({ ...base, series: [], markers: [{ t: 500, kind: 'x', label: 'm' }] }).marks).toHaveLength(1)
  })
})
