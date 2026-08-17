// Package gates holds the structural checks that keep the gateway kernel
// decoupled from the capabilities plugged into it.
//
// The import-direction rules live in .golangci.yml as depguard rules, where a
// mature tool already handles aliases and file matching. What lives here is
// what no off-the-shelf linter expresses: properties of the fact vocabulary
// and of the candidate loop that hold today only because every reviewer knows
// them. Prose knowledge stops scaling the moment a large amount of new code is
// written against these seams by people who have not absorbed it — these
// checks are that knowledge, executable.
//
// Everything here is a _test.go file, so none of it ships. `make gates` runs
// this package plus the behavioural tests that live next to the code they
// exercise (anchored by name in behavioural_anchor_test.go, so renaming one
// away cannot silently shrink the gate).
//
// Writing a check is half the work. A check that cannot fail is worse than no
// check, because it reports that something was verified. Every assertion in
// this package has been run against a deliberate violation and seen to fail,
// and several also assert they found a non-zero amount of code to inspect —
// a check that silently matched nothing reads exactly like a clean pass.
package gates

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// modulePath is this module's import prefix. Read from go.mod rather than
// hardcoded, so the checks keep working under a rename or a fork without
// anyone remembering to update a constant.
func modulePath(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "go.mod"))
	if err != nil {
		t.Fatalf("reading go.mod: %v", err)
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	t.Fatal("go.mod declares no module path")
	return ""
}

// repoRoot walks up from this package until it finds the go.mod, so the checks
// do not depend on where the test binary was invoked from.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory")
		}
		dir = parent
	}
}

// goFile is one parsed source file with the path it came from, so a failure
// can name a place rather than a package.
type goFile struct {
	rel  string // path relative to the repo root
	fset *token.FileSet
	ast  *ast.File
}

// pos renders a node's position as rel:line for a failure message.
func (f goFile) pos(n ast.Node) (string, int) {
	p := f.fset.Position(n.Pos())
	return f.rel, p.Line
}

// parseTree parses every Go file under one directory, recursively. It is the
// single walk every check in this package goes through — two checks walking
// the tree separately would eventually disagree about which files exist.
//
// Test files are included: the vocabulary properties checked here bind test
// code as much as production code (a test declaring a malformed record type is
// still declaring one), and the checks that need to exempt tests do so
// explicitly with a reason.
func parseTree(t *testing.T, dir string) []goFile {
	t.Helper()
	root := repoRoot(t)
	var out []goFile
	err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "vendor" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if perr != nil {
			return perr
		}
		rel, _ := filepath.Rel(root, path)
		out = append(out, goFile{rel: filepath.ToSlash(rel), fset: fset, ast: f})
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	if len(out) == 0 {
		t.Fatalf("no Go files under %s; the check would pass by finding nothing", dir)
	}
	return out
}

// methodReceiverName returns the bare type name a method is declared on,
// with any pointer star unwrapped — "Exchange" for both (rc Exchange) and
// (rc *Exchange), "" for non-method declarations or exotic receiver shapes.
// Shared by the checks that enumerate one type's methods: two scans
// hand-rolling the same unwrap would eventually drift about which shapes
// count.
func methodReceiverName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return ""
	}
	recv := fn.Recv.List[0].Type
	if star, ok := recv.(*ast.StarExpr); ok {
		recv = star.X
	}
	if id, ok := recv.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// recordField is one declared field of a Record-implementing struct: its name
// and a syntactic rendering of its type, good enough to compare against a
// known-bad shape without needing a type checker.
type recordField struct {
	name    string
	typeStr string
}

// recordType is one struct across the whole module that satisfies fact.Record
// — found by embedding fact.Base, the only way to reach the interface's
// unexported marker method from outside the fact package.
type recordType struct {
	pkg    string        // import path the type is declared under
	name   string        // the type's own name
	fields []recordField // declared fields, not counting the fact.Base embed
	file   string        // repo-relative path, for error messages
	line   int
}

// qualifiedName is how this type is written when referenced from another
// package: "example.com/mod/internal/fact.UsageReported".
func (r recordType) qualifiedName() string { return r.pkg + "." + r.name }

// fieldNames renders just the names, for checks that only need to know what a
// case arm touched rather than what shape a field is.
func (r recordType) fieldNames() []string {
	out := make([]string, len(r.fields))
	for i, f := range r.fields {
		out[i] = f.name
	}
	return out
}

// discoverRecordTypes returns one recordType per struct in the module that
// embeds fact.Base, keyed by qualifiedName.
//
// Shared by every check that needs to know what a Record actually carries —
// one asks whether any field looks like a routing effect, another whether a
// type switch's case arm reads all of them. Both reading one discovery is what
// keeps them from disagreeing about what a Record type even is.
func discoverRecordTypes(t *testing.T, files []goFile) map[string]recordType {
	t.Helper()
	mod := modulePath(t)
	out := map[string]recordType{}

	for _, f := range files {
		pkgPath := mod + "/" + filepath.ToSlash(filepath.Dir(f.rel))
		inFactPkg := strings.HasPrefix(f.rel, "internal/fact/")

		for _, decl := range f.ast.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok || st.Fields == nil {
					continue
				}
				if !embedsFactBaseIn(st, inFactPkg) {
					continue
				}
				var fields []recordField
				for _, field := range st.Fields.List {
					if len(field.Names) == 0 {
						continue // the fact.Base embed itself
					}
					typeStr := renderExprType(field.Type)
					for _, n := range field.Names {
						fields = append(fields, recordField{name: n.Name, typeStr: typeStr})
					}
				}
				rel, line := f.pos(ts)
				rt := recordType{pkg: pkgPath, name: ts.Name.Name, fields: fields, file: rel, line: line}
				out[rt.qualifiedName()] = rt
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("found no type embedding fact.Base; the discovery itself is broken")
	}
	return out
}

// embedsFactBaseIn reports whether a struct embeds fact.Base — the marker that
// satisfies Record's unexported method from outside the fact package — or, for
// a struct declared inside the fact package itself, embeds the bare Base.
//
// The package identifier is matched by its default name. A file importing fact
// under an alias and embedding the marker through it would slip past; today no
// file in the module aliases that import, and the discovery's own zero-found
// guard turns "every file started aliasing it" into a loud failure rather than
// a silent one.
func embedsFactBaseIn(st *ast.StructType, inFactPkg bool) bool {
	for _, field := range st.Fields.List {
		if len(field.Names) != 0 {
			continue // not embedded
		}
		switch t := field.Type.(type) {
		case *ast.SelectorExpr:
			if pkg, ok := t.X.(*ast.Ident); ok && pkg.Name == "fact" && t.Sel.Name == "Base" {
				return true
			}
		case *ast.Ident:
			if inFactPkg && t.Name == "Base" {
				return true
			}
		}
	}
	return false
}

// renderExprType renders a field's type expression back to source-like text,
// for the handful of shapes a struct field actually uses (plain identifiers
// and package-qualified names). It does not need to handle every Go type —
// only enough for a name-shaped comparison.
func renderExprType(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		if pkg, ok := t.X.(*ast.Ident); ok {
			return pkg.Name + "." + t.Sel.Name
		}
	case *ast.StarExpr:
		return "*" + renderExprType(t.X)
	}
	return ""
}
