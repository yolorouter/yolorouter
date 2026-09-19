package gates

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The decision-table completeness and combine-fold properties are behavioural
// tests living next to the code they exercise, in internal/gateway — they call
// real production functions, which a source-text check cannot. This file holds
// the two structural properties no behavioural test can express, because there
// is no input that makes "a Record type has a field shaped like a routing
// effect" or "code outside decision.go reads a routing-relevant field of
// fact.Fact" observable at runtime. Both are silent until the day someone
// reads the wrong field under load — which from the outside looks like no bug
// at all: no error, no panic, just a decision the table never made.

// TestNoRecordCarriesALoopEffect keeps the accounting half of the fact
// vocabulary unable to smuggle control flow.
//
// fact.Record is the accounting half; fact.Fact is the routing half, and
// Fact.Kind is the only input the decision table takes. The split is enforced
// today because Record's method set has nowhere to put a LoopEffect — but
// nothing stops a future Record type from carrying a field named Loop or typed
// LoopEffect, and once persisted next to a Kind it happens to correlate with,
// that field is an easy thing for a later reader to reach for "just this
// once". This check keeps the field from existing at all.
//
// The type comparison strips a pointer and matches the bare name as well as
// any package-qualified spelling, because a Record may be declared inside the
// gateway package itself (test fakes already are) where LoopEffect needs no
// qualifier.
func TestNoRecordCarriesALoopEffect(t *testing.T) {
	files := parseTree(t, "internal")
	for _, rt := range discoverRecordTypes(t, files) {
		for _, f := range rt.fields {
			bare := strings.TrimPrefix(f.typeStr, "*")
			typeIsLoop := bare == "LoopEffect" || strings.HasSuffix(bare, ".LoopEffect")
			if f.name == "Loop" || typeIsLoop {
				t.Errorf("%s:%d: %s.%s looks like a routing effect on an accounting record; "+
					"Record types must not carry anything a future reader could mistake for "+
					"steering the relay loop", rt.file, rt.line, rt.name, f.name)
			}
		}
	}
}

// TestOnlyDecisionTableReadsRoutingFactFields keeps the decision table the
// single place a routing fact is turned into a decision.
//
// fact.Fact's own field comments draw the line: Kind is the one field the
// decision table keys on, Reporter is logging-only and legitimately stamped
// throughout the kernel, Detail and Status feed the status-passthrough fold,
// and Reason is the persisted refusal code. The fold and the code both belong
// to the decision package (ResolveBatch and the helpers around it), which the
// compiler already fences off; business logic in the kernel that branches on
// Status, Detail, or Reason is a second, unaudited place a routing decision
// gets made — invisible to anything that audits the table for completeness.
// Since the fold moved into its own package, no gateway file is trusted: the
// kernel consumes a Resolved verdict, never the raw fields.
//
// This is the one check that needs real type information rather than syntax:
// Status, Detail, and Reason are common field names, and only a type checker
// can tell a fact.Fact's Status from any other type's. Test files are excluded
// — a test reaching into a Fact to build a fixture is not a runtime bypass.
func TestOnlyDecisionTableReadsRoutingFactFields(t *testing.T) {
	root := repoRoot(t)
	mod := modulePath(t)
	gwDir := filepath.Join(root, "internal", "gateway")

	fset := token.NewFileSet()
	var files []*ast.File
	entries, err := os.ReadDir(gwDir)
	if err != nil {
		t.Fatalf("reading internal/gateway: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, filepath.Join(gwDir, e.Name()), nil, 0)
		if perr != nil {
			t.Fatalf("parsing %s: %v", e.Name(), perr)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		t.Fatal("no production files found under internal/gateway; the check would pass by finding nothing")
	}

	info := &types.Info{
		Types:      make(map[ast.Expr]types.TypeAndValue),
		Selections: make(map[*ast.SelectorExpr]*types.Selection),
	}
	cfg := &types.Config{
		Importer: importer.ForCompiler(fset, "source", nil),
		Error:    func(err error) { t.Logf("type-check note: %v", err) },
	}
	if _, err := cfg.Check(mod+"/internal/gateway", fset, files, info); err != nil {
		t.Logf("type-checking internal/gateway surfaced errors (see go build for the "+
			"authoritative list): %v", err)
	}

	factFactType := mod + "/internal/fact.Fact"
	// Counts every selector that resolved to a fact.Fact field, gated or not.
	// If the importer breaks, no selector resolves, and without this the check
	// would report a clean pass over code it never actually saw.
	resolvedFactSelectors := 0

	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			xt, ok := info.Types[sel.X]
			if !ok || xt.Type == nil {
				return true
			}
			named := namedTypeOf(xt.Type)
			if named == nil || named.String() != factFactType {
				return true
			}
			resolvedFactSelectors++
			switch sel.Sel.Name {
			case "Status", "Detail", "Reason":
				p := fset.Position(sel.Pos())
				rel, _ := filepath.Rel(root, p.Filename)
				t.Errorf("%s:%d: reads fact.Fact.%s outside the decision package — routing "+
					"decisions must go through the decision table; branching on this field here "+
					"is a second, unaudited place the relay loop can be steered from",
					filepath.ToSlash(rel), p.Line, sel.Sel.Name)
			}
			return true
		})
	}

	if resolvedFactSelectors == 0 {
		t.Fatal("no fact.Fact field access resolved anywhere in internal/gateway; " +
			"the type importer has broken and this check inspected nothing")
	}
}

