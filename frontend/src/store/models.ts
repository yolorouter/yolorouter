import { defineStore } from 'pinia'
import * as modelsApi from '../api/models'
import type { Model, ModelCandidate, CreateCandidateInput, UpdateCandidateInput } from '../api/models'

interface ModelsState {
  list: Model[]
  loading: boolean
  error: unknown | null
  // Monotonic token used by fetchList() to ignore stale responses. Each
  // call captures its value at start and, after awaiting, writes back to
  // state only if it is still the latest. Without this guard an older
  // failed request can clobber a newer successful one — e.g. the user
  // leaves the Models list while a request is in flight, then
  // ProviderDetailPage's onMounted calls fetchList() again on the same
  // store; if the newer request resolves before the older one rejects,
  // the stale rejection overwrites valid rows with a network error
  // (codex review finding).
  lastFetchId: number
}

export const useModelsStore = defineStore('models', {
  state: (): ModelsState => ({ list: [], loading: false, error: null, lastFetchId: 0 }),
  actions: {
    async fetchList() {
      const fetchId = ++this.lastFetchId
      this.loading = true
      this.error = null
      try {
        const { list } = await modelsApi.listModels()
        // A newer fetchList() started while this one was in flight — its
        // result is authoritative, so leave list/loading untouched.
        if (fetchId !== this.lastFetchId) return
        this.list = list
      } catch (err) {
        // Same staleness guard on the failure path: a stale error must not
        // overwrite a newer success, nor surface a misleading toast to a
        // caller that has already moved on.
        if (fetchId !== this.lastFetchId) return
        this.error = err
        throw err
      } finally {
        if (fetchId === this.lastFetchId) {
          this.loading = false
        }
      }
    },
    async fetchDetail(id: number): Promise<Model> {
      return modelsApi.getModel(id)
    },
    async create(name: string): Promise<Model> {
      const created = await modelsApi.createModel(name)
      await this.fetchList()
      return created
    },
    async update(id: number, name: string): Promise<Model> {
      return modelsApi.updateModel(id, name)
    },
    async setStatus(id: number, enabled: boolean) {
      await modelsApi.setModelStatus(id, enabled)
    },
    async testMapping(modelId: number, providerId: number, providerModelName: string, testType: 'basic' | 'streaming' | 'function_calling') {
      return modelsApi.testCandidateMapping(modelId, providerId, providerModelName, testType)
    },
    async createCandidate(modelId: number, input: CreateCandidateInput): Promise<ModelCandidate> {
      return modelsApi.createCandidate(modelId, input)
    },
    async updateCandidate(modelId: number, candidateId: number, input: UpdateCandidateInput): Promise<ModelCandidate> {
      return modelsApi.updateCandidate(modelId, candidateId, input)
    },
    async reorderCandidate(modelId: number, candidateId: number, direction: 'up' | 'down') {
      await modelsApi.reorderCandidate(modelId, candidateId, direction)
    },
    async setCandidateStatus(modelId: number, candidateId: number, enabled: boolean) {
      await modelsApi.setCandidateStatus(modelId, candidateId, enabled)
    },
    async testCandidate(modelId: number, candidateId: number, testType: 'basic' | 'streaming' | 'function_calling'): Promise<ModelCandidate> {
      return modelsApi.testCandidate(modelId, candidateId, testType)
    },
    async deleteCandidate(modelId: number, candidateId: number) {
      await modelsApi.deleteCandidate(modelId, candidateId)
    },
  },
})
