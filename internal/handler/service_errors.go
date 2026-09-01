package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/service/analytics"
	"github.com/yolorouter/yolorouter/pkg/errcode"
	"github.com/yolorouter/yolorouter/pkg/response"
)

// serviceErrorEntry maps one service-layer sentinel to the unified error
// envelope. The zero options cover the common shape — response.Error with
// the code's registered message; the two options carry the deliberate
// exceptions, so every exception is visible in the table instead of hiding
// in one handler's switch arm.
type serviceErrorEntry struct {
	sentinel error
	code     int
	// httpStatus overrides the status derived from the code's range mapping;
	// zero means derive as usual.
	httpStatus int
	// verbatim returns err.Error() as the message instead of the code's
	// registered text — only for sentinels whose wrapped text is known to
	// name client-supplied input, never internal error detail.
	verbatim bool
}

// serviceErrorTable is the single sentinel-to-envelope mapping every admin
// handler shares. It replaced four per-handler copies of the same switch;
// with one table, a sentinel that crosses handler domains (a model call
// surfacing a provider error, say) maps to its own code everywhere instead
// of falling to a generic 500 in the handlers that forgot it. Adding a
// sentinel means adding a row here, not a case to every handler that might
// see it.
var serviceErrorTable = []serviceErrorEntry{
	{sentinel: errcode.ErrAPIKeyNotFound, code: errcode.APIKeyNotFound},
	{sentinel: errcode.ErrAPIKeyEmptyAllowlist, code: errcode.APIKeyEmptyAllowlist},
	{sentinel: errcode.ErrCustomSystemPromptTooLong, code: errcode.CustomSystemPromptTooLong},
	{sentinel: errcode.ErrCustomSystemPromptEmpty, code: errcode.CustomSystemPromptEmpty},
	{sentinel: errcode.ErrCompressEnabledRequired, code: errcode.CompressEnabledRequired},
	{sentinel: errcode.ErrAPIKeyPlaintextUnavailable, code: errcode.APIKeyPlaintextUnavailable},
	// 409 is not produced by httpStatusForCode's range mapping; set it
	// explicitly, mirroring the system_settings CAS-conflict path.
	{sentinel: errcode.ErrAPIKeyConflict, code: errcode.APIKeyConflict, httpStatus: http.StatusConflict},

	{sentinel: errcode.ErrProviderNotFound, code: errcode.ProviderNotFound},
	{sentinel: errcode.ErrProviderNameTaken, code: errcode.ProviderNameTaken},
	{sentinel: errcode.ErrProviderKeyNotFound, code: errcode.ProviderKeyNotFound},
	{sentinel: errcode.ErrProviderKeyLabelTaken, code: errcode.ProviderKeyLabelTaken},
	{sentinel: errcode.ErrProviderKeyNotVerified, code: errcode.ProviderKeyNotVerified},
	{sentinel: errcode.ErrProviderKeyNeedsReentry, code: errcode.ProviderKeyNeedsReentry},
	{sentinel: errcode.ErrProviderKeyTestNotSaved, code: errcode.ProviderKeyTestNotSaved},
	// Safe to return verbatim: the wrapped text names exactly which
	// client-supplied provider_type/protocol_endpoints value failed
	// validation (e.g. "invalid provider_type: claude"), not internal
	// crypto/gorm error detail.
	{sentinel: errcode.ErrProviderProtocolInvalid, code: errcode.ProviderProtocolInvalid, verbatim: true},
	// validatePlaintextLength (provider_service.go) wraps this sentinel —
	// Gin's own binding tags currently make that check unreachable via HTTP,
	// but without this row a future change (a looser binding tag, a new
	// non-HTTP caller) would silently fall to the generic 500 instead of the
	// intended 400.
	{sentinel: errcode.ErrProviderKeyTooShort, code: errcode.ProviderKeyTooShort},
	{sentinel: errcode.ErrProviderNoTestableModel, code: errcode.ProviderNoTestableModel},

	{sentinel: errcode.ErrModelNotFound, code: errcode.ModelNotFound},
	{sentinel: errcode.ErrModelNameTaken, code: errcode.ModelNameTaken},
	{sentinel: errcode.ErrModelCandidateNotFound, code: errcode.ModelCandidateNotFound},
	{sentinel: errcode.ErrModelCandidateProviderTaken, code: errcode.ModelCandidateProviderTaken},
	{sentinel: errcode.ErrModelCandidateNotVerified, code: errcode.ModelCandidateNotVerified},
	{sentinel: errcode.ErrModelSchedulingModeInvalid, code: errcode.ModelSchedulingModeInvalid},
	{sentinel: errcode.ErrModelOutputModalityInvalid, code: errcode.ModelOutputModalityInvalid},
	{sentinel: errcode.ErrModelBillingInvalid, code: errcode.ModelBillingInvalid},

	// The analytics sentinels live outside pkg/errcode; their wrapped text
	// names the offending query parameter, so it is returned verbatim.
	// Dimension/bucket validation mostly happens at the handler (via
	// parseDimensionParam / parseBucketParam), so these rows are a defensive
	// second line — easier to keep the mapping than to prove the handler is
	// the only entry point.
	{sentinel: repository.ErrInvalidBucket, code: errcode.InvalidParam, verbatim: true},
	{sentinel: analytics.ErrInvalidDimension, code: errcode.InvalidParam, verbatim: true},
}

// writeServiceError maps a service-layer error to the unified error envelope
// through serviceErrorTable. An unmatched error becomes a generic 500 with a
// fixed message — never err.Error(), whose wrapped crypto/gorm text would
// leak internal detail to the client.
func writeServiceError(c *gin.Context, err error) {
	for _, e := range serviceErrorTable {
		if !errors.Is(err, e.sentinel) {
			continue
		}
		msg := errcode.GetMessage(e.code)
		if e.verbatim {
			msg = err.Error()
		}
		if e.httpStatus != 0 {
			response.ErrorStatus(c, e.httpStatus, e.code, msg)
			return
		}
		response.Error(c, e.code, msg)
		return
	}
	response.Error(c, errcode.InternalError, errcode.GetMessage(errcode.InternalError))
}
