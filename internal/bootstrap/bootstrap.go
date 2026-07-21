// Package bootstrap provides the shared initialization sequence (config
// load/generate + validate -> logger -> database connection) used by every
// resource-initializing subcommand (serve, db:migrate, db:rollback, db:status,
// ...). help/version/unknown-command paths never call Init.
package bootstrap

import (
	"fmt"
	"strconv"
	"strings"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/config"
	"github.com/yolorouter/yolorouter/pkg/database"
	"github.com/yolorouter/yolorouter/pkg/logger"
)

// App bundles the shared dependencies every resource-initializing subcommand
// (serve, db:migrate, db:rollback, ...) needs. help/version/unknown-command
// paths never call Init.
type App struct {
	Config *config.Config
	DB     *gorm.DB
}

func Init(explicitConfigPath string) (*App, error) {
	cfg, err := config.Load(explicitConfigPath)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	// logger.Init has no failure mode (it never returns an error).
	logger.Init(logger.Config{Level: cfg.Log.Level})

	dbCfg := database.Config{
		Driver:     cfg.Database.Driver,
		SQLitePath: cfg.Database.SQLitePath,
	}
	if cfg.Database.Driver == "postgres" {
		dbCfg.PostgresDSN = buildPostgresDSN(cfg.Database)
	}

	// database.Init sets the package-level database.DB variable rather than
	// returning the *gorm.DB directly (see pkg/database/database.go).
	if err := database.Init(dbCfg); err != nil {
		return nil, fmt.Errorf("init database: %w", err)
	}

	return &App{Config: cfg, DB: database.DB}, nil
}

// Close releases the database connection and flushes buffered log output.
// Failures here are logged rather than escalated as an overall command
// failure (the same "log and continue" convention gracefulShutdown uses for
// its own cleanup phases) — but they must still be logged, not silently
// discarded, or a real close/flush failure (e.g. a pending transaction that
// wouldn't commit, or a full disk dropping the last buffered log lines)
// becomes completely invisible.
func (a *App) Close() error {
	sqlDB, err := a.DB.DB()
	if err == nil {
		if closeErr := sqlDB.Close(); closeErr != nil {
			logger.Error("close database connection", zap.Error(closeErr))
		}
	}
	if syncErr := logger.Sync(); syncErr != nil {
		logger.Error("flush log output", zap.Error(syncErr))
	}
	return nil
}

// buildPostgresDSN builds a libpq keyword/value connection string, properly
// quoting and escaping each value — a plain fmt.Sprintf (as an earlier
// version did) breaks as soon as any value contains whitespace, a single
// quote, or a backslash (all valid in a Postgres password, for instance).
// sslmode defaults to "disable" only because DatabaseConfig.SSLMode itself
// defaults to "disable" for local-dev convenience (see internal/config);
// remote/production deployments should set database.sslmode explicitly.
func buildPostgresDSN(db config.DatabaseConfig) string {
	sslMode := db.SSLMode
	if sslMode == "" {
		sslMode = "disable"
	}
	parts := []string{
		"host=" + escapeDSNValue(db.Host),
		"port=" + strconv.Itoa(db.Port),
		"user=" + escapeDSNValue(db.User),
		"password=" + escapeDSNValue(db.Password),
		"dbname=" + escapeDSNValue(db.DBName),
		"sslmode=" + escapeDSNValue(sslMode),
	}
	return strings.Join(parts, " ")
}

// escapeDSNValue quotes and escapes a single libpq keyword/value pair's
// value per the format documented for PQconnectdb: values containing
// whitespace or a single quote must be wrapped in single quotes, with
// embedded backslashes and single quotes backslash-escaped.
func escapeDSNValue(v string) string {
	if v == "" {
		return "''"
	}
	if !strings.ContainsAny(v, " '\\\t\n\r\v\f") {
		return v
	}
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `'`, `\'`)
	return "'" + v + "'"
}
