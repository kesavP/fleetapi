import { createRouter, createWebHistory } from 'vue-router'
import ConfigurationView from './views/ConfigurationView.vue'
import ApplicationsView from './views/ApplicationsView.vue'
import ServiceView from './views/ServiceView.vue'
import MapView from './views/MapView.vue'
import TracesView from './views/TracesView.vue'
import TraceView from './views/TraceView.vue'
import LogsView from './views/LogsView.vue'
import ProfilesView from './views/ProfilesView.vue'
import AlertsView from './views/AlertsView.vue'
import IncidentsView from './views/IncidentsView.vue'
import IncidentView from './views/IncidentView.vue'

// Where "/" goes depends on whether there is a connection: to the applications if so, else to the page that makes one.
export function makeRoutes(session) {
  const home = () => (session && session.get() ? '/applications' : '/configuration/connection')
  return [
    { path: '/', name: 'home', redirect: home },
    // The project's settings: its connection, and how its data is stored. "/connection" was the whole page before it had tabs.
    { path: '/configuration', redirect: '/configuration/connection' },
    { path: '/configuration/connection', name: 'connection', component: ConfigurationView, meta: { tab: 'connection' } },
    { path: '/configuration/clickhouse', name: 'clickhouse', component: ConfigurationView, meta: { tab: 'clickhouse' } },
    { path: '/configuration/incidents', name: 'incident-rules', component: ConfigurationView, meta: { tab: 'incidents' } },
    { path: '/connection', redirect: '/configuration/connection' },
    { path: '/applications', name: 'applications', component: ApplicationsView },
    { path: '/applications/:name', name: 'application', component: ServiceView },
    // Applications were called services: old links and bookmarks still arrive.
    { path: '/services', redirect: '/applications' },
    { path: '/services/:name', redirect: (to) => ({ name: 'application', params: to.params }) },
    { path: '/map', name: 'map', component: MapView },
    { path: '/traces', name: 'traces', component: TracesView },
    { path: '/traces/:id', name: 'trace', component: TraceView },
    { path: '/logs', name: 'logs', component: LogsView },
    { path: '/profiles', name: 'profiles', component: ProfilesView },
    { path: '/alerts', name: 'alerts', component: AlertsView },
    { path: '/incidents', name: 'incidents', component: IncidentsView },
    { path: '/incidents/:id', name: 'incident', component: IncidentView },
    { path: '/:rest(.*)*', redirect: '/' },
  ]
}

export function createAppRouter(session, history = createWebHistory(import.meta.env.BASE_URL)) {
  return createRouter({ history, routes: makeRoutes(session) })
}
