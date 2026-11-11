<script setup>
import { computed, inject, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { STATUS, TTL_CHOICES, clock, clusterProblem, createEnrollment, enrollCommand, hubUrl, listEnrollments, revokeEnrollment, secondsLeft, valuesSnippet } from '../api/enrollments'

// Clusters: what has been enrolled, and how to enroll another. Making or revoking an enrollment needs a MANAGE key, asked for each time and
// kept nowhere (not in storage, not after the call). The page then shows ONE command to run on the cluster; the cluster trades the one-time
// token in it for an ingest key of its own, so no key is ever shown here or copied by hand.
const props = defineProps({
  client: { type: Object, default: null },
  session: { type: Object, default: null },
  storage: { type: Object, default: null },
  pollMs: { type: Number, default: 4000 },
})
const client = props.client || inject('client')
const session = props.session || inject('session')
const connected = computed(() => session.get())

// Per-browser conveniences that are not secrets: where the hub is, as the cluster reaches it, and who is asking (it is recorded).
const KEY_URL = 'fleet-ui.hub-url'
const KEY_ACTOR = 'fleet-ui.actor'
const store = () => {
  try {
    return props.storage || window.localStorage
  } catch {
    return null
  }
}
const recall = (k) => {
  try {
    const s = store()
    return (s && s.getItem(k)) || ''
  } catch {
    return ''
  }
}
const remember = (k, v) => {
  try {
    const s = store()
    if (s) s.setItem(k, v)
  } catch {
    // not remembered; it still works
  }
}
const ACTOR = /^[A-Za-z0-9][A-Za-z0-9 ._@+-]{0,63}$/

const rows = ref([])
const loading = ref(false)
const listError = ref('')

async function load() {
  if (!connected.value) return
  loading.value = true
  listError.value = ''
  try {
    rows.value = await listEnrollments(client)
  } catch (e) {
    listError.value = e.message
  } finally {
    loading.value = false
  }
}

const form = reactive({ cluster: '', ttl: TTL_CHOICES[0].minutes, url: recall(KEY_URL), actor: recall(KEY_ACTOR), manageKey: '' })
const touched = ref(false)
const busy = ref(false)
const formError = ref('')

const problems = computed(() => ({
  cluster: clusterProblem(form.cluster),
  url: hubUrl(form.url).error || '',
  actor: form.actor && !ACTOR.test(form.actor) ? 'Letters, digits, spaces and . _ @ + - only.' : '',
  manageKey: form.manageKey.trim() ? '' : 'The manage key of this project is needed to make an enrollment.',
}))
const invalid = computed(() => Object.values(problems.value).some(Boolean))

// what was made, kept until it is dismissed: the token is not available again
const issued = ref(null)
const now = ref(Date.now())
let ticker = null
let poller = null

const left = computed(() => (issued.value ? secondsLeft(issued.value.expires_at, now.value) : 0))
const issuedRow = computed(() => (issued.value ? rows.value.find((r) => r.id === issued.value.id) : null))
// What the service last said, except that a token whose time has run out is expired whatever the last list said: it cannot be redeemed any more.
const issuedStatus = computed(() => {
  const s = issuedRow.value ? issuedRow.value.status : 'pending'
  return s === 'pending' && left.value === 0 ? 'expired' : s
})

function stopWatching() {
  if (ticker) clearInterval(ticker)
  if (poller) clearInterval(poller)
  ticker = poller = null
}
function watch() {
  stopWatching()
  now.value = Date.now()
  ticker = setInterval(() => (now.value = Date.now()), 1000)
  poller = setInterval(async () => {
    await load()
    if (issuedStatus.value !== 'pending') stopWatching()
  }, props.pollMs)
}
function dismiss() {
  stopWatching()
  issued.value = null
  copied.value = ''
}
onBeforeUnmount(() => {
  stopWatching()
  form.manageKey = ''
  issued.value = null
})

async function make() {
  touched.value = true
  formError.value = ''
  if (invalid.value || busy.value) return
  const h = hubUrl(form.url)
  busy.value = true
  try {
    const e = await createEnrollment(client, { cluster: form.cluster, ttlMinutes: form.ttl, manageKey: form.manageKey, actor: form.actor })
    const command = enrollCommand({ url: h.url, token: e.token })
    if (!command) throw new Error('The service answered with something that is not a token, so no command was made.')
    remember(KEY_URL, h.url)
    if (form.actor) remember(KEY_ACTOR, form.actor)
    issued.value = { id: e.id, cluster: e.cluster, token: e.token, expires_at: e.expires_at, command, url: h.url }
    form.manageKey = '' // used, and not kept
    touched.value = false
    form.cluster = ''
    copied.value = ''
    await load()
    watch()
  } catch (err) {
    formError.value = err.message
  } finally {
    busy.value = false
  }
}

const copied = ref('')
async function copy() {
  try {
    await navigator.clipboard.writeText(issued.value.command)
    copied.value = 'Copied.'
  } catch {
    copied.value = 'The browser would not copy it: select the command and copy it.'
  }
}
const snippet = computed(() => (issued.value ? valuesSnippet({ cluster: issued.value.cluster, url: issued.value.url }) : null))

// revoking: a manage key for this one call
const revoking = reactive({ id: '', key: '', error: '', busy: false })
function askRevoke(row) {
  revoking.id = revoking.id === row.id ? '' : row.id
  revoking.key = ''
  revoking.error = ''
}
async function confirmRevoke() {
  if (!revoking.key.trim() || revoking.busy) {
    revoking.error = 'The manage key of this project is needed.'
    return
  }
  revoking.busy = true
  revoking.error = ''
  try {
    await revokeEnrollment(client, revoking.id, { manageKey: revoking.key, actor: form.actor })
    revoking.id = ''
    revoking.key = ''
    await load()
  } catch (e) {
    revoking.error = e.message
  } finally {
    revoking.busy = false
  }
}
onMounted(load)

const when = (iso) => {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? '–' : d.toLocaleString()
}
const detail = (r) => {
  if (r.status === 'pending') return `expires ${when(r.expires_at)}`
  if (r.status === 'redeemed') return `enrolled ${when(r.redeemed_at)}`
  if (r.status === 'revoked') return `revoked ${when(r.revoked_at)}${r.revoked_by ? ' by ' + r.revoked_by : ''}`
  return `expired ${when(r.expires_at)}`
}
const canRevoke = (r) => r.status === 'pending' || r.status === 'redeemed'
</script>

<template>
  <section class="clusters" aria-label="Clusters">
    <p v-if="!connected" class="help">Not connected. <router-link to="/configuration/connection">Connect with a project and its read key</router-link> first.</p>

    <template v-else>
      <div class="panel">
        <h2>Add a cluster</h2>
        <p class="help">
          Makes a one-time token that lasts a few minutes, and a command to run on the cluster. The command trades the token for an ingest key of that
          cluster's own and saves it as the Secret the node-agent and the cluster collector read. The key never comes through this page, and the hub
          never reaches into the cluster.
        </p>
        <form novalidate @submit.prevent="make">
          <div class="enroll-fields">
            <label>
              Cluster name
              <input v-model.trim="form.cluster" type="text" data-testid="cluster" autocomplete="off" placeholder="gke" :aria-invalid="touched && !!problems.cluster" aria-describedby="cluster-e" />
              <small id="cluster-e" class="bad">{{ touched ? problems.cluster : '' }}</small>
            </label>
            <label>
              Hub address, as the cluster reaches it
              <input v-model.trim="form.url" type="text" data-testid="url" autocomplete="off" placeholder="http://hub:30081" :aria-invalid="touched && !!problems.url" aria-describedby="url-e" />
              <small id="url-e" class="bad">{{ touched ? problems.url : '' }}</small>
            </label>
            <label>
              The token lasts
              <select v-model.number="form.ttl" data-testid="ttl">
                <option v-for="c in TTL_CHOICES" :key="c.minutes" :value="c.minutes">{{ c.label }}</option>
              </select>
            </label>
            <label>
              Your name or email <span class="muted">(recorded, optional)</span>
              <input v-model.trim="form.actor" type="text" data-testid="actor" autocomplete="off" :aria-invalid="!!problems.actor" aria-describedby="actor-e" />
              <small id="actor-e" class="bad">{{ problems.actor }}</small>
            </label>
            <label>
              Manage key <span class="muted">(asked each time, kept nowhere)</span>
              <input v-model="form.manageKey" type="password" data-testid="manage-key" autocomplete="off" spellcheck="false" :aria-invalid="touched && !!problems.manageKey" aria-describedby="key-e" />
              <small id="key-e" class="bad">{{ touched ? problems.manageKey : '' }}</small>
            </label>
          </div>
          <p v-if="formError" class="error" role="alert" data-testid="form-error">{{ formError }}</p>
          <button type="submit" :disabled="busy" data-testid="make">{{ busy ? 'Making…' : 'Create enrollment' }}</button>
        </form>
      </div>

      <div v-if="issued" class="panel issued" role="region" :aria-label="`Run this on ${issued.cluster}`" data-testid="issued">
        <h3>Run this on <strong>{{ issued.cluster }}</strong></h3>
        <p class="help">
          On a machine that has <code>kubectl</code> pointed at that cluster and can reach the hub address. It prints nothing but what kubectl says; the key
          goes through the pipe and is not shown.
        </p>
        <pre class="command" data-testid="command" tabindex="0">{{ issued.command }}</pre>
        <div class="status" role="status" aria-live="polite">
          <button type="button" class="secondary" data-testid="copy" @click="copy">Copy</button>
          <span v-if="copied" class="muted" data-testid="copied">{{ copied }}</span>
          <span v-if="issuedStatus === 'pending'" data-testid="state" class="caution">Waiting for the cluster… the token works for {{ clock(left) }} more, once.</span>
          <span v-else-if="issuedStatus === 'redeemed'" data-testid="state" class="ok">{{ issued.cluster }} enrolled. Its key was made and saved on the cluster.</span>
          <span v-else-if="issuedStatus === 'revoked'" data-testid="state" class="bad">This enrollment was revoked.</span>
          <span v-else data-testid="state" class="bad">The token expired without being used. Make another.</span>
        </div>
        <p class="warn" role="note">This is the only time the token is shown. Anyone who has it can enroll a cluster until it is used or expires, so do not paste it anywhere else.</p>
        <details v-if="snippet">
          <summary>Next: install the agents</summary>
          <p class="help">In that cluster's values for the spoke chart (the Secret the command made is what they read by default):</p>
          <pre class="command" data-testid="snippet">{{ snippet }}</pre>
        </details>
        <button type="button" class="secondary" data-testid="dismiss" @click="dismiss">Done</button>
      </div>

      <h2>Clusters</h2>
      <p v-if="listError" class="error" role="alert">{{ listError }}</p>
      <p v-else-if="loading && !rows.length" class="help" role="status">Asking the telemetry service…</p>
      <p v-else-if="!rows.length" class="help" data-testid="none">No cluster has been enrolled this way yet.</p>
      <table v-else class="services" aria-label="Enrolled clusters">
        <thead><tr><th>Cluster</th><th>Status</th><th>Made</th><th></th></tr></thead>
        <tbody>
          <template v-for="r in rows" :key="r.id">
            <tr :data-id="r.id" :data-status="r.status">
              <td>{{ r.cluster }}</td>
              <td>
                <span class="status-pill" :class="(STATUS[r.status] || {}).tone">{{ (STATUS[r.status] || { label: r.status }).label }}</span>
                <small class="muted"> {{ detail(r) }}</small>
              </td>
              <td>{{ when(r.created_at) }}<small v-if="r.created_by" class="muted"> by {{ r.created_by }}</small></td>
              <td>
                <button v-if="canRevoke(r)" type="button" class="link" data-testid="revoke" @click="askRevoke(r)">{{ r.status === 'redeemed' ? 'Cut off its key' : 'Revoke' }}</button>
              </td>
            </tr>
            <tr v-if="revoking.id === r.id" class="revoke-row">
              <td colspan="4">
                <form class="inline" @submit.prevent="confirmRevoke">
                  <label>
                    Manage key
                    <input v-model="revoking.key" type="password" data-testid="revoke-key" autocomplete="off" spellcheck="false" />
                  </label>
                  <button type="submit" :disabled="revoking.busy" data-testid="revoke-confirm">{{ revoking.busy ? 'Revoking…' : r.status === 'redeemed' ? 'Cut off the key' : 'Revoke the token' }}</button>
                  <button type="button" class="secondary" @click="askRevoke(r)">Cancel</button>
                </form>
                <p v-if="r.status === 'redeemed'" class="help">The cluster's agents will be refused at their next report. To enroll it again, make a new enrollment.</p>
                <p v-if="revoking.error" class="error" role="alert" data-testid="revoke-error">{{ revoking.error }}</p>
              </td>
            </tr>
          </template>
        </tbody>
      </table>
      <p class="help">The keys in the hub's own configuration are not listed: only clusters enrolled here are.</p>
    </template>
  </section>
</template>
