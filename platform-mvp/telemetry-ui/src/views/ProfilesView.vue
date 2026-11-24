<script setup>
import { computed, inject, onMounted, reactive, ref } from 'vue'
import TelemetryNotice from '../components/TelemetryNotice.vue'
import { useRoute, useRouter } from 'vue-router'
import { WINDOWS } from '../api/services'
import { STACK_LIMITS, buildTree, formatValue, frameHue, layoutRows, loadStacks, loadTargets, meanCores, nodeAt, parseType, topFunctions } from '../api/profiles'

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
  type: String(q.type || ''),
  seconds: pick(Number(q.window), WINDOWS.map((w) => w.seconds), WINDOWS[1].seconds),
  limit: pick(Number(q.limit), STACK_LIMITS, STACK_LIMITS[1]),
})
const targets = ref(new Map())
const stacks = ref(null)
const loaded = ref({ service: '', type: '', seconds: 0 }) // what the picture on screen was loaded for
const loading = ref(false)
const ready = ref(false) // the list of services has been read
const error = ref('')
const path = ref([])
const hover = ref(null)
const find = ref('')

const services = computed(() => [...targets.value.keys()])
const types = computed(() => (targets.value.get(f.service) || []).map((t) => t.type))
const tree = computed(() => (stacks.value ? buildTree(stacks.value) : null))
const focus = computed(() => (tree.value ? nodeAt(tree.value, path.value) : null))
const layout = computed(() => (focus.value ? layoutRows(focus.value.node, focus.value.path) : null))
const top = computed(() => (stacks.value ? topFunctions(stacks.value) : null))
const unit = computed(() => parseType(loaded.value.type).unit)
const cores = computed(() => (tree.value ? meanCores(tree.value.value, loaded.value.type, loaded.value.seconds) : null))
const full = computed(() => stacks.value && stacks.value.length >= f.limit)
const term = computed(() => find.value.trim().toLowerCase())
const hits = (name) => !term.value || name.toLowerCase().includes(term.value)
const matching = computed(() => (term.value && layout.value ? layout.value.rows.flatMap((r) => r.nodes).filter((n) => hits(n.name)).length : 0))
const share = (v, of) => (of > 0 ? `${Math.round((v / of) * 1000) / 10}%` : '–')

function remember() {
  const query = {}
  if (f.service) query.service = f.service
  if (f.type) query.type = f.type
  if (f.seconds !== WINDOWS[1].seconds) query.window = String(f.seconds)
  if (f.limit !== STACK_LIMITS[1]) query.limit = String(f.limit)
  router.replace({ query })
}

function chooseService() {
  // A type belongs to a service: keep the chosen one if the new service has it, else the first it has (cpu first when present).
  const have = types.value
  if (!have.includes(f.type)) f.type = have.find((t) => t.startsWith('cpu')) || have[0] || ''
}

async function load() {
  if (!connected.value || !f.service || !f.type) return
  loading.value = true
  error.value = ''
  remember()
  try {
    stacks.value = await loadStacks(client, f, { seconds: f.seconds, limit: f.limit })
    loaded.value = { service: f.service, type: f.type, seconds: f.seconds }
    path.value = []
    hover.value = null
  } catch (e) {
    error.value = e.message
    stacks.value = null
  } finally {
    loading.value = false
  }
}

onMounted(async () => {
  if (!connected.value) return
  try {
    targets.value = await loadTargets(client)
  } catch (e) {
    error.value = e.message
    return
  } finally {
    ready.value = true
  }
  // Start at the first service when none (or an unknown one) was asked for: the quickest way to a picture.
  if (!targets.value.has(f.service)) f.service = services.value[0] || ''
  chooseService()
  load()
})

const zoom = (n) => (path.value = n.path)
const reset = () => (path.value = [])
const crumbs = computed(() => (focus.value ? focus.value.path.map((name, i) => ({ name, path: focus.value.path.slice(0, i + 1) })) : []))
const style = (n) => ({ left: `${n.left}%`, width: `${n.width}%`, background: `hsl(${frameHue(n.name)} 85% 68%)` })
const label = (n) => `${n.name} — ${formatValue(n.value, unit.value)} (${share(n.value, focus.value.node.value)} of view, ${share(n.value, tree.value.value)} of all), self ${formatValue(n.self, unit.value)}`
</script>

