import { reactive } from 'vue'
import { DEFAULTS, cleanConfig } from './rules'
import { reconcile } from './state'
import { checkProject } from './run'
import { loadMap } from '../map/load'

// Keeps the alerts of the connected project, from one of two places:
//   - the alert service (server/), when it is deployed: it checks every project all the time, with or without anyone looking,
//     and this store only reads what it found;
//   - otherwise this page, which runs the same rules itself on a timer: only while the app is open in a tab, and kept for that
//     tab's session (sessionStorage, per project).
// The service is tried first on every check, so deploying it (or removing it) takes effect without a reload. Only "it is not
// there" (the proxy answers 502, or 404) means "check for yourself"; a refused key or an unreachable app is an error, not a
// reason to quietly start a second opinion.
//
// A failed evaluation never resolves anything: if the API cannot be read, an alert that was firing is still firing as far as
// anyone knows, and the page says it could not look. Absence of data is not the same as absence of a problem.

const KEY = 'fleet-ui.alerts.' //        + project: that project's alerts
const CONFIG_KEY = 'fleet-ui.alerts-config' // the thresholds: a preference of this tab, not of one project
export const DEFAULT_INTERVAL_MS = 60_000

function browserStorage() {
  try {
    return window.sessionStorage
  } catch {
    return null
  }
}

export function createAlertStore({
  client,
  session,
  storage = browserStorage(),
  now = () => Date.now(),
  load = loadMap,
  timers = { set: (f, ms) => setInterval(f, ms), clear: (t) => clearInterval(t) },
  intervalMs = DEFAULT_INTERVAL_MS,
} = {}) {
  const state = reactive({
    project: '',
    alerts: [],
    checked: 0,
    skipped: [],
    config: { ...DEFAULTS },
    lastRun: 0,
    lastError: '',
    errorSince: 0,
    running: false,
    source: '', //          'server' or 'browser': who did the last check
    serverConfig: null, //  the thresholds the service uses (read-only here)
    serverInterval: 0, //   how often it checks, in seconds
  })
  let timer = null
  let busy = false

  function persist() {
    if (!storage) return
    try {
      storage.setItem(CONFIG_KEY, JSON.stringify(state.config))
      if (state.project) storage.setItem(KEY + state.project, JSON.stringify({ alerts: state.alerts, lastRun: state.lastRun }))
    } catch {
      // storage refused (private mode, full): the alerts simply do not survive a reload
    }
  }
  function restore(project) {
    state.project = project
    state.alerts = []
    state.lastRun = 0
    state.lastError = ''
    state.errorSince = 0
    state.checked = 0
    state.skipped = []
    if (!storage || !project) return
    try {
      const saved = JSON.parse(storage.getItem(KEY + project) || 'null')
      if (saved && Array.isArray(saved.alerts)) {
        state.alerts = saved.alerts
        state.lastRun = Number(saved.lastRun) || 0
      }
    } catch {
      // unreadable: start clean
    }
  }

  if (storage) {
    try {
      state.config = cleanConfig(JSON.parse(storage.getItem(CONFIG_KEY) || 'null'))
    } catch {
      // unreadable: the defaults stay
    }
  }

  /** The alert service's answer, or null when it is not there (or is not what answered). Other failures are thrown. */
  async function fromServer() {
    try {
      const d = await client.get('alerts')
      return d && Array.isArray(d.alerts) ? d : null
    } catch (e) {
      if (e && (e.kind === 'unavailable' || e.status === 404)) return null
      throw e
    }
  }

  async function evaluateNow() {
    const s = session.get()
    if (!s) return
    if (s.project !== state.project) restore(s.project)
    if (busy) return
    busy = true
    state.running = true
    const t = now()
    const cfg = { ...state.config }
    try {
      const server = await fromServer()
      if (session.get() && session.get().project !== state.project) return
      if (server) {
        // What the service found is shown as it is; nothing is kept here (it keeps it), and thresholds are the chart's.
        state.source = 'server'
        state.alerts = server.alerts
        state.checked = server.checked || 0
        state.skipped = Array.isArray(server.skipped) ? server.skipped : []
        state.lastRun = server.lastRun || 0
        state.lastError = server.lastError || ''
        state.errorSince = server.errorSince || 0
        state.serverConfig = server.config || null
        state.serverInterval = server.intervalSeconds || 0
        return
      }
      if (state.source === 'server') restore(state.project) // the service went away: go back to what this tab had kept
      state.source = 'browser'
      const result = await checkProject(client, cfg, { now: () => t, load })
      if (session.get() && session.get().project === state.project) {
        state.alerts = reconcile(state.alerts, result.alerts, t)
        state.checked = result.checked
        state.skipped = result.skipped
        state.lastRun = t
        state.lastError = ''
        state.errorSince = 0
        persist()
      }
    } catch (e) {
      if (!state.lastError) state.errorSince = t
      state.lastError = e && e.message ? e.message : 'failed'
    } finally {
      busy = false
      state.running = false
    }
  }

  return {
    state,
    intervalMs,
    evaluate: evaluateNow,
    start() {
      if (timer) return
      timer = timers.set(evaluateNow, intervalMs)
      evaluateNow()
    },
    stop() {
      if (timer) timers.clear(timer)
      timer = null
    },
    /** Forget this project's alerts (on disconnect). */
    reset() {
      if (state.project && storage) {
        try {
          storage.removeItem(KEY + state.project)
        } catch {
          // nothing to clean
        }
      }
      restore('')
    },
    setConfig(partial) {
      state.config = cleanConfig(partial, state.config)
      persist()
    },
    resetConfig() {
      state.config = { ...DEFAULTS }
      persist()
    },
    firing: () => state.alerts.filter((a) => a.status === 'firing'),
  }
}
