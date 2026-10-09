// Package safehttp provides an SSRF-safe outbound HTTP transport shared by
// provider connection tests and intended for
// future gateway relay calls. Every dial validates the resolved IP
// before connecting; proxy environment variables are disabled; redirects
// are not followed by callers that use http.Client{CheckRedirect} (left to
// the caller, since this package only controls the Transport).
package safehttp

import (
	"context"
	"fmt"
	"net"
	"net/http"
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

// NewTransport returns an *http.Transport with SSRF protections wired in:
// custom DialContext validating every resolved IP before connecting,
// environment-variable proxying disabled, and no other defaults changed.
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
	dialer := &net.Dialer{Timeout: connectTimeout}
	return &http.Transport{
		Proxy:               nil, // never honor HTTP_PROXY/HTTPS_PROXY/NO_PROXY
		TLSHandshakeTimeout: tlsHandshake,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return safeDialContext(ctx, dialer, network, addr, allowPrivate, connectTimeout)
		},
	}
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
	var lastErr error
	for _, ipAddr := range ips {
		if err := checkIPAllowed(ipAddr.IP, allowPrivate); err != nil {
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
