package main

import "testing"

func TestCommHealthIgnoresIsolatedFailures(t *testing.T) {
	var h commHealth
	if !h.observe("", 12) || !h.ok {
		t.Fatal("the first good cycle should mark communication ok")
	}
	for i := 1; i < commErrorAfterCycles; i++ {
		if h.observe("connection refused", 0) || !h.ok {
			t.Fatalf("failure %d of %d should not mark communication down yet", i, commErrorAfterCycles)
		}
	}
	if h.observe("", 12) || !h.ok {
		t.Fatal("a good cycle in between should keep communication ok")
	}
}

func TestCommHealthGoesDownAfterConsecutiveFailures(t *testing.T) {
	var h commHealth
	h.observe("", 12)
	var changed bool
	for i := 0; i < commErrorAfterCycles; i++ {
		changed = h.observe("connection refused", 0)
	}
	if !changed || h.ok || h.lastErr != "connection refused" {
		t.Fatalf("after %d failures in a row communication should be down (ok=%v changed=%v)", commErrorAfterCycles, h.ok, changed)
	}
	if !h.observe("", 12) || !h.ok {
		t.Fatal("one good cycle should bring communication back")
	}
}

func TestCommHealthDownWhenNeverReachable(t *testing.T) {
	var h commHealth
	for i := 0; i < commErrorAfterCycles; i++ {
		if h.down() {
			t.Fatalf("down after only %d failures", i)
		}
		h.observe("connection refused", 0)
	}
	if !h.down() || h.ok {
		t.Fatal("an inverter that never answered should count as down after the threshold")
	}
}
