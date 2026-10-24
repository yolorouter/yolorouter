// @vitest-environment happy-dom
//
// The model pricing page's skeleton baseline: the /model-pricing route's
// component renders its PageHeader shell and the shared EmptyState while the
// grouped comparison table has not landed yet (the next ticket replaces the
// body). Assertions read the zh-CN / en copy through the locale modules so a
// copy edit cannot fake a failure, while a wiring change (missing header
// piece, dropped empty state, unwired refresh) still fails.
import { afterEach, describe, expect, it } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import naive from 'naive-ui'
import { defineComponent } from 'vue'

import ModelPricingPage from './ModelPricingPage.vue'
import zhCN from '../../locales/zh-CN'
import en from '../../locales/en'

// App.vue nests pages under n-config-provider > n-message-provider; the same
// host here means the page mounts exactly as it does inside the shell.
const Host = defineComponent({
  components: { ModelPricingPage },
  template: '<NConfigProvider><NMessageProvider><ModelPricingPage /></NMessageProvider></NConfigProvider>',
})

function mountPage(locale: 'en' | 'zh-CN'): VueWrapper {
  const i18n = createI18n({
    legacy: false,
    locale,
    fallbackLocale: 'zh-CN',
    messages: { en, 'zh-CN': zhCN },
  })
  return mount(Host, { global: { plugins: [i18n, naive] } })
}

describe('ModelPricingPage skeleton', () => {
  let wrapper: VueWrapper | null = null

  afterEach(() => {
    wrapper?.unmount()
    wrapper = null
  })

  it('renders the page header with the pricing title and description (zh-CN)', () => {
    wrapper = mountPage('zh-CN')
    const title = wrapper.find('h1.page-header__title')
    expect(title.exists(), 'page header title renders').toBe(true)
    expect(title.text()).toContain(zhCN.modelPricing.pageTitle)
    expect(wrapper.text()).toContain(zhCN.modelPricing.pageDescription)
  })

  it('shows the shared empty state for the not-yet-loaded content', () => {
    wrapper = mountPage('zh-CN')
    const empty = wrapper.find('.empty-state')
    expect(empty.exists(), 'the shared EmptyState renders').toBe(true)
    expect(empty.text()).toContain(zhCN.modelPricing.emptyTitle)
    expect(empty.text()).toContain(zhCN.modelPricing.emptyDescription)
  })

  it('carries the refresh action and renders the en copy too', () => {
    wrapper = mountPage('en')
    const refresh = wrapper.findAll('button').find((b) => (b.text() ?? '').includes(en.modelPricing.refresh))
    expect(refresh, 'refresh button renders').toBeTruthy()
    expect(wrapper.text()).toContain(en.modelPricing.pageTitle)
    expect(wrapper.text()).toContain(en.modelPricing.pageDescription)
  })
})
