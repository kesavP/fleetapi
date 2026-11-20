import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import TimeSeries from './TimeSeries.vue'

const from = Date.parse('2026-11-18T10:00:00')
const to = Date.parse('2026-11-18T11:00:00')
const pts = (...vs) => vs.map(([m, v]) => ({ t: from + m * 60000, v }))
const mk = (props = {}) => mount(TimeSeries, { props: { label: 'Memory', from, to, ...props } })

describe('TimeSeries', () => {
  it('draws one line per series with a name for the whole chart and the facts in text', () => {
    const w = mk({ series: [{ name: 'memory', points: pts([0, 100], [30, 200], [60, 150]) }], format: (v) => `${v} MiB` })
    expect(w.findAll('[data-testid=ts-line]')).toHaveLength(1)
    const label = w.get('svg').attributes('aria-label')
    expect(label).toContain('Memory')
    expect(label).toContain('10:00 – 11:00')
    expect(label).toContain('memory: now 150 MiB, highest 200 MiB')
    expect(w.get('svg').attributes('role')).toBe('img')
    expect(w.get('.ts-legend').text()).toContain('150 MiB')
  })
  it('says there is no data instead of drawing an empty chart', () => {
    const w = mk({ series: [{ name: 'memory', points: [] }] })
    expect(w.get('[data-testid=ts-empty]').text()).toBe('Memory: no data in this window.')
    expect(w.find('svg').exists()).toBe(false)
  })
  it('marks moments on the chart and lists them as text', () => {
    const w = mk({ series: [{ name: 'm', points: pts([0, 1], [60, 1]) }], markers: [{ t: from + 15 * 60000, kind: 'started', label: 'Incident started' }, { t: from + 40 * 60000, kind: 'restart', label: '2 restarts' }] })
    expect(w.findAll('[data-testid=ts-mark]')).toHaveLength(2)
    expect(w.get('.ts-marks').text()).toContain('10:15 Incident started')
    expect(w.get('.ts-marks').text()).toContain('10:40 2 restarts')
  })
  it('shows a limit line, named, and none when there is no limit', () => {
    const series = [{ name: 'm', points: pts([0, 1], [60, 2]) }]
    const w = mk({ series, limit: { value: 4, label: 'memory limit' } })
    expect(w.find('[data-testid=ts-limit]').exists()).toBe(true)
    expect(w.get('.ts-legend').text()).toContain('memory limit')
    expect(mk({ series }).find('[data-testid=ts-limit]').exists()).toBe(false)
  })
  it('draws a dashed series (yesterday) apart from today, and a series with no data says so', () => {
    const w = mk({ series: [{ name: 'today', points: pts([0, 1], [60, 2]) }, { name: 'yesterday', points: pts([0, 1], [60, 1]), dashed: true }, { name: 'none', points: [] }] })
    expect(w.findAll('g.line.dashed')).toHaveLength(1)
    expect(w.findAll('.ts-legend li')[2].text()).toContain('no data')
  })
  it('shows names as text, never as markup', () => {
    const w = mk({ series: [{ name: '<img src=x onerror=alert(1)>', points: pts([0, 1], [60, 1]) }], markers: [{ t: from + 60000, kind: 'event', label: '<b>x</b>' }] })
    expect(w.find('img').exists()).toBe(false)
    expect(w.find('.ts-marks b').exists()).toBe(false)
  })
})
