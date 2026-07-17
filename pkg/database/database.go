// Package database provides GORM database connection initialization.
// Supports SQLite (default, zero-dependency single file) and PostgreSQL
// (production/multi-instance), selected via Config.Driver — design doc §6.
package database

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/glebarez/sqlite" // pure-Go sqlite driver: no cgo, keeps cross-compilation simple (design doc §9)
	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/yolorouter/yolorouter-ce/pkg/logger"
)

// DB is the global database connection instance.
var DB *gorm.DB

// sqliteConnectionQuery is the file: URI query string every application-
// level SQLite connection should open with: mode=rw (never silently
// create/recreate the database file — see
// ensureSQLiteFileRestrictivePermissions) plus foreign_keys enabled on
// every physical connection the driver opens (survives connection pool
// churn, unlike a one-off PRAGMA exec against a single connection).
// Shared between database.Init's own connection and ResetSQLite's
// post-delete reconnect so both stay consistent — a narrower query (e.g.
// BackupSQLite's read-only "mode=rw" alone) is fine for connections that
// only ever read, but any connection that runs migrations or application
// writes needs this full one.
const sqliteConnectionQuery = "mode=rw&_pragma=foreign_keys(1)"

// Config holds database connection configuration.
type Config struct {
	Driver          string // sqlite | postgres
	SQLitePath      string
	PostgresDSN     string
	MaxIdleConns    int
	MaxOpenConns    int
	ConnMaxLifetime time.Duration
	LogLevel        string
}

// Init initializes the database connection for the configured driver.
func Init(cfg Config) error {
	logLevel := gormlogger.Silent
	switch cfg.LogLevel {
	case "info":
		logLevel = gormlogger.Info
	case "warn":
		logLevel = gormlogger.Warn
	case "error":
		logLevel = gormlogger.Error
	}
	gormLog := &GormLogger{LogLevel: logLevel}

	var dialector gorm.Dialector
	switch cfg.Driver {
	case "postgres":
		dialector = postgres.New(postgres.Config{DSN: cfg.PostgresDSN, PreferSimpleProtocol: true})
	case "sqlite":
		// Ensure the parent directory exists regardless of how this config
		// was obtained — first-run auto-generation already creates it, but
		// an existing hand-edited or previously-generated config.yaml could
		// point at a data directory that no longer exists (e.g. cleaned up
		// by hand), which would otherwise surface as an unhelpful
		// "unable to open database file" error from SQLite itself.
		if err := os.MkdirAll(filepath.Dir(cfg.SQLitePath), 0o755); err != nil {
			return fmt.Errorf("create sqlite data directory: %w", err)
		}
		if err := ensureSQLiteFileRestrictivePermissions(cfg.SQLitePath); err != nil {
			return fmt.Errorf("create sqlite database file: %w", err)
		}
		// mode=rw (the same file:...?mode=rw URI form BackupSQLite uses) means
		// this Open can only ever touch a file that already exists — closing
		// the residual gap between ensureSQLiteFileRestrictivePermissions
		// pre-creating the file above and this Open actually happening (e.g.
		// SQLitePath resolving through a dangling symlink, or the file being
		// deleted in between by something else): a plain path DSN's default
		// mode=rwc would let SQLite silently recreate the file right here with
		// its own default (world/group-readable) permissions instead.
		//
		// _pragma=foreign_keys(1) (glebarez/go-sqlite's per-connection pragma
		// hook, applied every time the driver opens a new physical
		// connection) replaces a one-off `PRAGMA foreign_keys = ON` exec'd
		// against whichever single connection happened to be open right
		// after Open — SetMaxOpenConns(1) below bounds the pool to one
		// connection at a time, but doesn't guarantee database/sql never
		// closes and reopens a fresh physical connection (e.g. after a
		// dropped connection), which would silently come back up with
		// foreign key enforcement off again. sqliteConnectionQuery is the
		// same query string ResetSQLite's own reconnect uses, so both paths
		// stay consistent.
		dialector = sqlite.Open(sqliteFileURI(cfg.SQLitePath, sqliteConnectionQuery))
	default:
		return fmt.Errorf("unsupported database driver %q (must be sqlite or postgres)", cfg.Driver)
	}

	db, err := gorm.Open(dialector, &gorm.Config{
		Logger: gormLog,
		NowFunc: func() time.Time {
			return time.Now().UTC()
		},
	})
	if err != nil {
		return err
	}

	sqlDB, err := db.DB()
	if err != nil {
		return err
	}

	if cfg.Driver == "sqlite" {
		// SQLite 部署形态本身只支持单服务实例（设计文档 §6），单连接即可避免
		// "database is locked" 类并发写冲突。
		// foreign_keys is enabled via the DSN's _pragma=foreign_keys(1) above
		// (see the comment there), not a one-off Exec here — that would only
		// cover whichever single physical connection happened to be open at
		// this moment, not ones database/sql opens later to replace it.
		sqlDB.SetMaxOpenConns(1)
	} else {
		maxIdle := cfg.MaxIdleConns
		if maxIdle <= 0 {
			maxIdle = 10
		}
		maxOpen := cfg.MaxOpenConns
		if maxOpen <= 0 {
			maxOpen = 100
		}
		connLifetime := cfg.ConnMaxLifetime
		if connLifetime <= 0 {
			connLifetime = time.Hour
		}
		sqlDB.SetMaxIdleConns(maxIdle)
		sqlDB.SetMaxOpenConns(maxOpen)
		sqlDB.SetConnMaxLifetime(connLifetime)
	}

	DB = db

	logger.Info("database connected successfully", zap.String("driver", cfg.Driver))
	return nil
}

