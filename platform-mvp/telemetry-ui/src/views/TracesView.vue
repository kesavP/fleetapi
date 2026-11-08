<script setup>
import { computed, inject, onMounted, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { WINDOWS } from '../api/services'
import { SPAN_LIMITS, filterProblem, searchSpans, serviceNames } from '../api/search'
import { formatDuration } from '../api/serviceStats'
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
  name: String(q.name || ''),
  status: pick(String(q.status || ''), ['error', 'ok', 'unset'], ''),
  minDurationMs: String(q.min || ''),
  rootOnly: q.root === 'true',
  seconds: pick(Number(q.window), WINDOWS.map((w) => w.seconds), WINDOWS[1].seconds),
  limit: pick(Number(q.limit), SPAN_LIMITS, SPAN_LIMITS[0]),
})
const rows = ref([])
const names = ref([])
const loading = ref(false)
const searched = ref(false)
const error = ref('')
const problem = computed(() => filterProblem(f))
const full = computed(() => searched.value && rows.value.length >= f.limit)

// The filters live in the address, so a search can be shared, bookmarked and reached with the back button.
function remember() {
  const query = {}
  if (f.service) query.service = f.service
  if (f.name) query.name = f.name
  if (f.status) query.status = f.status
  if (f.minDurationMs) query.min = f.minDurationMs
  if (f.rootOnly) query.root = 'true'
  if (f.seconds !== WINDOWS[1].seconds) query.window = String(f.seconds)
  if (f.limit !== SPAN_LIMITS[0]) query.limit = String(f.limit)
  router.replace({ query })
}

async function search() {
  if (!connected.value || problem.value) return
  loading.value = true
  error.value = ''
  remember()
  try {
    rows.value = await searchSpans(client, f, { seconds: f.seconds })
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
    <h2>Traces</h2>
    <p v-if="!connected" class="help">Not connected. <router-link to="/">Connect with a project and its read key</router-link> first.</p>
    <template v-else>
      <p class="help">
        Spans the node-agents recorded, newest first. The node-agent records each request as its own span, so a span is usually a
        trace of one.
      </p>
      <form class="filters" @submit.prevent="search">
        <div><label for="service">Service</label><input id="service" v-model="f.service" list="service-names" placeholder="any" autocomplete="off" /></div>
        <datalist id="service-names"><option v-for="n in names" :key="n" :value="n" /></datalist>
        <div><label for="name">Span name (exact)</label><input id="name" v-model="f.name" placeholder="any" autocomplete="off" /></div>
        <div><label for="status">Status</label>
          <select id="status" v-model="f.status"><option value="">any</option><option value="error">error</option><option value="ok">ok</option><option value="unset">unset</option></select></div>
        <div><label for="min">Slower than (ms)</label><input id="min" v-model="f.minDurationMs" inputmode="numeric" placeholder="0" /></div>
        <div><label for="window">Window</label>
          <select id="window" v-model.number="f.seconds"><option v-for="w in WINDOWS" :key="w.seconds" :value="w.seconds">Last {{ w.label }}</option></select></div>
        <div><label for="limit">Show</label>
          <select id="limit" v-model.number="f.limit"><option v-for="n in SPAN_LIMITS" :key="n" :value="n">newest {{ n }}</option></select></div>
        <div class="check"><input id="root" v-model="f.rootOnly" type="checkbox" /><label for="root">Root spans only</label></div>
        <div><button type="submit" :disabled="loading || !!problem">{{ loading ? 'Searching…' : 'Search' }}</button></div>
      </form>
      <p v-if="problem" class="error" role="alert">{{ problem }}</p>
      <p v-if="error" class="error" role="alert">{{ error }} <router-link to="/">Check the connection.</router-link></p>
      <template v-else-if="searched">
        <p v-if="!rows.length && !loading" class="help">No spans match in this window. Widen it, or loosen a filter.</p>
        <p v-if="full" class="warn" role="status">Showing the newest {{ f.limit }} spans; more match. Narrow the window or add a filter to see the rest.</p>
        <table v-if="rows.length" class="services spans">
          <thead><tr><th>Time</th><th>Service</th><th>Name</th><th>Kind</th><th>Duration</th><th>Status</th><th>Trace</th></tr></thead>
          <tbody>
            <tr v-for="s in rows" :key="s.span_id">
              <td>{{ dateTime(s.time) }}</td>
              <td class="name"><router-link :to="{ name: 'service', params: { name: s.service } }">{{ s.service }}</router-link></td>
              <td class="name">{{ s.name }}</td>
              <td>{{ (s.kind || '').toLowerCase() }}</td>
              <td>{{ formatDuration(s.duration_ns) }}</td>
              <td :class="{ bad: s.status_code === 'ERROR' }">{{ (s.status_code || '').toLowerCase() }}</td>
              <td class="name"><router-link :to="{ name: 'trace', params: { id: s.trace_id } }" :title="s.trace_id">{{ shortId(s.trace_id) }}</router-link></td>
            </tr>
          </tbody>
        </table>
        <p v-if="rows.length" class="help count">{{ rows.length }} span{{ rows.length === 1 ? '' : 's' }}</p>
      </template>
    </template>
  </section>
</template>
