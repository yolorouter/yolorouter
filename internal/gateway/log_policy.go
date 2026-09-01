package gateway

// This file is the enforcement point of a payload's LogPolicy: the single
// place where the kernel decides what of a request's bodies may reach
// storage, and in what form. Everything a capability could see about bodies
// flows through the exchange, so filtering here — once, after settlement,
// before anything records — is one decision instead of one per consumer.

// payloadLogView is what the kernel keeps of a payload's answers about its
// own bodies once a request is admitted. The methods those answers come from
// stay on the Payload (a policy can be per-request); this holds the answers,
// not the payload, so nothing downstream needs the whole interface to know
// what may be stored.
type payloadLogView struct {
	policy LogPolicy
	// sanitize is the payload's rendering of one body. Called only for the
	// bodies the policy stores rendered — a body stored raw is kept as the
	// bytes that arrived, and routing it through a string-typed render would
	// copy every request body to no effect.
	sanitize func(BodyKind, string, []byte) string
	// requestContentType is the caller's own Content-Type, carried because a
	// sanitizer asked for a content type may need the multipart boundary
	// inside it to make sense of the body at all.
	requestContentType string
}

// admitBodyLog reads the policy view off a freshly admitted payload.
//
// Read here, at admission, rather than at record time: the answers describe
// the whole request, the payload is serial, and the record path already has
// enough to do without holding the interface for two more calls.
func admitBodyLog(p Payload, requestContentType string) *payloadLogView {
	return &payloadLogView{
		policy:             p.LogPolicy(),
		sanitize:           p.SanitizeForLog,
		requestContentType: requestContentType,
	}
}

// capBody applies the policy's byte cap to a body being kept. Slicing rather
// than copying: the slice shares backing with the body it caps, and a cap
// that allocated would reintroduce exactly the copy the raw form exists to
// avoid.
func (v *payloadLogView) capBody(body []byte) []byte {
	if v.policy.MaxBytes > 0 && int64(len(body)) > v.policy.MaxBytes {
		return body[:v.policy.MaxBytes]
	}
	return body
}

// renderBody produces what storage may keep of one rendered body: the
// payload's rendering of it, capped to the policy's byte limit. A nil body
// renders nil — an absent body is not the same fact as an empty one, and a
// row that shows "" for a body the modality redacted is the readable outcome
// of that difference.
func (v *payloadLogView) renderBody(k BodyKind, contentType string, body []byte) []byte {
	if len(body) == 0 {
		return nil
	}
	rendered := v.capBody([]byte(v.sanitize(k, contentType, body)))
	if len(rendered) == 0 {
		return nil
	}
	return rendered
}

// applyBodyLogPolicy enforces the admitted payload's policy over everything
// this exchange captured, in place, before any recorder runs.
//
// For each of the four bodies the policy says dropped, raw, or rendered:
// dropped bodies are cleared, raw bodies are kept as the bytes that arrived
// (capped, without a copy), rendered bodies are replaced by what the payload
// renders of them. The stream capture file is the caller-facing body in file
// form, so a policy that drops the client response keeps no capture file
// either — the file is never opened, which is a different guarantee from
// opening and deleting it afterwards.
//
// A nil view (no payload was admitted) is left alone on purpose. A request
// the modality refused at the door never became anyone's payload, and its
// bodies keep the kernel's own account of them — the account that predates
// policies and is what an operator diagnosing "why did the gateway say no"
// needs to see.
func (rc *Exchange) applyBodyLogPolicy() {
	v := rc.payloadLog
	if v == nil {
		return
	}
	// The compressed variant is the same caller request after a capability
	// shrank it: one body, one policy decision, rendered with the content
	// type the caller actually sent.
	switch v.policy.Storage(BodyClientRequest) {
	case BodyStoredRaw:
		rc.bodies.SetRequest(v.capBody(rc.bodies.Request()))
		rc.bodies.SetCompressedRequest(v.capBody(rc.bodies.CompressedRequest()))
	case BodyStoredRendered:
		rc.bodies.SetRequest(v.renderBody(BodyClientRequest, v.requestContentType, rc.bodies.Request()))
		rc.bodies.SetCompressedRequest(v.renderBody(BodyClientRequest, v.requestContentType, rc.bodies.CompressedRequest()))
	default:
		rc.bodies.SetRequest(nil)
		rc.bodies.SetCompressedRequest(nil)
	}
	switch v.policy.Storage(BodyUpstreamRequest) {
	case BodyStoredRaw:
		rc.bodies.SetUpstreamRequest(v.capBody(rc.bodies.UpstreamRequest()))
	case BodyStoredRendered:
		rc.bodies.SetUpstreamRequest(v.renderBody(BodyUpstreamRequest, rc.upstreamContentType, rc.bodies.UpstreamRequest()))
	default:
		rc.bodies.SetUpstreamRequest(nil)
	}
	switch v.policy.Storage(BodyUpstreamResponse) {
	case BodyStoredRaw:
		rc.bodies.SetUpstreamResponse(v.capBody(rc.bodies.UpstreamResponse()))
	case BodyStoredRendered:
		rc.bodies.SetUpstreamResponse(v.renderBody(BodyUpstreamResponse, "", rc.bodies.UpstreamResponse()))
	default:
		rc.bodies.SetUpstreamResponse(nil)
	}
	switch v.policy.Storage(BodyClientResponse) {
	case BodyStoredRaw:
		rc.bodies.SetResponse(v.capBody(rc.bodies.Response()))
	case BodyStoredRendered:
		rc.bodies.SetResponse(v.renderBody(BodyClientResponse, "", rc.bodies.Response()))
	default:
		rc.bodies.SetResponse(nil)
		rc.bodies.DropStream()
	}
}
