package outboundproxy

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

// Global is the global outbound proxy configuration (system_settings rows).
type Global struct {
	Mode string // off | system | custom
	URL  string // used when Mode == custom
}

// Override is one provider's per-provider proxy stance (providers columns).
// Mode "" means inherit the Global configuration.
type Override struct {
	Mode string // "" (inherit) | off | custom
	URL  string // used when Mode == custom
}

// EnvFunc reads one environment variable; nil means os.Getenv. Injected so
// the resolution is a pure function of its arguments and tests never depend
// on (or mutate) the real process environment.
type EnvFunc func(name string) string

func (e EnvFunc) get(name string) string {
	if e == nil {
		return os.Getenv(name)
	}
	return e(name)
}

// pickEnv returns the first non-empty value among names (uppercase first,
// lowercase fallback — the standard library's own precedence).
func (e EnvFunc) pickEnv(names ...string) string {
	for _, name := range names {
		if v := e.get(name); v != "" {
			return v
		}
	}
	return ""
}

// Resolve returns the proxy URL one outbound request to target should use,
// or nil for a direct connection. Priority: provider override wins over the
// global setting; an explicit provider "off" forces direct even when the
// global setting proxies; an inherit (empty) provider mode defers to the
// global mode. A malformed stored URL or an unknown stored mode surfaces as
// an error instead of silently falling back to direct — traffic the admin
// explicitly asked to proxy must never leak out unproxied because a stored
// value went bad.
func Resolve(target *url.URL, override Override, global Global, env EnvFunc) (*url.URL, error) {
	switch override.Mode {
	case ModeOff:
		return nil, nil
	case ModeCustom:
		return ParseProxyURL(override.URL)
	case ProviderModeInherit:
		return resolveGlobal(target, global, env)
	default:
		return nil, fmt.Errorf("outbound proxy: unknown provider proxy_mode %q", override.Mode)
	}
}

// ResolveGlobal resolves the proxy for a request with no provider binding
// (secondary fetches): the global setting alone, same error contract as
// Resolve.
func ResolveGlobal(target *url.URL, global Global, env EnvFunc) (*url.URL, error) {
	return resolveGlobal(target, global, env)
}

func resolveGlobal(target *url.URL, global Global, env EnvFunc) (*url.URL, error) {
	switch global.Mode {
	case ModeOff, "":
		return nil, nil
	case ModeCustom:
		return ParseProxyURL(global.URL)
	case ModeSystem:
		return proxyFromEnvironment(target, env)
	default:
		return nil, fmt.Errorf("outbound proxy: unknown global proxy_mode %q", global.Mode)
	}
}

// proxyFromEnvironment resolves the proxy for target from the process
// environment, following the standard library's ProxyFromEnvironment
// semantics: HTTPS_PROXY for https targets and HTTP_PROXY for http
// targets, each read uppercase-first with a lowercase fallback and never
// borrowed across schemes (an HTTP_PROXY-only environment leaves https
// targets direct, exactly as the standard library behaves); schemes other
// than http/https are never proxied. NO_PROXY exemptions apply, and
// localhost/loopback targets are always direct. With no proxy variables
// set the result is nil — system mode on a process without proxy env is
// exactly "off". Implemented here against an injectable env reader rather
// than calling http.ProxyFromEnvironment because the standard library
// caches the parsed environment process-wide on first use, which would
// couple resolution to whichever caller ran first instead of the current
// configuration.
func proxyFromEnvironment(target *url.URL, env EnvFunc) (*url.URL, error) {
	host := target.Hostname()
	if host == "localhost" {
		return nil, nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil, nil
	}
	if bypassed(target, env) {
		return nil, nil
	}
	var proxy string
	switch target.Scheme {
	case "https":
		proxy = env.pickEnv("HTTPS_PROXY", "https_proxy")
	case "http":
		proxy = env.pickEnv("HTTP_PROXY", "http_proxy")
	default:
		// The standard library only proxies the http and https schemes.
		return nil, nil
	}
	if proxy == "" {
		return nil, nil
	}
	return ParseProxyURL(proxy)
}

// bypassed reports whether NO_PROXY exempts target from proxying. Entry
// semantics follow the curl/Go convention: "*" exempts everything; an entry
// matches the exact host, any subdomain at a dot boundary ("example.com"
// matches "api.example.com"), an IP literal exactly, or a CIDR block when
// the entry contains a slash; an entry carrying a port matches only that
// port of the target.
func bypassed(target *url.URL, env EnvFunc) bool {
	list := env.pickEnv("NO_PROXY", "no_proxy")
	if list == "" {
		return false
	}
	host := target.Hostname()
	port := target.Port()
	for _, entry := range strings.Split(list, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if entry == "*" {
			return true
		}
		entryHost := entry
		// A port-bearing entry only matches when the target carries the
		// same port. splitHostPort handles the bracketed IPv6 form.
		if h, p, ok := splitHostPort(entry); ok {
			entryHost = h
			if p != "" && p != port {
				continue
			}
		}
		if hostMatchesEntry(host, entryHost) {
			return true
		}
	}
	return false
}

// splitHostPort splits "host:port" / "[v6]:port" and reports whether the
// entry carried a port at all. An unbracketed IPv6 literal (no port) does
// not split and is handled whole by hostMatchesEntry.
func splitHostPort(entry string) (host, port string, ok bool) {
	if !strings.Contains(entry, ":") {
		return "", "", false
	}
	if h, p, err := net.SplitHostPort(entry); err == nil {
		return h, p, true
	}
	return "", "", false
}

// hostMatchesEntry compares host against one port-less NO_PROXY entry.
func hostMatchesEntry(host, entry string) bool {
	entry = strings.TrimPrefix(entry, ".")
	if entry == "" {
		return false
	}
	if host == entry {
		return true
	}
	if strings.HasSuffix(host, "."+entry) {
		return true
	}
	// CIDR entries only apply to IP-literal targets.
	if strings.Contains(entry, "/") {
		if ip := net.ParseIP(host); ip != nil {
			if _, ipnet, err := net.ParseCIDR(entry); err == nil && ipnet.Contains(ip) {
				return true
			}
		}
		return false
	}
	// An IP-literal entry matches only the same IP (never as a "suffix" —
	// 1.2.3.4 must not "match" 3.4 by string tail).
	if entryIP := net.ParseIP(entry); entryIP != nil {
		if ip := net.ParseIP(host); ip != nil && ip.Equal(entryIP) {
			return true
		}
		return false
	}
	return false
}
