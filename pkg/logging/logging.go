// Package logging provides a dual-writer logger that writes to both stdout
// (for Docker logs) and a file (data/proxy.log). It offers leveled logging
// (DEBUG, INFO, WARN, ERROR) with timestamps and caller prefixes.
package logging

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// LogLevel controls verbosity.
type LogLevel int

const (
	// LevelDebug logs everything.
	LevelDebug LogLevel = iota
	// LevelInfo logs info, warn, error.
	LevelInfo
	// LevelWarn logs warn and error only.
	LevelWarn
	// LevelError logs error only.
	LevelError
)

const (
	// defaultLogFile is the path to the log file inside the data directory.
	defaultLogFile = "data/proxy.log"
)

var (
	mu       sync.Mutex
	level    LogLevel = LevelInfo
	fileW    *os.File
	stdW     io.Writer
	combined io.Writer
	logger   *log.Logger
)

// Init initializes the dual-writer logger. It creates the log file at
// data/proxy.log (appends if it exists) and also writes to stdout. Call this
// once at program start, before any logging calls.
func Init(lvl LogLevel) error {
	mu.Lock()
	defer mu.Unlock()

	level = lvl

	// Ensure data directory exists
	dir := filepath.Dir(defaultLogFile)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create log directory: %w", err)
		}
	}

	f, err := os.OpenFile(defaultLogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to open log file %s: %w", defaultLogFile, err)
	}
	fileW = f
	stdW = os.Stdout
	combined = io.MultiWriter(stdW, fileW)
	logger = log.New(combined, "", log.LstdFlags|log.Lmicroseconds)

	return nil
}

// Close closes the log file. Call at shutdown.
func Close() {
	mu.Lock()
	defer mu.Unlock()
	if fileW != nil {
		fileW.Close()
		fileW = nil
	}
}

// SetLevel changes the log level at runtime.
func SetLevel(lvl LogLevel) {
	mu.Lock()
	defer mu.Unlock()
	level = lvl
}

// GetLevel returns the current log level.
func GetLevel() LogLevel {
	mu.Lock()
	defer mu.Unlock()
	return level
}

// logf is the internal formatted logger.
func logf(lvl LogLevel, prefix, format string, args ...any) {
	mu.Lock()
	defer mu.Unlock()
	if lvl < level || logger == nil {
		return
	}
	logger.Printf(prefix+" "+format, args...)
}

// logln is the internal line logger (no format string).
func logln(lvl LogLevel, prefix string, args ...any) {
	mu.Lock()
	defer mu.Unlock()
	if lvl < level || logger == nil {
		return
	}
	logger.Println(append([]any{prefix}, args...)...)
}

// Debugf logs a DEBUG-level formatted message.
func Debugf(format string, args ...any) {
	logf(LevelDebug, "[DEBUG]", format, args...)
}

// Debug logs a DEBUG-level message.
func Debug(args ...any) {
	logln(LevelDebug, "[DEBUG]", args...)
}

// Infof logs an INFO-level formatted message.
func Infof(format string, args ...any) {
	logf(LevelInfo, "[INFO]", format, args...)
}

// Info logs an INFO-level message.
func Info(args ...any) {
	logln(LevelInfo, "[INFO]", args...)
}

// Warnf logs a WARN-level formatted message.
func Warnf(format string, args ...any) {
	logf(LevelWarn, "[WARN]", format, args...)
}

// Warn logs a WARN-level message.
func Warn(args ...any) {
	logln(LevelWarn, "[WARN]", args...)
}

// Errorf logs an ERROR-level formatted message.
func Errorf(format string, args ...any) {
	logf(LevelError, "[ERROR]", format, args...)
}

// Error logs an ERROR-level message.
func Error(args ...any) {
	logln(LevelError, "[ERROR]", args...)
}

// Fatalf logs an ERROR-level formatted message and exits the process.
func Fatalf(format string, args ...any) {
	logf(LevelError, "[FATAL]", format, args...)
	Close()
	os.Exit(1)
}
