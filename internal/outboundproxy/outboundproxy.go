// Package outboundproxy owns the outbound proxy configuration vocabulary
// and the two pure decisions built on it: whether a candidate proxy URL is
// storable (save-time validation), and which proxy one outbound request
// should use (priority resolution). Both are side-effect-free functions so
// every transport call site — relay, connection probes, secondary fetches —
// resolves through the exact same logic.
package outboundproxy

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/yolorouter/yolorouter/internal/service/safehttp"
)

// Global proxy modes, stored in system_settings.proxy_mode.
const (
	// ModeOff sends all upstream traffic directly — the shipped default, so
	// an existing deployment changes nothing on upgrade.
	ModeOff = "off"
	// ModeSystem resolves proxies from the process environment
	// (HTTP_PROXY/HTTPS_PROXY/NO_PROXY), the same standard-library
	// convention container orchestrators inject.
	ModeSystem = "system"
	// ModeCustom proxies through the admin-declared proxy_url.
	ModeCustom = "custom"
)

// ProviderModeInherit is the providers.proxy_mode value meaning "this
// provider takes the global setting" — the column default, so every
// existing provider row keeps behaving exactly as before the column
// existed. The other legal provider values are ModeOff (force direct even
// when the global setting proxies) and ModeCustom (proxy through this
// provider's own proxy_url). ModeSystem is deliberately NOT a provider
// value: system mode is a statement about the whole process environment,
// and a per-provider variant of it would read env vars under one provider
// but not another for no operational benefit.
const ProviderModeInherit = ""

// ValidGlobalMode reports whether mode is one of the three storable global
// proxy modes.
func ValidGlobalMode(mode string) bool {
	return mode == ModeOff || mode == ModeSystem || mode == ModeCustom
}

// ValidProviderMode reports whether mode is one of the three storable
// provider proxy modes ("" = inherit).
func ValidProviderMode(mode string) bool {
	return mode == ProviderModeInherit || mode == ModeOff || mode == ModeCustom
}

// supportedSchemes are the proxy URL schemes the standard library transport
// handles natively. socks5 and socks5h are equivalent in Go — both resolve
// DNS on the proxy side — so socks5h is accepted purely as an alias for
// users coming from curl conventions; the stored URL passes through
// unchanged and the transport treats both identically.
var supportedSchemes = map[string]bool{
	"http":    true,
	"https":   true,
	"socks5":  true,
	"socks5h": true,
}

// ValidateProxyURL checks that raw is a storable outbound proxy URL:
// parseable, host present, scheme one of the four the transport supports,
// and the proxy address outside the always-denied reserved ranges.
//
// Address policy: a proxy address is admin-declared trusted configuration,
// so private and loopback addresses are allowed on purpose — a LAN proxy or
// a loopback test stub is the primary self-hosted topology — and only the
// reserved ranges that have no legitimate unicast use (multicast,
// benchmark, reserved, unspecified) are rejected. IP literals are checked
// directly; a named host is resolved best-effort with the caller's ctx and
// each result checked — a resolution failure accepts the URL rather than
// rejecting it, because save-time DNS is advisory: the transport enforces
// the same range check on every dial, which is the authoritative gate.
func ValidateProxyURL(ctx context.Context, raw string) error {
	if raw == "" {
		return fmt.Errorf("proxy URL is empty")
	}
	u, err := ParseProxyURL(raw)
	if err != nil {
		return err
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("proxy URL %q has no host", raw)
	}
	if ip := net.ParseIP(host); ip != nil {
		if !safehttp.ProxyAddressAllowed(ip) {
			return fmt.Errorf("proxy address %s is in a reserved range (multicast, benchmark, reserved, or unspecified)", host)
		}
		return nil
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil {
		// Unresolvable at save time is not a verdict: the name may only be
		// resolvable where the router actually runs (docker networks, split
		// DNS). The transport re-checks the address on every dial.
		return nil
	}
	for _, ip := range ips {
		if !safehttp.ProxyAddressAllowed(ip) {
			return fmt.Errorf("proxy address %s resolves to %s, which is in a reserved range (multicast, benchmark, reserved, or unspecified)", host, ip)
		}
	}
	return nil
}

// ParseProxyURL parses raw and enforces the transport's supported scheme
// set. It is the single parse path for stored proxy URLs so a value that
// once validated can never fail to parse differently at resolution time.
func ParseProxyURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("proxy URL %q is not parseable: %w", raw, err)
	}
	if !supportedSchemes[u.Scheme] {
		return nil, fmt.Errorf("proxy URL %q must use one of the http, https, socks5, or socks5h schemes", raw)
	}
	return u, nil
}
