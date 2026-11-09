<script setup>
import { computed, inject, onMounted, ref } from 'vue'
import { WINDOWS } from '../api/services'
import { loadMap } from '../map/load'
import { NODE_H, NODE_W, edgePath, layout } from '../map/layout'

// Takes the client and session by injection so a test can hand it fakes.
const props = defineProps({ client: { type: Object, default: null }, session: { type: Object, default: null } })
const client = props.client || inject('client')
const session = props.session || inject('session')

const connected = computed(() => session.get())
const seconds = ref(WINDOWS[1].seconds)
const map = ref(null)
const loading = ref(false)
const fatal = ref('')
const selected = ref('')

const placed = computed(() => (map.value ? layout(map.value.nodes, map.value.edges) : null))
const byId = computed(() => new Map((map.value ? map.value.nodes : []).map((n) => [n.id, n])))
const drawn = computed(() => {
  if (!placed.value) return []
  const { positions, back } = placed.value
  return map.value.edges
    .filter((e) => positions.has(e.from) && positions.has(e.to))
    .map((e, i) => {
      const p = edgePath(positions.get(e.from), positions.get(e.to), back.has(`${e.from}\n${e.to}`))
      return { ...e, key: `${e.from}>${e.to}`, i, ...p, level: level(e), width: width(e) }
    })
})

// Colour and thickness carry the two things worth seeing at a glance: failing, and busy.
function level(e) {
  return e.errPct >= 5 ? 'bad' : e.errPct >= 1 ? 'caution' : 'ok'
}
function width(e) {
  const load = (e.rps ?? 0) + (e.rps == null ? e.connPerSec : 0)
  return Math.round((1.5 + Math.min(4, Math.log10(1 + load) * 1.6)) * 10) / 10
}
const rate = (x) => (x >= 100 ? Math.round(x) : x >= 10 ? x.toFixed(1) : x.toFixed(2))
const edgeLabel = (e) => (e.rps != null ? `${rate(e.rps)}/s` : `${rate(e.connPerSec)} conn/s`)
const edgeText = (e) => {
  const parts = [e.rps != null ? `${rate(e.rps)} ${e.unit}` : `${rate(e.connPerSec)} connections/s, no calls seen`]
  parts.push(`${e.errPct.toFixed(1)}% errors (${e.errBasis})`)
  if (e.p95ms != null) parts.push(`p95 ${e.p95ms} ms`)
  return parts.join(', ')
}
const trim = (s, n = 24) => (s.length > n ? s.slice(0, n - 1) + '…' : s)

const touches = (e) => !selected.value || e.from === selected.value || e.to === selected.value
const linked = computed(() => {
  const s = new Set([selected.value])
  for (const e of drawn.value) if (touches(e)) { s.add(e.from); s.add(e.to) }
  return s
})
const node = computed(() => byId.value.get(selected.value) || null)
const inbound = computed(() => drawn.value.filter((e) => e.to === selected.value))
const outbound = computed(() => drawn.value.filter((e) => e.from === selected.value))
const table = computed(() => [...drawn.value].sort((a, b) => b.errPct - a.errPct || (b.rps ?? 0) - (a.rps ?? 0)))

const select = (id) => (selected.value = selected.value === id ? '' : id)
const label = (id) => (byId.value.get(id) ? byId.value.get(id).label : id)

async function load() {
  if (!connected.value) return
  loading.value = true
  fatal.value = ''
  try {
    map.value = await loadMap(client, { seconds: seconds.value })
    if (selected.value && !byId.value.has(selected.value)) selected.value = ''
  } catch (e) {
    fatal.value = e.message
    map.value = null
  } finally {
    loading.value = false
  }
}
onMounted(load)
</script>

