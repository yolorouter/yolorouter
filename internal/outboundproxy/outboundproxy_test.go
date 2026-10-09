package outboundproxy

import (
	"context"
	"net/url"
	"testing"
)

// envOf builds an EnvFunc from a map, so resolver tests never touch the
// real process environment.
func envOf(kv map[string]string) EnvFunc {
	return func(name string) string { return kv[name] }
}

func targetOf(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse target %q: %v", raw, err)
	}
	return u
}

// --- Save-time validation (the storable-URL contract) -----------------------

// TestValidateProxyURLAcceptsFourSchemes pins the four schemes the transport
// supports, including socks5h as an accepted alias of socks5 (both resolve
// DNS on the proxy side in Go; accepting both only spares curl users a
// rewrite). A loopback address is used throughout to also pin, in the same
// table, that private/loopback proxy addresses are legal.
func TestValidateProxyURLAcceptsFourSchemes(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1:7890",
		"https://127.0.0.1:7890",
		"socks5://127.0.0.1:1080",
		"socks5h://127.0.0.1:1080",
		// Round-trip shapes worth pinning alongside the schemes.
		"http://192.168.1.5:7890",            // LAN literal
		"http://user:pass@10.0.0.2:3128",     // embedded credentials
		"socks5://[::1]:1080",                // bracketed IPv6 loopback
		"http://127.0.0.1:7890/ignored/path", // path is legal
		"  socks5://127.0.0.1:1080  ",        // surrounding whitespace trims
	} {
		if err := ValidateProxyURL(context.Background(), raw); err != nil {
			t.Errorf("ValidateProxyURL(%q) = %v, want accepted", raw, err)
		}
	}
}

// TestValidateProxyURLRejectsIllegalValues covers the refused shapes: an
// unsupported scheme, a schemeless string that cannot parse as a URL, the
// empty string, and a URL whose host is missing.
func TestValidateProxyURLRejectsIllegalValues(t *testing.T) {
	for _, raw := range []string{
		"ftp://proxy.example.com:21", // scheme outside the supported four
		"proxy.example.com:7890",     // no scheme: url.Parse yields scheme=""
		"//proxy.example.com:7890",   // protocol-relative, still schemeless
		"://missing-scheme",          // unparseable
		"",                           // empty
		"http://",                    // scheme but no host
		"http://:7890",               // port but no host
	} {
		if err := ValidateProxyURL(context.Background(), raw); err == nil {
			t.Errorf("ValidateProxyURL(%q) = nil, want refused", raw)
		}
	}
}

// TestValidateProxyURLRejectsReservedRanges pins the address policy: only
// the always-denied reserved ranges are refused — multicast, benchmark,
// reserved, and the unspecified addresses — while private and loopback
// addresses are allowed (an admin-declared LAN or loopback proxy is the
// primary self-hosted topology).
func TestValidateProxyURLRejectsReservedRanges(t *testing.T) {
	for _, raw := range []string{
		"http://224.0.0.1:7890",    // multicast v4
		"http://239.255.255.1:80",  // multicast v4 upper edge
		"socks5://240.0.0.1:1080",  // reserved v4
		"http://255.255.255.255:1", // reserved v4 broadcast
		"http://0.0.0.0:7890",      // unspecified v4
		"http://[::]:7890",         // unspecified v6
		"http://[ff00::1]:7890",    // multicast v6
		"http://198.18.0.1:7890",   // benchmark range
		"http://198.19.255.1:80",   // benchmark range upper edge
	} {
		if err := ValidateProxyURL(context.Background(), raw); err == nil {
			t.Errorf("ValidateProxyURL(%q) = nil, want refused (reserved range)", raw)
		}
	}
	for _, raw := range []string{
		"http://127.0.0.1:7890",   // loopback v4 — allowed
		"http://127.8.8.8:7890",   // the whole 127/8 is loopback, not just .0.0.1
		"socks5://10.1.2.3:1080",  // private v4
		"http://172.16.0.9:3128",  // private v4 (172.16/12)
		"http://192.168.0.1:7890", // private v4 (192.168/16)
		"http://169.254.1.1:7890", // link-local incl. LAN-side metadata — admin-declared
		"http://100.64.0.1:7890",  // CGNAT / mesh range
		"http://[::1]:1080",       // loopback v6
		"http://[fe80::1]:7890",   // link-local v6
		"http://[fc00::1]:7890",   // unique-local v6
	} {
		if err := ValidateProxyURL(context.Background(), raw); err != nil {
			t.Errorf("ValidateProxyURL(%q) = %v, want allowed (private/loopback are legal proxy addresses)", raw, err)
		}
	}
}

