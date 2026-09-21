// frontend/src/utils/modelFilterOptions.ts
import type { SelectOption } from 'naive-ui'

// Merges the analytics page's model-filter options: the admin-configured
// catalog plus the distinct names that recently carried traffic. A deleted
// model stays selectable for as long as its history does — logs and stats
// keep the name, so by-name filtering keeps working on it. A name present
// in both lists is one and the same model (a same-name recreation merges
// with its own history), so the overlap collapses to a single option.
//
// historyNames is null when the history endpoint failed: the dropdown then
// degrades to the catalog alone instead of losing its options entirely —
// the same degrade-don't-break rule loadFilterOptions applies to the other
// catalogs.
export function mergeModelFilterOptions(
  catalogNames: string[],
  historyNames: string[] | null,
): SelectOption[] {
  const names = new Set<string>()
  for (const name of [...catalogNames, ...(historyNames ?? [])]) {
    // Legacy log rows can carry an empty model name. An empty option would
    // render as a blank row and filter nothing (the query builder skips
    // empty values), so drop it — the same guard the member path applies
    // to its report-derived options.
    if (name) names.add(name)
  }
  // One shared ordering for both sources: the catalog arrives in creation
  // order and the history list in SQL ASC, and either alone would shuffle
  // the merge as its source changes. Code-unit order, not locale compare,
  // so every client sorts the same list the same way.
  return [...names].sort().map((name) => ({ label: name, value: name }))
}
