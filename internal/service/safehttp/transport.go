// Package safehttp provides an SSRF-safe outbound HTTP transport shared by
// provider connection tests and intended for
// future gateway relay calls. Every direct dial validates the resolved IP
// before connecting; proxy environment variables are never honored, the
// only proxy source being an injected ProxyResolver (NewProxyingTransport),
// under which the dial goes to the proxy address — reserved-range-checked
// only, with target validation intentionally skipped; redirects
// are not followed by callers that use http.Client{CheckRedirect} (left to
// the caller, since this package only controls the Transport).
package safehttp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// deniedCIDRs are NEVER dialable, regardless of allowPrivate: they have no
// legitimate unicast-destination use (multicast, benchmark, reserved, and the
// unspecified address, which many stacks route to local services). Keeping
// them blocked even when an operator opts into private upstreams means a
// mistyped or malicious base_url resolving to e.g. 0.0.0.0 still can't be
// reached — allowPrivate only ever opens the privateCIDRs below.
var deniedCIDRs []*net.IPNet

// privateCIDRs are blocked by default but become dialable when allowPrivate
// is set — the loopback/private/link-local/CGNAT/unique-local ranges a
// self-hosted operator legitimately points Yolorouter at (a LAN box, a
// localhost model server, or a Tailscale 100.64.0.0/10 mesh peer). Note this
// range includes cloud metadata (169.254.169.254), which is why enabling
// allowPrivate is documented as unsafe on multi-tenant/exposed deployments.
var privateCIDRs []*net.IPNet

func init() {
	denied := []string{
		"224.0.0.0/4",   // multicast v4
		"198.18.0.0/15", // benchmark
		"240.0.0.0/4",   // reserved
		"0.0.0.0/32",    // unspecified v4
		"ff00::/8",      // multicast v6
		"::/128",        // unspecified v6
	}
	private := []string{
		"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", // private v4
		"127.0.0.0/8",    // loopback v4
		"169.254.0.0/16", // link-local v4, incl. cloud metadata (169.254.169.254)
		"100.64.0.0/10",  // CGNAT (e.g. Tailscale mesh addresses)
		"::1/128",        // loopback v6
		"fe80::/10",      // link-local v6
		"fc00::/7",       // unique-local v6
	}
	deniedCIDRs = parseCIDRs(denied)
	privateCIDRs = parseCIDRs(private)
}

func parseCIDRs(ranges []string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(ranges))
	for _, r := range ranges {
		_, ipnet, err := net.ParseCIDR(r)
		if err != nil {
			panic(fmt.Sprintf("safehttp: invalid built-in CIDR %q: %v", r, err))
		}
		out = append(out, ipnet)
	}
	return out
}

// checkIPAllowed reports an error if ip falls in a disallowed range. The
// always-denied ranges (deniedCIDRs) are rejected unconditionally; the
// loopback/private/link-local ranges (privateCIDRs) are rejected only when
// allowPrivate is false. Both lists are a best-effort snapshot, not a
// guaranteed-exhaustive one — they should be cross-checked
// against the IANA Special-Purpose Address Registry as it evolves.
func checkIPAllowed(ip net.IP, allowPrivate bool) error {
	normalized := ip
	if v4 := ip.To4(); v4 != nil {
		// Normalize IPv4-mapped IPv6 addresses (::ffff:a.b.c.d) before
		// checking — otherwise this exact form bypasses a pure IPv4 CIDR
		// match.
		normalized = v4
	}
	if err := matchCIDR(normalized, ip, deniedCIDRs); err != nil {
		return err
	}
	if !allowPrivate {
		if err := matchCIDR(normalized, ip, privateCIDRs); err != nil {
			return err
		}
	}
	return nil
}

// matchCIDR returns a disallowed-range error if normalized falls in any of
// cidrs; original is used only for the error message (the un-normalized form
// the caller passed).
func matchCIDR(normalized, original net.IP, cidrs []*net.IPNet) error {
	for _, ipnet := range cidrs {
		if ipnet.Contains(normalized) {
			return fmt.Errorf("address %s is in a disallowed range (%s)", original, ipnet)
		}
	}
	return nil
}

// ProxyAddressAllowed reports whether ip may serve as the address of a
// configured outbound proxy. A proxy address is admin-declared trusted
// configuration, not an arbitrary dial target: private and loopback ranges
// are allowed (a LAN proxy or loopback test stub is the primary self-hosted
// topology), and only the always-denied reserved ranges (multicast,
// benchmark, reserved, unspecified) are rejected — the same ranges
// checkIPAllowed refuses under any allowPrivate setting. Both the save-time
// proxy-URL validation and the proxy dial path call this so the address
// policy has one source of truth.
func ProxyAddressAllowed(ip net.IP) bool {
	normalized := ip
	if v4 := ip.To4(); v4 != nil {
		normalized = v4
	}
	return matchCIDR(normalized, ip, deniedCIDRs) == nil
}

