<script setup>
import { computed, inject, onMounted, reactive, ref, watch } from 'vue'
import TelemetryNotice from '../components/TelemetryNotice.vue'
import { useRoute, useRouter } from 'vue-router'
import CategoryFilter from '../components/CategoryFilter.vue'
import NamespacePicker from '../components/NamespacePicker.vue'
import { createCategoryStore } from '../categories'
import { WINDOWS } from '../api/services'
import { STATUSES, alertLevels, loadApplications, namespacesOf, statusOf } from '../api/applications'
import { formatBytes } from '../format'

// Takes the client, session and alert store by injection so a test can hand it fakes.
const props = defineProps({ client: { type: Object, default: null }, session: { type: Object, default: null }, alerts: { type: Object, default: null }, categories: { type: Object, default: null }, appCache: { type: Object, default: null } })
const client = props.client || inject('client')
const session = props.session || inject('session')
const alerts = props.alerts || inject('alerts', null)
const store = props.categories || inject('categories', null) || createCategoryStore({ session, storage: null })
const appCache = props.appCache || inject('appCache', null)
const route = useRoute()
const router = useRouter()

const connected = computed(() => session.get())
const PAGE_SIZES = [25, 50, 100]

// The choices live in the address, so a view can be shared, bookmarked and reached with the back button.
const q = route.query
const list = (v) => (v ? String(v).split(',').filter(Boolean) : [])
const f = reactive({
  seconds: [0, ...WINDOWS.map((w) => w.seconds)].includes(Number(q.window)) && Number(q.window) ? Number(q.window) : WINDOWS[1].seconds,
  search: String(q.q || ''),
  namespaces: list(q.ns),
  categories: list(q.cat).filter((c) => store.all().includes(c)).length ? list(q.cat).filter((c) => store.all().includes(c)) : ['application'],
  statuses: list(q.status).filter((s) => STATUSES.some((x) => x.key === s)),
  pageSize: PAGE_SIZES.includes(Number(q.rows)) ? Number(q.rows) : 50,
})
const page = ref(0)

const data = ref(null)
const loading = ref(false)
const error = ref('')

const levels = computed(() => alertLevels(alerts ? alerts.state.alerts : []))
// An application's category is where its namespace puts it, or where the user moved it to (see categories.js).
const apps = computed(() => (data.value ? data.value.apps.map((a) => ({ ...a, category: store.categoryFor(a.id, a.category), status: statusOf(a, levels.value.get(a.id)) })) : []))
const inCategory = computed(() => apps.value.filter((a) => f.categories.includes(a.category)))
const namespaces = computed(() => namespacesOf(inCategory.value))
// What is in scope before the status chips: category, namespace choice and search. The chips count and then narrow this.
// A namespace chosen earlier but no longer on offer (its kind was switched off) is not a choice any more: the picker shows "all" for it,
// so the table must too.
const chosen = computed(() => f.namespaces.filter((n) => namespaces.value.some((x) => x.name === n)))
const scoped = computed(() => {
  const term = f.search.trim().toLowerCase()
  return inCategory.value.filter(
    (a) => (!chosen.value.length || chosen.value.includes(a.namespace)) && (!term || a.name.toLowerCase().includes(term) || a.namespace.toLowerCase().includes(term)),
  )
})
const counts = computed(() => Object.fromEntries(STATUSES.map((s) => [s.key, scoped.value.filter((a) => a.status === s.key).length])))
const RANK = Object.fromEntries(STATUSES.map((s, i) => [s.key, i]))
const rows = computed(() =>
  scoped.value
    .filter((a) => !f.statuses.length || f.statuses.includes(a.status))
    .sort((a, b) => RANK[a.status] - RANK[b.status] || a.namespace.localeCompare(b.namespace) || a.name.localeCompare(b.name)),
)
const pages = computed(() => Math.max(1, Math.ceil(rows.value.length / f.pageSize)))
const visible = computed(() => rows.value.slice(page.value * f.pageSize, (page.value + 1) * f.pageSize))
const several = computed(() => new Set(rows.value.map((a) => a.namespace)).size > 1)

