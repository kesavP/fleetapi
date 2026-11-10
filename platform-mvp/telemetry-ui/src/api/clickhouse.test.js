import { describe, expect, it, vi } from 'vitest'
import { HIDDEN, diskLevel, formatRatio, formatSince, formatTtl, loadClickHouse, usedPercent } from './clickhouse'

describe('loadClickHouse', () => {
  const info = { connection: { reachable: true, protocol: 'native', database: 'telemetry', tls: false, version: '24.8.1', ping_ms: 2 }, storage: [{ type: 'logs' }], cluster: { servers: [{ host: 'h', disks: [] }] }, warnings: ['w'] }
  it('asks for the project\'s ClickHouse information and returns it', async () => {
    const get = vi.fn(async () => info)
    expect(await loadClickHouse({ get })).toEqual(info)
    expect(get).toHaveBeenCalledWith('clickhouse')
  })
  it('turns what is missing into empty lists and no cluster, never into null where the page loops', async () => {
    const r = await loadClickHouse({ get: async () => ({ connection: { reachable: true } }) })
    expect(r).toEqual({ connection: { reachable: true }, storage: [], cluster: null, warnings: [] })
    expect((await loadClickHouse({ get: async () => ({ connection: {}, cluster: { servers: 'x' } }) })).cluster).toBeNull()
  })
  it('refuses an answer that is not ClickHouse information, as from a service that is too old, and says so', async () => {
    for (const bad of [null, {}, [], 'x', { storage: [] }]) await expect(loadClickHouse({ get: async () => bad })).rejects.toThrow('not with ClickHouse information')
  })
  it('passes on a failure from the service unchanged', async () => {
    await expect(loadClickHouse({ get: async () => { throw new Error('Not found.') } })).rejects.toThrow('Not found.')
  })
  it('has the word the page shows where the address and credentials would be', () => {
    expect(HIDDEN).toBe('<hidden>')
  })
})

describe('formatTtl', () => {
  it('says retention in the unit that fits', () => {
    expect(formatTtl(7 * 86400)).toBe('7d')
    expect(formatTtl(14 * 86400)).toBe('14d')
    expect(formatTtl(12 * 3600)).toBe('12h')
    expect(formatTtl(90 * 60)).toBe('90m')
    expect(formatTtl(45)).toBe('45s')
  })
  it('is a dash for none', () => {
    for (const x of [0, -1, null, undefined, 'x']) expect(formatTtl(x)).toBe('–')
  })
})

describe('formatSince', () => {
  const NOW = Date.parse('2026-01-10T12:00:00Z')
  const ago = (ms) => new Date(NOW - ms).toISOString()
  it('says how long ago, in the unit that fits', () => {
    expect(formatSince(ago(20_000), NOW)).toBe('just now')
    expect(formatSince(ago(25 * 60_000), NOW)).toBe('25m ago')
    expect(formatSince(ago(5 * 3600_000), NOW)).toBe('5h ago')
    expect(formatSince(ago(47 * 3600_000), NOW)).toBe('47h ago')
    expect(formatSince(ago(7 * 86400_000), NOW)).toBe('7d ago')
  })
  it('is a dash for no date, and never goes negative for a clock that is a little behind', () => {
    for (const x of [null, undefined, '', 'soon']) expect(formatSince(x, NOW)).toBe('–')
    expect(formatSince(new Date(NOW + 5000).toISOString(), NOW)).toBe('just now')
  })
})

describe('formatRatio / usedPercent / diskLevel', () => {
  it('shows a compression ratio, or a dash when it is not known', () => {
    expect(formatRatio(11.04)).toBe('11.0x')
    expect(formatRatio(5.95)).toBe('6.0x')
    for (const x of [0, null, undefined, -2]) expect(formatRatio(x)).toBe('–')
  })
  it('works out how full a disk is', () => {
    expect(usedPercent({ free: 61, total: 100 })).toBe(39)
    expect(usedPercent({ free: 0, total: 100 })).toBe(100)
    expect(usedPercent({ free: 100, total: 100 })).toBe(0)
    expect(usedPercent({ free: 150, total: 100 })).toBe(0) // a free space larger than the disk is clamped, not negative
  })
  it('is null when the size is not known', () => {
    for (const d of [null, {}, { free: 5, total: 0 }, { free: -1, total: 10 }, { free: 'x', total: 10 }]) expect(usedPercent(d)).toBeNull()
  })
  it('is worried at 75% and at 90%', () => {
    expect([diskLevel(0), diskLevel(74), diskLevel(75), diskLevel(89), diskLevel(90), diskLevel(100), diskLevel(null)]).toEqual(['ok', 'ok', 'caution', 'caution', 'bad', 'bad', 'unknown'])
  })
})
