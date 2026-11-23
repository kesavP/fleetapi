<script setup>
import { computed, inject, onMounted, reactive, ref } from 'vue'
import { dateTime } from '../format'
import { loadRules, saveRule, sameSettings, settingsProblems } from '../api/rules'

// Rules: the settings the hub's detectors use to decide when to speak, kept ON THE HUB so that they can be changed without a deploy and are the same
// for everyone. Reading them takes the read key the page is connected with. Changing one takes the project's MANAGE key, typed here for the one save and
// forgotten after it, whatever the answer: it is never kept. A save is a new version on the hub (the history is kept, with who made it), and one that is
// based on an older version than the hub has is refused, so two people do not overwrite each other.
const props = defineProps({
  client: { type: Object, default: null },
  session: { type: Object, default: null },
  storage: { type: Object, default: null },
  loaders: { type: Object, default: null },
})
const client = props.client || inject('client')
const session = props.session || inject('session')
const load = (props.loaders && props.loaders.loadRules) || loadRules
const persist = (props.loaders && props.loaders.saveRule) || saveRule
const connected = computed(() => session.get())

// Who is asking is a per-browser convenience, not a secret: the hub records it with the change.
const KEY_ACTOR = 'fleet-ui.actor'
const ACTOR = /^[A-Za-z0-9][A-Za-z0-9 ._@+-]{0,63}$/
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

const TITLES = { 'rightsize.memory': 'Memory', 'rightsize.cpu': 'CPU', 'rightsize.storage': 'Storage' }
const WHAT = {
  'rightsize.memory': 'A container is over-provisioned when its memory use stayed below this share of its memory limit.',
  'rightsize.cpu': 'A container is over-provisioned when its CPU use stayed below this share of its CPU limit.',
  'rightsize.storage': 'A volume is over-provisioned when its use stayed below this share of its size. Off unless switched on: a volume usually cannot be made smaller, and one that is filling up looks over-provisioned until it has filled.',
}

const rows = ref([]) // [{rule, form, busy, error, conflict, ok}]
const loading = ref(false)
const error = ref('')
const identity = reactive({ key: '', actor: recall(KEY_ACTOR) })

const rowOf = (rule) => ({ rule, form: { ...rule.settings }, busy: false, error: '', conflict: false, ok: '' })

async function refresh() {
  if (!connected.value) return
  loading.value = true
  error.value = ''
  try {
    rows.value = (await load(client)).map(rowOf)
  } catch (e) {
    rows.value = []
    error.value = e.status === 404 ? 'This hub does not have the Rules API: it needs telemetry-ingest with rules (v6 or newer).' : e.message
  } finally {
    loading.value = false
  }
}
onMounted(refresh)

const actorProblem = computed(() => (identity.actor && !ACTOR.test(identity.actor) ? 'Letters, digits, spaces and . _ @ + - only.' : ''))
const problems = (r) => settingsProblems(r.form)
const dirty = (r) => !sameSettings(r.form, r.rule.settings)
const canSave = (r) => !r.busy && dirty(r) && !Object.keys(problems(r)).length

function useDefault(r) {
  r.form = { ...r.rule.default }
  r.error = ''
  r.conflict = false
  r.ok = ''
}

async function save(r) {
  if (r.busy) return
  r.error = ''
  r.conflict = false
  r.ok = ''
  const key = identity.key.trim()
  if (!canSave(r)) return
  if (actorProblem.value) return
  if (!key) {
    r.error = 'The manage key of this project is needed to change a rule.'
    return
  }
  r.busy = true
  try {
    const saved = await persist(client, r.rule, r.form, { key, actor: identity.actor })
    if (identity.actor) remember(KEY_ACTOR, identity.actor)
    r.rule = saved
    r.form = { ...saved.settings }
    r.ok = 'Saved. The detector uses it from its next look.'
  } catch (e) {
    r.error = e.message
    r.conflict = e.status === 409
  } finally {
    identity.key = '' // used, and not kept
    r.busy = false
  }
}

const settingsText = (s) => `${s.enabled ? 'on' : 'off'}, below ${s.percent}%, ${s.days} day${s.days === 1 ? '' : 's'}, judged by the ${s.percentile >= 100 ? 'highest use' : `${s.percentile}th percentile`}`
const status = (r) => (r.rule.saved ? `Saved as version ${r.rule.version}${r.rule.updatedBy ? ` by ${r.rule.updatedBy}` : ''}${r.rule.updatedAt ? ` on ${dateTime(r.rule.updatedAt)}` : ''}.` : 'Using the default: nothing has been saved for this project.')
</script>

