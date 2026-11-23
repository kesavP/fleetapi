import { mkdir, readFile, rename, writeFile } from 'node:fs/promises'
import { dirname } from 'node:path'
import { createClient } from '../src/api/client.js'
import { checkProject } from '../src/alerts/run.js'
import { reconcile } from '../src/alerts/state.js'
import { alertmanagerPayload, postToAlertmanager } from './notify.js'

// Evaluates every project on a timer and keeps their alerts: the same rules and the same lifecycle the browser runs
// (src/alerts), now with nobody looking. One project failing to be read never affects another, and a failed check never
// resolves anything (not being able to look is not the same as nothing being wrong).

const blank = () => ({ alerts: [], checked: 0, skipped: [], lastRun: 0, lastError: '', errorSince: 0, ok: 0, failed: 0, notify: { sent: 0, error: '' } })

export function createEvaluator({
  config,
  loadProjects,
  now = () => Date.now(),
  fetchImpl = (...a) => fetch(...a),
  check = checkProject,
  notify = postToAlertmanager,
  log = () => {},
  stateFile = config.stateFile,
}) {
  const intervalMs = config.intervalSeconds * 1000
  let projects = []
  let projectsError = ''
  const states = new Map()
  let ready = false
  let ticking = false

  async function restore() {
    // Known from the start, so a valid key gets "not checked yet" and not a 403 while the first check is still running.
    try {
      projects = loadProjects()
    } catch (e) {
      projectsError = e.message
    }
    if (!stateFile) return
    try {
      const saved = JSON.parse(await readFile(stateFile, 'utf8'))
      for (const [name, s] of Object.entries((saved && saved.projects) || {})) {
        if (Array.isArray(s.alerts)) states.set(name, { ...blank(), alerts: s.alerts, lastRun: Number(s.lastRun) || 0 })
      }
    } catch {
      // no file yet, or one that cannot be read: start clean
    }
  }

  async function persist() {
    if (!stateFile) return
    const data = { version: 1, projects: Object.fromEntries([...states].map(([n, s]) => [n, { alerts: s.alerts, lastRun: s.lastRun }])) }
    try {
      await mkdir(dirname(stateFile), { recursive: true })
      await writeFile(stateFile + '.tmp', JSON.stringify(data))
      await rename(stateFile + '.tmp', stateFile) // whole or not at all: a crash mid-write must not leave half a file
    } catch (e) {
      log({ level: 'warn', msg: 'could not save state', error: e.message })
    }
  }

  async function checkOne(p) {
    const st = states.get(p.name) || blank()
    states.set(p.name, st)
    const t = now()
    const client = createClient({ session: { get: () => ({ project: p.name, key: p.readKeys[0] }) }, base: config.readApi, fetchImpl })
    const before = new Map(st.alerts.map((a) => [a.key, a.status]))
    const wasHealthy = st.ok > 0 && !st.lastError // a quiet service logs nothing per check, so say once that it is looking
    try {
      const result = await check(client, config.thresholds, { now: () => t })
      st.alerts = reconcile(st.alerts, result.alerts, t)
      st.checked = result.checked
      st.skipped = result.skipped
      st.lastRun = t
      st.lastError = ''
      st.errorSince = 0
      st.ok++
      if (!wasHealthy) {
        log({ level: 'info', msg: 'check ok', project: p.name, services: result.checked, firing: st.alerts.filter((a) => a.status === 'firing').length, skipped: result.skipped.length })
      }
      for (const a of st.alerts) {
        if (before.get(a.key) !== a.status && (a.status === 'firing' || a.status === 'resolved')) {
          log({ level: 'info', msg: `alert ${a.status}`, project: p.name, rule: a.rule, service: a.service, severity: a.severity })
        }
      }
    } catch (e) {
      if (!st.lastError) st.errorSince = t
      st.lastError = e && e.message ? e.message : 'failed'
      st.failed++
      log({ level: 'warn', msg: 'check failed', project: p.name, error: st.lastError })
    }
    // Re-sent even when the check failed: what was firing is still firing as far as anyone knows, and Alertmanager lets go of an
    // alert that stops being sent.
    st.notify = await notify(config.alertmanagerUrl, alertmanagerPayload(p.name, st.alerts, t, intervalMs), fetchImpl)
    if (st.notify.error) log({ level: 'warn', msg: 'could not notify', project: p.name, error: st.notify.error })
  }

  async function tick() {
    if (ticking) return
    ticking = true
    try {
      try {
        projects = loadProjects()
        projectsError = ''
      } catch (e) {
        projectsError = e.message // keep the projects already known: a briefly unreadable file must not stop the checking
        log({ level: 'warn', msg: 'could not read the projects', error: e.message })
      }
      for (const name of [...states.keys()]) if (!projects.some((p) => p.name === name)) states.delete(name)
      await Promise.all(projects.map(checkOne))
      await persist()
    } finally {
      ticking = false
      ready = true
    }
  }

  return {
    restore,
    tick,
    ready: () => ready,
    projects: () => projects,
    projectsError: () => projectsError,
    /** What the UI is given for a project, or null for one that is not known. */
    snapshot(name) {
      const st = states.get(name)
      if (!st) return projects.some((p) => p.name === name) ? { ...blankSnapshot(config, intervalMs) } : null
      return {
        alerts: st.alerts,
        checked: st.checked,
        skipped: st.skipped,
        lastRun: st.lastRun,
        lastError: st.lastError,
        errorSince: st.errorSince,
        config: config.thresholds,
        intervalSeconds: config.intervalSeconds,
        evaluatedBy: 'server',
      }
    },
    /** Prometheus text exposition of what is firing and how the checking is going. */
    metrics() {
      const out = [
        '# HELP telemetry_alerts_firing Alerts firing, by project, rule and severity.',
        '# TYPE telemetry_alerts_firing gauge',
      ]
      for (const [name, st] of [...states].sort(([a], [b]) => a.localeCompare(b))) {
        const by = new Map()
        for (const a of st.alerts.filter((x) => x.status === 'firing')) {
          const k = `${a.rule}\n${a.severity}`
          by.set(k, (by.get(k) || 0) + 1)
        }
        for (const [k, n] of [...by].sort()) {
          const [rule, severity] = k.split('\n')
          out.push(`telemetry_alerts_firing{project="${name}",rule="${rule}",severity="${severity}"} ${n}`)
        }
      }
      const gauge = (metric, help, f) => {
        out.push(`# HELP ${metric} ${help}`, `# TYPE ${metric} gauge`)
        for (const [name, st] of [...states].sort(([a], [b]) => a.localeCompare(b))) out.push(`${metric}{project="${name}"} ${f(st)}`)
      }
      gauge('telemetry_alerts_last_check_timestamp_seconds', 'When the project was last checked successfully.', (s) => Math.floor(s.lastRun / 1000))
      gauge('telemetry_alerts_last_check_ok', '1 when the latest check could read the data, 0 when it could not.', (s) => (s.lastError ? 0 : s.lastRun ? 1 : 0))
      out.push('# HELP telemetry_alerts_checks_total Checks run, by project and result.', '# TYPE telemetry_alerts_checks_total counter')
      for (const [name, st] of [...states].sort(([a], [b]) => a.localeCompare(b))) {
        out.push(`telemetry_alerts_checks_total{project="${name}",result="ok"} ${st.ok}`, `telemetry_alerts_checks_total{project="${name}",result="error"} ${st.failed}`)
      }
      gauge('telemetry_alerts_notify_ok', '1 when the last post to Alertmanager worked or none was due, 0 when it failed.', (s) => (s.notify.error ? 0 : 1))
      return out.join('\n') + '\n'
    },
  }
}

function blankSnapshot(config, intervalMs) {
  return { alerts: [], checked: 0, skipped: [], lastRun: 0, lastError: '', errorSince: 0, config: config.thresholds, intervalSeconds: intervalMs / 1000, evaluatedBy: 'server' }
}
