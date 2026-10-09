package gateway

import (
	"context"
	"net/http"
	"time"

	"github.com/yolorouter/yolorouter/internal/service/safehttp"
)

// UpstreamClient sends the rewritten body to a provider. It reuses safehttp's
// SSRF-safe transport (provider_client.go's contract: every outbound dial is
// SSRF-checked) and disables redirect following so the decrypted upstream
// key cannot leak to a host the admin never confirmed.
type UpstreamClient struct {
	httpClient *http.Client
}

// UpstreamProxyStance is one provider's outbound-proxy stance as the kernel
// carries it to the transport: the provider row's stored proxy_mode and
// proxy_url, stamped onto the request context (WithUpstreamProxyStance) at
// the dispatch site so the deployment-injected resolver can honour
// per-provider overrides without the transport knowing anything about
// providers. Mode uses the stored spellings: "" (inherit the global
// setting), "off" (force direct), "custom" (proxy through URL).
type UpstreamProxyStance struct {
	Mode string
	URL  string
}

// upstreamProxyStanceKey is the context key type for UpstreamProxyStance; a
// private struct so no other package can collide with or forge the slot.
type upstreamProxyStanceKey struct{}

// WithUpstreamProxyStance returns ctx carrying stance, so an outbound
// request built from ctx resolves its proxy under the provider that
// dispatched it. Stamping rather than threading a parameter keeps the
// stance invisible to every layer between the dispatch site and the
// transport's Proxy callback — the only two parties with a say.
func WithUpstreamProxyStance(ctx context.Context, stance UpstreamProxyStance) context.Context {
	return context.WithValue(ctx, upstreamProxyStanceKey{}, stance)
}

// UpstreamProxyStanceFrom returns the proxy stance req's context carries,
// reporting false when the request was dispatched without one (a request
// with no provider binding, or a deployment that wires no proxy resolver
// at all). A false report is the "inherit the global setting" stance by
// construction: the resolver falls back to global-only resolution.
func UpstreamProxyStanceFrom(req *http.Request) (UpstreamProxyStance, bool) {
	stance, ok := req.Context().Value(upstreamProxyStanceKey{}).(UpstreamProxyStance)
	return stance, ok
}

// ProxyResolvers carries the two outbound proxy policies a deployment
// injects into the kernel: both are safehttp.ProxyResolver functions
// consulted per request (nil = always direct, the shipped behavior), so a
// configuration snapshot read behind them takes effect on new requests
// without rebuilding any transport.
//
// The two seats are deliberately distinct. Upstream serves provider-bound
// traffic (relay attempts, task polls) and may honour a per-provider
// override read back through UpstreamProxyStanceFrom. Fetch serves
// secondary fetches, whose destinations a third party chose and which carry
// no provider binding — global configuration only, never a provider's
// override, even though a fetch can run on a request whose context still
// carries the answering provider's stance.
type ProxyResolvers struct {
	Upstream safehttp.ProxyResolver
	Fetch    safehttp.ProxyResolver
}

// NewUpstreamClient builds a gateway upstream client. The Transport is the
// same SSRF-safe one provider_client.go uses for connection tests, so a
// provider that tests green also serves real traffic through the identical
// network path — including the allowPrivate relaxation, so a self-hosted
// LAN/localhost provider that tests green also relays (config.SecurityConfig.
// AllowPrivateUpstreams).
//
// proxy is the provider-bound resolver (ProxyResolvers.Upstream) mounted as
// the transport's per-request Proxy callback; nil keeps the never-proxy
// transport (Proxy stays nil, environment proxying never honored). The
// resolver sees the per-provider stance a dispatch stamped onto the request
// context (WithUpstreamProxyStance); a request without one resolves as
// global-only.
//
// headerTimeout bounds the wait for the response headers (the reasoning-model
// "thinking" phase before the first byte) and is applied to the transport so
// it stays independent of the per-attempt context that later bounds the body.
// A zero headerTimeout disables the header-phase bound (kept explicit rather
// than silently defaulting, so the caller stays in control of the value).
//
// connectTimeout bounds each TCP dial to the upstream and is forwarded to
// the SSRF-safe transport's net.Dialer; it tracks config.GatewayConfig.
// ConnectTimeout so the admin-tuned value, not a hard-coded constant, governs
// the gateway path.
//
// tlsHandshake bounds the TLS handshake after the TCP connect and is forwarded
// to transport.TLSHandshakeTimeout; it tracks config.GatewayConfig.
// TLSHandshakeTimeout so the admin-tuned value governs the gateway path.
func NewUpstreamClient(allowPrivate bool, headerTimeout, connectTimeout, tlsHandshake time.Duration, proxy safehttp.ProxyResolver) *UpstreamClient {
	transport := safehttp.NewProxyingTransport(allowPrivate, connectTimeout, tlsHandshake, proxy)
	transport.ResponseHeaderTimeout = headerTimeout
	return &UpstreamClient{
		httpClient: &http.Client{
			Transport: transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Timeout: 0, // per-attempt context in the relay loop bounds each call
		},
	}
}

// SendUpstreamRequest sends a fully-built upstream *http.Request (already
// carrying its context, URL, body, and codec-specific headers) and returns the
// raw response. A non-nil
// error means a transport-level failure (network/timeout/SSRF-block) — HTTP
// status codes, including 5xx, come back as a non-nil response with nil
// error.
func (c *UpstreamClient) SendUpstreamRequest(req *http.Request) (*http.Response, error) {
	return c.httpClient.Do(req)
}
