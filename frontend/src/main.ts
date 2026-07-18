import { createApp } from 'vue'
import { createPinia } from 'pinia'
import {
  create,
  NAlert,
  NButton,
  NConfigProvider,
  NDataTable,
  NDescriptions,
  NDescriptionsItem,
  NDialogProvider,
  NDrawer,
  NDrawerContent,
  NDropdown,
  NEmpty,
  NForm,
  NFormItem,
  NGlobalStyle,
  NInput,
  NLayout,
  NLayoutContent,
  NLayoutSider,
  NMessageProvider,
  NModal,
  NSelect,
  NSpace,
  NStep,
  NSteps,
  NSwitch,
  NTabPane,
  NTabs,
  NTag,
} from 'naive-ui'
import './styles/index.less'
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
    NConfigProvider,
    NDataTable,
    NDescriptions,
    NDescriptionsItem,
    NDialogProvider,
    NDrawer,
    NDrawerContent,
    NDropdown,
    NEmpty,
    NForm,
    NFormItem,
    NGlobalStyle,
    NInput,
    NLayout,
    NLayoutContent,
    NLayoutSider,
    NMessageProvider,
    NModal,
    NSelect,
    NSpace,
    NStep,
    NSteps,
    NSwitch,
    NTabPane,
    NTabs,
    NTag,
  ],
})

createApp(App).use(createPinia()).use(i18n).use(router).use(naive).mount('#app')
