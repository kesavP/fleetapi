import { defineCard, mine } from './registry'
import { duration, percent, perSecond, sum } from './format'

// How the application's outbound TCP connections go: how many are made, how many fail, how long one takes to set up, how many are
// open, how many segments are retransmitted. All from the node-agent's container_net_tcp_* series. Setup time is the TCP handshake:
// it does not include TLS or the first byte of a response.

const FAIL_CAUTION = 0.01
const FAIL_BAD = 0.05

export const connections = defineCard({
  id: 'connections',
  title: 'Connections',
  metrics: {
    connects: { metric: 'container_net_tcp_successful_connects_total', kind: 'rate' },
    failed: { metric: 'container_net_tcp_failed_connects_total', kind: 'rate' },
    time: { metric: 'container_net_tcp_connection_time_seconds_total', kind: 'rate', optional: true },
    active: { metric: 'container_net_tcp_active_connections', kind: 'gauge', optional: true },
    retransmits: { metric: 'container_net_tcp_retransmits_total', kind: 'rate', optional: true },
  },
  build(series, { id }) {
    const [connects, failed, time, active, retransmits] = ['connects', 'failed', 'time', 'active', 'retransmits'].map((k) => mine(series[k], id))
    if (![connects, failed, time, active, retransmits].some((l) => l.length)) return null
    const rows = []
    const made = sum(connects, (s) => s.rate)
    if (connects.length) rows.push({ label: 'Connects', value: perSecond(made), level: undefined })
    if (failed.length) {
      const lost = sum(failed, (s) => s.rate)
      const share = made + lost > 0 ? lost / (made + lost) : 0
      rows.push({
        label: 'Failed',
        value: `${perSecond(lost)} (${percent(share)})`,
        level: share >= FAIL_BAD ? 'bad' : share >= FAIL_CAUTION ? 'caution' : undefined,
      })
    }
    if (connects.length && time.length && made > 0) rows.push({ label: 'Setup time (mean)', value: duration(sum(time, (s) => s.rate) / made), level: undefined })
    if (active.length) rows.push({ label: 'Open connections', value: String(Math.round(sum(active, (s) => s.value))), level: undefined })
    if (retransmits.length) rows.push({ label: 'Retransmits', value: perSecond(sum(retransmits, (s) => s.rate)), level: undefined })
    return { rows }
  },
})
