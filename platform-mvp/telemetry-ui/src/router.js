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
import RecordedIncidentsView from './views/RecordedIncidentsView.vue'
import RecordedIncidentView from './views/RecordedIncidentView.vue'

// Where "/" goes depends on whether there is a connection: to the applications if so, else to the page that makes one.
export function makeRoutes(session, uiFeatures = null) {
  const home = () => (session && session.get() ? '/applications' : '/configuration/connection')
  return [
    { path: '/', name: 'home', redirect: home },
    // The project's settings: its connection, and how its data is stored. "/connection" was the whole page before it had tabs.
    { path: '/configuration', redirect: '/configuration/connection' },
    { path: '/configuration/connection', name: 'connection', component: ConfigurationView, meta: { tab: 'connection' } },
    { path: '/configuration/clusters', name: 'clusters', component: ConfigurationView, meta: { tab: 'clusters' } },
    { path: '/configuration/clickhouse', name: 'clickhouse', component: ConfigurationView, meta: { tab: 'clickhouse' } },
    { path: '/configuration/incidents', name: 'incident-rules', component: ConfigurationView, meta: { tab: 'incidents' } },
    // The detectors' rules, kept on the hub (the Incidents tab above keeps its own in this browser).
    { path: '/configuration/rules', name: 'rules', component: ConfigurationView, meta: { tab: 'rules' } },
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
    // The incidents the hub has recorded (its detector's, and people's through the API), apart from the ones /incidents works out from the metrics.
    { path: '/recorded', name: 'recorded-incidents', component: RecordedIncidentsView },
    { path: '/recorded/:id', name: 'recorded-incident', component: RecordedIncidentView },
    // The infrastructure costs: a feature the hub switches on (the page itself says so when it is off).
    // Its own chunk (read only when the page is opened), and only where this UI offers it: otherwise "/costs" is an unknown address.
    {
      path: '/costs',
      name: 'costs',
      component: () => import('./views/CostsView.vue'),
      beforeEnter: async () => {
        if (!uiFeatures) return true
        await uiFeatures.ready
        return uiFeatures.state.costs ? true : '/'
      },
    },
    { path: '/:rest(.*)*', redirect: '/' },
  ]
}

export function createAppRouter(session, history = createWebHistory(import.meta.env.BASE_URL), uiFeatures = null) {
  return createRouter({ history, routes: makeRoutes(session, uiFeatures) })
}
