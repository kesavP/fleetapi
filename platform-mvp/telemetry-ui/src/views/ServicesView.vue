<script setup>
import { computed, inject, onMounted, ref } from 'vue'
import { WINDOWS, loadServices } from '../api/services'

// Takes the client and session by injection so a test can hand it fakes.
const props = defineProps({ client: { type: Object, default: null }, session: { type: Object, default: null } })
const client = props.client || inject('client')
const session = props.session || inject('session')

const connected = computed(() => session.get())
const seconds = ref(WINDOWS[1].seconds)
const filter = ref('')
const services = ref([])
const errors = ref({})
const loading = ref(false)
const loaded = ref(false)
const fatal = ref('')

const shown = computed(() => {
  const f = filter.value.trim().toLowerCase()
  return f ? services.value.filter((s) => s.name.toLowerCase().includes(f)) : services.value
})
const failedSources = computed(() => Object.keys(errors.value))
// Every source failed: nothing can be shown, and the first reason says why (usually a bad or expired key).
const allFailed = computed(() => loaded.value && failedSources.value.length === 3)

async function load() {
  if (!connected.value) return
  loading.value = true
  fatal.value = ''
  try {
    const r = await loadServices(client, { seconds: seconds.value })
    services.value = r.services
    errors.value = r.errors
  } catch (e) {
    fatal.value = e.message
  } finally {
    loading.value = false
    loaded.value = true
  }
}

onMounted(load)
</script>

<template>
  <section class="panel">
    <h2>Services</h2>
    <p v-if="!connected" class="help">
      Not connected. <router-link to="/">Connect with a project and its read key</router-link> first.
    </p>
    <template v-else>
      <p class="help">
        Project <strong>{{ connected.project }}</strong>. A service is listed when it has traces or logs in the
        window, or profiles at any time.
      </p>
      <div class="toolbar">
        <label for="window">Window</label>
        <select id="window" v-model.number="seconds" @change="load">
          <option v-for="w in WINDOWS" :key="w.seconds" :value="w.seconds">Last {{ w.label }}</option>
        </select>
        <label for="filter">Filter</label>
        <input id="filter" v-model="filter" type="search" placeholder="name contains…" autocomplete="off" />
        <button type="button" class="secondary" :disabled="loading" @click="load">
          {{ loading ? 'Loading…' : 'Refresh' }}
        </button>
      </div>

      <p v-if="fatal" class="error" role="alert">{{ fatal }}</p>
      <p v-else-if="allFailed" class="error" role="alert">
        Nothing could be loaded: {{ Object.values(errors)[0] }}
        <router-link to="/">Check the connection.</router-link>
      </p>
      <template v-else>
        <p v-if="failedSources.length" class="warn" role="status">
          Not loaded: {{ failedSources.join(', ') }}. The list may be incomplete.
        </p>
        <p v-if="loaded && !loading && !services.length" class="help">
          No services in this window. Widen it, or check that the node-agents are sending.
        </p>
        <p v-else-if="loaded && !shown.length" class="help">No service matches "{{ filter }}".</p>
        <table v-if="shown.length" class="services">
          <thead>
            <tr><th>Service</th><th>Traces</th><th>Logs</th><th>Profiles</th></tr>
          </thead>
          <tbody>
            <tr v-for="s in shown" :key="s.name">
              <td class="name"><router-link :to="{ name: 'service', params: { name: s.name } }">{{ s.name }}</router-link></td>
              <td><span v-if="s.traces" class="yes" aria-label="has traces">●</span><span v-else class="no" aria-label="no traces">–</span></td>
              <td><span v-if="s.logs" class="yes" aria-label="has logs">●</span><span v-else class="no" aria-label="no logs">–</span></td>
              <td>{{ s.profiles.length ? s.profiles.join(', ') : '–' }}</td>
            </tr>
          </tbody>
        </table>
        <p v-if="shown.length" class="help count">{{ shown.length }} of {{ services.length }} services</p>
      </template>
    </template>
  </section>
</template>