// ProxyResolver decides, for one outbound request, which proxy URL the
// request should travel through, or nil for a direct connection. It is
// injected at assembly time (NewProxyingTransport) and consulted per request
// by the transport's Proxy callback, so a config snapshot read behind it
// takes effect on new requests without rebuilding the transport — the
// "dynamic proxy callback" mechanism. A returned error aborts the request:
// traffic the operator explicitly asked to proxy must never leak out
// unproxied because the stored configuration went bad.
type ProxyResolver func(*http.Request) (*url.URL, error)

// NewTransport returns an *http.Transport with SSRF protections wired in:
// custom DialContext validating every resolved IP before connecting,
// environment-variable proxying disabled, and no other defaults changed.
// It is NewProxyingTransport with a nil resolver (never-proxy semantics),
// kept as the original constructor so existing call sites and tests are
// untouched.
//
// allowPrivate=true opens ONLY the privateCIDRs ranges (loopback, private,
// link-local, CGNAT, unique-local) for self-hosted operators pointing at a
// LAN/localhost model server; the always-denied ranges (multicast, benchmark,
// reserved, unspecified) stay blocked, and the resolve-once, no-proxy, and dial-first-
// allowed-IP behavior is unchanged. See config.SecurityConfig.
// AllowPrivateUpstreams for the security caveat.
//
// connectTimeout bounds each TCP dial and is forwarded to the underlying
// net.Dialer. A zero value means no dial-level bound (the caller, not this
// package, owns the timeout policy).
//
// tlsHandshake bounds the TLS handshake after the TCP connect completes and
// is forwarded to transport.TLSHandshakeTimeout. A zero value means no
// handshake-level bound (kept explicit rather than silently defaulting, so
// the caller stays in control of the value).
func NewTransport(allowPrivate bool, connectTimeout, tlsHandshake time.Duration) *http.Transport {
	return assembleTransport(allowPrivate, connectTimeout, tlsHandshake, nil)
}

// NewProxyingTransport is the unified transport assembly every outbound call
// site funnels through: NewTransport's allowPrivate/timeouts wiring plus an
// injectable ProxyResolver mounted as the transport's Proxy callback. A nil
// resolver is exactly NewTransport (Proxy stays nil — environment proxying
// never reappears).
//
// Proxy semantics (SSRF redefined when the resolver returns a proxy URL):
//
//   - http, https, socks5, and socks5h schemes are all handled natively by
//     net/http; socks5 and socks5h are treated identically, and in both the
//     target hostname is handed to the proxy unresolved (proxy-side DNS).
//     There is no custom dialer and no extra dependency.
//   - The dial the transport performs is TO THE PROXY, so it runs under the
//     proxy-address policy (ProxyAddressAllowed: private and loopback proxy
//     addresses allowed — a LAN proxy or loopback test stub is the primary
//     self-hosted topology — reserved ranges rejected). The target's own
//     per-IP validation is skipped entirely: inside the proxy channel both
//     DNS and the connect happen proxy-side, so a local resolve-and-check
//     would be neither necessary nor correct.
//   - A resolver error fails the request outright; it never falls back to a
//     direct connection.
//   - Orthogonal to allowPrivate: with a proxy in play, reachability is the
//     proxy's decision and AllowPrivateUpstreams degrades to "may connect to
//     the proxy" for that request.
//
// Distinguishing a proxy dial from a target dial relies on the registry
// documented on proxyAddrRegistry.
func NewProxyingTransport(allowPrivate bool, connectTimeout, tlsHandshake time.Duration, proxy ProxyResolver) *http.Transport {
	if proxy == nil {
		return assembleTransport(allowPrivate, connectTimeout, tlsHandshake, nil)
	}
	proxies := &proxyAddrRegistry{addrs: make(map[string]struct{})}
	transport := assembleTransport(allowPrivate, connectTimeout, tlsHandshake, proxies)
	transport.Proxy = func(req *http.Request) (*url.URL, error) {
		u, err := proxy(req)
		if err != nil {
			return nil, err
		}
		if u == nil {
			// Direct: the callback returning nil, nil is how net/http spells
			// "no proxy for this request" — and a non-nil Proxy callback
			// already means the environment is never consulted.
			return nil, nil
		}
		// Register BEFORE returning: net/http calls Proxy synchronously in
		// roundTrip and only then queues the dial, so the registry entry is
		// guaranteed visible to DialContext for this request's connection.
		proxies.add(u)
		return u, nil
	}
	return transport
}

