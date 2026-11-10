import { reactive } from 'vue'
import { ANALYSIS_DEFAULTS } from './analyze.js'
import { SLO_DEFAULTS } from './detect.js'

// The rules the incident pages judge by, as the user sets them: the objectives, how fast an error budget may burn before it is an incident, what
// size of change counts, and which checks run. Kept in THIS BROWSER (localStorage, per project), like the application categories: they are the
// user's own way of looking, not data of the project, so another person or browser has its own. The read key cannot write anything to the
// service, and a shared store needs a write endpoint and a decision on who may change it; the interface here (get, set, reset) is small so
// that can replace the browser later without touching the pages.
//
// Values are kept in the units a person types (percent, minutes, milliseconds); toDetectConfig and toAnalysisConfig turn them into the
// fractions and seconds the detection and analysis use. Anything read back from storage is cleaned first: a value that is not a number in
// its range goes back to the default, so a hand-edited or old entry can never make the page misjudge.

export const KEY = 'fleet-ui.incidents.' // + the project
export const MAX_APPS = 100

// the field -> [label, minimum, maximum, unit]; the default is what the code uses when nothing is set
export const FIELDS = {
  'slo.availability': { label: 'Availability objective', min: 50, max: 99.999, unit: '% of requests should not fail', def: SLO_DEFAULTS.availability * 100 },
  'slo.latency': { label: 'Latency objective', min: 50, max: 99.999, unit: '% of requests should be fast enough', def: SLO_DEFAULTS.latency * 100 },
  'slo.latencyMs': { label: 'Fast enough means under', min: 10, max: 60000, unit: 'ms', def: SLO_DEFAULTS.latencySeconds * 1000 },
  'slo.burnThreshold': { label: 'Error budget burn rate that makes an incident', min: 1, max: 1000, unit: 'times the allowed rate', def: SLO_DEFAULTS.burnThreshold },
  'slo.shortMinutes': { label: 'Short window', min: 1, max: 60, unit: 'minutes', def: SLO_DEFAULTS.shortSeconds / 60 },
  'slo.longMinutes': { label: 'Long window', min: 5, max: 1440, unit: 'minutes', def: SLO_DEFAULTS.longSeconds / 60 },
  'slo.minRequests': { label: 'Fewest requests in a window to judge it', min: 1, max: 100000, unit: 'requests', def: SLO_DEFAULTS.minRequests },
  'slo.minMinutes': { label: 'Shortest incident', min: 1, max: 60, unit: 'minutes', def: SLO_DEFAULTS.minSeconds / 60 },
  'analysis.rttRatio': { label: 'Round trip counts as slower at', min: 1.1, max: 1000, unit: 'times before', def: ANALYSIS_DEFAULTS.rttRatio },
  'analysis.rttMinRiseMs': { label: '… and at least this much more', min: 0, max: 10000, unit: 'ms', def: ANALYSIS_DEFAULTS.rttMinRiseMs },
  'analysis.connRatio': { label: 'Connection time counts as slower at', min: 1.1, max: 1000, unit: 'times before', def: ANALYSIS_DEFAULTS.connRatio },
  'analysis.connMinRiseMs': { label: '… and at least this much more', min: 0, max: 10000, unit: 'ms', def: ANALYSIS_DEFAULTS.connMinRiseMs },
  'analysis.latencyRatio': { label: 'Latency counts as slower at', min: 1.1, max: 1000, unit: 'times before', def: ANALYSIS_DEFAULTS.latencyRatio },
  'analysis.latencyMinRiseMs': { label: '… and at least this much more', min: 0, max: 60000, unit: 'ms', def: ANALYSIS_DEFAULTS.latencyMinRiseMs },
  'analysis.errorPct': { label: 'Failing calls count at', min: 0.1, max: 100, unit: '% of calls', def: ANALYSIS_DEFAULTS.errorPct },
  'analysis.errorMinRisePct': { label: '… and at least this much more than before', min: 0, max: 100, unit: 'percentage points', def: ANALYSIS_DEFAULTS.errorMinRisePct },
  'analysis.cpuSharePct': { label: 'CPU counts as exhausted at', min: 10, max: 100, unit: '% of its limit', def: ANALYSIS_DEFAULTS.cpuShareOfLimit * 100 },
  'analysis.logRatio': { label: 'Error logs count at', min: 1, max: 1000, unit: 'times the rate before', def: ANALYSIS_DEFAULTS.logRatio },
  'analysis.logMinLines': { label: '… and at least', min: 1, max: 100000, unit: 'lines', def: ANALYSIS_DEFAULTS.logMinLines },
}

