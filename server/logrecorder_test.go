package server

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

// LogRecorder is a slog.Handler that keeps every record, at every level, for
// the tests on both sides of the package boundary.
type LogRecorder struct {
	mu   sync.Mutex
	recs []LogRecord
}

// LogRecord is one record a LogRecorder kept.
type LogRecord struct {
	Level slog.Level
	Msg   string
	Attrs string
}

func (r LogRecord) String() string { return fmt.Sprintf("%s %q %s", r.Level, r.Msg, r.Attrs) }

func (h *LogRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (h *LogRecorder) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *LogRecorder) WithGroup(string) slog.Handler            { return h }

func (h *LogRecorder) Handle(_ context.Context, r slog.Record) error {
	var attrs []string
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, a.String())
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, LogRecord{Level: r.Level, Msg: r.Message, Attrs: strings.Join(attrs, " ")})
	return nil
}

// Warnings returns the records at Warn or above.
func (h *LogRecorder) Warnings() []LogRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []LogRecord
	for _, r := range h.recs {
		if r.Level >= slog.LevelWarn {
			out = append(out, r)
		}
	}
	return out
}

// Records returns every record kept.
func (h *LogRecorder) Records() []LogRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]LogRecord(nil), h.recs...)
}

// Has reports whether a record with this level and message was logged.
func (h *LogRecorder) Has(level slog.Level, msg string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.recs {
		if r.Level == level && r.Msg == msg {
			return true
		}
	}
	return false
}
