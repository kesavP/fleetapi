import { describe, expect, it } from 'vitest'
import { dateTime, formatAge, formatBytes, shortId } from './format'

describe('dateTime', () => {
  const ms = Date.parse('2026-01-01T12:34:56Z')
  it('reads an ISO time and a millisecond timestamp alike', () => {
    expect(dateTime('2026-01-01T12:34:56Z')).toBe(new Date(ms).toLocaleString())
    expect(dateTime(ms)).toBe(new Date(ms).toLocaleString())
  })
  it('gives the text back when it is not a date', () => {
    expect(dateTime('soon')).toBe('soon')
    expect(dateTime(undefined)).toBe('')
  })
})

describe('formatAge', () => {
  it('says how long in the unit that fits', () => {
    expect(formatAge(0)).toBe('under a minute')
    expect(formatAge(59_999)).toBe('under a minute')
    expect(formatAge(12 * 60_000)).toBe('12 min')
    expect(formatAge(60 * 60_000)).toBe('1 h')
    expect(formatAge(125 * 60_000)).toBe('2 h 5 min')
    expect(formatAge(72 * 3600_000)).toBe('3 days')
    expect(formatAge(-5000)).toBe('under a minute')
  })
})

describe('formatBytes / shortId', () => {
  it('formats sizes', () => {
    expect(formatBytes(0)).toBe('0 B')
    expect(formatBytes(1536)).toBe('1.5 KiB')
    expect(formatBytes(64 * 1024 * 1024)).toBe('64 MiB')
    expect(formatBytes(null)).toBe('–')
  })
  it('shortens an id', () => {
    expect(shortId('abcdef0123456789')).toBe('abcdef01')
    expect(shortId('')).toBe('')
  })
})
