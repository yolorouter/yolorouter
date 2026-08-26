package compressors

import (
	"context"
	"regexp"
	"strings"
)

var (
	// npm's homogeneous noise classes: deprecation warnings (one per
	// transitive package, all saying "stop using this") and verbose-mode
	// http fetch traces. Both old (WARN) and current (warn) casings.
	npmDeprecatedRe = regexp.MustCompile(`^npm (warn|WARN) deprecated `)
	// Only SUCCESSFUL fetch traces are noise. A 401/404/5xx fetch line names
	// the failing registry, status and latency — diagnostic evidence the
	// later error block may not repeat — and stays verbatim.
	npmHTTPFetchRe = regexp.MustCompile(`^npm (verb )?http fetch \S+ (200|201|204|304)\b`)

	// pnpm's noise classes: the rewritten-in-place progress updates (only the
	// final one carries the totals that matter), the +++ progress bar, and
	// the same deprecation warnings under pnpm's " WARN " prefix.
	pnpmProgressRe   = regexp.MustCompile(`^Progress: resolved \d+`)
	pnpmBarRe        = regexp.MustCompile(`^\++$`)
	pnpmDeprecatedRe = regexp.MustCompile(`^\s*WARN\s+deprecated `)
	// pnpmSignatureRe gates folding on real pnpm framing. A bare +++ line or
	// a " WARN " prefix also occurs in other tools' output; without a
	// Progress/Packages/Done line this content is not pnpm's to touch.
	pnpmSignatureRe = regexp.MustCompile(`^(Progress: resolved \d+|Packages: [+-]?\d+|Done in \d+(\.\d+)?\s*m?s\b)`)
)

// Npm is a lossless-for-signal compressor for npm install output. It folds
// the homogeneous noise — deprecation warnings and verbose http fetch
// traces — into count markers, and preserves every error line (npm error /
// npm ERR! blocks describe the dependency conflict line by line), the
// install summary, and the audit outcome. Output with nothing to fold is
// returned unchanged so the chain can hand the block to the next candidate.
type Npm struct{}

func (n *Npm) Name() string { return "npm" }

func (n *Npm) Compress(ctx context.Context, content string) (string, error) {
	lines := strings.Split(content, "\n")
	var b strings.Builder
	deprecated, fetches := 0, 0
	var fence FenceTracker
	for i, raw := range lines {
		if i%256 == 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			default:
			}
		}
		if fence.Line(raw) || fence.Inside() {
			b.WriteString(raw)
			b.WriteByte('\n')
			continue
		}
		ln := ansiRe.ReplaceAllString(raw, "")
		if npmDeprecatedRe.MatchString(ln) {
			deprecated++
			continue
		}
		if npmHTTPFetchRe.MatchString(ln) {
			fetches++
			continue
		}
		b.WriteString(raw)
		b.WriteByte('\n')
	}
	if (deprecated == 0 && fetches == 0) || fence.Inside() {
		// Nothing folded, or a fence ran unclosed to EOF (the marker would
		// land inside the quoted span) — leave the block untouched.
		return content, nil
	}
	out := strings.TrimRight(b.String(), "\n")
	if deprecated > 0 {
		out += "\n" + collapsedMarker(deprecated, "deprecation warnings")
	}
	if fetches > 0 {
		out += "\n" + collapsedMarker(fetches, "http fetch lines")
	}
	return out, nil
}

// Pnpm is a lossless-for-signal compressor for pnpm install output. It folds
// the repeated progress updates (keeping the final one — it alone carries
// the resolved/reused/downloaded totals), the +++ progress bar, and the
// deprecation warnings, and preserves every error block (ERR_PNPM_*), the
// dependency lists, and the completion line. Output with nothing to fold is
// returned unchanged so the chain can hand the block to the next candidate.
type Pnpm struct{}

func (p *Pnpm) Name() string { return "pnpm" }

func (p *Pnpm) Compress(ctx context.Context, content string) (string, error) {
	lines := strings.Split(content, "\n")
	// Signature gate BEFORE any folding, fence-outside lines only: a quoted
	// example must not authorize folding the rest.
	var sigFence FenceTracker
	sawSignature := false
	for i, raw := range lines {
		if i%256 == 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			default:
			}
		}
		if sigFence.Line(raw) || sigFence.Inside() {
			continue
		}
		if pnpmSignatureRe.MatchString(ansiRe.ReplaceAllString(raw, "")) {
			sawSignature = true
			break
		}
	}
	if !sawSignature {
		return content, nil
	}
	// The final progress line is signal and every earlier one is a stale
	// version of it, so its position must be known before the fold walk. The
	// search shares the walk's fence handling — a fenced Progress example
	// after the real final line must not demote the real totals to
	// "intermediate".
	lastProgress := -1
	var scanFence FenceTracker
	for i, raw := range lines {
		if i%256 == 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			default:
			}
		}
		if scanFence.Line(raw) || scanFence.Inside() {
			continue
		}
		if pnpmProgressRe.MatchString(ansiRe.ReplaceAllString(raw, "")) {
			lastProgress = i
		}
	}
	var b strings.Builder
	progress, deprecated := 0, 0
	var fence FenceTracker
	for i, raw := range lines {
		if i%256 == 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			default:
			}
		}
		if fence.Line(raw) || fence.Inside() {
			b.WriteString(raw)
			b.WriteByte('\n')
			continue
		}
		ln := ansiRe.ReplaceAllString(raw, "")
		if pnpmProgressRe.MatchString(ln) && i != lastProgress {
			progress++
			continue
		}
		if pnpmBarRe.MatchString(ln) {
			progress++
			continue
		}
		if pnpmDeprecatedRe.MatchString(ln) {
			deprecated++
			continue
		}
		b.WriteString(raw)
		b.WriteByte('\n')
	}
	if (progress == 0 && deprecated == 0) || fence.Inside() {
		// Nothing folded, or a fence ran unclosed to EOF — leave untouched.
		return content, nil
	}
	out := strings.TrimRight(b.String(), "\n")
	if progress > 0 {
		out += "\n" + collapsedMarker(progress, "progress lines")
	}
	if deprecated > 0 {
		out += "\n" + collapsedMarker(deprecated, "deprecation warnings")
	}
	return out, nil
}
