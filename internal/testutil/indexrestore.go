package testutil

import (
	"testing"

	"github.com/colespringer/waxflow/container"
)

// DeclinesARestoreOnceRead fails t unless a restore lands on the reader that
// delivers the packets: a walked open's snapshot is adopted by an unread open
// and declined by one that has read a packet. open opens the same bytes
// afresh on each call, as a container.Indexer and a container.Walker.
func DeclinesARestoreOnceRead(t testing.TB, open func() (container.Demuxer, error)) {
	t.Helper()
	fresh := func() (container.Demuxer, container.Indexer) {
		t.Helper()
		d, err := open()
		if err != nil {
			t.Fatal(err)
		}
		ix, ok := d.(container.Indexer)
		if !ok {
			t.Fatalf("%T is not a container.Indexer", d)
		}
		return d, ix
	}
	walked, ix := fresh()
	w, ok := walked.(container.Walker)
	if !ok {
		t.Fatalf("%T is not a container.Walker", walked)
	}
	if err := w.Walk(t.Context()); err != nil {
		t.Fatal(err)
	}
	blob := ix.IndexSnapshot()
	if _, unread := fresh(); blob == nil || !unread.RestoreIndex(blob) {
		t.Fatal("an unread open declined the walked open's snapshot")
	}
	read, readIx := fresh()
	var pkt container.Packet
	if err := read.ReadPacket(&pkt); err != nil {
		t.Fatal(err)
	}
	if readIx.RestoreIndex(blob) {
		t.Errorf("an open that has delivered a packet adopted an index")
	}
}
