<script setup>
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import ClickHouseTab from '../components/ClickHouseTab.vue'
import IncidentSettingsTab from '../components/IncidentSettingsTab.vue'
import ConnectionView from './ConnectionView.vue'

// The project's settings, one tab each. The tab is a page of its own (an address), so it can be linked and the back button works.
const TABS = [
  { to: '/configuration/connection', key: 'connection', label: 'Connection' },
  { to: '/configuration/incidents', key: 'incidents', label: 'Incidents' },
  { to: '/configuration/clickhouse', key: 'clickhouse', label: 'ClickHouse' },
]
const route = useRoute()
const tab = computed(() => (route.meta && route.meta.tab) || 'connection')
</script>

<template>
  <div class="page-head"><h1>Configuration</h1></div>
  <nav class="tabs" aria-label="Configuration">
    <router-link v-for="t in TABS" :key="t.key" :to="t.to" class="tab" :class="{ active: tab === t.key }" :aria-current="tab === t.key ? 'page' : null">{{ t.label }}</router-link>
  </nav>
  <IncidentSettingsTab v-if="tab === 'incidents'" />
  <ClickHouseTab v-else-if="tab === 'clickhouse'" />
  <ConnectionView v-else />
</template>
