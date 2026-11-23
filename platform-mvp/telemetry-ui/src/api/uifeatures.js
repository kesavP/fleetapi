// Which pages THIS UI deployment offers, read from /ui-features.json, which the UI's own web server answers from its environment (UI_COSTS, set by the
// chart's telemetryUi.costs.enabled). It does not ask the hub: a page can be offered or withdrawn by the UI alone. Anything that is not exactly true is
// off, and so is a file that cannot be read (a development server has none).
import { reactive } from 'vue'

/** {costs: boolean}. Lets a failure through. */
export async function loadUiFeatures(fetchFn = (...a) => window.fetch(...a), url = `${import.meta.env.BASE_URL}ui-features.json`) {
  const res = await fetchFn(url, { cache: 'no-store', headers: { Accept: 'application/json' } })
  if (!res.ok) throw new Error(`ui-features.json: ${res.status}`)
  const data = await res.json()
  return { costs: !!data && typeof data === 'object' && !Array.isArray(data) && data.costs === true }
}

/** `state` is reactive ({costs, loaded}); `ready` resolves once the file has been read (or failed, which is "off"). */
export function createUiFeatures({ load = loadUiFeatures } = {}) {
  const state = reactive({ costs: false, loaded: false })
  const ready = (async () => {
    try {
      state.costs = (await load()).costs === true
    } catch {
      state.costs = false
    }
    state.loaded = true
  })()
  return { state, ready }
}
