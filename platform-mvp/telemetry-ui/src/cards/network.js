import { defineCard, mine } from './registry'
import { duration, sum } from './format'
import { formatBytes } from '../format'

// What the application sends and receives over TCP, and how far away its peers are: bytes a second from the node-agent's
// container_net_tcp_bytes_* counters, and the round-trip time to each peer address from container_net_latency_seconds.

const RTT_CAUTION = 0.1
const RTT_BAD = 0.5
const bytesPerSecond = (x) => `${formatBytes(x)}/s`
const peerOf = (labels) => labels.destination || labels.actual_destination || '(unknown)'

export const network = defineCard({
  id: 'network',
  title: 'Network',
  metrics: {
    sent: { metric: 'container_net_tcp_bytes_sent_total', kind: 'rate' },
    received: { metric: 'container_net_tcp_bytes_received_total', kind: 'rate' },
    rtt: { metric: 'container_net_latency_seconds', kind: 'mean', optional: true },
  },
  build(series, { id }) {
    const [sent, received, rtt] = ['sent', 'received', 'rtt'].map((k) => mine(series[k], id))
    if (![sent, received, rtt].some((l) => l.length)) return null
    const rows = []
    if (sent.length) rows.push({ label: 'Sent', value: bytesPerSecond(sum(sent, (s) => s.rate)), level: undefined })
    if (received.length) rows.push({ label: 'Received', value: bytesPerSecond(sum(received, (s) => s.rate)), level: undefined })
    const peers = new Map()
    for (const s of [...sent, ...received]) peers.set(peerOf(s.labels), (peers.get(peerOf(s.labels)) || 0) + s.rate)
    if (peers.size > 1) {
      const [peer, bytes] = [...peers].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))[0]
      rows.push({ label: 'Busiest peer', value: `${peer} (${bytesPerSecond(bytes)})`, level: undefined })
    }
    if (rtt.length) {
      const mean = sum(rtt, (s) => s.value) / rtt.length
      rows.push({ label: 'Round trip (mean)', value: duration(mean), level: mean >= RTT_BAD ? 'bad' : mean >= RTT_CAUTION ? 'caution' : undefined })
      if (rtt.length > 1) {
        const slow = rtt.reduce((a, b) => (b.value > a.value ? b : a))
        rows.push({ label: 'Slowest peer', value: `${slow.labels.destination_ip || '(unknown)'} (${duration(slow.value)})`, level: undefined })
      }
    }
    return { rows }
  },
})
