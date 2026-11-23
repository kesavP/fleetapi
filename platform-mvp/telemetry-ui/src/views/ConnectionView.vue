<script setup>
import { inject } from 'vue'
import { useRouter } from 'vue-router'
import ConnectionPanel from '../components/ConnectionPanel.vue'

// A new connection is judged at once and opens the applications; a disconnect forgets the project's alerts.
const alerts = inject('alerts', null)
const router = useRouter()
function connected() {
  if (alerts) alerts.evaluate()
  router.push('/applications')
}
</script>

<template>
  <ConnectionPanel @connected="connected" @disconnected="alerts && alerts.reset()" />
</template>
