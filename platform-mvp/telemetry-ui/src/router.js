import { createRouter, createWebHistory } from 'vue-router'
import ConnectionView from './views/ConnectionView.vue'
import ApplicationsView from './views/ApplicationsView.vue'
import ServiceView from './views/ServiceView.vue'
import MapView from './views/MapView.vue'
import TracesView from './views/TracesView.vue'
import TraceView from './views/TraceView.vue'
import LogsView from './views/LogsView.vue'
import ProfilesView from './views/ProfilesView.vue'
import AlertsView from './views/AlertsView.vue'

// Where "/" goes depends on whether there is a connection: to the applications if so, else to the page that makes one.
export function makeRoutes(session) {
  const home = () => (session && session.get() ? '/applications' : '/connection')
  return [
    { path: '/', name: 'home', redirect: home },
    { path: '/connection', name: 'connection', component: ConnectionView },
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
    { path: '/:rest(.*)*', redirect: '/' },
  ]
}

export function createAppRouter(session, history = createWebHistory(import.meta.env.BASE_URL)) {
  return createRouter({ history, routes: makeRoutes(session) })
}
