<script setup>
import { computed, inject, ref } from 'vue'
import { validProject } from '../api/client'

// Takes the client and session by injection so a test can hand it fakes.
const props = defineProps({ client: { type: Object, default: null }, session: { type: Object, default: null } })
const emit = defineEmits(['connected', 'disconnected'])
const client = props.client || inject('client')
const session = props.session || inject('session')

const saved = session.get()
const project = ref(saved ? saved.project : '')
const key = ref('')
const connected = ref(saved ? { project: saved.project, metrics: null } : null)
const busy = ref(false)
const error = ref('')

const projectProblem = computed(() =>
  project.value && !validProject(project.value) ? 'Lowercase letters, digits and dashes only.' : '',
)
const canConnect = computed(() => !busy.value && project.value && key.value && !projectProblem.value)

async function connect() {
  if (!canConnect.value) return
  busy.value = true
  error.value = ''
  // Try the credentials before keeping them: a wrong key must not be left in the session.
  const previous = session.get()
  session.set({ project: project.value, key: key.value })
  try {
    const names = await client.metrics()
    connected.value = { project: project.value, metrics: names.length }
    key.value = '' // the form does not hold on to the key once it is stored
    emit('connected', connected.value)
  } catch (e) {
    if (previous) session.set(previous)
    else session.clear()
    connected.value = null
    error.value = e.message
  } finally {
    busy.value = false
  }
}

function disconnect() {
  session.clear()
  connected.value = null
  key.value = ''
  error.value = ''
  emit('disconnected')
}
</script>

<template>
  <section class="panel" aria-labelledby="conn-title">
    <h2 id="conn-title">Connect to a project</h2>

    <div v-if="connected" class="connected" role="status">
      <p>
        Connected to project <strong>{{ connected.project }}</strong
        ><span v-if="connected.metrics !== null"> &middot; {{ connected.metrics }} metric names visible to this key</span>.
      </p>
      <button type="button" class="secondary" @click="disconnect">Disconnect</button>
    </div>

    <form v-else novalidate @submit.prevent="connect">
      <label for="project">Project</label>
      <input
        id="project"
        v-model.trim="project"
        type="text"
        autocomplete="off"
        spellcheck="false"
        :aria-invalid="projectProblem ? 'true' : 'false'"
        aria-describedby="project-help"
      />
      <p id="project-help" class="help" :class="{ bad: projectProblem }">
        {{ projectProblem || 'The cluster or site whose telemetry you want, as named when its agents were set up.' }}
      </p>

      <label for="key">Read key</label>
      <input id="key" v-model="key" type="password" autocomplete="off" spellcheck="false" aria-describedby="key-help" />
      <p id="key-help" class="help">A project's read key, not its ingest key. It is kept for this tab only.</p>

      <button type="submit" :disabled="!canConnect">{{ busy ? 'Connecting…' : 'Connect' }}</button>
      <p v-if="error" class="error" role="alert">{{ error }}</p>
    </form>
  </section>
</template>
