// Package diag is the damage reporting every demuxer shares: the Warnings
// list, the Strict refusal, and the translation of a source offset into a
// file offset for a source cut out behind a leading tag.
package diag

import (
	"fmt"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/internal/satmath"
	"github.com/colespringer/waxflow/waxerr"
)

// Log is a demuxer's damage reporting: it lists by default and refuses
// under Strict. It stores offsets as the source gave them and reports them
// as file offsets.
type Log struct {
	w      container.Warnings
	prefix string
	strict bool
	base   int64
}

// New is the Log of a demuxer whose errors start with prefix, under opts
// (nil is the zero options).
func New(prefix string, opts *container.DemuxerOptions) Log {
	l := Log{prefix: prefix}
	if opts != nil {
		l.strict, l.base = opts.Strict, opts.Base
	}
	return l
}

// Warn records tolerated damage at off. Under Strict it records nothing and
// returns the damage as malformed input, naming the file offset when off
// names one.
func (l *Log) Warn(off int64, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if l.strict {
		if off < 0 {
			return waxerr.Malformed(l.prefix, "%s", msg)
		}
		return waxerr.Malformed(l.prefix, "%s (at offset %d)", msg, l.At(off))
	}
	l.w.Damage(off, msg)
	return nil
}

// Note records a finding Strict never escalates, at off.
func (l *Log) Note(off int64, format string, args ...any) {
	l.w.Note(off, fmt.Sprintf(format, args...))
}

// At is off as a file offset, for a message that prints one, held at the
// largest int64 so a crafted offset never wraps. A negative offset names no
// position and stays as it is.
func (l *Log) At(off int64) int64 {
	if off < 0 {
		return off
	}
	return satmath.Add(off, l.base)
}

// Probe runs fn with Strict off and rolls back what it records. clean says
// fn recorded no damage; first is the first damage it listed, its offset
// the source's, for a caller that reports it again through Warn.
func (l *Log) Probe(fn func()) (clean bool, first *container.Warning) {
	strict, mark, seen := l.strict, l.w.Mark(), l.w.DamageCount()
	l.strict = false
	defer func() {
		l.strict = strict
		clean = l.w.DamageCount() == seen
		for _, w := range l.w.Since(mark) {
			if w.Kind == container.Damage {
				first = &w
				break
			}
		}
		l.w.Rollback(mark)
	}()
	fn()
	return
}

// Mark is where the list stands, for a scan whose findings are dropped
// unless it succeeds. Unlike Probe it leaves Strict as it is.
func (l *Log) Mark() container.WarningsMark { return l.w.Mark() }

// Rollback drops what was recorded after m, the count included.
func (l *Log) Rollback(m container.WarningsMark) { l.w.Rollback(m) }

// Strict reports whether the Log refuses damage. Inside a Probe it is false.
func (l *Log) Strict() bool { return l.strict }

// List is what was listed, in order, at file offsets.
func (l *Log) List() []container.Warning {
	ws := l.w.List()
	if l.base == 0 || len(ws) == 0 {
		return ws
	}
	out := make([]container.Warning, len(ws))
	for i, w := range ws {
		w.Offset = l.At(w.Offset)
		out[i] = w
	}
	return out
}

// DamageCount is every damage recorded, the list's cap and dedup aside.
func (l *Log) DamageCount() int { return l.w.DamageCount() }
