package lru

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// keys lists the cache from least to most recently used.
func keys(c *Cache[string, int]) []string {
	var out []string
	c.DeleteFunc(func(k string, _ int) bool {
		out = append(out, k)
		return false
	})
	return out
}

func wantKeys(t *testing.T, c *Cache[string, int], want ...string) {
	t.Helper()
	if got := keys(c); !slices.Equal(got, want) {
		t.Fatalf("cache holds %v, want %v (least recently used first)", got, want)
	}
	if n := c.Len(); n != len(want) {
		t.Fatalf("Len = %d, want %d", n, len(want))
	}
}

func TestEvictsLeastRecentlyUsed(t *testing.T) {
	c := New[string, int](2)
	c.Put("a", 1)
	c.Put("b", 2)
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatalf("Get(a) = %d, %v, want 1", v, ok)
	}
	if v, ok := c.Get("absent"); ok || v != 0 {
		t.Fatalf("Get of a key never put = %d, %v, want a miss", v, ok)
	}
	c.Put("c", 3)
	if _, ok := c.Peek("b"); ok {
		t.Error("b survived; it was the least recently used")
	}
	if _, ok := c.Peek("a"); !ok {
		t.Error("a was evicted after a Get refreshed it")
	}
	if n := c.Len(); n != 2 {
		t.Errorf("Len = %d, want the 2-entry bound", n)
	}
}

func TestPeekDoesNotRefresh(t *testing.T) {
	c := New[string, int](2)
	c.Put("a", 1)
	c.Put("b", 2)
	if v, ok := c.Peek("a"); !ok || v != 1 {
		t.Fatalf("Peek(a) = %d, %v, want 1", v, ok)
	}
	if _, ok := c.Peek("absent"); ok {
		t.Error("Peek reported a key that was never put")
	}
	c.Put("c", 3)
	if _, ok := c.Peek("a"); ok {
		t.Error("a survived the put at the bound: a Peek is not a use")
	}
	if _, ok := c.Peek("b"); !ok {
		t.Error("b was evicted ahead of the least recently used entry")
	}
}

func TestPutReplacesWithoutEvicting(t *testing.T) {
	c := New[string, int](2)
	c.Put("a", 1)
	c.Put("b", 2)
	c.Put("a", 10)
	if v, _ := c.Peek("a"); v != 10 {
		t.Errorf("a = %d after the replace, want 10", v)
	}
	// Nothing went, and the replace was a use, so b is now the oldest.
	wantKeys(t, c, "b", "a")
	if k, v, ok := c.RemoveOldest(); !ok || k != "b" || v != 2 {
		t.Errorf("RemoveOldest = %q=%d (%v), want b=2", k, v, ok)
	}
}

func TestDeleteFuncRemovesTheMatchesAndKeepsTheOrder(t *testing.T) {
	c := New[string, int](8)
	if n := c.DeleteFunc(func(string, int) bool { return true }); n != 0 {
		t.Fatalf("DeleteFunc removed %d from an empty cache", n)
	}
	for i, k := range []string{"a", "b", "c", "d"} {
		c.Put(k, i)
	}
	c.Get("a")
	wantKeys(t, c, "b", "c", "d", "a")
	if n := c.DeleteFunc(func(_ string, v int) bool { return v%2 == 1 }); n != 2 {
		t.Fatalf("DeleteFunc removed %d, want 2", n)
	}
	wantKeys(t, c, "c", "a")
	if n := c.DeleteFunc(func(string, int) bool { return true }); n != 2 {
		t.Fatalf("DeleteFunc removed %d, want both entries", n)
	}
	wantKeys(t, c)
	c.Put("e", 5)
	c.Put("f", 6)
	wantKeys(t, c, "e", "f")
}

