import { createApp } from 'vue'
import App from './App.vue'
import router from './router'
import { createClient } from './api/client'
import { createSession } from './api/session'
import { createAlertStore } from './alerts/store'
import './style.css'

const session = createSession()
const client = createClient({ session })
const alerts = createAlertStore({ client, session })

createApp(App).use(router).provide('session', session).provide('client', client).provide('alerts', alerts).mount('#app')
