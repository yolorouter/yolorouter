import { defineStore } from 'pinia'
import * as modelsApi from '../api/models'
import type { Model, ModelCandidate, CreateCandidateInput, UpdateCandidateInput } from '../api/models'

interface ModelsState {
  list: Model[]
  loading: boolean
}

export const useModelsStore = defineStore('models', {
  state: (): ModelsState => ({ list: [], loading: false }),
  actions: {
    async fetchList() {
      this.loading = true
      try {
        const { list } = await modelsApi.listModels()
        this.list = list
      } finally {
        this.loading = false
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
