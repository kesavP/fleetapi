<script setup>
import { computed, inject, onMounted, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import IncidentTabs from '../components/IncidentTabs.vue'
import { dateTime } from '../format'
import { loadRecorded, SEVERITIES, STATES } from '../incidents/recorded'
import { duration, loadStats, STATS_DAYS } from '../incidents/mttr'

// The incidents the hub's detector (or a person, through the API) has recorded for the project: they have a state that people move on, and
// stay until they are closed. They are read here; changing one takes a manage key, which this page never holds.
const props = defineProps({ client: { type: Object, default: null }, session: { type: Object, default: null }, loaders: { type: Object, default: null } })
const client = props.client || inject('client')
const session = props.session || inject('session')
const load = (props.loaders && props.loaders.loadRecorded) || loadRecorded
const readStats = (props.loaders && props.loaders.loadStats) || loadStats
const route = useRoute()
const router = useRouter()

const ACTIVE = 'open,acknowledged,investigating,mitigated'
// What the "State" filter offers: the ones still being dealt with (the default), all of them, or one.
const STATE_CHOICES = [{ key: 'active', label: 'Not yet resolved', value: ACTIVE }, { key: 'all', label: 'Any state', value: '' }, ...STATES.map((s) => ({ key: s, label: s[0].toUpperCase() + s.slice(1), value: s }))]
const pick = (v, ok, def) => (ok.includes(v) ? v : def)

const q = route.query
const f = reactive({ state: pick(String(q.state || ''), STATE_CHOICES.map((c) => c.key), 'active'), severity: pick(String(q.severity || ''), SEVERITIES, '') })
const connected = computed(() => session.get())
const result = ref(null)
const loading = ref(false)
const error = ref('')

async function refresh() {
  if (!connected.value) return
  const query = {}
  if (f.state !== 'active') query.state = f.state
  if (f.severity) query.severity = f.severity
  router.replace({ query })
  loading.value = true
  error.value = ''
  try {
    result.value = await load(client, { state: STATE_CHOICES.find((c) => c.key === f.state).value, severity: f.severity })
  } catch (e) {
    result.value = null
    error.value = e.message
  } finally {
    loading.value = false
  }
}
// How long incidents take, over a window the person picks; read on its own, so the list does not wait for it nor fail with it.
const days = ref(pick(Number(q.days) || 7, STATS_DAYS, 7))
const stats = ref(null)
const statsError = ref('')
async function refreshStats() {
  if (!connected.value) return
  statsError.value = ''
  try {
    stats.value = await readStats(client, days.value)
  } catch (e) {
    stats.value = null
    statsError.value = e.message
  }
}
const SEV_ORDER = ['critical', 'warning', 'info']
onMounted(() => Promise.all([refresh(), refreshStats()]))
</script>

<template>
  <div class="page-head"><h1>Incidents</h1></div>
  <IncidentTabs />
  <section class="panel">
    <p v-if="!connected" class="help">Not connected. <router-link to="/configuration/connection">Connect with a project and its read key</router-link> first.</p>
    <template v-else>
      <p class="help" data-testid="about">
        Incidents the hub has recorded: opened by its detector when an application burns its error budget too fast, or by a person through the API. They stay until closed,
        and have a state, a timeline and evidence. This page only reads them; changing one takes a manage key.
      </p>
      <section class="mttr" data-testid="mttr" aria-label="How long incidents take">
        <div class="filters">
          <div><label for="days">Over the last</label>
            <select id="days" v-model.number="days" @change="refreshStats"><option v-for="d in STATS_DAYS" :key="d" :value="d">{{ d }} days</option></select></div>
        </div>
        <p v-if="statsError" class="warn" role="note" data-testid="stats-error">The figures could not be read: {{ statsError }}</p>
        <template v-else-if="stats">
          <div class="tiles mttr-tiles">
            <div class="tile" data-testid="tile-mttr" title="From the start of the trouble to its resolution; false positives and duplicates are left out">
              <span class="k">MTTR (median)</span><strong>{{ duration(stats.mttr.median) }}</strong>
              <span class="sub">p90 {{ duration(stats.mttr.p90) }} · {{ stats.mttr.count }} resolved</span>
            </div>
            <div class="tile" data-testid="tile-mtta" title="From the incident being opened to someone taking it on">
              <span class="k">MTTA (median)</span><strong>{{ duration(stats.mtta.median) }}</strong><span class="sub">{{ stats.mtta.count }} taken on</span>
            </div>
            <div class="tile" data-testid="tile-mttd" title="From the start of the trouble to the detector opening the incident">
              <span class="k">MTTD (median)</span><strong>{{ duration(stats.mttd.median) }}</strong><span class="sub">{{ stats.mttd.count }} detected</span>
            </div>
            <div class="tile" data-testid="tile-counts">
              <span class="k">Opened · still open</span><strong>{{ stats.opened }} <small>· {{ stats.stillOpen }}</small></strong>
              <span class="sub">{{ stats.notCounted }} false positive or duplicate<template v-if="stats.reopened"> · {{ stats.reopened }} reopened</template></span>
            </div>
          </div>
          <p v-if="Object.keys(stats.bySeverity).length" class="help" data-testid="by-severity">
            MTTR by severity:
            <template v-for="(sev, i) in SEV_ORDER.filter((x) => stats.bySeverity[x])" :key="sev">{{ i ? ' · ' : '' }}{{ sev }} {{ duration(stats.bySeverity[sev].median) }} ({{ stats.bySeverity[sev].count }})</template>
          </p>
          <p v-if="stats.partial" class="warn" role="note" data-testid="partial">Only the newest 500 incidents were looked at, and they do not reach back the whole window: older ones are missing from these figures.</p>
        </template>
      </section>

      <form class="filters" @submit.prevent="refresh">
        <div><label for="state">State</label>
          <select id="state" v-model="f.state"><option v-for="c in STATE_CHOICES" :key="c.key" :value="c.key">{{ c.label }}</option></select></div>
        <div><label for="severity">Severity</label>
          <select id="severity" v-model="f.severity"><option value="">Any</option><option v-for="s in SEVERITIES" :key="s" :value="s">{{ s[0].toUpperCase() + s.slice(1) }}</option></select></div>
        <div><button type="submit" :disabled="loading">{{ loading ? 'Loading…' : 'Refresh' }}</button></div>
      </form>

      <p v-if="error" class="error" role="alert">{{ error }} <router-link to="/configuration/connection">Check the connection.</router-link></p>
      <template v-else-if="result">
        <p v-if="result.skipped" class="warn" role="note">{{ result.skipped }} entr{{ result.skipped === 1 ? 'y' : 'ies' }} the service sent could not be read and {{ result.skipped === 1 ? 'is' : 'are' }} left out.</p>
        <p v-if="!result.incidents.length" class="help" data-testid="none">No recorded incidents match.</p>
        <table v-else class="services incidents" aria-label="Recorded incidents">
          <thead><tr><th>Incident</th><th>Title</th><th>Severity</th><th>State</th><th>Started</th><th>Last change</th></tr></thead>
          <tbody>
            <tr v-for="i in result.incidents" :key="i.id">
              <td><router-link :to="{ name: 'recorded-incident', params: { id: i.id } }" class="name">{{ i.id }}</router-link></td>
              <td>{{ i.title }}</td>
              <td><span class="pill" :class="i.severity">{{ i.severity }}</span></td>
              <td><span class="pill state">{{ i.state }}</span></td>
              <td>{{ dateTime(i.startedAt) }}</td>
              <td>{{ dateTime(i.updatedAt) }}</td>
            </tr>
          </tbody>
        </table>
      </template>
      <p v-else-if="loading" class="help" role="status">Reading the recorded incidents…</p>
    </template>
  </section>
</template>