// assembleTransport builds the one *http.Transport every constructor above
// produces, differing only in whether the dial path must be able to
// recognize configured proxy addresses. proxies==nil keeps the historical
// never-proxy behavior byte-for-byte: the Proxy field is left nil (not a
// func returning nil) and every dial goes through the full target policy.
func assembleTransport(allowPrivate bool, connectTimeout, tlsHandshake time.Duration, proxies *proxyAddrRegistry) *http.Transport {
	dialer := &net.Dialer{Timeout: connectTimeout}
	return &http.Transport{
		Proxy:               nil, // never honor HTTP_PROXY/HTTPS_PROXY/NO_PROXY
		TLSHandshakeTimeout: tlsHandshake,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if proxies != nil && proxies.isProxyAddr(addr) {
				return dialProxyAddress(ctx, dialer, network, addr, connectTimeout)
			}
			return safeDialContext(ctx, dialer, network, addr, allowPrivate, connectTimeout)
		},
	}
}

// proxyAddrRegistry remembers the canonical dial addresses of proxies the
// resolver has returned, so DialContext can tell a dial TO a configured
// proxy (proxy-address policy: reserved ranges only) from a dial to an
// actual upstream target (full per-IP policy). net/http offers no direct
// signal for this: it resolves the Proxy callback's URL into a connect
// method and then hands DialContext the proxy's canonical address as an
// opaque string, so membership against the addresses we ourselves produced
// is the only correlation channel.
//
// Entries are never removed: the set is bounded by the distinct proxy
// addresses the admin's configuration has ever produced (config scale, a
// handful of entries). A subsequent direct request whose target happens to
// be exactly a registered proxy address is validated under the proxy policy
// instead of the target policy — that address is one the operator already
// declared dialable, so no new reachability is granted beyond it.
type proxyAddrRegistry struct {
	mu    sync.Mutex
	addrs map[string]struct{}
}

// add records u's canonical dial address.
func (r *proxyAddrRegistry) add(u *url.URL) {
	key := canonicalProxyAddr(u)
	r.mu.Lock()
	r.addrs[key] = struct{}{}
	r.mu.Unlock()
}

// isProxyAddr reports whether addr is a registered proxy dial address.
func (r *proxyAddrRegistry) isProxyAddr(addr string) bool {
	r.mu.Lock()
	_, ok := r.addrs[addr]
	r.mu.Unlock()
	return ok
}

// canonicalProxyAddr mirrors net/http's private canonicalAddr (GOROOT
// net/http/transport.go): for a proxied request, the address handed to
// DialContext is JoinHostPort(idnaASCII(u.Hostname()), port-or-scheme-default).
// For an ASCII host idnaASCII returns it unchanged (no case folding), which
// is where the mirror is byte-exact; a non-ASCII proxy host registers its
// unicode form while the transport would dial the punycode form — such a
// miss falls back to the STRICTER target policy (fail closed), never to a
// looser one, so the edge is deliberately not handled with an IDNA
// dependency.
func canonicalProxyAddr(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = proxySchemePort(u.Scheme)
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// proxySchemePort mirrors net/http's schemePort defaults for the schemes a
// proxy URL may carry.
func proxySchemePort(scheme string) string {
	switch scheme {
	case "http":
		return "80"
	case "https":
		return "443"
	case "socks5", "socks5h":
		return "1080"
	default:
		return ""
	}
}

// dialProxyAddress dials the address of a configured proxy: resolve the
// proxy host (an IP literal needs no DNS), reject reserved-range addresses
// with ProxyAddressAllowed — the transport-level backstop enforcing the
// same policy the save-time validation applies, so a value that slipped
// past or changed under the stored URL still cannot be dialed — and connect
// to the first address that passes. The target behind the proxy is never
// resolved or validated here: it is the proxy's side of the channel.
func dialProxyAddress(ctx context.Context, dialer *net.Dialer, network, addr string, connectTimeout time.Duration) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("split proxy host/port: %w", err)
	}
	var ips []net.IPAddr
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IPAddr{{IP: ip}}
	} else {
		// Same resolution contract as target dials: bounded by the caller's
		// ctx, never by connectTimeout (see safeDialContext's doc comment).
		ips, err = resolveHost(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("resolve proxy address %q: %w", host, err)
		}
	}
	connectCtx := ctx
	if connectTimeout > 0 {
		var cancel context.CancelFunc
		connectCtx, cancel = context.WithTimeout(ctx, connectTimeout)
		defer cancel()
	}
	return dialValidatedIPs(connectCtx, dialer, network, ips, port, checkProxyAddressAllowed)
}