// namedTypeOf unwraps a pointer to reach the named type underneath, since
// fact.Fact is read both by value and by pointer.
func namedTypeOf(t types.Type) *types.Named {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	n, _ := t.(*types.Named)
	return n
}

// TestTheVerdictIsAdoptedWholeOrNotAtAll holds the property three rounds of
// review kept finding broken in a new place.
//
// Everything a caller ends up seeing, and everything that gets persisted about
// why, has to come from ONE reported fact: the status, the error type, the
// words, the reason, and whether the verdict is worth remembering past the
// attempt. Assign those from separate folds and a batch can produce an answer
// no single reporter gave — a caller told their quota ran out when their
// payload was refused, a row filed under a reason belonging to neither.
//
// Earlier versions of this check tried to describe the SHAPE of a correct fold:
// count the scopes, or walk for the branch the assignment sits in. Both could
// be satisfied by code that still split the verdict, and the second could be
// walked around entirely by putting an assignment somewhere the walk did not
// descend. So the check stopped describing shapes. It reads every assignment in
// the function, and requires that none of these fields is written directly —
// the only way to set them is the one helper that sets all of them together.
func TestTheVerdictIsAdoptedWholeOrNotAtAll(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(root, "internal", "decision", "decision.go"), nil, 0)
	if err != nil {
		t.Fatalf("parsing decision.go: %v", err)
	}

	var combineFn, adoptFn *ast.FuncDecl
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		switch fd.Name.Name {
		case "Combine":
			combineFn = fd
		case "adoptVerdict":
			adoptFn = fd
		}
	}
	if combineFn == nil || adoptFn == nil {
		t.Fatal("Combine or adoptVerdict not found in decision.go; the fold was restructured and " +
			"this check needs re-aiming rather than passing by finding nothing")
	}

	// The fields that make up one verdict. Assigning any of them outside the
	// adopting helper is what splits it.
	verdictFields := map[string]bool{
		"Status": true, "Code": true, "ErrType": true, "Sticky": true,
		"statusFrom": true, "rejectDetail": true, "reason": true,
	}
	// The effects that legitimately fold on their own strength, and so are the
	// only fields combine may write directly.
	foldedEffects := map[string]bool{
		"Loop": true, "loopFrom": true, "Circuit": true,
		"Budget": true, "Settle": true, "Defined": true,
	}

	// The accumulator's name, so a write can be recognised whatever it is
	// spelled as.
	acc := accumulatorName(t, combineFn)

	// Every assignment anywhere in combine, however deeply nested. No manual
	// descent: a walk that has to know which statement kinds to enter is a walk
	// that can be stepped around by using one it forgot.
	//
	// Two things are rejected. A verdict field written by name is the obvious
	// one. The other is any whole-accumulator write — `*p = ...`, or assigning
	// a struct that contains these fields — which sets them without ever
	// naming them, and would let a rename or an extracted struct walk straight
	// past a check that only knew the seven names.
	var offenders []string
	note := func(what string, pos token.Pos) {
		offenders = append(offenders, fmt.Sprintf("%s at line %d", what, fset.Position(pos).Line))
	}
	ast.Inspect(combineFn, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		// The accumulator's own declaration seeds the fold with one operand
		// whole, which is the one place taking everything at once is right.
		if as.Tok == token.DEFINE {
			return true
		}
		for _, lhs := range as.Lhs {
			switch t := lhs.(type) {
			case *ast.SelectorExpr:
				if verdictFields[t.Sel.Name] {
					note(t.Sel.Name, t.Pos())
					continue
				}
				// A field that is itself a struct can carry the verdict inside
				// it. Only the effects folded by strength are safe to write.
				if id, ok := t.X.(*ast.Ident); ok && id.Name == acc && !foldedEffects[t.Sel.Name] {
					note("unrecognised field "+t.Sel.Name, t.Pos())
				}
			case *ast.StarExpr, *ast.Ident:
				if id, ok := lhs.(*ast.Ident); ok && id.Name != acc {
					continue // an ordinary local, not the accumulator
				}
				note("the whole accumulator", lhs.Pos())
			}
		}
		return true
	})
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("combine assigns verdict fields directly (%s): every part of a verdict must "+
			"come from the same fact, and a field written on its own can come from another one. "+
			"Set them through adoptVerdict, which takes them all at once", strings.Join(offenders, ", "))
	}

	// The helper has to set all of them, and set them unconditionally. A field
	// assigned inside an `if` is a field that keeps the losing fact's value
	// whenever the condition is false — the same split, one level down.
	assigned := map[string]bool{}
	for _, st := range adoptFn.Body.List {
		as, ok := st.(*ast.AssignStmt)
		if !ok {
			continue
		}
		for _, lhs := range as.Lhs {
			if sel, ok := lhs.(*ast.SelectorExpr); ok {
				assigned[sel.Sel.Name] = true
			}
		}
	}
	for field := range verdictFields {
		if !assigned[field] {
			t.Errorf("adoptVerdict does not set %s at the top of its body: a field it leaves "+
				"alone — or sets only under a condition — keeps whatever the losing fact put "+
				"there, which is the split this check exists to prevent", field)
		}
	}

	// And nothing else may write these fields. Restricting the search to
	// Combine would only move the problem: a second helper doing the
	// assignment is invisible to a check that looks at one function. The
	// unexported half is fenced by the compiler at the package boundary, but
	// the exported half (Status, Code, ErrType, Sticky) is assignable from the
	// kernel, so the kernel's tree is scanned alongside the package's own.
	for _, dir := range []string{
		filepath.Join("internal", "decision"),
		filepath.Join("internal", "gateway"),
	} {
		for _, f := range parseTree(t, dir) {
			// Deployment glue under the gateway tree wires adapters, not
			// verdicts; its row-copying assignments touch fields that
			// merely share names with the verdict vocabulary.
			if strings.HasPrefix(f.rel, "internal/gateway/osswire/") {
				continue
			}
			if strings.HasSuffix(f.rel, "_test.go") {
				continue
			}
			ast.Inspect(f.ast, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for _, lhs := range as.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if !ok || !verdictFields[sel.Sel.Name] {
						continue
					}
					rel, line := f.pos(sel)
					if rel == "internal/decision/decision.go" &&
						sel.Pos() >= adoptFn.Body.Pos() && sel.Pos() <= adoptFn.Body.End() {
						continue
					}
					t.Errorf("%s:%d writes %s outside adoptVerdict: the verdict has one place it "+
						"can be set, and a second one is a second fact it can come from",
						rel, line, sel.Sel.Name)
				}
				return true
			})
		}
	}
}

// accumulatorName returns the name combine folds into, read from its first
// statement rather than assumed, so renaming it does not quietly turn every
// check above into a no-op.
func accumulatorName(t *testing.T, fn *ast.FuncDecl) string {
	t.Helper()
	if len(fn.Body.List) > 0 {
		if as, ok := fn.Body.List[0].(*ast.AssignStmt); ok && len(as.Lhs) == 1 {
			if id, ok := as.Lhs[0].(*ast.Ident); ok {
				return id.Name
			}
		}
	}
	t.Fatal("combine does not open by declaring an accumulator; the fold was restructured and " +
		"the checks above would no longer know what a write to it looks like")
	return ""
}