<template>
  <section class="panel" data-testid="rules-tab">
    <p class="help" data-testid="rules-intro">
      Rules decide when the hub's detectors speak. They are kept on the hub, so a change applies to everyone and does not need a deploy: the detector reads them at its next look. These are
      not the same as the rules on the Incidents tab, which are kept in this browser. Reading them needs the project's read key; changing one needs its <strong>manage key</strong>, typed for each
      save and not kept. Each save is a new version, and a change based on an older version than the hub has is refused.
    </p>
    <p v-if="!connected" class="help">Not connected. <router-link to="/configuration/connection">Connect with a project and its read key</router-link> first.</p>
    <p v-else-if="error" class="error" role="alert" data-testid="rules-error">{{ error }}</p>
    <p v-else-if="loading && !rows.length" class="help" role="status">Reading the rules…</p>
    <template v-else-if="rows.length">
      <div class="filters identity">
        <div>
          <label for="rules-key">Manage key</label>
          <input id="rules-key" v-model="identity.key" type="password" data-testid="rules-key" autocomplete="off" spellcheck="false" />
        </div>
        <div>
          <label for="rules-actor">Your name (recorded with the change)</label>
          <input id="rules-actor" v-model.trim="identity.actor" type="text" data-testid="rules-actor" autocomplete="off" :aria-invalid="!!actorProblem" />
          <small class="bad" data-testid="err-actor">{{ actorProblem }}</small>
        </div>
      </div>

      <form v-for="r in rows" :key="r.rule.name" class="rule" :data-testid="`rule-${r.rule.name}`" @submit.prevent="save(r)">
        <h3>{{ TITLES[r.rule.name] }}</h3>
        <p class="help">{{ WHAT[r.rule.name] }}</p>
        <div class="filters">
          <div class="check">
            <input :id="`${r.rule.name}-enabled`" v-model="r.form.enabled" type="checkbox" name="enabled" />
            <label :for="`${r.rule.name}-enabled`">On</label>
          </div>
          <div>
            <label :for="`${r.rule.name}-percent`">Below (% of the limit)</label>
            <input :id="`${r.rule.name}-percent`" v-model.number="r.form.percent" type="number" name="percent" step="any" :aria-invalid="!!problems(r).percent" />
            <small class="bad" :data-testid="'err-percent'">{{ problems(r).percent || '' }}</small>
          </div>
          <div>
            <label :for="`${r.rule.name}-days`">Look back (days)</label>
            <input :id="`${r.rule.name}-days`" v-model.number="r.form.days" type="number" name="days" step="any" :aria-invalid="!!problems(r).days" />
            <small class="bad" :data-testid="'err-days'">{{ problems(r).days || '' }}</small>
          </div>
          <div>
            <label :for="`${r.rule.name}-percentile`">Percentile of use (100 = the highest)</label>
            <input :id="`${r.rule.name}-percentile`" v-model.number="r.form.percentile" type="number" name="percentile" step="any" :aria-invalid="!!problems(r).percentile" />
            <small class="bad" :data-testid="'err-percentile'">{{ problems(r).percentile || '' }}</small>
          </div>
        </div>
        <p class="muted" data-testid="status">{{ status(r) }}</p>
        <p class="muted" data-testid="default">Default: {{ settingsText(r.rule.default) }}.</p>
        <div class="actions">
          <button type="submit" data-testid="save" :disabled="!canSave(r)" @click.prevent="save(r)">{{ r.busy ? 'Saving…' : 'Save' }}</button>
          <button type="button" class="secondary" data-testid="reset" :disabled="r.busy" @click="useDefault(r)">Use the default values</button>
        </div>
        <p v-if="r.ok" class="ok" role="status" data-testid="rule-ok">{{ r.ok }}</p>
        <p v-if="r.error" class="error" role="alert" data-testid="rule-error">
          {{ r.error }}
          <button v-if="r.conflict" type="button" class="link" data-testid="reload" @click="refresh">Read the rules again</button>
        </p>
      </form>
    </template>
  </section>
</template>
