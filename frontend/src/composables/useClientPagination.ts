// frontend/src/composables/useClientPagination.ts
import { reactive } from 'vue'

export interface ClientPagination {
  page: number
  pageSize: number
  showSizePicker: boolean
  pageSizes: number[]
}

/**
 * Reactive pagination object + the two change handlers naive-ui's NDataTable
 * expects (@update:page / @update:page-size) for client-side paging. The data
 * is sliced in-page, so changing page size resets to page 1.
 *
 * Shared by the small admin-configured lists (providers / models / model
 * candidates) that are fetched in full. The server-paged ApiKeyListPage
 * deliberately does NOT use this — its pagination is computed from the store
 * and triggers a reload, a different shape.
 */
export function useClientPagination(defaultPageSize = 20) {
  const pagination = reactive<ClientPagination>({
    page: 1,
    pageSize: defaultPageSize,
    showSizePicker: true,
    pageSizes: [10, 20, 50],
  })
  function onPageChange(page: number) {
    pagination.page = page
  }
  function onPageSizeChange(pageSize: number) {
    pagination.pageSize = pageSize
    pagination.page = 1
  }
  return { pagination, onPageChange, onPageSizeChange }
}
