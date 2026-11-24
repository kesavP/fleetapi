import { describe, expect, it } from 'vitest'
import { CHECKS, FIELDS, KEY, cleanSettings, createIncidentSettingsStore, defaultSettings, isDefault, toAnalysisConfig, toAppConfig, toDetectConfig, validateSettings } from './settings'
import { ANALYSIS_DEFAULTS } from './analyze'
import { SLO_DEFAULTS } from './detect'

const memoryStorage = () => {
  const m = new Map()
  return { getItem: (k) => (m.has(k) ? m.get(k) : null), setItem: (k, v) => m.set(k, String(v)), removeItem: (k) => m.delete(k), m }
}
const sessionOf = (project) => { let p = project; const subs = []; return { get: () => (p ? { project: p, key: 'k' } : null), subscribe: (f) => { subs.push(f); return () => {} }, set: (x) => { p = x; subs.forEach((f) => f()) } } }

describe('defaults', () => {
  it('are what the detection and analysis use when nothing is set, so an untouched page behaves as before', () => {
    const d = defaultSettings()
    expect(toDetectConfig(d)).toEqual({ availability: SLO_DEFAULTS.availability, latency: SLO_DEFAULTS.latency, latencySeconds: SLO_DEFAULTS.latencySeconds, burnThreshold: SLO_DEFAULTS.burnThreshold, shortSeconds: SLO_DEFAULTS.shortSeconds, longSeconds: SLO_DEFAULTS.longSeconds, minRequests: SLO_DEFAULTS.minRequests, minSeconds: SLO_DEFAULTS.minSeconds })
    const a = toAnalysisConfig(d)
    for (const k of Object.keys(a)) if (k !== 'disabled') expect(a[k], k).toBeCloseTo(ANALYSIS_DEFAULTS[k], 9)
    expect(a.disabled).toEqual([])
    expect(toAppConfig(d)).toEqual({})
    expect(isDefault(d)).toBe(true)
  })
  it('have every field inside its own allowed range', () => {
    for (const [name, f] of Object.entries(FIELDS)) expect(f.def >= f.min && f.def <= f.max, name).toBe(true)
    expect(validateSettings(defaultSettings())).toEqual({})
  })
})

describe('cleanSettings', () => {
  it('keeps what is valid', () => {
    const s = cleanSettings({ slo: { availability: 99, latencyMs: 250, burnThreshold: 6 }, analysis: { rttRatio: 5 }, checks: { logs: false } })
    expect(s.slo).toMatchObject({ availability: 99, latencyMs: 250, burnThreshold: 6 })
    expect(s.analysis.rttRatio).toBe(5)
    expect(s.checks.logs).toBe(false)
    expect(s.checks.rtt).toBe(true)
  })
  it('puts a value that is not a number in its range back to the default, never to a guess', () => {
    const s = cleanSettings({ slo: { availability: 'abc', latency: 10, latencyMs: -5, burnThreshold: 1e9, minRequests: null, shortMinutes: NaN }, analysis: { errorPct: 0 } })
    expect(s).toEqual(defaultSettings())
  })
  it('accepts a number typed as text, as a form gives it', () => {
    expect(cleanSettings({ slo: { availability: '99.5' } }).slo.availability).toBe(99.5)
    expect(cleanSettings({ slo: { availability: '  ' } }).slo.availability).toBe(99.9)
  })
  it('survives anything stored', () => {
    for (const raw of [null, undefined, 5, 'x', [], { slo: 5 }, { checks: 'no' }, { apps: 7 }, { slo: null, analysis: [], checks: null, apps: null }]) expect(cleanSettings(raw)).toEqual(defaultSettings())
  })
  it('ignores a check it does not know and one that is not true or false', () => {
    const s = cleanSettings({ checks: { nope: false, rtt: 'no', cpu: false } })
    expect(Object.keys(s.checks)).toEqual(CHECKS.map((c) => c.key))
    expect(s.checks.rtt).toBe(true)
    expect(s.checks.cpu).toBe(false)
  })
  it('keeps the long window no shorter than the short one', () => {
    const s = cleanSettings({ slo: { shortMinutes: 30, longMinutes: 10 } })
    expect(s.slo.longMinutes).toBeGreaterThanOrEqual(s.slo.shortMinutes)
  })
  it('keeps valid per-application objectives, drops invalid ones and empty entries, and caps how many', () => {
    const s = cleanSettings({ apps: { '/k8s/shop/frontend': { availability: 99, latency: 'x' }, '/k8s/shop/cart': { availability: 10 }, '': { availability: 99 }, '/k8s/a': 3 } })
    expect(s.apps).toEqual({ '/k8s/shop/frontend': { availability: 99 } })
    const many = Object.fromEntries(Array.from({ length: 300 }, (_, i) => [`/k8s/ns/app${i}`, { availability: 99 }]))
    expect(Object.keys(cleanSettings({ apps: many }).apps).length).toBeLessThanOrEqual(100)
  })
  it('does not let a key like __proto__ reach anything', () => {
    cleanSettings(JSON.parse('{"apps":{"__proto__":{"availability":99}},"slo":{"__proto__":{"availability":1}}}'))
    expect({}.availability).toBeUndefined()
  })
})

