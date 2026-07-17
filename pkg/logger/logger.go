// Package logger provides structured logging based on Zap.
// Supports dual output: console (color) + file (JSON).
package logger

import (
	"os"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

var (
	globalLogger = zap.NewNop()
	sugarLogger  = globalLogger.Sugar()
)

// Config holds logger configuration.
type Config struct {
	Level    string
	Filename string // empty disables file output
	Console  bool
}

// Init initializes the logging system. Call once at process startup.
func Init(cfg Config) {
	level := zapcore.InfoLevel
	if cfg.Level != "" {
		_ = level.UnmarshalText([]byte(cfg.Level))
	}

	var cores []zapcore.Core

	if cfg.Console || cfg.Filename == "" {
		consoleEncoder := zapcore.NewConsoleEncoder(zapcore.EncoderConfig{
			TimeKey:        "T",
			LevelKey:       "L",
			NameKey:        "N",
			CallerKey:      "C",
			FunctionKey:    zapcore.OmitKey,
			MessageKey:     "M",
			StacktraceKey:  "S",
			LineEnding:     zapcore.DefaultLineEnding,
			EncodeLevel:    zapcore.CapitalColorLevelEncoder,
			EncodeTime:     zapcore.ISO8601TimeEncoder,
			EncodeDuration: zapcore.StringDurationEncoder,
			EncodeCaller:   zapcore.ShortCallerEncoder,
		})
		cores = append(cores, zapcore.NewCore(consoleEncoder, zapcore.AddSync(os.Stdout), level))
	}

	if cfg.Filename != "" {
		fileEncoder := zapcore.NewJSONEncoder(zapcore.EncoderConfig{
			TimeKey:        "timestamp",
			LevelKey:       "level",
			NameKey:        "module",
			CallerKey:      "caller",
			FunctionKey:    "func",
			MessageKey:     "msg",
			StacktraceKey:  "stacktrace",
			LineEnding:     zapcore.DefaultLineEnding,
			EncodeLevel:    zapcore.LowercaseLevelEncoder,
			EncodeTime:     zapcore.ISO8601TimeEncoder,
			EncodeDuration: zapcore.SecondsDurationEncoder,
			EncodeCaller:   zapcore.ShortCallerEncoder,
		})
		writer := zapcore.AddSync(&lumberjack.Logger{
			Filename:   cfg.Filename,
			MaxSize:    100, // MB
			MaxBackups: 7,
			MaxAge:     30, // days
			Compress:   true,
		})
		cores = append(cores, zapcore.NewCore(fileEncoder, writer, level))
	}

	globalLogger = zap.New(zapcore.NewTee(cores...), zap.AddCaller(), zap.AddCallerSkip(1))
	sugarLogger = globalLogger.Sugar()
}

// Debug logs a debug message.
func Debug(msg string, fields ...zap.Field) { globalLogger.Debug(msg, fields...) }

// Info logs an info message.
func Info(msg string, fields ...zap.Field) { globalLogger.Info(msg, fields...) }

// Warn logs a warning message.
func Warn(msg string, fields ...zap.Field) { globalLogger.Warn(msg, fields...) }

// Error logs an error message.
func Error(msg string, fields ...zap.Field) { globalLogger.Error(msg, fields...) }

// Fatal logs a fatal message and exits the program.
func Fatal(msg string, fields ...zap.Field) { globalLogger.Fatal(msg, fields...) }

// Sugar returns the sugared logger for printf-style logging.
func Sugar() *zap.SugaredLogger { return sugarLogger }

// Sync flushes any buffered log entries. Call before process exit.
func Sync() error { return globalLogger.Sync() }
