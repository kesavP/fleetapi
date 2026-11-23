<script setup>
import { computed, inject, onBeforeUnmount, onMounted, ref } from 'vue'
import { NOTICES, STEPS, loadTelemetryStatus } from '../api/telemetry'

// An error at the top of a page that depends on the node-agent, shown when the project has nothing from it: not installed, not running, or
// not able to reach the hub. Every page that is empty for that reason would otherwise show an empty table and leave the person to work out why.
// It reads the client and session by injection and shows nothing without them, so a page tested with its own fakes is unaffected. It is
// silent when the check cannot be made (a failed read is the page's own error to show) and never claims the agent is missing on a guess.
const props = defineProps({ client: { type: Object, default: null }, session: { type: Object, default: null } })
const client = props.client || inject('client', null)
const session = props.session || inject('session', null)

const state = ref('') // '' (not known, or fine), none, other, stale
const checking = ref(false)
const project = ref(session && session.get() ? session.get().project : '')
let alive = true
let run = 0

async function check() {
  if (!client || !session || !session.get()) {
    state.value = ''
    return
  }
  const mine = ++run
  checking.value = true
  try {
    const r = await loadTelemetryStatus(client)
    if (alive && mine === run) state.value = r.state === 'ok' ? '' : r.state
  } catch {
    if (alive && mine === run) state.value = ''
  } finally {
    if (mine === run) checking.value = false
  }
}

let stop = () => {}
onMounted(() => {
  check()
  if (session && session.subscribe) {
    stop = session.subscribe(() => {
      project.value = session.get() ? session.get().project : ''
      check()
    })
  }
})
onBeforeUnmount(() => {
  alive = false
  stop()
})

const notice = computed(() => (state.value ? NOTICES[state.value] : null))
</script>

<template>
  <section v-if="notice" class="telemetry-notice" role="alert" data-testid="telemetry-notice" :data-state="state">
    <h2><span class="tn-dot" aria-hidden="true">!</span>{{ notice.title(project) }}</h2>
    <p>{{ notice.lead }}</p>
    <p class="tn-where">Check, in the cluster that should be reporting:</p>
    <ol class="tn-steps">
      <li v-for="s in STEPS" :key="s.where">
        <strong>{{ s.ask }}</strong>
        <code class="tn-cmd">{{ s.where }}</code>
        <span class="tn-then">{{ s.then }}</span>
      </li>
    </ol>
    <p class="tn-foot">The namespace is <code>telemetry-agent</code> unless <code>nodeAgent.namespace</code> was changed. <button type="button" class="secondary" :disabled="checking" @click="check">{{ checking ? 'Checking…' : 'Check again' }}</button></p>
  </section>
</template>
