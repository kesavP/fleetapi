<script setup>
import { computed, inject, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { formatAge, dateTime } from '../format'
import { LOOKBACKS, listIncidents } from '../incidents/load'
import { isDefault, toAppConfig, toDetectConfig } from '../incidents/settings'

// Periods in which an application used its error budget up too fast: what it broke, when, and for how long. Each opens its analysis.
const props = defineProps({ client: { type: Object, default: null }, session: { type: Object, default: null }, loaders: { type: Object, default: null }, settings: { type: Object, default: null } })
const client = props.client || inject('client')
const session = props.session || inject('session')
const settings = props.settings || inject('incidentSettings')
const list = (props.loaders && props.loaders.listIncidents) || listIncidents
const route = useRoute()
const router = useRouter()

const connected = computed(() => session.get())
const seconds = ref(LOOKBACKS.find((w) => String(w.seconds) === String(route.query.window))?.seconds || LOOKBACKS[0].seconds)
const result = ref(null)
const loading = ref(false)
const error = ref('')

async function load() {
  if (!connected.value) return
  loading.value = true
  error.value = ''
  try {
    const s = settings.get()
    result.value = await list(client, { seconds: seconds.value, config: toDetectConfig(s), appConfig: toAppConfig(s) })
  } catch (e) {
    result.value = null
    error.value = e.message
  } finally {
    loading.value = false
  }
}
onMounted(load)
watch(seconds, () => {
  router.replace({ query: { ...route.query, window: String(seconds.value) } })
  load()
})

const rules = computed(() => {
  const s = settings.get()
  return { ...s.slo, custom: !isDefault(s), overrides: Object.keys(s.apps).length }
})
const kinds = (i) => i.kind.map((k) => (k === 'availability' ? 'Availability' : 'Latency'))
const duration = (i) => formatAge((i.end - i.start) * 1000)
const started = (i) => dateTime(i.start * 1000)
</script>

<template>
  <div class="page-head"><h1>Incidents</h1></div>
  <section class="panel">
    <p v-if="!connected" class="help">Not connected. <router-link to="/configuration/connection">Connect with a project and its read key</router-link> first.</p>
    <template v-else>
      <p class="help" data-testid="rules">
        Times when an application served too many failed or slow requests, judged against its objectives ({{ rules.availability }}% of requests should not fail, and {{ rules.latency }}% should be served in under {{ rules.latencyMs }} ms<template v-if="rules.overrides">; {{ rules.overrides }} application{{ rules.overrides === 1 ? ' has' : 's have' }} its own</template>).
        Open one to see what changed, and the likely cause.
        <router-link to="/configuration/incidents">{{ rules.custom ? 'These are your settings; change them' : 'Change the rules' }}</router-link>.
      </p>
      <form class="filters" @submit.prevent="load">
        <div><label for="window">Look back</label>
          <select id="window" v-model.number="seconds"><option v-for="w in LOOKBACKS" :key="w.seconds" :value="w.seconds">{{ w.label }}</option></select></div>
        <div><button type="submit" :disabled="loading">{{ loading ? 'Loading…' : 'Refresh' }}</button></div>
      </form>

      <p v-if="error" class="error" role="alert">{{ error }} <router-link to="/configuration/connection">Check the connection.</router-link></p>
      <template v-else-if="result">
        <p v-for="w in result.warnings" :key="w" class="warn" role="note">{{ w }}</p>
        <p v-if="!result.hasData" class="help">This project has no inbound request metrics, so there is nothing to judge. Are the node-agents sending them?</p>
        <p v-else-if="!result.incidents.length" class="help" data-testid="none">No incidents in the last {{ LOOKBACKS.find((w) => w.seconds === seconds)?.label }}.</p>
        <table v-else class="services incidents" aria-label="Incidents">
          <thead><tr><th>Incident</th><th>Application</th><th>Started</th><th>Duration</th><th>Objective broken</th><th>Status</th></tr></thead>
          <tbody>
            <tr v-for="i in result.incidents" :key="i.id">
              <td><router-link :to="{ name: 'incident', params: { id: i.id }, query: { window: String(seconds) } }" class="name">{{ i.id }}</router-link></td>
              <td><router-link :to="{ name: 'application', params: { name: i.app } }">{{ i.label }}</router-link></td>
              <td>{{ started(i) }}</td>
              <td>{{ duration(i) }}</td>
              <td><span v-for="k in kinds(i)" :key="k" class="pill critical">{{ k }}</span></td>
              <td>{{ i.open ? 'Ongoing' : 'Resolved' }}</td>
            </tr>
          </tbody>
        </table>
      </template>
      <p v-else-if="loading" class="help" role="status">Looking through the request metrics…</p>
    </template>
  </section>
</template>
