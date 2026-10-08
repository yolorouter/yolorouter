package audit

import (
	"reflect"
	"sort"

	"github.com/gin-gonic/gin"
)

// Event is the explicit-event layer's one-call entry for services that know
// business semantics the route shape cannot express: which fields of a
// setting changed and from what to what, which release a one-click update
// installed, which key a reveal exposed. Parking an event here never adds a
// row — the automatic layer merges it into the single record it already
// writes at request exit (see ExplicitEvent); the call supplies the row's
// richer language, not a second record.
//
// Every argument is optional in the merge's sense: an empty action keeps the
// route-derived action, an empty resource keeps the path-derived resource
// id, and an empty detail leaves the detail column empty. detail must
// already be redacted (build it with DiffDetail or RedactDetail) — the
// middleware re-runs the redaction pipeline at the capture boundary anyway,
// so a caller that forgets cannot leak, only lose its document.
func Event(c *gin.Context, action, resource, detail string) {
	SetExplicitEvent(c, ExplicitEvent{
		Action:     action,
		ResourceID: resource,
		Detail:     detail,
	})
}

// DiffDetail builds the changed-fields diff document the settings family
// parks with Event: {"field": {"old": <before>, "new": <after>}} for every
// field whose value actually changed, redacted and serialized. Unchanged
// fields are omitted — the audit trail records what changed, never the
// whole row — and a diff with no changed field is the empty string, so a
// write that changed nothing parks no document at all. A field present on
// only one side diffs against JSON null, which keeps sparse callers honest.
//
// DiffDetail is generic on purpose: it knows the document SHAPE the audit
// trail standardizes on ({old, new} per field), not any setting's fields.
// Callers map their own domain values into field names.
func DiffDetail(before, after map[string]any) string {
	fields := make([]string, 0, len(before)+len(after))
	seen := make(map[string]bool, len(before)+len(after))
	for name := range before {
		if !seen[name] {
			seen[name] = true
			fields = append(fields, name)
		}
	}
	for name := range after {
		if !seen[name] {
			seen[name] = true
			fields = append(fields, name)
		}
	}
	sort.Strings(fields)

	changed := make(map[string]any, len(fields))
	for _, name := range fields {
		oldV, newV := before[name], after[name]
		if reflect.DeepEqual(oldV, newV) {
			continue
		}
		changed[name] = map[string]any{"old": oldV, "new": newV}
	}
	if len(changed) == 0 {
		return ""
	}
	doc, err := RedactDetail(changed)
	if err != nil {
		// A map of scalars cannot fail to marshal; an error here means the
		// redaction pipeline refused the document, and no detail beats a
		// detail the pipeline could not vouch for.
		return ""
	}
	return doc
}