watch([() => f.search, () => f.namespaces.join(), () => f.categories.join(), () => f.statuses.join(), () => f.pageSize], () => {
  page.value = 0
  remember()
})
function remember() {
  const query = {}
  if (f.seconds !== WINDOWS[1].seconds) query.window = String(f.seconds)
  if (f.search.trim()) query.q = f.search.trim()
  if (f.namespaces.length) query.ns = f.namespaces.join(',')
  if (f.categories.join() !== 'application') query.cat = f.categories.join(',')
  if (f.statuses.length) query.status = f.statuses.join(',')
  if (f.pageSize !== 50) query.rows = String(f.pageSize)
  router.replace({ query })
}

async function load(force = false) {
  if (!connected.value) return
  loading.value = true
  error.value = ''
  remember()
  try {
    // Through the short cache when there is one (the Map's hover menu reads the same data); Refresh reads afresh.
    data.value = await (appCache
      ? appCache.get(client, { project: connected.value.project, seconds: f.seconds, force: force === true })
      : loadApplications(client, { seconds: f.seconds }))
  } catch (e) {
    error.value = e.message
    data.value = null
  } finally {
    loading.value = false
  }
}
onMounted(load)

const toggleStatus = (key) => (f.statuses = f.statuses.includes(key) ? f.statuses.filter((s) => s !== key) : [...f.statuses, key])
// A category made from here is shown at once: making one and not seeing it would look like nothing happened.
function addCategory(name) {
  const made = store.add(name)
  if (made && !f.categories.includes(made)) f.categories = [...f.categories, made]
}

const num = (x) => (x >= 100 ? String(Math.round(x)) : x >= 10 ? x.toFixed(1) : x.toFixed(2))
const cores = (c) => (c == null ? '–' : c >= 10 ? c.toFixed(1) : String(Math.round(c * 1000) / 1000))
const pct = (x) => `${Math.round(x * 10) / 10}%`
const label = (key) => STATUSES.find((s) => s.key === key).label

// Instances: what the application is SET to run and how many are ready, when the cluster collector says (the truth about a scale-down);
// otherwise what the node-agent sees running. Zero running is not painted as a failure by itself: the Silent status says what it is.
const instText = (a) => (a.workload ? `${a.workload.ready}/${a.workload.desired}` : a.instances ? `${a.instances.running}/${a.instances.seen}` : '–')
const instTitle = (a) => (a.workload ? 'ready / set to run, from the cluster' : a.instances ? 'running / seen, from the node-agent' : null)
const instClass = (a) =>
  a.workload
    ? { bad: a.workload.desired > 0 && a.workload.ready === 0, caution: a.workload.ready > 0 && a.workload.ready < a.workload.desired }
    : { caution: !!a.instances && a.instances.running > 0 && a.instances.running < a.instances.seen }
</script>

