import { createApp } from 'vue'
import { createPinia } from 'pinia'
import {
  create,
  NAlert,
  NButton,
  NDialogProvider,
  NDropdown,
  NEmpty,
  NForm,
  NFormItem,
  NInput,
  NMessageProvider,
  NModal,
  NSpace,
} from 'naive-ui'
import './style.css'
import { i18n } from './i18n'
import { router } from './router'
import App from './App.vue'

// naive-ui's own on-demand registration (its `create()` helper) rather
// than the default `import naive from 'naive-ui'` + `.use(naive)`, which
// globally registers all ~90 components — only the ones actually
// rendered anywhere in this app need to be listed here. Add a component
// to this list the first time a template uses it.
const naive = create({
  components: [
    NAlert,
    NButton,
    NDialogProvider,
    NDropdown,
    NEmpty,
    NForm,
    NFormItem,
    NInput,
    NMessageProvider,
    NModal,
    NSpace,
  ],
})

createApp(App).use(createPinia()).use(i18n).use(router).use(naive).mount('#app')
