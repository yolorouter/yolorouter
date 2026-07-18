import { defineStore } from 'pinia'
import * as providersApi from '../api/providers'
import type {
  Provider,
  CreateProviderInput,
  UpdateProviderInput,
  CreateKeyInput,
  UpdateKeyInput,
  TestKeyResult,
} from '../api/providers'

interface ProvidersState {
  list: Provider[]
  loading: boolean
}

export const useProvidersStore = defineStore('providers', {
  state: (): ProvidersState => ({ list: [], loading: false }),
  actions: {
    async fetchList() {
      this.loading = true
      try {
        const { list } = await providersApi.listProviders()
        this.list = list
      } finally {
        this.loading = false
      }
    },
    async fetchDetail(id: number): Promise<Provider> {
      return providersApi.getProvider(id)
    },
    async create(input: CreateProviderInput): Promise<Provider> {
      const created = await providersApi.createProvider(input)
      await this.fetchList()
      return created
    },
    async update(id: number, input: UpdateProviderInput): Promise<Provider> {
      const updated = await providersApi.updateProvider(id, input)
      await this.fetchList()
      return updated
    },
    // Deliberately does NOT refetch the list: ProviderDetailPage is the
    // only caller, and it already reloads its own single-provider detail
    // right after calling this — refreshing the unrelated full list here
    // was a wasted round trip on every status toggle (simplify-review
    // efficiency finding). If a future caller needs the list refreshed
    // too, have that caller await fetchList() itself.
    async setStatus(id: number, enabled: boolean) {
      await providersApi.setProviderStatus(id, enabled)
    },
    async createKey(providerId: number, input: CreateKeyInput) {
      return providersApi.createProviderKey(providerId, input)
    },
    async updateKey(providerId: number, keyId: number, input: UpdateKeyInput) {
      return providersApi.updateProviderKey(providerId, keyId, input)
    },
    async reorderKey(providerId: number, keyId: number, direction: 'up' | 'down') {
      await providersApi.reorderProviderKey(providerId, keyId, direction)
    },
    async setKeyStatus(providerId: number, keyId: number, enabled: boolean) {
      await providersApi.setProviderKeyStatus(providerId, keyId, enabled)
    },
    async testKey(providerId: number, keyId: number) {
      return providersApi.testProviderKey(providerId, keyId)
    },
    async testAll(providerId: number, enabledKeyCount: number) {
      return providersApi.testAllProviderKeys(providerId, enabledKeyCount)
    },
    async testKeyPreview(baseUrl: string, apiKey: string, model: string): Promise<TestKeyResult> {
      return providersApi.testKeyPreview(baseUrl, apiKey, model)
    },
  },
})
