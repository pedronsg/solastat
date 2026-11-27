package main

import "testing"

func TestUsableSerial(t *testing.T) {
	good := []string{"NAXIM-SM01-000002", "00000000a1b2c3d4", "b8:27:eb:12:34:56", "6f1c2b9e-3a4d-4e5f-8a9b-0c1d2e3f4a5b"}
	bad := []string{"", "  ", "unknown", "Unknown", "UNKNOWN", "none", "N/A", "0000000000000000",
		"00000000-0000-0000-0000-000000000000", "ffffffff-ffff-ffff-ffff-ffffffffffff", "00:00:00:00:00:00", "ab"}
	for _, v := range good {
		if !usableSerial(v) {
			t.Errorf("usableSerial(%q) = false, want true", v)
		}
	}
	for _, v := range bad {
		if usableSerial(v) {
			t.Errorf("usableSerial(%q) = true, want false", v)
		}
	}
}