<template>
  <div class="page-head">
    <h1>Applications</h1>
    <div class="head-right">
      <label for="window" class="sr-only">Time range</label>
      <select id="window" v-model.number="f.seconds" @change="load">
        <option v-for="w in WINDOWS" :key="w.seconds" :value="w.seconds">Last {{ w.label }}</option>
      </select>
      <button type="button" class="secondary" :disabled="loading" @click="load(true)">{{ loading ? 'Loading…' : 'Refresh' }}</button>
    </div>
  </div>

  <section class="panel flush">
    <TelemetryNotice />
    <p v-if="!connected" class="help pad">Not connected. <router-link to="/connection">Connect with a project and its read key</router-link> first.</p>
    <template v-else>
      <div class="filter-row">
        <div class="search-box">
          <svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" aria-hidden="true"><circle cx="11" cy="11" r="6" /><path d="M16 16l4 4" /></svg>
          <input v-model="f.search" type="search" placeholder="search" aria-label="Search applications" autocomplete="off" />
        </div>
        <NamespacePicker v-model="f.namespaces" :namespaces="namespaces" />
        <CategoryFilter v-model="f.categories" :categories="store.all()" :validate="store.problem" @add="addCategory" />
      </div>

      <div class="chips" role="group" aria-label="Filter by status">
        <button v-for="s in STATUSES" :key="s.key" type="button" class="chip" :class="[s.key, { on: f.statuses.includes(s.key) }]" :aria-pressed="f.statuses.includes(s.key)" :data-status="s.key" @click="toggleStatus(s.key)">
          <span class="chip-count">{{ counts[s.key] }}</span> {{ s.label }}
        </button>
      </div>

      <p v-if="error" class="error pad" role="alert">{{ error }} <router-link to="/connection">Check the connection.</router-link></p>
      <template v-else-if="data">
        <ul v-if="data.warnings.length" class="warn-list" role="status"><li v-for="w in data.warnings" :key="w">{{ w }}</li></ul>
        <p v-if="!apps.length && !loading" class="help pad" data-testid="none">
          No applications found in this window.<template v-if="!data.hasInventory"> The project has no container metrics yet: are the node-agents sending?</template>
        </p>
        <p v-else-if="!inCategory.length" class="help pad" data-testid="hidden">
          Every application is in a namespace kind that is switched off ({{ apps.length }}). Tick control-plane or monitoring above.
        </p>
        <p v-else-if="!rows.length" class="help pad" data-testid="no-match">No application matches these filters.</p>

        <div v-if="visible.length" class="table-wrap">
          <table class="apps">
            <thead>
              <tr>
                <th>Application</th><th>Type</th><th class="num">Traffic</th><th class="num">Errors</th><th class="num">Upstreams</th><th class="num">Instances</th>
                <th class="num">Restarts</th><th class="num">CPU</th><th class="num">Memory</th><th>Logs</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="a in visible" :key="a.id" :class="`st-${a.status}`" :data-status="a.status">
                <td class="app-cell" :title="label(a.status)">
                  <router-link :to="{ name: 'application', params: { name: a.id } }">{{ a.name }}</router-link>
                  <small v-if="several" class="muted ns">{{ a.namespace }}</small>
                  <small v-if="a.silence" class="silence" :class="`tone-${a.silence.tone}`" :title="a.silence.detail" data-testid="silence">{{ a.silence.label }}<template v-if="a.silence.cause === 'not-ready'"> · {{ a.workload.ready }}/{{ a.workload.desired }}</template></small>
                </td>
                <td>{{ a.type || '–' }}</td>
                <td class="num">{{ a.traffic ? `${num(a.traffic.rps)}/s` : '–' }}</td>
                <td class="num" :class="{ bad: a.traffic && a.traffic.errPct >= 5, caution: a.traffic && a.traffic.errPct >= 1 && a.traffic.errPct < 5 }">{{ a.traffic ? pct(a.traffic.errPct) : '–' }}</td>
                <td class="num" :class="{ caution: a.upstreams && a.upstreams.ok < a.upstreams.total }">{{ a.upstreams ? `${a.upstreams.ok}/${a.upstreams.total}` : '–' }}</td>
                <td class="num" :class="instClass(a)" :title="instTitle(a)">{{ instText(a) }}</td>
                <td class="num" :class="{ caution: a.restarts > 0 }">{{ a.restarts == null ? '–' : a.restarts }}</td>
                <td class="num" :title="a.cpuLimit ? `limit ${cores(a.cpuLimit)} cores` : null">{{ cores(a.cpuCores) }}</td>
                <td class="num" :title="a.memoryLimit ? `limit ${formatBytes(a.memoryLimit)}` : null">{{ a.memoryBytes == null ? '–' : formatBytes(a.memoryBytes) }}</td>
                <td>
                  <router-link v-if="a.uniqueErrors" class="log-link" :to="{ name: 'logs', query: { service: a.id, severity: 'ERROR' } }">{{ a.uniqueErrors }} unique {{ a.uniqueErrors === 1 ? 'error' : 'errors' }}</router-link>
                  <template v-else>–</template>
                </td>
              </tr>
            </tbody>
          </table>
        </div>

        <div v-if="rows.length" class="table-foot">
          <span class="muted" data-testid="count">{{ rows.length }} of {{ apps.length }} application{{ apps.length === 1 ? '' : 's' }}</span>
          <span class="pager">
            <label for="rows" class="muted">Rows per page:</label>
            <select id="rows" v-model.number="f.pageSize"><option v-for="n in PAGE_SIZES" :key="n" :value="n">{{ n }}</option></select>
            <span class="muted">{{ page * f.pageSize + 1 }}–{{ Math.min(rows.length, (page + 1) * f.pageSize) }} of {{ rows.length }}</span>
            <button type="button" class="icon-btn" aria-label="Previous page" :disabled="page === 0" @click="page--">‹</button>
            <button type="button" class="icon-btn" aria-label="Next page" :disabled="page >= pages - 1" @click="page++">›</button>
          </span>
        </div>
      </template>
    </template>
  </section>
</template>
