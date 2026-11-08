<script setup>
import { computed, inject, onMounted, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { WINDOWS } from '../api/services'
import { LOG_LIMITS, filterProblem, searchLogs, serviceNames } from '../api/search'
import { severityClass } from '../api/serviceStats'
import { dateTime, shortId } from '../format'

// Takes the client and session by injection so a test can hand it fakes.
const props = defineProps({ client: { type: Object, default: null }, session: { type: Object, default: null } })
const client = props.client || inject('client')
const session = props.session || inject('session')
const route = useRoute()
const router = useRouter()

const connected = computed(() => session.get())
const q = route.query
const pick = (v, ok, def) => (ok.includes(v) ? v : def)
const f = reactive({
  service: String(q.service || ''),
  severity: String(q.severity || ''),
  q: String(q.q || ''),
  traceId: String(q.trace_id || ''),
  seconds: pick(Number(q.window), WINDOWS.map((w) => w.seconds), WINDOWS[1].seconds),
  limit: pick(Number(q.limit), LOG_LIMITS, LOG_LIMITS[0]),
})
const rows = ref([])
const names = ref([])
const loading = ref(false)
const searched = ref(false)
const error = ref('')
const problem = computed(() => filterProblem(f))
const full = computed(() => searched.value && rows.value.length >= f.limit)
const entries = (o) => Object.entries(o || {}).sort(([a], [b]) => a.localeCompare(b))

function remember() {
  const query = {}
  if (f.service) query.service = f.service
  if (f.severity) query.severity = f.severity
  if (f.q) query.q = f.q
  if (f.traceId) query.trace_id = f.traceId
  if (f.seconds !== WINDOWS[1].seconds) query.window = String(f.seconds)
  if (f.limit !== LOG_LIMITS[0]) query.limit = String(f.limit)
  router.replace({ query })
}

async function search() {
  if (!connected.value || problem.value) return
  loading.value = true
  error.value = ''
  remember()
  try {
    rows.value = await searchLogs(client, f, { seconds: f.seconds })
  } catch (e) {
    error.value = e.message
    rows.value = []
  } finally {
    loading.value = false
    searched.value = true
  }
}

onMounted(() => {
  if (!connected.value) return
  serviceNames(client, { seconds: f.seconds }).then((n) => (names.value = n))
  search()
})
</script>

<template>
  <section class="panel">
    <h2>Logs</h2>
    <p v-if="!connected" class="help">Not connected. <router-link to="/">Connect with a project and its read key</router-link> first.</p>
    <template v-else>
      <p class="help">Log lines the node-agents collected, newest first.</p>
      <form class="filters" @submit.prevent="search">
        <div><label for="service">Service</label><input id="service" v-model="f.service" list="service-names" placeholder="any" autocomplete="off" /></div>
        <datalist id="service-names"><option v-for="n in names" :key="n" :value="n" /></datalist>
        <div><label for="severity">Severity (as logged)</label><input id="severity" v-model="f.severity" list="severities" placeholder="any" autocomplete="off" /></div>
        <datalist id="severities"><option value="ERROR" /><option value="WARN" /><option value="WARNING" /><option value="INFO" /><option value="DEBUG" /></datalist>
        <div><label for="text">Contains</label><input id="text" v-model="f.q" placeholder="any text" autocomplete="off" /></div>
        <div><label for="trace">Trace id</label><input id="trace" v-model="f.traceId" placeholder="32 hex characters" autocomplete="off" :aria-invalid="!!problem" /></div>
        <div><label for="window">Window</label>
          <select id="window" v-model.number="f.seconds"><option v-for="w in WINDOWS" :key="w.seconds" :value="w.seconds">Last {{ w.label }}</option></select></div>
        <div><label for="limit">Show</label>
          <select id="limit" v-model.number="f.limit"><option v-for="n in LOG_LIMITS" :key="n" :value="n">newest {{ n }}</option></select></div>
        <div><button type="submit" :disabled="loading || !!problem">{{ loading ? 'Searching…' : 'Search' }}</button></div>
      </form>
      <p v-if="problem" class="error" role="alert">{{ problem }}</p>
      <p v-if="error" class="error" role="alert">{{ error }} <router-link to="/">Check the connection.</router-link></p>
      <template v-else-if="searched">
        <p v-if="!rows.length && !loading" class="help">No log lines match in this window. Widen it, or loosen a filter.</p>
        <p v-if="full" class="warn" role="status">Showing the newest {{ f.limit }} lines; more match. Narrow the window or add a filter to see the rest.</p>
        <ul v-if="rows.length" class="log-lines">
          <li v-for="(l, i) in rows" :key="i" :data-sev="severityClass(l)">
            <div class="log-head">
              <time>{{ dateTime(l.time) }}</time>
              <span class="tag" :data-sev="severityClass(l)">{{ l.severity || '—' }}</span>
              <router-link class="name" :to="{ name: 'service', params: { name: l.service } }">{{ l.service }}</router-link>
              <router-link v-if="l.trace_id" class="name" :to="{ name: 'trace', params: { id: l.trace_id } }" :title="l.trace_id">trace {{ shortId(l.trace_id) }}</router-link>
            </div>
            <pre class="log-body">{{ l.body }}</pre>
            <details v-if="entries(l.log_attributes).length || entries(l.resource_attributes).length">
              <summary>Attributes</summary>
              <dl class="kv"><div v-for="[k, v] in [...entries(l.log_attributes), ...entries(l.resource_attributes)]" :key="k"><dt>{{ k }}</dt><dd class="name wrap">{{ v }}</dd></div></dl>
            </details>
          </li>
        </ul>
        <p v-if="rows.length" class="help count">{{ rows.length }} line{{ rows.length === 1 ? '' : 's' }}</p>
      </template>
    </template>
  </section>
</template>
