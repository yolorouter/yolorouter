import { createRouter, createWebHistory } from 'vue-router'
import DefaultLayout from '../layouts/DefaultLayout.vue'

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', component: DefaultLayout },
  ],
})
