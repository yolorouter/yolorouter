// frontend/src/utils/auditDetail.ts
//
// Pure projection of an audit row's `detail` column into display lines for
// the audit page's expanded row. Kept out of the component so the shapes
// are unit-testable without mounting anything.
//
// The backend stores detail as a JSON *string*, already redacted before
// persist (internal/audit's pipeline — plaintext secrets never reach the
// column). Two payload shapes occur in practice:
//
//   1. A settings diff: {"field": {"old": <before>, "new": <after>}, ...}
//      (audit.DiffDetail) — rendered as "field: before → after" per line.
//   2. Event context: {"username": "erin"} — rendered as "key: value".
//
// Anything else (empty string, non-JSON, an array, a scalar) returns null;
// the page then renders the raw text as one line (or nothing when empty) —
// a malformed detail must never blank the row, only lose the pretty form.

export interface AuditDetailLine {
  field: string
  /** Pre-rendered display text for the line: "before → after" or the value. */
  text: string
}

// Renders one JSON value as display text: strings come through verbatim
// (JSON quoting would plaster escape noise over a username), everything
// else keeps JSON.stringify's unambiguous form (numbers, booleans, null,
// nested objects/arrays stay explicit).
function valueText(value: unknown): string {
  if (typeof value === 'string') return value
  return JSON.stringify(value)
}

// A diff entry is an object carrying exactly the old/new pair the backend's
// DiffDetail writes. Extra keys mean it is plain event context that happens
// to hold those names — rendered as an ordinary value, never misread as a
// field-level diff.
function isDiffEntry(value: unknown): value is { old: unknown; new: unknown } {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return false
  const keys = Object.keys(value)
  return keys.length === 2 && 'old' in value && 'new' in value
}

/**
 * Parses an audit row's detail JSON into display lines, key order
 * preserved (the backend writes the diff/event fields in a stable order the
 * admin can scan). Returns null for an empty string, non-object JSON, or
 * anything unparseable — the caller decides how to degrade.
 */
export function auditDetailLines(detail: string): AuditDetailLine[] | null {
  if (!detail) return null
  let doc: unknown
  try {
    doc = JSON.parse(detail)
  } catch {
    return null
  }
  if (typeof doc !== 'object' || doc === null || Array.isArray(doc)) return null
  const lines: AuditDetailLine[] = []
  for (const [field, value] of Object.entries(doc as Record<string, unknown>)) {
    if (isDiffEntry(value)) {
      lines.push({ field, text: `${valueText(value.old)} → ${valueText(value.new)}` })
    } else {
      lines.push({ field, text: valueText(value) })
    }
  }
  return lines
}
