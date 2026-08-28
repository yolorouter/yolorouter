// Builds the copy for danger-action confirm dialogs and the model impact tab.
// Every builder degrades to a generic warning when the impact endpoint fails:
// a broken preview must never block the action itself.
import { getModelImpact, type ModelImpact, type ModelImpactKey } from '../api/models'
import { getProviderImpact, type ProviderImpact } from '../api/providers'
import type { ConfirmDisableCopy } from '../composables/useConfirmedStatusToggle'

type Translate = (key: string, values?: Record<string, unknown>) => string

// In a dialog, names beyond this many collapse into a "+N" suffix — a
// warning, not a report. The model impact tab passes fullNames instead, so
// the complete list is always reachable there.
const DIALOG_MAX_NAMES = 5

function joinNames(names: string[], limit?: number): string {
  if (limit === undefined || names.length <= limit) return names.join(', ')
  return `${names.slice(0, limit).join(', ')} (+${names.length - limit})`
}

function keyDisplayName(key: ModelImpactKey): string {
  return key.remark || key.key_prefix
}

// modelImpactOverview is the shared factual part: who references the model and
// how much live traffic asks for it. The disable confirm shows it truncated;
// the impact tab shows it with fullNames.
export function modelImpactOverview(t: Translate, impact: ModelImpact, fullNames = false): string {
  const limit = fullNames ? undefined : DIALOG_MAX_NAMES
  const lines: string[] = []
  if (impact.allowlisted_keys.length > 0) {
    lines.push(
      t('models.impactKeys', {
        count: impact.allowlisted_keys.length,
        names: joinNames(impact.allowlisted_keys.map(keyDisplayName), limit),
      }),
    )
  } else {
    lines.push(t('models.impactKeysNone'))
  }
  if (impact.allow_all_key_count > 0) {
    lines.push(t('models.impactAllowAll', { count: impact.allow_all_key_count }))
  }
  lines.push(t('models.impactTraffic', { count: impact.recent_request_count, days: impact.recent_window_days }))
  return lines.join('\n')
}

async function modelDisableContent(id: number, t: Translate): Promise<string> {
  const intro = t('models.impactDisableIntro')
  try {
    const impact = await getModelImpact(id)
    return [intro, modelImpactOverview(t, impact), t('common.confirmContinue')].join('\n')
  } catch {
    return [intro, t('common.confirmContinue')].join('\n')
  }
}

// modelDisableCopy / providerDisableCopy assemble the whole confirm-dialog
// copy, so the four pages that show these dialogs each pass one call instead
// of restating the same four-field object.
export async function modelDisableCopy(id: number, t: Translate): Promise<ConfirmDisableCopy> {
  return {
    title: t('models.confirmDisableModelTitle'),
    content: await modelDisableContent(id, t),
    positiveText: t('models.statusDisabled'),
    negativeText: t('models.cancel'),
  }
}

export async function modelRenameContent(id: number, oldName: string, t: Translate): Promise<string> {
  const intro = t('models.impactRenameIntro', { name: oldName })
  try {
    const impact = await getModelImpact(id)
    return [
      intro,
      t('models.impactTraffic', { count: impact.recent_request_count, days: impact.recent_window_days }),
      t('common.confirmContinue'),
    ].join('\n')
  } catch {
    return [intro, t('common.confirmContinue')].join('\n')
  }
}

async function providerDisableContent(id: number, t: Translate): Promise<string> {
  const intro = t('providers.confirmDisableProviderContent')
  try {
    const impact = await getProviderImpact(id)
    return [intro, ...providerImpactLines(impact, t).lines, t('common.confirmContinue')].join('\n')
  } catch {
    return [intro, t('common.confirmContinue')].join('\n')
  }
}

// A projected impact answer: display lines plus the stranded-model count
// that decides whether a dialog escalates its tone.
export interface ProviderImpactSummary {
  lines: string[]
  strandedCount: number
}

// The routing consequences shared by the provider disable dialog and the
// provider delete modal: affected models, the ones left with no routable
// source, and the caller keys that reach them. strandedCount comes from the
// same pass so the escalation signal can never disagree with the lines.
function providerImpactLines(impact: ProviderImpact, t: Translate): ProviderImpactSummary {
  if (impact.models.length === 0) {
    return { lines: [t('providers.impactModelsNone')], strandedCount: 0 }
  }
  const lines = [
    t('providers.impactModels', {
      count: impact.models.length,
      names: joinNames(
        impact.models.map((m) => m.name),
        DIALOG_MAX_NAMES,
      ),
    }),
  ]
  const stranded = impact.models.filter((m) => m.no_other_routable_source)
  if (stranded.length > 0) {
    lines.push(
      t('providers.impactStranded', {
        count: stranded.length,
        names: joinNames(
          stranded.map((m) => m.name),
          DIALOG_MAX_NAMES,
        ),
      }),
    )
    if (impact.affected_keys.length > 0) {
      lines.push(
        t('providers.impactKeys', {
          count: impact.affected_keys.length,
          names: joinNames(impact.affected_keys.map(keyDisplayName), DIALOG_MAX_NAMES),
        }),
      )
    }
    if (impact.allow_all_key_count > 0) {
      lines.push(t('providers.impactAllowAll', { count: impact.allow_all_key_count }))
    }
  }
  return { lines, strandedCount: stranded.length }
}

// What the delete modal shows for one provider: the cascade counts first,
// then the routing consequences. strandedCount > 0 is the escalation signal
// — models left with no routable source at all — which the modal renders as
// a hard error callout on top of the plain lines.
export function providerDeleteImpactView(impact: ProviderImpact, t: Translate): ProviderImpactSummary {
  const { lines, strandedCount } = providerImpactLines(impact, t)
  return {
    lines: [
      t('providers.deleteProviderCascadeCounts', {
        keys: impact.key_count,
        candidates: impact.candidate_count,
      }),
      ...lines,
    ],
    strandedCount,
  }
}

export async function providerDisableCopy(id: number, t: Translate): Promise<ConfirmDisableCopy> {
  return {
    title: t('providers.confirmDisableProviderTitle'),
    content: await providerDisableContent(id, t),
    positiveText: t('providers.statusDisabled'),
    negativeText: t('providers.cancel'),
  }
}
