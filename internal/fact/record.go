package fact

// Record is an accounting observation. It never moves the relay loop; it exists
// so settlement and the audit trail can describe what happened.
//
// Unlike Kind, this half of the vocabulary is open. A consumer switches on the
// record types it knows and persists everything else verbatim under
// RecordName(), so a build can report a record type another build's kernel has
// never heard of and still have it survive into the audit row. That property is
// what keeps deployment-specific capabilities from forcing kernel changes.
//
// Records must be typed structs, never a generic map. The struct is what keeps
// a renamed field a compile error rather than a value that silently stops being
// recorded.
type Record interface {
	// RecordName is the stable identifier used when a consumer does not
	// recognise the type. It is persisted, so it must not change once shipped.
	RecordName() string
	isRecord()
}

// Base is the opt-in marker every record type embeds.
//
// It is exported for one specific reason: the unexported isRecord method it
// carries is what makes this vocabulary opt-in, but an unexported marker on an
// unexported base would make it CLOSED — no type outside this package could
// ever satisfy Record, and the ability to add record types without touching the
// kernel is the entire reason this half of the vocabulary exists.
//
// Exporting the base keeps both properties: satisfying Record still requires
// deliberately embedding Base, so no type satisfies it by accident, and any
// package may do so. The routing half above is the one that is genuinely
// closed; do not copy its sealing here.
type Base struct{}

func (Base) isRecord() {}