// what the analysis checks, in the words a person would use; each may be turned off
export const CHECKS = [
  { key: 'rtt', label: 'Network round trip between applications' },
  { key: 'connect', label: 'Time to open a connection' },
  { key: 'latency', label: 'Latency (of an application and of the calls it makes)' },
  { key: 'errors', label: 'Failed requests and calls' },
  { key: 'cpu', label: 'CPU running out' },
  { key: 'logs', label: 'Error log lines' },
  { key: 'restarts', label: 'Restarts' },
  { key: 'oom', label: 'Out-of-memory kills' },
]

const GROUPS = ['slo', 'analysis']
const split = (field) => field.split('.')

export function defaultSettings() {
  const s = { slo: {}, analysis: {}, checks: Object.fromEntries(CHECKS.map((c) => [c.key, true])), apps: {} }
  for (const [field, f] of Object.entries(FIELDS)) {
    const [g, k] = split(field)
    s[g][k] = f.def
  }
  return s
}

// 99.9 percent as 0.999, not 0.9990000000000001: objectives have at most three decimals, so rounding to nine places loses nothing
const frac = (percent) => Math.round(percent * 1e7) / 1e9
const num = (v) => (typeof v === 'number' ? v : typeof v === 'string' && v.trim() !== '' ? Number(v) : NaN)
const inRange = (v, f) => Number.isFinite(v) && v >= f.min && v <= f.max

/**
 * Settings read from anywhere (storage, a form) as safe settings: every number in its range, every unknown thing dropped. A value that is
 * not valid goes back to the default, and a per-application objective that is not valid is left out.
 */
export function cleanSettings(raw) {
  const out = defaultSettings()
  const r = raw && typeof raw === 'object' ? raw : {}
  for (const [field, f] of Object.entries(FIELDS)) {
    const [g, k] = split(field)
    const v = num(r[g] && typeof r[g] === 'object' ? r[g][k] : undefined)
    if (inRange(v, f)) out[g][k] = v
  }
  if (out.slo.longMinutes < out.slo.shortMinutes) out.slo.longMinutes = Math.max(out.slo.shortMinutes, FIELDS['slo.longMinutes'].def)
  if (r.checks && typeof r.checks === 'object') for (const c of CHECKS) if (typeof r.checks[c.key] === 'boolean') out.checks[c.key] = r.checks[c.key]
  if (r.apps && typeof r.apps === 'object') {
    for (const [id, o] of Object.entries(r.apps).slice(0, MAX_APPS)) {
      if (!id || id.length > 200 || !o || typeof o !== 'object') continue
      const entry = {}
      for (const k of ['availability', 'latency']) {
        const v = num(o[k])
        if (inRange(v, FIELDS[`slo.${k}`])) entry[k] = v
      }
      if (Object.keys(entry).length) out.apps[id] = entry
    }
  }
  return out
}

/** What is wrong with settings as typed, by field: {field: message}. Empty when they can be saved. */
export function validateSettings(s) {
  const errors = {}
  for (const [field, f] of Object.entries(FIELDS)) {
    const [g, k] = split(field)
    const v = num(s && s[g] ? s[g][k] : undefined)
    if (!Number.isFinite(v)) errors[field] = `${f.label}: enter a number.`
    else if (v < f.min || v > f.max) errors[field] = `${f.label}: between ${f.min} and ${f.max}.`
  }
  if (!errors['slo.shortMinutes'] && !errors['slo.longMinutes'] && num(s.slo.longMinutes) < num(s.slo.shortMinutes)) errors['slo.longMinutes'] = 'The long window must not be shorter than the short one.'
  for (const [id, o] of Object.entries((s && s.apps) || {})) {
    for (const k of ['availability', 'latency']) {
      if (o[k] === undefined || o[k] === null || o[k] === '') continue
      const v = num(o[k])
      const f = FIELDS[`slo.${k}`]
      if (!inRange(v, f)) errors[`apps.${id}.${k}`] = `${id}: the ${k} objective must be between ${f.min} and ${f.max}.`
    }
  }
  return errors
}

