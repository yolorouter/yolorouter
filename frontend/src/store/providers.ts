import { defineStore } from 'pinia'
import * as providersApi from '../api/providers'
import type {
  Provider,
  CreateProviderInput,
  UpdateProviderInput,
  CreateKeyInput,
  UpdateKeyInput,
  TestKeyResult,
  ModelCatalogueResult,
} from '../api/providers'

interface ProvidersState {
  list: Provider[]
  loading: boolean
  // One-shot handoff from "create provider" to "import models": the list page
  // sets this before navigating to the new provider's detail page, which
  // consumes it once and auto-opens the import dialog. Deliberately NOT a URL
  // query: DefaultLayout keys its router-view by fullPath, so any query change
  // remounts the page and would wipe the just-opened dialog.
  pendingImportProviderId: number | null
}

export const useProvidersStore = defineStore('providers', {
  state: (): ProvidersState => ({ list: [], loading: false, pendingImportProviderId: null }),
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
      // Best-effort refresh: the create already committed on the server, so a
      // failed list refresh must not surface as a create failure (which would
      // report an error for a change that actually landed). The list refreshes
      // again on the next navigation/fetch.
      await this.refreshListBestEffort()
      return created
    },
    async update(id: number, input: UpdateProviderInput): Promise<Provider> {
      const updated = await providersApi.updateProvider(id, input)
      // Best-effort refresh (see create): a failed list refresh must not mask a
      // committed PATCH — the edit modal would otherwise report failure and
      // stay open even though the server persisted the change (and may have
      // invalidated keys).
      await this.refreshListBestEffort()
      return updated
    },
    async refreshListBestEffort() {
      try {
        await this.fetchList()
      } catch {
        // Ignore: the underlying mutation already succeeded; the list will
        // refresh on the next navigation/fetch.
      }
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
    async createKey(providerId: number, input: CreateKeyInput, destinationCount: number) {
      return providersApi.createProviderKey(providerId, input, destinationCount)
    },
    async updateKey(providerId: number, keyId: number, input: UpdateKeyInput, destinationCount: number) {
      return providersApi.updateProviderKey(providerId, keyId, input, destinationCount)
    },
    async reorderKey(providerId: number, keyId: number, direction: 'up' | 'down') {
      await providersApi.reorderProviderKey(providerId, keyId, direction)
    },
    async setKeyStatus(providerId: number, keyId: number, enabled: boolean) {
      await providersApi.setProviderKeyStatus(providerId, keyId, enabled)
    },
    async deleteKey(providerId: number, keyId: number) {
      await providersApi.deleteProviderKey(providerId, keyId)
    },
    async deleteProvider(id: number) {
      await providersApi.deleteProvider(id)
    },
    async testKey(providerId: number, keyId: number, destinationCount: number) {
      return providersApi.testProviderKey(providerId, keyId, destinationCount)
    },
    async testAll(providerId: number) {
      return providersApi.testAllProviderKeys(providerId)
    },
    async testKeyPreview(
      baseUrl: string,
      apiKey: string,
      model: string,
      providerType: string,
      protocolEndpoints = '',
    ): Promise<TestKeyResult> {
      return providersApi.testKeyPreview(baseUrl, apiKey, model, providerType, protocolEndpoints)
    },
    async listModelsPreview(baseUrl: string, apiKey: string, providerType: string): Promise<ModelCatalogueResult> {
      return providersApi.listModelsPreview(baseUrl, apiKey, providerType)
    },
    async listModelsForProvider(id: number): Promise<ModelCatalogueResult> {
      return providersApi.listModelsForProvider(id)
    },
  },
})
