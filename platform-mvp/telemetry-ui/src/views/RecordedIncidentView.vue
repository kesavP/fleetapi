<script setup>
import { computed, inject, onMounted, reactive, ref } from 'vue'
import { useRoute } from 'vue-router'
import IncidentTabs from '../components/IncidentTabs.vue'
import { dateTime } from '../format'
import { analyseIncident, describeEvidence, latestAnalysis, loadRecordedIncident } from '../incidents/recorded'
import { duration, loadSummary } from '../incidents/mttr'

// One recorded incident: what the detector (or a person) said, the evidence it points at, and the timeline of what happened to it. It only READS, but for
// one thing: asking Claude for resolution steps, which the hub keeps as a comment on the incident. That takes a manage key, typed for the one call and not kept.
const props = defineProps({ client: { type: Object, default: null }, session: { type: Object, default: null }, loaders: { type: Object, default: null } })
const client = props.client || inject('client')
const session = props.session || inject('session')
const load = (props.loaders && props.loaders.loadRecordedIncident) || loadRecordedIncident
const analyse = (props.loaders && props.loaders.analyseIncident) || analyseIncident
const summarise = (props.loaders && props.loaders.loadSummary) || loadSummary
const route = useRoute()

const connected = computed(() => session.get())
const data = ref(null) // {incident, events, warning}
const loading = ref(false)
const error = ref('')
// The instant summary is read beside the incident; it failing leaves the rest of the page as it is.
const summary = ref(null)
const summaryError = ref('')

async function refresh() {
  if (!connected.value) return
  loading.value = true
  error.value = ''
  const id = String(route.params.id || '')
  const sum = summarise(client, id).then(
    (s) => { summary.value = s; summaryError.value = '' },
    (e) => { summary.value = null; summaryError.value = e && e.message ? e.message : 'unknown error' },
  )
  try {
    data.value = await load(client, id)
  } catch (e) {
    data.value = null
    error.value = e.message
  } finally {
    loading.value = false
  }
  await sum
}
const KIND_LABEL = { rollout: 'Rollout', kubernetes: 'Kubernetes', process: 'Process', incident: 'Incident', other: 'Other' }
const STAGES = [
  { key: 'detect', label: 'Detected after', hint: 'from the start of the trouble to the incident being opened' },
  { key: 'acknowledge', label: 'Taken on after', hint: 'from the opening to someone taking it on' },
  { key: 'mitigate', label: 'Mitigated after', hint: 'from the start of the trouble' },
  { key: 'resolve', label: 'Resolved after', hint: 'from the start of the trouble: what MTTR is made of' },
]
onMounted(refresh)

// Asking Claude: the key is for this call and is forgotten whatever the answer is. The hub allows one analysis of an incident every two minutes.
const ask = reactive({ key: '', busy: false, error: '' })
const analysis = computed(() => (data.value ? latestAnalysis(data.value.events) : null))
async function askClaude() {
  if (ask.busy) return
  ask.error = ''
  const key = ask.key.trim()
  if (!key) {
    ask.error = 'The manage key of this project is needed to ask for an analysis.'
    return
  }
  ask.busy = true
  try {
    await analyse(client, String(route.params.id || ''), key)
    await refresh()
  } catch (e) {
    ask.error = e.message
  } finally {
    ask.key = ''
    ask.busy = false
  }
}

const evidence = computed(() => (data.value ? data.value.incident.evidence.map((e) => ({ id: e.id, ...describeEvidence(e) })) : []))
const when = (t) => (t ? dateTime(t) : '–')
</script>

