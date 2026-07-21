package version

import "testing"

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
