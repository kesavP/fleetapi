import { RULES } from '../src/alerts/rules.js'

// Telling Alertmanager. It is optional: the alerts are served to the UI and exported as metrics either way.
//
// Alertmanager keeps an alert alive only while it is re-sent, so every check re-sends each firing alert with an end time a few
// intervals ahead (if this service dies, they expire by themselves instead of firing for ever), and an alert that has just
// resolved is sent once with its real end time so the resolution is not delayed.

const NAMES = {
  'no-traffic': 'ServiceStoppedReceivingTraffic',
  refused: 'ServiceConnectionsFailing',
  errors: 'ServiceCallsFailing',
  'no-instances': 'ServiceNoInstances',
  'telemetry-gone': 'TelemetryMissing',
}

const iso = (ms) => new Date(ms).toISOString()

/** The body of POST /api/v2/alerts for one project, from its alerts at time `now` (ms). Pure. */
export function alertmanagerPayload(project, alerts, now, intervalMs, justResolvedSince = now - intervalMs) {
  const out = []
  for (const a of alerts) {
    if (a.status === 'firing') {
      out.push(build(project, a, a.firstSeen, now + 3 * intervalMs))
    } else if (a.status === 'resolved' && a.resolvedAt > justResolvedSince) {
      out.push(build(project, a, a.firstSeen, a.resolvedAt))
    }
  }
  return out
}

function build(project, a, startsAt, endsAt) {
  return {
    labels: { alertname: NAMES[a.rule] || a.rule, severity: a.severity, project, workload: a.service, service: a.label, rule: a.rule },
    annotations: { summary: a.summary, title: RULES[a.rule] || a.rule },
    startsAt: iso(startsAt),
    endsAt: iso(endsAt),
  }
}

/** Posts to Alertmanager; resolves to {sent, error}. Never throws: telling Alertmanager must not stop the checking. */
export async function postToAlertmanager(url, payload, fetchImpl = (...a) => fetch(...a), timeoutMs = 5000) {
  if (!url || payload.length === 0) return { sent: 0, error: '' }
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), timeoutMs)
  try {
    const res = await fetchImpl(`${url}/api/v2/alerts`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload), signal: controller.signal })
    if (!res.ok) return { sent: 0, error: `Alertmanager answered HTTP ${res.status}` }
    return { sent: payload.length, error: '' }
  } catch (e) {
    return { sent: 0, error: controller.signal.aborted ? 'Alertmanager did not answer in time' : `cannot reach Alertmanager (${e && e.cause && e.cause.code ? e.cause.code : 'network error'})` }
  } finally {
    clearTimeout(timer)
  }
}
