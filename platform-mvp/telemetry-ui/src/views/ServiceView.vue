<script setup>
import { computed, inject, onMounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { WINDOWS, loadService } from '../api/services'
import { hasMetrics, loadServiceMetrics } from '../api/serviceMetrics'
import { formatDuration, severityClass, summarizeLogs, summarizeSpans } from '../api/serviceStats'

// Takes the client and session by injection so a test can hand it fakes.
const props = defineProps({ client: { type: Object, default: null }, session: { type: Object, default: null } })
const client = props.client || inject('client')
const session = props.session || inject('session')
const route = useRoute()

const name = computed(() => String(route.params.name || ''))
const connected = computed(() => session.get())
const seconds = ref(WINDOWS[1].seconds)
const data = ref(null)
const loading = ref(false)
const metrics = ref(null)
const metricsError = ref('')

const spans = computed(() => (data.value ? summarizeSpans(data.value.spans, { from: data.value.from, to: data.value.to }) : null))
const logs = computed(() => (data.value ? summarizeLogs(data.value.logs) : null))
const errors = computed(() => (data.value ? data.value.errors : {}))
const failed = computed(() => Object.keys(errors.value))
const allFailed = computed(() => failed.value.length === 3)
const nothing = computed(
  () => data.value && !failed.value.length && !data.value.spans.length && !data.value.logs.length && !data.value.profiles.length,
)

const GROUP_LABEL = { server: 'Served (SERVER spans)', client: 'Calls out (CLIENT spans)', other: 'Other spans' }
const groups = computed(() => (spans.value ? Object.entries(spans.value.groups) : []))
const peak = computed(() => (spans.value ? Math.max(1, ...spans.value.timeline.map((b) => b.count)) : 1))
const recentSpans = computed(() => (data.value ? data.value.spans.slice(0, 15) : []))
const recentErrors = computed(() => {
  if (!data.value) return []
  const s = data.value.spans.filter((x) => x.status_code === 'ERROR').map((x) => ({ time: x.time, text: `${x.name}${x.status_message ? ': ' + x.status_message : ''}`, kind: 'span' }))
  const l = data.value.logs.filter((x) => severityClass(x) === 'error').map((x) => ({ time: x.time, text: x.body, kind: 'log' }))
  return [...s, ...l].sort((a, b) => Date.parse(b.time) - Date.parse(a.time)).slice(0, 10)
})
const recentLogs = computed(() => (data.value ? data.value.logs.slice(0, 15) : []))

const metricsApply = computed(() => hasMetrics(name.value))
const res = computed(() => (metrics.value ? metrics.value.resources : null))
const bytes = (b) => {
  if (b == null) return '–'
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let i = 0
  while (b >= 1024 && i < units.length - 1) {
    b /= 1024
    i++
  }
  return `${b >= 100 || i === 0 ? Math.round(b) : Math.round(b * 10) / 10} ${units[i]}`
}
const cores = (c) => (c == null ? '–' : c >= 10 ? c.toFixed(1) : c.toFixed(3).replace(/0+$/, '').replace(/\.$/, '') || '0')
const perSec = (x) => (x >= 100 ? Math.round(x) : x >= 10 ? x.toFixed(1) : x.toFixed(2))
const edgeRate = (e) => (e.rps != null ? `${perSec(e.rps)} ${e.unit}` : `${perSec(e.connPerSec)} conn/s`)
const pct = (x) => `${Math.round(x * 1000) / 10}%`
const clock = (iso) => new Date(iso).toLocaleTimeString()
const span = (ms) => {
  const m = Math.round(ms / 60000)
  return m >= 120 ? `${Math.round(m / 60)} hours` : `${m} minutes`
}
const shortId = (id) => (id ? id.slice(0, 8) : '')

async function load() {
  if (!connected.value || !name.value) return
  loading.value = true
  const wanted = name.value
  const metricsLoad = metricsApply.value
    ? loadServiceMetrics(client, wanted, { seconds: seconds.value })
        .then((m) => ((metrics.value = m), (metricsError.value = '')))
        .catch((e) => ((metrics.value = null), (metricsError.value = e.message)))
    : ((metrics.value = null), (metricsError.value = ''), Promise.resolve())
  try {
    data.value = await loadService(client, wanted, { seconds: seconds.value })
  } finally {
    await metricsLoad
    loading.value = false
  }
}

onMounted(load)
watch(name, load)
</script>

<template>
  <div class="page-head">
    <div><p class="crumb"><router-link to="/applications">← Applications</router-link></p><h1 class="svc-title">{{ name }}</h1></div>
  </div>
  <section class="panel">
    <p v-if="!connected" class="help">
      Not connected. <router-link to="/connection">Connect with a project and its read key</router-link> first.
    </p>
    <template v-else>
      <div class="toolbar">
        <label for="window">Window</label>
        <select id="window" v-model.number="seconds" @change="load">
          <option v-for="w in WINDOWS" :key="w.seconds" :value="w.seconds">Last {{ w.label }}</option>
        </select>
        <button type="button" class="secondary" :disabled="loading" @click="load">{{ loading ? 'Loading…' : 'Refresh' }}</button>
      </div>

      <p v-if="allFailed" class="error" role="alert">
        Nothing could be loaded: {{ Object.values(errors)[0] }} <router-link to="/connection">Check the connection.</router-link>
      </p>
      <template v-else-if="data">
        <p v-if="failed.length" class="warn" role="status">Not loaded: {{ failed.join(', ') }}. The page may be incomplete.</p>
        <p v-if="nothing" class="help">Nothing for this service in the window. Widen it, or check the name.</p>

        <h3>Traffic</h3>
        <p v-if="!metricsApply" class="help">Call and resource metrics are kept for Kubernetes workloads (names like <code>/k8s/&lt;namespace&gt;/&lt;name&gt;</code>); this service is not one.</p>
        <p v-else-if="metricsError" class="error" role="alert">{{ metricsError }}</p>
        <template v-else-if="metrics">
          <ul v-if="metrics.warnings.length" class="warn-list" role="status">
            <li v-for="w in metrics.warnings" :key="w">{{ w }}</li>
          </ul>
          <p v-if="!metrics.found" class="help">No call or resource metrics for this service in the window.</p>
          <template v-if="metrics.traffic && (metrics.traffic.inbound.length || metrics.traffic.outbound.length)">
            <div class="cards" data-section="traffic">
              <div class="card"><h3>Called by others</h3>
                <dl>
                  <div><dt>Rate</dt><dd>{{ perSec(metrics.traffic.totals.inbound.rps) }}/s</dd></div>
                  <div><dt>Errors</dt><dd :class="{ bad: metrics.traffic.totals.inbound.errPct >= 5 }">{{ metrics.traffic.totals.inbound.errPct.toFixed(1) }}%</dd></div>
                  <div><dt>Callers</dt><dd>{{ metrics.traffic.totals.inbound.edges }}</dd></div>
                </dl></div>
              <div class="card"><h3>Calls out</h3>
                <dl>
                  <div><dt>Rate</dt><dd>{{ perSec(metrics.traffic.totals.outbound.rps) }}/s</dd></div>
                  <div><dt>Errors</dt><dd :class="{ bad: metrics.traffic.totals.outbound.errPct >= 5 }">{{ metrics.traffic.totals.outbound.errPct.toFixed(1) }}%</dd></div>
                  <div><dt>Targets</dt><dd>{{ metrics.traffic.totals.outbound.edges }}</dd></div>
                </dl></div>
            </div>
            <template v-for="[title, list, key] in [['Called by', metrics.traffic.inbound, 'in'], ['Calls out to', metrics.traffic.outbound, 'out']]" :key="key">
              <table v-if="list.length" class="services calls" :data-table="key">
                <caption class="muted">{{ title }}</caption>
                <thead><tr><th>Service</th><th>Protocol</th><th>Rate</th><th>Errors</th><th>p95</th></tr></thead>
                <tbody>
                  <tr v-for="e in list" :key="e.peer">
                    <td class="name"><router-link v-if="!e.peer.startsWith('external:')" :to="{ name: 'application', params: { name: e.peer } }">{{ e.peerLabel }}</router-link><template v-else>{{ e.peerLabel }}</template></td>
                    <td>{{ e.protocols.length ? e.protocols.join(', ') : 'tcp' }}</td>
                    <td>{{ edgeRate(e) }}</td>
                    <td :class="{ bad: e.errPct >= 5 }">{{ e.errPct.toFixed(1) }}% <span class="muted">({{ e.errBasis }})</span></td>
                    <td>{{ e.p95ms != null ? `${e.p95ms} ms` : '–' }}</td>
                  </tr>
                </tbody>
              </table>
            </template>
          </template>
          <template v-if="res && Object.values(res).some((v) => v !== null)">
            <h3>Resources</h3>
            <div class="cards" data-section="resources">
              <div class="card"><h3>Instances</h3><dl><div><dt>Running</dt><dd>{{ res.instances ?? '–' }}</dd></div></dl></div>
              <div class="card"><h3>CPU</h3>
                <dl>
                  <div><dt>Used (mean)</dt><dd>{{ cores(res.cpuCores) }} cores</dd></div>
                  <div v-if="res.cpuLimitCores"><dt>Limit</dt><dd>{{ cores(res.cpuLimitCores) }} cores</dd></div>
                </dl></div>
              <div class="card"><h3>Memory</h3>
                <dl>
                  <div><dt>In use</dt><dd>{{ bytes(res.memoryBytes) }}</dd></div>
                  <div v-if="res.memoryLimitBytes"><dt>Limit</dt><dd>{{ bytes(res.memoryLimitBytes) }}</dd></div>
                </dl></div>
              <div class="card"><h3>In this window</h3>
                <dl>
                  <div><dt>Restarts</dt><dd :class="{ bad: res.restarts }">{{ res.restarts ?? '–' }}</dd></div>
                  <div><dt>OOM kills</dt><dd :class="{ bad: res.oomKills }">{{ res.oomKills ?? '–' }}</dd></div>
                </dl></div>
            </div>
          </template>
        </template>

        <template v-if="spans && spans.total">
          <p v-if="spans.truncated" class="warn" role="status">
            The service has more than {{ spans.total }} spans in this window, so the figures below describe only the
            newest {{ span(spans.to - spans.from) }} of it. Pick a shorter window for full coverage.
          </p>
          <div class="cards">
            <div v-for="[g, s] in groups" :key="g" class="card" :data-group="g">
              <h3>{{ GROUP_LABEL[g] }}</h3>
              <dl>
                <div><dt>Spans</dt><dd>{{ s.count }}</dd></div>
                <div><dt>Errors</dt><dd :class="{ bad: s.errors }">{{ s.errors }} ({{ pct(s.errorShare) }})</dd></div>
                <div><dt>p50</dt><dd>{{ formatDuration(s.p50) }}</dd></div>
                <div><dt>p95</dt><dd>{{ formatDuration(s.p95) }}</dd></div>
                <div><dt>p99</dt><dd>{{ formatDuration(s.p99) }}</dd></div>
              </dl>
            </div>
          </div>

          <h3>{{ GROUP_LABEL[spans.primary] }} over time</h3>
          <svg class="timeline" viewBox="0 0 300 60" preserveAspectRatio="none" role="img"
               :aria-label="`${spans.groups[spans.primary].count} spans, ${spans.groups[spans.primary].errors} errors, over ${span(spans.to - spans.from)}`">
            <g v-for="(b, i) in spans.timeline" :key="i">
              <rect :x="i * 10 + 1" :y="60 - (b.count / peak) * 58" width="8" :height="(b.count / peak) * 58" class="ok-bar" />
              <rect :x="i * 10 + 1" :y="60 - (b.errors / peak) * 58" width="8" :height="(b.errors / peak) * 58" class="err-bar" />
            </g>
          </svg>
          <p class="help axis"><span>{{ clock(spans.from) }}</span><span>{{ clock(spans.to) }}</span></p>
        </template>

        <h3>Recent errors</h3>
        <p v-if="!recentErrors.length" class="help">None in the window.</p>
        <ul v-else class="events">
          <li v-for="(e, i) in recentErrors" :key="i"><time>{{ clock(e.time) }}</time> <span class="tag">{{ e.kind }}</span> {{ e.text }}</li>
        </ul>

        <h3>Recent spans <router-link class="more" :to="{ name: 'traces', query: { service: name } }">All traces →</router-link></h3>
        <p v-if="!recentSpans.length" class="help">No spans in the window.</p>
        <table v-else class="services">
          <thead><tr><th>Time</th><th>Name</th><th>Kind</th><th>Duration</th><th>Status</th><th>Trace</th></tr></thead>
          <tbody>
            <tr v-for="s in recentSpans" :key="s.span_id">
              <td>{{ clock(s.time) }}</td>
              <td class="name">{{ s.name }}</td>
              <td>{{ (s.kind || '').toLowerCase() }}</td>
              <td>{{ formatDuration(s.duration_ns) }}</td>
              <td :class="{ bad: s.status_code === 'ERROR' }">{{ (s.status_code || '').toLowerCase() }}</td>
              <td class="name"><router-link :to="{ name: 'trace', params: { id: s.trace_id } }" :title="s.trace_id">{{ shortId(s.trace_id) }}</router-link></td>
            </tr>
          </tbody>
        </table>

        <h3>Logs <router-link class="more" :to="{ name: 'logs', query: { service: name } }">All logs →</router-link> <span v-if="logs && logs.total" class="muted">
          ({{ logs.bySeverity.error }} error, {{ logs.bySeverity.warn }} warn, {{ logs.bySeverity.info }} info<template v-if="logs.truncated">, newest {{ logs.total }} only</template>)
        </span></h3>
        <p v-if="!recentLogs.length" class="help">No logs in the window.</p>
        <ul v-else class="events">
          <li v-for="(l, i) in recentLogs" :key="i"><time>{{ clock(l.time) }}</time> <span class="tag" :data-sev="severityClass(l)">{{ l.severity || '—' }}</span> {{ l.body }} <router-link v-if="l.trace_id" class="more" :to="{ name: 'trace', params: { id: l.trace_id } }">trace</router-link></li>
        </ul>

        <h3>Profiles</h3>
        <p v-if="!data.profiles.length" class="help">No profiles for this service.</p>
        <p v-else class="profile-links">
          <router-link v-for="p in data.profiles" :key="p.type" :to="{ name: 'profiles', query: { service: name, type: p.type } }">{{ p.type }}</router-link>
        </p>
      </template>
    </template>
  </section>
</template>
