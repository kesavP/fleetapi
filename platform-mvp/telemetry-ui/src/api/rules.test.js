import { describe, expect, it, vi } from 'vitest'
import { RULE_NAMES, loadRules, parseRule, saveRule, sameSettings, settingsProblems } from './rules.js'

const row = (name = 'rightsize.memory', over = {}) => ({
  name, enabled: true, percent: 50, days: 7, percentile: 95, version: 0, saved: false,
  default: { enabled: true, percent: 50, days: 7, percentile: 95 }, ...over,
})

describe('parseRule', () => {
  it('keeps what the page shows, with the default beside it', () => {
    const r = parseRule(row('rightsize.cpu', { version: 3, saved: true, updated_by: 'sam', updated_at: '2026-11-17T10:00:00Z', percent: 40 }))
    expect(r).toEqual({
      name: 'rightsize.cpu', settings: { enabled: true, percent: 40, days: 7, percentile: 95 }, default: { enabled: true, percent: 50, days: 7, percentile: 95 },
      version: 3, saved: true, updatedBy: 'sam', updatedAt: '2026-11-17T10:00:00Z',
    })
  })
  it('fills what is missing with something safe, and says nothing was saved', () => {
    const r = parseRule(row('rightsize.memory'))
    expect(r.updatedBy).toBe('')
    expect(r.updatedAt).toBe('')
    expect(r.saved).toBe(false)
    expect(r.version).toBe(0)
  })
  it('skips a row that is not a rule it knows, or whose settings are not numbers', () => {
    for (const bad of [null, undefined, 'x', 5, {}, row('rightsize.network'), row('../etc'), row('rightsize.memory', { percent: 'fifty' }), row('rightsize.memory', { days: null }), row('rightsize.memory', { enabled: 'yes' }), row('rightsize.memory', { default: null }), row('rightsize.memory', { percentile: NaN })]) {
      expect(parseRule(bad)).toBeNull()
    }
  })
  it('does not trust a version that is not a whole number', () => {
    expect(parseRule(row('rightsize.memory', { version: -1 }))).toBeNull()
    expect(parseRule(row('rightsize.memory', { version: 1.5 }))).toBeNull()
  })
})

describe('loadRules', () => {
  it('reads the rules, in the order the page lists them, and leaves out what it cannot use', async () => {
    const client = { get: vi.fn(async () => [row('rightsize.storage'), null, row('rightsize.memory'), row('rightsize.future'), row('rightsize.cpu')]) }
    const out = await loadRules(client)
    expect(client.get).toHaveBeenCalledWith('rules')
    expect(out.map((r) => r.name)).toEqual(RULE_NAMES)
  })
  it('treats an answer that is not a list as none, and lets a failure through as the client words it', async () => {
    expect(await loadRules({ get: async () => null })).toEqual([])
    expect(await loadRules({ get: async () => ({ not: 'a list' }) })).toEqual([])
    await expect(loadRules({ get: async () => { throw new Error('The telemetry service is not reachable right now.') } })).rejects.toThrow('not reachable')
  })
})

