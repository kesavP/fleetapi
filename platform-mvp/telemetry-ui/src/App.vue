<script setup>
import { computed, inject, onBeforeUnmount, onMounted, watchEffect } from 'vue'

// The alert store evaluates on a timer for as long as the app is open, so the count shows on every page and in the tab title.
const alerts = inject('alerts', null)
const count = computed(() => (alerts ? alerts.state.alerts.filter((a) => a.status === 'firing').length : 0))
const worst = computed(() => (alerts && alerts.state.alerts.some((a) => a.status === 'firing' && a.severity === 'critical') ? 'critical' : 'warning'))

const baseTitle = typeof document !== 'undefined' ? document.title || 'Fleet telemetry' : 'Fleet telemetry'
watchEffect(() => {
  if (typeof document !== 'undefined') document.title = count.value ? `(${count.value}) ${baseTitle}` : baseTitle
})
onMounted(() => alerts && alerts.start())
onBeforeUnmount(() => alerts && alerts.stop())
</script>

<template>
  <header class="top">
    <h1>Fleet telemetry</h1>
    <p class="sub">What the node-agents send, per project: metrics, traces, logs, profiles and the calls between services.</p>
    <nav aria-label="Views">
      <router-link to="/">Connection</router-link>
      <router-link to="/services">Services</router-link>
      <router-link to="/map">Map</router-link>
      <router-link to="/traces">Traces</router-link>
      <router-link to="/logs">Logs</router-link>
      <router-link to="/profiles">Profiles</router-link>
      <router-link to="/alerts">Alerts<span v-if="count" class="badge" :class="worst" :aria-label="`${count} firing`">{{ count }}</span></router-link>
    </nav>
  </header>
  <main>
    <router-view />
  </main>
</template>