// TestParseProxyURLKeepsSocks5hVerbatim pins the alias treatment: the
// stored socks5h URL passes through unchanged (scheme included). Go's
// transport treats socks5 and socks5h identically — both resolve DNS on the
// proxy side — so there is no local-DNS variant to normalize to.
func TestParseProxyURLKeepsSocks5hVerbatim(t *testing.T) {
	u, err := ParseProxyURL("socks5h://proxy.lan:1080")
	if err != nil {
		t.Fatalf("ParseProxyURL: %v", err)
	}
	if u.Scheme != "socks5h" || u.Host != "proxy.lan:1080" {
		t.Fatalf("socks5h must pass through verbatim, got scheme=%q host=%q", u.Scheme, u.Host)
	}
}

// --- Priority resolution ----------------------------------------------------

func TestResolveProviderCustomWinsOverGlobalCustom(t *testing.T) {
	got, err := Resolve(targetOf(t, "https://api.example.com/v1"),
		Override{Mode: ModeCustom, URL: "socks5://10.0.0.9:1080"},
		Global{Mode: ModeCustom, URL: "http://10.0.0.1:7890"},
		nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.String() != "socks5://10.0.0.9:1080" {
		t.Fatalf("provider custom must win, got %v", got)
	}
}

func TestResolveProviderOffOverridesGlobalCustom(t *testing.T) {
	got, err := Resolve(targetOf(t, "https://api.example.com/v1"),
		Override{Mode: ModeOff},
		Global{Mode: ModeCustom, URL: "http://10.0.0.1:7890"},
		nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != nil {
		t.Fatalf("provider off must force direct even under global custom, got %v", got)
	}
}

func TestResolveProviderOffOverridesGlobalSystem(t *testing.T) {
	got, err := Resolve(targetOf(t, "https://api.example.com/v1"),
		Override{Mode: ModeOff},
		Global{Mode: ModeSystem},
		envOf(map[string]string{"HTTPS_PROXY": "http://10.0.0.1:7890"}),
	)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != nil {
		t.Fatalf("provider off must force direct even under global system, got %v", got)
	}
}

func TestResolveProviderCustomOverridesGlobalSystem(t *testing.T) {
	got, err := Resolve(targetOf(t, "https://api.example.com/v1"),
		Override{Mode: ModeCustom, URL: "http://10.0.0.9:7890"},
		Global{Mode: ModeSystem},
		envOf(map[string]string{"HTTPS_PROXY": "http://10.0.0.1:7890"}),
	)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.String() != "http://10.0.0.9:7890" {
		t.Fatalf("provider custom must beat system env, got %v", got)
	}
}

func TestResolveInheritUsesGlobalCustom(t *testing.T) {
	got, err := Resolve(targetOf(t, "https://api.example.com/v1"),
		Override{Mode: ProviderModeInherit},
		Global{Mode: ModeCustom, URL: "http://10.0.0.1:7890"},
		nil,
	)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.String() != "http://10.0.0.1:7890" {
		t.Fatalf("inherit must fall through to global custom, got %v", got)
	}
}

func TestResolveEverythingOffIsDirect(t *testing.T) {
	got, err := Resolve(targetOf(t, "https://api.example.com/v1"),
		Override{Mode: ProviderModeInherit},
		Global{Mode: ModeOff},
		envOf(map[string]string{"HTTPS_PROXY": "http://10.0.0.1:7890"}), // off ignores env entirely
	)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != nil {
		t.Fatalf("off must be direct regardless of env, got %v", got)
	}
}

func TestResolveUnknownModesAreErrorsNotSilentDirect(t *testing.T) {
	if _, err := Resolve(targetOf(t, "https://a.example.com"), Override{Mode: "sometimes"}, Global{Mode: ModeOff}, nil); err == nil {
		t.Fatal("unknown provider mode must error, not silently go direct")
	}
	if _, err := Resolve(targetOf(t, "https://a.example.com"), Override{Mode: ProviderModeInherit}, Global{Mode: "sometimes"}, nil); err == nil {
		t.Fatal("unknown global mode must error, not silently go direct")
	}
}

func TestResolveCorruptStoredURLErrorsInsteadOfDirect(t *testing.T) {
	// A stored custom URL that no longer parses must fail the request
	// visibly; falling back to direct would route explicitly-proxied
	// traffic out an unproxied interface.
	if _, err := Resolve(targetOf(t, "https://a.example.com"),
		Override{Mode: ProviderModeInherit},
		Global{Mode: ModeCustom, URL: "ftp://not-a-supported-scheme"},
		nil,
	); err == nil {
		t.Fatal("corrupt stored proxy URL must error, not fall back to direct")
	}
}

// --- system mode (environment) ----------------------------------------------

func TestResolveSystemModeUsesHTTPSProxyForHTTPSTargets(t *testing.T) {
	got, err := Resolve(targetOf(t, "https://api.example.com/v1"),
		Override{Mode: ProviderModeInherit},
		Global{Mode: ModeSystem},
		envOf(map[string]string{
			"HTTPS_PROXY": "http://10.0.0.1:7890",
			"HTTP_PROXY":  "http://10.0.0.2:7890",
		}),
	)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.String() != "http://10.0.0.1:7890" {
		t.Fatalf("https target must use HTTPS_PROXY, got %v", got)
	}
}

func TestResolveSystemModeHTTPTargetFallsBackToHTTPProxy(t *testing.T) {
	// Scheme-specific reads with no cross-scheme fallback: https-only env
	// does not proxy plain http targets, and http-only env does not proxy
	// https targets either (pinned by the companion test below).
	got, err := Resolve(targetOf(t, "http://api.example.com/v1"),
		Override{Mode: ProviderModeInherit},
		Global{Mode: ModeSystem},
		envOf(map[string]string{"HTTP_PROXY": "http://10.0.0.2:7890"}),
	)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.String() != "http://10.0.0.2:7890" {
		t.Fatalf("http target must use HTTP_PROXY, got %v", got)
	}
	got, err = Resolve(targetOf(t, "http://api.example.com/v1"),
		Override{Mode: ProviderModeInherit},
		Global{Mode: ModeSystem},
		envOf(map[string]string{"HTTPS_PROXY": "http://10.0.0.1:7890"}),
	)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != nil {
		t.Fatalf("HTTPS_PROXY alone must not proxy an http target, got %v", got)
	}
}

// TestResolveSystemModeHTTPSTargetDoesNotFallBackToHTTPProxy pins the other
// half of the scheme-specific contract: HTTP_PROXY is for http targets only.
// A process that sets just HTTP_PROXY leaves https traffic direct, exactly
// as the standard library's ProxyFromEnvironment behaves — the variables
// are never borrowed across schemes.
func TestResolveSystemModeHTTPSTargetDoesNotFallBackToHTTPProxy(t *testing.T) {
	got, err := Resolve(targetOf(t, "https://api.example.com/v1"),
		Override{Mode: ProviderModeInherit},
		Global{Mode: ModeSystem},
		envOf(map[string]string{"HTTP_PROXY": "http://10.0.0.2:7890"}),
	)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != nil {
		t.Fatalf("HTTP_PROXY alone must not proxy an https target, got %v", got)
	}
}

func TestResolveSystemModeNoProxyExemptsMatchingHosts(t *testing.T) {
	env := envOf(map[string]string{
		"HTTPS_PROXY": "http://10.0.0.1:7890",
		"NO_PROXY":    "internal.example.com, .corp.example.com, 192.168.0.0/16, pinned.example.com:8443",
	})
	cases := []struct {
		target string
		want   string // "" means direct (nil)
	}{
		{"https://internal.example.com/v1", ""},                        // exact host match
		{"https://api.internal.example.com/v1", ""},                    // subdomain of an exact entry
		{"https://api.corp.example.com/v1", ""},                        // leading-dot entry matches subdomains
		{"https://corp.example.com/v1", ""},                            // leading-dot entry matches the bare host too
		{"https://192.168.4.5/v1", ""},                                 // CIDR entry covers IP target
		{"https://pinned.example.com:8443/v1", ""},                     // port-bearing entry matches same port
		{"https://pinned.example.com/v1", "http://10.0.0.1:7890"},      // port-bearing entry does NOT match other ports
		{"https://api.example.com/v1", "http://10.0.0.1:7890"},         // no entry matches
		{"https://notinternal.example.com/v1", "http://10.0.0.1:7890"}, // suffix must sit at a dot boundary
		{"https://internal-example.com/v1", "http://10.0.0.1:7890"},    // dots, not hyphens, delimit
		{"https://192.169.4.5/v1", "http://10.0.0.1:7890"},             // outside the CIDR
	}
	for _, tc := range cases {
		got, err := Resolve(targetOf(t, tc.target), Override{Mode: ProviderModeInherit}, Global{Mode: ModeSystem}, env)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", tc.target, err)
		}
		gotStr := ""
		if got != nil {
			gotStr = got.String()
		}
		if gotStr != tc.want {
			t.Errorf("Resolve(%q) = %q, want %q", tc.target, gotStr, tc.want)
		}
	}
}

func TestResolveSystemModeNoProxyWildcardExemptsEverything(t *testing.T) {
	got, err := Resolve(targetOf(t, "https://api.example.com/v1"),
		Override{Mode: ProviderModeInherit},
		Global{Mode: ModeSystem},
		envOf(map[string]string{"HTTPS_PROXY": "http://10.0.0.1:7890", "NO_PROXY": "*"}),
	)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != nil {
		t.Fatalf("NO_PROXY=* must exempt everything, got %v", got)
	}
}

func TestResolveSystemModeLoopbackTargetsAreNeverProxied(t *testing.T) {
	for _, target := range []string{
		"https://localhost/v1",
		"https://localhost:8443/v1",
		"https://127.0.0.1/v1",
		"https://[::1]:8443/v1",
	} {
		got, err := Resolve(targetOf(t, target), Override{Mode: ProviderModeInherit}, Global{Mode: ModeSystem},
			envOf(map[string]string{"HTTPS_PROXY": "http://10.0.0.1:7890", "NO_PROXY": ""}))
		if err != nil {
			t.Fatalf("Resolve(%q): %v", target, err)
		}
		if got != nil {
			t.Errorf("Resolve(%q) = %v, want direct (loopback/localhost never proxied)", target, got)
		}
	}
}

// TestResolveSystemModeWithoutEnvIsExactlyOff pins the equivalence: on a
// process with no proxy variables set, system mode resolves every target
// direct — the same answer as mode off.
func TestResolveSystemModeWithoutEnvIsExactlyOff(t *testing.T) {
	for _, target := range []string{"https://api.example.com/v1", "http://api.example.com/v1"} {
		got, err := Resolve(targetOf(t, target), Override{Mode: ProviderModeInherit}, Global{Mode: ModeSystem}, envOf(nil))
		if err != nil {
			t.Fatalf("Resolve(%q): %v", target, err)
		}
		if got != nil {
			t.Fatalf("system mode with no proxy env must be direct, got %v for %q", got, target)
		}
	}
}

// TestResolveSystemModeLowercaseEnvFallback covers the lowercase variable
// names container images often set.
func TestResolveSystemModeLowercaseEnvFallback(t *testing.T) {
	got, err := Resolve(targetOf(t, "https://api.example.com/v1"),
		Override{Mode: ProviderModeInherit},
		Global{Mode: ModeSystem},
		envOf(map[string]string{"https_proxy": "http://10.0.0.3:7890", "no_proxy": "skip.example.com"}),
	)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got == nil || got.String() != "http://10.0.0.3:7890" {
		t.Fatalf("lowercase https_proxy must be honored, got %v", got)
	}
}

// TestResolveGlobalForProviderlessRequests exercises the entry point a
// no-provider-bound consumer (secondary fetches) uses: global configuration
// alone, both arms.
func TestResolveGlobalForProviderlessRequests(t *testing.T) {
	got, err := ResolveGlobal(targetOf(t, "https://cdn.example.com/file"),
		Global{Mode: ModeCustom, URL: "socks5://10.0.0.9:1080"}, nil)
	if err != nil || got == nil || got.String() != "socks5://10.0.0.9:1080" {
		t.Fatalf("ResolveGlobal custom = %v, %v; want the custom proxy", got, err)
	}
	got, err = ResolveGlobal(targetOf(t, "https://cdn.example.com/file"), Global{Mode: ModeOff}, nil)
	if err != nil || got != nil {
		t.Fatalf("ResolveGlobal off = %v, %v; want direct", got, err)
	}
}

// TestEnvFuncNilDefaultsToOSENV pins that a nil EnvFunc reads the real
// process environment — the production wiring — rather than panicking or
// reading nothing.
func TestEnvFuncNilDefaultsToOSENV(t *testing.T) {
	t.Setenv("HTTP_PROXY_TEST_SENTINEL", "") // ensure cleanup semantics exist
	// Read a variable the test binary is guaranteed to have: GOCACHE is set
	// by the go tool for every test run; if it were ever absent the PATH
	// fallback still proves the os.Getenv route rather than a stub.
	var f EnvFunc
	if f.get("PATH") == "" && f.get("GOCACHE") == "" {
		t.Fatal("nil EnvFunc read neither PATH nor GOCACHE — it is not reading os.Getenv")
	}
}
