package eventlog

import (
	"testing"
	"time"
)

func entryAt(i int) Entry {
	return Entry{Ts: time.Unix(int64(i), 0), Text: string(rune('a' + i))}
}

func TestRecentReturnsNewestFirst(t *testing.T) {
	l := New(10)
	for i := 0; i < 3; i++ {
		l.Add(entryAt(i))
	}
	got, total := l.Recent(10, 0)
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
	want := []string{"c", "b", "a"}
	for i, e := range got {
		if e.Text != want[i] {
			t.Fatalf("got[%d].Text = %q, want %q", i, e.Text, want[i])
		}
	}
}

func TestOverCapacityDropsOldest(t *testing.T) {
	l := New(3)
	for i := 0; i < 5; i++ { // 0,1,2,3,4 — capacity 3, so 0 and 1 fall off
		l.Add(entryAt(i))
	}
	got, total := l.Recent(10, 0)
	if total != 3 {
		t.Fatalf("total = %d, want 3 (capped at capacity)", total)
	}
	want := []string{"e", "d", "c"}
	for i, e := range got {
		if e.Text != want[i] {
			t.Fatalf("got[%d].Text = %q, want %q", i, e.Text, want[i])
		}
	}
}

func TestRecentPagination(t *testing.T) {
	l := New(10)
	for i := 0; i < 5; i++ {
		l.Add(entryAt(i))
	}
	// newest-first order is 4,3,2,1,0 — page starting at offset 2, limit 2
	// should be 2,1.
	got, total := l.Recent(2, 2)
	if total != 5 {
		t.Fatalf("total = %d, want 5", total)
	}
	if len(got) != 2 || got[0].Text != "c" || got[1].Text != "b" {
		t.Fatalf("Recent(2, 2) = %+v, want [c b]", got)
	}
}

func TestRecentOffsetPastEndReturnsEmpty(t *testing.T) {
	l := New(10)
	l.Add(entryAt(0))
	got, total := l.Recent(10, 5)
	if len(got) != 0 {
		t.Fatalf("expected no entries past the end, got %+v", got)
	}
	if total != 1 {
		t.Fatalf("total = %d, want 1", total)
	}
}

func TestEmptyLog(t *testing.T) {
	l := New(10)
	got, total := l.Recent(10, 0)
	if len(got) != 0 || total != 0 {
		t.Fatalf("Recent on empty log = %+v, %d, want [], 0", got, total)
	}
}

func TestClear(t *testing.T) {
	l := New(3)
	for i := 0; i < 5; i++ {
		l.Add(Entry{Code: i})
	}
	l.Clear()
	if got, total := l.Recent(10, 0); total != 0 || len(got) != 0 {
		t.Fatalf("after Clear: %d entries (total %d)", len(got), total)
	}
	l.Add(Entry{Code: 9})
	if got, total := l.Recent(10, 0); total != 1 || got[0].Code != 9 {
		t.Fatalf("after Clear and one Add: %+v (total %d)", got, total)
	}
}
