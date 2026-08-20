package gates

import (
	"fmt"
	"go/ast"
	"strings"
	"testing"
)

// TestServeStartupMigratesThroughBackupOrchestration pins that the serve
// startup path reaches migrations only through database.MigrateWithBackup,
// never by calling database.RunMigrations directly.
//
// The serve path is the one that runs unattended: the in-app updater swaps
// the binary and restarts, a Docker pull swaps the image, and in both cases
// the next serve start is the first (and only) moment a schema upgrade can
// be preceded by an automatic SQLite snapshot. Wiring serve back to the
// bare RunMigrations would compile, pass every unit test (cmd has no test
// harness of its own), and silently ship upgrades with no rollback point —
// exactly the regression this check exists to make loud.
//
// Scope is deliberately serve.go only. The maintenance commands in
// db_commands.go (db:migrate, db:reset's re-migrate) stay on the bare
// RunMigrations by design: they are explicit operator actions, and
// db:backup already exists for taking a snapshot first.
func TestServeStartupMigratesThroughBackupOrchestration(t *testing.T) {
	files := parseTree(t, "cmd/yolorouter")

	var serve *goFile
	for i := range files {
		if files[i].rel == "cmd/yolorouter/serve.go" {
			serve = &files[i]
			break
		}
	}
	if serve == nil {
		t.Fatal("cmd/yolorouter/serve.go not found; the check would pass by finding nothing")
	}

	// Resolve the local name pkg/database is imported under in serve.go
	// (default "database", but an alias must not blind the check).
	databaseName := ""
	for _, imp := range serve.ast.Imports {
		if strings.Trim(imp.Path.Value, `"`) == "github.com/yolorouter/yolorouter/pkg/database" {
			if imp.Name != nil {
				databaseName = imp.Name.Name
			} else {
				databaseName = "database"
			}
		}
	}
	if databaseName == "" {
		t.Fatal("serve.go no longer imports pkg/database; the call-site checks inspected nothing")
	}

	orchestrationCalls := 0
	var violations []string
	ast.Inspect(serve.ast, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok || ident.Name != databaseName {
			return true
		}
		switch sel.Sel.Name {
		case "RunMigrations":
			rel, line := serve.pos(call)
			violations = append(violations, fmt.Sprintf(
				"%s:%d: direct %s.RunMigrations call — serve must migrate through %s.MigrateWithBackup so a schema upgrade always leaves a snapshot behind",
				rel, line, databaseName, databaseName))
		case "MigrateWithBackup":
			orchestrationCalls++
		}
		return true
	})

	if orchestrationCalls == 0 {
		t.Errorf("serve.go never calls %s.MigrateWithBackup: the startup path lost its pre-migration backup", databaseName)
	}
	for _, v := range violations {
		t.Error(v)
	}
}
