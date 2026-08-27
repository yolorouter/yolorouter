package fact

// Record types whose declarations are deployment-specific: the shared
// roster in records.go is the vocabulary both distributions agree on, while
// the declarations here differ by deployment — extra fields only one
// kernel's delivery code reports, or whole types only one deployment's
// capabilities produce. Consumers treat them like any other record.

// FinishReasonObserved carries the raw stop signals seen on the wire. The
// normalisation into a small set of buckets belongs to whoever consumes this,
// not here: the rule for ranking an abnormal stop above an inferred tool call is
// a policy, and policies do not belong in the vocabulary.
type FinishReasonObserved struct {
	Base
	Raw            string
	SawToolCall    bool
	SawSemanticEnd bool
}

func (FinishReasonObserved) RecordName() string { return "finish_reason_observed" }
