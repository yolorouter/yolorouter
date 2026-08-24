package version

import (
	"context"
	"testing"
	"time"
)

// TestResolveRepo drives every precedence branch of the repo resolution:
// disabled short-circuits even with a configured repo; an explicit
// config repo overrides the compiled-in default; empty config falls back to
// the default; and everything-empty yields "" (feature disabled). The
// compiled-in default is a package var, so each case sets it explicitly to
// stay hermetic against other tests in this package.
func TestResolveRepo(t *testing.T) {
	orig := DefaultGitHubRepo
	t.Cleanup(func() { DefaultGitHubRepo = orig })

	tests := []struct {
		name        string
		enabled     bool
		githubRepo  string
		defaultRepo string
		want        string
	}{
		{name: "disabled returns empty even with configured repo", enabled: false, githubRepo: "a/b", defaultRepo: "owner/repo", want: ""},
		{name: "disabled returns empty even with only default", enabled: false, githubRepo: "", defaultRepo: "owner/repo", want: ""},
		{name: "enabled explicit repo wins over default", enabled: true, githubRepo: "fork/ce", defaultRepo: "owner/repo", want: "fork/ce"},
		{name: "enabled empty repo falls back to default", enabled: true, githubRepo: "", defaultRepo: "owner/repo", want: "owner/repo"},
		{name: "enabled empty repo and empty default is disabled", enabled: true, githubRepo: "", defaultRepo: "", want: ""},
		{name: "enabled explicit repo with empty default uses explicit", enabled: true, githubRepo: "a/b", defaultRepo: "", want: "a/b"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			DefaultGitHubRepo = tc.defaultRepo
			got := ResolveRepo(tc.enabled, tc.githubRepo)
			if got != tc.want {
				t.Fatalf("ResolveRepo(enabled=%v, githubRepo=%q) with default %q = %q, want %q",
					tc.enabled, tc.githubRepo, tc.defaultRepo, got, tc.want)
			}
		})
	}
}

// TestProxyURL covers the passthrough (empty proxy) and the trailing-slash
// normalization, so a proxy with or without a trailing slash both produce
// exactly one separator before the target URL.
func TestProxyURL(t *testing.T) {
	const raw = "https://github.com/yolorouter/yolorouter/releases/latest"
	tests := []struct {
		name  string
		proxy string
		want  string
	}{
		{name: "empty proxy passes through", proxy: "", want: raw},
		{name: "proxy with trailing slash", proxy: "https://gh.example.com/", want: "https://gh.example.com/" + raw},
		{name: "proxy without trailing slash", proxy: "https://gh.example.com", want: "https://gh.example.com/" + raw},
		{name: "proxy with multiple trailing slashes", proxy: "https://gh.example.com///", want: "https://gh.example.com/" + raw},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ProxyURL(tc.proxy, raw); got != tc.want {
				t.Fatalf("ProxyURL(%q, raw) = %q, want %q", tc.proxy, got, tc.want)
			}
		})
	}
}

func TestUpdateRoutes(t *testing.T) {
	tests := []struct {
		name  string
		proxy string
		want  []string
	}{
		{name: "explicit proxy keeps direct fallback", proxy: "https://gh.example.com/", want: []string{"https://gh.example.com/", ""}},
		{name: "no proxy: direct first then built-in mirror", proxy: "", want: []string{"", DefaultMirror}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := UpdateRoutes(tc.proxy)
			if len(got) != len(tc.want) {
				t.Fatalf("UpdateRoutes(%q) = %v, want %v", tc.proxy, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("UpdateRoutes(%q)[%d] = %q, want %q", tc.proxy, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestRouteAttemptContextSplitsShortWalk pins both ends of the reserve on a
// short walk — the release lookup's ten seconds. The first route has to keep
// enough to do its job (a half left an explicit proxy five seconds where it
// used to have ten, and a mirror answering in seven stopped working), and
// the route behind it has to get enough to do its (a fallback that only
// survives its predecessor's fast failures is no fallback against a hang).
func TestRouteAttemptContextSplitsShortWalk(t *testing.T) {
	const budget = 10 * time.Second
	walk, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	walkDeadline, _ := walk.Deadline()

	attempt, cancelAttempt := RouteAttemptContext(walk, false)
	defer cancelAttempt()
	attemptDeadline, ok := attempt.Deadline()
	if !ok {
		t.Fatal("attempt context lost the walk deadline")
	}

	// One number decides both ends: whatever is reserved is what the routes
	// behind get, and the rest is what this one keeps. Spelled out as a
	// literal rather than derived from reserveShare — deriving it would move
	// with the constant and pin nothing.
	const wantReserved = 2500 * time.Millisecond
	reserved := walkDeadline.Sub(attemptDeadline)
	if reserved.Round(500*time.Millisecond) != wantReserved {
		t.Errorf("reserved %v of a %v walk, want %v (leaving the first route %v)",
			reserved.Round(100*time.Millisecond), budget, wantReserved, budget-wantReserved)
	}
}

// TestRouteAttemptContextCapsLongWalk pins that a long walk is unaffected by
// the share: an asset download is budgeted in minutes, so fallbackReserve is
// the smaller of the two and decides.
func TestRouteAttemptContextCapsLongWalk(t *testing.T) {
	walk, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	walkDeadline, _ := walk.Deadline()

	attempt, cancelAttempt := RouteAttemptContext(walk, false)
	defer cancelAttempt()
	attemptDeadline, _ := attempt.Deadline()

	if reserved := walkDeadline.Sub(attemptDeadline); reserved.Round(time.Second) != fallbackReserve {
		t.Errorf("reserved %v of a 10m walk, want fallbackReserve (%v)", reserved.Round(time.Second), fallbackReserve)
	}
}

// TestRouteAttemptContextFinalSpendsEverything: the last route has nothing
// behind it to protect.
func TestRouteAttemptContextFinalSpendsEverything(t *testing.T) {
	walk, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	walkDeadline, _ := walk.Deadline()

	attempt, cancelAttempt := RouteAttemptContext(walk, true)
	defer cancelAttempt()

	attemptDeadline, ok := attempt.Deadline()
	if !ok || !attemptDeadline.Equal(walkDeadline) {
		t.Errorf("final attempt deadline = %v, want the walk's own %v", attemptDeadline, walkDeadline)
	}
}
