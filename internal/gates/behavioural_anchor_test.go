package gates

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// TestBehaviouralGateTestsStillExist pins the names `make gates` runs by.
//
// Half of the structural guarantees live as ordinary behavioural tests next to
// the code they exercise — the decision table's completeness, the combine
// fold's algebra, the Delivery validity rules, observer input isolation. The
// gates target runs those by exact name with `go test -run`, and `-run` has a
// property that quietly defeats the whole arrangement: a pattern that matches
// nothing is not an error. Exit code 0, "no tests to run", and a renamed or
// deleted test drops out of the gate without anything turning red.
//
// This check closes that hole: each name the Makefile invokes must exist as a
// test function in the file expected to hold it. Renaming one now fails here,
// which is the loud signal `-run` never gives. When a rename is intentional,
// this table and the Makefile move together — they are two halves of one list.
func TestBehaviouralGateTestsStillExist(t *testing.T) {
	root := repoRoot(t)
	anchors := []struct {
		file  string
		tests []string
	}{
		{
			file: "internal/decision/decision_test.go",
			tests: []string{
				"TestDecisionTableIsComplete",
				"TestCombineIsCommutative",
				"TestCombineIsAssociative",
				"TestCombineNeverProducesUndefined",
				"TestAVerdictIsRememberedOnlyByTheFactThatSuppliedIt",
			},
		},
		{
			file:  "internal/gateway/extension_test.go",
			tests: []string{"TestObserversCannotReachEachOtherOrTheAuditBody"},
		},
		{
			file:  "internal/gateway/sticky_scope_test.go",
			tests: []string{"TestAVerdictDoesNotOutliveTheKeyThatEarnedIt"},
		},
		{
			file:  "internal/gateway/usage_roundtrip_test.go",
			tests: []string{"TestEveryCountSurvivesTheDeliveryHop"},
		},
		{
			file: "internal/gateway/attempt_decision_test.go",
			tests: []string{
				"TestFoldUpstreamDecision",
				"TestKernelBaselineTimelineEntries",
			},
		},
		{
			file: "internal/gateway/kernel_sink_test.go",
			tests: []string{
				"TestReportKernelFactStampsProvenanceAndFolds",
				"TestSettlementNotesCarryKernelProvenance",
			},
		},
		{
			file:  "internal/fact/delivery_test.go",
			tests: []string{"TestValidateRejectsImpossibleDeliveries"},
		},
	}

	for _, anchor := range anchors {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, filepath.Join(root, filepath.FromSlash(anchor.file)), nil, 0)
		if err != nil {
			t.Errorf("parsing %s: %v — the file a gate's behavioural half lives in is gone", anchor.file, err)
			continue
		}
		declared := map[string]bool{}
		for _, decl := range f.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Recv == nil {
				declared[fd.Name.Name] = true
			}
		}
		for _, name := range anchor.tests {
			if !declared[name] {
				t.Errorf("%s no longer declares %s: `go test -run` exits 0 on a pattern that "+
					"matches nothing, so without this check the gate would silently stop "+
					"running it", anchor.file, name)
			}
		}
	}
}