/** The settings that detection takes (listIncidents' `config`): fractions and seconds. */
export function toDetectConfig(s) {
  const c = cleanSettings(s).slo
  return {
    availability: frac(c.availability),
    latency: frac(c.latency),
    latencySeconds: c.latencyMs / 1000,
    burnThreshold: c.burnThreshold,
    shortSeconds: c.shortMinutes * 60,
    longSeconds: c.longMinutes * 60,
    minRequests: c.minRequests,
    minSeconds: c.minMinutes * 60,
  }
}

/** Per-application objectives, as detection takes them (listIncidents' `appConfig`): {applicationId: {availability?, latency?}} as fractions. */
export function toAppConfig(s) {
  const out = {}
  for (const [id, o] of Object.entries(cleanSettings(s).apps)) {
    out[id] = {}
    if (o.availability !== undefined) out[id].availability = frac(o.availability)
    if (o.latency !== undefined) out[id].latency = frac(o.latency)
  }
  return out
}

/** The settings that analysis takes (loadIncident's `config`): thresholds, and the kinds of issue that are turned off. */
export function toAnalysisConfig(s) {
  const c = cleanSettings(s)
  const a = c.analysis
  return {
    rttRatio: a.rttRatio, rttMinRiseMs: a.rttMinRiseMs,
    connRatio: a.connRatio, connMinRiseMs: a.connMinRiseMs,
    latencyRatio: a.latencyRatio, latencyMinRiseMs: a.latencyMinRiseMs,
    errorPct: a.errorPct, errorMinRisePct: a.errorMinRisePct,
    cpuShareOfLimit: frac(a.cpuSharePct),
    logRatio: a.logRatio, logMinLines: a.logMinLines,
    disabled: CHECKS.filter((k) => !c.checks[k.key]).map((k) => k.key),
  }
}

/** True when the settings are the defaults, and so nothing is being overridden. */
export function isDefault(s) {
  return JSON.stringify(cleanSettings(s)) === JSON.stringify(defaultSettings())
}

function browserStorage() {
  try {
    return window.localStorage
  } catch {
    return null
  }
}

/** The settings of the connected project, kept in this browser. `get()` is always valid settings, whatever was stored. */
export function createIncidentSettingsStore({ session, storage = browserStorage() } = {}) {
  const state = reactive({ project: '', settings: defaultSettings() })

  function load(project) {
    state.project = project
    state.settings = defaultSettings()
    if (!storage || !project) return
    try {
      const saved = JSON.parse(storage.getItem(KEY + project) || 'null')
      if (saved) state.settings = cleanSettings(saved)
    } catch {
      // unreadable: the defaults
    }
  }
  const sync = () => {
    const project = (session && session.get() ? session.get().project : '') || ''
    if (project !== state.project) load(project)
  }
  sync()
  if (session && session.subscribe) session.subscribe(sync)

  return {
    state,
    get: () => state.settings,
    /** Keeps the settings (cleaned). Returns false, and keeps nothing, when they are not valid. */
    set(settings) {
      if (Object.keys(validateSettings(settings)).length) return false
      state.settings = cleanSettings(settings)
      if (storage && state.project) {
        try {
          if (isDefault(state.settings)) storage.removeItem(KEY + state.project)
          else storage.setItem(KEY + state.project, JSON.stringify(state.settings))
        } catch {
          // storage refused (private mode, full): the settings last for this visit only
        }
      }
      return true
    },
    reset() {
      state.settings = defaultSettings()
      if (storage && state.project) {
        try {
          storage.removeItem(KEY + state.project)
        } catch {
          // nothing kept to remove
        }
      }
    },
  }
}

export { GROUPS }
