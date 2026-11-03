package container_test

import (
	"testing"

	"github.com/colespringer/waxflow/container"
)

// TestWarningsCapDedupeAndCount: damage is listed up to the cap and
// deduplicated, counted past both; notes are listed on their own cap and
// take no damage slot.
func TestWarningsCapDedupeAndCount(t *testing.T) {
	var w container.Warnings
	for i := range container.MaxWarnings + 10 {
		w.Damage(int64(i), "damaged block, skipped")
	}
	w.Damage(0, "damaged block, skipped") // seen already
	w.Note(0, "a note")
	w.Note(0, "a note") // notes are not deduplicated: each is a fact stated once by its caller
	damage, notes := 0, 0
	for _, x := range w.List() {
		if x.Kind == container.Damage {
			damage++
		} else {
			notes++
		}
	}
	if damage != container.MaxWarnings {
		t.Errorf("%d damage entries listed, want the cap of %d", damage, container.MaxWarnings)
	}
	if notes != 2 {
		t.Errorf("%d notes listed, want 2", notes)
	}
	if got := w.DamageCount(); got != container.MaxWarnings+11 {
		t.Errorf("DamageCount = %d, want every damage recorded (%d)", got, container.MaxWarnings+11)
	}
}

// TestWarningsRollback: a mark taken before a speculative scan says what the
// scan added, and rolling back to it drops those entries and their count.
func TestWarningsRollback(t *testing.T) {
	var w container.Warnings
	w.Damage(1, "before")
	m := w.Mark()
	w.Damage(2, "during")
	w.Note(3, "during too")
	if since := w.Since(m); len(since) != 2 {
		t.Errorf("Since the mark lists %d entries, want 2", len(since))
	}
	w.Rollback(m)
	if l := w.List(); len(l) != 1 || l[0].Msg != "before" {
		t.Errorf("after the rollback the list is %v, want the entry before the mark", l)
	}
	if got := w.DamageCount(); got != 1 {
		t.Errorf("DamageCount = %d after the rollback, want 1", got)
	}
	w.Damage(2, "again")
	if got := w.DamageCount(); got != 2 {
		t.Errorf("DamageCount = %d after a damage past the rollback, want 2", got)
	}
}
