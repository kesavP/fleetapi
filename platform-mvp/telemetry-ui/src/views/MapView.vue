<script setup>
import { computed, inject, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import TelemetryNotice from '../components/TelemetryNotice.vue'
import { useRoute, useRouter } from 'vue-router'
import AppIcon from '../components/AppIcon.vue'
import CategoryFilter from '../components/CategoryFilter.vue'
import MoveToCategory from '../components/MoveToCategory.vue'
import NamespacePicker from '../components/NamespacePicker.vue'
import { WINDOWS } from '../api/services'
import { alertLevels, categoryOf, healthItems, loadApplications } from '../api/applications'
import { createCategoryStore } from '../categories'
import { loadMap } from '../map/load'
import { DIRECTIONS, NODE_H, NODE_W, edgePath, layout } from '../map/layout'

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

// The choices live in the address, as on the Applications page: a view can be shared, and the back button works.
const q = route.query
const list = (v) => (v ? String(v).split(',').filter(Boolean) : [])
const validCats = list(q.cat).filter((c) => store.all().includes(c))
const f = reactive({
  seconds: WINDOWS.some((w) => w.seconds === Number(q.window)) ? Number(q.window) : WINDOWS[1].seconds,
  search: String(q.q || ''),
  namespaces: list(q.ns),
  categories: validCats.length ? validCats : ['application'],
  direction: DIRECTIONS.includes(q.dir) ? q.dir : 'lr',
})
const map = ref(null)
const loading = ref(false)
const fatal = ref('')
const selected = ref('')
const zoom = ref(1)

function remember() {
  const query = {}
  if (f.seconds !== WINDOWS[1].seconds) query.window = String(f.seconds)
  if (f.search.trim()) query.q = f.search.trim()
  if (f.namespaces.length) query.ns = f.namespaces.join(',')
  if (f.categories.join() !== 'application') query.cat = f.categories.join(',')
  if (f.direction !== 'lr') query.dir = f.direction
  router.replace({ query })
}
watch([() => f.search, () => f.namespaces.join(), () => f.categories.join(), () => f.direction], () => {
  remember()
  zoom.value = 1
})

// What is shown: the applications of the namespace kinds that are on and the namespaces chosen, the external endpoints they call, and the
// calls among those. Everything else stays out of the picture, so a busy cluster is not drawn whole.
const internal = computed(() => (map.value ? map.value.nodes.filter((n) => !n.external) : []))
// An application's category is where its namespace puts it, or where the user moved it to (see categories.js).
const categoryOfNode = (n) => store.categoryFor(n.id, categoryOf(n.ns))
const inKinds = computed(() => internal.value.filter((n) => f.categories.includes(categoryOfNode(n))))
const namespaces = computed(() => {
  const by = new Map()
  for (const n of inKinds.value) by.set(n.ns, (by.get(n.ns) || 0) + 1)
  return [...by].map(([name, count]) => ({ name, count })).sort((a, b) => a.name.localeCompare(b.name))
})
// A namespace chosen earlier but no longer on offer (its kind was switched off) is not a choice any more: the picker shows "all" for it,
// so the picture must too.
const chosen = computed(() => f.namespaces.filter((n) => namespaces.value.some((x) => x.name === n)))
const shownInternal = computed(() => new Set(inKinds.value.filter((n) => !chosen.value.length || chosen.value.includes(n.ns)).map((n) => n.id)))
const edges = computed(() => {
  if (!map.value) return []
  const nodeOf = new Map(map.value.nodes.map((n) => [n.id, n]))
  return map.value.edges.filter((e) => {
    if (!shownInternal.value.has(e.from)) return false
    const to = nodeOf.get(e.to)
    return shownInternal.value.has(e.to) || (to && to.external)
  })
})
const nodes = computed(() => {
  if (!map.value) return []
  const used = new Set(edges.value.flatMap((e) => [e.from, e.to]))
  // an application with no call in the window is still an application of the project: it stays, on its own
  return map.value.nodes.filter((n) => (n.external ? used.has(n.id) : shownInternal.value.has(n.id)))
})
const placed = computed(() => (nodes.value.length ? layout(nodes.value, edges.value, { direction: f.direction }) : null))
const byId = computed(() => new Map(nodes.value.map((n) => [n.id, n])))
const levels = computed(() => alertLevels(alerts ? alerts.state.alerts : []))

// The hover menu lists how each application is doing (instances, CPU, memory, DNS, logs). That is the Applications page's data, read at the
// same time as the map and shared with that page through a short cache. If it cannot be read the map is still drawn and the menu says so.
const appData = ref(new Map())
const appsError = ref('')
const NOT_LOADED = ['instances', 'cpu', 'memory', 'dns', 'logs'].map((key) => ({ key, label: { instances: 'Instances', cpu: 'CPU', memory: 'Memory', dns: 'DNS', logs: 'Logs' }[key], status: 'unknown', hint: 'not loaded', anchor: key }))
const menuItems = (n) => (appData.value.has(n.id) ? healthItems(appData.value.get(n.id)) : NOT_LOADED)

const drawn = computed(() => {
  if (!placed.value) return []
  const { positions, back } = placed.value
  return edges.value
    .filter((e) => positions.has(e.from) && positions.has(e.to))
    .map((e) => ({ ...e, key: `${e.from}>${e.to}`, ...edgePath(positions.get(e.from), positions.get(e.to), back.has(`${e.from}\n${e.to}`), f.direction), level: level(e), width: width(e) }))
})

// Colour carries health, thickness a little of the traffic; nothing else is on a line until it is asked for.
function level(e) {
  return e.errPct >= 5 ? 'bad' : e.errPct >= 1 ? 'caution' : 'ok'
}
function width(e) {
  const load = (e.rps ?? 0) + (e.rps == null ? e.connPerSec : 0)
  return Math.round((1.2 + Math.min(1.8, Math.log10(1 + load) * 0.9)) * 10) / 10
}
const rate = (x) => (x >= 100 ? Math.round(x) : x >= 10 ? x.toFixed(1) : x.toFixed(2))
const edgeLabel = (e) => (e.rps != null ? `${rate(e.rps)}/s` : `${rate(e.connPerSec)} conn/s`)
const edgeText = (e) => {
  const parts = [e.rps != null ? `${rate(e.rps)} ${e.unit}` : `${rate(e.connPerSec)} connections/s, no calls seen`]
  parts.push(`${e.errPct.toFixed(1)}% errors (${e.errBasis})`)
  if (e.p95ms != null) parts.push(`p95 ${e.p95ms} ms`)
  return parts.join(', ')
}

// A node's health from what the map knows: a firing alert, or how many of the calls it answers fail. (The Applications page has more:
// instances, restarts.) An external endpoint has none.
const inboundErr = computed(() => {
  const by = new Map()
  for (const e of drawn.value) {
    const cur = by.get(e.to) || { attempts: 0, failures: 0 }
    cur.attempts += e.attempts
    cur.failures += e.failures
    by.set(e.to, cur)
  }
  return by
})
function status(n) {
  if (n.external) return 'external'
  const alert = levels.value.get(n.id)
  const inb = inboundErr.value.get(n.id)
  const pct = inb && inb.attempts > 0 ? (100 * inb.failures) / inb.attempts : 0
  if (alert === 'critical' || pct >= 5) return 'bad'
  if (alert === 'warning' || pct >= 1) return 'caution'
  return 'ok'
}
const STATUS_TEXT = { ok: 'healthy', caution: 'warning', bad: 'failing', external: 'external endpoint' }

const DATABASES = new Set(['postgres', 'mysql', 'redis', 'memcached', 'mongo', 'clickhouse', 'cassandra', 'foundationdb'])
const BROKERS = new Set(['kafka', 'rabbitmq', 'nats'])
const kind = (n) => (n.external ? 'external' : n.serves.length ? n.serves[0] : '')
const icon = (n) => (n.external ? 'globe' : DATABASES.has(n.serves[0]) ? 'database' : BROKERS.has(n.serves[0]) ? 'queue' : 'workload')
const subtitle = (n) => (n.external ? 'external endpoint' : n.serves.length ? `${n.serves[0]} / ns:${n.ns}` : `ns:${n.ns}`)
const trim = (s, n = 26) => (s.length > n ? s.slice(0, n - 1) + '…' : s)

const term = computed(() => f.search.trim().toLowerCase())
const matches = (n) => !term.value || n.name.toLowerCase().includes(term.value) || n.ns.toLowerCase().includes(term.value)
const matching = computed(() => (term.value ? nodes.value.filter(matches).length : 0))

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
const showLabel = (e) => e.errPct >= 1 || (!!selected.value && touches(e)) // a failing call says so; the rest speak when asked
const select = (id) => (selected.value = selected.value === id ? '' : id)
const label = (id) => (byId.value.get(id) ? byId.value.get(id).label : id)

// Sizing: the picture is scaled down to fit the width of the space it has (never up, and not below MIN_SCALE, where its text would stop
// being readable: past that it scrolls), then by the zoom buttons. Selecting a node never changes the size: its details open below.
const PADDING = 16 // the scroller's padding, both sides
const MIN_SCALE = 0.65
const canvas = ref(null)
const canvasWidth = ref(0)
let watcher = null
// The scroller only exists once there is something to draw, so it is watched for, not looked for at mount.
watch(canvas, (el, was) => {
  if (watcher) watcher.disconnect()
  watcher = null
  if (el && typeof ResizeObserver !== 'undefined') {
    canvasWidth.value = Math.max(0, el.clientWidth - PADDING)
    watcher = new ResizeObserver(([entry]) => (canvasWidth.value = entry.contentRect.width))
    watcher.observe(el)
  } else if (!el && was) {
    canvasWidth.value = 0
  }
})
onMounted(load)
onBeforeUnmount(() => watcher && watcher.disconnect())
const fit = computed(() => (placed.value && canvasWidth.value ? Math.min(1, canvasWidth.value / placed.value.width) : 1))
const scale = computed(() => Math.max(MIN_SCALE, fit.value) * zoom.value)
const zoomBy = (factor) => (zoom.value = Math.min(3, Math.max(0.4, Math.round(zoom.value * factor * 100) / 100)))

// ---- "Move the app to a category" ----
// Opened from the card's three dots. Moving an application out of every category that is shown makes it leave the picture, so the page says
// what happened, offers to show that category, and can undo it.
const menuFor = ref('')
const notice = ref(null)
const defaultCategory = (n) => categoryOf(n.ns)
const toggleMenu = (id) => (menuFor.value = menuFor.value === id ? '' : id)
function moveNode(n, category) {
  const before = categoryOfNode(n)
  if (!store.move(n.id, category, defaultCategory(n))) return
  menuFor.value = ''
  notice.value = { text: `Moved ${n.label} to ${category}.`, category, hidden: !f.categories.includes(category), undo: () => { store.move(n.id, before, defaultCategory(n)); notice.value = null } }
}
function addAndMove(n, name) {
  const made = store.add(name)
  if (made) moveNode(n, made)
}
const showCategory = (c) => {
  if (!f.categories.includes(c)) f.categories = [...f.categories, c]
  notice.value = null
}
// Which side of the card the popup opens on: away from the edge of the picture, so it is not cut off.
const POPUP_W = 230
const menuSide = (n) => (placed.value && placed.value.positions.get(n.id).x + NODE_W + POPUP_W > placed.value.width ? 'left' : 'right')
const closeMenu = () => (menuFor.value = '')
if (typeof document !== 'undefined') document.addEventListener('click', closeMenu)
onBeforeUnmount(() => document.removeEventListener('click', closeMenu))

// A category made from here is shown at once: making one and not seeing it would look like nothing happened.
function addCategory(name) {
  const made = store.add(name)
  if (made && !f.categories.includes(made)) f.categories = [...f.categories, made]
}

async function load(force = false) {
  if (!connected.value) return
  loading.value = true
  fatal.value = ''
  remember()
  // The menu's data is read alongside the map, and its failure does not stop the map.
  const appsRead = (appCache ? appCache.get(client, { project: connected.value.project, seconds: f.seconds, force: force === true }) : loadApplications(client, { seconds: f.seconds }))
    .then((r) => ((appData.value = new Map(r.apps.map((a) => [a.id, a]))), (appsError.value = '')))
    .catch((e) => ((appData.value = new Map()), (appsError.value = e.message)))
  try {
    map.value = await loadMap(client, { seconds: f.seconds })
    if (selected.value && !byId.value.has(selected.value)) selected.value = ''
  } catch (e) {
    fatal.value = e.message
    map.value = null
  } finally {
    await appsRead
    loading.value = false
  }
}
</script>

<template>
  <div class="page-head">
    <h1>Service map</h1>
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
        <span v-if="term" class="muted" role="status" data-testid="matches">{{ matching }} match{{ matching === 1 ? '' : 'es' }}</span>
        <CategoryFilter v-model="f.categories" :categories="store.all()" :validate="store.problem" @add="addCategory" />
        <div class="seg" role="group" aria-label="Layout">
          <button type="button" class="seg-btn" :class="{ on: f.direction === 'lr' }" :aria-pressed="f.direction === 'lr'" title="Left to right" aria-label="Left to right" @click="f.direction = 'lr'"><AppIcon name="flowRight" /></button>
          <button type="button" class="seg-btn" :class="{ on: f.direction === 'tb' }" :aria-pressed="f.direction === 'tb'" title="Top to bottom" aria-label="Top to bottom" @click="f.direction = 'tb'"><AppIcon name="flowDown" /></button>
        </div>
      </div>

      <p v-if="fatal" class="error pad" role="alert">{{ fatal }} <router-link to="/connection">Check the connection.</router-link></p>
      <template v-else-if="map">
        <ul v-if="map.warnings.length" class="warn-list" role="status">
          <li v-for="w in map.warnings" :key="w">{{ w }}</li>
        </ul>
        <p v-if="!map.edges.length && !loading" class="help pad">
          No calls between services in this window.
          <template v-if="!map.hasData">The project has none of the connection or request counters yet: are the node-agents sending?</template>
          <template v-else>Widen the window, or check that the services are receiving traffic.</template>
        </p>
        <p v-else-if="map.edges.length && !nodes.length" class="help pad" data-testid="hidden">Nothing to show with these filters. Choose other namespaces, or tick control-plane or monitoring.</p>

        <div v-if="placed" class="map-canvas">
          <div class="map-stage">
          <div class="map-tools" role="group" aria-label="Zoom">
            <button type="button" class="icon-btn" aria-label="Zoom in" @click="zoomBy(1.25)"><AppIcon name="zoomIn" /></button>
            <button type="button" class="icon-btn" aria-label="Zoom out" @click="zoomBy(0.8)"><AppIcon name="zoomOut" /></button>
            <button type="button" class="icon-btn" aria-label="Fit to the page" @click="zoom = 1"><AppIcon name="fit" /></button>
          </div>
          <div ref="canvas" class="map-scroll">
            <div class="map-surface" :style="{ width: Math.round(placed.width * scale) + 'px', height: Math.round(placed.height * scale) + 'px' }">
              <div class="map-inner" :style="{ width: placed.width + 'px', height: placed.height + 'px', transform: `scale(${scale})` }">
                <svg class="map" :viewBox="`0 0 ${placed.width} ${placed.height}`" :width="placed.width" :height="placed.height" role="group" aria-label="Service map" @click="selected = ''">
                  <defs>
                    <marker v-for="l in ['ok', 'caution', 'bad']" :id="`arrow-${l}`" :key="l" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="11" markerHeight="11" markerUnits="userSpaceOnUse" orient="auto-start-reverse">
                      <path d="M 0 1 L 10 5 L 0 9 z" :class="`arrow ${l}`" />
                    </marker>
                  </defs>
                  <g v-for="e in drawn" :key="e.key" class="edge" :class="[e.level, { dim: !touches(e) }]">
                    <path :d="e.d" class="hit" fill="none" />
                    <path :d="e.d" class="line" :stroke-width="e.width" :marker-end="`url(#arrow-${e.level})`" fill="none" />
                    <title>{{ label(e.from) }} → {{ label(e.to) }}: {{ edgeText(e) }}</title>
                    <text v-if="showLabel(e)" :x="e.mx" :y="e.my - 5" text-anchor="middle" class="edge-label">{{ edgeLabel(e) }}{{ e.errPct >= 1 ? ` · ${e.errPct.toFixed(0)}% err` : '' }}</text>
                  </g>
                </svg>
                <div v-for="n in nodes" :key="n.id" class="node" :class="{ ext: n.external, sel: n.id === selected, dim: (selected && !linked.has(n.id)) || (term && !matches(n)), raised: menuFor === n.id }" :data-status="status(n)"
                     role="group" :aria-label="`${n.label}, ${STATUS_TEXT[status(n)]}${kind(n) ? ', ' + kind(n) : ''}`"
                     :style="{ left: placed.positions.get(n.id).x + 'px', top: placed.positions.get(n.id).y + 'px', width: NODE_W + 'px', height: NODE_H + 'px' }" @click.stop="select(n.id)">
                  <span class="dot" :class="status(n)" aria-hidden="true" />
                  <div class="node-text">
                    <router-link v-if="!n.external" class="node-name" :to="{ name: 'application', params: { name: n.id } }" :title="n.label" @click.stop>{{ trim(n.name) }}</router-link>
                    <span v-else class="node-name">{{ trim(n.name) }}</span>
                    <span class="node-sub">{{ trim(subtitle(n), 32) }}</span>
                  </div>
                  <button v-if="!n.external" type="button" class="kebab" aria-haspopup="menu" :aria-expanded="menuFor === n.id" :aria-label="`Actions for ${n.label}`" @click.stop="toggleMenu(n.id)">⋮</button>
                  <button type="button" class="node-select" :aria-pressed="n.id === selected" :aria-label="`Show the calls of ${n.label}`" @click.stop="select(n.id)"><AppIcon :name="icon(n)" class="node-icon" /></button>
                  <div v-if="!n.external" class="node-menu" role="menu" :aria-label="`Details of ${n.label}`" @click.stop>
                    <router-link v-for="item in menuItems(n)" :key="item.key" class="node-menu-item" role="menuitem" :to="{ name: 'application', params: { name: n.id }, hash: `#${item.anchor}` }" :title="item.hint">
                      <span class="dot" :class="item.status" aria-hidden="true" />{{ item.label }}{{ ' ' }}<small>{{ item.hint }}</small>
                    </router-link>
                  </div>
                  <MoveToCategory v-if="menuFor === n.id" class="move-pop" :class="menuSide(n)" :current="categoryOfNode(n)" :categories="store.all()" :validate="store.problem"
                                  @move="(c) => moveNode(n, c)" @add="(name) => addAndMove(n, name)" @close="closeMenu" />
                </div>
              </div>
            </div>
          </div>
          </div>
        </div>
        <section v-if="node" class="detail map-detail" aria-live="polite">
          <button type="button" class="chip-x close" aria-label="Close details" @click="selected = ''">×</button>
          <h3>{{ node.label }} <span v-if="node.external" class="tag">external</span> <span class="tag" :class="`st-${status(node)}`">{{ STATUS_TEXT[status(node)] }}</span></h3>
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
        <p v-if="notice" class="notice pad" role="status" data-testid="notice">
          {{ notice.text }}<template v-if="notice.hidden"> It is hidden, because <strong>{{ notice.category }}</strong> is not shown.
            <button type="button" class="link" @click="showCategory(notice.category)">Show {{ notice.category }}</button></template>
          <button type="button" class="link" @click="notice.undo()">Undo</button>
        </p>
        <p v-if="placed && !selected" class="help pad hint">Click an application to see its calls. Hover one for its instances, CPU, memory, DNS and logs; ⋮ moves it to a category.</p>
        <p v-if="appsError" class="help pad" role="status" data-testid="apps-error">The hover menu could not load how each application is doing: {{ appsError }}</p>

        <p v-if="map.learned && map.learned.used" class="help pad" data-testid="learned">
          Connections that could not be completed are shown against the application behind the Service address they dialled
          ({{ map.learned.used }} address{{ map.learned.used === 1 ? '' : 'es' }}), matched from the successful connections to it earlier.
        </p>

        <details v-if="table.length" class="calls-table">
          <summary>All calls ({{ table.length }})</summary>
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
        </details>
      </template>
    </template>
  </section>
</template>
