package logging

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

type Logger struct {
	base   *slog.Logger
	level  string
	closer io.Closer
}

// New selects stdout or a private append-only file and keeps the level for authentication filtering.
func New(level, destination string) (*Logger, error) {
	var output io.Writer = os.Stdout
	var closer io.Closer
	if destination != "stdout" {
		// New directories use mode 0700; the opened log file is explicitly tightened to mode 0600.
		if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
			return nil, err
		}
		file, err := os.OpenFile(destination, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return nil, err
		}
		if err := file.Chmod(0600); err != nil {
			_ = file.Close()
			return nil, err
		}
		output = file
		closer = file
	}
	return &Logger{base: slog.New(slog.NewJSONHandler(output, nil)), level: level, closer: closer}, nil
}

func (l *Logger) Close() error {
	if l.closer != nil {
		return l.closer.Close()
	}
	return nil
}

// These event-specific methods always emit regardless of configured severity; upstream_errors relies on them.
func (l *Logger) UpstreamError(message string, attrs ...any) {
	l.base.Error(message, attrs...)
}

func (l *Logger) Retry(message string, attrs ...any) {
	l.base.Warn(message, attrs...)
}

func (l *Logger) AuthenticationFailure() {
	// The default mode focuses on upstream incidents and deliberately omits downstream key failures.
	if l.level != "upstream_errors" {
		l.base.Error("downstream authentication failed")
	}
}

func (l *Logger) Error(message string, attrs ...any) {
	// Operational failures are always visible; the default still omits authentication failures.
	l.base.Error(message, attrs...)
}
