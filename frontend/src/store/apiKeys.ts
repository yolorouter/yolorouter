import { defineStore } from 'pinia'
import * as apiKeysApi from '../api/apiKeys'
import type { APIKey, APIKeyPage, CreateAPIKeyInput, UpdateAPIKeyInput } from '../api/apiKeys'

interface ApiKeysState {
  list: APIKey[]
  total: number
  page: number
  pageSize: number
  query: string
  loading: boolean
  error: unknown | null
  // Monotonic token so a stale list response can't clobber a newer one if a
  // second fetchList() starts before the first resolves (same guard pattern
  // the models / providers stores use — see .claude/frontend-conventions.md
  // "Pinia store").
  lastFetchId: number
}

export const useApiKeysStore = defineStore('apiKeys', {
  state: (): ApiKeysState => ({
    list: [],
    total: 0,
    page: 1,
    pageSize: 20,
    query: '',
    loading: false,
    error: null,
    lastFetchId: 0,
  }),
  actions: {
    async fetchList() {
      const fetchId = ++this.lastFetchId
      this.loading = true
      this.error = null
      try {
        const res: APIKeyPage = await apiKeysApi.listAPIKeys(this.query, this.page, this.pageSize)
        // A newer fetchList() started while this one was in flight — its
        // result is authoritative, so leave state untouched.
        if (fetchId !== this.lastFetchId) return
        this.list = res.list
        this.total = res.total
      } catch (err) {
        if (fetchId !== this.lastFetchId) return
        this.error = err
        throw err
      } finally {
        if (fetchId === this.lastFetchId) this.loading = false
      }
    },
    setQuery(q: string) {
      this.query = q
      this.page = 1
    },
    setPage(page: number) {
      this.page = page
    },
    setPageSize(pageSize: number) {
      this.pageSize = pageSize
      this.page = 1
    },
    async create(input: CreateAPIKeyInput) {
      return apiKeysApi.createAPIKey(input)
    },
    async update(id: number, input: UpdateAPIKeyInput) {
      return apiKeysApi.updateAPIKey(id, input)
    },
    async revoke(id: number) {
      await apiKeysApi.revokeAPIKey(id)
    },
  },
})