describe('settingsProblems', () => {
  const ok = { enabled: true, percent: 50, days: 7, percentile: 95 }
  it('is empty for settings the hub takes, including at its limits', () => {
    expect(settingsProblems(ok)).toEqual({})
    for (const mod of [{ percent: 100 }, { percent: 0.5 }, { days: 1 }, { days: 30 }, { percentile: 50 }, { percentile: 100 }, { percentile: 99.5 }]) {
      expect(settingsProblems({ ...ok, ...mod })).toEqual({})
    }
  })
  it('says what is wrong, for the field that is wrong, as the hub would', () => {
    expect(Object.keys(settingsProblems({ ...ok, percent: 0 }))).toEqual(['percent'])
    expect(Object.keys(settingsProblems({ ...ok, percent: 101 }))).toEqual(['percent'])
    expect(Object.keys(settingsProblems({ ...ok, percent: -3 }))).toEqual(['percent'])
    expect(Object.keys(settingsProblems({ ...ok, days: 0 }))).toEqual(['days'])
    expect(Object.keys(settingsProblems({ ...ok, days: 31 }))).toEqual(['days'])
    expect(Object.keys(settingsProblems({ ...ok, days: 7.5 }))).toEqual(['days'])
    expect(Object.keys(settingsProblems({ ...ok, percentile: 49 }))).toEqual(['percentile'])
    expect(Object.keys(settingsProblems({ ...ok, percentile: 101 }))).toEqual(['percentile'])
    expect(Object.keys(settingsProblems({ enabled: true, percent: 0, days: 0, percentile: 0 })).sort()).toEqual(['days', 'percent', 'percentile'])
  })
  it('refuses what is empty, not a number, or text', () => {
    for (const v of ['', null, undefined, NaN, 'abc', Infinity]) {
      expect(Object.keys(settingsProblems({ ...ok, percent: v }))).toEqual(['percent'])
      expect(Object.keys(settingsProblems({ ...ok, days: v }))).toEqual(['days'])
      expect(Object.keys(settingsProblems({ ...ok, percentile: v }))).toEqual(['percentile'])
    }
  })
  it('puts the limits in the message so a person can fix it', () => {
    expect(settingsProblems({ ...ok, percent: 0 }).percent).toMatch(/0.*100/)
    expect(settingsProblems({ ...ok, days: 99 }).days).toMatch(/1.*30/)
    expect(settingsProblems({ ...ok, percentile: 10 }).percentile).toMatch(/50.*100/)
  })
})

describe('sameSettings', () => {
  it('compares what the hub would keep, and treats a number typed as text as the number', () => {
    const a = { enabled: true, percent: 50, days: 7, percentile: 95 }
    expect(sameSettings(a, { ...a })).toBe(true)
    expect(sameSettings(a, { ...a, percent: '50' })).toBe(true)
    for (const mod of [{ enabled: false }, { percent: 40 }, { days: 8 }, { percentile: 99 }]) expect(sameSettings(a, { ...a, ...mod })).toBe(false)
  })
})

describe('saveRule', () => {
  const rule = parseRule(row('rightsize.memory', { version: 2, saved: true }))
  const answer = row('rightsize.memory', { version: 3, saved: true, percent: 40, updated_by: 'sam', updated_at: '2026-11-17T10:00:00Z' })
  it('sends the whole rule, with the version it was based on, to its own path, with the manage key given for this call', async () => {
    const client = { send: vi.fn(async () => answer) }
    const out = await saveRule(client, rule, { enabled: true, percent: 40, days: 7, percentile: 95 }, { key: 'the-manage-key', actor: 'sam' })
    expect(client.send).toHaveBeenCalledWith('PUT', 'rules/rightsize.memory', {
      key: 'the-manage-key', actor: 'sam', body: { enabled: true, percent: 40, days: 7, percentile: 95, expected_version: 2 },
    })
    expect(out.version).toBe(3)
    expect(out.settings.percent).toBe(40)
    expect(out.updatedBy).toBe('sam')
  })
  it('sends a number that was typed as text as the number', async () => {
    const client = { send: vi.fn(async () => answer) }
    await saveRule(client, rule, { enabled: true, percent: '40', days: '7', percentile: '95' }, { key: 'k' })
    expect(client.send.mock.calls[0][2].body).toMatchObject({ percent: 40, days: 7, percentile: 95 })
  })
  it('refuses a rule it does not know, and settings the hub would refuse, before sending anything', async () => {
    const client = { send: vi.fn() }
    await expect(saveRule(client, { ...rule, name: '../incidents' }, rule.settings, { key: 'k' })).rejects.toThrow('not a rule')
    await expect(saveRule(client, rule, { ...rule.settings, percent: 0 }, { key: 'k' })).rejects.toThrow('percent')
    expect(client.send).not.toHaveBeenCalled()
  })
  it('refuses an answer that is not a rule, and lets a failure through as the client words it', async () => {
    await expect(saveRule({ send: async () => ({ nonsense: 1 }) }, rule, rule.settings, { key: 'k' })).rejects.toThrow('not a rule')
    await expect(saveRule({ send: async () => { throw new Error('the rule has changed since it was read: it is now at version 3') } }, rule, rule.settings, { key: 'k' })).rejects.toThrow('changed since')
  })
})
