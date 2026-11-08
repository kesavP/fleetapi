<script setup>
import { inject } from 'vue'
import ConnectionPanel from '../components/ConnectionPanel.vue'

// A new connection is judged at once, and a disconnect forgets the project's alerts.
const alerts = inject('alerts', null)
</script>

<template>
  <ConnectionPanel @connected="alerts && alerts.evaluate()" @disconnected="alerts && alerts.reset()" />
  <section class="panel">
    <h2>What comes next</h2>
    <p class="help">
      Metrics, traces, logs, profiles and the service map are added on top of this connection. Until then this page
      shows that the UI can reach the API with a read key.
    </p>
  </section>
</template>
