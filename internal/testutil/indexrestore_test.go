package testutil

import (
	"context"
	"testing"

	"github.com/colespringer/waxflow/container"
)

// indexedFake delivers packets and keeps an index a restore may adopt only
// before its reader has delivered one. second puts the index behind a reader
// of its own, which no delivery advances.
type indexedFake struct {
	second    bool
	delivered int // packets the packet reader has delivered
	indexed   int // packets the index's reader has delivered
}

func (f *indexedFake) Tracks() []container.Track { return nil }

func (f *indexedFake) ReadPacket(*container.Packet) error {
	if f.delivered++; !f.second {
		f.indexed = f.delivered
	}
	return nil
}

func (f *indexedFake) KeepsIndex() bool      { return true }
func (f *indexedFake) IndexSnapshot() []byte { return []byte("index") }
func (f *indexedFake) RestoreIndex(blob []byte) bool {
	return f.indexed == 0 && string(blob) == "index"
}
func (f *indexedFake) Walk(context.Context) error { return nil }
func (f *indexedFake) Walked() bool               { return true }

// TestDeclinesARestoreOnceReadReportsASecondReader: an index on the packet
// reader passes, and one behind a second reader is reported.
func TestDeclinesARestoreOnceReadReportsASecondReader(t *testing.T) {
	for _, second := range []bool{false, true} {
		rec := &errTB{TB: t}
		DeclinesARestoreOnceRead(rec, func() (container.Demuxer, error) { return &indexedFake{second: second}, nil })
		if reported := len(rec.errs) != 0; reported != second {
			t.Errorf("second reader %v: reported %q", second, rec.errs)
		}
	}
}