<template>
  <div class="page-head"><h1>Service map</h1></div>
  <section class="panel">
    <p v-if="!connected" class="help">
      Not connected. <router-link to="/connection">Connect with a project and its read key</router-link> first.
    </p>
    <template v-else>
      <p class="help">
        Who calls whom in project <strong>{{ connected.project }}</strong>, from the node-agents' connection and request
        counters. Line thickness is traffic; red lines are failing.
      </p>
      <div class="toolbar">
        <label for="window">Window</label>
        <select id="window" v-model.number="seconds" @change="load">
          <option v-for="w in WINDOWS" :key="w.seconds" :value="w.seconds">Last {{ w.label }}</option>
        </select>
        <button type="button" class="secondary" :disabled="loading" @click="load">{{ loading ? 'Loading…' : 'Refresh' }}</button>
      </div>

      <p v-if="fatal" class="error" role="alert">{{ fatal }} <router-link to="/connection">Check the connection.</router-link></p>
      <template v-else-if="map">
        <ul v-if="map.warnings.length" class="warn-list" role="status">
          <li v-for="w in map.warnings" :key="w">{{ w }}</li>
        </ul>
        <p v-if="!map.edges.length && !loading" class="help">
          No calls between services in this window.
          <template v-if="!map.hasData">The project has none of the connection or request counters yet: are the node-agents sending?</template>
          <template v-else>Widen the window, or check that the services are receiving traffic.</template>
        </p>

        <div v-if="placed && map.edges.length" class="map-scroll">
          <svg class="map" :viewBox="`0 0 ${placed.width} ${placed.height}`" :width="placed.width" :height="placed.height" role="group" aria-label="Service map" @click.self="selected = ''">
            <defs>
              <marker v-for="l in ['ok', 'caution', 'bad']" :id="`arrow-${l}`" :key="l" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
                <path d="M 0 0 L 10 5 L 0 10 z" :class="`arrow ${l}`" />
              </marker>
            </defs>
            <g v-for="e in drawn" :key="e.key" class="edge" :class="[e.level, { dim: !touches(e) }]">
              <path :d="e.d" :stroke-width="e.width" :marker-end="`url(#arrow-${e.level})`" fill="none">
                <title>{{ label(e.from) }} → {{ label(e.to) }}: {{ edgeText(e) }}</title>
              </path>
              <text :x="e.mx" :y="e.my - 4" text-anchor="middle" class="edge-label">{{ edgeLabel(e) }}{{ e.errPct >= 1 ? ` · ${e.errPct.toFixed(0)}% err` : '' }}</text>
            </g>
            <g v-for="n in map.nodes" :key="n.id" class="node" :class="{ ext: n.external, sel: n.id === selected, dim: selected && !linked.has(n.id) }"
               :transform="`translate(${placed.positions.get(n.id).x} ${placed.positions.get(n.id).y})`" tabindex="0" role="button"
               :aria-pressed="n.id === selected" :aria-label="`${n.label}${n.serves.length ? ', ' + n.serves.join(', ') : ''}`"
               @click.stop="select(n.id)" @keydown.enter.prevent="select(n.id)" @keydown.space.prevent="select(n.id)">
              <rect :width="NODE_W" :height="NODE_H" rx="8" />
              <text :x="NODE_W / 2" :y="n.serves.length ? 19 : 27" text-anchor="middle" class="node-name">{{ trim(n.name) }}</text>
              <text v-if="n.serves.length" :x="NODE_W / 2" y="35" text-anchor="middle" class="node-sub">{{ n.serves.join(', ') }}</text>
              <title>{{ n.label }}</title>
            </g>
          </svg>
        </div>

        <section v-if="node" class="detail" aria-live="polite">
          <h3>{{ node.label }} <span v-if="node.external" class="tag">external</span></h3>
          <p v-if="!node.external"><router-link :to="{ name: 'application', params: { name: node.id } }">Open the service page</router-link></p>
          <p v-if="!inbound.length && !outbound.length" class="help">No calls in the window.</p>
          <template v-if="outbound.length">
            <h4>Calls out</h4>
            <ul class="events"><li v-for="e in outbound" :key="e.key"><span :class="e.level">●</span> {{ label(e.to) }} — {{ edgeText(e) }}</li></ul>
          </template>
          <template v-if="inbound.length">
            <h4>Called by</h4>
            <ul class="events"><li v-for="e in inbound" :key="e.key"><span :class="e.level">●</span> {{ label(e.from) }} — {{ edgeText(e) }}</li></ul>
          </template>
        </section>
        <p v-else-if="map.edges.length" class="help">Select a service to see its calls.</p>

        <template v-if="table.length">
          <h3>All calls</h3>
          <table class="services calls">
            <thead><tr><th>From</th><th>To</th><th>Protocol</th><th>Rate</th><th>Errors</th><th>p95</th></tr></thead>
            <tbody>
              <tr v-for="e in table" :key="e.key">
                <td class="name">{{ label(e.from) }}</td>
                <td class="name">{{ label(e.to) }}</td>
                <td>{{ e.protocols.length ? e.protocols.join(', ') : 'tcp' }}</td>
                <td>{{ e.rps != null ? `${rate(e.rps)} ${e.unit}` : `${rate(e.connPerSec)} conn/s` }}</td>
                <td :class="{ bad: e.level === 'bad' }">{{ e.errPct.toFixed(1) }}% <span class="muted">({{ e.errBasis }})</span></td>
                <td>{{ e.p95ms != null ? `${e.p95ms} ms` : '–' }}</td>
              </tr>
            </tbody>
          </table>
          <p v-if="map.ignored.loopback" class="help">{{ map.ignored.loopback }} loopback connection(s) left out: they stay inside one pod.</p>
        </template>
      </template>
    </template>
  </section>
</template>
