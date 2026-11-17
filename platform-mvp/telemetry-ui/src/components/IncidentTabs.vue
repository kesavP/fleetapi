<script setup>
import { computed } from 'vue'
import { useRoute } from 'vue-router'

// The two lists of incidents: the ones this page works out from the metrics in the browser, and the ones the hub's detector has recorded.
const route = useRoute()
const recorded = computed(() => route.path === '/recorded' || route.path.startsWith('/recorded/'))
</script>

<template>
  <nav class="tabs" aria-label="Incident lists">
    <router-link to="/incidents" class="tab" :class="{ active: !recorded }" :aria-current="!recorded ? 'page' : null">From the metrics</router-link>
    <router-link to="/recorded" class="tab" :class="{ active: recorded }" :aria-current="recorded ? 'page' : null">Recorded by the hub</router-link>
  </nav>
</template>