// ensureSQLiteFileRestrictivePermissions creates path with 0600 permissions
// if it doesn't already exist yet — a no-op if it does, since retroactively
// tightening an existing database's permissions is a separate, deliberate
// operation this scaffolding doesn't perform on the caller's behalf. This
// matters because SQLite's own default file creation (when Open/exec first
// touches a genuinely new file) uses 0644 masked by the process umask —
// world/group-readable under any typical umask like 022 — so without
// pre-creating the file ourselves, every fresh database (first boot, or
// after db:reset deletes and recreates one) would be exposed to other
// local users on the same machine for its entire lifetime, not just a
// transient window.
func ensureSQLiteFileRestrictivePermissions(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil
		}
		return err
	}
	return f.Close()
}

// GormLogger is a custom GORM Logger that bridges to the pkg/logger zap instance.
type GormLogger struct {
	LogLevel gormlogger.LogLevel
}

func (l *GormLogger) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	newLogger := *l
	newLogger.LogLevel = level
	return &newLogger
}

func (l *GormLogger) Info(ctx context.Context, msg string, data ...interface{}) {
	if l.LogLevel >= gormlogger.Info {
		logger.Sugar().Infof(msg, data...)
	}
}

func (l *GormLogger) Warn(ctx context.Context, msg string, data ...interface{}) {
	if l.LogLevel >= gormlogger.Warn {
		logger.Sugar().Warnf(msg, data...)
	}
}

func (l *GormLogger) Error(ctx context.Context, msg string, data ...interface{}) {
	if l.LogLevel >= gormlogger.Error {
		logger.Sugar().Errorf(msg, data...)
	}
}

func (l *GormLogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	if l.LogLevel <= gormlogger.Silent {
		return
	}

	elapsed := time.Since(begin)
	sql, rows := fc()

	fields := []zap.Field{
		zap.String("duration", elapsed.String()),
		zap.Int64("rows", rows),
		zap.String("sql", sql),
	}

	switch {
	case err != nil && !errors.Is(err, gorm.ErrRecordNotFound) && l.LogLevel >= gormlogger.Error:
		logger.Error("database error", append(fields, zap.Error(err))...)
	case elapsed > 200*time.Millisecond && l.LogLevel >= gormlogger.Warn:
		logger.Warn("slow database query", fields...)
	case l.LogLevel >= gormlogger.Info:
		logger.Debug("database query", fields...)
	}
}
