// Package safehttp provides an SSRF-safe outbound HTTP transport shared by
// M2's provider connection tests and (per design doc §5) intended for
// future M6 gateway relay calls. Every dial validates the resolved IP
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
// guaranteed-exhaustive one — design doc §5 notes they should be cross-checked
// against the IANA Special-Purpose Address Registry as it evolves.
func checkIPAllowed(ip net.IP, allowPrivate bool) error {
	normalized := ip
	if v4 := ip.To4(); v4 != nil {
		// Normalize IPv4-mapped IPv6 addresses (::ffff:a.b.c.d) before
		// checking — otherwise this exact form bypasses a pure IPv4 CIDR
		// match (design doc §5 item 1).
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
func NewTransport(allowPrivate bool) *http.Transport {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Transport{
		Proxy: nil, // design doc §5 item 3: never honor HTTP_PROXY/HTTPS_PROXY/NO_PROXY
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return safeDialContext(ctx, dialer, network, addr, allowPrivate)
		},
	}
}

// safeDialContext resolves addr's host exactly once, validates every
// returned IP, and connects directly to the first one that passes — the
// original hostname is never re-resolved for the actual connection (design
// doc §5 item 2: a second, independent resolution would reopen a
// DNS-rebinding window between "checked" and "connected").
func safeDialContext(ctx context.Context, dialer *net.Dialer, network, addr string, allowPrivate bool) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("split host/port: %w", err)
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", host, err)
	}
	return dialResolvedIPs(ctx, dialer, network, ips, port, allowPrivate)
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
