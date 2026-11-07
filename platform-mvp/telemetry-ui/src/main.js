import { createApp } from 'vue'
import App from './App.vue'
import router from './router'
import { createClient } from './api/client'
import { createSession } from './api/session'
import './style.css'

const session = createSession()
const client = createClient({ session })

createApp(App).use(router).provide('session', session).provide('client', client).mount('#app')
