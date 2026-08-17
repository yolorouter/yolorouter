package gates

import (
	"go/ast"
	"go/token"
	"regexp"
	"strings"
	"testing"
)

// TestCommentsNameOnlyIdentifiersThatExist catches the residue a rename or
// deletion leaves in prose: a comment (or an error-message string) that still
// names a symbol the code no longer has. The compiler cannot see these — one
// sweep after a refactor found sixteen of them across five commits, each a
// comment confidently pointing a reader at something that does not exist.
//
// The check is deliberately narrow to stay quiet:
//
//   - In comments, only lowerCamel tokens (an internal capital, in the shape
//     of recordAttempt or writeServiceError) are checked. UpperCamel is skipped —
//     prose is full of product names shaped like it (OpenAI, GitHub) — and
//     snake_case is skipped because comments legitimately quote wire field
//     names that are not Go identifiers.
//   - In strings, only `rc.<field>` selector spellings are checked: an
//     assertion message quoting a field the Exchange no longer has.
//
// Existence is judged case-insensitively against every identifier in the
// scanned tree, so a comment quoting a wire name whose Go spelling only
// differs in case (nextPageToken vs NextPageToken) does not trip it.
func TestCommentsNameOnlyIdentifiersThatExist(t *testing.T) {
	files := parseTree(t, "internal")
	files = append(files, parseTree(t, "cmd")...)
	files = append(files, parseTree(t, "pkg")...)

	// codeIdents is every identifier the code itself uses or declares,
	// lowercased. proseIdents additionally holds every identifier-shaped
	// token inside a code string literal — that half is what keeps wire
	// vocabulary out of the comment violations: a comment saying
	// generateContent is not dangling when the code builds URLs from that
	// very string. The rc-selector check below deliberately uses ONLY
	// codeIdents: an Exchange field can never be a wire name, and judging it
	// against string contents would let a stale assertion message vouch for
	// itself.
	codeIdents := map[string]bool{}
	proseIdents := map[string]bool{}
	wordish := regexp.MustCompile(`[A-Za-z][A-Za-z0-9_]*`)
	for _, f := range files {
		ast.Inspect(f.ast, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.Ident:
				codeIdents[strings.ToLower(v.Name)] = true
			case *ast.BasicLit:
				if v.Kind == token.STRING {
					for _, w := range wordish.FindAllString(v.Value, -1) {
						proseIdents[strings.ToLower(w)] = true
					}
				}
			}
			return true
		})
	}

	// exchangeMembers is exactly what `rc.<x>` prose may name: the fields of
	// gateway.Exchange and the methods declared on it. Judging those
	// references against the whole tree's identifiers would let any
	// same-spelled symbol anywhere (a Close, a Status) vouch for a member
	// that is gone — the false pass this gate exists to prevent.
	exchangeMembers := map[string]bool{}
	for _, f := range files {
		if !strings.HasPrefix(f.rel, "internal/gateway/") {
			continue
		}
		ast.Inspect(f.ast, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.TypeSpec:
				if v.Name.Name != "Exchange" {
					return true
				}
				st, ok := v.Type.(*ast.StructType)
				if !ok {
					return true
				}
				for _, fld := range st.Fields.List {
					for _, name := range fld.Names {
						exchangeMembers[name.Name] = true
					}
				}
			case *ast.FuncDecl:
				if methodReceiverName(v) == "Exchange" {
					exchangeMembers[v.Name.Name] = true
				}
			}
			return true
		})
	}
	if len(exchangeMembers) == 0 {
		t.Fatal("no Exchange fields or methods found; the type moved or was renamed and the " +
			"rc-selector half of this check would pass by knowing nothing")
	}

	// allowedDanglingTokens are prose tokens that look like identifiers but
	// deliberately are not in this codebase. Every entry carries its reason;
	// an entry without one is a fix that should have happened instead.
	allowedDanglingTokens := map[string]string{
		"gofmt":      "tool name, not an identifier",
		"golangci":   "tool name, not an identifier",
		"goroutine":  "language term",
		"goroutines": "language term",
		"httptest":   "stdlib package quoted in prose without an import in that file",
		"lowercamel": "this check's own vocabulary",
		"uppercamel": "this check's own vocabulary",

		"macos": "product name",
		"vllm":  "product name",

		"hosta": "hypothetical host in test prose",
		"hostb": "hypothetical host in test prose",

		"manualreset":  "Windows CreateEvent parameter name",
		"initialstate": "Windows CreateEvent parameter name",

		"filestat":        "stdlib internal type (os.fileStat) quoted in prose",
		"rwunwrapper":     "net/http internal interface name quoted in prose",
		"backgroundread":  "net/http internal goroutine name quoted in prose",
		"resolvedoptions": "browser Intl API quoted in prose",

		"statusiskeyrotation":       "historical: one of the helpers classifyUpstreamStatus replaced",
		"statusiscandidatefailover": "historical: one of the helpers classifyUpstreamStatus replaced",
		"clienterrortypefor":        "historical: one of the helpers classifyUpstreamStatus replaced",
		"keyoutcome":                "historical: one of the helpers classifyUpstreamStatus replaced",

		// Stale references inside the parity-bound protocols tree: fixing the
		// prose there means a two-repo sync pass, so these are recorded debt
		// rather than silently passed.
		"partialresp":                 "protocols tree: prose name for the partial-response return; fix with the next parity sync",
		"checkdone":                   "protocols tree: stale caller name; fix with the next parity sync",
		"buildterminalusage":          "protocols tree: stale helper name; fix with the next parity sync",
		"buildmessagedeltapatchusage": "protocols tree: stale helper name; fix with the next parity sync",
	}

	lowerCamel := regexp.MustCompile(`\b[a-z][a-z0-9]*[A-Z][A-Za-z0-9]*\b`)
	rcSelector := regexp.MustCompile(`\brc\.([A-Za-z_][A-Za-z0-9_]*)`)

	check := func(rel string, line int, text string, fromString bool) {
		if !fromString {
			for _, tok := range lowerCamel.FindAllString(text, -1) {
				low := strings.ToLower(tok)
				if codeIdents[low] || proseIdents[low] || allowedDanglingTokens[low] != "" {
					continue
				}
				t.Errorf("%s:%d: comment names %q, which exists nowhere in the code: a rename or "+
					"deletion left this reference behind, and it now points a reader at nothing "+
					"(fix the comment, or allowlist the token with a reason)", rel, line, tok)
			}
		}
		// Exact-case and Exchange-scoped on purpose: a member reference in
		// prose is spelled the way the code spells it, and it must name a
		// member the TYPE has — not merely any identifier in the tree.
		for _, m := range rcSelector.FindAllStringSubmatch(text, -1) {
			if exchangeMembers[m[1]] || allowedDanglingTokens[strings.ToLower(m[1])] != "" {
				continue
			}
			t.Errorf("%s:%d: %q names an Exchange member that no longer exists — the field moved "+
				"or was renamed and this text kept the old spelling", rel, line, m[0])
		}
	}

	for _, f := range files {
		for _, cg := range f.ast.Comments {
			for _, c := range cg.List {
				pos := f.fset.Position(c.Pos())
				check(f.rel, pos.Line, c.Text, false)
			}
		}
		ast.Inspect(f.ast, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			pos := f.fset.Position(lit.Pos())
			check(f.rel, pos.Line, lit.Value, true)
			return true
		})
	}
}
