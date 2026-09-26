package gates

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSystemdUnitTemplatesCarryRestartRateLimit pins the restart rate-limit
// lines in both systemd unit templates of scripts/install.sh.
//
// The units set Restart=always with RestartSec=3. That combination quietly
// defeats systemd's *default* rate limit: 5 starts per 10 seconds can never
// accumulate when every attempt spends 3 seconds running before it dies, so a
// startup loop — a failed upgrade whose migration aborts on every boot, each
// round rewriting the multi-gigabyte backup before exiting — restarts forever,
// grinding the disk with no one watching. The explicit window below
// (5 starts per 300 seconds) is reachable by that same cadence (5 × ~3s of
// life fits in 300s with room to spare), so the loop stops itself and the
// unit settles into failed state until an operator runs
// `systemctl reset-failed && systemctl restart`.
//
// Both scopes matter: the system unit serves production hosts, the user unit
// is what a non-root install gets. A template regressing here still installs,
// still passes the installer's own health check, and only shows its missing
// brake the next time a bad release loops — this check makes that regression
// loud at review time instead.
//
// Installs made before the limit existed cannot be fixed by this script; the
// README's service-install section documents the two lines for existing
// deployments to add by hand.
func TestSystemdUnitTemplatesCarryRestartRateLimit(t *testing.T) {
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "scripts", "install.sh"))
	if err != nil {
		t.Fatalf("reading scripts/install.sh: %v", err)
	}
	// A Windows checkout gets this file with CRLF endings (Git for Windows
	// defaults to core.autocrlf=true and the repo ships no .gitattributes
	// pinning it to LF). The extractor below matches `}` exactly, so feed
	// it LF-only text regardless of the platform that ran the checkout.
	src := strings.ReplaceAll(string(b), "\r\n", "\n")

	for _, fn := range []string{"write_systemd_system", "write_systemd_user"} {
		body := shellFunctionBody(t, src, fn)

		// Restart=always stays: the limit exists to bound a looping unit,
		// not to change how a healthy one is supervised. Losing it would
		// trade the death spiral for a weaker failure mode.
		for _, line := range []string{
			"StartLimitIntervalSec=300",
			"StartLimitBurst=5",
			"Restart=always",
		} {
			if !strings.Contains(body, line) {
				t.Errorf("%s: unit template lost `%s` — without it a startup loop restarts forever under RestartSec=3 (the default 5-per-10s limit is mathematically unreachable at that cadence)", fn, line)
			}
		}

		// The StartLimit* directives belong to the [Unit] section (they moved
		// there from [Service] in systemd 230); inside [Service] a modern
		// systemd ignores them, which reads exactly like a working limit
		// until the day it is needed. Pin their section.
		unitIdx := strings.Index(body, "[Unit]")
		serviceIdx := strings.Index(body, "[Service]")
		limitIdx := strings.Index(body, "StartLimitIntervalSec=")
		if unitIdx < 0 || serviceIdx < 0 || limitIdx < 0 {
			t.Errorf("%s: unit template is missing a [Unit]/[Service]/StartLimit line the section check needs (got [Unit]@%d [Service]@%d StartLimit@%d); refusing to pass by inspecting nothing", fn, unitIdx, serviceIdx, limitIdx)
			continue
		}
		if unitIdx >= limitIdx || limitIdx >= serviceIdx {
			t.Errorf("%s: StartLimitIntervalSec must sit in the [Unit] section (before [Service]); systemd ignores it inside [Service]", fn)
		}
	}
}

// shellFunctionBody extracts one `name() { ... }` body from a shell script,
// from the defining line to the closing brace at column 0. The unit templates
// are heredocs inside these functions, so the heredoc text is part of the
// body. A missing function is fatal, never an empty string: a check that
// silently matched nothing reads exactly like a clean pass.
func shellFunctionBody(t *testing.T, src, name string) string {
	t.Helper()
	start := -1
	for i, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(line, name+"() {") {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("scripts/install.sh no longer defines %s(); the rate-limit check inspected nothing", name)
	}
	lines := strings.Split(src, "\n")
	for j := start + 1; j < len(lines); j++ {
		if lines[j] == "}" {
			body := strings.Join(lines[start+1:j], "\n")
			if strings.TrimSpace(body) == "" {
				t.Fatalf("%s() has an empty body; the rate-limit check inspected nothing", name)
			}
			return body
		}
	}
	t.Fatalf("%s() has no closing brace at column 0; cannot extract its body", name)
	return ""
}
