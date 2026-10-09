// Proxy resolution wiring: this file builds the two safehttp.ProxyResolver
// functions the kernel's outbound surfaces consume (gateway.ProxyResolvers)
// out of the deployment's own configuration sources. It is assembly glue on
// purpose — the resolution decisions themselves are the pure functions in
// internal/outboundproxy, and the per-request cache read belongs to the
// system settings service — so what lives here is only the join: request →
// (per-provider stance from the request context, global snapshot from the
// cache) → outboundproxy.Resolve / ResolveGlobal.

package osswire

import (
	"net/http"
	"net/url"

	"go.uber.org/zap"

	"github.com/yolorouter/yolorouter/internal/gateway"
	"github.com/yolorouter/yolorouter/internal/outboundproxy"
	"github.com/yolorouter/yolorouter/internal/service/systemsettings"
	"github.com/yolorouter/yolorouter/pkg/logger"
)

// NewProxyResolvers builds the kernel's proxy resolver pair over the system
// settings service's cached outbound-proxy snapshot (30s TTL, so an admin's
// save reaches new requests without a restart).
//
// Upstream (provider-bound traffic — relay attempts, task polls): the
// dispatch site stamped the provider's stance onto the request context
// (gateway.WithUpstreamProxyStance); a request that carries none (a
// deployment surface with no provider binding, or a probe without a saved
// provider) resolves as an inherit stance, i.e. global-only.
//
// Fetch (secondary fetches): global-only by construction — the deployment
// speaks for these, never a provider.
//
// Both resolvers read the snapshot per request and hand it to
// outboundproxy unchanged: a corrupt stored value surfaces as a resolution
// error (the request aborts rather than leaking out unproxied), while a
// settings read that fails outright serves the fail-open snapshot the
// settings service already chose (last-known-good, or the shipped off /
// direct default on a cold failure) — a DB hiccup never starts routing
// traffic through an address the admin did not choose.
func NewProxyResolvers(settings *systemsettings.SystemSettingsService) gateway.ProxyResolvers {
	resolveGlobal := func(req *http.Request) outboundproxy.Global {
		snap, _, err := settings.GetOutboundProxy(req.Context())
		if err != nil {
			// The snapshot is still the best-known configuration (the
			// service returns last-known-good, or the off default when it
			// never had one); the error only says the refresh failed.
			logger.Warn("outbound proxy: settings read failed, resolving on last-known snapshot",
				zap.Error(err))
		}
		return outboundproxy.Global{Mode: snap.Mode, URL: snap.URL}
	}
	return gateway.ProxyResolvers{
		Upstream: func(req *http.Request) (*url.URL, error) {
			override := outboundproxy.Override{}
			if stance, ok := gateway.UpstreamProxyStanceFrom(req); ok {
				override = outboundproxy.Override{Mode: stance.Mode, URL: stance.URL}
			}
			return outboundproxy.Resolve(req.URL, override, resolveGlobal(req), nil)
		},
		Fetch: func(req *http.Request) (*url.URL, error) {
			return outboundproxy.ResolveGlobal(req.URL, resolveGlobal(req), nil)
		},
	}
}
