<script setup>
import { computed, inject, onBeforeUnmount, onMounted, watchEffect } from 'vue'
import SideNav from './components/SideNav.vue'

// The alert store evaluates on a timer for as long as the app is open (or reads the alert service), so the count shows on every page
// in the menu and in the tab title.
const alerts = inject('alerts', null)
const count = computed(() => (alerts ? alerts.state.alerts.filter((a) => a.status === 'firing').length : 0))

const baseTitle = typeof document !== 'undefined' ? document.title || 'Fleet telemetry' : 'Fleet telemetry'
watchEffect(() => {
  if (typeof document !== 'undefined') document.title = count.value ? `(${count.value}) ${baseTitle}` : baseTitle
})
onMounted(() => alerts && alerts.start())
onBeforeUnmount(() => alerts && alerts.stop())
</script>

<template>
  <div class="shell">
    <SideNav />
    <main class="content">
      <router-view />
    </main>
  </div>
</template>
