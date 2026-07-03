package admission

import "testing"

func TestPoolExhaustionAndRelease(t *testing.T) {
	p := New(2, 1)

	r1, ok1 := p.AcquireLive()
	r2, ok2 := p.AcquireLive()
	if !ok1 || !ok2 {
		t.Fatal("two live slots must acquire")
	}
	if _, ok := p.AcquireLive(); ok {
		t.Fatal("third live acquire must fail")
	}
	if p.LiveInUse() != 2 {
		t.Fatalf("LiveInUse = %d", p.LiveInUse())
	}

	// Job pool is independent of the live pool.
	if _, ok := p.AcquireJob(); !ok {
		t.Fatal("job slot must acquire while live pool is saturated")
	}

	r1()
	r1() // idempotent: a double release must not free someone else's slot
	if p.LiveInUse() != 1 {
		t.Fatalf("LiveInUse after release = %d", p.LiveInUse())
	}
	if _, ok := p.AcquireLive(); !ok {
		t.Fatal("released slot must be reusable")
	}
	r2()
}

func TestMinimumOneSlot(t *testing.T) {
	p := New(0, -3)
	if _, ok := p.AcquireLive(); !ok {
		t.Fatal("live pool floor is one slot")
	}
	if _, ok := p.AcquireJob(); !ok {
		t.Fatal("job pool floor is one slot")
	}
}
