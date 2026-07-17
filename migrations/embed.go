// Package migrations embeds the SQL migration files into the compiled
// binary so the single-binary deployment (design doc §9) is actually
// self-contained — earlier this package didn't exist and migrations were
// read from a "migrations/<driver>" path relative to the process's working
// directory, which breaks for any deployment that doesn't run from the repo
// root (systemd, Docker, ...).
package migrations

import "embed"

//go:embed sqlite/*.sql
var SQLiteFS embed.FS

//go:embed postgres/*.sql
var PostgresFS embed.FS
