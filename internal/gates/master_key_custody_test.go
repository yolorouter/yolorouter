package gates

import (
	"fmt"
	"go/ast"
	"strings"
	"testing"
)

// TestMasterKeyHasOneCustodian pins that crypto.SecretBox is the ONLY way
// production code under internal/ touches the at-rest master key. Two
// concrete regressions are banned:
//
//   - a struct field named masterKey/MasterKey — the pre-SecretBox shape,
//     where six structs each held their own copy of the raw key bytes and
//     every new module grew one more;
//   - a direct call to the crypto package's bare Encrypt/Decrypt — the
//     recipe that forces the caller to supply key bytes, i.e. to hold them.
//
// The config hand-off (router.Deps.ProviderMasterKey, decoded once in
// cmd/yolorouter and boxed once in router.New) is intentionally outside
// both patterns: it is named neither masterKey nor MasterKey, and it never
// calls the bare functions. A module that legitimately needs to encrypt or
// decrypt a stored secret takes a crypto.SecretBox.
//
// Known heuristic edges, accepted: a field smuggling key bytes under a
// different name is not caught (the call-site ban is the stronger net), and
// a dot-import of the crypto package would make bare calls invisible to the
// selector check — dot imports collide with the ubiquitous "crypto" name
// and don't survive lint, so no carve-out is spent on them.
func TestMasterKeyHasOneCustodian(t *testing.T) {
	files := parseTree(t, "internal")
	inspectedFiles := 0
	cryptoImporters := 0
	var violations []string
	for _, f := range files {
		if strings.HasSuffix(f.rel, "_test.go") {
			continue
		}
		inspectedFiles++

		// Resolve the local name the crypto package is imported under in
		// THIS file (default "crypto", aliased e.g. "ycrypto" elsewhere).
		cryptoName := ""
		for _, imp := range f.ast.Imports {
			if strings.Trim(imp.Path.Value, `"`) == "github.com/yolorouter/yolorouter/pkg/crypto" {
				cryptoImporters++
				if imp.Name != nil {
					cryptoName = imp.Name.Name
				} else {
					cryptoName = "crypto"
				}
			}
		}

		rel := f.rel
		ast.Inspect(f.ast, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.StructType:
				for _, fld := range node.Fields.List {
					for _, name := range fld.Names {
						if name.Name == "masterKey" || name.Name == "MasterKey" {
							violations = append(violations, fmt.Sprintf(
								"%s: struct field %q — hold a crypto.SecretBox instead of raw key bytes",
								rel, name.Name))
						}
					}
				}
			case *ast.CallExpr:
				if cryptoName == "" {
					return true
				}
				sel, ok := node.Fun.(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "Encrypt" && sel.Sel.Name != "Decrypt") {
					return true
				}
				if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == cryptoName {
					violations = append(violations, fmt.Sprintf(
						"%s: direct %s.%s call — go through a crypto.SecretBox",
						rel, cryptoName, sel.Sel.Name))
				}
			}
			return true
		})
	}
	if inspectedFiles == 0 {
		t.Fatal("inspected no production files; the check would pass by finding nothing")
	}
	// The call-site ban is only exercised where the crypto package is in
	// scope; if no production file imports it anymore, this gate is checking
	// air and needs re-pointing at wherever secret handling moved.
	if cryptoImporters == 0 {
		t.Fatal("no production file under internal/ imports pkg/crypto; the bare-call ban inspected nothing")
	}
	for _, v := range violations {
		t.Error(v)
	}
}