// checkProxyAddressAllowed adapts ProxyAddressAllowed to dialValidatedIPs's
// policy shape, with the message wording the save-time validation already
// uses so both surfaces read identically.
func checkProxyAddressAllowed(ip net.IP) error {
	if ProxyAddressAllowed(ip) {
		return nil
	}
	return fmt.Errorf("proxy address %s is in a reserved range (multicast, benchmark, reserved, or unspecified)", ip)
}

// safeDialContext resolves addr's host exactly once, validates every
// returned IP, and connects directly to the first one that passes — the
// original hostname is never re-resolved for the actual connection (a
// second, independent resolution would reopen a
// DNS-rebinding window between "checked" and "connected").
//
// DNS resolution uses ctx (the caller's own, longer-lived budget) directly,
// NOT connectTimeout. connectTimeout is a dial-phase-only bound: it is
// enforced as a SHARED deadline across the multi-IP DIAL phase alone (see
// dialResolvedIPs), applied AFTER resolution has already completed. Sharing
// connectTimeout across resolution too (a prior version of this function did)
// let a slow-but-healthy resolution — a cold cache or a long CNAME chain,
// commonly a few seconds — eat into the dial budget: with a 5s
// connectTimeout, a 4s resolution left only ~1s for the dial itself, so
// every candidate IP failed with DeadlineExceeded and the caller saw a bogus
// failover/502 despite DNS and the upstream both being healthy. Without this
// dial timeout, each IP would independently consume a full connectTimeout
// (via dialer.Timeout), so N unreachable IPs would take N*connectTimeout
// instead of connectTimeout. The shared context deadline is the
// authoritative dial budget; dialer.Timeout is kept as a per-dial safety net
// but the context deadline always wins — once it expires, remaining IPs fail
// immediately with ctx.Err().
//
// A zero connectTimeout means "no dial-level bound" (see NewTransport's doc
// comment) and must NOT reach context.WithTimeout: WithTimeout(ctx, 0)
// produces an already-expired context, which would fail every dial
// immediately instead of disabling the bound. In that case the parent ctx is
// used directly, unmodified, for the dial phase too.

// resolveHost is net.DefaultResolver.LookupIPAddr behind a variable so a test
// can see which context resolution is handed. That is the whole of the
// "connectTimeout does not bound DNS" contract, and it is invisible from
// outside: an implementation that wrongly passes the connect deadline to the
// resolver still returns the same connection for any reachable host, and any
// test that tries to catch it by making the deadline tiny is really testing the
// platform's clock granularity rather than this code.
var resolveHost = func(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

func safeDialContext(ctx context.Context, dialer *net.Dialer, network, addr string, allowPrivate bool, connectTimeout time.Duration) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("split host/port: %w", err)
	}
	// Resolution is bounded only by the caller's own ctx (the per-attempt
	// budget), never by connectTimeout — see the doc comment above.
	ips, err := resolveHost(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", host, err)
	}
	connectCtx := ctx
	if connectTimeout > 0 {
		var cancel context.CancelFunc
		connectCtx, cancel = context.WithTimeout(ctx, connectTimeout)
		defer cancel()
	}
	return dialResolvedIPs(connectCtx, dialer, network, ips, port, allowPrivate)
}

// dialResolvedIPs tries each resolved IP in order, skipping any that fails
// checkIPAllowed, and dials the first one that both passes the check and
// connects successfully. Split out from safeDialContext so tests can feed
// it a fixed IP list without depending on real DNS resolution.
func dialResolvedIPs(ctx context.Context, dialer *net.Dialer, network string, ips []net.IPAddr, port string, allowPrivate bool) (net.Conn, error) {
	return dialValidatedIPs(ctx, dialer, network, ips, port, func(ip net.IP) error {
		return checkIPAllowed(ip, allowPrivate)
	})
}

// dialValidatedIPs is the policy-generic core both dial paths share (target
// dials via checkIPAllowed, proxy dials via checkProxyAddressAllowed): try
// each IP in order, skip the ones the policy rejects, dial the first one
// that both passes and connects, and surface the last failure otherwise.
func dialValidatedIPs(ctx context.Context, dialer *net.Dialer, network string, ips []net.IPAddr, port string, validate func(net.IP) error) (net.Conn, error) {
	var lastErr error
	for _, ipAddr := range ips {
		if err := validate(ipAddr.IP); err != nil {
			lastErr = err
			continue
		}
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ipAddr.IP.String(), port))
		if err != nil {
			lastErr = err
			continue
		}
		return conn, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no addresses to dial")
	}
	return nil, lastErr
}
