import { loadService } from '../api/services'
import { loadServiceMetrics } from '../api/serviceMetrics'

/**
 * What the report needs about one alert: the service's spans and logs and its calls and resources. Each part is read on its own;
 * one failing leaves the rest, and `errors` says which, so the report can say what is missing instead of leaving it out.
 * An alert on the whole project has no service to read.
 */
export async function gatherContext(client, alert, { project, seconds = 1800, metricsSeconds = 600, now = () => Date.now() } = {}) {
  const t = now()
  const base = { project, now: t, spans: [], logs: [], traffic: null, resources: null, errors: {}, limit: 1000 }
  if (alert.service === '*') return base

  const [svc, met] = await Promise.allSettled([
    loadService(client, alert.service, { seconds, now: () => t }),
    loadServiceMetrics(client, alert.service, { seconds: metricsSeconds, now: () => t }),
  ])
  if (svc.status === 'fulfilled') {
    base.spans = svc.value.spans
    base.logs = svc.value.logs
    for (const [k, v] of Object.entries(svc.value.errors)) if (k !== 'profiles') base.errors[k] = v
  } else {
    base.errors.spans = base.errors.logs = svc.reason && svc.reason.message ? svc.reason.message : 'failed'
  }
  if (met.status === 'fulfilled') {
    base.traffic = met.value.traffic
    base.resources = met.value.resources
  } else {
    base.errors.metrics = met.reason && met.reason.message ? met.reason.message : 'failed'
  }
  return base
}
