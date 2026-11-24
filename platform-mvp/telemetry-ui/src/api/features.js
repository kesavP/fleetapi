// Which optional features the hub has switched on (GET .../features): the UI shows a page only where the hub can serve it, so a page is never a dead link and
// a hub that has not switched a feature on is not asked for it. Anything that is not exactly true is off: a feature is never shown on a guess.
import { reactive } from 'vue'

/** {costs: boolean}. Lets a failure through. */
export async function loadFeatures(client) {
  const data = await client.get('features')
  return { costs: !!data && typeof data === 'object' && !Array.isArray(data) && data.costs === true }
}

/**
 * The features of the project the page is connected to, kept up to date: asked when connected and again when the project changes, off when disconnected or
 * when the hub cannot say. `state` is reactive ({costs, loaded}); `refresh()` asks again. An answer that arrives after the project has changed again is dropped.
 */
export function createFeatureStore({ client, session, load = loadFeatures }) {
  const state = reactive({ costs: false, loaded: false })
  let latest = 0
  async function refresh() {
    const mine = ++latest
    if (!session.get()) {
      state.costs = false
      state.loaded = false
      return
    }
    let features = { costs: false }
    try {
      features = await load(client)
    } catch {
      // the hub cannot say (no such feature, or not reachable): off
    }
    if (mine !== latest) return
    state.costs = features.costs === true
    state.loaded = true
  }
  session.subscribe(refresh)
  refresh()
  return { state, refresh }
}
