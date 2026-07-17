import { createApp } from 'vue'
import { createPinia } from 'pinia'
import './style.css'
import { i18n } from './i18n'
import { router } from './router'
import App from './App.vue'

createApp(App).use(createPinia()).use(i18n).use(router).mount('#app')
