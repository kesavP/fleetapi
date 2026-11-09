<script setup>
import { computed, inject, onMounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { buildWaterfall, getTrace, validTraceId } from '../api/search'
import { formatDuration } from '../api/serviceStats'
import { dateTime } from '../format'

// Takes the client and session by injection so a test can hand it fakes.
const props = defineProps({ client: { type: Object, default: null }, session: { type: Object, default: null } })
const client = props.client || inject('client')
const session = props.session || inject('session')
const route = useRoute()

const id = computed(() => String(route.params.id || '').toLowerCase())
const connected = computed(() => session.get())
const spans = ref(null)
const error = ref('')
const loading = ref(false)
const selected = ref('')

const wf = computed(() => (spans.value ? buildWaterfall(spans.value) : null))
const services = computed(() => (spans.value ? [...new Set(spans.value.map((s) => s.service))].sort() : []))
const current = computed(() => (wf.value && wf.value.rows.find((r) => r.span.span_id === selected.value)) || null)
const entries = (o) => Object.entries(o || {}).sort(([a], [b]) => a.localeCompare(b))

async function load() {
  if (!connected.value) return
  if (!validTraceId(id.value)) {
    error.value = 'A trace id is 32 hex characters.'
    return
  }
  loading.value = true
  error.value = ''
  selected.value = ''
  try {
    spans.value = await getTrace(client, id.value)
    if (spans.value.length) selected.value = buildWaterfall(spans.value).rows[0].span.span_id
  } catch (e) {
    error.value = e.message
    spans.value = null
  } finally {
    loading.value = false
  }
}
onMounted(load)
watch(id, load)
</script>

<template>
  <div class="page-head">
    <div><p class="crumb"><router-link to="/traces">← Traces</router-link></p><h1 class="svc-title" :title="id">Trace {{ id.slice(0, 8) }}…</h1></div>
  </div>
  <section class="panel">
    <p v-if="!connected" class="help">Not connected. <router-link to="/connection">Connect with a project and its read key</router-link> first.</p>
    <p v-else-if="error" class="error" role="alert">{{ error }}</p>
    <p v-else-if="loading" class="help">Loading…</p>
    <template v-else-if="spans">
      <p v-if="!spans.length" class="help">No spans found for this trace. It may be older than the retention, or from another project.</p>
      <template v-else>
        <p class="help">
          {{ spans.length }} span{{ spans.length === 1 ? '' : 's' }} in {{ services.length }} service{{ services.length === 1 ? '' : 's' }}
          ({{ services.join(', ') }}), {{ formatDuration(wf.totalMs * 1e6) }} from first start to last end.
          <router-link :to="{ name: 'logs', query: { trace_id: id } }">Logs for this trace</router-link>
        </p>
        <p v-if="spans.length > 1 && !wf.linked" class="help">These spans carry no parent links, so they are listed by start time, not nested.</p>
        <ol class="waterfall" aria-label="Spans of the trace">
          <li v-for="r in wf.rows" :key="r.span.span_id">
            <button type="button" class="wf-row" :class="{ sel: r.span.span_id === selected, bad: r.span.status_code === 'ERROR' }" :aria-pressed="r.span.span_id === selected" @click="selected = r.span.span_id">
              <span class="wf-label" :style="{ paddingLeft: `${r.depth * 14}px` }"><span class="name">{{ r.span.name }}</span> <span class="muted">{{ r.span.service }}</span></span>
              <span class="wf-track"><span class="wf-bar" :class="{ bad: r.span.status_code === 'ERROR' }" :style="{ left: `${r.left}%`, width: `${r.width}%` }" /></span>
              <span class="wf-dur">{{ formatDuration(r.span.duration_ns) }}</span>
            </button>
          </li>
        </ol>

        <section v-if="current" class="detail" aria-live="polite">
          <h3>{{ current.span.name }}</h3>
          <dl class="kv">
            <div><dt>Service</dt><dd><router-link :to="{ name: 'application', params: { name: current.span.service } }">{{ current.span.service }}</router-link></dd></div>
            <div><dt>Started</dt><dd>{{ dateTime(current.span.time) }}</dd></div>
            <div><dt>Duration</dt><dd>{{ formatDuration(current.span.duration_ns) }}</dd></div>
            <div><dt>Kind</dt><dd>{{ (current.span.kind || '').toLowerCase() }}</dd></div>
            <div><dt>Status</dt><dd :class="{ bad: current.span.status_code === 'ERROR' }">{{ (current.span.status_code || '').toLowerCase() }}<template v-if="current.span.status_message"> — {{ current.span.status_message }}</template></dd></div>
            <div><dt>Span id</dt><dd class="name">{{ current.span.span_id }}</dd></div>
            <div v-if="current.span.parent_span_id"><dt>Parent</dt><dd class="name">{{ current.span.parent_span_id }}</dd></div>
          </dl>
          <template v-if="entries(current.span.span_attributes).length">
            <h4>Span attributes</h4>
            <dl class="kv"><div v-for="[k, v] in entries(current.span.span_attributes)" :key="k"><dt>{{ k }}</dt><dd class="name wrap">{{ v }}</dd></div></dl>
          </template>
          <template v-if="entries(current.span.resource_attributes).length">
            <h4>Resource attributes</h4>
            <dl class="kv"><div v-for="[k, v] in entries(current.span.resource_attributes)" :key="k"><dt>{{ k }}</dt><dd class="name wrap">{{ v }}</dd></div></dl>
          </template>
          <template v-if="(current.span.event_names || []).length">
            <h4>Events</h4>
            <ul class="events"><li v-for="(n, i) in current.span.event_names" :key="i"><time>{{ dateTime(current.span.event_times && current.span.event_times[i]) }}</time> {{ n }}</li></ul>
          </template>
        </section>
      </template>
    </template>
  </section>
</template>
