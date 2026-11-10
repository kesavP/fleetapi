<script setup>
import { computed, inject, onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import IncidentGraph from '../components/IncidentGraph.vue'
import { formatAge, dateTime } from '../format'
import { SLO_DEFAULTS } from '../incidents/detect'
import { LOOKBACKS, listIncidents, loadIncident } from '../incidents/load'
import { formatMs, issueSentence, segments } from '../incidents/narrative'
import { toAnalysisConfig, toAppConfig, toDetectConfig } from '../incidents/settings'

// One incident: what broke, and why, as far as the metrics, traces, logs and profiles can say. The words come from fixed sentence shapes
// over the findings (incidents/narrative.js), so every number in them can be traced.
const props = defineProps({ client: { type: Object, default: null }, session: { type: Object, default: null }, loaders: { type: Object, default: null }, settings: { type: Object, default: null } })
const client = props.client || inject('client')
const session = props.session || inject('session')
const settings = props.settings || inject('incidentSettings')
const list = (props.loaders && props.loaders.listIncidents) || listIncidents
const analyse = (props.loaders && props.loaders.loadIncident) || loadIncident
const route = useRoute()

const connected = computed(() => session.get())
const seconds = LOOKBACKS.find((w) => String(w.seconds) === String(route.query.window))?.seconds || LOOKBACKS[0].seconds
const state = ref(null) // {incident, findings, narrative, evidence, warnings}
const loading = ref(false)
const error = ref('')
const missing = ref(false)
const tab = ref('overview')
const details = ref(true)

async function load() {
  if (!connected.value) return
  loading.value = true
  error.value = ''
  missing.value = false
  try {
    const rules = settings.get()
    const { incidents } = await list(client, { seconds, config: toDetectConfig(rules), appConfig: toAppConfig(rules) })
    const incident = incidents.find((i) => i.id === route.params.id)
    if (!incident) {
      state.value = null
      missing.value = true
      return
    }
    state.value = await analyse(client, incident, { config: toAnalysisConfig(rules) })
  } catch (e) {
    state.value = null
    error.value = e.message
  } finally {
    loading.value = false
  }
}
onMounted(load)

const inc = computed(() => (state.value ? state.value.incident : null))
const when = (s) => dateTime(s * 1000)
const duration = computed(() => (inc.value ? formatAge((inc.value.end - inc.value.start) * 1000) : ''))
const ago = computed(() => (inc.value ? `${formatAge(Date.now() - inc.value.start * 1000)} ago` : ''))
const pct = (x) => (x == null ? '–' : `${(Math.round(x * 10000) / 100).toFixed(2).replace(/\.?0+$/, '')}%`)
const burn = (x) => (x == null ? '–' : String(Math.round(x)))
const sloRows = computed(() => {
  if (!inc.value) return []
  const s = inc.value.slo
  const text = (o) => `${Math.round(o * 10000) / 100}%`.replace(/\.0+%/, '%')
  return [
    s.availability && { ...s.availability, name: 'Availability', target: s.availability.objective, objective: `${text(s.availability.objective)} of requests should not fail` },
    s.latency && { ...s.latency, name: 'Latency', target: s.latency.objective, objective: `${text(s.latency.objective)} of requests should be served faster than ${formatMs(s.latencySeconds * 1000).replace(' ', '')}` },
  ].filter(Boolean)
})
const threshold = computed(() => (inc.value && inc.value.slo && inc.value.slo.burnThreshold) || SLO_DEFAULTS.burnThreshold)
const path = computed(() => (state.value ? state.value.narrative.path : []))
const nameOf = (id) => (state.value ? state.value.findings.nodes.find((n) => n.id === id)?.name || id : id)
const changes = computed(() => (state.value ? state.value.findings.issues.map(issueSentence) : []))
const time = (iso) => dateTime(iso)
</script>

<template>
  <div class="page-head">
    <div>
      <p class="crumb"><router-link to="/incidents">Incidents</router-link> › {{ route.params.id }}</p>
      <h1>{{ route.params.id }}</h1>
    </div>
  </div>
  <section class="panel inc">
    <p v-if="!connected" class="help">Not connected. <router-link to="/configuration/connection">Connect with a project and its read key</router-link> first.</p>
    <template v-else>
      <p v-if="error" class="error" role="alert">{{ error }} <router-link to="/configuration/connection">Check the connection.</router-link></p>
      <p v-else-if="missing" class="help" data-testid="missing">This incident is not in the last {{ LOOKBACKS.find((w) => w.seconds === seconds)?.label }}. It may be older, or the figures may have changed. <router-link to="/incidents">Back to the incidents.</router-link></p>
      <p v-else-if="loading" class="help" role="status">Comparing the incident with the hour before it…</p>

      <template v-if="state">
        <h2 class="inc-title" data-testid="title"><span class="inc-dot" aria-hidden="true">!</span><span><template v-for="(s, i) in segments(state.narrative.title)" :key="i"><code v-if="s.code">{{ s.text }}</code><template v-else>{{ s.text }}</template></template></span></h2>
        <p class="inc-meta">
          <span>Started: <strong>{{ when(inc.start) }}</strong> ({{ ago }})</span>
          <span v-if="!inc.open">Resolved: <strong>{{ when(inc.end) }}</strong></span><span v-else>Still ongoing</span>
          <span>Duration: <strong>{{ duration }}</strong></span>
          <span>Application: <router-link :to="{ name: 'application', params: { name: inc.app } }">{{ inc.label }}</router-link></span>
          <span>Analysis: <strong class="ok">Done</strong> <button type="button" class="link" :disabled="loading" aria-label="Analyse again" @click="load">↻</button></span>
        </p>
        <p v-for="w in state.warnings" :key="w" class="warn" role="note">{{ w }}</p>

        <table class="services slo" aria-label="Service level objectives">
          <thead><tr><th>Service level objective (SLO)</th><th>Objective</th><th>Compliance</th><th>Error budget burn rate</th></tr></thead>
          <tbody>
            <tr v-for="r in sloRows" :key="r.name">
              <td>{{ r.name }}</td>
              <td>{{ r.objective }}</td>
              <td :class="{ bad: r.compliance < r.target }">{{ pct(r.compliance) }}</td>
              <td>
                <span>1h: <b :class="{ bad: r.burn1h >= threshold }">{{ burn(r.burn1h) }}</b></span>
                <span> 5m: <b :class="{ bad: r.burn5m >= threshold }">{{ burn(r.burn5m) }}</b></span>
                <span class="muted"> threshold: {{ threshold }}</span>
              </td>
            </tr>
          </tbody>
        </table>

        <div class="tabs" role="tablist" aria-label="Incident">
          <button type="button" role="tab" class="tab" :class="{ active: tab === 'overview' }" :aria-selected="tab === 'overview'" @click="tab = 'overview'">Overview</button>
          <button type="button" role="tab" class="tab" :class="{ active: tab === 'traces' }" :aria-selected="tab === 'traces'" @click="tab = 'traces'">Traces</button>
        </div>

        <div v-if="tab === 'overview'" role="tabpanel">
          <h3>Root cause</h3>
          <p v-for="(p, n) in state.narrative.summary" :key="n" data-testid="summary"><template v-for="(s, i) in segments(p)" :key="i"><code v-if="s.code">{{ s.text }}</code><template v-else>{{ s.text }}</template></template></p>
          <button type="button" class="link" :aria-expanded="details" @click="details = !details">{{ details ? 'Show less details' : 'Show more details' }}</button>

          <template v-if="details">
            <IncidentGraph :nodes="state.findings.nodes" :edges="state.findings.edges" :app="inc.app" :cause="state.findings.rootCause" />
            <p v-if="state.narrative.propagation"><template v-for="(s, i) in segments(state.narrative.propagation)" :key="i"><code v-if="s.code">{{ s.text }}</code><template v-else>{{ s.text }}</template></template></p>
            <p v-if="path.length > 1" data-testid="path"><strong>Request path:</strong> <template v-for="(id, i) in path" :key="id"><template v-if="i"> → </template><code>{{ nameOf(id) }}</code></template></p>
            <template v-if="changes.length">
              <h4>What changed</h4>
              <ul class="changes"><li v-for="(c, n) in changes" :key="n"><template v-for="(s, i) in segments(c)" :key="i"><code v-if="s.code">{{ s.text }}</code><template v-else>{{ s.text }}</template></template></li></ul>
            </template>
            <p class="help">Compared with the {{ formatAge((state.windows.before[1] - state.windows.before[0]) * 1000) }} before the incident. Checked {{ state.findings.checked.applications }} application{{ state.findings.checked.applications === 1 ? '' : 's' }} and {{ state.findings.checked.calls }} call{{ state.findings.checked.calls === 1 ? '' : 's' }} on {{ state.findings.checked.signals.join(', ') }}.</p>
          </template>

          <template v-if="state.evidence.logs.length">
            <h3>Error logs</h3>
            <div v-for="(l, n) in state.evidence.logs" :key="n" class="logline"><small><code>{{ nameOf(l.service) }}</code> · {{ l.count }} line{{ l.count === 1 ? '' : 's' }}</small><pre>{{ l.sample }}</pre></div>
          </template>
          <template v-if="state.evidence.profile">
            <h3>Where <code>{{ nameOf(state.evidence.profile.service) }}</code> spends its CPU</h3>
            <table class="services functions" aria-label="Hottest functions">
              <thead><tr><th>Function</th><th>Self</th><th>Total</th></tr></thead>
              <tbody><tr v-for="f in state.evidence.profile.functions" :key="f.name"><td class="name">{{ f.name }}</td><td>{{ Math.round((100 * f.self) / state.evidence.profile.total) }}%</td><td>{{ Math.round((100 * f.total) / state.evidence.profile.total) }}%</td></tr></tbody>
            </table>
          </template>
        </div>

        <div v-else role="tabpanel">
          <template v-for="g in [{ key: 'failed', title: 'Failed requests', rows: state.evidence.traces.failed }, { key: 'slow', title: 'Slowest requests', rows: state.evidence.traces.slow }]" :key="g.key">
            <h3>{{ g.title }}</h3>
            <p v-if="!g.rows.length" class="help">None in the incident.</p>
            <table v-else class="services spans" :aria-label="g.title">
              <thead><tr><th>Time</th><th>Request</th><th>Status</th><th>Duration</th></tr></thead>
              <tbody>
                <tr v-for="r in g.rows" :key="r.traceId">
                  <td>{{ time(r.time) }}</td>
                  <td><router-link :to="{ name: 'trace', params: { id: r.traceId } }">{{ r.name }}</router-link></td>
                  <td>{{ r.status }}<template v-if="r.message"> · {{ r.message }}</template></td>
                  <td>{{ formatMs(r.durationMs) }}</td>
                </tr>
              </tbody>
            </table>
          </template>
        </div>
      </template>
    </template>
  </section>
</template>
