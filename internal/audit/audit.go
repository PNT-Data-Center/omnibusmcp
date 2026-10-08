// Package audit writes a JSON-lines record of every tool call and every
// rejected request.
package audit

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// Logger records audit events. The zero value discards everything.
type Logger struct {
	log    *slog.Logger
	closer io.Closer
}

// Open creates the logger. path "-" writes to stderr (journald under systemd).
func Open(path string) (*Logger, error) {
	if path == "-" {
		return newLogger(os.Stderr, nil), nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	return newLogger(f, f), nil
}

// Discard returns a logger that drops all events (tests, CLI listings).
func Discard() *Logger { return newLogger(io.Discard, nil) }

// New writes events to w (tests).
func New(w io.Writer) *Logger { return newLogger(w, nil) }

func newLogger(w io.Writer, c io.Closer) *Logger {
	return &Logger{log: slog.New(slog.NewJSONHandler(w, nil)), closer: c}
}

// Call statuses.
const (
	StatusOK = "ok"
	// StatusError: the tool ran and reported an error, or rejected its input.
	StatusError = "error"
	// StatusRejected: the request failed at protocol level (e.g. unknown tool).
	StatusRejected = "rejected"
)

// ToolCall records a single tool invocation.
func (l *Logger) ToolCall(tool, session string, args any, took time.Duration, status string, callErr error) {
	attrs := []any{
		"tool", tool,
		"session", session,
		"args", args,
		"duration_ms", took.Milliseconds(),
	}
	if callErr != nil {
		attrs = append(attrs, "error", callErr.Error())
	}
	l.log.Info("tool_call", append(attrs, "status", status)...)
}

// AuthFailure records a rejected HTTP request.
func (l *Logger) AuthFailure(remote, reason string) {
	l.log.Warn("auth_failure", "remote", remote, "reason", reason)
}

// Event records a lifecycle event such as startup.
func (l *Logger) Event(msg string, attrs ...any) { l.log.Info(msg, attrs...) }

// Close releases the underlying file.
func (l *Logger) Close() error {
	if l.closer != nil {
		return l.closer.Close()
	}
	return nil
}
