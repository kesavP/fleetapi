<script setup>
import { computed, inject, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { DEFAULTS, RULES } from '../alerts/rules'
import { sorted } from '../alerts/state'
import { buildReport } from '../alerts/report'
import { gatherContext } from '../alerts/context'
import { dateTime, formatAge } from '../format'

// Takes the client, session and alert store by injection so a test can hand it fakes.
const props = defineProps({ client: { type: Object, default: null }, session: { type: Object, default: null }, store: { type: Object, default: null } })
const client = props.client || inject('client')
const session = props.session || inject('session')
const store = props.store || inject('alerts')
const state = store.state

const connected = computed(() => session.get())
const ordered = computed(() => sorted(state.alerts))
const active = computed(() => ordered.value.filter((a) => a.status !== 'resolved'))
const resolved = computed(() => ordered.value.filter((a) => a.status === 'resolved'))
const clock = ref(Date.now()) // so "since 5 min" moves while the page is open
let tick = null
const age = (ms) => formatAge(clock.value - ms)

const FIELDS = [
  { key: 'recentSeconds', label: 'Recent window', unit: 'seconds', help: 'What is being judged.' },
  { key: 'baselineSeconds', label: 'Baseline window', unit: 'seconds', help: 'What normal is; it ends where the recent window starts. Longer keeps a long outage visible for longer.' },
  { key: 'minBaselineRate', label: 'Minimum normal traffic', unit: 'per second', help: 'A service quieter than this is not expected to have traffic.' },
  { key: 'dropRatio', label: 'Traffic drop', unit: 'ratio', help: 'No traffic when the recent rate is below this share of normal (0.05 is 5%).' },
  { key: 'failedConnectShare', label: 'Failing connections', unit: '%', help: 'Share of connection attempts failing.' },
  { key: 'minFailedConnects', label: 'Failed connections needed', unit: 'in window', help: 'Fewer than this are ignored.' },
  { key: 'errorShare', label: 'Failing calls', unit: '%', help: 'Share of calls failing (5xx, gRPC server errors).' },
  { key: 'minCalls', label: 'Calls needed', unit: 'in window', help: 'Fewer than this are ignored.' },
]
const set = (key, e) => store.setConfig({ [key]: e.target.value })
const window = (seconds) => (seconds >= 3600 ? `${Math.round(seconds / 360) / 10} h` : `${Math.round(seconds / 60)} min`)

const analysis = reactive({ key: '', busy: false, text: '', error: '', note: '' })
async function analyze(a) {
  analysis.key = a.key
  analysis.busy = true
  analysis.text = analysis.error = analysis.note = ''
  try {
    const ctx = await gatherContext(client, a, { project: connected.value.project })
    analysis.text = buildReport(a, ctx, state.alerts)
  } catch (e) {
    analysis.error = e.message || 'Could not gather the evidence.'
  } finally {
    analysis.busy = false
  }
}
const close = () => (analysis.key = '')
async function copy(event) {
  try {
    await navigator.clipboard.writeText(analysis.text)
    analysis.note = 'Copied to the clipboard.'
  } catch {
    // no clipboard access (an insecure address, or the browser said no): select the text so Ctrl+C works
    // (found from the button: a ref inside the list of alerts would be an array of elements)
    const box = event && event.target && event.target.closest('[data-analysis]')
    const area = box && box.querySelector('textarea')
    if (area) {
      area.focus()
      area.select()
    }
    analysis.note = 'Could not copy automatically. The text is selected: press Ctrl+C (or Cmd+C).'
  }
}
const tokens = computed(() => Math.round(analysis.text.length / 4))
const evidence = (a) => Object.entries(a.evidence || {}).filter(([, v]) => !Array.isArray(v))
const callers = (a) => Object.values(a.evidence || {}).find((v) => Array.isArray(v)) || []

onMounted(() => {
  tick = setInterval(() => (clock.value = Date.now()), 30_000)
  if (connected.value && !state.lastRun) store.evaluate()
})
onBeforeUnmount(() => clearInterval(tick))
</script>

<template>
  <section class="panel">
    <h2>Alerts</h2>
    <p v-if="!connected" class="help">Not connected. <router-link to="/">Connect with a project and its read key</router-link> first.</p>
    <template v-else>
      <p v-if="state.source === 'server'" class="help" data-testid="source-server">
        Is each service still <strong>serving</strong>? The <strong>alert service</strong> in the cluster compares the last
        {{ Math.round((state.serverConfig ? state.serverConfig.recentSeconds : 300) / 60) }} minutes with the
        {{ window(state.serverConfig ? state.serverConfig.baselineSeconds : 3600) }} before them every {{ state.serverInterval || 60 }} seconds,
        whether or not anyone has this app open. This page shows what it found.
      </p>
      <p v-else class="help" data-testid="source-browser">
        Is each service still <strong>serving</strong>? Every {{ Math.round(store.intervalMs / 1000) }} seconds this page compares the last
        {{ Math.round(state.config.recentSeconds / 60) }} minutes with the {{ window(state.config.baselineSeconds) }} before them.
        <strong>Nothing is checking while this app is closed:</strong> no alert service is deployed, so the checking is done here, and what
        it has found is kept for this tab's session only.
      </p>

      <div class="toolbar">
        <button type="button" class="secondary" :disabled="state.running" @click="store.evaluate()">{{ state.running ? 'Checking…' : state.source === 'server' ? 'Refresh' : 'Check now' }}</button>
        <span v-if="state.lastRun" class="muted" data-testid="last-run">Last checked {{ dateTime(state.lastRun) }}; {{ state.checked }} services looked at.</span>
        <span v-else-if="!state.lastError" class="muted" data-testid="not-yet">{{ state.source === 'server' ? 'The alert service has not finished its first check.' : 'Not checked yet.' }}</span>
      </div>

      <p v-if="state.lastError" class="error" role="alert">
        {{ state.source === 'server' ? 'The alert service could not check' : 'Could not check' }}{{ state.lastRun && state.errorSince ? ` since ${dateTime(state.errorSince)}` : '' }}: {{ state.lastError }}
        <template v-if="active.length"> The alerts below are as of the last successful check; they have not been cleared.</template>
      </p>
      <ul v-if="state.skipped.length" class="warn-list" role="status"><li v-for="s in state.skipped" :key="s">{{ s }}</li></ul>

      <p v-if="state.lastRun && !active.length && !state.lastError" class="ok-note" data-testid="all-clear">
        Nothing is wrong that these rules can see: every service that had traffic still has it, and none is failing.
      </p>

      <article v-for="a in active" :key="a.key" class="alert" :class="[a.severity, a.status]" data-alert>
        <header>
          <span class="pill" :class="a.severity">{{ a.severity }}</span>
          <strong>{{ RULES[a.rule] || a.rule }}</strong>
          <router-link v-if="a.service !== '*'" class="name" :to="{ name: 'service', params: { name: a.service } }">{{ a.label }}</router-link>
          <span v-else class="name">whole project</span>
          <span v-if="a.status === 'pending'" class="tag">confirming</span>
        </header>
        <p>{{ a.summary }}</p>
        <p class="muted">Since {{ dateTime(a.firstSeen) }} ({{ age(a.firstSeen) }})</p>
        <dl v-if="evidence(a).length" class="kv small"><div v-for="[k, v] in evidence(a)" :key="k"><dt>{{ k }}</dt><dd>{{ v }}</dd></div></dl>
        <p v-if="callers(a).length" class="muted">Busiest callers: {{ callers(a).map((c) => `${c.label} (${c.rate}/s)`).join(', ') }}</p>
        <button type="button" :disabled="analysis.busy" @click="analyze(a)">{{ analysis.busy && analysis.key === a.key ? 'Gathering evidence…' : 'Analyze with Claude' }}</button>

        <div v-if="analysis.key === a.key && !analysis.busy" class="analysis" data-analysis>
          <p v-if="analysis.error" class="error" role="alert">{{ analysis.error }}</p>
          <template v-else>
            <p class="help">
              This is what the telemetry shows about the alert, ready to paste into Claude. Nothing has been sent anywhere.
              <strong>Read it first:</strong> log lines, error messages and statements can hold things you would not share; common secrets are masked, but that is not a guarantee.
            </p>
            <textarea class="report" readonly :value="analysis.text" rows="14" aria-label="Analysis request" />
            <div class="toolbar">
              <button type="button" @click="copy($event)">Copy</button>
              <button type="button" class="secondary" @click="close">Close</button>
              <span class="muted">{{ analysis.text.length.toLocaleString() }} characters (about {{ tokens.toLocaleString() }} tokens)</span>
              <span v-if="analysis.note" class="muted" role="status">{{ analysis.note }}</span>
            </div>
          </template>
        </div>
      </article>

      <details v-if="resolved.length" class="resolved">
        <summary>Resolved in the last day ({{ resolved.length }})</summary>
        <ul class="events">
          <li v-for="a in resolved" :key="a.key" data-resolved>
            <span class="pill" :class="a.severity">{{ a.severity }}</span> <strong>{{ RULES[a.rule] || a.rule }}</strong> {{ a.label }}
            <span class="muted">— {{ dateTime(a.firstSeen) }} to {{ dateTime(a.resolvedAt) }} ({{ formatAge(a.resolvedAt - a.firstSeen) }})</span>
          </li>
        </ul>
      </details>

      <details v-if="state.source === 'server'" class="thresholds" data-testid="thresholds-server">
        <summary>Thresholds</summary>
        <p class="help">Set where the alert service is deployed (<code>telemetryAlerts.thresholds</code> in the chart), so they cannot be changed here.</p>
        <dl v-if="state.serverConfig" class="kv small"><div v-for="f in FIELDS" :key="f.key"><dt>{{ f.label }} ({{ f.unit }})</dt><dd>{{ state.serverConfig[f.key] }}</dd></div></dl>
      </details>
      <details v-else class="thresholds">
        <summary>Thresholds</summary>
        <div class="fields">
          <div v-for="f in FIELDS" :key="f.key">
            <label :for="`t-${f.key}`">{{ f.label }} <span class="muted">({{ f.unit }})</span></label>
            <input :id="`t-${f.key}`" type="number" min="0" step="any" :value="state.config[f.key]" @change="set(f.key, $event)" />
            <span class="help">{{ f.help }} Default {{ DEFAULTS[f.key] }}.</span>
          </div>
        </div>
        <button type="button" class="secondary" @click="store.resetConfig()">Reset to defaults</button>
      </details>
    </template>
  </section>
</template>
