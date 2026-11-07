import { createRouter, createWebHistory } from 'vue-router'
import OverviewView from './views/OverviewView.vue'

// One route for now. The views for metrics, traces, logs, profiles and the map are added here.
export const routes = [
  { path: '/', name: 'overview', component: OverviewView },
  { path: '/:rest(.*)*', redirect: '/' },
]

export default createRouter({ history: createWebHistory(import.meta.env.BASE_URL), routes })
