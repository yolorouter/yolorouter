// frontend/src/utils/ccswitchExport.ts
//
// Pure planning logic for CC-Switch export dialogs. The dialog components own
// presentation and fetching; this module owns the decisions — which model
// choices to offer, how each is annotated, which one is preselected, and when
// to degrade to manual entry — so those rules are testable without a DOM.
//
// One source per fact, reused across the rules below:
//   - discovered names come from the gateway's own /v1/models AUTHED WITH THE
//     KEY being exported: the only list correct for both roles (members
//     cannot read the admin catalog) and the only one reflecting the key's
//     real scope. The gateway lists management-enabled models only.
//   - the admin catalog knows running_status but not the key's scope, so it
//     only ever annotates or backfills — never replaces a discovery result.

// Structural slice of the admin catalog rows the planner reads. The full
// Model type satisfies it; tests can build minimal rows.
export interface CCSwitchCatalogModel {
  id: number
  name: string
  running_status: string
}

// The pair the export dialog hands its opener on confirm — the deep-link
// params minus the profile name, which the opening page owns (each page
// names its rows its own way).
export interface CCSwitchConfirmPayload {
  // Undefined only on the legacy placeholder path (key predates plaintext
  // storage); the composable substitutes its placeholder.
  apiKey?: string
  // Undefined only when manual entry was left empty — the deep link's
  // model param is optional.
  model?: string
}

export interface CCSwitchModelChoice {
  name: string
  // Tri-state: true/false = the admin catalog marks the model available or
  // not; null = no catalog (member view) — availability unknown.
  available: boolean | null
}

export type CCSwitchModelPlan =
  | { mode: 'select'; choices: CCSwitchModelChoice[]; preselect: string }
  | { mode: 'manual' }

export interface CCSwitchModelPlanInput {
  // Names the gateway returned for this key, or null when discovery could
  // not run at all — transient failure, or a legacy key with no readable
  // plaintext to authenticate with.
  discovered: string[] | null
  // The viewer's admin catalog, or null when the viewer cannot read one
  // (member sessions).
  catalog: CCSwitchCatalogModel[] | null
  key: { allow_all_models: boolean; model_ids: number[] }
}

// planCCSwitchModelChoices decides what the model picker offers:
//
//   discovery result (non-empty)  →  those names, annotated with catalog
//                                    availability when a catalog exists
//   else admin catalog scoped to the key (non-empty)
//                                 →  the scoped catalog names, annotated
//   else                          →  manual entry (may stay empty — the
//                                    deep link's model param is optional)
//
// Preselection keeps the pre-dialog auto-pick rule users had: the first
// choice the catalog marks available, else the first choice. Members have no
// catalog, so discovery order alone decides for them.
export function planCCSwitchModelChoices(input: CCSwitchModelPlanInput): CCSwitchModelPlan {
  const { discovered, catalog, key } = input

  if (discovered && discovered.length > 0) {
    const choices: CCSwitchModelChoice[] = discovered.map((name) => ({
      name,
      available: catalog
        ? catalog.some((m) => m.name === name && m.running_status === 'available')
        : null,
    }))
    return { mode: 'select', choices, preselect: preselectAmong(choices) }
  }

  if (catalog) {
    const scoped = key.allow_all_models
      ? catalog
      : catalog.filter((m) => key.model_ids.includes(m.id))
    if (scoped.length > 0) {
      const choices: CCSwitchModelChoice[] = scoped.map((m) => ({
        name: m.name,
        available: m.running_status === 'available',
      }))
      return { mode: 'select', choices, preselect: preselectAmong(choices) }
    }
  }

  return { mode: 'manual' }
}

function preselectAmong(choices: CCSwitchModelChoice[]): string {
  return choices.find((c) => c.available === true)?.name ?? choices[0].name
}