describe('validateSettings', () => {
  it('is empty for settings that can be saved', () => {
    const s = defaultSettings()
    s.slo.availability = 99.5
    expect(validateSettings(s)).toEqual({})
  })
  it('names each field that is not a number or out of range', () => {
    const s = defaultSettings()
    s.slo.availability = 'abc'
    s.slo.latencyMs = 5
    s.analysis.errorPct = 101
    const e = validateSettings(s)
    expect(Object.keys(e).sort()).toEqual(['analysis.errorPct', 'slo.availability', 'slo.latencyMs'])
    expect(e['slo.availability']).toContain('enter a number')
    expect(e['slo.latencyMs']).toContain('between 10 and 60000')
  })
  it('refuses a long window shorter than the short one', () => {
    const s = defaultSettings()
    s.slo.shortMinutes = 30
    s.slo.longMinutes = 10
    expect(validateSettings(s)['slo.longMinutes']).toContain('must not be shorter')
  })
  it('checks per-application objectives, and lets one be left empty', () => {
    const s = defaultSettings()
    s.apps = { '/k8s/shop/frontend': { availability: 20, latency: '' }, '/k8s/shop/cart': { availability: null, latency: 99 } }
    expect(Object.keys(validateSettings(s))).toEqual(['apps./k8s/shop/frontend.availability'])
  })
})

describe('conversion to what detection and analysis take', () => {
  it('turns percents and minutes into fractions and seconds', () => {
    const s = defaultSettings()
    Object.assign(s.slo, { availability: 99, latency: 95, latencyMs: 250, shortMinutes: 10, longMinutes: 120, minMinutes: 3 })
    expect(toDetectConfig(s)).toMatchObject({ availability: 0.99, latency: 0.95, latencySeconds: 0.25, shortSeconds: 600, longSeconds: 7200, minSeconds: 180 })
  })
  it('gives per-application objectives as fractions', () => {
    const s = defaultSettings()
    s.apps = { '/k8s/shop/frontend': { availability: 99, latency: 95 }, '/k8s/shop/cart': { availability: 99.99 } }
    expect(toAppConfig(s)).toEqual({ '/k8s/shop/frontend': { availability: 0.99, latency: 0.95 }, '/k8s/shop/cart': { availability: 0.9999 } })
  })
  it('lists the checks that are turned off, and turns CPU percent into a share', () => {
    const s = defaultSettings()
    s.checks.logs = false
    s.checks.rtt = false
    s.analysis.cpuSharePct = 80
    const a = toAnalysisConfig(s)
    expect(a.disabled.sort()).toEqual(['logs', 'rtt'])
    expect(a.cpuShareOfLimit).toBe(0.8)
  })
  it('never passes on something invalid: it goes through the cleaning first', () => {
    const bad = { slo: { availability: 5, burnThreshold: 'x' } }
    expect(toDetectConfig(bad)).toEqual(toDetectConfig(defaultSettings()))
  })
})

