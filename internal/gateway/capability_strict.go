package gateway

// The model-capability strict entry refusals. Two site switches turn an
// admin's declaration that the ADDRESSED model cannot use tools or cannot
// reason into an immediate 400 for a request that carries the matching
// parameter: the caller finds the configuration error at the door instead
// of as an unpredictable upstream refusal after a round trip.
//
// The evaluation point is pinned: the addressed model's row, asked once
// before any candidate is walked. A landing the chain fails over to never
// re-asks the question — routing cannot change what the caller addressed —
// and the reasoning side has a separate, gentler rule for landings (the
// egress clear in reasoning_clear.go) precisely because a failover target
// the caller never named is not the caller's error to be refused over.
//
// Tri-state leniency, same as every capability declaration: nil (never
// declared) never refuses, and true is support — both leave the request to
// the routing that already existed. The "carries" questions read the
// payload's one-pass view of the caller's body, computed at admission;
// field presence is the criterion there rather than the decoders' value
// validation, because a same-protocol landing forwards the field whatever
// shape it has, and a refusal that depends on which landing would have won
// is not a deterministic gate.

import (
	"fmt"
	"net/http"

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/settings"
)

// errTypeToolsNotSupported and errTypeReasoningNotSupported are the
// caller-facing error types of the two strict refusals. Like the
// context-limit one, they are not decision-table values — no capability
// reports them mid-flight — so they live with their only producer.
const (
	errTypeToolsNotSupported     = "tools_not_supported"
	errTypeReasoningNotSupported = "reasoning_not_supported"
)

// strictPayloadView is what the strict entry refusals need from an admitted
// payload: whether the caller's body carries the two parameter families the
// switches police. Only the text family implements it; a payload that does
// not (speech, images, video) carries neither concept, and the type
// assertion reading this view leaves such a request untouched.
type strictPayloadView interface {
	CarriesTools() bool
	CarriesReasoning() bool
}

// capabilityStrictRejection is the refusal decision isolated from any wire
// shape. Nil return means the request proceeds. Tools answers first: a
// refusal naming a parameter the caller can remove outright is more
// actionable than one about the reasoning they must also drop, and a
// request violating both gets one answer, not two. A nil model reads as
// nothing declared — the production mount always has the addressed row.
func capabilityStrictRejection(m *Model, carriesTools, carriesReasoning bool, ce settings.CapabilityEnforcementSetting) *Rejection {
	if m == nil {
		return nil
	}
	if ce.ToolsStrict && carriesTools && m.SupportsTools != nil && !*m.SupportsTools {
		return &Rejection{
			Status:     http.StatusBadRequest,
			ErrorType:  errTypeToolsNotSupported,
			Message:    fmt.Sprintf("model %q is declared not to support tools; remove the tools parameter", m.Name),
			FailReason: errTypeToolsNotSupported,
			Fault:      fact.FaultClient,
		}
	}
	if ce.ReasoningStrict && carriesReasoning && m.SupportsReasoning != nil && !*m.SupportsReasoning {
		return &Rejection{
			Status:     http.StatusBadRequest,
			ErrorType:  errTypeReasoningNotSupported,
			Message:    fmt.Sprintf("model %q is declared not to support reasoning; remove the reasoning parameter", m.Name),
			FailReason: errTypeReasoningNotSupported,
			Fault:      fact.FaultClient,
		}
	}
	return nil
}

// precheckCapabilityStrict mounts the refusal at the door. The switches and
// the addressed model come from the exchange; the carries questions come
// from the admitted payload's view. A payload that offers no view (any
// non-text modality) passes untouched — the switches have nothing to say
// about a speech or image body.
func (s *Service) precheckCapabilityStrict(rc *Exchange, m *Model, payload Payload) *Rejection {
	view, ok := payload.(strictPayloadView)
	if !ok {
		return nil
	}
	return capabilityStrictRejection(m, view.CarriesTools(), view.CarriesReasoning(), rc.settings.Capability)
}
