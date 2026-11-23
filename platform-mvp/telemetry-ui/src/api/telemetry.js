// Whether the project's node-agents are reporting. The UI cannot look into a cluster; it can look at what the hub has stored, and the absence of
// that is the node-agent not being set up (not installed, not running, or not able to reach the hub).
//
//   none    the project has no metrics at all: nothing has ever arrived
//   other   there are metrics, but none of the node-agent's own (its names start with container_): something else is sending, the agent is not
//   stale   the node-agent has reported, but not in the last FRESH_SECONDS: it has stopped, or lost its way to the hub
//   ok      it is reporting
//
// A check that cannot be made (the read fails, or only the freshness probe fails) is never turned into one of the first three: saying the
// agent is missing when it could not be asked would send someone to fix what is not broken. The metric list failing throws, and the caller
// stays quiet (the page itself reports a failed read).

export const AGENT_PREFIX = 'container_'
export const FRESH_SECONDS = 600
// Metrics the agent reports on every scrape, in the order they are preferred as the probe of whether it is still reporting.
const PROBES = ['container_resources_memory_rss_bytes', 'container_net_tcp_listen_info', 'container_resources_cpu_usage_seconds_total']

export async function loadTelemetryStatus(client, { now = () => Date.now() } = {}) {
  const raw = await client.get('metrics')
  const names = Array.isArray(raw) ? raw.filter((n) => typeof n === 'string') : []
  if (!names.length) return { state: 'none', metrics: 0, agentMetrics: 0 }
  const agent = names.filter((n) => n.startsWith(AGENT_PREFIX))
  if (!agent.length) return { state: 'other', metrics: names.length, agentMetrics: 0 }

  const probe = PROBES.find((p) => agent.includes(p)) || agent[0]
  const to = Math.floor(now() / 1000)
  try {
    const data = await client.get('metrics/query_range', { name: probe, from: to - FRESH_SECONDS, to, step: 60 })
    const series = data && Array.isArray(data.series) ? data.series : []
    const fresh = series.some((s) => Array.isArray(s.points) && s.points.length > 0)
    if (!fresh && !(data && data.truncated)) return { state: 'stale', metrics: names.length, agentMetrics: agent.length }
  } catch {
    // the probe could not be made: not a finding
  }
  return { state: 'ok', metrics: names.length, agentMetrics: agent.length }
}

/** Text for each state: what is wrong, said in terms of what the person sees. */
export const NOTICES = {
  none: {
    title: (p) => `No telemetry has arrived for the project “${p}”`,
    lead: 'Nothing is stored for this project. The node-agent is most likely not installed in the cluster, or it cannot reach the hub. Without it there are no applications, service map, traces, logs or profiles.',
  },
  other: {
    title: (p) => `No node-agent metrics for the project “${p}”`,
    lead: 'Metrics are arriving, but none of them come from the node-agent (its metrics are named container_…). Without the node-agent there are no applications, service map, traces, logs or profiles.',
  },
  stale: {
    title: (p) => `The node-agent has stopped reporting for the project “${p}”`,
    lead: `The newest node-agent metric is more than ${FRESH_SECONDS / 60} minutes old. The agents may have stopped, or lost their connection to the hub.`,
  },
}

/** What to check, in the cluster that should be reporting. `where` is the command to run and `then` says how to read what it shows. */
export const STEPS = [
  {
    ask: 'Is the node-agent installed?',
    where: 'kubectl -n telemetry-agent get daemonset node-agent',
    then: 'Not found: it was never installed in this cluster. Turn it on in the spoke chart (nodeAgent.enabled=true, with the hub address in collectorURL, an image tag and the ingest key Secret).',
  },
  {
    ask: 'Is it running on every node?',
    where: 'kubectl -n telemetry-agent get pods -o wide',
    then: 'Pods that are not Running: describe one for the reason (a missing image tag, Pod Security blocking a privileged pod, a node taint).',
  },
  {
    ask: 'Can it reach the hub, with the right key?',
    where: 'kubectl -n telemetry-agent logs ds/node-agent --tail=50',
    then: '401: the key is not this project’s ingest key. Connection refused or a timeout: collectorURL is not reachable from this cluster’s pods (it must not be localhost, and a ClusterIP service on the hub is not reachable from another cluster).',
  },
]
