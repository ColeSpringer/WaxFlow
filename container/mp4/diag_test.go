package mp4

import (
	"testing"

	"github.com/colespringer/waxflow/container"
)

// TestMP4CountsDamagePastTheListCap: the count of damage goes on past the
// list's cap, so container.DamageOf sees damage met once the list is full.
func TestMP4CountsDamagePastTheListCap(t *testing.T) {
	d := &Demuxer{}
	for i := range 100 {
		if err := d.warn(int64(i), "damage %d", i); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(d.Warnings()); n != container.MaxWarnings {
		t.Errorf("listed %d, want the cap of %d", n, container.MaxWarnings)
	}
	if n := container.DamageOf(d); n != 100 {
		t.Errorf("DamageOf = %d after 100 findings", n)
	}
}