describe('the store', () => {
  it('starts with the defaults, and keeps what is set for the connected project only', () => {
    const storage = memoryStorage()
    const session = sessionOf('gke')
    const store = createIncidentSettingsStore({ session, storage })
    expect(isDefault(store.get())).toBe(true)
    const s = defaultSettings()
    s.slo.availability = 99
    expect(store.set(s)).toBe(true)
    expect(JSON.parse(storage.m.get(KEY + 'gke')).slo.availability).toBe(99)
    expect(storage.m.has(KEY + 'dc1')).toBe(false)
    // a new store (a reload) reads it back
    expect(createIncidentSettingsStore({ session: sessionOf('gke'), storage }).get().slo.availability).toBe(99)
    // another project has its own
    expect(createIncidentSettingsStore({ session: sessionOf('dc1'), storage }).get().slo.availability).toBe(99.9)
  })
  it('follows a change of project', () => {
    const storage = memoryStorage()
    const session = sessionOf('gke')
    const store = createIncidentSettingsStore({ session, storage })
    const s = defaultSettings()
    s.slo.burnThreshold = 6
    store.set(s)
    session.set('dc1')
    expect(store.get().slo.burnThreshold).toBe(14.4)
    session.set('gke')
    expect(store.get().slo.burnThreshold).toBe(6)
  })
  it('keeps nothing and says so for settings that are not valid', () => {
    const storage = memoryStorage()
    const store = createIncidentSettingsStore({ session: sessionOf('gke'), storage })
    const s = defaultSettings()
    s.slo.availability = 3
    expect(store.set(s)).toBe(false)
    expect(storage.m.size).toBe(0)
    expect(isDefault(store.get())).toBe(true)
  })
  it('keeps nothing at all when the settings are the defaults, so there is nothing to carry', () => {
    const storage = memoryStorage()
    const store = createIncidentSettingsStore({ session: sessionOf('gke'), storage })
    const s = defaultSettings()
    s.slo.burnThreshold = 6
    store.set(s)
    store.set(defaultSettings())
    expect(storage.m.size).toBe(0)
  })
  it('resets to the defaults and forgets what was stored', () => {
    const storage = memoryStorage()
    const store = createIncidentSettingsStore({ session: sessionOf('gke'), storage })
    const s = defaultSettings()
    s.slo.burnThreshold = 6
    store.set(s)
    store.reset()
    expect(isDefault(store.get())).toBe(true)
    expect(storage.m.size).toBe(0)
  })
  it('reads defaults when what is stored is garbage, or cannot be read', () => {
    const storage = memoryStorage()
    storage.m.set(KEY + 'gke', '{not json')
    expect(isDefault(createIncidentSettingsStore({ session: sessionOf('gke'), storage }).get())).toBe(true)
    storage.m.set(KEY + 'gke', JSON.stringify({ slo: { availability: 'x', burnThreshold: 6 } }))
    expect(createIncidentSettingsStore({ session: sessionOf('gke'), storage }).get().slo.burnThreshold).toBe(6) // what is valid is kept
    const refusing = { getItem: () => { throw new Error('no') }, setItem: () => { throw new Error('no') }, removeItem: () => { throw new Error('no') } }
    const store = createIncidentSettingsStore({ session: sessionOf('gke'), storage: refusing })
    const s = defaultSettings()
    s.slo.burnThreshold = 6
    expect(store.set(s)).toBe(true) // works for this visit
    expect(store.get().slo.burnThreshold).toBe(6)
  })
  it('keeps no settings for no project', () => {
    const storage = memoryStorage()
    const store = createIncidentSettingsStore({ session: sessionOf(null), storage })
    const s = defaultSettings()
    s.slo.burnThreshold = 6
    store.set(s)
    expect(storage.m.size).toBe(0)
  })
})
