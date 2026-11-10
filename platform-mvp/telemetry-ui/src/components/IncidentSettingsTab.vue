<script setup>
import { computed, inject, onMounted, reactive, ref } from 'vue'
import { CHECKS, FIELDS, MAX_APPS, cleanSettings, defaultSettings, isDefault, validateSettings } from '../incidents/settings'

// The rules the Incidents pages judge by. Saved in THIS BROWSER for the connected project: another person, or this person in another
// browser, has their own. (The read key cannot change anything on the service, and sharing needs a decision on who may.)
const props = defineProps({ settings: { type: Object, default: null }, session: { type: Object, default: null }, client: { type: Object, default: null }, appCache: { type: Object, default: null } })
const store = props.settings || inject('incidentSettings')
const session = props.session || inject('session')
const client = props.client || inject('client', null)
const appCache = props.appCache || inject('appCache', null)

const connected = computed(() => session.get())
const copy = (s) => JSON.parse(JSON.stringify(s))
const draft = reactive(copy(store.get()))
const saved = ref(false)
const apps = ref([]) // [{id, label}] the project's applications, to pick one from

const errors = computed(() => validateSettings(draft))
const invalid = computed(() => Object.keys(errors.value).length > 0)
const dirty = computed(() => JSON.stringify(cleanSettings(draft)) !== JSON.stringify(store.get()) || invalid.value)
const canReset = computed(() => !isDefault(draft) || !isDefault(store.get()))
const fresh = () => (saved.value = false)

function replace(s) {
  const c = copy(s)
  for (const k of ['slo', 'analysis', 'checks', 'apps']) draft[k] = c[k]
}
function save() {
  if (invalid.value) return
  saved.value = store.set(draft)
  if (saved.value) replace(store.get())
}
function reset() {
  store.reset()
  replace(defaultSettings())
  saved.value = true
}

onMounted(async () => {
  if (!connected.value || !appCache || !client) return
  try {
    const r = await appCache.get(client, { project: connected.value.project, seconds: 3600 })
    apps.value = (r.apps || []).filter((a) => a.hasMetrics).map((a) => ({ id: a.id, label: a.label }))
  } catch {
    apps.value = [] // the list is a convenience: without it, overrides already set can still be changed
  }
})

const pick = ref('')
const free = computed(() => apps.value.filter((a) => !(a.id in draft.apps)))
const labelOf = (id) => (apps.value.find((a) => a.id === id) || { label: id }).label
const overrideCount = computed(() => Object.keys(draft.apps).length)
function addApp() {
  if (!pick.value || pick.value in draft.apps || overrideCount.value >= MAX_APPS) return
  draft.apps[pick.value] = { availability: '', latency: '' }
  pick.value = ''
  fresh()
}
function removeApp(id) {
  delete draft.apps[id]
  fresh()
}
const SECTIONS = [
  { title: 'Objectives', hint: 'What every application should deliver, unless it has its own below.', fields: ['availability', 'latency', 'latencyMs'] },
  { title: 'When it is an incident', hint: 'A burn rate of 1 uses the error budget exactly as fast as the objective allows. It is an incident when both windows burn at least this fast, so the long window says it matters and the short one that it is still happening.', fields: ['burnThreshold', 'shortMinutes', 'longMinutes', 'minRequests', 'minMinutes'] },
]
const ANALYSIS_ROWS = ['rttRatio', 'rttMinRiseMs', 'connRatio', 'connMinRiseMs', 'latencyRatio', 'latencyMinRiseMs', 'errorPct', 'errorMinRisePct', 'cpuSharePct', 'logRatio', 'logMinLines']
const field = (g, k) => FIELDS[`${g}.${k}`]
</script>