func TestRemoveOldestTakesTheLeastRecentlyUsed(t *testing.T) {
	c := New[string, int](4)
	if k, v, ok := c.RemoveOldest(); ok || k != "" || v != 0 {
		t.Fatalf("RemoveOldest on an empty cache = %q=%d (%v)", k, v, ok)
	}
	c.Put("a", 1)
	c.Put("b", 2)
	c.Put("c", 3)
	c.Get("a")
	for _, want := range []string{"b", "c", "a"} {
		k, _, ok := c.RemoveOldest()
		if !ok || k != want {
			t.Fatalf("RemoveOldest = %q (%v), want %q", k, ok, want)
		}
		if _, held := c.Peek(k); held {
			t.Errorf("%q is still held after its removal", k)
		}
	}
	if n := c.Len(); n != 0 {
		t.Errorf("Len = %d after removing every entry", n)
	}
	if _, _, ok := c.RemoveOldest(); ok {
		t.Error("RemoveOldest found an entry in a drained cache")
	}
}

func TestDeleteAtEitherEndKeepsTheOrder(t *testing.T) {
	c := New[string, int](3)
	c.Put("a", 1)
	c.Put("b", 2)
	c.Put("c", 3)
	if !c.Delete("a") || c.Delete("a") {
		t.Error("Delete reports whether the key was held")
	}
	c.Put("d", 4)
	wantKeys(t, c, "b", "c", "d")
	if !c.Delete("d") {
		t.Error("Delete missed the most recently used entry")
	}
	c.Put("e", 5)
	wantKeys(t, c, "b", "c", "e")
	c.Put("f", 6)
	wantKeys(t, c, "c", "e", "f")
}

func TestABoundOfOneHoldsTheLastPut(t *testing.T) {
	c := New[string, int](1)
	c.Put("a", 1)
	c.Put("a", 2)
	wantKeys(t, c, "a")
	c.Put("b", 3)
	wantKeys(t, c, "b")
	if v, ok := c.Get("b"); !ok || v != 3 {
		t.Errorf("Get(b) = %d, %v, want 3", v, ok)
	}
}

func TestNewRefusesABoundBelowOne(t *testing.T) {
	for _, max := range []int{0, -3} {
		func() {
			defer func() {
				msg, _ := recover().(string)
				if want := fmt.Sprintf("max %d", max); !strings.HasPrefix(msg, "lru: ") || !strings.Contains(msg, want) {
					t.Errorf("New(%d) panicked with %q, want the package and %q", max, msg, want)
				}
			}()
			New[string, int](max)
			t.Errorf("New(%d) returned", max)
		}()
	}
}

func TestUnboundedNeverEvicts(t *testing.T) {
	c := NewUnbounded[int, int]()
	for i := range 100 {
		c.Put(i, i)
	}
	if n := c.Len(); n != 100 {
		t.Errorf("Len = %d, want every entry put", n)
	}
	if k, _, ok := c.RemoveOldest(); !ok || k != 0 {
		t.Errorf("RemoveOldest = %d (%v), want the first entry put", k, ok)
	}
}

func TestDeleteFirstFuncRemovesTheLeastRecentlyUsedMatchAndStops(t *testing.T) {
	for _, tc := range []struct {
		name    string
		matches []string
		want    []string // what stays, least recently used first
		visits  int
	}{
		{"first", []string{"b", "d"}, []string{"c", "d", "a"}, 1},
		{"middle", []string{"d", "a"}, []string{"b", "c", "a"}, 3},
		{"last", []string{"a"}, []string{"b", "c", "d"}, 4},
		{"none", nil, []string{"b", "c", "d", "a"}, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New[string, int](8)
			for i, k := range []string{"a", "b", "c", "d"} {
				c.Put(k, i)
			}
			c.Get("a") // the order is now b, c, d, a
			visits := 0
			went := c.DeleteFirstFunc(func(k string, _ int) bool {
				visits++
				return slices.Contains(tc.matches, k)
			})
			if went != (tc.matches != nil) {
				t.Errorf("DeleteFirstFunc reported %v", went)
			}
			if visits != tc.visits {
				t.Errorf("del ran %d times, want %d", visits, tc.visits)
			}
			wantKeys(t, c, tc.want...)
			c.Put("e", 9)
			wantKeys(t, c, append(tc.want, "e")...)
		})
	}
}