<template>
  <div class="page-head"><h1>Profiles</h1></div>
  <section class="panel">
    <TelemetryNotice />
    <p v-if="!connected" class="help">Not connected. <router-link to="/connection">Connect with a project and its read key</router-link> first.</p>
    <template v-else>
      <p class="help">Where a service spends its CPU or memory, from the node-agents' continuous profiles. Wider means more.</p>
      <p v-if="error" class="error" role="alert">{{ error }} <router-link to="/connection">Check the connection.</router-link></p>
      <p v-else-if="ready && !services.length" class="help">No profiles in this project yet. Are the node-agents sending profiles?</p>
      <form v-if="services.length" class="filters" @submit.prevent="load">
        <div><label for="service">Service</label>
          <select id="service" v-model="f.service" @change="chooseService"><option v-for="s in services" :key="s" :value="s">{{ s }}</option></select></div>
        <div><label for="type">Profile</label>
          <select id="type" v-model="f.type"><option v-for="t in types" :key="t" :value="t">{{ t }}</option></select></div>
        <div><label for="window">Window</label>
          <select id="window" v-model.number="f.seconds"><option v-for="w in WINDOWS" :key="w.seconds" :value="w.seconds">Last {{ w.label }}</option></select></div>
        <div><label for="limit">Stacks</label>
          <select id="limit" v-model.number="f.limit"><option v-for="n in STACK_LIMITS" :key="n" :value="n">top {{ n }}</option></select></div>
        <div><button type="submit" :disabled="loading || !f.service || !f.type">{{ loading ? 'Loading…' : 'Show' }}</button></div>
      </form>

      <template v-if="stacks && !error">
        <p v-if="!stacks.length" class="help">No samples for this service and profile in the window. Widen it, or pick another profile.</p>
        <template v-else>
          <p class="help summary">
            <strong>{{ formatValue(tree.value, unit) }}</strong> in {{ stacks.length }} stack{{ stacks.length === 1 ? '' : 's' }}
            <template v-if="cores !== null"> — about {{ cores < 0.01 ? cores.toExponential(1) : cores.toFixed(2) }} cores on average</template>
          </p>
          <p v-if="full" class="warn" role="status">
            Only the top {{ f.limit }} stacks were returned, so lighter ones are missing and the figures below are of those. Choose a larger count to include more.
          </p>

          <div class="flame-tools">
            <nav class="crumbs" aria-label="Zoom path">
              <button type="button" class="link" :disabled="!crumbs.length" @click="reset">all</button>
              <template v-for="c in crumbs" :key="c.path.join('/')"> / <button type="button" class="link" @click="zoom(c)">{{ c.name }}</button></template>
            </nav>
            <div class="find"><label for="find">Highlight</label><input id="find" v-model="find" type="search" placeholder="function name" autocomplete="off" />
              <span v-if="term" class="muted" role="status">{{ matching }} match{{ matching === 1 ? '' : 'es' }}</span></div>
          </div>
          <p class="hover" aria-live="polite">{{ hover ? label(hover) : 'Hover a box for details; click one to zoom in.' }}</p>
          <div class="flame" role="group" aria-label="Flame graph; the table below has the same information">
            <div v-for="row in layout.rows" :key="row.depth" class="flame-row">
              <button v-for="n in row.nodes" :key="n.path.join('/')" type="button" tabindex="-1" class="flame-node" :class="{ dim: !hits(n.name) }" :style="style(n)"
                      :title="label(n)" @mouseenter="hover = n" @focus="hover = n" @click="zoom(n)">{{ n.name }}</button>
            </div>
          </div>
          <p v-if="layout.minFraction > 0.001" class="help">Boxes narrower than {{ Math.round(layout.minFraction * 1000) / 10 }}% of the view are not drawn, to keep the page responsive. Zoom in to see them.</p>

          <h3>Hottest functions</h3>
          <table class="services functions">
            <thead><tr><th>Function</th><th>Self</th><th>Self %</th><th>Total</th><th>Total %</th></tr></thead>
            <tbody>
              <tr v-for="fn in top.functions" :key="fn.name">
                <td class="name wrap">{{ fn.name }}</td>
                <td>{{ formatValue(fn.self, unit) }}</td><td>{{ share(fn.self, top.all) }}</td>
                <td>{{ formatValue(fn.total, unit) }}</td><td>{{ share(fn.total, top.all) }}</td>
              </tr>
            </tbody>
          </table>
          <p class="help count">{{ top.functions.length }} of {{ top.count }} functions, by self time. <em>Self</em> is time spent in the function itself; <em>total</em> includes what it calls.</p>
        </template>
      </template>
    </template>
  </section>
</template>
