// "Silent": an application that has stopped sending data, and why, as far as that can be said.
//
// The node-agent can only say that data stopped. What it cannot say is whether that was meant: a workload scaled down to zero, a workload
// that should be running and is not, and one that is running but not being seen all look the same from there. The cluster collector's inventory
// says what a workload is SET to run and how many are ready, which is the difference. Without it, all that can be said is "it stopped".
//
// Zero running instances is NOT a failure by itself, and is not judged as one: it is a fact that needs a reason.

/**
 * null when the application is sending data, or when nothing can be said about it; otherwise {cause, label, detail, tone}:
 *   scaled-to-zero  the inventory says it is set to run nothing                          (tone info)
 *   not-ready       it is set to run N and fewer are ready                               (tone bad)
 *   no-data         it is ready, and nothing arrives from it                              (tone caution)
 *   removed         it was seen in this window and is no longer in the cluster            (tone info)
 *   stopped         it was seen in this window and has stopped; there is no inventory    (tone caution)
 *
 * `agentAlive` says whether ANY application is sending: when none is, the node-agent (or the way to the hub) is the problem, and saying
 * that every application is silent would be wrong; the page's notice says it once. `app.workload` is its inventory entry, null when there is
 * none; `hasInventory` says whether there is an inventory at all, so that "not in it" can be told from "no inventory".
 */
export function silenceOf(app, { agentAlive, hasInventory = false }) {
  if (!agentAlive) return null
  if (app.instances && app.instances.running > 0) return null
  const wl = app.workload
  if (wl) {
    if (wl.kind === 'cronjob') return null // idle between runs: no data is how it normally is
    if (wl.desired === 0) return { cause: 'scaled-to-zero', tone: 'info', label: 'Scaled to zero', detail: 'It is set to run no replicas: scaled down, not failing.' }
    if (wl.ready < wl.desired) {
      const why = wl.reasons && wl.reasons.length ? `: ${wl.reasons.join(', ')}` : ''
      return { cause: 'not-ready', tone: 'bad', label: 'Not ready', detail: `${wl.ready} of ${wl.desired} ready${why}` }
    }
    return { cause: 'no-data', tone: 'caution', label: 'Ready, no data', detail: `${wl.ready} ready, and nothing arrives from them: is the node-agent on their nodes?` }
  }
  if (app.instances && app.instances.seen > 0) {
    return hasInventory
      ? { cause: 'removed', tone: 'info', label: 'Removed', detail: 'It sent data earlier in this window and is no longer in the cluster.' }
      : { cause: 'stopped', tone: 'caution', label: 'Stopped sending', detail: 'It sent data earlier in this window and has stopped. Without the cluster collector, whether that was meant cannot be said.' }
  }
  return null
}
