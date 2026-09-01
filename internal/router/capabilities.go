package router

import (
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/capability/compress"
	"github.com/yolorouter/yolorouter/internal/capability/contentinspect"
	"github.com/yolorouter/yolorouter/internal/capability/imagesize"
	"github.com/yolorouter/yolorouter/internal/capability/maxtokens"
	"github.com/yolorouter/yolorouter/internal/capability/modelname"
	"github.com/yolorouter/yolorouter/internal/capability/ratelimit"
	"github.com/yolorouter/yolorouter/internal/capability/requestlog"
	"github.com/yolorouter/yolorouter/internal/capability/systemprompt"
	"github.com/yolorouter/yolorouter/internal/capability/visionfallback"
	"github.com/yolorouter/yolorouter/internal/gateway"
)

// registerCapabilities is the assembly: the only place that sees both the
// kernel and the capabilities it runs, which is what lets a capability stay
// unaware of the kernel entirely.
//
// The binding function passed to each registration is the compile-time proof
// that an Exchange satisfies the view its capability asked for: were the
// inspector to need a getter the Exchange does not have, that line would fail
// to compile.
//
// It is a function rather than a block inside route setup so that the order
// below is something a test can execute and pin. Admission order is not
// decoration — it is acquisition order, and therefore the reverse of
// compensation order.
func registerCapabilities(svc *gateway.Service, db *gorm.DB, loopbackBase string) {
	gateway.RegisterIngressRewriter(svc, compress.New(), gateway.StageCompress,
		func(e *gateway.Exchange) compress.View { return e })
	gateway.RegisterIngressRewriter(svc, visionfallback.New(db, loopbackBase), gateway.StageVisionFallback,
		func(e *gateway.Exchange) visionfallback.View { return e })
	gateway.RegisterUpstreamErrorObserver(svc, contentinspect.New(),
		func(e *gateway.Exchange) contentinspect.View { return e })
	gateway.RegisterEgressRewriter(svc, imagesize.New(), gateway.StageImageSize,
		func(e *gateway.Exchange) imagesize.View { return e })
	gateway.RegisterEgressRewriter(svc, systemprompt.New(), gateway.StageCustomPrompt,
		func(e *gateway.Exchange) systemprompt.View { return e })
	gateway.RegisterResponseCodecWrapper(svc, modelname.New(), gateway.StageModelName,
		func(e *gateway.Exchange) modelname.View { return e })
	gateway.RegisterEgressRewriter(svc, maxtokens.New(), gateway.StageMaxTokens,
		func(e *gateway.Exchange) maxtokens.View { return e })
	gateway.RegisterAdmission(svc, ratelimit.NewLimiter(), gateway.AdmitOnArrival,
		func(e *gateway.Exchange) ratelimit.View { return e })
	gateway.RegisterRecorder(svc, requestlog.New(db),
		func(e *gateway.Exchange) requestlog.View { return e })
}
