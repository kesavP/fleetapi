import { evaluate } from './rules.js'
import { loadMap } from '../map/load.js'

/**
 * One check of one project: read the metric list, look at the recent window and at the baseline that ends where it begins (in
 * parallel), and judge them. Returns {alerts, checked, skipped}, the conditions that hold now; turning them into alerts with a
 * lifecycle is reconcile's job (state.js). Throws if the data cannot be read, and the caller must NOT treat that as "no alerts":
 * not being able to look is not the same as nothing being wrong.
 *
 * Shared by the browser (alerts/store.js) and the service (server/evaluator.js), so a rule behaves the same in both.
 */
export async function checkProject(client, config, { now = () => Date.now(), load = loadMap } = {}) {
  const t = now()
  const present = new Set((await client.get('metrics')) || [])
  const [recent, baseline] = await Promise.all([
    load(client, { seconds: config.recentSeconds, now: () => t, present }),
    // the baseline ends where the recent window begins, so a service that stopped is not hidden in its own baseline
    load(client, { seconds: config.baselineSeconds, now: () => t - config.recentSeconds * 1000, present }),
  ])
  return { ...evaluate({ recent, baseline }, config), at: t }
}
