import { createApp } from 'vue'
import App from './App.vue'
import { createAppRouter } from './router'
import { createClient } from './api/client'
import { createSession } from './api/session'
import { createAlertStore } from './alerts/store'
import { createCategoryStore } from './categories'
import { createIncidentSettingsStore } from './incidents/settings'
import { createFeatureStore } from './api/features'
import { createUiFeatures } from './api/uifeatures'
import { createApplicationsCache } from './api/applications'
import './style.css'

const session = createSession()
const client = createClient({ session })
const alerts = createAlertStore({ client, session })
const uiFeatures = createUiFeatures()
const router = createAppRouter(session, undefined, uiFeatures)
const categories = createCategoryStore({ session })
const incidentSettings = createIncidentSettingsStore({ session })
const appCache = createApplicationsCache()
const features = createFeatureStore({ client, session })

createApp(App).use(router).provide('session', session).provide('client', client).provide('alerts', alerts).provide('categories', categories).provide('incidentSettings', incidentSettings).provide('appCache', appCache).provide('features', features).provide('uiFeatures', uiFeatures).mount('#app')
