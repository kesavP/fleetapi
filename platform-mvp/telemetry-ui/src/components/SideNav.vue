<script setup>
import { computed, inject, onBeforeUnmount, ref } from 'vue'
import { useRoute } from 'vue-router'
import AppIcon from './AppIcon.vue'

// The main menu: a sidebar, one entry per page. The project it is connected to is at the bottom, with the way to change it.
// Takes the session and alert store by injection so a test can hand it fakes.
const props = defineProps({ session: { type: Object, default: null }, alerts: { type: Object, default: null } })
const session = props.session || inject('session')
const alerts = props.alerts || inject('alerts', null)

const ITEMS = [
  { to: '/applications', label: 'Applications', icon: 'apps' },
  { to: '/map', label: 'Map', icon: 'map' },
  { to: '/traces', label: 'Traces', icon: 'traces' },
  { to: '/logs', label: 'Logs', icon: 'logs' },
  { to: '/profiles', label: 'Profiles', icon: 'profiles' },
  { to: '/alerts', label: 'Alerts', icon: 'alerts', badge: true },
]

// A page beneath a menu entry (an application, a trace) keeps its entry lit. Vue Router's own "active" class only follows nested
// routes, and these are siblings, so it is decided from the address here.
const route = useRoute()
const isActive = (to) => route.path === to || route.path.startsWith(to + '/')

const project = ref((session.get() || {}).project || '')
const stop = session.subscribe(() => (project.value = (session.get() || {}).project || ''))
onBeforeUnmount(stop)

const firing = computed(() => (alerts ? alerts.state.alerts.filter((a) => a.status === 'firing') : []))
const worst = computed(() => (firing.value.some((a) => a.severity === 'critical') ? 'critical' : 'warning'))

// Collapsed to icons, remembered for the next visit. A preference, not a secret, so it may live in localStorage.
const KEY = 'fleet-ui.sidebar'
const read = () => {
  try {
    return window.localStorage.getItem(KEY) === 'collapsed'
  } catch {
    return false
  }
}
const collapsed = ref(read())
function toggle() {
  collapsed.value = !collapsed.value
  try {
    window.localStorage.setItem(KEY, collapsed.value ? 'collapsed' : 'open')
  } catch {
    // not remembered; it still works for this visit
  }
}
</script>

<template>
  <aside class="side" :class="{ collapsed }">
    <div class="brand">
      <span class="logo" aria-hidden="true">F</span>
      <span class="brand-text"><strong>Fleet</strong><small>telemetry</small></span>
    </div>

    <nav aria-label="Main menu" class="menu">
      <router-link v-for="i in ITEMS" :key="i.to" :to="i.to" class="item" :class="{ active: isActive(i.to) }" :aria-current="isActive(i.to) ? 'page' : null" :title="collapsed ? i.label : null">
        <AppIcon :name="i.icon" />
        <span class="label">{{ i.label }}</span>
        <span v-if="i.badge && firing.length" class="badge" :class="worst" :aria-label="`${firing.length} firing`">{{ firing.length }}</span>
      </router-link>
    </nav>

    <div class="foot">
      <router-link to="/connection" class="item project" :class="{ active: isActive('/connection') }" :aria-current="isActive('/connection') ? 'page' : null" :title="collapsed ? `Project ${project || '(not connected)'}` : null">
        <AppIcon name="connection" />
        <span class="label">
          <small>Project</small>
          <strong data-testid="project">{{ project || 'Not connected' }}</strong>
        </span>
      </router-link>
      <button type="button" class="item collapse" :aria-label="collapsed ? 'Expand the menu' : 'Collapse the menu'" :aria-expanded="!collapsed" @click="toggle">
        <AppIcon :name="collapsed ? 'expand' : 'collapse'" />
        <span class="label">Collapse</span>
      </button>
    </div>
  </aside>
</template>
