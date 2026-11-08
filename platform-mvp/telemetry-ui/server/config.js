// The alert service's settings, from the environment. Everything has a default that suits the chart, and a bad value stops the
// service at start-up with a message that says which setting and why, instead of running with something nobody chose.

import { DEFAULTS, cleanConfig } from '../src/alerts/rules.js'

function integer(env, name, def, { min = 1, max = Number.MAX_SAFE_INTEGER } = {}) {
  const raw = env[name]
  if (raw === undefined || raw === '') return def
  const n = Number(raw)
  if (!Number.isInteger(n) || n < min || n > max) throw new Error(`${name} must be a whole number from ${min} to ${max}, not "${raw}"`)
  return n
}

function url(env, name, def) {
  const raw = env[name]
  if (raw === undefined || raw === '') return def
  let u
  try {
    u = new URL(raw)
  } catch {
    throw new Error(`${name} is not a URL: "${raw}"`)
  }
  if (u.protocol !== 'http:' && u.protocol !== 'https:') throw new Error(`${name} must be an http or https URL`)
  return raw.replace(/\/+$/, '')
}

export function loadConfig(env = process.env) {
  let rules = {}
  if (env.RULES_JSON) {
    try {
      rules = JSON.parse(env.RULES_JSON)
    } catch (e) {
      throw new Error(`RULES_JSON is not valid JSON: ${e.message}`, { cause: e })
    }
    if (rules === null || typeof rules !== 'object' || Array.isArray(rules)) throw new Error('RULES_JSON must be an object of thresholds')
    const unknown = Object.keys(rules).filter((k) => !(k in DEFAULTS))
    if (unknown.length) throw new Error(`RULES_JSON has unknown threshold(s): ${unknown.join(', ')}; known: ${Object.keys(DEFAULTS).join(', ')}`)
  }
  return {
    listen: integer(env, 'PORT', 8081, { min: 1, max: 65535 }),
    readApi: url(env, 'READ_API', 'http://telemetry-ingest.monitoring.svc.cluster.local'),
    projectsFile: env.PROJECTS_FILE || '/etc/telemetry/projects/projects.json',
    stateFile: env.STATE_FILE || '',
    intervalSeconds: integer(env, 'INTERVAL_SECONDS', 60, { min: 10, max: 3600 }),
    alertmanagerUrl: url(env, 'ALERTMANAGER_URL', ''),
    thresholds: cleanConfig(rules),
  }
}
