import { createRouter, createWebHistory } from 'vue-router'
import OverviewView from './views/OverviewView.vue'
import ServicesView from './views/ServicesView.vue'
import ServiceView from './views/ServiceView.vue'
import MapView from './views/MapView.vue'
import TracesView from './views/TracesView.vue'
import TraceView from './views/TraceView.vue'
import LogsView from './views/LogsView.vue'
import ProfilesView from './views/ProfilesView.vue'
import AlertsView from './views/AlertsView.vue'

// The views for metrics, traces, logs, profiles and the map are added here as they are built.
export const routes = [
  { path: '/', name: 'overview', component: OverviewView },
  { path: '/services', name: 'services', component: ServicesView },
  { path: '/services/:name', name: 'service', component: ServiceView },
  { path: '/map', name: 'map', component: MapView },
  { path: '/traces', name: 'traces', component: TracesView },
  { path: '/traces/:id', name: 'trace', component: TraceView },
  { path: '/logs', name: 'logs', component: LogsView },
  { path: '/profiles', name: 'profiles', component: ProfilesView },
  { path: '/alerts', name: 'alerts', component: AlertsView },
  { path: '/:rest(.*)*', redirect: '/' },
]

export default createRouter({ history: createWebHistory(import.meta.env.BASE_URL), routes })