<template>
  <section class="inc-settings" aria-label="Incident rules">
    <p v-if="!connected" class="help">Not connected. <router-link to="/configuration/connection">Connect with a project and its read key</router-link> first.</p>
    <form v-else novalidate @submit.prevent="save">
      <p class="help" data-testid="where">These rules are kept in this browser, for the project <strong>{{ connected.project }}</strong>. Someone else, or you in another browser, sees the defaults until they set their own.</p>

      <div v-for="sec in SECTIONS" :key="sec.title" class="panel">
        <h2>{{ sec.title }}</h2>
        <p class="help">{{ sec.hint }}</p>
        <div v-for="k in sec.fields" :key="k" class="rule">
          <label :for="`slo-${k}`">{{ field('slo', k).label }}</label>
          <span class="rule-input"><input :id="`slo-${k}`" v-model.number="draft.slo[k]" type="number" step="any" :min="field('slo', k).min" :max="field('slo', k).max" :aria-invalid="!!errors[`slo.${k}`]" :aria-describedby="`slo-${k}-e`" @input="fresh" /> <span class="unit">{{ field('slo', k).unit }}</span></span>
          <small :id="`slo-${k}-e`" class="bad" role="alert">{{ errors[`slo.${k}`] }}</small>
        </div>
      </div>

      <div class="panel">
        <h2>Applications with their own objectives</h2>
        <p class="help">An application that should be held to more, or less, than the rest. Leave a box empty to use the objective above.</p>
        <table v-if="overrideCount" class="services overrides" aria-label="Applications with their own objectives">
          <thead><tr><th>Application</th><th>Availability %</th><th>Latency %</th><th></th></tr></thead>
          <tbody>
            <tr v-for="(o, id) in draft.apps" :key="id">
              <td>{{ labelOf(id) }}</td>
              <td><input v-model.number="o.availability" type="number" step="any" :aria-label="`${labelOf(id)} availability objective`" :aria-invalid="!!errors[`apps.${id}.availability`]" @input="fresh" /><small class="bad" role="alert">{{ errors[`apps.${id}.availability`] }}</small></td>
              <td><input v-model.number="o.latency" type="number" step="any" :aria-label="`${labelOf(id)} latency objective`" :aria-invalid="!!errors[`apps.${id}.latency`]" @input="fresh" /><small class="bad" role="alert">{{ errors[`apps.${id}.latency`] }}</small></td>
              <td><button type="button" class="secondary" :aria-label="`Remove ${labelOf(id)}`" @click="removeApp(id)">Remove</button></td>
            </tr>
          </tbody>
        </table>
        <div class="add-app">
          <label for="pick-app">Add an application</label>
          <select id="pick-app" v-model="pick" :disabled="!free.length || overrideCount >= MAX_APPS"><option value="">{{ free.length ? 'Choose…' : 'No other application found' }}</option><option v-for="a in free" :key="a.id" :value="a.id">{{ a.label }}</option></select>
          <button type="button" class="secondary" :disabled="!pick" @click="addApp">Add</button>
        </div>
      </div>

      <div class="panel">
        <h2>What counts as a change</h2>
        <p class="help">The analysis compares the incident with the hour before it. Something counts only when it is both several times what it was and at least this much more, so a small number doubling is not an event.</p>
        <div v-for="k in ANALYSIS_ROWS" :key="k" class="rule">
          <label :for="`analysis-${k}`">{{ field('analysis', k).label }}</label>
          <span class="rule-input"><input :id="`analysis-${k}`" v-model.number="draft.analysis[k]" type="number" step="any" :min="field('analysis', k).min" :max="field('analysis', k).max" :aria-invalid="!!errors[`analysis.${k}`]" :aria-describedby="`analysis-${k}-e`" @input="fresh" /> <span class="unit">{{ field('analysis', k).unit }}</span></span>
          <small :id="`analysis-${k}-e`" class="bad" role="alert">{{ errors[`analysis.${k}`] }}</small>
        </div>
      </div>

      <div class="panel">
        <h2>What is checked</h2>
        <p class="help">A check that is turned off is left out of the analysis completely: it is not listed, and it cannot be named as the cause.</p>
        <label v-for="c in CHECKS" :key="c.key" class="check"><input v-model="draft.checks[c.key]" type="checkbox" @change="fresh" /> {{ c.label }}</label>
      </div>

      <div class="actions">
        <button type="submit" :disabled="!dirty || invalid">Save</button>
        <button type="button" class="secondary" :disabled="!canReset" @click="reset">Reset to the defaults</button>
        <span class="status" role="status"><template v-if="saved">Saved in this browser.</template><template v-else-if="invalid" ><span class="bad">Some values are not valid.</span></template></span>
      </div>
    </form>
  </section>
</template>
