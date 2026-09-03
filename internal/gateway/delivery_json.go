package gateway

import (
	"net/http"

	"github.com/yolorouter/yolorouter/internal/fact"
)

// commitJSONAnswer is the commit→write→flush handshake every whole-body
// JSON answer in the gateway shares, video and images alike. It owns the
// failure deliveries and reports nil when the body went out whole,
// leaving the success verdict to the caller: a delivered answer and a
// delivered refusal settle differently, and the tail that writes them
// must not be the thing that decides that.
//
// The dashscope image refusal keeps its own handshake because its body
// carries the dialect's own content type rather than plain JSON.
func commitJSONAnswer(tools DeliveryTools, status int, body []byte) *fact.Delivery {
	tools.Client.Inject(http.Header{"Content-Type": {"application/json"}})
	if cerr := tools.Client.Commit(status); cerr != nil {
		d := fact.Undelivered(http.StatusInternalServerError, fact.VerdictSettled, fact.FaultGateway,
			"commit_failed: "+cerr.Error(), cerr)
		return &d
	}
	if _, werr := tools.Client.Write(body); werr != nil {
		d := fact.Truncated(status, 499, fact.FaultClient, "client_write_timeout", werr)
		return &d
	}
	if ferr := tools.Client.Flush(); ferr != nil {
		d := fact.Truncated(status, 499, fact.FaultClient, "client_write_timeout", ferr)
		return &d
	}
	return nil
}