<template>
  <div class="page-head"><h1>Incident <span class="name">{{ route.params.id }}</span></h1></div>
  <IncidentTabs />
  <section class="panel">
    <p v-if="!connected" class="help">Not connected. <router-link to="/configuration/connection">Connect with a project and its read key</router-link> first.</p>
    <p v-else-if="error" class="error" role="alert">{{ error }} <router-link to="/recorded">Back to the recorded incidents.</router-link></p>
    <p v-else-if="loading && !data" class="help" role="status">Reading the incident…</p>
    <template v-else-if="data">
      <h2 data-testid="title">{{ data.incident.title }}</h2>
      <p>
        <span class="pill" :class="data.incident.severity">{{ data.incident.severity }}</span>
        <span class="pill state">{{ data.incident.state }}</span>
        <span v-if="data.incident.source" class="muted"> {{ data.incident.source === 'detected' ? 'opened by the detector' : 'opened by a person' }}</span>
      </p>
      <p v-if="data.incident.summary" class="summary" data-testid="summary">{{ data.incident.summary }}</p>

      <dl class="facts" data-testid="facts">
        <div v-if="data.incident.app"><dt>Application</dt><dd><router-link :to="{ name: 'application', params: { name: data.incident.app } }">{{ data.incident.app }}</router-link></dd></div>
        <div><dt>Started</dt><dd>{{ when(data.incident.startedAt) }}</dd></div>
        <div><dt>Opened</dt><dd>{{ when(data.incident.createdAt) }}</dd></div>
        <div><dt>Last change</dt><dd>{{ when(data.incident.updatedAt) }}</dd></div>
        <div v-if="data.incident.assignee"><dt>Assignee</dt><dd>{{ data.incident.assignee }}</dd></div>
        <div v-if="data.incident.resolution"><dt>Resolution</dt><dd>{{ data.incident.resolution }}</dd></div>
        <div v-if="data.incident.reopened"><dt>Reopened</dt><dd>{{ data.incident.reopened }} time{{ data.incident.reopened === 1 ? '' : 's' }}</dd></div>
      </dl>
      <p class="help" data-testid="transitions">
        <template v-if="data.incident.allowed.length">It can move to: {{ data.incident.allowed.join(', ') }}. </template>
        This page does not change the incident: to move it, call the incidents API with a manage key of this project.
      </p>

      <section class="instant" data-testid="instant">
        <h3>Instant summary</h3>
        <p v-if="summaryError" class="warn" role="note" data-testid="summary-error">The instant summary could not be read: {{ summaryError }}</p>
        <template v-else-if="summary">
          <p class="headline" data-testid="summary-headline">{{ summary.headline }}</p>
          <dl class="facts timing" data-testid="timing">
            <div v-for="st in STAGES" :key="st.key" :title="st.hint">
              <dt>{{ st.label }}</dt><dd>{{ summary.timing[st.key] === null ? 'not yet' : duration(summary.timing[st.key]) }}</dd>
            </div>
          </dl>
          <template v-if="summary.findings.length">
            <h4>What happened near the start</h4>
            <ol class="findings" data-testid="findings">
              <li v-for="(f, i) in summary.findings" :key="i">
                <span class="pill kind" :class="f.kind">{{ KIND_LABEL[f.kind] }}</span>
                {{ f.text }} <span v-if="f.at" class="muted">· {{ when(f.at) }}</span>
              </li>
            </ol>
          </template>
          <ul v-if="summary.gaps.length" class="gaps muted" data-testid="gaps">
            <li v-for="g in summary.gaps" :key="g">{{ g }}</li>
          </ul>
          <details v-if="summary.timeline.length" data-testid="summary-timeline">
            <summary>Everything around it ({{ summary.timeline.length }})</summary>
            <table class="services" aria-label="Around the incident">
              <thead><tr><th>When</th><th>From</th><th>What</th></tr></thead>
              <tbody>
                <tr v-for="(e, i) in summary.timeline" :key="i"><td>{{ when(e.at) }}</td><td>{{ KIND_LABEL[e.source] }}</td><td class="what">{{ e.text }}</td></tr>
              </tbody>
            </table>
          </details>
          <p class="help">Worked out by the hub from the incident, its workload's rollouts, Kubernetes events and process lineage. No model is involved; it lists what happened, not why.</p>
        </template>
        <p v-else class="help" role="status">Reading the summary…</p>
      </section>

      <section v-if="analysis" class="analysis" data-testid="analysis">
        <h3>Resolution steps from Claude</h3>
        <p class="muted">Written {{ when(analysis.when) }}. A suggestion from the incident's summary and evidence list only: check it before acting.</p>
        <pre class="analysis-text">{{ analysis.body }}</pre>
      </section>
      <form class="analyse" @submit.prevent="askClaude">
        <p class="help" data-testid="analyse-note">
          Asks Claude for resolution steps. This sends the incident's title, severity and summary and where its evidence is to the Anthropic API, with no logs or traces,
          and nothing else. It needs this project's manage key (the one that changes incidents; not an Anthropic key, which the hub already holds), typed here for this one call and not kept. It can take a minute or two, and each analysis costs money.
        </p>
        <label for="analyse-key">Manage key of this project (not an Anthropic key)</label>
        <input id="analyse-key" v-model="ask.key" type="password" data-testid="analyse-key" autocomplete="off" spellcheck="false" />
        <button type="submit" data-testid="analyse" :disabled="ask.busy" @click.prevent="askClaude">Analyse with Claude</button>
        <span v-if="ask.busy" data-testid="analyse-busy" role="status">Claude is working…</span>
        <p v-if="ask.error" class="error" role="alert" data-testid="analyse-error">{{ ask.error }}</p>
      </form>

      <h3>Evidence</h3>
      <p v-if="!evidence.length" class="help" data-testid="no-evidence">Nothing is attached.</p>
      <ul v-else class="evidence" data-testid="evidence">
        <li v-for="e in evidence" :key="e.id">
          <strong>{{ e.label }}</strong>
          <router-link v-if="e.to" :to="e.to">{{ e.text || 'open' }}</router-link>
          <a v-else-if="e.href" :href="e.href" rel="noopener noreferrer" target="_blank">{{ e.text }}</a>
          <template v-else>{{ e.text }}</template>
          <span v-if="e.note" class="muted"> · {{ e.note }}</span>
          <span v-if="e.addedBy" class="muted"> · added by {{ e.addedBy }}</span>
        </li>
      </ul>

      <h3>Timeline</h3>
      <p v-if="data.warning" class="warn" role="note">{{ data.warning }}</p>
      <p v-else-if="!data.events.length" class="help" data-testid="no-events">No events.</p>
      <table v-else class="services" aria-label="Timeline">
        <thead><tr><th>When</th><th>Who</th><th>What</th></tr></thead>
        <tbody>
          <tr v-for="e in data.events" :key="e.seq">
            <td>{{ when(e.when) }}</td>
            <td>{{ e.actor }}</td>
            <td class="what">{{ e.text }}</td>
          </tr>
        </tbody>
      </table>
    </template>
  </section>
</template>
