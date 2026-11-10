import { createApp } from 'vue'
import App from './App.vue'
import { createAppRouter } from './router'
import { createClient } from './api/client'
import { createSession } from './api/session'
import { createAlertStore } from './alerts/store'
import { createCategoryStore } from './categories'
import { createIncidentSettingsStore } from './incidents/settings'
import { createApplicationsCache } from './api/applications'
import './style.css'

const session = createSession()
const client = createClient({ session })
const alerts = createAlertStore({ client, session })
const router = createAppRouter(session)
const categories = createCategoryStore({ session })
const incidentSettings = createIncidentSettingsStore({ session })
const appCache = createApplicationsCache()

createApp(App).use(router).provide('session', session).provide('client', client).provide('alerts', alerts).provide('categories', categories).provide('incidentSettings', incidentSettings).provide('appCache', appCache).mount('#app')
