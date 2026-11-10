import { workloadOf } from '../map/workload.js'

// DNS lookups an application makes, from the node-agent's container_dns_requests_total{request_type, domain, status}: how many a second, how
// many fail, and for which names.
//
// "Failing" is a server that could not answer: servfail, refused, a malformed or unimplemented request. NXDOMAIN, a name that does not
// exist, is counted apart and is NOT a failure: on Kubernetes a lookup of a short name tries each search domain in turn and every miss is an
// NXDOMAIN, so a healthy application has plenty.

export const DNS_METRIC = 'container_dns_requests_total'
export const FAILED_STATUSES = new Set(['servfail', 'refused', 'format_error', 'not_implemented'])
export const MAX_DOMAINS = 10

/** [{labels, rate}] -> the lookups of one application (an application id), or null when it made none. */
export function summarizeDns(series, id) {
  const mine = (series || []).filter((s) => {
    const w = workloadOf(s.labels.container_id)
    return !!w && w.id === id
  })
  if (!mine.length) return null
  const by = new Map()
  let rps = 0
  let failedRps = 0
  let nxdomainRps = 0
  for (const { labels, rate } of mine) {
    const status = labels.status || ''
    const domain = labels.domain || '(unknown)'
    rps += rate
    const d = by.get(domain) || { domain, rps: 0, failedRps: 0, nxdomainRps: 0 }
    d.rps += rate
    if (FAILED_STATUSES.has(status)) {
      failedRps += rate
      d.failedRps += rate
    } else if (status === 'nxdomain') {
      nxdomainRps += rate
      d.nxdomainRps += rate
    }
    by.set(domain, d)
  }
  const domains = [...by.values()].sort((a, b) => b.failedRps - a.failedRps || b.rps - a.rps || a.domain.localeCompare(b.domain)).slice(0, MAX_DOMAINS)
  return { rps, failedRps, nxdomainRps, failedShare: rps > 0 ? failedRps / rps : 0, domains, domainCount: by.size }
}
